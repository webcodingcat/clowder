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
| 2 | three hosts: replication, failover with fencing | |
| 3 | backup and restore, GitOps | |
| 4 | comfort and operations | |

Phase 1d replaced SOPS with values of their own: tagged `!secret` in `cluster.yaml` and in
Secrets, encrypted for the cluster and the recovery key (`secret encrypt`, `rekey`,
`decrypt`), with the credentials in the user's config directory and access back through a
code from a host (`user recover`, `recovery rotate`). Phase 2 is next: three hosts.

Until `v1.0.0`, the state and `cluster.yaml` may change between releases without a way
to upgrade a running cluster.

## Known gaps to close in phase 2

They do no harm on a single host.

- Raft and the forwarding of writes run on the node address, which other hosts reach only
  through the WireGuard mesh.
- `CatchUp` makes sure the state holds the whole log only on the leader; a follower returns
  once it knows a leader. Before a host that was away starts its stacks, it must have
  applied the leader's commit index.
- `CatchUp` runs once, when the agent starts. A host that becomes leader later may not
  have applied every committed entry yet, and its loops and `decide` would act on that
  state. It needs a barrier when it gains the lead, before they act.
- The agent runs its stacks one after another in one loop. A long image pull or `mkfs`
  holds up the others, and a command left behind on a suspended DRBD volume leaves a
  goroutine behind at every pass. It also runs `Ensure` on every volume at every pass,
  which it should skip when nothing changed.
- A placement is several commands in a row (push-outs, volumes, the stack's host). A new
  leader repairs what one left half done in its next pass; with failover, one command per
  placement would leave fewer states between.
- `State.Revoked` keeps every revoked serial; with many hosts and users it should drop
  those of certificates long expired.
- `storage.Ensure` promotes a resource with `--force` when this host has no `primary`
  marker. That is right for a new volume on one host; after a failover it would force an
  outdated copy. Whether a volume was ever initialised must come from the state, not a
  local file.
- A stack whose `compose up` fails after a reboot or a restart of Docker stays `running` in
  the state; only the agent's log says so. The host should report it.
- A volume's copy that no stack on its host uses stays mounted, and with replicas its DRBD
  resource stays primary: on one host the next stack that names it takes it over once the
  old one stopped. With several hosts a copy without a stack must be unmounted and
  secondary, so a stack on another host can take the volume.
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
- `examples/hetzner` ignores later changes to the server's user data, so a storage class
  added later gets a volume but no thin pool on an existing host.

## Before users get permissions

Users have none yet: every user may do everything.

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
