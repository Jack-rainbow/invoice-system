import { useEffect, useId, useRef, useState, type ReactNode } from 'react';
import type { DraftFields, FieldBoxMap } from '../../shared/api/generated.ts';
import { calculateEstimate, EstimateError } from './amounts.ts';
import type { Estimate } from './amounts.ts';

type Props = Readonly<{
  initialFields?: DraftFields;
  // Display only. Formal cost will still be calculated by the server.
  generationCost?: '0' | '1';
  // Authenticated draft service supplies only its server-watermarked JPEG plus
  // the confirmation facts bound to it. Never pass an original template or a
  // clean artifact through this callback.
  requestWatermarkedPreview?: (fields: DraftFields, signal: AbortSignal) => Promise<PreviewResult>;
  // Maps a field key to its rectangle on the rendered page (canvas fractions);
  // used to highlight the field being edited on the live preview.
  fieldBoxes?: FieldBoxMap;
  // Maps a failed preview to user wording.
  previewErrorText?: (failure: unknown) => string;
  // Persists the complete field set. The container decides create vs optimistic save.
  onSaveDraft?: (fields: DraftFields) => Promise<void>;
  // Paid formal generation, click-triggered only. One click = one idempotency key.
  onConfirmGeneration?: () => Promise<void>;
  // Formal file export: settles the paid version once, then downloads.
  onExport?: (format: 'pdf' | 'png') => Promise<void>;
  // Clears the editor into a fresh draft.
  onReset?: () => void;
  // Extra sections rendered under the field groups (presets, batch, …).
  extraFormSections?: ReactNode;
  generationPending?: boolean;
  savedRevision?: number;
  savePending?: boolean;
}>;

// What a successful free preview proves: the watermarked pixels plus the
// server confirmation the paid generation must echo back.
export type PreviewResult = Readonly<{ image: Blob; manifestId: string; revision: number; expiresAt: string }>;

// Live preview follows typing with a debounce: each distinct content becomes a
// saved revision and one render slot, so the interval keeps steady typing
// inside the free-preview budget instead of one render per keystroke.
const livePreviewDebounce = 1500;

function emptyFields(): DraftFields {
  return { recipient_name: '', product_code: '', quantity: '1', unit_code: '', unit_description: '',
    description: '', issued_at: new Date().toISOString(), certification_display_at: null, unit_price: '' };
}

