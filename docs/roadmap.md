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
| 2b | closer to Kubernetes: access, configuration and secrets, host roles ([below](#phase-2b-closer-to-kubernetes)) | in progress on `phase-2b`: steps 1 to 5b done, step 5c next |
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
- After step 5c, the live tests run once in between. By then access, configuration,
  secrets, roles, the rules of who may call what and the cheaper copies of the state are
  in place, and every host still holds the keys, so the cluster is complete and not a
  bridge. A fault found there lies in the first steps, not in all of them. `test/live`,
  `bin/dev live` and the lab of roles change for this run, before step 6.
- At the end come the live tests over the whole phase, then a last review round over the
  whole, then the merge. The state and every format change, so a cluster of an earlier
  build must be set up anew.
- Each step updates the documentation of what it changes: DEVELOPING.md (Words: actor,
  endpoint, sealed, voter, role, active and in service, and the three things `cli`
  names: a certificate's role, the owner channel and the actor's prefix; Layers: the
  entries of `secretvalue` and `hostrole` follow their code), `docs/developing/`, the README, the README of
  `examples/hetzner`, and in this file the Known gaps (each new limit below that says so)
  and Keys of a removed host (steps 3 and 6).
- Two keys keep their names apart. The **data key** encrypts the secrets in the state
  (DEVELOPING.md's "sealed" becomes "encrypted with the data key"). The **sealing key** is
  the cluster's age key that a `SealedSecret` is encrypted for.

### Step 1: access the way of kubeadm

Done on `phase-2b` (lint and unit tests green, the full review loop over it; the live
tests come after step 5c). Beyond the plan, the step added `clowder config delete-context`,
and `config import --as <name>` (not `--context`, which picks the context a command uses)
with `--replace` for renewed credentials. `test/live` already follows, as lint builds it.

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
tests come after step 5c). As decided with the operator, the step went beyond the plan.
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

Done on `phase-2b` (lint and unit tests green, the full review loop over it; the live
tests come after step 5c). A value comes in one of two ways, as in Kubernetes with
Bitnami's sealed secrets, and both end up as the same Secret in the state:

- A `Secret` holds its values in clear (`spec.data` in Base64, `spec.stringData` as
  text). The CLI sends it over mTLS, and the cluster encrypts it with the data key. It
  does not belong in Git. `clowder create secret` sends one without a file and, like
  `kubectl create`, refuses one that exists.
- A `SealedSecret` holds each value in `spec.encryptedData`, sealed for the sealing key and
  bound to its namespace and name, as one line of Base64 as kubeseal writes it. It goes into
  Git. `clowder secret seal` makes one from the Secrets of a file or from
  `--from-literal`/`--from-file`, with the public sealing key from the cluster or offline
  from `--cert`. The apply stores it as the Secret of the same name; `get`, `delete -f` and
  `diff -f` work on that Secret.

Deviations from Kubernetes, each for a reason: `data` and `stringData` stand under `spec`,
like every clowder resource's fields; a SealedSecret is no resource of its own in the
cluster and has no `template`, because it is only a way in; the values are sealed with age,
which the cluster already uses, instead of RSA.

As decided with the operator, the same value applied again changes nothing (the state
compares digests), and there is no recovery key: `clowderd sealing-key export [--public]`
prints the sealing key for keeping offline. `secret decrypt --identity` and
`secret rekey --identity` take it; rekey always opens the values itself and seals them for
the cluster of the context or for `--cert`. Phase 3 decides whether backups also keep the
sealing key. Step 4 starts from here.

What changed, for the operator: `!secret` values and `secret encrypt` are gone; files hold
`SealedSecret`s (`clowder secret seal`), or `Secret`s that stay out of Git.
`recovery rotate` is gone: export the sealing key right after `init`
(`ssh root@<host> clowderd sealing-key export`) and keep it offline. The state and the
API change, so a cluster of an earlier build must be set up anew. The details are in
[specs.md](developing/specs.md) (Secrets and sealed values) and
[cluster.md](developing/cluster.md) (Secret values).

### Step 4: host roles

Done on `phase-2b` (lint and unit tests green, the full review loop over it; the live
tests come after step 5c). A host has roles, as a Kubernetes node does, in the new package
`hostrole`:

- `control-plane`: may vote in Raft and lead, and runs `clowderd credentials`,
  `clowderd sealing-key export` and `clowderd recover`. That only control-plane hosts
  serve the CLI's API and forward its commands came with step 5.
- `worker`: runs stacks and holds volume copies with a disk.
- `ingress`: may take the Floating IP. Every host still runs Traefik for its own stacks.

The roles come from `--roles` at `clowderd init` (default all three; it needs
`control-plane`) and at `clowderd join` (default `worker,ingress`). A join token names the
roles it allows (`clowder host join-token --roles`, default `worker,ingress`), and the join
picks from them; there is no separate control-plane token. The host's certificate names
its roles (as its organizations), `host list` shows them, and they are fixed once the host
has joined. Voters are control-plane hosts (1, 3 or 5 by `voterCount`); `MinHAHosts`, the
floor of three in `host remove` and `no-ha` count the control-plane hosts in service. The
placement puts stacks and copies with a disk on workers only; a tiebreaker may go on any
host, so a control-plane host without the worker role needs DRBD but no thin pool. Such a
host is never fenced, as it has no stacks. Hosts without the control plane follow the
Raft log as non-voters.

As decided with the operator, the step went beyond the plan in two points:

- The phase `passive` is gone. It was only about the vote, and `voterCount` over the
  control-plane hosts already keeps the second one without a vote; a joining host becomes
  active once it reports. With every host's roles at their defaults, the second host now
  runs stacks without a vote, where it waited before.
- A volume gets its second copy with a disk only once a third host can take its tiebreaker
  (`minTiebreakerHosts`, counted over all hosts in service). Before, the passive phase kept
  a second host from taking copies; without it, two workers would hold two copies without
  a third vote, and DRBD would stop writing when either failed.

Deviations from Kubernetes, each for a reason: the roles stand in the host's certificate
rather than in labels, so that step 5 can check a host's calls from its certificate alone;
the join token names the roles instead of a second secret, as it opens only once.

`test/live` makes its tokens with all three roles, so the lab still has three hosts that
do everything; the lab of roles comes before the interim live tests. In steps 4 and 5
every host still holds the data key; step 6 removes it from the other hosts. Step 5
starts from here.

What changed, for the operator: `clowder host join-token` allows `worker,ingress` unless
`--roles` says more, so a token for another control-plane host needs `--roles
control-plane,worker,ingress`. The second host of a cluster runs stacks without a vote,
and a volume of replicas 2 gets its second copy once three hosts are there.
`examples/hetzner` takes a `roles` map (all three by default) and gives volumes only to
workers. The state, the certificates and the join change, so a cluster of an earlier build
must be set up anew. The details are in [cluster.md](developing/cluster.md) (Cluster state,
Certificates, Raft, Joining hosts, Placement).

### Step 5: who may call what

Done on `phase-2b` (lint and unit tests green, the full review loop over it; the interim
live tests come after step 5c). Every way into a host checks the caller's roles, so an attacker who
takes over a worker's certificate gets no further than its own stacks. The plan below
stands as built; [cluster.md](developing/cluster.md) (Who may call what, A host without
the control plane, Raft, Certificates) has the details. As decided with the operator, the
step went beyond the plan in these points:

- **A worker answers only the control plane.** Its API takes only control-plane hosts
  already in the TLS handshake, so it serves neither the CLI nor the join, and it listens
  only on the private network (the underlay and the node address), not on its public
  address. Only the control plane calls a worker; a worker calls the voters (beats) and
  the leader (its reports, the commit index), so it forwards nothing for others.
  `examples/hetzner` opens the API's port only on control-plane hosts (a second Cloud
  firewall).
- **The join goes through the control plane,** as kubeadm's does: a worker refuses it,
  and the joining host takes only a control-plane host.
- **`internal/sealing-key`**, which the table below missed, takes only the host's own
  certificate, as `internal/credentials` does; on a worker, the handshake refuses both.
- **`clowderd credentials` lists only control-plane hosts** as the CLI's endpoints.
- `set-cli-cutoff` is checked in the API's route table only, not again in the state: the
  table is the one place that says who may send what, and Raft takes entries only from
  the control plane.

What changed, for the operator: the CLI and `clowderd join --via` reach a cluster only
through control-plane hosts, and a worker's API answers neither on its public address
nor without a control-plane certificate. Credentials of an earlier build that list
workers among their endpoints skip them; fetch new ones. An agent whose `host.json`, its
certificate and its state on disk name different roles does not start. The plan:

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
- **One set of roles per host:** a host's roles stand in the state, in its certificate and
  in its `host.json` (step 4). The checks above read the certificate, so the agent checks
  at its start that the three agree, and refuses to run otherwise.

Until step 6, every host still holds the data key and with it the CA's key, so root on a
worker can still issue itself any certificate; the rules above hold against a worker's
certificate, and against root on a worker only from step 6 on. Step 5b starts from here.
In the interim live tests, the lab of roles has workers whose public API port is closed,
so `test/live` reaches the cluster through control-plane hosts only.

### Step 5b: copy the state only where it changes

Done on `phase-2b` (lint and unit tests green, the full review loop over it; the interim
live tests come after step 5c). Before it, the state was copied whole through a JSON
round trip (`State.Copy`) for every `change-resources` and, after every applied entry,
for the loops' shared copy (`raftnode` `fsm.sharedCopy`), even for an entry that changed
one host's report. With `resource.MaxClusterSecretBytes` in secrets, each such copy
allocated about 33 MiB.

The choice, made with the operator: **a typed clone that shares what never changes in
place** (`State.Clone`, `state/clone.go`). It replaces `State.Copy` everywhere: in the batch,
in the shared copy, in the dry run and in `clowderd recover`.

- It copies the maps, the resources and their statuses, and shares the specs, a stack's
  `Services`, the encrypted values and the certificates' bytes, which the handlers only
  ever replace whole. It leaves the audit out.
- The guarantees stay: a batch still works on a clone and puts it in place only if every
  change passes, and a loop still reads a clone that no command changes.
- The tests guard the rule: every command in the state's tests is checked against a
  clone taken before it, `clone_test.go` fails when `Clone` shares a field it should copy,
  and `statetest.ReadOnlyCopy` catches a loop that writes into a shared spec.
- The cost (`TestAReportAndAnApplyCostLessThanTheSecrets`): with 4 MiB of secrets, a report
  with the clone after it allocates about 18 KiB, and an apply of one resource about
  31 KiB.
- `SharedCopy` no longer returns an error, because a clone cannot fail.

The other ways weighed: copy-on-write in every handler (cheaper still, but every handler
changes and a forgotten clone is a silent bug), and an undo of a failed batch (it does not
help the shared copy, and an undo of `putStack` is easy to get wrong). Step 5c starts from
here.

### Step 5c: smaller documentation

The documentation part of step 8, brought forward: every session reads the docs, and
their size costs each one. Without new behavior.

- `docs/roadmap.md` keeps the state and the plan; the finished steps of phase 2b shrink
  to what still holds (the decisions and the Known gaps), and the Git log keeps the
  history.
- `docs/developing/cluster.md` is cut down and split where a part stands on its own (for
  example the API and the CLI, Recover), so that a reader opens only the part about the
  package they change.
- DEVELOPING.md's Words name each word in one place, not in the table and again below
  it.
- Comments that have grown into essays (such as `MaxRequestBytes` in `api/server.go`)
  keep the reason and lose the rest.
- Sentences that are simple but hard to read are rewritten in plain technical English
  (normal word order, one point per sentence).
- The README states no progress (the roadmap does): its line on what comes next and its
  Getting started match the roadmap.

Review: the full loop (junior and senior); the documentation changes much, and a junior
must understand it. Step 8 then only checks the documentation against the code again.

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
  that are not behavior (comments that contradict each other, long table cells).
- **The agent's loops start one way:** today `agent/run.go` (`startLoops`,
  `runOnceCaughtUp`) starts them as functions with arguments, with a parts struct, as
  methods, through a constructor and as a value's `run`. Every loop gets the same form.
