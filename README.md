# 单据系统

Go + React/Vite 在线单据系统，按目标文档逐步落地。完整范围包括账号/MFA、浏览器持钥、积分账本、像素水印预览、不可变正式版本、PDF/PNG 下载、批量、审计和故障恢复。

**当前处于开发阶段，不能商用。** 已接通受保护 CLI 初始化、管理员 MFA、零积分开户、人工加分、客户设备授权、草稿、水印预览，以及**正式生成付费链路**（确认 manifest → 容量准入 → 1分预留 → CLEAN PDF/PNG → 耐久回执 → 读租约下载 → 0分免费重下）与**指定公钥换机**（PAIR 配对 → 管理员批准 RESET → 指定私钥单次领取）。批量、免费修复、MFA 因子更换仍未实现，未接入端点继续 503，生产入口仍拒绝启动。

## 依据与当前成果

- [目标基线](docs/specs/targets.md)与[v1.1补充](docs/specs/targets-v1.1.md)：13项业务规则、15项核心场景、9项交付物。
- [开发设计](docs/specs/design.md)与[实施计划](docs/superpowers/plans/2026-11-04-implementation.md)：完整数据、接口、事务及A/E门禁。
- [实施状态](docs/implementation-status.md)：实际通过、未实现与未验证项目。
- [OpenAPI](api/openapi.json)：53项业务操作中37项接入、6项partial、10项planned，逐项标注当前适用范围（契约1.6.0）。
- [管理员链路审查与证据](docs/reviews/admin-access-2026-11-04.md)：R0自审、已修复问题和验证边界。

已写入并验证的部分：定点金额/字段、P-256与浏览器密钥、令牌/TOTP与真实Argon2id、AES-GCM敏感字段加密、MFA管理员会话/CSRF、开户和人工加分、001—013迁移、草稿与预设、水印预览、正式生成与双格式下载、积分流水与操作找回、设备交接与账号管理、React登录/客户管理/编辑器/生成确认/版本下载。

## 不变规则

- 新普通用户可用/冻结积分均0；编辑、预览免费，成功的新版本消费1分，管理员成本0。
- 同版本PDF/PNG共用权益，重复下载免费，0余额仍能领取本人已购买版本。
- 单价/数量手填，IVA固定16%，先舍入小计再算税额，采用定点HALF_UP。
- 免费预览只有服务端像素水印，浏览器不能取得未授权clean文件或私有完整模板。
- 账号绑定一套浏览器密钥和一个有效会话；换机批准指定公钥。纯网页不承诺物理硬件识别。
- 账本与授权以真实数据库为准；不得用前端状态、内存钱包或假用户替代。

## 工具链与安全补丁

当前目标环境已可用：Go1.27.3、PostgreSQL17.11、Node24；React19.3.0、TypeScript5.9.3、Vite8.3.4，前端锁文件已生成。原设计Vite8.3.0存在已公布开发服务漏洞，改用同一8.3系列安全补丁；原始规格保留，实际依赖以go.mod/go.sum和pnpm-lock.yaml为准。

x/crypto锁定官方修复提交 v0.58.1-0.20261009181718-2f1f834330b2，处理未使用OpenPGP包的模块级漏洞标记。Argon2/Blake2b共18个算法文件与审查版本逐字节一致，并重新验证真实64MiB密码哈希。不存在前端混淆防破解承诺。

本机PG17位于.runtime/postgresql-17.11，为官方源码/校验和验证后编译的私有测试工具，没有安装到系统，不作为生产数据库包。其他机器可提供已有PG17的PG_TEST_BIN_DIR。此前DNS/共享内存/监听限制已解除，旧失败日志仅作历史记录。

## 安装与检查

准备Go、Node24、pnpm、PG17工具和Chrome；所有命令在项目根执行。NODE/GO/PNPM可指定已安装程序路径，Make会让pnpm使用选定Node的目录。

```sh
pnpm --dir frontend install --frozen-lockfile --ignore-scripts --registry=https://registry.npmjs.org
make install-tools
make generate
make test-unit vet-go
make build-go build-web
```

install-tools把固定sqlc1.31.1和govulncheck1.9.0安装到项目.runtime/bin；不升级全局工具。generate执行真实sqlc与OpenAPI TypeScript生成。test-unit包括Go、Node设备/API、编辑金额和契约测试，不含真实数据库/浏览器或系统E2E。

项目内测试缓存可通过GOMODCACHE/GOCACHE指向.runtime目录；GOTOOLCHAIN=auto使用go.mod声明的Go1.27.3。Go1.25仅是历史检查环境，当前module已要求1.27，不能继续以1.25替代发布检查。

### 真实数据库

```sh
make test-db
make test-db-integration
# 若使用另一个已安装PG17：
PG_TEST_BIN_DIR=/absolute/path/to/postgresql17/bin make test-db-integration
```

