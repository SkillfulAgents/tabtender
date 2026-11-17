import assert from 'node:assert/strict';
import { test } from 'node:test';
import puppeteer from 'puppeteer-core';
import { authHeaders, createSession, deleteSession, withoutToken } from './helpers.mjs';

test('Puppeteer controls a session through the returned cdpUrl', async () => {
  const session = await createSession();
  try {
    // No headers: the URL is all a client needs.
    const browser = await puppeteer.connect({ browserWSEndpoint: session.cdpUrl });
    const page = await browser.newPage();
    await page.setContent('<title>puppeteer was here</title><p id="p">hello</p>');
    assert.equal(await page.title(), 'puppeteer was here');
    assert.equal(await page.$eval('#p', (element) => element.textContent), 'hello');
    await browser.disconnect();

    // Reconnect with the API key instead, and find the same page.
    const again = await puppeteer.connect({ browserWSEndpoint: withoutToken(session.cdpUrl), headers: authHeaders });
    const titles = await Promise.all((await again.pages()).map((candidate) => candidate.title()));
    assert.ok(titles.includes('puppeteer was here'), `titles: ${titles}`);
    await again.disconnect();
  } finally {
    await deleteSession(session.id);
  }
});
