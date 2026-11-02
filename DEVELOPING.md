# Developing clowder

How the code fits together and which rules hold it together, with their reasons. Numbers,
paths and timeouts live in the code as named constants; this file names them instead of
repeating them. [STYLE.md](STYLE.md) has the coding standards, `bin/dev` the commands.

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
| [Ingress, certificates, alerts and metrics](docs/developing/loops.md) | Traefik, hoster and ingress, certificates of the routes, alerts, metrics |
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
2. **Any agent takes the request.** `api/resources.go` `applyResources` turns each
   resource into a `put-resource` command (`putCommand`): it reads the spec strictly, and
   opens a secret's value with the cluster's key and seals it.
3. **The leader decides.** `api/resources.go` `runOnLeader` hands the commands to the leader
   (`internalChanges` on a follower). `runChanges` tries each on a copy of the state and
   proposes only those that change something.
4. **The state applies.** `state/resources.go` `applyPutResource` checks who may and whose
   the resource is, then calls the kind's `put`, here `state/stacks.go` `putStack`. New
   volumes and the new stack are `pending`.
5. **The placement finds a host.** `controller/placement.go` `RunPlacement` runs on the
   leader. `scheduler/place.go` `Plan` picks a host for the stack and a pool for each new
   volume. The controller commits `put-volume` and `set-stack-status starting`.
6. **The host runs it.** `agent/stacks.go` `stackRunner.pass` sees the stack placed on its
   host. `agent/stack_apply.go` `apply` renders the Compose file (`render`), makes the
   volumes (`ensureVolumes`), and `up` writes the secrets to tmpfs and runs
   `docker compose up`. The host reports `running` (`reportRunning`, through
   `agent/loops.go` `commitAsHost`).
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
- A stack or volume that finds no place waits, with the reason in `get stacks`.
- `clowder stack restart shop [app]` recreates the services named (or all) and those that
  depend on them, with their images pulled again, without a new generation.

## Words

| Word | Means |
|---|---|
| underlay | the network the hosts share at the hoster (private network or public addresses) |
| node address | a host's address in the overlay (`network.NodeAddress`); on `clowder0`, reached by other hosts through WireGuard |
| Floating IP | the cluster's one public address; the hoster routes it to the ingress host |
| leader | the Raft leader among the agents; it decides (placement, failover) and commits every write |
| voter | a host that takes part in Raft's elections; others follow the log only |
| quorum | a majority of the voters (Raft) or of a volume's copies (DRBD); without it, nothing changes |
| fencing | powering a failed host off through the hoster's API before its stacks start elsewhere |
| tiebreaker | a diskless DRBD member that gives a volume with two copies its third vote |
| active, passive | a host that runs stacks; a second host that waits until there are three |
| actor | who sent a command: `leader`, `host/<id>` or `user/<name>` |
| channel, owner | the way a resource came in: `cli` (`clowder apply`) or, later, a GitOps source; a resource's owner is the channel that made it, and only that channel changes it |
| sealed | encrypted with the cluster's data key before it goes into the state |
| generation | of a resource: counts the changes of its spec; an apply that changes nothing does not count. A volume's data generation grows with a restore or class move and is part of its LV's name |
| pushed out, coming back | a service stopped to make room for one of higher priority; it comes back once there is room again |
| retired copy | a volume's copy the state gave up (the volume deleted, moved or restored); its host removes it, and only then is the data gone |
| leavers, in use | on a host: the stacks that left it and stop first; the volumes such a stack still uses because its stop failed, which no other stack may take yet |
| pass | one run of a loop that brings its part of the host or the cluster to what the state says; a loop runs a pass at every change of the state and every 30 s |
| room | what a host offers its services (memory, CPUs) less what they reserve: the scheduler's one measure |
| desired | what the state wants on this host, read once per pass; the agent compares the host with it |

## The two binaries

| Binary | Package | Runs on | Does |
|---|---|---|---|
| `clowder` | `cmd/clowder`, `internal/cli` | Linux, macOS, Windows | the user's CLI: reads the cluster directory, talks to an agent over mTLS |
| `clowderd` | `cmd/clowderd`, `internal/agent` | Linux | the agent (`run`, systemd service `clowderd`) and the host commands `check`, `init`, `user recovery-code`; `join` and `recover` to come |

`cmd/clowderd` has the build constraint `linux`. Both get their version from
`internal/buildinfo.Version`, which `bin/dev build` stamps from the Git tag, or
`0.0.0~dev+<commit>` without one.

## Layers

The package layout is planned for the whole of v1; packages appear as the phases add them.

- **Portable**: `cli`, `api`, `clusterspec` (`cluster.yaml`'s types and checks),
  `clusterdir` (the cluster directory's files), `secretvalue` (values tagged
  `!secret`), `yamlpath` (the names of places in YAML), `pki`, `recoverycode` (the codes
  from a host that give access back), `compose`, `units`,
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
  services only the leader uses (hoster API, ACME, Git remotes, alert channels), but never
  changes the host. Their clients sit beside it: `hoster` (with `hoster/hetzner`),
  `notify`, and `x/crypto/acme`; depguard keeps them out of the CLI, the state and the
  scheduler. The agent starts its loops, so `agent` imports `controller`, never the
  other way round; it also takes `controller.Propose` for the commands it writes itself.
- **Changes the host**: `agent`, `supervisor`, `raftnode` (Raft over mTLS), `datakey` (the
  key file), `command` (host tools with a timeout) and the drivers `network`, `storage`,
  `docker`, `ingress` (Traefik's files; it renders, the agent writes).
- **Reads the host**: `hostcheck` (for `clowderd check`). It compiles anywhere, but only the
  agent uses it, so depguard keeps it out of the CLI like the packages above.

## Host tools (`internal/command`)

Every host tool runs through `command.Run` or `command.RunWithInput`, with no exception
(lvs, drbdadm, mkfs, mount, nft, ip, systemctl, docker, ...): with a timeout, and left behind
if it does not finish. A process that touches a volume can hang in state D while DRBD
suspends I/O, and then not even SIGKILL ends it; any other tool that hangs must not stop a
reconciler either. `command.Stream` (logs that follow) is the one without a timeout; only
tools that touch no volume use it. `command.ExitCode` tells a tool's own exit code from a
timeout or a missing binary.

## Volumes

Without quorum, DRBD suspends I/O (`on-no-quorum suspend-io`): a write then hangs in the
kernel in state D, and neither a timeout nor a signal ends it. So the agent never writes to a
volume in its own loops and never waits for a process that does. Work on volumes (freeze,
snapshot, copy) runs in separate processes that the agent watches for progress. Health
reports never depend on a volume.

One exception holds until phase 2: `storage.Ensure` runs `mkfs` and `mkdir data/` from the
stack runner's loop, each through `internal/command` with its timeout, so a hang leaves the
process behind, not the agent; but a long `mkfs` holds up the other stacks of that pass. The
roadmap's gaps of phase 2 name the change (a stack or volume worked on apart).

## Logging and errors

- `internal/logging` sets up `log/slog`: JSON on stderr for the agent (systemd passes it to
  journald), text for the CLI.
- Errors are wrapped with `%w` and say what was tried; the CLI prints the chain once at the
  top (`clowder: ...`).
