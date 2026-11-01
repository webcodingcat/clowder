# Ingress, certificates, alerts and metrics

Part of [DEVELOPING.md](../../DEVELOPING.md): Traefik on every host, and the leader's
loops that talk to the world outside.

## Traefik (`internal/ingress`, `agent/traefik.go`)

- Every agent runs Traefik (`ingress.Image`, pinned) as the Compose project
  `clowder-traefik` in the host network, with `restart: "no"` like every container. It is no
  stack: the state knows nothing of it, its files live in `<Dir>/traefik/` beside `stacks/`
  (so the search for stacks that left never finds it), and a stack's project starts with its
  namespace, which `names` refuses to start with `clowder-`. `clowdersOwn` answers whether a
  project is clowder's own; every place of the stack runner that treats Traefik apart asks
  it: the supervisor's rule (restart always, also when unhealthy) and the clean-up of
  secrets.
- `ingress.RenderStatic` writes `traefik.yml`: the entrypoints `web` and `websecure` on the
  Floating IP :80 and :443, `internal` on the node address :7480 (phase 2) trusting
  `X-Forwarded-For` only from the node network, and `ping` on 127.0.0.1:7446 for the health
  check. The public entrypoints trust no `X-Forwarded-For`, so Traefik sets the client's
  address itself. `ingress.RenderCompose` writes the Compose file.
