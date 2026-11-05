import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { importPublicJwk, jwkThumbprint, verifyBytes } from '../../src/shared/device/keys.ts';

test('stored WebCrypto vectors have valid public-only signatures and fail changed bases', async () => {
  const fixture = JSON.parse(await readFile(new URL('./vectors/webcrypto-p256.json', import.meta.url), 'utf8'));
  const publicKey = await importPublicJwk(fixture.public_jwk);
  assert.equal(await jwkThumbprint(fixture.public_jwk), fixture.thumbprint);
  assert.equal(fixture.vectors.length, 4);
  for (const vector of fixture.vectors) {
    const bytes = new TextEncoder().encode(vector.signature_base);
    assert.equal(Buffer.from(bytes).toString('base64'), vector.signature_base_utf8_base64);
    const signature = new Uint8Array(Buffer.from(vector.signature_base64, 'base64'));
    assert.equal(signature.length, 64);
    assert.equal(await verifyBytes(publicKey, bytes, signature), true);
    assert.equal(await verifyBytes(publicKey, new TextEncoder().encode(vector.signature_base + 'changed'), signature), false);
    assert.equal(vector.headers.signature, 'sig1=:' + vector.signature_base64 + ':');
  }
});
