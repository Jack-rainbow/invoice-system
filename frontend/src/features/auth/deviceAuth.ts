import type { ChallengeRequest, ChallengeResponse, DeviceContextResponse, InitialPasswordSetResponse, MeResponse, SessionResponse } from '../../shared/api/generated.ts';
import { csrfToken, rawRequest, readResponse, request, RequestError } from '../../shared/api/authTransport.ts';
import { openDeviceKeyStore } from '../../shared/device/keyStore.ts';
import type { CandidateDeviceKey } from '../../shared/device/keyStore.ts';
import { signDeviceRequest } from '../../shared/device/rfc9421.ts';

type Challenge = ChallengeResponse & { receivedAt: number };
async function challenge(input: ChallengeRequest, signal?: AbortSignal): Promise<Challenge> {
  const value = await request<ChallengeResponse>('/auth/device/challenges', input, undefined, signal);
  return { ...value, receivedAt: performance.now() };
}
async function keyFor(thumbprint: string): Promise<CandidateDeviceKey> {
  const store = await openDeviceKeyStore();
  try { return await store.requireExistingKeyByThumbprint(thumbprint); } finally { store.close(); }
}
// An administrator session carries no device binding by design; its business
// requests travel on the complete-MFA cookie with CSRF instead of a proof. The
// context endpoint tells them apart from a USER browser that merely lost its
// key — the latter still has a server-side binding and must not fall back.
let adminSessionCache: Promise<boolean> | null = null;
function adminSession(signal?: AbortSignal): Promise<boolean> {
  adminSessionCache ??= request<DeviceContextResponse>('/auth/device/context', undefined, undefined, signal)
    .then((context) => !context.binding_id && !context.key_thumbprint)
    .catch(() => false);
  return adminSessionCache;
}

