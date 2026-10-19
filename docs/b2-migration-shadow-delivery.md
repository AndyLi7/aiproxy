# R2：operation-bound disabled shadow

本批基线 `6c893399eafb6b2605c8236850e1561dd2aa1b52`，独立分支 `codex/b2-migration-shadow-cas`。只实现停止状态资源的原子创建与恢复，没有 probe/seal/execute 写入口、应用执行 adapter、自动 schema 安装或发布。完整 A/C 仍未完成。

## 创建与恢复

管理路径：`POST /api/channel-migrations/operations/:id/shadow` 和同路径 `GET`。复用 header-only AdminAuth/no-store，拒绝 query auth、重复/未知/大小写不精确字段、null 非可空值、越界 body。请求 body 上限 640 KiB，key 上限 64 KiB，configsJson 上限 256 KiB。schema 以 `MigrationShadowRequest`/`MigrationShadowProjection` 为准：body 为 `{request,key}`；request 包含 version/operationId/source/target。projection 只沿用 channel inventory 字段，不建立供应商参数映射，不恢复火山中国版。

source 冻结 original inventory hash/epoch/revision/storage/from；target 冻结 credentialId/digest/完整白名单投影。runtime features 来自网关当前二进制，不能由客户端指定或视作全副本证明。credentialId 仅审计绑定；网关不能证明应用凭据 envelope/revision，应用仍需自身 lease/source/凭据 CAS。

在同一 control 事务：验证全量原渠道集合（含 deleted）、原 raw model_config 全集、原 revision/epoch/storage/runtime features；核对原 from 身份且 enabled；拒绝同 op 已存在 prepared/barrier/family effect；创建 disabled channel + 唯一 operation/channel resource + revision+1；验证实际落库 target 全投影/原始数字，最后从当前全源验证唯一合法差异。失败一并回滚。模型配置、原报价、未知字段没有回写。

服务器另外冻结原 from/target key 的 SourceFamilyHash，纳入 requestHash/checkpointHash。客户端不能提供/替换 familyHash。重放需原 operation/精确请求摘要、完整有效 receipt、原 epoch/storage、当前 revision >= createdRevision、原 family hash；核验 receipt 精确 shadow identity/projection/disabled/未删除后，才允许从当前全源移除这一个确证成员并规范化原 revision，重算 original source hash。正常 admission 不改变原集合；无关 family 操作增 revision 仍可恢复，相关 family/head 改变拒绝。不是按 name 认领，也不是任意删一行来适配 hash。

GET 无 DML，复用 PG repeatable-read/control SHARE 或 SQLite snapshot；不使用 revision=self 写锁。response source 为 `gateway-migration-shadow-v1`，固定 version/state/reason/operationId/epoch/storageGeneration/currentRevision/receipt；ready receipt 包含原 request/source hash、createdRevision/createdAtMs decimal、from/shadow identity、shadowProjectionHash/checkpointHash。Go hashes 全部 opaque。receipt 不返回 key/raw configs。

**absence 限定**：完整当前存储快照中无该 operation resource，且无其已知 prepared/barrier/family effect，才报告 absent。不是任意删行/备份恢复的历史证明。测试外部删 resource 但保留 shadow：GET 当前快照 absent，原 POST source CAS conflict、channel 数不增加。消费者不能将 absent 当成改 source/重用 operation/删除旧 shadow 的授权；独立恢复水位仍是上线门槛。

## 凭据与兼容

现网关 Channel.Key 是既有 text 存储，没有此批可证明的加密 hook；本批沿用它，不声称加密。key 只在认证请求内存和该 channel 现有字段，resource/requestJSON/log/错误/回执无 key。敏感事务使用丢弃 SQL logger，已知所有源 key 与目标 key 检查防复制进 metadata。白名单配置还复用 inventory typed 安全检查；未知 opaque 配置继续 unsupported，不声称能发现任意伪装秘密。

source 原始配置只读，数字通过 raw JSON 保持。target configs 使用 json.Number，落库后精确 JSON 数字对照；Go identity 沿用实际原 channel serializer/ChannelIdentityOf 语义，不在 TS 猜 hash。目标非配置字段也全部与请求投影对照，触发器/存储侧更改不能静默接受。

