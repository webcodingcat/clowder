# Developing clowder

How the code fits together and which rules hold it together. [STYLE.md](STYLE.md) has the
coding standards, `bin/dev` the commands.

## Getting started

You need Bash and Docker. On Windows, use Git Bash or WSL; Docker Desktop with WSL2 works.
From PowerShell, call it through Git's Bash: `& "C:\Program Files\Git\bin\bash.exe" bin/dev test`.

```bash
bin/dev test     # unit tests, seconds
bin/dev lint     # all style checks
bin/dev build    # binaries into dist/
dist/linux-amd64/clowder version
```

Go is not needed on the host: `bin/dev` runs everything in pinned images (`golang:1.27.3`,
`golangci/golangci-lint:v2.15.0`, ...) and keeps Go's caches in the Docker volume
`clowder-gocache`. If you do have Go 1.27 installed, `go test ./...` works the same.

### Live tests with HLab

The live tests run against HLab, a local stand-in for Hetzner Cloud with real VMs (it needs
`/dev/kvm`). The live tests use only its interface: `hlab env` prints the API endpoint, the
token, the Storage Box access and the Docker network of a running lab. (The spike also reaches
into the lab's container; it goes away with it.) Point `HLAB` at its wrapper:

```bash
export HLAB=/path/to/hlab/bin/hlab
spike/run.sh up      # the spike of phase 0: a lab "clowder" with three hosts
```

The live tests replace `spike/`; they live in `test/live` behind the build tag `live`.

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

Known gaps to close in phase 2, harmless on a single host:

- Raft and the forwarding of writes run on the node address, which other hosts reach only
  through the WireGuard mesh.
- `CatchUp` makes sure only on the leader that the state holds the whole log; a follower
  returns once it knows a leader. Before a host that was away starts its stacks, it must
  have applied the leader's commit index.
- The agent runs its stacks one after another in one loop: a long image pull or `mkfs`
  holds up the others, and a command left behind on a suspended DRBD volume leaves a
  goroutine behind at every pass.

## A stack's way

1. `clowder stack create shop --age <key>` registers the name (`create-stack`): the stack is
   `created`, and nothing places or runs it yet. It writes what is missing of `shop/`:
   `clowder.yaml` (the stack's Compose file, `name: shop`), an empty `secrets.sops.yaml` and
   the `.sops.yaml` that encrypts it for the key.
2. `clowder stack apply shop` loads and checks `shop/clowder.yaml` on the user's machine and
   sends one self-contained Compose file (`PUT /v1/stacks/shop`). The agent that
   takes the call checks it against the cluster and commits `put-stack` through the leader:
   the stack is `pending`.
3. The leader's placement picks a host and a pool for each volume, commits the volumes
   (`put-volume`) and the stack's host (`set-stack-status starting`).
4. That host's agent makes the volumes (LV, DRBD, ext4, mount), renders the Compose file,
   runs `docker compose up` and reports `running`. Its supervisor restarts what crashes.
5. `clowder stack ps|logs shop` asks the stack's host, through any agent.

## Words

| Word | Means |
|---|---|
| underlay | the network the hosts share at the hoster (private network or public addresses) |
| node address | `10.77.0.<ID + 1>`, a host's address in the overlay; on `clowder0`, reached by other hosts through WireGuard |
| Floating IP | the cluster's one public address; the hoster routes it to the ingress host |
| leader | the Raft leader among the agents; it decides (placement, failover) and commits every write |
| voter | a host that takes part in Raft's elections; others follow the log only |
| quorum | a majority of the voters (Raft) or of a volume's copies (DRBD); without it, nothing changes |
| fencing | powering a failed host off through the hoster's API before its stacks start elsewhere |
| tiebreaker | a diskless DRBD member that gives a volume with two copies its third vote |
| active, passive | a host that runs stacks; a second host that waits until there are three |
| actor | who sent a command: `leader`, `host/<id>` or `user/<name>` |
| sealed | encrypted with the cluster's data key before it goes into the state |

## The two binaries

| Binary | Package | Runs on | Does |
|---|---|---|---|
| `clowder` | `cmd/clowder`, `internal/cli` | Linux, macOS, Windows | the user's CLI: reads the cluster directory, talks to an agent over mTLS |
| `clowderd` | `cmd/clowderd`, `internal/agent` | Linux | the agent (`run`, systemd service `clowderd`) and the host commands `check`, `init`; `join` and `recover` to come |

`cmd/clowderd` has the build constraint `linux`. Both get their version from
`internal/buildinfo.Version`, which `bin/dev build` stamps from the Git tag (or
`0.0.0~dev+<commit>`).

## Layers

The package layout is planned for the whole of v1; packages appear as the phases add them.
Packages in *italics* do not exist yet.

- **Portable**: `cli`, `api`, `clusterdir`, `sopsage`, `pki`, `compose`, `units`, `names`
  (the rules for names users give), `buildinfo`, `logging`, `atomicfile` (files written whole
  or not at all). The CLI may import
  only these. `.golangci.yml` (depguard, rule
  `cli-portable`) refuses direct imports of the Linux-only packages in `cmd/clowder` and
  `internal/cli`; the cross build for Windows and macOS in `bin/dev build` catches indirect
  ones.
- **Pure logic, no I/O**: `scheduler`, `state`. Their tests need no host, files or network;
  depguard (rule `state-pure`) keeps `os`, `net/http` and randomness out of both.
  `state` uses the types and `Check` of `clusterdir`, never its file access.
- **Decides**: `controller` (the leader's loops) writes Raft commands. It talks to external
  services only the leader uses (hoster API, ACME, Git remotes, alert channels), but never
  changes the host.
- **Changes the host**: `agent`, `supervisor`, `raftnode` (Raft over mTLS), `datakey` (the
  key file), `command` (host tools with a timeout) and the drivers `network`, `storage`,
  `docker`, *`ingress`*.
- **Reads the host**: `hostcheck` (for `clowderd check`). It compiles anywhere, but only the
  agent uses it, so depguard keeps it out of the CLI like the packages above.

## Stacks (`internal/compose`)

- A stack's files are bound to it by name: the first file says `name: <stack>` as written
  (`CheckNames` reads it before compose-go fills in or normalizes anything) and no later file
  sets one. `Load` and `Parse` compare the project's name with the stack, so a file for one
  stack is refused for another, by the CLI and again by the API.
- `Load` reads Compose files with compose-go and refuses what breaks clowder's model: what ties
  a container to one host (bind mounts except `/etc/localtime` read-only, `secrets` and
  `configs` from files, devices, anonymous and external volumes, external networks), what
  shares the host's namespaces (`network_mode: host`, `pid`/`ipc: host`), and what clowder
  sets itself (`build`, `container_name`, replicas, network drivers and subnets, port ranges,
  host addresses, reserved ports). Every problem is reported at once (`Problems`); only an
  unknown key ends the checks of its `x-clowder` block.
- What reaches beyond the container (`privileged`, a capability other than Docker's default
  set and the few that act in the container's own namespaces, `security_opt` other than
  `no-new-privileges`, `cgroup_parent`) is listed per service (`Service.Privileges`). Such a
  container can read the host's keys, other stacks' volumes and secrets, and change the rules
  that keep containers apart, so the API allows it only for stacks that `cluster.yaml` names
  in `stacks.privileged`; and `cluster apply` refuses a `cluster.yaml` that a stack of the
  cluster would no longer pass (its privileges, storage classes, priorities).
- `Source` fills in the variables and env files of the user's machine; the result is stored
  in the cluster state and on the stack's host as it is, not sealed. Passwords belong in the
  stack's secrets file, never in a `${VARIABLE}` or the Compose file.
- Secrets: a service asks with `x-clowder.secrets: [name]` (a file `/run/secrets/<name>` in
  the container, for images that read `*_FILE`) or `x-clowder.secret_env: {VARIABLE: name}`.
  The stack names its SOPS file in `x-clowder.secrets_file` (relative to its first file; a
  map of names to strings; several stacks may share one). Compose's own `secrets:` are
  refused, and so are `configs:` with `file` or `environment`: docker compose on the host
  would read the agent's environment. `Source` leaves `secrets_file` out (the user's path).
  `Render` writes the secrets as Compose secrets with `file:` in `Target.SecretsDir`;
  `RenderSecretEnv` writes the variables as a second Compose file, which `bin/dev
  test-tools` checks together with the rendered one (golden file
  `testdata/shop/secrets.golden.yaml`). A variable ends up in Docker's container
  configuration on the host's disk and in `docker inspect`; a file does not.
- The `x-clowder` blocks are decoded strictly: an unknown key is an error, not ignored.
- `Render` writes the file docker compose gets on a host: `restart: "no"` (the agent restarts),
  the agent's resolver, each port on the Floating IP and the node address, labels, a fixed
  bridge name per network (`BridgeName`), volumes bound to `<mount>/data`. Every `$` in a
  value becomes `$$` (`EscapeDollars`): the values were filled in on the user's machine, and
  docker compose on the host would fill in variables once more, from the agent's
  environment.
- The rendered example `testdata/shop/rendered.golden.yaml` is the golden file. After an
  intended change: `bin/dev test ./internal/compose -update`, then review the diff.
  `bin/dev test-tools` checks it with Debian's `docker-compose` and that it fills in nothing.

## Cluster directory (`internal/clusterdir`)

- `Find` picks the directory (`--cluster`, `$CLOWDER_CLUSTER`, the working directory), `Load`
  reads `cluster.yaml` strictly and `Check` lists every problem.
- `Secrets` and `Credentials` decrypt `secrets.sops.yaml` and `credentials/<user>.sops.yaml`
  with the user's age key, as SOPS finds it (`SOPS_AGE_KEY_FILE`, `SOPS_AGE_KEY`, SOPS's
  default path). `testdata/test-age-key.txt` is a test-only key for the fixtures.
- `Schema` makes the JSON schema of `cluster.yaml` from the Go types.

## SOPS files (`internal/sopsage`)

The SOPS library brings the SDKs of every cloud KMS along (the CLI grew from 2.5 to 44 MB),
so `sopsage` implements the part of the SOPS format clowder needs: YAML files with age keys,
read and written.

- `Decrypt` opens the data key with one of the identities, decrypts every value (AES-256-GCM,
  the key path as additional data) and checks the MAC; `Encrypt` writes what `sops encrypt`
  writes with default settings. Comments are dropped.
- `LoadIdentities` looks where SOPS looks: the SSH key, `SOPS_AGE_KEY`, `SOPS_AGE_KEY_FILE`
  or `sops/age/keys.txt` in the user's config directory. `SOPS_AGE_KEY_CMD` is not
  supported.
- Files with settings it does not implement (`*_regex`, key groups, no age recipient) are
  refused, not guessed at.
- `testdata/plain*.sops.yaml` were written by the real sops from `testdata/plain.yaml`.
  `bin/dev test-sops` (part of `all` and CI) runs `cross_test.go` (build tag `sops`)
  against Alpine's sops in both directions.

## Cluster state (`internal/state`)

Every agent holds the same `State` and applies the same `Command`s in Raft's order.

- `Apply` is deterministic: no clock (a command carries the leader's time in `At`), no
  randomness, no I/O. A handler checks everything before it changes anything, so a refused
  command leaves no trace; the state's `Index` follows the log either way.
- A command has a kind, a payload of the type with the same name (`AddHost`, `PutStack`, ...)
  and an actor (`leader`, `host/<id>`, `user/<name>`) that the API takes from the caller's
  certificate. Which actor may send which kind is the API's check; the state checks only
  what it alone can tell: a host reports only about itself and never moves another host,
  and about a stack only while it runs on that host, without moving it.
- In use so far: `bootstrap` (init), `apply-config`, `add-user` (the first user),
  `set-host-phase` (host actions), `create-stack`, `put-stack` and `delete-stack` (the user;
  with `WithVolumes` the volumes go in the same command, so the placement cannot put one back
  in between), `put-volume` and `set-stack-status` (placement, the stack's host),
  `report-host` (each agent). The other kinds (`delete-volume`, hosts joining, operations,
  users beyond the first) and the fields they fill are there for the next steps; they are
  tested, but nothing sends them yet.
- A stack is registered before it is applied: `create-stack` makes it `created` (no Source,
  `CreatedBy` from the actor, `Owner` the channel: cli or gitops). Only `put-stack` moves it
  on, to `pending`; nothing places a `created` stack or sets its status. `put-stack` changes
  registered stacks only.
- Host phases (`phaseChanges`; L = the leader, on its own or for a user; H = the host itself):

  ```
  joining ─L→ passive ─L→ active ⇄L draining
     └──────L──────────────↗  │ L        │ L
                         failed ←────────┘
             active ←H── failed ─L→ fenced ─H→ recovering ─H→ active
                                    fenced ⇄L disabled (flapping brake)
  any phase ─L→ removed
  ```

- Versions: `CommandVersion` and `SnapshotVersion`, from 1; this release reads its own and
  the one before. Payload fields are added, not changed; unknown fields are ignored. The
  json tags of `clusterdir.Config` are part of this format.
- Operations hold locks (`stack/<name>`, `volume/<stack>/<volume>`); a change to a locked
  stack or volume is refused with a `LockedError`, unless the holding operation makes it.
  Only failover takes locks over (`TakeOver`).
- Invariants the state holds itself, as the last place to catch a wrong command: one first
  user, valid names, volumes with 0 to 3 replicas and a DRBD minor exactly when replicated,
  one copy per host, a stack deleted only with its volumes, a removed host's certificate
  revoked. `time.Now` is banned from the package by lint (forbidigo).
- `Copy` gives a deep copy without the audit, for work too slow for the state's lock (the
  placement parses every stack) or that keeps parts of the state (the agent's stacks).
- Secrets arrive sealed (`Sealed`, age for the data key); `state` never sees them in clear.
  A stack's secrets come with its `put-stack` and replace the old ones as a whole: whoever
  cannot decrypt the stack's secrets file can remove its secrets with an apply, never keep
  them for a changed stack. The limits (`MaxStackSecrets`, `MaxSecretBytes`,
  `MaxStackSecretsBytes`) are here; the state checks the count and the names, the API the
  sizes it sees in clear.
- Snapshots are gzip-compressed JSON (`WriteSnapshot`, `ReadSnapshot`). The audit keeps the
  last 10 000 commands, finished operations the last 100.

## Certificates (`internal/pki`)

- The cluster CA issues host certificates (server and client, valid for the host's addresses)
  and user certificates (client). The role is the certificate's organizational unit, the
  name its common name (`host-<id>` or the user's name).
- Keys are Ed25519 and never leave the machine that made them: the CA signs public keys.
- Certificates end by revocation, not expiry (CA 20 years, others 10): hosts may stay off
  for months. `Verify` takes the revoked serials from the state.
- `TrustProof` (HMAC-SHA256 with the bootstrap key over the CA certificate) is how a CLI or
  a joining host trusts the CA on first contact.

## Raft (`internal/raftnode`) and the data key (`internal/datakey`)

- `raftnode` runs hashicorp/raft: log and stable store in BoltDB (`raft.db`), a snapshot every
  8192 entries (2 kept), the state of `internal/state` as the replicated machine. Raft's own
  log lines go through `log/slog` (hclog's bridge).
- The network is mutual TLS on port 7400 of the node address: both sides show a host
  certificate of the cluster CA that is not revoked (the state's list). Users' certificates and
  other clusters' hosts fail the handshake. Removing a host revokes its certificate, so it
  leaves Raft and the API at once. `pki.ClientTLSToHosts` and `pki.ServerTLSForHosts` are the one way
  hosts are checked, by Raft and by the API alike: the chain and the role, not the name.
- `Propose` commits a command through the leader and returns its index, or the state's
  refusal. `Read` gives the current state under a lock; nothing may keep it, as `Apply` changes
  it in place. `WaitIndex` lets a read follow a write made through another member.
- `Bootstrap` makes the first host a cluster of its own; on a member with a log it does
  nothing. `AddMember` is for joins.
- The data key is an age identity in a file only root reads (`/var/lib/clowder/keys`).
  `Seal` and `Open` turn secrets into `state.Sealed` and back; joining hosts get the key with
  their join answer.
- The tests run real members on 127.0.0.1 (a restart from disk, three hosts over mTLS,
  strangers refused); `go test -race` passes on them.

## API (`internal/api`) and the CLI (`internal/cli`)

- Every agent serves `/v1` on port 7443 with mutual TLS. Each call is checked against the CA
  and the state's revocation list, so a revoked certificate is refused at once on every agent.
  User routes take user certificates, `/v1/internal/*` only host certificates.
- Any agent answers. Reads come from its own copy of the state; writes it commits as the
  leader or forwards to the leader's `/v1/internal/propose`. A write answers with its Raft
  index (`X-Clowder-Index`); the client sends it on with later reads (`X-Clowder-Min-Index`),
  and the agent waits until it has applied it.
- The actor of a command is the caller of the first agent; its time (`At`) is the leader's
  clock. `/v1/internal/propose` takes two things only: a user's command that a follower
  forwards (the kinds the user routes make), and a host's report about itself (its version,
  its phase, a step done). The leader's own commands (operations, placements, new hosts) are
  made on the leader and never come in from outside; a host never speaks for another host or
  for the leader. A forwarding host names the user, since the user's certificate ended at the
  first agent.
- A refusal of the state is HTTP 409 with its reason; a write that did not commit is 503.
- `api.HostActions` lists the user's host actions with the phases they start from; the server
  checks them and the CLI makes its subcommands from them.
- CLI and agent refuse each other more than one minor version apart (`X-Clowder-Version`,
  `buildinfo.Compatible`); development builds and callers without a version talk to
  everything.
- The client tries the seeds of `cluster.yaml` in turn and trusts any host certificate of the
  cluster's CA, whatever name the seed has. A read goes on to the next seed after any network
  error; a write only if it never left (`httptrace`), since one that was sent may have
  committed without an answer.
- An operation that holds a lock names itself in the change (`Operation` in `PutStack`,
  `PutVolume`, ...); everyone else is refused while the lock is held.
- The CLI finds the cluster directory (`--cluster`, `$CLOWDER_CLUSTER`, the working directory)
  and the user (`--user`, `$CLOWDER_USER`, the only credentials file), then talks to the
  cluster with the user's certificate: `cluster status|apply|diff|schema`, `host list`,
  `host drain|undrain|remove|enable`, `stack create|apply|delete|ps|logs`.
- `stack create <name>` registers the stack (`POST /v1/stacks/<name>`; again for a stack
  still `created` it does nothing, so a failed run can be repeated) and writes the missing
  files of its directory (`--dir`, default `./<name>`), never overwriting one: an existing
  `clowder.yaml` must be the stack's, and `--age` is needed only for a SOPS file to write
  (`clusterdir.WriteSOPS`, `WriteSOPSConfig`, shared with `cluster bootstrap`). Stack
  directories belong outside the cluster directory, whose `.sops.yaml` rule would match
  their files; working there, users point the CLI at the cluster with `$CLOWDER_CLUSTER`.
  Colleagues are added with sops itself: their key into the stack's `.sops.yaml`, then
  `sops updatekeys secrets.sops.yaml` in the stack's directory.
- `stack apply` loads the stack's files on the user's machine (`compose.Load`, all problems
  at once; by default `<name>/clowder.yaml`) and sends the merged, self-contained file
  (`compose.Stack.Source`). If the stack asks for secrets, it decrypts the secrets file with
  the user's age key (`clusterdir.ReadSOPS`, which refuses values left in clear, as for the
  cluster's own SOPS files) and sends the values asked for, each a string and not empty, in
  clear inside mutual TLS like `cluster apply`. The agent
  parses it again and checks it against the cluster (storage classes, privileges), and
  `sealStackSecrets` checks that exactly the secrets the stack asks for came, within the
  limits, and seals them. No message names a value. `stack delete` refuses
  a stack with volumes unless `--delete-volumes` says so. `stack ps <name>` and `stack logs`
  ask the stack's host through `/v1/internal/stacks/<name>/ps|logs`, wherever the CLI came
  in.
- Who can read a stack's secrets: whoever can decrypt its secrets file, and every host (a
  stack must start anywhere). Until there are permissions, also every user of the cluster
  through a stack named in `stacks.privileged`: such a container reaches the secrets and the
  data key on its host. Every request body is limited to 1 MiB (`maxRequestBytes`).
- `cluster apply` sends `cluster.yaml` with the secrets of `secrets.sops.yaml` in clear inside
  mutual TLS (`PUT /v1/cluster`); the agent seals them with the data key before they go into
  the state. Its hash covers both, so `cluster diff` tells changed fields of `cluster.yaml`
  from changed secrets without the cluster showing any secret.
- The first user has no certificate yet (`cluster bootstrap`): `GET /v1/bootstrap` gives the
  CA certificate with a `pki.TrustProof` for each valid bootstrap key, and the CLI trusts the CA
  only if one matches its key. `POST /v1/bootstrap`, over TLS checked against that CA, carries
  the user's public key with an HMAC of the bootstrap key and a time (at most 5 minutes off).
  It works once: when the cluster has a user, users add the others. The CLI then writes the
  cluster directory (`clusterdir.Create`).
- The tests run real HTTPS servers: a follower forwarding to the leader, refused callers, the
  CLI against an agent with a cluster directory and SOPS-encrypted credentials.

## The agent (`internal/agent`)

- A host's files are under `/var/lib/clowder`: `host.json` (ID, name, addresses, ports),
  `keys/` (data key, WireGuard key), `pki/` (CA certificate, host certificate and key), `raft/`.
  init and join write `host.json` last, so a host with it has everything else.
- `clowderd init --bootstrap-key ... --underlay ...` (from cloud-init) makes the first host:
  the CA, the data key, the cluster's age key, the host's certificate and WireGuard key, a
  Raft group with this host as its only voter and the bootstrap command. Then it enables and
  starts `clowderd.service`. It refuses a host that has a `raft/` directory.
- `clowderd run` (the service): the node address on `clowder0`, the Raft member, the API on
  the underlay and the node address. It waits up to 15 s for its state to catch up with the
  Raft log (on the leader a barrier), then tells systemd `READY=1`, and feeds the watchdog
  while the state's lock answers.
- `HostParts` are what the agent changes on its host: the network, the storage driver and
  Docker. `clowderd run` passes the real ones, the test fakes.
- `clowder0` is a dummy interface with the node address (`internal/network`), so the address
  exists on a host without peers; `run` creates it at every start.
- `run` also starts the containers' DNS resolver and keeps the nft rules as the state says
  (`reconcileNetwork`): after a change of the state when the rules differ, and every 30 s to
  repair what someone changed by hand. `HostNetwork` is what the agent changes in the
  network; the test uses a fake.
- The stacks of the host (`stackRunner`, a pass after a change of the state and every 30 s):
  for each stack the state places here, render its Compose file, `Ensure` its volume copies,
  write the file to `/var/lib/clowder/stacks/<name>/compose.yaml` and run `compose up`
  unless that file came up before, then report `running`. A failed `compose up` is tried
  again at the next pass. Stacks that left the host get `compose down`; volume copies the
  state no longer places here (or in an older generation) are removed. Each stack's
  `volumes.json` records its copies before they are made, so a crash leaves nothing unknown.
- A stack's secrets live on tmpfs only (`Files.SecretsDir`, `/run/clowder-secrets`, made by
  the agent itself, not a systemd `RuntimeDirectory`, which would go at each stop of the
  agent while containers use it):

  ```
  /run/clowder-secrets/<stack>/      0700
    files/<name>                     0444  bind-mounted to /run/secrets/<name>
    env.yaml                         0600  the secret variables, a second Compose file
  ```

  Before `compose up` the runner opens the sealed values and writes what is new or changed
  (by rename: a running container keeps the old file until it is recreated); after a
  successful `compose up` it removes what the stack no longer asks for. The hash that decides
  about `compose up` covers the rendered file, `env.yaml` and the secret files; a changed
  secret comes with a new generation, whose label recreates the containers. The secrets of a
  stack that is not placed here and has no Compose file left go at each pass.
- After a boot no container runs (`restart: "no"`): the runner remembers which files came
  up only in memory, so its first pass runs `compose up` for every stack, and so does the
  pass after Docker restarted (the supervisor's events broke off). The first pass waits until
  the state has caught up with the Raft log, so a host that was away does not start a stack
  that has moved on.
- The agent also reports its version, memory, CPUs and pools (`report-host`, only when
  they changed; the pools' use rounded to GiB), logs Docker in to the registries of
  `secrets.sops.yaml`, runs the supervisor and, while it leads, the placement
  (`controller.RunPlacement`). Once a week it trims the volumes (`storage/last-trim`).
- The test of the package goes the whole way on 127.0.0.1: init, the agent with a notify
  socket, the user's bootstrap (and a wrong key, a second user refused), apply and diff, the
  Floating IP reaching the nft rules, and a stack with a volume applied, listed and deleted.
  `stacks_test.go` runs single passes of the runner against a state in memory: a failed
  `compose up`, a new volume generation, a stack that leaves the host, secrets written,
  changed and removed. That docker compose fails on a missing secret file, and recreates
  a container for a new generation, is for the live tests: CI has no Docker daemon.

## Network (`internal/network`)

- `clowder0` carries the node address and the resolver's address `169.254.77.53`.
- The table `inet clowder` (`Render`, golden file `testdata/isolation.golden.nft`) keeps
  containers in their place. From a container bridge (`cbr-*`, `docker0`, `br-*`) the host
  answers only the resolver, the local Traefik on the Floating IP (HTTP hairpin) and the
  container's own connections. Beyond the host, the metadata API (`169.254.169.254`, whose
  user data holds the bootstrap key) is dropped, and so are other hosts' node addresses
  unless clowder redirected the packet there itself (DNAT). `Apply` checks with `nft -c`, then
  replaces the table in one transaction; Docker's rules stay untouched.
- The resolver answers on `169.254.77.53:53` (UDP and TCP) and passes every question on to
  the host's resolvers (`/etc/resolv.conf`). Names of exported services come with exports.
- Published ports (DNAT to the stack's host, 1b) and the marks for answers over WireGuard
  (phase 2) join the ruleset later.

## Storage (`internal/storage`)

- A copy of a volume is a thin LV in a pool of its storage class. With replicas, a DRBD 9
  resource sits on it (`RenderResource`, golden files in `testdata/` that `bin/dev test-tools`
  checks with `drbdadm`): quorum majority with
  `suspend-io`, `auto-promote no`, node-id = host ID, port 10000 + minor on the node address,
  `max-peers 4`. With replicas 0 it is a plain LV.
- `Driver.Ensure` does only what is missing: the LV, the resource, the primary role, ext4,
  the mount under `/var/lib/clowder/volumes/<stack>/<volume>` and `data/` in it. Markers in
  `/var/lib/clowder/storage` let it pick up after a crash at any point: `<LV>.created` (clowder
  made the LV), `<resource>.metadata` (DRBD metadata written), `<resource>.primary` (the
  first promotion, made with `--force`, worked). It refuses an LV it did not create and never
  wrote metadata on, and a device with another file system: those it leaves to a human. (A
  crash between `lvcreate` and its marker leaves an empty LV that a human must look at.)
- It reads the tools' own exit codes: only `lvs` exiting with 5 means "no LV", only `blkid`
  with 2 "no file system", only `findmnt` with 1 "not mounted". Any other failure (a timeout,
  a device not ready) stops `Ensure`, so a guess never creates over or formats real data.
- The mount guard: containers get `data/`, which exists only in the mounted file system, so a
  container whose volume is not mounted fails to start instead of writing to the root file
  system.
- `PoolUsage` reads a pool's size and how full its data and metadata are.
- `Trim` (fstrim) gives deleted blocks back to the pool.
- A copy's LV is `compose.VolumeName(stack, volume, generation)`; its DRBD resource has the
  same name, so a restored volume (a new generation) is a new LV next to the old one.

## Placement (`internal/scheduler`, `internal/controller`)

- `scheduler.Plan` reads the state and returns where pending stacks go and why the others
  wait. So far a stack goes to the first active host where every new volume fits its pool
  (`Fits`: without overcommit, the pool's volumes and the new one stay within the pool less
  its reserve; a class's fallbacks included). Volumes and DRBD
  minors are only taken for a stack that fits whole.
- `controller.RunPlacement` runs on every agent but acts only on the leader: it plans on a
  copy of the state (`State.Copy`), outside the lock Raft's `Apply` waits for, then writes the
  volumes (`put-volume`), then the stack's host and `starting` (`set-stack-status`). The
  stack's host does the rest.

## Containers (`internal/docker`, `internal/supervisor`)

- `docker.Client` talks to Docker's API on its socket for events, start and restart, and
  runs `docker compose` (up, down, ps, logs) and `docker login` through `internal/command`.
  `up` gets the rendered file and, for a stack with secret variables, `env.yaml`; down, ps
  and logs need the rendered file alone.
  `command.Stream` passes the logs on as they come; it has no timeout, as it touches no
  volume.
- Docker restarts nothing (`restart: "no"`); the supervisor follows Docker's events and
  applies each service's restart rule with a backoff (1 s doubling to 60 s, reset after
  10 minutes up). A container stopped by hand (kill, stop) stays stopped; `unhealthy` means
  a restart unless the service says otherwise. While the agent runs `compose up` or `down`
  for a stack, it `Hold`s the stack, so the supervisor does not fight it.
- A start that fails counts as another crash and is tried again after the backoff; a
  container that is gone (`docker.ErrNoContainer`) is let be. `destroy` events drop what the
  supervisor knew about a container. When the events break off, what died in between went
  unseen (a restart of Docker kills every container): the supervisor tells the agent, which
  then runs `compose up` for every stack as after a boot.
- `compose down` removes the stack's Docker volumes too; they only bind a directory of
  clowder's volume, whose data stays until clowder removes the copy.

## Host tools (`internal/command`)

Every host tool runs through `command.Run` or `command.RunWithInput`, with no exception (lvs,
drbdadm, mkfs, mount, nft, ip, systemctl, docker, ...): with a timeout, and left behind if it
does not finish. `command.Stream` (logs that follow) is the one without a timeout; only tools
that touch no volume use it. A process that touches a volume can hang in state D while DRBD suspends I/O, and
then not even SIGKILL ends it; any other tool that hangs must not stop a reconciler either.
`command.ExitCode` tells a tool's own exit code from a timeout or a missing binary.

## Host check (`internal/hostcheck`, `clowderd check`)

Checks the operator base and changes nothing: sysctls, time sync, watchdog, WireGuard, DRBD 9
built for the running kernel, drbd-utils ≥ 9.28, DRBD autostart and usage report off, LVM
filter, thin pools (`--pool`), Docker's `daemon.json`, docker compose, unattended-upgrades and
the Floating IP (`--floating-ip`). Each check reads through the `Host` interface, so the unit
tests use a fake host; exit status 1 if any check fails.

Each command has 10 s. One that hangs (`lvs` while DRBD suspends I/O) is left behind with its
goroutine: fine for `clowderd check`, which exits; the agent will run the checks in their own
process for the same reason (Volumes).

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

`packaging/` holds the nfpm configurations, the systemd units and the
maintainer scripts (reload systemd after install and removal, stop the agent before removal).

- `clowderd` depends on Debian packages and on `drbd-dkms` and `drbd-utils` from LINBIT's
  public repository (dist `proxmox-9`); the host's APT sources must list it. `coccinelle`
  lets the DKMS build run offline.
- `clowderd.service`: `Type=notify`, `WatchdogSec=15`, `Restart=always` without a start
  limit, `KillMode=process` (helpers finish their step; containers belong to dockerd). The
  package installs it without enabling it; `clowderd init` enables and starts it.
- `clowder-tasks.slice`: memory limit for the agent's short-lived tasks.

## CI

`.github/workflows/ci.yml` runs `bin/dev lint`, `bin/dev test`, `bin/dev test-sops`,
`bin/dev test-tools` and `bin/dev package` on every push and pull request and keeps `dist/` as
an artifact. `test-tools` checks the renderers' golden files with Debian's `nft -c`,
`drbdadm` and `docker-compose` (`test/tools/check-golden-files.sh`). The live tests need `/dev/kvm` and stay
manual.

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
