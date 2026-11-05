# 单据系统 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 完整实现目标文档 R01—R13、G01—G15 与 D01—D09，依实际证据验证后才结束目标。

**Architecture:** Go 业务单体、PostgreSQL 事务与 outbox、React/Vite SPA、隔离 renderd、私有不可变对象存储。先验证标准协议和核心边界，再接真实账号、积分、单据与下载；本地模型、SQL 测试及单元测试不代替端到端验收。

**Tech Stack:** 当前 Go1.27.3、PostgreSQL17.11、React19.3、Vite8.3.4、Node24；早期Go1.25/PG16仅是历史兼容证据。Vite8.3.0的已公布漏洞以同系列补丁修复；module要求Go1.27。

**Spec:** `docs/specs/targets.md`、`docs/specs/targets-v1.1.md`、`docs/specs/design.md`，原始文档副本内容保持不变。

## Global Constraints

- 新用户 available=0、frozen=0；管理员生成成本 0。
- USER 成功不可变新版本成本 1；PDF、PNG 及同版本重下共用权益，余额 0 可重下。
- 单价最多 2 位、数量最多 6 位小数；固定 IVA 16%，整数定点与 HALF_UP，不用 float64 财务计算。
- 免费预览只有像素水印，正式文件只通过完整鉴权和已完成交付领取。
- 一账号一浏览器公钥、一 ACTIVE 会话；换机授权指定公钥，密码／Cookie 不单独授予业务权。
- 所有业务状态以真实 PostgreSQL 为准，不用 SQLite 或内存钱包替代目标实现。
- 正式文件不可覆盖，旧租约不能发布，GC 与发布及读租约互斥。
- 授权、秘密、用户数据不进入前端 public、日志或不受保护端点。
- 只新增本项目和隔离测试数据；不改既有文档、HTML、其他数据库或全局配置；不自动提交、推送或发布。
- 若目标工具链或依赖网络不可用，应记录实际失败；不得降级目标、使用假数据或跳过检查标为完成。当前本地Go1.27/PG17/Node24已可用。

## Review Focus

| 反例 | 应有结果 | 覆盖任务 |
| --- | --- | --- |
| 大数量、边界税额、科学计数法或 Unicode 数字 | 精确计算或明确拒绝，无溢出 | Task 1 |
| 不合法曲线、错误公钥、DER 误编码、签名后正文变化 | 验证失败，原用户不被踢下线 | Task 3 |
| 并发消费、重复加分、Commit 不确定 | 一次状态变更，原键恢复 | Task 2、5 |
| 已知密码者抢占换机，旧 epoch 或旧预登录 | 只有批准公钥成功，旧上下文失效 | Task 3、5 |
| 迟到 PUT、清理竞争、同步确认前读取 | 原已购对象不变，未确认文件不交付 | Task 6 |

## 任务与执行顺序

### Task 1:金额与输入领域代码（W05 的独立前置）

**Files:** `backend/internal/money/money.go`、`money_test.go`。
**Interfaces:** `Calculate(unitPrice, quantity string) (Totals, error)`；Totals 返回 Subtotal、Tax、Total 两位小数字符串及供快照使用的整数值。

- [ ] 编写真实单元测试，先运行并确认缺实现失败。固定例：100×5 -> 500.00/80.00/580.00，86.21×5 -> 431.05/68.97/500.02，0.10×3 -> 0.30/0.05/0.35，0.05×0.1 -> 0.01/0.00/0.01；负数、指数、非 ASCII、超限拒绝。
- [ ] 用 math/big.Int 实现规范化解析、定点乘法和范围检查，测试补齐零值与最大输入。
- [ ] 执行 `GOTOOLCHAIN=local GOPROXY=off go test ./internal/money`，明确当前 Go 1.25 本地证据范围。

### Task 2:完整有序迁移与真实 SQL 约束（W01—W04 数据基础）

**Files:** `backend/db/migrations/001_identity.sql` 至 `009_archive_audit.sql`、`backend/db/tests/constraints.sql`、`scripts/test-db.sh`。
**Interfaces:** 所有表／列对应 design 第 5、15 节；migration 路径固定，后续 Go repository 使用相同列名。

- [ ] 先编写 psql 行为测试，在新空测试库运行并看到表／约束不存在。
- [ ] 按依赖顺序创建全部表、外键、CHECK、条件唯一索引及不可变账本保护；后补环形外键。
- [ ] 测试负余额、ACTIVE 设备／会话冲突、重复操作／版本／订单、REPAIR 不付费、对象类型与归属、manifest／账本不可改。
- [ ] 在私有 Unix socket 测试集群运行完整 migration 与 SQL 行为测试。PG16 的证据明确标为兼容性检查，PG17 实际门禁保留。

### Task 3:设备密码学与浏览器持钥基础（W00/W03）

