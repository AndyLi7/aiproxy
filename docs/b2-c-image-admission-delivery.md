# C：图片预留、attempt 与钱包回执代次保护

基于已由主控验收的 relay `693e10b`。本批接入独立 `/v1/images/tasks` 的真实入口；使用关闭只覆盖明确未接纳的预留释放，完整终态/账单/执行者排空和最终 safe-disable 尚未完成。

## 实现

- `ReserveImageTask` 在共同 admission control 锁下把初始 use、任务、日志和 accounting outbox 原子提交。完整身份摘要由服务端选中的渠道产生；新预留的创建时间使用数据库时钟。不存在任务但仍有未关闭 use 时拒绝重建，不把丢失 task/outbox 当作没有发送过。
- generation / identity / attempt 是服务端临时字段，不接受公共 JSON；持久依据在既有 admission 表中，没有新增 schema。重放仍按原 owner/model/fingerprint 验证，返回已冻结任务；不会再执行 callback 或产生新的钱包/供应商调用。
- 明确钱包拒绝的释放核对完整 generation、创建时间、owner、billing operation、usage ID，拒绝任何已认领 attempt。任务/日志/outbox 删除与未接纳 use 关闭在同一事务。旧正负回执不能释放或启动后来重建的同 ID 任务。即使 C 在钱包调用期间才激活，释放失败后的 failed/refunded 回退也必须通过 generation 校验，不可按裸 ID 修改新任务。
- 每个 failover attempt 在 wallet `begin_attempt` 前持久认领。首个 attempt 只能消费已冻结预留的精确渠道身份，封口不撤销已有资格；后续换渠道 attempt 重新执行当前 family/proof/identity 准入。每个 reservation generation/index 只认领一次，未知钱包答复不会授权重发。
- `SaveImageTaskAttempt` 在钱包返回后再次锁定同代预留与该 attempt use。新 attempt 在行锁后及写后用数据库时间校验现有图片提交期限；不新增业务时长、不改变已接纳任务生成期限。时钟未知/未来、已过期、身份不符或代次变化均拒绝发送。供应商和钱包调用保持在 DB 事务之外。
- 激活前已冻结预留没有 C generation 时，初始 attempt 必须核对其持久 usage 路由、渠道类型、历史 key 标记和实际数据库完整 key 身份，再在同一事务采用 generation；不能把新 task 当旧任务。历史短标记不单独授权，实际选中身份必须与存储渠道一致。故障转移后的重新加载保留临时 generation。
- `imageprepayment.Sync` 在钱包调用前捕获 generation，返回后以同代及 status/attempt 快照写结果和账单。迟到 get/refund 回执不可污染新预留或覆盖后来 attempt。原报价、计量、图片/TTS/视频规则均未修改。

## 验证

- `/tmp/b2-c-image-regression.jsonl`：5 包 race 回归，112 根/352 项通过，0 fail；另 1 个子测试 `TestImageTaskMeasuredAdmissionBeforePaidSubmit/real_wallet_prepaid` 因未启动应用 `prepayment-wallet-server.ts` fixture 跳过，未计为通过。其余钱包与供应商测试为 loopback 合成服务。
- 最后补充激活跨越钱包回执窗口及 C 激活/旧任务真实 failover 后，定向 race 14 根/42 项，0 fail / 0 skip：`/tmp/b2-c-image-final-targeted.jsonl`。含 429→备用、所有连接失败、激活前冻结任务→备用，实际验证原价格、pricing version、日志/outbox 与重放保持。
- 实际图片控制器 + 钱包/供应商 HTTP：admit 时已存在初始 use，begin_attempt 和供应商发送时已存在 attempt use；钱包 admit 期间封口后首发仍可完成，而另一新 task 在钱包调用前拒绝。明确拒绝释放、迟到拒绝/成功、钱包未知、钱包 get 回执期间新代替换均覆盖，拒绝分支没有供应商调用。
- 实际 PostgreSQL16 双连接行锁：prepare / save 各 changed/live/expired，release / billing 各 changed/live，共 10 场景（1 根/11 项）全部通过，0 skip：`/tmp/b2-c-image-postgres.jsonl`。expired 为等待任务行锁期间自然跨过数据库期限，未用改动版本替代；changed 在等待中改变 billing operation。失败无部分 attempt/billing/释放，use 保留。隔离临时库/容器/网络清理为 0：`/tmp/b2-c-image-pg-state.json`；无拉取镜像、现有 DB 或生产操作。
- `go -C core build ./...` exit 0：`/tmp/b2-c-image-build.log`。提交前 security-scan worktree/staged 与人工全差异审阅，精确 staged 记录 `/tmp/b2-c-image-security-staged.log`。

## 尚未完成

已接纳/未知的初始与 attempt use 仍保守 open；普通终态或钱包 refunded 本身不是供应商已停止、后台核账与活跃执行者均结束的证据。本批不关闭这些记录，也不声称最终 drain/safe-disable 完成。图片 polling/archive 的全部 executor 围栏及持久使用关闭继续后续落实；现有失败/退款规则不被改写成 C 证明。

真实 G5 issuer/全副本与执行者/restore watermark 仍缺部署证据，生产 C 继续不可激活。应用真实 prepare/live历史共享key取证、gateway CAS adapter/受限probe、API/UI、drain/revert 继续实现；不以默认关闭当作完整交付。保持 A/B 工作区隔离，无推送、合并、部署、真实付费供应商或 bucket 调用。
