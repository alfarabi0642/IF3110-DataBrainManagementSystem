import assert from 'node:assert/strict';
import { mkdir, writeFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import puppeteer from 'puppeteer-core';

const origin = process.env.E2E_ORIGIN;
assert.match(origin ?? '', /^http:\/\/127\.0\.0\.1:\d+$/);
const base = `/databases/${process.env.E2E_TARGET}`;
const output = fileURLToPath(new URL('../../.tmp/e2e/', import.meta.url));
await mkdir(output, { recursive: true });
const browser = await puppeteer.launch({
  ...(process.env.PUPPETEER_EXECUTABLE_PATH ? { executablePath: process.env.PUPPETEER_EXECUTABLE_PATH } : { channel: 'chrome' }),
  headless: true,
});
const checks = [], errors = [];
try {
  const page = await browser.newPage();
  page.setDefaultTimeout(15000);
  page.on('pageerror', e => errors.push(e.message));
  await page.emulateTimezone('Asia/Jakarta');
  await page.emulateMediaFeatures([{ name: 'prefers-reduced-motion', value: 'reduce' }]);
  const ready = () => page.waitForFunction(() => !document.querySelector('[role=status]')?.textContent.includes('Loading'));
  const go = async path => { await page.goto(origin + path, { waitUntil: 'networkidle0' }); await ready(); };
  const fill = async (selector, value) => {
    await page.focus(selector);
    await page.keyboard.down('Control'); await page.keyboard.press('A'); await page.keyboard.up('Control');
    await page.keyboard.type(value);
  };
  const submit = async (action, status) => {
    const result = page.waitForResponse(r => r.url().split('?')[0] === origin + '/api' + action && r.request().method() === 'POST');
    await page.$eval(`form[action="${action}"]`, f => f.requestSubmit());
    const response = await result;
    assert.equal(response.status(), status);
    await page.waitForFunction(() => [...document.querySelectorAll('button')].every(b => !b.disabled));
    return response;
  };
  const api = (path, body) => page.evaluate(async (path, body) => {
    const session = await (await fetch('/api/session')).json();
    const response = await fetch('/api' + path, body ? {
      method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': session.data.csrf_token }, body: JSON.stringify(body),
    } : {});
    return { status: response.status, ...(await response.json()) };
  }, path, body);
  await go('/login');
  await fill('#username', 'draft_editor_a'); await fill('#password', 'wrong-password');
  await submit('/login', 401);
  assert.ok(await page.$('[role=alert]'));
  await fill('#password', 'BrowserFixture123!'); await submit('/login', 200);
  await page.waitForFunction(() => location.pathname === '/databases' && !!document.querySelector('.account'));
  checks.push('actual login invalid/valid, cookie session and CSRF');
  const users = await api('/admin/users'), groups = await api('/admin/groups');
  assert.equal(users.status, 200); assert.equal(groups.status, 200);
  const connectionPath = '/admin/databases/' + process.env.E2E_TARGET;
  for (const [width, height] of [[1440, 1000], [390, 844]]) {
    await page.setViewport({ width, height, deviceScaleFactor: 1, isMobile: false, hasTouch: false });
    const original = (await api(connectionPath)).data;
    const update = (revision, name) => api(connectionPath, { expected_revision: revision, name,
      username: original.username, ssl_mode: original.ssl_mode, is_active: original.is_active });
    await go(connectionPath);
    const concurrent = await update(original.config_revision, 'Concurrent connection');
    assert.equal(concurrent.status, 200);
    await fill('#name', 'Preserved connection input'); await submit(connectionPath, 409);
    assert.equal(await page.$eval('#name', e => e.value), 'Preserved connection input');
    assert.equal(await page.$eval('form.form-panel button', e => e.textContent), 'Save over the current version');
    await fill('#name', '   ');
    const invalid = await submit(connectionPath, 422);
    assert.equal((await invalid.json()).error.code, 'VALIDATION_FAILED');
    assert.equal(await page.$eval('[name=expected_revision]', e => e.value), concurrent.data.config_revision);
    await fill('#name', 'Preserved connection input');
    let interruptedRevision;
    const interrupt = request => {
      if (new URL(request.url()).pathname === '/api' + connectionPath && request.method() === 'POST') {
        interruptedRevision = JSON.parse(request.postData()).expected_revision;
        void request.abort('failed');
      } else void request.continue();
    };
    await page.setRequestInterception(true); page.on('request', interrupt);
    await page.$eval(`form[action="${connectionPath}"]`, f => f.requestSubmit());
    await page.waitForFunction(() => document.querySelector('[role=alert]')?.textContent.includes('Unable to reach the server.'));
    await page.setRequestInterception(false); page.off('request', interrupt);
    assert.equal(interruptedRevision, concurrent.data.config_revision);
    assert.equal(await page.$eval('[name=expected_revision]', e => e.value), concurrent.data.config_revision);
    const newer = await update(concurrent.data.config_revision, 'Newer concurrent connection');
    assert.equal(newer.status, 200);
    const conflict = await submit(connectionPath, 409);
    assert.equal(JSON.parse(conflict.request().postData()).expected_revision, concurrent.data.config_revision);
    assert.equal((await conflict.json()).error.code, 'REVISION_CONFLICT');
    assert.equal((await api(connectionPath)).data.name, 'Newer concurrent connection');
    assert.equal(await page.$eval('#name', e => e.value), 'Preserved connection input');
    assert.equal(await page.$eval('form.form-panel button', e => e.textContent), 'Save over the current version');
    await fill('#name', '   '); await submit(connectionPath, 422);
    assert.equal(await page.$eval('[name=expected_revision]', e => e.value), newer.data.config_revision);
    await fill('#name', original.name);
    const retry = await submit(connectionPath, 200);
    assert.equal(JSON.parse(retry.request().postData()).expected_revision, newer.data.config_revision);
    assert.equal((await api(connectionPath)).data.name, original.name);
  }
  checks.push('connection overwrite survives validation/network errors on desktop/mobile; later revisions require explicit overwrite');
  const created = await api(base + '/drafts', { title: 'Browser draft', sql: 'NOT SQL;' });
  assert.equal(created.status, 201);
  const draftPath = base + '/drafts/' + created.data.draft.id;
  await go(draftPath);
  const sql = 'CREATE TABLE public.browser_committed(id int PRIMARY KEY); INSERT INTO public.browser_committed VALUES(1);';
  await fill('#sql', sql); await submit(draftPath, 200);
  const saved = (await api(draftPath)).data.draft;
  assert.equal((await api(draftPath, { title: 'Other editor', sql, expected_revision: saved.revision })).status, 200);
  await fill('#title', 'Keep my unsaved input'); await submit(draftPath, 409);
  assert.equal(await page.$eval('#title', e => e.value), 'Keep my unsaved input');
  assert.ok(await page.$('[role=alert]'));
  page.once('dialog', dialog => dialog.accept());
  await go(draftPath);
  checks.push('draft save/conflict preserves submitted input');
  await go(draftPath + '/review');
  await page.waitForFunction(() => location.search.includes('request_key='));
  const reviewURL = new URL(page.url());
  await page.reload({ waitUntil: 'networkidle0' });
  assert.equal(page.url(), reviewURL.href);
  await submit(draftPath + '/execute', 200);
  await page.waitForFunction(() => /\/migrations\/[^/]+$/.test(location.pathname));
  const attempt = new URL(page.url()).pathname;
  assert.ok((await page.$eval('.status', e => e.textContent)).includes('committed'));
  const replay = await api(draftPath + '/execute', Object.fromEntries(reviewURL.searchParams));
  assert.equal(replay.data.id, attempt.split('/').pop());
  checks.push('canonical review/refresh, actual commit, same-key replay and receipt');
  const routes = [
    ['/databases', 'databases'], ['/admin/users', 'users'], ['/admin/users/' + users.data[0].id, 'user'],
    ['/admin/groups', 'groups'], ['/admin/groups/' + groups.data[0].id, 'group'],
    ['/admin/databases', 'connections'], ['/admin/databases/' + process.env.E2E_TARGET, 'connection'],
    [base + '/explorer?schema=public&table=browser_cells', 'explorer'],
    [base + '/drafts', 'drafts'], [draftPath, 'draft'], [base + '/migrations', 'history'], [attempt, 'attempt'],
  ];
  for (const [device, width, height] of [['desktop', 1440, 1000], ['mobile', 390, 844]]) {
    await page.setViewport({ width, height, deviceScaleFactor: 1, isMobile: false, hasTouch: false });
    for (const theme of ['light', 'dark']) {
      await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: theme }, { name: 'prefers-reduced-motion', value: 'reduce' }]);
      for (const [route, name] of routes) {
        await go(route);
        await page.evaluate(async () => {
          await Promise.all(['16px Geist', '16px "Geist Mono"', '16px "Instrument Serif"', 'italic 16px "Instrument Serif"'].map(f => document.fonts.load(f)));
          await document.fonts.ready;
        });
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth), name + ' overflow');
        assert.ok(await page.evaluate(() => [...document.fonts].every(f => f.status === 'loaded')));
        if (name === 'explorer') {
          const text = await page.$eval('.table-wrap', e => e.textContent);
          assert.ok(text.includes('9007199254740993') && text.includes('NULL') && text.includes('(empty)'));
          await page.$eval('.table-wrap', e => { e.scrollLeft = e.scrollWidth; });
        }
        await page.screenshot({ path: output + `${name}-${device}-${theme}.png`, fullPage: true });
      }
    }
  }
  checks.push('desktop/mobile light/dark direct routes, font readiness, NULL/precision and scroll');
  await go(base + '/explorer?schema=public&table=browser_cells');
  await page.click('.dbnav a[href$="/migrations"]'); await ready();
  await page.goBack({ waitUntil: 'networkidle0' }); assert.ok(page.url().includes('/explorer?'));
  await page.goForward({ waitUntil: 'networkidle0' }); assert.ok(page.url().endsWith('/migrations'));
  await page.setCookie({ name: 'databrain_sid', value: process.env.E2E_EXPIRED_TOKEN, url: origin, httpOnly: true, sameSite: 'Lax' });
  await go(base + '/drafts');
  await page.waitForFunction(() => location.pathname === '/login');
  checks.push('Back/Forward and actual expired server session');
  await fill('#username', 'runner_actor_0'); await fill('#password', 'BrowserFixture123!');
  await submit('/login', 200);
  await page.waitForFunction(() => location.pathname === '/databases' && !!document.querySelector('.account'));
  for (const [route, code] of [['/admin/users', 'ADMIN_REQUIRED'], [base + '/drafts', 'DATABASE_ACCESS_DENIED']]) {
    await go(route);
    assert.equal(await page.$('.account'), null);
    const text = await page.$eval('.meta', e => e.textContent);
    assert.ok(text.includes(code) && text.includes('Request ID'));
  }
  checks.push('viewer denials use backend authorization, real request IDs and the original error shell');
  assert.deepEqual(errors, []);
  console.log(JSON.stringify({ checks, browser: await browser.version() }));
} finally {
  await writeFile(output + 'results.json', JSON.stringify({ checks, errors }, null, 2));
  await browser.close();
}
