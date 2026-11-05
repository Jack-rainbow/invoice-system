import test from 'node:test';
import assert from 'node:assert/strict';
import { createDeviceKey, importPublicJwk, jwkThumbprint, signBytes, verifyBytes } from '../../src/shared/device/keys.ts';
import { publicJwk, thumbprint } from './fixtures.mjs';

test('RFC7638 fixed P-256 point has the independent thumbprint', async () => {
  assert.equal(await jwkThumbprint(publicJwk), thumbprint);
  assert.equal(await jwkThumbprint({ y: publicJwk.y, x: publicJwk.x, crv: 'P-256', kty: 'EC' }), thumbprint);
});

test('generated private key cannot be exported; public DTO has only four members', async () => {
  const key = await createDeviceKey();
  assert.equal(key.privateKey.extractable, false);
  assert.deepEqual(key.privateKey.usages, ['sign']);
  await assert.rejects(crypto.subtle.exportKey('jwk', key.privateKey));
  assert.deepEqual(Object.keys(key.publicJwk).sort(), ['crv', 'kty', 'x', 'y']);
  assert.equal(key.thumbprint, await jwkThumbprint(key.publicJwk));
});

test('signBytes is raw 64-byte P-256 r||s; wrong message and public key fail', async () => {
  const key = await createDeviceKey();
  const other = await createDeviceKey();
  const bytes = new TextEncoder().encode('message');
  const signature = await signBytes(key.privateKey, bytes);
  assert.equal(signature.length, 64);
  assert.equal(await verifyBytes(key.publicKey, bytes, signature), true);
  assert.equal(await verifyBytes(key.publicKey, new TextEncoder().encode('changed'), signature), false);
  assert.equal(await verifyBytes(other.publicKey, bytes, signature), false);
  for (const length of [0, 63, 65, 70]) {
    assert.equal(await verifyBytes(key.publicKey, bytes, new Uint8Array(length)), false);
  }
});

test('reject unknown/private members, wrong curve, noncanonical and off-curve coordinates', async () => {
  const invalid = [null, [], {}, { ...publicJwk, d: 'secret' }, { ...publicJwk, alg: 'ES256' },
    Object.defineProperty({ ...publicJwk }, 'd', { value: 'secret', enumerable: false }),
    { ...publicJwk, [Symbol('private')]: 'secret' },
    { ...publicJwk, crv: 'P-384' }, { ...publicJwk, kty: 'RSA' },
    { ...publicJwk, x: publicJwk.x + '=' }, { ...publicJwk, x: publicJwk.x.slice(1) },
    { ...publicJwk, x: publicJwk.x.slice(0, -1) + 'Z' },
    { ...publicJwk, x: 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA', y: 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' }];
  for (const jwk of invalid) {
    await assert.rejects(importPublicJwk(jwk), undefined, JSON.stringify(jwk));
    await assert.rejects(jwkThumbprint(jwk), undefined, JSON.stringify(jwk));
  }
});

test('private signing API rejects exportable or non-P256 keys', async () => {
  const exportable = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, true, ['sign', 'verify']);
  const wrongCurve = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-384' }, false, ['sign', 'verify']);
  await assert.rejects(signBytes(exportable.privateKey, new Uint8Array([1])));
  await assert.rejects(signBytes(wrongCurve.privateKey, new Uint8Array([1])));
});
