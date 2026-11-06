# The cluster

Part of [DEVELOPING.md](../../DEVELOPING.md): the replicated state, the certificates,
Raft, the API and the CLI, and the placement.

## Cluster state (`internal/state`)

Every agent holds the same `State` and applies the same `Command`s in Raft's order.

- **Deterministic.** `Apply` uses no clock (a command carries the leader's time in `At`), no
  randomness and no I/O. Lint bans the clock from the package (forbidigo).
- **A refused command leaves no trace.** A handler only reads the state. It checks the
  command and returns the change it makes as a function, and `Apply` runs that function
  only when the handler returned no error. The change itself cannot fail, so nothing can
  refuse a command halfway through. This costs nothing, whereas working on a copy of the
  state would copy all of it for every command (see `Copy`). The one handler that works on
  a copy is `change-resources` (below), because what each of its changes checks depends on
  the changes before it. The state's `Index` follows the log either way. The tests check
  what the types cannot, namely that the checks change nothing: every command that a test
  may see refused (`apply`, `changes`, `refuse`) is compared with the state before it.
- **Commands.** A command has a kind, a payload of the type with the same name
  (`Bootstrap`, `ChangeResources`, ...) and an actor (`leader`, `host/<id>`, `cli/<name>`).
  The API takes the actor from the caller's certificate (`IsCLIActor` accepts any name).
  The cluster has no users: the name is for the audit, and whoever holds a CLI certificate
  controls the cluster, as with kubeadm's `admin.conf`. The API checks which actor may
  send which kind. `bootstrap` (`Bootstrap`, `ErrNotBootstrapped`) is Raft's word for the
  first command, which `clowderd init` writes; users have no bootstrap. The state checks
  only what it alone can tell: a host reports only about itself and never moves another
  host, and it reports about a stack only while the stack runs on it, without moving it.
- **The commands in use so far,** by sender:

  | Who | Commands |
  |---|---|
  | `clowderd init` | `bootstrap` (the keys, the cluster's name, the first host) |
  | `clowderd recover` | `recover-remap` |
  | the CLI | `set-recovery-recipient`, `set-host-phase` (host actions), `add-join-token`, `change-resources` (`/v1/apply`, `/v1/delete`; the leader decides), `restart-stack`, `request-move` |
  | root on a host | `set-cli-cutoff` (`clowderd credentials --revoke-older`, as that host) |
  | the leader, for a joining host | `add-host` |
  | the members loop | `set-host-phase` (joining, passive, active; removed for a host that left), `set-host-voter` |
  | the placement, the stack's host | `put-volume`, `set-stack-status`, `set-stack-wait-reason`, `set-pushed-out`, `start-operation` (a move) |
  | the moves loop | `put-volume`, `set-stack-status`, `advance-operation`, `extend-operation`, `roll-back-operation`, `finish-operation` |
  | the Floating IP's loop | `confirm-server`, `set-ingress` |
  | the ACME loop | `put-acme-account`, `put-acme-challenge`, `delete-acme-challenge`, `put-certificate`, `delete-certificate` |
  | the fencing loop | `set-host-phase` (failed, active again; fenced or back to failed for its operation; disabled), `start-operation`, `advance-operation`, `finish-operation` (a fence) |
  | the alerts | `put-alert`, `resolve-alert` |
  | each agent | `report-host`, `forget-copy`, `set-volume-initialized`, `set-copy-synced`, `report-peer-forgotten`, `advance-operation` (a move's step on the host), `set-host-phase` (its own: recovering, active) |

  Every kind has a sender. A kind is added with the phase that sends it (backups later),
  not before.
- **Resources** (phase 1c) are the configuration (`Config`, phase 2b), namespaces
  (`Namespaces`), and, keyed by `<namespace>/<name>`, stacks (`Stacks`), volumes
  (`Volumes`) and secrets (`Secrets`), each with a `Meta`. The kinds and their specs are in
  `internal/resource` (a stack's in `internal/stackspec`, the configuration's in
  `internal/clusterspec`), which the CLI and the API share. There are three counters, each
  with one job:

  | Counter | Counts |
  |---|---|
  | `Meta.Generation` | the changes of a resource's spec |
  | `Meta.ResourceVersion` | the index of the command that last changed the resource, spec or status |
  | `Volume.DataGeneration` | a volume's data: each new, empty LV (`-g<N>`) gets the next number |

  A volume gets a new data generation when a volume of replicas 0 moves, fails over or
  starts anew after a recover. Restores (phase 3) will raise it too.

  - **One command for every user's change.** `change-resources` (`ChangeResources`)
    carries a list of changes. Each is one put (`PutResource`) or one delete
    (`DeleteResource`) with the resource's kind. `applyChangeResources` applies them in
    order to a copy of the state, and checks each against the state with the changes before
    it already applied. So a Secret placed before the configuration that names it, or a
    namespace placed before its stacks, goes in together with them. The first refusal
    refuses the whole command, as a `ChangeError` with the change's index, so the API can
    name the resource that was refused. A change that changes nothing counts as no change;
    a command made only of such changes leaves the state unchanged.
  - `resourceKinds` holds each kind's `find`, `put` and `remove`; `put` and `remove`
    return their change, as a handler does. A put (`applyPutResource`) checks in a fixed
    order:
    1. The owner: a resource of another channel is refused, even when the spec would not
       change.
    2. Whether anything changes at all.
    3. Everything else.
  - **Unchanged is not a change.** When a spec equals the stored one, nothing changes:
    neither `Generation` nor `ResourceVersion`, and no audit entry is written. So a GitOps
    loop that applies the same files restarts nothing and keeps the audit short. A secret
    is compared by its digest, because the same value seals differently every time; a
    stack is compared by its normalized spec. The status commands (`put-volume`,
    `set-stack-status`, `set-stack-wait-reason`, `set-pushed-out`) work the same way. Such
    a handler returns `errUnchanged`, and `Apply` treats the command as applied without a
    trace.
  - **The configuration** (`Config`, a `ClusterConfig`: its `Meta` and the
    `clusterspec.Config` with the defaults filled in) is the single `ClusterConfiguration`.
    It is in no namespace and is named `cluster` (`resource.ClusterConfigurationName`).
    Until the first one is applied, its generation is 0 and its spec is empty
    (`Configured`). It can be changed but never deleted (`removeConfig`), because fencing
    and the Floating IP would silently stop.
  - **The state checks against the configuration itself**, in the log's order: a stack's
    priority categories and privileges, a volume's storage class, and a host's underlay.
    `putConfig` refuses a configuration that an existing resource of the cluster would no
    longer pass, or that names a value that `clowder-system` lacks. No check happens
    outside the state, where a later command could make it stale, so no generation of the
    configuration travels with a command.
  - `Apply` returns the resources a command changed (`[]Change`, with the command's index
    as their `ResourceVersion`); a handler records each one with `changed` or `deleted`.
  - A namespace's owner owns only the namespace; any channel may put resources into it.
    `default` belongs to no channel and cannot be deleted. A namespace can be deleted only
    when it holds nothing; nothing cascades. Its spec names its image pull secrets
    (`imagePullSecrets`). These are not checked at apply, because the Secrets live in the
    namespace and so come after it.
  - **`clowder-system`** (`names.System`) belongs to the cluster. Bootstrap creates it
    together with `default`. It belongs to no channel and cannot be deleted. It holds only
    the Secrets that the configuration names: no stacks, no volumes and no image pull
    secrets (a put of it with an empty spec is unchanged). Its name starts with
    `names.ReservedPrefix`, which no user's namespace may start with
    (`names.ValidNamespace`).
  - A volume is created `pending`, without a copy and without a DRBD minor; the placement
    places it with the first stack that names it. Its class must be in the configuration's
    `storageClasses`. Its spec cannot change yet. A new volume starts one `DataGeneration`
    above the retired copies of a deleted volume with the same ID, and the state refuses an
    LV name that a volume or a retired copy already has (the name holds only 32 bits of a
    hash). `put-volume` changes a volume's copies and status, never its `Meta` or spec. A
    volume serves one stack. A volume that a stack names cannot be deleted, and a deleted
    stack leaves its volumes, with their data, for the next stack that names them.
  - A secret's values are sealed, one per key (`SealedSecret.Data`, with keys as
    `names.ValidSecretKey` allows). The API computes one digest over all of them and one
    per key (`KeyDigests`), so a diff can tell which value changed. The limits apply to the
    values in clear (`resource.MaxSecretBytes` per secret, per namespace and in the
    cluster). The state derives the clear size from the sealed length (`ClearSize`: age adds
    a header of fixed length and a tag per 64 KiB), and a test in `datakey` checks that
    against real sealing. The cluster also holds at most `resource.MaxClusterSecrets`. One
    function checks all the limits (`checkSecretLimits`), and `putSecret` calls it. A
    secret that a stack or the configuration names cannot be deleted. A put that drops a
    key that a stack's `secretEnv` or the configuration names is refused (`checkKeysKept`),
    because whatever names the key would fail without it.
  - **A stack's spec** is stored normalized (`stackspec.Normalize`). The state reads it
    itself, so nothing derived travels with the command. `putStack`:
    - runs `stackspec.Check` and resolves the priorities with the configuration's
      categories;
    - checks privileges against `stacks.privileged`;
    - refuses a published port or HTTP route that another stack has, volumes or secrets
      that its namespace lacks, and a key of `secretEnv` that its Secret lacks;
    - keeps what the scheduler needs of each service (`Stack.Services`: the priority as
      written, the reservations, the dependencies).
  - **A stack's status.** A new stack is `pending`. A changed stack that runs or fails is
    `starting` until its host reports the new generation up. A host's `set-stack-status`
    names the generation and the restart times it ran with. A report about an older
    generation or other restart times is refused, so a report from before an apply or a
    restart cannot mark the stack `running`. With `running`, the host reports the digests of
    the secrets its containers came up with (`AppliedSecrets`). Only the host reports
    `failing`, always with the reason (`UpError`, for display only). The digests stay,
    because containers of an earlier `compose up` may still run with them. A failing stack
    keeps its host and its room.
  - **`restart-stack`** records the time of a restart per service (`Stack.Restarted`). It
    covers the services named (none means all) and every service that depends on one of
    them (`stackspec.Spec.WithDependents`), because a dependency is only enforced when a
    container starts. It changes no spec, so the generation stays the same; a running stack
    becomes `starting` again. Any channel may restart a stack, whoever owns it, so an
    operator can restart a stack that GitOps applies.
  - **`set-stack-wait-reason`** records why the placement finds no place for a stack or for
    a volume it names (`WaitReason`). It changes nothing else, so it never unplaces a stack
    that runs. The state is the placement's memory of the reason: the placement writes a
    change once, also after a change of leader, and clears it when the stack no longer
    waits.
  - **Planned on a state.** The leader plans on a copy of the state and commits afterwards,
    and a user's apply may come in between. So its `set-stack-status`, `set-pushed-out` and
    `put-volume` name the index of the state they were planned with (`PlannedAt`). The
    handler refuses a change to a stack or volume whose `ResourceVersion` is higher
    (`checkPlanned`, `ErrStale`), because it was changed since, or deleted and created anew
    under the same name. The placement then plans again. A host's reports name no plan;
    instead they are checked against the stack's generation, restarts and host. The leader
    places only a stack that has no host yet. Moving a placed stack is the job of an
    operation that holds its lock.
- **Operations and locks.** An operation is a long task in steps, such as moving a stack or
  fencing a host.
  - The leader starts it (`start-operation`) and takes its locks (`stack/<id>`,
    `volume/<id>`, `host/<id>`), each on something that exists in the state. If another
    operation holds one of them, the start fails. The operation's ID is the index of the
    start.
  - Whoever owns the current step, a host or the leader, records it as done
    (`advance-operation`) and names the next owner. The command names the step it saw last
    (`FromStep`), so a step reported twice, after a slow host or a change of leader, is
    refused.
  - The leader finishes the operation (`finish-operation`), which frees the locks. The
    state keeps the last `keptFinished` finished operations.
  - While an operation holds a lock, every handler that changes a stack, a volume or a
    host's phase refuses the change (`checkUnlocked`), unless the leader makes it for that
    operation (`Operation` in `set-stack-status`, `put-volume` and `set-host-phase`). A
    host's reports ignore locks: `set-stack-status` from the host says what runs where,
    which the operation waits for, and `set-volume-initialized` says what a copy holds. A
    table test holds every kind of command to this rule
    (`TestEveryKindOfCommandIsCheckedForLocks`), so a new kind needs a case or a reason.
  - Each step has a deadline (`Operation.Deadline`, `StepTime` unless the sender gives
    one). The leader gives more time to a step that makes progress (`extend-operation`),
    and turns one back before the kind's point of no return (`roll-back-operation`). The
    operation is then `rolling-back` until the leader has undone what it did and finishes
    it as failed.
- **Fencing a host** (`fence-host`, `HostFence`; the fencing loop starts and drives it,
  loops.md, Fencing). It locks the host, and its start makes the host `fencing` in the same
  command. Its steps are `FenceStart`, then `FenceOff` once the server is off and the host
  is fenced. It has no rollback: if the server cannot be powered off, the operation moves
  the host back to failed and fails. The leader finishes it once the stacks the host had
  (`HostFence.Stacks`, recorded when it is fenced) run elsewhere, or after `StepTime`. Its
  lock is what limits fencing to one host at a time.
- **Moving a stack** (`move-stack`, `StackMove`; the placement starts it, and
  `controller.RunMoves` drives it). It locks the stack and its volumes.
  - Its steps, each named after the last step done:
    1. `MoveCopies`: the leader placed a copy of each volume on the new host.
    2. `MoveSynced`: the new host has the copies synced (`VolumeCopy.Synced`) and the
       images pulled.
    3. `MoveStopped`: the old host stopped the stack, and its copies are secondary.
    4. `MoveHost`: the leader set the stack's host. A volume of replicas 0 gets a new,
       empty data generation there.

    The leader finishes the move once the stack runs on the new host.
  - The hosts do the work through their loops. From `MoveSynced` on, `StopsForMove` takes
    the stack off its old host's runner and reconciler, and the new host's reconciler
    promotes once the stack is placed there. `MoveStopped` is the point of no return.
  - The deadline of `MoveCopies` allows twice the time of a resync at
    `replication.resyncRate`. Once it has passed, the leader asks the new host how much is
    left (`/v1/internal/volumes/progress`) and extends the deadline while that shrinks.
  - A move that ends answers a user's `request-move` (`Stack.MoveTo`), whether it worked
    or not. A move that worked records `Stack.LastMove`; one that failed records
    `Stack.MoveFailed`. Within `MoveCooldown` the placement then starts no move of its own
    for that stack (a user's request still goes ahead), so a move that cannot work does not
    resync copies over and over.
  - If a host of the move goes down (no longer active or draining), the move ends at once
    (`abandon`). Before `MoveStopped` it rolls back, and after it the move fails; either way
    the stack keeps its old host. From `MoveHost` on, a new host that goes down ends the
    move as failed, and the failover takes the stack.
  - A running stack that cannot move gets the alert `stack-waiting` with "cannot move".
  - To follow a move through the code: `scheduler.place` decides it, `controller.startMove`
    starts it, `controller.mover.placeCopies` (with `scheduler.MoveVolumes`) runs the first
    step, `agent.moveSteps.synced` and `stoppedHere` record the hosts' steps,
    `controller.mover.setHost` records the leader's last step, and `controller.mover.late`
    watches the deadlines.
- **Retired copies.** A volume copy's data is removed only when the state says so. A delete
  of a volume (`change-resources`), and a `put-volume` that changes the data generation or
  takes a copy off a host, each add the copies they give up to `Retired` (volume, data
  generation, host). The host removes those copies and forgets each with `forget-copy`.
  The copies of a removed host go with the host. A copy that is merely missing from the
  state stays, because a bug in the placement must not cost data. A retired copy keeps its
  volume's DRBD minor taken (`RetiredMinors`) until every host has forgotten its copies.
  The resource on the host still has the minor, and drbdadm refuses every command while two
  resources share one.
- **Initialized volumes.** `Volume.Initialized` says that a copy of the current data
  generation was primary with its file system. The first host whose copy reaches that
  state reports it with `set-volume-initialized` (only a host with a copy with a disk in
  that generation may). Until then, a host may promote with `--force` and format; after
  that, never, on any host. A copy without a file system then lacks the data; it is not
  new. `put-volume` keeps the flag within a data generation and clears it with a new one
  (a new LV).
- **Host phases** (`phaseChanges`; L = the leader, on its own or for a user; H = the host
  itself):

  ```
  joining ─L→ passive ─L→ active ⇄L draining
     └──────L──────────────↗
  active, draining, recovering ─L→ failed
  failed ─L→ active                       (heard again, before the fence)
  failed ─O→ fencing ─O→ fenced           (the server is off)
             fencing ─O→ failed           (it could not be powered off)
  failed ─U→ fenced                       (fence-confirm)
  fenced ─H→ recovering ─H→ active        (all its copies synced)
  fenced ─L→ disabled ─U→ fenced          (flapping brake, host enable)
  any phase ─L→ removed
  ```

  O is the fence-host operation (`OperationFenceHost`, `HostFence`). U is the CLI; the
  state also allows the leader these changes (`byLeaderOrCLI`), and the API lets the CLI
  make only these. Each change checks the phase it starts from (`From`), so of two changes
  that race, only the first passes.

  | From | To | Who | When |
  |---|---|---|---|
  | `joining` | `passive`, `active` | members loop | the host reported; passive while it is the second |
  | `passive` | `active` | members loop | the third host is there |
  | `active` | `draining` and back | user | `host drain`, `host undrain` |
  | `active`, `draining`, `recovering` | `failed` | fencing loop | a majority of the voters has not heard it for `FailAfter` |
  | `failed` | `active` | fencing loop | a majority heard it again within `backWithin`; never the host itself |
  | `failed` | `fencing` | the operation's start | `start-operation` of a fence, in the same command (see below) |
  | `fencing` | `fenced` | the operation | its server is off (see below) |
  | `fencing` | `failed` | the operation | `PowerOff` failed `powerOffTries` times; the operation fails |
  | `failed` | `fenced` | user | `host fence-confirm`, after the user checked that the server is off |
  | `fenced` | `recovering` | the host | its agent runs again and has caught up with the leader |
  | `recovering` | `active` | the host | every copy of it with a disk is synced (`checkCaughtUp`) |
  | `fenced` | `disabled` | fencing loop | fenced `FlapLimit` times within `FlapWindow` (`Host.Flapping`) |
  | `disabled` | `fenced` | user | `host enable`: clears `FencedAt`; the leader powers it on |
  | any | `removed` | `clowderd recover` | `recover-remap`: every host except the one that recovers (Recover below) |

  `failed` to `fencing` happens only for a failed host with a checked server and a stack
  that another host can take (`needsFence`). At `fencing` to `fenced`, the time is added to
  `FencedAt`, the host's copies are no longer synced, and the operation records the host's
  stacks.

  In words: a new host joins as `joining` (`add-host`). The members loop makes it
  `passive` (waiting) or `active` (running stacks), and a user can drain it for
  maintenance and undrain it again. A drain moves the host's stacks off it and keeps new
  ones off. Its volume copies stay, because the host comes back after the maintenance. A
  host that the voters stop hearing becomes `failed`, and the leader powers it off before
  any of its stacks start elsewhere (loops.md, Fencing). A host can never leave `failed` by
  itself. It cannot tell whether the leader is about to fence it, and it would keep writing
  while a peer promotes its volume (split-brain). Once a fence has started, nothing stops
  it, not even a late beat: `fencing` has no change back to `active`, and only its
  operation moves it on (`checkFenceDecided` keeps the operation from ending while the
  host is still fencing). While an operation holds `host/<id>` (fencing, drain, remove),
  only that operation changes the host's phase, and the host's own phase reports are
  refused too. `KeepsStacks` says which phases run the stacks placed on the host. A failed
  or fencing host keeps running them, because no other host runs them before it is off; a
  fenced, recovering or disabled host runs none.

  What each phase means elsewhere, in one place (Y: yes; the named functions decide):

  | Phase | runs its stacks (`state.KeepsStacks`) | its stacks fail over (`state.FailsOver`) | takes new stacks (`scheduler.takesStacks`) | its copies count (`scheduler.counts`) | votes (`controller.votes`) |
  |---|---|---|---|---|---|
  | `joining`, `passive` | | | | | |
  | `active` | Y | | Y, unless leaving | Y, unless leaving | Y |
  | `draining` | Y | | | Y, unless leaving | Y |
  | `failed`, `fencing` | Y | | | Y | Y |
  | `fenced` | | Y | | for `CopyWait` | Y |
  | `recovering` | | | | Y | Y |
  | `disabled` | | Y | | | Y |
  | `removed` | | | | | |

  A failed host's copies are no longer synced (`unsync`), because the other copies went on
  without it. So no stack fails over or moves to it until DRBD has caught its copies up and
  the host reports that.

  **Removing** a host (`checkRemoval`; the CLI asks first) is refused for the last host that
  runs stacks (`active` or `draining`), and for a host that runs stacks while a stack or a
  volume copy is on it. When a user removes such a host, it leaves instead
  (`SetHostPhase.Leaving`, `ErrNotEmpty`). It drains, the placement moves its copies off
  too (they no longer count), and the members loop removes it once nothing is left on it.
  An undrain takes it back. Once `MinHAHosts` hosts have run stacks, the state is `HA` for
  good, and no removal may leave fewer than three hosts that run stacks; a replacement
  joins first. A removed host's certificate is revoked. Every other host forgets it as a
  DRBD peer and reports that (`report-peer-forgotten`), and `Forgetting` holds the hosts
  that have not done so yet. Only then is its ID free for a host that joins (`HostIDFree`),
  because DRBD's node-id is the host ID and a peer's slot in the metadata outlives the
  host. A host that is down reports nothing, so the ID waits for it; once that host is
  removed too, nobody waits for it any more. The removal does not take the data key away: a
  removed host that kept its disk keeps the CA's key and the secrets (roadmap, Known gaps).

- **Versions.** Every agent must apply every entry of the log, or the states drift apart for
  good (if a `revoke` is skipped, a certificate keeps working on that agent). So:
  - **Fail-stop.** An entry that the agent cannot read (not JSON, a `CommandVersion` it
    does not read, a kind it does not know) is not a refusal. `Apply` leaves the state and
    its index as they are and returns `state.ErrUnreadable`; `ReadSnapshot` does the same
    for a snapshot it cannot read. The fsm halts (`raftnode.Node.Halted`): it applies,
    restores and snapshots nothing more. The agent ends with exit status 3, which systemd
    does not restart (Shipping), and the other hosts see it fail. A refusal
    (`CommandError`, and `ErrNotBootstrapped`) is the same on every agent of a version, so
    it moves the index on.
  - **The gate.** Each kind names the release that introduced it (`commandKinds`).
    `Node.Propose` commits a kind only once `State.MinVersion`, the oldest release that an
    agent reported, includes that release (`buildinfo.Includes`: `0.2.0~rc1` includes
    `0.2.0`). Otherwise the command fails with `state.ErrNotReadByAll`, which is not a
    refusal, and its caller tries again later. Development builds read every kind of their
    code and do not count. Neither do hosts that have not reported a version yet (a host
    that joins). Fail-stop catches whatever slips through.
  - **Until `v1.0.0`** the state, the commands and the resources' specs change freely,
    without migrations. Only agents and CLIs of the same minor version work together
    (`buildinfo.Compatible`), and a patch release changes neither the state nor the
    commands. `CommandVersion` and `SnapshotVersion` stay at 1, so fail-stop and the gate
    catch a new kind, but not a payload whose fields changed their meaning. That is why
    different minor versions must not run side by side. A cluster is updated host by host
    only within a minor version (Shipping, Updating a cluster).
  - **From `v1.0.0`** a release reads its own `CommandVersion` and `SnapshotVersion` and
    the previous one, and CLI and agents work together across one minor version. Payload
    fields are added, not changed. Unknown fields are ignored, so an agent of the previous
    release drops a new field from its state. A field that later commands depend on
    therefore needs its own gate on `MinVersion`.
  - The json tags of `clusterspec.Config` are part of this format.
- **Invariants** that the state enforces itself, as the last place to catch a wrong
  command:
  - valid names (`internal/names`);
  - volumes with 0 to 3 replicas, and a DRBD minor exactly when replicated (a pending
    volume has neither copy nor minor);
  - one copy per host, one stack per volume;
  - nothing deleted that a stack names;
  - a removed host's certificate revoked;
  - no name, underlay, server or certificate of a host twice among the hosts that are not
    removed (`CheckJoiningHost`);
  - every host with a server ID, an IPv4 underlay and an IPv4 public address;
  - each underlay in `privateNetwork` once a configuration names it;
  - every joined host's server checked with Hetzner.
- **Revocations.** `Revoked` holds each revoked host certificate with its expiry time (the
  host's `CertificateNotAfter`); removing the host adds it (`revoke`). The list grows only
  there, so `revoke` also drops the certificates that expired more than `revokedKept`
  before the command. No handshake accepts them any more, and the margin covers a host
  whose clock runs behind. It goes by the command's `At`, so every agent drops the same
  entries.
- **The CLI's certificates** are not listed, just as kubeadm lists none: whoever holds one
  controls the cluster. `CLICutoff` (`set-cli-cutoff`, from `clowderd credentials
  --revoke-older`) refuses every CLI certificate that starts before it. It only moves
  forward, so a revoked laptop never gets back in. A host sets it, because root on that
  host issued the new certificate.
- **Secrets** arrive sealed (`Sealed`, age for the data key); `state` never sees them in
  clear. A `Secret` holds them by key, with a digest (`SealedSecret`). The cluster's own
  secrets (the Hetzner token, the alert channels' URLs and passwords, the heartbeat's URL)
  are Secrets of `clowder-system`, which the configuration names by `{name, key}`
  (`names.SecretKeyRef`; `clusterspec.SecretPlaces` lists the fields that do). Readers
  find a value through such a reference or a spec, never by taking a name apart:
  `SystemSecret(ref)`, `HetznerToken` (the token the configuration names), and
  `RegistryLogins(namespace)` (the sealed `server`, `username` and `password` of each of
  the namespace's image pull secrets). Until the first configuration, the loops that need
  the Hetzner token or a Floating IP find none (`ErrNotConfigured`) and wait. Until the
  first `set-recovery-recipient` there is no `RecoveryRecipient`, and nothing can be
  encrypted for the cluster (`GET /v1/encryption` says so). A stack names at most
  `MaxStackSecrets`. Who can read a secret is described under API, below.
- **The cluster's name** (`ClusterName`) comes from `clowderd init --cluster-name`, like
  kubeadm's `clusterName`, and must pass `names.Valid`. `cluster status`, the alerts and
  new credentials carry it; nothing is checked against it.
- **Join tokens** (`internal/jointoken`) consist of an ID and a 256-bit secret.
  `add-join-token` keeps only the secret's hash under the ID, with its expiry time: the
  command's `At` plus its TTL (`MinJoinTokenTTL` to `MaxJoinTokenTTL`, at most
  `MaxJoinTokens` open). `add-host` checks the token against its own `At`
  (`CheckJoinToken`) and deletes it in the same command, so a token works only once.
  `dropExpiredTokens` drops expired tokens a while later (`joinTokenKept`), so a late join
  learns that its token expired.
- `Copy` returns a deep copy without the audit. It is for work that is too slow for the
  state's lock (the placement weighs every stack) or that keeps parts of the state (the
  agent's stacks). The loops do not make their own copies: `raftnode.Node.SharedCopy` makes
  one copy per change, which every loop reads and none changes. A dry run applies commands
  to its copy, so it makes its own.
- Snapshots are gzip-compressed JSON (`WriteSnapshot`, `ReadSnapshot`). The audit keeps the
  last `auditSize` commands.

## Certificates (`internal/pki`)

- The cluster CA issues host certificates (server and client, valid for the host's
  addresses) and CLI certificates (client). The role is the certificate's organizational
  unit, and the name is its common name: `host-<id>`, or the CLI's name (`clowderd
  credentials --name`, or `DefaultCLIName` without one, just as kubeadm's `admin.conf` is
  `kubernetes-admin`). The name must pass `names.Valid`. A CLI name is for the audit and
  grants nothing.
- Keys are Ed25519, and the CA signs public keys. Hosts' keys never leave the machine that
  created them. `clowderd credentials` creates the CLI's key on a host and hands it over in
  a CLI config, as kubeadm does with `admin.conf`.
- A host certificate ends by revocation, not expiry, because hosts may stay off for months
  (`leafLifetime`). `Verify` takes the revoked serials from the state, which keeps each one
  until a while after it expired. A CLI certificate lasts `cliLifetime`, a year like
  kubeadm's client certificates, or ends sooner through the CLI cutoff: the API compares
  its start (`Identity.NotBefore`) with `State.CLICutoff`. Nothing renews it. Every CLI
  command warns from `renewWarning` before its end (`cli/connect.go`), and the user fetches
  a new one from a host, as with `kubeadm certs renew admin.conf`.
- `TrustProof` (HMAC-SHA256 over the CA certificate) is how a joining host trusts the CA on
  first contact. Its proof key is the join token's hash (`jointoken.ProofKey`). The CLI
  needs no proof, because its first credentials come over SSH with the CA included.
- `tls_test.go` tests both TLS configs against every peer they must refuse: none, the CLI,
  a host of another CA, a stranger showing a host's certificate behind its own, and a
  revoked, an expired and a not yet valid host. It runs the handshakes on 127.0.0.1.

## Raft (`internal/raftnode`) and the data key (`internal/datakey`)

- `raftnode` runs hashicorp/raft, with the log and stable store in BoltDB, a snapshot every
  `snapshotThreshold` entries, and the state of `internal/state` as the replicated
  machine. Raft's own log lines go through `log/slog`.
- Raft uses mutual TLS on the node address: both sides present a host certificate of the
  cluster CA that is not revoked. CLI certificates and other clusters' hosts fail the
  handshake, and removing a host revokes its certificate, so it leaves Raft and the API at
  once. `pki.ClientTLSToHosts` and `pki.ServerTLSForHosts` are the one way hosts are
  checked, by Raft and by the API alike: by the chain and the role, not by the name.
- **Leading starts with a barrier.** `followLead` sets `IsLeader` only once Raft's barrier
  has applied every entry from before the lead. A new leader may not yet have applied what
  its predecessor committed, and its loops and `runOnLeader` would then decide on an old
  state. `VerifyLeader` asks a majority whether this member still leads. The leader asks
  before it powers a server off or moves the Floating IP, because a leader cut off from the
  others believes that it leads until its lease runs out.
- **Catching up.** `CatchUp` waits for a leader (and, on the leader, for the barrier). A
  follower then asks for the leader's commit index (`api.Server.CatchUpWithLeader`,
  `/v1/internal/commit-index`, answered after `VerifyLeader`) and waits until its state
  holds that index (`WaitCommitted`). Raft's own entries never reach the state, so it waits
  for Raft to have applied the index, and then for the last command up to it. The agent
  starts its stacks and volumes only after that (host.md, The agent), because a host that
  was away must not start a stack that moved in the meantime. Until then, the API also
  holds users' calls, for at most `waitTimeout` (`ServerConfig.CaughtUp`), because a state
  that has not caught up may still be empty and answer that the cluster is not
  bootstrapped. The hosts' own routes do not wait, because the catch-up, the beats and the
  joins use them.
- `Propose` commits a command through the leader and returns its index, or the state's
  refusal. `Read` gives the current state under a lock; no caller may keep it, because
  `Apply` changes it in place. `WaitIndex` lets a read follow a write made through another
  member. `Changes` serves the watch from the member's ring of changes (API, The watch).
- `Bootstrap` makes the first host a cluster of its own; on a member that has a log it does
  nothing. The leader changes the members one at a time (`members.go`: `AddNonvoter`,
  `AddVoter`, `DemoteVoter`, `RemoveMember`, `TransferLeadership`), from the members loop
  (`controller/members.go`). Each pass makes one change, in this order
  (`nextMemberStep`):
  1. A leader that is being removed hands over its lead. The API does that before it
     commits the removal (`handOverLeadOf`), because the removal revokes the leader's
     certificate. If the state then refuses the removal, the lead has moved for nothing,
     which does no harm. The loop is the fallback for a removal that the API did not see
     coming: the others accept no new connection from a revoked leader, but Raft's open
     connections stay, and so would its lead.
  2. Every host that is not removed is a member; a joining host joins as a non-voter. A
     joining or passive host that votes loses its vote. This happens to a host that took
     the ID of a removed host before that host left Raft: it finds the old host's vote.
  3. A joining host whose agent reported (`report-host`) becomes `passive` while it is the
     second host of a cluster that is not `HA` yet, and `active` otherwise. A passive host
     becomes active once there are three hosts.
  4. Of the hosts that run stacks or are expected to come back after a failure (`votes`:
     active, draining, failed, fencing, fenced, recovering, disabled), 1, 3 or 5 vote
     (`voterCount`): first those that already vote, then those with the lowest IDs. Only an
     active or draining host gets a new vote. A failed voter keeps its vote, because taking
     it would take a live host's vote instead (with three hosts, the cluster would stop at
     the next failure). The leader keeps its own vote.
  5. A removed host leaves Raft, after step 4 has given its vote to another host.
  6. `set-host-voter` mirrors each host's vote in the state, which shows it.

  Raft changes its members one by one, so the third host's join passes through a few
  seconds with two voters, during which the loss of either one stops the cluster until it
  is back. The leader handles one join at a time (`joiner.mu`), so two hosts never get the
  same ID.
- The data key is an age identity in a file that only root can read. `Seal` and `Open` turn
  secrets into `state.Sealed` and back. Joining hosts receive the key with their join
  answer.
- `ReadState` reads the state from the log on disk without Raft (the last snapshot, then
  the commands after it). `Recover` makes the member its cluster's only voter
  (`raft.RecoverCluster`). Both are for `clowderd recover`, while the agent is stopped.
- The tests run real members on 127.0.0.1: a restart from disk, three hosts over mTLS,
  strangers refused, and a member that recovers alone.

## Recover (`clowderd recover`, `agent/recover.go`)

A cluster that has lost the majority of its voters for good decides nothing, but the one
host left still has current copies of its volumes. `clowderd recover --power-off` lets it
go on alone, and only on a human's word. The danger is the hosts that are gone. If they
were switched on again, two of them would still have the old Raft configuration's majority
and their copies would have DRBD's quorum, so they would elect a leader and fence the host
that recovered. So `Recover` takes them out at Hetzner, in this order. Each step is safe to
run again after a failure.

1. It stops `clowderd.service` and reads the state from the log on disk
   (`raftnode.ReadState`). The hosts to retire are all hosts except this one that are not
   removed. A cluster of one host that never recovered has nothing to recover.
2. It asks each of them over its underlay whether it knows a leader (`api.KnowsLeader`,
   `/v1/internal/leader`: a leader answers once a majority has confirmed it, a follower
   names the leader it hears). It refuses if one does, because that cluster is not lost,
   even when this host reaches only a follower of it; its hosts leave with `host remove`.
   It also refuses if a host answers without saying, such as with a refused certificate
   (the others may have removed this host) or with an answer that does not come in time.
   Only a host that accepts no connection at all (`api.ErrNotReached`) counts as gone, and
   step 3 names it.
3. It shows what the remap will do, worked out on a copy of the state, and asks for this
   host's name, which confirms which host goes on alone (`--yes` for scripts). Up to this
   point nothing has changed, and a refusal starts the agent again.
4. For each host, it sets the label `clowder-retired` with the serial of the host's
   certificate (`hetzner.Client.SetRetired`), then powers the server off; Hetzner confirms
   both. The label comes first, because a server that is off but not labeled could come
   back. A server that the leader never checked is checked first.
5. It restarts Raft with this host as the only voter (`raftnode.Recover`). The member opens
   on its own and commits `recover-remap` as the leader. The API forwards no such command.
6. DRBD goes on without the other hosts (`storage.Driver.DropPeers`): `del-peer` for each,
   then quorum off (DRBD refuses that while a peer is configured), then `resume-io` for a
   resource whose writes were suspended. Phase 0 established this order. Quorum is off only
   until the agent adjusts the resource to its file, which now names this host alone.
7. It starts the agent. Its loops do the rest: the mesh drops the removed hosts, the
   Floating IP comes here, the placement places what waits, and `forgetPeers` runs
   `forget-peer`.

`recover-remap` (`state/recover.go`) makes every other host `removed` (its certificate
revoked, its ID free once this host has forgotten it) and fits the rest to what this host
holds:

- A volume of replicas 1 or more keeps a synced copy with a disk here. A volume without one
  waits for a restore (`VolumeRestorePending`): the placement gives it no copy, and its
  stack waits with the reason. A tiebreaker of it here is retired. An unsynced copy here
  stays a copy, because the state never removes what may hold older data: `get volumes`
  shows it, and a delete of the volume removes it. Until restores arrive (phase 3), the way
  out is to delete the volume and apply it anew.
- A volume of replicas 0 whose LV was on another host starts anew, empty, as after a
  failover.
- Every stack that is not on this host, or that names a volume without data, is placed
  anew. Every running operation ends as failed and frees its locks.
- This host is active, the only voter and the ingress host. `HA` is false again, so the
  cluster grows as a new one does (the second host is passive), and `RecoveredAt` raises
  the alert `no-ha` until three hosts run stacks.

A retired host's agent ends at its start (host.md, `checkRetired`), because the label
names its certificate. A server that is set up anew gets a certificate of its own and
starts, label or not. If Hetzner's API does not answer at the start, a retired host starts
as before (a known risk, roadmap), so recover tells the user to set such servers up anew
or delete them.

## API (`internal/api`) and the CLI (`internal/cli`)

### Requests and the leader

- **Every call is checked.** Every agent serves `/v1` with mutual TLS (`agent.APIPort`).
  Each call is checked against the CA, the state's revocation list and, for the CLI,
  `CLICutoff`, so a revoked certificate is refused at once on every agent. The CLI's routes
  accept CLI certificates, and `/v1/internal/*` accepts only host certificates. Every
  request body is limited (`MaxRequestBytes`; its comment explains why 16 MiB). A larger
  one gets 413, and `apply` then advises applying the namespaces' directories one at a
  time.
- **Any agent answers.** Reads come from its own copy of the state. Writes it commits as
  the leader, or forwards to the leader's `/v1/internal/propose`. A write answers with its
  Raft index (`X-Clowder-Index`). The client sends that index with later reads
  (`X-Clowder-Min-Index`), and the agent waits until it has applied it. Without a leader
  (during an election, after a peer restarted, or while a new leader is still at its
  barrier), every path to the leader (a command, the batch of an apply, a join, the
  catch-up) waits up to `leaderWait` for one (`waitForLeader`) instead of failing at once.
- **Who speaks.** The actor of a command is the caller of the first agent, and its time
  (`At`) is the leader's clock. `/v1/internal/propose` accepts only two things:
  - the CLI's command that a follower forwards (`cliKinds`, exactly the kinds that the
    CLI's routes make, with any CLI actor);
  - what a host commits as itself (`hostKinds`: its reports, and the cutoff of `clowderd
    credentials --revoke-older`).

  `/v1/internal/changes` accepts only the CLI's resource commands (Resources, below). The
  leader's own commands (placements, the ingress) are made on the leader and never come in
  from outside. A host never speaks for another host or for the leader.
- **A host certificate may speak for the CLI.** The CLI's certificate ends at the first
  agent, so the leader cannot check that a forwarded command really came from the CLI. This
  is on purpose, just as kubeadm's control plane trusts its own nodes. A host holds the
  data key, the CA's key and the whole Raft state, and root on a host can print credentials
  anyway (`clowderd credentials`), so a host is trusted with everything. A host names the
  CLI's actor (`cli/<name>`) and could name another; that is accepted, because the name
  grants nothing. The agent that takes a CLI request logs its actor with every write (`a
  CLI request`, in that agent's journal only). The record that every agent keeps is the
  actor of the Raft command and the state's `Audit`. The checks on the internal routes
  catch a bug in an agent, not a host that lies. What keeps strangers out is the CA, and
  removing a host revokes its certificate.
- A refusal by the state is HTTP 409 with its reason; a write that did not commit is 503.
- **JSON names.** What users write and read as resources (`resource.Object`, the specs and
  the statuses that `get` shows, including the ClusterConfiguration) is camelCase, as in
  the files users apply. Everything else (commands, the state, snapshots, the API's other
  bodies) is snake_case.
- `api.HostActions` lists the user's host actions with the phases they start from. The
  server checks them, and the CLI builds its subcommands from them.
- An agent refuses a caller, CLI or agent, whose version is not compatible with its own
  (`X-Clowder-Version`, `buildinfo.Compatible`: the same minor version before `v1.0.0`,
  then at most one minor version apart). Development builds and callers without a version
  talk to everything. The agent logs a warning once per version when a development build
  talks to another build.

### Resources

`changes.go` changes resources and `resources.go` reads them; the CLI's `apply`, `get` and
`delete` are described under The CLI, below.

- `POST /v1/apply` (`ApplyRequest`) and `POST /v1/delete` (`DeleteRequest`) take
  resources in order and run them as one `change-resources`, all or nothing. They answer
  per resource (`ChangeResult`):
  - `applied` or `deleted`;
  - `unchanged`;
  - `refused`: the state said no, with the reason, at the resource that its `ChangeError`
    names;
  - `failed`: did not commit (no leader, no quorum); try again;
  - `not-applied`: every other resource of a request that was refused or failed.

  They are the only way to change resources. Every user's change comes through the channel
  `cli`, whatever the certificate's name. Requests are read strictly, so a typo is an
  error, not an empty request. The internal routes read leniently, because an agent of the
  next release may send fields that this one does not know.
- The agent that took the request turns each resource into its put (`putChange`). It reads
  the spec strictly (`resource.DecodeClusterConfiguration`, `DecodeNamespaceSpec`,
  `DecodeVolumeSpec`, `stackspec.Decode`): a field that the kind does not have is an error,
  and `replicas` has no default. It seals a secret's values and computes its digest
  (`sealSecret`: `digest.HMAC` over the ID and every key and value, with a key that
  `Keys.Derive` gives for "clowder secret resource digest", separate from the containers'
  labels). A spec that cannot be read is refused at its resource, before the leader sees
  the request.
- **The leader decides** (`runOnLeader`). A follower hands the changes on as a batch
  (`changeBatch`) to `/v1/internal/changes`, because its own state may be behind and would
  answer `unchanged` or `refused` for what has changed since. `runChanges` first applies
  the one `change-resources` to a copy of the leader's state (`State.Copy`), with every
  check of the state. A refusal answers at once, and a batch that changes nothing is not
  proposed (no Raft entry). `?dryRun=true` stops there and answers what a real apply would,
  with index 0. Only a real change is proposed, and the state checks it once more as it
  commits. A batch that changed the configuration returns its warnings (`configWarnings`,
  `clusterspec.ServiceURLWarnings`: `hetzner.api` or `acme.directory` set, and over
  `http://`).
- **A dry run tells what would change** (`describe`). For each applied resource it says
  whether the resource is new (`Created`) or which parts differ (`Differs`). For most kinds
  these are the spec's top-level fields; for a Secret they are the keys that were added,
  removed or whose value changed, judged by the keys' digests and never by a value
  (`secretDifferences`). It also returns the warnings of a configuration it changes, and
  what a new Hetzner token sees.
- **What a new Hetzner token sees** (`checkNewToken`, `TokenCheck`). When a dry run changes
  the token, in the configuration or in the Secret it names, the leader opens the new
  token and asks Hetzner once, with one request for the project's servers
  (`ServerConfig.HetznerServers`, `hetzner.Client.ServerIDs`). It counts the hosts that are
  not removed and whose server it sees (`Hosts`, `Seen`), or says why it could not ask
  (`Error`). Only the token counts here, not `hetzner.api`, because a test lab's cluster
  never moves to Hetzner's own API. A server's ID is unique across Hetzner, and a token
  sees only its own project's servers. So a wrong token powers off no other server; it
  reaches none of the cluster's, and fencing and the Floating IP stop working. That is why
  the CLI asks before it applies one (The CLI, below). The check never refuses, because a
  token may be changed on purpose before its project is.
- `GET /v1/resources/{kind}` lists the resources of `?namespace=` or of
  `?allNamespaces=true`. There is no default, so no reader gets the wrong namespace by
  accident. It returns the state's index that it read at (for the watch). `GET
  /v1/resources/{kind}/{namespace}/{name}` and `/v1/resources/{kind}/{name}` (a namespace,
  the configuration) read one resource. The kind is written in lower case. A secret shows
  its keys and its digest (`SecretStatus`), never its values. Any other resource that a
  read returns can be applied again as it is: `resource.Metadata`'s owner, generation and
  resource version, and the status, belong to the cluster, and an apply ignores them.
- **The watch** (`watch.go`): `GET /v1/watch?kind=&namespace=|allNamespaces=true&since=`
  streams the changes of a kind after `since`, the index of the client's list, as one JSON
  object per line (`WatchEvent`: the resource, its resource version, and whether it is
  gone). Spec and status changes are both events. The client reads a resource itself when
  it needs it.
  - Every agent keeps the last `state.ChangeRingSize` changes (`state.ChangeRing`). The
    ring is filled only from the Raft log (`raftnode`'s `fsm.Apply`), so a dry run never
    reaches it. Hosts' reports about themselves are not changes, so on a quiet cluster the
    ring covers hours.
  - The ring knows the index from which it is complete: the snapshot the agent started from
    or installed, or the oldest change it dropped. A `since` before that gets 410, and the
    client lists again, as with Kubernetes. So a client that listed on one agent can watch
    on another.
  - A `since` that this agent has not applied yet waits at most `watchWaitTimeout`, then
    gets 504, because a follower cut off from the leader would wait forever.
  - The stream runs until the client stops. It ends early when the ring has dropped changes
    that the client has not read yet; the client then lists again too, and says so. The
    agent's stop ends the stream (`Server.EndStreams`, as for logs that follow), because
    `Shutdown` would wait for it.

### The CLI

- **The client** tries the endpoints of the context in turn and trusts any host certificate
  of the cluster's CA, whatever name the endpoint has. A read moves on to the next endpoint
  after any network error. A write moves on only if it was never sent (`httptrace`),
  because a write that was sent may have committed without an answer.
- **The endpoints** are all the CLI knows. It learns no other agents from the cluster, so
  it reaches the cluster only while an endpoint answers. A kubeconfig has one `server`, and
  kubectl reaches several control-plane nodes only through a load balancer, a VIP or DNS
  in front of them. clowder keeps a list instead, so a cluster needs nothing in front of
  it, and whoever puts something there lists its one address. `clowderd credentials` writes
  the public addresses of the hosts at that time (`CredentialsAnswer.Endpoints`), or what
  `--endpoint` names. After joins and removals, the user fetches new credentials or edits
  the endpoints (`host list` shows the public addresses).
- **An endpoint that does not answer** costs little. One that has not connected within
  `--connect-timeout` (`api.DefaultConnectTimeout` without the flag), or has not finished
  TLS within the same time again, counts as unreachable. A host that is cut off drops the
  packets, and the request's own timeout (`answerTimeout`) would hold up the next endpoint.
  An agent that set up the connection and then hangs still holds a request for
  `answerTimeout`. The client starts each request with the endpoint that answered last
  (`inTurn`), so an endpoint that is down costs its timeout once per command, not once per
  request. Every client that asks endpoints shares this transport (`endpointTransport`),
  including the join and recover. `clowderd join` has no `--connect-timeout` and waits the
  default.
- **The CLI** reads its credentials file (`--config`, `$CLOWDER_CONFIG`,
  `~/.clowder/config`; `clientconfig`, specs.md). It takes the context that `--context`
  names, or else the current one (`globals.connect`), and warns from `renewWarning` before
  the context's certificate ends. Which cluster a command reaches depends on the context,
  as with kubectl; where the files come from is up to the user. `clowder config
  get-contexts|use-context|delete-context|import` manage the file (`cli/config.go`;
  `withHint` adds advice on what to do to `clientconfig`'s errors).

  `import <file|->` adds the one context that `clowderd credentials` prints, because
  kubectl has no merge, and merging by hand is where keys get lost. It adds the context
  under the cluster's name or under `--as` (not `--context`, which picks the context a
  command uses). It refuses a name that is taken, or with `--replace` puts the context in
  the old one's place, as after a renewal.

  When an agent's certificate is from another CA than the context's, the CLI says so
  (`foreignCA`: another cluster at that address, or one set up anew) instead of showing
  only x509's error text. This applies to `cluster status`, `host
  list|drain|undrain|remove`, `apply`, `diff`, `get`, `delete` and `stack ps|logs|restart`.
- **Exit codes** are the same for every command:

  | Code | When |
  |---|---|
  | 0 | the command did what was asked |
  | 2 | a wrong call: an unknown command, or flags or arguments that do not fit (`usageError`) |
  | 1 | anything else: refused, missing, not reachable |

  `checkArgs` turns every command's argument errors into a `usageError`. It also gives each
  command that only groups other commands (`clowder stack`) a `RunE`, which shows the help
  and treats an unknown subcommand as a wrong call; cobra would otherwise end as if it had
  succeeded. A refusal shows the state's reason without the command's kind
  (`state.CommandError`). `diff` is the one exception, like `kubectl diff`: it returns 1
  when something would change and 2 when it could not tell (`ExitCode`, below).
- **`apply -f <file|directory|->`** (`resources.go`, `files.go`) reads YAML documents.
  `resource.ParseDocuments` accepts a file of clowder's resources only, or a file of other
  documents, but never both; another apiVersion of clowder's is an error. For a directory,
  it reads the YAML files in it by name, but not those in subdirectories. Hidden files,
  other files and files of other documents are skipped and named on stderr.

  A `Secret`'s values are stored encrypted in `spec.data`, one per key, tagged `!secret`.
  `resource.ParseDocuments` takes them out before it turns YAML into JSON, and refuses
  `!secret` anywhere else. The CLI sends them as they are (`SecretSpec.Encrypted`). A
  `Secret` in clear is refused, both in a file and on stdin, because the CLI never handles
  a value in clear. `secret encrypt [<namespace>/]<name> (--from-literal <key>=<value> |
  --from-file <key>=<path|->)...` writes a Secret, with each value encrypted for the
  recipients that the cluster names and for the Secret's ID.

  A resource without a namespace is in `default`. The resources are sent in the order of
  the files, as kubectl applies them: each file from top to bottom, a directory's files by
  name, all in one request. So whatever a resource needs must come before it. Each result
  is printed, and any resource that is not applied, deleted or unchanged makes the command
  fail. `--dry-run` checks everything and changes nothing. The warnings and what a new
  Hetzner token sees go to stderr (`printWarnings`).
- **The CLI asks before it applies a new Hetzner token** (`confirmToken`). An apply that
  contains the ClusterConfiguration or a Secret of `clowder-system` (`mayChangeToken`) is
  first sent as a dry run. If the answer has a `HetznerToken`, the CLI shows what the new
  token sees (`printTokenCheck`: "sees the servers of 3 of 3 hosts", "sees none", "could
  not be checked"), asks for `yes` on a terminal as `host remove` does, and then applies.
  Without a terminal, `--yes` is the only way. A GitOps source (phase 3) has nobody to ask,
  so only the alert `hetzner-token` reports a bad token (loops.md, Alerts).
- **`diff -f <file|directory|->`** sends the resources as a dry run and prints each one that
  would change (`printDiff`): whether it is new or which fields differ, and for a resource
  the cluster would refuse, the reason. Like `kubectl diff`, it ends with 0 when nothing
  would change, 1 when something would (`errDiffers`, without a message), and 2 when it
  could not tell (`diffFailed`), including a refusal.
- **`get <kind> [[<namespace>/]<name>]`** and **`delete <kind> [<namespace>/]<name>`**
  accept a kind in singular or plural, and take the namespace as kubectl does: from `-n`,
  or in the name (`shop/data`), or else `default`. `-A` lists every namespace. Where both
  name one, they must agree; a namespace takes neither (`selectFlags.resolve`, with a table
  test of every combination).

  `get` prints a table, `-o yaml` or `-o json`. The YAML documents keep the API's field
  order, and `apply` accepts them again; a secret shows only its digest. `get --watch`
  (`watch.go`) lists, then shows each resource that changes, until Ctrl+C: as table rows
  without repeating the header, or as one document each. A name filters on the client.
  After a 410, or when the agent ends the stream, it lists again and continues.

  `delete -f` deletes the resources of files from the bottom up, the reverse order of an
  apply, all or nothing, with the namespaces that the files name (no `-n`). The
  configuration and the namespaces `default` and `clowder-system` are never deleted.
  Deleting a volume deletes its data, so a delete that names one needs `--with-data`
  (`DeleteRequest.WithData`). Without it, the API refuses the whole request before anything
  is deleted. Otherwise a delete of a directory would remove the stack and then stop at its
  volume.
- **`stack ps|logs [<namespace>/]<stack>`** (`stack.go`) ask the stack's host
  (`GET /v1/stacks/{namespace}/{name}/ps|logs`, then `/v1/internal/stacks/<project>/...`,
  `names.Project`), whichever agent the CLI connected to. **`stack restart
  [<namespace>/]<stack> [<service>...]`** sends `restart-stack` (`POST
  /v1/stacks/{namespace}/{name}/restart`, `RestartRequest`). The list of stacks, also as
  JSON for scripts and the live tests, comes from `get stacks`. Its table shows a priority
  that comes from the configuration's default as `(default)`. `cluster status` shows the
  configuration's generation (`api.Status.Generation`), or that there is none.

### Secret values

- **A Secret's values** are opened by the agent that takes the request, with the cluster's
  age key (`sealSecret`, `secretvalue.OpenFor`; the key comes from `clusterIdentity` and is
  sealed in the state). The agent refuses a value that is not encrypted for the Secret's
  ID. So a value copied from another Secret, such as the Hetzner token of
  `clowder-system`, which a container would then read, does not pass. Then the agent seals
  each value and computes the digest.
- **The cluster's own values** are Secrets of `clowder-system` and come in like every
  other Secret, with `apply`. The configuration names them by `{name, key}`
  (`hetzner.tokenSecretRef`, the channels' `webhookSecretRef`, `urlSecretRef` and
  `smtp.passwordSecretRef`, the heartbeat's `urlSecretRef`), just as the hcloud controller
  reads `kube-system/hcloud`. No stack lives in `clowder-system`, so no container gets
  them. These open them: the leader's loops (`controller.OpenHetzner`, `notify.New` with
  `notify.Secrets`), the join, `clowderd recover` and the agent's start (`checkRetired`).
- **Who can read a secret:**
  - whoever holds the recovery key, because every value in the files is also encrypted for
    it;
  - every host, because a stack must be able to start anywhere;
  - whoever holds the CLI's credentials, through a stack that names the secret, or through
    a stack in `stacks.privileged`, whose containers reach the secrets and the data key on
    their host.
- **`GET /v1/encryption`** names the recipients that every value is encrypted for: the
  cluster's age key and the recovery key (`State.RecoveryRecipient`).
- **`secret rekey -f <file|directory>`** (`cli/rekey.go`) re-encrypts every value of the
  files for these recipients. A value made by hand with `age` may lack the recovery key,
  and after a new recovery key every value lacks it. `fileValues` (`cli/values.go`) reads
  the values, and what each one must be for, the same way apply reads the file: a value
  appears only in a Secret's `spec.data` and is for the Secret's ID. The cluster encrypts
  them anew (`POST /v1/encryption/rekey`, in requests below `MaxRequestBytes`). It refuses
  a value meant for another place, as an apply would (`secretvalue.OpenFor`), answers only
  with ciphertext, and logs who asked; nothing goes into the state. `secretvalue.Rewrite`
  puts the new armor in place and writes the file all or nothing. With `--identity`, the
  CLI opens the values itself with the recovery key, makes the same checks, and encrypts
  them for the cluster it talks to. This is the way to a new cluster when the old one is
  lost. A refused file stops the run; the files reported before it are done.
- **`secret decrypt --identity <file|-> -f <file>`** (`cli/decrypt.go`) is for emergencies
  and needs no cluster. It prints one line per value: where it is (the Secret's ID and the
  key, `shop/db[password]`), a tab, and the value. A value that is not printable text, has
  a space at either end, or starts like a marker is shown as `base64:`. A value that does
  not open, or is meant for another place, is shown as `error:` with the reason, and the
  command fails after printing every line.

### Joining hosts

- **A host joins** with a token that the CLI created (`clowder host join-token`,
  `api/join.go`, `agent/join.go`).
  - `GET /v1/join?token=<id>` returns the CA certificate with a `pki.TrustProof` made with
    the token's hash, while the token is open. The host trusts the CA only if the proof
    matches its token (`api.FindClusterByToken`).
  - `POST /v1/join`, over TLS checked against that CA, carries the token itself, the host's
    name, addresses, server ref and public keys (its own key and WireGuard's; the private
    halves stay on the host), a nonce and the time. Any agent accepts it and hands it to
    the leader (`/v1/internal/join`). The joining host waits longer for the answer than the
    leader may take (`joinClientTimeout`).
  - The leader (`agent.joiner`) accepts a request only within `joinMaxAge`, and each one
    only once while it leads (a nonce in its memory). This is little more than insurance,
    because only the cluster's agents see a request, over TLS, and whoever saw one has seen
    its token.
  - The leader compares the token's hash and requires an applied configuration
    (`Configured`). It chooses the lowest free ID and issues the host's certificate. It
    checks the host against the state (`CheckJoiningHost`), checks that the request comes
    from the host's underlay or public address (`JoinRequest.Source`, which the agent that
    accepted it sets from the connection), and checks its server with Hetzner
    (`hetzner.Client.CheckServer`: the server has the host's addresses). Then it commits
    `add-host`. Hetzner vouches for the addresses, and the source vouches for the host
    that has them. Without the source check, whoever holds a token could name another
    server of the project, which fencing would later power off.
  - A refusal says why, because only the holder of an open token's secret gets that far,
    and the token stays open for another try. The answer carries the certificate, the data
    key and the other hosts, for the host's first WireGuard interfaces (A host, Join).
- **The join's routes need no certificate,** so each agent accepts at most
  `triesPerSender` tries a minute per sender and `triesInAll` in total, counted in memory
  (`api/limit.go`); after that it answers 429. A sender is its IPv4 address, or its /64
  for IPv6.
- **The server ID** decides which server fencing powers off and where the Floating IP goes,
  and the Hetzner token may act on every server of its project. A joined host's server is
  checked as it joins. The first host's server, whose ID init takes from the metadata
  service, is checked by the Floating IP's loop once a configuration names the token
  (`confirm-server`). Only a host with a checked server holds the Floating IP
  (`set-ingress`).

### Credentials from a host

- **The cluster has no users,** just as kubeadm's has none: whoever holds a CLI certificate
  controls it. Every credential comes from root on a host, the first one too, like
  kubeadm's `admin.conf` and `kubeadm kubeconfig user`: `clowderd init` creates the
  cluster, then `ssh root@<host> clowderd credentials | clowder config import -`. The CA is
  trusted through SSH, so the first host's user data holds no secret, and no route except
  the join's is open without a certificate.
- **`clowderd credentials`** (`agent/credentials.go`) creates the CLI's key on the host and
  asks its own agent, with the host's certificate, for a certificate with the name from
  `--name` (`POST /v1/internal/credentials`, `api/credentials.go`). The agent opens the
  CA's key from the state with the data key, issues the certificate, and answers with the
  cluster's name and the public addresses of the hosts that are not removed. The command
  prints a CLI config with one context named after the cluster, with those addresses as its
  endpoints unless `--endpoint` names others. The key crosses SSH, as kubeadm's
  `admin.conf` does.
- **`--revoke-older`** also commits `set-cli-cutoff` as that host, with the new
  certificate's start time. From then on every agent refuses the CLI certificates that
  start before it (`Server.cliCutoffPassed`), including a stolen laptop's. It needs the
  leader. The certificate is returned only once the cutoff is committed and the host's own
  agent has applied it; the other agents follow within moments. Certificate times count in
  seconds, so a certificate issued in the same second as the new one stays valid.
- **`recovery rotate`** creates a recovery key with the CLI's credentials alone, because
  they control the cluster anyway. A new cluster has no recovery key, so this also creates
  the first one. The CLI creates the key and hands it over (`handOverRecoveryKey`: shown
  once, with its last 8 characters typed back, or written to a new file with
  `--recovery-out`, for scripts). Up to this point nothing has reached the cluster. Then
  `POST /v1/recovery/rotate` commits `set-recovery-recipient`. Values encrypted afterwards
  are for the new key, and `secret rekey` brings the files to it. Old ciphertext, for
  example in the Git history, stays readable with the old key.

### Tests

- The tests run real HTTPS servers: a follower forwarding to the leader, refused callers,
  and the CLI against an agent with a credentials file in a temporary home.
  `agent/run_test.go` runs the whole way: init, the first credentials printed and
  imported, the recovery key, new credentials with `--revoke-older`, apply and diff.

## Placement (`internal/scheduler`, `internal/controller`)

- `scheduler.Plan` reads the state and returns its decisions: what hosts over capacity push
  out, where pending stacks go (highest priority first), which stacks move (`Move`), how
  the volumes' copies change (`Copies`), why stacks wait, and which pushed-out service may
  come back. A stack or volume that an operation holds is left to that operation. A volume
  is placed with the first stack that names it, on that stack's host, with one copy, and
  must fit its pool there (`Fits`). Without overcommit, the pool's volumes and the new one
  must stay within the pool minus its reserve, including a class's fallbacks. Volumes,
  DRBD minors and room are taken only for a stack that fits.
- **Copies** (`copies.go`, `keepCopies`) change by one change per volume and pass.
  - A missing copy goes to the host that takes stacks and has the most free space in the
    pool; a tiebreaker on that host gets a disk.
  - A copy beyond `replicas` is removed once the others are synced, the one on a leaving
    host first, but never the one where its stack runs.
  - A volume with two copies gets a tiebreaker (diskless) on a host without a copy, the
    one with the fewest, once three hosts take stacks.
  - **Quorum while copies change** (`fitTiebreaker`): the change that gives a volume a
    second copy with a disk, or takes it away, adds or removes the tiebreaker in the same
    record. Two copies without their third vote lose DRBD's quorum if one fails (one of
    two nodes). The copy that a move gives up becomes the tiebreaker on its host, so no
    node leaves the resource: its host detaches the disk and removes the LV, whose pool
    the copy's class still names. A volume of replicas 1 has a tiebreaker while a move
    gives it two copies.
  - A copy on a host that is leaving does not count; a copy on a draining host does.
  - **The resync limit:** a new copy with a disk starts only while no other copy syncs, in
    the whole cluster, so each copy resyncs at `replication.resyncRate` and no link
    carries more. A move whose copies are not yet synced counts as that one resync, even
    though it syncs the copies of all its stack's volumes at once.
  - A volume may have one copy more than `replicas` (`checkVolume`): the one that a move
    or a leaving host leaves behind, until `keepCopies` gives it up. A volume of replicas 0
    that no stack runs with, on a leaving host, starts anew, empty, on another host
    (`leaveThrownAway`).
  - **After a failure** (`counts`): a copy on a failed or fencing host counts, because the
    leader has not decided yet. A copy on a fenced host counts for `CopyWait` after the
    fence, because the leader powers it on again. A copy on a recovering host counts while
    it catches up. A copy on a disabled host, or on one fenced for longer, is created anew
    elsewhere. Once the host is back, the surplus copy is removed, starting with the copy
    that is not synced. Only a copy on a host that runs (active, draining, recovering)
    takes the resync limit; a copy on a host that is down waits for it.
- **Failover** (`failover.go`): a stack whose host is fenced or disabled
  (`state.FailsOver`) gets another host at once, with `set-stack-status` and no move. The
  old host is off and stops nothing, and the state allows such a change of host without an
  operation (`checkLeaderStatus`), but only to an active host. The new host must have a
  synced copy with a disk of each of the stack's volumes of replicas 1 or more, because
  nothing can sync from the host that is down. A volume of replicas 0 starts anew there,
  empty, in a new data generation. Of those hosts, the best fit wins, as for any placement.
  A stack that finds no host waits on its old host, with the reason in `get stacks` and the
  alert `stack-waiting` ("is down"). With replicas 1 it waits until the host is back (or,
  once phase 3 brings restores, until a user restores the volume elsewhere). A failed
  host's stacks stay where they are until it is fenced.
- **Room, one measure for everything.** A host offers what it reported (memory, CPUs) minus
  `systemReserve` (default 512M, 0.25). Each service of a stack on it that is `starting` or
  `running` takes its reservations (`resources.reserve`, default `defaults.reservations`,
  128M and 0.1), unless it is pushed out. Nothing measures what containers really use. The
  state keeps what the scheduler needs of each service (`Stack.Services`: the priority as
  written, the reservations, `dependsOn`).
- **Priorities.** A service's effective priority is the lowest of its own priority (or the
  stack's) and those of everything it depends on. Stopping a service stops what depends on
  it, so a running service never depends on a pushed-out one. Categories are resolved with
  the current configuration at every pass (`Priorities.Resolve`), so a changed category
  takes effect without a new apply. Pending stacks are placed by the priority of their
  highest service. Only `dependsOn` within a stack counts. A service that another stack
  imports (with imports, phase 4) may be pushed out, and that stack loses it as it would
  lose any service it does not own.
- **Waiting.** `Stack.WaitingSince` is set with the first wait reason and kept while only
  the reason's text changes. It is in the state, so the alert for a stack that waits too
  long survives a change of leader.
- **Placing** (`place`) tries, in this order:
  1. a host that has the stack's data and where all of the stack fits, choosing the one
     with the least memory left;
  2. a host where all of it fits once its volumes get copies there (a move, within the
     resync limit);
  3. for a stack without a host, room made by moving stacks of lower priority to hosts that
     have their data (`makeRoom`; not a stack moved within `MoveCooldown`);
  4. last, a host where it fits by pushing out (`fitStack`).

  The same order picks the new host of a stack that moves: one that a user asked to move
  (`Stack.MoveTo`), or one on a draining host. A stack moves with a move-stack operation
  ([Cluster state](#cluster-state-internalstate)). While it moves, its room counts on the
  new host, and on the old host until it has stopped there (`stillOn`). A move to the host
  the stack already runs on cancels a user's request, and a request to a host that no
  longer takes stacks is dropped with that host's phase change. Pushing out works like
  this:
  - Its services go one by one: highest effective priority first, and at equal priority,
    the services that others depend on first.
  - Each takes free room, or pushes out services of any stack below its own priority,
    lowest first, each together with what depends on it.
  - A service that still finds no room starts pushed out, together with what depends on it
    within the stack. The stack waits only if its first service finds no room.

  An example. A host offers 1 GiB and runs `blog`: one service at priority 500 that
  reserves 600 MiB. A new stack `shop` arrives: `api` at 800 and `cache` at 200, each
  reserving 300 MiB.
  1. `api` goes first, because it has the higher priority. It takes 300 of the 400 MiB
     free.
  2. `cache` needs 300 MiB, and 100 are left. It may push out only services below its own
     200, and `blog` is at 500. So `cache` starts pushed out, and `blog` keeps running.

  A service is only as strong as its own priority: `api` at 800 does not let `cache` push
  out `blog`. The new stack's list of pushed-out services is written afresh at every
  placement, so a list left by an earlier leader is replaced.
- **Over capacity** (after a raised `systemReserve`, or a stack whose new generation
  reserves more): services below the host's highest effective priority are pushed out,
  lowest first, until the rest fits. Services at that highest priority are never pushed out
  for this. If that is not enough, nothing is pushed out, and the leader raises the alert
  `over-capacity`.
- **Coming back.** A pushed-out service whose dependencies all run comes back once its
  reservations plus 5 % fit its host's free room for 60 s. The highest effective priority
  goes first, but a service that does not fit does not block a smaller one. There is one
  service per decision, because its return changes the room, and only in a pass that
  places and pushes out nothing, so a waiting stack gets room before a pushed-out service
  does. The 60 s are kept in the leader's memory and start again after a change of leader.
- `controller.RunPlacement` runs on every agent but acts only on the leader. It plans on a
  copy of the state (`SharedCopy`), outside the lock that Raft's `Apply` waits for.
  - It first writes the push-outs of hosts over capacity. If one fails, it places nothing
    in that pass, because the placements count on the room it frees.
  - For each placement it writes what the stack pushes out (`set-pushed-out`, one command
    for every stack it touches), then the volumes (`put-volume`), then the stack's host
    and `starting` (`set-stack-status`). If the leader changes in between, the services
    stay pushed out without the new stack and come back by the rule above. The stack's
    host does the rest.
  - Each command names the state the pass planned with (`PlannedAt`, starting at the copy's
    index). A command of the pass that directly follows the planned state (index + 1)
    becomes part of it, so the next command may change what that one changed. When
    something else came in between, the state refuses a command about what that changed
    (`ErrStale`, logged as info), and the next pass plans again.
  - After the placements it starts the moves (`set-pushed-out` on the new host first, then
    `start-operation`) and writes the copies' changes (`put-volume`).

  `controller.RunMoves` runs the leader's steps of each move and watches their deadlines;
  `agent/moves.go` records a host's steps once its loops have done them.
- **On the host** a pushed-out service is left out of `compose up` (`--scale <service>=0`,
  which also removes its running container), and the supervisor's rule for it is "no
  restart". Only the leader changes `Stack.PushedOut`; a host's status report no longer
  carries it. A new generation drops services it no longer has. Whether compose up leaves
  the rest running is checked by the live tests, because `bin/dev test-tools` has no Docker
  daemon.
