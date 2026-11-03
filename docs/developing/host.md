# A host

Part of [DEVELOPING.md](../../DEVELOPING.md): the agent and the drivers that change its host.

## The agent (`internal/agent`)

- **Files.** A host's files are under `DefaultDir`: `host.json` (ID, name, addresses, ports),
  `net.json` (the WireGuard mesh it last applied), `keys/`, `pki/`, `raft/`, `stacks/`,
  `storage/`, `volume-records/`, `volumes/`. init and join write `host.json` last, so a
  host with it has everything else.
- **`clowderd init`** (from cloud-init) makes the first host: the CA, the data key, the
  cluster's age key, the host's certificate and WireGuard key, a Raft group with this host as
  its only voter and the bootstrap command. Then it enables and starts `clowderd.service`.
  It refuses a host that has a `raft/` directory. The bootstrap key comes from a file
  (`--bootstrap-key-file`; `clowder cluster bootstrap --key-file` alike), never from the
  command line, where `/proc` and shell histories keep it. `--public-address` is where users
  reach the API when the underlay is a private network; it goes into `host.json` and the
  state, like `--server-ref`, the hoster's ID of the server (for fencing). The host's
  name in the cluster is its hostname, by which DRBD names it in a resource. The
  bootstrap key has at least `pki.MinBootstrapKeyLength` characters.
- **`clowderd join`** (from cloud-init) makes a host a member of a cluster, with a join
  token from a file (`--token-file`) and an agent of the cluster as `--seed`; the other
  flags are init's. In this order:
  1. It makes the host's own key and WireGuard key and keeps them in `keys/join.json`.
  2. It trusts the cluster's CA by the token's proof and sends the join
     (`api.JoiningCluster.Join`). The answer goes into `join.json` at once.
  3. From `join.json` it writes the keys, the certificate and the data key (`writeKeys`,
     as init does), `clowder0`, and `net.json` with the other hosts as peers, then
     `host.json`, and deletes `join.json`.

  A join that fails after step 2 goes on from `join.json` the next time and asks the
  cluster nothing. A host whose answer never arrived is joining in the state, with its
  token used: `clowder host remove` it, and it joins anew with a new token. The leader
  adds the host to Raft as a non-voter at once, and moves it on once its agent runs and
  reports (The cluster, Raft). It refuses a host that has a `raft/` directory or `host.json`.
