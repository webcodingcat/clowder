# A host

Part of [DEVELOPING.md](../../DEVELOPING.md): the agent and the drivers that change its host.

## The agent (`internal/agent`)

- **Files.** A host's files are under `DefaultDir`: `host.json` (ID, name, addresses, ports),
  `net.json` (the WireGuard mesh the host last applied), `keys/`, `pki/`, `raft/`,
  `stacks/`, `storage/`, `volume-records/` and `volumes/`. init and join write `host.json`
  last, so a host that has it has everything else.
- **`clowderd init`** (run from cloud-init) creates the first host. It creates the CA, the
  data key, the cluster's sealing key, the host's certificate and WireGuard key, and a Raft
  group with this host as its only voter and the bootstrap command. The bootstrap command
  carries the cluster's name (`--cluster-name`, or `DefaultClusterName` without the flag;
  it must pass `names.Valid`), and the CA is named after it. Then init enables and starts
  `clowderd.service`. It refuses a host that has a `raft/` directory. `--roles` gives the
  host's roles (`hostrole.DefaultInit`, all three, without the flag); the first host needs
  `control-plane`. Three flags are required, and each goes into `host.json` and the state:
  - `--underlay`: the server's address in the Hetzner private network;
  - `--public-address`: its public IPv4, where users reach the API;
  - `--server-id`: its Hetzner Cloud ID (for the Floating IP and fencing).

  The host's name in the cluster is its hostname, which DRBD uses to name it in a
  resource. The first credentials come from `clowderd credentials` afterwards; init takes
  no key for them.
- **`clowderd join`** (run from cloud-init) makes a host a member of a cluster. It reads a
  join token from a file (`--token-file`) and joins through the cluster agent named by
  `--via`; the other flags are the same as init's, but `--roles` is
  `hostrole.DefaultJoin` (`worker,ingress`) without the flag, and the token must allow
  the roles. It works in this order:
  1. It creates the host's own key and WireGuard key and keeps them in `keys/join.json`.
  2. It trusts the cluster's CA through the token's proof and sends the join
     (`api.JoiningCluster.Join`). The answer goes into `join.json` immediately. Its
     certificate must name the roles the host asked for (`checkJoinCertificate`).
  3. From `join.json` it writes the keys, the certificate and the data key (`writeKeys`,
     as init does), `clowder0`, and `net.json` with the other hosts as peers. Then it
     writes `host.json` and deletes `join.json`.

  A join that fails after step 2 continues from `join.json` the next time and does not ask
  the cluster again. A host whose answer never arrived is shown as joining in the state,
  with its token used up. Remove it with `clowder host remove`, and it joins anew with a
  new token. The leader adds the host to Raft as a non-voter at once, and promotes it once
  its agent runs and reports (The cluster, Raft). join refuses a host that has a `raft/`
  directory or `host.json`.
