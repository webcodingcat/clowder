# Roadmap

clowder is built in phases; each ends with live tests on HLab
([live-tests.md](live-tests.md)). This file is the one place that says how far it is.

| Phase | Builds | State |
|---|---|---|
| 0 | the spike: DRBD quorum and fencing, WireGuard, the real client IP, on three hosts | done |
| 1a | the foundation on one host: stacks, cluster directory, host check, state, PKI, Raft, API, network, storage, containers | done, passes the live tests |
| 1b | ingress (Traefik, ACME), priorities, alerts, metrics, still on one host | done, passes the live tests |
| 1c | namespaces and resources: `clowder apply`, stacks with a schema of their own, volumes and secrets as resources, still on one host | done, passes the live tests |
| 1d | encrypted values (`!secret`), access and recovery, still on one host | done, passes the live tests |
| 2 | three hosts: replication, failover with fencing | done, passes the live tests |
| 2b | access and host roles the way of kubeadm ([below](#phase-2b-access-and-host-roles-the-way-of-kubeadm)) | planned |
| 3 | backup and restore, GitOps | |
| 4 | comfort and operations | |

Phase 1d replaced SOPS with values of their own: tagged `!secret` in `cluster.yaml` and in
Secrets, encrypted for the cluster and the recovery key (`secret encrypt`, `rekey`,
`decrypt`), with the credentials in the user's config directory and access back from root
on a host (now `clowderd credentials`, below).

Phase 2 began with the live tests, before any code for several hosts: `bin/dev live up`,
`run` and `down` apart, with a lock against two runs; scenarios that each check their
baseline and run alone (`run -run <test>`); and `TestBoundaries`, which pins what a
container reaches on one host before the network changes for several. Then the volume
reconciler: every host keeps the copies the state places on it, with or without a stack;
a copy no stack there uses is unmounted and secondary; the state knows whether a volume
holds data (`set-volume-initialized`), and only a volume that does not is promoted with
`--force` and formatted; and the steps that write to a volume run in a job per volume,
which no pass waits for. Then versions: an agent that meets a Raft entry or snapshot it
cannot read stops (exit status 3, which systemd does not restart) instead of leaving it
out; the leader writes a new kind of command only once the oldest agent reads it; before
`v1.0.0` only the same minor version works together, development builds with a warning;
and [Updating a cluster](developing/shipping.md#updating-a-cluster) says in which order.
Then the leader's plans and the locks: the placement's commands name the state they were
planned with, and the state refuses one about a stack or volume that changed since; the
leader places only stacks that have no host yet. Operations came back into the state,
with locks on stacks, volumes and hosts that every handler of theirs minds, and steps
that only their owner records, each once. Nothing starts an operation yet: moving a
stack and fencing bring the first, with their steps, deadlines and rollbacks, and the
placement then leaves locked stacks alone. Then the network for several hosts: a
WireGuard interface per peer with its own port, route and routing table for the answers
that come back without SNAT, MSS clamping, and the connections of a former peer
forgotten. The containers' boundaries go first: a packet with a source outside its
bridge's network is dropped before anything is accepted, and beyond what was redirected,
the hosts' addresses, the private network (`private_network` in `cluster.yaml`, which
took the place of `network`) and the overlay stay closed. The host's own processes no
longer reach the metadata service either. The DNAT to another host, and the conntrack
cleanup when its target moves, came with the ingress failover.

Then the join:

- A host joins with a token that a user makes (`clowder host join-token`). The token
  opens once and expires; the state keeps only its hash.
- The bootstrap key lets the first user in. After that it opens nothing, not even a
  proof of the CA. `join_keys` is gone from `cluster.yaml`.
- The leader asks Hetzner whether a joining host's server is its own: the server has
  the host's underlay as an address, and no server serves two hosts. It checks the first
  host's server too, once `cluster.yaml` brings the Hetzner token. Only a host with a
  checked server gets the Floating IP.
- The leader keeps Raft's members. A second host waits passive. With the third, three
  hosts vote; from six hosts on, five vote.
- A cluster that had three hosts keeps three: `host remove` refuses to go below. A leader
  that is removed hands its lead over first. A removed host's ID is free again once every
  host forgot it as a DRBD peer.
- A host keeps its WireGuard mesh in `net.json`. After a reboot it finds its peers before
  Raft runs.
- The live tests run on three hosts and replace one. They show that a join with a wrong
  token or a wrong server is refused.

The bootstrap command changed with it, so a cluster of an earlier build must be set up
anew. `clowderd init` takes no `--name` any more: a host's name in the cluster is its
hostname, which DRBD knows it by.

Then the placement over hosts:

- Each copy's host tells the state whether the copy is synced (`set-copy-synced`);
  `get volumes` shows `degraded`, `resyncing` or `ok`, and `/metrics` what is left of a
  resync.
- The placement keeps the copies: a missing one goes to the host with the most free
  space, a surplus one goes once the others are synced, and a volume with two copies
  gets a diskless tiebreaker on a third host. A new copy starts only while no other
  syncs, in the whole cluster: the resync limit. A host forgets a peer that left one
  of its resources, so the slots in DRBD's metadata do not run out.
- A stack moves with an operation, `move-stack`: copies on the new host, synced, the
  images pulled; then the old host stops the stack, and the new one starts it. Each
  step has a deadline; a resync that is late but gets on gets more time, anything else
  before the stack stopped rolls back. A user moves a stack (`clowder stack move`),
  `host drain` moves all of a host's, a stack whose volumes are elsewhere moves to
  them, and stacks of lower priority move to make room for a new one (not twice within
  ten minutes).
- Every host pulls the images of the stacks it may take over before it needs them.
- `host remove` of a host that still has stacks or copies lets it leave: they move off
  it first, then the leader removes it.
- A host takes a copy it gives up down only once no primary peer is connected to it,
  so the primary of two copies keeps its quorum.
- The ACME self-check asks the ingress host's Traefik, not the leader's own.
- The live tests use all three hosts: they move a stack with its data from host to host,
  add a copy and a tiebreaker, and drain a host (`TestPlacement`).

The state changed with it, so a cluster of an earlier build must be set up anew.

Then failover:

- Every agent beats to every voter every five seconds, outside the Raft log. A host a
  majority of the voters has not heard for 45 s is `failed`; the leader alone, cut off
  from it, fails no host. A failed host that is heard again is active again, but only
  the leader says so, never the host.
- The leader fences a failed host that has stacks another host can take: it powers its
  server off through Hetzner's API, and only then do its stacks start elsewhere, on a host
  with a synced copy of their data. A host without such stacks, such as one drained and
  rebooting for maintenance, stays failed until it is heard again. Once the
  fence started (`fencing`), nothing stops it; a server that cannot be powered off leaves
  the host failed, with an alert, until a user runs `clowder host fence-confirm`. One host
  at a time, and only while a majority of the voters stays active.
- The fenced host is powered on again, comes back as `recovering`, and is `active` once its
  copies caught up. Fenced three times within an hour, it stays off (`disabled`) until
  `clowder host enable`. A volume of replicas 1 waits for its host; one of replicas 0
  starts anew, empty; a copy on a host that does not come back within twenty minutes is made
  anew elsewhere.
- A host that was away starts its stacks only once it holds the leader's commit index,
  and a new leader acts only once it has applied the log. Each stack comes up in a job of
  its own. Hetzner's API is asked again while it is busy.
- The live tests fail hosts through HLab's faults: frozen, cut off, with Hetzner's API
  down, with an agent that crashes at once, and during a move (`TestFailover`,
  `TestNoFalseFailover`, `TestCrashingAgent`, `TestMoveInterrupted`).
- The live tests found three older faults, now fixed: a deleted volume's DRBD minor went
  to a new volume while a host still had the old copy's resource (`RetiredMinors`); a
  resource file whose `adjust` failed counted as applied and was never adjusted again; and
  a volume copy whose step failed was tried again at once, in a tight loop. Every way to
  the leader waits a moment for one during an election.

The state changed with it, so a cluster of an earlier build must be set up anew.

Then the ingress failover and DRBD's quorum:

- Every host routes every stack, as the Floating IP may move to any. A published port of a
  stack on another host goes there by DNAT with the client's address, through WireGuard,
  and Docker's FORWARD chain lets it pass (`DOCKER-USER`); a container's own connection
  to it leaves from its host's node address (hairpin). HTTP goes from the ingress host's
  Traefik to the stack host's, with the client in `X-Forwarded-For`. When a port's target
  changes, every host forgets its connections, so a UDP flow follows the stack.
- The Floating IP follows a drain or a failure of its host, and stays where it is when
  that host is back.
- A volume's copies and its tiebreaker change in one command: the copy a move gives up
  becomes the tiebreaker on its host, so no node leaves the resource, and a volume of
  replicas 1 has a tiebreaker while it moves. A host that fails in between no longer
  takes the other copy's quorum.
- Each host tells the state which of its copies stand alone from a peer (a split brain)
  and which primaries lack DRBD's quorum: the alerts `split-brain` and `no-quorum`, and
  `/metrics` has each copy's role, data, quorum and split brain. `clowder volume
  resolve-splitbrain <volume> --keep <host>` connects the copies again, with the data of
  the copy kept.
- An agent that knows no leader for two minutes alerts by itself (`no-leader`).
- An agent that just started holds users' calls until its state caught up with the
  leader's; before, a call right after a restart could find the state empty.
- The weekly trim skips a copy without quorum and tries again an hour later, as it does
  after a failure.
- The live tests serve a stack from another host than the ingress host, move it and the
  Floating IP, fail a host right after a move, and take two hosts away
  (`TestIngressFailover`, `TestQuorum`). A split brain could not be made by hand: a copy
  promoted apart from the newer data has no quorum, and its writes hang.

The state changed with it, so a cluster of an earlier build must be set up anew.

Then the way out when the majority is gone for good:

- `clowderd recover --power-off`, as root on the host that is left, powers the other hosts
  off through Hetzner's API, labels their servers `clowder-retired` with their certificate's
  serial, and makes this host the only voter. It refuses while another host knows a
  leader or answers without saying, and asks for the cluster's name first.
- The cluster keeps what that host holds: a volume with a synced copy there runs on, one
  without waits for a restore (`restore-pending`), one of replicas 0 starts empty. The
  other hosts are removed, and the cluster grows again as a new one does, with the alert
  `no-ha` until three hosts run stacks.
- A retired host's agent ends at its start (exit status 4), before Raft or a stack; a
  server set up anew has another certificate and starts.
- The live tests take two hosts away for good, recover, switch the retired servers on
  again and grow back to three (`TestRecover`).

The state changed with it, so a cluster of an earlier build must be set up anew.

The live tests closed the phase: they now check every point the phase set out to show.

- A host is replaced while a stack runs with a copy on it: the copy moves to the new host,
  and the stack runs on in the same container (`TestReplace`).
- A cluster of one host grows to three while a stack runs, and its volume gets its copy
  and its tiebreaker (`TestRecover`).
- A volume of 20 GB resyncs while the leader and one host are cut apart and every host
  pauses for 20 s: no host fails (`TestNoFalseFailover`).
- The alerts `fencing-blocked` and `host-disabled` open and resolve (`TestFailover`).
- `bin/dev live run` checks again that the checkout is at the release's tag after `live up
  <tag>`.
- DRBD 9.3 forgets a peer that a resource never knew, or forgot already, without a word;
  it refuses only a node-id out of range.

Before it, the first release became installable and the commands honest: the APT
repository keeps every version, `examples/hetzner` pins `0.1.0`, `bin/dev live release
<tag>` tests a published release (the [release checklist](developing/shipping.md#release-checklist)),
an upgrade restarts the agent, `host remove` asks first and keeps the last host that runs
stacks, `host drain` says that it moves nothing, and a cluster has one user. `v0.1.0` is
out; its release candidate passed the live tests without the test hook.

Code without a caller went then; each phase brings back what it needs:

- phase 3: `backup:` in `cluster.yaml`, and resources owned by a GitOps source.

Then the layer between clowder and its one cloud went: clowder runs on Hetzner Cloud,
with a Hetzner private network between the hosts, and nothing else.

- `internal/hetzner` is the client, called directly; the interface, the plugin switch
  and the hand-made fakes of the tests are gone. The tests use a fake of the API
  (`hetzner/hetznertest`) through the real client.
- `cluster.yaml` has `hetzner: {token, api}` in place of `hoster: {type, token, api}`,
  and `private_network` is required. A `cluster.yaml` with `hoster` or the older
  `network` fails with the key that took its place.
- A host has a server ID (`--server-id`, Hetzner's ID; it was `--server-ref`), an
  underlay in `private_network`, which the state checks at each join and each
  `cluster apply`, and a public address; all three are required. The leader's check of
  a server takes the underlay among its private addresses and the public address as its
  public IPv4.
- The client reads each answer to its end, so its connections are used again.

The state, the join request and `clowderd init`'s and `join`'s flags changed with it,
so a cluster of an earlier build must be set up anew.

Then access went the way of kubeadm, before real production use:

- A cluster has no users. The CLI's certificate (`cli`, as `admin.conf`) controls it;
  the actor of its commands is `cli`, so a host that forwards one has no name to forge.
  `Authorize` and the user names went (`cluster bootstrap` takes none).
- More credentials, or new ones after a lost laptop, come from root on a host:
  `clowderd credentials` prints a `credentials.yaml`. With `--revoke-older` every agent
  refuses the CLI certificates issued before it (`State.CLICutoff`). The codes from a
  host, `user recover` and the code of `recovery rotate` went.
- `hetzner.api` and `acme.directory` are for test labs: set, they may be `http://`, and
  `cluster apply`, `diff` and the leader's log warn about them.

The state, the bootstrap request and `credentials.yaml` changed with it: a cluster of an
earlier build must be set up anew, and credentials of an earlier build are refused.

Until `v1.0.0`, the state and `cluster.yaml` may change between releases without a way
to upgrade a running cluster.

## Phase 2b: access and host roles the way of kubeadm

Before phase 3, while the structure is still cheap to change, clowder moves closer to
Kubernetes and kubeadm in how users get in, how long their certificates last, how the
cluster's configuration and secrets come in, and which hosts hold the cluster's keys.
clowder goes beyond Kubernetes only where it would otherwise lose a protection (the
CLI's cutoff, the check of the Hetzner token).

Six packages, in this order, each in a session of its own. Each changes the state or a
file format, so a cluster of an earlier build is set up anew. Each touches access, keys
or a format, so each gets the full review loop (junior and senior), and ends with the
live tests.

### Package 1: CLI certificates and credentials

- **A name in each CLI certificate,** for the audit: `clowderd credentials --name
  <name>`, held to `names.Valid`, as `kubeadm kubeconfig user --client-name`. Without
  it the name is `admin`, as kubeadm's `admin.conf` is always `kubernetes-admin`. The
  certificate's common name is the name; its organizational unit
  stays `cli`. The actor of its commands becomes `cli/<name>`, in the agent's log and in
  the Raft command. The name grants nothing: every CLI certificate controls the cluster,
  and a resource's owner stays the channel `cli`. The cutoff (`--revoke-older`) stays the
  only revocation, as Kubernetes cannot revoke client certificates at all.
- **One year,** kubeadm's default for client certificates. Host certificates keep
  `leafLifetime`, the CA `caLifetime`. From 30 days before the end, every CLI command
  warns on stderr. A new certificate comes from `clowderd credentials` again (as
  `kubeadm certs renew admin.conf`); the CLI does not renew itself.
- **The credentials work as a kubeconfig.** One file holds contexts, each with its
  endpoints, the CA, the key and the certificate; `current_context` picks one. The CLI
  reads `--config`, else `$CLOWDER_CONFIG`, else `~/.clowder/config` (as
  `~/.kube/config`), with `--context` for another context in one call and `clowder
  config use-context|get-contexts`. `clowderd credentials` prints a file with one
  context, so one file from the host is all a user needs. `clowder config import <file>
  [--context <name>]` adds it to the user's file: kubectl has no such command, and
  merging by hand is where keys get lost. A context's name is the user's own, as in a
  kubeconfig: the cluster's name without `--context`, and a name already taken needs
  one.
- **The cluster's name is optional,** as kubeadm's `clusterName`: `clowderd init
  --cluster-name <name>`, `clowder-cluster` without it. It is kept in the state, shown by
  `cluster status`, and names the context of new credentials; it checks nothing.
- **`endpoints` leave `cluster.yaml`:** the credentials name the server, as a kubeconfig
  does. Which addresses they list is the operator's choice: those of the control-plane
  hosts, or one address in front of them (a Floating IP of their own, as kubeadm's
  `--control-plane-endpoint`). `clowderd credentials` writes the public addresses of the
  control-plane hosts at the time, or what `--endpoint` names. The CLI trusts any host
  certificate of the CA, whatever the address, so both work. The known gap that the CLI
  learns no hosts stays for the first way: after joins and removes, the user fetches new
  credentials or edits the endpoints.
- The CLI no longer finds its credentials through the cluster directory; until package 2,
  only `cluster apply`, `diff` and the `secret` commands read `cluster.yaml`.

For the operator: the credentials move to `~/.clowder/config` in a new format,
`endpoints` go from `cluster.yaml` into it, `clowderd credentials` takes a `--name`,
`clowderd init` takes an optional `--cluster-name`, and the CLI's certificate is renewed
once a year.

### Package 2: configuration and secrets the way of Kubernetes

- **`cluster.yaml` becomes a resource,** `kind: ClusterConfiguration`, as kubeadm's
  `ClusterConfiguration`: one per cluster, without a namespace, applied with `clowder
  apply -f` together with namespaces, secrets and stacks. Its fields are camelCase, as
  every resource's. `cluster apply` and `cluster diff` go (`apply --dry-run` and a
  `clowder diff -f`, as `kubectl diff`, take their place), and with them the cluster
  directory: `--cluster`, `$CLOWDER_CLUSTER`, `clusterdir.Find` and `clusterdir.Create`.
  clowder does not care where the files come from. Its `metadata.name` is always
  `cluster`, as OpenShift names its configuration resources.
- **No name check,** as Kubernetes checks none: `cluster.yaml`'s `name` goes (the
  cluster's name comes from `clowderd init`, package 1), and `clowderd recover` asks for
  the host's name instead of the cluster's. What the name guarded against, a configuration with another project's Hetzner token
  (fencing would power off that project's servers), the token itself now shows: the
  leader refuses a configuration whose token does not see the servers of every host
  (`hetzner.Client.CheckServer`, as at a join).
- **No files next to it:** `acme.ca` and an alert channel's `smtp.ca` stand in the
  resource as PEM, as Kubernetes' `caBundle` does. `clusterspec.FileNames`,
  `CheckFiles` and `ClusterConfig.Files` go.
- **No secret values in it:** the Hetzner token, the registries' passwords and the
  alert channels' URLs and passwords are Secrets in the namespace `clowder-system`, which
  the configuration names, as the hcloud controller reads `kube-system/hcloud`. `apply`
  refuses a configuration that names a Secret that does not exist, and a delete of a
  Secret it names. `!secret`, `clusterspec.SecretPlaces` and the case of `cluster.yaml`
  in `secret rekey` go.
- **`Secret` and `SealedSecret`,** as in Kubernetes with Bitnami's sealed secrets. A
  `Secret` carries its value in clear (`stringData`, `data`), or comes from `clowder
  create secret <namespace>/<name> --from-literal|--from-file`; the CLI sends it over
  mTLS and the cluster seals it. It does not belong in Git. A `SealedSecret` carries its
  value encrypted for the cluster's age key and its own namespace and name (today's
  `!secret` value), made by `clowder secret seal` as `kubeseal` makes one; it goes into
  Git. Both end as the same sealed secret in the state; a stack or the configuration
  names a Secret and does not care which way it came.
- **No recovery key:** as with sealed secrets, the cluster's age key is exported and kept
  offline: `clowderd sealing-key export`, as root on a host. `secret decrypt` and `secret
  rekey` take that key in place of the recovery key. The recovery key, `recovery rotate`,
  `/v1/recovery/rotate` and the second recipient of every value go. Whether phase 3's
  backups keep the key too is decided there.

For the operator: `cluster.yaml` is a `ClusterConfiguration` applied with `clowder apply`,
without `name`, with its CAs inline and its secrets as Secrets in `clowder-system`; `!secret` values
become `SealedSecret`s (`clowder secret seal`); the recovery key gives way to an exported
sealing key.

### Package 3: the first access as `kubeadm init`

- `clowderd init` makes the cluster as now; the first credentials come over SSH, as
  kubeadm's `admin.conf` does: `ssh root@<host> clowderd credentials --name <name>`. The
  CA is trusted through SSH.
- Gone: the bootstrap key (`examples/hetzner`'s `random_password`, the user data,
  `/etc/clowder/bootstrap-key`, `--bootstrap-key-file`), `GET` and `POST /v1/bootstrap`,
  `finish-bootstrap`, `MinBootstrapKeyLength`, the limit on the bootstrap's routes,
  and `clowder cluster bootstrap`. `TrustProof` stays for join tokens. The module's
  output and the README take the place of the configuration the bootstrap wrote: a
  `ClusterConfiguration` to apply.
- The live tests fetch their credentials over SSH.

Why: no secret is left in the user data or the metadata service, which closes the window
at boot that the module's README describes, and a whole protocol goes. The price is SSH,
which `examples/hetzner` already opens to `admin_cidrs`.

For the operator: no bootstrap key; `ssh … clowderd credentials`, then `clowder config
import`, then `clowder apply` of the configuration.

### Package 4: control plane and workers – roles, join, Raft

- A host has the role control plane, worker, or both. Today every host has both; a
  cluster of three stays so. Only a worker runs stacks and holds volume copies: a
  control-plane host without the worker role runs none. `clowderd init` makes a host
  with both roles, `--worker=false` one of the control plane only; `clowderd join` makes
  a worker, `--control-plane` adds the control plane (and `--worker=false` drops the
  worker). A control-plane join needs a token made with `clowder host join-token
  --control-plane`; only its answer carries the data key. The token's role replaces
  kubeadm's `--certificate-key`.
- Only control-plane hosts hold the data key (and with it the CA's key and the cluster's
  age key), vote in Raft (1, 3 or 5 of them, as now), lead, serve the CLI's API, and run
  `clowderd credentials` and `clowderd recover`. The passive phase applies to them.
- Workers follow the Raft log as non-voters, so the agent's loops keep reading the
  state as now. They see every spec, which Kubernetes does not show a node, and the
  secrets only sealed.
- The placement places stacks and copies on workers only. A tiebreaker holds no data
  and no load, so it may go on any host, a control-plane host without the worker role
  too. High availability needs three voters and enough workers for the volumes' copies;
  three hosts with both roles are enough.

### Package 5: workers get only their stacks' secrets

- A worker asks a control-plane host over mTLS for the secrets of a stack the state
  places on it, and the control-plane host checks that it does, as Kubernetes' node
  authorizer. Stacks start only after the catch-up with the leader anyway.
- A worker commits only host commands about itself, as Kubernetes' NodeRestriction, and
  never forwards the CLI's commands; today every host forwards them as `cli`.
- The ingress host needs the TLS keys of every route. A worker that holds the Floating
  IP fetches them as it fetches secrets, and the control-plane host checks that it is
  the ingress host.

### Package 6: recover and fencing with workers

- Fencing and failover stay as they are: every host beats to the voters.
- `clowderd recover` runs on a control-plane host, which may hold no copies: the remap
  must take the copies on workers that are still reachable into account instead of
  retiring every other host. The package starts with a design of its own.

### After phase 2b

- A removed worker holds no keys; revoking its certificate is enough. `cluster
  rotate-keys` (phase 4) is needed only after a control-plane host is removed, which
  makes [Keys of a removed host](#keys-of-a-removed-host) smaller.

## Known gaps

What phase 2 left open, sorted by the phase that closes it.

Phase 3 (backup and restore):

- A volume that waits for a restore after a recover waits until phase 3 brings restores;
  until then, deleting it and applying it anew gives an empty one. An unsynced copy left
  on the host that recovered stays as it is, for a human.

Phase 4 (comfort and operations):

- A removed host keeps the cluster's keys until `cluster rotate-keys` exists (Keys of a
  removed host, below); until then, wipe its disk.
- A host that comes back after a fence resyncs all its copies at once, each at the
  resync limit's rate; the limit holds back only new copies meanwhile.
- A failed host that is not fenced (it needs none, no `cluster.yaml` applied yet, a server
  not confirmed, Hetzner's API down for good) keeps its copies counted: none is made anew
  elsewhere until a user runs `host fence-confirm`. The alert `fencing-blocked` says so.
- A drained host that fails comes back active, not drained: its drain is lost; a user's
  remove is kept.
- A disabled voter keeps its vote until it is removed: with more than three hosts, an
  active host that does not vote does not take its place.
- The CLI learns no hosts from the cluster: the user keeps `endpoints` in `cluster.yaml`
  current after each join and remove, or the CLI cannot reach the cluster while the
  endpoints it lists are down.
- A published port whose stack moved while the agent of the ingress host was down keeps
  its connections to the old host until they end: the agent forgets connections only
  when it sees a target change.
- `replicas` cannot change yet: raising it (1 to 2 or 3) only needs the placement to
  allow it, as copies come online now.
- Images are never pruned (no `image prune`), and every host pulls those of the stacks it
  may take over: the root file system fills up over time.
- A read sees a write of the same `clowder` call (the index header), not one of the
  call before: `clowder get` right after `clowder apply` may show, for a moment, the
  state of a follower that has not applied the apply yet. The CLI could keep the last
  index it wrote in its config directory and ask for it.
- A cluster that had three hosts never goes below three, a dead host too: a
  replacement joins first. If none can (examples/hetzner joins through its first host),
  the cluster stays at two; `clowderd recover --power-off` is the way out only when two
  are gone.
- A retired host whose agent starts while Hetzner's API does not answer starts as
  before, and two such hosts would have the old cluster's majority. recover tells the
  user to set retired servers up anew or delete them.
- recover knows the hosts that this host's log knows. A host the others added after this
  one fell behind is neither asked nor retired: power its server off by hand.
- A placement is several commands in a row (push-outs, volumes, the stack's host). A new
  leader repairs what one left half done in its next pass; one command per placement
  would leave fewer states between.
- `examples/hetzner` ignores later changes to the server's user data, so a storage class
  added later gets a volume but no thin pool on an existing host.

When one is needed:

- A Prometheus in a stack or outside the cluster cannot reach `/metrics` yet: only the
  hosts reach the node address. The way in (through Traefik, or a permission for certain
  stacks) comes when one is needed.

Not live-tested, as the lab cannot make it:

- `volume resolve-splitbrain` never ran on real DRBD, only against unit tests: no fault of
  the lab makes a split brain while the quorum holds.
- recover on a host that rebooted since the others went: its DRBD resources come up from
  the agent's files, with this host alone.

## Keys of a removed host

A cluster has no users: whoever holds the CLI's credentials controls it, as with
kubeadm's `admin.conf`. What a removed host keeps is the open point.

- A removed host keeps the data key, and with it the CA's key and every `!secret`
  value: a host that kept its disk can still issue certificates the revocations do not
  name, host certificates and CLI certificates after the cutoff alike. Rotating the data
  key, the CA and the cluster's age key (`cluster rotate-keys`, planned for phase 4)
  closes it. Until then, wipe a removed host's disk.
- The CLI checks an agent's certificate against the CA, not against the revocations, as
  kubectl does: a removed host could answer it as the cluster, with recipients of its own
  for `secret encrypt`. The agents refuse it, but only the CLI's `endpoints` keep the CLI
  from asking it, so remove a host's address from `endpoints` as the host goes. With the
  CA's key on such a host, no list the CLI pinned would help; `cluster rotate-keys`
  closes this too.
- Whoever holds the CLI's credentials can check a guessed value against a secret's
  digest; they can read the value through a privileged stack anyway.

## What the spike of phase 0 showed

On three HLab hosts, before any clowder code existed. The design rests on it.

- Published ports reach a container on another host through WireGuard with DNAT and no
  SNAT: the container sees the client's own address, over UDP and TCP, and no packet with
  the Floating IP as source leaves that host. After the Floating IP moves it works through
  the new ingress host; a running UDP flow follows a moved target only after
  `conntrack -D`.
- DRBD 9 with quorum majority and `suspend-io`: a host cut off from its peers suspends its
  writes; after it is powered off through the API, a peer promotes with a diskless
  tiebreaker and the data is intact. The fenced host boots with no resource up and resyncs
  as secondary. A copy moves to another pool while its host stays primary (detach,
  attach), a snapshot on the secondary with `fsfreeze` on the primary mounts with intact
  data, and a host whose peers are gone for good goes on alone after `del-peer`,
  `forget-peer` and `quorum=off`.
- Measured: the first sync of 4 GiB took 87 s at the resync limit, with a round-trip time
  over WireGuard of at most 112 ms meanwhile; 1 GiB of writes with protocol C raised it to
  11 ms at most.
- After a reboot Docker starts neither a `restart: "no"` container nor, in this setup, an
  `unless-stopped` one; a container whose volume is not mounted cannot start (the mount
  guard).
- The watchdog: a host whose systemd stops feeding it reboots after about 140 s. In HLab's
  VMs systemd fed the `iTCO_wdt` device, not softdog.
