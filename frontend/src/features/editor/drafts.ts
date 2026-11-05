import type { DocumentList, DraftFields, DraftResponse, PreviewResponse, RenderProfileList } from '../../shared/api/generated.ts';
import { signedImage, signedRequest } from '../auth/deviceAuth.ts';

// Every call is a fresh RFC9421 proof obtained by the shared signer. Nothing is
// cached here: a nonce is single-use and a response can be stale after another
// window saves the same draft.

export function listDocuments(cursor?: string): Promise<DocumentList> {
  const query = '/documents?limit=5' + (cursor ? '&cursor=' + encodeURIComponent(cursor) : '');
  return signedRequest<DocumentList>(query);
}

export function createDraft(fields: DraftFields): Promise<DraftResponse> {
  return signedRequest<DraftResponse>('/documents', { fields });
}

export function getDraft(id: string): Promise<DraftResponse> {
  return signedRequest<DraftResponse>('/documents/' + encodeURIComponent(id));
}

export function saveDraft(id: string, expectedRevision: number, fields: DraftFields): Promise<DraftResponse> {
  return signedRequest<DraftResponse>('/documents/' + encodeURIComponent(id), { expected_revision: expectedRevision, fields }, undefined, undefined, undefined, 'PATCH');
}

export function deleteDraft(id: string, expectedRevision: number): Promise<void> {
  return signedRequest<void>('/documents/' + encodeURIComponent(id), { expected_revision: expectedRevision }, undefined, undefined, undefined, 'DELETE');
}

// Free preview: the request binds one saved revision. Repeating it with the same
// content merges server-side instead of consuming another render slot.
export function createPreview(id: string, expectedRevision: number): Promise<PreviewResponse> {
  return signedRequest<PreviewResponse>('/documents/' + encodeURIComponent(id) + '/previews', { expected_revision: expectedRevision });
}

export function getPreview(id: string): Promise<PreviewResponse> {
  return signedRequest<PreviewResponse>('/previews/' + encodeURIComponent(id));
}

export function getPreviewImage(id: string, signal?: AbortSignal): Promise<Blob> {
  return signedImage('/previews/' + encodeURIComponent(id) + '/image', signal);
}

// Profiles expose metadata only. Adoption is what lets an existing draft leave a
// revoked template without losing its revision history.
export function listRenderProfiles(): Promise<RenderProfileList> {
  return signedRequest<RenderProfileList>('/render-profiles');
}

export function changeDraftProfile(id: string, expectedRevision: number, targetProfileId: string): Promise<DraftResponse> {
  return signedRequest<DraftResponse>('/documents/' + encodeURIComponent(id) + '/render-profile',
    { expected_revision: expectedRevision, target_profile_id: targetProfileId });
}
