# Ingress, fencing, certificates, alerts and metrics

Part of [DEVELOPING.md](../../DEVELOPING.md). This file covers Traefik, which runs on every
host, and the leader's loops that talk to the outside world: Hetzner Cloud (Floating IP,
fencing), ACME and the alert channels.

## Traefik (`internal/ingress`, `agent/traefik.go`)

- Every agent runs Traefik (`ingress.Image`, pinned) as the Compose project
  `clowder-traefik` in the host network. Like every container, it has `restart: "no"`.
- Traefik is not a stack, and the state knows nothing about it. Its files live in
  `<Dir>/traefik/` next to `stacks/`, so the search for stacks that left never finds them.
  `names` refuses namespaces that start with `clowder-`, so no stack's project name can
  clash. Every place that treats Traefik differently asks `names.ClowdersOwn`: the
  supervisor (always restart it, even when unhealthy) and the stack runner's clean-up of
  secrets.
- `ingress.RenderStatic` writes `traefik.yml` with these entrypoints:
  - `web` and `websecure`: the Floating IP, ports 80 and 443. They trust no
    `X-Forwarded-For`, so Traefik sets the client's address itself.
  - `internal`: the node address, port 7480. It trusts `X-Forwarded-For` only from the node
    network.
  - `ping`: 127.0.0.1:7446, for the health check.

  `ingress.RenderCompose` writes the Compose file. Golden files are in
  `internal/ingress/testdata`; `bin/dev test-tools` checks the Compose file with Debian's
  docker-compose.
- `traefikRunner` has its own loop next to the stack runner, so a slow image pull for a
  stack never holds up the ingress. It runs compose up (`loop.WhilePaused`, like the stack
  runner) when the hash of the Compose file and `traefik.yml` differs from the one Traefik
  was started with: at the first pass, after Docker restarts
  (`loop.Signal.DockerRestarted`), and after a new image pin or Floating IP. A file removed
  or changed by hand comes back at the next pass. The runner reads only the Floating IP,
  the routes and the certificates from the state, because it runs at every Docker event.
- **Routes.** `ingress.RenderDynamic` writes `routes.yaml` for Traefik's file provider. For
  each service with `http` it writes routers `<entrypoint>.<project>.<service>` and a
  Traefik service `<project>.<service>`. Its servers are the service's containers on this
  host that run and, if they have a health check, are healthy (`docker.Inspect`), at their
  address in the network whose name sorts first. A route with no such container keeps its
  routers with no servers, so Traefik answers 503, not 404. Traefik watches the directory,
  so new routes need no restart.
- Every host routes every stack, because the Floating IP may move to any host. A stack on
  this host also gets a router on `internal`. A stack on another host is forwarded to that
  host's `internal` entrypoint (`Route.Forwarded`). Only the public entrypoints forward, so
  a request is forwarded at most once. TLS ends on the ingress host, which passes the
  request on as HTTP from its node address. `X-Forwarded-For` starts with the client; the
  stack host's Traefik appends the ingress host's node address.
- **HTTPS.** A route whose domain has a certificate gets a router on `websecure` and a
  router on `web` that redirects to it. Until the certificate exists, the route has only a
  plain router on `web`, so nobody sees Traefik's self-signed default certificate.
- The runner writes every certificate in the state, with its decrypted key, into
  `/run/clowder-secrets/clowder-traefik/`. This is the tmpfs of the stacks' secrets (0700),
  mounted read-only. It writes the certificates before the routes that name them. Traefik
  watches only the routes, so each file is named after a hash of its chain: a renewed
  certificate changes the routes, and Traefik reloads it. Old files are removed after the
  routes.
- **ACME challenges.** The router `acme-challenge` on `web` and `internal` sends
  `/.well-known/acme-challenge/` of every domain to the agent on 127.0.0.1:7445. It comes
  before every stack's router. The agent answers from the state (`State.Challenges`), so
  whichever host holds the Floating IP can answer the CA.
- The runner runs a pass at every change of the state, every 30 s, and at once when the
  supervisor sees a Docker event (start, die, health status; `loop.Signal.Now`). So a
  container that turns unhealthy leaves its route within seconds.

## Hetzner and ingress (`internal/hetzner`, `internal/controller`)

clowder runs only on Hetzner Cloud, with a Hetzner private network between the hosts. It
calls Hetzner's API directly, with no abstraction layer.

