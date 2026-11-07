# The API (`internal/api`) and the CLI (`internal/cli`)

Part of [DEVELOPING.md](../../DEVELOPING.md): how a call reaches the cluster, who may make
it, and what the CLI does with the answer.

## Requests and the leader

- **Two sides.** `api.Server` answers the calls to this agent. `api.Peers` (`peers.go`) asks
  the other agents as this host: it commits through the leader (`Peers.Propose`), sends
  beats and asks voters for silences, asks the leader for this host's secrets and its
  commit index, and asks a host for its copies' progress. The agent builds `Peers` first
  and hands it to its parts, and the server forwards through it too. Both get the same
  `HostConfig`: the state, the certificates, the roles, the data key and the addresses.
- **Every call is checked.** Every agent serves `/v1` with mutual TLS (`hostfiles.APIPort`).
  Each call is checked against the CA, the state's revocation list and, for the CLI,
  `CLICutoff`, so a revoked certificate is refused at once on every agent. Every request
  body is limited (`MaxRequestBytes`). A larger one gets 413, and `apply` then advises
  applying the namespaces' directories one at a time.
- **Any control-plane agent answers.** Reads come from its own copy of the state. Writes it
  commits as the leader, or forwards to the leader's `/v1/internal/propose`. A write
  answers with its Raft index (`X-Clowder-Index`). The client sends that index with later
  reads (`X-Clowder-Min-Index`), and the agent waits until it has applied it.
- **Reads of resources are consistent** (`consistent`), as kube-apiserver's are. The
  index above reaches only the reads of the same CLI process, so before it lists or gets
  resources, a follower asks the leader for its commit index and waits until it has
  applied it (`CatchUpWithLeader`). A command thus sees every write acknowledged before it,
  through any agent. Such a read needs a leader and gets 503 without one. `status`, the
  host list and the audit read the agent's own copy, so they answer without a leader; the
  watch follows a list's index and needs no more.
- **Waiting for a leader.** During an election, after a peer restarted, or while a new
  leader is still at its barrier, there is no leader. Every path to the leader (a command,
  the batch of an apply, a join, the catch-up) then waits for one (`waitForLeader`)
  instead of failing at once. The wait and the call to the leader share one budget:
  `waitTimeout`, or `joinTimeout` for a join.
- A refusal by the state is HTTP 409 with its reason; a write that did not commit is 503.
- `api.HostActions` lists the user's host actions with the phases they start from. The
  server checks them, and the CLI builds its subcommands from them.
- **Versions.** An agent refuses a caller, CLI or agent, whose version is not compatible
  with its own (`X-Clowder-Version`, `buildinfo.Compatible`: the same minor version before
  `v1.0.0`, then at most one minor version apart). Development builds and callers without
  a version talk to everything. The agent logs a warning once per version when a
  development build talks to another build.

## Who may call what

Each route names its callers (`Server.routes`, `api/access.go`), like Kubernetes' node
authorizer. A refused caller gets 403.

| Routes | Caller (`caller`) |
|---|---|
| the CLI's (`/v1/…` except `internal`) | a CLI certificate (`cliCaller`) |
| `GET` and `POST /v1/join` | anyone with an open join token (`handleOpen`, limited per sender) |
| `internal/propose`, `internal/beats` (both), `internal/commit-index`, `internal/leader`, `internal/hosts/self/secrets`, `internal/hosts/self/tls` | any host (`anyHost`) |
| `internal/changes`, `internal/join`, `internal/hand-over`, `internal/alerts/failures`, `internal/stacks/…/ps\|logs`, `internal/volumes/progress`, `internal/volumes/…/resolve-splitbrain`, `internal/recover/copies`, `internal/recover/rejoin` | a control-plane host (`controlPlaneHost`) |
| `internal/credentials`, `internal/sealing-key` | this host's own certificate (`thisHost`) |

`TestEachRouteTakesItsCallers` holds the table a second time, so a new route cannot come
without its callers.

