// Shared helpers for the client conformance suite.
//
// The suite runs against an already running tabtenderd, located through:
//   TABTENDER_URL        base URL of the service (e.g. http://127.0.0.1:3000)
//   TABTENDER_API_KEY    API key
//   TABTENDER_HEADLESS   "true"/"false" to force the session mode (optional)
import http from 'node:http';

export const baseUrl = (process.env.TABTENDER_URL ?? '').replace(/\/$/, '');
export const apiKey = process.env.TABTENDER_API_KEY ?? '';

if (!baseUrl || !apiKey) {
  throw new Error('TABTENDER_URL and TABTENDER_API_KEY must be set');
}

export const authHeaders = { Authorization: `Bearer ${apiKey}` };

export async function api(method, path, body) {
  const response = await fetch(baseUrl + path, {
    method,
    headers: { ...authHeaders, 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await response.text();
  return { status: response.status, body: text ? JSON.parse(text) : undefined };
}

export async function createSession(overrides = {}) {
  const request = { ttlSeconds: 300, ...overrides };
  const headless = process.env.TABTENDER_HEADLESS;
  if (headless && request.headless === undefined) {
    request.headless = headless === 'true';
  }
  const { status, body } = await api('POST', '/v1/sessions', request);
  if (status !== 201) {
    throw new Error(`session creation failed: ${status} ${JSON.stringify(body)}`);
  }
  return body;
}

export async function deleteSession(id) {
  return api('DELETE', `/v1/sessions/${id}`);
}

// Starts a tiny local site so tests have a real http origin for cookies and
// storage. The browser under test runs on the same machine as this suite
// unless TABTENDER_URL points at a remote tunnel, in which case tests that
// need the site are expected to skip.
export async function startSite() {
  const server = http.createServer((request, response) => {
    response.writeHead(200, { 'Content-Type': 'text/html' });
    response.end(`<!doctype html><title>tabtender fixture</title><h1 id="path">${request.url}</h1>`);
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address();
  return {
    url: `http://127.0.0.1:${port}`,
    close: () => new Promise((resolve) => server.close(resolve)),
  };
}

export async function waitFor(predicate, { timeoutMs = 15000, intervalMs = 100, what = 'condition' } = {}) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    if (await predicate()) return;
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await new Promise((resolve) => setTimeout(resolve, intervalMs));
  }
}
