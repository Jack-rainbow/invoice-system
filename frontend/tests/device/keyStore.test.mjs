import test from 'node:test';
import assert from 'node:assert/strict';
import { openDeviceKeyStore } from '../../src/shared/device/keyStore.ts';

test('no IndexedDB stops key initialization; no in-memory/plaintext fallback', async () => {
  assert.equal(typeof indexedDB, 'undefined');
  await assert.rejects(openDeviceKeyStore(), { code: 'DEVICE_KEY_STORAGE_UNAVAILABLE' });
});
