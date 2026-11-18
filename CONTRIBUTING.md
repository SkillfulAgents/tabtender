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

## Releasing

Pushing a version tag on a commit of `main` runs the
[Release workflow](.github/workflows/release.yml): the full CI suite, then
signed builds, then a GitHub release with `Tabtender.dmg`, the two Windows
archives, and `SHA256SUMS`.

```sh
git tag v0.1.0-rc.1 && git push origin v0.1.0-rc.1   # a pre-release
git tag v0.1.0 && git push origin v0.1.0             # a release; becomes "latest"
```

Move the "Unreleased" notes in `CHANGELOG.md` under the new version first.
Running the workflow by hand (**Actions → Release → Run workflow**) is a
rehearsal: the same builds, kept as workflow artifacts, nothing published.

A tag is never published unsigned; the workflow fails early when one of these
repository secrets is missing:

| Secret | What it is |
| --- | --- |
| `CSC_LINK` | The Developer ID Application certificate with its private key, as a base64-encoded `.p12` |
| `CSC_KEY_PASSWORD` | The password of that `.p12` |
| `APPLE_API_KEY` | An App Store Connect API key for notarization, as a base64-encoded `.p8` |
| `APPLE_API_KEY_ID`, `APPLE_API_ISSUER` | That key's ID and its issuer ID |
| `AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, `AZURE_CLIENT_SECRET` | An app registration allowed to sign with the Azure Artifact Signing account |
| `AZURE_CODE_SIGNING_ENDPOINT`, `AZURE_CODE_SIGNING_ACCOUNT`, `AZURE_CODE_SIGNING_PROFILE` | The signing account's endpoint URL, its name, and the certificate profile |

By contributing you agree that your contribution is licensed under the
project's [MIT license](LICENSE).
