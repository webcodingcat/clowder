# What users write

Part of [DEVELOPING.md](../../DEVELOPING.md): the names, files and specs that users write,
and how they become what runs.

## Namespaces and names (`internal/names`)

- **Namespaces** keep resources apart: two namespaces may each have a stack, a volume and a
  secret called `shop`. The bootstrap creates `default` and `clowder-system`
  (`names.System`). `clowder-system` holds the cluster's own Secrets, like `kube-system` in
  Kubernetes; it cannot be deleted and holds no stacks or volumes. Users and the state name
  a resource `<namespace>/<name>` (`names.ID`, the key of `State.Stacks`, `Volumes`,
  `Secrets`). A namespace's spec names its registry logins (`imagePullSecrets`, below).
- **One rule for every name** (`names.Valid`): a DNS label of at most `names.MaxLength`
  characters, made of lowercase letters, digits and `-`. There is no `_`: on the host, `_`
  joins a namespace and a stack, so it separates them unambiguously. Namespaces that users
  create must not start with `clowder-`, the prefix of clowder's own projects (Traefik) and
  of `clowder-system`. Stacks, services, volumes and secrets all follow this rule. A
  secret's keys follow Kubernetes' rule (`names.ValidSecretKey`): letters, digits, `-`, `_`
  and `.`, but not `.` or `..`, because each key is a file name in the container.
- **The names on the host are built here, and only here**, from the namespace and the
  stack, so two namespaces never share a name:

  | What | Name |
  |---|---|
  | Compose project, the agent's stack directory, the secrets' directory, the supervisor's pause, Traefik's service | `<namespace>_<stack>` (`names.Project`) |
  | the stack's one Docker network | `<namespace>_<stack>_net` (`names.StackNetwork`) |
  | container | `<namespace>_<stack>_<service>` (`names.Container`) |
  | volume ID | `<namespace>/<volume>` (`names.ID`) |
  | LV, DRBD resource, mount point, Docker volume | `clw-<readable>-<hash8>-g<data generation>` (`names.VolumeName`) |
  | the agent's record of a volume's copies | `volume-records/<namespace>_<volume>.json` (`names.FileName`) |
  | bridge | `cbr-` and 11 hex digits of a hash of the stack's ID (`names.BridgeName`) |
  | alert keys | `stack-waiting/<namespace>/<stack>`, `stack-failing/<namespace>/<stack>` |
  | metrics | the labels `namespace` and `stack` |

- **The LV name has the tightest limits.** LVM and DRBD accept 127 characters, and
  device-mapper accepts 128 for `<vg>-<lv>`, in which it writes every `-` twice. So
  `VolumeName` cuts the readable part (the ID with `/` as `-`) to 24 characters and adds the
  first 8 hex digits of a hash of the whole ID. The hash also keeps `a-b/c` apart from
  `a/b-c`. The generation has at most six digits (`names.MaxDataGeneration`; the state
  refuses more), and a pool's volume group has at most `names.MaxVolumeGroupLength`
  characters (`clusterspec` refuses more). The worst case is 107 of 128 characters in
  device-mapper (`TestVolumeNamesFitDeviceMapper`). The margin is small, so a longer part
  needs the arithmetic again.

## Stack specs (`internal/stackspec`)

A stack is a resource with clowder's own schema; Compose is only the way the agent runs it
on a host (`compose.Render`).

- **Only what clowder knows runs.** `Decode` reads YAML (and so JSON) strictly: a field
  that the schema does not have is an error. `env` accepts any scalar and keeps its text as
  written (`8080`, `1.0`, `yes`). The CLI keeps a number's text when it turns YAML into
  JSON (`resource.ParseDocuments`), so `1.0` arrives as `"1.0"`. Only a number that JSON
  cannot write as it stands arrives the way YAML reads it (`0x10` as `"16"`), and `True`
  arrives as `"true"`.
- **What would break clowder's model has no field.** Nothing ties a container to one host
  (no bind mounts except `/etc/localtime` read-only, no devices), nothing shares the host's
  namespaces, and clowder sets the rest itself (names, networks, the restart policy). A
  route's host must be a domain name and its path a URL path, because both go into a
  Traefik rule.
