import assert from 'node:assert/strict';
import { readFile, stat, mkdir } from 'node:fs/promises';
import { createHmac, randomBytes, randomUUID } from 'node:crypto';
import { resolve } from 'node:path';
import { chromium } from 'playwright-core';
import { verifyEditor } from '../editor/browser-form.mjs';

const fixturePath = process.argv[2];
if (!fixturePath || (await stat(fixturePath)).mode & 0o077) throw Error('A private runner fixture is required');
const fixture = JSON.parse(await readFile(fixturePath, 'utf8'));
if (!/^https:\/\/127\.0\.0\.1:\d+$/.test(fixture.origin)) throw Error('Only private loopback TLS fixtures are accepted');
if (!process.env.CHROME_EXECUTABLE) throw Error('CHROME_EXECUTABLE is required');
const password = randomBytes(24).toString('base64url');
const sensitive = [password, fixture.bootstrapLink];
const browser = await chromium.launch({ executablePath: process.env.CHROME_EXECUTABLE, headless: true });
let phase = 'launch';
try {
  const context = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1365, height: 900 } });
  const page = await context.newPage(); const pageErrors = [];
  page.on('pageerror', () => pageErrors.push('pageerror'));
  async function api(path, body, headers = {}) {
    return page.evaluate(async ({ path, body, headers }) => {
      if (body !== undefined && headers['X-CSRF-Token'] === undefined) {
        const c = await fetch('/api/v1/auth/csrf', { cache: 'no-store' });
        headers['X-CSRF-Token'] = (await c.json()).csrf_token;
      }
      const r = await fetch('/api/v1' + path, { method: body === undefined ? 'GET' : 'POST', credentials: 'same-origin',
        headers: body === undefined ? headers : { 'Content-Type': 'application/json', ...headers }, body: body === undefined ? undefined : JSON.stringify(body) });
      return { status: r.status, data: r.status === 204 ? null : await r.json() };
    }, { path, body, headers });
  }
  function otp(seed, step) {
    let bits = ''; for (const letter of seed) bits += 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'.indexOf(letter).toString(2).padStart(5, '0');
    const secret = Buffer.from(bits.match(/.{8}/g).map(x => parseInt(x, 2)));
    const counter = Buffer.alloc(8); counter.writeBigUInt64BE(BigInt(step));
    const h = createHmac('sha1', secret).update(counter).digest(); const offset = h[19] & 15;
    const code = ((h.readUInt32BE(offset) & 0x7fffffff) % 1000000).toString().padStart(6, '0');
    sensitive.push(code); return code;
  }
  phase = 'bootstrap';
  await page.goto(fixture.bootstrapLink, { waitUntil: 'networkidle' });
  assert.equal(new URL(page.url()).hash, '');
  // 2FA removed: the password alone completes enrollment and opens the session.
  await page.getByLabel('设置管理员密码', { exact: true }).fill(password);
  await page.getByRole('button', { name: '设置密码并启用', exact: true }).click();
  await page.getByTestId('signed-in-user').waitFor();
  const initial = await api('/me'); assert.equal(initial.status, 200); assert.deepEqual(initial.data.wallet, { available: '0', frozen: '0' });
  const cookies = await context.cookies();
  const session = cookies.find(c => c.name === '__Host-session');
  assert.ok(session?.secure && session.httpOnly && session.path === '/' && session.sameSite === 'Lax');
  phase = 'logout and password-only login';
  await page.getByRole('button', { name: '退出登录', exact: true }).click();
  await page.getByRole('heading', { name: '登录单据工作台', exact: true }).waitFor();
  assert.equal((await api('/me')).status, 401);
  await page.getByLabel('用户名', { exact: true }).fill(fixture.admin);
  await page.getByLabel('登录密码', { exact: true }).fill(password);
  await page.getByRole('button', { name: '登录', exact: true }).click();
  await page.getByTestId('signed-in-user').waitFor();
  // Password-only login yields the full session immediately.
  assert.equal((await api('/me')).status, 200);
  assert.equal((await api('/admin/users')).status, 200);
  phase = 'create zero-credit customer';
  await page.getByLabel('客户用户名', { exact: true }).fill(fixture.customer);
  await page.getByLabel('核验渠道引用', { exact: true }).fill('manual-test-channel');
  await page.getByLabel('开户原因', { exact: true }).fill('authorized browser integration test');
  const createdResponse = page.waitForResponse(r => r.request().method() === 'POST' && new URL(r.url()).pathname === '/api/v1/admin/users');
  await page.getByRole('button', { name: '新增客户', exact: true }).click();
  const created = await createdResponse; assert.equal(created.status(), 201);
  const customer = await created.json(); const createKey = created.request().headers()['idempotency-key'];
  sensitive.push(customer.setup_link);
  assert.deepEqual(customer.wallet, { available: '0', frozen: '0' });
  assert.equal(customer.user.role, 'USER'); assert.equal(customer.user.onboarding_state, 'NEEDS_SETUP');
  await page.getByTestId('customer-row').waitFor();
  assert.equal(await page.getByTestId('available').innerText(), '0');
  const repeatCreate = await api('/admin/users', { username: fixture.customer, customer_verification_ref: 'manual-test-channel', reason: 'authorized browser integration test' }, { 'Idempotency-Key': createKey });
  assert.equal(repeatCreate.status, 409); assert.equal(repeatCreate.data.code, 'SETUP_LINK_ALREADY_ISSUED');
  assert.equal(repeatCreate.data.setup_link, undefined);
  phase = 'grant and retry';
  await page.getByLabel('增加分值', { exact: true }).fill('5');
  await page.getByLabel('加分原因', { exact: true }).fill('authorized integration grant');
  await page.getByRole('button', { name: '增加积分', exact: true }).click();
  await page.getByTestId('grant-result').waitFor();
  const firstGrant = await page.getByTestId('grant-result').innerText();
  assert.match(firstGrant, /可用积分 5/);
  const grantRetry = page.waitForResponse(r => new URL(r.url()).pathname.endsWith('/credit-grants'));
  await page.getByRole('button', { name: '再次确认本次操作', exact: true }).click();
  assert.equal((await grantRetry).status(), 200); assert.equal(await page.getByTestId('grant-result').innerText(), firstGrant);
  const grantPath = `/admin/users/${customer.user.id}/credit-grants`;
  phase = 'CSRF and privilege tampering';
  assert.equal((await api(grantPath, { amount: '9', reason: 'must reject' }, { 'X-CSRF-Token': 'forged', 'Idempotency-Key': randomUUID() })).status, 403);
  assert.equal((await api(grantPath, { amount: '9', reason: 'must reject', role: 'SUPERADMIN' }, { 'Idempotency-Key': randomUUID() })).status, 400);
  const outsider = await browser.newContext({ ignoreHTTPSErrors: true });
  const c = await outsider.request.get(fixture.origin + '/api/v1/auth/csrf');
  const denied = await outsider.request.post(fixture.origin + '/api/v1' + grantPath, { data: { amount: '9', reason: 'must reject' },
    headers: { Origin: fixture.origin, 'X-CSRF-Token': (await c.json()).csrf_token, 'Idempotency-Key': randomUUID() } });
  assert.equal(denied.status(), 401); await outsider.close();
  phase = 'pagination and cursor scope';
  const secondCustomer = await api('/admin/users', { username: 'page_customer', customer_verification_ref: 'manual-test-channel', reason: 'pagination test' }, { 'Idempotency-Key': randomUUID() });
  assert.equal(secondCustomer.status, 201); sensitive.push(secondCustomer.data.setup_link);
  const users = await api('/admin/users?limit=1'); assert.equal(users.status, 200); assert.equal(users.data.items.length, 1);
  assert.equal(users.data.items[0].user.username, 'page_customer'); assert.equal(users.data.items[0].device_state, 'INITIAL_PENDING');
  const cursor = users.data.next_cursor; assert.equal(typeof cursor, 'string');
  const next = await api('/admin/users?limit=1&cursor=' + encodeURIComponent(cursor)); assert.equal(next.status, 200);
  assert.equal(next.data.items[0].user.username, fixture.customer); assert.equal(next.data.items[0].wallet.available, '5'); assert.equal(next.data.next_cursor, null);
  assert.equal((await api('/admin/users?username=flow&cursor=' + encodeURIComponent(cursor))).status, 400);
  assert.equal((await api('/admin/users?cursor=' + encodeURIComponent(cursor + 'x'))).status, 400);
  assert.equal((await api('/admin/users?limit=0')).status, 400);
  assert.equal((await api('/admin/users?limit=1&limit=2')).status, 400);
  phase = 're-login without second factor';
  assert.equal((await api('/auth/logout', {})).status, 204);
  assert.equal((await api('/auth/login', { username: fixture.admin, password })).status, 200);
  assert.equal((await api('/me')).status, 200);
  await page.reload({ waitUntil: 'networkidle' }); await page.getByTestId('signed-in-user').waitFor();
  phase = 'authenticated editor';
  await page.getByRole('button', { name: '制作单据', exact: true }).click();
  const screenshots = resolve('output/playwright'); await mkdir(screenshots, { recursive: true });
  // The admin editor now rides the same authenticated draft workspace: the
  // preview channel is real, cost display stays 0.
  await verifyEditor(page, screenshots, 'editor', { preview: 'server' });
  assert.equal(await page.getByText('管理员生成免积分', { exact: true }).count(), 1);
  assert.deepEqual(pageErrors, []);
  console.log(JSON.stringify({ status: 'PASS', browser: browser.version(), checks: ['protected CLI bootstrap without 2FA', 'Secure HttpOnly session', 'logout', 'password-only login', 'USER zero wallet', 'setup secret not replayed', 'grant retry once', 'CSRF and role tampering denied', 'anonymous grant denied', 'pagination and authenticated cursor', 'authenticated editor and admin cost display'] }));
} catch (error) {
  let message = String(error?.message ?? error);
  for (const value of sensitive) if (value) message = message.replaceAll(value, '[REDACTED]');
  console.error(`FAIL phase=${phase}: ${message}`); process.exitCode = 1;
} finally { await browser.close(); }
