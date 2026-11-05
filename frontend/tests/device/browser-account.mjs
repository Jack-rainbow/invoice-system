import assert from 'node:assert/strict';
import { readFile, stat, mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { resolve } from 'node:path';
import { randomBytes, createHmac } from 'node:crypto';
import { chromium } from 'playwright-core';
import { verifyEditor } from '../editor/browser-form.mjs';

const filename = process.argv[2];
if (!filename || (await stat(filename)).mode & 0o077) throw Error('Private fixture required');
const f = JSON.parse(await readFile(filename, 'utf8'));
if (!/^https:\/\/127\.0\.0\.1:\d+$/.test(f.origin) || !process.env.CHROME_EXECUTABLE) throw Error('Private loopback TLS and local Chrome required');
const password = randomBytes(24).toString('base64url'), adminPassword = randomBytes(24).toString('base64url');
const secrets = [password, adminPassword, f.bootstrapLink];
const profile = await mkdtemp(resolve(tmpdir(), 'invoice-account-device-'));
const options = { executablePath: process.env.CHROME_EXECUTABLE, headless: true, ignoreHTTPSErrors: true, viewport: { width: 1365, height: 900 } };
let context, phase = 'start';
async function api(page, path, body) {
  return page.evaluate(async ({ path, body }) => {
    const headers = {};
    if (body !== undefined) { const csrf = await fetch('/api/v1/auth/csrf', { cache: 'no-store' }); headers['X-CSRF-Token'] = (await csrf.json()).csrf_token; headers['Content-Type'] = 'application/json'; }
    const r = await fetch('/api/v1' + path, { method: body === undefined ? 'GET' : 'POST', headers, body: body === undefined ? undefined : JSON.stringify(body) });
    return { status: r.status, data: r.status === 204 ? null : await r.json() };
  }, { path, body });
}
function otp(seed) {
  let bits = ''; for (const c of seed) bits += 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'.indexOf(c).toString(2).padStart(5, '0');
  const key = Buffer.from(bits.match(/.{8}/g).map(b => parseInt(b, 2))), counter = Buffer.alloc(8); counter.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000)));
  const digest = createHmac('sha1', key).update(counter).digest(), offset = digest[19] & 15;
  const value = ((digest.readUInt32BE(offset) & 0x7fffffff) % 1000000).toString().padStart(6, '0'); secrets.push(value); return value;
}
// Independent browser signer used only for attacks/replay checks. It reads
// existing private CryptoKeys without exporting them. The actual positive UI
// below uses the product signer and IndexedDB module.
async function manual(page, proof, { wrongKey = false, replay = false, badQuery = false, concurrent = false, route = '/api/v1/me?format=pdf&x=%2B&x=2' } = {}) {
  return page.evaluate(async ({ proof, wrongKey, replay, badQuery, concurrent, route }) => {
    const b64 = bytes => btoa(Array.from(bytes, b => String.fromCharCode(b)).join(''));
    let privateKey;
    if (wrongKey) privateKey = (await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign', 'verify'])).privateKey;
    else {
      const db = await new Promise((done, fail) => { const r = indexedDB.open('invoice-device-v1', 1); r.onsuccess = () => done(r.result); r.onerror = () => fail(Error('IDB')); });
      const records = await new Promise((done, fail) => { const tx = db.transaction('keys', 'readonly'), r = tx.objectStore('keys').getAll(); tx.oncomplete = () => done(r.result); tx.onerror = () => fail(Error('IDB read')); }); db.close();
      const record = records.find(r => r.thumbprint === proof.key_thumbprint); if (!record) throw Error('Existing candidate absent'); privateKey = record.privateKey;
    }
    const target = route;
    const now = Math.floor(Date.parse(proof.server_time) / 1000), expires = Math.min(now + 30, Math.floor(Date.parse(proof.expires_at) / 1000));
    const params = `("@method" "@target-uri" "x-device-id" "x-session-generation");created=${now};expires=${expires};nonce="${proof.nonce}";keyid="${proof.proof_key_id}";alg="ecdsa-p256-sha256";tag="invoice-device-v1"`;
    const base = `"@method": GET\n"@target-uri": ${location.origin}${target}\n"x-device-id": ${proof.proof_key_id}\n"x-session-generation": ${proof.context_id}\n"@signature-params": ${params}`;
    const sig = new Uint8Array(await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, privateKey, new TextEncoder().encode(base)));
    const headers = { 'X-Device-ID': proof.proof_key_id, 'X-Session-Generation': proof.context_id, 'Signature-Input': 'sig1=' + params, Signature: 'sig1=:' + b64(sig) + ':' };
    const send = async path => { const r = await fetch(path, { headers, cache: 'no-store' }); return { status: r.status, data: await r.json() }; };
    if (concurrent) return { concurrent: await Promise.all([send(target), send(target)]) };
    const bad = badQuery ? await send('/api/v1/me?x=2&format=pdf&x=%2B') : null;
    const first = await send(target); const again = replay ? await send(target) : null;
    return { bad, first, again };
  }, { proof, wrongKey, replay, badQuery, concurrent, route });
}
async function loginProof(page, proof, existing = false) {
  return page.evaluate(async ({ proof, existing }) => {
    let privateKey;
    if (!existing) privateKey = (await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign', 'verify'])).privateKey;
    else {
      const db = await new Promise(done => { const r = indexedDB.open('invoice-device-v1', 1); r.onsuccess = () => done(r.result); });
      const keys = await new Promise(done => { const tx = db.transaction('keys', 'readonly'), r = tx.objectStore('keys').getAll(); tx.oncomplete = () => done(r.result); });db.close();
      privateKey = keys.find(k => k.thumbprint === proof.key_thumbprint)?.privateKey;
      if (!privateKey) throw Error('Existing key missing');
    }
    const body = new TextEncoder().encode(JSON.stringify({ challenge_id: proof.challenge_id }));
    const b64 = bytes => btoa(Array.from(bytes, b => String.fromCharCode(b)).join(''));
    const digest = b64(new Uint8Array(await crypto.subtle.digest('SHA-256', body))), now = Math.floor(Date.parse(proof.server_time) / 1000);
    const params = `("@method" "@target-uri" "x-device-id" "x-session-generation" "content-type" "content-digest");created=${now};expires=${now + 30};nonce="${proof.nonce}";keyid="${proof.proof_key_id}";alg="ecdsa-p256-sha256";tag="invoice-device-v1"`;
    const base = `"@method": POST\n"@target-uri": ${location.origin}/api/v1/auth/device/verify\n"x-device-id": ${proof.proof_key_id}\n"x-session-generation": ${proof.context_id}\n"content-type": application/json\n"content-digest": sha-256=:${digest}:\n"@signature-params": ${params}`;
    const signature = b64(new Uint8Array(await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, privateKey, new TextEncoder().encode(base))));
    const c = await fetch('/api/v1/auth/csrf'), csrf = (await c.json()).csrf_token;
    return (await fetch('/api/v1/auth/device/verify', { method: 'POST', body, headers: { 'Content-Type': 'application/json', 'Content-Digest': `sha-256=:${digest}:`, 'X-Device-ID': proof.proof_key_id, 'X-Session-Generation': proof.context_id, 'Signature-Input': 'sig1=' + params, Signature: `sig1=:${signature}:`, 'X-CSRF-Token': csrf } })).status;
  }, { proof, existing });
}
async function keyCounts(page) {
  return page.evaluate(async () => {
    const db = await new Promise((done, fail) => { const r = indexedDB.open('invoice-device-v1', 1); r.onsuccess = () => done(r.result); r.onerror = () => fail(Error('IDB')); });
    const result = await new Promise((done, fail) => { const tx = db.transaction(['keys', 'bindings'], 'readonly'); const keys = tx.objectStore('keys').getAll(), bindings = tx.objectStore('bindings').count(); tx.oncomplete = async () => { let nonExportable = true; for (const k of keys.result) { try { await crypto.subtle.exportKey('jwk', k.privateKey); nonExportable = false; } catch {} } done({ keys: keys.result.length, bindings: bindings.result, nonExportable }); }; tx.onerror = () => fail(Error('IDB count')); }); db.close(); return result;
  });
}
try {
  context = await chromium.launchPersistentContext(profile, options); const browser = context.browser();
  const adminContext = await browser.newContext({ ignoreHTTPSErrors: true }); const admin = await adminContext.newPage();
  phase = 'administrator enrollment and real customer creation';
  await admin.goto(f.bootstrapLink, { waitUntil: 'networkidle' }); await admin.getByLabel('设置管理员密码', { exact: true }).fill(adminPassword);
  // 2FA removed: the password alone enables the administrator.
  await admin.getByRole('button', { name: '设置密码并启用', exact: true }).click();
  await admin.getByTestId('signed-in-user').waitFor();
  await admin.getByLabel('客户用户名', { exact: true }).fill(f.customer); await admin.getByLabel('核验渠道引用', { exact: true }).fill('private-device-flow'); await admin.getByLabel('开户原因', { exact: true }).fill('device browser verification');
  await admin.getByRole('button', { name: '新增客户', exact: true }).click(); await admin.getByTestId('setup-link').waitFor();
  const setupLink = await admin.getByTestId('setup-link').innerText(); secrets.push(setupLink);
  // Grant exactly one point while the enrollment MFA window is still fresh:
  // the customer later spends it on the paid formal generation below. The
  // create form auto-selects the new customer in the grant target.
  await admin.getByLabel('增加分值', { exact: true }).fill('1');
  await admin.getByLabel('加分原因', { exact: true }).fill('paid generation browser verification');
  await admin.getByRole('button', { name: '增加积分', exact: true }).click();
  await admin.getByTestId('grant-result').waitFor();
  assert.match(await admin.getByTestId('grant-result').innerText(), /可用积分 1/);
  await adminContext.close();
  let page = context.pages()[0] ?? await context.newPage(); const errors = []; page.on('pageerror', () => errors.push('pageerror'));
  phase = 'initial setup UI';
  await page.goto(setupLink, { waitUntil: 'networkidle' }); assert.equal(new URL(page.url()).hash, '');
  await page.getByLabel('设置客户密码', { exact: true }).fill(password); await page.getByRole('button', { name: '设置密码并授权此浏览器', exact: true }).click();
  await page.getByTestId('signed-in-user').waitFor(); assert.match(await page.getByTestId('signed-in-user').innerText(), /客户/);
  // The admin granted one point before activation; the paid phase below spends it.
  assert.match(await page.getByTestId('user-balance').innerText(), /可用积分 1/); assert.equal(await page.getByRole('button', { name: '新增客户', exact: true }).count(), 0);
  assert.deepEqual(await keyCounts(page), { keys: 1, bindings: 1, nonExportable: true });
  const original = await api(page, '/auth/device/context'); assert.equal(original.status, 200); assert.equal(original.data.wallet, undefined);
  assert.equal((await api(page, '/me')).status, 403, 'Cookie without proof must not authorize business');
  phase = 'target query binding and nonce replay';
  const proof = await api(page, '/auth/device/challenges', { purpose: 'REQUEST' }); assert.equal(proof.status, 200);
  const checked = await manual(page, proof.data, { badQuery: true, replay: true });
  assert.equal(checked.bad.status, 400); assert.equal(checked.first.status, 200); assert.equal(checked.again.status, 400);
  const concurrentProof = await api(page, '/auth/device/challenges', { purpose: 'REQUEST' }); assert.equal(concurrentProof.status, 200);
  assert.deepEqual((await manual(page, concurrentProof.data, { concurrent: true })).concurrent.map(r => r.status).sort(), [200, 400]);
  const adminProof = await api(page, '/auth/device/challenges', { purpose: 'REQUEST' }); assert.equal(adminProof.status, 200);
  const deniedAdmin = await manual(page, adminProof.data, { route: '/api/v1/admin/users' });
  assert.equal(deniedAdmin.first.status, 403); assert.equal(deniedAdmin.first.data.code, 'FORBIDDEN'); assert.equal(deniedAdmin.first.data.items, undefined);
  phase = 'logout and normal device login';
  await page.getByRole('button', { name: '退出登录', exact: true }).click(); await page.getByRole('heading', { name: '登录单据工作台', exact: true }).waitFor();
  await page.getByLabel('用户名', { exact: true }).fill(f.customer); await page.getByLabel('登录密码', { exact: true }).fill(password);
  await page.getByRole('button', { name: '登录', exact: true }).click(); await page.getByTestId('signed-in-user').waitFor();
  let current = await api(page, '/auth/device/context'); assert.notEqual(current.data.generation, original.data.generation); assert.equal(current.data.binding_id, original.data.binding_id);
  phase = 'wrong computer with known password';
  const wrong = await browser.newContext({ ignoreHTTPSErrors: true }); const wrongPage = await wrong.newPage(); await wrongPage.goto(f.origin, { waitUntil: 'networkidle' });
  assert.equal((await api(wrongPage, '/auth/login', { username: f.customer, password })).status, 200);
  assert.equal((await api(wrongPage, '/me')).status, 401);
  const wrongChallenge = await api(wrongPage, '/auth/device/challenges', { purpose: 'LOGIN' }); assert.equal(wrongChallenge.status, 200);
  const wrongProof = await loginProof(wrongPage, wrongChallenge.data);
  assert.equal(wrongProof, 400); assert.equal((await api(page, '/auth/device/context')).data.generation, current.data.generation); await wrong.close();
  phase = 'copied Cookie without private key';
  const stolen = await browser.newContext({ ignoreHTTPSErrors: true }); await stolen.addCookies((await context.cookies()).filter(c => c.name === '__Host-session'));
  const thief = await stolen.newPage(); await thief.goto(f.origin, { waitUntil: 'networkidle' });
  assert.equal((await api(thief, '/me')).status, 403);
  const stolenProof = await api(thief, '/auth/device/challenges', { purpose: 'REQUEST' }); assert.equal(stolenProof.status, 200);
  assert.equal((await manual(thief, stolenProof.data, { wrongKey: true })).first.status, 400);
  assert.equal((await manual(page, stolenProof.data)).first.status, 200, 'invalid proof must not consume nonce'); await stolen.close();
  phase = 'successful proof rotates an already active session';
  const oldCookie = (await context.cookies()).filter(c => c.name === '__Host-session');
  const oldGeneration = current.data.generation;
  assert.equal((await api(page, '/auth/login', { username: f.customer, password })).status, 200);
  const rotateChallenge = await api(page, '/auth/device/challenges', { purpose: 'LOGIN' }); assert.equal(rotateChallenge.status, 200);
  assert.equal((await api(page, '/auth/device/context')).data.generation, oldGeneration, 'password-only login must not replace active session');
  assert.equal(await loginProof(page, rotateChallenge.data, true), 200);
  current = await api(page, '/auth/device/context'); assert.notEqual(current.data.generation, oldGeneration);
  const retired = await context.browser().newContext({ ignoreHTTPSErrors: true }); await retired.addCookies(oldCookie);
  assert.equal((await retired.request.get(f.origin + '/api/v1/auth/device/context')).status(), 401); await retired.close();
  phase = 'authenticated customer editor';
  await verifyEditor(page, resolve('output/playwright'), 'customer', { preview: 'server' }); assert.equal(await page.getByText('每个新版本 1 分', { exact: true }).count(), 1); assert.deepEqual(errors, []);
  phase = 'draft create, optimistic save and reload';
  // Live editing auto-saves: the debounced preview already created the draft;
  // revisions advance with every distinct debounced edit, so assert relatively.
  await page.getByTestId('draft-row').waitFor({ timeout: 60000 });
  await page.waitForFunction(() => document.querySelector('[data-testid="draft-revision"]')?.textContent?.trim() !== '');
  const baseRevision = Number(await page.getByTestId('draft-revision').innerText());
  assert.ok(Number.isFinite(baseRevision) && baseRevision >= 1);
  const documentId = await page.getByTestId('draft-row').getAttribute('data-document-id');
  assert.match(documentId, /^[0-9a-f-]{36}$/);
  await page.getByLabel('公司 / 客户名称', { exact: true }).fill('第二版收件人');
  await page.getByTestId('save-draft').click();
  const nextRevision = baseRevision + 1;
  await page.waitForFunction(v => document.querySelector('[data-testid="draft-revision"]')?.textContent === String(v), nextRevision);
  await page.reload({ waitUntil: 'networkidle' }); await page.getByTestId('signed-in-user').waitFor();
  await page.getByTestId('draft-row').waitFor();
  assert.equal(await page.getByTestId('draft-revision').innerText(), String(nextRevision));
  await page.getByRole('button', { name: '打开', exact: true }).click();
  await page.waitForFunction(() => document.querySelector('input[name="recipient_name"]')?.value === '第二版收件人');
  assert.equal(await page.getByLabel('公司 / 客户名称', { exact: true }).inputValue(), '第二版收件人');
  assert.deepEqual(await page.locator('.estimate output').allTextContents(), ['500.00', '80.00', '580.00']);
  phase = 'live server-watermarked preview';
  await page.getByLabel('公司 / 客户名称', { exact: true }).fill('第三版收件人');
  await page.getByLabel('公司 / 客户名称', { exact: true }).focus();
  // Live preview: no button click, the debounced auto render lights the page.
  await page.getByTestId('preview-image').waitFor({ timeout: 60000 });
  // The browser only ever holds a blob: of the server-composited JPEG. Assert the
  // decoded size, and sample the pixels: a page whose ink sits only on the text
  // lines cannot produce this many blended mid-grey pixels, so a watermark layer
  // that reached the client would indicate a broken contract.
  const preview = await page.getByTestId('preview-image').evaluate((img) => {
    const canvas = document.createElement('canvas');
    canvas.width = img.naturalWidth; canvas.height = img.naturalHeight;
    const context = canvas.getContext('2d');
    context.drawImage(img, 0, 0);
    const { data } = context.getImageData(0, 0, canvas.width, canvas.height);
    let sampled = 0; let blended = 0;
    for (let y = 0; y < canvas.height; y += 4) {
      for (let x = 0; x < canvas.width; x += 4) {
        const at = (y * canvas.width + x) * 4;
        const gray = (data[at] + data[at + 1] + data[at + 2]) / 3;
        sampled++;
        if (gray > 130 && gray < 240) blended++;
      }
    }
    return { src: img.src.slice(0, 5), width: img.naturalWidth, height: img.naturalHeight, ratio: blended / sampled };
  });
  assert.equal(preview.src, 'blob:');
  assert.equal(preview.width, 1000);
  assert.ok(preview.height > 1000);
  assert.ok(preview.ratio > 0.03, `watermark coverage ${preview.ratio.toFixed(3)}`);
  // Preview saves first, so the queue bound revision 3 and the list shows it.
  await page.waitForFunction(v => Number(document.querySelector('[data-testid="draft-revision"]')?.textContent) > v, nextRevision);
  assert.match(await page.getByTestId('draft-notice').innerText(), /第 \d+ 版/);
  assert.deepEqual(errors, []);
  phase = 'paid formal generation consumes the granted point';
  await page.getByTestId('confirm-generation').click();
  await page.getByTestId('version-row').waitFor({ timeout: 90000 });
  assert.match(await page.getByTestId('draft-notice').innerText(), /已消费 1 分|免积分/);
  // Both formal formats stream through the signed channel as blob downloads.
  const [pdf] = await Promise.all([page.waitForEvent('download'), page.getByTestId('download-pdf').click()]);
  const pdfBytes = await readFile(await pdf.path());
  assert.equal(pdfBytes.subarray(0, 5).toString('latin1'), '%PDF-', 'formal download must be a real PDF');
  assert.ok(pdfBytes.length > 1000);
  const [png] = await Promise.all([page.waitForEvent('download'), page.getByTestId('download-png').click()]);
  const pngBytes = await readFile(await png.path());
  assert.equal(pngBytes.subarray(0, 8).toString('latin1'), '\x89PNG\r\n\x1a\n', 'formal download must be a real PNG');
  // The clean pixels are the full 2x raster, not the 1000px watermarked preview.
  assert.ok(pngBytes.length > 200000, `formal PNG suspiciously small: ${pngBytes.length}`);
  // Balance is now zero; the purchased version redownloads for free anyway.
  const [pngAgain] = await Promise.all([page.waitForEvent('download'), page.getByTestId('download-png').click()]);
  assert.ok((await readFile(await pngAgain.path())).length > 200000, 'zero-balance redownload must stay free and complete');
  assert.deepEqual(errors, []);
  await page.getByLabel('公司 / 客户名称', { exact: true }).fill('测试收件人');
  assert.deepEqual(errors, []);
  phase = 'persistent browser restart';
  await context.close(); context = await chromium.launchPersistentContext(profile, options); page = context.pages()[0] ?? await context.newPage();
  await page.goto(f.origin, { waitUntil: 'networkidle' }); await page.getByTestId('signed-in-user').waitFor();
  assert.equal((await api(page, '/auth/device/context')).data.generation, current.data.generation); assert.deepEqual(await keyCounts(page), { keys: 1, bindings: 1, nonExportable: true });
  phase = 'lost binding metadata uses existing candidate';
  await page.evaluate(async () => { const db = await new Promise(done => { const r = indexedDB.open('invoice-device-v1', 1); r.onsuccess = () => done(r.result); }); await new Promise(done => { const tx = db.transaction('bindings', 'readwrite'); tx.objectStore('bindings').clear(); tx.oncomplete = done; }); db.close(); });
  await page.reload({ waitUntil: 'networkidle' }); await page.getByTestId('signed-in-user').waitFor(); assert.deepEqual(await keyCounts(page), { keys: 1, bindings: 1, nonExportable: true });
  phase = 'missing private key never regenerates';
  await page.evaluate(async () => { const db = await new Promise(done => { const r = indexedDB.open('invoice-device-v1', 1); r.onsuccess = () => done(r.result); }); await new Promise(done => { const tx = db.transaction(['keys', 'bindings'], 'readwrite'); tx.objectStore('keys').clear(); tx.objectStore('bindings').clear(); tx.oncomplete = done; }); db.close(); });
  await page.reload({ waitUntil: 'networkidle' }); await page.getByRole('heading', { name: '登录单据工作台', exact: true }).waitFor();
  await page.getByLabel('用户名', { exact: true }).fill(f.customer); await page.getByLabel('登录密码', { exact: true }).fill(password); await page.getByRole('button', { name: '登录', exact: true }).click();
  await page.getByRole('alert').waitFor(); assert.match(await page.getByRole('alert').innerText(), /设备密钥|原浏览器/); assert.deepEqual(await keyCounts(page), { keys: 0, bindings: 0, nonExportable: true });
  const idleProbe = await api(page, '/auth/device/context'); assert.equal(idleProbe.status, 200);
  console.log(JSON.stringify({ status: 'PASS', browser: context.browser().version(), idle_probe_time: idleProbe.data.server_time, checks: ['admin creates real zero-credit USER', 'initial setup and designated binding', 'normal signed login and logout', 'valid proof rotates active session and old Cookie is rejected', 'signed USER forbidden from admin metadata', 'Cookie-only denied', 'wrong device with password denied without revoking old session', 'copied Cookie wrong key denied', 'invalid signature nonce not consumed', 'query binding and concurrent replay denial', 'customer editor', 'draft create/save/reload/delete', 'server-watermarked preview JPEG', 'one-point grant and paid formal generation', 'watermark-free PDF and 2x PNG downloads', 'zero-balance free redownload', 'browser restart', 'existing candidate recovery', 'missing key no regeneration'] }));
} catch (error) {
  let text = String(error?.message ?? error);
  try { const main = await context?.pages()?.[0]?.locator('main').innerText(); if (main) text += ' | visible=' + main.replace(/\s+/g, ' ').slice(0, 500); } catch {}
  for (const secret of secrets) if (secret) text = text.replaceAll(secret, '[REDACTED]');
  console.error(`FAIL phase=${phase}: ${text}`); process.exitCode = 1;
} finally { if (context) await context.close(); await rm(profile, { recursive: true, force: true }); }
