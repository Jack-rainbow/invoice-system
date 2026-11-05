import { useEffect, useId, useRef, useState } from 'react';
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
  // Maps a failed preview to user wording. The attempt may still have saved the
  // draft, so the message must not claim that nothing happened.
  previewErrorText?: (failure: unknown) => string;
  // Persists the complete field set. The container decides create vs optimistic save.
  onSaveDraft?: (fields: DraftFields) => Promise<void>;
  // Submits the confirmed preview as a paid formal version. One call = one
  // click = one idempotency key; never invoked from a mount effect.
  onConfirmGeneration?: () => Promise<void>;
  generationPending?: boolean;
  savedRevision?: number;
  savePending?: boolean;
}>;

// What a successful free preview proves: the watermarked pixels plus the
// server confirmation the paid generation must echo back.
export type PreviewResult = Readonly<{ image: Blob; manifestId: string; revision: number; expiresAt: string }>;

const textFields = [
  { key: 'recipient_name', label: '收件人名称', placeholder: '填写收件人名称', hint: '最多 160 字符', full: true },
  { key: 'product_code', label: '商品编码', placeholder: '例如 53131620', hint: '最多 32 字符', full: false },
  { key: 'unit_code', label: '单位代码', placeholder: '例如 DPC', hint: '最多 32 字符', full: false },
  { key: 'description', label: '商品名称与说明', placeholder: '填写商品名称、规格', hint: '单行，最多 240 字符', full: true },
  { key: 'issued_at', label: '开具时间', placeholder: '2026-11-04T10:00:00-06:00', hint: '填写日期、时间及其时区偏移', full: true },
] as const;

function emptyFields(): DraftFields {
  return { recipient_name: '', product_code: '', quantity: '1', unit_code: '', unit_description: '',
    description: '', issued_at: new Date().toISOString(), certification_display_at: null, unit_price: '' };
}

// Live preview follows typing with a debounce: each distinct content becomes a
// saved revision and one render slot, so the interval keeps steady typing
// inside the free-preview budget instead of one render per keystroke.
const livePreviewDebounce = 1500;