- **The documentation still fits:** step 5c made it smaller; this step checks it against
  the code of steps 6 to 8 and shrinks steps 5b to 8 in this file as 5c did the others.
- **A new release, not an update:** phase 2b's release fits no earlier cluster, so every
  command kind is new with it. The command kinds' release gate (`since`,
  `State.CheckKnown`) starts anew from that release or goes, and the README's Getting
  started and `examples/hetzner`'s `clowderd_version` point to the new release.
- **The plan for phase 3 and later:** phases 3 and 4 and the Known gaps are checked
  against phase 2b (`ClusterConfiguration`, Secrets, roles, sealing key) and rewritten
  where they no longer fit: backup and restore with the sealing key, GitOps that cannot
  answer the question before a new token, and `cluster rotate-keys` for the control plane
  only.
- Review: the full loop (junior and senior), even though no behavior is new. The
  documentation changes much, and a junior must understand it.

### The live tests (after step 5c, and at the end)

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
- The run after step 5c has every scenario except those of steps 6 and 7 (a worker that
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

- A host's roles are fixed once it has joined. To change them, remove the host and let it
  join anew, as with `kubeadm reset`.
- With one control-plane host, a worker that restarts while the control-plane host is down
  starts none of its stacks: a host waits for the leader's commit index before it starts
  any (and from step 6 on, their secrets come from the control plane and are kept on no
  disk). The stacks that run keep running.
- Hosts without the control plane follow the Raft log, so they see every spec, which
  Kubernetes does not show a node.
- In a cluster with two workers and no third host, a volume of replicas 1 that moves has
  two copies without a tiebreaker for the length of the move; a failure of either host
  then stops its writes until the host is back.

- A removed host keeps the cluster's keys until `cluster rotate-keys` exists (see Keys of
  a removed host, below). Until then, wipe its disk.
