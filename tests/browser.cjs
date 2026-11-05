// Real browser integration tests. All services and databases are temporary.
// Runtime dependencies remain Go-only; Playwright is a development tool.
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const { createHash } = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const net = require('node:net');
const http = require('node:http');
const { spawn } = require('node:child_process');
const root = path.resolve(__dirname, '..');
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
async function freePorts() {
  const servers = [];
  for (let i = 0; i < 3; i++) {
    const server = net.createServer();
    await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
    servers.push(server);
  }
  const ports = servers.map(server => server.address().port);
  await Promise.all(servers.map(server => new Promise(resolve => server.close(resolve))));
  return ports;
}
(async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'roi-browser-'));
  let app, browser, source;
  let output = '', calls = 0;
  try {
    source = http.createServer((req, res) => {
      const gitPath = new URL(req.url, 'http://localhost').pathname;
      if (gitPath === '/') { res.setHeader('Content-Type', 'text/html'); res.end('<html><head><meta name="application-name" content="Forgejo"><link rel="icon" href="/forge-icon.svg"></head></html>'); return; }
      if (/^\/api\/v1\/repos\/team\/(tool|other)\/releases$/.test(gitPath)) {
        res.setHeader('Content-Type', 'application/json');
        res.end(JSON.stringify([
          { name: 'Tool preview', tag_name: 'v3.0.0-rc1', prerelease: true, published_at: '2024-03-03T12:00:00Z', body: 'Preview notes' },
          { name: 'Tool stable', tag_name: 'v2.0.0', prerelease: false, published_at: '2024-02-02T12:00:00Z', body: '# Highlights\n\n**Stable notes**\n\n- Fast\n- Safe' },
        ])); return;
      }
      if (/^\/api\/v1\/repos\/team\/(tool|other)\/tags$/.test(gitPath)) { res.setHeader('Content-Type', 'application/json'); res.end(JSON.stringify([{ name: 'v4.0.0', message: 'Stable tag notes', commit: { created: '2024-04-04T12:00:00Z' } }])); return; }
      if (req.url === '/broken') { res.statusCode = 503; res.end('temporarily unavailable'); return; }
      calls++;
      res.setHeader('Content-Type', 'application/rss+xml');
      res.end('<rss version="2.0"><channel><title>Local test feed</title><link>https://example.org</link><description>Test</description><item><guid>local-1</guid><pubDate>Wed, 14 Oct 2009 16:12:40 +0000</pubDate><title>Local article</title><description>Article body</description></item></channel></rss>');
    });
    await new Promise(resolve => source.listen(0, '127.0.0.1', resolve));
    const feedURL = `http://127.0.0.1:${source.address().port}/feed`;
    const [apiPort, adminPort, opsPort] = await freePorts();
    const api = `http://127.0.0.1:${apiPort}/greader`;
    const admin = `http://127.0.0.1:${adminPort}`;
    const config = fs.readFileSync(path.join(root, 'examples/config.yaml'), 'utf8')
      .replace(':8080', `:${apiPort}`).replace(':8081', `:${adminPort}`).replace(':8082', `:${opsPort}`)
      .replace(/^([ \t]*)public_url:.*$/m, '$1public_url: https://reader.example.org').replace('./data/roi.db', path.join(dir, 'roi.db')).replace(/interval: \S+/, 'interval: 1s')
      .replace('login_per_minute: 10', 'login_per_minute: 1000').replace('account_login_per_minute: 60', 'account_login_per_minute: 1000')
      .replace('enabled: false # Set true for daily online SQLite backups.', 'enabled: true # Set true for daily online SQLite backups.').replace('./data/backups/automatic', path.join(dir, 'backups')).replace('time: "03:00"', 'time: "00:00"')
      .replace('enabled: false # Daily ROI snapshots, only when subscription export changes.', 'enabled: true # Daily ROI snapshots, only when subscription export changes.').replace('./data/backups/subscriptions', path.join(dir, 'subscription-backups'))
      .replace('allow_private_networks: false', 'allow_private_networks: true').replace('per_host_delay: 2s', 'per_host_delay: 0s');
    fs.writeFileSync(path.join(dir, 'config.yaml'), config);
    app = spawn(process.env.ROI_BINARY || path.join(root, 'bin', 'roi'), [], {
      env: { ...process.env, ROI_CONFIG: path.join(dir, 'config.yaml'), ROI_CHECK_CONFIG: 'false' },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    app.stdout.on('data', b => output += b);
    app.stderr.on('data', b => output += b);
    for (let i = 0; i < 100; i++) {
      if (app.exitCode !== null) throw new Error(`roi exited: ${output}`);
      try { if ((await fetch(`http://127.0.0.1:${opsPort}/health`)).ok) break; } catch {}
      await sleep(100);
    }
    browser = await chromium.launch({ headless: true, ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE } : {}) });
    const context = await browser.newContext({ colorScheme: 'light', timezoneId: 'Asia/Tokyo', locale: 'en-GB', viewport: { width: 1280, height: 900 } });
    const page = await context.newPage();
    const errors = [];
    page.on('pageerror', e => errors.push(e.message));
    await page.goto(admin + '/');
    assert.equal(new URL(page.url()).pathname, '/login');
    assert.equal(await page.locator('html').getAttribute('lang'), 'en');
    const bg = () => page.evaluate(() => getComputedStyle(document.documentElement).backgroundColor);
    const light = await bg();
    await page.getByRole('button', { name: 'Dark theme' }).click();
    const dark = await bg();
    assert.notEqual(light, dark);
    await page.reload();
    assert.equal(await bg(), dark);
    await page.getByLabel('Username').fill('admin');
    await page.getByLabel('Password', { exact: true }).fill('roi-admin-test');
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    await page.waitForURL(admin + '/');
    // Keep the same browser context and credentials across an actual restart.
    const sessionBeforeRestart = (await context.cookies()).find(c => c.name === 'roi_session');
    const csrfBeforeRestart = await page.locator('input[name="csrf"]').first().inputValue();
    const stopped = new Promise(resolve => app.once('exit', resolve));
    app.kill('SIGTERM');
    await Promise.race([stopped, sleep(10000).then(() => { throw new Error('restart shutdown timeout'); })]);
    app = spawn(process.env.ROI_BINARY || path.join(root, 'bin', 'roi'), [], {
      env: { ...process.env, ROI_CONFIG: path.join(dir, 'config.yaml'), ROI_CHECK_CONFIG: 'false' },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    app.stdout.on('data', b => output += b);
    app.stderr.on('data', b => output += b);
    for (let i = 0; i < 100; i++) {
      if (app.exitCode !== null) throw new Error(`roi exited after restart: ${output}`);
      try { if ((await fetch(`http://127.0.0.1:${opsPort}/health`)).ok) break; } catch {}
      await sleep(100);
    }
    await page.reload();
    assert.equal(new URL(page.url()).pathname, '/');
    assert.equal((await context.cookies()).find(c => c.name === 'roi_session').value, sessionBeforeRestart.value);
    assert.equal(await page.locator('input[name="csrf"]').first().inputValue(), csrfBeforeRestart);
    await page.evaluate(() => { window.__roiDocumentMarker = 'preserved'; });
    let documentRequests = 0;
    page.on('request', request => { if (request.isNavigationRequest() && request.frame() === page.mainFrame()) documentRequests++; });
    assert.equal(await page.locator('#summary .card').count(), 4);
    assert.deepEqual(await page.locator('#summary .card span').allTextContents(), ['Subscriptions', 'Entries', 'Unread', 'Starred']);
    const menuLinks = page.locator('.top-nav a');
    assert.equal(await menuLinks.count(), 6);
    for (const link of await menuLinks.all()) {
      assert.equal(await link.locator('svg').count(), 1);
      assert.equal(await link.locator('span').isVisible(), true);
      assert.equal(await link.getAttribute('aria-label'), await link.locator('span').innerText());
    }
    async function navigate(section) {
      if (await page.locator('body').getAttribute('data-page') === section) return;
      await page.locator('.top-nav').getByRole('link', { name: section === 'import-export' ? 'Import/Export' : section[0].toUpperCase() + section.slice(1), exact: true }).click();
      await page.waitForFunction(value => document.body.dataset.page === value && document.querySelector('.top-nav [aria-current=page]')?.getAttribute('href') === '/' + value, section);
    }
    await navigate('devices');
    await navigate('security');
    await page.evaluate(() => history.back());
    await page.waitForFunction(() => document.body.dataset.page === 'devices' && location.pathname === '/devices');
    await page.evaluate(() => history.forward());
    await page.waitForFunction(() => document.body.dataset.page === 'security' && location.pathname === '/security');
    await navigate('feeds');
    const plainContext = await browser.newContext({ javaScriptEnabled: false });
    await plainContext.addCookies(await context.cookies());
    const plainPage = await plainContext.newPage();
    for (const section of ['feeds', 'starred', 'devices', 'security', 'import-export', 'maintenance']) {
      await plainPage.goto(admin + '/' + section);
      assert.equal(await plainPage.locator('.top-nav [aria-current=page]').getAttribute('href'), '/' + section);
      assert.equal(await plainPage.locator('#page-content').getAttribute('data-page'), section);
    }
    await plainContext.close();
    async function action(button, route) {
      if (['/import', '/import-roi'].includes(route) && !(await button.evaluate(node => !!node.closest('#import-preview-dialog')))) {
        const [previewResponse] = await Promise.all([
          page.waitForResponse(r => new URL(r.url()).pathname === '/preview-import' && r.request().method() === 'POST'),
          button.click(),
        ]);
        assert.equal(previewResponse.status(), 200, await previewResponse.text());
        await page.locator('#import-preview-dialog').waitFor({ state: 'visible' });
        await action(page.getByRole('button', { name: 'Confirm import', exact: true }), route);
        return;
      }
      const [response] = await Promise.all([
        page.waitForResponse(r => new URL(r.url()).pathname === route && r.request().method() === 'POST'),
        button.click(),
      ]);
      if (response.status() !== 200) throw new Error(route + ': ' + response.status() + ' ' + await response.text());
      await page.waitForFunction(() => !document.querySelector('form[data-busy=true]'));
    }
    async function revokeAction(button, route) {
      const isBrowserSession = route === '/revoke-browser-sessions' || (await button.evaluate(node => node.closest('[data-session-id]')?.dataset.sessionRealm)) === 'admin';
      await button.click();
      const dialog = page.locator('#revoke-dialog');
      assert.equal(await dialog.isVisible(), true);
      const revokeDescription = await dialog.locator('#revoke-description').innerText();
      assert.equal(revokeDescription.includes('API'), !isBrowserSession);
      assert.ok((await dialog.locator('h2').innerText()).includes(isBrowserSession ? 'browser' : 'Reader API'));
      assert.equal(await page.evaluate(() => document.activeElement?.textContent), 'Cancel');
      await action(dialog.getByRole('button', { name: 'Confirm revocation', exact: true }), route);
    }
    async function disabledView(checked) {
      await Promise.all([
        page.waitForResponse(r => { const u = new URL(r.url()); return u.searchParams.get('fragment') === '1' && (u.searchParams.get('disabled') === '1') === checked; }),
        page.getByRole('switch', { name: 'Disabled only', exact: true }).setChecked(checked),
      ]);
      await page.waitForFunction(value => document.querySelector('#feed-list .list-caption').textContent.includes(value), checked ? 'disabled subscriptions' : 'enabled subscriptions');
    }
    assert.equal(await bg(), dark);
    assert.equal(await page.title(), 'ροή');
    const cookie = (await context.cookies()).find(c => c.name === 'roi_session');
    assert.equal(cookie.httpOnly, true);
    assert.equal(cookie.sameSite, 'Strict');
    await page.getByRole('button', { name: 'System theme' }).click();
    assert.equal(await bg(), light);
    await page.emulateMedia({ colorScheme: 'dark' });
    assert.equal(await bg(), dark);
    await page.getByRole('button', { name: 'Light theme' }).click();
    assert.equal(await bg(), light);
    const tab = await context.newPage();
    await tab.goto(admin + '/');
    await page.getByRole('button', { name: 'Dark theme' }).click();
    await tab.waitForFunction(() => document.documentElement.dataset.theme === 'dark');
    await tab.close();
    const noStorage = await browser.newContext();
    await noStorage.addInitScript(() => Object.defineProperty(window, 'localStorage', { get() { throw Error('blocked'); } }));
    const isolated = await noStorage.newPage();
    await isolated.goto(admin + '/login');
    await isolated.getByRole('button', { name: 'Light theme' }).click();
    assert.equal(await isolated.evaluate(() => document.documentElement.dataset.theme), 'light');
    await noStorage.close();
    const opml = `<opml><body><outline text="Local"><outline text="Local test feed" xmlUrl="${feedURL}"/></outline></body></opml>`;
    async function importOPML() {
      await navigate('import-export');
      await page.locator('input[name=opml]').setInputFiles({ name: 'test.opml', mimeType: 'text/xml', buffer: Buffer.from(opml) });
      await action(page.getByRole('button', { name: 'Import OPML' }), '/import');
      await navigate('feeds');
    }
    // Preview/cancel must leave the subscription list unchanged.
    assert.ok((await page.locator('.build-version').innerText()).length > 0);
    await navigate('maintenance');
    assert.equal(await page.locator('.opml-panel').count(), 0);
    assert.equal(await page.locator('#summary').count(), 0);
    const storageTable = page.getByRole('table', { name: 'Storage statistics', exact: true });
    assert.equal(await page.locator('#backup-statistics').count(), 1);
    assert.equal(await page.locator('#subscription-backups').count(), 1);
    assert.ok((await page.getByRole('table', { name: 'Subscription backup statistics', exact: true }).innerText()).includes('keep 30 copies'));
    await page.locator('#subscription-backup-files summary').click();
    assert.equal(await page.getByRole('table', { name: 'Stored subscription backups', exact: true }).locator('tbody tr').count(), 1);
    await page.getByRole('table', { name: 'Daily backup history', exact: true }).getByRole('cell', { name: /^[1-9]\d* \/ 0$/ }).waitFor();
    await page.locator('#backup-files summary').click();
    assert.equal(await page.getByRole('table', { name: 'Stored backups', exact: true }).locator('tbody tr').count(), 1);
    for (const label of ['Entry data', 'SQLite', 'WAL']) assert.equal(await storageTable.getByRole('rowheader', { name: label, exact: true }).count(), 1);
    await navigate('import-export');
    await page.locator('input[name=opml]').setInputFiles({ name: 'test.opml', mimeType: 'text/xml', buffer: Buffer.from(opml) });
    await page.locator('.opml-panel').evaluate(node => node.dataset.uploadMarker = 'preserved');
    await page.waitForResponse(response => {
      const url = new URL(response.url());
      return url.pathname === '/import-export' && url.searchParams.get('fragment') === '1';
    });
    await sleep(100);
    assert.equal(await page.locator('.opml-panel').getAttribute('data-upload-marker'), 'preserved');
    assert.equal(await page.locator('input[name=opml]').evaluate(node => node.files.length), 1);
    await Promise.all([
      page.waitForResponse(r => new URL(r.url()).pathname === '/preview-import'),
      page.getByRole('button', { name: 'Import OPML', exact: true }).click(),
    ]);
    const importDialog = page.locator('#import-preview-dialog');
    await importDialog.waitFor({ state: 'visible' });
    assert.ok((await page.locator('#import-summary').innerText()).includes('1 to add'));
    await importDialog.getByRole('button', { name: 'Cancel', exact: true }).click();
    await navigate('feeds');
    assert.equal(await page.getByRole('table', { name: 'RSS Feeds', exact: true }).locator('[data-feed-id]').count(), 0);
    await navigate('import-export');
    // Invalid individual records are visible and cannot be confirmed.
    await page.getByLabel('ROI file', { exact: true }).setInputFiles({ name: 'invalid.roi', mimeType: 'application/yaml', buffer: Buffer.from('format: roi\nversion: 1\nsources:\n - type: rss\n   url: file:///invalid\n') });
    await Promise.all([
      page.waitForResponse(r => new URL(r.url()).pathname === '/preview-import'),
      page.getByRole('button', { name: 'Import ROI', exact: true }).click(),
    ]);
    await importDialog.waitFor({ state: 'visible' });
    assert.ok((await page.locator('#import-summary').innerText()).includes('1 invalid'));
    assert.equal(await page.getByRole('button', { name: 'Confirm import', exact: true }).isDisabled(), true);
    await importDialog.getByRole('button', { name: 'Cancel', exact: true }).click();
    await importOPML();
    const feedTable = page.getByRole('table', { name: 'RSS Feeds', exact: true });
    assert.equal(await feedTable.getByRole('columnheader', { name: 'Folders', exact: true }).count(), 1);
    assert.equal(await feedTable.locator('.folder-tag').innerText(), 'Local');
    const buttonSizes = await feedTable.locator('.actions button, .actions .button').evaluateAll(nodes => nodes.map(n => ({ width: n.getBoundingClientRect().width, height: n.getBoundingClientRect().height })));
    assert.ok(buttonSizes.every(size => size.width === 36 && size.height === 36));
    assert.equal(await feedTable.locator('.feed-title').evaluate(node => node.firstElementChild.hasAttribute('data-rename')), true);
    const pencil = await feedTable.locator('[data-rename]').evaluate(node => ({ width: node.getBoundingClientRect().width, font: parseFloat(getComputedStyle(node).fontSize) }));
    assert.ok(Math.abs(pencil.width - pencil.font - 8) < 1);
    await context.grantPermissions(['clipboard-read', 'clipboard-write'], { origin: admin });
    await feedTable.getByRole('button', { name: 'RSS settings', exact: true }).click();
    const rssDialog = page.locator('#rss-settings-dialog');
    await rssDialog.getByLabel('User-Agent', { exact: true }).fill('BrowserTest/1 ("quoted")');
    await action(rssDialog.getByRole('button', { name: 'Save RSS settings', exact: true }), '/rss-settings');
    assert.equal(await rssDialog.isVisible(), false);
    assert.equal(await feedTable.locator('[data-feed-id]').getAttribute('data-user-agent'), 'BrowserTest/1 ("quoted")');
    await feedTable.getByRole('button', { name: 'RSS settings', exact: true }).click();
    assert.equal(await rssDialog.getByLabel('User-Agent', { exact: true }).inputValue(), 'BrowserTest/1 ("quoted")');
    await rssDialog.getByLabel('User-Agent', { exact: true }).fill('');
    await action(rssDialog.getByRole('button', { name: 'Save RSS settings', exact: true }), '/rss-settings');
    assert.equal(await feedTable.locator('[data-feed-id]').getAttribute('data-user-agent'), '');
    const copyURL = feedTable.getByRole('button', { name: 'Copy feed URL', exact: true });
    assert.equal(await copyURL.evaluate(node => node.nextElementSibling.matches('a.feed-url')), true);
    await copyURL.click();
    assert.equal(await page.evaluate(() => navigator.clipboard.readText()), feedURL);
    assert.equal(await page.locator('#notice').innerText(), 'URL copied.');
    // A denied Clipboard API request still uses the plain-HTTP-compatible fallback.
    await page.evaluate(async () => {
      window.__roiClipboard = navigator.clipboard;
      await navigator.clipboard.writeText('cleared');
      Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async () => { throw Error('denied'); } } });
    });
    await copyURL.click();
    assert.equal(await page.evaluate(() => window.__roiClipboard.readText()), feedURL);
    await page.evaluate(() => {
      window.__roiExecCommand = document.execCommand;
      document.execCommand = () => false;
    });
    await copyURL.click();
    assert.ok((await page.locator('#notice').innerText()).includes('Copy failed'));
    assert.equal(await page.locator('.copy-buffer').count(), 0);
    await page.evaluate(() => {
      Object.defineProperty(navigator, 'clipboard', { configurable: true, value: window.__roiClipboard });
      document.execCommand = window.__roiExecCommand;
    });

    assert.equal(await page.getByText('Subscriptions and sync', { exact: true }).count(), 0);
    assert.equal(await page.locator('.opml-panel').count(), 0);
    assert.equal(await page.locator('.section-heading').first().evaluate(node => node.contains(document.querySelector('form[action="/sync"]'))), true);
    assert.equal(await feedTable.getByRole('row').count(), 2);
    assert.equal(await feedTable.getByRole('columnheader', { name: 'Consecutive errors', exact: true }).count(), 1);
    assert.equal(await feedTable.locator('td.failures').innerText(), '0');
    await importOPML();
    assert.equal(await feedTable.getByRole('row').count(), 2);
    async function createPersonalCredentials(name, useUI = false) {
      if (useUI) {
        await navigate('devices');
        await page.getByRole('button', { name: 'Add API device', exact: true }).click();
        const dialog = page.locator('#api-device-dialog');
        await dialog.getByLabel('Device name', { exact: true }).fill(name);
        await action(dialog.getByRole('button', { name: 'Create device', exact: true }), '/api-devices/create');
        const credentialsDialog = page.locator('#api-credentials-dialog');
        await credentialsDialog.waitFor({ state: 'visible' });
        const credentials = { login: await page.locator('#api-personal-login').inputValue(), code: await page.locator('#api-sign-in-code').inputValue() };
        assert.equal(await page.locator('#api-reader-url').inputValue(), 'https://reader.example.org/greader');
        assert.equal(await credentialsDialog.getByRole('button', { name: 'Copy API URL', exact: true }).count(), 1);
        for (const name of ['Copy API URL', 'Copy login', 'Copy code']) {
          const button = credentialsDialog.getByRole('button', { name, exact: true });
          assert.equal(await button.textContent(), '');
          assert.equal(await button.locator('svg').count(), 1);
          assert.equal(await button.evaluate(el => {
            const input = el.parentElement.querySelector('input');
            const a = input.getBoundingClientRect(), b = el.getBoundingClientRect();
            return b.left >= a.right && Math.abs(a.top - b.top) <= 1;
          }), true);
        }
        await credentialsDialog.getByRole('button', { name: 'Copy API URL', exact: true }).click();
        await page.waitForFunction(async () => (await navigator.clipboard.readText()) === 'https://reader.example.org/greader');
        await page.evaluate(async () => {
          window.__roiCredentialClipboard = navigator.clipboard;
          await navigator.clipboard.writeText('cleared');
          Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async () => { throw Error('denied'); } } });
        });
        await credentialsDialog.getByRole('button', { name: 'Copy API URL', exact: true }).click();
        await page.waitForFunction(async () => (await window.__roiCredentialClipboard.readText()) === 'https://reader.example.org/greader');
        await page.evaluate(() => {
          Object.defineProperty(navigator, 'clipboard', { configurable: true, value: window.__roiCredentialClipboard });
          delete window.__roiCredentialClipboard;
        });
        assert.ok(credentials.login.startsWith('device_'));
        assert.equal(credentials.code.length, 64);
        await credentialsDialog.getByRole('button', { name: 'Done', exact: true }).click();
        await page.waitForFunction(() => document.querySelector('#api-sign-in-code').value === '');
        assert.equal((await page.locator('#api-sign-in-code').inputValue()).length, 0);
        assert.equal(await page.locator('#api-personal-login').inputValue(), '');
        return credentials;
      }
      const csrf = await page.locator('input[name=csrf]').first().inputValue();
      const response = await context.request.post(admin + '/api-devices/create', { form: { csrf, device_name: name }, headers: { 'X-ROI-Request': 'async', Accept: 'application/json' } });
      assert.equal(response.status(), 200);
      return (await response.json()).credentials;
    }
    async function personalLogin(name) {
      const credentials = await createPersonalCredentials(name);
      return context.request.post(api + '/accounts/ClientLogin', { form: { Email: credentials.login, Passwd: credentials.code } });
    }
    const firstCredentials = await createPersonalCredentials('Browser test device', true);
    const login = await context.request.post(api + '/accounts/ClientLogin', { form: { Email: firstCredentials.login, Passwd: firstCredentials.code } });
    assert.equal(login.status(), 200);
    const replay = await context.request.post(api + '/accounts/ClientLogin', { form: { Email: firstCredentials.login, Passwd: firstCredentials.code } });
    assert.equal(replay.status(), 403);

    const auth = (await login.text()).split('\n').find(line => line.startsWith('Auth=')).slice(5);
    const headers = { Authorization: 'GoogleLogin auth=' + auth };
    const get = async route => { const r = await context.request.get(api + '/reader/api/0/' + route, { headers }); assert.equal(r.status(), 200); return r.json(); };
    assert.deepEqual((await get('subscription/list')).subscriptions, []);
    assert.deepEqual((await get('stream/items/ids')).itemRefs, []);
    const authID = createHash('sha256').update(auth).digest('hex');
    const approvedRow = page.locator(`#reader-sessions [data-session-id="${authID}"]`);
    await approvedRow.waitFor({ timeout: 10000 });
    assert.ok((await approvedRow.innerText()).includes('Pending approval'));
    await approvedRow.getByRole('button', { name: 'Approve device', exact: true }).click();
    const deviceDialog = page.locator('#device-dialog');
    await deviceDialog.getByLabel('Device name', { exact: true }).fill('Reeder on MacBook');
    await action(deviceDialog.getByRole('button', { name: 'Approve device', exact: true }), '/approve-session');
    assert.ok((await approvedRow.innerText()).includes('Reeder on MacBook'));
    assert.equal(await approvedRow.getByRole('button', { name: 'Approve device', exact: true }).count(), 0);
    await approvedRow.getByRole('button', { name: 'Edit device name', exact: true }).click();
    await deviceDialog.getByLabel('Device name', { exact: true }).fill('Reeder desktop');
    await action(deviceDialog.getByRole('button', { name: 'Save device name', exact: true }), '/rename-device');
    assert.ok((await approvedRow.innerText()).includes('Reeder desktop'));
    assert.equal(await approvedRow.locator('.device-title').evaluate(node => node.firstElementChild.hasAttribute('data-device-edit')), true);
    let refs;
    for (let i = 0; i < 50; i++) { refs = await get('stream/items/ids'); if (refs.itemRefs.length) break; await sleep(100); }
    assert.equal(refs.itemRefs.length, 1);
    const originalItemIDs = refs.itemRefs.map(item => item.id);
    await action(approvedRow.getByRole('button', { name: 'Hide feeds', exact: true }), '/device-feeds');
    assert.equal(await approvedRow.getAttribute('data-feeds-hidden'), 'true');
    assert.deepEqual((await get('subscription/list')).subscriptions, []);
    assert.deepEqual((await get('stream/items/ids')).itemRefs, []);
    assert.equal(await approvedRow.getByRole('button', { name: 'Approve device', exact: true }).count(), 0);
    await action(approvedRow.getByRole('button', { name: 'Show feeds', exact: true }), '/device-feeds');
    assert.equal(await approvedRow.getAttribute('data-feeds-hidden'), 'false');
    assert.deepEqual((await get('stream/items/ids')).itemRefs.map(item => item.id), originalItemIDs);
    assert.ok((await approvedRow.innerText()).includes('Reeder desktop'));

    assert.equal(await page.getByRole('columnheader', { name: 'Type / status', exact: true }).count(), 0);
    assert.equal((await approvedRow.innerText()).includes('Never expires'), false);
    assert.equal(await page.locator('#reader-sessions').getByRole('columnheader', { name: 'Created', exact: true }).count(), 1);
    assert.equal(await page.locator('#reader-sessions').getByRole('columnheader', { name: 'Created / expires', exact: true }).count(), 0);
    await navigate('feeds');
    const publicationTime = page.locator('time[data-unix="1255536760"]').first();
    await publicationTime.waitFor({ timeout: 10000 });
    assert.equal(await publicationTime.getAttribute('data-time-zone'), 'Asia/Tokyo');
    assert.ok((await publicationTime.innerText()).includes('01:12:40'));
    assert.equal(await publicationTime.innerText(), '2009.10.15 01:12:40');
    assert.equal(/GMT|UTC/.test(await publicationTime.innerText()), false);
    const otherTimezone = await browser.newContext({ timezoneId: 'America/New_York', locale: 'en-GB' });
    await otherTimezone.addCookies(await context.cookies());
    const otherTimezonePage = await otherTimezone.newPage();
    await otherTimezonePage.goto(admin + '/');
    const nyTime = otherTimezonePage.locator('time[data-unix="1255536760"]').first();
    assert.equal(await nyTime.getAttribute('data-time-zone'), 'America/New_York');
    assert.ok((await nyTime.innerText()).includes('12:12:40'));
    assert.equal(await nyTime.innerText(), '2009.10.14 12:12:40');
    assert.equal(/GMT|UTC/.test(await nyTime.innerText()), false);
    await otherTimezone.close();
    await action(feedTable.getByRole('button', { name: 'Sync now', exact: true }), '/sync');
    assert.equal(await publicationTime.getAttribute('data-time-zone'), 'Asia/Tokyo');
    assert.equal(await publicationTime.innerText(), '2009.10.15 01:12:40');
    assert.equal(Object.hasOwn(refs, 'continuation'), false);
    const item = refs.itemRefs[0].id;
    const subscriptions = await get('subscription/list');
    const feed = subscriptions.subscriptions[0].id;
    const content = await context.request.post(api + '/reader/api/0/stream/items/contents', { headers, form: { i: BigInt(item).toString(16).padStart(16, '0'), T: 'x' } });
    assert.equal(content.status(), 200);
    const article = (await content.json()).items[0];
    assert.equal(article.title, 'Local article');
    const published = Date.parse('2009-10-14T16:12:40Z') / 1000;
    assert.equal(article.published, published);
    assert.equal(article.timestampUsec, String(published * 1000000));
    assert.equal(refs.itemRefs[0].timestampUsec, article.timestampUsec);
    assert.ok(Number(article.crawlTimeMsec) > Date.parse('2020-01-01T00:00:00Z'));
    const actionToken = (await (await context.request.get(api + '/reader/api/0/token', { headers })).text()).trim();
    for (const op of ['a', 'r']) {
      const change = await context.request.post(api + '/reader/api/0/edit-tag', { headers, form: { i: item, [op]: 'user/-/state/com.google/starred', T: actionToken } });
      assert.equal(change.status(), 200);
      await page.waitForFunction(expected => [...document.querySelectorAll('#summary .card')].find(n => n.querySelector('span')?.textContent === 'Starred')?.querySelector('strong').textContent === expected, op === 'a' ? '1' : '0');
      await navigate('starred');
      const starredSection = page.locator('#starred-articles');
      if (op === 'a') await starredSection.getByRole('link', { name: 'Local article', exact: true }).waitFor();
      else assert.ok((await starredSection.innerText()).includes('No starred articles.'));
      await navigate('feeds');
    }
    await feedTable.getByRole('button', { name: 'Edit name', exact: true }).click();
    await page.getByRole('textbox', { name: 'Subscription name', exact: true }).fill('My local feed');
    await action(page.getByRole('button', { name: 'Save name', exact: true }), '/rename');
    assert.equal(await feedTable.locator('.name strong').innerText(), 'My local feed');
    assert.equal((await get('subscription/list')).subscriptions[0].title, 'My local feed');
    await action(page.getByRole('button', { name: 'Sync all', exact: true }), '/sync');
    await page.waitForFunction(() => [...document.querySelectorAll('#summary .card')].find(n => n.querySelector('span')?.textContent === 'Unread')?.querySelector('strong').textContent === '1');
    await action(feedTable.getByRole('button', { name: 'Disable', exact: true }), '/disable');
    assert.equal((await get('subscription/list')).subscriptions.length, 0);
    assert.equal((await get('stream/items/ids')).itemRefs.length, 0);
    await sleep(100);
    const before = calls;
    await sleep(1500);
    assert.equal(calls, before, 'disabled feed was fetched');
    await disabledView(true);
    await page.getByRole('link', { name: 'Delete…', exact: true }).click();
    assert.equal(await page.getByRole('dialog', { name: 'Delete subscription?' }).isVisible(), true);
    assert.equal(await page.evaluate(() => document.activeElement?.textContent), 'Cancel');
    await page.getByRole('button', { name: 'Cancel', exact: true }).click();
    await action(feedTable.getByRole('button', { name: 'Enable', exact: true }), '/enable');
    assert.equal((await get('subscription/list')).subscriptions.length, 1);
    const unsubscribe = await context.request.post(api + '/reader/api/0/subscription/edit', { headers, form: { ac: 'unsubscribe', s: feed, T: 'x' } });
    assert.equal(unsubscribe.status(), 200);
    await page.waitForFunction(() => document.querySelector('#feed-list button[aria-label=Enable]') !== null, { timeout: 10000 });
    assert.equal(await feedTable.getByRole('button', { name: 'Enable', exact: true }).count(), 1);
    await page.getByRole('link', { name: 'Delete…', exact: true }).click();
    await action(page.getByRole('button', { name: 'Delete permanently', exact: true }), '/delete');
    assert.equal(await feedTable.getByText('No disabled subscriptions.', { exact: true }).count(), 1);
    await disabledView(false);
    assert.equal(await feedTable.getByText('Import an OPML file to add subscriptions.').count(), 1);
    assert.equal((await get('subscription/list')).subscriptions.length, 0);

    const extra = `<opml><body><outline text="Engineering"><outline text="A healthy" xmlUrl="${feedURL}"/><outline text="Z broken" xmlUrl="${feedURL.replace('/feed', '/broken')}"/></outline></body></opml>`;
    await navigate('import-export');
    await page.locator('input[name=opml]').setInputFiles({ name: 'extra.opml', mimeType: 'text/xml', buffer: Buffer.from(extra) });
    await action(page.getByRole('button', { name: 'Import OPML' }), '/import');
    await navigate('feeds');
    await page.waitForFunction(() => document.querySelector('#feed-list tr[data-feed-id]')?.dataset.title === 'Z broken');
    assert.ok(await feedTable.locator('tr[data-feed-id]').first().getAttribute('class').then(c => c.includes('has-error')));
    const healthyRow = feedTable.getByRole('row').filter({ hasText: 'A healthy' });
    await action(healthyRow.getByRole('button', { name: 'Pause', exact: true }), '/toggle');
    assert.ok((await feedTable.locator('tbody tr').first().innerText()).includes('A healthy'));
    assert.equal(await healthyRow.evaluate(node => node.classList.contains('is-paused')), true);
    await page.getByRole('button', { name: 'Add Git repository', exact: true }).click();
    const gitDialog = page.locator('#git-dialog');
    await gitDialog.getByLabel('Repository URL', { exact: true }).fill('https://git.haproxy.org/?p=haproxy-3.4.git;a=summary');
    assert.equal(await gitDialog.getByLabel('Track', { exact: true }).inputValue(), 'tags');
    assert.equal(await gitDialog.locator('option[value=releases]').evaluate(option => option.disabled), true);
    await gitDialog.getByLabel('Service', { exact: true }).selectOption('gitweb');
    assert.equal(await gitDialog.getByLabel('Track', { exact: true }).inputValue(), 'tags');
    await gitDialog.getByRole('button', { name: 'Cancel', exact: true }).click();
    await page.getByRole('button', { name: 'Add Git repository', exact: true }).click();
    await gitDialog.getByLabel('Repository URL', { exact: true }).fill(feedURL.replace('/feed', '/team/tool'));
    await action(gitDialog.getByRole('button', { name: 'Save Git source', exact: true }), '/git-source');
    const gitRow = page.getByRole('table', { name: 'GIT Releases Feeds', exact: true }).locator('tbody tr').filter({ hasText: ' :: team/tool' });
    await gitRow.waitFor();
    assert.ok((await gitRow.innerText()).includes('GIT_Releases'));
    assert.ok((await gitRow.innerText()).includes('forgejo'));
    const gitSubscription = (await get('subscription/list')).subscriptions.find(sub => sub.title.endsWith(' :: team/tool'));
    assert.ok(gitSubscription.iconUrl.endsWith('/forge-icon.svg'));
    let gitItems;
    for (let i = 0; i < 50; i++) {
      gitItems = await get('stream/contents/' + gitSubscription.id);
      if (gitItems.items.length) break;
      await sleep(100);
    }
    assert.equal(gitItems.items.length, 1);
    assert.ok(gitItems.items[0].summary.content.includes('<strong>Stable notes</strong>'));
    const forbiddenGit = await context.request.post(api + '/reader/api/0/subscription/edit', { headers, form: { s: gitSubscription.id, ac: 'edit', a: 'user/1/label/GIT_Releases' } });
    assert.equal(forbiddenGit.status(), 403);
    await gitRow.getByRole('button', { name: 'Git source settings', exact: true }).click();
    await gitDialog.getByLabel('Include test and preview versions', { exact: true }).check();
    await action(gitDialog.getByRole('button', { name: 'Save Git source', exact: true }), '/git-source');
    for (let i = 0; i < 50; i++) { gitItems = await get('stream/contents/' + gitSubscription.id); if (gitItems.items.length === 2) break; await sleep(100); }
    assert.equal(gitItems.items.length, 2);
    await gitRow.getByRole('button', { name: 'Git source settings', exact: true }).click();
    await gitDialog.getByLabel('Include test and preview versions', { exact: true }).uncheck();
    await gitDialog.getByLabel('Track', { exact: true }).selectOption('tags');
    await gitDialog.getByLabel('Tag filter', { exact: true }).fill('v4.*');
    await action(gitDialog.getByRole('button', { name: 'Save Git source', exact: true }), '/git-source');
    for (let i = 0; i < 50; i++) { gitItems = await get('stream/contents/' + gitSubscription.id); if (gitItems.items.some(item => item.title === 'v4.0.0')) break; await sleep(100); }
    assert.ok(gitItems.items.some(item => item.title === 'v4.0.0'));
    assert.ok(gitItems.items.every(item => item.title !== 'Tool preview'));
    assert.equal(await gitRow.getAttribute('data-git-tag-filter'), 'v4.*');
    assert.ok(gitItems.items.every(item => item.title.startsWith('v4.')));
    await gitRow.getByRole('button', { name: 'Git source settings', exact: true }).click();
    assert.equal(await gitDialog.getByLabel('Tag filter', { exact: true }).inputValue(), 'v4.*');
    await gitDialog.getByRole('button', { name: 'Cancel', exact: true }).click();
    const gitTable = page.getByRole('table', { name: 'GIT Releases Feeds', exact: true });
    assert.equal(await feedTable.locator('[data-git-edit]').count(), 0);
    assert.equal(await gitTable.locator('[data-git-edit]').count(), 1);
    assert.equal(await page.locator('section[aria-labelledby=git-feeds-heading] [data-git-add]').count(), 1);
    await navigate('import-export');
    const [roiDownload] = await Promise.all([
      page.waitForEvent('download'),
      page.getByRole('link', { name: 'Export ROI', exact: true }).click(),
    ]);
    assert.match(roiDownload.suggestedFilename(), /^roi-\d{8}T\d{6}\.\d{9}Z\.roi$/);
    const roiText = fs.readFileSync(await roiDownload.path(), 'utf8');
    assert.ok(roiText.includes('format: roi\n'));
    assert.ok(roiText.includes('mode: tags'));
    assert.ok(roiText.includes('tag_filter: v4.*'));
    assert.equal(roiText.includes('password_hash'), false);
    const opmlExport = await context.request.get(admin + '/export');
    assert.equal((await opmlExport.text()).includes('/team/tool'), false);
    assert.match(opmlExport.headers()['content-disposition'], /roi-\d{8}T\d{6}\.\d{9}Z\.opml/);
    assert.ok(roiText.includes(feedURL));
    assert.ok(roiText.includes('type: rss'));
    async function importROI(text) {
      await navigate('import-export');
      await page.getByLabel('ROI file', { exact: true }).setInputFiles({ name: 'sources.roi', mimeType: 'application/yaml', buffer: Buffer.from(text) });
      await action(page.getByRole('button', { name: 'Import ROI', exact: true }), '/import-roi');
      await navigate('feeds');
    }
    await importROI(roiText);
    assert.equal(await gitTable.locator('[data-git-edit]').count(), 1);
    const addedROI = roiText + '\n  - type: git\n    repository: ' + feedURL.replace('/feed', '/team/other') + '\n    service: forgejo\n    title: ROI imported Git\n    mode: releases\n    include_preview: false\n';
    await importROI(addedROI);
    assert.equal(await gitTable.locator('[data-git-edit]').count(), 2);
    assert.equal(await feedTable.getByText('ROI imported Git', { exact: true }).count(), 0);
    assert.equal(await gitTable.getByText('ROI imported Git', { exact: true }).count(), 1);
    await importROI(addedROI + '\n  - type: rss\n    url: ' + feedURL.replace('/feed', '/roi-rss') + '\n    title: ROI imported RSS\n    folders: [ROI/Test]\n    paused: true\n');
    const roiRSSRow = feedTable.locator('tr').filter({ hasText: 'ROI imported RSS' });
    assert.equal(await roiRSSRow.count(), 1);
    assert.equal(await roiRSSRow.getByText('ROI/Test', { exact: true }).count(), 1);
    assert.equal(await roiRSSRow.getByRole('button', { name: 'Resume', exact: true }).count(), 1);
    assert.equal(await gitTable.getByText('ROI imported RSS', { exact: true }).count(), 0);
    await action(page.getByRole('button', { name: 'Sync Git feeds', exact: true }), '/sync');
    async function resetFeedAndCheck(row) {
      const id = await row.getAttribute('data-feed-id');
      const title = await row.getAttribute('data-title');
      const repository = await row.getAttribute('data-repository');
      const gitMode = await row.getAttribute('data-git-mode');
      const refsRoute = 'stream/items/ids?s=' + encodeURIComponent('feed/' + id);
      const oldIDs = (await get(refsRoute)).itemRefs.map(item => item.id);
      await row.getByRole('button', { name: 'Reset feed', exact: true }).click();
      const dialog = page.getByRole('dialog', { name: 'Reset feed?', exact: true });
      assert.equal(await dialog.isVisible(), true);
      assert.equal(await dialog.locator('.reset-name').innerText(), title);
      assert.equal(await dialog.locator('.reset-url').innerText(), repository);
      assert.equal(await page.evaluate(() => document.activeElement?.textContent), 'Cancel');
      assert.ok((await dialog.innerText()).includes('read/starred state'));
      await dialog.getByRole('button', { name: 'Cancel', exact: true }).click();
      assert.deepEqual((await get(refsRoute)).itemRefs.map(item => item.id), oldIDs);
      await row.getByRole('button', { name: 'Reset feed', exact: true }).click();
      await action(dialog.getByRole('button', { name: 'Reset and resync', exact: true }), '/reset');
      assert.equal(await dialog.isVisible(), false);
      let freshIDs = [];
      for (let i = 0; i < 100; i++) {
        freshIDs = (await get(refsRoute)).itemRefs.map(item => item.id);
        if (freshIDs.length && freshIDs.every(id => !oldIDs.includes(id))) break;
        await sleep(100);
      }
      assert.ok(freshIDs.length > 0, 'reset feed was not downloaded again');
      assert.ok(freshIDs.every(id => !oldIDs.includes(id)), 'reset reused deleted article IDs');
      assert.equal(await row.getAttribute('data-title'), title);
      assert.equal(await row.getAttribute('data-repository'), repository);
      assert.equal(await row.getAttribute('data-git-mode'), gitMode);
      assert.equal(await page.evaluate(() => window.__roiDocumentMarker), 'preserved');
    }
    await resetFeedAndCheck(feedTable.locator('[data-feed-id]').filter({ hasText: 'A healthy' }));
    await resetFeedAndCheck(gitTable.locator('[data-feed-id]').filter({ hasText: '/team/tool' }));
    await resetFeedAndCheck(roiRSSRow);
    await page.waitForFunction(() => [...document.querySelectorAll('#feed-list tr')].some(row => row.textContent.includes('ROI imported RSS') && row.querySelector('button[aria-label="Pause"]')));
    if (process.env.ROI_SCREENSHOT_DIR) {
      fs.mkdirSync(process.env.ROI_SCREENSHOT_DIR, { recursive: true });
      await page.setViewportSize({ width: 1600, height: 1000 });
      await page.getByRole('button', { name: 'Light theme' }).click();
      await page.screenshot({ path: path.join(process.env.ROI_SCREENSHOT_DIR, 'admin-light.png'), fullPage: true });
      await page.getByRole('button', { name: 'Dark theme' }).click();
      await page.screenshot({ path: path.join(process.env.ROI_SCREENSHOT_DIR, 'admin-dark.png'), fullPage: true });
    }
    // Personal device actions do not affect other devices or reload the page.
    const temporaryCredentials = await createPersonalCredentials('Temporary API device', true);
    const personalTable = page.getByRole('table', { name: 'Reader API devices', exact: true });
    const temporaryRow = personalTable.locator('tr[data-api-device-id]').filter({ hasText: temporaryCredentials.login });
    await temporaryRow.getByRole('button', { name: 'Edit API device name', exact: true }).click();
    await page.locator('#api-device-dialog').getByLabel('Device name', { exact: true }).fill('Renamed temporary device');
    await action(page.locator('#api-device-dialog').getByRole('button', { name: 'Save name', exact: true }), '/api-devices/rename');
    assert.ok((await temporaryRow.innerText()).includes('Renamed temporary device'));
    await temporaryRow.getByRole('button', { name: 'Generate new sign-in code', exact: true }).click();
    await page.locator('#api-device-confirm-dialog').getByRole('button', { name: 'Cancel', exact: true }).click();
    await temporaryRow.getByRole('button', { name: 'Generate new sign-in code', exact: true }).click();
    await action(page.locator('#api-device-confirm-dialog').getByRole('button', { name: 'Confirm', exact: true }), '/api-devices/code');
    assert.equal(await page.locator('#api-reader-url').inputValue(), 'https://reader.example.org/greader');
    assert.equal(await page.locator('#api-personal-login').inputValue(), temporaryCredentials.login);
    const renewedCode = await page.locator('#api-sign-in-code').inputValue();
    assert.notEqual(renewedCode, temporaryCredentials.code);
    await page.locator('#api-credentials-dialog').getByRole('button', { name: 'Done', exact: true }).click();
    const invalidOldCode = await context.request.post(api + '/accounts/ClientLogin', { form: { Email: temporaryCredentials.login, Passwd: temporaryCredentials.code } });
    assert.equal(invalidOldCode.status(), 403);
    const temporaryLogin = await context.request.post(api + '/accounts/ClientLogin', { form: { Email: temporaryCredentials.login, Passwd: renewedCode } });
    assert.equal(temporaryLogin.status(), 200);
    const temporaryToken = (await temporaryLogin.text()).split('\n').find(line => line.startsWith('Auth=')).slice(5);
    await temporaryRow.getByRole('button', { name: 'Delete API device', exact: true }).click();
    await page.locator('#api-device-confirm-dialog').getByRole('button', { name: 'Cancel', exact: true }).click();
    assert.equal(await temporaryRow.count(), 1);
    await temporaryRow.getByRole('button', { name: 'Delete API device', exact: true }).click();
    await action(page.locator('#api-device-confirm-dialog').getByRole('button', { name: 'Confirm', exact: true }), '/api-devices/delete');
    assert.equal(await temporaryRow.count(), 0);
    const deletedDevice = await context.request.get(api + '/reader/api/0/user-info', { headers: { Authorization: 'GoogleLogin auth=' + temporaryToken } });
    assert.equal(deletedDevice.status(), 401);
    // A second Reader session can be inspected and revoked without affecting the first.
    const secondLogin = await personalLogin('Second Reader');
    assert.equal(secondLogin.status(), 200);
    const secondAuth = (await secondLogin.text()).split('\n').find(line => line.startsWith('Auth=')).slice(5);
    const secondID = createHash('sha256').update(secondAuth).digest('hex');
    const pendingSecond = await context.request.get(api + '/reader/api/0/subscription/list', { headers: { Authorization: 'GoogleLogin auth=' + secondAuth } });
    assert.deepEqual((await pendingSecond.json()).subscriptions, []);
    const sessionRow = page.locator(`[data-session-id="${secondID}"]`);
    await sessionRow.waitFor({ timeout: 10000 });
    assert.equal(await page.locator(`#reader-sessions [data-session-id="${secondID}"]`).count(), 1);
    assert.equal(await page.locator("#browser-sessions [data-session-id]").count(), 0);
    assert.ok((await sessionRow.innerText()).includes('127.0.0.1'));
    await sessionRow.getByRole('button', { name: 'Revoke session', exact: true }).click();
    await page.locator('#revoke-dialog').getByRole('button', { name: 'Cancel', exact: true }).click();
    assert.equal(await sessionRow.count(), 1);
    await revokeAction(sessionRow.getByRole('button', { name: 'Revoke session', exact: true }), '/revoke-session');
    assert.equal(await sessionRow.count(), 0, 'revoked session still displayed');
    const revokedOne = await context.request.get(api + '/reader/api/0/user-info', { headers: { Authorization: 'GoogleLogin auth=' + secondAuth } });
    assert.equal(revokedOne.status(), 401);
    const stillActive = await context.request.get(api + '/reader/api/0/user-info', { headers });
    assert.equal(stillActive.status(), 200);
    const failedAdmin = await context.request.post(admin + '/login', { form: { username: 'admin', password: 'incorrect-test-secret' } });
    assert.equal(failedAdmin.status(), 403);
    const failedAPI = await context.request.post(api + '/accounts/ClientLogin', { form: { Email: 'reader', Passwd: 'incorrect-test-secret' } });
    assert.equal(failedAPI.status(), 403);
    await navigate('security');
    assert.equal(await page.locator('#browser-sessions [data-session-id]').count(), 1);
    await page.waitForFunction(() => {
      const rows = [...document.querySelectorAll('#security-scroll tbody tr')].map(row => row.textContent);
      return ['admin', 'api'].every(service => rows.some(text => text.includes('Login failed') && text.includes(service) && text.includes('127.0.0.1')));
    }, null, { timeout: 10000 });
    assert.equal(await page.getByRole('heading', { name: 'Recent Reader API requests', exact: true }).count(), 0);
    assert.equal(await page.getByRole('heading', { name: 'Session history and admin events', exact: true }).count(), 0);
    assert.equal(await page.getByRole('link', { name: 'Session history', exact: true }).count(), 0);
    const icon = await context.request.get(admin + '/assets/favicon.svg');
    assert.equal(icon.status(), 200);
    assert.ok(icon.headers()['content-type'].includes('image/svg+xml'));
    assert.equal(await page.locator('link[rel=icon]').getAttribute('href'), '/assets/favicon.svg');
    assert.equal(await page.locator('.app-header h1').innerText(), 'ροή');
    async function assertMobileLayout(width) {
      await page.setViewportSize({ width, height: 844 });
      for (const link of await page.locator('.top-nav a').all()) {
        assert.equal(await link.locator('span').isVisible(), false);
        assert.equal(await link.locator('svg').isVisible(), true);
        assert.ok((await link.getAttribute('aria-label')).length > 0);
        const bounds = await link.boundingBox();
        assert.ok(bounds.width >= 44 && bounds.height >= 44);
      }
      const layout = await page.evaluate(() => {
        const viewport = document.documentElement.clientWidth;
        const overflowing = [...document.querySelectorAll('body *')].filter(element => {
          // Wide tables are intentionally scrollable inside their containers.
          if (element.closest('.scroll') || !element.getClientRects().length) return false;
          const bounds = element.getBoundingClientRect();
          return bounds.right > viewport + 1 || bounds.left < -1;
        }).map(element => ({ tag: element.tagName, id: element.id, class: element.className,
          text: element.textContent.trim().slice(0, 100), width: element.getBoundingClientRect().width }));
        return { viewport, pageWidth: document.documentElement.scrollWidth, overflowing };
      });
      if (layout.pageWidth > layout.viewport && process.env.ROI_SCREENSHOT_DIR) {
        fs.mkdirSync(process.env.ROI_SCREENSHOT_DIR, { recursive: true });
        await page.screenshot({ path: path.join(process.env.ROI_SCREENSHOT_DIR, `overflow-${width}.png`), fullPage: true });
      }
      assert.ok(layout.pageWidth <= layout.viewport, 'Horizontal page overflow: ' + JSON.stringify(layout));
    }
    for (const section of ['feeds', 'starred', 'devices', 'security', 'import-export', 'maintenance']) {
      await navigate(section);
      for (const width of [390, 320]) await assertMobileLayout(width);
    }

    const originalVersion = await page.locator('.build-version').innerText();
    await page.locator('.build-version').evaluate(element => element.textContent = 'v1.2.3-release-candidate.123 (123456789abc)');
    for (const width of [390, 320]) await assertMobileLayout(width);
    await page.locator('.build-version').evaluate((element, text) => element.textContent = text, originalVersion);
    await navigate('devices');
    // A full pending queue cannot hide an approved device permanently.
    for (let i = 0; i < 100; i++) {
      const pending = await personalLogin('Pending Reader ' + i);
      assert.equal(pending.status(), 200);
    }
    await page.waitForFunction(() => document.querySelector('#reader-sessions nav')?.textContent.includes('101 active sessions'), null, { timeout: 15000 });
    const pages = page.getByRole('navigation', { name: 'Reader API sessions pages', exact: true });
    await pages.getByRole('link', { name: 'Next', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('#reader-sessions nav')?.textContent.includes('Page 2'));
    assert.equal(await page.locator(`#reader-sessions [data-session-id="${authID}"]`).count(), 1);
    await pages.getByRole('link', { name: 'Previous', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('#reader-sessions nav')?.textContent.includes('Page 1'));
    assert.equal(documentRequests, 0, 'session pagination reloaded the dashboard');
    await revokeAction(page.getByRole('button', { name: 'Revoke API tokens', exact: true }), '/revoke-api-tokens');
    const revoked = await context.request.get(api + '/reader/api/0/user-info', { headers });
    assert.equal(revoked.status(), 401);
    const relogin = await personalLogin('Reader after global revoke');
    assert.equal(relogin.status(), 200);
    assert.equal(documentRequests, 0, 'dashboard actions reloaded the document');
    assert.equal(await page.evaluate(() => window.__roiDocumentMarker), 'preserved');
    await navigate('security');
    await page.locator('#browser-sessions').getByRole('button', { name: 'Revoke session', exact: true }).click();
    assert.equal(await page.locator('#revoke-dialog h2').innerText(), 'Revoke browser session?');
    assert.equal((await page.locator('#revoke-description').innerText()).includes('API'), false);
    await page.locator('#revoke-dialog').getByRole('button', { name: 'Cancel', exact: true }).click();
    await revokeAction(page.getByRole('button', { name: 'Revoke browser sessions', exact: true }), '/revoke-browser-sessions');
    await page.waitForURL(admin + '/login');
    assert.deepEqual(errors, []);
    console.log('Browser integration passed: persistent login/CSRF across server restart, build identity, permanent API sessions/browser revoke text/session columns/browser timezones and fragment localization, import preview/cancel/invalid blocking, RSS/Git reset confirmation/cancel/resync/new IDs/paused resume, themes, async actions without reload, rename/folders, disabled filter, unread, consistent icons/copy URL/clipboard fallback, separate RSS/Git tables, ROI export/import/deduplication, Git releases/tags/preview filtering/Markdown/icons/admin-only folder, device approval/default empty API/device naming, security events/individual revocation/IPs/favicon/delete confirmation, login/logout, OPML deduplication, Reader IDs/dates, token revocation, error counters, disabled polling/API isolation, enable, API unsubscribe, admin-only permanent deletion, mobile layout.');
  } finally {
    if (browser) await browser.close();
    if (app && app.exitCode === null) {
      const exited = new Promise(resolve => app.once('exit', resolve));
      app.kill('SIGTERM');
      await Promise.race([exited, sleep(3000)]);
      if (app.exitCode === null) app.kill('SIGKILL');
    }
    if (source) await new Promise(resolve => source.close(resolve));
    fs.rmSync(dir, { recursive: true, force: true });
  }
})().catch(e => { console.error(e); process.exitCode = 1; });