- **`Check`** lists every problem at once (`Problems`): names, image, mounts at clean
  absolute paths, ports that are not reserved (`ReservedPort`), two services with the same
  published port or route, routes, dependencies that exist and form no cycle, durations,
  sizes, restart rules. The state checks what only the cluster knows: priority categories,
  privileges, the namespace's volumes and secrets, and other stacks' ports and routes.
- **`Normalize`** fills in the schema's own defaults (`restart: always`,
  `restartUnhealthy`, a port's `published` and `protocol`, a route's path). It also writes
  in one form what may be written in several (sorted sets, a route's host in lower case,
  capabilities without `CAP_`), so two specs that mean the same thing are equal. Defaults
  of the configuration (reservations, priority) stay out: the scheduler applies them when it
  places the stack, so a changed configuration changes no spec.
- **The cluster grants privileges.** `Service.Privileges` lists, per service, what reaches
  beyond the container: `privileged`, a capability outside `harmlessCapabilities`, and a
  `securityOpt` other than `no-new-privileges`. Such a container can read the host's keys
  and other stacks' volumes and secrets, and change the rules that keep containers apart.
  So only stacks that the configuration names in `stacks.privileged` (as
  `<namespace>/<stack>`) may have privileges. Otherwise the state refuses the stack, and it
  also refuses a configuration that takes such a stack off the list.
