# Developing clowder

This file explains how the code fits together, the rules that keep it that way, and the
reasons for them. Numbers, paths and timeouts are named constants in the code, so this file
names the constant instead of repeating its value. [STYLE.md](STYLE.md) has the coding
standards, and `bin/dev` has the commands. How to use clowder is in the
[README](README.md), [docs/operating.md](docs/operating.md) and
[docs/resources.md](docs/resources.md).

New here? Read [Start here](#start-here-one-apply-through-the-code) first, then the file
about the package you change. This file covers the whole picture and the rules that apply
everywhere: [Getting started](#getting-started), [The big picture](#the-big-picture),
[Words](#words), [The two binaries](#the-two-binaries), [Layers](#layers),
[Host tools](#host-tools-internalcommand), [Volumes](#volumes),
[Logging and errors](#logging-and-errors). Each package has its own section in one of these
files:

| File | Packages |
|---|---|
| [What users write](docs/developing/specs.md) | `names`, `stackspec`, `compose`, `clusterspec`, `clientconfig`, `secretvalue` |
| [The cluster state](docs/developing/state.md) | `state`: commands, resources, operations, host phases, versions |
| [The API and the CLI](docs/developing/api.md) | `api`, `cli`: access, resources, the watch, secret values, joins, credentials |
| [Placement](docs/developing/placement.md) | `scheduler`, and the placement in `controller` |
| [Certificates, Raft and Recover](docs/developing/cluster.md) | `pki`, `raftnode`, `datakey`, `clowderd recover` |
| [A host](docs/developing/host.md) | `agent`, `network`, `storage`, `docker`, `supervisor`, `hostcheck` |
| [Ingress, fencing, certificates, alerts and metrics](docs/developing/loops.md) | `ingress`, `hetzner`, the other loops of `controller`, `notify`, `metrics` |
| [Shipping](docs/developing/shipping.md) | packages and units, CI, releases |

## Getting started

You need Bash and Docker. On Windows, use Git Bash or WSL; Docker Desktop with WSL2 works.
From PowerShell, call the script through Git's Bash: `& "C:\Program Files\Git\bin\bash.exe" bin/dev test`.

```bash
bin/dev test     # unit tests with the race detector, seconds
bin/dev lint     # all style checks
bin/dev build    # binaries into dist/
dist/linux-amd64/clowder version
```

You do not need Go on the host: `bin/dev` runs everything in pinned images and keeps Go's
caches in a Docker volume. If Go is installed, `go test ./...` works the same way.

[docs/live-tests.md](docs/live-tests.md) describes the live tests, which run on real VMs
in HLab. [docs/roadmap.md](docs/roadmap.md) says how far the work is and what comes next.

## The big picture

```
user's machine                 any control-plane agent leader                stack's host
──────────────                 ─────────────────────── ──────                ────────────
clowder apply -f    ──mTLS──▶  API: read, encrypt ──▶ leader decides ──▶ Raft log ──▶ every agent's state
  (resources)                                            │
                                                         ▼
                                                   placement: host, volumes
                                                         │
                                                         ▼
                                                   state says "starting on host 0"
                                                                              │
                                                       stack runner ◀─────────┘
                                                       volumes, secrets, compose up
                                                       supervisor restarts what crashes
```

## Start here: one apply, through the code

This walkthrough follows `clowder apply -f shop/` from the user's machine to the running
containers. Each step names the file and function to read; the package's section explains
the rules.

1. **The CLI reads the files.** `cli/apply.go` `newApplyResources` calls
   `cli/files.go` `readResourceFiles`. It reads the YAML files of the directory in order of
   their names, and the resources in each file from top to bottom. A secret's values are in
   clear (a `Secret`) or sealed for the cluster (a `SealedSecret`). `api.Client.ApplyResources`
   sends the resources in that order in one request (`POST /v1/apply`).
2. **Any control-plane agent takes the request.** `api/changes.go` `applyResources` turns each resource
   into a put (`putChange`). It reads the spec strictly. It opens a SealedSecret's values
   with the sealing key (`unsealSecret`), and it encrypts every secret's values with the
   data key (`encryptSecret`).
3. **The leader decides.** `api/changes.go` `runOnLeader` hands the batch to the leader
   (through `internalChanges` on a follower). `runChanges` turns the batch into one
   `change-resources` command, tries the command on a copy of the state, and proposes it
   only if it changes something.
4. **The state applies the command.** `state/resources.go` `applyChangeResources` applies
   the puts in order on a copy, all of them or none. `applyPutResource` checks who owns
   the resource (not the actor: the cluster has no users), then calls the kind's `put`, here
   `state/stacks.go` `putStack`. New volumes and the new stack are `pending`.
5. **The placement finds a host.** `controller/placement.go` `RunPlacement` runs on the
   leader. `scheduler/place.go` `Plan` picks a host for the stack and a pool for each new
   volume. The controller commits `put-volume` and `set-stack-status starting`.
6. **The host runs the stack.** `agent/volumes/reconciler.go` `Reconciler.pass` sees the
   volume copies placed on its host and a stack that names them. `plan` decides the steps,
   and a job per volume creates the copy, makes it primary and mounts it.
   `agent/stacks/runner.go` `Runner.pass` sees the stack placed on its host and waits until
   its copies are mounted (`stackVolumes`). `agent/stacks/apply.go` `apply` renders the
   Compose file (`render`), and `up` writes the secrets to tmpfs and runs
   `docker compose up`. The host then reports `running` (`reportRunning`, through
   `agent/loop` `CommitAsHost`). If that fails, it reports `failing` with the reason
   (`reportFailing`) and tries again at the next pass.
7. **The supervisor keeps the stack running.** Docker restarts nothing by itself;
   `supervisor.Supervisor` follows Docker's events and restarts what crashes.

What follows from this:

- `clowder get stacks` shows the stack. `clowder stack ps|logs shop` asks the stack's
  host, through any control-plane agent.
- A changed spec raises the stack's generation, and a running stack becomes `starting`
  again. It keeps its host; the placement adds only the volumes that the stack newly names.
  Compose recreates only the containers whose configuration or secrets changed.
- A changed secret recreates the containers that use it, without a new generation.
- Applying the same files again changes nothing.
- A stack or volume that finds no place waits, and `get stacks` shows the reason. A stack
  that is `failing` waits as well, and shows why its host could not bring it up.
- `clowder stack restart shop [app]` recreates the named services (or all of them) and the
  services that depend on them. It pulls their images again and does not create a new
  generation.

## Words

Each word is explained here once, in alphabetical order.

- **active, in service**: an active host does the work of its roles: it takes stacks if it
  is a worker, and may vote if it is a control-plane host. The first host starts active.
  A joining host, and one back from `recovering`, becomes active once its agent reports.
  In service (`state.InService`) means active or draining.
- **actor**: who sent a command: `leader`, `host/<id>` or `cli/<name>`. `<name>` is the
  name in the CLI's certificate (`state.CLIActor`, `state.IsCLIActor`). The cluster has no
  users: the name is for the audit and grants nothing, and whoever holds a CLI certificate
  controls the cluster. A control-plane host may speak for the CLI; a worker acts only for
  itself ([Who may call what](docs/developing/api.md#who-may-call-what)).
- **channel, owner**: the way a resource came in. A resource's owner is the channel that
  made it, and only that channel changes the resource. Today the only channel is `cli`
  (`clowder apply`, whatever the certificate's name).
- **`cli`** names three separate things: the channel above, the role of a CLI certificate
  (its organizational unit, `pki.Role`; not a host's role, which a host certificate names
  as its organizations), and the prefix of the CLI's actor.
- **clowder-system**: the reserved namespace for the cluster's own Secrets
  (`names.System`), like `kube-system`: the Hetzner token and the alert channels' URLs and
  passwords. It always exists and holds no stacks or volumes.
- **configuration**: the cluster's one `ClusterConfiguration` resource, named `cluster`
  (`clusterspec.Config`), as in kubeadm. It holds everything cluster-wide and names its
  secrets in `clowder-system`.
- **context**: the way into one cluster in the CLI's credentials file
  (`~/.clowder/config`, `clientconfig`), like a kubeconfig context. It holds the
  endpoints, the CA, and the CLI's key and certificate; `current_context` picks one.
- **data generation**: of a volume, a number in its LV's name that grows with each new,
  empty LV. A new LV comes when a volume of replicas 0 moves, fails over, or starts anew
  after a recover.
- **data key**: the age key that every host keeps (`datakey`). It encrypts secrets before
  they go into the state ("encrypted with the data key").
- **desired**: what the state wants on this host, read once per pass. The agent compares
  the host with it.
- **endpoint**: an agent address that the CLI asks, from a context's `endpoints`: a host's
  public address, or one in front of the hosts. The CLI tries them in turn and learns no
  others.
- **fencing**: powering off a failed host's server through Hetzner's API before its stacks
  start elsewhere.
- **Floating IP**: the cluster's one public address. Hetzner routes it to the ingress host.
- **generation**: of a resource, the count of its spec's changes. An apply that changes
  nothing does not count.
- **leader**: the Raft leader among the agents. It decides (placement, failover) and
  commits every write.
- **leavers, in use**: leavers are the stacks that left a host because they moved
  elsewhere or were deleted. The host stops them before it starts anything else. A volume
  is in use while a leaver whose stop failed still holds it, and no other stack may take
  the volume until then.
- **node address**: a host's address in the overlay (`network.NodeAddress`), on
  `clowder0`. Other hosts reach it through WireGuard.
- **pass**: one run of a loop that brings its part of the host or the cluster to what the
  state says. A loop runs a pass at every change of the state and every 30 s.
- **pushed out, coming back**: a service stopped to make room for one of higher priority,
  and back once there is room.
- **quorum**: a majority of the voters (Raft) or of a volume's copies (DRBD). Without it,
  nothing changes.
- **retired copy**: a volume's copy that the state gave up: the volume was deleted, the
  copy moved off its host, or its data was given up for a new, empty LV. The data is gone
  only once the host has removed the copy.
- **role**: what a host does, like a Kubernetes node role (`hostrole`), fixed when it
  joins. A `control-plane` host may vote, lead, serve the CLI and the join, and run the
  host commands that need the cluster's keys (`clowderd credentials`, `sealing-key`,
  `recover`). A `worker` runs stacks and holds volume copies with a disk. An `ingress`
  host may take the Floating IP. A tiebreaker needs no role. The join token names the
  roles a host may take, and the host's certificate names the roles it took.
- **room**: what a host offers its services (memory, CPUs) minus what they reserve. It is
  the scheduler's only measure.
- **sealed**: sealed for the cluster's sealing key, as a SealedSecret's value is.
- **sealing key**: the cluster's age key that SealedSecrets are sealed for
  (`state.ClusterKeys.SealingKey`).
- **tiebreaker**: a diskless DRBD member that gives a volume with two copies its third
  vote.
- **underlay**: the Hetzner private network that the hosts share (the configuration's
  `privateNetwork`). A host's underlay is its address there.
- **voter**: a control-plane host that takes part in Raft's elections; the other hosts
  only follow the log. 1, 3 or 5 hosts vote. The second control-plane host waits for its
  vote until a third comes (`state.MinHAHosts`), because two voters would stop when either
  fails.


## The two binaries

| Binary | Package | Runs on | Does |
|---|---|---|---|
| `clowder` | `cmd/clowder`, `internal/cli` | Linux, macOS, Windows | the user's CLI: reads its credentials file and the resource files, talks to an agent over mTLS |
| `clowderd` | `cmd/clowderd`, `internal/agent` | Linux | the agent (`run`, systemd service `clowderd`) and the host commands `check`, `init`, `join`, `credentials`, `sealing-key`, `recover` |

`cmd/clowderd` has the build constraint `linux`. Both binaries get their version from
`internal/buildinfo.Version`. `bin/dev build` sets it from the Git tag, or to
`0.0.0~dev+<commit>` when there is no tag.

## Layers

The package layout is planned for all of v1; packages appear as the phases add them.

- **Portable**: `cli`, `api`, `clusterspec` (the configuration's types and checks),
  `clientconfig` (the CLI's credentials file and its contexts), `secretvalue` (the
  sealed values of SealedSecrets), `pki`, `jointoken` (the tokens that hosts join with),
  `hostrole` (a host's roles), `compose`, `units`, `names` (the rules for the names that
  users give, and the names on the host that are built from them), `buildinfo`, `logging`,
  `atomicfile` (files written whole or not at all), `metrics` (Prometheus' text format), `stackspec` (a
  stack's schema), `resource` (the kinds of resources and their specs), `digest` (hashes of
  lists of parts: the secrets' digests, what an agent applied). The CLI may import only
  these packages. depguard (rule `cli-portable` in `.golangci.yml`) rejects direct imports
  of the Linux-only packages in `cmd/clowder` and `internal/cli`; the cross build for
  Windows and macOS catches indirect ones.
- **Pure logic, no I/O**: `scheduler`, `state`, and the packages the state reads:
  `resource`, `stackspec` and `clusterspec`. Their tests need no host, files or network.
  depguard (rule `state-pure`) keeps out `os`, `net/http` and randomness, and forbidigo
  keeps out the clock (`time.Now`, `time.Since`, `time.Until`).
- **Decides**: `controller` (the leader's loops) writes Raft commands. It talks to external
  services that only the leader uses (Hetzner's API, ACME, alert channels),
  but it never changes the host. One of its loops runs on every agent, not only on the
  leader: `QuorumWatch`, which alerts through the alert channels while there is no leader.
  The clients of these services are separate packages: `hetzner`, `notify` and
  `x/crypto/acme`. Hetzner is the only cloud clowder runs on, with no layer in between;
  `hetzner/hetznertest` fakes its API for the tests. depguard keeps these
  clients out of the CLI, the state and the scheduler. The join, `clowderd recover` and the
  agent's start use `hetzner` too. The agent starts the controller's loops, so `agent`
  imports `controller` and never the other way round. The agent also uses
  `controller.Propose` for the commands it writes itself.
- **Changes the host**: `agent`, `supervisor`, `raftnode` (Raft over mTLS), `datakey` (the
  key file), `command` (host tools with a timeout), and the drivers `network`, `storage`,
  `docker` and `ingress` (Traefik's files; `ingress` renders them, the agent writes them).
  Within the agent, `agent/stacks` is the stack runner, `agent/volumes` is the volume
  reconciler, and `agent/loop` holds what all of the agent's loops share (when a pass runs,
  the supervisor's signal, committing as the host). `agent` imports all three.
  `agent/stacks` and `agent/volumes` import `agent/loop` but not each other; each reaches
  the other through what the agent hands it (an interface, a function). None of them
  imports `agent`. `agent/agenttest` has the fakes that the tests of all four share.
- **Reads the host**: `hostcheck` (for `clowderd check`). It compiles anywhere, but only the
  agent uses it, so depguard keeps it out of the CLI like the packages above.

## Host tools (`internal/command`)

Every host tool runs through `command.Run` or `command.RunWithInput`, without exception
(lvs, drbdadm, mkfs, mount, nft, ip, systemctl, docker, ...). Both give the tool a timeout.
If a tool does not finish in time, the agent leaves it behind and goes on. The reason: while
DRBD suspends I/O, a process that touches a volume can hang in state D, and then not even
SIGKILL ends it. A tool that hangs for any other reason must not stop a loop either.
`command.Stream` (for logs that follow) is the only call without a timeout, so only tools
that touch no volume use it. `command.ExitCode` tells the tool's own exit code apart from a
timeout or a missing binary.

## Volumes

Without quorum, DRBD suspends I/O (`on-no-quorum suspend-io`). A write then hangs in the
kernel in state D, and neither a timeout nor a signal ends it. So the agent never writes to
a volume in its own loops and never waits for a process that does. Work on volumes (freeze,
snapshot, copy) runs in separate processes, and the agent watches them for progress. Health
reports never depend on a volume.

The volume reconciler's passes only read (the state, the LVs, the DRBD roles, the mount
table). Everything that writes to a volume (`mkfs`, `mkdir data/`, a mount, an unmount, a
promotion) runs in a job per volume. The job is a goroutine that the pass does not wait
for, and it runs each tool through `internal/command` with a timeout. A job that hangs holds
up only its own volume and the stacks that wait for it.

There is one exception: the weekly trim (`fstrim`, `agent/volumes/trim.go`) writes to
mounted copies from its own loop and waits for each one. It trims only a copy whose DRBD
has quorum, so fstrim does not start on a volume that hangs. If the quorum is lost while
fstrim runs, its timeout lets the loop go on, and only the trim waits.

## Logging and errors

- `internal/logging` sets up `log/slog`: JSON on stderr for the agent (systemd passes it to
  journald), text for the CLI.
- Errors are wrapped with `%w` and say what was tried. The CLI prints the chain once, at the
  top (`clowder: ...`).
