import { base64, decodeBase64url, signBytes } from './keys.ts';

export type ProofPurpose = 'REQUEST' | 'INITIAL_SETUP' | 'LOGIN' | 'PAIR' | 'BIND';
export type DeviceContext = Readonly<{
  deviceId: string; sessionGeneration: string; bindingEpoch: number; purpose: ProofPurpose;
}>;
export type DeviceChallenge = Readonly<{
  nonce: string; proofKeyId: string; contextId: string; bindingEpoch: number;
  purpose: ProofPurpose; serverTime: number; expiresAt: number;
}>;
export type DeviceRequestInput = Readonly<{
  publicOrigin: string; target: string; method: string;
  context: DeviceContext; challenge: DeviceChallenge;
  /** Calibrated from server_time, not unadjusted Date.now(). Seconds since epoch. */
  serverNow: number; validitySeconds?: number;
  body?: Uint8Array; contentType?: string; idempotencyKey?: string;
}>;
export type PreparedDeviceRequest = Readonly<{
  method: string; targetUri: string; signatureBase: string;
  headers: Readonly<Record<string, string>>;
  /** Returns a copy: mutating it cannot change the signed content. */
  body: Uint8Array<ArrayBuffer> | undefined;
}>;
export type RequestOptions = Readonly<{ csrfToken?: string; signal?: AbortSignal }>;
export type SignedDeviceRequest = PreparedDeviceRequest & Readonly<{ toRequest(options?: RequestOptions): Request }>;

const methods = new Set(['GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS']);
const purposes = new Set(['REQUEST', 'INITIAL_SETUP', 'LOGIN', 'PAIR', 'BIND']);
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

