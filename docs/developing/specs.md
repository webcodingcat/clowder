# What users write

Part of [DEVELOPING.md](../../DEVELOPING.md): the names, the files and the specs users
write, and how they become what runs.

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
  | Compose project, the agent's stack directory, the secrets' directory, the supervisor's pause, Traefik's service | `<namespace>_<stack>` (`names.Project`) |
  | the stack's one Docker network | `<namespace>_<stack>_net` (`names.StackNetwork`) |
  | container | `<namespace>_<stack>_<service>` (`names.Container`) |
  | volume ID | `<namespace>/<volume>` (`names.ID`) |
  | LV, DRBD resource, mount point, Docker volume | `clw-<readable>-<hash8>-g<data generation>` (`names.VolumeName`) |
  | the agent's record of a volume's copies | `volume-records/<namespace>_<volume>.json` (`names.FileName`) |
  | bridge | `cbr-` and 11 hex digits of a hash of the stack's ID (`names.BridgeName`) |
  | alert keys | `stack-waiting/<namespace>/<stack>`, `stack-failing/<namespace>/<stack>` |
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
- **Secrets in `cluster.yaml`** stand at their places, encrypted and tagged `!secret`
  (Encrypted values, below): the hoster's token, `join_keys`, the registries' passwords,
  the alert channels' URLs and passwords, the heartbeat's URL.
  `clusterspec.SecretPlaces` lists these places, the one list of them. In `Config` a
  secret is its name (`SecretRef`), which is its path (`hoster.token`, `join_keys[0]`):
  `Check` holds each name to its place, so no CLI can name a secret otherwise than the
  state expects.
- `clusterdir` reads the directory. `Find` picks it (`--cluster`, `$CLOWDER_CLUSTER`, the
  working directory). `Load` takes the values out of `cluster.yaml`
  (`secretvalue.Extract`), checks that the places they came from are exactly
  `SecretPlaces` (a `!secret` elsewhere, or a place in clear, is an error), decodes the
  rest strictly and keeps the values apart, still encrypted (`Directory.Encrypted`). It
  does not run `Check`: the commands that need only the name and the seeds (`secret
  encrypt`) work with the `cluster.yaml` the bootstrap writes, which is not complete yet.
  `cluster apply` and `diff` run it. `Schema` makes the JSON schema of `cluster.yaml` from
  the Go types; it lives here so the state does not link the schema library. Editors with
  the yaml-language-server need `customTags: ["!secret scalar"]`.
- `Create` writes the `cluster.yaml` of `cluster bootstrap`: name, seed, the bootstrap
  key encrypted, and as comments what the first `cluster apply` still needs.
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
  since once the cluster has its user a bootstrap cannot be repeated. It writes the
  credentials before the cluster directory: `cluster.yaml` can be written by hand, the
  user's key cannot be had again.
- **Files next to `cluster.yaml`** (`acme.ca`): `clusterspec.FileNames` lists those it
  names, by paths relative to it and written with `/` (the CLI runs on Windows too), and
  `Check` refuses
  absolute ones, `..` and more than 8. `cluster apply` reads them (`Directory.Files`) and
  sends their contents along; `CheckFiles` refuses a missing one, one over 64 KiB and an
  `acme.ca` without a certificate, in the CLI and again in the state, which keeps them with
  the config (`ClusterConfig.Files`). `cluster diff` names those that differ.
- Durations (`acme.renew_before`, `alerts.heartbeat.interval`) are read by
  `units.ParseDuration`: Go's format, which may start with whole days (`30d`, `1d12h`).

## Encrypted values (`internal/secretvalue`)

A secret's value can stand in a YAML file encrypted, tagged `!secret`: age's ASCII armor
in a literal block scalar, for two recipients, the cluster's age key and the recovery key.
`cluster.yaml`'s values are such, and a `Secret`'s (in `spec.value`). The CLI never opens
a value: `clowder secret encrypt` (a Secret, or `--cluster-yaml`) encrypts
for the recipients the cluster names (`GET /v1/encryption`, asked each time, never kept
in a file someone could change), and only the cluster opens them.

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
- `Block` writes a value as it stands after its key: `!secret |` and the armor below,
  indented; `secret encrypt` and the bootstrap's `cluster.yaml` write it.
- No error message carries a value: `Open` drops json's messages, which quote the text
  they choke on.
