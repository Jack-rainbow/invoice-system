import type { GenerationResponse, VersionList, VersionSummary } from '../../shared/api/generated.ts';
import { signedFetch } from '../auth/deviceAuth.ts';
import { readResponse } from '../../shared/api/authTransport.ts';

// Paid actions are click-triggered only. Each confirm click mints one UUID
// idempotency key and signs it as a covered component; retrying the same
// operation reuses its key so the server can only ever bill one version per
// confirmation.

export async function createGeneration(documentId: string, expectedRevision: number, manifestId: string, acceptedCost: '0' | '1', idempotencyKey: string): Promise<GenerationResponse> {
  const response = await signedFetch('/documents/' + encodeURIComponent(documentId) + '/generations',
    { expected_revision: expectedRevision, accepted_preview_manifest_id: manifestId, accepted_cost_points: acceptedCost },
    undefined, undefined, undefined, 'POST', idempotencyKey);
  return readResponse<GenerationResponse>(response);
}

export async function getGeneration(id: string, signal?: AbortSignal): Promise<GenerationResponse> {
  const response = await signedFetch('/generations/' + encodeURIComponent(id), undefined, undefined, undefined, signal, 'GET');
  return readResponse<GenerationResponse>(response);
}

export async function listVersions(documentId: string, signal?: AbortSignal): Promise<VersionSummary[]> {
  const response = await signedFetch('/documents/' + encodeURIComponent(documentId) + '/versions', undefined, undefined, undefined, signal, 'GET');
  const page = await readResponse<VersionList>(response);
  return page.items;
}

// Downloads stream through the same signed channel as JSON; the blob URL is
// released shortly after the browser takes the file.
export async function downloadVersion(versionId: string, format: 'pdf' | 'png'): Promise<void> {
  const response = await signedFetch('/versions/' + encodeURIComponent(versionId) + '/download?format=' + format, undefined, undefined, undefined, undefined, 'GET');
  if (!response.ok) { await readResponse<unknown>(response); return; }
  const file = await response.blob();
  const url = URL.createObjectURL(file);
  const anchor = document.createElement('a');
  anchor.href = url;
  anchor.download = 'documento.' + format;
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  setTimeout(() => URL.revokeObjectURL(url), 30_000);
}
