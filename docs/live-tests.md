# Live tests with HLab

The live tests run against HLab, a local stand-in for Hetzner Cloud with real VMs. HLab
needs `/dev/kvm`. The tests use only its interface: `hlab env` prints the API endpoint,
the token and the Docker network of a running lab. That network is one namespace shared
by every tool started in it. It holds `192.0.2.2`, where the hosts reach those tools and
from where the tools reach the hosts. Point `HLAB` at its wrapper:

```bash
export HLAB=/path/to/hlab/bin/hlab
bin/dev live         # about 20 minutes; tears everything down if it passes
bin/dev live down    # after a failure: destroy the host, stop the lab
```

Run them only at the end of a piece of work, on its finished state, and only while no one
else uses the lab `clowder-live`. Without HLab, `bin/dev all` checks everything but these
tests: lint, the unit tests, SOPS, the renderers against the real tools, the packages.

## What `bin/dev live` sets up

- It builds the packages and starts the lab `clowder-live` (`test/live/lab.yaml`, with
  ports apart from other labs).
- In the lab's network it serves the `clowderd` package over HTTP.
- It runs a private registry on `192.0.2.2:5000`, with TLS from a throw-away CA and a
  password.
- It runs a mail server for the alerts on `192.0.2.2:1025`: mailpit, with STARTTLS from
  the registry's certificate, any login, and its API on `:8025`.
- It applies `examples/hetzner` with the test hook: the package's URL and SHA-256,
  `admin_cidrs` = `192.0.2.2/32`, a third storage class `small` of 10 GB, and the
  published port `udp/9987`.
- It runs `go test -tags live ./test/live`, which gets HLab's ACME directory and CA from
  `hlab env`.

Everything a run keeps is in the Docker volume `clowder-live`: the OpenTofu state, a
throw-away SSH key and `known_hosts`, the registry's files, the cluster and stack
directories. A failed run leaves the host for a look. The next `bin/dev live` refuses to
start until `bin/dev live down` removed it.

## The steps

`TestOneHost` drives the host with the real CLI and SSH, in steps that build on each
other. Each step has a deadline (`steps` in `live_test.go`). On a failure it prints
cloud-init's status and the journals of `clowder-setup` and `clowderd`.

1. The setup makes the first host and deletes its key file.
2. The user bootstraps, fills in `cluster.yaml` from the module's outputs and applies it;
   the join keys stay in `secrets.sops.yaml`, the registry's login joins them. The host
   trusts the registry's CA (`/etc/docker/certs.d`), and the test pushes its image there,
   then logs out, so only the agent's login pulls it.
3. A stack runs from the private registry with a replicated and a plain volume, a secret
   file and a secret variable, applied as resources (`clowder apply -f`).
4. An agent restart leaves its containers running, unchanged.
5. The supervisor: a crashed container (`kill -9` of its process; `docker kill` would be a
   stop by hand) runs again within 2 s by Docker's own times, an `unhealthy` one is
   restarted, and an update of one service recreates that one, once, and no other.
6. After a reboot the stack comes back with its data, no container started before the
   agent, and `clowderd check` passes. The setup's marker is gone before, so the setup runs
   again at that boot, as after its own reboot into a newer kernel. With the agent stopped
   and a volume unmounted, its container cannot start and nothing lands on the root file
   system (the mount guard).
7. A changed secret file recreates its container only.
8. With the agent stopped, `docker compose up` fails on a missing secret file; started
   again, the agent brings the stack back.
9. A volume that fits no pool leaves its stack `pending` with the reason in `get stacks`,
   and the running stack untouched.
10. Twenty more stacks run.
11. The stack goes, then its volumes.
12. The Floating IP, moved off the host by hand through the Cloud API, comes back.
13. HTTPS: a zone `example.com` in HLab's DNS points `shop.example.com` at the Floating IP;
    the cluster orders its certificate at HLab's Pebble (HTTP-01), and the stack behind it
    answers with the test's own address in `X-Forwarded-For`. HTTP redirects to HTTPS.
14. A UDP service on a published port sees the test's own address.
15. A container reaches `https://shop.example.com` through the Floating IP (hairpin).
16. Renewal: the test sets `acme.renew_before` longer than the certificate's time left but
    shorter than its lifetime. Exactly one new certificate comes. Then it sets the value
    longer than the lifetime: no renewal, and the alert `renew-before-ignored` is open
    until the value is removed.
17. Alerts: `clowder alert test` reaches Discord and a webhook (both received by the test)
    and the mail server. The heartbeat stops while the agent is stopped and comes back
    after its start. Filling the pool of `small` beyond 80 % raises `pool-use`; deleting
    the volume brings its all-clear. The step removes its channels at the end.
18. `promtool check metrics` accepts `/metrics`, and a Prometheus on the host scrapes it.
19. Pushing out: the stack `store` has db (critical, 900), app (the stack's high, 700,
    depending on db) and cache (low, 200). `system_reserve` leaves the host 100M free
    besides them. A stack at 800 that needs 300M pushes out cache; one that needs 500M
    pushes out app too; db always stays. Deleted, they let both come back. A stack whose
    second service (priority 100) finds nothing below it starts with that service pushed
    out.
20. The memory, idle, with 21 stacks and at the end, is logged: the agent must stay under
    150 MB, the agent and Traefik together under 300 MB.

The test reads `clowder get stacks -o json` and `docker inspect`. Values it compares it
reads within a step. What carries over between steps is stated where it does: `shop`
lives from step 3 to 11, `echo` from step 13 to 19.
