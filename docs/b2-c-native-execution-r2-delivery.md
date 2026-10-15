# NATIVE-EXEC-R2：区分历史 epoch 与当前执行者资格

父提交 `17e2bf433d7958f0ed6b8b57048006c5463cba92` 已通过 R1 原证明源故障反例，但主控复现新的兼容回归：真实 Activate 消费 sequence 2 的有效 proof 后，历史调用行仍被要求属于当前 epoch，导致旧任务重放、轮询、结算、恢复与交付无法继续。本批独立修复 R2，不声明完整 C 或安全停用完成。

## 历史事实与当前资格

native 执行协议和记录版本升为 2，增加 ExecutorProofJSON。内容仅为已验证的部署事实：原 proof ID / sequence、epoch / storage / watermark、membership / fence、执行者列表及原签发 / 有效期限。没有渠道凭据或私有 nonce；数据只来自服务端 VerifiedDeploymentProof，不新增外部输入接口。该字段参与原调用的私有授权摘要，返回时不能换成另一份历史事实。

每条历史行仍核对同一 use 完整摘要、任务摘要、存储世代、调用类型和状态时间；其 epoch / service / instance / build 则与该行原快照核对。原快照再通过现存 ChannelAdmissionProof 的原 ID、ConsumedSequence、FactHash 核验，成员列表摘要和成员本身均检查。未知版本、无历史快照、JSON 损坏、原 epoch / member / storage / sequence / proof ID 或指纹不匹配均拒绝；不忽略任何同 use 的冲突行。

历史 proof 的正常过期不是历史内容失效：不会要求旧 proof 今天仍在有效期。明确 Revoked、历史 proof 行丢失或指纹被改属于不可信事实，保留旧记录并拒绝，不降级成来源临时不可用。这一区分也适用于旧调用自己的返回记录；自然过期和合法换 epoch 后，只有原私有 handle 能报告该旧调用返回。

当前执行者只能复用同一当前 epoch、同一服务端执行身份的可信记录。旧 epoch 行即使 process 标签重复，也只算历史，不能作为新 epoch 的来源故障兼容依据。新 epoch / 新成员首次建立调用记录，仍须当前完整有效 proof、锁后的成员 / 存储 / 期限 / 撤销检查和最终 CAS。两次合法 Activate 后，可追加第三代记录，第一代 active / returned 行不被改写或清空。

缺少历史行时，只有同 epoch 的原冻结兼容路径可以在来源临时不可用时保持无覆盖继续；跨 epoch 且没有新有效证明不能借此绕过校验。真正未安装 native 扩展的旧任务保留既有路径，不额外读取证明。原 owner / claim token / until 下界、dispatch owner、数据库锁后和写后租约校验、完整冻结身份及钱包幂等流程保持。

## 协议与最小安装边界

[SQLite v2 草案](../core/migrations/channel-native-execution-v2/sqlite.sql)与 [PostgreSQL v2 草案](../core/migrations/channel-native-execution-v2/postgres.sql)依赖已有 channel-execution-v1，原 native 协议键 id=2 的版本为 2，新增列 executor_proof_json。只用于尚未安装 native 扩展的环境的首次安装审阅；没有运行时 AutoMigrate，也没有对现有 / 生产库执行。

此前 native v1 扩展从未验收或部署。旧 SQL 保留作历史，v2 草案不修改既有 v1 表、不把协议行强行改为 2，更不会为未知旧行填入今天的 proof。对 v1 或缺列 / 部分安装继续拒绝。SQLite / PostgreSQL 测试确认：先安装旧 v1，再执行 fresh-v2 草案，不会自动升级，原协议仍为 1，缺失历史列仍缺失，use 保持 open。

若某隔离实验环境已有 native v1 行，应保留原记录与未知状态，暂停把它当 v2 运行；必须另行审阅迁移与历史证据方案。本批没有安全自动补证方案，也没有删除旧表、重置记录或默认补造历史。该限制只针对未发布的 native v1 扩展，不能套到已部署但根本没有 native 扩展的旧任务路径上。

## 验证证据

日志均在本机 /tmp/b2-native-exec-r2-*，不提交生成物，各组重叠不相加。

- before.jsonl：按主控精确反例复现；实际 Engine 有扩展五条路径失败，无扩展 / 晚安装十个对照通过；模型有旧行失败、无旧行通过。包含根结论共 11 pass / 8 fail，保留失败证据。
- targeted.jsonl：race 18 根 / 139 项通过。原实际 Engine 15 个子场景扩为每场景连续两次真实 Activate，五条路径各在第二、第三 epoch 执行；供应商提交和钱包 admit / begin_attempt 仍各一次。旧 active 与 returned 行共存，在两次切换后逐字段保持原样，只有随后原 handle 的显式返回能更新旧 active。
- final-targeted.jsonl：补充无历史行跨 epoch 来源故障拒绝，以及真正无 native 扩展继续的对照后，最终代码 race 19 根 / 142 项全通过，0 skip / fail。包含 R1 证明源故障原对照、同 token 延期、同秒新 claim 隔离、旧世代及租约边界。
- native-regression.jsonl：完整 native 包 race，149 根 / 304 项 pass，5 根 fixture skip，0 fail。显式排除自动 Docker postgres 子测试并禁用外部钱包 / bucket / supplier 配置。
- regression.jsonl：相关五包 race，147 根 / 529 项，528 pass、原 real_wallet_prepaid 子项 1 skip，0 fail。
- postgres.jsonl：临时 PostgreSQL16 显式 -race，6 根 / 17 项全通过。含原锁等待 / 并发 / claim / 延期边界，以及旧 proof 自然过期后连续两次真实 Activate、原 active / returned 不改写、原 handle 迟到返回、fresh-v2 不升级旧 v1。SQL 在隔离事务执行并重放。pg-state.json 为 exit 0、自建容器 / 网络均 0。
- 最后跨 epoch 无历史行的额外拒绝条件由 final-targeted 覆盖；此前完整回归和 PG 证据对应该条件加入前的其余相同实现，没有把它们称为最终 SHA 全套重跑。
- wallet-identity.jsonl：最后增加钱包原 billing operation 与唯一 attempt 断言后，真实 Engine 两次切换 race 1 根 / 16 项全通过；重复 accept_attempt 始终使用原 attempt，没有创建新的钱包业务身份。
- build.log：最后条件补齐后再次 go -C core build ./...，exit 0。提交前执行 security-scan worktree / staged、人工差异审阅、diff check 与文档链接检查。

测试扩写曾把幂等 accept_attempt 调用也错误断言为仅一次，v1 对照同样失败；改为核对唯一钱包准入 / begin_attempt 和唯一供应商提交，再完整重跑通过。accept_attempt 对原 attempt 的幂等确认允许重复。本轮资金验证仍为既有本地假钱包调用证据，不冒充真实钱包账本联调；五个外部 fixture 跳过项仍同 R1。

## 未完成与限制

所有 use 仍 open，所有调用 continuation_state 仍 unproven。正常换 epoch、历史 proof 过期、找到可信历史记录或没有 active 行，都不生成连续覆盖或关闭资格；晚安装 / 来源不可用期间的未知窗口不补造。独立恢复水位、可信 issuer、全副本围栏仍未完成；同库所有事实一致回滚不能靠本方案证明安全。

B 的真实长转存组合、完整执行者移交与联合关闭、shadow / probe / SealPrepared 与应用 C2 后续范围仍未完成。本批无真实供应商 / bucket / 钱包操作，无 push / merge / deploy，无已有开发库或生产库迁移。