export function structuredString(value: string): string {
  if (typeof value !== 'string' || /[^\x20-\x7e]/.test(value)) throw new TypeError('Structured strings require printable ASCII');
  return '"' + value.replace(/\\/g, '\\\\').replace(/"/g, '\\"') + '"';
}

function identifier(value: string): void {
  if (typeof value !== 'string' || !/^[A-Za-z0-9_-]{1,128}$/.test(value)) throw new TypeError('Invalid context identifier');
}

function second(value: number): void {
  if (!Number.isSafeInteger(value) || value < 0 || value > 999999999999999) throw new TypeError('Invalid epoch seconds');
}

function targetUri(publicOrigin: string, target: string): string {
  const origin = new URL(publicOrigin);
  if (origin.origin !== publicOrigin || origin.username || origin.password ||
      (origin.protocol !== 'https:' && !(origin.protocol === 'http:' && ['localhost', '127.0.0.1', '[::1]'].includes(origin.hostname)))) {
    throw new TypeError('PUBLIC_ORIGIN must be a canonical HTTPS origin (loopback HTTP only for development)');
  }
  if (typeof location !== 'undefined' && location.origin !== publicOrigin) throw new TypeError('PUBLIC_ORIGIN is not this browser origin');
  if (typeof target !== 'string' || /[^\x21-\x7e]|[\\#]/.test(target) || /%(?![A-Fa-f0-9]{2})/.test(target)) {
    throw new TypeError('Unsafe target URI');
  }
  const absolute = target.startsWith('/') && !target.startsWith('//') ? publicOrigin + target : target;
  const parsed = new URL(absolute);
  // URL parser normalization must never silently change the bytes being signed.
  if (parsed.origin !== publicOrigin || parsed.username || parsed.password || parsed.hash || parsed.href !== absolute ||
      /%(?:2e|2f|5c)/i.test(parsed.pathname) || parsed.pathname.includes('//')) throw new TypeError('Ambiguous or foreign target URI');
  return absolute;
}

/** Fixed invoice-device-v1 serializer, not a general RFC9421 parser or verifier. */
export async function prepareDeviceRequest(input: DeviceRequestInput): Promise<PreparedDeviceRequest> {
  // Snapshot synchronously before the first await; body is never JSON-reserialized.
  const { publicOrigin, target, method, contentType, idempotencyKey, serverNow } = input;
  const context = { ...input.context }; const challenge = { ...input.challenge };
  const validitySeconds = input.validitySeconds ?? 30;
  if (input.body !== undefined && !(input.body instanceof Uint8Array)) throw new TypeError('Body must be raw Uint8Array');
  const body = input.body === undefined ? undefined : new Uint8Array(input.body);
  if (!methods.has(method)) throw new TypeError('Unsupported method; use canonical uppercase');
  const uri = targetUri(publicOrigin, target);
  identifier(context.deviceId); identifier(context.sessionGeneration);
  if (!purposes.has(context.purpose) || !Number.isSafeInteger(context.bindingEpoch) || context.bindingEpoch < 0 ||
      challenge.proofKeyId !== context.deviceId || challenge.contextId !== context.sessionGeneration ||
      challenge.bindingEpoch !== context.bindingEpoch || challenge.purpose !== context.purpose) throw new TypeError('Challenge context mismatch');
  const nonce = decodeBase64url(challenge.nonce);
  if (nonce.length < 16 || nonce.length > 64) throw new TypeError('Nonce must contain 128 to 512 bits');
  second(challenge.serverTime); second(challenge.expiresAt); second(serverNow);
  if (!Number.isInteger(validitySeconds) || validitySeconds < 1 || validitySeconds > 60 ||
      challenge.expiresAt <= challenge.serverTime || challenge.expiresAt - challenge.serverTime > 60 ||
      serverNow < challenge.serverTime || serverNow >= challenge.expiresAt) throw new TypeError('Invalid or expired challenge time');
  if ((method === 'GET' || method === 'HEAD') && (body !== undefined || contentType !== undefined)) throw new TypeError('GET/HEAD cannot have body headers or body');
  if ((body === undefined) !== (contentType === undefined)) throw new TypeError('Body and content-type must be supplied together');
  // Fixed JSON profile avoids browser/HTTP field normalization disagreements.
  if (contentType !== undefined && contentType !== 'application/json') throw new TypeError('Only canonical application/json is supported');
  if (idempotencyKey !== undefined && !uuid.test(idempotencyKey)) throw new TypeError('Idempotency-Key must be a UUID');
  const headers: Record<string, string> = {
    'x-device-id': context.deviceId, 'x-session-generation': context.sessionGeneration,
  };
  const components = ['@method', '@target-uri', 'x-device-id', 'x-session-generation'];
  const lines = [`"@method": ${method}`, `"@target-uri": ${uri}`,
    `"x-device-id": ${context.deviceId}`, `"x-session-generation": ${context.sessionGeneration}`];
  if (body !== undefined && contentType !== undefined) {
    headers['content-type'] = contentType;
    headers['content-digest'] = 'sha-256=:' + base64(new Uint8Array(await crypto.subtle.digest('SHA-256', body))) + ':';
    for (const name of ['content-type', 'content-digest']) {
      components.push(name); lines.push(`${structuredString(name)}: ${headers[name]}`);
    }
  }
  if (idempotencyKey !== undefined) {
    headers['idempotency-key'] = idempotencyKey;
    components.push('idempotency-key'); lines.push(`"idempotency-key": ${idempotencyKey}`);
  }
  const params = `(${components.map(structuredString).join(' ')});created=${serverNow};expires=${Math.min(serverNow + validitySeconds, challenge.expiresAt)}` +
    `;nonce=${structuredString(challenge.nonce)};keyid=${structuredString(context.deviceId)};alg="ecdsa-p256-sha256";tag="invoice-device-v1"`;
  headers['signature-input'] = 'sig1=' + params;
  lines.push('"@signature-params": ' + params);
  return Object.freeze({ method, targetUri: uri, signatureBase: lines.join('\n'), headers: Object.freeze(headers),
    get body() { return body === undefined ? undefined : new Uint8Array(body); } });
}

export async function signDeviceRequest(input: DeviceRequestInput, privateKey: CryptoKey): Promise<SignedDeviceRequest> {
  const prepared = await prepareDeviceRequest(input);
  const signature = await signBytes(privateKey, new TextEncoder().encode(prepared.signatureBase));
  const body = prepared.body;
  const headers = Object.freeze({ ...prepared.headers, signature: 'sig1=:' + base64(signature) + ':' });
  return Object.freeze({ ...prepared, headers,
    get body() { return body === undefined ? undefined : new Uint8Array(body); },
    toRequest(options: RequestOptions = {}) {
      const requestHeaders: Record<string, string> = { ...headers };
      if (!['GET', 'HEAD', 'OPTIONS'].includes(prepared.method) && !options.csrfToken) throw new TypeError('Write requires CSRF token');
      if (options.csrfToken !== undefined) {
        if (!/^[\x21-\x7e]{1,512}$/.test(options.csrfToken)) throw new TypeError('Invalid CSRF token');
        requestHeaders['x-csrf-token'] = options.csrfToken;
      }
      return new Request(prepared.targetUri, { method: prepared.method, headers: requestHeaders,
        body: body === undefined ? undefined : new Uint8Array(body), credentials: 'same-origin', mode: 'same-origin',
        redirect: 'error', cache: 'no-store', signal: options.signal });
    },
  });
}
