import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { chromium } from 'playwright-core';
import { api, authHeaders, createSession, deleteSession, startSite, waitFor } from './helpers.mjs';

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
    assert.match(session.cdpUrl, /^wss?:\/\/.+\/v1\/sessions\/sess_[a-z0-9]+\/cdp$/);
    const browser = await chromium.connectOverCDP(session.cdpUrl, { headers: authHeaders });
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

test('the CDP endpoint rejects connections without a valid API key', async () => {
  const session = await createSession();
  try {
    await assert.rejects(chromium.connectOverCDP(session.cdpUrl, { timeout: 10000 }), /401/);
    await assert.rejects(
      chromium.connectOverCDP(session.cdpUrl, { headers: { Authorization: 'Bearer wrong-key' }, timeout: 10000 }),
      /401/,
    );
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
  await assert.rejects(chromium.connectOverCDP(session.cdpUrl, { headers: authHeaders, timeout: 10000 }));
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
