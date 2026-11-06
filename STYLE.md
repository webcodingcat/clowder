# Style

The official standards come first, and `bin/dev lint` checks them; `bin/dev format`
applies what the formatters can fix.

| Files | Standard | Tool |
|---|---|---|
| Go | [Effective Go](https://go.dev/doc/effective_go), [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) | `gofumpt`, `goimports`, `golangci-lint` (`.golangci.yml`) |
| Bash | [Google Shell Style Guide](https://google.github.io/styleguide/shellguide.html) | `shfmt -i 2 -ci -bn`, `shellcheck` |
| OpenTofu | [OpenTofu/Terraform style conventions](https://developer.hashicorp.com/terraform/language/style) | `tofu fmt` |
| YAML | yamllint defaults | `yamllint` (`.yamllint`) |
| Commits | [Git's conventions](https://git-scm.com/docs/SubmittingPatches#describe-changes): imperative subject (~50 characters), blank line, why | by review |

## Rules on top of the standards

- English throughout.
- Readable before short: use descriptive names (`host`, `volume`, `stack`, not `h`, `v`,
  `s`). Use single letters only for loop variables, receivers and one-line closures.
  Understandable abbreviations (`cfg`, `ip`, `cmd`, IDs like `hostID`) are fine. One
  exception: a test fixture that the whole test works through, the way it would work
  through a receiver (`c := newCluster(t)` in the state's tests, `l` for the controller's
  leader).
- One job per function; no `Run()` that only calls `run()`.
- Comments and doc comments say why (DRBD's or the kernel's behavior, a caveat); the code
  says what. Doc comments are sentences; other comments may be short lowercase fragments.
- Imports in three groups: standard library, others, this module (`goimports` with the
  local prefix sorts them).
- JSON and YAML names: what users write and read as resources (`resource.Object`, the
  specs, the statuses that `get` shows) is camelCase, as in Kubernetes. Everything else
  (commands, the state, snapshots, the API's other bodies, the CLI's credentials file) is
  snake_case.
- Errors: wrap them with `fmt.Errorf("what was tried: %w", err)`, and compare them with
  `errors.Is` and `errors.As`. Messages start lowercase and have no trailing period.
- Logging: use `log/slog` with key–value pairs (`"host", name`), never `fmt.Print` for logs.
- Tests: they live next to the code (`foo_test.go`), and their names say what they check
  (`TestVersionPrintsTheBuildVersion`). Use table tests where cases differ only in data.
  Renderers of configuration (nft, DRBD, Traefik, Compose) are tested against golden files
  in `testdata/`.
- Shell scripts start with a comment that says what they do and how to call them; every
  function has a header comment.
