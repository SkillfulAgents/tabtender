import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { chromium } from 'playwright-core';
import { api, authHeaders, baseUrl, createSession, deleteSession, startSite, waitFor, withoutToken } from './helpers.mjs';

let site;
before(async () => {
  site = await startSite();
});
after(async () => {
  await site.close();
});

test('Playwright controls a session through the returned cdpUrl', async () => {
  const session = await createSession();
  try {
    assert.match(session.cdpUrl, /^wss?:\/\/.+\/v1\/sessions\/sess_[a-z0-9]+\/cdp\?token=[A-Za-z0-9_-]{43}$/);
    // No headers: the URL is all a client needs.
    const browser = await chromium.connectOverCDP(session.cdpUrl);
    const context = browser.contexts()[0];
    const page = await context.newPage();
    await page.goto(`${site.url}/hello`);
    assert.equal(await page.locator('#path').textContent(), '/hello');
    assert.equal(await page.evaluate(() => 6 * 7), 42);
    // A screenshot exercises large binary-ish payloads through the proxy.
    const screenshot = await page.screenshot();
    assert.ok(screenshot.length > 1000);
    await browser.close();
  } finally {
    await deleteSession(session.id);
  }
});

test('the API key in a header opens the CDP endpoint without the token', async () => {
  const session = await createSession();
  try {
    const browser = await chromium.connectOverCDP(withoutToken(session.cdpUrl), { headers: authHeaders });
    const page = await browser.contexts()[0].newPage();
    assert.equal(await page.evaluate(() => 1 + 1), 2);
    await browser.close();
  } finally {
    await deleteSession(session.id);
  }
});

test('the CDP endpoint rejects connections without a valid credential', async () => {
  const [session, other] = await Promise.all([createSession(), createSession()]);
  try {
    const bare = withoutToken(session.cdpUrl);
    const otherToken = new URL(other.cdpUrl).searchParams.get('token');
    await assert.rejects(chromium.connectOverCDP(bare, { timeout: 10000 }), /401/);
    await assert.rejects(
      chromium.connectOverCDP(bare, { headers: { Authorization: 'Bearer wrong-key' }, timeout: 10000 }),
      /401/,
    );
    // Another session's token opens only that other session.
    await assert.rejects(chromium.connectOverCDP(`${bare}?token=${otherToken}`, { timeout: 10000 }), /401/);
  } finally {
    await deleteSession(session.id);
    await deleteSession(other.id);
  }
});

test('a session token is not accepted by the REST API', async () => {
  const session = await createSession();
  try {
    const token = new URL(session.cdpUrl).searchParams.get('token');
    for (const path of ['/v1/sessions', '/v1/health', `/v1/sessions/${session.id}`]) {
      const response = await fetch(`${baseUrl}${path}?token=${token}`);
      assert.equal(response.status, 401, path);
    }
    // Reading the session again returns the same working URL.
    const again = await api('GET', `/v1/sessions/${session.id}`);
    assert.equal(again.body.cdpUrl, session.cdpUrl);
  } finally {
    await deleteSession(session.id);
  }
});

test('clients can disconnect and reconnect without losing the session', async () => {
  const session = await createSession();
  try {
    const first = await chromium.connectOverCDP(session.cdpUrl, { headers: authHeaders });
    const page = await first.contexts()[0].newPage();
    await page.goto(`${site.url}/survivor`);
    await page.evaluate(() => {
      localStorage.setItem('note', 'still here');
      window.name = 'kept-across-reconnect';
    });
    await first.close(); // Disconnects the CDP client; the browser keeps running.

    const status = await api('GET', `/v1/sessions/${session.id}`);
    assert.equal(status.body.status, 'running');

    const second = await chromium.connectOverCDP(session.cdpUrl, { headers: authHeaders });
    const pages = second.contexts()[0].pages();
    const survivor = pages.find((candidate) => candidate.url().endsWith('/survivor'));
    assert.ok(survivor, `expected the page to survive, found: ${pages.map((p) => p.url())}`);
    assert.equal(await survivor.evaluate(() => localStorage.getItem('note')), 'still here');
    assert.equal(await survivor.evaluate(() => window.name), 'kept-across-reconnect');
    await second.close();
  } finally {
    await deleteSession(session.id);
  }
});