脚本自行创建全新临时集群/数据库，禁用TCP，拒绝外部连接变量；先确认空库失败，再执行001—010全部迁移和63个负例。integration模式以随机marker、数据库名与data_directory验证归属后运行真实Go多连接/提交测试。只清理本次资源，日志在.runtime/db-test.*/。防误连guard不是操作系统沙箱。

人工发分服务见[grant.go](backend/internal/credit/grant.go)：锁内复查管理员近期MFA/会话/因子/epoch/HOLD；钱包、账本、审计和幂等同事务。PG17测试已覆盖20并发同键、冲突/溢出/授权撤销/回滚、提交40001/40P01后原ID重试。该service现已接入受保护管理员HTTP；浏览器链路另由test-admin-flow验证。

### 真实浏览器

```sh
CHROME_EXECUTABLE=/absolute/path/to/chrome make test-browser
CHROME_EXECUTABLE=/absolute/path/to/chrome make test-admin-flow
# 兼容别名：make test-browser-editor 运行同一个真实账号测试
```

使用前端锁定的playwright-core1.62.1和现有Chrome，不下载浏览器，不打开用户既有登录配置。可用PLAYWRIGHT_CORE_PATH显式选择已有同版本工具。第一个测试验证真实IndexedDB/CryptoKey刷新、重启和丢钥拒绝；test-admin-flow先构建前端，再创建专属PG17集群，执行真实CLI、HTTPS Go handler与Chrome中的MFA/开户/加分/重试/重放拒绝/分页/编辑器。browser-form.mjs仅为登录后的编辑器检查模块，不再提供匿名演示入口。普通test-db-integration不带browser tag，单独验证数据库事务。只证明Mac Chrome已测，不能代替Windows/Edge/Safari矩阵或服务端水印/授权验收。

### 开发入口

```sh
make dev-api
make dev-web
```

默认config/development.example.json只提供本地基础入口：healthz存活，readyz和未接业务仍503。Vite的HTTP开发代理用于基础编辑开发，不能替代带Secure Cookie的HTTPS账号联调。账号联调使用下面的同源HTTPS方式或test-admin-flow。当前单据表单仍只在组件内存，关闭页面不保留；没有假余额或本地无水印导出。production.example.json只是无密钥示例，不能直接生产使用。

### 管理员同源 HTTPS 开发

运行前由开发者准备独立 PG17 开发库并按序应用001—010迁移；不要把测试脚本的临时库作为常驻服务使用。将明确host/user/dbname的DSN写入`.runtime/development/postgres.dsn`，权限0600；准备浏览器信任的本地TLS证书和私钥，对应示例配置中的路径。相对路径以配置文件所在目录解析。

```sh
mkdir -p .runtime/development
make build-go build-admin build-web
.runtime/bin/admin keyring-init -output .runtime/development/keyring.json
.runtime/bin/admin init -config config/admin.development.example.json -username owner -output .runtime/development/bootstrap.json
.runtime/bin/api -config config/admin.development.example.json
```

在本机私下读取0600的bootstrap.json并打开一次性链接，设置12–128字符密码、登记验证器、确认TOTP并保存只显示一次的恢复码。密码与链接不放在命令参数、日志、Git或截图中。初始化链接15分钟、启用上下文5分钟；中断且尚未启用时，可用相同受保护CLI的`reissue`子命令和全新output路径撤销旧凭据后重发。已启用管理员的因子全丢失恢复尚未交付。

新增客户钱包为0；设置链接只在首次成功响应显示。丢失响应的同键重试会提示已签发，不返回旧明文；显式客户链接重签及客户激活页面仍待实现，当前客户不能开展业务。加分按钮保留原幂等键和原参数以恢复同一操作；点击“新增一笔操作”才创建新键。写操作要求5分钟内MFA，读取客户列表只要求有效完整MFA会话。列表游标由服务端加密认证并绑定管理员、筛选条件和排序版本。

`test-admin-flow`使用测试专属自签名HTTPS和临时Chrome上下文，仅该测试显式忽略证书信任错误；实际应用配置没有关闭TLS验证。该测试不启动持久开发服务，结束后清理本次数据库和凭据文件。

## 下一步与完成边界

剩余：批量（batch-drafts/estimate/逐行订单）、REPAIR 免费修复与 GC、password/change 与 MFA 因子更换、前端交接/预设管理页面、独立 worker 进程与 Asynq 队列、同步备库与灾备演练、Linux 隔离 renderd、Windows/Edge/Safari 矩阵和 A01—A53/E01—E08 完整验收。`make build/migrate/dev-up/dev-workers/test-e2e/smoke` 命令入口已提供（dev-up/dev-workers 为进程内一体化部署的文档化入口）。

当前单机PG测试不证明同步副本耐久或不确定网络提交已演练。没有Git提交、推送或生产发布；完整目标保持active。
