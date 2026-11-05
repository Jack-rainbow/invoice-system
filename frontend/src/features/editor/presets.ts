import type { CreditEntryList, PresetList, PresetResponse } from '../../shared/api/generated.ts';
import type { DraftFields } from '../../shared/api/generated.ts';
import { signedRequest } from '../auth/deviceAuth.ts';

// Presets are the owner's reusable field sets; amounts are never stored — the
// server recomputes them when the preset fields are applied to a draft.

export function listPresets(cursor?: string): Promise<PresetList> {
  const query = '/presets?limit=100' + (cursor ? '&cursor=' + encodeURIComponent(cursor) : '');
  return signedRequest<PresetList>(query);
}

export function createPreset(name: string, fields: DraftFields): Promise<PresetResponse> {
  return signedRequest<PresetResponse>('/presets', { name, fields }, undefined, undefined, undefined, 'POST');
}

export function updatePreset(id: string, name: string, fields: DraftFields): Promise<PresetResponse> {
  return signedRequest<PresetResponse>('/presets/' + encodeURIComponent(id), { name, fields }, undefined, undefined, undefined, 'PATCH');
}

export function deletePreset(id: string): Promise<void> {
  return signedRequest<void>('/presets/' + encodeURIComponent(id), undefined, undefined, undefined, undefined, 'DELETE');
}

export function listCreditEntries(cursor?: string): Promise<CreditEntryList> {
  const query = '/credits/entries?limit=20' + (cursor ? '&cursor=' + encodeURIComponent(cursor) : '');
  return signedRequest<CreditEntryList>(query);
}
