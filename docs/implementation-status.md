风险摘要：🟥 High 0 · 🟧 Medium 0 · 🟨 Low 0 · 🟦 观察 5 · 🟩 通过 10

# 代码实施状态

更新日期：2026-11-05。摘要只针对本次管理员/设备/草稿/预览/正式生成/预设/流水/交接链路的检查分组；不表示完整系统验收或安全认证。完整目标保持 active，当前不能商用，没有生产发布。

## 已接通的真实链路

受保护 CLI 创建唯一待启用超级管理员及0钱包，向0600新文件写15分钟一次性链接；浏览器设置密码、登记TOTP、确认后才得到正式MFA会话和只显示一次的恢复码。日常密码登录只获得受限预登录，MFA成功后才创建8小时/30分钟空闲超时的唯一有效会话。近期MFA窗口5分钟，TOTP已接受时间步和恢复码不能重放。

React后台新增USER及0可用/0冻结钱包，核验渠道引用加密保存，开户原因进入审计；只发一次设置链接，不创建客户密码、设备绑定或正式会话。人工加分接入原pgx/sqlc事务服务，原键重试只保留一次GRANT。列表支持keyset分页和筛选，认证游标绑定管理员/筛选/排序版本。

客户设备链路：设置链接只允许一次 INITIAL_SETUP 挑战和签名设置密码；BIND 指定公钥转正 ACTIVE 绑定与唯一会话；正常登录由 LOGIN 挑战和 RFC9421 P-256 签名轮换会话；REQUEST 挑战+签名是业务调用的唯一方式，nonce 单次消费。错误公钥、篡改正文、跨账号挑战锁、复制 Cookie 无密钥等攻击在真实 Chrome 中被拒绝。

草稿持久化与免费预览：POST/GET/PATCH/DELETE /documents 与列表、revision 乐观锁、服务端金额重算、水印像素合成（1000px JPEG、30°/32% 水印、TEXT_DOES_NOT_FIT 拒绝）、profile 显式采用闭环，均与此前版本一致。

**正式生成链路（本次新增，M04）**：`POST /documents/{id}/generations` 在一个事务内完成：锁内复查会话/epoch/HOLD → 校验 expected_revision 与确认 manifest（owner/revision/profile/content_hash/有效期）→ COST_CHANGED/CREDITS_INSUFFICIENT/容量不足在冻结前拒绝 → 单 formal lane 规划容量票（320s/行预算、300s 首等上限、1800s 窗口）→ 钱包 available-1/frozen+1 写唯一 RESERVE → 不可变 document_versions（快照=manifest canonical bytes，金额与 CHECK 约束同源）+ 订单 + GENERATE job + outbox + 幂等记录原子提交。同键重放返回原订单实时状态；换新键请求同 revision 返回原版本不重复计费；失败释放后的显式重试走 reserve_attempt+1 并重排容量票。formal worker 按 ticket 计划顺序认领（SKIP LOCKED+租约90s+attempt_no），渲染 CLEAN 2x PNG（3164×4096）与单页 PDF（569.52×737.28pt、FlateDecode 嵌入、无水印），对象经 `artifact_blobs` 条件写（唯一 key 不可覆盖）走 UPLOADING→READY→PUBLISHED，CONSUME 与订单 COMPLETED、job SUCCEEDED、artifacts 引用、FINALIZE_DELIVERY outbox 同事务。finalizer 先做真实 WAL 探针（durability_probes 新 nonce）再插 delivery_receipts（触发器要求 COMPLETED+双格式 PUBLISHED+content_hash 匹配）；下载要求 COMPLETED+回执，锁对象建立 150s 读租约，余额 0 重复下载免费。Sweep 恢复过期租约（RETRY_WAIT/终止释放）与超 deadline 的 RESERVED 订单（唯一 RELEASE）。管理员成本 0 仍占容量票并记审计。

**商品预设与流水（本次新增）**：presets 四端点（创建/列表/改名替换/删除，owner+name 唯一、500 配额、字段走草稿同等校验、金额不存储）；`GET /credits/entries` 认证游标分页展示 GRANT/RESERVE/CONSUME/RELEASE；`POST /operations/lookup` 按 operation+idempotency_key 找回本人幂等操作（GENERATE 附订单实时状态）。管理员"制作单据"标签同样挂接完整草稿工作台（成本0）：签名层按 DeviceContext 区分——管理员会话（无绑定）自动回落 Cookie+CSRF 通道，USER 会话仍强制设备签名，丢钥匙客户不受回落影响（真实浏览器两向验证）。

