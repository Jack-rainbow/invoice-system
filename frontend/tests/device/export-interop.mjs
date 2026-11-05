import { writeFile, mkdir } from 'node:fs/promises';
import { createDeviceKey, base64 } from '../../src/shared/device/keys.ts';
import { signDeviceRequest } from '../../src/shared/device/rfc9421.ts';
import { requestInput } from './fixtures.mjs';

// Public-only, real WebCrypto outputs for the Go standard-library primitive
// verifier and the future maintained RFC9421 library. No private key is emitted.
const key = await createDeviceKey();
const body = new TextEncoder().encode('{"hello":"world"}');
const requests = [
  requestInput({ target: '/api/v1/versions/abc/download?format=pdf&x=%2B&x=2' }),
  requestInput({ method: 'POST', target: '/api/v1/documents', body, contentType: 'application/json', idempotencyKey: '33333333-3333-4333-8333-333333333333' }),
  requestInput({ method: 'PATCH', target: '/api/v1/documents/abc', body, contentType: 'application/json' }),
  requestInput({ method: 'DELETE', target: '/api/v1/documents/abc', body, contentType: 'application/json' }),
];
const vectors = [];
for (const input of requests) {
  const signed = await signDeviceRequest(input, key.privateKey);
  vectors.push({ method: signed.method, target_uri: signed.targetUri, signature_base: signed.signatureBase,
    signature_base_utf8_base64: base64(new TextEncoder().encode(signed.signatureBase)),
    signature_base64: signed.headers.signature.slice(6, -1),
    body_base64: signed.body === undefined ? null : base64(signed.body), headers: signed.headers });
}
const output = JSON.stringify({ profile: 'invoice-device-v1', algorithm: 'ecdsa-p256-sha256',
  public_jwk: key.publicJwk, thumbprint: key.thumbprint, vectors }, null, 2) + '\n';
if (process.argv.includes('--write')) {
  const directory = new URL('./vectors/', import.meta.url);
  await mkdir(directory, { recursive: true });
  await writeFile(new URL('webcrypto-p256.json', directory), output);
  console.log('Wrote 4 public-only WebCrypto P-256 interoperability vectors');
} else process.stdout.write(output);
