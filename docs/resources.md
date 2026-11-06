# Resources

This is the reference of what you write in resource files and apply with
`clowder apply -f`. It covers the six kinds: Namespace, Volume, Stack, Secret, SealedSecret
and ClusterConfiguration. It also covers the rules that every resource follows.

- [Files and apply](#files-and-apply)
- [Names and namespaces](#names-and-namespaces)
- [Namespace](#namespace)
- [Volume](#volume)
- [Stack](#stack)
- [Secret and SealedSecret](#secret-and-sealedsecret)
- [ClusterConfiguration](#clusterconfiguration)
- [Sizes and durations](#sizes-and-durations)

## Files and apply

Every resource has the same frame, as in Kubernetes:

```yaml
apiVersion: clowder/v1
kind: Volume
metadata: {namespace: shop, name: data}
spec: {class: fast, size: 10G, replicas: 1}
```

- **`apiVersion`** is `clowder/v1`. A resource of another `clowder/` version is an error.
- **`metadata.namespace`** is left out for a Namespace and the ClusterConfiguration. For the
  other kinds it defaults to `default`.
- **Fields are read strictly.** A field that clowder does not know is an error, not ignored.
  JSON works too, because JSON is YAML.
- **`status`** and the metadata that the cluster sets (`owner`, `generation`,
  `resourceVersion`) are ignored by an apply. So you can apply what `clowder get` printed,
  except for a Secret, whose values never leave the cluster.

**What `-f` reads.** `-f` takes a file, a directory or `-` for stdin. In a directory, clowder
reads the files ending in `.yaml` or `.yml`, in the order of their names. It skips hidden
files, subdirectories and files without clowder's resources (such as a Compose file), and
says which files it skipped. A file must hold either only clowder's resources or none of them.
A file that mixes them is an error.

**Order.** clowder applies the resources in the order it reads them: the files in order of
their names, each from top to bottom. The cluster checks each resource against the
resources before it. So a resource comes after what it needs:

- a Namespace before the resources in it;
- a Secret or SealedSecret before the Stack that uses it, and before the
  ClusterConfiguration that names it;
- a Volume before the Stack that mounts it.

The `imagePullSecrets` of a Namespace are the exception. They are not checked at apply,
so the Secrets they name may come after the Namespace.

**All or nothing.** The cluster applies all resources of one apply or none. `--dry-run`
checks everything against the cluster and changes nothing. `clowder diff -f` shows what an
apply would change and ends with exit code 1 when something would change, as `kubectl diff`
does.

**Applying again.** A resource whose spec has not changed is left alone: its generation
stays, and nothing restarts. A stack spec that is written differently but means the same
counts as unchanged, for example the same `dependsOn` in another order. So applying the
same files again changes nothing.

**Deleting.** `clowder delete -f` deletes the resources of the files from the bottom up, in
the reverse order of an apply, all or none. `clowder delete <kind> [<namespace>/]<name>`
deletes one resource. Nothing cascades: a resource that another resource still uses cannot
be deleted. The sections below say what each kind allows.

## Names and namespaces

**Names.** Namespaces, volumes, stacks, services and secrets all follow one rule, the rule of a
DNS label:

- lowercase letters, digits and `-`;
- starting and ending with a letter or a digit;
- at most 63 characters.

There is no `_` and no `.` in a name. Other names have their own rules:

| What | Rule |
|---|---|
| a key of a Secret | letters, digits, `-`, `_` and `.`; at most 253 characters; not `.` or `..` (each key is a file name in the container) |
| an environment variable (`env`, `secretEnv`) | letters, digits and `_`, not starting with a digit |
| the host of an HTTP route | a domain name of at least two labels, such as `shop.example.com` |

**Namespaces** keep resources apart. Two namespaces may each have a stack, a volume and a
Secret called `shop`. Commands name a resource as `<namespace>/<name>`; a name without a
namespace means the namespace `default`.

- **`default`** exists from the start and cannot be deleted.
- **`clowder-system`** exists from the start and cannot be deleted. It holds the cluster's
  own Secrets, which the ClusterConfiguration names, like `kube-system` in Kubernetes. It
  holds no stacks and no volumes, and its Namespace takes no `imagePullSecrets`.
- **`clowder-`** is reserved. A namespace that you create must not start with it.

## Namespace

```yaml
apiVersion: clowder/v1
kind: Namespace
metadata: {name: shop}
spec:
  imagePullSecrets: [{name: registry}]
```

| Field | Meaning | Default |
|---|---|---|
| `imagePullSecrets` | registry logins: Secrets of this namespace, each named once, with the keys `server`, `username` and `password` | none |

A Namespace may have no `spec` at all. The registry logins belong to the namespace, as
Kubernetes puts `imagePullSecrets` on a namespace's default service account. The hosts pull
every image of the namespace's stacks with them. A missing Secret or key does not stop a pull,
as with the kubelet. If the pull then fails, `clowder get stacks` names the missing login.

A namespace can be deleted only when it holds no stacks, volumes or Secrets.

## Volume

```yaml
apiVersion: clowder/v1
kind: Volume
metadata: {namespace: shop, name: data}
spec: {class: fast, size: 10G, replicas: 1}
```

| Field | Meaning | Default |
|---|---|---|
| `class` | a storage class of the ClusterConfiguration | required |
| `size` | the size, such as `10G` ([sizes](#sizes-and-durations)) | required |
| `replicas` | `1` to `3`: copies on as many hosts, kept in sync by DRBD; `0`: one plain LV without DRBD | required |

- **`replicas` has no default**, because `0` and `1` mean very different things. With `0`, the
  data is not kept when the stack moves to another host. With `2`, the volume also gets a
  tiebreaker on a third host.
- **A volume cannot change yet.** An apply with another class, size or number of replicas
  for an existing volume is refused.
- **A volume serves one stack.** A second stack that mounts it is refused. Deleting the
  stack keeps the volume and its data for the next stack that mounts it.
- **Deleting a volume deletes its data.** So `clowder delete` deletes a volume only with
  `--with-data`, and only when no stack mounts it.

## Stack

A stack is a set of services that run together on one host. Its spec is clowder's own
schema, not a Compose file, although many fields are named like Compose's.

```yaml
apiVersion: clowder/v1
kind: Stack
metadata: {namespace: shop, name: web}
spec:
  priority: high
  services:
    db:
      image: postgres:17
      env: {POSTGRES_USER: shop}
      secretEnv: {POSTGRES_PASSWORD: {name: db, key: password}}
      volumes: [{volume: data, path: /var/lib/postgresql/data}]
      resources: {reserve: {memory: 512M, cpus: 0.5}}
      healthcheck: {command: [pg_isready], interval: 10s}
    app:
      image: registry.example.com/shop:1.4
      dependsOn: [db]
      secrets: [db]
      http: {host: shop.example.com, port: 8080}
```

| Field | Meaning | Default |
|---|---|---|
| `priority` | the priority of every service that names none ([priority](#priority)) | the cluster's default category |
| `services` | the services by name; at least one | required |

### Services

Every field of a service is optional except `image`.

| Field | Meaning | Default |
|---|---|---|
| `image` | the image, with its registry and tag | required |
| `command`, `entrypoint` | lists, as Docker's exec form: `[serve, --port, "8080"]` | the image's |
| `workingDir`, `user` | the working directory, and the user (such as `1000` or `app`) | the image's |
| `env` | variables by name; a value may be any YAML scalar, and its text is kept as written (`8080`, `1.0`, `yes`) | none |
| `secretEnv` | variables set to one value of a Secret: `{VARIABLE: {name: <secret>, key: <key>}}`, like Kubernetes' `secretKeyRef` | none |
| `secrets` | Secrets of the namespace as files: each key at `/run/secrets/<secret>/<key>` | none |
| `volumes` | volumes of the namespace: `{volume, path, readOnly}` | none |
| `tmpfs` | file systems in memory: `{path, size}` | none |
| `localtime` | `true` mounts the host's `/etc/localtime`, read-only | `false` |
| `ports` | ports published on the cluster's Floating IP: `{port, published, protocol}` ([ports](#ports-and-http-routes)) | none |
| `http` | one HTTP route through the cluster's ingress: `{host, port, path}` ([routes](#ports-and-http-routes)) | none |
| `dependsOn` | other services of the stack that this one starts after | none |
| `healthcheck` | `{command, interval, timeout, startPeriod, retries}` | none |
| `restart` | `always`, `no`, `on-failure` or `on-failure:<n>` with `n` above 0 | `always` |
| `restartUnhealthy` | restart the container when its healthcheck fails | `true` |
| `resources` | `reserve` and `limit`, each `{memory, cpus}` ([resources](#resources-and-placement)) | the cluster's reservation |
| `stopGracePeriod` | how long a stop waits before it kills, such as `30s` | Docker's |
| `stopSignal` | the signal that stops the container, such as `SIGINT` | the image's |
| `readOnly` | `true` mounts the container's root file system read-only | `false` |
| `init` | `true` runs an init process as PID 1 | `false` |
| `shmSize` | the size of `/dev/shm`, such as `128M` | Docker's |
| `capAdd`, `capDrop` | Linux capabilities, with or without `CAP_`, in any case | none |
| `privileged` | a privileged container ([privileges](#privileges)) | `false` |
| `securityOpt` | Docker's security options | none |
| `priority` | this service's own priority ([priority](#priority)) | the stack's |

What would tie a container to one host or let it into the host has no field. There are no
bind mounts except `localtime`, no devices and no sharing of the host's namespaces. clowder
sets the container names, the network and the restart policy itself.

**Variables and secrets.** A variable is in `env` or in `secretEnv`, not in both. A file
from `secrets` is the safer way: the value of a `secretEnv` variable also ends up in
Docker's container configuration on the host's disk and in `docker inspect`; a file does
not. Images that read `*_FILE` variables work with the files. A stack names at most 64
Secrets in all. A Secret named in `secretEnv` must have the key; a Secret named in
`secrets` may have any keys.

**Mounts.** Each path of `volumes`, `tmpfs` and `localtime` is a clean absolute path
(`/data`, not `/data/` or `data`), and no path is mounted twice in one service. Docker
mounts an outer path before an inner one, in whatever order you write them.

**Healthchecks.** `command` is required and runs in the container in exec form; for a
shell, write `[sh, -c, "..."]`. `interval`, `timeout` and `startPeriod` are positive
durations such as `10s`. `retries` is 0 or more. A field that you leave out takes Docker's
default.

**Dependencies.** `dependsOn` names other services of the same stack, and the services
must not depend on each other in a circle. A service that depends on a service with a
healthcheck starts once that service is healthy. `clowder stack restart` also restarts
every service that depends on a restarted one.

**Restarts.** clowder restarts the containers itself, by the `restart` rule; Docker
never restarts them. A healthcheck that fails restarts the container unless
`restartUnhealthy` is `false`.

### Ports and HTTP routes

```yaml
ports: [{port: 9987, protocol: udp}, {port: 8443, published: 9443}]
http: {host: shop.example.com, port: 8080, path: /api}
```

| Field | Meaning | Default |
|---|---|---|
| `ports[].port` | the container's port | required |
| `ports[].published` | the port on the Floating IP | `port` |
| `ports[].protocol` | `tcp` or `udp` | `tcp` |
| `http.host` | the domain name of the route, in any case | required |
| `http.port` | the container's port that serves HTTP | required |
| `http.path` | the route takes the requests below this path; it starts with `/` and holds only the characters of a URL path | `/` |

- **A published port belongs to one service** in the whole cluster, by protocol and port.
  The same holds for an HTTP route, by host and path.
- **Reserved ports.** The host and clowder use these ports, so no stack may publish them:
  22, 53, 80, 443, 7400 to 7499, 10000 to 10999, 20000 to 29999, and 51800 to 51831.
- **An HTTP route gets a certificate** from Let's Encrypt (or the ACME directory of the
  ClusterConfiguration) once the domain's DNS points at the Floating IP. The service
  sees the client's address in `X-Forwarded-For`.

### Resources and placement

```yaml
resources:
  reserve: {memory: 512M, cpus: 0.5}
  limit: {memory: 1G, cpus: 2}
```

- **`reserve`** is what the scheduler sets aside for the service on its host. It places
  stacks by these reservations. A service without its own memory or CPUs reserves the
  cluster's `defaults.reservations` (128M and 0.1 CPUs unless the ClusterConfiguration says
  otherwise). A value of 0 also takes the default, so no service reserves nothing.
- **`limit`** is the most that the container may use.
- `memory` is a [size](#sizes-and-durations); `cpus` is a number such as `0.5`, and not
  negative.

### Priority

A priority is a whole number from 0 to 1000, or the name of a category of the
ClusterConfiguration. A service takes its own `priority`, else the stack's, else the
cluster's default category. Without its own categories, the cluster has `critical` (900),
`high` (700), `normal` (500) and `low` (200), and the default is `normal`.

On full hosts, stacks of lower priority move to make room, or a new stack pushes out
services of lower priority, which come back when there is room. An apply that names a
category the cluster does not have is refused.

### Privileges

Some fields reach beyond the container. Such a container can read the host's keys and the
volumes and Secrets of other stacks. So the cluster allows them only for the stacks that
the ClusterConfiguration names in `stacks.privileged`. A service has privileges when it has:

- `privileged: true`;
- a `capAdd` outside these: `AUDIT_WRITE`, `CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `FSETID`,
  `IPC_LOCK`, `KILL`, `MKNOD`, `NET_ADMIN`, `NET_BIND_SERVICE`, `NET_BROADCAST`, `NET_RAW`,
  `SETFCAP`, `SETGID`, `SETPCAP`, `SETUID`, `SYS_CHROOT`, `SYS_NICE`;
- a `securityOpt` other than `no-new-privileges` (or `no-new-privileges:true`).

### Changing and deleting a stack

An apply with a changed spec recreates only the containers whose configuration changed.
A container that uses a changed Secret is recreated with the new values. Deleting a stack
stops its containers and keeps its volumes.

## Secret and SealedSecret

A Secret's values come in one of two ways, as in Kubernetes with Bitnami's sealed secrets.
Both become the same Secret in the cluster, so a stack does not care which way it came.

**Secret**, with the values in clear. Keep such a file out of Git.

```yaml
apiVersion: clowder/v1
kind: Secret
metadata: {namespace: shop, name: db}
spec:
  stringData: {password: s3cret}
  data: {cert: LS0tLS1CRUdJTi...}
```

| Field | Meaning |
|---|---|
| `data` | values in Base64, by key |
| `stringData` | values as text, by key; for a key in both, `stringData` wins |

Unlike in Kubernetes, both stand under `spec`. The CLI sends the values over its encrypted
connection, and the cluster encrypts them before it stores them.
`clowder create secret [<namespace>/]<name> --from-literal <key>=<value> --from-file
<key>=<path|->` creates a Secret without a file. Like `kubectl create`, it refuses a Secret
that exists.

**SealedSecret**, with each value sealed for the cluster. It can go into Git.

```yaml
apiVersion: clowder/v1
kind: SealedSecret
metadata: {namespace: shop, name: db}
spec:
  encryptedData:
    password: YWdlLWVuY3J5cHRpb24ub3JnL3YxCi0+IFgyNTUxOSAu...
```

| Field | Meaning |
|---|---|
| `encryptedData` | sealed values by key, each one line of Base64 |

`clowder secret seal` writes SealedSecrets, as kubeseal does: from the Secrets of a file
(`-f <file|->`), or from `--from-literal` and `--from-file` for one Secret. Each value is
sealed for one Secret, by namespace and name; another Secret refuses it. The apply turns a
SealedSecret into the Secret of the same namespace and name. `get`, `delete` and `diff`
show and act on that Secret. Unlike Bitnami's, a SealedSecret has no `template`.

**Rules for both:**

- A Secret has at least one key, and no value is empty.
- Keys follow the [rule of a key](#names-and-namespaces).
- Limits, counted on the values in clear: 64 KiB per Secret, 256 KiB per namespace, 4 MiB
  and 1000 Secrets in the cluster.
- A change must keep every key that a stack's `secretEnv` or the ClusterConfiguration
  names.
- A Secret cannot be deleted while a stack of its namespace or the ClusterConfiguration
  names it.

## ClusterConfiguration

The cluster's settings are one resource without a namespace, always named `cluster`, like
kubeadm's ClusterConfiguration. It holds no secrets. Instead, it names values of Secrets in
`clowder-system` as `{name, key}`. Apply these Secrets above the configuration, in the same
apply or an earlier one.

```yaml
apiVersion: clowder/v1
kind: ClusterConfiguration
metadata: {name: cluster}
spec:
  hetzner:
    tokenSecretRef: {name: hetzner, key: token}
  privateNetwork: 10.0.0.0/16
  ingress: {floatingIP: 203.0.113.10}
  storageClasses:
    fast: {pool: vg-fast/thin}
    bulk: {pool: vg-bulk/thin}
```

| Field | Meaning | Default |
|---|---|---|
| `hetzner.tokenSecretRef` | the Hetzner Cloud project's API token | required |
| `hetzner.api` | the full `http://` or `https://` URL of another Hetzner API, for test labs | Hetzner's API |
| `privateNetwork` | the Hetzner private network of the hosts, an IPv4 network such as `10.0.0.0/16`; it must not overlap `10.77.0.0/24` or `10.78.0.0/16`, which clowder uses | required |
| `ingress.floatingIP` | the Floating IP, an IPv4 address | required |
| `ingress.preferredHosts` | host names in order of preference for the Floating IP; it stays on a host that can hold it | none |
| `storageClasses` | the storage classes by name; at least one | required |
| `storageClasses.<class>.pool` | a thin pool on each host, as `<volume group>/<thin pool>`; the volume group has at most 19 characters | |
| `storageClasses.<class>.reserve` | the part of the pool kept free for snapshots and metadata, `0%` to `90%` | `20%` |
| `storageClasses.<class>.fallback` | other classes, each with a pool, to use in turn instead of a pool of its own | |
| `replication.resyncRate` | the most that a resync of a volume copy sends per second, as a size; the cluster starts a new copy only while no other copy resyncs | `50M` |
| `priorities.categories` | priority categories by name, each 0 to 1000 | `critical` 900, `high` 700, `normal` 500, `low` 200 |
| `priorities.default` | the category of services that name no priority; required with your own categories | `normal` |
| `stacks.privileged` | the stacks, as `<namespace>/<stack>`, that may have [privileges](#privileges) | none |
| `defaults.reservations` | what a service reserves without its own `reserve`: `{memory, cpus}` | `128M`, `"0.1"` |
| `systemReserve` | what each host keeps for itself (the system, Docker, clowder): `{memory, cpus}` | `512M`, `"0.25"` |
| `alerts.channels` | where alerts go ([alerts](#alerts)) | none |
| `alerts.heartbeat` | a URL that the cluster calls at an interval ([alerts](#alerts)) | none |
| `acme.directory` | the full URL of another ACME directory, for test labs | Let's Encrypt |
| `acme.ca` | a PEM certificate to trust for the directory, at most 64 KiB | none |
| `acme.email` | the ACME account's contact address | none |
| `acme.renewBefore` | how long before its end a certificate is renewed, such as `30d` | when a third of its lifetime is left |

A storage class has either a `pool` or a `fallback`, not both. In `defaults.reservations`
and `systemReserve`, write `cpus` as text in quotes (`"0.1"`), unlike a stack's `cpus`.

When `hetzner.api` or `acme.directory` is set, an apply warns that clowder talks to another
service than Hetzner's or Let's Encrypt's. Over `http://`, the token or the ACME requests
cross the network unencrypted.

### Alerts

```yaml
alerts:
  channels:
    - {type: discord, minSeverity: warning, webhookSecretRef: {name: alerts, key: discord}}
    - type: email
      smtp: {host: mail.example.com, port: 465, user: clowder,
             passwordSecretRef: {name: alerts, key: smtp}}
      from: clowder@example.com
      to: [ops@example.com]
  heartbeat: {urlSecretRef: {name: alerts, key: heartbeat}, interval: 5m}
```

| Field | Meaning | Default |
|---|---|---|
| `type` | `discord`, `email` or `webhook` | required |
| `minSeverity` | the lowest severity sent: `info`, `warning` or `critical` | `info` |
| `webhookSecretRef` | a `discord` channel's webhook URL | required for `discord` |
| `urlSecretRef` | a `webhook` channel's URL | required for `webhook` |
| `smtp` | an `email` channel's server: `{host, port, user, passwordSecretRef, ca}`; port 465 uses TLS from the start, any other port must offer STARTTLS; `ca` is a PEM certificate to trust, at most 64 KiB | required for `email` |
| `from`, `to` | an `email` channel's sender and recipients | required for `email` |
| `heartbeat.urlSecretRef` | the URL that the cluster calls; a service outside raises an alarm when the calls stop | required |
| `heartbeat.interval` | how often it calls | `1m` |

A channel has only its own type's fields. The URLs are Secrets, because whoever has a
webhook's or the heartbeat's URL can post to it or fake it.

### Changing the configuration

- The configuration can be changed but not deleted, because fencing and the Floating IP
  need it.
- An apply that would break what the cluster has is refused: dropping a storage class that
  a volume uses, a priority category that a stack names, or a stack in `stacks.privileged`
  that has privileges; or a private network that no longer holds a host.
- An apply that changes the Hetzner token, in the configuration or in the Secret it names,
  first says how many of the hosts' servers the new token sees and asks before it goes on.
  `--yes` skips the question.
- Before the first configuration, the cluster has no Hetzner token, no certificates and no
  alerts.

## Sizes and durations

- **Sizes** are a whole number and a unit: `K`, `M`, `G` or `T`, in steps of 1024
  (`512M`, `10G`). A volume shows its size in the largest whole unit, so `1024M` shows as
  `1G`.
- **Durations in a stack** are like `10s`, `1m30s` or `2h`.
- **Durations in the ClusterConfiguration** may also start with whole days: `30d`, `1d12h`.
