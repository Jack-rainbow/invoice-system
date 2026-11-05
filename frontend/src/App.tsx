import { AdminWorkspace } from './features/auth/AdminWorkspace.tsx';
export function App() {
 return <div className="app-shell"><header className="app-header"><a className="brand" href="/">▤ 单据工作台</a></header><main><AdminWorkspace /></main><footer>普通用户每个成功新版本消费 1 分，管理员免积分；同版本再次下载免费。</footer></div>;
}