- Until step 6 of phase 2b, every host holds the data key and the CA's key. The route
  table keeps a worker's certificate to its own work, but root on a worker can issue
  itself any certificate.
- A host that comes back after a fence resyncs all its copies at once, each at the rate of
  the resync limit; meanwhile the limit holds back only new copies.
- A failed host that is not fenced keeps its copies counted, and no copy is made anew
  elsewhere until a user runs `host fence-confirm`. The alert `fencing-blocked` says so.
  This happens when the host needs no fence, no ClusterConfiguration is applied yet, a
  server is not confirmed, or Hetzner's API is down for good.
- A drained host that fails comes back active, not drained: its drain is lost. A user's
  remove is kept.
- A disabled voter keeps its vote until it is removed: with more than three control-plane
  hosts, an active one that does not vote does not take its place.
- The CLI learns no hosts from the cluster. `clowderd credentials` writes the
  control-plane hosts of the time into the context's `endpoints`. After joins and removes, the user fetches new
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
- A cluster that had three control-plane hosts never goes below three, even when one is
  dead: a replacement joins first. If none can join (`examples/hetzner` joins through its
  first host), the cluster stays at two control-plane hosts. `clowderd recover --power-off` is the way out
  only when two hosts are gone.
- The check for a retired host fails open: a retired host whose agent starts while
  Hetzner's API does not answer starts as before (`checkRetired`), and two such hosts
  would have the old cluster's majority. This is on purpose, so that an outage at Hetzner
  does not keep hosts down. recover tells the user to set retired servers up anew or
  delete them.
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

- A removed host keeps the data key, and with it the CA's key, the sealing key and so
  every SealedSecret's value. A host that kept its disk can still issue certificates that
  the revocations do not name: host certificates, and CLI certificates after the cutoff.
  Rotating the data key, the CA and the sealing key (`cluster rotate-keys`, planned for
  phase 4) closes this; a new sealing key then needs every file sealed anew with
  `secret rekey`. Until then, wipe a removed host's disk.
- The CLI checks an agent's certificate against the CA, not against the revocations, as
  kubectl does. So a removed host could answer the CLI as the cluster, with a sealing key
  of its own for `secret seal`, or take the values of `create secret` and of a `Secret`
  in clear. The agents refuse such a host, but only the endpoints of
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
