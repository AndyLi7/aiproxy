# C：实际 native dispatcher 的持久接手与监督退出

基于已验收 `d72940a6c95c4c51f0370e733d7e3b2ef4349352`，不重新打开已关闭的 R1 / R2。本批接入实际 DispatchRound 的 detached worker，记录 fork 前 offered、正确 child 的单次 accepted、以及 supervisor 观察业务及所拥有清理结束后的 exited。仍不提供 use 关闭或安全停用资格。

## 实际路径与唯一领取

既有 ClaimNativeDispatch 及公共 claim / lease / 排队年龄接口不变。实际 DispatchRound 在 claim 成功后、启动任何该任务的 goroutine 前调用 PrepareNativeDispatchHandoff；已有 native v2 execution 与新 offered 在同一控制事务中创建。绑定原 task / use / 完整冻结渠道身份、native v2 历史部署事实、原 dispatch owner 和原 deadline；原期限在准备期间改变也不能借用。

同一 use / task / 唯一 dispatch claim / 原期限形成 claim hash，由控制锁下检查和唯一索引双重约束。重复准备（包括成功但响应丢失后再试）拒绝并回滚此次新 execution，不找回原私有能力，也不产生第二个合法 child。原能力仅在进程内传给 worker 与 supervisor，JSON 为 {}。新的唯一 claim 可以登记自己的调用，但旧能力不能接手或完成它。

child 在路由 resolver、钱包和 provider 前执行 Accept：完整冻结事实、原 claim / deadline、历史 proof、数据库锁后当前时间均重新核对。只有 offered 可单次转 accepted；重复接受不授予第二次执行权。状态写后再次校验实际 DB 租约，延迟 / 换 owner / 失租拒绝，写期间自然过期回滚。

准备持久化失败时不 fork、不调钱包/provider；任务保留既有 dispatching claim，交给原恢复规则，不重置队列年龄、不强行退款或重新入队掩盖未知。该失败不会改变其他任务已发生的业务结果。

## supervisor 观察的退出范围

已登记路径由 supervisor 创建 child。child 的业务函数返回且函数内 defer（包括 dispatch context / wallet context 取消、纳入该调用栈的 provider 清理）退出后，观察器才发出正常完成结果；supervisor 随后以独立有界 DB context 写 RecordExit。在同一事务里更新 handoff exited 与原 execution returned / error_returned，并再次校验原租约。WaitGroup 覆盖 supervisor 的持久化尝试。

parent 在 offered 后退出、child 未接受、child panic、进程消失、写入失败、失租或换代均不合成正常退出。生产 panic 传播方式未改变，不通过 recover 把 panic 改成成功。观察器只在正常返回时报告 returned；原私有能力丢失也不能从数据库摘要恢复。

这里证明的是该 worker 所拥有的业务及清理范围，不是 goroutine 调度尾部、供应商异步任务、未 join 的 adaptor goroutine、后续 poll / billing 或全执行者退出。实际测试让 provider 启动一个仍阻塞的后台 goroutine：即使 handoff exited，原 execution 的 continuation_state 仍 unproven、use 仍 open。WaitGroup 返回、时间到期、没有 active 行均不是排空证明。

合法 requeue 会清除 dispatch owner；若 supervisor 随后不能再证明原完整租约，仍保守保留 accepted / active，不伪造 exit。该情况及崩溃缺口需后续可靠移交 / 退出与围栏方案处理，本批不靠删除旧记录解决。

## 可选扩展、安装中途与历史兼容

[SQLite 草案](../core/migrations/channel-dispatch-handoff-v1/sqlite.sql)和 [PostgreSQL 草案](../core/migrations/channel-dispatch-handoff-v1/postgres.sql)依赖 native execution v2。新增 channel_admission_dispatch_handoffs 表和协议 id=3 / version=1。native v2 本身不升级版本或补历史，只有准备时的内部原子写入回调与共享原校验函数被复用。

