# Contributing

Issues and pull requests are welcome. For a security problem, follow
[SECURITY.md](SECURITY.md) instead of opening an issue.

## Layout

The [Architecture section of the README](README.md#architecture) maps the
repository. The short version: `tabtenderd` (Go, `cmd/` and `internal/`) owns
all behavior; the macOS app (`apps/macos`) is a client of its management API.

## Building and testing

You need Go (the version in `go.mod`), Node 22 for the client suite, and
Google Chrome. The macOS app needs Xcode 16 or later.

```sh
make build      # bin/tabtenderd
make test       # unit tests, with the race detector
make e2e        # real tabtenderd against a real browser
make clients    # Playwright and Puppeteer against a real browser
make lint       # gofmt, go vet, golangci-lint
```

For the app: `cd apps/macos && swift test` (set `TABTENDERD_PATH` to a built
daemon to include the integration tests), and `make app` for a bundle.

`packaging/macos/e2e-launchd.sh` and `packaging/windows/e2e-service.ps1`
register a real background service on the machine they run on. They are meant
for CI; run them locally only on a machine where that is acceptable.

## Expectations for a change

- Tests come with the change. Behavior a user can observe gets an end-to-end
  test (`test/e2e`, or `test/clients` when it concerns CDP clients); logic
  gets unit tests next to it.
- CI must pass on macOS, Windows, and Linux. `GOOS=windows go vet ./...` and
  `GOOS=linux go vet ./...` catch most cross-platform breakage locally.
- Public API changes update `docs/api.md` and `docs/openapi.yaml`.
- Comments explain why, not what.
- Secrets never reach logs, URLs other than a session's `cdpUrl`, or child
  processes.

By contributing you agree that your contribution is licensed under the
project's [MIT license](LICENSE).
