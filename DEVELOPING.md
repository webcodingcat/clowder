# Developing clowder

How the code fits together and which rules hold it together, with their reasons. Numbers,
paths and timeouts live in the code as named constants; this file names them instead of
repeating them. [STYLE.md](STYLE.md) has the coding standards, `bin/dev` the commands.

New here? Read [Start here](#start-here-one-apply-through-the-code), then the section of
the package you change. The sections:

- The whole: [Getting started](#getting-started), [The big picture](#the-big-picture),
  [Words](#words), [The two binaries](#the-two-binaries), [Layers](#layers).
- What users write: [Namespaces and names](#namespaces-and-names-internalnames),
  [Stack specs](#stack-specs-internalstackspec), [Rendering](#rendering-internalcompose),
  [Cluster config and directory](#cluster-config-internalclusterspec-and-directory-internalclusterdir),
  [SOPS files](#sops-files-internalsopsage),
  [Encrypted values](#encrypted-values-internalsecretvalue).
- The cluster: [Cluster state](#cluster-state-internalstate),
  [Certificates](#certificates-internalpki),
  [Raft and the data key](#raft-internalraftnode-and-the-data-key-internaldatakey),
  [API and the CLI](#api-internalapi-and-the-cli-internalcli),
  [Placement](#placement-internalscheduler-internalcontroller).
- A host: [The agent](#the-agent-internalagent), [Network](#network-internalnetwork),
  [Storage](#storage-internalstorage), [Containers](#containers-internaldocker-internalsupervisor),
  [Host tools](#host-tools-internalcommand), [Host check](#host-check-internalhostcheck-clowderd-check),
  [Volumes](#volumes).
- The leader's loops: [Traefik](#traefik-internalingress-agenttraefikgo),
  [Hoster and ingress](#hoster-and-ingress-internalhoster-internalcontroller),
  [Certificates of the routes](#certificates-of-the-routes-controlleracmego),
  [Alerts](#alerts-internalnotify-controllerwantedgo-controlleralertsgo),
  [Metrics](#metrics-internalmetrics-agentmetricsgo).
- Shipping: [Logging and errors](#logging-and-errors), [Packages and units](#packages-and-units),
  [CI](#ci), [Releases](#releases).

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
step names the file and function to read; the sections below explain the rules.

1. **The CLI reads the files.** `cli/resources.go` `newApplyResources` calls
   `cli/files.go` `readResources`: the YAML files of the directory, a secret in a SOPS file
   of its own. `applyRank` sorts them: namespaces, then volumes and secrets, then stacks.
   `api.Client.ApplyResources` sends them in one request (`POST /v1/apply`).
2. **Any agent takes the request.** `api/resources.go` `applyResources` turns each
   resource into a `put-resource` command (`putCommand`): it reads the spec strictly and
   seals a secret's value.
3. **The leader decides.** `decide` hands the commands to the leader
   (`internalChanges` on a follower). `runChanges` tries each on a copy of the state and
   proposes only those that change something.
4. **The state applies.** `state/resources.go` `applyPutResource` checks who may and whose
   the resource is, then calls the kind's `put`, here `state/stacks.go` `putStack`. New
   volumes and the new stack are `pending`.
5. **The placement finds a host.** `controller/placement.go` `RunPlacement` runs on the
   leader. `scheduler/place.go` `Plan` picks a host for the stack and a pool for each new
   volume. The controller commits `put-volume` and `set-stack-status starting`.
6. **The host runs it.** `agent/stacks.go` `stackRunner.pass` sees the stack placed on its
   host. `apply` renders the Compose file (`compose.Render`), makes the volumes
   (`storage.Ensure`), writes the secrets to tmpfs, and `up` runs `docker compose up`. The
   host reports `running` (`commitAsHost`).
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

## The two binaries

| Binary | Package | Runs on | Does |
|---|---|---|---|
| `clowder` | `cmd/clowder`, `internal/cli` | Linux, macOS, Windows | the user's CLI: reads the cluster directory, talks to an agent over mTLS |
| `clowderd` | `cmd/clowderd`, `internal/agent` | Linux | the agent (`run`, systemd service `clowderd`) and the host commands `check`, `init`; `join` and `recover` to come |

`cmd/clowderd` has the build constraint `linux`. Both get their version from
`internal/buildinfo.Version`, which `bin/dev build` stamps from the Git tag, or
`0.0.0~dev+<commit>` without one.

## Layers

The package layout is planned for the whole of v1; packages appear as the phases add them.
Packages in *italics* do not exist yet.

- **Portable**: `cli`, `api`, `clusterspec` (`cluster.yaml`'s types and checks),
  `clusterdir` (the cluster directory's files), `sopsage`, `secretvalue` (values tagged
  `!secret`), `yamlpath` (the names of places in YAML), `pki`, `compose`, `units`,
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
  scheduler.
- **Changes the host**: `agent`, `supervisor`, `raftnode` (Raft over mTLS), `datakey` (the
  key file), `command` (host tools with a timeout) and the drivers `network`, `storage`,
  `docker`, `ingress` (Traefik's files; it renders, the agent writes).
- **Reads the host**: `hostcheck` (for `clowderd check`). It compiles anywhere, but only the
  agent uses it, so depguard keeps it out of the CLI like the packages above.

## Namespaces and names (`internal/names`)

- **Namespaces** keep resources apart: two namespaces may each have a stack, a volume and a
  secret `shop`. The bootstrap makes `default`. Users and the state name a resource
  `<namespace>/<name>` (`names.ID`, the key of `State.Stacks`, `Volumes`, `Secrets`).
- **One rule for every name** (`names.Valid`): a DNS label of at most `names.MaxLength`
  characters, lowercase letters, digits and `-`. No `_`: on the host it joins a namespace and
  a stack, so it separates them unambiguously. Namespaces must not start with `clowder-`,
  the prefix of clowder's own projects (Traefik). Stacks, services, volumes and secrets all
  follow the rule.
- **The names on the host are built here, and only here**, from the namespace and the stack,
  so two namespaces never share one:

  | What | Name |
  |---|---|
  | Compose project, the agent's stack directory, the secrets' directory, the supervisor's hold, Traefik's service | `<namespace>_<stack>` (`names.Project`) |
  | the stack's one Docker network | `<namespace>_<stack>_net` (`names.StackNetwork`) |
  | container | `<namespace>_<stack>_<service>` (`names.Container`) |
  | volume ID | `<namespace>/<volume>` (`names.ID`) |
  | LV, DRBD resource, mount point, Docker volume | `clw-<readable>-<hash8>-g<data generation>` (`names.VolumeName`) |
  | the agent's record of a volume's copies | `volume-records/<namespace>_<volume>.json` (`names.FileName`) |
  | bridge | `cbr-` and 11 hex digits of a hash of the stack's ID (`names.BridgeName`) |
  | locks, alert keys | `stack/<namespace>/<stack>`, `volume/<volume ID>`, `stack-waiting/<namespace>/<stack>` |
  | metrics | the labels `namespace` and `stack` |

- **The LV name has the tightest limits:** LVM and DRBD take 127 characters, device-mapper
  128 for `<vg>-<lv>`, in which it writes every `-` twice. So `VolumeName` cuts the readable
  part (the ID with `/` as `-`) to 24 characters and adds the first 8 hex digits of a hash of
  the whole ID, which also keeps `a-b/c` apart from `a/b-c`; the generation has at most six
  digits (`names.MaxDataGeneration`, the state refuses more), a pool's volume group at most
  `names.MaxVolumeGroupLength` characters (`clusterspec` refuses more). The worst case is 107
  of 128 characters in device-mapper (`TestVolumeNamesFitDeviceMapper`): the margin is
  small, so a longer part needs the arithmetic again.

## Stack specs (`internal/stackspec`)

A stack is a resource with a schema of clowder's own; Compose is only how the agent runs it
on a host (`compose.Render`).

- **Only what clowder knows runs.** `Decode` reads YAML (and so JSON) strictly: a field the
  schema does not have is an error. `env` takes any scalar and keeps its text as written
  (`8080`, `1.0`, `yes`); the CLI keeps a number's text when it turns YAML into JSON
  (`resource.ParseDocuments`), so `1.0` arrives as `"1.0"`. Only a number JSON cannot
  write as it stands arrives as YAML reads it (`0x10` as `"16"`), and `True` as `"true"`.
- **What would break clowder's model has no field**: nothing ties a container to one host
  (no bind mounts but `/etc/localtime` read-only, no devices), nothing shares the host's
  namespaces, and clowder sets the rest itself (names, networks, the restart policy). A
  route's host must be a domain name and its path a URL path: both go into a Traefik rule.
- **`Check`** lists every problem at once (`Problems`): names, image, mounts at clean
  absolute paths, ports that are not reserved (`ReservedPort`), two services with the same
  published port or route, routes, dependencies that exist and go in no circle, durations,
  sizes, restart rules. What only the cluster knows (priority categories, privileges, the
  namespace's volumes and secrets, other stacks' ports and routes) the state checks.
- **`Normalize`** fills in the schema's own defaults (`restart: always`, `restartUnhealthy`,
  a port's `published` and `protocol`, a route's path) and writes in one way what may be
  written in several (sorted sets, a route's host in lower case, capabilities without
  `CAP_`), so two specs that mean the same are equal. Defaults of `cluster.yaml`
  (reservations, priority) stay out: the scheduler takes them when it places, so a changed
  `cluster.yaml` changes no spec.
- **Privileges are the cluster's to give.** What reaches beyond the container
  (`privileged`, a capability outside `harmlessCapabilities`, a `securityOpt` other than
  `no-new-privileges`) is listed per service (`Service.Privileges`). Such a container can
  read the host's keys and other stacks' volumes and secrets, and change the rules that keep
  containers apart. So only stacks that `cluster.yaml` names in `stacks.privileged` (as
  `<namespace>/<stack>`) may have it; the state refuses the stack otherwise, and an
  `apply-config` that takes such a stack off the list.
- **Secrets.** A service names secrets of its namespace as files (`secrets`, at
  `/run/secrets/<name>`, for images that read `*_FILE`) or as variables (`secretEnv:
  {VARIABLE: secret}`). A variable also ends up in Docker's container configuration on the
  host's disk and in `docker inspect`; a file does not.
- **No I/O, no clock**: the state reads specs, so depguard and forbidigo hold `stackspec` to
  the state's rules.
- **A new field:** its type with both tags (`yaml`, `json`) and a comment in `spec.go`, its
  check in `check.go`, its default or order in `Normalize` if it has one, its rendering in
  `internal/compose/render.go`, the field in `internal/compose/testdata/spec/stack.yaml`,
  then `bin/dev test ./internal/compose -update` and a look at the golden file's diff.

## Rendering (`internal/compose`)

- **`Render`** writes the file docker compose gets on a host from a stack's spec, with its
  own types: the stack's project `<namespace>_<stack>` (`names.Project`) as its name, one
  network (`names.StackNetwork`) with a fixed bridge name (`names.BridgeName`), container
  names without Compose's replica number (`names.Container`), `restart: "no"` (the agent
  restarts), the agent's resolver, each port on the Floating IP and the node address,
  volumes bound to `<mount>/data` under the copy's LV name, secrets as files in
  `Target.SecretsDir`; a dependency on a service with a healthcheck waits until it is
  healthy. HTTP routes are not in it: Traefik routes them. `RenderSecretEnv` writes the
  secret variables as a second Compose file. Every `$` in a value becomes `$$`
  (`escapeDollars`): docker compose on the host would fill in variables from the agent's
  environment.
- **Labels, and which change recreates what.** Every container gets `clowder.stack` (its
  project); one that asks for secret files also gets `clowder.secrets`, a digest of them
  (`Target.SecretLabels`); one a user restarted gets `clowder.restarted`, the time of the
  restart (`Target.Restarted`), so a new restart recreates exactly its services. Nothing
  else: Compose recreates exactly the containers whose configuration changed, and a
  changed secret file keeps its path, so only the label tells Compose. Secret variables
  are in the second Compose file, which Compose compares itself, and a volume's new data
  generation is a new volume name. The label is an HMAC-SHA256
  (`digest.HMAC`) with a key derived from the data key, over the names and the state's
  digests of the service's secret files: it is on disk and in `docker inspect`, and says
  nothing about a short password. A change to these labels changes every container's
  configuration: an agent of a release with other labels recreates all containers once
  after the upgrade.
- **Golden files.** `testdata/spec/rendered.golden.yaml` and `secrets.golden.yaml`, from
  `testdata/spec/stack.yaml`. After an intended change: `bin/dev test ./internal/compose
  -update`, then review the diff. `bin/dev test-tools` checks both with Debian's
  `docker-compose`, and that it fills in nothing.

## Cluster config (`internal/clusterspec`) and directory (`internal/clusterdir`)

- `clusterspec` is `cluster.yaml` without files: its types (`Config`), their defaults
  (`WithDefaults`) and `Check`, which lists every problem. The state reads it, so it does
  no I/O.
- `clusterdir` reads the directory. `Find` picks it (`--cluster`, `$CLOWDER_CLUSTER`, the
  working directory), `Load` reads `cluster.yaml` strictly and runs `clusterspec.Check`.
  `Schema` makes the JSON schema of `cluster.yaml` from the Go types; it lives here so the
  state does not link the schema library.
- `ReadSOPS` decrypts a SOPS file with the user's age key, as SOPS finds it. It refuses a
  file with values left in clear (a rule like `unencrypted_suffix`): these files hold
  secrets. `Secrets` reads through it; `DecryptSOPS` does the same for content read
  elsewhere (a `Secret` resource on stdin or in a file of `apply`).
- `WriteSOPS` and `WriteSOPSConfig` write a SOPS file and the `.sops.yaml` beside it, for
  `cluster bootstrap`. `testdata/test-age-key.txt` is a test-only key for the fixtures.
- **The user's credentials** (`credentials.go`) are not in the cluster directory, which a
  team shares in a repository, but in the user's config directory
  (`os.UserConfigDir()/clowder/clusters/<name>/credentials.yaml`, `CredentialsPath`): the
  user's name, key and certificate, and the cluster's CA, which the CLI trusts. Plain YAML,
  0600 in a directory of 0700, as SSH keeps its keys. `<name>` is `cluster.yaml`'s `name`,
  so `clusterspec.Check` holds it to `names.Valid`: it becomes a directory. One file per
  cluster, one user per file. The tests point `XDG_CONFIG_HOME` (and `HOME`, `AppData`) at
  a temporary directory.
- **`cluster bootstrap`** checks first (`CheckBootstrap`) that it writes over neither a
  `cluster.yaml` nor credentials of a cluster of the same name (`--force` replaces those),
  since once the cluster has its first user a bootstrap cannot be repeated. It writes the
  credentials before the cluster directory: `cluster.yaml` can be written by hand, the
  first user's key cannot be had again.
- **Files next to `cluster.yaml`** (`acme.ca`): `clusterspec.FileNames` lists those it
  names, by paths relative to it and written with `/` (the CLI runs on Windows too), and
  `Check` refuses
  absolute ones, `..` and more than 8. `cluster apply` reads them (`Directory.Files`) and
  sends their contents along; `CheckFiles` refuses a missing one, one over 64 KiB and an
  `acme.ca` without a certificate, in the CLI and again in the state, which keeps them with
  the config (`ClusterConfig.Files`). `cluster diff` names those that differ.
- Durations (`acme.renew_before`, `alerts.heartbeat.interval`) are read by
  `units.ParseDuration`: Go's format, which may start with whole days (`30d`, `1d12h`).

## SOPS files (`internal/sopsage`)

The SOPS library brings the SDKs of every cloud KMS along (the CLI grew from 2.5 to 44 MB),
so `sopsage` implements the part of the SOPS format clowder needs: YAML files with age keys,
read and written.

- `Decrypt` opens the data key with one of the identities, decrypts every value (AES-256-GCM,
  the key path as additional data) and checks the MAC; `Encrypt` writes what `sops encrypt`
  writes with default settings. Comments are dropped. `LeftInClear` names the values SOPS
  kept in clear.
- No error message carries a value: a file may be read by a wrong key or be broken, and the
  message goes to a terminal or a log.
- `LoadIdentities` looks where SOPS looks: the SSH key, `SOPS_AGE_KEY`, `SOPS_AGE_KEY_FILE`
  or `sops/age/keys.txt` in the user's config directory. `SOPS_AGE_KEY_CMD` is not
  supported.
- Files with settings it does not implement (`*_regex`, key groups, no age recipient) are
  refused, not guessed at.
- `testdata/plain*.sops.yaml` were written by the real sops from `testdata/plain.yaml`.
  `bin/dev test-sops` runs `cross_test.go` (build tag `sops`) against Alpine's sops in both
  directions.

## Encrypted values (`internal/secretvalue`)

A secret's value can stand in a YAML file encrypted, tagged `!secret`: age's ASCII armor
in a literal block scalar. The CLI and the agent do not use it yet; it is to replace the
SOPS files.

- **The envelope.** What age encrypts is JSON, `{"v":1,"for":"<place>","value":"<Base64>"}`.
  `for` is the place the value is for (a secret's ID, or `cluster` for `cluster.yaml`), so
  whoever opens it can refuse a value copied elsewhere; `Open` returns it and the caller
  checks. The value is Base64: no escaping that changes its size, any bytes. A higher `v`
  is refused as written by a newer clowder.
- **Before decoding.** yaml.v3 decodes an unknown tag as a string in places, and turning
  YAML into JSON drops it. So `Extract` reads the node tree first: it takes each value out
  by its path (`internal/yamlpath`: `hoster.token`, `join_keys[0]`,
  `registries["ghcr.io"].password`), puts the path in its place as a plain string, and the
  caller decodes as before. It knows paths, not kinds of files: which paths may hold a
  value, the caller checks.
- **Only `!secret |`** (also `|-`, `|+`), on one line, with the armor below. Quoted, plain,
  folded and indented-indicator forms, anchors and tags on keys, mappings or lists are
  refused: only a block on lines of its own can be replaced in place.
- **`Rewrite`** replaces the armor of each value with new armor and leaves everything else
  of the file as it is (comments, indentation, other documents, CRLF line ends). yaml.v3
  gives a node's first line only, so `locate` finds the block's end by its indentation, as
  YAML does, and checks the lines it found against the value YAML read before it cuts.
  Golden file: `testdata/rewrite.yaml`, `bin/dev test ./internal/secretvalue -update`.
- No error message carries a value: `Open` drops json's messages, which quote the text
  they choke on.

## Cluster state (`internal/state`)

Every agent holds the same `State` and applies the same `Command`s in Raft's order.

- **Deterministic.** `Apply` uses no clock (a command carries the leader's time in `At`), no
  randomness and no I/O; lint bans the clock from the package (forbidigo). A handler checks
  everything before it changes anything, so a refused command leaves no trace; the state's
  `Index` follows the log either way.
- **Commands.** A command has a kind, a payload of the type with the same name (`AddHost`,
  `PutResource`, ...) and an actor (`leader`, `host/<id>`, `user/<name>`) that the API takes from
  the caller's certificate. Which actor may send which kind is the API's check. The state
  checks only what it alone can tell: a host reports only about itself and never moves
  another host, and about a stack only while it runs there, without moving it.
- **In use so far:** `bootstrap` (init), `apply-config`, `add-user` (the first user),
  `set-host-phase` (host actions), `put-resource` and `delete-resource` (the user, through
  `/v1/apply` and `/v1/delete`, decided on the leader), `restart-stack` (`stack restart`),
  `put-volume`, `set-stack-status`, `set-stack-wait-reason` and `set-pushed-out`
  (placement, the stack's host), `set-ingress` (the ingress loop), `put-acme-account`,
  `put-acme-challenge`, `delete-acme-challenge`, `put-certificate` and
  `delete-certificate` (the ACME loop), `put-alert` and `resolve-alert` (the alerts), `report-host` and `forget-copy` (each agent). The other
  kinds (hosts joining, operations, users beyond the first) are there for the next steps;
  they are tested, but nothing sends them yet.
- **Resources** (phase 1c): namespaces (`Namespaces`), and by `<namespace>/<name>` stacks
  (`Stacks`), volumes (`Volumes`) and secrets (`Secrets`), each with a `Meta`. The kinds
  and their specs are in `internal/resource` (a stack's in `internal/stackspec`), which the
  CLI and the API share. Three counters, each with one job:

  | Counter | Counts |
  |---|---|
  | `Meta.Generation` | the changes of a resource's spec |
  | `Meta.ResourceVersion` | the index of the command that last changed the resource, spec or status |
  | `Volume.DataGeneration` | a volume's data: a restore or a class move makes a new LV (`-g<N>`) |

  - `put-resource` and `delete-resource` carry the kind; `resourceKinds` holds each kind's
    `find`, `put` and `remove`. A handler checks in a fixed order: `Authorize` (the actor,
    the verb, the kind, the namespace; it allows everything until users get permissions,
    and sits here so that the leader decides, not the agent that took the request), the
    owner (another channel is refused, even for a spec that would change nothing), whether
    anything changes, then the rest.
  - **Unchanged is not a change.** A spec equal to the stored one (a secret's by its
    digest, as the same value seals differently every time; a stack's normalized) changes
    nothing: not `Generation`, not `ResourceVersion`, and leaves no audit entry, so a GitOps
    loop that applies the same files restarts nothing and keeps the audit. The status
    commands (`put-volume`, `set-stack-status`, `set-stack-wait-reason`, `set-pushed-out`)
    that write what is there change nothing either. They answer with `errUnchanged`, which
    `Apply` takes as applied without a trace.
  - **The state checks against `cluster.yaml` itself**, in the log's order: a stack's
    priority categories and privileges, a volume's storage class; `apply-config` refuses
    what a resource of the cluster would no longer pass. No check happens outside the state
    that a later command could make stale, so no revision travels with a command.
  - `Apply` returns the resources a command changed (`[]Change`, with the command's index
    as their `ResourceVersion`); a handler records each with `changed` or `deleted`.
  - A namespace's owner owns only the namespace; any channel may put resources into it.
    `default` belongs to no channel and cannot be deleted. A namespace goes only when it
    holds nothing; nothing cascades.
  - A volume is made `pending`, without a copy and without a DRBD minor: the placement
    places it with the first stack that names it. Its class must be in `cluster.yaml`. Its
    spec cannot change yet. A new volume starts one `DataGeneration` above the retired
    copies of a deleted one of the same ID, and the state refuses an LV name a volume or a
    retired copy has already (the name holds only 32 bits of a hash). `put-volume` changes
    a volume's copies and status, never its `Meta` or spec. A volume serves one stack; one
    a stack names cannot be deleted, and a deleted stack leaves its volumes, with their
    data, for the next stack that names them.
  - A secret's value is sealed; the API computes its digest. The limits are on the values
    in clear (`resource.MaxSecretBytes`, per namespace, in the cluster), which the state
    reads from the sealed length (`ClearSize`: age adds a header of fixed length and a tag
    per 64 KiB); a test in `datakey` holds that against real sealing. A secret a stack
    names cannot be deleted.
  - **A stack's spec** is stored normalized (`stackspec.Normalize`). The state reads it
    itself, so nothing derived travels with the command. `putStack`:
    - runs `stackspec.Check` and resolves the priorities with `cluster.yaml`'s categories;
    - checks privileges against `stacks.privileged`;
    - refuses a published port or HTTP route that another stack has, and volumes or
      secrets its namespace lacks;
    - keeps what the scheduler needs of each service (`Stack.Services`: the priority as
      written, the reservations, the dependencies).
  - **A stack's status.** A new stack is `pending`. A changed one that runs is `starting`
    until its host reports the new generation up. A host's `set-stack-status` names the
    generation and the restart times it ran with; one about an older generation or other
    restart times is refused, so a report from before an apply or a restart cannot mark
    the stack `running`. With `running` the host
    reports the digests of the secrets its containers came up with (`AppliedSecrets`).
  - **`restart-stack`** records the time of a restart per service (`Stack.Restarted`): for
    the services named (none: all) and every service that depends on one of them
    (`stackspec.Spec.WithDependents`), since a dependency holds only when a container
    starts. It changes no spec, so the generation stays; a running stack is `starting`
    again. Any channel may restart a stack, whoever owns it, so an operator can restart one
    that GitOps applies. Who may is `Authorize`'s (verb `restart`).
  - **`set-stack-wait-reason`** records why the placement finds no place for a stack or a
    volume it names (`WaitReason`). It changes nothing else, so it never unplaces a stack
    that runs. The state is the placement's memory of the reason: it writes a change once,
    also after a change of leader, and clears it when the stack no longer waits.
- **Retired copies.** A volume copy's data goes only when the state says so:
  `delete-resource` of a volume, and a `put-volume` that changes the data generation or
  takes a copy off a host each add the copies they give up to `Retired` (volume, data
  generation, host). The host removes those and forgets each with `forget-copy`; a removed
  host's go with it. A copy that is merely missing from the state stays: a bug in the
  placement must not cost data.
- **Host phases** (`phaseChanges`; L = the leader, on its own or for a user; H = the host
  itself):

  ```
  joining ─L→ passive ─L→ active ⇄L draining
     └──────L──────────────↗  │ L        │ L
                         failed ←────────┘
             active ←H── failed ─L→ fenced ─H→ recovering ─H→ active
                                    fenced ⇄L disabled (flapping brake)
  any phase ─L→ removed
  ```

  In words: a new host joins as `joining`; the leader makes it `passive` (waiting) or
  `active` (running stacks), and drains it for maintenance and back. A host that stops
  answering is `failed`; it comes back `active` by itself, or the leader powers it off
  (`fenced`) before its stacks start elsewhere. A fenced host that is back recovers
  (`recovering`, then `active`); one that fails again and again is `disabled` until a user
  enables it. Any host can be `removed`.

- **Versions.** Until `v1.0.0` the state, the commands and `cluster.yaml` change freely:
  no cluster runs in production, so there are no migrations and no gates, and
  `CommandVersion` and `SnapshotVersion` stay at 1. **From `v1.0.0`** these rules hold:
  - A release reads its own `CommandVersion` and `SnapshotVersion` and the one before.
  - Payload fields are added, not changed. Unknown fields are ignored, so an agent of the
    release before drops a new field from its state. A field that later commands depend
    on therefore needs a gate on `State.MinVersion`.
  - An agent of the release before refuses a new command kind ("unknown command"), so its
    state misses that command. A kind may come without a gate only if what it changes may
    differ between agents for a while, as the display-only `set-stack-wait-reason`.
  - The json tags of `clusterspec.Config` are part of this format.
- **Locks.** Operations hold locks (`stack/<namespace>/<stack>`, `volume/<volume ID>`); a
  change to a locked stack or volume is refused with a `LockedError`, unless the holding
  operation makes it (`Operation` in the payload). Only failover takes locks over
  (`TakeOver`).
- **Invariants** the state holds itself, as the last place to catch a wrong command: one
  first user, valid names (`internal/names`), volumes with 0 to 3 replicas and a DRBD minor
  exactly when replicated (a pending volume has neither copy nor minor), one copy per host,
  one stack per volume, nothing deleted that a stack names, a removed host's certificate
  revoked.
- **Secrets** arrive sealed (`Sealed`, age for the data key); `state` never sees them in
  clear. Those `cluster.yaml` names are `ClusterSecrets`, the namespaces' `Secrets`. A
  stack names at most `MaxStackSecrets`. Who can read a secret: API, below.
- `Copy` gives a deep copy without the audit, for work too slow for the state's lock (the
  placement weighs every stack) or that keeps parts of the state (the agent's stacks).
- Snapshots are gzip-compressed JSON (`WriteSnapshot`, `ReadSnapshot`). The audit keeps the
  last `auditSize` commands, finished operations the last `keptFinished`.

## Certificates (`internal/pki`)

- The cluster CA issues host certificates (server and client, valid for the host's addresses)
  and user certificates (client). The role is the certificate's organizational unit, the
  name its common name (`host-<id>` or the user's name).
- Keys are Ed25519 and never leave the machine that made them: the CA signs public keys.
- Certificates end by revocation, not expiry: hosts may stay off for months. `Verify` takes
  the revoked serials from the state.
- `TrustProof` (HMAC-SHA256 with the bootstrap key over the CA certificate) is how a CLI or a
  joining host trusts the CA on first contact.

## Raft (`internal/raftnode`) and the data key (`internal/datakey`)

- `raftnode` runs hashicorp/raft: log and stable store in BoltDB, a snapshot every
  `snapshotThreshold` entries, the state of `internal/state` as the replicated machine.
  Raft's own log lines go through `log/slog`.
- Raft speaks mutual TLS on the node address: both sides show a host certificate of the
  cluster CA that is not revoked. Users' certificates and other clusters' hosts fail the
  handshake, and removing a host revokes its certificate, so it leaves Raft and the API at
  once. `pki.ClientTLSToHosts` and `pki.ServerTLSForHosts` are the one way hosts are checked,
  by Raft and by the API alike: the chain and the role, not the name.
- `Propose` commits a command through the leader and returns its index, or the state's
  refusal. `Read` gives the current state under a lock; nothing may keep it, as `Apply`
  changes it in place. `WaitIndex` lets a read follow a write made through another member.
  `Changes` answers the watch from the member's ring of changes (API, The watch).
- `Bootstrap` makes the first host a cluster of its own; on a member with a log it does
  nothing. `AddMember` is for joins.
- The data key is an age identity in a file only root reads. `Seal` and `Open` turn secrets
  into `state.Sealed` and back; joining hosts get the key with their join answer.
- The tests run real members on 127.0.0.1: a restart from disk, three hosts over mTLS,
  strangers refused.

## API (`internal/api`) and the CLI (`internal/cli`)

- **Every call is checked.** Every agent serves `/v1` with mutual TLS (`agent.APIPort`), and
  each call is checked against the CA and the state's revocation list, so a revoked
  certificate is refused at once on every agent. User routes take user certificates,
  `/v1/internal/*` only host certificates. Every request body is limited
  (`maxRequestBytes`).
- **Any agent answers.** Reads come from its own copy of the state; writes it commits as the
  leader or forwards to the leader's `/v1/internal/propose`. A write answers with its Raft
  index (`X-Clowder-Index`); the client sends it on with later reads (`X-Clowder-Min-Index`),
  and the agent waits until it has applied it.
- **Who speaks.** The actor of a command is the caller of the first agent; its time (`At`) is
  the leader's clock. `/v1/internal/propose` takes two things only: a user's command that a
  follower forwards (`userKinds`, exactly the kinds the user routes and the bootstrap make),
  and a host's report about itself (`hostKinds`); `/v1/internal/changes` takes a user's
  resource commands only (Resources, below). The leader's own commands (operations,
  placements, new hosts) are made on the leader and never come in from outside; a host never
  speaks for another host or for the leader.
- A refusal of the state is HTTP 409 with its reason; a write that did not commit is 503.
- **Resources** (`resources.go`; the CLI's `apply`, `get` and `delete` below):
  - `POST /v1/apply` (`ApplyRequest`) and `POST /v1/delete` (`DeleteRequest`) take
    resources in order and answer per resource (`ChangeResult`): `applied` or `deleted`,
    `unchanged`,
    `refused` (the state's no), `failed` (did not commit: no leader, no quorum; try again),
    and `not-tried` after a refusal or failure, since what follows usually needs it. No
    transaction: a repeat converges. They are the only way to change resources; every
    user's change is the channel `cli`. Requests are read strictly, so a typo is an error,
    not an empty request; the internal routes read leniently, as an agent of the next
    release may send fields this one does not know.
  - The agent that took the request turns each resource into its `put-resource`
    (`putCommand`): the spec read strictly (`resource.DecodeVolumeSpec`, a field the kind
    does not have is an error; `replicas` has no default), a secret's value sealed and its
    digest computed (`sealSecret`: `digest.HMAC` over the ID and the value, with a key
    `Keys.Derive` gives for "clowder secret resource digest", apart from the containers'
    labels).
  - **The leader decides** (`decide`): a follower hands the commands on as a batch to
    `/v1/internal/changes`, since its own state may be behind and would answer
    `unchanged` or `refused` for what has changed since. `runChanges` applies each command
    to a copy of the leader's state first (`State.Copy`), with every check of the state: a
    refusal answers at once, an unchanged resource is not proposed (no Raft entry), and
    `?dryRun=true` ends there and answers what a real apply would, with index 0, so a
    volume in a namespace the same request makes passes too. Only a change is proposed;
    the state checks it once more as it commits.
  - `GET /v1/resources/{kind}` lists the resources of `?namespace=` or of
    `?allNamespaces=true` (no default, so no reader gets the wrong namespace by
    accident), with the state's index it read at (for the watch); `GET
    /v1/resources/{kind}/{namespace}/{name}` and `/v1/resources/namespace/{name}` read one.
    The kind is written in lower case. A secret shows its digest, never its value; any
    other resource a read answers applies again as it is (`resource.Metadata`'s owner,
    generation and resource version and the status are the cluster's, an apply ignores
    them).
  - **The watch** (`watch.go`): `GET /v1/watch?kind=&namespace=|allNamespaces=true&since=`
    streams the changes of a kind after `since`, the index of the client's list, one JSON
    object per line (`WatchEvent`: the resource, its resource version, whether it is gone).
    Spec and status changes are both events. The client reads a resource itself when it
    needs it.
    - Every agent keeps the last `state.ChangeRingSize` changes (`state.ChangeRing`),
      filled only from the Raft log (`raftnode`'s `fsm.Apply`), so a dry run never reaches
      it. Hosts' reports about themselves are no change, so on a quiet cluster the ring
      covers hours.
    - The ring knows the index it is complete from: the snapshot the agent started from or
      installed, or the oldest change it dropped. A `since` before that answers 410, and
      the client lists afresh, as with Kubernetes. So a client that listed on one agent
      can watch on another.
    - A `since` this agent has not applied yet waits at most `watchWaitTimeout`, then
      answers 504: a follower cut off from the leader would wait forever.
    - The stream runs until the client stops. It ends early when the ring dropped changes
      the client has not read yet; the client lists afresh then too, and says so. The
      agent's stop ends it (`Server.EndStreams`, as for logs that follow): `Shutdown`
      would wait for it.
- **JSON names.** What users write and read as resources (`resource.Object`, the specs and
  the statuses `get` shows) is camelCase, as in the files users apply; everything else
  (commands, the state, snapshots, the other bodies of the API, `cluster.yaml`) is
  snake_case.
- `api.HostActions` lists the user's host actions with the phases they start from; the server
  checks them and the CLI makes its subcommands from them.
- CLI and agent refuse each other more than one minor version apart (`X-Clowder-Version`,
  `buildinfo.Compatible`); development builds and callers without a version talk to
  everything.
- **The client** tries the seeds of `cluster.yaml` in turn and trusts any host certificate of
  the cluster's CA, whatever name the seed has. A read goes on to the next seed after any
  network error; a write only if it never left (`httptrace`), since one that was sent may
  have committed without an answer.
- **The CLI** finds the cluster directory (`--cluster`, `$CLOWDER_CLUSTER`, the working
  directory) and by its `name` the user's credentials, then talks to the cluster with the
  user's certificate. An agent whose certificate is of another CA than the credentials'
  is named as such (`foreignCA`: a cluster of the same name, or set up anew) instead of
  with x509's words alone: `cluster status|apply|diff|schema`,
  `host list|drain|undrain|remove|enable`, `apply`, `get`, `delete`, `stack ps|logs|restart`.
- **Exit codes**, for every command: 0 when it did what was asked, 2 for a wrong call
  (`usageError`: an unknown command, flags or arguments that do not fit; `checkArgs` makes
  every command's argument errors one, and gives each command of commands a `RunE` that
  shows its help and takes an unknown command as a wrong call, where cobra would end as
  if done), 1 for anything else (refused, missing, not reachable). A refusal shows the
  state's reason without the command's kind (`state.CommandError`).
- **`apply -f <file|directory|->`** (`resources.go`, `files.go`) reads YAML documents
  (`resource.ParseDocuments`: clowder's resources only, or a file of other documents, never
  both; another apiVersion of clowder's is an error). A directory's YAML files are read,
  not those below it; hidden files, other files and files of other documents are skipped
  and named on stderr, and a cluster directory is refused. A SOPS file (by its content:
  the key `sops`) of a resource (SOPS keeps the keys, so `apiVersion` shows) is decrypted
  with the user's age key (`clusterdir.DecryptSOPS`) and must hold exactly one `Secret`,
  so `sopsage` keeps every value encrypted; a SOPS file of anything else, such as the
  cluster's secrets, is skipped unopened. A `Secret` in clear in a file is refused: such a
  file ends up in a repository or a backup. In clear, a secret comes only on stdin. A
  resource without a namespace is in `default`. The resources go namespaces first, then
  volumes and secrets, then stacks, in one request (`applyRank`); each result is printed,
  and one not applied, deleted or unchanged makes the command fail. `--dry-run` checks
  everything and changes nothing.
- **`get <kind> [[<namespace>/]<name>]`** and **`delete <kind> [<namespace>/]<name>`**
  take a kind singular or plural, and the namespace as kubectl does: `-n`, or in the name
  (`shop/data`), else `default`; `-A` lists every namespace. Where both name one, they must
  agree; a namespace takes neither (`selectFlags.resolve`, with a table test of every
  combination). `get` prints a table, or `-o yaml` (documents in the API's field order,
  which `apply` takes again; a secret shows only its digest) or `-o json`. `get --watch`
  (`watch.go`) lists, then shows each resource that changes, until Ctrl+C: a table's rows
  without the header again, or one document each; a name filters on the client. After a
  410, or when the agent ends the stream, it lists afresh and goes on. `delete -f`
  deletes the resources of files in the reverse order of an apply, with the namespaces the
  files name (no `-n`). Deleting a volume deletes its data, so a delete that names one needs
  `--with-data` (`DeleteRequest.WithData`). The API refuses the whole request without it,
  before anything goes: a delete of a directory would otherwise take the stack and stop at
  its volume.
- **`stack ps|logs [<namespace>/]<stack>`** (`stack.go`) ask the stack's host
  (`GET /v1/stacks/{namespace}/{name}/ps|logs`, then `/v1/internal/stacks/<project>/...`,
  `names.Project`), wherever the CLI came in. **`stack restart [<namespace>/]<stack>
  [<service>...]`** sends `restart-stack` (`POST /v1/stacks/{namespace}/{name}/restart`,
  `RestartRequest`). The list of stacks, also as JSON for scripts
  and the live tests, is `get stacks`; its table shows a priority that comes from
  `cluster.yaml`'s default as `(default)`.
- **Who can read a secret:** whoever may apply it, or decrypt the SOPS file it came in;
  every host (a stack must start anywhere); and, until there are permissions, every user of
  the cluster, through a stack that names it or one named in `stacks.privileged`, as such a
  container reaches the secrets and the data key on its host.
- **`cluster apply`** sends `cluster.yaml` with the secrets of `secrets.sops.yaml` in clear
  inside mutual TLS; the agent seals them before they go into the state. Its hash covers
  both, so `cluster diff` tells changed fields of `cluster.yaml` from changed secrets without
  the cluster showing any secret.
- **The first user** has no certificate yet (`cluster bootstrap`). `GET /v1/bootstrap` gives
  the CA certificate with a `pki.TrustProof` for each valid bootstrap key, and the CLI trusts
  the CA only if one matches its key. `POST /v1/bootstrap`, over TLS checked against that CA,
  carries the user's public key with an HMAC of the bootstrap key and a time
  (`bootstrapMaxAge`). It works once: when the cluster has a user, users add the others. The
  CLI then writes the credentials (`clusterdir.WriteCredentials`) and the cluster directory
  (`clusterdir.Create`).
- The tests run real HTTPS servers: a follower forwarding to the leader, refused callers, the
  CLI against an agent with a cluster directory and credentials in a temporary config
  directory.

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
- **`clowderd run`** (the service) sets up `clowder0` with the node address (a dummy
  interface, so the address exists on a host without peers), the Raft member, and the API on
  the underlay, the node address and the public address, each once. The API binds them with
  `IP_FREEBIND` (`freeBind`): at boot the public address may come up after the agent, which
  must neither fail nor lock users out then. It waits up to `catchUpTimeout` for its state
  to catch up with the Raft log, then tells systemd `READY=1`, and feeds the watchdog while
  the state's lock answers. When it stops, it ends its loops and waits for them, at most
  `loopsTimeout` (a host tool left behind on a suspended volume must not hold up the stop),
  before the resolver and the Raft member close (`stopLoops`).
- `HostParts` are what the agent changes on its host (the network, the storage driver and
  Docker) and the hoster's API. `clowderd run` passes the real ones, the tests fakes.
- **Reconcilers** run after a change of the state and every `reconcileInterval`, to repair
  what someone changed by hand: `reconcileNetwork` keeps the nft rules as the state says,
  and the stack runner keeps the stacks.
- **The stack runner** (`stackRunner`) keeps everything of a stack under its project
  (`names.Project`). A pass first stops the stacks that left the host (`compose down`), and
  only then, for each stack the state places here, renders its Compose file, `Ensure`s its
  volume copies, writes its secrets, runs `compose up` unless all of it came up before, and
  reports `running`: a volume that passes from one stack to another is never used by both
  at once. A failed `compose up` is tried again at the next pass. Before `compose up` the
  runner pulls the images of the services a user restarted since their containers came up:
  their `clowder.restarted` label differs from the state's time. An image may have changed
  under the same tag. Docker keeps the label, so a reboot pulls nothing again. Last, volume
  copies the state retired here are removed and forgotten ([Cluster state](#cluster-state-internalstate), Retired copies). A copy
  the state neither places nor retires here stays, with a warning once per run.
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
  secret file changes its services' digest label, which recreates them ([Rendering](#rendering-internalcompose), Labels).
- **After a boot no container runs** (`restart: "no"`). The runner remembers only in memory
  which files came up, so its first pass runs `compose up` for every stack, and so does the
  pass that a restart of Docker starts at once (the supervisor's events broke off,
  `forgetApplied`). The first pass waits until
  the state has caught up with the Raft log, so a host that was away does not start a stack
  that has moved on.
- The agent also logs Docker in to the registries of `secrets.sops.yaml`, runs the
  supervisor and, while it leads, the placement (`controller.RunPlacement`). Every
  `trimInterval` it trims the volumes.
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
  rules, and a volume and a stack applied, listed and deleted. `stacks_test.go` runs single
  passes of the runner against a state in memory: a failed `compose up`, a new data
  generation, a volume that outlives its stack, a volume that passes to the next stack only
  once the old one stopped (and waits while a stop fails), secrets written, changed and
  removed, the label changing with a secret, the secrets the containers came up with
  reported. That docker compose fails on a missing secret file and
  recreates exactly the containers that changed is for the live tests: CI has no Docker
  daemon.

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
  ([Rendering](#rendering-internalcompose)). The DNAT that brings them to a stack on another host, and the marks for
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
- **It never guesses.** Only a tool's own exit code means "not there" (`lvs` 5, `blkid` 2,
  `findmnt` 1). Any other failure, a timeout or a device not ready, stops `Ensure`, so a
  guess never creates over or formats real data.
- **The mount guard:** containers get `data/`, which exists only in the mounted file system,
  so a container whose volume is not mounted fails to start instead of writing to the root
  file system.
- `Remove` picks up after a crash at any point: a resource without its file is down
  already, an LV that `lvs` does not find is gone already.
- `PoolUsage` reads a pool's size and use; `Trim` (fstrim) gives deleted blocks back.

## Placement (`internal/scheduler`, `internal/controller`)

- `scheduler.Plan` reads the state and returns its decisions: what hosts over capacity push
  out, where pending stacks go (highest priority first), why the others wait, and which
  pushed-out service may come back. A volume is placed with the first stack that names it,
  on that stack's host, and must fit its pool there (`Fits`: without overcommit, the pool's
  volumes and the new one stay within the pool less its reserve, a class's fallbacks
  included); a placed volume stays where its copy is, so a stack that names one goes only
  to that host. Volumes, DRBD minors and room are taken only for a stack that fits.
- **Room, one measure for everything.** A host offers what it reported (memory, CPUs) less
  `system_reserve` (default 512M, 0.25). Each service of a stack on it that is `starting` or
  `running` takes its reservations (`resources.reserve`, default `defaults.reservations`,
  128M and 0.1), unless it is pushed out. Nothing measures what containers really use. The
  state keeps what the scheduler needs of each service (`Stack.Services`: the priority as
  written, the reservations, `dependsOn`).
- **Priorities.** A service's effective priority is the lowest of its own (else the
  stack's) and of everything it depends on: stopping a service stops what depends on it, so
  a running service never depends on a pushed-out one. Categories are resolved with the
  current `cluster.yaml` at every pass (`Priorities.Resolve`), so a changed category counts
  without a new apply; pending stacks are placed by their highest service's. Only
  `dependsOn` within a stack counts: a service another stack imports (with imports, phase
  4) may be pushed out, and that stack loses it as it would lose any service it does not
  own.
- **Waiting.** `Stack.WaitingSince` is set with the first wait reason and kept while only
  its text changes; it is in the state so the alert for a stack that waits too long
  survives a change of leader.
- **Placing.** A new stack goes to the active host where all of it fits with the least
  memory left. If none has room, it goes to the first host where it fits by pushing out
  (`fitStack`):
  - Its services go one by one: highest effective priority first, and at equal priority
    what others depend on first.
  - Each takes free room, or pushes out services of any stack below its own priority,
    lowest first, each with what depends on it.
  - A service that finds no room even so starts pushed out, with what depends on it
    within the stack. The stack waits only if its first service finds no room.

  So a stack's `cache` at 200 never pushes out another stack's service at 500 because the
  stack's `api` is at 800. The new stack's own list is written afresh at every placement,
  so one left by an earlier leader is replaced.
- **Over capacity** (a raised `system_reserve`, a stack whose new generation reserves more):
  services below the host's highest effective priority go, lowest first, until the rest
  fits; services at that priority never go for this. If that is not enough, nothing goes and
  the leader raises the alert `over-capacity`.
- **Coming back.** A pushed-out service whose dependencies all run comes back once its
  reservations and 5 % more fit its host's free room for 60 s: highest effective priority
  first, but one that does not fit does not block a smaller one. One service per decision,
  since its return changes the room, and only in a pass that places and pushes out nothing:
  a waiting stack gets room before a pushed-out service does. The 60 s are kept in the
  leader's memory and start again after a change of leader.
- `controller.RunPlacement` runs on every agent but acts only on the leader. It plans on a
  copy of the state (`State.Copy`), outside the lock Raft's `Apply` waits for. It first
  writes the push-outs of hosts over capacity; if one fails, it places nothing in that
  pass, since the placements count on the room it frees. Per placement
  it writes what the stack pushes out (`set-pushed-out`, one command for every stack it
  touches), then the volumes (`put-volume`), then the stack's host and `starting`
  (`set-stack-status`). If the leader changes in between, the services stay pushed out
  without the new stack and come back by the rule above. The stack's host does the rest.
- **On the host** a pushed-out service is left out of `compose up` (`--scale <service>=0`,
  which also removes its running container), and the supervisor's rule for it is "no
  restart". Only the leader changes `Stack.PushedOut`; a host's status report no longer
  carries it. A new generation drops services it no longer has. Whether compose
  up leaves the rest running is checked by the live tests: `bin/dev test-tools` has no
  Docker daemon.

## Traefik (`internal/ingress`, `agent/traefik.go`)

- Every agent runs Traefik (`ingress.Image`, pinned) as the Compose project
  `clowder-traefik` in the host network, with `restart: "no"` like every container. It is no
  stack: the state knows nothing of it, its files live in `<Dir>/traefik/` beside `stacks/`
  (so the search for stacks that left never finds it), and a stack's project starts with its
  namespace, which `names` refuses to start with `clowder-`. `clowdersOwn` answers whether a
  project is clowder's own; every place of the stack runner that treats Traefik apart asks
  it: the supervisor's rule (restart always, also when unhealthy) and the clean-up of
  secrets.
- `ingress.RenderStatic` writes `traefik.yml`: the entrypoints `web` and `websecure` on the
  Floating IP :80 and :443, `internal` on the node address :7480 (phase 2) trusting
  `X-Forwarded-For` only from the node network, and `ping` on 127.0.0.1:7446 for the health
  check. The public entrypoints trust no `X-Forwarded-For`, so Traefik sets the client's
  address itself. `ingress.RenderCompose` writes the Compose file.
- `traefikRunner` has a loop of its own beside the stack runner, so a long image pull of a
  stack never holds up the ingress. It runs compose up as the stack runner does
  (`composeUpHeld`, under the supervisor's hold) when the hash of the Compose file and
  `traefik.yml` differs from the one Traefik came up with: at the first pass after the agent
  started, after Docker restarted (`forget`), and after a new image pin or Floating IP. Each
  of its files is compared with what is on disk, so one removed or changed by hand comes
  back at the next pass. It reads only the Floating IP, the stacks' routes and the
  certificates from the state, no copy of all of it: it passes at every event of Docker's.
- **Routes.** `ingress.RenderDynamic` writes the file provider's `routes.yaml`: per service
  with `http` a service `<project>.<service>` whose servers are the containers of that
  service on this host that run and, if they have a health check, are healthy
  (`docker.Inspect`; the address in the network whose name sorts first), and routers
  `<entrypoint>.<project>.<service>`. A route without such a container keeps its routers with
  no servers, so Traefik answers 503, not 404. Traefik watches the directory, so new routes
  need no restart.
- **HTTPS.** A route whose domain has a certificate gets a router on `websecure` and one on
  `web` that redirects to it; before that only a plain router on `web`, so nobody sees
  Traefik's self-signed default certificate. The runner writes every certificate of the
  state and its opened key into `/run/clowder-secrets/clowder-traefik/` (the tmpfs of the
  stacks' secrets, 0700; mounted read-only), before the routes that name them. Traefik
  watches only the routes, so the files are named after a hash of the chain: a renewed
  certificate changes the routes, and Traefik reads it; old files go after the routes.
- **ACME challenges.** The router `acme-challenge` on `web` sends
  `/.well-known/acme-challenge/` of every domain, before every stack's router, to the agent
  on 127.0.0.1:7445, which answers from the state (`State.Challenges`): whichever host
  holds the Floating IP can answer the CA.
- The runner's passes come at every change of the state, every 30 s, and at once when the
  supervisor sees an event of Docker's (`look`: start, die, health status): a container that
  turns unhealthy leaves its route within seconds.
- Golden files in `internal/ingress/testdata`; `bin/dev test-tools` checks the Compose file
  with Debian's docker-compose.

## Hoster and ingress (`internal/hoster`, `internal/controller`)

- `cluster.yaml` names the hoster in a section of its own, chosen by `type` like backup
  targets and alert channels: `hoster: {type: hetzner, token: {secret: ...}}`, and `api`
  for a lab's API instead of the hoster's (a full `http://` or `https://` URL: the token
  goes there).
- `hoster.Hoster` is what clowder asks of a hoster: where the Floating IP is routed, route
  it, power a server off and on (fencing, phase 2). A server is its `ServerRef`, the
  hoster's ID that `clowderd init --server-ref` records. `hoster.New` picks the plugin.
- `hoster/hetzner` is a small client of the Hetzner Cloud API on `net/http`: the official
  SDK would bring Prometheus' client and more into clowderd. It finds the Floating IP among
  the project's (the API cannot look one up by its address) and waits for each action to
  finish. Its errors carry the API's message, never the token.
- `controller.RunIngress` acts only on the leader. The ingress host (`State.Ingress`,
  `set-ingress`) is the recorded one while it is active and has a server ref, so the
  Floating IP never switches back by itself; otherwise the first of
  `ingress.preferred_hosts` that can, else the one with the lowest ID that can. At a change
  of the state and every 30 s it asks the hoster where the Floating IP is and routes it to
  the ingress host when it is elsewhere (moved by hand, or a new ingress host); it reads
  the API at most once per 30 s, about 120 of Hetzner's 3600 requests an hour.
- Every host has the Floating IP on its public interface, held or not (`clowderd check`
  requires it): the hoster routes it to one host only, so whatever binds it (published
  ports, Traefik) can bind it on every host and at boot.
- Only the leader's loops call the hoster: the controller opens the token from the state
  and makes the client (`HostParts.Hoster`, a fake in the tests). depguard keeps `hoster`
  out of the CLI, the state and the scheduler.
- **clowder's ports** lie in 7400-7499, which `stackspec.ReservedPort` keeps from the stacks:
  Raft 7400 and the API 7443 (node address and underlay), `/metrics` 7444 and Traefik's
  internal entrypoint 7480 (node address), the ACME challenges 7445 and Traefik's health
  check 7446 (127.0.0.1). On the Floating IP, 80 and 443 are Traefik's.

## Certificates of the routes (`controller/acme.go`)

- `controller.RunACME` acts only on the leader, with `golang.org/x/crypto/acme`, at a
  change of the state and every 30 s. It keeps one certificate per domain of an HTTP route
  (`State.Certificates`, the key sealed) and deletes those of domains no stack names.
- The account (`State.ACMEAccount`, the key sealed) is made at the first order and belongs
  to its directory: a new `acme.directory` gets a new account. `acme.ca` (a file next to
  `cluster.yaml`) is trusted besides the system's CAs: a test CA such as Pebble.
- An order, one domain after the other: the domain must resolve to the Floating IP and
  nothing else, or no order (retry in 10 minutes: it never reached the CA, so it does not
  count towards the backoff, and Let's Encrypt's limit on failed validations stays
  untouched). Then the answer to the HTTP-01 challenge goes into the state, and the leader
  asks the Floating IP :80 for it with the domain as host until Traefik returns it (at most
  2 minutes). The Floating IP is on every host's interface, so this reaches the leader's
  own Traefik: on one host the ingress host. With several hosts it proves nothing about
  the ingress host; phase 2 must ask that host, for example through its internal
  entrypoint. Only then may the CA validate. When the order ends, the leader removes the
  answer from the state; a new leader removes answers an old one left behind. A failed
  account makes the pass end: Let's Encrypt allows few new accounts per address.
- A failed order backs off per domain: 10 minutes, doubling up to 24 hours, kept in the
  leader's memory. A new leader starts again at none, so even a leader that changes often
  stays well below Let's Encrypt's limits.
- **Renewal** is relative to the lifetime (`NotAfter` minus `NotBefore`, both in the
  state), because Let's Encrypt makes lifetimes shorter: by default when a third is left.
  `acme.renew_before` overrides that while it is shorter than the lifetime; a longer one
  would renew every new certificate at once, so the leader keeps the default for that
  certificate and raises the alert `renew-before-ignored`. Both follow from the state
  alone, so no setting and no change of leader can make it renew in a loop.
- The tests run the whole order against a small ACME CA of their own
  (`controller/acme_test.go`), which validates by asking for the challenge.

## Alerts (`internal/notify`, `controller/wanted.go`, `controller/alerts.go`)

- **Channels** (`alerts.channels` in `cluster.yaml`, each with an optional
  `min_severity`): Discord (an embed colored by severity), webhook (a JSON POST of
  `notify.Message`) and email (port 465 with TLS from the start, any other port only with
  STARTTLS; `smtp.ca` is a file next to `cluster.yaml`). URLs and passwords are secrets;
  errors never name a URL. `http://` works, as the live test's receivers need.
- **In the state** only the open alerts (`State.Alerts`, `put-alert`, `resolve-alert`): a
  key of kind and subject (`pool-use/node0/vg/thin/data`), so one condition never has two
  open alerts; a condition that gets worse keeps its key and changes its severity.
  Subjects and messages hold no value that changes from report to report: each change is
  a Raft write.
- **In the leader's memory** (`controller.Alerts`): the deliveries. A message goes out when
  an alert is raised or gets worse, every 6 h while it is open, and when it is resolved,
  to each channel whose `min_severity` it reaches; the all-clear goes with the highest
  severity told, so a channel for critical alerts gets it after a downgrade too. A failed
  message is tried again after 1 minute, doubling up to an hour, and its channel gets
  nothing more in that pass, so a dead mail server cannot hold up the loop; the heartbeat
  goes first. Messages wait by the channel's place in `alerts.channels`, so a new
  `cluster.yaml` drops them. `clowder cluster status` shows the failed ones: a follower
  asks the leader (`GET /v1/internal/alerts/failures`).
  After a change of leader the new leader reminds of each open alert once: a duplicate
  rather than a lost alert. The heartbeat (`alerts.heartbeat`) is a GET every `interval`
  (default 1m), in a loop of its own, so a slow channel cannot make it late.
  `clowder alert test` sends a test alert to every channel at once, from the agent the CLI
  asks: the one exception to "only the leader calls external services", so a channel can be
  tried also while the cluster has no leader.
- **Conditions** that the state does not show are held in memory (`controller.Conditions`)
  with the time they began, by the loop that sees them: the Floating IP not held (the
  ingress loop), a host over capacity (the placement), Docker not answering (the alerts,
  from `report-host`). After a change of leader they start again, so a new alert comes a
  little later rather than wrongly; one that is open stays until the loop that holds its
  condition has looked (`Conditions.Seen`). An agent that leads no more forgets them.

| Alert | Severity | Raised | Resolved |
|---|---|---|---|
| `pool-use` (data or metadata) | warning / critical | ≥ 80 % / ≥ 95 % | < 75 % / back to warning < 90 % |
| `root-use` | warning | ≥ 90 % | < 85 % |
| `docker-down` | critical | 2 min without an answer | answers |
| `ingress-not-held` | critical | 5 min | held again |
| `over-capacity` | warning | at once | fits again |
| `certificate-not-renewed` | warning | less than half the renewal window left | renewed |
| `renew-before-ignored` | warning | for any certificate | value changed |
| `stack-waiting` | warning | 5 min (`WaitingSince`) | placed |

A value is resolved below its raise threshold, so one that hovers there does not raise and
resolve at every report. Likewise an alert that gets better and then worse again, up to the
severity the channels were told of, sends nothing new: only its reminder and its
all-clear. The quorum watch (an agent without quorum alerts by itself) comes with phase 2:
one host has no quorum to lose.

## Metrics (`internal/metrics`, `agent/metrics.go`)

- Every agent serves `/metrics` on its node address, port 7444 (`MetricsPort`), plain HTTP
  without authentication: only the cluster's hosts reach the node address, through
  WireGuard; containers do not (the nft table). Nothing in `cluster.yaml` changes that.
- `internal/metrics` writes Prometheus' text format by hand, sorted, so it is tested
  against golden files (`agent/testdata/metrics-*.golden.txt`); the official client
  library would bring more into clowderd than the few dozen values need.
- Each agent exports what is about its own host: its phase and whether it leads, the Raft
  index, the pools' use, Docker, the root file system, the containers of its stacks by
  state, the supervisor's restarts per service and its own resident memory. What the
  cluster has once only the leader exports, so a Prometheus that scrapes every host
  counts it once: each host's room as the scheduler counts it (`scheduler.Rooms`), the
  certificates' expiry, the open alerts by severity and the failed deliveries.
- `promtool check metrics` and a scraping Prometheus come with the live tests.

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
  While the agent runs `compose up` or `down` for a stack, it `Hold`s the stack's project,
  so the supervisor does not fight it.
- A start that fails counts as another crash; a container that is gone
  (`docker.ErrNoContainer`) is let be. `destroy` events drop what the supervisor knew about a
  container. When the events break off, what died in between went unseen (a restart of
  Docker kills every container): the supervisor tells the agent, which then runs
  `compose up` for every stack as after a boot.

## Host tools (`internal/command`)

Every host tool runs through `command.Run` or `command.RunWithInput`, with no exception
(lvs, drbdadm, mkfs, mount, nft, ip, systemctl, docker, ...): with a timeout, and left behind
if it does not finish. A process that touches a volume can hang in state D while DRBD
suspends I/O, and then not even SIGKILL ends it; any other tool that hangs must not stop a
reconciler either. `command.Stream` (logs that follow) is the one without a timeout; only
tools that touch no volume use it. `command.ExitCode` tells a tool's own exit code from a
timeout or a missing binary.

## Host check (`internal/hostcheck`, `clowderd check`)

Checks the operator base and changes nothing: sysctls, time sync, watchdog, WireGuard, DRBD 9
built for the running kernel, drbd-utils, DRBD autostart and usage report off, LVM filter,
thin pools (`--pool`), Docker's `daemon.json`, docker compose, unattended-upgrades and the
Floating IP (`--floating-ip`). Each check reads through the `Host` interface, so the unit
tests use a fake host; exit status 1 if any check fails. A command that hangs (`lvs` while
DRBD suspends I/O) is left behind after `commandTimeout`: fine for `clowderd check`, which
exits; the agent will run the checks in their own process for the same reason (Volumes).

## Volumes

Without quorum, DRBD suspends I/O (`on-no-quorum suspend-io`): a write then hangs in the
kernel in state D, and neither a timeout nor a signal ends it. So the agent never writes to a
volume in its own loops and never waits for a process that does. Work on volumes (freeze,
snapshot, copy) runs in separate processes that the agent watches for progress. Health
reports never depend on a volume.

## Logging and errors

- `internal/logging` sets up `log/slog`: JSON on stderr for the agent (systemd passes it to
  journald), text for the CLI.
- Errors are wrapped with `%w` and say what was tried; the CLI prints the chain once at the
  top (`clowder: ...`).

## Packages and units

`packaging/` holds the nfpm configurations, the systemd units and the maintainer scripts
(reload systemd after install and removal, stop the agent before removal).

- `clowderd` depends on Debian packages and on `drbd-dkms` and `drbd-utils` from LINBIT's
  public repository; the host's APT sources must list it. `coccinelle` lets the DKMS build
  run offline.
- `clowderd.service`: `Type=notify` with a watchdog, `Restart=always` without a start limit,
  `KillMode=process` (helpers finish their step; containers belong to dockerd). The package
  installs it without enabling it; `clowderd init` enables and starts it.
- `clowder-tasks.slice`: memory limit for the agent's short-lived tasks.

## CI

`.github/workflows/ci.yml` runs `bin/dev lint`, `bin/dev test`, `bin/dev test-sops`,
`bin/dev test-tools` and `bin/dev package` on every push and pull request and keeps `dist/`
as an artifact. `test-tools` checks the renderers' golden files with Debian's `nft -c`,
`drbdadm` and `docker-compose` (`test/tools/check-golden-files.sh`). The live tests need
`/dev/kvm` and stay manual.

## Releases

A tag `v<major>.<minor>.<patch>` starts `.github/workflows/release.yml`:

1. `bin/dev all` (lint, test, test-sops, test-tools, package); the version comes from the tag.
2. CLI archives for Linux, macOS (`.tar.gz`) and Windows (`.zip`).
3. `packaging/apt/publish.sh` adds the Debian packages to the public APT repository
   `webcodingcat/clowder-apt` with reprepro and signs it; GitHub Pages serves it at
   `https://webcodingcat.github.io/clowder-apt` (distribution `trixie`, component `main`,
   public key `clowder.asc`). Its `README.md` shows how a host adds it.
4. A GitHub release with the packages and the archives. It comes last: a version is out once
   it is in the APT repository. If only this step fails, create the release by hand
   (`gh release create`); re-running the workflow would publish the version again.

The workflow needs two secrets in this repository:

| Secret | What |
|---|---|
| `APT_SIGNING_KEY` | ASCII-armored private OpenPGP key without passphrase that signs the APT repository |
| `APT_DEPLOY_KEY` | private SSH key; its public half is a deploy key with write access on `webcodingcat/clowder-apt` |

`publish.sh` imports the key into a throw-away keyring and publishes the public half as
`clowder.asc`; `conf/` and `db/` stay in the APT repository so the next release adds to it.
