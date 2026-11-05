import { useEffect, useId, useRef, useState } from 'react';
import type { DraftFields } from '../../shared/api/generated.ts';
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

// The authenticated page should mount this component with a key derived from
// account and document IDs. It intentionally has no localStorage or paid action.
export function EditorForm({ initialFields, requestWatermarkedPreview, previewErrorText, onSaveDraft, onConfirmGeneration, generationPending = false, savedRevision, savePending = false, generationCost = '1' }: Props) {
  const prefix = useId();
  const [fields, setFields] = useState<DraftFields>(() => ({ ...(initialFields ?? emptyFields()) }));
  const [previewURL, setPreviewURL] = useState<string | null>(null);
  const [pending, setPending] = useState(false);
  const [previewError, setPreviewError] = useState('');
  // A paid confirmation may only follow a preview of exactly these fields:
  // any edit invalidates the confirmation without touching the stored draft.
  const [confirmedRevision, setConfirmedRevision] = useState<number | null>(null);
  const sequence = useRef(0);
  const controller = useRef<AbortController | null>(null);
  const heldURL = useRef<string | null>(null);

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
    if (heldURL.current) { URL.revokeObjectURL(heldURL.current); heldURL.current = null; }
    setPreviewURL(null); setPending(false); setPreviewError(''); setConfirmedRevision(null);
    setFields((previous) => ({ ...previous, [field]: value }));
  }

  async function preview() {
    if (!requestWatermarkedPreview || !formValid || pending) return;
    const requestSequence = ++sequence.current;
    controller.current?.abort();
    const abort = new AbortController(); controller.current = abort;
    setPending(true); setPreviewError('');
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
        setPreviewError(previewErrorText?.(failure) ?? '暂时无法获取预览，请稍后重试。');
      }
    } finally {
      if (sequence.current === requestSequence) setPending(false);
    }
  }

  return <div className="editor-layout">
    <form className="card editor-card" onSubmit={(event) => { event.preventDefault(); void preview(); }}>
      <div className="section-heading"><h2>单据资料</h2><span className="muted">当前单据为一行商品</span></div>
      <div className="field-grid">
        {textFields.map(({ key, label, placeholder, hint, full }) => <div className={full ? 'field full' : 'field'} key={key}>
          <label htmlFor={`${prefix}-${key}`}>{label}</label>
          <input id={`${prefix}-${key}`} name={key} value={fields[key]} required placeholder={placeholder}
            aria-describedby={`${prefix}-${key}-hint`} onChange={(event) => edit(key, event.target.value)} />
          <small id={`${prefix}-${key}-hint`}>{hint}</small>
        </div>)}
        <div className="field full">
          <label htmlFor={`${prefix}-unit-description`}>单位说明 <span className="optional">选填</span></label>
          <textarea id={`${prefix}-unit-description`} name="unit_description" rows={2} value={fields.unit_description}
            aria-describedby={`${prefix}-unit-description-hint`} onChange={(event) => edit('unit_description', event.target.value)} />
          <small id={`${prefix}-unit-description-hint`}>最多两行，每行 40 字符</small>
        </div>
        <div className="field">
          <label htmlFor={`${prefix}-price`}>单价 <span className="optional">MXN</span></label>
          <input id={`${prefix}-price`} name="unit_price" inputMode="decimal" value={fields.unit_price} required
            aria-describedby={`${prefix}-amount-hint`} placeholder="0.00" onChange={(event) => edit('unit_price', event.target.value)} />
        </div>
        <div className="field">
          <label htmlFor={`${prefix}-quantity`}>数量</label>
          <input id={`${prefix}-quantity`} name="quantity" inputMode="decimal" value={fields.quantity} required
            aria-describedby={`${prefix}-amount-hint`} onChange={(event) => edit('quantity', event.target.value)} />
        </div>
        <div className="field full">
          <label htmlFor={`${prefix}-certification`}>认证显示时间 <span className="optional">选填</span></label>
          <input id={`${prefix}-certification`} name="certification_display_at" value={fields.certification_display_at ?? ''}
            aria-describedby={`${prefix}-certification-hint`} placeholder="含时区的日期和时间"
            onChange={(event) => edit('certification_display_at', event.target.value || null)} />
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
          {pending ? '正在获取预览…' : requestWatermarkedPreview ? '查看水印预览' : '预览服务尚未接入'}
        </button>
        {onConfirmGeneration && <button type="button" data-testid="confirm-generation" disabled={confirmedRevision === null || generationPending}
          onClick={() => void onConfirmGeneration()}>
          {generationPending ? '正在生成…' : generationCost === '0' ? '生成正式版（免积分）' : `确认生成正式版（消费 ${generationCost} 分）`}
        </button>}
        <span className="muted">编辑和预览免费{requestWatermarkedPreview ? '；查看预览会先保存当前内容' : ''}{confirmedRevision === null ? '；生成前需先预览当前内容' : `；已确认第 ${confirmedRevision} 版预览`}</span>
      </div>
    </form>
    <aside className="card preview-card" aria-busy={pending}>
      <div className="section-heading"><h2>水印预览</h2><span className="status-pill">预览免费</span></div>
      {previewURL ? <img className="preview-image" data-testid="preview-image" src={previewURL} alt="当前单据的服务端水印预览" />
        : <div className="preview-placeholder"><span className="page-symbol" aria-hidden="true">▤</span>
          <h3>预览将在这里显示</h3><p>填写资料后查看带水印的票面。成功生成后可下载无预览水印的正式文件。</p></div>}
      {previewError && <p className="field-error" role="alert">{previewError}</p>}
      <div className="preview-note"><strong>{generationCost === '0' ? '管理员生成免积分' : '每个新版本 1 分'}</strong><p>同版本的 PDF 和 PNG 共用权益，再次下载免费。</p></div>
    </aside>
  </div>;
}