test('several clients share a session without disturbing each other', async () => {
  const session = await createSession();
  try {
    const first = await chromium.connectOverCDP(session.cdpUrl);
    const firstPage = await first.contexts()[0].newPage();
    await firstPage.goto(`${site.url}/first`);

    // A second client joins while the first is at work, and sees its page.
    const second = await chromium.connectOverCDP(session.cdpUrl);
    assert.equal((await api('GET', `/v1/sessions/${session.id}`)).body.connectedClients, 2);
    const seen = second.contexts()[0].pages().map((p) => p.url());
    assert.ok(seen.some((url) => url.endsWith('/first')), `second client sees: ${seen}`);
    const secondPage = await second.contexts()[0].newPage();
    await secondPage.goto(`${site.url}/second`);

    // Both keep working side by side.
    const [a, b] = await Promise.all([
      firstPage.evaluate(() => location.pathname),
      secondPage.evaluate(() => location.pathname),
    ]);
    assert.deepEqual([a, b], ['/first', '/second']);

    // One leaving does not affect the other, nor the pages it opened.
    await first.close();
    assert.equal(await secondPage.evaluate(() => 40 + 2), 42);
    await waitFor(async () => (await api('GET', `/v1/sessions/${session.id}`)).body.connectedClients === 1);
    const remaining = second.contexts()[0].pages().map((p) => p.url());
    assert.ok(remaining.some((url) => url.endsWith('/first')), `pages after the first client left: ${remaining}`);
    await second.close();
  } finally {
    await deleteSession(session.id);
  }
});

test('sessions do not share cookies or storage', async () => {
  const [a, b] = await Promise.all([createSession(), createSession()]);
  try {
    const browserA = await chromium.connectOverCDP(a.cdpUrl, { headers: authHeaders });
    const browserB = await chromium.connectOverCDP(b.cdpUrl, { headers: authHeaders });
    const pageA = await browserA.contexts()[0].newPage();
    const pageB = await browserB.contexts()[0].newPage();
    await pageA.goto(site.url);
    await pageB.goto(site.url);

    await pageA.evaluate(() => {
      document.cookie = 'owner=a; path=/';
      localStorage.setItem('owner', 'a');
    });
    await pageB.reload();
    assert.equal(await pageB.evaluate(() => document.cookie), '');
    assert.equal(await pageB.evaluate(() => localStorage.getItem('owner')), null);
    assert.equal(await pageA.evaluate(() => document.cookie), 'owner=a');

    // Lifecycles are independent too: ending A leaves B fully usable.
    await deleteSession(a.id);
    assert.equal(await pageB.evaluate(() => 1 + 1), 2);
    await browserB.close();
    await browserA.close().catch(() => {});
  } finally {
    await deleteSession(a.id);
    await deleteSession(b.id);
  }
});

test('terminating a session disconnects its clients', async () => {
  const session = await createSession();
  const browser = await chromium.connectOverCDP(session.cdpUrl, { headers: authHeaders });
  let disconnected = false;
  browser.on('disconnected', () => {
    disconnected = true;
  });
  const deleted = await deleteSession(session.id);
  assert.equal(deleted.status, 200);
  assert.equal(deleted.body.status, 'terminated');
  assert.equal(deleted.body.endReason, 'deleted');
  await waitFor(() => disconnected, { what: 'client disconnect' });
  // Neither the old URL nor the API key can reach an ended session.
  await assert.rejects(chromium.connectOverCDP(session.cdpUrl, { timeout: 10000 }), /409/);
  await assert.rejects(chromium.connectOverCDP(session.cdpUrl, { headers: authHeaders, timeout: 10000 }), /409/);
});

test('a named profile persists browsing state across sessions', async () => {
  const profile = `conformance-${Date.now()}`;
  const first = await createSession({ profile });
  try {
    // The profile is exclusively locked while in use.
    const conflict = await api('POST', '/v1/sessions', { profile });
    assert.equal(conflict.status, 409);
    assert.equal(conflict.body.error.code, 'profile_in_use');

    const browser = await chromium.connectOverCDP(first.cdpUrl, { headers: authHeaders });
    const page = await browser.contexts()[0].newPage();
    await page.goto(site.url);
    await page.evaluate(() => localStorage.setItem('persisted', 'yes'));
    // Like any browser, Chrome writes storage to disk asynchronously; a write
    // made in the very last instant before shutdown can be lost.
    await new Promise((resolve) => setTimeout(resolve, 1000));
    await browser.close();
  } finally {
    await deleteSession(first.id);
  }

  const second = await createSession({ profile });
  try {
    const browser = await chromium.connectOverCDP(second.cdpUrl, { headers: authHeaders });
    const page = await browser.contexts()[0].newPage();
    await page.goto(site.url);
    assert.equal(await page.evaluate(() => localStorage.getItem('persisted')), 'yes');
    await browser.close();
  } finally {
    await deleteSession(second.id);
  }
});