**Files:** `backend/internal/devicekey/`、`frontend/src/shared/device/`、`frontend/tests/device/`。
**Interfaces:** 公钥只接受 EC/P-256/x/y；RFC7638 thumbprint；64 字节 P-256 r||s；前端 `getOrCreateDeviceKey`、`signBytes` 与固定 profile 构造，私钥不导出。

- [ ] 先写并运行 Go 与 Node/WebCrypto 测试：有效证明、错误消息／公钥、非法坐标、私钥字段、错误签名长度及固定摘要样本。
- [ ] 实现标准原语与 IndexedDB 存储模块，接口只返回公钥／CryptoKey，不输出私钥字符串。
- [ ] 运行本地测试；真实浏览器重启与经网关 RFC9421/Go httpsig 互通是后续 E01/E02 门禁，不能用原语测试替代。

### Task 4:工程、HTTP 边界与契约（W01）

**Files:** `backend/internal/config/`、`backend/internal/httpapi/`、`backend/cmd/api/`、`frontend/package.json`、`frontend/vite.config.ts`、`api/openapi.json`、`Makefile`、`README.md`。
**Interfaces:** `/healthz` liveness；`/readyz` 返回实际依赖就绪；所有 `/api/v1` 业务默认拒绝，未来 handler 通过显式认证级别注册。

- [ ] 写配置与 HTTP 行为测试，先证明不合法生产 Origin、缺 Secret、未就绪及未知路由行为。
- [ ] 实现最小可构建入口、请求限制、安全错误与服务生命周期；不为演示加入假账号／假钱包。
- [ ] 前端建立目标版本配置与真实 apiClient/设备模块；网络无法安装时报告缺口，不把 mock 画面当实际功能。
- [ ] 按 design 第 8 节提交 OpenAPI 契约，明确已实现路径与未接入路径，生成客户端类型作为后续接口输入。

### Task 5:真实账号、权限与账本服务（W02—W05/W11）

**Files:** `backend/internal/account/`、`device/`、`credit/`、`document/`、`platform/postgres/`；相应前端 auth/admin/editor 页面。
**Interfaces:** 按目标设计 service 签名与 OpenAPI；所有持久化使用 pgx/v5、同一 Tx/sqlc WithTx。

- [ ] 先对真实 PG 编写开户 0 分、MFA 阶段隔离、指定公钥绑定、旧 epoch、并发 GRANT/RESERVE/RELEASE、跨用户资源与重试行为测试。
- [ ] 依次实现 Argon2id、单次 setup/bootstrap、TOTP/恢复码、HttpOnly Cookie/CSRF、正式请求签名、指定设备交接和实际钱包。
- [ ] 接入表单、列表、预设与计算；逐项用 A01—A16、A23、A27—A39、A41—A44、A48、A51 验证真实结果。

### Task 6:像素预览、付费文件与恢复（W06—W09/W12）

**Files:** `backend/internal/render/`、`generation/`、`artifact/`、`cmd/renderd/`、`cmd/worker/`、`cmd/outbox/`、私有模板、前端 preview/history/download。
**Interfaces:** manifest 固定内容；任务与容量票；不可变对象；delivery receipt；下载读租约；REPAIR 无付费 order。

- [ ] 先写像素水印、预览／正式隔离、旧 lease、重复消息、PUT/GC 竞争、同步确认前无文件和历史免费修复的真实边界测试。
- [ ] 实现隔离 renderd、服务端合成、任务／outbox、容量准入、原子结算、耐久探针及回执、代理下载。
- [ ] 打通 G02—G09；覆盖 A17—A22、A26、A45、A47、A49、A50、A52 及 E03—E08。

### Task 7:批量、资源治理与完整发布证据（W10/W13/W14）

**Files:** 批量 service/UI、GC/配额、`deploy/`、CI、操作与恢复手册、integration/e2e 测试。

- [ ] 先验证 100 行输入但容量不足整批不预留、5 行允许预算、逐行结算、异常恢复和资源总量。
- [ ] 实现 batch-drafts/estimate/batches、运营审计、历史归档、受控回收、生产配置与恢复工单。
- [ ] 执行完整 A01—A39/A41—A53，记录 A40 边界，完成 E01—E08，交付 D01—D09。无真实证据项保持未完成，不提前标记目标达成。

## 当前环境与执行裁定

Ruling: 当前环境的 Go1.25/PG16 仅用于离线兼容测试，生产目标仍 Go1.27/PG17 — 现有编译器可验证独立逻辑且不改变发布目标 — 代价为目标版本构建／集成仍必须补验。

Ruling: 目录从零建立独立 Git 仓库分支 codex/implementation，不自动 commit/push — 无现有应用历史或 main 修改，用户未授权提交 — 代价为阶段审查用未提交文件清单和哈希包而不是 commit diff。

Ruling: 下载不可用时不改用 SQLite、内存账本或降低 React/Vite 目标版本 — 保持既定最终目标与可验证范围 — 代价为外部依赖和完整运行有待网络恢复。

