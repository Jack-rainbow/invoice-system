// Real IndexedDB/CryptoKey checks. No fake-IDB implementation is used.
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { resolve, sep } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import ts from 'typescript';

if (!process.env.CHROME_EXECUTABLE) {
  throw new Error('Set CHROME_EXECUTABLE to an installed Chrome binary; this runner never downloads browsers');
}
const { chromium } = process.env.PLAYWRIGHT_CORE_PATH
  ? await import(pathToFileURL(resolve(process.env.PLAYWRIGHT_CORE_PATH, 'index.mjs')).href)
  : await import('playwright-core');
const root = resolve(fileURLToPath(new URL('../../', import.meta.url)));
const server = createServer(async (req, res) => {
  try {
    const pathname = new URL(req.url, 'http://localhost').pathname;
    if (pathname === '/') { res.setHeader('Content-Type', 'text/html'); res.end('<!doctype html><title>Device persistence test</title>'); return; }
    const filename = resolve(root, '.' + pathname);
    if (!filename.startsWith(root + sep) || !filename.endsWith('.ts')) { res.writeHead(404); res.end(); return; }
    const source = await readFile(filename, 'utf8');
    res.setHeader('Content-Type', 'text/javascript');
    res.end(ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022,
      module: ts.ModuleKind.ESNext } }).outputText);
  } catch { res.writeHead(404); res.end(); }
});
await new Promise((done, fail) => {
  server.once('error', fail); server.listen(0, '127.0.0.1', done);
});
const origin = `http://127.0.0.1:${server.address().port}`;
const profile = await mkdtemp(resolve(tmpdir(), 'invoice-device-browser-'));
let browser;
const launch = () => chromium.launchPersistentContext(profile, {
  executablePath: process.env.CHROME_EXECUTABLE, headless: true,
});
try {
  browser = await launch();
  let page = await browser.newPage();
  await page.goto(origin);
  const saved = await page.evaluate(async () => {
    const { openDeviceKeyStore } = await import('/src/shared/device/keyStore.ts');
    const store = await openDeviceKeyStore({ databaseName: 'invoice-device-real-browser-test' });
    const candidate = await store.createCandidateDeviceKey();
    await store.rememberBinding('binding-test', candidate.id, candidate.key.thumbprint);
    const key = await store.requireBoundDeviceKey('binding-test');
    if (key.privateKey.extractable) throw Error('Extractable private key');
    const others = await Promise.all([store.createCandidateDeviceKey(), store.createCandidateDeviceKey()]);
    if (others[0].id === others[1].id) throw Error('Candidate collision');
    store.close();
    return { id: candidate.id, thumbprint: candidate.key.thumbprint };
  });
  await page.reload();
  const reloadThumbprint = await page.evaluate(async () => {
    const { openDeviceKeyStore } = await import('/src/shared/device/keyStore.ts');
    const store = await openDeviceKeyStore({ databaseName: 'invoice-device-real-browser-test' });
    const key = await store.requireBoundDeviceKey('binding-test');
    store.close(); return key.thumbprint;
  });
  assert.equal(reloadThumbprint, saved.thumbprint);
  await browser.close(); browser = undefined;
  browser = await launch(); page = await browser.newPage(); await page.goto(origin);
  const restarted = await page.evaluate(async ({ id, thumbprint }) => {
    const { openDeviceKeyStore } = await import('/src/shared/device/keyStore.ts');
    const { signBytes, verifyBytes } = await import('/src/shared/device/keys.ts');
    const store = await openDeviceKeyStore({ databaseName: 'invoice-device-real-browser-test' });
    const key = await store.requireBoundDeviceKey('binding-test');
    const bytes = new TextEncoder().encode('after-browser-restart');
    const valid = await verifyBytes(key.publicKey, bytes, await signBytes(key.privateKey, bytes));
    let exportRejected = false;
    try { await crypto.subtle.exportKey('jwk', key.privateKey); } catch { exportRejected = true; }
    let mismatchRejected = false;
    try { await store.rememberBinding('another-binding', id, 'wrong-thumbprint'); } catch { mismatchRejected = true; }
    // Delete only this test's record and prove no lookup creates replacement keys.
    await new Promise((done, fail) => {
      const request = indexedDB.open('invoice-device-real-browser-test', 1);
      request.onerror = () => fail(request.error);
      request.onsuccess = () => {
        const db = request.result; const tx = db.transaction('keys', 'readwrite');
        tx.objectStore('keys').delete(id); tx.oncomplete = () => { db.close(); done(); }; tx.onerror = () => fail(tx.error);
      };
    });
    let lostRejected = false;
    try { await store.requireBoundDeviceKey('binding-test'); } catch (e) { lostRejected = e.code === 'DEVICE_KEY_UNAVAILABLE'; }
    let missingRejected = false;
    try { await store.requireExistingDeviceKey(id); } catch (e) { missingRejected = e.code === 'DEVICE_KEY_UNAVAILABLE'; }
    store.close();
    await new Promise((done, fail) => {
      const request = indexedDB.deleteDatabase('invoice-device-real-browser-test');
      request.onsuccess = () => done(); request.onerror = () => fail(request.error); request.onblocked = () => fail(Error('delete blocked'));
    });
    const empty = await openDeviceKeyStore({ databaseName: 'invoice-device-real-browser-test' });
    let erasedRejected = false;
    try { await empty.requireBoundDeviceKey('binding-test'); } catch (e) { erasedRejected = e.code === 'DEVICE_KEY_UNAVAILABLE'; }
    empty.close();
    return { sameThumbprint: key.thumbprint === thumbprint, privateExtractable: key.privateKey.extractable,
      valid, exportRejected, mismatchRejected, lostRejected, missingRejected, erasedRejected };
  }, saved);
  assert.deepEqual(restarted, { sameThumbprint: true, privateExtractable: false, valid: true, exportRejected: true,
    mismatchRejected: true, lostRejected: true, missingRejected: true, erasedRejected: true });
  console.log('PASS real Chrome IndexedDB: CryptoKey reload/restart, private export denial, concurrent candidates, fingerprint mismatch, lost record and erased database fail closed');
} finally {
  if (browser) await browser.close();
  await new Promise((done) => server.close(done));
  await rm(profile, { recursive: true, force: true });
}
