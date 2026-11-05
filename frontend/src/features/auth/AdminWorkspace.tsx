import { useEffect, useRef, useState } from 'react';
import type { AdminUserList, BootstrapConfirmResponse, BootstrapStartResponse, CreateUserResponse, GrantResponse, MeResponse, PreSessionResponse } from '../../shared/api/generated.ts';
import { DraftWorkspace } from '../editor/DraftWorkspace.tsx';

import { request, RequestError } from '../../shared/api/authTransport.ts';
import { DeviceKeyStorageError } from '../../shared/device/keyStore.ts';
import { authenticatedMe, initialDeviceSetup, loginDevice, logoutDevice } from './deviceAuth.ts';

const messages: Record<string, string> = {
  AUTHENTICATION_REQUIRED: '身份验证失败或会话已过期，请重新验证。', CSRF_INVALID: '页面验证已过期，请重试当前操作。',
  RATE_LIMITED: '尝试过于频繁，请稍后再试。', FORBIDDEN: '当前操作未获授权，请重新验证管理员身份并核对客户状态。',
  IDEMPOTENCY_CONFLICT: '该操作编号已用于其他内容，请核对操作结果。', SETUP_LINK_ALREADY_ISSUED: '用户已创建；设置链接只显示一次，需要明确重新签发。',
  INVALID_REQUEST: '填写内容不符合要求，请检查后重试。', REVISION_CONFLICT: '单据已在其他窗口更新，请重新载入后再保存。', DOCUMENT_HAS_VERSIONS: '该单据已生成正式版本，不能删除。', DEVICE_KEY_UNAVAILABLE: '当前浏览器没有获准的设备密钥，请使用原浏览器，或联系管理员换机。', DEVICE_KEY_STORAGE_UNAVAILABLE: '浏览器无法可靠保存设备密钥，已停止激活。', DEVICE_PROOF_INVALID: '设备验证失败，请重新登录；如更换浏览器，请联系管理员。',
};

