export type PublicDeviceJwk = Readonly<{ kty: 'EC'; crv: 'P-256'; x: string; y: string }>;

export type DeviceKey = Readonly<{
  privateKey: CryptoKey;
  publicKey: CryptoKey;
  publicJwk: PublicDeviceJwk;
  thumbprint: string;
}>;

export function base64(bytes: Uint8Array): string {
  return btoa(Array.from(bytes, (byte) => String.fromCharCode(byte)).join(''));
}

export function base64url(bytes: Uint8Array): string {
  return base64(bytes).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

/** Reject aliases (including nonzero trailing pad bits), not just bad characters. */
export function decodeBase64url(value: unknown): Uint8Array<ArrayBuffer> {
  if (typeof value !== 'string' || !/^[A-Za-z0-9_-]+$/.test(value) || value.length % 4 === 1) {
    throw new TypeError('Invalid canonical base64url');
  }
  const bytes = Uint8Array.from(atob(value.replace(/-/g, '+').replace(/_/g, '/')), (c) => c.charCodeAt(0));
  if (base64url(bytes) !== value) throw new TypeError('Noncanonical base64url');
  return bytes;
}

function publicJwkDto(input: unknown): PublicDeviceJwk {
  if (input === null || typeof input !== 'object' || Array.isArray(input)) throw new TypeError('Invalid public JWK');
  const obj = input as Record<string, unknown>;
  const members = Reflect.ownKeys(obj);
  const { kty, crv, x, y } = obj;
  if (members.length !== 4 || members.some((member) => typeof member !== 'string') ||
      (members as string[]).sort().join(',') !== 'crv,kty,x,y' || kty !== 'EC' || crv !== 'P-256') {
    throw new TypeError('Only EC/P-256 public kty/crv/x/y members are accepted');
  }
  if (decodeBase64url(x).length !== 32 || decodeBase64url(y).length !== 32) {
    throw new TypeError('P-256 coordinates must each be 32 bytes');
  }
  return Object.freeze({ kty: 'EC', crv: 'P-256', x: x as string, y: y as string });
}

export async function importPublicJwk(input: unknown): Promise<CryptoKey> {
  // Import also checks whether the coordinates describe an actual curve point.
  return crypto.subtle.importKey('jwk', publicJwkDto(input), { name: 'ECDSA', namedCurve: 'P-256' }, true, ['verify']);
}

export async function jwkThumbprint(input: unknown): Promise<string> {
  const jwk = publicJwkDto(input);
  await importPublicJwk(jwk);
  const canonical = JSON.stringify({ crv: jwk.crv, kty: jwk.kty, x: jwk.x, y: jwk.y });
  return base64url(new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(canonical))));
}

function assertP256(key: CryptoKey, type: 'private' | 'public'): void {
  if (!(key instanceof CryptoKey) || key.type !== type || key.algorithm.name !== 'ECDSA' ||
      (key.algorithm as EcKeyAlgorithm).namedCurve !== 'P-256') throw new TypeError('Expected P-256 CryptoKey');
  if (type === 'private' && (key.extractable || !key.usages.includes('sign'))) {
    throw new TypeError('Device private key must be non-extractable and sign-only');
  }
}

export async function createDeviceKey(): Promise<DeviceKey> {
  const pair = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign', 'verify']);
  const exported = await crypto.subtle.exportKey('jwk', pair.publicKey);
  // Never export the private key. Strip public WebCrypto metadata at this boundary.
  const publicJwk = publicJwkDto({ kty: exported.kty, crv: exported.crv, x: exported.x, y: exported.y });
  return Object.freeze({ ...pair, publicJwk, thumbprint: await jwkThumbprint(publicJwk) });
}

export async function signBytes(privateKey: CryptoKey, bytes: Uint8Array): Promise<Uint8Array<ArrayBuffer>> {
  assertP256(privateKey, 'private');
  if (!(bytes instanceof Uint8Array)) throw new TypeError('Signing input must be raw bytes');
  const signature = new Uint8Array(await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, privateKey, new Uint8Array(bytes)));
  if (signature.length !== 64) throw new Error('WebCrypto did not return P-256 r||s');
  return signature;
}

export async function verifyBytes(publicKey: CryptoKey, bytes: Uint8Array, signature: Uint8Array): Promise<boolean> {
  assertP256(publicKey, 'public');
  if (!(bytes instanceof Uint8Array) || !(signature instanceof Uint8Array) || signature.length !== 64) return false;
  return crypto.subtle.verify({ name: 'ECDSA', hash: 'SHA-256' }, publicKey, new Uint8Array(signature), new Uint8Array(bytes));
}
