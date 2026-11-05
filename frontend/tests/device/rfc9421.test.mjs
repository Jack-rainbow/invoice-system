import test from 'node:test';
import assert from 'node:assert/strict';
import { prepareDeviceRequest, signDeviceRequest, structuredString } from '../../src/shared/device/rfc9421.ts';
import { createDeviceKey, verifyBytes } from '../../src/shared/device/keys.ts';
import { digest, requestInput, getSignatureBase, getSignatureInput, postSignatureBase } from './fixtures.mjs';

test('GET preserves original escaped path/query and uses exact RFC signature base', async () => {
  const result = await prepareDeviceRequest(requestInput({ target: '/api/v1/versions/abc/download?format=pdf&x=%2B&x=2' }));
  assert.equal(result.signatureBase, getSignatureBase);
  assert.equal(result.headers['signature-input'], getSignatureInput);
  assert.equal(result.headers['content-digest'], undefined);
  assert.equal(result.body, undefined);
});

test('POST signs exact raw bytes, content type/digest and idempotency key', async () => {
  const result = await prepareDeviceRequest(requestInput({ method: 'POST', target: '/api/v1/documents',
    body: new TextEncoder().encode('{"hello":"world"}'), contentType: 'application/json',
    idempotencyKey: '33333333-3333-4333-8333-333333333333' }));
  assert.equal(result.headers['content-digest'], digest);
  assert.equal(result.signatureBase, postSignatureBase);
});

test('PATCH/DELETE body digest covers empty bytes and a one-byte mutation', async () => {
  const first = await prepareDeviceRequest(requestInput({ method: 'PATCH', body: new Uint8Array(0), contentType: 'application/json' }));
  const second = await prepareDeviceRequest(requestInput({ method: 'DELETE', body: new Uint8Array([32]), contentType: 'application/json' }));
  assert.equal(first.headers['content-digest'], 'sha-256=:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=:');
  assert.notEqual(second.headers['content-digest'], first.headers['content-digest']);
});

test('signature binds idempotency value and emits standard signature header', async () => {
  const key = await createDeviceKey();
  const input = requestInput({ method: 'POST', body: new Uint8Array([123, 125]), contentType: 'application/json',
    idempotencyKey: '33333333-3333-4333-8333-333333333333' });
  const signed = await signDeviceRequest(input, key.privateKey);
  assert.match(signed.headers.signature, /^sig1=:[A-Za-z0-9+/]{86}==:$/);
  const signature = Uint8Array.from(atob(signed.headers.signature.slice(6, -1)), (c) => c.charCodeAt(0));
  assert.equal(await verifyBytes(key.publicKey, new TextEncoder().encode(signed.signatureBase), signature), true);
  const changed = await prepareDeviceRequest({ ...input, idempotencyKey: '44444444-4444-4444-8444-444444444444' });
  assert.equal(await verifyBytes(key.publicKey, new TextEncoder().encode(changed.signatureBase), signature), false);
});

test('body and input headers are snapshotted before asynchronous signing', async () => {
  const key = await createDeviceKey();
  const body = new TextEncoder().encode('{"hello":"world"}');
  const input = requestInput({ method: 'POST', body, contentType: 'application/json' });
  const pending = signDeviceRequest(input, key.privateKey);
  body.fill(0); input.target = '/attacker'; input.context.deviceId = 'bad';
  const signed = await pending;
  const exposedBody = signed.body; exposedBody.fill(0);
  assert.equal(new TextDecoder().decode(signed.body), '{"hello":"world"}');
  const req = signed.toRequest({ csrfToken: 'csrf-example' });
  assert.equal(await req.text(), '{"hello":"world"}');
  assert.equal(req.url, 'https://invoice.example/api/v1/me');
  assert.equal(req.redirect, 'error'); assert.equal(req.credentials, 'same-origin');
  assert.equal(req.headers.get('content-digest'), digest);
  assert.equal(req.headers.get('x-csrf-token'), 'csrf-example');
  assert.throws(() => signed.toRequest({}), /CSRF/);
});

test('reject foreign, insecure, normalized, credentialed and ambiguous targets', async () => {
  for (const target of ['https://other.example/api/v1/me', '//other.example/api/v1/me', 'https://u:p@invoice.example/api/v1/me',
    '/api/v1/me#fragment', '/api/../me', '/api/%2e%2e/me', '/api/%2fme', '/api/%5cme', '/api\\me', '/api/white space',
    '/api/%zz', '/api/\r\nx-evil:a', 'https://invoice.example:443/api/v1/me']) {
    await assert.rejects(prepareDeviceRequest(requestInput({ target })), undefined, target);
  }
  await assert.rejects(prepareDeviceRequest(requestInput({ publicOrigin: 'http://invoice.example' })));
  await assert.rejects(prepareDeviceRequest(requestInput({ publicOrigin: 'https://invoice.example/path' })));
});

test('GET/HEAD never accept body or body headers; only uppercase allowed methods', async () => {
  for (const method of ['GET', 'HEAD']) {
    await assert.rejects(prepareDeviceRequest(requestInput({ method, body: new Uint8Array(0), contentType: 'application/json' })));
    await assert.rejects(prepareDeviceRequest(requestInput({ method, contentType: 'application/json' })));
  }
  for (const method of ['get', 'TRACE', 'CONNECT', 'POST\r\n']) {
    await assert.rejects(prepareDeviceRequest(requestInput({ method })));
  }
  await assert.rejects(prepareDeviceRequest(requestInput({ method: 'POST', body: new Uint8Array([1]) })));
  await assert.rejects(prepareDeviceRequest(requestInput({ method: 'POST', body: '{}', contentType: 'application/json' })));
  await assert.rejects(prepareDeviceRequest(requestInput({ method: 'POST', contentType: 'application/json\r\nx: bad', body: new Uint8Array(0) })));
});

test('reject stale/future/unbounded nonce time and mismatched challenge context', async () => {
  const base = requestInput();
  for (const patch of [
    { serverNow: 1800000060 }, { serverNow: 1799999999 }, { validitySeconds: 0 }, { validitySeconds: 61 },
    { serverNow: NaN }, { serverNow: 1800000000.5 },
    { challenge: { ...base.challenge, expiresAt: 1800000061 } },
    { challenge: { ...base.challenge, nonce: 'tiny' } },
    { challenge: { ...base.challenge, nonce: 'AAECAwQFBgcICQoLDA0ODx' } },
    { challenge: { ...base.challenge, nonce: 'bad\r\nvalue' } },
    { challenge: { ...base.challenge, proofKeyId: 'other' } },
    { challenge: { ...base.challenge, contextId: 'old-context' } },
    { challenge: { ...base.challenge, bindingEpoch: 2 } },
    { challenge: { ...base.challenge, purpose: 'BIND' } },
    { context: { ...base.context, purpose: 'UNKNOWN' } },
    { context: { ...base.context, deviceId: 'unsafe\nvalue' } },
    { idempotencyKey: 'not-a-uuid' },
  ]) await assert.rejects(prepareDeviceRequest({ ...base, ...patch }), undefined, JSON.stringify(patch));
  const capped = await prepareDeviceRequest(requestInput({ serverNow: 1800000050, validitySeconds: 30 }));
  assert.match(capped.headers['signature-input'], /;expires=1800000060;/);
});

test('RFC structured strings escape quote/backslash and reject non-printable ASCII', () => {
  assert.equal(structuredString('a"b\\c'), '"a\\"b\\\\c"');
  for (const value of ['a\nb', 'a\rb', '\u0000', '中文', '\u007f']) assert.throws(() => structuredString(value));
});