**指定设备交接与账号管理（本次新增）**：候选浏览器密码预登录后取得 PAIR 挑战并以候选私钥签名→登记 proof_verified 配对（15分钟，5次/分限流）；管理员 `POST /admin/users/{id}/device-reset`（近期 MFA+幂等）核验指定候选与 expected_binding_epoch 后：撤销旧绑定/会话/预登录、auth_version+1、epoch+1、置 RESET_PENDING、签发 10 分钟只允许该公钥的 RESET transfer；新浏览器凭 transfer_id 走 BIND 挑战+指定私钥签名原子领取（CompleteResetBinding），错误公钥/过期/取消/旧 epoch 均失败且不回退为密码抢占。配套：`GET /auth/device/transfers/{id}`（本人最少状态）、`POST .../device-transfers/{transfer_id}/cancel`、`GET /admin/users/{id}/device-binding`（当前绑定+epoch+已证明候选+PENDING 交接，供人工核验完整摘要）、`PATCH /admin/users/{id}/status`（停用即撤销全部会话，历史权益保留）、`POST /admin/users/{id}/password-reset`（一次性 15 分钟链接，不动绑定）、`GET /admin/credit-entries`、`GET /admin/audit-logs`（均 M 会话，首页≤100 条）。

**CFDI 高保真版式（本次新增）**：原 HTML 原型（index-auto-calc）的完整静态版式——FACTURA 玫红页眉/FOLIO、Emisor/Receptor 双栏、元数据区、六列商品表、汇总/大写金额、支付区、左下二维码块与右下三段签章串、页脚——全部数据化进 template.json（runs/rules/字段框/QR 块），渲染器改为模板解释器；任何版式字节变化即新哈希、必须新 profile。迁移 014 撤销 mxn-iva16-raster-v1 并注册 mxn-iva16-cfdi-raster-v2（ACTIVE）。合规边界：二维码为确定性演示图案（不可扫描、不编码任何声明），签章串为固定中性常量，票面加注 DEMOSTRACIÓN · SIN VALOR FISCAL；可填字段改为固定字号+宽度预算（超限 TEXT_DOES_NOT_FIT，不缩字），收件人/单位说明/描述/大写金额支持限行换行，商品节注释组随描述行数下移。修复一处并行集成测试污染（preview 测试硬编码恢复旧 profile id，跨包共享库下随机击穿他人用例，已改为撤销谁恢复谁）。另修复 main.go 启动上下文变量遮蔽导致队列 worker 拿到已取消 context 静默退出的缺陷（演示包预览卡 QUEUED 的根因）。

业务入口配置要求同源HTTPS，Cookie为Secure/HttpOnly/__Host-，写请求要求匹配Origin和绑定当前Cookie的CSRF；严格JSON拒绝多余角色字段等。MFA种子AES-GCM加密，密码真实64MiB Argon2id。全局1200/分与单节点600/分限流。

## 当前工作包