// The authenticated page mounts this component with a key derived from account
// and document IDs. It intentionally has no localStorage or paid action.
export function EditorForm({ initialFields, requestWatermarkedPreview, fieldBoxes, previewErrorText, onSaveDraft, onConfirmGeneration, generationPending = false, savedRevision, savePending = false, generationCost = '1' }: Props) {
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

  return <div className="editor-layout">
    <form className="card editor-card" onSubmit={(event) => { event.preventDefault(); void preview(true); }}>
      <div className="section-heading"><h2>单据资料</h2><span className="muted">当前单据为一行商品 · 右侧实时预览</span></div>
      <div className="field-grid">
        {textFields.map(({ key, label, placeholder, hint, full }) => <div className={full ? 'field full' : 'field'} key={key}>
          <label htmlFor={`${prefix}-${key}`}>{label}</label>
          <input id={`${prefix}-${key}`} name={key} value={fields[key]} required placeholder={placeholder}
            aria-describedby={`${prefix}-${key}-hint`} onChange={(event) => edit(key, event.target.value)} {...focusProps(key)} />
          <small id={`${prefix}-${key}-hint`}>{hint}</small>
        </div>)}
        <div className="field full">
          <label htmlFor={`${prefix}-unit-description`}>单位说明 <span className="optional">选填</span></label>
          <textarea id={`${prefix}-unit-description`} name="unit_description" rows={2} value={fields.unit_description}
            aria-describedby={`${prefix}-unit-description-hint`} onChange={(event) => edit('unit_description', event.target.value)} {...focusProps('unit_description')} />
          <small id={`${prefix}-unit-description-hint`}>最多两行，每行 40 字符</small>
        </div>
        <div className="field">
          <label htmlFor={`${prefix}-price`}>单价 <span className="optional">MXN</span></label>
          <input id={`${prefix}-price`} name="unit_price" inputMode="decimal" value={fields.unit_price} required
            aria-describedby={`${prefix}-amount-hint`} placeholder="0.00" onChange={(event) => edit('unit_price', event.target.value)} {...focusProps('unit_price')} />
        </div>
        <div className="field">
          <label htmlFor={`${prefix}-quantity`}>数量</label>
          <input id={`${prefix}-quantity`} name="quantity" inputMode="decimal" value={fields.quantity} required
            aria-describedby={`${prefix}-amount-hint`} onChange={(event) => edit('quantity', event.target.value)} {...focusProps('quantity')} />
        </div>
        <div className="field full">
          <label htmlFor={`${prefix}-certification`}>认证显示时间 <span className="optional">选填</span></label>
          <input id={`${prefix}-certification`} name="certification_display_at" value={fields.certification_display_at ?? ''}
            aria-describedby={`${prefix}-certification-hint`} placeholder="含时区的日期和时间"
            onChange={(event) => edit('certification_display_at', event.target.value || null)} {...focusProps('certification_display_at')} />
          <small id={`${prefix}-certification-hint`}>仅作为票面文字展示，不代表已完成税务认证。</small>
        </div>
      </div>
      <div className="estimate" aria-live="polite">
        <div><span>小计</span><output>{estimate?.subtotal ?? '—'}</output></div>
        <div><span>IVA 16%</span><output>{estimate?.tax ?? '—'}</output></div>
        <div className="total"><span>总计 <small>MXN</small></span><output>{estimate?.total ?? '—'}</output></div>
      </div>
      <p className={amountError && fields.unit_price ? 'field-error' : 'muted'} id={`${prefix}-amount-hint`}>
        {amountError || '金额为即时试算，正式金额以确认的服务端预览为准。'}
      </p>
      {timeError && <p className="field-error" role="alert">{timeError}</p>}
      <div className="form-actions">
        {onSaveDraft && <button type="button" data-testid="save-draft" disabled={!formValid || savePending}
          onClick={() => void onSaveDraft({ ...fields })}>
          {savePending ? '正在保存…' : savedRevision ? `保存草稿（当前第 ${savedRevision} 版）` : '保存草稿'}
        </button>}
        <button type="submit" disabled={!requestWatermarkedPreview || !formValid || pending}>
          {pending ? '正在刷新预览…' : '刷新预览'}
        </button>
        {onConfirmGeneration && <button type="button" data-testid="confirm-generation" disabled={confirmedRevision === null || generationPending}
          onClick={() => void onConfirmGeneration()}>
          {generationPending ? '正在生成…' : generationCost === '0' ? '生成正式版（免积分）' : `确认生成正式版（消费 ${generationCost} 分）`}
        </button>}
      </div>
    </form>
    <aside className="card preview-card" aria-busy={pending || liveUpdating}>
      <div className="section-heading"><h2>实时预览</h2>
        <span className="muted">{liveUpdating || pending ? '预览更新中…' : confirmedRevision === null ? '等待有效内容' : `已同步第 ${confirmedRevision} 版`}</span></div>
      <div className="preview-canvas">
        {previewURL
          ? <div className="preview-sheet" data-testid="preview-sheet">
              <img className="preview-image" data-testid="preview-image" src={previewURL} alt="当前单据的服务端水印实时预览" />
              {highlight && <div className="field-highlight" data-testid="field-highlight" style={highlight} />}
            </div>
          : <div className="preview-placeholder"><span className="page-symbol" aria-hidden="true">▤</span>
            <h3>预览将在这里显示</h3><p>填写资料后，右侧会自动生成带水印的票面预览；正在编辑的字段会在票面上高亮定位。</p></div>}
        {(liveUpdating || pending) && previewURL && <span className="preview-live-pill">更新中…</span>}
      </div>
      {previewError && <p className="field-error" role="alert">{previewError}</p>}
      <div className="preview-note"><strong>{generationCost === '0' ? '管理员生成免积分' : '每个新版本 1 分'}</strong><p>同版本的 PDF 和 PNG 共用权益，再次下载免费。</p></div>
    </aside>
  </div>;
}