- **`clowderd credentials [--name <name>] [--endpoint <address>]... [--revoke-older]`**
  (`agent/credentials.go`) prints a CLI config with one context, as `kubeadm kubeconfig
  user` does. It creates the key locally and asks its own agent for the certificate, using
  the host's certificate (`/v1/internal/credentials`, in [API and the
  CLI](cluster.md#credentials-from-a-host)). The agent must be running; `--revoke-older`
  also needs the cluster's leader. Like `clowderd sealing-key export` and `clowderd
  recover`, it runs only on a control-plane host: it reads the roles from `host.json`
  (`Host.checkControlPlane`), which the join wrote from its certificate.
- **`clowderd recover --power-off`** makes this host the only one in its cluster when the
  others are gone for good. It powers them off and labels their servers `clowder-retired`
  ([Recover](cluster.md#recover-clowderd-recover-agentrecovergo)). It stops the agent while
  it works on Raft's files and starts it again when it is done.
- **`clowderd run`** (the service) starts in this order:
  1. It reads the state that its Raft log on disk holds (`raftnode.ReadState`) and checks
     that `host.json`, the host's certificate and that state name the same roles
     (`Host.checkRoles`; a state that does not know the host yet is not compared). The API
     checks callers by their certificates, while the loops read `host.json` and the state,
     so the agent does not start while they differ; they differ only after a change by
     hand.
  2. It asks Hetzner whether `clowderd recover` retired this host (`checkRetired`). A host
     is retired when the label `clowder-retired` of its server names the host's
     certificate ([Recover](cluster.md#recover-clowderd-recover-agentrecovergo)). The
     agent asks with the token from that state on disk. A retired server's agent ends with `ErrRetired`, exit status
     4, before it starts Raft, the mesh or any stack, because two retired hosts would
     still hold the old cluster's majority. A cluster without a ClusterConfiguration has
     no token to ask with. A host whose API call does not return within
     `retiredCheckTimeout` starts as before.
  3. It sets up `clowder0` with the node address. This is a dummy interface, so the
     address exists even on a host without peers. Then it restores the WireGuard mesh from
     `net.json` (`restoreMesh`). Raft runs over the mesh, so after a reboot a host would
     otherwise have to learn its peers from a leader that it cannot reach without them.
     The mesh from the state replaces it once the state answers.
  4. It starts the Raft member, which waits up to `catchUpTimeout` for its state to catch
     up with the Raft log. If the state has met an entry or snapshot it cannot read by
     then, the agent ends here, before its loops start. A follower may meet such an entry
     later, as it applies the leader's log.
  5. It starts the containers' DNS resolver.
  6. `newDaemon` creates the parts, each from what they all share (`shared`: the files,
     this host, its keys, the Raft member, the host's parts, the logger). Then it starts
     the API, once each on the underlay, the node address and, on a control-plane host,
     the public address (`Host.apiAddresses`). A host without the control plane serves
     only the control plane, which reaches it over the private network. It
     binds them with `IP_FREEBIND` (`freeBind`), because at boot the public address may
     come up after the agent, and the agent must neither fail nor lock users out then.
     Then it starts the server for ACME's challenges and the metrics. If any of them cannot
     listen, the agent ends before it has started any container. Once they serve, only a
     failing API ends the agent. Without the other two the host still runs its stacks, so
     their failure is only logged.
  7. It starts the loops (`startLoops`):
     - this host's loops that start no container: the network, the supervisor, and the
       beats to the voters;
     - the leader's loops, which do nothing while this host does not lead;
     - once the state has caught up with the leader's commit index (`runOnceCaughtUp`,
       `waitForState`; cluster.md, Raft): the report, Traefik, the volume reconciler with
       its trim, the forgetting of removed hosts (`forgetPeers`: `storage.ForgetPeer`, then
       `report-peer-forgotten`), the prefetcher, the steps of moves (`moveSteps`), the
       report of a host that is back after a fence (`reportRecovery`), and the stack
       runner. They wait because a host that was away must not start a stack that moved
       in the meantime.
  8. It tells systemd `READY=1` and feeds the watchdog as long as the state's lock
     answers.

  When the agent stops, it ends its loops and waits for them, at most `loopsTimeout`,
  because a host tool left behind on a suspended volume must not hold up the stop. The
  resolver and the Raft member close only once every loop has ended (`stopLoops`). A loop
  left behind may still use them, so in that case they stay open and the agent exits with
  an error. The end of the process closes them, and systemd restarts the agent. Raft's log
  is on disk and survives that.

  The agent also stops when its Raft member halts at an entry it cannot read
  (`Node.Halted`, [Versions](cluster.md)). It then exits with status 3 (`exitUnreadable`
  in `cmd/clowderd`), which systemd does not restart (Shipping). The same applies to
  status 4 of a retired server.
- `HostParts` are what the agent changes on its host: the network, the storage driver and
  Docker. `clowderd run` passes the real ones, and the tests pass fakes. The tests fake
  Hetzner's API with `hetzner/hetznertest`, named in the configuration's `hetzner.api`.
- **Reconcilers** run after each change of the state and every `loop.Interval`, so they
  repair what someone changed by hand. `reconcileNetwork` keeps the nft rules as the state
  says, the volume reconciler keeps the volume copies, and the stack runner keeps the
  stacks.
- **The volume reconciler** (`agent/volumes`, `volumes.Reconciler`) keeps every copy that
  the state places on this host, whether or not a stack here uses it. Its target for a
  copy is:
  - primary and mounted, when a stack placed here names the volume;
  - left as it is, while a stack's `volumes.json` lists its LV (`stacks.Files.HeldLVs`).
    This covers a leaver whose `compose down` has not worked yet, and every copy while a
    list cannot be read;
  - otherwise unmounted and secondary, so a stack on another host can take the volume.

  A pass only reads: the state, the held LVs, and what the host has
  (`storage.Driver.Observe`: `lvs` per volume group, `drbdsetup status --json` and
  `findmnt`, each once for all copies). `plan` (pure, in `plan.go`) turns target and
  observation into steps: `ensure`, `promote` or `promote --force`, `mount` or `mount
  --format`, `set initialized`, `unmount`, `demote`. When the copy is already as wanted
  there are no steps, and the pass runs no tool for it. `--force` and `--format` are used
  only for a volume that the state does not know as initialized ([Cluster
  state](cluster.md#cluster-state-internalstate), Initialized volumes). Two cases are
  errors for a human: a plain copy whose LV is gone while the volume holds data, and
  another device at the mount point.

  A tiebreaker (`VolumeCopy.Tiebreaker`, `storage.Volume.Diskless`) is only the resource;
  it is never promoted or mounted. The pass tells the state when a copy with a disk
  becomes synced or stops being synced (`set-copy-synced`, from the disk state that
  `Observe` reads; a resource that is down reports nothing). It keeps how much each copy
  has left to resync (`Progress`, for the leader and `/metrics`), and whether a copy is
  unmounted and secondary as wanted (`Released`, for a move). A stack that stops for a move
  (`State.StopsForMove`) no longer names any copy here, so its copies go secondary.

  The steps of a volume run in a job, a goroutine of its own. mkfs, `mkdir data/`, a mount
  or an unmount write to the volume and hang while DRBD suspends I/O, and they must hold up
  neither the other volumes nor the agent. A volume has one job at a time (`busy`). The
  pass does not wait for it, and a job that succeeded starts the next pass when it ends. A
  job that failed leaves the retry to the next change of the state or `loop.Interval`,
  because a step that keeps failing would spin if it were retried at once. `Run` waits for
  the jobs when it stops, within the agent's `loopsTimeout`.

  The stack runner asks `Mounted(lv)`. It returns the mount point once the copy is ready,
  the error that keeps the copy from getting there (logged once, when it is new), or
  neither while the copy is on its way. A copy that becomes ready starts a pass of the
  stack runner. Retired copies are also removed in a job, and then forgotten ([Cluster
  state](cluster.md#cluster-state-internalstate), Retired copies), once no stack holds
  them. A host takes a copy down only once no primary peer is connected to it
  (`storage` `checkNoPrimaryPeer`, `ErrNotYet`): with quorum majority, the primary of a
  volume with two copies would otherwise lose its quorum. A copy that the state neither places nor retires here stays, with a warning once
  per run. The pass is in `reconciler.go`, the targets in `desired.go`, the records in
  `records.go`, and the trim in `trim.go`.
- **The stack runner** (`agent/stacks`, `stacks.Runner`) keeps everything that belongs to a
  stack under its project (`names.Project`). A pass first stops the stacks that left the
  host (`compose down`). Only then does it start a job for each stack that the state places
  here and that has no job running (`Runner.start`), so one stack's long image pull, or a
  `compose up` that hangs on a suspended volume, holds up no other stack. A pass that finds
  a stack's job still running leaves it alone, and the job starts another pass when it
  ends. A leaver whose job is still running is stopped at a later pass, and its volumes
  count as in use until then.

  The job renders the stack's Compose file with its mounted volume copies, writes its
  secrets, runs `compose up` unless all of it came up before, and reports `running`. So a
  volume that passes from one stack to another is never used by both at once. A stack that
  fails to come up is reported `failing` with the first line of the error
  (`reportFailing`, `Stack.UpError`), once, and is tried again at every pass. A failure can
  come from any step, from its secrets to `compose up`, or from a copy that the reconciler
  cannot mount. The leader raises `stack-failing` if it lasts. A stack that waits for a
  leaver's volume, or for a copy on its way, is not failing.

  Before `compose up`, the runner pulls the images of the services that a user restarted
  since their containers came up: their `clowder.restarted` label differs from the time in
  the state. It pulls because an image may have changed under the same tag. Docker keeps
  the label, so a reboot does not pull again. The pass is in `runner.go`, one stack's steps
  in `apply.go`, and the leavers in `leavers.go`. The agent hands the runner its parts
  (`stacks.Parts`): where its files are (`stacks.Files`), this host, the Raft member,
  Docker, the volume reconciler, the data key and the supervisor.

  A stack that stops for a move counts as a leaver here, as if it had left. A host that is
  not `state.KeepsStacks` (fenced, recovering, disabled) runs no stacks, because its stacks
  may be running elsewhere by then.
- **The prefetcher** (`stacks.Prefetcher`) pulls the images of the stacks that this host may
  take in a failover or a move, before it needs them (`docker.Client.PullMissing`). These
  are the stacks whose volumes with data all have a copy with a disk here, the stacks
  without such volumes on any host that runs stacks, and the stack that moves here. It
  pulls one image at a time, in its own loop, so a long pull holds up nothing else.
- **The steps of moves** (`agent/moves.go`, `moveSteps`) record each step of a move that
  this host owns, once its loops have done it. On the new host, the steps are each copy
  synced (or the volume never held data) and the images present. On the old host, they are
  the stack stopped (`stacks.Files.Stopped`) and each copy released.
- **A leaver that fails to stop** may still use its volumes, and no other stack may take
  them until it has stopped: two databases on one file system would ruin it. Its volumes
  are the LVs in its `volumes.json`, next to `compose.yaml`. Before `compose up`, the runner
  adds the LVs of the new Compose file to it, because the containers of the previous file
  keep their volumes until `compose up` has worked. After that, the file lists only the new
  LVs. Without a readable file, the leaver counts as using every volume, and every stack
  waits a pass.
- **Crashes leave nothing unknown.** The files show how far the runner and the reconciler
  got. A volume's record (`volume-records/`, one file per volume, separate from the stacks)
  lists its copies on this host before they are created, and drops a copy only after it is
  removed. So a copy is found whatever became of its stack. Only the volume's job writes
  the record. A stack's `compose.yaml` exists until `compose down` has worked, its
  directory is removed right after, and its secrets are removed once `compose.yaml` is
  gone.
- **Secrets live on tmpfs only** (`Files.SecretsDir`, `DefaultSecretsDir`). The agent creates
  the directory itself, because a systemd `RuntimeDirectory` would be removed at each stop
  of the agent while the containers still use it.

  ```
  <SecretsDir>/<project>/                      root only
    files/<secret>/<key>                       readable by all: bind-mounted to /run/secrets/<secret>/<key>
    env.yaml                                   root only: the secret variables, a second Compose file
  <SecretsDir>/clowder-registries/<namespace>/ root only
    config.json                                root only: the namespace's registry logins
  ```

  A service's `secrets: [db]` gives it every key of the Secret `db` as a file, the way
  Kubernetes mounts a secret. Its `secretEnv: {VAR: {name, key}}` sets a variable to one
  key's value, in `env.yaml`. The registries' directory starts with
  `names.ReservedPrefix`, so the clean-up of the stacks' secrets (`removeSecretsOfOthers`)
  leaves it alone.

  At every pass the runner decrypts the values and writes what is new or changed, by
  rename, so a running container keeps the old file until it is recreated. Unchanged files
  and directories get their owner and mode reset (`atomicfile.EnsureDir`,
  `atomicfile.WriteChanged`), so rights changed by hand or left over from before do not
  persist. The agent does the same for `SecretsDir` at its start. A stack without secrets
  has nothing there to guard. After a successful `compose up`, the runner removes what the
  stack no longer asks for. The hash that decides about `compose up` covers the rendered
  file, `env.yaml` and the secret files. A changed secret file changes its services'
  digest label, which recreates them ([Rendering](specs.md#rendering-internalcompose),
  Labels).
- **After a boot no container runs** (`restart: "no"`). The runner remembers only in memory
  what each stack came up with, so its first pass runs `compose up` for every stack. A
  restart of Docker leaves the containers running (`live-restore` in `daemon.json`), but
  any container that dies meanwhile goes unseen. The supervisor notices the restart (its
  event stream breaks off) and starts a pass at once (`loop.Signal.DockerRestarted`). The
  runner stores Docker's restart count with each stack it brought up (`loop.BroughtUp`).
  So after a restart, even one during `compose up`, it runs `compose up` for every stack
  again, which starts what died and leaves the rest alone. The first pass waits until the
  state has caught up with the Raft log, so a host that was away does not start a stack
  that has since moved to another host.
- **Registry logins belong to a namespace**, just as Kubernetes pulls with the
  `imagePullSecrets` of a namespace's default service account. Before a stack's `compose
  up` or pull, and before the prefetcher's pull, the agent writes the Docker config of the
  stack's namespace (`RegistryConfigs.Write`). The runner and the prefetcher share this one
  value, so its lock keeps their writes apart. The config holds the opened logins of the
  namespace's image pull secrets (`state.RegistryLogins`), written by rename into the
  registries' directory on the secrets' tmpfs, and Docker pulls with it (`docker
  --config`). A namespace without logins gets an empty config, so no stack pulls with
  root's own logins or another namespace's. Like the kubelet, the agent leaves out a
  missing Secret or key and pulls without it. An image that needs no login is still
  pulled, and a pull that fails names what was left out, in `get stacks`. The runner
  removes the configs of namespaces that have no stack running here or that this host may
  take (`removeRegistryConfigsOfOthers`), so their logins stay on the host no longer than
  needed.
- The agent also runs the supervisor and, while it leads, the leader's loops: the placement
  (`controller.RunPlacement`) and those in [Ingress, certificates, alerts and
  metrics](loops.md). Every `trimInterval` it trims the copies that the reconciler last saw
  mounted. Each copy's volume is claimed as a job's would be, so no job unmounts it
  meanwhile. The driver skips a copy that is no longer mounted, because fstrim on an empty
  mount point would trim the root file system. fstrim writes to the volume, so it trims
  only a copy that has DRBD's quorum (or a plain copy). The others wait for the next round,
  an hour later, and so does the whole trim until every mounted copy has been trimmed (the
  exception in [Volumes](../../DEVELOPING.md#volumes)).
- **The host's report** (`hostReporter`, `report-host`) carries the inputs of the alerts:
  - version, memory and CPUs;
  - the pools' data and metadata use;
  - whether Docker answers a ping (`DockerDown`), and how full the root file system is;
  - the volumes whose copy here has split from a peer (`SplitBrain`) or is primary without
    DRBD's quorum (`NoQuorum`), from the reconciler's last pass (`Seen`).

  It runs in its own loop, after the state has caught up, so a Docker that hangs cannot
  hold up the report that says so. Each probe ends by its driver's timeout (Docker's ping,
  lvs). It probes every 30 s, and when a new generation of the configuration may name a
  new pool, but not at every change of the state, because lvs is slow on a host whose DRBD
  hangs. A probe that fails keeps the value that the state has, so a pool whose `lvs` timed
  out does not vanish for the placement. The report commits only when a value changed, and
  every value changes seldom: each is a bool or a whole percent. Percents are rounded up,
  so a threshold trips early rather than late, and a small pool trips as early as a large
  one.
- **Tests.**
  - The package's own test runs the whole way on 127.0.0.1: init, the agent with a notify
    socket, the user's first credentials from the host, apply and diff, the Floating IP
    reaching the nft rules, and a volume and a stack applied, listed and deleted. It also
    checks that a `Run` that fails at its start leaves nothing open. `restart_test.go`
    covers the supervisor's restart rules.
  - `agent/agenttest` has the fakes of Docker, the storage driver (which keeps each copy's
    state in memory), the volume reconciler and a cluster in memory. The cluster fails a
    test whose loop writes into its copy of the state (`statetest.ReadOnlyCopy`).
  - In `agent/stacks`, `runner_test.go` runs single passes of the runner against that
    cluster, with a fake supervisor, fake Docker and a fake reconciler. It covers a failed
    `compose up` reported as `failing`, compose run only while the supervisor is paused, a
    new data generation, a stack that waits for its copy and fails with its error, the LVs
    the stacks hold, secrets written, changed and removed, the label changing with a
    secret, and the reported secrets the containers came up with.
  - `leavers_test.go` checks that a volume passes to the next stack only once the old one
    has stopped, and that it waits while a stop fails, while the leavers cannot be listed,
    and while a failed `compose up` leaves old containers on old volumes.
    `containers_test.go` covers the namespaces' Docker configs and `compose ps`.
  - `secrets_test.go` runs the same way: a secret that does not open keeps its stack down,
    only changed secret files are replaced, the rights come back at every pass, and a
    stack's secrets are removed only once its containers have stopped.
  - In `agent/volumes`, `plan_test.go` is the table of target and observation (missing,
    down after a boot, secondary, primary, mounted, never initialized, held by a leaver,
    another device). `reconciler_test.go` runs passes and waits for their jobs against the
    cluster and the fake driver. It covers a copy formatted once and then left alone; a
    copy without a stack made secondary and unmounted, then found again by the next stack
    without `--force`; a held copy left alone; a hanging job that holds up no other volume;
    an error that reaches the stack; a plain copy of data never recreated; a new data
    generation; a copy the state lost; removal once no stack holds it; the records; and the
    weekly trim of mounted copies across restarts.
  - That docker compose fails on a missing secret file, and that it recreates exactly the
    containers that changed, is left to the live tests, because CI has no Docker daemon.

## Network (`internal/network`)

- `clowder0` carries the node address and the resolver's address (`network.Resolver`).
- **The agent's network loop** (`agent/network.go`) reads the wanted network from the
  state (`readWantedNetwork`). It applies the nft rules first, then the WireGuard mesh, at
  every change and every loop interval. Every other host is a peer until it is removed, and
  the addresses of all hosts stay closed to containers. The loop keeps the mesh it applied
  in `net.json`. A joined host's first start, and every start after a reboot, apply that
  file before Raft (`restoreMesh`).
- **The WireGuard mesh** (`ApplyMesh`) has one interface per peer, `clw-<m>`, so each peer
  has its own port, route and routing table:
  - `clw-<m>` listens on `51800 + m` and reaches peer *m* at its underlay on
    `51800 + <own ID>`, with `AllowedIPs 0.0.0.0/0`. Answers without SNAT carry a client's
    address, and the peers are the cluster's own hosts. Its configuration
    (`RenderWireGuard`, golden file `testdata/clw-2.golden.conf`, which `bin/dev
    test-tools` loads with `wg`) holds the private key, so it is passed to `wg syncconf`
    on standard input.
  - Its MTU is the underlay interface's MTU minus 80.
  - It carries the host's own node address as `/32 noprefixroute`, because the reverse
    path filter drops what arrives on an interface without an IPv4 address. It also
    carries the route to the peer's node address, with the own node address as `src`.
  - Table `100 + m` has `default dev clw-<m>`. The rule with priority `100 + m` sends
    packets marked `0x100 + m` (mask `0xfff`) there.
  - A former peer loses all three. First, `conntrack -D --mark` forgets the connections
    that came from it, because its ID may come back with another host. If the agent stops
    halfway, the remaining interface or rule brings the next pass back to it.

  At every pass the loop sets each peer's interface and routes again (`wg syncconf` and
  `ip … replace` change only what differs) and replaces any rule that is not as wanted. A
  peer that fails holds up no other, and the errors are collected. Only `clw-<host ID>` is
  clowder's own, and so is a rule whose priority and table are both `100 + m`. The tools
  run through `Host.Run`, so `wireguard_test.go` checks the commands against what the host
  has.
- **The table `inet clowder`** (`Render`, golden file `testdata/isolation.golden.nft`)
  keeps containers in their place:
  - `guard_in`: from a container bridge, the host answers only the resolver, the local
    Traefik on the Floating IP (HTTP hairpin) and the container's own connections. A
    spoofed source is checked first, as in `guard_fwd`.
  - `guard_fwd` covers traffic beyond the host, in this order:
    1. A packet whose source would not route back through the bridge it came from is
       dropped (`fib saddr . iif oif missing`). So a container with NET_RAW can neither
       spoof nor slip into another container's connection (conntrack does not look at the
       interface).
    2. Established connections, and what clowder or Docker redirected (`ct status dnat`:
       exports, hairpin, published ports), pass.
    3. These are dropped: the metadata API; `@cluster_hosts` (every host's underlay and
       public address, including a removed host's, as its server may still run); the
       private network (`privateNetwork` of the configuration, which must not overlap
       `clusterspec.ClowderRanges`); and the overlay.

    The rules are IPv4 only, because Docker gives the bridges no IPv6 (`daemon.json`).
  - `guard_out`: the host's own processes, including Traefik in the host's network, cannot
    reach the metadata API either. A joining host's user data holds its join token, and
    clowderd never needs it. cloud-init reads the user data at boot in its network stage,
    before `network-online.target` and so before `clowderd.service` starts, and the table
    does not survive a reboot. The resolver's upstreams come from `/etc/resolv.conf`, not
    from the metadata API. A known limit: a tool that reads the service later gets no
    answer, such as Hetzner's `hc-utils` when a private network's interface is plugged in
    while the host runs. `examples/hetzner` therefore attaches the network when it creates
    the server.
  - `marks`: a new connection that comes in through `clw-<m>` keeps the mark `0x100 + m`,
    and a container's answer gets the mark back, so the answer leaves through its peer's
    table. The host's own processes (Traefik) get no mark: what reaches them from a peer
    comes from the peer's node address, and the route to that address leads back. So a DNAT
    from another host that keeps the client's address must lead to a container.
  - A connection that was open before the table first loads (at the agent's start) passes
    as established until it ends.
  - `mss`: TCP through `clw-*` gets its MSS from the route's MTU; UDP must fit by itself.
  - The drop rules count (`counter`), so `nft list table inet clowder` shows what they
    dropped since the table was last loaded (every loop interval).

  `ApplyRules` checks the table with `nft -c`, then replaces it in one transaction; Docker's
  rules stay untouched.
- The resolver passes every query on to the host's resolvers (`/etc/resolv.conf`). Names of
  exported services come with exports.
- Published ports are bound on every host's Floating IP and node address
  ([Rendering](specs.md#rendering-internalcompose)). A port of a stack on another host is
  forwarded (`Ruleset.Forwards`, the same on every host, because the Floating IP may move
  to any host):
  - `forwards` (nat, before Docker's rules): the Floating IP's port goes by DNAT to the node
    address of the stack's host, with the mark `0x200`. It leaves through that peer's
    WireGuard interface with the client's address, and Docker's publication on that host
    takes it to the container. A port of a stack on this host is handled by Docker alone.
  - Docker's FORWARD chain drops what Docker did not publish. `AllowForwards` keeps one rule
    at the top of `DOCKER-USER` (iptables) that lets connections with DNAT and the mark
    pass, in both directions. This is needed because no nft table can overrule another
    table's drop.
  - `hairpin` (nat): a container's own connection to a forwarded port leaves from this
    host's node address, because an answer to its bridge address would reach a bridge of
    the same network on the stack's host.
  - conntrack keeps a connection's target, so a UDP flow that never pauses would stay with
    the old host. When a port's target changes, the agent therefore forgets the connections
    to the Floating IP on that port (`ForgetForwards`). It does not do this at the agent's
    start, because it does not know the previous rules then. A port that moved while the
    agent was down keeps its connections until they end.

## Storage (`internal/storage`)

- A copy of a volume is a thin LV in a pool of its storage class. With replicas, a DRBD 9
  resource sits on top of it (`RenderResource`, golden files in `testdata/` that `bin/dev
  test-tools` checks with `drbdadm`). The resource uses a quorum majority with
  `suspend-io`, `auto-promote no`, node-id = host ID, and port `drbdBasePort` + minor on
  the node address. With replicas 0, the copy is a plain LV. A copy's LV is named
  `names.VolumeName(volume ID, data generation)`, and its resource and mount point
  (`volumes/<LV>`) have the same name. So a new data generation (today a new, empty LV of
  replicas 0; from phase 3 on also a restore) is a new LV next to the old one, mounted
  separately.
- **Each step is a method, and does only what is missing.**
  - `Ensure` creates the LV and, with replicas, brings the resource up (`drbdadm adjust`,
    which also applies new peers). A new resource comes up secondary; `Ensure` changes no
    role and mounts nothing.
  - `Promote` (with `force` only for a volume that never held data) and `Mount` (ext4, the
    mount and `data/`; `format` only for such a volume) make a copy usable here. `Unmount`
    and `Demote` give it up.
  - The volume reconciler decides which steps a copy needs from `Observe`, which only
    reads: the volume groups' LVs, the roles of all resources (`drbdsetup status --json`;
    a resource that is down is not listed), the mount table, and whether the resource file
    is current. A file counts as current only once `adjust` has applied it too (the marker
    `<resource>.adjusted`), so an `adjust` that failed, for example without quorum, runs
    again at the next pass.
  - Markers in the driver's `StateDir` let `Ensure` pick up after a crash at any point
    (`created`: clowder created the LV; `metadata`: DRBD metadata written).
  - A tiebreaker (`Volume.Diskless`) has no LV and no metadata. `Ensure` writes its
    resource file and runs `adjust`, and `Remove` takes it down. A copy that became the
    tiebreaker on its host keeps its `Pool`. Once `adjust` has detached the disk, `Ensure`
    removes the LV and its markers (`removeDetachedLV`), so a disk that comes back gets new
    metadata.
  - When a resource's file loses a peer (a copy went away), `Ensure` first notes the peer
    (the marker `<resource>.forget`) and forgets it after the `adjust` (`ForgetPeerOf`),
    so the next copy finds a free slot in the metadata (`maxPeers`).
  - `Observe` also reads each copy's disk state and how much it differs from its peers
    (`Observed.Disk`, `OutOfSyncBytes`; the latter only toward the peers it resyncs from).
  - `ForgetPeer` forgets a removed host on every resource (`drbdsetup forget-peer`), so
    its node-id can serve a new host. It fails, and the agent tries again, while a resource
    is down or still has the peer configured (its `adjust` takes the peer out). It also
    fails for any refusal other than `unknownPeer`: a node-id without a slot has nothing to
    forget, and DRBD 9.3 silently forgets a peer it never knew or has already forgotten.

  Whether a volume holds data is not a marker; the state knows it for every host
  (`Volume.Initialized`). The driver refuses an LV that it did not create and never wrote
  metadata on or formatted (plain or replicated), a device with another file system, and a
  device without a file system when the volume holds data. It leaves those to a human. A
  crash between `lvcreate` and its marker leaves an empty LV that a human must look at.
- **It never guesses.** Only a tool's own answer means "not there": `blkid`'s exit code 2,
  `findmnt`'s exit code 1, and for an LV its name missing from the volume group's list
  (`lvs` has no exit code of its own for that). Any other failure, such as a timeout or a
  device that is not ready, stops the step, and a table that cannot be read stops
  `Observe`. So a guess never creates over real data or formats it. A mount point with
  another device mounted stops the step too, because that data would reach the containers
  as the volume's data.
- **The mount guard:** containers get `data/`, which exists only in the mounted file system.
  So a container whose volume is not mounted fails to start instead of writing to the root
  file system.
- `Remove` picks up after a crash at any point: a resource without its file is already
  down, and an LV that `lvs` does not find is already gone. It takes a resource down only
  once no connected peer is primary (`checkNoPrimaryPeer`), because a primary with two
  copies would lose its quorum and suspend its stack's writes. The primary goes first, or
  drops the copy from its resource.
- `PoolUsage` reads a pool's size and use; `Trim` (fstrim) returns the deleted blocks of
  mounted copies.
- `Observe` reads each copy's quorum (`Observed.Quorum`) and whether a peer has split from
  it (`SplitBrain`). That is `connection-state` `StandAlone`, after a split brain that the
  `after-sb-*` rules could not resolve, or after another refusal a human must look at.
  `ResolveSplitBrain` connects those peers again (`drbdadm connect resource:peer`); the
  copies that give up their changes (`--discard-my-data`) must be secondary. A human
  decides which copy is kept. `clowder volume resolve-splitbrain <volume> --keep <host>`
  asks the hosts that give up their changes first, then the host to keep and the
  tiebreaker, through any control-plane agent (`api/splitbrain.go`). It refuses while the stack runs on a
  host other than the one kept.

## Containers (`internal/docker`, `internal/supervisor`)

- `docker.Client` talks to Docker's API on its socket for events, start and restart. It
  runs `docker compose` and `docker pull` through `internal/command`, each pull with the
  Docker config of the stack's namespace (`withConfig`: `--config`; without one, Docker
  would use root's own). `up` gets the rendered file and, for a stack with secret
  variables, `env.yaml`; down, ps and logs need only the rendered file. `compose down`
  also removes the stack's Docker volumes. They only bind a directory of clowder's volume,
  whose data stays until clowder removes the copy.
- **Docker restarts nothing** (`restart: "no"`), so that after a boot nothing starts before
  the agent has looked at the cluster. The supervisor follows Docker's events and applies
  each service's restart rule with `DefaultBackoff`. The first restart happens at once, so
  a crashed service runs again within 2 s; then the wait is 1 s, doubling up to a minute.
  `unhealthy` means a restart with the same backoff, unless the service says otherwise.
- **A manual stop is respected:** `docker stop`, or a kill with a signal that ends the
  container (`endsContainer`). A kill with another signal, such as a reload with SIGHUP, is
  not a stop. While the agent runs `compose up` or `down` for a stack, it `Pause`s the
  supervisor for the stack's project, so the supervisor does not fight it.
- A start that fails counts as another crash; a container that is gone
  (`docker.ErrNoContainer`) is left alone. `destroy` events drop what the supervisor knew
  about a container. When the events break off, anything that died in between went unseen
  (Docker restarted, and its containers keep running). The supervisor tells the agent,
  which then runs `compose up` for every stack, as after a boot.

## Host check (`internal/hostcheck`, `clowderd check`)

`clowderd check` checks the operator base and changes nothing. It checks sysctls, time
sync, the watchdog, WireGuard, DRBD 9 built for the running kernel, drbd-utils, DRBD
autostart and usage report off, the LVM filter, thin pools (`--pool`), Docker's
`daemon.json`, docker compose, unattended-upgrades and the Floating IP (`--floating-ip`).
Each check reads through the `Host` interface, so the unit tests use a fake host. The exit
status is 1 if any check fails. A command that hangs (`lvs` while DRBD suspends I/O) is
left behind after `commandTimeout`. That is fine for `clowderd check`, which exits; for the
same reason the agent will run the checks in their own process
([Volumes](../../DEVELOPING.md#volumes)).
