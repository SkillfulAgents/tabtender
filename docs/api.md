# Tabtender API reference

Tabtender has two HTTP APIs, both served by `tabtenderd`:

| API | Transport | Authentication | Audience |
| --- | --- | --- | --- |
| [Public API](#public-api) | TCP (`server.host:server.port`, default `127.0.0.1:3000`), and the tunnel if one is enabled | API key | Agents and automation clients |
| [Management API](#management-api) | Unix domain socket (macOS and Linux only) | Same OS user as the service | The macOS app and local administration |

A machine-readable description of the public API is in [`openapi.yaml`](openapi.yaml).

## Conventions

- Request and response bodies are JSON (`Content-Type: application/json`).
- Timestamps are RFC 3339 in UTC, with fractional seconds.
- Durations are whole seconds.
- Request bodies are decoded strictly: unknown fields, wrong types, and
  trailing data are rejected with `400 validation_error`. Bodies are limited
  to 1 MiB.
- Every JSON response carries a `Content-Length` header (no chunked encoding).

### Errors

Every error response has the same shape:

```json
{ "error": { "code": "validation_error", "message": "ttlSeconds 99999 exceeds the maximum session TTL of 14400 seconds" } }
```

`code` is stable and meant for programs; `message` is meant for people.

| Status | Code | Meaning |
| --- | --- | --- |
| 400 | `validation_error` | Malformed body, unknown field, or a value outside its limits |
| 401 | `unauthorized` | Missing or wrong API key (public API only) |
| 404 | `not_found` | No such endpoint |
| 404 | `session_not_found` | Unknown session ID |
| 409 | `profile_in_use` | The named profile is locked by another session |
| 409 | `session_not_running` | CDP connection to a session that has ended or is not running yet |
| 426 | `upgrade_required` | The CDP endpoint was requested without a WebSocket upgrade |
| 429 | `capacity_exceeded` | `sessions.maxConcurrent` sessions already exist |
| 500 | `internal_error` | Unexpected failure |
| 502 | `browser_launch_failed` | The browser could not be started (for example, Chrome is not installed) |
| 502 | `browser_unreachable` | The session's browser did not accept the CDP connection |
| 503 | `shutting_down` | The service is stopping, or the session was terminated while it was starting |

The management API additionally uses `environment_controlled` (409),
`apply_failed` (500), and `profile_not_found` (404), described below.

---

## Public API

### Authentication

Every request, including the CDP WebSocket handshake, must carry the API key:

```
Authorization: Bearer <api key>
```

Requests without a valid key get `401 unauthorized` with a
`WWW-Authenticate: Bearer realm="tabtender"` header — whatever the path, so an
unauthenticated caller cannot tell which endpoints or sessions exist. The key
is not accepted in the query string.

### The session object

```json
{
  "id": "sess_ru5g7woaasehntbed6f5ygzgqi",
  "status": "running",
  "headless": true,
  "profile": "work",
  "createdAt": "2026-11-17T00:54:16.66132Z",
  "expiresAt": "2026-11-17T01:54:17.066109Z",
  "cdpUrl": "ws://127.0.0.1:3000/v1/sessions/sess_ru5g7woaasehntbed6f5ygzgqi/cdp",
  "connectedClients": 0
}
```

| Field | Type | Notes |
| --- | --- | --- |
| `id` | string | `sess_` followed by 26 random base32 characters (128 bits) |
| `status` | string | `starting`, `running`, `terminating`, or `terminated` |
| `headless` | boolean | Whether the browser runs without a window |
| `profile` | string | Named persistent profile. Omitted for ephemeral sessions |
| `createdAt` | timestamp | When the session was requested |
| `expiresAt` | timestamp | When the TTL ends. The TTL starts once the browser is ready, so `expiresAt - createdAt` is the TTL plus the browser's launch time |
| `cdpUrl` | string | WebSocket URL of the session's CDP endpoint. Omitted once `terminated` |
| `connectedClients` | integer | CDP clients currently connected |
| `endedAt` | timestamp | Only when `terminated` |
| `endReason` | string | Only when `terminated`: `deleted`, `expired`, `crashed` (the browser exited by itself), or `shutdown` (the service stopped) |

**Which `cdpUrl` you get.** The URL is built for the caller:

1. If `server.publicUrl` is configured, it is always used.
2. If you addressed the service by a loopback name (`localhost`, `127.0.0.1`,
   `::1`), you get a URL on the address you used.
3. Otherwise, if a tunnel is up, you get the tunnel's public `wss://` URL.
4. Otherwise you get a URL on the `Host` you used — `wss://` if the request
   arrived over TLS or with `X-Forwarded-Proto: https`, else `ws://`.

The browser's own CDP port is bound to loopback and never appears in any
response.

### `POST /v1/sessions` — create a session

Launches a browser and returns once its CDP endpoint is ready.

Request body (optional; every field is optional):

| Field | Type | Default | Notes |
| --- | --- | --- | --- |
| `ttlSeconds` | integer | `sessions.defaultTTL` | 1 to `sessions.maxTTL` |
| `headless` | boolean | `browser.headless` | `false` is rejected where no desktop exists (a Windows Service) |
| `profile` | string | — | 1–64 characters of letters, digits, `-`, `_`, starting with a letter or digit. Omit for an ephemeral profile that is deleted with the session |

```sh
curl -X POST http://127.0.0.1:3000/v1/sessions \
  -H "Authorization: Bearer $KEY" \
  -d '{"ttlSeconds": 1800, "headless": false, "profile": "work"}'
```

Responses: `201` with the [session object](#the-session-object); `400
validation_error`; `409 profile_in_use`; `429 capacity_exceeded`; `502
browser_launch_failed`; `503 shutting_down`.

### `GET /v1/sessions` — list active sessions

Returns sessions that have not ended, oldest first.

```json
{ "sessions": [ { "id": "sess_…", "status": "running", "…": "…" } ] }
```

### `GET /v1/sessions/{id}` — session status

Returns the [session object](#the-session-object). Sessions that ended within
the last ten minutes are still returned, with `status: "terminated"`:

```json
{
  "id": "sess_ru5g7woaasehntbed6f5ygzgqi",
  "status": "terminated",
  "headless": true,
  "profile": "work",
  "createdAt": "2026-11-17T00:54:16.66132Z",
  "expiresAt": "2026-11-17T01:54:17.066109Z",
  "connectedClients": 0,
  "endedAt": "2026-11-17T00:54:17.308753Z",
  "endReason": "deleted"
}
```

Responses: `200`; `404 session_not_found`.

### `DELETE /v1/sessions/{id}` — terminate a session

Shuts the browser down, closes its CDP connections, and removes an ephemeral
profile. Returns once cleanup is complete.

Responses: `200` with the terminated session object; `404 session_not_found`
(including for sessions that have already ended).

### `GET /v1/sessions/{id}/cdp` — CDP WebSocket

Upgrades to a WebSocket connected to the session's browser-level CDP endpoint
(the equivalent of Chrome's `/devtools/browser/<id>`).

- Text and binary messages are relayed verbatim in both directions and never
  inspected. There is no message size limit.
- Any number of clients may connect to one session.
- Disconnecting does not end the session; reconnect to the same URL at any
  time while it is running.
- The server sends WebSocket pings every 30 seconds, so idle connections stay
  open through proxies and tunnels.
- When the session ends, clients are closed with status `1001` (going away)
  and reason `session terminated`. A close initiated by the browser is passed
  through with its original status code and reason.

```js
const browser = await chromium.connectOverCDP(session.cdpUrl, {
  headers: { Authorization: `Bearer ${apiKey}` },
});
```

Handshake errors are ordinary HTTP responses: `401 unauthorized`; `404
session_not_found`; `409 session_not_running`; `426 upgrade_required`; `502
browser_unreachable`.

### `GET /v1/health` — health and capacity

```json
{
  "status": "ok",
  "version": "1.0.0",
  "uptimeSeconds": 4211,
  "sessions": { "active": 1, "max": 8, "available": 7 },
  "browser": { "runtime": "chrome", "available": true },
  "tunnel": { "provider": "cloudflare-quick", "state": "running", "url": "https://example.trycloudflare.com", "error": "" }
}
```

| Field | Notes |
| --- | --- |
| `status` | `ok`, or `degraded` when the configured browser cannot be found |
| `browser.available` | Whether the browser executable was found; `browser.error` explains when it was not |
| `tunnel.provider` | The configured provider (`none` for local-only access) |
| `tunnel.state` | `disabled`, `starting`, `running`, `error`, or `stopped` |
| `tunnel.url` | Public base URL while `running`, otherwise empty |
| `tunnel.error` | Last error while `error` or `starting`, otherwise empty |

---

## Management API

The management API is how the macOS app configures the service. It listens on
a Unix domain socket and is not reachable over the network:

| Platform | Socket |
| --- | --- |
| macOS | `~/Library/Application Support/Tabtender/tabtenderd.sock` |
| Linux | `~/.local/share/tabtender/tabtenderd.sock` |
| Windows | Not available — use the configuration file, environment variables, and the public API |

`TABTENDER_MGMT_SOCKET` overrides the path. The socket file is owner-only, and
each connection's kernel-reported peer credentials must match the user the
service runs as. No API key is needed.

```sh
SOCK="$HOME/Library/Application Support/Tabtender/tabtenderd.sock"
curl --unix-socket "$SOCK" http://tabtender/mgmt/v1/status
```

(The host name in the URL is ignored.)

Every [public endpoint](#public-api) is also served on this socket, without a
key. `cdpUrl` values returned there use the tunnel's URL when a tunnel is up,
and the service's local address otherwise.

### `GET /mgmt/v1/status`

```json
{
  "version": "1.0.0",
  "pid": 15043,
  "startedAt": "2026-11-17T00:54:15.157294Z",
  "uptimeSeconds": 1,
  "localUrl": "http://127.0.0.1:3000",
  "publicUrl": "",
  "sessions": { "active": 1, "max": 8, "available": 7 },
  "browser": { "runtime": "chrome", "available": true, "executable": "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" },
  "tunnel": { "provider": "none", "state": "disabled", "url": "", "error": "" },
  "tunnelTokenSource": "",
  "configPath": "/Users/me/Library/Application Support/Tabtender/config.yaml",
  "dataDir": "/Users/me/Library/Application Support/Tabtender",
  "logFile": "/Users/me/Library/Application Support/Tabtender/logs/tabtenderd.log",
  "pendingRestart": []
}
```

| Field | Notes |
| --- | --- |
| `publicUrl` | `server.publicUrl` if set, else the tunnel URL while it is running, else empty |
| `browser.executable` | The resolved browser path (only exposed on this API) |
| `tunnelTokenSource` | Where the Cloudflare tunnel token comes from: `environment`, the secret store (`keychain`, `dpapi`, `file`), or empty when there is none |
| `pendingRestart` | Restart-required settings whose saved value differs from the running one |

### `GET /mgmt/v1/config`

Returns the configuration in layers, so a UI can tell what is stored from what
is in force.

```json
{
  "path": "/Users/me/Library/Application Support/Tabtender/config.yaml",
  "file": {
    "server": { "host": "127.0.0.1", "port": 3000, "publicUrl": "" },
    "sessions": { "defaultTTL": 3600, "maxTTL": 14400, "maxConcurrent": 8 },
    "browser": { "runtime": "chrome", "executablePath": "auto", "headless": false, "extraArgs": [] },
    "tunnel": { "provider": "none", "cloudflare": { "executablePath": "auto", "hostname": "" } },
    "app": { "startOnLogin": true, "preventSleep": true }
  },
  "effective": { "…": "same shape as file, with environment overrides applied" },
  "envOverrides": {
    "sessions.maxConcurrent": { "variable": "TABTENDER_MAX_SESSIONS", "value": "4" }
  },
  "restartRequiredKeys": ["server.host", "server.port"],
  "pendingRestart": []
}
```

| Field | Notes |
| --- | --- |
| `file` | The file layer over the defaults: what is, or would be, written to `config.yaml` |
| `effective` | `file` with environment overrides applied: what the service uses |
| `envOverrides` | Settings pinned by an environment variable, keyed by dotted setting name. They cannot be changed through this API |
| `restartRequiredKeys` | Settings that only take effect after a restart. Everything else applies immediately |

### `PUT /mgmt/v1/config`

Updates the file layer. The body is merged over the current file layer: send
the whole `file` document or only what you are changing — fields you omit keep
their current values. (Arrays such as `browser.extraArgs` are replaced as a
whole.)

```sh
curl --unix-socket "$SOCK" -X PUT http://tabtender/mgmt/v1/config \
  -d '{"sessions": {"defaultTTL": 900, "maxTTL": 14400, "maxConcurrent": 4}}'
```

The service validates the result, writes `config.yaml` atomically, and applies
every setting except `restartRequiredKeys` to the running service (session
limits, browser settings for new sessions, tunnel provider, sleep prevention).
Environment-controlled settings keep their environment value and are never
written to the file.

Responses:

- `200` with the same shape as `GET /mgmt/v1/config`.
- `400 validation_error` — nothing was saved. All problems are reported in one
  message.
- `500 apply_failed` — the configuration was valid and **has been saved**, but
  part of it could not be applied to the running service (for example the
  tunnel could not be reconfigured).

### `GET /mgmt/v1/apikey`

```json
{ "apiKey": "tt_<64 hex characters>", "source": "keychain", "rotatable": true }
```

`source` is `environment` when the key is pinned by `TABTENDER_API_KEY`
(then `rotatable` is `false`), otherwise the secret store: `keychain`,
`dpapi`, or `file`.

### `POST /mgmt/v1/apikey/rotate`

Generates a new 256-bit key, stores it, and returns it in the same shape as
`GET /mgmt/v1/apikey`. The old key stops working immediately; existing CDP
connections stay open.

Responses: `200`; `409 environment_controlled` when the key is pinned by
`TABTENDER_API_KEY`.

### `PUT /mgmt/v1/tunnel/token`

Stores the Cloudflare tunnel token used by the `cloudflare-named` provider and
restarts the tunnel. An empty token deletes the stored one. The token is never
returned by any endpoint.

```json
{ "token": "eyJhIjoi…" }
```

Responses: `200` with `{"tunnelTokenSource": "keychain"}`; `409
environment_controlled` when the token is pinned by
`TABTENDER_CLOUDFLARE_TUNNEL_TOKEN`.

### `POST /mgmt/v1/tunnel/restart`

Restarts the active tunnel provider and returns its status (the same object as
`tunnel` in `/v1/health`). The tunnel comes up in the background, so the
returned state is typically `starting`. Restarting a Quick Tunnel gives it a
new URL.

### `GET /mgmt/v1/profiles`

Lists named profiles on disk.

```json
{ "profiles": [ { "name": "work", "inUse": true, "sessionId": "sess_ru5g7woaasehntbed6f5ygzgqi" } ] }
```

`sessionId` is present only while the profile is in use.

### `DELETE /mgmt/v1/profiles/{name}`

Deletes a named profile and all of its browsing data.

Responses: `204`; `400 validation_error` for an invalid name; `404
profile_not_found`; `409 profile_in_use`.

### `GET /mgmt/v1/logs`

Returns recent log entries from an in-memory buffer of the last 1000, oldest
first.

| Query parameter | Default | Notes |
| --- | --- | --- |
| `limit` | `200` | Positive integer; the most recent `limit` entries are returned |
| `level` | `info` | Minimum level: `info`, `warn`, or `error` (debug entries are not buffered) |

```json
{
  "entries": [
    {
      "time": "2026-11-16T16:54:17.070659-08:00",
      "level": "INFO",
      "message": "session started",
      "attrs": { "session": "sess_ru5g7woaasehntbed6f5ygzgqi", "pid": "15057", "headless": "true", "profile": "work", "ttl": "1h0m0s" }
    }
  ]
}
```

`attrs` values are always strings; the field is omitted when an entry has no
attributes. Credentials are never logged.
