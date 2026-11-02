# A host

Part of [DEVELOPING.md](../../DEVELOPING.md): the agent and the drivers that change its host.

## The agent (`internal/agent`)

- **Files.** A host's files are under `DefaultDir`: `host.json` (ID, name, addresses, ports),
  `keys/`, `pki/`, `raft/`, `stacks/`, `storage/`, `volume-records/`, `volumes/`. init and
  join write `host.json` last, so a host with it has everything else.
- **`clowderd init`** (from cloud-init) makes the first host: the CA, the data key, the
  cluster's age key, the host's certificate and WireGuard key, a Raft group with this host as
  its only voter and the bootstrap command. Then it enables and starts `clowderd.service`.
  It refuses a host that has a `raft/` directory. The bootstrap key comes from a file
  (`--bootstrap-key-file`; `clowder cluster bootstrap --key-file` alike), never from the
  command line, where `/proc` and shell histories keep it. `--public-address` is where users
  reach the API when the underlay is a private network; it goes into `host.json` and the
  state, like `--server-ref`, the hoster's ID of the server (for fencing).
- **`clowderd user recovery-code <user>`** prints the codes that give the user access back,
  one for `user recover` and one for `recovery rotate`, each in its command (the code from
  a host, in [API and the CLI](cluster.md#api-internalapi-and-the-cli-internalcli)). It reads the data key only and changes
  nothing, so it works while the agent is down; it does not know the users, so a mistyped
  name gives codes that fit nowhere.
- **`clowderd run`** (the service) starts in this order:
  1. `clowder0` with the node address: a dummy interface, so the address exists on a host
     without peers.
  2. The Raft member. It waits up to `catchUpTimeout` for its state to catch up with the
     Raft log.
  3. The containers' DNS resolver.
  4. `newDaemon` makes the parts, each from what they all share (`shared`: the files,
     this host, its keys, the Raft member, the host's parts, the logger); then the API, once each on the underlay, the node
     address and the public address. It binds them with `IP_FREEBIND` (`freeBind`): at
     boot the public address may come up after the agent, which must neither fail nor
     lock users out then. Then the answers to ACME's challenges and the metrics. If any
     of them cannot listen, the agent ends before it started any container. Once they
     serve, only an API that fails ends the agent: without the other two the host still
     runs its stacks, so their failure is logged.
  5. The loops (`startLoops`): this host's that start no container (nft rules,
     supervisor, trim), the leader's, which do nothing while it does not lead, and, once
     the state caught up (`runOnceCaughtUp`), the report, Traefik and the stack runner.
  6. It tells systemd `READY=1` and feeds the watchdog as long as the state's lock answers.

  When it stops, it ends its loops and waits for them, at most `loopsTimeout`: a host tool
  left behind on a suspended volume must not hold up the stop. Only once every loop ended do
  the resolver and the Raft member close (`stopLoops`). A loop left behind may still use
  them, so then they stay open and the agent exits with an error: the end of the process
  closes them, and systemd restarts the agent. Raft's log is on disk and survives that.
- `HostParts` are what the agent changes on its host (the network, the storage driver and
  Docker) and the hoster's API. `clowderd run` passes the real ones, the tests fakes.
- **Reconcilers** run after a change of the state and every `loop.Interval`, to repair
  what someone changed by hand: `reconcileNetwork` keeps the nft rules as the state says,
  and the stack runner keeps the stacks.
- **The stack runner** (`agent/stacks`, `stacks.Runner`) keeps everything of a stack under its project
  (`names.Project`). A pass first stops the stacks that left the host (`compose down`), and
  only then, for each stack the state places here, renders its Compose file, `Ensure`s its
  volume copies, writes its secrets, runs `compose up` unless all of it came up before, and
  reports `running`: a volume that passes from one stack to another is never used by both
  at once. A stack that fails to come up (any step, from its secrets to `compose up`) is
  reported `failing` with the first line of the error (`reportFailing`, `Stack.UpError`),
  once, and tried again at every pass; the leader raises `stack-failing` if it lasts. A
  stack that waits for a leaver's volume is not failing. Before `compose up` the
  runner pulls the images of the services a user restarted since their containers came up:
  their `clowder.restarted` label differs from the state's time. An image may have changed
  under the same tag. Docker keeps the label, so a reboot pulls nothing again. Last, volume
  copies the state retired here are removed and forgotten ([Cluster state](cluster.md#cluster-state-internalstate), Retired copies). A copy
  the state neither places nor retires here stays, with a warning once per run. The pass
  is in `runner.go`; one stack's steps in `apply.go`, the leavers in `leavers.go`,
  the volume copies in `volumes.go` and their records in `records.go`. The agent hands the
  runner its parts (`stacks.Parts`): where its files are (`stacks.Files`), this host, the
  Raft member, Docker, the storage driver, the data key and the supervisor.
- **A leaver that fails to stop** may still use its volumes, and no other stack may take
  them until it stopped: two databases on one file system would ruin it. Its volumes are
  the LVs in its `volumes.json` beside `compose.yaml`. Before `compose up` the runner adds
  the LVs of the new Compose file to it, as the containers of the last one keep theirs
  until `compose up` worked; after that, the file lists only the new ones. Without a
  readable file, the leaver counts as using every volume, and every stack waits a pass.
- **Crashes leave nothing unknown.** The files tell how far the runner got. A volume's
  record (`volume-records/`, one file per volume, apart from the stacks) lists its copies
  on this host before they are made, and loses one only after it is removed, so a copy is
  found whatever became of its stack. A stack's `compose.yaml` exists until `compose down`
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
  metrics](loops.md). Every `trimInterval` it trims the volumes.
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
  of Docker, the storage driver and a cluster in memory are in `agent/agenttest`; the
  cluster fails a test whose loop writes into its copy of the state
  (`statetest.ReadOnlyCopy`). In `agent/stacks`, `runner_test.go` runs single passes of the
  runner against that cluster, with a fake supervisor and fake Docker: a failed `compose up`
  reported `failing`, compose only while the supervisor is paused, a new data generation, a
  volume that outlives its stack, secrets written, changed and removed, the label changing
  with a secret, the secrets the containers came up with reported. `leavers_test.go`: a
  volume passes to the next stack only once the old one stopped, and waits while a stop
  fails, while the leavers cannot be listed, and while a failed `compose up` leaves old
  containers on old volumes. `containers_test.go`: registry logins, `compose ps`. `secrets_test.go`, `records_test.go` and `volumes_test.go` run the same way: a
  secret that does not open keeps its stack down, only changed secret files are replaced,
  and a stack's secrets go only once its containers stopped; each copy recorded once, a
  record that does not read left alone, the weekly trim across restarts. That docker
  compose fails on a missing secret file and recreates exactly the containers that changed
  is for the live tests: CI has no Docker daemon.

## Network (`internal/network`)

- `clowder0` carries the node address and the resolver's address (`network.Resolver`).
- The table `inet clowder` (`Render`, golden file `testdata/isolation.golden.nft`) keeps
  containers in their place. From a container bridge the host answers only the resolver,
  the local Traefik on the Floating IP (HTTP hairpin) and the container's own connections.
  Beyond the host, the metadata API (whose user data holds the bootstrap key) is dropped, and
  so are other hosts' node addresses unless clowder redirected the packet there itself
  (DNAT). `Apply` checks with `nft -c`, then replaces the table in one transaction; Docker's
  rules stay untouched.
- The resolver passes every question on to the host's resolvers (`/etc/resolv.conf`). Names
  of exported services come with exports.
- Published ports are bound on every host's Floating IP and node address
  ([Rendering](specs.md#rendering-internalcompose)). The DNAT that brings them to a stack on another host, and the marks for
  answers over WireGuard, join the ruleset in phase 2, where two hosts can test them.

## Storage (`internal/storage`)

- A copy of a volume is a thin LV in a pool of its storage class. With replicas, a DRBD 9
  resource sits on it (`RenderResource`, golden files in `testdata/` that `bin/dev
  test-tools` checks with `drbdadm`): quorum majority with `suspend-io`, `auto-promote no`,
  node-id = host ID, port `drbdBasePort` + minor on the node address. With replicas 0 it is a
  plain LV. A copy's LV is `names.VolumeName(volume ID, data generation)` and its resource
  and mount point (`volumes/<LV>`) have the same name, so a restored volume (a new
  generation) is a new LV next to the old one, mounted apart from it.
- **`Ensure` does only what is missing:** the LV, the resource, the primary role, ext4, the
  mount and `data/` in it. Markers in the driver's `StateDir` let it pick up after a crash at
  any point (`created`: clowder made the LV; `metadata`: DRBD metadata written; `primary`:
  the first promotion, made with `--force`, worked). It refuses an LV it did not create and
  never wrote metadata on or formatted (plain or replicated), and a device with another file
  system: those it leaves to a
  human. A crash between `lvcreate` and its marker leaves an empty LV that a human must look
  at.
- **It never guesses.** Only a tool's own answer means "not there": `blkid`'s exit code 2,
  `findmnt`'s 1, and for an LV its name missing from the volume group's list (`lvs` has no
  exit code of its own for that). Any other failure, a timeout or a device not ready, stops
  `Ensure`, so a guess never creates over or formats real data. A mount point with another
  device mounted stops it too: that data would reach the containers as the volume's.
- **The mount guard:** containers get `data/`, which exists only in the mounted file system,
  so a container whose volume is not mounted fails to start instead of writing to the root
  file system.
- `Remove` picks up after a crash at any point: a resource without its file is down
  already, an LV that `lvs` does not find is gone already.
- `PoolUsage` reads a pool's size and use; `Trim` (fstrim) gives deleted blocks back.

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