// Builds one RFC9421 proof and returns the raw response so callers can read
// either JSON or an authenticated binary stream from the same signed channel.
// Paid idempotent writes must pass their Idempotency-Key so it becomes a signed
// component; the server rejects an unsigned or swapped key.
export async function signedFetch(path: string, body?: object, provided?: Challenge, candidate?: CandidateDeviceKey, signal?: AbortSignal, method?: 'GET' | 'POST' | 'PATCH' | 'DELETE', idempotencyKey?: string): Promise<Response> {
  const verb = method ?? (body === undefined ? 'GET' : 'POST');
  const csrf = body === undefined ? undefined : await csrfToken(signal);
  try {
    const proof = provided ?? await challenge({ purpose: 'REQUEST' }, signal);
    const selected = candidate ?? await keyFor(proof.key_thumbprint);
    if (selected.key.thumbprint !== proof.key_thumbprint) throw new RequestError('DEVICE_KEY_UNAVAILABLE');
    const serverNow = Math.floor(Date.parse(proof.server_time) / 1000 + (performance.now() - proof.receivedAt) / 1000);
    const prepared = await signDeviceRequest({ publicOrigin: location.origin, target: '/api/v1' + path, method: verb,
      context: { deviceId: proof.proof_key_id, sessionGeneration: proof.context_id, bindingEpoch: proof.binding_epoch, purpose: proof.purpose },
      challenge: { proofKeyId: proof.proof_key_id, contextId: proof.context_id, bindingEpoch: proof.binding_epoch, purpose: proof.purpose,
        nonce: proof.nonce, serverTime: Math.floor(Date.parse(proof.server_time) / 1000), expiresAt: Math.floor(Date.parse(proof.expires_at) / 1000) },
      serverNow, ...(body === undefined ? {} : { body: new TextEncoder().encode(JSON.stringify(body)), contentType: 'application/json' }),
      ...(idempotencyKey === undefined ? {} : { idempotencyKey }),
    }, selected.key.privateKey);
    signal?.throwIfAborted();
    return fetch(prepared.toRequest({ csrfToken: csrf, signal }));
  } catch (failure) {
    if (signal?.aborted || !(await adminSession(signal))) throw failure;
    return rawRequest(path, body, idempotencyKey, signal, verb);
  }
}
export async function signedRequest<T>(path: string, body?: object, provided?: Challenge, candidate?: CandidateDeviceKey, signal?: AbortSignal, method?: 'GET' | 'POST' | 'PATCH' | 'DELETE', idempotencyKey?: string): Promise<T> {
  return readResponse<T>(await signedFetch(path, body, provided, candidate, signal, method, idempotencyKey));
}
// The server-watermarked preview is the only image a browser may hold. Keep the
// type and byte ceiling checked here so a misrouted clean file cannot render.
export async function signedImage(path: string, signal?: AbortSignal): Promise<Blob> {
  const response = await signedFetch(path, undefined, undefined, undefined, signal, 'GET');
  if (!response.ok) { await readResponse<unknown>(response); throw new RequestError('UNAVAILABLE'); }
  const image = await response.blob();
  if (image.type !== 'image/jpeg' || image.size === 0 || image.size > 2 * 1024 * 1024) throw new RequestError('PREVIEW_INVALID');
  return image;
}
async function rememberConfirmed(candidate: CandidateDeviceKey): Promise<void> {
  const context = await request<DeviceContextResponse>('/auth/device/context');
  if (!context.binding_id || context.key_thumbprint !== candidate.key.thumbprint) throw new RequestError('DEVICE_KEY_UNAVAILABLE');
  // Valid signed business authentication proves that this persisted candidate
  // is exactly the current binding. Metadata alone never confirms a binding.
  await signedRequest<MeResponse>('/me', undefined, undefined, candidate);
  const store = await openDeviceKeyStore();
  try { await store.rememberBinding(context.binding_id, candidate.id, candidate.key.thumbprint); } finally { store.close(); }
}
export async function initialDeviceSetup(token: string, password: string): Promise<void> {
  const store = await openDeviceKeyStore(); let candidate: CandidateDeviceKey;
  try { candidate = await store.createCandidateDeviceKey(); } finally { store.close(); }
  const first = await challenge({ purpose: 'INITIAL_SETUP', setup_token: token, public_jwk: candidate.key.publicJwk });
  const result = await signedRequest<InitialPasswordSetResponse>('/auth/password/set', { token, new_password: password, public_jwk: candidate.key.publicJwk, challenge_id: first.challenge_id }, first, candidate);
  const bind = await challenge({ purpose: 'BIND', transfer_id: result.transfer_id });
  try { await signedRequest<SessionResponse>('/auth/device/bind', { transfer_id: result.transfer_id, challenge_id: bind.challenge_id }, bind, candidate); }
  catch (failure) {
    // An uncertain response may already have installed the formal Cookie. Only
    // server metadata plus a fresh successful signature can reconcile it.
    try { await rememberConfirmed(candidate); return; } catch { throw failure; }
  }
  await rememberConfirmed(candidate);
}
export async function loginDevice(): Promise<void> {
  const proof = await challenge({ purpose: 'LOGIN' }); const candidate = await keyFor(proof.key_thumbprint);
  await signedRequest<SessionResponse>('/auth/device/verify', { challenge_id: proof.challenge_id }, proof, candidate);
  await rememberConfirmed(candidate);
}
export async function authenticatedMe(signal?: AbortSignal): Promise<MeResponse> {
  const context = await request<DeviceContextResponse>('/auth/device/context', undefined, undefined, signal);
  if (!context.binding_id) return request<MeResponse>('/me', undefined, undefined, signal);
  if (!context.key_thumbprint) throw new RequestError('DEVICE_KEY_UNAVAILABLE');
  const candidate = await keyFor(context.key_thumbprint);
  const result = await signedRequest<MeResponse>('/me', undefined, undefined, candidate, signal);
  if (result.generation === context.generation) {
    const store = await openDeviceKeyStore();
    try { await store.rememberBinding(context.binding_id, candidate.id, candidate.key.thumbprint); } finally { store.close(); }
  }
  return result;
}
export async function logoutDevice(): Promise<void> { adminSessionCache = null; await signedRequest<void>('/auth/logout', {}); }
