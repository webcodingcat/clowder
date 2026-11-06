# Developing clowder

This file explains how the code fits together, the rules that keep it that way, and the
reasons for them. Numbers, paths and timeouts are named constants in the code, so this file
names the constant instead of repeating its value. [STYLE.md](STYLE.md) has the coding
standards, and `bin/dev` has the commands.

New here? Read [Start here](#start-here-one-apply-through-the-code) first, then the part
about the package you change. This file covers the whole picture and the rules that apply
everywhere: [Getting started](#getting-started), [The big picture](#the-big-picture),
[Words](#words), [The two binaries](#the-two-binaries), [Layers](#layers),
[Host tools](#host-tools-internalcommand), [Volumes](#volumes),
[Logging and errors](#logging-and-errors). Each package has its own section in one of these
files:

| File | Sections |
|---|---|
| [What users write](docs/developing/specs.md) | namespaces and names, stack specs, rendering, the cluster's configuration, the CLI's credentials, encrypted values |
| [The cluster](docs/developing/cluster.md) | cluster state, certificates, Raft and the data key, API and the CLI, placement |
| [A host](docs/developing/host.md) | the agent, network, storage, containers, host check |
| [Ingress, fencing, certificates, alerts and metrics](docs/developing/loops.md) | Traefik, Hetzner and ingress, fencing, certificates of the routes, alerts, metrics |
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
in HLab. [docs/roadmap.md](docs/roadmap.md) describes the phases, the known gaps and the
results of the phase 0 spike.

## The big picture

```
user's machine                 any agent               leader                stack's host
──────────────                 ─────────               ──────                ────────────
clowder apply -f    ──mTLS──▶  API: read, seal  ──▶  leader decides ──▶ Raft log ──▶ every agent's state
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
   their names, and the resources in each file from top to bottom. A secret's values are
   encrypted in these files. `api.Client.ApplyResources` sends the resources in that order
   in one request (`POST /v1/apply`).
2. **Any agent takes the request.** `api/changes.go` `applyResources` turns each resource
   into a put (`putChange`). It reads the spec strictly, decrypts a secret's values with the
   cluster's key and seals them.
3. **The leader decides.** `api/changes.go` `runOnLeader` hands the batch to the leader
   (through `internalChanges` on a follower). `runChanges` turns the batch into one
   `change-resources` command, tries the command on a copy of the state, and proposes it
   only if it changes something.
4. **The state applies the command.** `state/resources.go` `applyChangeResources` applies
   the puts in order on a copy, all of them or none. `applyPutResource` checks that the
   actor may make the change and who owns the resource, then calls the kind's `put`, here
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
  host, through any agent.
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

| Word | Means |
|---|---|
| underlay | the Hetzner private network that the hosts share; a host's underlay is its address there |
| configuration | the cluster's one `ClusterConfiguration` resource (`clusterspec.Config`), with everything cluster-wide |
| clowder-system | the reserved namespace for the cluster's own Secrets (`names.System`) |
| node address | a host's address in the overlay (`network.NodeAddress`) |
| endpoint | an agent address that the CLI asks, from a context's `endpoints` |
| context | the way into one cluster in the CLI's credentials file (`~/.clowder/config`) |
| Floating IP | the cluster's one public address; Hetzner routes it to the ingress host |
| leader | the Raft leader among the agents; it decides (placement, failover) and commits every write |
| voter | a host that takes part in Raft's elections; other hosts only follow the log |
| quorum | a majority of the voters (Raft) or of a volume's copies (DRBD); without it, nothing changes |
| fencing | powering off a failed host's server through Hetzner's API before its stacks start elsewhere |
| tiebreaker | a diskless DRBD member that gives a volume with two copies its third vote |
| active, passive | the two phases of a host: an active host runs stacks, a passive one waits |
| actor | who sent a command: `leader`, `host/<id>` or `cli/<name>` |
| channel, owner | the way a resource came in; a resource's owner is the channel that made it |
| sealed | encrypted with the cluster's data key before it goes into the state |
| generation | of a resource: counts the changes of its spec |
| data generation | of a volume: a number in its LV's name that grows with each new, empty LV |
| pushed out, coming back | a service stopped to make room for one of higher priority, and back once there is room |
| retired copy | a volume's copy that the state gave up; its host removes it |
| leavers, in use | stacks that left a host; a volume they still hold is in use |
| pass | one run of a loop that brings its part of the host or the cluster to what the state says |
| room | what a host offers its services (memory, CPUs) minus what they reserve |
| desired | what the state wants on this host, read once per pass |

More on some of these words:

- **underlay**: the configuration's `privateNetwork`.
- **configuration**: it is named `cluster` and corresponds to kubeadm's
  `ClusterConfiguration`. It names its secrets in `clowder-system`.
- **clowder-system**: it holds the Hetzner token and the alert channels' URLs and
  passwords, like `kube-system` in Kubernetes. It always exists and holds no stacks or
  volumes.
- **node address**: it is on `clowder0`, and other hosts reach it through WireGuard.
- **endpoint**: the address is a host's public address or one in front of the hosts. The
  CLI's credentials file (`clientconfig`) lists the endpoints of each context. The CLI tries
  them in turn and learns no others.
- **context**: like a context in a kubeconfig, it holds the endpoints, the CA, and the CLI's
  key and certificate. `current_context` picks one.
- **active, passive**: an active host takes and runs stacks and may vote. The first host
  starts active. A host that joins becomes active, and so does one that comes back from
  `recovering`. A passive host is the second host of a cluster that is not yet highly
  available. It waits without stacks and without a vote until a third host joins
  (`state.MinHAHosts`), then it becomes active too.
- **actor**: `<name>` is the name in the CLI's certificate (`state.CLIActor`,
  `state.IsCLIActor`). The cluster has no users: the name is for the audit and grants
  nothing. Whoever holds a CLI certificate controls the cluster, as with kubeadm's
  `admin.conf`. A host is trusted as fully as the CLI. It holds the data key and the CA's
  key, it forwards the CLI's commands with their actor (and could name another actor), and
  it prints credentials for root (`clowderd credentials`). So a compromised host means a
  compromised cluster.
- **channel, owner**: today the only channel is `cli` (`clowder apply`, whatever the
  certificate's name is); a GitOps source will come later. Only the owning channel changes a
  resource. The word `cli` names three separate things: this channel, the role of a CLI
  certificate (its organizational unit), and the prefix of the CLI's actor.
- **generation**: an apply that changes nothing does not count.
- **data generation**: a new, empty LV comes when a volume with replicas 0 moves, fails
  over, or starts anew after a recover. Restores (phase 3) will raise it too.
- **retired copy**: the state gives up a copy when the volume is deleted, when the copy
  moves off its host, or when its data is given up for a new, empty LV. The data is gone
  only once the host has removed the copy.
- **leavers, in use**: leavers are the stacks that left the host because they moved
  elsewhere or were deleted. The host stops them before it starts anything else. A volume
  is in use while a leaver whose stop failed still holds it, and no other stack may take
  the volume until then.
- **pass**: a loop runs a pass at every change of the state and every 30 s.
- **room**: it is the scheduler's only measure.
- **desired**: the agent compares the host with it.

## The two binaries

| Binary | Package | Runs on | Does |
|---|---|---|---|
| `clowder` | `cmd/clowder`, `internal/cli` | Linux, macOS, Windows | the user's CLI: reads its credentials file and the resource files, talks to an agent over mTLS |
| `clowderd` | `cmd/clowderd`, `internal/agent` | Linux | the agent (`run`, systemd service `clowderd`) and the host commands `check`, `init`, `join`, `credentials`, `recover` |

`cmd/clowderd` has the build constraint `linux`. Both binaries get their version from
`internal/buildinfo.Version`. `bin/dev build` sets it from the Git tag, or to
`0.0.0~dev+<commit>` when there is no tag.

## Layers

The package layout is planned for all of v1; packages appear as the phases add them.

- **Portable**: `cli`, `api`, `clusterspec` (the configuration's types and checks),
  `clientconfig` (the CLI's credentials file and its contexts), `secretvalue` (values
  tagged `!secret`), `yamlpath` (the names of places in YAML), `pki`, `jointoken` (the
  tokens that hosts join with), `compose`, `units`, `names` (the rules for the names that
  users give, and the names on the host that are built from them), `buildinfo`, `logging`, `atomicfile`
  (files written whole or not at all), `metrics` (Prometheus' text format), `stackspec` (a
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
  services that only the leader uses (Hetzner's API, ACME, Git remotes, alert channels),
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
