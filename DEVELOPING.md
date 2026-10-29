# Developing clowder

How the code fits together and which rules hold it together, with their reasons. Numbers,
paths and timeouts live in the code as named constants; this file names them instead of
repeating them. [STYLE.md](STYLE.md) has the coding standards, `bin/dev` the commands.

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

### Live tests with HLab

The live tests run against HLab, a local stand-in for Hetzner Cloud with real VMs (it needs
`/dev/kvm`). They use only its interface: `hlab env` prints the API endpoint, the token, the
Storage Box access and the Docker network of a running lab. (The spike also reaches into the
lab's container; it goes away with it.) Point `HLAB` at its wrapper:

```bash
export HLAB=/path/to/hlab/bin/hlab
spike/run.sh up      # the spike of phase 0: a lab "clowder" with three hosts
```

The live tests will replace `spike/`; they will live in `test/live` behind the build tag
`live` (not written yet).

## Roadmap

clowder is built in phases; each ends with live tests on HLab.

| Phase | Builds | State |
|---|---|---|
| 0 | the spike: DRBD quorum and fencing, WireGuard, the real client IP, on three hosts | done |
| 1a | the foundation on one host: stacks, cluster directory, host check, state, PKI, Raft, API, network, storage, containers | in progress |
| 1b | ingress (Traefik, ACME), priorities, alerts, still on one host | |
| 2 | three hosts: replication, failover with fencing | |
| 3 | backup and restore, GitOps | |
| 4 | comfort and operations | |

Known gaps to close in phase 2. They do no harm on a single host:

- Raft and the forwarding of writes run on the node address, which other hosts reach only
  through the WireGuard mesh.
- `CatchUp` makes sure the state holds the whole log only on the leader; a follower returns
  once it knows a leader. Before a host that was away starts its stacks, it must have applied
  the leader's commit index.
- The agent runs its stacks one after another in one loop. A long image pull or `mkfs`
  holds up the others, and a command left behind on a suspended DRBD volume leaves a
  goroutine behind at every pass. It also runs `Ensure` on every volume at every pass,
  which it should skip when nothing changed.
- `storage.Ensure` promotes a resource with `--force` when this host has no `primary` marker.
  That is right for a new volume on one host; after a failover it would force an outdated
  copy. Whether a volume was ever initialised must come from the state, not a local file.
- A volume a stack no longer names stays: nothing sends `delete-volume` yet.

## The big picture

```
user's machine                 any agent               leader                stack's host
──────────────                 ─────────               ──────                ────────────
clowder stack apply ──mTLS──▶  API: check, seal  ──▶  Raft log ──▶ every agent's state
  (files, secrets)                                       │
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

## A stack's way

1. `clowder stack create shop --age <key>` registers the name (`create-stack`). The stack is
   `created`: nothing places or runs it yet. The command writes what is missing of `shop/`:
   `clowder.yaml` (the stack's Compose file, `name: shop`), an empty `secrets.sops.yaml` and
   the `.sops.yaml` that encrypts it for the key.
2. `clowder stack apply shop` loads and checks `shop/clowder.yaml` on the user's machine,
   decrypts the secrets the stack asks for and sends both (`PUT /v1/stacks/shop`). The agent
   that takes the call checks the stack against the cluster, seals the secrets and commits
   `put-stack` through the leader: the stack is `pending`.
3. The leader's placement picks a host and a pool for each volume and commits the volumes
   (`put-volume`) and the stack's host (`set-stack-status starting`).
4. That host's agent makes the volumes (LV, DRBD, ext4, mount), renders the Compose file,
   writes the secrets to tmpfs, runs `docker compose up` and reports `running`.
5. `clowder stack ps|logs shop` asks the stack's host, through any agent.
6. A later `stack apply` raises the stack's generation. The stack keeps its host, the
   placement adds only volumes it newly names, and the host's agent brings the new
   generation up; Compose recreates the containers whose configuration changed.

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
| sealed | encrypted with the cluster's data key before it goes into the state |
| generation | of a stack: counts its applies, a label on its containers; of a volume: grows with a restore or class move, part of its LV's name |

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

- **Portable**: `cli`, `api`, `clusterdir`, `sopsage`, `pki`, `compose`, `units`, `names`
  (the rules for the names users give), `buildinfo`, `logging`, `atomicfile` (files written
  whole or not at all). The CLI may import only these. depguard (rule `cli-portable` in
  `.golangci.yml`) refuses direct imports of the Linux-only packages in `cmd/clowder` and
  `internal/cli`; the cross build for Windows and macOS catches indirect ones.
- **Pure logic, no I/O**: `scheduler`, `state`. Their tests need no host, files or network;
  depguard (rule `state-pure`) keeps `os`, `net/http` and randomness out of both. `state` uses
  the types and `Check` of `clusterdir`, never its file access.
- **Decides**: `controller` (the leader's loops) writes Raft commands. It talks to external
  services only the leader uses (hoster API, ACME, Git remotes, alert channels), but never
  changes the host.
- **Changes the host**: `agent`, `supervisor`, `raftnode` (Raft over mTLS), `datakey` (the
  key file), `command` (host tools with a timeout) and the drivers `network`, `storage`,
  `docker`, *`ingress`*.
- **Reads the host**: `hostcheck` (for `clowderd check`). It compiles anywhere, but only the
  agent uses it, so depguard keeps it out of the CLI like the packages above.

## Stacks (`internal/compose`)

- **Bound to their name.** The first file of a stack says `name: <stack>` as written, and no
  later file sets one (`CheckNames` reads it before compose-go fills in or normalizes
  anything). `Load` and `Parse` compare the project's name with the stack, so the CLI and
  again the API refuse a file for another stack.
- **What breaks clowder's model is refused** (`Load`, `load.go` for the stack, `service.go`
  for its services): what ties a container to one host (bind mounts except `/etc/localtime`
  read-only, configs from a file, devices, anonymous and external volumes, external
  networks), what shares the host's namespaces (`network_mode: host`, `pid`/`ipc: host`),
  and what clowder sets itself (`build`, `container_name`, replicas, network drivers and
  subnets, port ranges, host addresses, `ReservedPort`). Every problem is reported at once
  (`Problems`); the `x-clowder` blocks are decoded strictly, and only an unknown key there
  ends the checks of its block.
- **Privileges are the cluster's to give.** What reaches beyond the container (`privileged`,
  a capability outside `harmlessCapabilities`, `security_opt` other than
  `no-new-privileges`, `cgroup_parent`) is listed per service (`Service.Privileges`). Such a
  container can read the host's keys and other stacks' volumes and secrets, and change the
  rules that keep containers apart. So the API allows it only for stacks that `cluster.yaml`
  names in `stacks.privileged`, and `cluster apply` refuses a `cluster.yaml` that a stack of
  the cluster would no longer pass (privileges, storage classes, priorities). Both checks
  hold at commit: `put-stack` carries the `cluster.yaml` revision it was checked against,
  `apply-config` the stacks revision (`State.StacksRevision`), and the state refuses either
  when the other changed in between.
- **Variables are filled in on the user's machine.** `Source` fills in the variables and
  env files and drops the user's paths; the result is stored in the state and on the host as
  it is, not sealed. Passwords therefore belong in the stack's secrets file, never in a
  `${VARIABLE}` or the Compose file.
- **Secrets.** A service asks with `x-clowder.secrets: [name]` (a file `/run/secrets/<name>`
  in the container, for images that read `*_FILE`) or `x-clowder.secret_env: {VARIABLE: name}`.
  The stack names its SOPS file in `x-clowder.secrets_file`: relative to its first file, a
  map of names to strings, possibly shared by several stacks. Compose's own `secrets:` are
  refused, and so are configs from a variable: docker compose on the host would read the
  agent's environment. A variable also ends up in Docker's container configuration on the
  host's disk and in `docker inspect`; a file does not.
- **Rendering.** `Render` writes the file docker compose gets on a host: `restart: "no"` (the
  agent restarts), the agent's resolver, each port on the Floating IP and the node address,
  labels, a fixed bridge name per network (`BridgeName`), volumes bound to `<mount>/data`,
  secrets as files in `Target.SecretsDir`. `RenderSecretEnv` writes the secret variables as a
  second Compose file. Every `$` in a value becomes `$$` (`EscapeDollars`): the values were
  filled in on the user's machine, and docker compose on the host would fill in variables
  once more, from the agent's environment.
- **Golden files.** `testdata/shop/rendered.golden.yaml` and `secrets.golden.yaml`. After an
  intended change: `bin/dev test ./internal/compose -update`, then review the diff.
  `bin/dev test-tools` checks both with Debian's `docker-compose`, and that it fills in
  nothing.

## Cluster directory (`internal/clusterdir`)

- `Find` picks the directory (`--cluster`, `$CLOWDER_CLUSTER`, the working directory), `Load`
  reads `cluster.yaml` strictly and `Check` lists every problem. `Schema` makes the JSON
  schema of `cluster.yaml` from the Go types.
- `ReadSOPS` decrypts a SOPS file with the user's age key, as SOPS finds it. It refuses a
  file with values left in clear (a rule like `unencrypted_suffix`): these files hold
  secrets. `Secrets`, `Credentials` and the stacks' secrets all read through it.
- `WriteSOPS` and `WriteSOPSConfig` write a SOPS file and the `.sops.yaml` beside it, for
  `cluster bootstrap` and `stack create` alike. `testdata/test-age-key.txt` is a test-only
  key for the fixtures.

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

## Cluster state (`internal/state`)

Every agent holds the same `State` and applies the same `Command`s in Raft's order.

- **Deterministic.** `Apply` uses no clock (a command carries the leader's time in `At`), no
  randomness and no I/O; lint bans `time.Now` from the package (forbidigo). A handler checks
  everything before it changes anything, so a refused command leaves no trace; the state's
  `Index` follows the log either way.
- **Commands.** A command has a kind, a payload of the type with the same name (`AddHost`,
  `PutStack`, ...) and an actor (`leader`, `host/<id>`, `user/<name>`) that the API takes from
  the caller's certificate. Which actor may send which kind is the API's check. The state
  checks only what it alone can tell: a host reports only about itself and never moves
  another host, and about a stack only while it runs there, without moving it.
- **In use so far:** `bootstrap` (init), `apply-config`, `add-user` (the first user),
  `set-host-phase` (host actions), `create-stack`, `put-stack` and `delete-stack` (the user),
  `put-volume` and `set-stack-status` (placement, the stack's host), `report-host` (each
  agent). The other kinds (`delete-volume`, hosts joining, operations, users beyond the
  first) are there for the next steps; they are tested, but nothing sends them yet.
- **Stacks.** `create-stack` registers a stack as `created` (no Source, `CreatedBy` from the
  actor, `Owner` the channel: cli or gitops); only `put-stack` moves it on, to `pending`, and
  nothing places a `created` stack. `put-stack` changes registered stacks only.
  `delete-stack` with `WithVolumes` takes the volumes along in the same command, so the
  placement cannot put one back in between.
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

- **Versions.** `CommandVersion` and `SnapshotVersion`; a release reads its own and the one
  before. Payload fields are added, not changed; unknown fields are ignored, so an agent of
  the release before drops a new field from its state. A field that later commands depend on
  therefore needs a gate on `State.MinVersion`. The json tags of `clusterdir.Config` are part
  of this format. No release exists yet; the rule holds from the first one on.
- **Locks.** Operations hold locks (`stack/<name>`, `volume/<stack>/<volume>`); a change to a
  locked stack or volume is refused with a `LockedError`, unless the holding operation makes
  it (`Operation` in the payload). Only failover takes locks over (`TakeOver`).
- **Invariants** the state holds itself, as the last place to catch a wrong command: one
  first user, valid names (`internal/names`), volumes with 0 to 3 replicas and a DRBD minor
  exactly when replicated, one copy per host, a stack deleted only with its volumes, a
  removed host's certificate revoked.
- **Secrets** arrive sealed (`Sealed`, age for the data key); `state` never sees them in
  clear. A stack's secrets come with its `put-stack` and replace the old ones as a whole:
  whoever cannot decrypt the stack's secrets file can remove its secrets with an apply, never
  keep them for a changed stack. The limits are here (`MaxStackSecrets`, `MaxSecretBytes`,
  `MaxStackSecretsBytes`); the state checks the count and the names, the API the sizes it sees
  in clear.
- `Copy` gives a deep copy without the audit, for work too slow for the state's lock (the
  placement parses every stack) or that keeps parts of the state (the agent's stacks).
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
  and a host's report about itself (`hostKinds`). The leader's own commands (operations,
  placements, new hosts) are made on the leader and never come in from outside; a host never
  speaks for another host or for the leader.
- A refusal of the state is HTTP 409 with its reason; a write that did not commit is 503.
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
  directory) and the user (`--user`, `$CLOWDER_USER`, the only credentials file), then talks
  to the cluster with the user's certificate: `cluster status|apply|diff|schema`,
  `host list|drain|undrain|remove|enable`, `stack create|apply|delete|ps|logs`.
- **`stack create`** registers the stack (`POST /v1/stacks/<name>`; for the user who created
  it and while it is `created`, again does nothing, so a failed run can be repeated). It then
  writes the missing files of the stack's directory and never overwrites one; an existing
  `clowder.yaml` must be the stack's. Stack directories belong outside the cluster
  directory, whose `.sops.yaml` rule would match their files; users there point the CLI at
  the cluster with `$CLOWDER_CLUSTER`. Colleagues are added with sops itself: their key into
  the stack's `.sops.yaml`, then `sops updatekeys secrets.sops.yaml` in the stack's
  directory.
- **`stack apply`** loads the stack's files on the user's machine (`compose.Load`, all
  problems at once) and sends the self-contained file (`compose.Stack.Source`). If the stack
  asks for secrets, the CLI decrypts its secrets file (`clusterdir.ReadSOPS`) and sends the
  values asked for, each a non-empty string, in clear inside mutual TLS like `cluster apply`.
  The agent parses the stack again, checks it against the cluster (`checkAgainstCluster`),
  and `sealStackSecrets` checks that exactly the secrets the stack asks for came, within the
  limits, and seals them.
- **`stack delete`** refuses a stack with volumes unless `--delete-volumes` says so. `stack ps`
  and `stack logs` ask the stack's host through `/v1/internal/stacks/<name>/ps|logs`,
  wherever the CLI came in.
- **Who can read a stack's secrets:** whoever can decrypt its secrets file; every host (a
  stack must start anywhere); and, until there are permissions, every user of the cluster
  through a stack named in `stacks.privileged`, as such a container reaches the secrets and
  the data key on its host.
- **`cluster apply`** sends `cluster.yaml` with the secrets of `secrets.sops.yaml` in clear
  inside mutual TLS; the agent seals them before they go into the state. Its hash covers
  both, so `cluster diff` tells changed fields of `cluster.yaml` from changed secrets without
  the cluster showing any secret.
- **The first user** has no certificate yet (`cluster bootstrap`). `GET /v1/bootstrap` gives
  the CA certificate with a `pki.TrustProof` for each valid bootstrap key, and the CLI trusts
  the CA only if one matches its key. `POST /v1/bootstrap`, over TLS checked against that CA,
  carries the user's public key with an HMAC of the bootstrap key and a time
  (`bootstrapMaxAge`). It works once: when the cluster has a user, users add the others. The
  CLI then writes the cluster directory (`clusterdir.Create`).
- The tests run real HTTPS servers: a follower forwarding to the leader, refused callers, the
  CLI against an agent with a cluster directory and SOPS-encrypted credentials.

## The agent (`internal/agent`)

- **Files.** A host's files are under `DefaultDir`: `host.json` (ID, name, addresses, ports),
  `keys/`, `pki/`, `raft/`, `stacks/`, `storage/`, `volumes/`. init and join write
  `host.json` last, so a host with it has everything else.
- **`clowderd init`** (from cloud-init) makes the first host: the CA, the data key, the
  cluster's age key, the host's certificate and WireGuard key, a Raft group with this host as
  its only voter and the bootstrap command. Then it enables and starts `clowderd.service`.
  It refuses a host that has a `raft/` directory.
- **`clowderd run`** (the service) sets up `clowder0` with the node address (a dummy
  interface, so the address exists on a host without peers), the Raft member, and the API on
  the underlay and the node address. It waits up to `catchUpTimeout` for its state to catch
  up with the Raft log, then tells systemd `READY=1`, and feeds the watchdog while the
  state's lock answers.
- `HostParts` are what the agent changes on its host: the network, the storage driver and
  Docker. `clowderd run` passes the real ones, the tests fakes.
- **Reconcilers** run after a change of the state and every `reconcileInterval`, to repair
  what someone changed by hand: `reconcileNetwork` keeps the nft rules as the state says,
  and the stack runner keeps the stacks.
- **The stack runner** (`stackRunner`), for each stack the state places here: render its
  Compose file, `Ensure` its volume copies, write its secrets, run `compose up` unless all of
  it came up before, then report `running`. A failed `compose up` is tried again at the next
  pass. Stacks that left the host get `compose down`; volume copies the state no longer
  places here, or places in a newer generation, are removed.
- **Crashes leave nothing unknown.** The files tell how far a stack got: `volumes.json`
  records its copies before they are made, its `compose.yaml` exists until `compose down`
  worked, its directory goes with the last copy, and its secrets go once `compose.yaml` is
  gone.
- **Secrets live on tmpfs only** (`Files.SecretsDir`, `DefaultSecretsDir`). The agent makes
  the directory itself; a systemd `RuntimeDirectory` would go at each stop of the agent while
  the containers still use it.

  ```
  <SecretsDir>/<stack>/        root only
    files/<name>               readable by all: bind-mounted to /run/secrets/<name>
    env.yaml                   root only: the secret variables, a second Compose file
  ```

  Before `compose up` the runner opens the sealed values and writes what is new or changed,
  by rename, so a running container keeps the old file until it is recreated. After a
  successful `compose up` it removes what the stack no longer asks for. The hash that decides
  about `compose up` covers the rendered file, `env.yaml` and the secret files; a changed
  secret comes with a new generation, whose label recreates the containers.
- **After a boot no container runs** (`restart: "no"`). The runner remembers only in memory
  which files came up, so its first pass runs `compose up` for every stack, and so does the
  pass after Docker restarted (the supervisor's events broke off). The first pass waits until
  the state has caught up with the Raft log, so a host that was away does not start a stack
  that has moved on.
- The agent also reports its version, memory, CPUs and pools (`report-host`, only when
  they changed; the pools' use rounded to `usedBytesStep`), logs Docker in to the registries
  of `secrets.sops.yaml`, runs the supervisor and, while it leads, the placement
  (`controller.RunPlacement`). Every `trimInterval` it trims the volumes.
- **Tests.** The package's test goes the whole way on 127.0.0.1: init, the agent with a
  notify socket, the user's bootstrap, apply and diff, the Floating IP reaching the nft
  rules, and a stack created, applied, listed and deleted. `stacks_test.go` runs single
  passes of the runner against a state in memory: a failed `compose up`, a new volume
  generation, a stack that leaves the host, secrets written, changed and removed. That
  docker compose fails on a missing secret file and recreates a container for a new
  generation is for the live tests: CI has no Docker daemon.

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
- Published ports (DNAT to the stack's host, 1b) and the marks for answers over WireGuard
  (phase 2) join the ruleset later.

## Storage (`internal/storage`)

- A copy of a volume is a thin LV in a pool of its storage class. With replicas, a DRBD 9
  resource sits on it (`RenderResource`, golden files in `testdata/` that `bin/dev
  test-tools` checks with `drbdadm`): quorum majority with `suspend-io`, `auto-promote no`,
  node-id = host ID, port `drbdBasePort` + minor on the node address. With replicas 0 it is a
  plain LV. A copy's LV is `compose.VolumeName(stack, volume, generation)` and its resource
  has the same name, so a restored volume (a new generation) is a new LV next to the old one.
- **`Ensure` does only what is missing:** the LV, the resource, the primary role, ext4, the
  mount and `data/` in it. Markers in the driver's `StateDir` let it pick up after a crash at
  any point (`created`: clowder made the LV; `metadata`: DRBD metadata written; `primary`:
  the first promotion, made with `--force`, worked). It refuses an LV it did not create and
  never wrote metadata on, and a device with another file system: those it leaves to a
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

- `scheduler.Plan` reads the state and returns where pending stacks go and why the others
  wait. So far a stack goes to the first active host where every new volume fits its pool
  (`Fits`: without overcommit, the pool's volumes and the new one stay within the pool less
  its reserve, a class's fallbacks included). Volumes and DRBD minors are taken only for a
  stack that fits whole.
- `controller.RunPlacement` runs on every agent but acts only on the leader. It plans on a
  copy of the state (`State.Copy`), outside the lock Raft's `Apply` waits for, then writes the
  volumes (`put-volume`), then the stack's host and `starting` (`set-stack-status`). The
  stack's host does the rest.

## Containers (`internal/docker`, `internal/supervisor`)

- `docker.Client` talks to Docker's API on its socket for events, start and restart, and
  runs `docker compose` and `docker login` through `internal/command`. `up` gets the
  rendered file and, for a stack with secret variables, `env.yaml`; down, ps and logs need
  the rendered file alone. `compose down` removes the stack's Docker volumes too; they only
  bind a directory of clowder's volume, whose data stays until clowder removes the copy.
- **Docker restarts nothing** (`restart: "no"`), so that after a boot nothing starts before
  the agent has looked at the cluster. The supervisor follows Docker's events and applies
  each service's restart rule with `DefaultBackoff`; `unhealthy` means a restart with the
  same backoff unless the service says otherwise.
- **A stop by hand is kept:** `docker stop`, or a kill with a signal that ends the container
  (`endsContainer`). A kill with another signal, such as a reload with SIGHUP, is no stop.
  While the agent runs `compose up` or `down` for a stack, it `Hold`s the stack, so the
  supervisor does not fight it.
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