- 子表与协议都不存在：保持原 dispatcher 路径；不会为已 fork 的工作补造 offered / accepted。child 执行过程中安装子扩展也不补记录。
- 部分安装、错误版本、缺列、孤立协议行或 DB 错误：拒绝准备，不能误当完全未安装。
- claim / 准入之后、fork 之前安装完整子扩展：可以如实登记这次未来交接，不声称此前连续覆盖。
- 已有当前身份的可信 native v2 记录时，来源临时不可用仍可继续；合法 Activate 到新 epoch 时按新有效 proof 登记，历史行保持原归属，不重引 R1 / R2。
- 所有执行扩展都晚安装且原任务没有可信调用记录、来源暂不可用时，沿用已验收同 epoch 冻结兼容路径；不创建任何声称可信的 execution / handoff，原 use 继续 open。此处是明确的无覆盖历史工作，不是把持久化失败当成功。跨 epoch 无新 proof 仍拒绝。实际测试保留这个未知窗口。

协议存在不证明所有副本都已升级，历史缺口不因后续成功记录而消失。已有未发布 native v1 不能自动升级或补证。SQL 只在新建隔离测试库 / schema 执行并重放，没有应用已有开发库或生产库。

## 验证

证据在本机 /tmp/b2-dispatch-handoff-*，各组有重叠，不相加。

- final-targeted.jsonl：最终代码 race 14 根 / 58 项 pass，0 skip / fail。覆盖单次/并发接受、重复准备、旧能力与新 claim 隔离、准备原期限、offer/accept/exit 写期间自然失租回滚、历史 / 存储 / owner / lease / 能力损坏、来源不可用 / 合法换 epoch、部分安装与孤立协议。
- 实际 DispatchRound 测试：provider defer 清理阻塞期间不能 exited 或完成 WaitGroup；准备失败无 fork / 钱包 / provider、年龄不重置；重复 child 不二次调用、失租 child 在钱包/provider 前拒绝；child panic 与 exit 写失败保留 accepted / active；中途安装不补历史；provider 背景工作仍活跃时不关闭 use。
- native-regression.jsonl：全 native 包 race，156 根 / 323 项 pass，5 根外部 fixture skip，0 fail；排除自动 Docker postgres 子测试，未配置外部钱包 / supplier / bucket。
- regression.jsonl：相关五包 race，153 根 / 569 项，568 pass、原 real_wallet_prepaid 子项 1 skip，0 fail。包括 R1 / R2、image / relay 和恢复领取 / 延期回归。
- postgres.jsonl：真实 PostgreSQL16 显式 -race，7 根 / 24 项全通过。新增 accept / exit × live / 等锁后自然过期 / owner 被改六个场景，只有 live 前进；其余保留 offered / accepted 与 active。并重跑 native 历史连续 epoch / 原 lease 等边界。SQL 在事务内执行并重放。pg-state.json：exit 0，临时容器 / 网络均 0。
- 完整回归 / PG 之后补充了孤立协议和准备原 deadline 两个拒绝条件；最终相关 58 项和 build 已重跑，不冒充这些旧日志是在最终条件后全套重跑。
- build.log：最终代码 go -C core build ./... exit 0。提交前 worktree / staged security-scan、人工差异审阅、git diff --check 和文档本地链接检查。

测试初版误以为已排队任务没有原 native v2 调用记录，因此对证明源不可用场景期待空 handoff；实际 Submit 已有可信记录，正常登记并 exited 才是正确兼容行为。修正断言后重跑通过，并另增真正所有扩展晚安装、没有历史记录的无覆盖对照。

## 仍未完成

所有 use 仍 open，continuation_state 仍 unproven。图片 / native recovery 外层 / poll / billing 等其他后台接手、联合终态与账单关闭、安全停用、disabled shadow / probe / SealPrepared / family+revision CAS、应用 C2 后续范围仍继续开发。真实 issuer / 全副本围栏 / 独立恢复水位仍是 C 启用门槛。

B 长转存实际组合尚未合并验证，工作区未互改 / 互合。没有真实钱包账本、付费供应商或 bucket 联调，没有 push / merge / deploy，也未进入 publication B。