- `traefikRunner` has a loop of its own beside the stack runner, so a long image pull of a
  stack never holds up the ingress. It runs compose up as the stack runner does
  (`composeUpHeld`, under the supervisor's hold) when the hash of the Compose file and
  `traefik.yml` differs from the one Traefik came up with: at the first pass after the agent
  started, after Docker restarted (`forgetApplied`), and after a new image pin or Floating IP. Each
  of its files is compared with what is on disk, so one removed or changed by hand comes
  back at the next pass. It reads only the Floating IP, the stacks' routes and the
  certificates from the state, no copy of all of it: it passes at every event of Docker's.
- **Routes.** `ingress.RenderDynamic` writes the file provider's `routes.yaml`: per service
  with `http` a service `<project>.<service>` whose servers are the containers of that
  service on this host that run and, if they have a health check, are healthy
  (`docker.Inspect`; the address in the network whose name sorts first), and routers
  `<entrypoint>.<project>.<service>`. A route without such a container keeps its routers with
  no servers, so Traefik answers 503, not 404. Traefik watches the directory, so new routes
  need no restart.
- **HTTPS.** A route whose domain has a certificate gets a router on `websecure` and one on
  `web` that redirects to it; before that only a plain router on `web`, so nobody sees
  Traefik's self-signed default certificate. The runner writes every certificate of the
  state and its opened key into `/run/clowder-secrets/clowder-traefik/` (the tmpfs of the
  stacks' secrets, 0700; mounted read-only), before the routes that name them. Traefik
  watches only the routes, so the files are named after a hash of the chain: a renewed
  certificate changes the routes, and Traefik reads it; old files go after the routes.
- **ACME challenges.** The router `acme-challenge` on `web` sends
  `/.well-known/acme-challenge/` of every domain, before every stack's router, to the agent
  on 127.0.0.1:7445, which answers from the state (`State.Challenges`): whichever host
  holds the Floating IP can answer the CA.
- The runner's passes come at every change of the state, every 30 s, and at once when the
  supervisor sees an event of Docker's (`passNow`: start, die, health status): a container that
  turns unhealthy leaves its route within seconds.
- Golden files in `internal/ingress/testdata`; `bin/dev test-tools` checks the Compose file
  with Debian's docker-compose.

## Hoster and ingress (`internal/hoster`, `internal/controller`)

- `cluster.yaml` names the hoster in a section of its own, chosen by `type` like backup
  targets and alert channels: `hoster: {type: hetzner, token: !secret ...}`, and `api`
  for a lab's API instead of the hoster's (a full `http://` or `https://` URL: the token
  goes there).
- `hoster.Hoster` is what clowder asks of a hoster: where the Floating IP is routed, route
  it, power a server off and on (fencing, phase 2). A server is its `ServerRef`, the
  hoster's ID that `clowderd init --server-ref` records. `hoster.New` picks the plugin.
- `hoster/hetzner` is a small client of the Hetzner Cloud API on `net/http`: the official
  SDK would bring Prometheus' client and more into clowderd. It finds the Floating IP among
  the project's (the API cannot look one up by its address) and waits for each action to
  finish. Its errors carry the API's message, never the token.
- `controller.RunFloatingIP` (`floatingip.go`) acts only on the leader. The ingress host (`State.Ingress`,
  `set-ingress`) is the recorded one while it is active and has a server ref, so the
  Floating IP never switches back by itself; otherwise the first of
  `ingress.preferred_hosts` that can, else the one with the lowest ID that can. At a change
  of the state and every 30 s it asks the hoster where the Floating IP is and routes it to
  the ingress host when it is elsewhere (moved by hand, or a new ingress host); it reads
  the API at most once per 30 s, about 120 of Hetzner's 3600 requests an hour.
- Every host has the Floating IP on its public interface, held or not (`clowderd check`
  requires it): the hoster routes it to one host only, so whatever binds it (published
  ports, Traefik) can bind it on every host and at boot.
- Only the leader's loops call the hoster: the controller opens the token from the state
  and makes the client (`HostParts.Hoster`, a fake in the tests). depguard keeps `hoster`
  out of the CLI, the state and the scheduler.
- **clowder's ports** lie in 7400-7499, which `stackspec.ReservedPort` keeps from the stacks:
  Raft 7400 and the API 7443 (node address and underlay), `/metrics` 7444 and Traefik's
  internal entrypoint 7480 (node address), the ACME challenges 7445 and Traefik's health
  check 7446 (127.0.0.1). On the Floating IP, 80 and 443 are Traefik's.

## Certificates of the routes (`controller/acme.go`)

- `controller.RunACME` acts only on the leader, with `golang.org/x/crypto/acme`, at a
  change of the state and every 30 s. It keeps one certificate per domain of an HTTP route
  (`State.Certificates`, the key sealed) and deletes those of domains no stack names.
- The account (`State.ACMEAccount`, the key sealed) is made at the first order and belongs
  to its directory: a new `acme.directory` gets a new account. `acme.ca` (a file next to
  `cluster.yaml`) is trusted besides the system's CAs: a test CA such as Pebble.
- An order, one domain after the other: the domain must resolve to the Floating IP and
  nothing else, or no order (retry in 10 minutes: it never reached the CA, so it does not
  count towards the backoff, and Let's Encrypt's limit on failed validations stays
  untouched). Then the answer to the HTTP-01 challenge goes into the state, and the leader
  asks the Floating IP :80 for it with the domain as host until Traefik returns it (at most
  2 minutes). The Floating IP is on every host's interface, so this reaches the leader's
  own Traefik: on one host the ingress host. With several hosts it proves nothing about
  the ingress host; phase 2 must ask that host, for example through its internal
  entrypoint. Only then may the CA validate. When the order ends, the leader removes the
  answer from the state; a new leader removes answers an old one left behind. A failed
  account makes the pass end: Let's Encrypt allows few new accounts per address.
- A failed order backs off per domain: 10 minutes, doubling up to 24 hours, kept in the
  leader's memory. A new leader starts again at none, so even a leader that changes often
  stays well below Let's Encrypt's limits.
- **Renewal** is relative to the lifetime (`NotAfter` minus `NotBefore`, both in the
  state), because Let's Encrypt makes lifetimes shorter: by default when a third is left.
  `acme.renew_before` overrides that while it is shorter than the lifetime; a longer one
  would renew every new certificate at once, so the leader keeps the default for that
  certificate and raises the alert `renew-before-ignored`. Both follow from the state
  alone, so no setting and no change of leader can make it renew in a loop.
- The tests run the whole order against a small ACME CA of their own
  (`controller/acme_test.go`), which validates by asking for the challenge.

## Alerts (`internal/notify`, `controller/wanted.go`, `controller/alerts.go`)

- **Channels** (`alerts.channels` in `cluster.yaml`, each with an optional
  `min_severity`): Discord (an embed colored by severity), webhook (a JSON POST of
  `notify.Message`) and email (port 465 with TLS from the start, any other port only with
  STARTTLS; `smtp.ca` is a file next to `cluster.yaml`). URLs and passwords are secrets;
  errors never name a URL. `http://` works, as the live test's receivers need.
- **In the state** only the open alerts (`State.Alerts`, `put-alert`, `resolve-alert`): a
  key of kind and subject (`pool-use/node0/vg/thin/data`), so one condition never has two
  open alerts; a condition that gets worse keeps its key and changes its severity.
  Subjects and messages hold no value that changes from report to report: each change is
  a Raft write.
- **In the leader's memory** (`controller.Alerts`): the deliveries. A message goes out when
  an alert is raised or gets worse, every 6 h while it is open, and when it is resolved,
  to each channel whose `min_severity` it reaches; the all-clear goes with the highest
  severity told, so a channel for critical alerts gets it after a downgrade too. A failed
  message is tried again after 1 minute, doubling up to an hour, and its channel gets
  nothing more in that pass, so a dead mail server cannot hold up the loop; the heartbeat
  goes first. Messages wait by the channel's place in `alerts.channels`, so a new
  `cluster.yaml` drops them. `clowder cluster status` shows the failed ones: a follower
  asks the leader (`GET /v1/internal/alerts/failures`).
  After a change of leader the new leader reminds of each open alert once: a duplicate
  rather than a lost alert. The heartbeat (`alerts.heartbeat`) is a GET every `interval`
  (default 1m), in a loop of its own, so a slow channel cannot make it late.
  `clowder alert test` sends a test alert to every channel at once, from the agent the CLI
  asks: the one exception to "only the leader calls external services", so a channel can be
  tried also while the cluster has no leader.
- **Conditions** that the state does not show are held in memory (`controller.Conditions`)
  with the time they began, by the loop that sees them: the Floating IP not held (the
  Floating IP's loop), a host over capacity (the placement), Docker not answering (the alerts,
  from `report-host`). After a change of leader they start again, so a new alert comes a
  little later rather than wrongly; one that is open stays until the loop that holds its
  condition has looked (`Conditions.Seen`). An agent that leads no more forgets them.

| Alert | Severity | Raised | Resolved |
|---|---|---|---|
| `pool-use` (data or metadata) | warning / critical | ≥ 80 % / ≥ 95 % | < 75 % / back to warning < 90 % |
| `root-use` | warning | ≥ 90 % | < 85 % |
| `docker-down` | critical | 2 min without an answer | answers |
| `ingress-not-held` | critical | 5 min | held again |
| `over-capacity` | warning | at once | fits again |
| `certificate-not-renewed` | warning | less than half the renewal window left | renewed |
| `renew-before-ignored` | warning | for any certificate | value changed |
| `stack-waiting` | warning | 5 min (`WaitingSince`) | placed |

A value is resolved below its raise threshold, so one that hovers there does not raise and
resolve at every report. Likewise an alert that gets better and then worse again, up to the
severity the channels were told of, sends nothing new: only its reminder and its
all-clear. The quorum watch (an agent without quorum alerts by itself) comes with phase 2:
one host has no quorum to lose.

## Metrics (`internal/metrics`, `agent/metrics.go`)

- Every agent serves `/metrics` on its node address, port 7444 (`MetricsPort`), plain HTTP
  without authentication: only the cluster's hosts reach the node address, through
  WireGuard; containers do not (the nft table). Nothing in `cluster.yaml` changes that.
- `internal/metrics` writes Prometheus' text format by hand, sorted, so it is tested
  against golden files (`agent/testdata/metrics-*.golden.txt`); the official client
  library would bring more into clowderd than the few dozen values need.
- Each agent exports what is about its own host: its phase and whether it leads, the Raft
  index, the pools' use, Docker, the root file system, the containers of its stacks by
  state, the supervisor's restarts per service and its own resident memory. What the
  cluster has once only the leader exports, so a Prometheus that scrapes every host
  counts it once: each host's room as the scheduler counts it (`scheduler.Rooms`), the
  certificates' expiry, the open alerts by severity and the failed deliveries.
- `promtool check metrics` and a scraping Prometheus come with the live tests.
