# A host

Part of [DEVELOPING.md](../../DEVELOPING.md): the agent and the drivers that change its host.

## The agent (`internal/agent`)

The package holds the daemon and the one-shot host commands. Some files have similar names:

| File | Holds |
|---|---|
| `init.go`, `join.go` | `clowderd init` and `clowderd join`, run on the new host |
| `joiner.go` | the leader's side of a join |
| `recover.go` | `clowderd recover` |
| `recovery.go` | a fenced host's report that it is back (`recovering`, then `active`) |
| `retired.go` | the check at start whether `clowderd recover` retired this host |
| `forget.go` | forgetting removed hosts as DRBD peers |
| `restart.go` | the supervisor's restart rules, taken from the state |
| `run.go` | `clowderd run`: what starts in which order |
| `controlplanecache.go` | what the control plane gave this host (its stacks' secrets, the certificates' keys), kept in memory |

- **Files.** A host's files are under `DefaultDir`: `host.json` (ID, name, addresses, ports),
  `net.json` (the WireGuard mesh the host last applied), `keys/`, `pki/`, `raft/`,
  `stacks/`, `storage/`, `volume-records/` and `volumes/`. init and join write `host.json`
  last, so a host with `host.json` has all the other files too.
- **`clowderd init`** (run from cloud-init) creates the first host. It creates the CA, the
  data key, the cluster's sealing key, the host's certificate and WireGuard key. It also
  creates a Raft group with this host as the only voter, and the bootstrap command. Then it
  enables and starts `clowderd.service`. init refuses a host that has a `raft/` directory.
  - `--cluster-name` names the cluster and its CA. The default is `DefaultClusterName`, and
    the name must pass `names.Valid`.
  - `--roles` gives the host's roles. The default is `hostrole.DefaultInit` (all three). The
    first host needs `control-plane`.
  - Three flags are required. Each goes into `host.json` and the state:
    `--underlay` (the address in the Hetzner private network), `--public-address` (the
    public IPv4, where users reach the API) and `--server-id` (the Hetzner Cloud ID, for the
    Floating IP and fencing).

  The host's name in the cluster is its hostname, because DRBD uses the hostname in a
  resource. init takes no key for the first credentials; run `clowderd credentials`
  afterwards.
- **`clowderd join`** (run from cloud-init) makes a host a member of a cluster. It reads a
  join token from `--token-file` and joins through the agent named by `--via`. The other
  flags are the same as init's. Without `--roles`, the roles are `hostrole.DefaultJoin`
  (`worker,ingress`), and the token must allow them. The steps:
  1. Create the host's key and WireGuard key, and keep them in `keys/join.json`.
  2. Trust the cluster's CA through the token's proof and send the join
     (`api.JoiningCluster.Join`). Write the answer into `join.json` at once. The
     certificate must name the roles that the host asked for (`checkJoinCertificate`).
  3. From `join.json`, write the keys, the certificate and, on a control-plane host only,
     the data key (`writeKeys`, as init does), `clowder0`, and `net.json` with the other
     hosts as peers. Then write `host.json` and delete `join.json`.

  If a join fails after step 2, the next attempt continues from `join.json` without asking
  the cluster again. If the answer never arrived, the state shows the host as joining and
  its token is used up; remove it with `clowder host remove` and join again with a new
  token. The leader adds a joining host to Raft as a non-voter at once and promotes it
  once its agent runs and reports (The cluster, Raft). join refuses a host that has a
  `raft/` directory or `host.json`.
- **`clowderd credentials [--name <name>] [--endpoint <address>]... [--revoke-older]`**
  (`agent/credentials.go`) prints a CLI config with one context, like `kubeadm kubeconfig
  user`. It creates the key locally and asks its own agent for the certificate, using the
  host's certificate ([Credentials from a host](api.md#credentials-from-a-host)). The
  agent must be running; `--revoke-older` also needs the leader. Like `clowderd
  sealing-key export` and `clowderd recover`, it runs only on a control-plane host, as
  `host.json` says (`Host.checkControlPlane`; the join wrote the roles from its
  certificate).
