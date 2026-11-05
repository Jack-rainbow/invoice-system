// Independent expected values: P-256 generator point; SHA-256 values checked
// with Python hashlib, not with the production helpers under test.
export const publicJwk = {
  kty: 'EC', crv: 'P-256',
  x: 'axfR8uEsQkf4vOblY6RA8ncDfYEt6zOg9KE5RdiYwpY',
  y: 'T-NC4v4af5uO5-tKfA-eFivOM1drMV7Oy7ZAaDe_UfU',
};
export const thumbprint = 'xx0BcA-wMohw8atYDJOe6peGModklG2wRHBlXHMvl0M';
export const digest = 'sha-256=:k6I5cakU5erL8KjSUVTNownDwccvu5kU1Hxg88toFYg=:';
export const context = Object.freeze({
  deviceId: '11111111-1111-4111-8111-111111111111',
  sessionGeneration: '22222222-2222-4222-8222-222222222222',
  bindingEpoch: 1, purpose: 'REQUEST',
});
export const challenge = Object.freeze({
  proofKeyId: context.deviceId, contextId: context.sessionGeneration,
  bindingEpoch: 1, purpose: 'REQUEST', nonce: 'AAECAwQFBgcICQoLDA0ODw',
  serverTime: 1800000000, expiresAt: 1800000060,
});
export function requestInput(extra = {}) {
  return {
    publicOrigin: 'https://invoice.example', target: '/api/v1/me', method: 'GET',
    context: { ...context }, challenge: { ...challenge }, serverNow: 1800000000,
    validitySeconds: 30, ...extra,
  };
}
export const getSignatureInput = 'sig1=("@method" "@target-uri" "x-device-id" "x-session-generation");created=1800000000;expires=1800000030;nonce="AAECAwQFBgcICQoLDA0ODw";keyid="11111111-1111-4111-8111-111111111111";alg="ecdsa-p256-sha256";tag="invoice-device-v1"';
export const getSignatureBase = '"@method": GET\n"@target-uri": https://invoice.example/api/v1/versions/abc/download?format=pdf&x=%2B&x=2\n"x-device-id": 11111111-1111-4111-8111-111111111111\n"x-session-generation": 22222222-2222-4222-8222-222222222222\n"@signature-params": ("@method" "@target-uri" "x-device-id" "x-session-generation");created=1800000000;expires=1800000030;nonce="AAECAwQFBgcICQoLDA0ODw";keyid="11111111-1111-4111-8111-111111111111";alg="ecdsa-p256-sha256";tag="invoice-device-v1"';
export const postSignatureBase = '"@method": POST\n"@target-uri": https://invoice.example/api/v1/documents\n"x-device-id": 11111111-1111-4111-8111-111111111111\n"x-session-generation": 22222222-2222-4222-8222-222222222222\n"content-type": application/json\n"content-digest": sha-256=:k6I5cakU5erL8KjSUVTNownDwccvu5kU1Hxg88toFYg=:\n"idempotency-key": 33333333-3333-4333-8333-333333333333\n"@signature-params": ("@method" "@target-uri" "x-device-id" "x-session-generation" "content-type" "content-digest" "idempotency-key");created=1800000000;expires=1800000030;nonce="AAECAwQFBgcICQoLDA0ODw";keyid="11111111-1111-4111-8111-111111111111";alg="ecdsa-p256-sha256";tag="invoice-device-v1"';
