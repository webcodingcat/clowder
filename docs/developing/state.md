# The cluster state (`internal/state`)

Part of [DEVELOPING.md](../../DEVELOPING.md). Every agent holds the same `State` and
applies the same `Command`s in Raft's order. This file explains the rules of the state and
what each part of it means. [Raft](cluster.md#raft-internalraftnode-and-the-data-key-internaldatakey)
explains how the commands get there.

## Rules

- **Deterministic.** `Apply` uses no clock, no randomness and no I/O. A command carries
  the leader's time in `At`. Lint bans the clock from the package (forbidigo).
- **A refused command leaves no trace.** A handler only reads the state. It checks the
  command and returns the change as a function. `Apply` runs that function only when the
  handler returned no error, and the function itself cannot fail. So no command is refused
  halfway through, and no command needs a copy of the state. The one exception is
  `change-resources` ([Resources](#resources)). The state's `Index` follows the log in
  both cases.
- **A spec or an encrypted value is replaced whole, never changed in place.** Every clone
  of the state shares them ([Copies of the state](#copies-of-the-state)). A handler that
  wrote into one would change the loops' shared copy under them, and a refused batch would
  leave the write behind.
- **The tests check what the types cannot.** Every command in the state's tests is
  compared with a clone taken before it, so a check that changes something, or a write
  into a shared spec, fails the test.

## Commands

A command has a kind, a payload of the type with the same name (`Bootstrap`,
`ChangeResources`, ...) and an actor: `leader`, `host/<id>` or `cli/<name>`. The API takes
the actor from the caller's certificate and decides which actor may send which kind
([Who may call what](api.md#who-may-call-what)). The CLI's name is for the audit only
(`IsCLIActor` accepts any name). The state checks only what it alone can tell: a host
reports only about itself and never moves another host, and it reports about a stack only
while the stack runs on it.

`bootstrap` (`Bootstrap`, `ErrNotBootstrapped`) is the first command, which `clowderd init`
writes. The word comes from Raft; users have no bootstrap.

| Who | Commands |
|---|---|
| `clowderd init` | `bootstrap` (the keys, the cluster's name, the first host) |
| `clowderd recover` | `recover-remap` |
| the CLI | `set-host-phase` (host actions), `add-join-token`, `change-resources` (`/v1/apply`, `/v1/delete`; the leader decides), `restart-stack`, `request-move` |
| root on a host | `set-cli-cutoff` (`clowderd credentials --revoke-older`, as that host) |
| the leader, for a joining host | `add-host` |
| the members loop | `set-host-phase` (joining to active; removed for a host that left), `set-host-voter` |
| the placement | `put-volume`, `set-stack-status`, `set-stack-wait-reason`, `set-pushed-out`, `start-operation` (a move) |
| the moves loop | `put-volume`, `set-stack-status`, `advance-operation`, `extend-operation`, `roll-back-operation`, `finish-operation` |
| the Floating IP's loop | `confirm-server`, `set-ingress` |
| the ACME loop | `put-acme-account`, `put-acme-challenge`, `delete-acme-challenge`, `put-certificate`, `delete-certificate` |
| the fencing loop | `set-host-phase` (failed, active again; fenced or back to failed for its operation; disabled), `start-operation`, `advance-operation`, `finish-operation` (a fence) |
| the alerts | `put-alert`, `resolve-alert` |
| each agent | `report-host`, `forget-copy`, `set-volume-initialized`, `set-copy-synced`, `report-peer-forgotten`, `advance-operation` (a move's step on the host), `set-host-phase` (its own: recovering, active) |

Every kind has a sender. A kind is added together with the code that sends it, not before.

`Apply` returns the resources a command changed (`[]Change`, with the command's index as
their `ResourceVersion`). A handler records each one with `changed` or `deleted`.

## Resources

The resources are the configuration (`Config`), the namespaces (`Namespaces`), and, keyed
by `<namespace>/<name>`, the stacks (`Stacks`), volumes (`Volumes`) and secrets
(`Secrets`). Each has a `Meta`. The kinds and their specs are in `internal/resource`; a
stack's spec is in `internal/stackspec` and the configuration's in `internal/clusterspec`.
The CLI and the API share these packages. [What users write](../resources.md) describes
the fields.

Three counters, each with one job:

| Counter | Counts |
|---|---|
| `Meta.Generation` | the changes of a resource's spec |
| `Meta.ResourceVersion` | the index of the command that last changed the resource, spec or status |
| `Volume.DataGeneration` | a volume's data: each new, empty LV (`-g<N>`) gets the next number |

A volume gets a new data generation when a volume of replicas 0 moves, fails over, or
starts anew after a recover.

### One command for every user's change

`change-resources` (`ChangeResources`) carries a list of changes. Each is a put
(`PutResource`) or a delete (`DeleteResource`) of one resource.

- `applyChangeResources` applies the changes in order to a clone of the state. Each change
  is checked against the state with the changes before it. Only if all pass does the clone
  replace the state. So a Secret can come before the configuration that names it, or a
  namespace before its stacks, in the same command. This handler works on a clone because
  each check depends on the changes before it.
- The first refusal refuses the whole command, as a `ChangeError` with the change's index,
  so the API can name the refused resource.
- `resourceKinds` holds each kind's `find`, `put` and `remove`. `put` and `remove` return
  their change, as a handler does. A put (`applyPutResource`) checks, in this order: the
  owner (a resource of another channel is refused, even when the spec would not change),
  then whether anything changes at all, then everything else.

**Unchanged is not a change.** When a spec equals the stored one, nothing changes: not
`Generation`, not `ResourceVersion`, and no audit entry is written. So a GitOps loop that
applies the same files restarts nothing and keeps the audit short. A secret is compared by
its digest, because the same value seals differently every time. A stack is compared by
its normalized spec. The status commands (`put-volume`, `set-stack-status`,
`set-stack-wait-reason`, `set-pushed-out`) work the same way: their handler returns
`errUnchanged`, and `Apply` treats the command as applied without a trace.

### The configuration

`Config` is a `ClusterConfig`: its `Meta`, and the `clusterspec.Config` with the defaults
filled in. It is the single `ClusterConfiguration`, in no namespace, named `cluster`
(`resource.ClusterConfigurationName`). Until the first one is applied, its generation is 0
and its spec is empty (`Configured`). It can be changed but never deleted
(`removeConfig`), because fencing and the Floating IP would stop without a word.

The state itself checks resources against the configuration, in the log's order: a
stack's priority categories and privileges, a volume's storage class, and a host's
underlay. `putConfig` refuses a configuration that an existing resource would no longer
pass, or that names a value missing from `clowder-system`. No check happens outside the
state, where a later command could make it stale, so no command carries a generation of
the configuration.

### Namespaces

- A namespace's owner owns only the namespace; any channel may put resources into it.
- A namespace can be deleted only when it is empty; nothing cascades. `default` belongs to
  no channel and cannot be deleted.
- Its spec names its image pull secrets (`imagePullSecrets`). They are not checked at
  apply, because the Secrets live in the namespace and so come after it.
- **`clowder-system`** (`names.System`) belongs to the cluster. Bootstrap creates it
  together with `default`. It belongs to no channel and cannot be deleted. It holds only
  the Secrets that the configuration names: no stacks, no volumes and no image pull
  secrets (a put of it with an empty spec is unchanged). Its name starts with
  `names.ReservedPrefix`, which no user's namespace may start with (`names.ValidNamespace`).

### Volumes

- A volume is created `pending`, without a copy and without a DRBD minor. The placement
  places it with the first stack that names it. Its class must be in the configuration's
  `storageClasses`, and its spec cannot change yet.
- A new volume starts one `DataGeneration` above the retired copies of a deleted volume
  with the same ID. The state refuses an LV name that a volume or a retired copy already
  has, because the name holds only 32 bits of a hash.
- `put-volume` changes a volume's copies and status, never its `Meta` or spec.
- A volume serves one stack. A volume that a stack names cannot be deleted. A deleted
  stack leaves its volumes, with their data, for the next stack that names them.

### Secrets

- A Secret's values arrive encrypted with the data key (`Encrypted`), so the state never
  sees them in clear. They are stored by key (`EncryptedSecret.Data`, keys as
  `names.ValidSecretKey` allows). The API computes one digest over all of them and one per
  key (`KeyDigests`), so a diff can tell which value changed.
- The limits count the values in clear (`resource.MaxSecretBytes` per secret, per
  namespace and in the cluster), and the cluster holds at most `resource.MaxClusterSecrets`.
  The state derives the clear size from the encrypted length (`ClearSize`: age adds a
  header of fixed length and a tag per 64 KiB); a test in `datakey` checks this against
  real encryption. `checkSecretLimits` checks all limits, and `putSecret` calls it. A stack
  names at most `MaxStackSecrets`.
- A secret that a stack or the configuration names cannot be deleted. A put that drops a
  key that a stack's `secretEnv` or the configuration names is refused (`checkKeysKept`),
  because whatever names the key would fail without it.
- The cluster's sealing key is in the state too, encrypted with the data key
  (`ClusterKeys.SealingKey`).
- The cluster's own secrets (the Hetzner token, the alert channels' URLs and passwords,
  the heartbeat's URL) are Secrets of `clowder-system`. The configuration names them by
  `{name, key}` (`names.SecretKeyRef`); `clusterspec.SecretPlaces` lists the fields that
  do. Readers find a value through such a reference or a spec, never by taking a name
  apart: `SystemSecret(ref)`, `HetznerToken`, and `RegistryLogins(namespace)` (each
  image pull secret with the keys `server`, `username` and `password`). Until the
  first configuration, the loops that need the Hetzner token or a Floating IP get
  `ErrNotConfigured` and wait.
- Who can read a secret is in [Secret values](api.md#secret-values). A host without the
  data key may read only `SecretsOfHost`: the secrets that the stacks it runs
  (`StacksToRun`) name, and their namespaces' image pull secrets.

### Stacks

**The spec** is stored normalized (`stackspec.Normalize`). The state reads it itself, so
nothing derived travels with the command. `putStack`:

- runs `stackspec.Check` and resolves the priorities with the configuration's categories;
- checks privileges against `stacks.privileged`;
- refuses a published port or HTTP route that another stack has, volumes or secrets that
  its namespace lacks, and a key of `secretEnv` that its Secret lacks;
- keeps what the scheduler needs of each service (`Stack.Services`: the priority as
  written, the reservations, the dependencies).

**The status.** A new stack is `pending`. A changed stack that runs or fails is `starting`
until its host reports the new generation up.

- A host's `set-stack-status` names the generation and the restart times it ran with. A
  report about an older generation or other restart times is refused, so a report from
  before an apply or a restart cannot mark the stack `running`.
- With `running`, the host reports the digests of the secrets its containers came up with
  (`AppliedSecrets`).
- Only the host reports `failing`, always with the reason (`UpError`, for display only).
  The digests stay, because containers of an earlier `compose up` may still run with them.
  A failing stack keeps its host and its room.

**`restart-stack`** records the time of a restart per service (`Stack.Restarted`). It
covers the services named (none means all) and every service that depends on one of them
(`stackspec.Spec.WithDependents`), because a dependency is only enforced when a container
starts. It changes no spec, so the generation stays; a running stack becomes `starting`
again. Any channel may restart any stack, so an operator can restart a stack that GitOps
applies.

**`set-stack-wait-reason`** records why the placement finds no place for a stack or for a
volume it names (`WaitReason`). It changes nothing else, so it never unplaces a stack that
runs. The state is the placement's memory: the placement writes a reason once, also after
a change of leader, and clears it when the stack no longer waits.

### Planned on a state

The leader plans on a copy of the state and commits afterwards, and a user's apply may
come in between. So its `set-stack-status`, `set-pushed-out` and `put-volume` name the
index of the state they were planned with (`PlannedAt`). The handler refuses a change to a
stack or volume whose `ResourceVersion` is higher (`checkPlanned`, `ErrStale`), because it
changed since, or was deleted and created anew under the same name. The placement then
plans again.

A host's reports name no plan. They are checked against the stack's generation, restarts
and host instead. The leader places only a stack that has no host yet; moving a placed
stack is the job of an operation that holds its lock.

## Operations and locks

An operation is a long task in steps, such as moving a stack or fencing a host.

- The leader starts it (`start-operation`) and takes its locks (`stack/<id>`,
  `volume/<id>`, `host/<id>`), each on something that exists in the state. If another
  operation holds one of them, the start fails. The operation's ID is the index of the
  start.
- Whoever owns the current step, a host or the leader, records it as done
  (`advance-operation`) and names the next owner. The command names the step it saw last
  (`FromStep`), so a step reported twice, after a slow host or a change of leader, is
  refused.
- The leader finishes the operation (`finish-operation`), which frees the locks. The state
  keeps the last `keptFinished` finished operations.
- While an operation holds a lock, every handler that changes a stack, a volume or a
  host's phase refuses the change (`checkUnlocked`), unless the leader makes it for that
  operation (`Operation` in `set-stack-status`, `put-volume` and `set-host-phase`). A
  host's reports ignore locks: `set-stack-status` from the host says what runs where, which
  the operation waits for, and `set-volume-initialized` says what a copy holds.
  `TestEveryKindOfCommandIsCheckedForLocks` holds every kind to this rule, so a new kind
  needs a case or a reason.
- Each step has a deadline (`Operation.Deadline`, `StepTime` unless the sender gives one).
  The leader gives more time to a step that makes progress (`extend-operation`), and turns
  one back before the kind's point of no return (`roll-back-operation`). The operation is
  then `rolling-back` until the leader has undone its work and finishes it as failed.

### Fencing a host

`fence-host` (`HostFence`) is started and driven by the fencing loop
([Fencing](loops.md#fencing-controllerhealthgo-controllerfencinggo-agenthealthgo)).

- It locks the host, and its start makes the host `fencing` in the same command.
- Its steps are `FenceStart`, then `FenceOff` once the server is off and the host is
  fenced.
- It has no rollback: if the server cannot be powered off, the operation moves the host
  back to failed and fails.
- The leader finishes it once the stacks the host had (`HostFence.Stacks`, recorded at the
  fence) run elsewhere, or after `StepTime`.
- Its lock limits fencing to one host at a time.

### Moving a stack

`move-stack` (`StackMove`) is started by the placement and driven by
`controller.RunMoves`. It locks the stack and its volumes. Each step is named after the
last thing done:

1. `MoveCopies`: the leader placed a copy of each volume on the new host.
2. `MoveSynced`: the new host has the copies synced (`VolumeCopy.Synced`) and the images
   pulled.
3. `MoveStopped`: the old host stopped the stack, and its copies are secondary.
4. `MoveHost`: the leader set the stack's host. A volume of replicas 0 gets a new, empty
   data generation there.

The leader finishes the move once the stack runs on the new host.

- From `MoveSynced` on, `StopsForMove` takes the stack off its old host's runner and
  reconciler. The new host's reconciler promotes once the stack is placed there.
  `MoveStopped` is the point of no return.
- The deadline of `MoveCopies` allows twice the time of a resync at
  `replication.resyncRate`. After it, the leader asks the new host how much is left
  (`/v1/internal/volumes/progress`) and extends the deadline while that shrinks.
- A move that ends answers a user's `request-move` (`Stack.MoveTo`), whether it worked or
  not. A move that worked records `Stack.LastMove`, one that failed `Stack.MoveFailed`.
  Within `MoveCooldown` the placement then starts no move of its own for that stack (a
  user's request still goes ahead), so a move that cannot work does not resync copies
  over and over.
- If a host of the move goes down (no longer active or draining), the move ends at once
  (`abandon`). Before `MoveStopped` it rolls back, after it the move fails; either way the
  stack keeps its old host. From `MoveHost` on, a new host that goes down ends the move as
  failed, and the failover takes the stack.
- A running stack that cannot move gets the alert `stack-waiting` with "cannot move".

To follow a move through the code: `scheduler.place` decides it, `controller.startMove`
starts it, `controller.mover.placeCopies` (with `scheduler.MoveVolumes`) runs the first
step, `agent.moveSteps.synced` and `stoppedHere` record the hosts' steps,
`controller.mover.setHost` records the leader's last step, and `controller.mover.late`
watches the deadlines.

## Volume copies

**Retired copies.** A copy's data is removed only when the state says so. A delete of a
volume, and a `put-volume` that changes the data generation or takes a copy off a host,
add the copies they give up to `Retired` (volume, data generation, host). The host removes
those copies and forgets each with `forget-copy`. The copies of a removed host go with the
host. A copy that is merely missing from the state stays, because a bug in the placement
must not cost data. A retired copy keeps its volume's DRBD minor taken (`RetiredMinors`)
until every host has forgotten its copies: the resource on the host still has the minor,
and drbdadm refuses every command while two resources share one.

**Initialized volumes.** `Volume.Initialized` says that a copy of the current data
generation was primary with its file system. The first host whose copy reaches that state
reports it with `set-volume-initialized`; only a host with a copy with a disk in that
generation may. Until then, a host may promote with `--force` and format. After that,
never, on any host: a copy without a file system then lacks the data, it is not new.
`put-volume` keeps the flag within a data generation and clears it with a new one.

## Host phases

`phaseChanges` holds the allowed changes. The state also allows the leader the user's
changes (`byLeaderOrCLI`); the API lets the CLI make only these. Each change names the
phase it starts from (`From`), so of two changes that race, only the first passes.

| From | To | Who | When |
|---|---|---|---|
| `joining` | `active` | members loop | the host reported |
| `active` | `draining` and back | user | `host drain`, `host undrain` |
| `active`, `draining`, `recovering` | `failed` | fencing loop | a majority of the voters has not heard it for `FailAfter` |
| `failed` | `active` | fencing loop | a majority heard it again within `backWithin`; never the host itself |
| `failed` | `fencing` | the operation's start | `start-operation` of a fence, in the same command |
| `fencing` | `fenced` | the operation | its server is off |
| `fencing` | `failed` | the operation | `PowerOff` failed `powerOffTries` times; the operation fails |
| `failed` | `fenced` | user | `host fence-confirm`, after the user checked that the server is off |
| `fenced` | `recovering` | the host | its agent runs again and has caught up with the leader |
| `recovering` | `active` | the host | every copy of it with a disk is synced (`checkCaughtUp`) |
| `fenced` | `disabled` | fencing loop | fenced `FlapLimit` times within `FlapWindow` (`Host.Flapping`) |
| `disabled` | `fenced` | user | `host enable`: clears `FencedAt`; the leader powers it on |
| any | `removed` | `clowderd recover` | `recover-remap`: every host except the one that recovers |

`failed` to `fencing` happens only for a failed host with a checked server and a stack
that another host can take (`needsFence`). At `fencing` to `fenced`, the time is added to
`FencedAt`, the host's copies are no longer synced, and the operation records the host's
stacks.

The reasons behind the table:

- A drain moves the host's stacks off it and keeps new ones off. Its volume copies stay,
  because the host comes back after the maintenance.
- A host can never leave `failed` by itself. It cannot tell whether the leader is about to
  fence it, and it would keep writing while a peer promotes its volume (split brain).
- Once a fence has started, nothing stops it, not even a late beat. `fencing` has no
  change back to `active`; only its operation moves it on, and `checkFenceDecided` keeps
  the operation from ending while the host is still `fencing`.
- While an operation holds `host/<id>` (fencing, drain, remove), only that operation
  changes the host's phase, and the host's own phase reports are refused too.
- A failed host's copies are no longer synced (`unsync`), because the other copies went on
  without it. No stack fails over or moves to it until DRBD has caught its copies up and
  the host reports that.

What each phase means elsewhere (Y: yes; the named functions decide). Only a worker has
stacks and takes them, and only a control-plane host votes:

| Phase | runs its stacks (`state.KeepsStacks`) | its stacks fail over (`state.FailsOver`) | takes new stacks (`scheduler.takesStacks`) | its copies count (`scheduler.counts`) | votes (`controller.votes`) |
|---|---|---|---|---|---|
| `joining` | | | | | |
| `active` | Y | | Y, unless leaving | Y, unless leaving | Y |
| `draining` | Y | | | Y, unless leaving | Y |
| `failed`, `fencing` | Y | | | Y | Y |
| `fenced` | | Y | | for `CopyWait` | Y |
| `recovering` | | | | Y | Y |
| `disabled` | | Y | | | Y |
| `removed` | | | | | |

A failed or fencing host keeps running its stacks, because no other host runs them before
it is off.

### Removing a host

`checkRemoval` (the CLI asks first) refuses to remove:

- the last control-plane host in service (`active` or `draining`, `InService`);
- a host in service while a stack or a volume copy is on it. The host leaves instead
  (`SetHostPhase.Leaving`, `ErrNotEmpty`): it drains, the placement moves its copies off
  too (they no longer count), and the members loop removes it once nothing is left. An
  undrain takes it back.

Once `MinHAHosts` control-plane hosts have been in service, the state is `HA` for good,
and no removal may leave fewer than three of them; a replacement joins first
(`checkMayGo`). A host without the control plane may always go once it is empty.

A removed host's certificate is revoked. Every other host forgets it as a DRBD peer and
reports that (`report-peer-forgotten`); `Forgetting` holds the hosts that have not done so
yet. Only then is its ID free for a new host (`HostIDFree`), because DRBD's node-id is the
host ID and a peer's slot in the metadata outlives the host. A host that is down reports
nothing, so the ID waits for it; once that host is removed too, nobody waits for it. A
removed control-plane host keeps the data key ([Known limits](../operating.md#known-limits)).

## Hosts, certificates and tokens

- **Roles** (`internal/hostrole`, `Host.Roles`) are fixed once a host has joined. The first
  host needs `control-plane` (`bootstrap`). The state refuses a stack placed on, or moved
  to, a host without `worker`, and a copy with a disk there (`checkWorker`); a tiebreaker
  may go on any host. Only an ingress host holds the Floating IP (`set-ingress`).
- **Revocations.** `Revoked` holds each revoked host certificate with its expiry time (the
  host's `CertificateNotAfter`). Removing a host adds it (`revoke`). The list grows only
  there, so `revoke` also drops the certificates that expired more than `revokedKept`
  before the command: no handshake accepts them any more, and the margin covers a host
  whose clock runs behind. It goes by the command's `At`, so every agent drops the same
  entries.
- **The CLI's certificates** are not listed, as kubeadm lists none: whoever holds one
  controls the cluster. `CLICutoff` (`set-cli-cutoff`, from `clowderd credentials
  --revoke-older`) refuses every CLI certificate that starts before it. It only moves
  forward, so a revoked laptop never gets back in. A host sets it, because root on that
  host issued the new certificate.
- **The cluster's name** (`ClusterName`) comes from `clowderd init --cluster-name` and must
  pass `names.Valid`. `cluster status`, the alerts and new credentials carry it; nothing
  is checked against it.
- **Join tokens** (`internal/jointoken`) are an ID and a 256-bit secret. `add-join-token`
  keeps only the secret's hash under the ID, with its expiry time (the command's `At` plus
  its TTL, `MinJoinTokenTTL` to `MaxJoinTokenTTL`, at most `MaxJoinTokens` open) and the
  roles a host may take with it (`JoinToken.Roles`). `add-host` checks the token against
  its own `At` (`CheckJoinToken`), checks the host's roles against the token's
  (`CheckRoles`), and deletes the token in the same command, so a token works only once.
  `dropExpiredTokens` drops expired tokens a while later (`joinTokenKept`), so a late join
  learns that its token expired.

## Versions

Every agent must apply every entry of the log, or the states drift apart for good. If an
agent skipped a `revoke`, for example, a certificate would keep working on that agent.

- **Fail-stop.** An entry that the agent cannot read (not JSON, a `CommandVersion` it does
  not read, a kind it does not know) is not a refusal. `Apply` leaves the state and its
  index as they are and returns `state.ErrUnreadable`; `ReadSnapshot` does the same for a
  snapshot. The fsm halts (`raftnode.Node.Halted`) and applies, restores and snapshots
  nothing more. The agent ends with exit status 3, which systemd does not restart
  ([Shipping](shipping.md#packages-and-units)), and the other hosts see it fail. A refusal
  (`CommandError`, `ErrNotBootstrapped`) is the same on every agent of a version, so it
  moves the index on.
- **The gate.** Each kind names the release that introduced it (`commandKinds`).
  `Node.Propose` commits a kind only once `State.MinVersion`, the oldest release that an
  agent reported, includes that release (`buildinfo.Includes`: `0.2.0~rc1` includes
  `0.2.0`). Otherwise the command fails with `state.ErrNotReadByAll`, which is not a
  refusal, and its caller tries again later. Development builds read every kind of their
  code and do not count, and neither do hosts that have not reported a version yet. Fail-stop
  catches whatever slips through. Before `v1.0.0` the gate rarely acts, because a patch
  release adds no kind and different minor versions do not run together; it is there for
  the rules from `v1.0.0` on ([roadmap](../roadmap.md), step 8, decides whether it starts
  anew with phase 2b's release).
- **Until `v1.0.0`** the state, the commands and the specs change freely, without
  migrations. Only agents and CLIs of the same minor version work together
  (`buildinfo.Compatible`), and a patch release changes neither the state nor the
  commands. `CommandVersion` and `SnapshotVersion` stay at 1, so fail-stop and the gate
  catch a new kind, but not a payload whose fields changed their meaning. That is why
  different minor versions must not run side by side. A cluster is updated host by host
  only within a minor version ([Updating a cluster](../operating.md#updating-a-cluster)).
- **From `v1.0.0`** a release reads its own `CommandVersion` and `SnapshotVersion` and the
  previous one, and CLI and agents work together across one minor version. Payload fields
  are added, not changed. Unknown fields are ignored, so an agent of the previous release
  drops a new field from its state. A field that later commands depend on therefore needs
  its own gate on `MinVersion`.
- The json tags of `clusterspec.Config` are part of this format.

## Invariants

The state enforces these itself, as the last place to catch a wrong command:

- valid names (`internal/names`);
- volumes with 0 to 3 replicas, and a DRBD minor exactly when replicated (a pending volume
  has neither copy nor minor);
- one copy per host, one stack per volume;
- nothing deleted that a stack names;
- a removed host's certificate revoked;
- no name, underlay, server or certificate of a host twice among the hosts that are not
  removed (`CheckJoiningHost`);
- every host with a server ID, an IPv4 underlay and an IPv4 public address;
- each underlay in `privateNetwork` once a configuration names it;
- every joined host's server checked with Hetzner.

## Copies of the state

`Clone` (`clone.go`) returns a copy without the audit. It is for work that is too slow for
the state's lock (the placement weighs every stack), that keeps parts of the state (the
agent's stacks), or that changes its copy (`change-resources`, a dry run).

- It copies what the handlers change in place: the maps, the resources, their statuses.
- It shares what they only ever replace whole: the specs, a stack's `Services`, the
  encrypted values and the certificates' bytes. So a clone costs about as much as the
  statuses, not as much as the secrets.
- `clone_test.go` fails when `Clone` shares a field that it should copy, and
  `statetest.ReadOnlyCopy` catches a loop that writes into a shared spec.
- The loops make no clones of their own: `raftnode.Node.SharedCopy` makes one per change,
  which every loop reads and none changes.

Snapshots are gzip-compressed JSON (`WriteSnapshot`, `ReadSnapshot`). The audit keeps the
last `auditSize` commands.
