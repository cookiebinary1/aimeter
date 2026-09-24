# Contributing

Thanks for taking the time. Bug reports, provider additions and layout fixes
are all welcome.

## Getting started

```sh
git clone https://github.com/cookiebinary1/aimeter
cd aimeter
go build -o aimeter .
./aimeter -show-creds     # prints where each credential resolved from
```

`-show-creds` never prints key values, so its output is safe to paste into an
issue.

## Before opening a pull request

```sh
gofmt -l .                # must print nothing
go vet ./... && go vet -tags omp ./...
go test ./...
go build -tags omp .      # the optional OMP build must keep compiling
```

CI runs the Go checks on Linux, macOS and Windows. On Linux/macOS it also
checks the shell installer with `sh -n install.sh` and
`python3 tests/test_install.py`. Installer tests use local fixtures and make
no network requests.

## Conventions

- Code comments and all user-facing strings are **English**.
- Keep the binary dependency-light: no cgo, no runtime beyond the Go
  standard library and the Bubble Tea/Lip Gloss stack already in `go.mod`.
- Credentials are **read-only**. A provider must never write, refresh or
  transmit a key anywhere except to that provider's own API.
- New behaviour needs a test that fails without the change. Layout work is
  covered by `layout_test.go`, which asserts panels never overflow or wrap
  between 24 and 100 columns.

## Adding a provider

Two options, in order of preference:

1. **No code** — if the service exposes usage as JSON, describe it as a
   [custom provider](README.md#custom-providers) in `credentials.json`. No PR
   needed.
2. **Built-in** — add a `fetchX(ctx, Creds) ([]Gauge, string, error)` to
   `main.go`, register it in `fetchAll`, and resolve its key in
   `credentials.go` through the existing chain (env → config → keychain).
   Add the env var name to the README table.

Return gauges with `Used` in 0-100, or `Used: -1` when the service reports
only a balance. Set `Window` and `Reset` when a quota window is known so the
elapsed-time bar can render.

## Reporting bugs

Include the output of `aimeter -show-creds`, your terminal width if the issue
is visual, and the `aimeter -version` string.
