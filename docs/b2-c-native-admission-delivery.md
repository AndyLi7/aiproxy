# C：native 实际入口与预留代次保护

本批在 `da7f9ac` 基础上接入 native 实际入口、预留释放与迟到钱包回执保护，并约束普通渠道身份编辑/删除。不是完整 C 验收，不启用迁移、不部署、不操作真实供应商或现有数据库。

## 已实现

- public native 与管理员 trial 的实际 plan 均由服务端 `ChannelIdentityOf` 计算完整身份；客户 header/body 不提供授权身份。`Plan.AdmissionIdentityHash` 和 NativeTask 的 AdmissionIdentityHash/AdmissionGeneration/AdmissionRecoveryOwner 都是临时字段，不新增任务列、不改 frozen contract、quote、价格或计量。
- `ReserveNativeTask` 在 control → task 的数据库事务内写 use 与任务预留。已冻结任务保留原 ownership/fingerprint/quote 等校验，不等待新部署证明；新任务校验精确身份、当前 family grant、enabled 和可信本地部署快照。仅设置 `Direct` 或内部入口不能绕过屏障。
- `resource_key = SHA256(canonical JSON [native, group, token, task ID])` 绑定逻辑任务。每次新预留用独立随机 generation 建 use，use 的 ID/身份/请求摘要保持不变。任务无故消失而 use 仍 open/unknown 时拒绝重新预留；只有原子释放留下的精确 closure hash 才允许下一代。
- `ReleaseNativeReservation` 的可选服务端证据包含 generation、创建时间及恢复 owner。实际 Submit 仅在 WalletRefusal 后传入；恢复器仅在应用明确的 Prepayment-not-found 后传入。超时、普通 404、任务丢失不构成释放证明。
- 释放在同一事务锁定完整任务，验证仍 reserved、无 upstream、精确 generation/创建时间、重试未刷新，删除任务并关闭对应 use。恢复器另验证 owner 与锁后的数据库时间，并在 DELETE SQL 再验 lease。关闭记录不能被旧一代的迟到回执复用。
- 认领恢复任务会更新 updated_at。因此恢复证据保留认领前任务更新时间和认领后观察时间：前者验证陈旧门槛，后者作为未被并发重试刷新的条件。没有改变旧路径的认领/退款规则。
- 正向回执也有代次保护：reserved → submitting、enqueue、reserved SyncBilling 的保存使用 `WithNativeReservationGeneration`。恢复器带上自身 owner，在写入前、提交前均检查数据库时间内的有效 lease。迟到旧代 begin_attempt/waiting/get 不能推进新代或调用供应商；零行/冲突不走未保护回退。所有钱包/供应商/外部证明刷新均在事务外。
- `UpdateChannel` / 单个和批量 Delete 在 control → channel 锁内保护 use/barrier 所引用的物理身份，以及可识别的 legacy native/image/async 未完成记录。路由字段仍可改，普通重新 enabled 不能绕过 family grant；既有显式 status 停用没有被冒充为迁移安全停用。

## 默认关闭、存储与锁序

- 无 C 表且无 runtime adapter：原路径。完整表安装、epoch 空且没有使用/屏障/family 历史：旧行为在同一 control 锁内执行，不能与 Activate 穿插。
- 激活或存在历史后，即使本地证明源撤回/应用迁移开关关闭，持久保护仍适用；新任务缺证明拒绝。已有任务读取/冻结重试不等同新准入。
- 部分表、读取错误、DB/LogDB 不同 GORM handle、已安装但字段不全均拒绝受保护操作。MySQL 仍仅支持从未安装 C 的旧路径；新增 C 未证明适用性。
- 本批增加 use.resource_key；fresh SQL 位于 `core/migrations/channel-admission-v1/`。安装过早期草案的环境另有一次性的 `channel-admission-resource-v1/` 审阅 SQL，不自动执行、不补造旧 use 的归属。SQLite 升级草案已测，PostgreSQL 本批测试为 fresh SQL；旧草案升级脚本在 PostgreSQL 未单独执行。
- control → task 包装内只调用 bounded DB 操作；enqueue 不取得 scope 锁。事务提交后才运行 fast-path claim，继续原 scope → task 次序。没有 control/task → scope 的嵌套取锁。
- 精确失租保护目前针对本批的 reserved 预留链路；本批不宣称所有非 reserved 执行者、旧二进制或全部入口已完成 G5 围栏。

## 验证（2026-10-16）

- 定向 `go test -race -json`：68 个根测试、含子测试 145 项，0 fail / 0 skip。精确命令 `/tmp/b2-c-runtime-test-command.json`；结果 `/tmp/b2-c-runtime-regression.jsonl`。包含 foundation 回归、实际 Engine.Submit/enqueue/RecoverOnce/SyncBilling、真实 public resolver 与原 trial HTTP 兼容、SQLite 重开/崩溃回滚/并发 seal、迟到代次/失租/钱包未知结果、默认关闭及部分存储、普通渠道修改/删除。
- 已有本地 PostgreSQL 16 镜像、新建隔离临时数据库：2 个根测试、含子测试 8 项，0 fail / 0 skip；实际 Go 双连接持有 task 行锁后，释放与正向写入分别覆盖 lease 过期、owner 改变、仍有效。并验证 fresh SQL 重放不激活。结果 `/tmp/b2-c-runtime-postgres-final.jsonl`；资源清理 `/tmp/b2-c-runtime-pg-final-state.json`（容器/网络剩余均 0）。无镜像拉取、无真实数据服务。
- `go build ./...` exit 0，`/tmp/b2-c-runtime-build.log`。未改应用仓库，因此本批不重复 pnpm build/tsc；应用此前 tsc 的 237 项基线仍不是通过。
- security-scan 脚本与人工差异复核：SQL 参数化、运行时证明和代次均为服务端内部输入、无客户绕过、无明文 key 新增持久化或输出、无外部写入。提交前另执行 staged 扫描，结果 `/tmp/b2-c-runtime-security-staged.log`。

## 未完成与下一批

图片/普通/流式/prepay/internal/demo/generic async 仍需接入共同协议；本批虽然普通身份编辑能看见 standalone use，但未宣称 generic 上游受理窗口已经实际产生该记录。除明确未接纳的 native 预留释放外，已接纳任务的 use 仍保留；任务终结/账单/worker lease 的使用关闭与最终安全停用另行完成。

激活后 disabled shadow 的一般 trial 仍拒绝；受限迁移 probe 需要服务端权限与持久证据，不以通用 trial 绕过来代替。C2 prepare/review/commit/recovery/drain/revert 管理流程继续实现。

真实部署证明签发、全部入口及 executor fence、独立 restore watermark 适配仍未闭合。测试 proof 均为合成值，无生产 adapter 或激活 HTTP 入口；永久关闭不是完整 C 的完成标准。

共享文件：`common/nativetask/{execute,dispatch,recovery}.go` 仅传递身份/代次/恢复 owner 并包装预留写入；`model/native_task_result.go` 增临时字段与 Reserve/Release 入口包装。未修改 `native_task_queue.go`、TaskContract、视频时长/计量/转存策略；主控已获接口预告。
