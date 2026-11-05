import { useEffect, useRef, useState } from 'react';
import type { CreditEntry, DocumentSummary, DraftFields, PresetResponse, VersionSummary } from '../../shared/api/generated.ts';
import { RequestError } from '../../shared/api/authTransport.ts';
import { changeDraftProfile, createDraft, createPreview, deleteDraft, getDraft, getPreview, getPreviewImage, listDocuments, listRenderProfiles, saveDraft } from './drafts.ts';
import { createGeneration, downloadVersion, getGeneration, listVersions } from './generations.ts';
import { createPreset, deletePreset, listCreditEntries, listPresets } from './presets.ts';
import type { PreviewResult } from './EditorForm.tsx';
import { EditorForm } from './EditorForm.tsx';

const messages: Record<string, string> = {
  REVISION_CONFLICT: '该单据已在其他窗口保存，已为你刷新最新内容，请核对后再次保存。',
  DOCUMENT_HAS_VERSIONS: '该单据已生成正式版本，不能删除。',
  FORBIDDEN: '当前账号无权操作该单据。',
  DEVICE_PROOF_INVALID: '设备验证失败，请重新登录。',
  DEVICE_KEY_UNAVAILABLE: '当前浏览器没有获准的设备密钥，请使用原浏览器。',
  RATE_LIMITED: '操作过于频繁，请稍后重试。',
  TEXT_DOES_NOT_FIT: '字段内容超出票面可用空间，请调整后重试。',
  VALIDATION_ERROR: '填写内容不符合要求，请检查后重试。',
  INVALID_REQUEST: '填写内容不符合要求，请检查后重试。',
  // Preview outcomes keep their own wording: the draft is saved either way and
  // only the free preview failed.
  PREVIEW_STALE: '内容已更新，旧预览不再有效，请重新查看预览。',
  PROFILE_REVOKED: '服务端票面模板已更新，请重新保存草稿后再预览。',
  DEPENDENCY_UNAVAILABLE: '预览服务暂时不可用，请稍后重试。',
  PREVIEW_TIMEOUT: '预览生成较慢，请稍后重试。',
  PREVIEW_INVALID: '收到的预览不是有效的水印图片，请重试。',
  // Paid generation outcomes: none of them silently charged the wallet.
  CREDITS_INSUFFICIENT: '可用积分不足。请联系管理员补充积分后再次确认。',
  COST_CHANGED: '生成成本已变化，请重新预览并确认。',
  GENERATION_CAPACITY_EXCEEDED: '当前排队较多，暂不能受理新生成，请稍后重试。',
  IDEMPOTENCY_CONFLICT: '该确认已按其他内容受理，请刷新后查看任务状态。',
  RESULT_PENDING: '正式文件仍在交付确认中，请稍候在版本列表中下载。',
  GENERATION_TIMEOUT: '生成耗时超过预期，任务仍在处理，请稍后在版本列表查看结果。',
};

const statusLabels: Record<DocumentSummary['status'], string> = {
  DRAFT: '草稿', GENERATING: '生成中', HAS_VERSIONS: '已有正式版本',
};

// Field validation the server reports per draft field, in the user's words.
const fieldLabels: Record<string, string> = {
  recipient_name: '收件人名称', product_code: '商品编码', unit_code: '单位代码',
  unit_description: '单位说明', description: '商品名称与说明', issued_at: '开具时间',
  certification_display_at: '认证显示时间', unit_price: '单价', quantity: '数量', fields: '表单',
};
const fieldCodesText: Record<string, string> = {
  REQUIRED: '不能为空', INVALID_FORMAT: '格式不符合要求（时间需为含时区偏移的完整格式，如 2026-11-05T10:00:00-06:00；金额只能是数字和点号）',
  TEXT_DOES_NOT_FIT: '内容超出票面可用空间', OUT_OF_RANGE: '数值超出允许范围',
};

function fieldDetail(details: unknown): string | null {
  if (!details || typeof details !== 'object' || !('field_errors' in details)) return null;
  const errors = (details as { field_errors: Array<{ field: string; code: string }> }).field_errors;
  if (!Array.isArray(errors) || errors.length === 0) return null;
  return errors.map(error => `${fieldLabels[error.field] ?? error.field}：${fieldCodesText[error.code] ?? '内容不符合要求'}`).join('；') + '。';
}

type Current = Readonly<{ id: string; revision: number; fields: DraftFields }>;

// Polling stays inside one attempt. The server merges duplicate content
// requests, so a short interval cannot pile up renders; the timeout keeps a
// stuck request from holding the button forever.
const previewPollInterval = 600;
const previewTimeout = 30_000;
const generationPollInterval = 700;
const generationTimeout = 120_000;

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => { signal.removeEventListener('abort', abort); resolve(); }, ms);
    function abort() { clearTimeout(timer); reject(new DOMException('Aborted', 'AbortError')); }
    signal.addEventListener('abort', abort, { once: true });
  });
}

