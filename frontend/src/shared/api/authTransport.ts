export class RequestError extends Error {
  readonly code: string;
  readonly details?: unknown;
  constructor(code: string, details?: unknown) { super(code); this.code = code; this.details = details; }
}
export async function readResponse<T>(response: Response): Promise<T> {
  if (!response.ok) {
    let code = 'UNAVAILABLE';
    let details: unknown;
    try {
      const data: unknown = await response.json();
      if (data && typeof data === 'object' && 'code' in data && typeof data.code === 'string') code = data.code;
      if (data && typeof data === 'object' && 'details' in data) details = (data as { details: unknown }).details;
    } catch { /* No raw upstream page. */ }
    throw new RequestError(code, details);
  }
  return (response.status === 204 ? undefined : await response.json()) as T;
}
export async function csrfToken(signal?: AbortSignal): Promise<string> {
  const response = await fetch('/api/v1/auth/csrf', { credentials: 'same-origin', mode: 'same-origin', cache: 'no-store', redirect: 'error', signal });
  const data = await readResponse<{ csrf_token: string }>(response);
  if (typeof data.csrf_token !== 'string') throw new RequestError('UNAVAILABLE');
  return data.csrf_token;
}
export async function request<T>(path: string, body?: object, key?: string, signal?: AbortSignal): Promise<T> {
  const headers: Record<string, string> = {};
  if (body !== undefined) { headers['X-CSRF-Token'] = await csrfToken(signal); headers['Content-Type'] = 'application/json'; }
  if (key) headers['Idempotency-Key'] = key;
  const response = await fetch('/api/v1' + path, { method: body === undefined ? 'GET' : 'POST', headers,
    body: body === undefined ? undefined : JSON.stringify(body), credentials: 'same-origin', mode: 'same-origin', cache: 'no-store', redirect: 'error', signal });
  return readResponse<T>(response);
}

// rawRequest is the cookie+CSRF channel used by complete-MFA administrator
// sessions, which hold no device key by design. It returns the raw Response so
// binary streams (downloads, images) share one transport surface.
export async function rawRequest(path: string, body: object | undefined, key: string | undefined, signal: AbortSignal | undefined, method: 'GET' | 'POST' | 'PATCH' | 'DELETE' = 'POST'): Promise<Response> {
  const headers: Record<string, string> = {};
  if (body !== undefined) { headers['X-CSRF-Token'] = await csrfToken(signal); headers['Content-Type'] = 'application/json'; }
  if (key) headers['Idempotency-Key'] = key;
  return fetch('/api/v1' + path, { method, headers,
    body: body === undefined ? undefined : JSON.stringify(body), credentials: 'same-origin', mode: 'same-origin', cache: 'no-store', redirect: 'error', signal });
}
