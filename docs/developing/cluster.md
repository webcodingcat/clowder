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
- **Commands.** A command has a kind, a payload of the type with the same name (`AddUser`,
  `PutResource`, ...) and an actor (`leader`, `host/<id>`, `user/<name>`) that the API takes from
  the caller's certificate. Which actor may send which kind is the API's check. The state
  checks only what it alone can tell: a host reports only about itself and never moves
  another host, and about a stack only while it runs there, without moving it.
- **The commands in use so far,** by who sends them:

  | Who | Commands |
  |---|---|
  | `clowderd init` | `bootstrap` |
  | `clowderd recover` | `recover-remap` |
  | the user | `apply-config`, `add-user` (the bootstrap's user), `recover-user` and `set-recovery-recipient` (with a code from a host), `set-host-phase` (host actions), `add-join-token`, `put-resource` and `delete-resource` (`/v1/apply`, `/v1/delete`; the leader decides), `restart-stack`, `request-move` |
  | the leader, for a joining host | `add-host` |
  | the members loop | `set-host-phase` (joining, passive, active; removed for a host that left), `set-host-voter` |
  | the placement, the stack's host | `put-volume`, `set-stack-status`, `set-stack-wait-reason`, `set-pushed-out`, `start-operation` (a move) |
  | the moves loop | `put-volume`, `set-stack-status`, `advance-operation`, `extend-operation`, `roll-back-operation`, `finish-operation` |
  | the Floating IP's loop | `confirm-server-ref`, `set-ingress` |
  | the ACME loop | `put-acme-account`, `put-acme-challenge`, `delete-acme-challenge`, `put-certificate`, `delete-certificate` |
  | the fencing loop | `set-host-phase` (failed, active again; fenced or back to failed for its operation; disabled), `start-operation`, `advance-operation`, `finish-operation` (a fence) |
  | the alerts | `put-alert`, `resolve-alert` |
  | each agent | `report-host`, `forget-copy`, `set-volume-initialized`, `set-copy-synced`, `report-peer-forgotten`, `advance-operation` (a move's step on the host), `set-host-phase` (its own: recovering, active) |

  Every kind has a sender: a kind comes with the phase that sends it (backups later),
  not before.
- **Resources** (phase 1c): namespaces (`Namespaces`), and by `<namespace>/<name>` stacks
  (`Stacks`), volumes (`Volumes`) and secrets (`Secrets`), each with a `Meta`. The kinds
  and their specs are in `internal/resource` (a stack's in `internal/stackspec`), which the
  CLI and the API share. Three counters, each with one job:

  | Counter | Counts |
  |---|---|
  | `Meta.Generation` | the changes of a resource's spec |
  | `Meta.ResourceVersion` | the index of the command that last changed the resource, spec or status |
  | `Volume.DataGeneration` | a volume's data: each new, empty LV (`-g<N>`) gets the next one, as when a volume of replicas 0 moves, fails over or starts anew after a recover; restores (phase 3) will too |

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
  - **A stack's status.** A new stack is `pending`. A changed one that runs or fails is
    `starting` until its host reports the new generation up. A host's `set-stack-status`
    names the generation and the restart times it ran with; one about an older generation
    or other restart times is refused, so a report from before an apply or a restart cannot
    mark the stack `running`. With `running` the host reports the digests of the secrets
    its containers came up with (`AppliedSecrets`). Only the host reports `failing`, always
    with why (`UpError`, display only); the digests stay, as containers of an earlier
    `compose up` may still run with them. A failing stack keeps its host and its room.
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
  - **Planned on a state.** The leader plans on a copy of the state and commits after, and
    a user's apply may come in between. So its `set-stack-status`, `set-pushed-out` and
    `put-volume` name the index of the state they were planned with (`PlannedAt`), and
    the handler refuses a change to a stack or volume whose `ResourceVersion` is higher
    (`checkPlanned`, `ErrStale`): changed since, or deleted and made anew under the same
    name. The placement then plans again. A host's reports name no plan: they are held to
    the stack's generation, restarts and host instead. The leader places only a stack that
    has no host yet; moving a placed one is for an operation that holds its lock.
- **Operations and locks.** An operation is a long task in steps, such as moving a stack
  or fencing a host. The leader starts it (`start-operation`) and takes its locks
  (`stack/<id>`, `volume/<id>`, `host/<id>`), each on something the state has; another
  operation that holds one of them makes the start fail. Its ID is the index of the start.
  Whoever owns the current step, a host or the leader, records it done
  (`advance-operation`) and names the next owner; the command names the step it saw last
  (`FromStep`), so a step told twice, after a slow host or a change of leader, is refused.
  The leader finishes it (`finish-operation`), which frees the locks; the state keeps the
  last `keptFinished` finished ones. While an operation holds a lock, every handler that
  changes a stack, a volume or a host's phase refuses the change (`checkUnlocked`), unless
  the leader makes it for that operation (`Operation` in `set-stack-status`, `put-volume`
  and `set-host-phase`). A host's reports mind no lock: `set-stack-status` from the host
  tells what runs where, which the operation waits for, and `set-volume-initialized` what
  a copy holds. A table test holds every kind of command to this
  (`TestEveryKindOfCommandIsCheckedForLocks`): a new kind needs a case or a reason.
  Each step has a deadline (`Operation.Deadline`, `StepTime` unless the sender gives one);
  the leader gives a step that gets on more time (`extend-operation`) and turns one back
  before the kind's point of no return (`roll-back-operation`): the operation is then
  `rolling-back` until the leader has undone what it did and finishes it as failed.
- **Fencing a host** (`fence-host`, `HostFence`; the fencing loop starts and drives it,
  loops.md, Fencing). It locks the host, and its start makes the host `fencing` in the
  same command. Its steps: `FenceStart`, then `FenceOff` once the server is off and the
  host fenced. It has no rollback: if the server cannot be powered off, the operation
  moves the host back to failed and fails. The leader finishes it once the stacks the host
  had (`HostFence.Stacks`, recorded as it is fenced) run elsewhere, or after `StepTime`.
  Its lock is what keeps fencing to one host at a time.
- **Moving a stack** (`move-stack`, `StackMove`; the placement starts it, `controller.RunMoves`
  drives it). It locks the stack and its volumes.
  - Its steps, each the last one done: `MoveCopies` (the leader placed a copy of each
    volume on the new host), `MoveSynced` (the new host has them synced,
    `VolumeCopy.Synced`, and the images pulled), `MoveStopped` (the old host stopped the
    stack and its copies are secondary), `MoveHost` (the leader set the stack's host; a
    volume of replicas 0 gets a new data generation there, empty). The leader finishes it
    once the stack runs on the new host.
  - The hosts do the work through their loops: from `MoveSynced` on, `StopsForMove` takes
    the stack off its old host's runner and reconciler, and the new host's reconciler
    promotes once the stack is placed there. `MoveStopped` is the point of no return.
  - The deadline of `MoveCopies` allows the resync at `replication.resync_rate` twice
    over. Once it passed, the leader asks the new host how much is left
    (`/v1/internal/volumes/progress`) and extends while that shrinks.
  - A move that ends answers a user's `request-move` (`Stack.MoveTo`), worked or not. One
    that moved records `Stack.LastMove`; one that failed `Stack.MoveFailed`, and within
    `MoveCooldown` the placement starts no move of its own for that stack again (a user's
    request still goes), so a move that cannot work does not resync copies over and over.
  - A host of the move that goes down (no longer active or draining) ends it at once
    (`abandon`): before `MoveStopped` it rolls back, after it the move fails, and either
    way the stack keeps its old host. From `MoveHost` on, a new host that goes down ends
    it as failed, and the failover takes the stack.
  - A running stack that cannot move gets the alert `stack-waiting` with "cannot move".
  - To follow one through the code: `scheduler.place` decides it, `controller.startMove`
    starts it, `controller.mover.placeCopies` (with `scheduler.MoveVolumes`) runs the first
    step, `agent.moveSteps.synced` and `stoppedHere` record the hosts' steps,
    `controller.mover.setHost` the leader's last one, and `controller.mover.late` minds the
    deadlines.
- **Retired copies.** A volume copy's data goes only when the state says so:
  `delete-resource` of a volume, and a `put-volume` that changes the data generation or
  takes a copy off a host each add the copies they give up to `Retired` (volume, data
  generation, host). The host removes those and forgets each with `forget-copy`; the
  copies of a removed host go with the host. A copy that is merely missing from the state
  stays: a bug in the placement must not cost data. A retired copy keeps its volume's DRBD
  minor taken (`RetiredMinors`) until every host forgot its copies: the resource on the
  host has the minor, and drbdadm refuses every command while two resources share one.
- **Initialized volumes.** `Volume.Initialized` says that a copy of the current data
  generation was primary with its file system. The first host that has its copy so tells
  with `set-volume-initialized` (only a host with a copy with a disk in that generation).
  Until then a host may promote with `--force` and format; after that never, on any host:
  a copy without a file system then lacks the data, it is not new. `put-volume` keeps the
  flag within a data generation and clears it with a new one (a new LV).
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

  O is the fence-host operation (`OperationFenceHost`, `HostFence`), U a user (the state
  allows the leader these too, `byLeaderOrUser`; the API lets a user make only these).
  Each move checks the phase it starts from (`From`), so of two that race only the first
  passes.

  | From | To | Who | When |
  |---|---|---|---|
  | `joining` | `passive`, `active` | members loop | the host reported; passive while it is the second |
  | `passive` | `active` | members loop | the third host is there |
  | `active` | `draining` and back | user | `host drain`, `host undrain` |
  | `active`, `draining`, `recovering` | `failed` | fencing loop | a majority of the voters has not heard it for `FailAfter` |
  | `failed` | `active` | fencing loop | a majority heard it again within `backWithin`; never the host itself |
  | `failed` | `fencing` | the operation's start | `start-operation` of a fence, in the same command; only a failed host with a checked server ref and a stack another host can take (`needsFence`) |
  | `fencing` | `fenced` | the operation | its server is off: the time joins `FencedAt`, the host's copies are no longer synced, the operation records the host's stacks |
  | `fencing` | `failed` | the operation | `PowerOff` failed `powerOffTries` times; the operation fails |
  | `failed` | `fenced` | user | `host fence-confirm`, after the user checked the server is off |
  | `fenced` | `recovering` | the host | its agent runs again and caught up with the leader |
  | `recovering` | `active` | the host | every copy of it with a disk is synced (`checkCaughtUp`) |
  | `fenced` | `disabled` | fencing loop | fenced `FlapLimit` times within `FlapWindow` (`Host.Flapping`) |
  | `disabled` | `fenced` | user | `host enable`: clears `FencedAt`; the leader powers it on |
  | any | `removed` | `clowderd recover` | `recover-remap`: every host but the one that recovers (Recover below) |

  In words: a new host joins as `joining` (`add-host`); the members loop makes it
  `passive` (waiting) or `active` (running stacks), and a user drains it for maintenance
  and back. A drain moves the host's stacks off it and keeps new ones off; its volume
  copies stay, as the host comes back after the maintenance. A host the voters stop
  hearing is `failed`; the leader powers it off before any of its stacks start elsewhere
  (loops.md, Fencing). A host can never leave `failed` by itself: it cannot tell whether the
  leader is about to fence it, and while a peer promotes its volume it would write on
  (split-brain). Once a fence started, nothing stops it, a late beat neither: `fencing`
  has no move back to `active`, and only its operation moves it on
  (`checkFenceDecided` keeps the operation from ending while the host is still fencing).
  While an operation holds `host/<id>` (fencing, drain, remove), only it moves the host,
  and the host's own reports of a phase are refused too. `KeepsStacks` says which phases
  run the stacks placed on the host: a failed or fencing host goes on, since no other host
  runs them before it is off; a fenced, recovering or disabled one runs none.

  What each phase means elsewhere, in one place (Y: yes; the functions decide):

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

  A failed host's copies are no longer synced (`unsync`): the others went on without it,
  so until DRBD caught them up and the host says so, no stack fails over or moves to it.

  **Removing** (`checkRemoval`; the CLI asks first) refuses the last host that runs stacks
  (`active` or `draining`), and a host that runs stacks while a stack or a volume copy is
  on it. A user who removes such a host makes it leave instead
  (`SetHostPhase.Leaving`, `ErrNotEmpty`): it drains, the placement moves its copies off
  too (they no longer count), and the members loop removes it once nothing is left on it;
  an undrain takes it back. Once `MinHAHosts` hosts ran stacks, the state is `HA` for good, and no removal may
  leave fewer than three that run stacks: a replacement joins first. A removed host's
  certificate is revoked, and every other host forgets it as a DRBD peer and reports it
  (`report-peer-forgotten`); `Forgetting` holds who has not yet. Only then is its ID free
  for a host that joins (`HostIDFree`), as DRBD's node-id is the host ID and a peer's slot
  in the metadata outlives the host. A host that is down reports nothing, so the ID waits
  for it; once it is removed too, nobody waits for it any more. The removal does not take
  the data key away: a removed host that kept its disk keeps the CA's key and the
  secrets (roadmap, Known gaps).

- **Versions.** Every agent must apply every entry of the log, or the states drift apart
  for good (a `revoke` left out, and a certificate goes on working there). So:
  - **Fail-stop.** An entry the agent cannot read (not JSON, a `CommandVersion` it does
    not read, a kind it does not know) is no refusal: `Apply` leaves the state and its
    index as they are and returns `state.ErrUnreadable`; so does `ReadSnapshot` for a
    snapshot it cannot read. The fsm halts (`raftnode.Node.Halted`): it applies,
    restores and snapshots nothing more. The agent ends with exit status 3, which
    systemd does not restart (Shipping), and the other hosts see it fail. A refusal
    (`CommandError`, and `ErrNotBootstrapped`) is the same on every agent of a version,
    so it moves the index on.
  - **The gate.** Each kind names the release that brought it (`commandKinds`).
    `Node.Propose` commits a kind only once `State.MinVersion`, the oldest release an
    agent reported, includes that release (`buildinfo.Includes`: `0.2.0~rc1` includes
    `0.2.0`); else the command fails with `state.ErrNotReadByAll`, no refusal, and its
    caller tries again later. Development builds read every kind of their code and do
    not count, nor do hosts that have not reported a version yet (a host that joins);
    fail-stop catches what slips through.
  - **Until `v1.0.0`** the state, the commands and `cluster.yaml` change freely, with no
    migrations: only agents and CLIs of the same minor version work together
    (`buildinfo.Compatible`), and a patch release changes neither the state nor the
    commands. `CommandVersion` and `SnapshotVersion` stay at 1, so fail-stop and the gate
    catch a new kind, not a payload whose fields changed their meaning: that is why
    different minor versions must not run side by side. A cluster is updated host by host
    only within a minor version (Shipping, Updating a cluster).
  - **From `v1.0.0`** a release reads its own `CommandVersion` and `SnapshotVersion` and
    the one before, and CLI and agents work together across one minor version. Payload
    fields are added, not changed. Unknown fields are ignored, so an agent of the release
    before drops a new field from its state; a field that later commands depend on
    therefore needs a gate of its own on `MinVersion`.
  - The json tags of `clusterspec.Config` are part of this format.
- **Invariants** the state holds itself, as the last place to catch a wrong command: one
  user, valid names (`internal/names`), volumes with 0 to 3 replicas and a DRBD minor
  exactly when replicated (a pending volume has neither copy nor minor), one copy per host,
  one stack per volume, nothing deleted that a stack names, a removed host's certificate
  revoked, no name, underlay, server ref or certificate of a host twice among the hosts
  that are not removed (`CheckJoiningHost`), and every joined host's server ref checked
  with the hoster.
- **Revocations.** `Revoked` holds each revoked certificate with when it expires, which the
  state knows from the command that recorded the certificate (`AddUser`, `RecoverUser`,
  the host's `CertificateNotAfter`); `revoke` puts it there. The list grows only there, so
  `revoke` also drops the certificates that expired more than `revokedKept` before the
  command: no handshake takes them any more, and the margin covers a host whose clock runs
  behind. It goes by the command's `At`, so every agent drops the same.
- **Secrets** arrive sealed (`Sealed`, age for the data key); `state` never sees them in
  clear. Those of `cluster.yaml` are `ClusterSecrets`, by name (`clusterspec.SecretPlaces`),
  each with a digest; the namespaces' are `Secrets`. Readers find a value through the
  spec, never by taking a name apart: `ClusterSecret(config.Hoster.Token)`,
  `RegistryLogins` (the user from the spec, the password sealed). Until the first
  `apply-config` the config is revision 0 with an empty spec; the loops that need a hoster
  or a Floating IP find none and wait. init's bootstrap key is `BootstrapKey`, sealed,
  until `add-user` drops it. A stack names at most `MaxStackSecrets`. Who can read a
  secret: API, below.
- **Join tokens** (`internal/jointoken`): an ID and a 256-bit secret. `add-join-token`
  keeps only the secret's hash under the ID, with when it expires: the command's `At` plus
  its TTL (`MinJoinTokenTTL` to `MaxJoinTokenTTL`, at most `MaxJoinTokens` open).
  `add-host` checks the token against its own `At` (`CheckJoinToken`) and deletes it in
  the same command, so a token opens once; `dropExpiredTokens` drops the expired ones a
  while after (`joinTokenKept`), so a late join hears that its token expired.
- `Copy` gives a deep copy without the audit, for work too slow for the state's lock (the
  placement weighs every stack) or that keeps parts of the state (the agent's stacks). The
  loops do not copy themselves: `raftnode.Node.SharedCopy` makes one copy per change, which
  every loop reads and none changes; a dry run, which applies commands to its copy, makes
  its own.
- Snapshots are gzip-compressed JSON (`WriteSnapshot`, `ReadSnapshot`). The audit keeps the
  last `auditSize` commands.

## Certificates (`internal/pki`)

- The cluster CA issues host certificates (server and client, valid for the host's addresses)
  and user certificates (client). The role is the certificate's organizational unit, the
  name its common name (`host-<id>` or the user's name).
- Keys are Ed25519 and never leave the machine that made them: the CA signs public keys.
- Certificates end by revocation, not expiry: hosts may stay off for months. `Verify` takes
  the revoked serials from the state, which keeps each until a while after it expired.
- `TrustProof` (HMAC-SHA256 over the CA certificate) is how a CLI or a joining host trusts
  the CA on first contact: with the bootstrap key, a recovery code, or a join token's hash
  (`jointoken.ProofKey`). A bootstrap key a user chose has at least
  `MinBootstrapKeyLength` characters, as the offer lets anyone try keys offline until the
  bootstrap.
- `tls_test.go` puts both TLS configs before every peer they must refuse: none, a user, a
  host of another CA, a stranger showing a host's certificate behind its own, a revoked, an
  expired and a not yet valid host; and runs handshakes on 127.0.0.1.

## Raft (`internal/raftnode`) and the data key (`internal/datakey`)

- `raftnode` runs hashicorp/raft: log and stable store in BoltDB, a snapshot every
  `snapshotThreshold` entries, the state of `internal/state` as the replicated machine.
  Raft's own log lines go through `log/slog`.
- Raft speaks mutual TLS on the node address: both sides show a host certificate of the
  cluster CA that is not revoked. Users' certificates and other clusters' hosts fail the
  handshake, and removing a host revokes its certificate, so it leaves Raft and the API at
  once. `pki.ClientTLSToHosts` and `pki.ServerTLSForHosts` are the one way hosts are checked,
  by Raft and by the API alike: the chain and the role, not the name.
- **Leading starts with a barrier.** `followLead` sets `IsLeader` only once Raft's barrier
  has applied every entry before the lead: a new leader may not have applied what its
  predecessor committed, and its loops and `runOnLeader` would decide on an old state.
  `VerifyLeader` asks a majority whether this member still leads; the leader asks before
  it powers a server off or moves the Floating IP, as one cut off from the others believes
  it leads until its lease runs out.
- **Catching up.** `CatchUp` waits for a leader (and on the leader for the barrier). A
  follower then asks the leader's commit index (`api.Server.CatchUpWithLeader`,
  `/v1/internal/commit-index`, answered after `VerifyLeader`) and waits until its state
  holds it (`WaitCommitted`): Raft's own entries never reach the state, so it waits for
  Raft to have applied the index and then for the last command up to it. The agent starts
  its stacks and volumes only after that (host.md, The agent): a host that was away must
  not start a stack that moved meanwhile. Until then the API holds users' calls too, for
  `waitTimeout` at most (`ServerConfig.CaughtUp`): a state that has not caught up may
  still be empty and answer that the cluster is not bootstrapped. The hosts' own routes do
  not wait, as the catch-up, the beats and the joins use them.
- `Propose` commits a command through the leader and returns its index, or the state's
  refusal. `Read` gives the current state under a lock; nothing may keep it, as `Apply`
  changes it in place. `WaitIndex` lets a read follow a write made through another member.
  `Changes` answers the watch from the member's ring of changes (API, The watch).
- `Bootstrap` makes the first host a cluster of its own; on a member with a log it does
  nothing. The leader changes the members one at a time (`members.go`: `AddNonvoter`,
  `AddVoter`, `DemoteVoter`, `RemoveMember`, `TransferLeadership`), from the members loop
  (`controller/members.go`). Each pass makes one change, in this order
  (`nextMemberStep`):
  1. A leader that is removed hands its lead over. The API does that before it commits
     the removal (`handOverLeadOf`), as the removal revokes the leader's certificate. A
     removal the state then refuses has moved the lead for nothing, which does no harm.
     The loop is the fallback for a removal the API did not see coming: the others take
     no new connection of a revoked leader, but Raft's open ones go on, and so would its
     lead.
  2. Every host that is not removed is a member; a joining host joins as a non-voter. A
     joining or passive host that votes loses its vote: one that took the ID of a
     removed host before that left Raft finds the old host's vote.
  3. A joining host whose agent reported (`report-host`) becomes `passive` while it is
     the second host of a cluster that is not `HA` yet, else `active`. A passive host
     becomes active once there are three.
  4. Of the hosts that run stacks or are to come back after a failure (`votes`: active,
     draining, failed, fencing, fenced, recovering, disabled), 1, 3 or 5 vote
     (`voterCount`): those that voted already, then the lowest IDs. Only an active or
     draining host gets a vote; a failed voter keeps its own, as taking it would take a
     live host's instead (with three, the cluster would stop at the next failure). The
     leader keeps its own vote.
  5. A removed host leaves Raft, after step 4 gave its vote to another.
  6. `set-host-voter` mirrors each host's vote in the state, which shows it.

  Raft changes its members one by one, so the third host's join passes through seconds
  with two voters, in which the loss of either stops the cluster until it is back. The
  leader takes one join at a time (`joiner.mu`), so two hosts never get the same ID.
- The data key is an age identity in a file only root reads. `Seal` and `Open` turn secrets
  into `state.Sealed` and back; joining hosts get the key with their join answer.
- `ReadState` reads the state from the log on disk without Raft (the last snapshot, then
  the commands after it), and `Recover` makes the member its cluster's only voter
  (`raft.RecoverCluster`): both for `clowderd recover`, while the agent is stopped.
- The tests run real members on 127.0.0.1: a restart from disk, three hosts over mTLS,
  strangers refused, a member that recovers alone.

## Recover (`clowderd recover`, `agent/recover.go`)

A cluster that lost the majority of its voters for good decides nothing; the one host left
still has current copies of its volumes. `clowderd recover --power-off` makes it go on
alone, only on a human's word. The danger is the hosts that went: switched on again, two
of them would still have the old Raft configuration's majority and their copies DRBD's
quorum, elect a leader and fence the host that recovered. So `Recover` takes them out
at the hoster, in this order, each step safe to run again after a failure:

1. It stops `clowderd.service` and reads the state from the log on disk
   (`raftnode.ReadState`): the hosts to retire are every one but this that is not removed.
   A cluster of one host that never recovered has nothing to recover.
2. It asks each of them over its underlay whether it knows a leader (`api.KnowsLeader`,
   `/v1/internal/leader`: a leader once a majority confirmed it, a follower the one it
   hears) and refuses if one does: that cluster is not lost, even when this host reaches
   only a follower of it, and its hosts leave with `host remove`. It refuses too if one
   answers without saying (a refused certificate: the others may have removed this host;
   an answer that does not come in time); only a host that takes no connection at all
   (`api.ErrNotReached`) counts as gone, and step 3 names it.
3. It shows what the remap will do, worked out on a copy of the state, and asks for the
   cluster's name (`--yes` for scripts). Until here nothing changed, and a refusal starts
   the agent again.
4. Per host: the label `clowder-retired` with the serial of the host's certificate
   (`hoster.SetRetired`), then the power off, both confirmed by the hoster; the label
   first, as a server that is off but not labeled could come back. A server ref the
   leader never checked is checked first.
5. Raft with this host as the only voter (`raftnode.Recover`); the member opens on its own
   and commits `recover-remap` as the leader. The API forwards no such command.
6. DRBD goes on without the other hosts (`storage.Driver.DropPeers`): `del-peer` for each,
   then quorum off (DRBD refuses that while a peer is configured), then `resume-io` for a
   resource whose writes were suspended. Phase 0 showed this order. Quorum is off only
   until the agent adjusts the resource to its file, which now names this host alone.
7. It starts the agent. Its loops do the rest: the mesh drops the removed hosts, the
   Floating IP comes here, the placement places what waits, and `forgetPeers` runs
   `forget-peer`.

`recover-remap` (`state/recover.go`) makes every other host `removed` (its certificate
revoked, its ID free once this host forgot it) and fits the rest to what this host holds:

- A volume of replicas 1 and more keeps a synced copy with a disk here. One without waits
  for a restore (`VolumeRestorePending`): the placement gives it no copy, and its stack
  waits with the reason. A tiebreaker of it here is retired; an unsynced copy here stays a
  copy, as the state never removes what may hold older data: `get volumes` shows it, and a
  delete of the volume removes it. Until restores come (phase 3), deleting the volume and
  applying it anew is the way out.
- A volume of replicas 0 whose LV was on another host starts anew, empty, as after a
  failover.
- Every stack not on this host, or that names a volume without data, is placed anew;
  every running operation ends as failed and frees its locks.
- This host is active, the only voter and the ingress host. `HA` is false again, so the
  cluster grows as a new one does (the second host is passive), and `RecoveredAt` raises
  the alert `no-ha` until three hosts run stacks.

A retired host's agent ends at its start (host.md, `checkRetired`): the label names its
certificate. A server set up anew gets a certificate of its own and starts, label or
not. Should the hoster's API not answer at the start, a retired host starts as before
(a known risk, roadmap): recover tells the user to set such servers up anew or delete
them.

## API (`internal/api`) and the CLI (`internal/cli`)

### Requests and the leader

- **Every call is checked.** Every agent serves `/v1` with mutual TLS (`agent.APIPort`), and
  each call is checked against the CA and the state's revocation list, so a revoked
  certificate is refused at once on every agent. User routes take user certificates,
  `/v1/internal/*` only host certificates. Every request body is limited
  (`MaxRequestBytes`, its comment works out why 16 MiB); a larger one answers 413, and
  `apply` then advises to apply the namespaces' directories one at a time.
- **Any agent answers.** Reads come from its own copy of the state; writes it commits as the
  leader or forwards to the leader's `/v1/internal/propose`. A write answers with its Raft
  index (`X-Clowder-Index`); the client sends it on with later reads (`X-Clowder-Min-Index`),
  and the agent waits until it has applied it. Without a leader (an election, a peer that
  restarted, a new leader still at its barrier) every way to the leader (a command, the
  batch of an apply, a join, the catch-up) waits up to `leaderWait` for one
  (`waitForLeader`) instead of failing at once.
- **Who speaks.** The actor of a command is the caller of the first agent; its time (`At`) is
  the leader's clock. `/v1/internal/propose` takes two things only: a user's command that a
  follower forwards (`userKinds`, exactly the kinds the user routes, the bootstrap and the
  recovery make),
  and a host's report about itself (`hostKinds`); `/v1/internal/changes` takes a user's
  resource commands only (Resources, below). The leader's own commands
  (placements, the ingress) are made on the leader and never come in from outside; a host never
  speaks for another host or for the leader.
- **A host certificate may speak for any user.** The user's certificate ends at the first
  agent, so the leader cannot check that a forwarded command really came from the user it
  names: any host certificate may send a user's command with any user as its actor. This
  is on purpose. A host holds the data key and the whole Raft state, and root on a host can
  make a recovery code for any user anyway, so a host is trusted with everything. The checks
  on the internal routes catch an agent's bug, not a host that lies; what keeps strangers
  out is the CA, and removing a host revokes its certificate.
- A refusal of the state is HTTP 409 with its reason; a write that did not commit is 503.
- **JSON names.** What users write and read as resources (`resource.Object`, the specs and
  the statuses `get` shows) is camelCase, as in the files users apply; everything else
  (commands, the state, snapshots, the other bodies of the API, `cluster.yaml`) is
  snake_case.
- `api.HostActions` lists the user's host actions with the phases they start from; the server
  checks them and the CLI makes its subcommands from them.
- An agent refuses a caller, CLI or agent, whose version is not compatible with its own
  (`X-Clowder-Version`, `buildinfo.Compatible`: the same minor version before `v1.0.0`,
  then at most one minor version apart). Development builds and callers without a
  version talk to everything; the agent logs a warning once per version when a
  development build talks to another build.

### Resources

`changes.go` changes resources, `resources.go` reads them; the CLI's `apply`, `get` and
`delete` are under The CLI, below.

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
- **The leader decides** (`runOnLeader`): a follower hands the commands on as a batch to
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

### The CLI

- **The client** tries the seeds of `cluster.yaml` in turn and trusts any host certificate of
  the cluster's CA, whatever name the seed has. A read goes on to the next seed after any
  network error; a write only if it never left (`httptrace`), since one that was sent may
  have committed without an answer.
- **The CLI** finds the cluster directory (`--cluster`, `$CLOWDER_CLUSTER`, the working
  directory) and by its `name` the user's credentials, then talks to the cluster with the
  user's certificate. An agent whose certificate is of another CA than the credentials'
  is named as such (`foreignCA`: a cluster of the same name, or set up anew) instead of
  with x509's words alone: `cluster status|apply|diff|schema`,
  `host list|drain|undrain|remove`, `apply`, `get`, `delete`, `stack ps|logs|restart`.
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

### Secret values

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

### The first user and joining hosts

- **The user** has no certificate yet (`cluster bootstrap`). `GET /v1/bootstrap` gives
  the CA certificate with a `pki.TrustProof` made with the bootstrap key, and the CLI
  trusts the CA only if it matches its key (`api.FindCluster`). The offer also says
  whether the cluster has a user already (`bootstrapped`): then it has no proof, as the
  key opens nothing any more, and the CLI stops (`api.ErrBootstrapped`) before it makes a
  recovery key, so nobody holds two and does not know which counts. Else it makes the
  recovery key and hands it over: shown once with its last 8 characters typed back, or
  into a new file (`--recovery-out`, for scripts); nothing has reached the cluster yet.
  `POST /v1/bootstrap`, over TLS checked against that CA, carries the user's public key
  and the recovery key's recipient with an HMAC of the bootstrap key and a time
  (`bootstrapMaxAge`); the state keeps the recipient with the user (`add-user`). It
  works once: a cluster has one user, and further certificates come only through
  `user recover`. The CLI then writes the
  credentials (`clusterdir.WriteCredentials`), checks that the cluster's recovery
  recipient is the key it made, and writes `cluster.yaml` to fill in (`clusterdir.Create`).
- **A host joins** with a token a user made (`clowder host join-token`, `api/join.go`,
  `agent/join.go`). `GET /v1/join?token=<id>` gives the CA certificate with a
  `pki.TrustProof` made with the token's hash while the token is open, and the host trusts
  the CA only if it matches its token (`api.FindClusterByToken`). `POST /v1/join`, over TLS
  checked against that CA, carries the token itself, the host's name, addresses, server
  ref and public keys (its own key and WireGuard's; the private halves stay on the host),
  a nonce and the time. Any agent takes it and hands it to the leader
  (`/v1/internal/join`); the joining host waits longer for the answer than the leader
  may take (`joinClientTimeout`). The leader (`agent.joiner`) takes a request only for
  `joinMaxAge`, and each once while it leads (a nonce in its memory): little more than
  insurance, as only the cluster's agents see a request, over TLS, and whoever saw one
  saw its token. It compares the token's hash, needs the applied `cluster.yaml`, chooses the
  lowest free ID, issues the host's certificate, checks the host against the state
  (`CheckJoiningHost`), that the request comes from the host's underlay or public address
  (`JoinRequest.Source`, which the agent that took it sets from the connection), and
  its server with the hoster (`hoster.CheckServerRef`: the server has the host's
  addresses), and commits `add-host`. The hoster vouches for addresses, the source for
  the host that has them: without it, whoever holds a token could name another server
  of the project, which fencing would later power off. A refusal
  says why: only the holder of an open token's secret gets that far, and the token stays
  open for another try. The answer carries the certificate, the data key and the other
  hosts, for the host's first WireGuard interfaces (A host, Join).
- **The server ref** decides which server fencing powers off and where the Floating IP
  goes, and the hoster's token may act on every server of its project. A joined host's
  is checked as it joins; the first host's, which init takes from the metadata service,
  the Floating IP's loop checks once `cluster.yaml` brings the hoster's token
  (`confirm-server-ref`). Only a host with a checked one holds the Floating IP
  (`set-ingress`).

### Codes from a host

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
  A sender is its IPv4 address, or for IPv6 its /64. The bootstrap's two routes are limited
  the same way, with a count of their own: they too work with the data key for anyone who
  asks, and strangers at the bootstrap must not block a recovery.
- **`recovery rotate --code`** needs the user's credentials and their own code to rotate: stolen
  credentials alone cannot swap the recovery key. The CLI makes the new key and hands it
  over as the bootstrap does, then `POST /v1/recovery/rotate` commits
  `set-recovery-recipient`. Values encrypted afterwards are for the new key; `secret rekey`
  brings the files to it. Old ciphertext, in the Git history say, stays readable with the
  old key.

### Tests

- The tests run real HTTPS servers: a follower forwarding to the leader, refused callers, the
  CLI against an agent with a cluster directory and credentials in a temporary config
  directory.

## Placement (`internal/scheduler`, `internal/controller`)

- `scheduler.Plan` reads the state and returns its decisions: what hosts over capacity push
  out, where pending stacks go (highest priority first), which stacks move (`Move`), how
  the volumes' copies change (`Copies`), why stacks wait, and which pushed-out service may
  come back. A stack or volume that an operation holds is left to it. A volume is placed
  with the first stack that names it, on that stack's host, with one copy, and must fit its
  pool there (`Fits`: without overcommit, the pool's volumes and the new one stay within
  the pool less its reserve, a class's fallbacks included). Volumes, DRBD minors and room
  are taken only for a stack that fits.
- **Copies** (`copies.go`, `keepCopies`), one change per volume and pass: a missing copy
  goes to the host that takes stacks with the most free space in the pool (a tiebreaker
  there gets a disk); a copy beyond `replicas` goes once the others are synced, the one on
  a leaving host first, never the one where its stack runs; a volume with two copies gets
  a tiebreaker (diskless) on a host without a copy, the one with the fewest, once three
  hosts take stacks. **Quorum while copies change** (`fitTiebreaker`): the change that
  gives a volume a second copy with a disk, or takes it away, brings or takes the
  tiebreaker in the same record, as two copies without their third vote lose DRBD's
  quorum should one fail (one of two nodes). The copy a move gives up becomes the
  tiebreaker on its host, so no node leaves the resource: its host detaches the disk and
  removes the LV, whose pool the copy's class still names. A volume of replicas 1 has a
  tiebreaker while a move gives it two copies. A copy on a host that leaves does not count,
  one on a draining host
  does. **The resync limit:** a new copy with a disk starts only while no other copy
  syncs, in the whole cluster, so each resyncs at `replication.resync_rate` and no link
  carries more. A move that has not got its copies synced counts as that one resync, though
  it syncs the copies of all its stack's volumes at once. A volume may have one copy more
  than `replicas` (`checkVolume`): the one a move or a leaving host leaves behind, until
  `keepCopies` gives it up. A volume of replicas 0 that no stack runs with on a leaving
  host starts anew, empty, on another (`leaveThrownAway`).
  **After a failure** (`counts`): a copy on a failed or fencing host counts, as the
  leader has not decided yet; one on a fenced host for `CopyWait` after the fence, as the
  leader powers it on again; one on a recovering host while it catches up. A copy on a
  disabled host, or one fenced for longer, is made anew elsewhere, and once the host is
  back, the surplus goes: the copy that is not synced first. Only a copy on a host that
  runs (active, draining, recovering) takes the resync limit; one on a host that is down
  waits for it.
- **Failover** (`failover.go`): a stack whose host is fenced or disabled
  (`state.FailsOver`) gets another host at once, with `set-stack-status` and no move: the
  old host is off and stops nothing, and the state allows such a change of host without an
  operation (`checkLeaderStatus`), only to an active host. The new host must have a synced
  copy with a disk of each of the stack's volumes of replicas 1 and more, as nothing can
  sync from the host that is down; a volume of replicas 0 starts anew there, empty, in a
  new data generation. Of those hosts, the best fit, as for any placement. A stack that
  finds none waits on its old host, with the reason in `get stacks` and the alert
  `stack-waiting` ("is down"): with replicas 1 until the host is back (or, once phase 3
  brings restores, a user restores the volume elsewhere). A failed host's stacks stay
  where they are until it is fenced.
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
- **Placing** (`place`), in this order: a host that has the stack's data
  and where all of it fits, the one with the least memory left; then one where all of it
  fits once its volumes get copies there (a move, within the resync limit); then, for a
  stack without a host, room made by moving stacks of lower priority to hosts that have
  their data (`makeRoom`, not a stack moved within `MoveCooldown`); last, a host where it
  fits by pushing out (`fitStack`). The same order picks the new host of a stack that
  moves: one a user asked to move (`Stack.MoveTo`), one on a draining host. A stack moves
  with a move-stack operation ([Cluster state](#cluster-state-internalstate)); while it
  moves, its room counts on the new host, and on the old one until it stopped there
  (`stillOn`). A move to the host the stack runs on takes a user's request back, and a
  request to a host that no longer takes stacks goes with that host's phase change. Pushing out works like this:
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
  Each command names the state the pass planned with (`PlannedAt`, starting at the copy's
  index). A command of the pass that directly follows it (index + 1) becomes part of it,
  so the next command may change what it changed; when something else came in between,
  the state refuses a command about what that changed (`ErrStale`, logged as info), and
  the next pass plans again.
  After the placements it starts the moves (`set-pushed-out` on the new host first, then
  `start-operation`) and writes the copies' changes (`put-volume`). `controller.RunMoves`
  runs the leader's steps of each move and minds their deadlines; `agent/moves.go` records
  a host's steps once its loops have done them.
- **On the host** a pushed-out service is left out of `compose up` (`--scale <service>=0`,
  which also removes its running container), and the supervisor's rule for it is "no
  restart". Only the leader changes `Stack.PushedOut`; a host's status report no longer
  carries it. A new generation drops services it no longer has. Whether compose
  up leaves the rest running is checked by the live tests: `bin/dev test-tools` has no
  Docker daemon.
