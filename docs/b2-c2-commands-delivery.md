# C2-COMMANDS-1：持久准备、封口及原子启用/pin 命令

基线：`5606008ad6d719a723ede0d2680accd31b2501da`。独立工作区 `b2-gateway-c2-commands`；不合入其他正在开发的 poll 批次。仅本地命令层，**不是完整 A/C2，也未启用实际迁移**。

## 本批接口与权限边界

本批新 prepared 命令仅支持正向迁移，`Seal.RevertOf` 必须为空，非空显式拒绝；旧 `Seal` 的最新退休记录/精确反向身份/`revertOf` 规则保持。实际早期逆向可能以仍 enabled 的旧渠道为目标，不能先普通 status 停用来伪装 shadow，须后续独立安全逆向命令。测试覆盖 live 目标及仅由 fixture 预置 disabled 的拒绝对照，后者不是 safe-disable 验收；晚到旧 commit 在 current-head 更换后拒绝。

- `ReadMigrationSnapshot(ctx, from, to)`：锁定 control 和物理身份，返回 gateway `Epoch/Revision/StorageGeneration/ConfigModels/PinHash/ProjectionHash/FamilyHash`。ConfigModels 是全部 `model_configs` 行；PinHash 包含全表每行全部原始持久列（包括未知列、价格、时间），不是应用 TS `migrationPinSetHash`。ProjectionHash 绑定 shadow 的原始 models/model_mapping/sets/configs/priority/backup_only；FamilyHash 绑定 from/to 两个当前 family 事实。客户端不计算/拼造这些授权哈希。
- `PrepareMigration(ctx, request)`：request 冻结 Seal(operation/epoch/预期 revision/from/to/revertOf)、from/to credential ID、上述三个哈希。服务端 `LoadMigrationProbe` 必须验证真实受限 probe 执行/账单、准确操作和身份、证据撤销状态；返回不可变 EvidenceHash、完整 RequestHash、StorageGeneration、IssuedAtMs/ValidUntilMs。准备持久化完整 ProbeFactHash，不能用同 EvidenceHash 更换事实或延长旧审阅的有效期。
- `SealPrepared(ctx, request)`：旧渠道 enabled、新 shadow disabled，受保护的 family 交换和 prepared→sealed 同事务；旧的 `Seal` 仍保持双方 enabled 的既有语义。普通无关 admission 可以推进全局 revision，准备只要求原 family hash 未变；最终 SealRevision 来自锁内当前 control+1，不能用请求里的 ExpectedRevision+1 推算。
- `CommitPreparedPins(ctx, request, sealRevision)`：**一个事务**校验当前 operation/family head、完整 before-hash、原身份和投影，并执行 shadow enable、全部 pin 修改、committed 回执。没有独立 enable 命令。业务写、最终 control CAS 之后重新读 DB 时间和精确 deployment/probe 事实；任一过期/错误全回滚。
- `ReadPreparedMigration(ctx, operationID)`：prepared 核对原 family 和 disabled/before 集合；sealed 核对当前 head 和 disabled/before；committed 核对当前 head、完整 enabled/after 集合及同 operation 持久回执。返回的是当前可回读事实，历史 barrier 或相同外形不能冒充成功。abandoned 仅是无授权的审计行。
- `AbandonPrepared(ctx, operationID, requestHash)`：仅 prepared、精确原身份、shadow disabled、无本操作 barrier/family grant/commit 痕迹才允许 abandoned；相同重复幂等。保留 shadow、probe/use、审计和未知工作；不删除、不退款、不停用、不撤销 key、不证明排空。sealed/committed 拒绝；旧 operation 的 Prepare/Seal/Commit 全拒绝，新操作必须重新获取证据。

没有新增 HTTP/controller 入口，也没有接受 `passed=true` 的路径。`LoadDeployment` 与 `LoadMigrationProbe` 都只来自服务端；缺真实 issuer、全副本/执行者围栏、独立恢复水位或本地 probe 事实时拒绝迁移，不能把这种缺失算作完整交付。

## Probe 持久撤销与锁序

新增独立 `channel_migration_probe_records(evidence_hash, fact_hash, revoked)`。可信 updater 必须先安装经过验证的完整事实；命令不替浏览器或普通请求自注册证据。安装/撤销生产者本批尚未实现；未来必须不可覆盖既有事实、不可复活 revoked 行。测试只使用明确标记 synthetic 的隔离 fixture。