- **A host without the control plane** serves only the control plane. Its API takes only
  control-plane hosts, already in the TLS handshake (`Server.TLSConfig`,
  `pki.ServerTLSForControlPlane`), so it serves neither the CLI nor the join. It listens
  only on the underlay and the node address. Only the control plane calls it (a stack's
  `ps` and `logs`, the copies' progress, a split brain, `clowderd recover`). Its own calls
  go to the voters (beats) and to the leader (its reports, the commit index, its secrets).
  `TestAWorkerTakesOnlyTheControlPlane` checks this.
- **Who speaks.** The actor of a command is the caller of the first agent, and its time
  (`At`) is the leader's clock. `/v1/internal/propose` accepts only the kinds in one table,
  `proposers`, and for each kind only its senders (`mayPropose`, decided by the actor):
  - `cli`: the CLI's command that a control-plane follower forwards, with any CLI actor.
    These are exactly the kinds that the CLI's write routes make
    (`TestCLIKindsAreTheKindsOfTheCLIsWrites`);
  - `anyHost`: what any host reports about itself, as itself. The state checks that each
    report is about the actor's own host (its stacks, its copies, the operation it owns),
    as Kubernetes' NodeRestriction does;
  - `controlPlane`: what a control-plane host does for the cluster, as itself (the cutoff
    of `clowderd credentials --revoke-older`).

  A kind may have more than one sender (`set-host-phase` is the CLI's and a host's own).
  A kind that is not in the table only the leader writes.

  `/v1/internal/changes` accepts only the CLI's resource commands ([Resources](#resources)).
  The leader's own commands (placements, the ingress) are made on the leader and never come
  in from outside. A host never speaks for another host or for the leader.
- **A control-plane host may speak for the CLI.** The CLI's certificate ends at the first
  agent, so the leader cannot check that a forwarded command really came from the CLI.
  This is on purpose, as Kubernetes trusts its API servers. A control-plane host names the
  CLI's actor (`cli/<name>`) and could name another; that is accepted, because the name
  grants nothing. The agent that takes a CLI request logs its actor with every write ("a
  CLI request", in that agent's journal only). The record that every agent keeps is the
  actor of the Raft command and the state's `Audit`. What keeps strangers out is the CA,
  and removing a host revokes its certificate. A worker can speak neither for the CLI nor
  for another host. Only control-plane hosts hold the CA's key
  ([Keys only on the control plane](#keys-only-on-the-control-plane)).

## Keys only on the control plane

Only control-plane hosts hold the data key, and with it the CA's key, the sealing key and
every secret. A join's answer carries the data key only for them (`clowderd/daemon/joinleader.go`), and
the agent reads it only on them (`hostfiles.Files.ReadCredentials`). On any other host
`HostConfig.Keys` is nil, and whatever needs a key refuses with 503 (`errNoKeys`,
answered through `writeFailure`).

Any other host asks the leader for what it needs, and gets only that, as Kubernetes' node
authorizer gives a node only the secrets of its pods (`api/hostsecrets.go`):

- **`GET /v1/internal/hosts/self/secrets`** opens the secrets that the caller may read
  (`state.SecretsOfHost`): those that the stacks it runs name (`state.StacksToRun`, the
  same list the host's stack runner reads), and the image pull secrets of their
  namespaces. The answer gives each with its digest (`HostSecret`).
- **`GET /v1/internal/hosts/self/tls`** opens the certificates' keys, by domain, only while
  the state names the caller as the host of the Floating IP (`state.Ingress`). The host
  that takes over the Floating IP gets the keys from the control plane. The leader moves
  the Floating IP and is on the control plane, so it can always give them.
- **Each for itself.** Both take any host (`anyHost`) and answer for the caller's own
  certificate. The host sends its state's index (`X-Clowder-Min-Index`), so the leader
  does not judge from an older state than the host's.
- **One way on every host.** `Peers.SecretsOfThisHost` and `CertificatesOfThisHost` open
  the values themselves on a control-plane host, with the same rules, and ask the leader
  on any other. The agent keeps the answers in memory and asks again only for a new
  digest or chain ([A host](host.md)).
- **Deviations from Kubernetes.** A host asks for all it may read in one call, not secret
  by secret, because the answer is small and replaces what the host kept. The
  `clowder.secrets` label of a container is a SHA-256 over the state's digests, which are
  keyed already, so the host needs no key of its own for it ([specs.md](specs.md)).

## Resources

`changes.go` changes resources, and `resources.go` reads them.

**`POST /v1/apply`** (`ApplyRequest`) and **`POST /v1/delete`** (`DeleteRequest`) take
resources in order and run them as one `change-resources`, all or nothing. They are the
only way to change resources, and every change comes through the channel `cli`, whatever
the certificate's name. They answer per resource (`ChangeResult`):

- `applied` or `deleted`;
- `unchanged`;
- `refused`: the state said no, with the reason, at the resource that its `ChangeError`
  names;
- `failed`: did not commit (no leader, no quorum); try again;
- `not-applied`: every other resource of a request that was refused or failed.

Requests are read strictly, so a typo is an error, not an empty request.
`internal/propose` and `internal/changes` read leniently, because an agent of the next
release may forward commands with fields that this one does not know. The other internal
routes (`internal/join`, a split brain's resolve) read strictly.

**The first agent prepares each resource** (`putChange`):

- It reads the spec strictly (`resource.DecodeClusterConfiguration`, `DecodeNamespaceSpec`,
  `DecodeVolumeSpec`, `stackspec.Decode`). A field that the kind does not have is an
  error, and `replicas` has no default.
- It opens a SealedSecret's values (`unsealSecret`), then encrypts a secret's values with
  the data key and computes its digest (`encryptSecret`). The digest is `digest.HMAC` over
  the ID and every key and value, with a key that `Keys.Derive` gives for "clowder secret
  resource digest", separate from the containers' labels.
- A spec that cannot be read is refused at its resource, before the leader sees the
  request. So is a resource that the request already named, such as a Secret and a
  SealedSecret of the same name, because the state would keep only the second without a
  word.

**The leader decides** (`runOnLeader`). A follower hands the changes on as a batch
(`changeBatch`) to `/v1/internal/changes`, because its own state may be behind and would
answer `unchanged` or `refused` for what has changed since. `runChanges` first applies the
`change-resources` to a clone of the leader's state, with every check of the state.

- A refusal answers at once, and a batch that changes nothing is not proposed (no Raft
  entry).
- `?dryRun=true` stops there and answers what a real apply would, with index 0.
- A real change is proposed, and the state checks it once more as it commits. The answer
  per resource comes from the trial on the clone. If another command commits in between,
  the commit may be refused (the answer then says so), or may change less than the answer
  says: a resource that the other command already set to the same spec shows as applied.
- A batch that changed the configuration returns its warnings (`configWarnings`,
  `clusterspec.ServiceURLWarnings`: `hetzner.api` or `acme.directory` set, and over
  `http://`).

**A dry run says what would change** (`describe`). For each applied resource it says
whether the resource is new (`Created`) or which parts differ (`Differs`). For most kinds
these are the spec's top-level fields. For a Secret they are the keys that were added,
removed or changed, judged by the keys' digests and never by a value
(`secretDifferences`). It also returns the configuration's warnings and what a new Hetzner
token sees.

**What a new Hetzner token sees** (`checkNewToken`, `TokenCheck`). When a dry run changes
the token, in the configuration or in the Secret it names, the leader opens the new token
and asks Hetzner once for the project's servers (`ServerConfig.HetznerServers`,
`hetzner.Client.ServerIDs`). It counts the hosts that are not removed and whose server it
sees (`Hosts`, `Seen`), or says why it could not ask (`Error`).

- Only the token counts, not `hetzner.api`, because a test lab's cluster never moves to
  Hetzner's own API.
- A server's ID is unique across Hetzner, and a token sees only its own project's
  servers. So a wrong token powers off no other server: it reaches none of the cluster's,
  and fencing and the Floating IP stop working. That is why the CLI asks before it applies
  one ([The CLI](#the-cli)).
- The check never refuses, because a token may be changed on purpose before its project
  is.

**Reads.** `GET /v1/resources/{kind}` lists the resources of `?namespace=` or of
`?allNamespaces=true`. There is no default, so no reader gets the wrong namespace by
accident. It returns the state's index that it read at, for the watch. `GET
/v1/resources/{kind}/{namespace}/{name}` and `/v1/resources/{kind}/{name}` (a namespace,
the configuration) read one resource. The kind is written in lower case. A secret shows
its keys and its digest (`SecretStatus`), never its values. Any other resource can be
applied again as it is read: the owner, generation, resource version and status belong to
the cluster, and an apply ignores them.

### The watch

`GET /v1/watch?kind=&namespace=|allNamespaces=true&since=` (`watch.go`) streams the
changes of a kind after `since`, the index of the client's list, as one JSON object per
line (`WatchEvent`: the resource, its resource version, and whether it is gone). Spec and
status changes are both events. The client reads a resource itself when it needs it.

- Every agent keeps the last `state.ChangeRingSize` changes (`state.ChangeRing`). Only the
  Raft log fills the ring (`raftnode`'s `fsm.Apply`), so a dry run never reaches it.
  Hosts' reports about themselves are not changes, so on a quiet cluster the ring covers
  hours.
- The ring knows the index from which it is complete: the snapshot the agent started from
  or installed, or the oldest change it dropped. A `since` before that gets 410, and the
  client lists again, as with Kubernetes. So a client that listed on one agent can watch
  on another.
- A `since` that this agent has not applied yet waits at most `watchWaitTimeout`, then
  gets 504, because a follower cut off from the leader would wait forever.
- The stream runs until the client stops. It ends early when the ring has dropped changes
  that the client has not read yet; the client then lists again and says so. The agent's
  stop ends the stream (`Server.EndStreams`, as for logs that follow), because `Shutdown`
  would wait for it.

## The CLI

### Reaching the cluster

- **The credentials file** (`--config`, `$CLOWDER_CONFIG`, `~/.clowder/config`;
  [`clientconfig`](specs.md#the-clis-credentials-internalclientconfig)). The CLI takes the
  context that `--context` names, or else the current one (`globals.connect`), and warns
  from `renewWarning` before the context's certificate ends. `clowder config
  get-contexts|use-context|delete-context|import` manage the file (`cli/config.go`;
  `withHint` adds advice to `clientconfig`'s errors).
- **`config import <file|->`** adds the one context that `clowderd credentials` prints.
  It adds one context rather than merging files: kubectl has no merge, and merging by
  hand is where keys get lost. It adds the context under the cluster's name or under `--as` (not `--context`,
  which picks the context a command uses). It refuses a name that is taken; with
  `--replace` it puts the context in the old one's place, as after a renewal.
- **The client** tries the context's endpoints in turn and trusts any control-plane host
  certificate of the cluster's CA, whatever name the endpoint has
  (`pki.ClientTLSToControlPlane`). A worker among the endpoints fails the handshake and is
  skipped like one that cannot be reached, so a removed worker cannot answer as the
  cluster. A read moves on to the next endpoint after any network error. A write moves on
  only if it was never sent (`httptrace`), because a write that was sent may have
  committed without an answer.
- **The endpoints** are all the CLI knows; it learns no others from the cluster. A
  kubeconfig has one `server`, and kubectl reaches several control-plane nodes only
  through a load balancer, a VIP or DNS. clowder keeps a list instead, so a cluster needs
  nothing in front of it. `clowderd credentials` writes the public addresses of the
  control-plane hosts at that time (`CredentialsAnswer.Endpoints`), or what `--endpoint`
  names.
- **An endpoint that does not answer costs little.** One that has not connected within
  `--connect-timeout` (`api.DefaultConnectTimeout` without the flag), or has not finished
  TLS within the same time again, counts as unreachable. A host that is cut off drops the
  packets, and the request's own timeout (`answerTimeout`) would hold up the next
  endpoint. An agent that set up the connection and then hangs still holds a request for
  `answerTimeout`. The client starts each request with the endpoint that answered last
  (`inTurn`), so an endpoint that is down costs its timeout once per command, not once per
  request. Every client that asks endpoints shares this transport (`endpointTransport`),
  including the join and recover. `clowderd join` has no `--connect-timeout` and waits the
  default.
- **Another CA.** When an agent's certificate is from another CA than the context's, the
  CLI says so (`foreignCA`: another cluster at that address, or one set up anew) instead
  of showing only x509's error text.

### Exit codes

| Code | When |
|---|---|
| 0 | the command did what was asked |
| 2 | a wrong call: an unknown command, or flags or arguments that do not fit (`usageError`) |
| 1 | anything else: refused, missing, not reachable |

`checkArgs` turns every command's argument errors into a `usageError`. It also gives each
command that only groups other commands (`clowder stack`) a `RunE`, which shows the help
and treats an unknown subcommand as a wrong call; cobra would otherwise end as if it had
succeeded. A refusal shows the state's reason without the command's kind
(`state.CommandError`). `diff` is the one exception ([below](#the-commands)).

### The commands

- **`apply -f <file|directory|->`** (`resources.go`, `files.go`) reads YAML documents.
  `resource.ParseDocuments` accepts a file of clowder's resources only, or a file of other
  documents, but never both; another apiVersion of clowder's is an error. For a directory,
  it reads the YAML files in it by name, not those in subdirectories. Hidden files, other
  files and files of other documents are skipped and named on stderr.

  A resource without a namespace is in `default`. The resources are sent in the order of
  the files, as kubectl applies them: each file from top to bottom, a directory's files by
  name, all in one request. So whatever a resource needs must come before it. Each result
  is printed, and any resource that is not applied, deleted or unchanged makes the command
  fail. `--dry-run` checks everything and changes nothing. The warnings and what a new
  Hetzner token sees go to stderr (`printWarnings`).

  The CLI sends a `Secret` (values in clear) and a `SealedSecret` (values sealed for the
  sealing key) as they are, over its mTLS connection.
- **The CLI asks before it applies a new Hetzner token** (`confirmToken`). An apply that
  contains the ClusterConfiguration or a Secret of `clowder-system` (`mayChangeToken`) is
  first sent as a dry run. If the answer has a `HetznerToken`, the CLI shows what the new
  token sees (`printTokenCheck`: "sees the servers of 3 of 3 hosts", "sees none", "could
  not be checked"), asks for `yes` on a terminal as `host remove` does, and then applies.
  Without a terminal, `--yes` is the only way. A GitOps source would have nobody to ask,
  so there only the alert `hetzner-token` would report a bad token
  ([Alerts](loops.md#alerts-internalnotify-controllerwantedgo-controlleralertsgo)).
- **`create secret [<namespace>/]<name> (--from-literal <key>=<value> | --from-file
  <key>=<path|->)...`** (`create.go`) sends one Secret without a file. Like `kubectl
  create`, it refuses a Secret that exists (`ApplyRequest.Create`, then
  `state.PutResource.Create`: "Secret <namespace>/<name> already exists"); a change goes
  through `apply`.
- **`diff -f <file|directory|->`** sends the resources as a dry run and prints each one
  that would change (`printDiff`): whether it is new or which fields differ, and for a
  resource the cluster would refuse, the reason. Like `kubectl diff`, it ends with 0 when
  nothing would change, 1 when something would (`errDiffers`, without a message), and 2
  when it could not tell (`diffFailed`), including a refusal.
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
- **`delete -f`** deletes the resources of files from the bottom up, the reverse order of
  an apply, all or nothing, in the namespaces that the files name (no `-n`). The
  configuration and the namespaces `default` and `clowder-system` are never deleted.
  Deleting a volume deletes its data, so a delete that names one needs `--with-data`
  (`DeleteRequest.WithData`). Without it, the API refuses the whole request before
  anything is deleted; otherwise a delete of a directory would remove the stack and then
  stop at its volume.
- **`stack ps|logs [<namespace>/]<stack>`** (`stack.go`) ask the stack's host (`GET
  /v1/stacks/{namespace}/{name}/ps|logs`, then `/v1/internal/stacks/<project>/...`,
  `names.Project`), through whichever agent the CLI reached. **`stack restart
  [<namespace>/]<stack> [<service>...]`** sends `restart-stack` (`POST
  /v1/stacks/{namespace}/{name}/restart`, `RestartRequest`).
- **`get stacks`** is the list of stacks, also as JSON for scripts and the live tests. Its
  table shows a priority that comes from the configuration's default as `(default)`.
  `cluster status` shows the configuration's generation (`api.Status.Generation`), or that
  there is none.

## Secret values

[Secrets and sealed values](specs.md#secrets-and-sealed-values-internalsecretvalue)
describes the format of a sealed value.

- **Two ways in, one Secret.** The agent that takes the request encrypts a `Secret`'s
  values with the data key and computes the digest (`api/changes.go` `encryptSecret`). It
  first opens a `SealedSecret`'s values with the sealing key (`unsealSecret`,
  `secretvalue.OpenFor`; `sealingKey` decrypts the key from the state). It refuses a value
  that is not sealed for the Secret's ID. So a value copied from another Secret, such as
  the Hetzner token of `clowder-system`, does not pass, and no container can read it that
  way.
- **A SealedSecret is not a resource of its own,** unlike Bitnami's. The apply stores it as
  the Secret of the same namespace and name (`resource.StoredKind`), and a Secret of the
  same name is the same resource. The results keep the kind that was sent ("SealedSecret
  shop/db: applied"). `delete -f` of a SealedSecret deletes the Secret, and `get` shows
  Secrets only. There is no `template`. Values are sealed with age (X25519), not with RSA
  as in kubeseal.
- **The cluster's own values** are Secrets of `clowder-system` and come in with `apply` like
  every other Secret. The configuration names them by `{name, key}`, as the hcloud
  controller reads `kube-system/hcloud`. No stack lives in `clowder-system`, so no
  container gets them. These open them: the leader's loops (`controller.OpenHetzner`,
  `notify.New` with `notify.Secrets`), the join, `clowderd recover` and the agent's start
  (`checkRetired`).
- **Who can read a secret:**
  - whoever holds the sealing key, because every SealedSecret in the files is sealed for
    it;
  - every control-plane host, which holds the data key;
  - any other host while it runs a stack that names the secret
    ([Keys only on the control plane](#keys-only-on-the-control-plane));
  - whoever holds the CLI's credentials, through a stack that names the secret, or through
    a stack in `stacks.privileged`, whose containers reach the secrets on their host, and
    the data key on a control-plane host.
- **`GET /v1/sealing-key`** names the public half of the sealing key
  (`{"recipient":"age1..."}`, `api/cluster.go`). **`secret seal`** (`cli/secret.go`) asks
  for it each time, unless `--cert <file>` gives it, and seals each value for the ID of its
  Secret (`secretvalue.Seal`). It writes each value on one line, as kubeseal does.
- **`clowderd sealing-key export [--public]`** (`clowderd/sealingkey`) asks the host's own
  agent for the sealing key, with the host's certificate (`GET /v1/internal/sealing-key`),
  as `clowderd credentials` does for a certificate. The agent logs a warning ("the sealing
  key was exported"). The command prints the key (`AGE-SECRET-KEY-1...`), or with
  `--public` only its public half (`age1...`), which `secret seal --cert` takes. The help
  of `clowderd init` says to export the key right after init and keep it offline.
- **`secret rekey -f <file|directory> --identity <file|-> [--cert <file>]`**
  (`cli/rekey.go`) opens every sealed value of the files with the old sealing key and seals
  it again for the cluster of the context, or for `--cert`. This is the way to a new
  cluster when the old one is lost. `--identity` is required, because no cluster opens a
  value for the CLI.
  - `fileValues` (`cli/values.go`) refuses a file that apply would refuse, then reads the
    values of its SealedSecrets and the Secret each one must be for. A value meant for
    another Secret is refused, as an apply would (`secretvalue.OpenFor`).
  - `replaceValues` puts each new value in place of the old one on its line, and the file
    is written all or nothing. A refused file stops the run; the files reported before it
    are done.
- **`secret decrypt --identity <file|-> -f <file>`** (`cli/decrypt.go`) is for emergencies
  and needs no cluster. It opens the SealedSecrets of a file with the sealing key and
  prints one line per value: where it is (the Secret's ID and the key, `shop/db[password]`),
  a tab, and the value. A value that is not printable text, has a space at either end, or
  starts like a marker is shown as `base64:`. A value that does not open, or is meant for
  another Secret, is shown as `error:` with the reason, and the command fails after
  printing every line.

## Joining hosts

A host joins with a token that the CLI created (`clowder host join-token`, `api/join.go`,
`clowderd/join`). The token names the roles a host may take (`--roles`, `worker,ingress`
by default), and `clowderd join --roles` picks from them. So whoever reads a worker's
token from its user data cannot join as a control-plane host. kubeadm needs a second
secret for that (`--certificate-key`). clowder's token opens only once, so the roles in it
are enough.

1. `GET /v1/join?token=<id>` returns the CA certificate with a `pki.TrustProof` made with
   the token's hash, while the token is open. The host trusts the CA only if the proof
   matches its token (`api.FindClusterByToken`).
2. `POST /v1/join`, over TLS checked against that CA, carries the token itself, the host's
   name, addresses, server ref and public keys (its own key and WireGuard's; the private
   halves stay on the host), a nonce and the time. Any control-plane agent accepts it and
   hands it to the leader (`/v1/internal/join`). A worker refuses the joining host in the
   handshake, and the joining host accepts only a control-plane host
   (`pki.ClientTLSToControlPlane`), as kubeadm joins through the control plane's endpoint.
   The joining host waits longer for the answer than the leader may take
   (`joinClientTimeout`).
3. The leader (`agent.joinLeader`) accepts a request only within `joinMaxAge`, and each one
   only once while it leads (a nonce in its memory). This is little more than insurance:
   only the cluster's agents see a request, over TLS, and whoever saw one has seen its
   token.
4. The leader compares the token's hash, checks that the host's roles are within the
   token's (`JoinToken.CheckRoles`), and requires an applied configuration
   (`Configured`). It chooses the lowest free ID and issues the host's certificate. It
   checks the host against the state (`CheckJoiningHost`), checks that the request comes
   from the host's underlay or public address (`JoinRequest.Source`, set from the
   connection by the agent that accepted it), and checks its server with Hetzner
   (`hetzner.Client.CheckServer`: the server has the host's addresses). Then it commits
   `add-host`. Hetzner vouches for the addresses, and the source vouches for the host that
   has them. Without the source check, whoever holds a token could name another server of
   the project, which fencing would later power off.
5. A refusal says why, because only the holder of an open token's secret gets that far,
   and the token stays open for another try. The answer carries the certificate, the data
   key and the other hosts, for the host's first WireGuard interfaces
   ([Join](host.md#the-agent-internalclowderd)).

- **The join's routes need no certificate,** so each control-plane agent accepts at most
  `triesPerSender` tries a minute per sender and `triesInAll` in total, counted in memory
  (`api/limit.go`). After that it answers 429. A sender is its IPv4 address, or its /64 for
  IPv6.
- **The server ID** decides which server fencing powers off and where the Floating IP
  goes, and the Hetzner token may act on every server of its project. A joined host's
  server is checked as it joins. The first host's server, whose ID init takes from the
  metadata service, is checked by the Floating IP's loop once a configuration names the
  token (`confirm-server`). Only a host with a checked server holds the Floating IP
  (`set-ingress`).

## Credentials from a host

The cluster has no users, as kubeadm's has none: whoever holds a CLI certificate controls
it. Every credential comes from root on a host, the first one too, like kubeadm's
`admin.conf` and `kubeadm kubeconfig user`: `clowderd init` creates the cluster, then `ssh
root@<host> clowderd credentials | clowder config import -`. The CA is trusted through SSH,
so the first host's user data holds no secret, and no route except the join's is open
without a certificate.

- **`clowderd credentials`** (`clowderd/credentials`) creates the CLI's key on the host. It
  asks its own agent, with the host's certificate, for a certificate with the name from
  `--name` (`POST /v1/internal/credentials`, `api/credentials.go`). The agent opens the
  CA's key from the state with the data key, issues the certificate, and answers with the
  cluster's name and the public addresses of the control-plane hosts that are not
  removed. Only the host's own certificate may ask (`thisHost`), and only a control-plane
  host takes it in the handshake. The command prints a CLI config with one context named
  after the cluster, with those addresses as its endpoints unless `--endpoint` names
  others. The key crosses SSH.
- **`--revoke-older`** also commits `set-cli-cutoff` as that host, with the new
  certificate's start time. From then on, every agent refuses the CLI certificates that
  start before it (`Server.cliCutoffPassed`), including a stolen laptop's. It needs the
  leader. The certificate is returned only once the cutoff is committed and the host's own
  agent has applied it; the other agents follow within moments. Certificate times count in
  seconds, so a certificate issued in the same second as the new one stays valid.

## Tests

The tests run real HTTPS servers: a follower forwarding to the leader, refused callers,
and the CLI against an agent with a credentials file in a temporary home.
`clowderd/daemon/run_test.go` runs the whole way: init, the first credentials printed and imported,
the sealing key exported, new credentials with `--revoke-older`, apply and diff.