- **`clowderd recover --power-off`** makes this host the only member of its cluster when the
  other hosts are gone for good. It powers them off and labels their servers
  `clowder-retired` ([Recover](cluster.md#recover-clowderd-recover-agentrecovergo)). It
  stops the agent while it changes Raft's files and starts it again afterwards.
- **`clowderd run`** (the service) starts in this order:
  1. It reads the state from its Raft log on disk (`raftnode.ReadState`) and checks that
     `host.json`, the host's certificate and the state name the same roles
     (`Host.checkRoles`; a state that does not know the host yet is not compared). The API
     trusts the certificates, but the loops read `host.json` and the state, so the agent
     does not start while they differ. Only a change by hand makes them differ.
  2. A control-plane host asks Hetzner whether `clowderd recover` retired it
     (`checkRetired`), with the token from the state on disk. A host is retired when its
     server's label `clowder-retired` names the host's certificate. A retired host's agent
     ends with `ErrRetired` (exit status 4) before it starts Raft, the mesh or any stack,
     because two retired voters would still hold the old cluster's majority. Other hosts
     hold no token and never vote; no control-plane host takes a retired worker's revoked
     certificate. The check is skipped when the cluster has no ClusterConfiguration (no
     token), and the agent starts normally when Hetzner does not answer within
     `retiredCheckTimeout`.
  3. It sets up `clowder0`, a dummy interface, with the node address, so the address
     exists even without peers. Then it restores the WireGuard mesh from `net.json`
     (`restoreMesh`). Raft runs over the mesh, so without this file a rebooted host would
     have to learn its peers from a leader it cannot reach. The mesh from the state
     replaces it once the state answers.
  4. It starts the Raft member, which waits up to `catchUpTimeout` for the state to catch
     up with the Raft log. If the state meets an entry or snapshot it cannot read in that
     time, the agent ends here, before its loops start. A follower may also meet such an
     entry later, when it applies the leader's log.
  5. It starts the containers' DNS resolver.
  6. `newDaemon` creates the parts from what they all share (`shared`: the files, this
     host, its keys (the data key on a control-plane host only), the Raft member, the
     host's parts, the logger). It starts the API on
     the underlay, the node address and, on a control-plane host, the public address
     (`Host.apiAddresses`). Other hosts serve only the control plane, which calls them
     over the private network. The listeners use `IP_FREEBIND` (`freeBind`), because at
     boot the public address may come up after the agent, which must neither fail nor
     lock users out then. Then it starts the servers for ACME's challenges and the
     metrics. If any of them cannot listen, the agent ends before it starts a container.
     Later, only a failing API ends the agent; the host runs its stacks without the other
     two, so their failure is only logged.
  7. It starts the loops (`startLoops`):
     - this host's loops that start no container: the network, the supervisor, and the
       beats to the voters;
     - on a control-plane host only (`startControlPlaneLoops`), which holds the data key:
       the leader's loops, which do nothing while this host does not lead, and the watch
       that alerts while there is no leader (`QuorumWatch`). Other hosts never vote, so
       they never lead;
     - after the state has caught up with the leader's commit index (`runOnceCaughtUp`,
       `waitForState`; [Raft](cluster.md#raft-internalraftnode-and-the-data-key-internaldatakey)):
       the report, Traefik, the volume reconciler and its trim, forgetting removed hosts
       (`forgetPeers`: `storage.ForgetPeer`, then `report-peer-forgotten`), the
       prefetcher, the steps of moves (`moveSteps`), the report of a host that is back
       after a fence (`reportRecovery`), and the stack runner. They wait because a host
       that was away must not start a stack that has moved in the meantime.
  8. It tells systemd `READY=1`. It feeds the watchdog as long as the state's lock
     answers.

  When the agent stops, it ends its loops and waits for them for at most `loopsTimeout`,
  because a host tool hanging on a suspended volume must not block the stop. The resolver
  and the Raft member close only after every loop has ended (`stopLoops`). If a loop is
  left behind, it may still use them, so they stay open and the agent exits with an error.
  The process exit closes them, and systemd restarts the agent; Raft's log is on disk.

  When its Raft member halts at an entry it cannot read (`Node.Halted`,
  [Versions](state.md#versions)), the agent exits with status 3 (`exitUnreadable` in
  `cmd/clowderd`). systemd restarts it neither after status 3 nor 4 (Shipping).
- **`HostParts`** are what the agent changes on its host: the network, the storage driver
  and Docker. `clowderd run` passes the real ones, and the tests pass fakes. The tests
  fake Hetzner's API with `hetzner/hetznertest`, set in the configuration's `hetzner.api`.
- **Reconcilers** run after each change of the state and every `loop.Interval`, so they
  repair changes made by hand. `reconcileNetwork` keeps the nft rules, the volume
  reconciler keeps the volume copies, and the stack runner keeps the stacks.
- **The volume reconciler** (`agent/volumes`, `volumes.Reconciler`) keeps every copy that
  the state places on this host, whether or not a stack here uses it. The target for a
  copy is:
  - primary and mounted, when a stack placed here names the volume;
  - unchanged, while a stack's `volumes.json` lists its LV (`stacks.Files.HeldLVs`). This
    covers a leaver whose `compose down` has not worked yet, and every copy while a list
    cannot be read;
  - otherwise unmounted and secondary, so a stack on another host can take the volume.

  A pass only reads: the state, the held LVs, and the host (`storage.Driver.Observe`:
  `lvs` per volume group, `drbdsetup status --json` and `findmnt`, each once for all
  copies). `plan` (pure, in `plan.go`) compares target and observation and returns the
  steps: `ensure`, `promote` or `promote --force`, `mount` or `mount --format`,
  `set initialized`, `unmount`, `demote`. A copy that is already as wanted needs no steps,
  and the pass runs no tool for it. `--force` and `--format` are used only for a volume
  that the state does not know as initialized ([Volume copies](state.md#volume-copies),
  Initialized volumes). Two cases are errors for a human: a plain copy whose LV is gone
  while the volume holds data, and another device at the mount point.

  A tiebreaker (`VolumeCopy.Tiebreaker`, `storage.Volume.Diskless`) is only the DRBD
  resource; it is never promoted or mounted. A stack that stops for a move
  (`State.StopsForMove`) names no copy here, so its copies go secondary. The pass also
  records:
  - when a copy with a disk becomes or stops being synced (`set-copy-synced`, from the
    disk state that `Observe` reads; a resource that is down reports nothing);
  - how much each copy still has to resync (`Progress`, for the leader and `/metrics`);
  - whether a copy is unmounted and secondary as wanted (`Released`, for a move).

  A volume's steps run in a job, its own goroutine, because mkfs, `mkdir data/`, mount
  and unmount write to the volume and hang while DRBD suspends I/O. A volume has at most
  one job at a time (`busy`). The pass does not wait for it; a successful job starts the
  next pass. A failed job is retried at the next change of the state or `loop.Interval`,
  because an immediate retry of a failing step would spin. `Run` waits for the jobs when
  it stops, within the agent's `loopsTimeout`.

  The stack runner calls `Mounted(lv)`. It returns the mount point once the copy is ready,
  the error that keeps it from getting there (logged once, when new), or neither while it
  is on its way. A copy that becomes ready starts a pass of the stack runner.

  Retired copies are removed in a job too, once no stack holds them, and then forgotten
  ([Volume copies](state.md#volume-copies), Retired copies). A host takes a copy down only
  when no connected peer is primary (`storage` `checkNoPrimaryPeer`, `ErrNotYet`);
  otherwise, with a quorum majority, the primary of a two-copy volume would lose its
  quorum. A copy that the state neither places nor retires here stays, with one warning
  per run.

  Files: the pass is in `reconciler.go`, the targets in `desired.go`, the records in
  `records.go`, and the trim in `trim.go`.
- **The stack runner** (`agent/stacks`, `stacks.Runner`) keeps everything of a stack under
  its Compose project (`names.Project`). A pass first stops the stacks that left the host
  (`compose down`). Then it starts a job for each stack that the state places here and
  that has no job running (`Runner.start`). So a long image pull, or a `compose up` that
  hangs on a suspended volume, blocks no other stack. While a stack's job runs, passes
  leave the stack alone, and the job starts another pass when it ends. A leaver whose job
  still runs is stopped by a later pass; its volumes count as in use until then. A stack
  that stops for a move counts as a leaver. A host that is not `state.KeepsStacks`
  (fenced, recovering, disabled) runs no stacks, because they may already run elsewhere.

  The job renders the Compose file with the mounted volume copies, writes the secrets,
  runs `compose up` unless nothing changed since it last worked, and reports `running`.
  Because leavers stop first, two stacks never use a volume at once. A stack that fails to
  come up, at any step or because the reconciler cannot mount a copy, is reported
  `failing` once with the first line of the error (`reportFailing`, `Stack.UpError`) and
  is tried again at every pass. The leader raises `stack-failing` if this lasts. Waiting
  for a leaver's volume or for a copy on its way is not failing.

  Before `compose up`, the runner pulls the images of the services that a user restarted
  since their containers came up (their `clowder.restarted` label differs from the time in
  the state), because an image may have changed under the same tag. Docker keeps the
  label, so a reboot does not pull again.

  Files: the pass is in `runner.go`, one stack's steps in `apply.go`, and the leavers in
  `leavers.go`. The agent hands the runner its parts (`stacks.Parts`): the file locations
  (`stacks.Files`), this host, the Raft member, Docker, the volume reconciler,
  `OpenSecret` and the supervisor.
- **Secrets from the control plane.** `OpenSecret` gives a Secret's values for the digest
  that the state names. The agent (`newSecretOpener`, `controlPlaneCache`) keeps what
  `api.Server.SecretsOfThisHost` gave it in memory, and asks again only when the state
  names a digest it does not have, as the kubelet keeps its pods' secrets. Each answer
  replaces all it kept. After a failed call it waits `askAgainAfter` before it asks
  again, so a pass does not wait for the API's timeout each time. Traefik's runner keeps
  the certificates' keys the same way and drops them once the host no longer holds the
  Floating IP; while no leader answers, it keeps the key files it wrote before. On a
  control-plane host
  the API server opens the values itself; elsewhere it asks the leader
  ([api.md](api.md#keys-only-on-the-control-plane)). A stack whose secrets do not come
  waits, and `get stacks` says why.
- **The prefetcher** (`stacks.Prefetcher`) pulls the images of the stacks that this host
  may take in a failover or a move, before it needs them (`docker.Client.PullMissing`).
  These are, on a worker in service, the stacks whose volumes with data all have a copy
  with a disk here, the stacks without such volumes, and the stack that moves here. It
  pulls one image at a time in its own loop, so a long pull blocks nothing else. It pulls
  without logins (`RegistryConfigs.WithoutLogins`), because a host gets a namespace's
  logins only for the stacks that run on it. An image that fails to pull in a namespace
  with image pull secrets is left for the stack's start, and a move does not wait for it
  (`Ready`).
- **The steps of moves** (`agent/moves.go`, `moveSteps`) record each step of a move that
  this host owns, once its loops have done it. On the new host, the steps are: each copy
  synced (or the volume never held data), and the images present. On the old host: the
  stack stopped (`stacks.Files.Stopped`), and each copy released.
- **A leaver that fails to stop** may still use its volumes. No other stack may take them
  until it has stopped, because two databases on one file system would ruin it. The
  leaver's volumes are the LVs in its `volumes.json`, next to `compose.yaml`. Before
  `compose up`, the runner adds the LVs of the new Compose file to this list, because the
  containers of the previous file keep their volumes until `compose up` has worked.
  Afterwards the file lists only the new LVs. If the file cannot be read, the leaver counts
  as using every volume, and every stack waits a pass.
- **Crashes leave nothing unknown.** The files show how far the runner and the reconciler
  got. A volume's record (`volume-records/`, one file per volume, separate from the
  stacks) lists its copies on this host before they are created and drops a copy only
  after it is removed. So a copy is found whatever happened to its stack. Only the
  volume's job writes the record. A stack's `compose.yaml` exists until `compose down` has
  worked, and its directory is removed right after. Its secrets are removed once
  `compose.yaml` is gone.
- **Secrets live on tmpfs only** (`Files.SecretsDir`, `DefaultSecretsDir`). The agent
  creates the directory itself. A systemd `RuntimeDirectory` would be removed at every stop
  of the agent, while the containers still use it.

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
  `atomicfile.WriteChanged`), so rights changed by hand do not persist; the agent does the
  same for `SecretsDir` at its start. After a successful `compose up`, the runner removes
  what the stack no longer uses. The hash that decides whether to run `compose up` covers
  the rendered file, `env.yaml` and the secret files. A changed secret file changes its
  services' digest label, which recreates them
  ([Rendering](specs.md#rendering-internalcompose), Labels).
- **After a boot no container runs** (`restart: "no"`). The runner keeps only in memory
  what each stack came up with, so its first pass runs `compose up` for every stack (after
  the state has caught up, see step 7 of `clowderd run`).

  A restart of Docker leaves the containers running (`live-restore` in `daemon.json`), but
  a container that dies during the restart goes unseen. The supervisor notices the restart
  because its event stream breaks off, and starts a pass at once
  (`loop.Signal.DockerRestarted`). The runner stores Docker's restart count with each
  stack it brought up (`loop.BroughtUp`). So after a restart, even one during
  `compose up`, it runs `compose up` for every stack again. This starts what died and
  leaves the rest alone.
- **Registry logins belong to a namespace**, as Kubernetes pulls with the
  `imagePullSecrets` of a namespace's default service account. Before every `compose up`,
  the runner writes the Docker config of the stack's namespace (`RegistryConfigs.Write`),
  by rename into the registries' directory on the secrets' tmpfs. It holds the logins of
  the namespace's image pull secrets in clear (`state.RegistryLogins`, opened through
  `OpenSecret`), and Docker pulls with it (`docker --config`). The runner and the
  prefetcher share this one value, and its lock keeps their writes apart.
  - A namespace without logins gets an empty config, so no stack pulls with root's own
    logins or another namespace's.
  - Like the kubelet, the agent skips a missing Secret or key and pulls without it. A
    pull that then fails names what was skipped, in `get stacks`.
  - The runner removes the configs of namespaces with no stack here
    (`removeRegistryConfigsOfOthers`), so logins stay no longer than needed.
- **Other loops.** The agent also runs the supervisor and, while it leads, the leader's
  loops: the placement (`controller.RunPlacement`) and those in [Ingress, certificates,
  alerts and metrics](loops.md).
- **The trim** (the exception in [Volumes](../../DEVELOPING.md#volumes)): every
  `trimInterval`, the reconciler trims the copies that its last pass saw mounted, and
  only those with DRBD's quorum or no DRBD. It claims each volume as a job would, so no
  job unmounts it meanwhile. The driver skips a copy that is no longer mounted, because
  fstrim on an empty mount point would trim the root file system. Until every mounted copy
  is trimmed, the trim tries again every hour.
- **The host's report** (`hostReporter`, `report-host`) carries the inputs of the alerts:
  - version, memory and CPUs;
  - the pools' data and metadata use;
  - whether Docker answers a ping (`DockerDown`), and how full the root file system is;
  - the volumes whose copy here has split from a peer (`SplitBrain`) or is primary without
    DRBD's quorum (`NoQuorum`), from the reconciler's last pass (`Seen`).

  It runs in its own loop after the state has caught up, so a hanging Docker cannot block
  the report that says so. Each probe ends at its driver's timeout (Docker's ping, lvs).
  It probes every `loop.Interval` and when a new configuration generation may name a new
  pool, but not at every change of the state, because lvs is slow when DRBD hangs. A
  failed probe keeps the value in the state, so a pool does not vanish for the placement
  when `lvs` times out. The report commits only when a value changed, which is seldom:
  each value is a bool or a whole percent. Percents are rounded up, so a threshold trips
  early rather than late, and a small pool trips as early as a large one.
- **Tests.**
  - The package's own test runs the whole way on 127.0.0.1: init, the agent with a notify
    socket, the first credentials, apply and diff, the Floating IP in the nft rules, and a
    volume and a stack applied, listed and deleted. It also checks that a `Run` that fails
    at its start leaves nothing open. `restart_test.go` covers the supervisor's restart
    rules.
  - `agent/agenttest` has fakes of Docker, the storage driver (each copy's state in
    memory), the volume reconciler, and a cluster in memory. The cluster fails a test
    whose loop writes into its copy of the state (`statetest.ReadOnlyCopy`).
  - `agent/stacks` runs single passes against that cluster: `runner_test.go` (failing
    stacks, the paused supervisor, data generations, waiting for copies, held LVs, secret
    labels), `leavers_test.go` (a volume passes on only after the old stack has stopped,
    including when a stop or `compose up` fails or the leavers cannot be listed),
    `secrets_test.go` (decrypting, replacing only what changed, rights, removal after the
    stop) and `containers_test.go` (registry configs, `compose ps`).
  - In `agent/volumes`, `plan_test.go` is a table of target and observation, and
    `reconciler_test.go` runs passes and waits for their jobs: formatting once, release
    and takeover without `--force`, held copies, a hanging job, errors that reach the
    stack, data generations, removal, the records and the weekly trim.
  - CI has no Docker daemon, so the live tests check that docker compose fails on a
    missing secret file and recreates exactly the containers that changed.

## Network (`internal/network`)

- `clowder0` carries the node address and the resolver's address (`network.Resolver`).
- **The agent's network loop** (`agent/network.go`) reads the wanted network from the
  state (`readWantedNetwork`). At every change and every loop interval, it applies the nft
  rules first and then the WireGuard mesh. Every other host is a peer until it is removed,
  and the addresses of all hosts stay closed to containers. The loop keeps the mesh it
  applied in `net.json` for the next start (`restoreMesh`, step 3 of `clowderd run`).
- **The WireGuard mesh** (`ApplyMesh`) has one interface per peer, `clw-<m>`, so each peer
  has its own port, route and routing table:
  - `clw-<m>` listens on `51800 + m` and reaches peer *m* at its underlay on
    `51800 + <own ID>`, with `AllowedIPs 0.0.0.0/0`: answers without SNAT carry a
    client's address, and the peers are the cluster's own hosts. The configuration
    (`RenderWireGuard`, golden file `testdata/clw-2.golden.conf`, which `bin/dev
    test-tools` loads with `wg`) holds the private key, so it goes to `wg syncconf` on
    standard input.
  - Its MTU is the underlay interface's MTU minus `wireGuardOverhead`.
  - It carries the host's own node address as `/32 noprefixroute`, because the reverse
    path filter drops what arrives on an interface without an IPv4 address. It also
    carries the route to the peer's node address, with the own node address as `src`.
  - Table `100 + m` has `default dev clw-<m>`. The rule with priority `100 + m` sends
    packets marked `0x100 + m` (mask `0xfff`) to that table.
  - A former peer loses all three. First, `conntrack -D --mark` forgets the connections
    that came from it, because its ID may come back with another host. If the agent stops
    halfway, the remaining interface or rule makes the next pass finish the job.

  At every pass the loop sets each peer's interface and routes again (`wg syncconf` and
  `ip … replace` change only what differs) and replaces any rule that is not as wanted. A
  failing peer blocks no other peer, and the errors are collected. Clowder owns only the
  interfaces `clw-<host ID>` and the rules whose priority and table are both `100 + m`.
  The tools run through `Host.Run`, so `wireguard_test.go` checks the commands against
  what the host has.
- **The table `inet clowder`** (`Render`, golden file `testdata/isolation.golden.nft`)
  keeps containers in their place:
  - `guard_in`: from a container bridge, the host answers only the resolver, the local
    Traefik on the Floating IP (HTTP hairpin) and the container's own connections. A
    spoofed source is checked first, as in `guard_fwd`.
  - `guard_fwd` covers traffic beyond the host, in this order:
    1. A packet is dropped if its source would not route back through the bridge it came
       from (`fib saddr . iif oif missing`). So a container with NET_RAW can neither spoof
       nor slip into another container's connection (conntrack does not look at the
       interface).
    2. Established connections pass, and so does what clowder or Docker redirected
       (`ct status dnat`: exports, hairpin, published ports).
    3. These are dropped: the metadata API; `@cluster_hosts` (every host's underlay and
       public address, including a removed host's, because its server may still run); the
       private network (the configuration's `privateNetwork`, which must not overlap
       `clusterspec.ClowderRanges`); and the overlay.

    The rules are IPv4 only, because Docker gives the bridges no IPv6 (`daemon.json`).
  - `guard_out`: the host's own processes, including Traefik in the host's network, cannot
    reach the metadata API either, because a joining host's user data holds its join
    token. clowderd never needs the metadata API: cloud-init reads the user data at boot
    before `network-online.target` (so before `clowderd.service` and this table, which
    does not survive a reboot), and the resolver's upstreams come from
    `/etc/resolv.conf`. A known limit: a tool that reads the metadata service later gets
    no answer, such as Hetzner's `hc-utils` when a private network's interface is plugged
    in while the host runs. So `examples/hetzner` attaches the network when it creates
    the server.
  - `marks`: a new connection that comes in through `clw-<m>` keeps the mark `0x100 + m`.
    A container's answer gets the mark back, so the answer leaves through its peer's
    table. The host's own processes (Traefik) get no mark. What reaches them from a peer
    comes from the peer's node address, and the route to that address leads back anyway.
    So a DNAT from another host that keeps the client's address must lead to a container.
  - A connection that was open before the table first loads (at the agent's start) passes
    as established until it ends.
  - `mss`: TCP through `clw-*` gets its MSS from the route's MTU; UDP must fit by itself.
  - The drop rules count (`counter`), so `nft list table inet clowder` shows what they
    dropped since the table was last loaded (every loop interval).

  `ApplyRules` checks the table with `nft -c`, then replaces it in one transaction.
  Docker's rules stay untouched.
- The resolver passes every query on to the host's resolvers (`/etc/resolv.conf`). Names of
  exported services come with exports.
- Published ports are bound on every host's Floating IP and node address
  ([Rendering](specs.md#rendering-internalcompose)). A port of a stack on another host is
  forwarded (`Ruleset.Forwards`). The rules are the same on every host, because the
  Floating IP may move to any host:
  - `forwards` (nat, before Docker's rules): DNAT sends the Floating IP's port to the node
    address of the stack's host, with the mark `0x200`. The packet leaves through that
    peer's WireGuard interface with the client's address, and Docker's publication on that
    host takes it to the container. A port of a stack on this host is handled by Docker
    alone.
  - Docker's FORWARD chain drops what Docker did not publish, and no nft table can overrule
    another table's drop. So `AllowForwards` keeps one iptables rule at the top of
    `DOCKER-USER` that lets connections with DNAT and the mark pass, in both directions.
  - `hairpin` (nat): a container's own connection to a forwarded port leaves from this
    host's node address. An answer to the container's bridge address would reach a bridge
    of the same network on the stack's host.
  - conntrack keeps a connection's target, so a UDP flow that never pauses would stay with
    the old host. When a port's target changes, the agent therefore forgets the connections
    to the Floating IP on that port (`ForgetForwards`). It does not do this at the agent's
    start, because it does not know the previous rules then. A port that moved while the
    agent was down keeps its connections until they end.

## Storage (`internal/storage`)

- A copy of a volume is a thin LV in a pool of its storage class. With replicas, a DRBD 9
  resource sits on top of the LV (`RenderResource`, golden files in `testdata/` that
  `bin/dev test-tools` checks with `drbdadm`). The resource uses a quorum majority with
  `suspend-io`, `auto-promote no`, node-id = host ID, and port `drbdBasePort` + minor on
  the node address. With replicas 0, the copy is a plain LV.

  A copy's LV is named `names.VolumeName(volume ID, data generation)`, and its resource
  and mount point (`volumes/<LV>`) have the same name. So a new data generation (today a
  new, empty LV of replicas 0) is a new LV next to the old one, mounted separately.
- **Each step is a method, and does only what is missing.**
  - `Ensure` creates the LV and, with replicas, brings the resource up (`drbdadm adjust`,
    which also applies new peers). A new resource comes up secondary. `Ensure` changes no
    role and mounts nothing.
  - `Promote` and `Mount` (ext4, the mount and `data/`) make a copy usable here. `force`
    and `format` are only for a volume that never held data. `Unmount` and `Demote` give
    the copy up.
  - `Observe` only reads, and the volume reconciler decides each copy's steps from it. It
    reads the volume groups' LVs, the roles of all resources (`drbdsetup status --json`; a
    resource that is down is not listed), the mount table, and whether each resource file
    is current. A file counts as current only after `adjust` has applied it (the marker
    `<resource>.adjusted`). So an `adjust` that failed, for example without quorum, runs
    again at the next pass. `Observe` also reads each copy's disk state, how much it
    differs from the peers it resyncs from (`Observed.Disk`, `OutOfSyncBytes`), its
    quorum (`Observed.Quorum`) and whether a peer has split from it (`SplitBrain`, below).
  - Markers in the driver's `StateDir` let `Ensure` continue after a crash at any point
    (`created`: clowder created the LV; `metadata`: DRBD metadata written).
  - A tiebreaker (`Volume.Diskless`) has no LV and no metadata. `Ensure` writes its
    resource file and runs `adjust`, and `Remove` takes it down. A copy that became the
    tiebreaker on its host keeps its `Pool`. After `adjust` has detached the disk, `Ensure`
    removes the LV and its markers (`removeDetachedLV`), so a disk that comes back gets new
    metadata.
  - When a resource's file loses a peer (a copy went away), `Ensure` first notes the peer
    (the marker `<resource>.forget`). After the `adjust`, it forgets the peer
    (`ForgetPeerOf`), so the next copy finds a free slot in the metadata (`maxPeers`).
  - `ForgetPeer` forgets a removed host on every resource (`drbdsetup forget-peer`), so
    its node-id can serve a new host. It fails, and the agent tries again, while a resource
    is down or still has the peer configured (its `adjust` takes the peer out). It also
    fails on any refusal other than `unknownPeer` (a node-id without a slot, so nothing to
    forget). DRBD 9.3 silently forgets a peer it never knew or has already forgotten.

  Whether a volume holds data is not a marker. The state knows it for every host
  (`Volume.Initialized`). The driver refuses, and leaves to a human: an LV that it did not
  create and never wrote metadata on or formatted (plain or replicated); a device with
  another file system; and a device without a file system when the volume holds data. A
  crash between `lvcreate` and its marker leaves an empty LV that a human must look at.
- **It never guesses.** Only a tool's own answer means "not there": `blkid`'s exit code 2,
  `findmnt`'s exit code 1, and for an LV its name missing from the volume group's list
  (`lvs` has no exit code for that). Any other failure, such as a timeout or a device that
  is not ready, stops the step, and a table that cannot be read stops `Observe`. So the
  driver never creates over real data or formats it because of a guess. A mount point with
  another device mounted stops the step too, because that data would reach the containers
  as the volume's data.
- **The mount guard:** containers get `data/`, which exists only in the mounted file
  system. So a container whose volume is not mounted fails to start instead of writing to
  the root file system.
- `Remove` continues after a crash at any point: a resource without its file is already
  down, and an LV that `lvs` does not find is already gone. It takes a resource down only
  when no connected peer is primary (`checkNoPrimaryPeer`). Otherwise a primary with two
  copies would lose its quorum and suspend its stack's writes. The primary must go first,
  or drop the copy from its resource.
- `PoolUsage` reads a pool's size and use. `Trim` (fstrim) gives the deleted blocks of
  mounted copies back to the pool.
- **Split brain.** A peer that has split from a copy (`SplitBrain`) shows as
  `connection-state` `StandAlone`, after a split brain that the `after-sb-*` rules could
  not resolve or after another refusal that a human must look at. `ResolveSplitBrain`
  connects those peers again (`drbdadm connect resource:peer`); the copies that give up
  their changes (`--discard-my-data`) must be secondary. A human picks the copy to keep
  with `clowder volume resolve-splitbrain <volume> --keep <host>`, through any
  control-plane agent (`api/splitbrain.go`). It asks the hosts that give up their changes
  first, then the host to keep and the tiebreaker. It refuses while the stack runs on a
  host other than the one kept.

## Containers (`internal/docker`, `internal/supervisor`)

- `docker.Client` talks to Docker's API on its socket for events, start and restart. It
  runs `docker compose` and `docker pull` through `internal/command`. Each pull uses the
  Docker config of the stack's namespace (`withConfig`: `--config`); without it, Docker
  would use root's own config. `up` gets the rendered file and, for a stack with secret
  variables, `env.yaml`; down, ps and logs need only the rendered file. `compose down`
  also removes the stack's Docker volumes. These only bind a directory of clowder's
  volume, whose data stays until clowder removes the copy.
- **Docker restarts nothing** (`restart: "no"`), so after a boot nothing starts before the
  agent has looked at the cluster. The supervisor follows Docker's events and applies each
  service's restart rule with `DefaultBackoff`. The first restart happens at once, so a
  crashed service runs again within 2 s. Then the wait starts at 1 s and doubles up to a
  minute. `unhealthy` means a restart with the same backoff, unless the service says
  otherwise.
- **A manual stop is respected:** `docker stop`, or a kill with a signal that ends the
  container (`endsContainer`). A kill with another signal, such as a reload with SIGHUP, is
  not a stop. While the agent runs `compose up` or `down` for a stack, it `Pause`s the
  supervisor for the stack's project, so the two do not fight.
- A start that fails counts as another crash. A container that is gone
  (`docker.ErrNoContainer`) is left alone. `destroy` events drop what the supervisor knew
  about a container. When the event stream breaks off, the supervisor tells the agent,
  which runs `compose up` for every stack (see After a boot no container runs, above).

## Host check (`internal/hostcheck`, `clowderd check`)

`clowderd check` checks the operator base and changes nothing. It checks sysctls, time
sync, the watchdog, WireGuard, DRBD 9 built for the running kernel, drbd-utils, DRBD
autostart and usage report off, the LVM filter, thin pools (`--pool`), Docker's
`daemon.json`, docker compose, unattended-upgrades and the Floating IP (`--floating-ip`).
Each check reads through the `Host` interface, so the unit tests use a fake host. The exit
status is 1 if any check fails.

A command that hangs (`lvs` while DRBD suspends I/O) is left behind after
`commandTimeout`. That is fine for `clowderd check`, because it exits. For the same reason,
the agent will run the checks in their own process ([Volumes](../../DEVELOPING.md#volumes)).