命令锁顺序为 control → prepared → channel → deployment/probe 事实，模型配置集合的所有正常插入/保存/删除也先取 control。独立 probe updater 只能更新事实行，不可反向取得 control/channel 锁。命令等待 channel 时已提交的 probe 撤销必须被锁后检查看见；命令已持有 probe 行锁后，撤销排队并在线性化顺序上发生在命令之后。最后写后仍复核时间、事实 hash 和撤销，不能靠旧 snapshot 的有效期猜测。

## Pin 改写与兼容

服务器不接任意 configs，只从 DB 原始 JSON 改以下字段：

- `config.upstream_prepayment_v1.routes[i].channelId / credentialScope`；仅该 route 原有 `keyFingerprint` 时验证并改写，原来缺失则保持缺失。
- `config.x_token_platform_native_result_v1.channelId / credentialScope / keyFingerprint`，必须与唯一对应 route 的 endpoint/身份精确匹配。

除此以外的 JSON 值、未知嵌套内容和数值词法通过 RawMessage 保留；超出 JS 安全整数甚至 int64 的未知数字不经过 float64。JSON 对象排序/空白可能规范化，但语义和未知数值精度不变；重复键拒绝。原价、quoteVersion、计量字段、加密版本及 plugin 等其他持久列不重写，也不执行普通 ModelConfig BeforeSave 的规格标准化。任务中的已冻结报价/合同不修改。

准备期间，相关 channel patch/status/delete 及相关 model config 保存/删除/新 pin 均拒绝；无关对象正常操作继续，但无关配置变动也会使全表审阅哈希失效，需 abandon 后重新审阅，不静默扩大授权。关闭开关不删除现有屏障。未安装本扩展时保留既有路径；partial、缺列、错版本拒绝新命令及涉及该扩展的写。SQLite/PostgreSQL 支持隔离验证；分库和未证明 MySQL 拒绝迁移，不以本批推定它们已支持。

## 应用端口衔接

下一批应用必须合并 `enableShadow`/`savePins` 为单个 CommitPreparedPins；旧 journal 的 enabling/switching_pins 只能重读 prepared/当前 head/原子回执后决定恢复。enabled+before 不代表已成功，enabled+after 也不足以证明该 operation 原子提交。请求必须另外冻结 gateway opaque 哈希和完整配置成员；不能复用 TS 归一化哈希替代。既有 confirmSwitch 仍独立，`CommittedAtMs` 不是 drain 起点，也不证明缓存/全副本已切换。

## 存储、发布和后续

`core/migrations/channel-migration-prepared-v1/{sqlite,postgres}.sql` 仅供审核及空白隔离库演练，依赖 admission-v1，不 backfill、不激活、不安装 probe。两种方言均重复执行验证；没有运行线上迁移。

还缺：真实受限 probe/可信 updater/issuer 适配、HTTP/API 和实际应用 adapter、switch/cache 证据、完整 executor/poll/billing closure、safe-disable/drain/revert 编排。use 仍 open/unproven，不自动清理旧身份或共享 key；fal 保持薄适配。新增发布仍按本地→PR审核→精确 staging 验收→GitHub main→用户新的明确生产确认，生产 SQL 另审；本批无推送/部署/真实 DB/真实 bucket/供应商或付费调用。

## 验证

- 既有 admission/modelconfig/patch 加本批命令 `-race`：109 根、418 项通过，无 skip/fail。此运行在最后 epoch/forward-only 收口前；收口后重跑所有本批命令 `-race`：11 根、34 项通过，无 skip/fail，不把两次结果相加冒称一次全量。
- 最终精确源码真实临时 PostgreSQL16：3 根、29 项通过，无 skip/fail。覆盖 prepare/seal/commit 的 live、deployment/probe 自然过期、两种独立撤销、投影变化、取消，最终回执/最终 control 写后过期回滚、普通新增行的持久串行化及 SQL 重放。MySQL 没有实例实测。
- 最终 `go build ./core/...` 成功；security-scan 12 文件 clean，人工复核参数化查询、证据来源、状态/钱款/共享 key 边界通过。
- 证据：`/private/tmp/b2-c2-commands-final-rlh6xH/`。测试容器 `b2-c2-commands-pg` 使用 localhost 随机端口和 tmpfs，已删除；无另建网络，无留存临时数据库凭据。
