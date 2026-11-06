# Roadmap

clowder is built in phases, and each phase ends with live tests on HLab
([live-tests.md](live-tests.md)). This file is the one place that says how far the work is.

| Phase | Builds | State |
|---|---|---|
| 0 | the spike: DRBD quorum and fencing, WireGuard, the real client IP, on three hosts | done |
| 1a | the foundation on one host: stacks, cluster directory, host check, state, PKI, Raft, API, network, storage, containers | done, passes the live tests |
| 1b | ingress (Traefik, ACME), priorities, alerts, metrics, still on one host | done, passes the live tests |
| 1c | namespaces and resources: `clowder apply`, stacks with a schema of their own, volumes and secrets as resources, still on one host | done, passes the live tests |
| 1d | encrypted values (`!secret`), access and recovery, still on one host | done, passes the live tests |
| 2 | three hosts: replication, failover with fencing | done, passes the live tests |
| 2b | closer to Kubernetes: access, configuration and secrets, host roles ([below](#phase-2b-closer-to-kubernetes)) | in progress on `phase-2b`: steps 1 and 2 done, step 3 next |
| 3 | backup and restore, GitOps | |
| 4 | comfort and operations | |

## Phase 2b: closer to Kubernetes

Before phase 3, while the structure is still cheap to change, clowder moves closer to
Kubernetes and kubeadm in four areas: how users get in, how long their certificates last,
how the cluster's configuration and secrets come in, and which hosts hold the cluster's
keys. clowder goes beyond Kubernetes only where it would otherwise lose a protection or a
function: the CLI's cutoff, the question before a new Hetzner token, and a list of
endpoints.

### How it is built

The phase is one package, without intermediate states: no code exists only to bridge two
steps (such as a bootstrap that writes the new credentials, or a recovery key that lasts
until the sealing key comes). It is built on the branch `phase-2b` and goes to `main` only
once it is complete.

- Each step below gets its own session, in this order. After each step, lint and the unit
  tests are green.
- Each step gets the full review loop (junior and senior) over what it changed. Steps 1
  to 7 touch access, keys, the state or a format; step 8 touches much of the documentation.
- After step 5, the live tests run once in between. By then access, configuration,
  secrets, roles and the rules of who may call what are in place, and every host still
  holds the keys, so the cluster is complete and not a bridge. A fault found there lies in
  five steps, not eight. `test/live`, `bin/dev live` and the lab of roles change for this
  run, before step 6.
- At the end come the live tests over the whole phase, then a last review round over the
  whole, then the merge. The state and every format change, so a cluster of an earlier
  build must be set up anew.
- Each step updates the documentation of what it changes: DEVELOPING.md (Words: actor,
  endpoint, sealed, voter, active and passive, and the three things `cli` names: a
  certificate's role, the owner channel and the actor's prefix; Layers: `clusterdir` and
  `secretvalue` go with their code), `docs/developing/`, the README, the README of
  `examples/hetzner`, and in this file the Known gaps (each new limit below that says so)
  and Keys of a removed host (steps 3 and 6).
- Two keys keep their names apart. The **data key** encrypts the secrets in the state
  (DEVELOPING.md's "sealed" becomes "encrypted with the data key"). The **sealing key** is
  the cluster's age key that a `SealedSecret` is encrypted for.

### Step 1: access the way of kubeadm

Done on `phase-2b` (lint and unit tests green, the full review loop over it; the live
tests come after step 5). Beyond the plan, the step added `clowder config delete-context`,
and `config import --as <name>` (not `--context`, which picks the context a command uses)
with `--replace` for renewed credentials. A new cluster has no recovery key until the
first `clowder recovery rotate`, and `GET /v1/encryption` answers 409 until then.
`test/live` already follows, as lint builds it. Step 2 starts from here.

What changed, for the operator: there is no bootstrap key any more, and the first
credentials come over SSH, as kubeadm's `admin.conf` does (`ssh root@<host> clowderd
credentials`, then `clowder config import`). The credentials work as a kubeconfig: they
live in `~/.clowder/config`, hold contexts, and name the endpoints. Each CLI certificate
carries a name for the audit (`--name`, default `admin`; the actor is `cli/<name>`), which
grants nothing, and lasts one year. `cluster.yaml` has no `name` and no `endpoints`, and
`clowderd init` takes an optional `--cluster-name`. The details are in
[specs.md](developing/specs.md) (the CLI's credentials) and
[cluster.md](developing/cluster.md) (Credentials from a host).

### Step 2: the configuration as a resource

Done on `phase-2b` (lint and unit tests green, the full review loop over it; the live
tests come after step 5). As decided with the operator, the step went beyond the plan.
Secrets already have keys (`spec.data: {<key>: !secret}`), and each value is bound to the
Secret's ID. Whatever names a value names a Secret and a key (`{name, key}`, as
`secretKeyRef`): the configuration's `hetzner.tokenSecretRef`, `webhookSecretRef`,
`urlSecretRef` and `smtp.passwordSecretRef`, the heartbeat's `urlSecretRef`, and a
stack's `secretEnv: {VAR: {name, key}}`; `secrets: [db]` puts each key at
`/run/secrets/db/<key>`. A missing image pull secret is left out, as the kubelet does,
and a pull that fails names it. `clusterspec.SecretPlaces` stays, as the list of the
fields that name a Secret. `cluster schema` went, and `clowder diff -f` exits as
`kubectl diff` does (0, 1, 2). Step 3 starts from here: the values come in through
`Secret` (`stringData`, `data` in clear, `clowder create secret`) and `SealedSecret`
(today's `!secret` values in `encryptedData`), with the keys as they are now.

What changed, for the operator: `cluster.yaml` is now a resource, `kind:
ClusterConfiguration` named `cluster`, with camelCase fields, applied with `clowder apply`
like any other resource; `cluster apply`, `cluster diff` and the cluster directory went.
`apply` works in the order of the files, as kubectl does. The configuration has no
`registries` (registry logins belong to the namespaces), its CAs stand in it as PEM, and
its secrets are Secrets in the reserved namespace `clowder-system`. Before it applies a new
Hetzner token, the CLI says how many of the hosts' servers the token sees and asks. Once
applied, the configuration can be changed but not deleted. The details are in
[specs.md](developing/specs.md) (the cluster's configuration) and
[cluster.md](developing/cluster.md) (Resources).

### Step 3: Secret and SealedSecret

- **Two ways in,** as in Kubernetes with Bitnami's sealed secrets.
  - A `Secret` carries its value in clear (`stringData`, `data`), or comes from
    `clowder create secret <namespace>/<name> --from-literal|--from-file`. The CLI sends it
    over mTLS, and the cluster encrypts it with the data key. A `Secret` does not belong in
    Git.
  - A `SealedSecret` carries its value encrypted for the sealing key and for its own
    namespace and name (today's `!secret` value). `clowder secret seal` makes one, as
    `kubeseal` does: with the public half of the sealing key from the cluster
    (`GET /v1/encryption`), or offline from a file (`--cert`, as in kubeseal) that
    `clowderd sealing-key export --public` writes. A `SealedSecret` goes into Git.
  - Both end up as the same secret in the state. A stack, a namespace or the
    configuration names a Secret and does not care how it came in. `!secret` goes.
- **The same value applied again changes nothing,** as now: the state compares the value's
  digest, not the ciphertext, which differs at each seal. `get` and `diff` show a secret's
  digest, never its value.
- **No recovery key.** As with sealed secrets, the sealing key is exported and kept
  offline: `clowderd sealing-key export`, run as root on a host (a control-plane host from
  step 4 on) right after `init`. `secret decrypt --identity` takes the sealing key in
  place of the recovery key. `secret rekey --identity` seals the values in files anew for
  the cluster of the current context (or for `--cert`) when the old cluster is lost; with
  one recipient, that is all that is left of rekey. The recovery key, `recovery rotate`,
  `/v1/recovery/rotate` and the second recipient of every value go. Phase 3 decides
  whether backups also keep the sealing key.

For the operator: `!secret` values become `SealedSecret`s (`clowder secret seal`), or
`Secret`s that stay out of Git. The sealing key takes the place of the recovery key and is
exported right after `init`.

### Step 4: host roles

- **Three roles,** set with `--roles` at `clowderd init` (default
  `control-plane,worker,ingress`) and at `clowderd join` (default `worker,ingress`). A
  join token names the roles it allows (`clowder host join-token --roles …`, default
  `worker,ingress`), and the join picks from them. So whoever reads a worker's token from
  its user data cannot join as control plane. kubeadm needs a second secret for that
  (`--certificate-key`); clowder's token opens only once, so the roles in it are enough.
  A host's roles are fixed once it has joined. To change them, the host is removed and
  joins anew (as with `kubeadm reset`), a known gap. `examples/hetzner` takes each host's
  roles, all three by default, so a cluster of three hosts works as today.
  - `control-plane`: may vote in Raft, leads, serves the CLI's API, forwards the CLI's
    commands, and runs `clowderd credentials`, `clowderd sealing-key export` and
    `clowderd recover`.
  - `worker`: runs stacks and holds volume copies.
  - `ingress`: may take the Floating IP. Every host runs Traefik for its own stacks, and
    the ingress host's Traefik also takes what comes in on the Floating IP. TLS ends
    there, and the request goes on as HTTP through WireGuard to the Traefik of the stack's
    host, which needs no TLS key (loops.md, Traefik).
- **Voters are control-plane hosts:** 1, 3 or 5 of them by today's rule (`voterCount` in
  `controller/members.go`), counted over the control-plane hosts. `state.MinHAHosts`,
  the floor of three in `host remove` and the alert `no-ha` count the control-plane hosts.
  The passive phase is only about the vote: the second control-plane host waits for its
  vote until a third one comes, as two voters would stop Raft when either fails. If it is
  a worker, it runs stacks meanwhile. This is safe, because with one voter that voter
  alone decides about the workers.
- **One control-plane host gives high availability to the workers only,** by the
  operator's choice. A failed worker is fenced and its stacks move while the control-plane
  host lives. While the control-plane host is down, nothing is decided (no failover, no
  apply), and the stacks run on. A worker that restarts meanwhile starts none of its
  stacks, as a host waits for the leader's commit index before it starts any (and from
  step 6 on, their secrets come from the control plane and are kept on no disk): a known
  gap. `no-ha` stays quiet for a cluster that never had three control-plane hosts; it is
  about a cluster that lost them.
- Hosts without the control plane follow the Raft log as non-voters, so the agent's loops
  keep reading the state as now. They see every spec, which Kubernetes does not show a
  node: a known gap.
- The placement puts stacks and copies on workers only. A tiebreaker holds no data and no
  load, so it may go on any host. A control-plane host without the worker role needs DRBD
  for it, but no thin pool. Such a host runs no stacks, so its failure is never fenced, as
  a host without stacks is not fenced now; it keeps its vote.
- A host certificate names its roles, and `host list` shows them.
- The step touches `pki` (roles in the certificate), `jointoken` and the join (roles in
  the token), `state` (a host's roles, `MinHAHosts`, the passive phase), `controller`
  (members, placement, Floating IP), `scheduler`, `agent` (init, join), the flags of
  `clowderd`, and `examples/hetzner`.
- In steps 4 and 5 every host still holds the data key; step 6 removes it from the other
  hosts.

### Step 5: who may call what

Every way into a host checks the caller's roles, so an attacker who takes over a worker
gets no further than its own stacks. Today any host certificate passes everywhere.

- **The routes:** the server checks the caller's certificate, with a test per route
  (`api/server.go`). The CLI's routes answer on control-plane hosts only, and
  `stack ps|logs` go from there to the stack's host, as now.

  | Route | Caller |
  |---|---|
  | the CLI's (`/v1/…` except `internal`) | a CLI certificate, on a control-plane host |
  | `GET` and `POST /v1/join` | anyone with an open join token, as now (limited per sender) |
  | `internal/propose` | any host for its own reports (`host/<id>`); a control-plane host for the CLI's commands and `set-cli-cutoff` |
  | `internal/changes`, `internal/hand-over`, `internal/join` | a control-plane host |
  | `internal/credentials` | the agent's own host certificate only |
  | `internal/stacks/…/ps\|logs`, `internal/volumes/progress`, `internal/volumes/…/resolve-splitbrain`, `internal/alerts/failures` | a control-plane host |
  | `internal/beats` (`POST`, and `GET …/{id}`: `AskSilence`), `internal/commit-index`, `internal/leader` | any host |
  | `internal/recover/copies`, `internal/recover/rejoin` (step 7) | a control-plane host |
  | `internal/hosts/self/secrets`, `internal/hosts/self/tls` (step 6) | any host, for itself |
- **The host's commands split by role:** `hostKinds` (`api/server.go`) keeps a host's
  reports about itself. `set-cli-cutoff` and every kind that acts for the cluster come
  only from a control-plane host.
- **Raft takes only the control plane:** every host's Raft listener (`raftnode`,
  `pki.ServerTLSForHosts`) accepts a control-plane certificate only, as only the leader
  sends to a non-voter. Otherwise a worker could send entries of its own past every check
  above. A test has a worker try.
- **The CLI takes only control-plane hosts:** it refuses a host certificate without
  `control-plane`, so a removed worker cannot answer it as the cluster.

### Step 6: keys only on the control plane

- Only control-plane hosts hold the data key, and with it the CA's key and the sealing
  key. A join's answer carries the data key only for them.
- Other hosts ask a control-plane host over mTLS for what they need, and get only that, as
  Kubernetes' node authorizer gives a node only the secrets of its pods
  (`internal/hosts/self/secrets` and `…/tls`, step 5):
  - A worker gets the secrets and registry logins of the stacks the state places on it.
    Images that need a login are no longer pulled before a stack moves there, only when it
    starts, so its failover waits for the pull: a known gap. Images without a login are
    pulled before, as now.
  - An ingress host gets the TLS keys of the routes while it holds the Floating IP. So the
    move of the Floating IP needs a control-plane host, which the leader that moves it is
    anyway.
  - Every host gets the key of the secrets' digests, which today is derived from the data
    key (`agent/run.go`). It becomes a key of its own: made at `init`, kept in the state
    encrypted with the data key, and handed to every host in its join's answer. It shows
    when a secret changed and opens none.
- `QuorumWatch` (`agent/run.go`) runs on control-plane hosts only, as its alert channels
  are secrets. A worker cut off from all of them alerts nobody; the voters see it fail (a
  known gap).
- `checkRetired` (`agent/retired.go`) runs on control-plane hosts only, which hold the
  token (step 7 says why a retired worker needs none).
- A worker that restarts while the control plane is down starts none of its stacks: on top
  of the catch-up (step 4), their secrets are kept on no disk, by choice.

### Step 7: recover and fencing with roles

Agreed with the operator before the plan.

- **Fencing and failover stay as they are.** Every host beats to the voters. A host that a
  majority of them has not heard for 45 s is failed (with one control-plane host, that one
  decides). A failed host with stacks, a worker, is fenced. A control-plane host without
  the worker role has no stacks, so it is never fenced and keeps its vote. The leader
  fences and moves the Floating IP with the token, both on the control plane; the Floating
  IP goes to an `ingress` host only.
- **A power-off no longer holds up the fencing loop.** Today `tryPowerOff` tries for up to
  about 2.5 minutes within the loop's own pass, and meanwhile the loop reads no beats: a
  second host that fails then is found late, not only fenced late. The power-off of each
  host will run in a job of its own, as the volume reconciler's jobs do. The fence
  operation in the state records how far it got, so a new leader picks it up. The loop
  goes on reading beats and failing hosts, and starts no second job for a host that has
  one. A unit test fails a second host while the first host's power-off hangs.
- **`clowderd recover --power-off`** runs, as now, on a control-plane host that is left
  when the majority of the voters is gone for good, and asks for the host's name.
  - A host that takes no connection is retired, whatever its roles: labeled and powered
    off as now. A voter that answers stops the recover, as a host that knows a leader does
    now. A worker that answers stays in the cluster: it never voted, so it cannot form an
    old majority.
  - The remap keeps the copies that are left. Before it changes anything, recover asks
    each worker that stayed for its copies (`internal/recover/copies`: DRBD's disk state,
    the data generation), and a synced copy there counts as a synced copy on this host does
    now. Only copies on retired hosts go, and stacks there are placed anew, as after a
    failover. When only control-plane hosts were lost, the workers' stacks and volumes run
    on, and recover only brings Raft back.
  - Raft goes on with this host as its only voter. A worker that stayed may hold a higher
    term, or entries that the old majority committed without this host; its state applied
    them already, and the new leader would never send them. So recover tells each worker
    (`internal/recover/rejoin`, with the retired hosts) to do three things: drop the
    retired hosts from its DRBD resources (`DropPeers`: `del-peer`, quorum off,
    `resume-io`, as the recovering host does), set its Raft directory aside, and follow the
    new leader from its snapshot as a fresh non-voter. A non-voter's log is only a copy,
    so nothing is lost that the remap keeps. A unit test runs a worker that is ahead of
    the recovering host.
- **`checkRetired` only on the control plane** (step 6 moves it). A retired voter is the
  danger, as two of them could elect each other. Voters hold the token and check at their
  start, as now. A retired worker is harmless: it never votes, its certificate is revoked
  so no control-plane host accepts it, it is out of the WireGuard mesh and the DRBD
  resources, and it starts no stack before it has caught up with a leader it never
  reaches.
- **Every control-plane host gone:** the cluster is lost, as a Kubernetes cluster without
  its etcd is. The workers hold the log but no key. The way back is a new cluster, the
  `SealedSecret`s sealed anew with the exported sealing key, and the data from phase 3's
  backups. This is a known gap; three control-plane hosts are the advice.

### Step 8: clean up

The last step, before the final live tests, so that they run on what is merged.

- **Old and dead code goes:** what phase 2b left without a caller (helpers of the
  bootstrap, of the cluster directory, of `!secret` and of the recovery key, and constants
  and test fixtures that nothing uses), and what was dead before. `deadcode` and
  golangci-lint's `unused` find the candidates. Each one is checked by hand, as a test may
  be its only caller.
- **General tidying, without new behavior:** names and comments that still speak of users,
  the bootstrap, `cluster.yaml` or the recovery key, and the open points of the reviews
  that are not behavior (the wiring of the loops in `agent/run.go`, comments that
  contradict each other, long table cells).
- **The plan for phase 3 and later:** phases 3 and 4 and the Known gaps are checked
  against phase 2b (`ClusterConfiguration`, Secrets, roles, sealing key) and rewritten
  where they no longer fit: backup and restore with the sealing key, GitOps that cannot
  answer the question before a new token, and `cluster rotate-keys` for the control plane
  only.
- Review: the full loop (junior and senior), even though no behavior is new. The
  documentation changes much, and a junior must understand it.

### The live tests (after step 5, and at the end)

- `test/live` and `bin/dev live` follow the whole phase: credentials fetched over SSH into
  `~/.clowder/config` (through `$CLOWDER_CONFIG`), the `ClusterConfiguration` and the
  cluster's secrets applied with `clowder apply`, `SealedSecret`s in place of `!secret`,
  and no bootstrap key. The labs of three hosts with all roles keep today's scenarios.
- A new lab of five hosts for the roles: three with `control-plane` only, and two with
  `worker,ingress`. `bin/dev live up` takes a lab's layout, and `examples/hetzner` takes
  each host's roles.
- New scenarios for what is new:
  - a CLI certificate with a name, and its actor in the log
  - the warning before a certificate ends
  - the question before a token that sees no server, and the alert `hetzner-token`
  - a `Secret` in clear and a `SealedSecret`, both used in a stack
  - a registry login of a namespace
  - the sealing key exported and used by `secret decrypt`
  - in the lab of roles: a volume with its copies on the workers and its tiebreaker on
    the control plane; a worker that gets only the secrets of its stacks; a worker's token
    refused for the control plane; a worker refused on the control plane's routes and on
    Raft; the CLI refusing a worker; a worker's failover to the other worker; and a
    recover after the loss of two control-plane hosts in which the workers' stacks and
    copies stay.
- The run after step 5 has every scenario except those of steps 6 and 7 (a worker that
  gets only the secrets of its stacks, the recover with workers).
- All live tests must be green; a failure goes back to the step it came from.

### After phase 2b

- A removed worker holds no keys, and the CLI does not accept it, so revoking its
  certificate is enough. `cluster rotate-keys` (phase 4) is needed only after a
  control-plane host is removed, which makes [Keys of a removed
  host](#keys-of-a-removed-host) smaller.

## Known gaps

What phase 2 left open, sorted by the phase that closes it.

Phase 3 (backup and restore):

- After a recover, a volume that waits for a restore waits until phase 3 brings restores.
  Until then, deleting it and applying it anew gives an empty volume. An unsynced copy
  left on the recovering host stays as it is, for a human to handle.

Phase 4 (comfort and operations):

- A removed host keeps the cluster's keys until `cluster rotate-keys` exists (see Keys of
  a removed host, below). Until then, wipe its disk.
- A host that comes back after a fence resyncs all its copies at once, each at the rate of
  the resync limit; meanwhile the limit holds back only new copies.
- A failed host that is not fenced keeps its copies counted, and no copy is made anew
  elsewhere until a user runs `host fence-confirm`. The alert `fencing-blocked` says so.
  This happens when the host needs no fence, no ClusterConfiguration is applied yet, a
  server is not confirmed, or Hetzner's API is down for good.
- A drained host that fails comes back active, not drained: its drain is lost. A user's
  remove is kept.
- A disabled voter keeps its vote until it is removed: with more than three hosts, an
  active host that does not vote does not take its place.
- The CLI learns no hosts from the cluster. `clowderd credentials` writes the hosts of the
  time into the context's `endpoints`. After joins and removes, the user fetches new
  credentials or edits the endpoints; otherwise the CLI cannot reach the cluster while the
  endpoints it lists are down.
- If a stack with a published port moved while the agent of the ingress host was down,
  the port keeps its connections to the old host until they end: the agent forgets
  connections only when it sees a target change.
- `replicas` cannot change yet. Raising it (from 1 to 2 or 3) only needs the placement to
  allow it, as copies already come online.
- Images are never pruned (no `image prune`), and every host pulls the images of the
  stacks it may take over, so the root file system fills up over time.
- A read sees a write of the same `clowder` call (through the index header), but not one
  of the call before. So `clowder get` right after `clowder apply` may show, for a moment,
  the state of a follower that has not applied the apply yet. The CLI could keep the last
  index it wrote in its config directory and ask for it.
- A cluster that had three hosts never goes below three, even when a host is dead: a
  replacement joins first. If none can join (`examples/hetzner` joins through its first
  host), the cluster stays at two hosts. `clowderd recover --power-off` is the way out
  only when two hosts are gone.
- A retired host whose agent starts while Hetzner's API does not answer starts as before,
  and two such hosts would have the old cluster's majority. recover tells the user to set
  retired servers up anew or delete them.
- recover knows the hosts that this host's log knows. A host that the others added after
  this one fell behind is neither asked nor retired: power its server off by hand.
- A placement is several commands in a row (push-outs, volumes, the stack's host). A new
  leader repairs in its next pass what an old one left half done; one command per
  placement would leave fewer states in between.
- `examples/hetzner` ignores later changes to the server's user data, so a storage class
  added later gets a volume but no thin pool on an existing host.
- The question before a new Hetzner token lists the project's servers in one request, so a
  project with more than 50 servers is not checked: the answer says it could not check.

When one is needed:

- A Prometheus in a stack or outside the cluster cannot reach `/metrics` yet, as only the
  hosts reach the node address. A way in (through Traefik, or a permission for certain
  stacks) comes when one is needed.

Not live-tested, as the lab cannot produce it:

- `volume resolve-splitbrain` never ran on real DRBD, only in unit tests: no fault of the
  lab makes a split brain while the quorum holds. A copy promoted apart from the newer data
  has no quorum, and its writes hang.
- recover on a host that rebooted after the others went: its DRBD resources come up from
  the agent's files, with this host alone.

## Keys of a removed host

A cluster has no users: whoever holds the CLI's credentials controls it, as with kubeadm's
`admin.conf`. The open point is what a removed host keeps.

- A removed host keeps the data key, and with it the CA's key and every `!secret` value.
  A host that kept its disk can still issue certificates that the revocations do not name:
  host certificates, and CLI certificates after the cutoff. Rotating the data key, the CA
  and the cluster's age key (`cluster rotate-keys`, planned for phase 4) closes this. Until
  then, wipe a removed host's disk.
- The CLI checks an agent's certificate against the CA, not against the revocations, as
  kubectl does. So a removed host could answer the CLI as the cluster, with recipients of
  its own for `secret encrypt`. The agents refuse such a host, but only the endpoints of
  the CLI's context keep the CLI from asking it, so remove a host's address from them when
  the host goes. With the CA's key on such a host, no list the CLI pinned would help;
  `cluster rotate-keys` closes this too.
- Whoever holds the CLI's credentials can check a guessed value against a secret's digest.
  They can read the value through a privileged stack anyway.

## Decisions that still hold

The history of the finished phases is in the Git log. These decisions from it still apply:

- Until `v1.0.0`, the state and every format (the configuration, the credentials, the
  join request, the flags of `clowderd init` and `join`) may change between releases
  without a way to upgrade a running cluster. Such a release needs a cluster set up anew,
  and credentials of an earlier build are refused.
- Before `v1.0.0`, only agents and CLIs of the same minor version work together.
  [Updating a cluster](developing/shipping.md#updating-a-cluster) says in which order to
  update.
- clowder runs on Hetzner Cloud with a Hetzner private network between the hosts, and
  nothing else: `internal/hetzner` is called directly, without a layer for other clouds.
- A cluster has no users. The CLI's certificate controls it, as kubeadm's `admin.conf`
  does, and more credentials come from root on a host (`clowderd credentials`).
- `hetzner.api` and `acme.directory` exist for test labs. When they are set, they may be
  `http://`, and the apply and the leader's log warn about them.
- A host's name in the cluster is its hostname, as DRBD knows it by that name.
- Code without a caller was removed; each phase brings back what it needs. Phase 3 brings
  back `backup:` in the ClusterConfiguration and resources owned by a GitOps source.
- `v0.1.0` is the first release. The APT repository keeps every version, and
  `bin/dev live release <tag>` tests a published release (the [release
  checklist](developing/shipping.md#release-checklist)).

## What the spike of phase 0 showed

The spike ran on three HLab hosts, before any clowder code existed. The design rests on
these results.

- Published ports reach a container on another host through WireGuard, with DNAT and no
  SNAT: the container sees the client's own address, over UDP and TCP. After the Floating
  IP moves, this works through the new ingress host; a running UDP flow follows a moved
  target only after `conntrack -D`.
- DRBD 9 with quorum majority and `suspend-io`: a host cut off from its peers suspends its
  writes. After it is powered off through the API, a peer promotes with a diskless
  tiebreaker, and the data is intact. The fenced host boots with no resource up and
  resyncs as secondary. A host whose peers are gone for good goes on alone after
  `del-peer`, `forget-peer` and `quorum=off`.
- After a reboot, Docker starts neither a `restart: "no"` container nor, in this setup, an
  `unless-stopped` one. A container whose volume is not mounted cannot start (the mount
  guard).
- A host whose systemd stops feeding the watchdog reboots after about 140 s. In HLab's VMs
  systemd fed the `iTCO_wdt` device, not softdog.