| 工作包 | 实际成果 | 尚未满足 |
| --- | --- | --- |
| W00—W01 | Go1.27.3、PG17.11、Node24、锁文件、sqlc/TS生成、Go构建、React生产build、私有PG/Chrome测试 | 网关签名经真实代理、S3、Linux隔离、完整CI/部署 |
| W02—W03 | 超管CLI/密码/MFA/单会话/CSRF；浏览器持钥、初始授权、签名登录、会话轮换、防重放；PAIR 配对、RESET 指定公钥交接与取消全链路 | password/change、MFA 因子更换与运维恢复、前端交接页面 |
| W04/W11 | 零积分开户、加分HTTP/后台、审计原因/核验引用加密、认证分页；停用/启用、密码重置链接、设备核验视图、全局流水与审计检索 | ADJUST 恢复工单界面、断连未知提交演练与同步备库 |
| W05 | 字段规范化、定点HALF_UP、草稿持久化/乐观锁/列表、西班牙语大写；presets CRUD+配额+认证游标 | 前端预设管理独立页面（当前为编辑器内面板） |
| W06—W07 | 私有模板/字体、不可变raster profile、预览排队/合并/像素水印、profile 显式采用 | — |
| W08—W09 | 正式生成全链路：容量票准入、RESERVE/CONSUME/RELEASE、不可变版本、CLEAN 双格式、durability probe+delivery receipt、读租约下载、0分重下、管理员免积分、失败重试与 Sweep 恢复、operations/lookup | 独立 worker/outbox 进程（当前为 API 进程内循环）、批量 |
| W10 | 迁移与契约基础 | batch-drafts、estimate、逐行订单与结果页 |
| W12 | renderer manifest 随版本记录（image_digest/资产哈希/算法版本/纸张参数） | REPAIR 免费修复端点、GC 清理器 |
| W13—W14 | 本地检查和局部运行证据；`make build/migrate/dev-up/dev-workers/test-e2e/smoke` 命令入口 | 同步备库/灾备、Linux隔离、跨平台浏览器、全部G/A/E验收、生产发布 |

OpenAPI共53业务操作：37项implemented、6项partial、10项planned。planned 为 login/logout/mfa-challenges/device-challenges/password-set/device-bind 的partial之外：batch-drafts、batches/estimate、batches/{id}、mfa/enrollments×2、password/change。契约版本1.6.0-device-transfer-admin。

## 本次验证（V10—V12）

| 组 | 实际执行 | 结果与范围 |
| --- | --- | --- |
| V10 渲染单元 | CleanPNG 尺寸/字节、CleanPDF %PDF头、单页MediaBox 569.52×737.28 | 通过；PDF 为受控嵌入图像，无脚本/注释 |
| V11 生成集成（真实PG17） | 三个场景：付费链路（1分→RESERVE→worker→CONSUME→回执→PDF/PNG→余额0重下→改数量第二版再计1分，账本恰1 RESERVE/1 CONSUME/0 RELEASE）；拒绝矩阵（COST_CHANGED/REVISION_CONFLICT/PREVIOUS_STALE/CREDITS_INSUFFICIENT/跨owner 404，全部零流水）；管理员成本0（零钱包零流水生成+下载） | 通过；含同键重放、换键恢复、幂等参数冲突409 |
| V12 真实浏览器（Chrome 里程碑式） | device-flow 扩展：管理员UI加1分→客户水印预览→确认生成→消费1分→PDF(%PDF-头)/PNG(2x≈数百KB)下载→余额0再次下载→重启恢复；DB 断言 1 GRANT/1 RESERVE/1 CONSUME/0 RELEASE、1 COMPLETED 订单、2 PUBLISHED 对象、1 回执、1 COMPLETED 容量票、钱包0/0 | 通过；下载经签名通道 Blob，无水印文件不落预览缓存 |

另有 presets/流水/交接的服务级编译与既有回归（test-unit 42 ok、db-integration 全部包 ok、契约测试 11 ok）。presets CRUD 与 RESET 交接的独立集成测试尚未单独编写（端点已接通，风险由迁移触发器与既有设备测试部分覆盖）。

## 剩余观察

- O01：password/change、MFA 因子更换（enrollments）与已启用管理员因子全丢失恢复未完成；前端交接/预设管理页面未做（API 已可用）。
- O02：批量（batch-drafts/estimate/batches/逐行结果）未实现；`batches` 相关迁移已就绪。
- O03：REPAIR 免费修复端点与 GC 清理器未实现；renderer_manifests 已随版本记录，修复依赖可追溯。
- O04：worker/finalizer 为 API 进程内循环（300ms轮询+15s Sweep），无独立进程与 Asynq/Redis 队列；单进程内崩溃恢复已由租约+deadline Sweep 覆盖，跨进程部署未验证。
- O05：同步备库/灾备演练、Linux 隔离 renderd、Windows/Edge/Safari 矩阵、A01—A53/E01—E08 完整执行与生产发布未完成。

下一步优先：批量逐行订单 → REPAIR/GC → password/change 与 MFA 因子更换 → 前端交接与预设页面 → 契约 partial 项收口。完整R01—R13、G01—G15、D01—D09范围不缩减。没有Git提交、推送、外部消息或生产数据操作。