// The replicated editor: a grouped form column (收款方 / 商品明细 + extra
// sections) beside a large dotted preview canvas with the export toolbar and
// zoom controls. Paid actions are click-triggered only and gated on the
// confirmed preview of exactly the fields on screen.
export function EditorForm({ initialFields, requestWatermarkedPreview, fieldBoxes, previewErrorText, onSaveDraft, onConfirmGeneration, onExport, onReset, extraFormSections, generationPending = false, savedRevision, savePending = false, generationCost = '1' }: Props) {
  const prefix = useId();
  const [fields, setFields] = useState<DraftFields>(() => ({ ...(initialFields ?? emptyFields()) }));
  const [previewURL, setPreviewURL] = useState<string | null>(null);
  const [pending, setPending] = useState(false);
  const [previewError, setPreviewError] = useState('');
  const [liveUpdating, setLiveUpdating] = useState(false);
  // A paid confirmation may only follow a preview of exactly these fields:
  // any edit invalidates the confirmation without touching the stored draft.
  const [confirmedRevision, setConfirmedRevision] = useState<number | null>(null);
  // The form field being edited right now; its page rectangle lights up.
  const [activeField, setActiveField] = useState<string | null>(null);
  // Preview zoom in percent of the canvas width; 100 fits the panel width.
  const [zoom, setZoom] = useState(100);
  const sequence = useRef(0);
  const controller = useRef<AbortController | null>(null);
  const heldURL = useRef<string | null>(null);
  const fieldsKey = JSON.stringify(fields);

  useEffect(() => () => {
    sequence.current++;
    controller.current?.abort();
    if (heldURL.current) URL.revokeObjectURL(heldURL.current);
  }, []);

  let estimate: Estimate | undefined;
  let amountError = '';
  try { estimate = calculateEstimate(fields.unit_price, fields.quantity); }
  catch (error) {
    amountError = error instanceof EstimateError && error.code === 'AMOUNT_LIMIT'
      ? '总计超过允许范围，请调整单价或数量。'
      : '请填写有效的非负单价和数量。单价最多 2 位小数，数量最多 6 位小数。';
  }
  // Mirror the server's RFC3339 wire shape so a malformed timestamp is caught
  // before it can fail the save/preview round-trip.
  const timeShape = /^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$/;
  let timeError = '';
  if (!timeShape.test(fields.issued_at)) {
    timeError = '开具时间需为含时区偏移的完整格式，例如 2026-11-05T10:00:00-06:00。';
  } else if (fields.certification_display_at && !timeShape.test(fields.certification_display_at)) {
    timeError = '认证显示时间格式同开具时间（不需要时请留空），例如 2026-11-05T10:01:00-06:00。';
  }
  const formValid = !!estimate && !timeError;

  function edit<K extends keyof DraftFields>(field: K, value: DraftFields[K]) {
    sequence.current++;
    controller.current?.abort();
    setPreviewError(''); setConfirmedRevision(null);
    setFields((previous) => ({ ...previous, [field]: value }));
  }

  async function preview(manual: boolean) {
    if (!requestWatermarkedPreview || !formValid || (pending && manual)) return;
    const requestSequence = ++sequence.current;
    controller.current?.abort();
    const abort = new AbortController(); controller.current = abort;
    setPending(true);
    if (!manual) setLiveUpdating(true);
    setPreviewError('');
    try {
      const result = await requestWatermarkedPreview({ ...fields }, abort.signal);
      if (abort.signal.aborted || sequence.current !== requestSequence) return;
      const image = result.image;
      if (image.type !== 'image/jpeg' || image.size === 0 || image.size > 2 * 1024 * 1024) throw new Error('Invalid preview');
      if (heldURL.current) URL.revokeObjectURL(heldURL.current);
      const nextURL = URL.createObjectURL(image); heldURL.current = nextURL; setPreviewURL(nextURL);
      setConfirmedRevision(result.revision);
    } catch (failure) {
      if (!abort.signal.aborted && sequence.current === requestSequence) {
        // Live updates fail quietly on throttling: the last good image stays
        // and the next debounced attempt retries. Manual clicks stay loud.
        const code = (failure instanceof Error && 'code' in failure) ? String((failure as { code?: unknown }).code) : '';
        if (!manual && (code === 'RATE_LIMITED' || code === 'PREVIEW_TIMEOUT')) {
          setPreviewError('');
        } else {
          setPreviewError(previewErrorText?.(failure) ?? '暂时无法获取预览，请稍后重试。');
        }
      }
    } finally {
      if (sequence.current === requestSequence) { setPending(false); setLiveUpdating(false); }
    }
  }

  // Live preview: debounce the latest fields into one saved revision + render.
  const firstRender = useRef(true);
  useEffect(() => {
    if (firstRender.current) { firstRender.current = false; return; }
    if (!requestWatermarkedPreview || !formValid) return;
    setLiveUpdating(true);
    const timer = setTimeout(() => void preview(false), livePreviewDebounce);
    return () => { clearTimeout(timer); setLiveUpdating(false); };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fieldsKey, formValid]);

  const focusProps = (key: string) => ({
    onFocus: () => setActiveField(key),
    onBlur: () => setActiveField((current) => current === key ? null : current),
  });
  const activeBox = activeField ? fieldBoxes?.[activeField as keyof FieldBoxMap] : undefined;
  const highlight = activeBox
    ? { left: `${activeBox.x * 100}%`, top: `${activeBox.y * 100}%`,
        width: `${activeBox.w * 100}%`, height: `${activeBox.h * 100}%` }
    : null;
  const exportDisabled = confirmedRevision === null || generationPending;

  return <div className="tool-layout">
    <div className="form-column">
      <section className="form-group">
        <div className="form-group-heading"><h3>收款方</h3><span className="group-badge">Receptor</span></div>
        <div className="field">
          <label htmlFor={`${prefix}-recipient_name`}>公司 / 客户名称</label>
          <input id={`${prefix}-recipient_name`} name="recipient_name" value={fields.recipient_name} required
            placeholder="例如 USLAURENHANNAGROUPPLLC" onChange={(event) => edit('recipient_name', event.target.value)} {...focusProps('recipient_name')} />
        </div>
      </section>

      <section className="form-group">
        <div className="form-group-heading"><h3>商品明细</h3><span className="group-badge">Concepto</span></div>
        <div className="field-row">
          <div className="field"><label htmlFor={`${prefix}-product_code`}>产品代码</label>
            <input id={`${prefix}-product_code`} name="product_code" value={fields.product_code} required inputMode="numeric"
              placeholder="53131620" onChange={(event) => edit('product_code', event.target.value)} {...focusProps('product_code')} /></div>
          <div className="field"><label htmlFor={`${prefix}-quantity`}>数量</label>
            <input id={`${prefix}-quantity`} name="quantity" value={fields.quantity} required inputMode="decimal"
              placeholder="5" onChange={(event) => edit('quantity', event.target.value)} {...focusProps('quantity')} /></div>
        </div>
        <div className="field-row">
          <div className="field"><label htmlFor={`${prefix}-unit_code`}>单位</label>
            <input id={`${prefix}-unit_code`} name="unit_code" value={fields.unit_code} required
              placeholder="DPC" onChange={(event) => edit('unit_code', event.target.value)} {...focusProps('unit_code')} /></div>
          <div className="field"><label htmlFor={`${prefix}-unit_price`}>单价 <span className="optional">MXN</span></label>
            <input id={`${prefix}-unit_price`} name="unit_price" value={fields.unit_price} required inputMode="decimal"
              placeholder="86.21" onChange={(event) => edit('unit_price', event.target.value)} {...focusProps('unit_price')} /></div>
        </div>
        <div className="field"><label htmlFor={`${prefix}-unit_description`}>单位描述 <span className="optional">选填</span></label>
          <textarea id={`${prefix}-unit_description`} name="unit_description" rows={2} value={fields.unit_description}
            placeholder="Docenas de piezas" onChange={(event) => edit('unit_description', event.target.value)} {...focusProps('unit_description')} /></div>
        <div className="field"><label htmlFor={`${prefix}-description`}>商品描述</label>
          <input id={`${prefix}-description`} name="description" value={fields.description} required
            placeholder="Xerjoff Erba Gold Edp 100 ml" onChange={(event) => edit('description', event.target.value)} {...focusProps('description')} /></div>
        <div className="field-row">
          <div className="field"><label htmlFor={`${prefix}-issued_at`}>开具时间</label>
            <input id={`${prefix}-issued_at`} name="issued_at" value={fields.issued_at} required
              placeholder="2026-11-04T10:00:00-06:00" onChange={(event) => edit('issued_at', event.target.value)} {...focusProps('issued_at')} /></div>
          <div className="field"><label htmlFor={`${prefix}-certification_display_at`}>认证显示时间 <span className="optional">选填</span></label>
            <input id={`${prefix}-certification_display_at`} name="certification_display_at" value={fields.certification_display_at ?? ''}
              placeholder="仅为票面文字" onChange={(event) => edit('certification_display_at', event.target.value || null)} {...focusProps('certification_display_at')} /></div>
        </div>
        <div className="estimate" aria-live="polite">
          <div><span>小计</span><output>{estimate?.subtotal ?? '—'}</output></div>
          <div><span>IVA 16%</span><output>{estimate?.tax ?? '—'}</output></div>
          <div className="total"><span>总计 <small>MXN</small></span><output>{estimate?.total ?? '—'}</output></div>
        </div>
        <p className={amountError && fields.unit_price ? 'field-error' : 'muted'}>
          {amountError || '金额为即时试算，正式金额以确认的服务端预览为准。'}
        </p>
        {timeError && <p className="field-error" role="alert">{timeError}</p>}
        {previewError && <p className="field-error" role="alert">{previewError}</p>}
        <div className="form-actions">
          {onSaveDraft && <button type="button" className="ghost" data-testid="save-draft" disabled={!formValid || savePending}
            onClick={() => void onSaveDraft({ ...fields })}>
            {savePending ? '正在保存…' : savedRevision ? `保存草稿（第 ${savedRevision} 版）` : '保存草稿'}</button>}
        </div>
      </section>

      {extraFormSections}
    </div>

    <div className="canvas-column">
      <div className="canvas-toolbar">
        <span className="muted">{liveUpdating || pending ? '预览更新中…' : confirmedRevision === null ? '等待有效内容' : `已同步第 ${confirmedRevision} 版`}</span>
        <div className="canvas-toolbar-actions">
          {onReset && <button type="button" className="ghost" onClick={onReset}>↺ 重置</button>}
          {onExport && <button type="button" className="ghost" data-testid="export-png" disabled={exportDisabled} onClick={() => void onExport('png')}>导出 PNG</button>}
          {onConfirmGeneration && <button type="button" className="primary" data-testid="confirm-generation"
            disabled={exportDisabled} onClick={() => void onConfirmGeneration()}>
            {generationPending ? '正在生成…' : generationCost === '0' ? '生成票据（免积分）' : `生成票据（消费 ${generationCost} 分）`}</button>}
          {onExport && <button type="button" className="primary" data-testid="export-pdf" disabled={exportDisabled} onClick={() => void onExport('pdf')}>导出 PDF</button>}
        </div>
      </div>
      <div className="preview-canvas" aria-busy={pending || liveUpdating}>
        {previewURL
          ? <div className="preview-sheet" data-testid="preview-sheet" style={{ width: `${zoom}%` }}>
              <img className="preview-image" data-testid="preview-image" src={previewURL} alt="当前单据的服务端水印实时预览" />
              {highlight && <div className="field-highlight" data-testid="field-highlight" style={highlight} />}
            </div>
          : <div className="preview-placeholder"><span className="page-symbol" aria-hidden="true">▤</span>
            <h3>预览将在这里显示</h3><p>填写左侧资料后，这里自动生成带水印的票面预览；正在编辑的字段会在票面上高亮定位。</p></div>}
        {(liveUpdating || pending) && previewURL && <span className="preview-live-pill">更新中…</span>}
        {previewURL && <div className="preview-zoom" role="group" aria-label="预览缩放">
          <button type="button" onClick={() => setZoom((z) => Math.max(50, z - 10))} aria-label="缩小">−</button>
          <button type="button" className="zoom-value" onClick={() => setZoom(100)}>{zoom}%</button>
          <button type="button" onClick={() => setZoom((z) => Math.min(200, z + 10))} aria-label="放大">＋</button>
        </div>}
      </div>
      <div className="preview-note"><strong>{generationCost === '0' ? '管理员生成免积分' : '每个新版本 1 分'}</strong><p>同一版本的 PDF 与 PNG 共用权益，再次下载免费；正式文件无预览水印。</p></div>
    </div>
  </div>;
}
