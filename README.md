<p align="center">
  <img src="docs/assets/logo.png" alt="Tabtender" width="240">
</p>

# Tabtender

[![CI](https://github.com/SkillfulAgents/tabtender/actions/workflows/ci.yml/badge.svg)](https://github.com/SkillfulAgents/tabtender/actions/workflows/ci.yml)
[![Latest release](https://img.shields.io/github/v/release/SkillfulAgents/tabtender)](https://github.com/SkillfulAgents/tabtender/releases/latest)

**Download the latest release:**
[macOS](https://github.com/SkillfulAgents/tabtender/releases/latest/download/Tabtender.dmg) ·
[Windows x64](https://github.com/SkillfulAgents/tabtender/releases/latest/download/tabtender-windows-amd64.zip) ·
[Windows ARM64](https://github.com/SkillfulAgents/tabtender/releases/latest/download/tabtender-windows-arm64.zip) ·
[all releases and pre-releases](https://github.com/SkillfulAgents/tabtender/releases)

Tabtender is an open-source browser session server. It runs **native browser
processes** on macOS and Windows — no Docker — and exposes each one to remote
AI agents through the Chrome DevTools Protocol (CDP), behind a single
authenticated HTTP/WebSocket endpoint.

- Every session is its own Chrome process with its own profile, CDP endpoint,
  and lifecycle.
- Works directly with Playwright, Puppeteer, and raw CDP clients.
- API-key authentication, session TTLs, concurrency limits.
- Optional public access through a Cloudflare tunnel (one setting).
- macOS: a menu-bar app installs and manages the background service.
  Windows: a native Windows Service.

```
┌────────────┐  HTTPS / WSS   ┌─────────────────────────────┐  private pipe  ┌────────┐
│ AI agent   │ ─────────────▶ │ tabtenderd                  │ ─────────────▶ │ Chrome │
│ Playwright │  Bearer key    │ REST API · CDP proxy · TTLs │                ├────────┤
│ Puppeteer  │ ◀───────────── │ sessions · tunnel · auth    │ ─────────────▶ │ Chrome │
└────────────┘                └─────────────────────────────┘                └────────┘
```

## Quick start

### macOS

1. Download
   [`Tabtender.dmg`](https://github.com/SkillfulAgents/tabtender/releases/latest/download/Tabtender.dmg)
   (a universal build, signed and notarized), open it, and drag **Tabtender**
   to Applications. Google Chrome must be installed.
2. Open Tabtender from Applications. A menu-bar icon appears, and the app
   registers and starts the `tabtenderd` background service (a per-user
   launchd agent — no root). macOS announces the new background item.
3. Open **Settings → API Key** to copy your key.

Open the app from the Applications folder, not from the disk image: the
service is registered at the app's location, so the app declines to set it up
from a place that will disappear.

The service keeps running when you quit the menu-bar app, restarts after a
crash, and starts at login (configurable). The menu-bar app itself does not
start at login; open it whenever you want to look at the service.

A sleeping Mac answers no requests. By default the service keeps the Mac
awake only while sessions are active; to keep it reachable between sessions
too, set **Settings → General → Keep this Mac awake** to **Always**
(`app.keepAwake`). The display can still sleep, and a laptop still sleeps
when its lid is closed.

The same place lists the macOS settings that would still take the service
offline — the Mac's own sleep timers while "Always" is off, and automatic
logout after inactivity — each with a button that opens the setting in
System Settings.

**Updating.** Replace the app and open it once. macOS ties the background
service to the exact build that registered it, so the app registers the
service again when it finds an older daemon running or launchd refusing to
start the new one (this restarts the service and ends its sessions).

**Uninstalling.** See [Uninstalling](#uninstalling).

If the service does not start, look at
`~/Library/Application Support/Tabtender/logs/`: `tabtenderd.log` is the
service log, and `tabtenderd.crash.log` holds what it printed if it could not
start at all (for example, a configuration file it cannot read or a port
that is taken).

### Windows

Download and extract
[`tabtender-windows-amd64.zip`](https://github.com/SkillfulAgents/tabtender/releases/latest/download/tabtender-windows-amd64.zip)
(or [`-arm64`](https://github.com/SkillfulAgents/tabtender/releases/latest/download/tabtender-windows-arm64.zip)),
then from an
elevated Command Prompt or PowerShell window in that folder:

```powershell
.\install.cmd
Get-Content "$env:ProgramData\Tabtender\secrets\initial-api-key.txt"   # one-time key retrieval
```

`install.cmd` runs `install.ps1` with the execution policy lifted for that one
run; Windows otherwise refuses to run scripts from a downloaded zip. (If a
Group Policy enforces the execution policy, unblock the two `.ps1` files with
`Unblock-File` and run `.\install.ps1` directly.)

This installs `tabtenderd` as a Windows Service that starts at boot and runs
as its own unprivileged account, `NT SERVICE\tabtenderd`. Its data directory,
`%ProgramData%\Tabtender`, is accessible only to that account, SYSTEM, and
Administrators. To supply your own key instead, use
`.\install.cmd -ApiKey <key>`.

To upgrade, extract the new zip and run `.\install.cmd` again: the API key,
configuration, and browser profiles are kept. To remove it, see
[Uninstalling](#uninstalling).

A Windows Service runs in the non-interactive session 0, so sessions are
**headless** there. For headed browsers, stop the service and run
`tabtenderd.exe` from a console in your own session, with
`TABTENDER_DATA_DIR` pointing at a directory of your own.

### From source (any platform)

```sh
go run ./cmd/tabtenderd
```

The generated key is in the OS credential store; read it over the local
management socket (macOS and Linux):

```sh
curl --unix-socket ~/Library/Application\ Support/Tabtender/tabtenderd.sock http://tabtender/mgmt/v1/apikey   # macOS
curl --unix-socket ~/.local/share/tabtender/tabtenderd.sock http://tabtender/mgmt/v1/apikey                   # Linux
```

Or pin a key of your own (at least 32 characters): `TABTENDER_API_KEY=... go run ./cmd/tabtenderd`.

## Uninstalling

### macOS

1. In the app, choose **Settings → General → Unregister**. This stops the
   background service and ends its sessions.
2. Quit the app (**Quit** in the menu-bar popover).
3. Move `/Applications/Tabtender.app` to the Trash.

Do the first step before deleting the app. If the app is already gone and
macOS still lists the background item, remove it under **System Settings →
General → Login Items & Extensions**.

This keeps your data, so installing again picks up the same API key,
configuration, and profiles. To remove every trace as well:

```sh
# Configuration, named browser profiles, and logs
rm -rf ~/Library/Application\ Support/Tabtender

# The API key and, if you set one, the Cloudflare tunnel token (login Keychain)
security delete-generic-password -s com.skillfulagents.tabtender -a api-key
security delete-generic-password -s com.skillfulagents.tabtender -a cloudflare-tunnel-token

# The app's own preferences. Left behind, they make a later install wait for
# you to choose Register instead of setting the service up by itself.
defaults delete com.skillfulagents.tabtender
```

### Windows

From an elevated Command Prompt or PowerShell window in the folder you
installed from:

```powershell
.\uninstall.cmd              # removes the service, keeps the data
.\uninstall.cmd -PurgeData   # also deletes %ProgramData%\Tabtender: the API key, configuration, and profiles
```

## Using it

```js
import { chromium } from 'playwright';

const api = 'http://127.0.0.1:3000';
const headers = { Authorization: `Bearer ${apiKey}` };

const session = await (await fetch(`${api}/v1/sessions`, {
  method: 'POST',
  headers,
  body: JSON.stringify({ ttlSeconds: 1800 }),
})).json();

// cdpUrl carries a token for this one session, so it works on its own.
const browser = await chromium.connectOverCDP(session.cdpUrl);
const page = await browser.contexts()[0].newPage();
await page.goto('https://example.com');

await browser.close();                                   // disconnects; the session keeps running
await fetch(`${api}/v1/sessions/${session.id}`, { method: 'DELETE', headers });
```

Puppeteer: `puppeteer.connect({ browserWSEndpoint: session.cdpUrl })`.

The `cdpUrl` ends in `?token=…`, a credential scoped to that one session. Hand
it to an agent or any CDP client that cannot set headers — at the API it opens
that session's CDP endpoint and nothing else, so the API key never has to
leave your orchestrator. Mind what CDP itself allows, though: see
[Security](#security).

## API

Every request must carry `Authorization: Bearer <api key>`. The one exception
is a session's CDP WebSocket, which accepts either the API key header or the
session's own token (already part of its `cdpUrl`). This is a summary; the full reference,
including the local management API, is in [docs/api.md](docs/api.md), and the
public API is also described in [docs/openapi.yaml](docs/openapi.yaml).

| Method   | Endpoint                 | Purpose                                 |
| -------- | ------------------------ | --------------------------------------- |
| `POST`   | `/v1/sessions`           | Create a browser session                |
| `GET`    | `/v1/sessions`           | List active sessions                    |
| `GET`    | `/v1/sessions/{id}`      | Session status (also for recently ended sessions) |
| `DELETE` | `/v1/sessions/{id}`      | Terminate a session                     |
| `WS`     | `/v1/sessions/{id}/cdp`  | CDP WebSocket proxy                     |
| `GET`    | `/v1/health`             | Health, capacity, tunnel status         |

**Create a session** — all fields optional:

```json
{ "ttlSeconds": 1800, "headless": false, "profile": "my-persistent-profile" }
```

```json
{
  "id": "sess_pdba4wpt6fg3qx4bgjlmzhn5de",
  "status": "running",
  "headless": false,
  "createdAt": "2026-11-15T19:30:00Z",
  "expiresAt": "2026-11-15T20:00:00Z",
  "cdpUrl": "wss://browser.example.com/v1/sessions/sess_pdba4wpt6fg3qx4bgjlmzhn5de/cdp?token=Zk3…",
  "connectedClients": 0
}
```

Errors look like `{"error": {"code": "validation_error", "message": "..."}}`:

| Status | Code                    | Meaning                                         |
| ------ | ----------------------- | ----------------------------------------------- |
| 400    | `validation_error`      | Bad request (for example a TTL above the maximum) |
| 401    | `unauthorized`          | Missing or wrong API key (or session token, on the CDP endpoint) |
| 404    | `session_not_found`     | Unknown session                                 |
| 409    | `profile_in_use`        | The named profile is locked by another session  |
| 409    | `session_not_running`   | CDP connection to a session that has ended      |
| 429    | `capacity_exceeded`     | The concurrent session limit is reached         |
| 502    | `browser_launch_failed` | The browser could not be started                |

### Session lifecycle

- A session lives until its TTL expires or it is deleted — **not** until its
  client disconnects. Clients may disconnect and reconnect freely; tabs,
  cookies, and storage stay as they were.
- Ending a session shuts the browser down cleanly (falling back to a kill
  after a grace period). Expiry follows the wall clock: a session whose TTL
  ran out while the machine was asleep ends when it wakes. As with any browser, state written in the last
  instant before shutdown may not have reached the disk yet — give a named
  profile a moment after your final write before deleting its session.
- Sessions without a `profile` use an ephemeral profile that is deleted when
  the session ends. Named profiles persist on disk and are exclusively locked
  while in use.
- When a session ends (TTL, delete, browser crash, service stop), the whole
  browser process tree is terminated, CDP connections are closed, and
  temporary files are removed. `GET /v1/sessions/{id}` keeps reporting
  `status: "terminated"` with an `endReason` (`deleted`, `expired`, `crashed`,
  `shutdown`) for ten minutes.
- Sessions do not survive a restart of the service, and browsers do not
  outlive it: a browser exits when the service does, even if the service is
  killed. At startup, `tabtenderd`
  reconciles: browsers left behind by a previous run are terminated — only
  after verifying, from their command line, that they belong to this
  installation — and ephemeral profiles are removed. Named profiles are kept.
- Every CDP connection is independent: several clients can work in one
  session at once, each as if it had connected to the browser by itself.
- Browsers are driven through a private debugging pipe, never a debugging
  port, so nothing on the machine can reach a session's browser except
  through Tabtender's authenticated endpoint. `browser.extraArgs` that would
  open a debugging port are rejected.

## Configuration

Resolution order: **environment variables > configuration file > defaults.**

| Platform | Configuration file                                        |
| -------- | --------------------------------------------------------- |
| macOS    | `~/Library/Application Support/Tabtender/config.yaml`     |
| Windows  | `%ProgramData%\Tabtender\config.yaml`                     |
| Linux    | `~/.config/tabtender/config.yaml`                         |

```yaml
server:
  host: 127.0.0.1        # restart required
  port: 3000             # restart required
  # publicUrl: https://browsers.example.com   # pin the external URL (reverse proxy / mTLS ingress)

sessions:
  defaultTTL: 3600       # seconds
  maxTTL: 14400
  maxConcurrent: 8

browser:
  runtime: chrome
  executablePath: auto   # or an absolute path
  headless: false
  # extraArgs: ["--no-sandbox"]

tunnel:
  provider: none         # none | cloudflare-quick | cloudflare-named
  # cloudflare:
  #   hostname: browser.example.com   # cloudflare-named
  #   executablePath: auto

app:
  startOnLogin: true     # macOS
  preventSleep: true     # keep the machine awake while sessions are active
  keepAwake: false       # keep it awake whenever the service runs, so it stays reachable
```

| Variable                             | Overrides / purpose                          |
| ------------------------------------ | -------------------------------------------- |
| `TABTENDER_CONFIG_PATH`              | Configuration file location                  |
| `TABTENDER_DATA_DIR`                 | Data directory (profiles, state, logs)       |
| `TABTENDER_HOST`                     | `server.host`                                |
| `TABTENDER_PORT`                     | `server.port`                                |
| `TABTENDER_DEFAULT_SESSION_TTL`      | `sessions.defaultTTL`                        |
| `TABTENDER_MAX_SESSION_TTL`          | `sessions.maxTTL`                            |
| `TABTENDER_MAX_SESSIONS`             | `sessions.maxConcurrent`                     |
| `TABTENDER_BROWSER_RUNTIME`          | `browser.runtime`                            |
| `TABTENDER_CHROME_PATH`              | `browser.executablePath`                     |
| `TABTENDER_TUNNEL_PROVIDER`          | `tunnel.provider`                            |
| `TABTENDER_API_KEY`                  | API key (min. 32 characters); disables rotation |
| `TABTENDER_CLOUDFLARE_TUNNEL_TOKEN`  | Token for `cloudflare-named`                 |
| `TABTENDER_MGMT_SOCKET`              | Management socket path                       |
| `TABTENDER_SECRETS_BACKEND`          | `file` to bypass the OS credential store     |
| `TABTENDER_LOG_LEVEL`                | `debug`, `info`, `warn`, `error`             |

The service is the source of truth. The macOS app reads and writes
configuration through the service, which validates changes, writes the file
atomically, and applies everything except `server.host`/`server.port` live.
Values pinned by the environment are shown read-only and are never written to
the file. Saving through the app rewrites the file, so hand-written comments
in it are not preserved.

## Tunnels

Set `tunnel.provider` (or pick it in the macOS app):

- **`cloudflare-quick`** — a temporary `https://<random>.trycloudflare.com`
  URL. No account needed; intended for development. The URL changes whenever
  the tunnel restarts, and a new hostname can take up to a minute to resolve
  and route (Cloudflare answers `530` until then).
- **`cloudflare-named`** — a persistent tunnel. Create a tunnel in the
  Cloudflare dashboard, route a public hostname to `http://localhost:3000`,
  set `tunnel.cloudflare.hostname`, and provide the tunnel token (macOS app →
  Settings → Tunnel, `TABTENDER_CLOUDFLARE_TUNNEL_TOKEN`, or on Windows a
  `%ProgramData%\Tabtender\secrets\cloudflare-tunnel-token.provision` file).

Tabtender finds `cloudflared` on your `PATH`, or downloads a pinned,
checksum-verified release on first use. It supervises the process and restarts
it if it exits. While a tunnel is up, sessions created through it return
`wss://` CDP URLs on the public hostname; local callers keep getting local
URLs. `GET /v1/health` reports the tunnel state and URL.

A tunnel URL is never treated as a credential: the API key (or, for a CDP
connection, the session's token) is required regardless of how a request
arrives.

## Security

- A 256-bit API key is generated on first start and stored in the macOS
  Keychain, or on Windows in a DPAPI-encrypted file readable only by the
  service account, SYSTEM, and Administrators. Keys are compared in constant time and never logged.
- Each session has its own random 256-bit token, embedded in its `cdpUrl`.
  It authorizes that session's CDP endpoint only — never the REST API, never
  another session — is kept in memory only, stays the same for the session's
  life, and stops working when the session ends. Rotating the API key does
  not change it. The API key itself is never accepted in a URL.
- **Treat a `cdpUrl` as a secret.** Anyone holding it controls that session's
  browser until the session ends, and URLs are easier to leak than headers:
  reverse proxies, tunnels, and client libraries may log them. Tabtender
  never logs query strings. To revoke a leaked URL, delete the session.
- **A `cdpUrl` is as powerful as the browser it controls.** The token keeps
  its holder out of the REST API and out of other sessions' endpoints, but
  CDP is a debugging protocol: whoever drives a browser can make it read
  local files (`file://`) and write files (downloads) with the permissions of
  the user the service runs as — which is enough to reach that user's other
  data, Tabtender's included. Sessions are native, unsandboxed browsers by
  design. Hand a `cdpUrl` only to agents you would trust with that user
  account; for agents you would not, run Tabtender under a dedicated OS
  account or on a dedicated machine or VM.
- The server binds to loopback by default. Browsers expose no debugging port
  at all: Tabtender talks to each one over a private pipe.
- The local management channel is a Unix socket with owner-only permissions;
  each connection's peer credentials must match the service's user.
- Helper processes (Chrome, `cloudflared`) never inherit `TABTENDER_*`
  variables.
- On macOS, browsers are started as their own "responsible process", so the
  system attributes what Chrome does (such as updating itself) to Chrome,
  not to Tabtender.
- For mTLS, terminate it at a reverse proxy or the tunnel ingress and set
  `server.publicUrl`.

Sessions give you separate processes and separate browsing state. They are
**not a security boundary** between mutually untrusted users: everything runs
as one OS user, anyone holding the API key controls every session, and a
session's browser can read whatever that user can.

## Architecture

```
cmd/tabtenderd/        service entrypoint
internal/
  api/                 REST handlers, CDP WebSocket proxy, management API
  sessions/            session lifecycle, TTLs, limits, reconciliation
  runtime/             BrowserRuntime interface + registry
    chrome/            Chrome adapter (discovery, launch, CDP over the debugging pipe)
    platform/          OS process handling (process groups, job objects)
  tunnel/              TunnelProvider interface, registry, supervisor
    cloudflare/        Quick and named tunnels via cloudflared
  config/              YAML + environment resolution, atomic persistence
  auth/  secrets/      API key management, OS credential stores
  service/             launchd and Windows SCM integration
  daemon/              wiring
apps/macos/            SwiftUI menu-bar app (management client only)
packaging/             macOS .app/DMG, Windows service installer
docs/                  API reference and OpenAPI description
test/e2e/              end-to-end tests: real tabtenderd + real Chrome
test/clients/          Playwright and Puppeteer conformance suite
```

Two processes, strictly separated: `tabtenderd` (Go) owns all behavior;
`Tabtender.app` (SwiftUI) is a management client that talks to it over the
local socket and controls its launchd registration. Neither depends on the
other at runtime — closing or crashing the app never touches the service.

Browser runtimes and tunnel providers are plain Go interfaces selected by name
through a registry:

```go
type BrowserRuntime interface {
    Launch(ctx context.Context, opts BrowserOptions) (BrowserInstance, error)
    Close(ctx context.Context, instance BrowserInstance) error
}

type BrowserInstance interface {
    Connect(ctx context.Context) (CDPConn, error) // one independent CDP connection
    PID() int
    IsRunning() bool
}

type TunnelProvider interface {
    Start(ctx context.Context, localPort int) (string, error)
    Stop(ctx context.Context) error
    Status() TunnelStatus
}
```

Adding Edge, Chromium, ngrok, or Tailscale means registering one more
constructor; the session manager and API layer do not change (see
`internal/daemon/daemon_test.go`, which runs the whole service on substitute
adapters).

## Building

Requires Go (see `go.mod`); the macOS app additionally needs Xcode 16+.

```sh
make build      # bin/tabtenderd
make test       # unit tests
make e2e        # end-to-end tests against a real browser
make clients    # Playwright + Puppeteer suite
make app        # dist/Tabtender.app and DMG (macOS, universal)
```

Sign and notarize a macOS build with your own identity:

```sh
CODESIGN_IDENTITY="Developer ID Application: Example (TEAMID)" NOTARY_PROFILE=my-profile \
  packaging/macos/build-app.sh --dmg
```

Releases are built, signed, and published by the
[Release workflow](.github/workflows/release.yml) when a version tag is
pushed; see [CONTRIBUTING.md](CONTRIBUTING.md#releasing).

End-to-end tests honor `TABTENDER_CHROME_PATH`, `TABTENDER_E2E_HEADED=1`,
`TABTENDER_E2E_TUNNEL=1` (real Cloudflare Quick Tunnel), and
`TABTENDER_E2E_CHROME_ARGS`. The service lifecycle tests are
`packaging/macos/e2e-launchd.sh` (SMAppService + launchd) and
`packaging/windows/e2e-service.ps1` (Service Control Manager); CI runs all of
them on macOS, Windows, and Linux runners.

## Not in v1

A Windows GUI or client CLI · headed Chrome from a Windows Service · Linux as
a supported release target (it builds and is tested in CI) · automation APIs
beyond raw CDP · stealth/fingerprint patches · recording or streaming ·
multi-machine scheduling · multi-tenant accounts · dynamic plugins.

## License

[MIT](LICENSE)
