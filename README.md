# Tabtender

[![CI](https://github.com/SkillfulAgents/tabtender/actions/workflows/ci.yml/badge.svg)](https://github.com/SkillfulAgents/tabtender/actions/workflows/ci.yml)

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
┌────────────┐  HTTPS / WSS   ┌─────────────────────────────┐  loopback CDP  ┌────────┐
│ AI agent   │ ─────────────▶ │ tabtenderd                  │ ─────────────▶ │ Chrome │
│ Playwright │  Bearer key    │ REST API · CDP proxy · TTLs │                ├────────┤
│ Puppeteer  │ ◀───────────── │ sessions · tunnel · auth    │ ─────────────▶ │ Chrome │
└────────────┘                └─────────────────────────────┘                └────────┘
```

## Quick start

### macOS

1. Download `Tabtender-<version>.dmg` from the
   [releases page](https://github.com/SkillfulAgents/tabtender/releases) and
   drag **Tabtender** to Applications. Google Chrome must be installed.
2. Launch it. A menu-bar icon appears, and the app registers and starts the
   `tabtenderd` background service (a per-user launchd agent — no root).
3. Open **Settings → API Key** to copy your key.

The service keeps running when you quit the menu-bar app, restarts after a
crash, and starts at login (configurable).

> Release builds are currently ad-hoc signed, not notarized. On first launch,
> right-click the app and choose **Open**, or build it yourself with your own
> Developer ID (see [Building](#building)).

### Windows

Download and extract `tabtender-<version>-windows-<arch>.zip`, then from an
elevated PowerShell prompt:

```powershell
.\install.ps1
Get-Content "$env:ProgramData\Tabtender\secrets\initial-api-key.txt"   # one-time key retrieval
```

This installs `tabtenderd` as a Windows Service that starts at boot. To supply
your own key instead, use `.\install.ps1 -ApiKey <key>`. Remove everything with
`.\uninstall.ps1 -PurgeData`.

A Windows Service runs in the non-interactive session 0, so sessions are
**headless** there. Run `tabtenderd.exe` from a console in your own session for
headed browsers.

### From source (any platform)

```sh
go run ./cmd/tabtenderd
```

On macOS the generated key is in the Keychain; read it over the local
management socket:

```sh
curl --unix-socket ~/Library/Application\ Support/Tabtender/tabtenderd.sock http://tabtender/mgmt/v1/apikey
```

Or pin a key of your own (at least 32 characters): `TABTENDER_API_KEY=... go run ./cmd/tabtenderd`.

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

const browser = await chromium.connectOverCDP(session.cdpUrl, { headers });
const page = await browser.contexts()[0].newPage();
await page.goto('https://example.com');

await browser.close();                                   // disconnects; the session keeps running
await fetch(`${api}/v1/sessions/${session.id}`, { method: 'DELETE', headers });
```

Puppeteer: `puppeteer.connect({ browserWSEndpoint: session.cdpUrl, headers })`.

## API

Every request — including the CDP WebSocket handshake — must carry
`Authorization: Bearer <api key>`. This is a summary; the full reference,
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
  "cdpUrl": "wss://browser.example.com/v1/sessions/sess_pdba4wpt6fg3qx4bgjlmzhn5de/cdp",
  "connectedClients": 0
}
```

Errors look like `{"error": {"code": "validation_error", "message": "..."}}`:

| Status | Code                    | Meaning                                         |
| ------ | ----------------------- | ----------------------------------------------- |
| 400    | `validation_error`      | Bad request (for example a TTL above the maximum) |
| 401    | `unauthorized`          | Missing or wrong API key                        |
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
  after a grace period). As with any browser, state written in the last
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
- Sessions do not survive a restart of the service. At startup, `tabtenderd`
  reconciles: browsers left behind by a previous run are terminated — only
  after verifying, from their command line, that they belong to this
  installation — and ephemeral profiles are removed. Named profiles are kept.
- The CDP proxy relays text and binary frames verbatim and never inspects
  them. The browser's own CDP port listens on loopback only and is never
  exposed.

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

A tunnel URL is never treated as a credential: the API key is required on
every request regardless of how it arrives.

## Security

- A 256-bit API key is generated on first start and stored in the macOS
  Keychain, or on Windows in a DPAPI-encrypted file readable only by SYSTEM
  and Administrators. Keys are compared in constant time and never logged.
- The server binds to loopback by default. Chrome's CDP listeners are always
  loopback-only and never exposed.
- The local management channel is a Unix socket with owner-only permissions;
  each connection's peer credentials must match the service's user.
- Helper processes (Chrome, `cloudflared`) never inherit `TABTENDER_*`
  variables.
- For mTLS, terminate it at a reverse proxy or the tunnel ingress and set
  `server.publicUrl`.

Sessions give you separate processes and separate browsing state. They are
**not a security boundary** between mutually untrusted users: everything runs
as one OS user, and anyone holding the API key controls every session.

## Architecture

```
cmd/tabtenderd/        service entrypoint
internal/
  api/                 REST handlers, CDP WebSocket proxy, management API
  sessions/            session lifecycle, TTLs, limits, reconciliation
  runtime/             BrowserRuntime interface + registry
    chrome/            Chrome adapter (discovery, launch flags, shutdown)
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

Sign and notarize a macOS release with your own identity:

```sh
CODESIGN_IDENTITY="Developer ID Application: Example (TEAMID)" NOTARY_PROFILE=my-profile \
  packaging/macos/build-app.sh --dmg
```

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
