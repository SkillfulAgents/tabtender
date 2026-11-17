# Changelog

All notable changes are recorded here. Versions follow
[semantic versioning](https://semver.org); until 1.0, minor versions may
change behavior.

## Unreleased — first beta

The first version intended for use outside development.

### Sessions and CDP

- Native Chrome sessions over an authenticated REST and CDP WebSocket API,
  with TTLs, concurrency limits, and ephemeral or named persistent profiles.
- Per-session tokens: a session's `cdpUrl` carries a credential for that
  session alone, so CDP clients connect with the URL and nothing else.
- Browsers are driven through a private debugging pipe instead of a debugging
  port. No local process can reach a session's browser except through the
  authenticated endpoint, and browsers exit with the service.
- Every CDP connection is independent; several clients can share a session.
- Sessions expire by the wall clock, also across system sleep.

### Service

- macOS: menu-bar app that registers and manages a per-user launchd agent.
- Windows: service running as the unprivileged `NT SERVICE\tabtenderd`
  account, with in-place upgrades.
- Optional Cloudflare quick and named tunnels, supervised with backoff.
- Configuration through file, environment, and a local management API, with
  live reload.

### Known limitations

- macOS builds are not notarized; see the README for the one-time step.
- Sessions are not sandboxed: controlling a browser over CDP includes file
  access as the service's user.
- Linux builds and passes CI but is not a packaged release target.
