# Developing clowder

How the code fits together, and the rules that keep it that way, with their reasons.
Numbers, paths and timeouts are named constants in the code; this file names the constant
instead of repeating its value. [STYLE.md](STYLE.md) has the coding standards, `bin/dev` the commands.

New here? Read [Start here](#start-here-one-apply-through-the-code), then the part about
the package you change. This file has the whole and the rules that hold everywhere:
[Getting started](#getting-started), [The big picture](#the-big-picture),
[Words](#words), [The two binaries](#the-two-binaries), [Layers](#layers),
[Host tools](#host-tools-internalcommand), [Volumes](#volumes),
[Logging and errors](#logging-and-errors). Each package has its section in one of these:

| File | Sections |
|---|---|
| [What users write](docs/developing/specs.md) | namespaces and names, stack specs, rendering, cluster config and directory, encrypted values |
| [The cluster](docs/developing/cluster.md) | cluster state, certificates, Raft and the data key, API and the CLI, placement |
| [A host](docs/developing/host.md) | the agent, network, storage, containers, host check |
| [Ingress, fencing, certificates, alerts and metrics](docs/developing/loops.md) | Traefik, Hetzner and ingress, fencing, certificates of the routes, alerts, metrics |
| [Shipping](docs/developing/shipping.md) | packages and units, CI, releases |

## Getting started

You need Bash and Docker. On Windows, use Git Bash or WSL; Docker Desktop with WSL2 works.
From PowerShell, call it through Git's Bash: `& "C:\Program Files\Git\bin\bash.exe" bin/dev test`.

```bash
bin/dev test     # unit tests with the race detector, seconds
bin/dev lint     # all style checks
bin/dev build    # binaries into dist/
dist/linux-amd64/clowder version
```

Go is not needed on the host: `bin/dev` runs everything in pinned images and keeps Go's
caches in a Docker volume. With Go installed, `go test ./...` works the same.

The live tests with HLab, with real VMs, are in [docs/live-tests.md](docs/live-tests.md);
the phases, the known gaps and what the spike of phase 0 showed are in
[docs/roadmap.md](docs/roadmap.md).

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

Follow `clowder apply -f shop/` from the user's machine to the running containers. Each
step names the file and function to read; the package's section explains the rules.

1. **The CLI reads the files.** `cli/resources.go` `newApplyResources` calls
   `cli/files.go` `readResourceFiles`: the YAML files of the directory, a secret's value
   encrypted in them. `applyRank` sorts them: namespaces, then volumes and secrets, then stacks.
   `api.Client.ApplyResources` sends them in one request (`POST /v1/apply`).
2. **Any agent takes the request.** `api/changes.go` `applyResources` turns each
   resource into a `put-resource` command (`putCommand`): it reads the spec strictly, and
   opens a secret's value with the cluster's key and seals it.
3. **The leader decides.** `api/changes.go` `runOnLeader` hands the commands to the leader
   (`internalChanges` on a follower). `runChanges` tries each on a copy of the state and
   proposes only those that change something.
4. **The state applies.** `state/resources.go` `applyPutResource` checks who may and whose
   the resource is, then calls the kind's `put`, here `state/stacks.go` `putStack`. New
   volumes and the new stack are `pending`.
5. **The placement finds a host.** `controller/placement.go` `RunPlacement` runs on the
   leader. `scheduler/place.go` `Plan` picks a host for the stack and a pool for each new
   volume. The controller commits `put-volume` and `set-stack-status starting`.
6. **The host runs it.** `agent/volumes/reconciler.go` `Reconciler.pass` sees the volume
   copies placed on its host and a stack that names them: `plan` decides the steps, and a
   job per volume makes the copy, makes it primary and mounts it. `agent/stacks/runner.go`
   `Runner.pass` sees the stack placed on its host and waits until its copies are mounted
   (`stackVolumes`). `agent/stacks/apply.go` `apply` renders the Compose file (`render`),
   and `up` writes the secrets to tmpfs and runs `docker compose up`. The host reports
   `running` (`reportRunning`, through `agent/loop` `CommitAsHost`), or `failing` with why
   (`reportFailing`) and tries again at the next pass.
7. **The supervisor keeps it running.** Docker restarts nothing itself;
   `supervisor.Supervisor` follows Docker's events and restarts what crashes.

What follows from there:

- `clowder get stacks` shows the stack; `clowder stack ps|logs shop` asks its host,
  through any agent.
- A changed spec raises the stack's generation, and a running stack is `starting` again.
  It keeps its host; the placement adds only the volumes it newly names. Compose recreates
  only the containers whose configuration or secrets changed.
- A changed secret recreates the containers that use it, without a new generation.
- The same files applied again change nothing.
- A stack or volume that finds no place waits, with the reason in `get stacks`; so does a
  stack that is `failing`, with why its host could not bring it up.
- `clowder stack restart shop [app]` recreates the services named (or all) and those that
  depend on them, with their images pulled again, without a new generation.

## Words

| Word | Means |
|---|---|
| underlay | the Hetzner private network the hosts share (`private_network`); a host's underlay is its address there |
| node address | a host's address in the overlay (`network.NodeAddress`); on `clowder0`, reached by other hosts through WireGuard |
| endpoint | an address in `endpoints` of `cluster.yaml`: an agent the CLI asks, at a host's public address; the CLI learns no others |
| Floating IP | the cluster's one public address; Hetzner routes it to the ingress host |
| leader | the Raft leader among the agents; it decides (placement, failover) and commits every write |
| voter | a host that takes part in Raft's elections; others follow the log only |
| quorum | a majority of the voters (Raft) or of a volume's copies (DRBD); without it, nothing changes |
| fencing | powering a failed host's server off through Hetzner's API before its stacks start elsewhere |
| tiebreaker | a diskless DRBD member that gives a volume with two copies its third vote |
| active, passive | phases of a host. Active: it takes and runs stacks and may vote; the first host starts active, a host that joins becomes active, and so does one back from `recovering`. Passive: the second host of a cluster that is not yet highly available; it waits without stacks and without a vote until a third host joins (`state.MinHAHosts`), then it becomes active too |
| actor | who sent a command: `leader`, `host/<id>` or `cli`; the cluster has no users, whoever holds the CLI's credentials controls it (as kubeadm's `admin.conf`). A host is trusted as fully as the CLI: it holds the data key and the CA's key, forwards the CLI's commands as `cli` and prints credentials for root (`clowderd credentials`), so a compromised host is a compromised cluster |
| channel, owner | the way a resource came in: `cli` (`clowder apply`) or, later, a GitOps source; a resource's owner is the channel that made it, and only that channel changes it |
| sealed | encrypted with the cluster's data key before it goes into the state |
| generation | of a resource: counts the changes of its spec; an apply that changes nothing does not count |
| data generation | of a volume: part of its LV's name; grows with each new, empty LV (a volume of replicas 0 that moves, fails over or starts anew after a recover); restores (phase 3) will raise it too |
| pushed out, coming back | a service stopped to make room for one of higher priority; it comes back once there is room again |
| retired copy | a volume's copy the state gave up (the volume deleted, the copy moved off its host, or its data given up for a new, empty LV); its host removes it, and only then is the data gone |
| leavers, in use | on a host: leavers are the stacks that left the host (moved elsewhere or deleted); they are stopped before anything else starts. A volume is in use while a leaver whose stop failed still holds it; no other stack may take it until then |
| pass | one run of a loop that brings its part of the host or the cluster to what the state says; a loop runs a pass at every change of the state and every 30 s |
| room | what a host offers its services (memory, CPUs) less what they reserve: the scheduler's one measure |
| desired | what the state wants on this host, read once per pass; the agent compares the host with it |

## The two binaries

| Binary | Package | Runs on | Does |
|---|---|---|---|
| `clowder` | `cmd/clowder`, `internal/cli` | Linux, macOS, Windows | the user's CLI: reads the cluster directory, talks to an agent over mTLS |
| `clowderd` | `cmd/clowderd`, `internal/agent` | Linux | the agent (`run`, systemd service `clowderd`) and the host commands `check`, `init`, `join`, `credentials`, `recover` |

`cmd/clowderd` has the build constraint `linux`. Both get their version from
`internal/buildinfo.Version`, which `bin/dev build` stamps from the Git tag, or
`0.0.0~dev+<commit>` without one.

## Layers

The package layout is planned for the whole of v1; packages appear as the phases add them.

- **Portable**: `cli`, `api`, `clusterspec` (`cluster.yaml`'s types and checks),
  `clusterdir` (the cluster directory's files), `secretvalue` (values tagged
  `!secret`), `yamlpath` (the names of places in YAML), `pki`, `jointoken` (the tokens
  hosts join with),
  `compose`, `units`,
  `names` (the rules for the names users give, and the names on the host made of them),
  `buildinfo`, `logging`, `atomicfile` (files written whole or not at all), `metrics`
  (Prometheus' text format), `stackspec` (a stack's schema), `resource` (the kinds of
  resources and their specs), `digest` (hashes of lists of parts: the secrets' digests,
  what an agent applied). The CLI may import only these. depguard (rule `cli-portable` in
  `.golangci.yml`) refuses direct imports of the Linux-only packages in `cmd/clowder` and
  `internal/cli`; the cross build for Windows and macOS catches indirect ones.
- **Pure logic, no I/O**: `scheduler`, `state`, and what the state reads: `resource`,
  `stackspec` and `clusterspec`. Their tests need no host, files or network. depguard
  (rule `state-pure`) keeps `os`, `net/http`, randomness and `clusterdir` out, forbidigo
  the clock (`time.Now`, `time.Since`, `time.Until`).
- **Decides**: `controller` (the leader's loops) writes Raft commands. It talks to external
  services that only the leader uses (Hetzner's API, ACME, Git remotes, alert channels),
  but it never changes the host. One loop of it runs on every agent, not only on the
  leader: `QuorumWatch`, which alerts while there is no leader, through the alerts'
  channels. The clients of these services are separate packages: `hetzner` (the only
  cloud clowder runs on, with no layer in between; `hetzner/hetznertest` fakes its API
  for the tests), `notify`, and `x/crypto/acme`. depguard keeps them out of the CLI, the
  state and the scheduler. The join, `clowderd recover` and the agent's start use
  `hetzner` too. The agent starts the controller's loops, so `agent` imports
  `controller` and never the other way round. The agent also uses `controller.Propose`
  for the commands it writes itself.
- **Changes the host**: `agent`, `supervisor`, `raftnode` (Raft over mTLS), `datakey` (the
  key file), `command` (host tools with a timeout) and the drivers `network`, `storage`,
  `docker`, `ingress` (Traefik's files; it renders, the agent writes). Within the agent,
  `agent/stacks` is the stack runner, `agent/volumes` the volume reconciler and
  `agent/loop` what all of the agent's loops share (when a pass runs, the supervisor's
  signal, committing as the host); `agent` imports all three, `agent/stacks` and
  `agent/volumes` import `agent/loop` and not each other (each reaches the other through
  what the agent hands it: an interface, a function), and none imports `agent`.
  `agent/agenttest` has the fakes the tests of all four share.
- **Reads the host**: `hostcheck` (for `clowderd check`). It compiles anywhere, but only the
  agent uses it, so depguard keeps it out of the CLI like the packages above.

## Host tools (`internal/command`)

Every host tool runs through `command.Run` or `command.RunWithInput`, with no exception
(lvs, drbdadm, mkfs, mount, nft, ip, systemctl, docker, ...). Both give the tool a timeout;
a tool that does not finish in time is left behind and the agent goes on. The reason: while
DRBD suspends I/O, a process that touches a volume can hang in state D, and then not even
SIGKILL ends it. A tool that hangs for any other reason must not stop a loop either.
`command.Stream` (for logs that follow) is the only call without a timeout, so only tools
that touch no volume use it. `command.ExitCode` tells the tool's own exit code apart from a
timeout or a missing binary.

## Volumes

Without quorum, DRBD suspends I/O (`on-no-quorum suspend-io`): a write then hangs in the
kernel in state D, and neither a timeout nor a signal ends it. So the agent never writes to a
volume in its own loops and never waits for a process that does. Work on volumes (freeze,
snapshot, copy) runs in separate processes that the agent watches for progress. Health
reports never depend on a volume.

The volume reconciler's passes only read (the state, the LVs, the DRBD roles, the mount
table). What writes to a volume (`mkfs`, `mkdir data/`, a mount, an unmount, a promotion)
runs in a job per volume, a goroutine the pass does not wait for, and each tool in it
through `internal/command` with a timeout. A job that hangs holds up only its own volume,
and the stacks that wait for it.

One exception: the weekly trim (`fstrim`, `agent/volumes/trim.go`) writes to mounted copies
from a loop of its own and waits for each. It trims only a copy whose DRBD has its
quorum, so fstrim does not start on a volume that hangs; should the quorum go while it
runs, its timeout lets the loop go on, and only the trim waits.

## Logging and errors

- `internal/logging` sets up `log/slog`: JSON on stderr for the agent (systemd passes it to
  journald), text for the CLI.
- Errors are wrapped with `%w` and say what was tried; the CLI prints the chain once at the
  top (`clowder: ...`).
