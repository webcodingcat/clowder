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
| 2 | three hosts: replication, failover with fencing | started: the live tests, the volume reconciler, versions |
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

Before it, the first release became installable and the commands honest: the APT
repository keeps every version, `examples/hetzner` pins `0.1.0`, `bin/dev live release
<tag>` tests a published release (the [release checklist](developing/shipping.md#release-checklist)),
an upgrade restarts the agent, `host remove` asks first and keeps the last host that runs
stacks, `host drain` says that it moves nothing, and a cluster has one user. `v0.1.0` is
out; its release candidate passed the live tests without the test hook.

Code without a caller went then; each phase brings back what it needs:

- phase 2: the join (a command that adds a host, Raft members added at run time),
  fencing (powering servers off and on through the hoster), operations with locks on
  stacks and volumes, the hosts' own phase reports and `host enable` after the flapping brake;
- phase 3: `backup:` in `cluster.yaml`, and resources owned by a GitOps source;
- user management: more users per cluster, removing a user, revoking one certificate.

Until `v1.0.0`, the state and `cluster.yaml` may change between releases without a way
to upgrade a running cluster.

## Known gaps to close in phase 2

They matter once there are several hosts; on one, they cost at most time.

- Raft and the forwarding of writes run on the node address, which other hosts reach only
  through the WireGuard mesh.
- `CatchUp` makes sure the state holds the whole log only on the leader; a follower returns
  once it knows a leader. Before a host that was away starts its stacks, it must have
  applied the leader's commit index.
- `CatchUp` runs once, when the agent starts. A host that becomes leader later may not
  have applied every committed entry yet, and its loops and `runOnLeader` would act on that
  state. It needs a barrier when it gains the lead, before they act.
- The agent runs its stacks one after another in one loop. A long image pull holds up
  the others, and a `compose` command left behind on a suspended DRBD volume leaves a
  goroutine behind at every pass.
- A placement is several commands in a row (push-outs, volumes, the stack's host). A new
  leader repairs what one left half done in its next pass; with failover, one command per
  placement would leave fewer states between.
- A stack that moves to another host takes its volume once the old host's copy is
  secondary. Until `move-stack` orders that, the new host's `promote` fails while the
  old copy is still primary, and the stack is `failing` until a later pass promotes.
- A tiebreaker copy on a host (diskless) is refused with an error: the placement over
  hosts brings it.
- A stack whose placed volumes sit on different hosts (left by deleted stacks) waits; on
  one host it cannot happen.
- The ACME self-check asks the Floating IP, which reaches the leader's own Traefik; with
  several hosts it must ask the ingress host instead
  ([Certificates of the routes](developing/loops.md#certificates-of-the-routes-controlleracmego)).
- A Prometheus in a stack or outside the cluster cannot reach `/metrics` yet: only the
  hosts reach the node address. The way in (through Traefik, or a permission for certain
  stacks) comes when one is needed.
- A host that falls silent raises no alert: `docker-down` and the pools come from the
  host's own report, which a dead host does not send. On one host the heartbeat covers it;
  with several, the leader needs an alert for a host whose reports stop.
- `TestBoundaries` checks one host. With several, containers must not reach the other
  hosts' underlay either, and a packet with a spoofed source must be dropped.
- `examples/hetzner` ignores later changes to the server's user data, so a storage class
  added later gets a volume but no thin pool on an existing host.

## Before users get permissions

A cluster has one user so far, and it may do everything.

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
