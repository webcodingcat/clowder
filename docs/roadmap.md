# Roadmap

clowder is built in phases, and each phase ends with live tests on HLab
([live-tests.md](live-tests.md)). This file is the one place that says how far the work is
and what comes next. What users meet today, gaps included, is in
[operating.md](operating.md); the history of finished work is in the Git log.

| Phase | Builds | State |
|---|---|---|
| 0 | the spike: DRBD quorum and fencing, WireGuard, the real client IP, on three hosts | done |
| 1a | the foundation on one host: stacks, cluster directory, host check, state, PKI, Raft, API, network, storage, containers | done, passes the live tests |
| 1b | ingress (Traefik, ACME), priorities, alerts, metrics, still on one host | done, passes the live tests |
| 1c | namespaces and resources: `clowder apply`, stacks with a schema of their own, volumes and secrets as resources, still on one host | done, passes the live tests |
| 1d | encrypted values, access and recovery, still on one host | done, passes the live tests |
| 2 | three hosts: replication, failover with fencing | done, passes the live tests |
| 2b | closer to Kubernetes: access, configuration and secrets, host roles ([below](#phase-2b-closer-to-kubernetes)) | in progress on `phase-2b`: steps 1 to 6 done, the interim live tests green, step 7 next |
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
steps. It is built on the branch `phase-2b` and goes to `main` only once it is complete.

- Each step gets its own session, in the order below. After each step, lint and the unit
  tests are green.
- Each step gets the full review loop (junior and senior) over what it changed.
- After step 5c, the live tests run once in between. By then access, configuration,
  secrets, roles, the rules of who may call what and the cheaper copies of the state are in
  place, and every host still holds the keys, so the cluster is complete and not a bridge.
  A fault found there lies in the first steps, not in all of them. `test/live`, `bin/dev
  live` and the lab of roles change for this run, before step 6.
- At the end come the live tests over the whole phase, then a last review round over the
  whole, then the merge. The state and every format change, so a cluster of an earlier
  build must be set up anew.
- Each step updates the documentation of what it changes: the README, `docs/operating.md`
  (with its Known limits), `docs/resources.md`, DEVELOPING.md, `docs/developing/`, and the
  README of `examples/hetzner`.
- Two keys keep their names apart. The **data key** encrypts the secrets in the state. The
  **sealing key** is the cluster's age key that a `SealedSecret` is encrypted for.

### Steps 1 to 6: done

Done on `phase-2b`, each with lint and unit tests green and the full review loop; the
interim live tests after step 5c are green ([below](#the-live-tests-after-step-5c-and-at-the-end)).
What they built, and the decisions that still hold:

- **Step 1, access the way of kubeadm.** There is no bootstrap key. The first credentials
  come over SSH (`clowderd credentials`, then `clowder config import`), as kubeadm's
  `admin.conf` does. The credentials work as a kubeconfig, with contexts and a list of
  endpoints. A CLI certificate names an actor for the audit, grants nothing, and lasts one
  year.
- **Step 2, the configuration as a resource.** `cluster.yaml` became the resource
  `ClusterConfiguration` named `cluster`, applied with `clowder apply`; the cluster
  directory went. Whatever names a secret value names a Secret and a key (`{name, key}`,
  as `secretKeyRef`). The cluster's own secrets are Secrets of `clowder-system`. Before it
  applies a new Hetzner token, the CLI says how many of the hosts' servers the token sees
  and asks.
- **Step 3, Secret and SealedSecret.** A value comes in as a `Secret` in clear or as a
  `SealedSecret` sealed for the sealing key, as with Bitnami's sealed secrets; both end up
  as the same Secret. Deviations, each for a reason: `data` and `stringData` stand under
  `spec`, like every clowder resource's fields; a SealedSecret is no resource of its own
  and has no `template`, because it is only a way in; values are sealed with age, which
  the cluster already uses, instead of RSA. The same value applied again changes nothing.
  There is no recovery key: the sealing key is exported and kept offline.
- **Step 4, host roles.** A host has the roles `control-plane`, `worker` and `ingress`,
  fixed once it has joined. Voters are control-plane hosts; stacks and copies with a disk
  go on workers only; a tiebreaker may go on any host. The join token names the roles it
  allows. Deviations: the roles stand in the host's certificate rather than in labels, so
  that a host's calls can be checked from its certificate alone; the join token names the
  roles instead of a second secret, as it opens only once. The phase `passive` went, and
  a volume gets its second copy with a disk only once a third host can take its
  tiebreaker.
- **Step 5, who may call what.** Every way into a host checks the caller's roles, so an
  attacker who takes over a worker's certificate gets no further than its own stacks. A
  worker answers only the control plane, only on the private network. The join, the CLI
  and Raft take only control-plane hosts. An agent whose `host.json`, certificate and
  state name different roles does not start.
- **Step 5b, copy the state only where it changes.** A typed clone (`State.Clone`) shares
  what the handlers only ever replace whole (specs, encrypted values, certificates)
  instead of copying the whole state through JSON. Copy-on-write in every handler was
  weighed and rejected: every handler would change, and a forgotten clone is a silent bug.
- **Step 5c, smaller documentation.** The documentation is split by reader: the README,
  `docs/operating.md` and `docs/resources.md` for users; DEVELOPING.md and one file per
  package in `docs/developing/` for developers. Comments keep their reason and lose the
  rest.

- **Step 6, keys only on the control plane.** Only control-plane hosts hold the data key,
  and with it the CA's key, the sealing key and every secret; a join's answer carries it
  only for them. The rules of step 5 now hold against root on a worker too. Any other
  host asks the leader over mTLS for what it needs, and gets only that, as Kubernetes'
  node authorizer gives a node only the secrets of its pods
  (`internal/hosts/self/secrets` and `…/tls`, any host, for itself): the secrets and
  registry logins of the stacks that run on it (`state.SecretsOfHost`), and the routes'
  TLS keys while the state names it for the Floating IP. The host keeps the answers in
  memory and asks again only for a new digest or chain. The leader's loops, `QuorumWatch`
  and `checkRetired` run on control-plane hosts only. Decisions, each for a reason:
  - No key of the secrets' digests: the containers' label `clowder.secrets` became a
    SHA-256 over the state's digests, which are keyed already, as Helm's checksum
    annotations are unkeyed. A key of its own for every host would protect nothing more.
    The new label recreates every container with secret files once; the phase needs a
    cluster set up anew anyway.
  - Logins only for the stacks that run on a host, as Kubernetes does, not for those it
    may take over. The prefetcher pulls without logins; an image that needs one is pulled
    when its stack starts, and a move does not wait for it. A failover of such a stack
    waits for the pull (a known limit).
  - Only workers prefetch; a control-plane host without the worker role pulled images
    of stacks it can never take.
  - A worker that restarts while there is no leader starts none of its stacks: their
    secrets are kept on no disk, by choice. A worker cut off from every control-plane host
    alerts nobody; the voters see it fail. Both are known limits.

### Step 7: recover and fencing with roles

Agreed with the operator before the plan.

- **Fencing and failover stay as they are.** Every host sends beats to the voters. A host that
  a majority of the voters has not heard from for 45 s is failed (with one control-plane host, that one
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
  - The routes `internal/recover/copies` and `internal/recover/rejoin` take a
    control-plane host.
- **`checkRetired` only on the control plane** (step 6 moved it). A retired voter is the
  danger, as two of them could elect each other. Voters hold the token and check at their
  start, as now. A retired worker is harmless: it never votes, its certificate is revoked
  so no control-plane host accepts it, it is out of the WireGuard mesh and the DRBD
  resources, and it starts no stack before it has caught up with a leader it never
  reaches.
- **Every control-plane host gone:** the cluster is lost, as a Kubernetes cluster without
  its etcd is. The workers hold the log but no key. The way back is a new cluster, the
  `SealedSecret`s sealed anew with the exported sealing key, and the data from phase 3's
  backups. This is a known limit; three control-plane hosts are the advice.

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
  the code of steps 6 to 8 and shrinks steps 6 to 8 in this file as 5c did the others.
- **A new release, not an update:** phase 2b's release fits no earlier cluster, so every
  command kind is new with it. The command kinds' release gate (`since`,
  `State.CheckKnown`) starts anew from that release or goes, and `examples/hetzner`'s
  `clowderd_version` points to the new release.
- **The plan for phase 3 and later:** phases 3 and 4 and the open points below are checked
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
  each host's roles. A third layout, `single`, is a cluster of one host with every role,
  which clowder supports as a cluster of its own ([live-tests.md](live-tests.md#layouts)).
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
    copies stay. In the lab of roles, the workers' public API port is closed, so
    `test/live` reaches the cluster through control-plane hosts only.
- The run after step 5c has every scenario except those of steps 6 and 7 (a worker that
  gets only the secrets of its stacks, the recover with workers). It is green. Its faults
  were all in `test/live` and its labs, none in clowder: the tests had not followed the
  `ROLES` column of `host list`, a join request's roles, a Volume before its Stack in one
  apply, the cluster's name in alert mails, and the second copy that waits for a third
  active host; the labs accepted any Hetzner token; and a test raced the configuration
  against the host that alerts from its own state. `TestSingleHost` adds the cluster of
  one host.
- The scenario of step 6 is in `test/live` (`TestRoles`, "a worker gets only the secrets
  of its stacks") and runs for the first time in the final run.
- All live tests must be green; a failure goes back to the step it came from.

### After phase 2b

- A removed worker holds no keys, and the CLI does not accept it, so revoking its
  certificate is enough. `cluster rotate-keys` (phase 4) is needed only after a
  control-plane host is removed.

## Open points

[operating.md's Known limits](operating.md#known-limits) lists what users meet. These are
the phases that close them, and the points users do not see:

- **Phase 3 (backup and restore):** no backups; a volume that waits for a restore after a
  recover.
- **Phase 4 (comfort and operations):** `cluster rotate-keys` for a removed host's keys;
  changing a host's roles; changing `replicas`; pruning images; the CLI learning hosts
  from the cluster or keeping the last index it wrote, so that a `get` right after an
  `apply` sees it; a drained host that keeps its drain after a failure; a disabled voter
  that gives up its vote; workers that see only the specs they need; a resync of a host
  back from a fence within the resync limit.
- **When one is needed:** a way for Prometheus to reach `/metrics` (through Traefik, or a
  permission for certain stacks).
- **Not visible to users:** a placement is several commands in a row (push-outs, volumes,
  the stack's host). A new leader repairs in its next pass what an old one left half done;
  one command per placement would leave fewer states in between.

## Decisions that still hold

- Until `v1.0.0`, the state and every format (the configuration, the credentials, the
  join request, the flags of `clowderd init` and `join`) may change between releases
  without a way to upgrade a running cluster. Such a release needs a cluster set up anew,
  and credentials of an earlier build are refused.
- Before `v1.0.0`, only agents and CLIs of the same minor version work together.
  [Updating a cluster](operating.md#updating-a-cluster) says in which order to update.
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