- **`clowderd user recovery-code <user>`** prints the codes that give the user access back,
  one for `user recover` and one for `recovery rotate`, each in its command (the code from
  a host, in [API and the CLI](cluster.md#api-internalapi-and-the-cli-internalcli)). It reads the data key only and changes
  nothing, so it works while the agent is down; it does not know the users, so a mistyped
  name gives codes that fit nowhere.
- **`clowderd run`** (the service) starts in this order:
  1. `clowder0` with the node address: a dummy interface, so the address exists on a host
     without peers. Then the WireGuard mesh of `net.json` (`restoreMesh`): Raft runs over
     the mesh, so after a reboot a host would learn its peers from a leader it cannot
     reach without them. The state's mesh replaces it once the state answers.
  2. The Raft member. It waits up to `catchUpTimeout` for its state to catch up with the
     Raft log. If the state met an entry or snapshot it cannot read by then, the agent
     ends here, before its loops start; a follower may meet it later (`CatchUp` does not
     wait for a follower to apply the leader's log, see the roadmap).
  3. The containers' DNS resolver.
  4. `newDaemon` makes the parts, each from what they all share (`shared`: the files,
     this host, its keys, the Raft member, the host's parts, the logger); then the API, once each on the underlay, the node
     address and the public address. It binds them with `IP_FREEBIND` (`freeBind`): at
     boot the public address may come up after the agent, which must neither fail nor
     lock users out then. Then the answers to ACME's challenges and the metrics. If any
     of them cannot listen, the agent ends before it started any container. Once they
     serve, only an API that fails ends the agent: without the other two the host still
     runs its stacks, so their failure is logged.
  5. The loops (`startLoops`): this host's that start no container (the network,
     supervisor), the leader's, which do nothing while it does not lead, and, once the
     state caught up (`runOnceCaughtUp`), the report, Traefik, the volume reconciler with
     its trim, the forgetting of removed hosts (`forgetPeers`: `storage.ForgetPeer`, then
     `report-peer-forgotten`), the prefetcher, the steps of moves (`moveSteps`), and the
     stack runner.
  6. It tells systemd `READY=1` and feeds the watchdog as long as the state's lock answers.

  When it stops, it ends its loops and waits for them, at most `loopsTimeout`: a host tool
  left behind on a suspended volume must not hold up the stop. Only once every loop ended do
  the resolver and the Raft member close (`stopLoops`). A loop left behind may still use
  them, so then they stay open and the agent exits with an error: the end of the process
  closes them, and systemd restarts the agent. Raft's log is on disk and survives that.

  The agent also stops when its Raft member halts at an entry it cannot read
  (`Node.Halted`, [Versions](cluster.md)). It then exits with status 3 (`exitUnreadable`
  in `cmd/clowderd`), which systemd does not restart (Shipping).
- `HostParts` are what the agent changes on its host (the network, the storage driver and
  Docker) and the hoster's API. `clowderd run` passes the real ones, the tests fakes.
- **Reconcilers** run after a change of the state and every `loop.Interval`, to repair
  what someone changed by hand: `reconcileNetwork` keeps the nft rules as the state says,
  the volume reconciler the volume copies, and the stack runner the stacks.
- **The volume reconciler** (`agent/volumes`, `volumes.Reconciler`) keeps every copy the
  state places on this host, whether or not a stack here uses it. Its target for a copy:
  primary and mounted when a stack placed here names the volume; left as it is while a
  stack's `volumes.json` lists its LV (`stacks.Files.HeldLVs`: a leaver whose
  `compose down` has not worked yet; every copy while a list does not read); otherwise
  unmounted and secondary, so a stack on another host can take the volume. A pass only
  reads: the state, the held LVs, and what the host has (`storage.Driver.Observe`: `lvs`
  per volume group, `drbdsetup status --json`, `findmnt`, once each for all copies).
  `plan` (pure, in `plan.go`) turns target and observation into steps: `ensure`,
  `promote` or `promote --force`, `mount` or `mount --format`, `set initialized`,
  `unmount`, `demote`. With the copy in place there are none, and the pass runs no tool
  for it. `--force` and `--format` only for a volume the state does not know as
  initialized ([Cluster state](cluster.md#cluster-state-internalstate), Initialized
  volumes); a plain copy whose LV is gone while the volume holds data, and another device
  at the mount point, are errors for a human.
  A tiebreaker (`VolumeCopy.Tiebreaker`, `storage.Volume.Diskless`) is the resource alone,
  never promoted or mounted. The pass tells the state when a copy with a disk becomes
  synced or stops being so (`set-copy-synced`, from the disk state `Observe` reads; a
  resource that is down says nothing), keeps what each copy has left to resync
  (`Progress`, for the leader and `/metrics`), and tells whether a copy is unmounted and
  secondary as wanted (`Released`, for a move). A stack that stops for a move
  (`State.StopsForMove`) names no copy here any more, so its copies go secondary.
  The steps of a volume run in a job, a goroutine of their own: mkfs, `mkdir data/`, a
  mount or an unmount write to the volume and hang while DRBD suspends I/O, and they must
  hold up neither the other volumes nor the agent. A volume has one job at a time
  (`busy`); the pass does not wait for it, and the job starts the next pass when it ends.
  `Run` waits for the jobs when it stops, within the agent's `loopsTimeout`. The stack
  runner asks `Mounted(lv)`: the mount point once the copy is ready, the error that keeps
  it from getting there (it is logged once, when it is new), or neither while it is on its
  way. A copy that becomes ready starts a pass of the stack runner. Retired copies are
  removed in a job too and forgotten ([Cluster state](cluster.md#cluster-state-internalstate), Retired copies), once no stack holds them. A copy
  the state neither places nor retires here stays, with a warning once per run. The pass
  is in `reconciler.go`, the targets in `desired.go`, the records in `records.go`, the
  trim in `trim.go`.
- **The stack runner** (`agent/stacks`, `stacks.Runner`) keeps everything of a stack under its project
  (`names.Project`). A pass first stops the stacks that left the host (`compose down`), and
  only then, for each stack the state places here, renders its Compose file with its
  mounted volume copies, writes its secrets, runs `compose up` unless all of it came up
  before, and reports `running`: a volume that passes from one stack to another is never
  used by both at once. A stack that fails to come up (any step, from its secrets to
  `compose up`, or a copy the reconciler cannot mount) is
  reported `failing` with the first line of the error (`reportFailing`, `Stack.UpError`),
  once, and tried again at every pass; the leader raises `stack-failing` if it lasts. A
  stack that waits for a leaver's volume, or for a copy on its way, is not failing. Before `compose up` the
  runner pulls the images of the services a user restarted since their containers came up:
  their `clowder.restarted` label differs from the state's time. An image may have changed
  under the same tag. Docker keeps the label, so a reboot pulls nothing again. The pass
  is in `runner.go`; one stack's steps in `apply.go`, the leavers in `leavers.go`. The
  agent hands the runner its parts (`stacks.Parts`): where its files are
  (`stacks.Files`), this host, the Raft member, Docker, the volume reconciler, the data key
  and the supervisor.
  A stack that stops for a move is a leaver here, as if it had left.
- **The prefetcher** (`stacks.Prefetcher`) pulls the images of the stacks this host may
  take in a failover or a move, before it needs them (`docker.Client.PullMissing`): those
  whose volumes with data all have a copy with a disk here, those without such volumes on
  any host that runs stacks, and the stack that moves here. One pull at a time, in a loop
  of its own, so a long pull holds up nothing else.
- **The steps of moves** (`agent/moves.go`, `moveSteps`) record a move's step that this
  host owns once its loops have done it: on the new host, each copy synced (or the volume
  never held data) and the images there; on the old host, the stack stopped
  (`stacks.Files.Stopped`) and each copy released.
- **A leaver that fails to stop** may still use its volumes, and no other stack may take
  them until it stopped: two databases on one file system would ruin it. Its volumes are
  the LVs in its `volumes.json` beside `compose.yaml`. Before `compose up` the runner adds
  the LVs of the new Compose file to it, as the containers of the last one keep theirs
  until `compose up` worked; after that, the file lists only the new ones. Without a
  readable file, the leaver counts as using every volume, and every stack waits a pass.
- **Crashes leave nothing unknown.** The files tell how far the runner and the reconciler
  got. A volume's record (`volume-records/`, one file per volume, apart from the stacks)
  lists its copies on this host before they are made, and loses one only after it is
  removed, so a copy is found whatever became of its stack; only its volume's job writes
  it. A stack's `compose.yaml` exists until `compose down`
  worked, its directory goes right after, and its secrets go once `compose.yaml` is gone.
- **Secrets live on tmpfs only** (`Files.SecretsDir`, `DefaultSecretsDir`). The agent makes
  the directory itself; a systemd `RuntimeDirectory` would go at each stop of the agent while
  the containers still use it.

  ```
  <SecretsDir>/<project>/      root only
    files/<name>               readable by all: bind-mounted to /run/secrets/<name>
    env.yaml                   root only: the secret variables, a second Compose file
  ```

  Before `compose up` the runner opens the sealed values and writes what is new or changed,
  by rename, so a running container keeps the old file until it is recreated. After a
  successful `compose up` it removes what the stack no longer asks for. The hash that decides
  about `compose up` covers the rendered file, `env.yaml` and the secret files; a changed
  secret file changes its services' digest label, which recreates them ([Rendering](specs.md#rendering-internalcompose), Labels).
- **After a boot no container runs** (`restart: "no"`). The runner remembers only in memory
  what each stack came up with, so its first pass runs `compose up` for every stack. A
  restart of Docker leaves the containers running (`live-restore` in `daemon.json`), but
  what dies meanwhile goes unseen. The supervisor notices the restart (its event stream
  breaks off) and starts a pass at once (`loop.Signal.DockerRestarted`). The runner stores
  Docker's restart count with each stack it brought up (`loop.BroughtUp`), so after a
  restart, even one during `compose up`, it runs `compose up` for every stack again, which
  starts what died and leaves the rest alone. The first pass waits
  until the state has caught up with the Raft log, so a host that was away does not start
  a stack that has since moved to another host.
- The agent also logs Docker in to the registries of `cluster.yaml`, once per login
  (`logIn`), runs the
  supervisor and, while it leads, the leader's loops: the placement
  (`controller.RunPlacement`) and those in [Ingress, certificates, alerts and
  metrics](loops.md). Every `trimInterval` it trims the copies the reconciler last saw
  mounted, each with its volume claimed like a job's, so no job unmounts it meanwhile;
  the driver skips a copy no longer mounted (fstrim on an empty mount point would trim the
  root file system).
- **The host's report** (`hostReporter`, `report-host`): version, memory, CPUs, the pools'
  data and metadata use, whether Docker answers a ping (`DockerDown`) and how full the root
  file system is: the inputs of the alerts. It runs in a loop of its own, after the state
  caught up, so a Docker that hangs cannot hold up the report that says so; each probe ends
  by its driver's timeout (Docker's ping, lvs). It probes every 30 s and when a new
  `cluster.yaml` may name a new pool, not at every change of the state: lvs on a host whose
  DRBD hangs is slow. A probe that fails keeps the value the state has: a pool
  whose `lvs` timed out does not vanish for the placement. The report commits only when a
  value changed, and every value changes seldom: a bool or a whole percent, rounded up, so a
  threshold trips early rather than late, and a small pool as early as a large one.
- **Tests.** The package's test goes the whole way on 127.0.0.1: init, the agent with a
  notify socket, the user's bootstrap, apply and diff, the Floating IP reaching the nft
  rules, and a volume and a stack applied, listed and deleted; `Run` that fails at its
  start leaves nothing open; `restart_test.go`: the supervisor's restart rules. The fakes
  of Docker, the storage driver (which keeps each copy's state in memory), the volume
  reconciler and a cluster in memory are in `agent/agenttest`; the
  cluster fails a test whose loop writes into its copy of the state
  (`statetest.ReadOnlyCopy`). In `agent/stacks`, `runner_test.go` runs single passes of the
  runner against that cluster, with a fake supervisor, fake Docker and a fake reconciler:
  a failed `compose up` reported `failing`, compose only while the supervisor is paused, a
  new data generation, a stack that waits for its copy and fails with its error, the LVs
  the stacks hold, secrets written, changed and removed, the label changing
  with a secret, the secrets the containers came up with reported. `leavers_test.go`: a
  volume passes to the next stack only once the old one stopped, and waits while a stop
  fails, while the leavers cannot be listed, and while a failed `compose up` leaves old
  containers on old volumes. `containers_test.go`: registry logins, `compose ps`.
  `secrets_test.go` runs the same way: a secret that does not open keeps its stack down,
  only changed secret files are replaced, and a stack's secrets go only once its
  containers stopped. In `agent/volumes`, `plan_test.go` is the table of target and
  observation (missing, down after a boot, secondary, primary, mounted, never initialized,
  held by a leaver, another device); `reconciler_test.go` runs passes and waits for their
  jobs against the cluster and the fake driver: a copy formatted once and then left
  alone, a copy without a stack secondary and unmounted and found again by the next stack
  without `--force`, a held copy left alone, a hanging job that holds up no other volume,
  an error that reaches the stack, a plain copy of data never made anew, a new data
  generation, a copy the state lost, removal once no stack holds it, the records, the
  weekly trim of mounted copies across restarts. That docker
  compose fails on a missing secret file and recreates exactly the containers that changed
  is for the live tests: CI has no Docker daemon.

## Network (`internal/network`)

- `clowder0` carries the node address and the resolver's address (`network.Resolver`).
- **The agent's network loop** (`agent/network.go`) reads the wanted network from the
  state (`readWantedNetwork`) and applies the nft rules first, then the WireGuard mesh, at
  every change and every loop interval. Every other host is a peer until it is removed;
  the addresses of all hosts stay closed to containers. It keeps the mesh it applied in
  `net.json`, which a joined host's first start and every start after a reboot apply
  before Raft (`restoreMesh`).
- **The WireGuard mesh** (`ApplyMesh`) has an interface per peer, `clw-<m>`, so each peer
  has a port, a route and a routing table of its own:
  - `clw-<m>` listens on `51800 + m` and reaches peer *m* at its underlay on
    `51800 + <own ID>`, with `AllowedIPs 0.0.0.0/0`: answers without SNAT carry a
    client's address, and the peers are the cluster's own hosts. Its configuration
    (`RenderWireGuard`, golden file `testdata/clw-2.golden.conf`, which `bin/dev
    test-tools` loads with `wg`) holds the private key, so it reaches `wg syncconf` on
    its standard input.
  - Its MTU is the underlay interface's less 80.
  - It carries the own node address as `/32 noprefixroute` (the reverse path filter
    drops what arrives on an interface without an IPv4 address) and the route to the
    peer's node address with the own one as `src`.
  - Table `100 + m` has `default dev clw-<m>`; the rule of priority `100 + m` sends
    packets marked `0x100 + m` (mask `0xfff`) there.
  - A former peer loses all three. First `conntrack -D --mark` forgets the connections
    that came from it, as its ID may come back with another host; should the agent stop
    halfway, the interface or the rule left brings the next pass back to it.
  At every pass it sets each peer's interface and routes anew (`wg syncconf` and `ip …
  replace` change only what differs) and replaces a rule that is not as wanted. A peer
  that fails holds up no other; the errors come together. Only `clw-<host ID>` is its
  own, and a rule whose priority and table are both `100 + m`. The tools run through
  `Host.Run`, so `wireguard_test.go` checks the commands against what the host has.
- **The table `inet clowder`** (`Render`, golden file `testdata/isolation.golden.nft`)
  keeps containers in their place:
  - `guard_in`: from a container bridge the host answers only the resolver, the local
    Traefik on the Floating IP (HTTP hairpin) and the container's own connections; a
    spoofed source goes first, as in `guard_fwd`.
  - `guard_fwd`, beyond the host, in this order: a packet whose source would not route
    back through the bridge it came from is dropped (`fib saddr . iif oif missing`), so a
    container with NET_RAW can neither spoof nor slip into another's connection (conntrack
    does not look at the interface); then established connections and what clowder or
    Docker redirected (`ct status dnat`: exports, hairpin, published ports) pass; the
    metadata API, `@cluster_hosts` (every host's underlay and public address, a removed
    host's too, as its server may still run), the private network (`private_network` in
    `cluster.yaml`, which must not overlap `clusterspec.ClowderRanges`) and the overlay are
    dropped. The rules are IPv4 only: Docker gives the bridges no IPv6 (`daemon.json`).
  - `guard_out`: the host's own processes, Traefik in the host's network included, do not
    reach the metadata API either; its user data holds the bootstrap key, and clowderd
    never needs it. cloud-init reads it at boot in its network stage, before
    `network-online.target` and so before `clowderd.service` starts, and the table does
    not survive a reboot. The resolver's upstreams come from `/etc/resolv.conf`, not from
    there. A known limit: a tool that reads the service later, such as Hetzner's
    `hc-utils` when a private network's interface is plugged in while the host runs,
    gets no answer; `examples/hetzner` attaches the network when it makes the server.
  - `marks`: a new connection that comes in through `clw-<m>` keeps the mark `0x100 + m`,
    and a container's answer gets it back, so the answer leaves through its peer's table.
    The host's own processes (Traefik) get none: what reaches them from a peer comes from
    its node address, and the route to it leads back. So a DNAT from another host that
    keeps the client's address must lead to a container.
  - A connection open before the table first loads (at the agent's start) passes as
    established until it ends.
  - `mss`: TCP through `clw-*` gets its MSS from the route's MTU; UDP must fit itself.
  - The drop rules count (`counter`), so `nft list table inet clowder` shows what they
    dropped since the table was last loaded (every loop interval).
  `ApplyRules` checks with `nft -c`, then replaces the table in one transaction; Docker's rules
  stay untouched.
- The resolver passes every question on to the host's resolvers (`/etc/resolv.conf`). Names
  of exported services come with exports.
- Published ports are bound on every host's Floating IP and node address
  ([Rendering](specs.md#rendering-internalcompose)). The DNAT that brings them to a stack
  on another host, and the conntrack cleanup when its target moves, join the ruleset with
  the ingress failover, where several hosts can test them.

## Storage (`internal/storage`)

- A copy of a volume is a thin LV in a pool of its storage class. With replicas, a DRBD 9
  resource sits on it (`RenderResource`, golden files in `testdata/` that `bin/dev
  test-tools` checks with `drbdadm`): quorum majority with `suspend-io`, `auto-promote no`,
  node-id = host ID, port `drbdBasePort` + minor on the node address. With replicas 0 it is a
  plain LV. A copy's LV is `names.VolumeName(volume ID, data generation)` and its resource
  and mount point (`volumes/<LV>`) have the same name, so a restored volume (a new
  generation) is a new LV next to the old one, mounted apart from it.
- **Each step is a method, and does only what is missing.** `Ensure` makes the LV and, with
  replicas, brings the resource up (`drbdadm adjust`, which also applies new peers); a new
  resource comes up secondary, and `Ensure` changes no role and mounts nothing. `Promote`
  (with `force` only for a volume that never held data) and `Mount` (ext4, the mount and
  `data/`; `format` only for such a volume) make a copy usable here; `Unmount` and `Demote`
  give it up. Which steps a copy needs, the volume reconciler decides from `Observe`, which
  only reads: the volume groups' LVs, the roles of all resources (`drbdsetup status
  --json`; a resource that is down is not listed), the mount table, and whether the
  resource file is current. Markers in the driver's `StateDir` let `Ensure` pick up after a
  crash at any point (`created`: clowder made the LV; `metadata`: DRBD metadata written).
A tiebreaker (`Volume.Diskless`) has no LV and no metadata: `Ensure` writes
  its resource file and runs `adjust`, `Remove` takes it down. When a resource's file loses
  a peer (a copy went), `Ensure` notes the peer first (the marker `<resource>.forget`) and
  forgets it after the `adjust` (`ForgetPeerOf`), so the next copy finds a free slot in the
  metadata (`maxPeers`). `Observe` also reads each copy's disk state and how much it
  differs from its peers (`Observed.Disk`, `OutOfSyncBytes`, the latter only toward the peers it resyncs from).
  `ForgetPeer` forgets a removed host on every resource (`drbdsetup forget-peer`), so
  its node-id can serve a new host. It fails, and the agent tries again, while a resource
  is down or still has the peer configured (its `adjust` takes it out), and for any
  refusal but `unknownPeer`: a resource that never knew the peer has nothing to forget.
  Whether a volume holds data is no marker: the state knows, for every host
  (`Volume.Initialized`). The driver refuses an LV it did not create and never wrote
  metadata on or formatted (plain or replicated), a device with another file system, and a
  device without one when the volume holds data: those it leaves to a human. A crash
  between `lvcreate` and its marker leaves an empty LV that a human must look at.
- **It never guesses.** Only a tool's own answer means "not there": `blkid`'s exit code 2,
  `findmnt`'s 1, and for an LV its name missing from the volume group's list (`lvs` has no
  exit code of its own for that). Any other failure, a timeout or a device not ready, stops
  the step, and a table that does not read stops `Observe`, so a guess never creates over
  or formats real data. A mount point with another device mounted stops it too: that data
  would reach the containers as the volume's.
- **The mount guard:** containers get `data/`, which exists only in the mounted file system,
  so a container whose volume is not mounted fails to start instead of writing to the root
  file system.
- `Remove` picks up after a crash at any point: a resource without its file is down
  already, an LV that `lvs` does not find is gone already. It takes a resource down only once
  no connected peer is primary (`checkNoPrimaryPeer`): a primary with two copies would
  lose its quorum and suspend its stack's writes. The primary goes first, or drops the
  copy from its resource.
- `PoolUsage` reads a pool's size and use; `Trim` (fstrim) gives the deleted blocks of
  mounted copies back.

## Containers (`internal/docker`, `internal/supervisor`)

- `docker.Client` talks to Docker's API on its socket for events, start and restart, and
  runs `docker compose` and `docker login` through `internal/command`. `up` gets the
  rendered file and, for a stack with secret variables, `env.yaml`; down, ps and logs need
  the rendered file alone. `compose down` removes the stack's Docker volumes too; they only
  bind a directory of clowder's volume, whose data stays until clowder removes the copy.
- **Docker restarts nothing** (`restart: "no"`), so that after a boot nothing starts before
  the agent has looked at the cluster. The supervisor follows Docker's events and applies
  each service's restart rule with `DefaultBackoff`: the first restart at once, so a
  crashed service runs again within 2 s, then 1 s, doubling up to a minute; `unhealthy`
  means a restart with the same backoff unless the service says otherwise.
- **A stop by hand is kept:** `docker stop`, or a kill with a signal that ends the container
  (`endsContainer`). A kill with another signal, such as a reload with SIGHUP, is no stop.
  While the agent runs `compose up` or `down` for a stack, it `Pause`s the supervisor for
  the stack's project, so the supervisor does not fight it.
- A start that fails counts as another crash; a container that is gone
  (`docker.ErrNoContainer`) is let be. `destroy` events drop what the supervisor knew about a
  container. When the events break off, what died in between went unseen (Docker
  restarted; its containers keep running): the supervisor tells the agent, which then runs
  `compose up` for every stack as after a boot.

## Host check (`internal/hostcheck`, `clowderd check`)

Checks the operator base and changes nothing: sysctls, time sync, watchdog, WireGuard, DRBD 9
built for the running kernel, drbd-utils, DRBD autostart and usage report off, LVM filter,
thin pools (`--pool`), Docker's `daemon.json`, docker compose, unattended-upgrades and the
Floating IP (`--floating-ip`). Each check reads through the `Host` interface, so the unit
tests use a fake host; exit status 1 if any check fails. A command that hangs (`lvs` while
DRBD suspends I/O) is left behind after `commandTimeout`: fine for `clowderd check`, which
exits; the agent will run the checks in their own process for the same reason ([Volumes](../../DEVELOPING.md#volumes)).