计划与 ledger 持续更新；第一批可验证模块完成不等于整个目标完成。

## 环境裁定更新（2026-11-04）

权限更新后网络/IPC已真实可用。Go1.27.3与完整前端目标依赖已安装到项目测试缓存，PG17.11官方源码SHA256验证并在.runtime编译。下列历史下载/兼容裁定不再作为当前阻塞；不删除历史证据。

Ruling: module使用Go1.27，前端Vite升级同系列8.3.4安全补丁，crypto使用官方漏洞修复提交 — 实际依赖扫描发现风险，且目标环境现可用 — 代价为历史1.25命令不再适用，需保留新版锁文件和验证记录。

Task2迁移在PG17实际完成；Task5已接受保护CLI初始化、管理员MFA/会话/CSRF、零积分开户和加分的真实浏览器链路，仍为partial。普通用户设备签名、换机、其他账号管理、草稿、预览、正式产物与发布门禁不缩减。

## 草稿持久化进展（2026-11-04）

Task 5 的草稿部分已接入真实PG与浏览器：`document` 服务、`011_render_profile.sql`、五个 `/documents` 操作、客户工作台列表与保存。验证证据：document单元+集成测试、test-device-flow（创建/两版保存/刷新回填/删除）、test-admin-flow回归、完整前端build与契约测试。

Ruling: 唯一ACTIVE profile的manifest只声明纸张/时区/固定算法/字号下限，`assets`与`renderer_protocol`为null — 模板与字体字节尚未提供，不能声称可渲染 — 代价为预览/正式生成在资产就绪并新增不可变profile前继续拒绝。

Ruling: 单节点HTTP上限120→600/分 — 一次业务动作需挑战+请求两次调用，真实编辑节奏下120会误伤 — 代价为单节点突发容忍度提高，全局1200/分仍限制总量。

Task 5其余项（预设、换机交接、其他账号操作）与Task 6的版本/计费/下载保持未完成。

## 预览链路进展（2026-11-04）

Task 6 的免费预览部分已接通：`012_preview.sql` 注册唯一 ACTIVE profile `mxn-iva16-raster-v1`（旧占位 profile 置 REVOKED），`backend/internal/render/` 提供模板/字体资产加载、定点金额西语大写、固定画布排版、像素水印与有界 JPEG，`backend/internal/preview/` 提供排队合并、配额、worker `RunOnce` 与 `Sweep`，HTTP 新增 `POST /documents/{id}/previews`、`GET /previews/{id}`、`GET /previews/{id}/image`、`GET /render-profiles`、`POST /documents/{id}/render-profile`，前端在保存后申请预览、轮询、以 blob 显示。验证证据：render 单元测试（含水印像素差与确定性同字节）、preview 集成测试 10 例（合并/替代/撤销/过期退役/限流/清单不可变/删草稿退役/profile采用）、test-device-flow 新增 `server-watermarked preview JPEG` 检查（canvas 采样证明水印合入像素）、test-admin-flow 回归、前端 build 与 22 项 node 测试、契约测试 21 项 implemented。

Ruling: 预览渲染用 `go:embed` 编入二进制的纯 Go 光栅化（`golang.org/x/image`），不引入 chromedp 或 renderd 容器 — 当前环境无可用的容器隔离运行时，且资产哈希能随二进制一起被 manifest 校验 — 代价为版式表达能力受模板 JSON 限制，文字溢出只能失败并需注册新 profile 换版式。

Ruling: 资产字节变化必须注册新 profile，禁止编辑既有 profile 的 manifest — 已生成的 manifest 是历史票据的构成部分 — 代价为每次换字体/模板都要新增迁移与 profile 行。

Ruling: 撤销 profile 后只能通过显式 `POST /documents/{id}/render-profile` 迁移草稿，服务端不静默改写 — 票面在用户不知情下变化等于伪造 — 代价为前端需处理 `PROFILE_REVOKED` 并在 revision 递增后重试一次。

Ruling: `preview_manifests` 允许 DELETE、禁止 UPDATE，被 `document_versions` 引用时拒绝删除 — 免费预览不产生历史，但一旦成为正式版本依据就不可回收 — 代价为删草稿必须按 preview_objects → preview_requests → preview_manifests 顺序退役。

Ruling: profile manifest 的每个可执行声明都必须被编译期代码校验（资产哈希、renderer_protocol、watermark 六项参数），并由测试直接读取 `012_preview.sql` 的 manifest 字面量比对 — 只比对资产会让 manifest 声明与实际渲染脱节 — 代价为每次改水印参数都要同时改 Go 常量、迁移与契约版本，否则测试与运行期都拒绝。

Task 6 剩余：正式版本生成（CLEAN PDF/PNG）、积分 RESERVE/CONSUME 结算、下载与同版本免费重下、REPAIR 免费修复。
