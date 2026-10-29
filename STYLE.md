# Style

Official standards first, checked by `bin/dev lint`; `bin/dev format` applies what the
formatters can fix.

| Files | Standard | Tool |
|---|---|---|
| Go | [Effective Go](https://go.dev/doc/effective_go), [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) | `gofumpt`, `goimports`, `golangci-lint` (`.golangci.yml`) |
| Bash | [Google Shell Style Guide](https://google.github.io/styleguide/shellguide.html) | `shfmt -i 2 -ci -bn`, `shellcheck` |
| OpenTofu | [OpenTofu/Terraform style conventions](https://developer.hashicorp.com/terraform/language/style) | `tofu fmt` |
| YAML | yamllint defaults | `yamllint` (`.yamllint`) |
| Commits | [Git's conventions](https://git-scm.com/docs/SubmittingPatches#describe-changes): imperative subject (~50 characters), blank line, why | by review |

## Rules on top of the standards

- English throughout.
- Readable before short: descriptive names (`host`, `volume`, `stack`, not `h`, `v`, `s`);
  single letters only for loop variables, receivers and one-line closures. Understandable
  abbreviations (`cfg`, `ip`, `cmd`, IDs like `hostID`) are fine.
- One job per function; no `Run()` that only calls `run()`.
- Comments and doc comments say why (DRBD's or the kernel's behavior, a caveat); the code says
  what. Doc comments are sentences; other comments may be short lowercase fragments.
- Imports in three groups: standard library, others, this module (`goimports` with the local
  prefix sorts them).
- Errors: wrap with `fmt.Errorf("what was tried: %w", err)`; compare with `errors.Is` and
  `errors.As`. Messages start lowercase and have no trailing period.
- Logging: `log/slog` with key–value pairs (`"host", name`), never `fmt.Print` for logs.
- Tests: next to the code (`foo_test.go`), names say what they check
  (`TestVersionPrintsTheBuildVersion`); table tests where cases differ only in data; renderers
  of configuration (nft, DRBD, Traefik, Compose) are tested against golden files in
  `testdata/`.
- Shell scripts start with a comment that says what they do and how to call them; every
  function has a header comment.
