风险摘要：🟥 High 0 · 🟧 Medium 0 · 🟨 Low 0 · 🟦 观察 3 · 🟩 通过 4

# 管理员链路实现审查与验证

日期：2026-11-04。级别：R0-Strict，实现者自审，未新增独立审查者。摘要只计本次范围内开放发现与实际验证分组；不代表完整系统、独立安全审计、生产验收或不存在未知漏洞。

## 范围与反例

检查CLI初始化/重签、标准Argon2id/TOTP/Token的调用边界、AES-GCM/AAD、MFA预登录与正式会话、CSRF/Origin、账号开户、列表游标、人工加分接线及React敏感状态。直接依赖包括pgx/sqlc、现有credit授权事务、strictjson、配置/HTTPS与001—010数据库约束。本次未变更依赖版本，未重复漏洞扫描。

重点反例为密码单因素访问、重放TOTP/恢复码/开户设置链接、匿名或伪造身份加分、同键重复加分、会话撤销/因子版本/近期窗口、跨筛选游标和加密上下文互换。审查没有授权生产请求、外部写入或全球配置变更。

## 发现及修复

| 级别 | 位置 | 证据/影响 | 修复与复验 |
| --- | --- | --- | --- |
| Medium，已关闭 | account/service.go InitializeAdministrator | CountAdministrators的局部e遮蔽外层ErrNoRows，首次CLI一直回滚，管理员无法初始化；私有PG i9K9IN真实失败 | 对外层e赋值；同一真实CLI/PG/浏览器链路已GREEN |
| Medium，已关闭 | account/service.go CreateUser、account.sql AccountAudit | reason只参与请求哈希，未保存可审计的开户原因 | 同事务写reason-only元数据；DB精确断言原因且无设置链接/核验引用 |
| Medium，已关闭 | account/service.go ManagedUsers、httpapi/admin.go users | 客户超过20条报错，且只读列表错误要求近期MFA | keyset分页+认证游标，完整MFA读/近期MFA写分开；真实分页、篡改/换筛选拒绝和6分钟MF边界均通过 |
| Medium，已关闭 | account/service.go MFAChallenge/VerifyMFA | step-up事务不能依赖事务外会话快照 | 用户锁后重新锁读会话，锁内校验当前账号/因子/会话和DB时间；API返回DB实际验证时间，审计同事务；浏览器step-up通过 |
| Low，已关闭 | frontend EditorForm、App | 管理员编辑器显示每新版本1分，与管理员免积分要求不符 | 根据管理员上下文显示免积分；真实Chrome检查。正式计费免积分尚待生成服务实现 |

没有发现本次检查范围内仍开放的High/Medium；这不是完整产品可安全交付的结论。旧R1原语/credit审查不覆盖此次新增service/HTTP/UI。

## 实际四组验证

1. **Go与边界**：真实account/config/httpapi单元测试；密文篡改和跨用户/因子/版本/用途拒绝、随机nonce、CSRF上下文绑定、0600文件/不可覆盖、ASCII用户名、配置路径；全Go unit/vet与API/admin构建。
2. **PG回归**：PG17.11新专属Unix-socket集群，空库42P01 RED、001—010迁移、63个既有SQL负例和完整credit集成通过。日志`.runtime/admin-db-regression-latest.log`，集群记录`db-test.aLiLPy`。未接触既有数据库。
3. **真实前后端**：`CHROME_EXECUTABLE=... make test-admin-flow`实际执行CLI、Go HTTPS handler、PG17、Chrome157.0.8103.38和React dist。覆盖MFA启用/登录/退出/step-up、密码阶段拒绝、TOTP和恢复码重放拒绝、0积分开户、同键不重发旧明文链接、加5分/原操作重试一次、匿名/CSRF/角色字段篡改拒绝、认证分页、登录后编辑器及375px不溢出。最终DB确认余额5、1条GRANT、无USER密码/设备绑定/正式会话、核验引用加密、开户原因审计。完整MFA超过5分钟仍可读取管理元数据，开户/加分均拒绝。最新日志`.runtime/admin-flow-latest.log`，集群记录`db-test.5hruFo`。测试使用自签名证书并仅在测试Chrome上下文忽略信任错误；不代表实际证书部署已验证。
4. **前端与契约**：完整TS/Vite生产构建、33个Node用例（20设备/API、2金额、11契约/生成）、生成类型一致性；OpenAPI8项implemented/4项partial/41项planned。日志`.runtime/admin-checks-latest.log`，最终Go复验另见`.runtime/admin-go-final.log`。截图为编辑界面，未渲染正式票面。

