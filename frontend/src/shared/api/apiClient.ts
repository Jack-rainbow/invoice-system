import { signDeviceRequest } from '../device/rfc9421.ts';
import type { DeviceRequestInput, RequestOptions } from '../device/rfc9421.ts';

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly requestId?: string;
  constructor(status: number, code: string, message: string, requestId?: string) {
    super(message); this.name = 'ApiError'; this.status = status; this.code = code; this.requestId = requestId;
  }
}

type SignedFetchOptions = RequestOptions & Readonly<{ fetcher?: (request: Request) => Promise<Response> }>;

/** Caller obtains a fresh nonce for each attempt and preserves its business idem key.
 * This boundary never retries, supplies a fake user/wallet, or follows redirects.
 * Auth/query cache lifecycle remains the responsibility of the future real UI.
 */
export async function signedFetch(input: DeviceRequestInput, privateKey: CryptoKey, options: SignedFetchOptions = {}): Promise<Response> {
  options.signal?.throwIfAborted();
  const signed = await signDeviceRequest(input, privateKey);
  options.signal?.throwIfAborted();
  const response = await (options.fetcher ?? globalThis.fetch)(signed.toRequest(options));
  if (response.ok) return response;
  let payload: unknown;
  try { payload = await response.json(); } catch { /* Raw upstream response is never an error message. */ }
  const error = payload && typeof payload === 'object' ? payload as Record<string, unknown> : {};
  const code = typeof error.code === 'string' && /^[A-Z][A-Z0-9_]{0,63}$/.test(error.code) ? error.code : 'HTTP_ERROR';
  const message = typeof error.message === 'string' && error.message.length <= 512 ? error.message : '请求失败，请稍后重试';
  const requestId = typeof error.request_id === 'string' && /^[A-Za-z0-9_-]{1,128}$/.test(error.request_id) ? error.request_id : undefined;
  throw new ApiError(response.status, code, message, requestId);
}
