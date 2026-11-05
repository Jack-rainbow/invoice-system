import test from 'node:test';
import assert from 'node:assert/strict';
import { signedFetch, ApiError } from '../../src/shared/api/apiClient.ts';
import { createDeviceKey } from '../../src/shared/device/keys.ts';
import { requestInput } from '../device/fixtures.mjs';

test('signedFetch sends one signed same-origin Request with original bytes', async () => {
  const key = await createDeviceKey(); let count = 0;
  const result = await signedFetch(requestInput(), key.privateKey, {
    fetcher: async (request) => {
      count++;
      assert.equal(request.headers.get('x-session-generation'), '22222222-2222-4222-8222-222222222222');
      assert.match(request.headers.get('signature'), /^sig1=:/);
      assert.equal(request.redirect, 'error');
      assert.equal(request.mode, 'same-origin');
      return new Response('{"ok":true}', { headers: { 'content-type': 'application/json' } });
    },
  });
  assert.deepEqual(await result.json(), { ok: true }); assert.equal(count, 1);
});

test('stable error code is preserved and no automatic nonce/idempotency retry occurs', async () => {
  const key = await createDeviceKey(); let count = 0;
  await assert.rejects(signedFetch(requestInput(), key.privateKey, {
    fetcher: async () => { count++; return new Response(JSON.stringify({ code: 'SESSION_REPLACED', message: '重新登录', request_id: 'req_1' }), { status: 401 }); },
  }), (error) => error instanceof ApiError && error.code === 'SESSION_REPLACED' && error.status === 401 && error.requestId === 'req_1');
  assert.equal(count, 1);
});

test('non-JSON error cannot leak raw HTML or internal service text', async () => {
  const key = await createDeviceKey();
  await assert.rejects(signedFetch(requestInput(), key.privateKey, {
    fetcher: async () => new Response('<html>secret upstream trace</html>', { status: 502 }),
  }), (error) => error instanceof ApiError && error.code === 'HTTP_ERROR' && !error.message.includes('secret'));
});

test('CSRF requirement and abort prevent fetch; no cache or fallback state', async () => {
  const key = await createDeviceKey(); let count = 0;
  const fetcher = async () => { count++; return new Response(); };
  await assert.rejects(signedFetch(requestInput({ method: 'POST', body: new Uint8Array([123, 125]), contentType: 'application/json' }), key.privateKey, { fetcher }), /CSRF/);
  const controller = new AbortController(); controller.abort();
  await assert.rejects(signedFetch(requestInput(), key.privateKey, { fetcher, signal: controller.signal }), { name: 'AbortError' });
  assert.equal(count, 0);
});
