# The cluster

Part of [DEVELOPING.md](../../DEVELOPING.md): the replicated state, the certificates,
Raft, the API and the CLI, and the placement.

## Cluster state (`internal/state`)

Every agent holds the same `State` and applies the same `Command`s in Raft's order.

- **Deterministic.** `Apply` uses no clock (a command carries the leader's time in `At`), no
  randomness and no I/O; lint bans the clock from the package (forbidigo).
- **A refused command leaves no trace.** A handler only reads the state: it checks the
  command and returns the change it makes as a function, which `Apply` runs only when the
  handler returned no error. The change cannot fail, so nothing can refuse a command half
  way through. That costs nothing, where working on a copy of the state would copy all of
  it at every command (see `Copy`). The state's `Index` follows the log either way. The
  tests check what the types cannot, that the checks change nothing: every command a test
  may see refused (`apply`, `changes`, `refuse`) is compared with the state before it.
- **Commands.** A command has a kind, a payload of the type with the same name (`AddHost`,
  `PutResource`, ...) and an actor (`leader`, `host/<id>`, `user/<name>`) that the API takes from
  the caller's certificate. Which actor may send which kind is the API's check. The state
  checks only what it alone can tell: a host reports only about itself and never moves
  another host, and about a stack only while it runs there, without moving it.
- **The commands in use so far,** by who sends them:

  | Who | Commands |
  |---|---|
  | `clowderd init` | `bootstrap` |
  | the user | `apply-config`, `add-user` (the first user), `recover-user` and `set-recovery-recipient` (with a code from a host), `set-host-phase` (host actions), `put-resource` and `delete-resource` (`/v1/apply`, `/v1/delete`; the leader decides), `restart-stack` |
  | the placement, the stack's host | `put-volume`, `set-stack-status`, `set-stack-wait-reason`, `set-pushed-out` |
  | the Floating IP's loop | `set-ingress` |
  | the ACME loop | `put-acme-account`, `put-acme-challenge`, `delete-acme-challenge`, `put-certificate`, `delete-certificate` |
  | the alerts | `put-alert`, `resolve-alert` |
  | each agent | `report-host`, `forget-copy` |

  The other kinds (hosts that join, operations, users beyond the first) are for the next
  steps. They are tested, but nothing sends them yet.
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
    `find`, `put` and `remove`, where `put` and `remove` return their change like a
    handler. A handler checks in a fixed order:
    1. `Authorize`: may this actor do this to this kind in this namespace? It allows
       everything until users get permissions. It sits in the state so that the leader
       decides, not the agent that took the request.
    2. The owner: a resource of another channel is refused, even when the spec would not
       change.
    3. Whether anything changes at all.
    4. Everything else.
  - **Unchanged is not a change.** When a spec equals the stored one, nothing changes:
    neither `Generation` nor `ResourceVersion`, and no audit entry is written. So a GitOps
    loop that applies the same files restarts nothing and keeps the audit short. A secret
    is compared by its digest, because the same value seals differently every time; a
    stack by its normalized spec. The status commands (`put-volume`, `set-stack-status`,
    `set-stack-wait-reason`, `set-pushed-out`) work the same way. Such a handler returns
    `errUnchanged`, and `Apply` takes the command as applied without a trace.
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
    per 64 KiB); a test in `datakey` holds that against real sealing. The cluster also
    holds at most `resource.MaxClusterSecrets`. One function holds them all
    (`checkSecretLimits`), for the resources and `cluster.yaml`'s values together, and
    both `put-resource` and `apply-config` ask it. A secret a stack names cannot be
    deleted.
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
- **Revocations.** `Revoked` holds each revoked certificate with when it expires, which the
  state knows from the command that recorded the certificate (`AddUser`, `RecoverUser`,
  the host's `CertificateNotAfter`); `revoke` puts it there. The list grows only there, so
  `revoke` also drops the certificates that expired more than `revokedKept` before the
  command: no handshake takes them any more, and the margin covers a host whose clock runs
  behind. It goes by the command's `At`, so every agent drops the same. The command
  `revoke` takes only a serial the state knows, a user's or a host's.
- **Secrets** arrive sealed (`Sealed`, age for the data key); `state` never sees them in
  clear. Those of `cluster.yaml` are `ClusterSecrets`, by name (`clusterspec.SecretPlaces`),
  each with a digest; the namespaces' are `Secrets`. Readers find a value through the
  spec, never by taking a name apart: `ClusterSecret(config.Hoster.Token)`,
  `ValidJoinKeys`, `RegistryLogins` (the user from the spec, the password sealed). Until the
  first `apply-config` the config is revision 0 and names only init's bootstrap key
  (`FirstJoinKey`); the loops that need a hoster or a Floating IP find none and wait. A
  stack names at most `MaxStackSecrets`. Who can read a secret: API,
  below.
- `Copy` gives a deep copy without the audit, for work too slow for the state's lock (the
  placement weighs every stack) or that keeps parts of the state (the agent's stacks). The
  loops do not copy themselves: `raftnode.Node.SharedCopy` makes one copy per change, which
  every loop reads and none changes; a dry run, which applies commands to its copy, makes
  its own.
- Snapshots are gzip-compressed JSON (`WriteSnapshot`, `ReadSnapshot`). The audit keeps the
  last `auditSize` commands, finished operations the last `keptFinished`.

## Certificates (`internal/pki`)

- The cluster CA issues host certificates (server and client, valid for the host's addresses)
  and user certificates (client). The role is the certificate's organizational unit, the
  name its common name (`host-<id>` or the user's name).
- Keys are Ed25519 and never leave the machine that made them: the CA signs public keys.
- Certificates end by revocation, not expiry: hosts may stay off for months. `Verify` takes
  the revoked serials from the state, which keeps each until a while after it expired.
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
  (`MaxRequestBytes`, its comment works out why 16 MiB); a larger one answers 413, and
  `apply` then advises to apply the namespaces' directories one at a time.
- **Any agent answers.** Reads come from its own copy of the state; writes it commits as the
  leader or forwards to the leader's `/v1/internal/propose`. A write answers with its Raft
  index (`X-Clowder-Index`); the client sends it on with later reads (`X-Clowder-Min-Index`),
  and the agent waits until it has applied it.
- **Who speaks.** The actor of a command is the caller of the first agent; its time (`At`) is
  the leader's clock. `/v1/internal/propose` takes two things only: a user's command that a
  follower forwards (`userKinds`, exactly the kinds the user routes, the bootstrap and the
  recovery make),
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
- **Exit codes**, the same for every command:

  | Code | When |
  |---|---|
  | 0 | the command did what was asked |
  | 2 | a wrong call: an unknown command, or flags or arguments that do not fit (`usageError`) |
  | 1 | anything else: refused, missing, not reachable |

  `checkArgs` turns every command's argument errors into a `usageError`. It also gives
  each command that only holds other commands (`clowder stack`) a `RunE`: that shows the
  help and takes an unknown subcommand as a wrong call, where cobra would end as if done.
  A refusal shows the state's reason without the command's kind (`state.CommandError`).
- **`apply -f <file|directory|->`** (`resources.go`, `files.go`) reads YAML documents
  (`resource.ParseDocuments`: clowder's resources only, or a file of other documents, never
  both; another apiVersion of clowder's is an error). A directory's YAML files are read,
  not those below it; hidden files, other files and files of other documents are skipped
  and named on stderr, and a cluster directory is refused. A `Secret`'s value stands
  encrypted in `spec.value`, tagged `!secret` (`resource.ParseDocuments` takes it out
  before it turns YAML into JSON, and refuses `!secret` anywhere else); the CLI sends it as
  it is (`SecretSpec.Encrypted`). A `Secret` in clear is refused, in a file and on stdin:
  the CLI never handles a value in clear. `secret encrypt [<namespace>/]<name>` writes a
  Secret, its value encrypted for the recipients the cluster names and for the Secret's
  ID. A resource without a namespace is in `default`. The resources go namespaces first, then
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
- **A Secret's value** the agent that takes the request opens with the cluster's age key
  (`openValue`) and refuses unless it was encrypted for the Secret's ID: a value copied
  from another Secret or from `cluster.yaml` (the hoster's token, which a container would
  then read) does not pass. Then `sealSecret` as before.
- **Who can read a secret:**
  - whoever holds the recovery key: every value in the files is encrypted for it too;
  - every host, as a stack must be able to start anywhere;
  - until there are permissions, every user of the cluster: through a stack that names
    the secret, or through a stack in `stacks.privileged`, whose containers reach the
    secrets and the data key on their host.
- **`cluster apply`** (`PUT /v1/cluster`, `ClusterApply`) sends the spec with the
  secrets' names, the values still encrypted by name, and the files. The agent opens each
  value with the cluster's age key (`clusterIdentity`, sealed in the state), refuses one
  that is not for `cluster` (a `Secret`'s value copied into `cluster.yaml`), and seals it
  with a digest (`SealClusterSecret`: an HMAC over name and value with a key of its own).
  `apply-config` takes a value for every name the spec has and no other.
- **`cluster diff`** is the same request with `?dryRun=true`: the agent applies it to a
  copy of its state, with every check of a real apply (a refusal is 409 with the reason),
  and answers what would change (`ClusterDiff`): top-level fields, files, and secrets by
  name. The digests tell a changed value from one encrypted anew; no value leaves the
  cluster.
- **`GET /v1/encryption`** names the recipients every value is encrypted for: the
  cluster's age key and the recovery key (`State.RecoveryRecipient`).
- **`secret rekey -f <file|directory>`** (`cli/rekey.go`) brings every value of the files
  to these recipients: a value made by hand with `age` may lack the recovery key, and after
  a new recovery key every value does. `fileValues` (`cli/values.go`) reads the values and
  what each must be for: in a file named `cluster.yaml`, `cluster`; in any other file, read
  as apply reads it, its Secret's ID. The cluster encrypts them anew
  (`POST /v1/encryption/rekey`, in requests below `MaxRequestBytes`), refuses one meant for
  another place as an apply would (`secretvalue.OpenFor`), answers only with ciphertext,
  and logs who asked: nothing goes into the state. `secretvalue.Rewrite` puts the
  new armor in place, the file written whole or not at all. With `--identity`, the CLI
  opens the values itself with the recovery key, checks the same, and encrypts them for
  the cluster it talks to: the way to a new cluster when the old one is lost. A refused
  file stops the run; the files reported before it are done.
- **`secret decrypt --identity <file|-> -f <file>`** (`cli/decrypt.go`) is for an
  emergency and needs no cluster: one line per value, where it is (a Secret's ID, else the
  path in `cluster.yaml`), a tab, the value. A value that is not printable text, has a
  space at either end, or starts like a marker shows as `base64:`; one that does not open
  or is meant for another place shows as `error:` with the reason, and the command fails
  after printing every line.
- **The first user** has no certificate yet (`cluster bootstrap`). `GET /v1/bootstrap` gives
  the CA certificate with a `pki.TrustProof` for each valid bootstrap key, and the CLI trusts
  the CA only if one matches its key (`api.FindCluster`). The offer also says whether the
  cluster has a user already (`bootstrapped`): then the CLI stops before it makes a
  recovery key, so nobody holds two and does not know which counts. Else it makes the
  recovery key and hands it over: shown once with its last 8 characters typed back, or
  into a new file (`--recovery-out`, for scripts); nothing has reached the cluster yet.
  `POST /v1/bootstrap`, over TLS checked against that CA, carries the user's public key
  and the recovery key's recipient with an HMAC of the bootstrap key and a time
  (`bootstrapMaxAge`); the state keeps the recipient with the first user (`add-user`). It
  works once: when the cluster has a user, users add the others. The CLI then writes the
  credentials (`clusterdir.WriteCredentials`), checks that the cluster's recovery
  recipient is the key it made, and writes `cluster.yaml` with the bootstrap key encrypted
  (`clusterdir.Create`).
- **A code from a host** gives access back (`internal/recoverycode`, `api/recover.go`): root
  on a host runs `clowderd user recovery-code <user>`, which reads only the data key, so it
  works while the agent is down. A code is the first 80 bits of an HMAC over its use
  (`recoverycode.ForRecover`, `ForRotate`), the user and the quarter of an hour, with a key
  derived from the data key (`recoverycode.Purpose`), in Crockford's base32: case,
  `O`/`0`, `I`/`L`/`1` and hyphens do not matter. Every agent checks it alone, against the
  windows of the last 15 minutes and, in the last minute of one, the next (a host's clock a
  little ahead).
- **A code opens once.** A code seen on a console or in a log would otherwise let whoever
  saw it recover after the user, revoke the user's new certificate and take over. So
  `recover-user` and `set-recovery-recipient` carry the code's digest
  (`recoverycode.Code.Digest`, never the code: the log and the audit show the commands)
  and when it expires, the end of the window after its own. The handler refuses a digest
  that `State.UsedCodes` holds and records it otherwise. `useCode` drops the codes that
  expired more than `usedCodeKept` before the command, by the command's `At`, as `revoke`
  does. The agent looks in its own state first, so a used code gets the same 401 as a
  wrong one; through a follower that has not seen the use yet, the leader's state refuses
  it (409). Each use has its own code, so a user can recover and rotate the recovery key
  in the same quarter of an hour, and a code seen while rotating gives no recovery. A
  second recovery in the same window needs the next window's code.
- **`user recover <user> --code`** (`cli/recover.go`) makes a key pair, then asks
  `GET /v1/recover/offer?user=` for the CA with a `pki.TrustProof` made with each code the
  agent takes, and trusts the CA only if one matches the code typed in
  (`api.FindClusterByCode`). `POST /v1/recover`, over TLS checked against that CA, brings
  the user, the public key and the code; the agent checks the code, issues the certificate
  and commits `recover-user`, which revokes every other certificate of the user: a stolen
  laptop is locked out. As with the bootstrap, the agent sets the actor to the recovered
  user (`user/<name>`), so the audit shows a recovery as `recover-user` by that user. A
  user that does not exist gets the same offer and the same refusal as a wrong code, so the
  routes tell nobody which users there are. Name and seeds come from the cluster directory,
  or from `--name` and `--seed` without one.
- **Both routes come without a certificate,** so each agent takes at most `triesPerSender`
  tries a minute per sender and `triesInAll` in all, in memory (`api/limit.go`); then 429.
  A sender is its IPv4 address, or for IPv6 its /64.
- **`recovery rotate --code`** needs the user's credentials and their own code to rotate: stolen
  credentials alone cannot swap the recovery key. The CLI makes the new key and hands it
  over as the bootstrap does, then `POST /v1/recovery/rotate` commits
  `set-recovery-recipient`. Values encrypted afterwards are for the new key; `secret rekey`
  brings the files to it. Old ciphertext, in the Git history say, stays readable with the
  old key.
- The tests run real HTTPS servers: a follower forwarding to the leader, refused callers, the
  CLI against an agent with a cluster directory and credentials in a temporary config
  directory.

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

  An example. A host offers 1 GiB and runs `blog`: one service at priority 500 that
  reserves 600 MiB. A new stack `shop` comes: `api` at 800 and `cache` at 200, each
  reserving 300 MiB.
  1. `api` goes first, as it has the higher priority. It takes 300 of the 400 MiB free.
  2. `cache` needs 300 MiB, and 100 are left. It may push out only services below its own
     200; `blog` is at 500. So `cache` starts pushed out, and `blog` keeps running.

  A service is only as strong as its own priority: `api` at 800 does not let `cache`
  push out `blog`. The new stack's list of pushed-out services is written afresh at every
  placement, so one left by an earlier leader is replaced.
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
  copy of the state (`SharedCopy`), outside the lock Raft's `Apply` waits for. It first
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