type ManagedUser = Omit<CreateUserResponse, 'setup_link'>;
export function AdminWorkspace() {
  const [setupToken, setSetupToken] = useState(() => location.pathname === '/setup' ? new URLSearchParams(location.hash.slice(1)).get('token') ?? '' : '');
  const [bootstrapToken, setBootstrapToken] = useState(() => location.pathname === '/setup/admin' ? new URLSearchParams(location.hash.slice(1)).get('token') ?? '' : '');
  const [me, setMe] = useState<MeResponse | null>(null);
  const [checking, setChecking] = useState(!bootstrapToken && !setupToken);
  const [name, setName] = useState(''); const [password, setPassword] = useState('');
  const [error, setError] = useState(''); const [busy, setBusy] = useState(false); const busyRef = useRef(false);
  const [tab, setTab] = useState<'customers' | 'editor'>('customers');
  const [users, setUsers] = useState<ManagedUser[]>([]); const [selected, setSelected] = useState('');
  const [nextCursor, setNextCursor] = useState<string | null>(null);
  const [customer, setCustomer] = useState(''); const [verification, setVerification] = useState(''); const [reason, setReason] = useState('');
  const createKey = useRef<string | null>(null); const [setupLink, setSetupLink] = useState('');
  const [amount, setAmount] = useState('1'); const [grantReason, setGrantReason] = useState('');
  const grantKey = useRef<string | null>(null); const grantCommand = useRef<{ id: string; amount: string; reason: string } | null>(null);
  const [grantResult, setGrantResult] = useState<GrantResponse | null>(null); const [pendingGrant, setPendingGrant] = useState(false);
  const loadMe = async () => { const value = await authenticatedMe(); setMe(value); setBootstrapToken(''); setSetupToken(''); };
  const refreshUsers = async () => { const value = await request<AdminUserList>('/admin/users'); setUsers(value.items); setNextCursor(value.next_cursor); };
  const loadMoreUsers = async () => { if (!nextCursor) return; const value = await request<AdminUserList>('/admin/users?cursor=' + encodeURIComponent(nextCursor)); setUsers(previous => [...previous,...value.items.filter(u => !previous.some(p => p.user.id === u.user.id))]);setNextCursor(value.next_cursor); };
  useEffect(() => {
    if (location.hash) history.replaceState(null, '', location.pathname + location.search);
    if (bootstrapToken || setupToken) return;
    const abort = new AbortController();
    void authenticatedMe(abort.signal).then(value => { if (!abort.signal.aborted) setMe(value); })
      .catch(() => { if (!abort.signal.aborted) setMe(null); }).finally(() => { if (!abort.signal.aborted) setChecking(false); });
    return () => abort.abort();
  }, [bootstrapToken, setupToken]);
  async function act(action: () => Promise<void>) {
    if (busyRef.current) return;
    busyRef.current = true; setBusy(true); setError('');
    try { await action(); } catch (failure) { setError((failure instanceof RequestError || failure instanceof DeviceKeyStorageError) ? messages[failure.code] ?? '服务暂时不可用，请保留当前操作并稍后重试。' : '请求未完成，请使用原操作重试。'); }
    finally { busyRef.current = false; setBusy(false); }
  }
  if (checking) return <section className="card auth-card" role="status">正在核对登录状态…</section>;
  if (!me && setupToken) return <section className="card auth-card"><h1>激活客户设备</h1><p>此浏览器将保存不可导出的设备密钥，请使用日后登录的浏览器。</p>{error && <p className="field-error" role="alert">{error}</p>}<form onSubmit={e => {e.preventDefault();void act(async () => {await initialDeviceSetup(setupToken,password);setPassword('');await loadMe();});}}><label>设置客户密码<input type="password" autoComplete="new-password" required value={password} onChange={e => setPassword(e.target.value)} /></label><p className="muted">12–128 个字符。浏览器无法保存密钥时会停止激活。</p><button disabled={busy}>设置密码并授权此浏览器</button></form></section>;
  if (!me) return <section className="card auth-card">
    <h1>{bootstrapToken ? '启用超级管理员' : '登录单据工作台'}</h1>
    {error && <p className="field-error" role="alert">{error}</p>}
    {bootstrapToken ? <form onSubmit={e => { e.preventDefault(); void act(async () => {
      // 2FA removed: setting the password completes enrollment directly.
      const value = await request<BootstrapStartResponse>('/auth/admin/bootstrap/start', { bootstrap_token: bootstrapToken, new_password: password });
      await request<BootstrapConfirmResponse>('/auth/admin/bootstrap/confirm', { bootstrap_context: value.bootstrap_context, totp: '' });
      setPassword(''); await loadMe();
    }); }}>
      <label>设置管理员密码<input type="password" autoComplete="new-password" required value={password} onChange={e => setPassword(e.target.value)} /></label>
      <p className="muted">12–128 个字符。设置后即可登录，无需验证器。</p>
      <button disabled={busy}>设置密码并启用</button>
    </form> : <form onSubmit={e => { e.preventDefault(); void act(async () => {
      const result = await request<PreSessionResponse>('/auth/login', { username: name, password });
      setPassword('');
      if (result.stage === 'AUTHENTICATED') { await loadMe(); }
      else { await loginDevice(); await loadMe(); }
    }); }}>
      <label>用户名<input autoComplete="username" required value={name} onChange={e => setName(e.target.value)} /></label>
      <label>登录密码<input type="password" autoComplete="current-password" required value={password} onChange={e => setPassword(e.target.value)} /></label>
      <button disabled={busy}>登录</button>
    </form>}
  </section>;
  if (me.user.role === 'USER') return <>
    <header className="tool-topbar">
      <span className="tool-logo" aria-hidden="true">F</span>
      <div className="tool-title"><strong>单据模板生成器</strong><small>CFDI 版式 · 实时预览</small></div>
      <span className="spacer" />
      <div className="tool-user"><span data-testid="signed-in-user">{me.user.username} · 客户</span>
        <span className="balance" data-testid="user-balance">可用积分 {me.wallet.available}</span>
        <button type="button" className="ghost" disabled={busy} onClick={() => void act(async () => { await logoutDevice(); setMe(null); })}>退出登录</button></div>
    </header>
    {error && <div className="under-panels"><p className="field-error" role="alert">{error}</p></div>}
    <DraftWorkspace generationCost="1" />
  </>;
  return <>
    <header className="tool-topbar">
      <span className="tool-logo" aria-hidden="true">F</span>
      <div className="tool-title"><strong>单据模板生成器</strong><small>CFDI 版式 · 实时预览</small></div>
      <button type="button" className={'tool-tab' + (tab === 'editor' ? ' active' : '')} onClick={() => setTab('editor')}>制作单据</button>
      <button type="button" className={'tool-tab' + (tab === 'customers' ? ' active' : '')} onClick={() => setTab('customers')}>客户管理</button>
      <span className="spacer" />
      <div className="tool-user"><span data-testid="signed-in-user">{me.user.username} · 管理员</span>
        <button type="button" className="ghost" disabled={busy} onClick={() => void act(async () => { await request('/auth/logout', {}); setMe(null); setUsers([]); setSelected(''); setSetupLink(''); setGrantResult(null); grantKey.current = null; grantCommand.current = null; setPendingGrant(false); })}>退出登录</button></div>
    </header>
    {error && <p className="field-error" role="alert">{error}</p>}
    {tab === 'editor' ? <DraftWorkspace generationCost="0" /> : <>
      <div className="form-actions"><button type="button" disabled={busy} onClick={() => void act(refreshUsers)}>刷新客户列表</button></div>
      <div className="admin-layout">
        <form className="card" onSubmit={e => { e.preventDefault(); void act(async () => { createKey.current ??= crypto.randomUUID(); const result = await request<CreateUserResponse>('/admin/users', { username: customer, customer_verification_ref: verification, reason }, createKey.current); setUsers(previous => [result, ...previous.filter(u => u.user.id !== result.user.id)]); setSelected(result.user.id); setSetupLink(result.setup_link); createKey.current = null; setCustomer(''); setVerification(''); setReason(''); }); }}>
          <h2>新增客户</h2><label>客户用户名<input required value={customer} disabled={busy} onChange={e => { setCustomer(e.target.value); createKey.current = null; }} /></label>
          <label>核验渠道引用<input required value={verification} disabled={busy} onChange={e => { setVerification(e.target.value); createKey.current = null; }} /></label>
          <label>开户原因<input required value={reason} disabled={busy} onChange={e => { setReason(e.target.value); createKey.current = null; }} /></label><p className="muted">客户初始积分为 0，完成设备授权后才能使用业务。</p><button disabled={busy}>新增客户</button>
          {setupLink && <p>一次性设置链接（请通过登记渠道人工交付）：<code data-testid="setup-link">{setupLink}</code></p>}
        </form>
        <form className="card" onSubmit={e => { e.preventDefault(); void act(async () => {
          if (!grantCommand.current) { grantKey.current = crypto.randomUUID(); grantCommand.current = { id: selected, amount, reason: grantReason }; }
          setPendingGrant(true); const command = grantCommand.current; const result = await request<GrantResponse>(`/admin/users/${command.id}/credit-grants`, { amount: command.amount, reason: command.reason }, grantKey.current!);
          setGrantResult(result); setUsers(previous => previous.map(u => u.user.id === command.id ? { ...u, wallet: result.wallet } : u)); setPendingGrant(false);
        }); }}>
          <h2>增加积分</h2><label>目标客户<select required value={selected} disabled={busy || !!grantCommand.current} onChange={e => setSelected(e.target.value)}><option value="">请选择客户</option>{users.map(u => <option key={u.user.id} value={u.user.id}>{u.user.username}</option>)}</select></label>
          <label>增加分值<input inputMode="numeric" required value={amount} disabled={busy || !!grantCommand.current} onChange={e => setAmount(e.target.value)} /></label>
          <label>加分原因<input required value={grantReason} disabled={busy || !!grantCommand.current} onChange={e => setGrantReason(e.target.value)} /></label>
          <button disabled={busy || !selected}>{grantResult ? '再次确认本次操作' : pendingGrant ? '用原操作重试' : '增加积分'}</button>
          {grantResult && <><p data-testid="grant-result">操作编号：{grantResult.operation_id}，可用积分 {grantResult.wallet.available}</p><button type="button" disabled={busy} onClick={() => { grantCommand.current = null; grantKey.current = null; setGrantResult(null); }}>新增一笔操作</button></>}
        </form>
      </div>
      <section className="card"><h2>客户状态</h2><div className="customer-list">{users.map(u => <div key={u.user.id} data-testid="customer-row" data-user-id={u.user.id}><strong>{u.user.username}</strong><span>可用积分 <b data-testid="available">{u.wallet.available}</b> · 冻结积分 <b data-testid="frozen">{u.wallet.frozen}</b></span><span>{u.user.onboarding_state === 'NEEDS_SETUP' ? '等待设备激活' : '已设置'}</span></div>)}</div>{nextCursor && <button type="button" disabled={busy} onClick={() => void act(loadMoreUsers)}>加载更多客户</button>}</section>
    </>}
  </>;
}
