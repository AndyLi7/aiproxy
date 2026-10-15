# NATIVE-EXEC-R1：冻结任务不依赖当前证明源

父提交为未验收的 `5329779fe977a1ed8e38e3b3fda62266cc45002e`。主控真实 Engine 反例确认：成功 Submit queued 后 Seal，临时丢失 deployment 源，v1 下 Submit 重放 / Poll / SyncBilling 正常，安装执行扩展后六种组合全部被阻断。这是调用日志新增的兼容回归。本次只修复这一问题，不推进 publication B 或宣称完整 C 完成。

## 最小修复与身份边界

已有原生执行记录时，在控制 / 任务锁内核对同一 use 的所有记录，验证原 use 完整摘要、任务摘要、存储世代、epoch、协议版本、调用类型及状态时间一致性。只有当前服务端 service / instance / build 对应的持久记录可作为既有冻结工作的身份依据；不会选任意一条忽略同 use 的冲突记录。该路径不调用当前 LoadDeployment，不把证明源可用性再次加在已有工作之前。

执行表是既有服务端事务写入的可信数据库事实，没有新增外部写入 API。身份复用不恢复任何旧 nonce，不让新调用更新旧调用，也不授予新准入或关闭权限。新调用仍获得自己的进程私有 nonce；原代次、完整渠道身份、当前任务快照、恢复 claim token / 原 until 下界、dispatch owner 与锁后活租约检查全部保留。存储世代变化、换代、失租、损坏扩展和同 use 冲突记录均拒绝。

当前执行者没有对应身份记录而已有其他执行者记录时，必须取得新的有效服务端证明才建立该执行者的覆盖。首次覆盖也调用既有完整 lockDeploymentProof 校验持久 proof 指纹、成员、存储水位、有效期与撤销状态，并在写记录后执行 finalizeAdmissionProof；写期间过期会整体回滚。不会用“字符串格式像身份”替代这些校验。

扩展晚安装、确无任何调用记录且证明源临时不可用时，原冻结工作经任务 / use / 完整身份 / 服务端 executor / owner / 活租约验证后继续，返回 nil handle，不创建声称已验证的新执行事实。局部存储的 proof 缺失、撤销、损坏，或来源明确拒绝、返回非空但失败的快照，均拒绝，不能降级。可信来源契约明确：零快照加普通来源错误代表不可用；明确验证拒绝须返回 ErrChannelAdmissionConflict，或附非空被拒快照。新准入和 Seal 对任何来源错误均保持拒绝。

上述兼容路径只执行原业务幂等流程，不新增供应商发送权限。真实 Engine 测试逐项确认旧重放没有第二次 provider submit，封口后的新 ID 请求在钱包 / 供应商之前拒绝。

## 未覆盖窗口和恢复限制

没有记录不代表零执行；有记录或所有记录 returned 也不代表连续覆盖。所有 use 保持 open，所有调用 continuation_state 均保持 unproven。晚安装 / 证明源不可用窗口之后，即使恢复来源并成功写第一条记录，也不能补造此前覆盖；测试确认仍 open / unproven。后续联合关闭必须提供独立完整覆盖与执行者退出证据，不能凭现有行数或后来发现身份记录关闭 use。

同一服务端执行者进程重启可重用可信数据库中的冻结事实，但不恢复原私有返回能力。不同执行者无有效新证明仍拒绝。有原始记录时存储世代漂移拒绝；没有原始记录的历史任务仍无覆盖证据。把数据库和身份事实一起回滚成一致旧值，不能靠这些同库事实证明安全；独立恢复水位 / 可信 issuer / 全副本围栏仍是原有外部缺口。未增加表、迁移、缓存、客户端开关或自动激活。

## 验证

日志均为本机 `/tmp/b2-native-exec-r1-*`，各组有重叠，不相加宣称覆盖数量。

- `before.jsonl`：修复前复现主控原六个 extension 子场景失败，v1 六个通过。最终提交测试使用最新持久任务快照作为 billing 输入，排除快照时间差异。
- `targeted.jsonl`：race 14 根 / 94 项全通过，0 skip / fail。之后增加的首次覆盖写后 proof 过期断言及最终代码由下述完整回归覆盖。
- `native-regression.jsonl`：整个 native 包 race，148 根 / 288 项 pass，5 根 / 5 项 fixture skip，0 fail。原主控六失败组合全部转为通过；实际 Engine 再扩为 v1 / 已装扩展 / 晚装扩展 × 两种来源故障 × replay / Poll / billing / RecoverOnce / 已有结果 Deliver，共三十场景，并验证新请求拒绝、无重复供应商提交和未知窗口不能补连续覆盖。
- `regression.jsonl`：相关五包 race，143 根 / 499 项，498 pass、1 个 real_wallet_prepaid 子项 skip，0 fail。包含同 token 合法延期、同秒新 token 隔离、旧代次迟到返回、缺列 / 错误版本、损坏身份与执行记录、新 executor 缺失 / 撤销 / 过期 / 有效证明，以及首次覆盖写后过期回滚。
- `postgres.jsonl`：显式 -race 的 PostgreSQL16，4 根 / 15 项 pass，0 skip / fail；任务行锁等待后的 owner / 自然过期 / 延期、并发独立记录、同秒重领及延期不能复活均重跑。`pg-state.json` exit 0、临时容器 / 网络均 0。
- `build.log`：go -C core build ./... exit 0；随后只增加测试和说明文字。
- 提交前 security-scan worktree / staged、人工全差异审阅、diff check 和本地文档链接检查。

native 的五个外部 fixture 跳过项仍为 CorpusExecution、CrossServiceArchiveRecovery、RealWalletSettlementAndReplay、RealWalletRejectedSubmissionRefund、RealWalletLostBeginAttemptReplyRefundsWithoutPause。没有真实钱包、付费供应商或 bucket 联调；没有生产 / 已有开发数据库修改，没有 push / merge / deploy。B 的真实长转存组合仍未合并验证。

本轮测试编写时曾因扩展反例脚本未匹配格式而产生两个未使用 import 的编译失败；修正后所有最终回归完整通过，没有将该次失败计作通过。父批次文档把原 PostgreSQL harness 误标为 race，已纠正文档，本批明确用 -race 重跑。