- **Secrets.** A service names secrets of its namespace either as files or as variables.
  As files (`secrets: [db]`), each key is at `/run/secrets/db/<key>`, the way Kubernetes
  mounts a secret, for images that read `*_FILE`. As a variable
  (`secretEnv: {VARIABLE: {name: db, key: password}}`, like Kubernetes' `secretKeyRef`),
  it gets one value. The state refuses a stack whose variable names a key that its secret
  lacks, and a secret change that would remove such a key. A variable also ends up in
  Docker's container configuration on the host's disk and in `docker inspect`; a file does
  not.
- **No I/O, no clock.** The state reads specs, so depguard and forbidigo hold `stackspec`
  to the state's rules.
- **Adding a field:** add its type with both tags (`yaml`, `json`) and a comment in
  `spec.go`, its check in `check.go`, its default or order in `Normalize` if it has one, its
  rendering in `internal/compose/render.go`, and the field in
  `internal/compose/testdata/spec/stack.yaml`. Then run
  `bin/dev test ./internal/compose -update` and review the golden file's diff.

## Rendering (`internal/compose`)

- **`Render`** writes, from a stack's spec, the file that docker compose gets on a host. It
  uses its own types. The file has:
  - the stack's project `<namespace>_<stack>` (`names.Project`) as its name;
  - one network (`names.StackNetwork`) with a fixed bridge name (`names.BridgeName`);
  - container names without Compose's replica number (`names.Container`);
  - `restart: "no"`, because the agent does the restarts;
  - the agent's resolver;
  - each port on the Floating IP and on the node address;
  - volumes bound to `<mount>/data` under the copy's LV name;
  - secrets as files in `Target.SecretsDir`: a Compose secret `<secret>_<key>` per key (no
    secret's name contains `_`), with the target `/run/secrets/<secret>/<key>`;
  - a dependency on a service with a healthcheck waits until that service is healthy.

  HTTP routes are not in the file, because Traefik routes them. `RenderSecretEnv` writes the
  secret variables as a second Compose file. Every `$` in a value becomes `$$`
  (`escapeDollars`), because otherwise docker compose on the host would fill in variables
  from the agent's environment.
- **Labels, and which change recreates what.** Every container gets `clowder.stack` (its
  project). A container that asks for secret files also gets `clowder.secrets`, a digest of
  them (`Target.SecretLabels`). A container that a user restarted gets `clowder.restarted`,
  the time of the restart (`Target.Restarted`), so a new restart recreates exactly its
  services. There are no other labels. Compose recreates exactly the containers whose
  configuration changed, and a changed secret file keeps its path, so only the label tells
  Compose about the change. Secret variables are in the second Compose file, which Compose
  compares itself, and a volume's new data generation is a new volume name. The
  `clowder.secrets` label is an HMAC-SHA256 (`digest.HMAC`) with a key derived from the data
  key, over the names and the state's digests of the service's secret files. The label is
  on disk and in `docker inspect`, and it reveals nothing about a short password. A change
  to these labels changes every container's configuration, so an agent of a release with
  other labels recreates all containers once after the upgrade.
- **Golden files.** `testdata/spec/rendered.golden.yaml` and `secrets.golden.yaml` are
  rendered from `testdata/spec/stack.yaml`. After an intended change, run
  `bin/dev test ./internal/compose -update` and review the diff. `bin/dev test-tools`
  checks both files with Debian's `docker-compose`, and checks that it fills in nothing.

## The cluster's configuration (`internal/clusterspec`)

- **It is a resource** of the kind `ClusterConfiguration`, like kubeadm's: one per cluster,
  without a namespace, always named `cluster` (`resource.ClusterConfigurationName`), the
  way OpenShift names its configuration resources. It is applied with `clowder apply -f`
  together with the other resources and read strictly
  (`resource.DecodeClusterConfiguration`). Its fields are camelCase, like those of every
  resource (`privateNetwork`, `ingress.floatingIP`, `storageClasses`). The cluster's name
  and endpoints are not in it: the name comes from `clowderd init` and the endpoints from
  the credentials, so nothing has two sources.
- `clusterspec` holds the configuration's spec: the types (`Config`), their defaults
  (`WithDefaults`) and `Check`, which lists every problem of the spec on its own. The state
  reads it, so `clusterspec` does no I/O. The state runs `Check` and checks the rest
  against the cluster (`state/config.go` `putConfig`): the storage classes of the volumes,
  the priority categories and privileges of the stacks, the hosts' underlays, and the
  Secrets it names. Once applied, the configuration can be changed but not deleted, because
  fencing and the Floating IP need it. Before the first configuration, the cluster has no
  Hetzner token, no ACME and no alerts (`State.Configured`).
- **It contains no secrets.** It refers to values of Secrets in `clowder-system` by
  `{name, key}` (`names.SecretKeyRef`), as the hcloud controller reads
  `kube-system/hcloud`: `hetzner.tokenSecretRef`; a channel's `webhookSecretRef`,
  `urlSecretRef` and `smtp.passwordSecretRef`; and the heartbeat's `urlSecretRef`.
  `clusterspec.SecretPlaces` is the one list of these references. `Check` holds each one to
  the rules of a reference, and the state holds each one to the Secrets it has. The state
  refuses a configuration that names a missing Secret or key. It also refuses deleting such
  a Secret, and a Secret change that would remove such a key. In one apply, the Secret comes
  before the configuration.
- **No files next to it.** `acme.ca` and `smtp.ca` are in the configuration as PEM, like
  Kubernetes' `caBundle`, within `clusterspec.MaxCASize`. `Check` requires a certificate in
  each.
- **`hetzner.api` and `acme.directory`** are for test labs. When they are unset, clowder
  talks to Hetzner's API and Let's Encrypt over https. When they are set, they may also be
  `http://`, and `ServiceURLWarnings` warns about it: an apply returns the warning (the CLI
  prints it on stderr), and the leader logs it once per configuration
  (`controller.warnServiceURLs`).
- **Registry logins belong to a namespace,** as Kubernetes puts `imagePullSecrets` on a
  namespace's default service account. `spec.imagePullSecrets: [{name: ghcr}]` names
  Secrets of the namespace, each with the keys `server`, `username` and `password`
  (`resource.RegistryLoginKeys`). As in Kubernetes, they are not checked at apply: the
  Secrets live in the namespace, so they come after it. Like the kubelet, a host pulls
  without a missing login; if the pull then fails, `get stacks` names the missing login
  (`State.RegistryLogins`).
- Durations (`acme.renewBefore`, `alerts.heartbeat.interval`) are parsed by
  `units.ParseDuration`. It uses Go's format, which may start with whole days (`30d`,
  `1d12h`).

## The CLI's credentials (`internal/clientconfig`)

- **Where they are.** The credentials are not in the resource files, which a team shares
  in a repository, but in the user's own file, like kubectl's kubeconfig: `--config`, else
  `$CLOWDER_CONFIG`, else `~/.clowder/config` (`Path`).
- **What the file holds.** `contexts`, each the way into one cluster (`name`, `endpoints`,
  `ca`, `certificate`, `key`), and `current_context`. Unlike in a kubeconfig, a context
  holds everything itself, with no clusters and users joined by name, and it lists several
  endpoints. The keys are snake_case, like every clowder file that is not a resource.
  Whoever holds a context controls its cluster, as with kubeadm's `admin.conf`.
- **Reading and writing.** The file is plain YAML. `Parse` reads it strictly and checks it:
  every context is complete, its endpoints are addresses or host names, the names are
  unique, and the current context exists. `Write` writes it whole or not at all, with mode
  0600 in a directory of 0700, the way SSH keeps its keys. A missing or empty file reads as
  empty, so the first `config import` creates it.
- **Contexts.** A context's name is the user's choice. `Add` refuses a name that is taken,
  `Put` replaces the context in its place, and `Delete` removes it. The first context
  becomes the current one. The errors (`ErrNoCurrentContext`, `ErrNoContext`,
  `ErrContextTaken`) name no flag; the CLI says what to do about them.
- The tests point `HOME` (and `USERPROFILE`) or `$CLOWDER_CONFIG` at a temporary directory.

## Secrets and sealed values (`internal/secretvalue`)

A Secret's values come in one of two ways, as in Kubernetes with Bitnami's sealed secrets.
Both end up as the same Secret in the state, so whatever names a Secret does not care how
it came in.

- **`Secret`** (`resource.SecretSpec`): the values in clear, `spec.data` in Base64 and
  `spec.stringData` as text; for a key in both, `stringData` wins, as in Kubernetes. Unlike
  in Kubernetes, both stand under `spec`, like every clowder resource's fields. The CLI sends
  the values over its mTLS connection, and the cluster encrypts them with its data key. Such
  a file does not belong in Git. `clowder create secret [<namespace>/]<name>
  --from-literal <key>=<value> --from-file <key>=<path|->` sends one without a file, and,
  like `kubectl create`, refuses a Secret that exists (`api.ApplyRequest.Create`,
  `state.PutResource.Create`).
- **`SealedSecret`** (`resource.SealedSecretSpec`): `spec.encryptedData` holds each value
  sealed for the cluster's sealing key, one line of Base64 of age's ciphertext, as kubeseal
  writes it. It goes into Git. The apply turns it into the Secret of the same namespace and
  name (`resource.StoredKind`); `get`, `delete -f` and `diff -f` work on that Secret. Unlike
  Bitnami's, it is not a resource of its own in the cluster, and it has no `template`.
- **`clowder secret seal`** makes SealedSecrets, as kubeseal does: from the Secrets of a
  file (`-f <file|->`), or from `--from-literal` and `--from-file` for one Secret. It seals
  for the public sealing key that the cluster names (`GET /v1/sealing-key`), and the CLI
  asks for it each time instead of keeping a copy that someone could change. With
  `--cert <file>`, it seals offline for the public key that `clowderd sealing-key export
  --public` wrote, like kubeseal's `--cert`.

The rest of this section is about sealed values.

- **The envelope.** What age encrypts is JSON:
  `{"v":1,"for":"<secret ID>","value":"<Base64>"}`. `for` is the Secret the value is for (its
  ID, as Bitnami's sealed secrets bind a value to its namespace and name). So whoever
  opens it can refuse a value copied to another Secret, such as the Hetzner token copied
  into a stack's Secret. `Open` returns `for`, and the caller checks it (`OpenFor`). Within
  a Secret, a value may move between keys. The value is Base64, so it can hold any bytes
  and needs no escaping that would change its size. A higher `v` is refused, because a
  newer clowder wrote it.
- **One line.** A sealed value is plain Base64 with no line breaks, so it needs no YAML tag
  and no block. `secret rekey` (`cli/values.go` `fileValues`) first checks the file as
  apply reads it, then takes the values from its YAML nodes, which give each value's line.
  `replaceValues` puts the new value in
  place of the old one on that line and leaves the rest of the file as it is (comments,
  quotes, CRLF line ends). It refuses a value that YAML folded over several lines.
- No error message contains a value: `Open` drops json's messages, because they quote the
  text they fail on.