普通 BatchInsertChannels 在已装 migration storage 时补 control 串行化，避免 full source CAS 中插入 phantom。现普通 patch/delete/status/config 原 control 写协议保留；owned shadow 不允许普通编辑/启用/删除。无关普通对象写允许，但令此 operation source checkpoint conflict。余额/计数/测试观测不属于 inventory source，不阻断这些工作。

本批 resource 不可变、无 TTL/隐式释放。**safe cancel/consume 还缺**：永久保护 pending shadow 仅是当前阶段边界，不是完整用户可交付生命周期。未来本 op seal 改 family、commit 改 status/pins 时，必须专门消耗阶段 checkpoint；当前 created-phase read 不可冒充这些阶段后置。

基线已有 empty-key 已启用同状态保存缺陷由主控组合树独立修复；本批未修改 `withMigrationChannelStatus` 的该行，也未覆盖掉其锁修复。组合时保留主控已验收修正。

## schema 草案

仅新增独立 protocol v1 与 shadow_resources 两表，后者 operation PK、shadow channel UNIQUE；持久字段见 model。所有新表缺失：旧普通路径兼容、R2 不可用；partial/缺列/错版拒绝相关写。没有接 AutoMigrate/runtime migration。SQLite/PostgreSQL 隔离草案位于 `sql/b2-shadow-resources-{sqlite,postgres}-draft.sql`，仅在一次性库重放。MySQL、DB/LogDB 分库拒绝。

全集合保证依赖所有常规 writer 的 control 协议；实际 BatchInsert/patch/status/delete/config 已覆盖。直接 DBA/旧副本绕过及备份恢复不自称已证明，需要真实全副本围栏/外部恢复水位。schema 安装不提供 issuer。probe 生产与撤销、封口后安全逆向/重取证、联合 drain、安全停用、应用 phase adapter/UI 仍待后续。

## 验证记录

- 相关迁移/准入 race：`go test -race -json ./core/model ./core/controller ./core/router -run '^(TestMigration(Shadow|Inventory|Read)|TestPreparedMigration|TestAdmission|TestChannel.*(Mutation|Routing|Key))' -count=1`，124 根 / 518 个事件，517 pass、1 opt-in 旧 R1 跨语言 server fixture skip；本批真实 HTTP transport 独立实跑。
- 最后补空 target 配置被存储 trigger 注入的拒绝后：`go test -race -json ./core/model -run '^TestMigrationShadow' -count=1`，12 根 / 56 项全部通过，含 MySQL recording 的实际普通四入口 12 子场景。最后补充并非新的全套 517 重跑，不混称。
- 最终真实 PostgreSQL 16：`go test -race -json ./core/model -run '^TestPostgres(MigrationShadow|MigrationReadFacts)' -count=1`，7 根 / 25 项全部通过。含旧 read 无 DML / 锁等待 10 项，R2 control/channel/config 等待、取消、原子回滚、同 op/不同 op 并发、普通新增串行及 SQL 草案。
- 实际 publisher ordinary/video/image/TTS 配置源与原始 quote/contract 完整保持，SQLite/PG 草案隔离重放。
- 最终 `go build ./core/...` 通过；15 文件 security-scan script + 人工完整 diff 通过；`git diff --check` 通过。MySQL 仅 GORM/SQL recording，不称实例实测。

复跑 PG：`python3 /private/tmp/b2-shadow-final/reproduce-pg.py <精确网关archive目录> <结果JSONL路径>`。实际 HTTP：`go test -race ./core/model -run '^TestMigrationShadowHTTPTransportLostResponse$' -count=1`，自行创建/关闭 loopback Gin server 和测试 SQLite，不需真实账户。隔离证据目录 `/private/tmp/b2-shadow-final/`；真实 PG harness 只使用缓存 postgres:16-alpine、loopback、tmpfs，随机凭据仅进程内存，结束删除容器；不访问线上 DB/provider/bucket，无付费生成。
