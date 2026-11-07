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
| 2b | closer to Kubernetes: access, configuration and secrets, host roles ([below](#phase-2b-closer-to-kubernetes)) | in progress on `phase-2b`: steps 1 to 8 done, the final live tests green, the merge next |
| 2c | updates of clowder itself ([below](#phase-2c-updates-of-clowder)) | |
| 2d | GitOps as a resource ([below](#phase-2d-gitops)) | |
| 2e | the live tests' tooling in Go ([below](#phase-2e-the-live-tests-tooling)) | |
| 3 | backup and restore, RBAC, exported services ([below](#phase-3-data-access-and-services)) | |
| 4 | comfort and operations ([below](#phase-4-comfort-and-operations)) | |

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

- Each step got its own session, in the order below, with lint and the unit tests green
  and the full review loop (junior and senior) over what it changed.
- After step 5c, the live tests ran once in between.
- At the end come the live tests over the whole phase, then a last review round over the
  whole, then the merge. The state and every format change, so a cluster of an earlier
  build must be set up anew.
- Two keys keep their names apart. The **data key** encrypts the secrets in the state. The
  **sealing key** is the cluster's age key that a `SealedSecret` is encrypted for.

### Steps 1 to 8: done

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
  and with it the CA's key, the sealing key and every secret. Any other host asks the
  leader for what it needs and gets only that, as Kubernetes' node authorizer gives a
  node only the secrets of its pods: the secrets and registry logins of the stacks that
  run on it, and the routes' TLS keys while it holds the Floating IP. It keeps them in
  memory only. Decisions: the containers' label `clowder.secrets` is an unkeyed SHA-256
  over the state's digests, which are keyed already; logins only for the stacks a host
  runs, so a failover of a stack whose image needs a login waits for the pull; only
  workers prefetch images; a worker that restarts without a leader starts none of its
  stacks.
- **Step 7, recover and fencing with roles.** A failed host is fenced only if another
  active worker can take its stacks, so a control-plane host without the worker role keeps
  its vote. Each power-off runs in a job of its own, and fences of several hosts run at the
  same time. `clowderd recover --power-off` retires only the hosts that take no
  connection; a worker that answers stays, counts its synced copies, and rejoins as a fresh
  non-voter. Any control-plane host that answers stops the recover. A worker refuses a
  rejoin while it knows a leader. Losing every control-plane host is a known limit.
- **Step 8, clean up.** Dead code went, as `deadcode` and golangci-lint found it and a
  check by hand confirmed. Every loop of the agent and of the leader runs its passes one
  way (`internal/passes`); the agent's loops are values with `Run(ctx)`, in three groups
  that start at different times. The code of `clowderd` is one package per command under
  `internal/clowderd`, the daemon among them. `api.Peers` holds what an agent asks of the
  others, apart from `api.Server`, which answers; the agent builds it first, so no part
  waits for the server. A wait for the leader and the call to it share one budget. Who
  may propose which kind of command is one table (`proposers`). The
  command kinds' release gate went (see the decisions below); a worker's rejoin ends the
  agent with an exit status of its own that systemd counts as a success; `cpus` in the
  configuration is a number as in a stack; an alert channel refuses another type's
  fields; `examples/hetzner` installs `0.2.0`, the phase's release, which fits no earlier
  cluster.

### The final live tests

`test/live` follows the whole phase: credentials over SSH, the `ClusterConfiguration`
and the cluster's secrets applied with `clowder apply`, SealedSecrets, the layouts
`three`, `roles` (three control-plane hosts, two workers with ingress) and `single`
([live-tests.md](live-tests.md#layouts)). The interim run after step 5c was green; its
faults were all in `test/live` and its labs. Two scenarios run for the first time in the
final run: in `TestRoles`, a worker that gets only the secrets of its stacks (step 6), and
`TestRolesRecover`, a recover after the loss of two control-plane hosts in which the
workers' stacks and copies stay (step 7). All live tests must be green; a failure goes
back to the step it came from.

The final run found one fault: `clowder get` on a follower could miss the write of the
command before it, because a write's index reached only the reads of the same CLI process.
Reads of resources are now consistent, as kube-apiserver's are: a follower first catches up
with the leader's commit index, and asks the next leader if the one it knows has just
stepped down. `clowder get` thus needs a leader; `status` and `host list` do not. After
that, every scenario was green, in each layout. A run of lab 2's group of `three` lost its
first host to the load of the lab (no answer from `lvs` nor from the API) and was run again
alone.

Found in the last review round, for later: a table test that every kind of command that
any host may propose refuses another host's actor (today each handler checks it, each with
a test of its own); `state.applyPutVolume` split into named checks, and `sameJSON` made
explicit about a failed encoding; the lock of `controller.Fencing`'s jobs as a type of its
own; the depguard lists of `clowderd`'s commands replaced by a test of their imports.

## Phase 2c: updates of clowder

Today a new minor version stops every agent, and a release before `v1.0.0` may need the
cluster set up anew ([Updating a cluster](operating.md#updating-a-cluster)). Phase 2c
plans and builds how a running cluster moves to a new release:

- agents updated host by host, also across a minor version, so that N and N-1 run side by
  side (`buildinfo.Compatible` already allows it from `v1.0.0` on);
- a gate that writes a new kind of command, or a field that later commands depend on, only
  once every agent runs the release that brings it (step 8 removed the old gate, which
  could not act under today's rules);
- the order of an update (followers first, the leader last) and how the cluster shows its
  progress;
- the CLI told when it is older or newer than the cluster.

## Phase 2d: GitOps

A GitOps source becomes a resource of its own, which only the CLI may create. The source
applies the resource files of a Git repository, as `clowder apply` does, with
SealedSecrets for the values; the resources it makes belong to it (their owner is its
channel), and a prune removes what the files no longer name. To plan before it starts:

- whether a source may own the `ClusterConfiguration` and the Secrets of
  `clowder-system`. A source cannot answer the question before a new Hetzner token; the
  alternatives are that these stay with the CLI, or that a token that does not see every
  host's server is refused and alerted;
- polling and webhooks, and what happens to a commit whose files the cluster refuses
  (the old state keeps running).

## Phase 2e: the live tests' tooling

- The live labs' part of `bin/dev` (the labs' locks, the registry and mail containers, the
  hosts' image, splitting the scenarios among labs; about 700 of its 1000 lines of Bash)
  moves to Go next to `test/live`, where it can have unit tests.
- The large test files are split by topic: `test/live/live_test.go` and
  `internal/clowderd/daemon/daemontest`.

## Phase 3: data, access and services

- **Backup and restore.** The backups' loop freezes a copy's file system, snapshots it on
  the secondary, and sends it with restic to a Storage Box, with pre-hooks, retention and a
  prune that never runs during a backup. The Storage Box's credentials and the restic
  password are Secrets of `clowder-system`, named by a `backup:` section of the
  `ClusterConfiguration`; a worker gets them from the leader as it gets its stacks'
  secrets. A restore brings back a volume, a stack or a whole cluster: after a total loss,
  a new cluster from `clowderd init`, the resource files with their SealedSecrets sealed
  for the exported sealing key, and the volumes' data from the Storage Box. To decide
  before it starts: whether a backup holds the CA's key, so that the CLI's credentials of
  the lost cluster keep working.
- **RBAC as in Kubernetes.** Roles and RoleBindings (and their cluster-wide kinds) grant
  rights per namespace and kind to the name in a CLI certificate (its CN as the user, its
  O as the groups), as Kubernetes does with X.509 clients. The decision that a cluster
  has no users ends here.
- **Exported services.** A stack may export a service to stacks of other namespaces that
  import it: a virtual address and a DNS name, checked imports, export ports.

## Phase 4: comfort and operations

- Rolling updates of stacks: new containers start before the old ones go, where the
  service allows it.
- Changing a volume's `replicas` at any time.
- Changing a host's roles.
- Moving a volume to another storage class, online for `replicas` of 2 or more, with a
  short stop for 1; a fallback class when a class is full.
- Pruning images that no stack needs.
- The CLI learning the cluster's hosts, or keeping the last index it wrote, so that a
  `get` right after an `apply` sees it.
- A drained host that keeps its drain after a failure; a disabled voter that gives up its
  vote; workers that see only the specs they need; a resync of a host back from a fence
  within the resync limit.

## Open points

[operating.md's Known limits](operating.md#known-limits) lists what users meet; the
phases above close most of them. These remain:

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
- Before `v1.0.0`, only agents and CLIs of the same minor version work together, and a
  patch release adds no kind of command. So there is no release gate for the command
  kinds until phase 2c brings one.
- clowder runs on Hetzner Cloud with a Hetzner private network between the hosts, and
  nothing else: `internal/hetzner` is called directly, without a layer for other clouds.
- Until phase 3, a cluster has no users. The CLI's certificate controls it, as kubeadm's
  `admin.conf` does, and more credentials come from root on a host
  (`clowderd credentials`).
- clowder does not rotate the cluster's keys. A removed control-plane host's server is
  deleted or its disk wiped; a removed worker holds no keys.
- `hetzner.api` and `acme.directory` exist for test labs. When they are set, they may be
  `http://`, and the apply and the leader's log warn about them.
- A host's name in the cluster is its hostname, as DRBD knows it by that name.
- Code without a caller was removed; each phase brings back what it needs: phase 2c the
  release gate, phase 2d resources owned by a GitOps source, phase 3 `backup:` in the
  `ClusterConfiguration`.
- `v0.1.0` is the first release, `v0.2.0` the release of phase 2b. The APT repository
  keeps every version, and `bin/dev live release <tag>` tests a published release (the
  [release checklist](developing/shipping.md#release-checklist)).

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