- **Configuration.** `hetzner.tokenSecretRef` names the project's token: a `{name, key}` of
  a Secret in `clowder-system`. `privateNetwork` is the private network's range. Every
  host's underlay must lie in it; the state checks this at each join and each apply.
- `hetzner.api` and `acme.directory` replace Hetzner's API and Let's Encrypt in test labs.
  Each is a full `http://` or `https://` URL; the token is sent to `hetzner.api`.
  `clusterspec.ServiceURLWarnings` warns about both, and that `http://` sends the token or
  the ACME requests unencrypted. An apply that changes the configuration returns the
  warnings, and a dry run always does. The leader logs them once per configuration
  (`warnServiceURLs`, in the Floating IP's loop).
- **The client.** `hetzner.Client` reads and sets where the Floating IP is routed, checks a
  server, and powers a server off and on (fencing). A server is identified by its
  `ServerID`, which `clowderd init` and `join --server-id` record. Since the host names its
  server itself, `Client.CheckServer` checks that the server's private address is the
  host's underlay and its public IPv4 is the host's public address (else
  `ErrOtherServer`). The join checks the joining host's server; the Floating IP's loop
  checks the first host's.
- `SetRetired` and `RetiredSerial` manage the label `clowder-retired`, which holds the
  certificate serial of a host that `clowderd recover` took out of the cluster. The leader
  never uses them.
- The client is small and uses `net/http`; the official SDK would pull Prometheus' client
  and more into clowderd. The API cannot look up a Floating IP by its address, so the
  client searches the project's list. It waits for each action to finish, and reads every
  response to the end so the connection can be reused. Its errors carry the API's message,
  never the token.
- The client retries a request up to `attempts` times, with a growing wait and jitter,
  while the API answers 429, 5xx or `locked`, or the network fails. A short busy moment of
  the API must not hold up a fence. For 429 it waits until `RateLimit-Reset`, because
  Hetzner sends no `Retry-After`. Every request is safe to send twice. `SetRetired` reads
  the server's labels and writes them back with its own added, because the API replaces
  all labels at once.
- **A token Hetzner refuses** raises the condition `hetzner-token`. `hetzner.TokenRefused`
  covers 401 and 403. It also covers a server or Floating IP the project does not have
  (`ErrNoSuchServer`, `ErrNoSuchFloatingIP`), because a token of another project sees none
  of the cluster's. Only the calls that fencing and the Floating IP make anyway notice
  this; no loop sends extra requests for it. Before a new token is applied, the cluster
  checks it once with `ServerIDs` (one request for the project's servers), and the CLI asks
  the user ([Resources](api.md#resources) and [The commands](api.md#the-commands)).
- `hetzner/hetznertest` fakes the API (servers, Floating IPs, instant power actions,
  labels) and records what the requests changed. A test puts its URL in `hetzner.api`, so
  the real client talks to it.
- **Floating IP.** `controller.RunFloatingIP` (`floatingip.go`) runs only on the
  leader. It picks the ingress host (`State.Ingress`, `set-ingress`) among the active hosts
  with the role `ingress`. It keeps the recorded ingress host while that host is active, so
  the Floating IP never moves back by itself. Otherwise it takes the first host in
  `ingress.preferredHosts` that can take it, else the one with the lowest ID. A host whose
  server Hetzner has not confirmed is checked first (`confirm-server`); if the check fails,
  it gets no Floating IP.
- At each change of the state and every 30 s, the loop asks Hetzner where the Floating IP
  is. If it is elsewhere (moved by hand, or a new ingress host), the loop routes it to the
  ingress host. It reads the API at most once per 30 s, which is about 120 of Hetzner's 3600
  requests an hour. Before it routes the address, it checks that it still leads
  (`VerifyLeader`): a leader cut off from the others must not take the address back from
  the host that a new leader chose.
- Every host has the Floating IP on its public interface, whether it holds it or not
  (`clowderd check` requires this). Hetzner routes it to one host only, so published ports
  and Traefik can bind it on every host, from boot.
- These call Hetzner: the leader's loops, the leader's side of a join, `clowderd recover`
  and the agent's start. Each decrypts the token from its Secret in the state and creates
  the client (`controller.OpenHetzner`, `State.HetznerToken`).
- **clowder's ports** are in the range 7400-7499. `stackspec.ReservedPort` keeps stacks out
  of it:

  | Port | Use | Listens on |
  |---|---|---|
  | 7400 | Raft | node address and underlay |
  | 7443 | the API | node address and underlay |
  | 7444 | `/metrics` | node address |
  | 7480 | Traefik's internal entrypoint | node address |
  | 7445 | the ACME challenges | 127.0.0.1 |
  | 7446 | Traefik's health check | 127.0.0.1 |

  On the Floating IP, ports 80 and 443 belong to Traefik.

## Fencing (`controller/health.go`, `controller/fencing.go`, `agent/health.go`)

When a host fails, the leader powers it off through Hetzner's API before any of its stacks
or volumes start elsewhere. Until then the failed host is the only one that runs them, and
it may still write.

- **Beats.** Every agent tells every voter every `BeatInterval` that it is alive
  (`sendBeats`, `POST /v1/internal/beats`). It starts at agent start, before its state has
  caught up: a beat says that the agent runs, not what it runs. Beats are not in the Raft
  log. Each voter keeps the last beat of each host in memory (`controller.Heard`); after its
  own start it counts from that start. Since every voter has the beats, a new leader knows
  at once how long each host has been silent.
- **Failed** (`fencer.watch`). A host that is active, draining or recovering becomes
  `failed` when a majority of the voters has not heard from it for `FailAfter`. The leader
  counts itself and asks the others with `GET /v1/internal/beats/{id}` (`askTimeout` each).
  A voter that does not answer, and the host itself, always count against the change. So a
  cut between the leader and one host fails no host, and a pause of all hosts shorter
  than `FailAfter` fails none. A failed host that a majority hears again within
  `backWithin` is `active` again. The leader decides this, never the host
  ([Host phases](state.md#host-phases)). A failed host keeps running its stacks
  (`state.KeepsStacks`).
- **Only what needs it** (`needsFence`). A failed host is fenced only if it has a stack
  that another active host could take over. A power cycle gains nothing for a host without
  stacks (for example one drained and rebooting for maintenance), or for one whose stacks
  no other host could take. Such a host stays failed, with its alert, and becomes active
  again once a majority hears it.
- **Fenced** (`fencer.fence`, `fencer.step`). The leader starts a fence-host operation for
  the first failed host that may be fenced (`whyNotFence`). A host may be fenced when:
  - a configuration names the Hetzner token;
  - Hetzner has confirmed the host's server;
  - no operation holds the host;
  - a majority of the voters stays active without it (if more hosts fail at once, the
    network is the likely cause, and powering hosts off would not help);
  - the last attempt to power it off was at least `fenceAgain` ago.

  Only one host is fenced at a time; the next waits until the operation is over.
- Starting the operation makes the host `fencing`. The leader checks that it still leads
  (`VerifyLeader`) and powers the server off, up to `powerOffTries` times with a pause that
  doubles. Once the server is off, the host is `fenced` and its stacks fail over
  ([Failover](placement.md#failover)). If the power-off fails, the host goes back to
  `failed`, the operation fails, and the alert `fencing-blocked` is raised. A user can then
  run `clowder host fence-confirm` to state that the server is off. The operation ends once
  the host's stacks run elsewhere, or after `StepTime`.
- **Back** (`fencer.powerOn`, `agent/recovery.go`). A fenced host whose operation is over is
  powered on, once per fence and leader, if Hetzner confirmed its server (otherwise, after
  a `fence-confirm`, the user powers it on). The host boots and starts nothing: no
  container runs after a boot, and `stacks.Runner` starts none on a host that is not
  `KeepsStacks`. It catches up with the leader
  ([Raft](cluster.md#raft-internalraftnode-and-the-data-key-internaldatakey)) and reports
  itself `recovering`. The volume reconciler reports each copy synced once DRBD has caught
  it up, and then the host reports itself `active`. Its stacks stay where they went, except
  those with replicas 1, which waited for it.
- **The flapping brake.** A host fenced `FlapLimit` times within `FlapWindow` (`FencedAt`)
  is not powered on again. It becomes `disabled`, with the alert `host-disabled`, until a
  user runs `clowder host enable`. A host whose agent crashes right after each start is
  fenced once and stays `fenced`, because it never reports. So there is no reboot loop.
- **Voters keep their vote** while they are down
  ([Raft](cluster.md#raft-internalraftnode-and-the-data-key-internaldatakey), Members,
  step 4).
- `fencing_test.go` runs the loop against the fake leader and the fake API.

## Certificates of the routes (`controller/acme.go`)

- `controller.RunACME` runs only on the leader and uses `golang.org/x/crypto/acme`. It runs
  at each change of the state and every 30 s. It keeps one certificate per domain of an
  HTTP route (`State.Certificates`, key encrypted). It deletes the certificates of domains
  that no stack names.
- The account (`State.ACMEAccount`, key encrypted) is created at the first order. It
  belongs to its directory, so a new `acme.directory` gets a new account. `acme.ca` is a
  PEM certificate that is trusted in addition to the system's CAs, for a test CA such as
  Pebble. It is at most `clusterspec.MaxCASize`.
- Orders run one domain after another:
  1. The domain must resolve to the Floating IP and nothing else. Otherwise the leader
     orders nothing and tries again after `dnsRetry`. This does not count towards the
     backoff, because nothing reached the CA, so Let's Encrypt's limit on failed
     validations is not touched.
  2. The answer to the HTTP-01 challenge goes into the state.
  3. The leader asks the ingress host's Traefik for the answer, on its internal entrypoint
     (node address, port 7480, `checkAddress`) with the domain as host. It repeats this
     until the answer comes, for at most `selfCheckTimeout`. The CA's request through the
     Floating IP lands on that host, and that host may apply the challenge later than the
     leader. Without an ingress host, the leader asks the Floating IP on port 80.
  4. Only then may the CA validate.

  When the order ends, the leader removes the answer from the state. A new leader removes
  answers that an old leader left behind. A failed account ends the pass, because Let's
  Encrypt allows only a few new accounts per address.
- A failed order backs off per domain, from `firstBackoff` doubling up to `maxBackoff`. The
  backoff lives in the leader's memory, so a new leader starts without it. Even a leader
  that changes often stays well below Let's Encrypt's limits.
- **Renewal** depends on the certificate's lifetime (`NotAfter` minus `NotBefore`, both in
  the state), because Let's Encrypt is making lifetimes shorter. By default a certificate
  is renewed when a third of its lifetime is left. `acme.renewBefore` overrides this if it
  is shorter than the lifetime. A longer value would renew every new certificate at once,
  so for such a certificate the leader keeps the default and raises the alert
  `renew-before-ignored`. Both rules use only the state, so no setting and no change of
  leader can cause a renewal loop.
- The tests run the whole order against a small ACME CA of their own
  (`controller/acme_test.go`), which validates by requesting the challenge.

## Alerts (`internal/notify`, `controller/wanted.go`, `controller/alerts.go`)

- **Channels** are listed in `alerts.channels` of the configuration, each with an optional
  `minSeverity`. Discord gets an embed colored by severity, a webhook a JSON POST of
  `notify.Message`. Email uses TLS from the start on port 465 and STARTTLS on any other
  port; `smtp.ca` is a PEM certificate in the configuration.
- URLs and passwords are values of Secrets in `clowder-system` that the channel names
  (`webhookSecretRef`, `urlSecretRef`, `smtp.passwordSecretRef`; the heartbeat's
  `urlSecretRef`). `notify.New` opens them through `notify.Secrets`. Errors never contain a
  URL. `http://` is allowed, because the live test's receivers need it.
- **The state** holds only the open alerts (`State.Alerts`, `put-alert`, `resolve-alert`).
  Each alert's key is its kind and subject (`pool-use/node0/vg/thin/data`), so one
  condition never has two open alerts. When a condition gets worse, the alert keeps its key
  and changes its severity. Subjects and messages contain no value that changes from report
  to report, because every change is a Raft write.
- **The leader's memory** (`controller.Alerts`) holds the deliveries:
  - A message goes out when an alert is raised or gets worse, every `remindEvery` while it
    is open, and when it is resolved. It goes to each channel whose `minSeverity` it
    reaches. The all-clear carries the highest severity that was sent, so a channel for
    critical alerts gets it even after a downgrade.
  - A failed message is retried after `firstRetry`, doubling up to `lastRetry`. Its channel
    gets nothing more in that pass, so a dead mail server cannot hold up the loop.
  - Waiting messages are keyed by the channel's position in `alerts.channels` and the
    configuration's generation, so a new generation drops them. `clowder cluster status`
    shows the failed deliveries; a follower asks the leader for them
    (`GET /v1/internal/alerts/failures`).
  - A new leader sends one reminder for each open alert: a duplicate is better than a lost
    alert.
- The heartbeat (`alerts.heartbeat`) is a GET every `interval` (default 1m), in its own
  loop so a slow channel cannot delay it.
- `clowder alert test` sends a test alert to every channel, from the agent the CLI asks.
  It is the one exception to "only the leader calls external services", so a channel can
  be tested while the cluster has no leader.
- **Conditions** that the state does not show are kept in memory (`controller.Conditions`)
  with the time they began. The loop that sees a condition records it (`Observe`):
  - the Floating IP not held (the Floating IP's loop);
  - Hetzner refusing the token (the Floating IP's loop and the fencing loop);
  - a host over capacity (the placement);
  - a failed host that cannot be fenced (the fencing loop);
  - Docker not answering, and a stack failing (the alerts loop, from `report-host` and
    `set-stack-status`).

  A new leader starts with no conditions, so a new alert comes a little late rather than
  wrongly. An open alert stays until the loop that observes its condition has looked
  (`Conditions.Seen`). An agent that no longer leads forgets its conditions. Each alerts
  pass forgets the conditions of stacks and hosts that are gone (`conditionKeys`,
  `Conditions.Keep`).
- `wanted_test.go` and `conditions_test.go` test `wantedAlerts` and `Conditions` on a state
  in memory. `alerts_test.go` tests the loop: delivery, retries, channels, and forgetting.

| Alert | Severity | Raised | Resolved |
|---|---|---|---|
| `pool-use` (data or metadata) | warning / critical | ≥ 80 % / ≥ 95 % | < 75 % / back to warning < 90 % |
| `root-use` | warning | ≥ 90 % | < 85 % |
| `docker-down` | critical | 2 min without an answer | answers |
| `ingress-not-held` | critical | 5 min | held again |
| `hetzner-token` | critical | Hetzner refuses the token for a fencing or Floating IP call (`hetzner.TokenRefused`) | the next such call is not refused |
| `over-capacity` | warning | at once | fits again |
| `certificate-not-renewed` | warning | less than half the renewal window left | renewed |
| `renew-before-ignored` | warning | for any certificate | value changed |
| `stack-waiting` | warning | 5 min (`WaitingSince`) | placed |
| `stack-failing` | critical | 2 min `failing` | up, applied anew or restarted |
| `host-failed` | warning | `failed` or `fencing` | active again, or fenced |
| `fencing-blocked` | critical | a failed host the leader cannot fence (`whyNotFence`) | fenced, active again |
| `host-fenced` | critical | `fenced` or `recovering` | active again |
| `host-disabled` | critical | `disabled` (flapping brake) | enabled |
| `split-brain` | critical | a host reports a copy that has split from a peer (`Host.SplitBrain`) | connected again (`volume resolve-splitbrain`) |
| `no-quorum` | critical | a primary copy without DRBD's quorum for 2 min (`Host.NoQuorum`) | quorum back |
| `no-ha` | critical | `clowderd recover` left one host (`RecoveredAt`) | three control-plane hosts are in service |

An alert is resolved below the threshold that raised it, so a value that hovers near the
threshold does not raise and resolve it at every report. Likewise, an alert that gets
better and then worse again, up to the severity already sent, sends nothing new. Only its
reminders and its all-clear go out.

**Without a leader** the alerts above are silent: either a majority of the voters is gone
(two hosts of three), or this host is cut off. So every agent also watches for itself
(`QuorumWatch`). Once it has had no Raft leader for `noLeaderFor`, it sends
`no-leader/<host>` (critical) to the channels in its own last state. It sends a reminder
every `remindEvery`, and the all-clear once a leader is back. It keeps what it has sent in
memory, not in the state, so an agent that restarts without a leader sends the alert again.

## Metrics (`internal/metrics`, `agent/metrics.go`)

- Every agent serves `/metrics` on its node address, port 7444 (`MetricsPort`), over plain
  HTTP without authentication. Only the cluster's hosts reach the node address, through
  WireGuard; containers do not (the nft table blocks them). No setting changes this.
- `internal/metrics` writes Prometheus' text format by hand, sorted, so it can be tested
  against golden files (`agent/testdata/metrics-*.golden.txt`). The official client
  library would pull more into clowderd than a few dozen values need.
- Each agent exports values about its own host: its phase and whether it leads, the Raft
  index, the pools' use, Docker, the root file system, the containers of its stacks by
  state, the supervisor's restarts per service, and its own resident memory.
- Values that exist once per cluster are exported only by the leader, so a Prometheus that
  scrapes every host counts them once: each host's room as the scheduler counts it
  (`scheduler.Rooms`), the certificates' expiry, the open alerts by severity, and the
  failed deliveries.
- The live tests run `promtool check metrics` and a scraping Prometheus.