原始design/targets/targets-v1.1三份规格副本与outputs原文逐字节一致。Go/Node/PG/Chrome证据仅覆盖本机环境；本次没有完整浏览器矩阵或真实生产部署。

## 残余观察与交付限制

- O01：普通客户激活、Go设备签名和单次nonce、指定换机、其他账号管理及凭据丢失恢复仍未完成。当前USER不能开展业务，不能把开户成功解释成客户可用。
- O02：草稿持久化、私有票面/字体、服务端像素水印、正式版本/扣1分/双格式无水印文件及免费重下未完成。
- O03：任务/容量/对象/批量/归档、Linux隔离、同步备库/灾备和跨平台验收仍未完成；production拒绝启动，ready仍false。单机PG不能证明RPO或未知网络提交恢复。

本次只形成可继续开发的管理员链路检查点，完整目标保持active。没有Git提交、推送、外部消息或发布。所有秘密测试文件由私有runner在退出时清理，未写入报告或截图。

## 恢复边界

当前工程仍全部未跟踪，不能用git reset宣称可恢复。回退时只针对本次列明的账号/HTTP/UI/契约/配置文件及010新迁移，由保存的基线或逐文件比对恢复，不覆盖此前credit/字段/设备成果。010只增加节流表；若已有持久业务库，禁止把测试清库逻辑用于回退，需另行制定保留账本/账号的前向修复。原HTML、原规格、其他数据库和全局配置未修改。

## 审查源码指纹

下列摘要记录本次检查点，不是签名或独立认证；后续源码变更使其失效。

```text
73f11da1389937e0810ccafa6485baffc464758bdb3208f88bf557a87f0730ab  backend/internal/account/service.go
82e7aff946c51c02859afdbb8c3bc8579fe8f88b27422c5dde715c403bffeb42  backend/internal/account/keys.go
2c2e626fb192a1cc4bda986ba2f44b05fd82d697a873f6acfb2705c3bfa52355  backend/internal/account/pool.go
e7d5dc14c13c20842c6febcb7a3406bb33908ffe8770c7dabe19d09b6b5c9855  backend/internal/httpapi/admin.go
2b4d52e34b28b48b80cd1f6b611a1944815c080c04d4c0c5b86c7646fad800cc  backend/internal/httpapi/server.go
32c5d92568650a5ac155d982268949a8d0ed8124707b0f473d04536dc5264721  backend/internal/credit/grant.go
b789e0d0be4e4d5194023feffbb5a950110bbd92899286263d84a025266018c3  backend/cmd/admin/main.go
b59fa892637d6fe83767966e7bc91115c3ee8ec900f1dd81cf784a8163d1a4d2  backend/cmd/api/main.go
f56d7fc0adefc596b67d271cd6b86c5e697d50ef98c00bbf49383afed4900295  backend/db/queries/account.sql
f13312e7527ad183fbc6e9fa87fee04eb08dbc5a8d25c0826cc8c63d45a86640  backend/db/migrations/010_auth_limits.sql
2cdeed04def6011588cd991b15df3e959dec26dd3cae0babbd59011ac5fe2cf7  frontend/src/features/auth/AdminWorkspace.tsx
4671870ebd39b9c7f2b07bc24aa67fb6bd775102fc06f940e6f937a3e2f4b439  frontend/src/features/editor/EditorForm.tsx
670bb6b2bf9b80b07319b3431ee67295827a5c6b6a2776cd017789cd2e5aab49  backend/internal/httpapi/admin_flow_integration_test.go
9c15dd4af79e1a7df71203b1b8393952e5060e64f45461aa6373633990419476  frontend/tests/admin/browser-admin.mjs
```