// Owns the draft lifecycle for one signed-in user. The editor stays presentational;
// this container is the only place that decides create versus optimistic save,
// the only place that requests the server-watermarked preview, and the only
// place that confirms paid generation.
export function DraftWorkspace({ generationCost }: Readonly<{ generationCost: '0' | '1' }>) {
  const [items, setItems] = useState<DocumentSummary[]>([]);
  const [nextCursor, setNextCursor] = useState<string | null>(null);
  const [current, setCurrent] = useState<Current | null>(null);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(true);
  const [versions, setVersions] = useState<VersionSummary[]>([]);
  const [generating, setGenerating] = useState(false);
  const [presets, setPresets] = useState<PresetResponse[]>([]);
  const [presetName, setPresetName] = useState('');
  const [entries, setEntries] = useState<CreditEntry[]>([]);
  // The editor is remounted only when the user opens or starts another draft.
  // Keying it by revision would unmount it mid-preview, because a preview saves
  // first and therefore advances the revision it is rendering.
  const [formKey, setFormKey] = useState(0);
  const busyRef = useRef(false);
  // The confirmation facts of the last successful preview: what a paid
  // generation must echo. Cleared whenever the draft moves on.
  const confirmedRef = useRef<{ documentId: string; revision: number; manifestId: string } | null>(null);

  async function refresh(cursor?: string) {
    const page = await listDocuments(cursor);
    setItems((previous) => cursor ? [...previous, ...page.items.filter(item => !previous.some(p => p.id === item.id))] : page.items);
    setNextCursor(page.next_cursor);
  }

  async function refreshVersions(documentId: string) {
    try { setVersions(await listVersions(documentId)); } catch { setVersions([]); }
  }

  useEffect(() => {
    let live = true;
    void listDocuments()
      .then((page) => { if (live) { setItems(page.items); setNextCursor(page.next_cursor); } })
      .catch(() => { if (live) setError('暂时无法读取单据列表，请稍后重试。'); })
      .finally(() => { if (live) setLoading(false); });
    return () => { live = false; };
  }, []);

  useEffect(() => {
    if (current) void refreshVersions(current.id);
    else setVersions([]);
  }, [current?.id]);

  async function refreshPresets() {
    try { setPresets((await listPresets()).items); } catch { /* presets are optional chrome */ }
  }

  async function refreshEntries() {
    try { setEntries((await listCreditEntries()).items); } catch { /* entries are optional chrome */ }
  }

  useEffect(() => { void refreshPresets(); void refreshEntries(); }, []);

  function reason(failure: unknown): string {
    if (failure instanceof RequestError) {
      // Validation failures name the exact field instead of a generic hint.
      const detail = fieldDetail(failure.details);
      if (detail) return detail;
      return messages[failure.code] ?? '操作未完成，请稍后重试。';
    }
    return '操作未完成，请稍后重试。';
  }

  async function act(action: () => Promise<void>) {
    if (busyRef.current) return;
    busyRef.current = true; setBusy(true); setError(''); setNotice('');
    try { await action(); } catch (failure) { setError(reason(failure)); }
    finally { busyRef.current = false; setBusy(false); }
  }

  async function save(fields: DraftFields) {
    await act(async () => {
      const snapshot = current;
      const result = snapshot
        ? await saveDraft(snapshot.id, snapshot.revision, fields)
        : await createDraft(fields);
      setCurrent({ id: result.id, revision: result.revision, fields: result.fields });
      setNotice(`已保存为第 ${result.revision} 版；服务端金额 ${result.amounts.total} MXN。`);
      await refresh();
    });
  }

  // A preview costs nothing but a render slot: save the current fields so the
  // request binds one stored revision, queue it, then fetch only the JPEG the
  // server composited. The clean page never reaches this browser.
  async function previewWatermarked(fields: DraftFields, signal: AbortSignal): Promise<PreviewResult> {
    const snapshot = current;
    let saved = snapshot
      ? await saveDraft(snapshot.id, snapshot.revision, fields)
      : await createDraft(fields);
    setCurrent({ id: saved.id, revision: saved.revision, fields: saved.fields });
    setNotice(`预览前已保存为第 ${saved.revision} 版；服务端金额 ${saved.amounts.total} MXN。`);
    let queued;
    try {
      queued = await createPreview(saved.id, saved.revision);
    } catch (failure) {
      // The deployment may have revoked the template this draft was pinned to.
      // Adopting the current ACTIVE profile is an explicit revision bump, then
      // the same preview attempt continues on the new template.
      if (!(failure instanceof RequestError) || failure.code !== 'PROFILE_REVOKED') throw failure;
      const profiles = await listRenderProfiles();
      const active = profiles.items.find((profile) => profile.status === 'ACTIVE');
      if (!active) throw failure;
      saved = await changeDraftProfile(saved.id, saved.revision, active.id);
      setCurrent({ id: saved.id, revision: saved.revision, fields: saved.fields });
      setNotice(`票面模板已更新，已采用 ${active.name} 并保存为第 ${saved.revision} 版。`);
      queued = await createPreview(saved.id, saved.revision);
    }
    let state = queued;
    const deadline = Date.now() + previewTimeout;
    while (state.state === 'QUEUED' || state.state === 'RUNNING') {
      signal.throwIfAborted();
      if (Date.now() > deadline) throw new RequestError('PREVIEW_TIMEOUT');
      await sleep(previewPollInterval, signal);
      state = await getPreview(queued.id);
    }
    if (state.state === 'READY' && state.preview_manifest_id && state.confirmation_expires_at) {
      const image = await getPreviewImage(queued.id, signal);
      await refresh();
      confirmedRef.current = { documentId: saved.id, revision: saved.revision, manifestId: state.preview_manifest_id };
      return { image, manifestId: state.preview_manifest_id, revision: saved.revision, expiresAt: state.confirmation_expires_at };
    }
    if (state.state === 'FAILED' || state.state === 'EXPIRED') throw new RequestError(state.error_code);
    throw new RequestError('PREVIEW_STALE');
  }

  // One click, one idempotency key, one billable version. The key survives the
  // poll loop of this click so a slow network cannot mint a second order.
  async function confirmGeneration() {
    const confirmed = confirmedRef.current;
    if (!confirmed || generating) return;
    setGenerating(true); setError(''); setNotice('');
    const key = crypto.randomUUID();
    try {
      let state = await createGeneration(confirmed.documentId, confirmed.revision, confirmed.manifestId, generationCost, key);
      const deadline = Date.now() + generationTimeout;
      while (state.state === 'QUEUED' || state.state === 'RUNNING' || state.state === 'RETRY_WAIT' || state.state === 'FINALIZING') {
        if (Date.now() > deadline) throw new RequestError('GENERATION_TIMEOUT');
        await sleep(generationPollInterval, new AbortController().signal);
        state = await getGeneration(state.generation_id);
      }
      if (state.state === 'COMPLETED') {
        setNotice(`正式版 v${state.version_id.slice(0, 8)} 已生成${generationCost === '1' ? '，已消费 1 分' : ''}。PDF 与 PNG 均可在下方版本列表免费重复下载。`);
      } else {
        setNotice('本次生成未完成，冻结的 1 分已退回可用积分。调整内容后可重新预览并确认。');
      }
      await refresh();
      await refreshEntries();
      if (current) await refreshVersions(current.id);
    } catch (failure) {
      setError(reason(failure));
    } finally {
      setGenerating(false);
    }
  }

  async function open(id: string) {
    await act(async () => {
      const draft = await getDraft(id);
      setCurrent({ id: draft.id, revision: draft.revision, fields: draft.fields });
      confirmedRef.current = null;
      setFormKey((previous) => previous + 1);
      setNotice('');
    });
  }

  // Presets store only the field set the user typed; applying one opens a new
  // editable draft form with those values (server recomputes every amount).
  async function savePresetAs() {
    const name = presetName.trim();
    const fields = current?.fields;
    if (!name || !fields) return;
    await act(async () => {
      await createPreset(name, fields);
      setPresetName('');
      setNotice(`已保存商品预设「${name}」。`);
      await refreshPresets();
    });
  }

  function applyPreset(preset: PresetResponse) {
    setCurrent((previous) => previous ? { id: previous.id, revision: previous.revision, fields: preset.fields } : previous);
    if (!current) return;
    confirmedRef.current = null;
    setFormKey((previous) => previous + 1);
    setNotice(`已套用预设「${preset.name}」的字段，请核对金额后保存。`);
  }

  async function removePreset(preset: PresetResponse) {
    await act(async () => {
      await deletePreset(preset.id);
      await refreshPresets();
    });
  }

  async function remove(item: DocumentSummary) {
    await act(async () => {
      await deleteDraft(item.id, item.revision);
      if (current?.id === item.id) setCurrent(null);
      setNotice('草稿已删除。');
      await refresh();
    });
  }

  return <div className="draft-workspace">
    <section className="card" aria-busy={loading}>
      <div className="section-heading"><h2>我的单据</h2><span className="muted">最近 5 条，按创建时间倒序</span></div>
      {error && <p className="field-error" role="alert">{error}</p>}
      {notice && <p className="muted" data-testid="draft-notice">{notice}</p>}
      {loading ? <p className="muted">正在读取单据…</p> : items.length === 0
        ? <p className="muted" data-testid="draft-empty">还没有单据。填写资料后保存草稿。</p>
        : <div className="customer-list">{items.map(item => <div key={item.id} data-testid="draft-row" data-revision={item.revision} data-document-id={item.id}>
            <strong>{item.id.slice(0, 8)}</strong>
            <span>第 <b data-testid="draft-revision">{item.revision}</b> 版 · {statusLabels[item.status]}</span>
            <span>{new Date(item.created_at).toLocaleString()}</span>
            <span className="form-actions">
              <button type="button" disabled={busy} onClick={() => void open(item.id)}>打开</button>
              {item.status === 'DRAFT' && <button type="button" disabled={busy} onClick={() => void remove(item)}>删除</button>}
            </span>
          </div>)}</div>}
      <div className="form-actions">
        <button type="button" disabled={busy} onClick={() => { setCurrent(null); setFormKey((previous) => previous + 1); setNotice(''); setError(''); }}>新建草稿</button>
        {nextCursor && <button type="button" disabled={busy} onClick={() => void act(async () => { await refresh(nextCursor); })}>加载更多</button>}
      </div>
    </section>
    <EditorForm
      key={'draft-form-' + formKey}
      {...(current ? { initialFields: current.fields } : {})}
      generationCost={generationCost}
      onSaveDraft={save}
      requestWatermarkedPreview={previewWatermarked}
      onConfirmGeneration={confirmGeneration}
      generationPending={generating}
      previewErrorText={reason}
      {...(current ? { savedRevision: current.revision } : {})}
      savePending={busy}
    />
    {current && versions.length > 0 && <section className="card" data-testid="version-panel">
      <div className="section-heading"><h2>正式版本</h2><span className="muted">同版本 PDF / PNG 共用权益，重复下载免费</span></div>
      <div className="customer-list">{versions.map(version => <div key={version.id} data-testid="version-row">
        <strong>v{version.version_no}</strong>
        <span>第 {version.source_revision} 版草稿 · 总计 {version.amounts.total} MXN</span>
        <span>{new Date(version.created_at).toLocaleString()}</span>
        <span className="form-actions">
          {version.delivery_state === 'READY'
            ? <>
                <button type="button" data-testid="download-pdf" disabled={busy} onClick={() => void act(async () => { await downloadVersion(version.id, 'pdf'); })}>下载 PDF</button>
                <button type="button" data-testid="download-png" disabled={busy} onClick={() => void act(async () => { await downloadVersion(version.id, 'png'); })}>下载 PNG</button>
              </>
            : <span className="muted">交付确认中…</span>}
        </span>
      </div>)}</div>
    </section>}
    {presets.length > 0 && <section className="card" data-testid="preset-panel">
      <div className="section-heading"><h2>商品预设</h2><span className="muted">仅保存本人可填字段，金额由服务端重新计算</span></div>
      <div className="customer-list">{presets.map(preset => <div key={preset.id} data-testid="preset-row">
        <strong>{preset.name}</strong>
        <span>{preset.fields.description}</span>
        <span className="form-actions">
          <button type="button" disabled={busy || !current} onClick={() => applyPreset(preset)}>套用</button>
          <button type="button" disabled={busy} onClick={() => void removePreset(preset)}>删除</button>
        </span>
      </div>)}</div>
    </section>}
    <section className="card" data-testid="ledger-panel">
      <div className="section-heading"><h2>积分流水</h2><span className="muted">可用与冻结分开记录；预留不是最终扣费</span></div>
      {entries.length === 0 ? <p className="muted">暂无流水。</p> : <div className="customer-list">{entries.map(entry => <div key={entry.id} data-testid="ledger-row">
        <strong>{entry.kind}</strong>
        <span>可用 {entry.delta_available.startsWith('-') ? '' : '+'}{entry.delta_available} · 冻结 {entry.delta_frozen.startsWith('-') ? '' : '+'}{entry.delta_frozen}</span>
        <span>余额 {entry.after_available} / 冻结 {entry.after_frozen}</span>
        <span>{new Date(entry.created_at).toLocaleString()}</span>
      </div>)}</div>}
      {current && <div className="form-actions">
        <label htmlFor="preset-name">预设名称</label>
        <input id="preset-name" value={presetName} maxLength={160} placeholder="将当前草稿存为预设" onChange={(event) => setPresetName(event.target.value)} />
        <button type="button" data-testid="save-preset" disabled={busy || !presetName.trim() || !current} onClick={() => void savePresetAs()}>保存为预设</button>
      </div>}
    </section>
  </div>;
}
