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
| 3 | backup and restore, GitOps | |
| 4 | comfort and operations | |

Phase 1d replaced SOPS with values of their own: tagged `!secret` in `cluster.yaml` and in
Secrets, encrypted for the cluster and the recovery key (`secret encrypt`, `rekey`,
`decrypt`), with the credentials in the user's config directory and access back through a
code from a host (`user recover`, `recovery rotate`).

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
- The leader asks the hoster whether a joining host's server is its own: the server has
  the host's underlay as an address, and no server serves two hosts. It checks the first
  host's server too, once `cluster.yaml` brings the hoster's token. Only a host with a
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
  server off through the hoster, and only then do its stacks start elsewhere, on a host
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
  its own. The hoster's API is asked again while it is busy.
- The live tests fail hosts through HLab's faults: frozen, cut off, with the hoster's API
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
  off through the hoster, labels their servers `clowder-retired` with their certificate's
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

- phase 3: `backup:` in `cluster.yaml`, and resources owned by a GitOps source;
- user management: more users per cluster, removing a user, revoking one certificate.

Until `v1.0.0`, the state and `cluster.yaml` may change between releases without a way
to upgrade a running cluster.

## Known gaps

What phase 2 left open, sorted by the phase that closes it.

Phase 3 (backup and restore):

- A volume that waits for a restore after a recover waits until phase 3 brings restores;
  until then, deleting it and applying it anew gives an empty one. An unsynced copy left
  on the host that recovered stays as it is, for a human.

Phase 4 (comfort and operations):

- A host that comes back after a fence resyncs all its copies at once, each at the
  resync limit's rate; the limit holds back only new copies meanwhile.
- A failed host that is not fenced (it needs none, or there is no hoster, a server ref not
  confirmed, the hoster's API down for good) keeps its copies counted: none is made anew
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
- A retired host whose agent starts while the hoster's API does not answer starts as
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

## Before users get permissions

A cluster has one user so far, and it may do everything.

- A removed host keeps the data key, and with it the CA's key and every `!secret`
  value: a host that kept its disk can still issue certificates the revocations do not
  name. Rotating the data key, the CA and the cluster's age key (`cluster rotate-keys`)
  comes before users get permissions. Until then, wipe a removed host's disk.
- The CLI checks an agent's certificate against the CA, not against the revocations: a
  removed host that kept its key could answer it as the cluster, with recipients of its
  own for `secret encrypt`. The CLI needs the revocation list or the cluster's recipients
  pinned in the credentials.
- The leader trusts the actor a host names for a user's command it forwards
  (`/v1/internal/propose`, `/v1/internal/changes`). That holds while root on any host has
  every key anyway.
- The state already asks `Authorize` on the leader for every change of a resource; with
  permissions it reads the cluster's role bindings there.
- Whoever may use the API can check a guessed value against a secret's digest. Until there
  are permissions that is every user, who can read the value through a privileged stack
  anyway.

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
