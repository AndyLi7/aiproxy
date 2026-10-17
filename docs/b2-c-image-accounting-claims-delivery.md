# ACCOUNTING-CLAIM-1：ordinary image accounting 领取保护

基线：`7a542cc8fecbce7ee261450f3acc8862208c927c`。独立分支 `codex/b2-image-accounting-claims`。仅本地开发，未推送、部署、访问既有数据库或调用真实供应商。

## 实现范围

- 实际 `processOneAsyncUsage` 的 `MeasuredImage` 优先分支在处理 pending/failed/complete 前校验原 claim。普通 image 的 `completeAsyncUsage` 使用同一 accounting scope。没有 task 的同步 measured image 使用 usage→log；有 task 的使用 task→usage→log。
- 已绑定持久 task 的 `PrepaymentQuoteJSON` 与当前持久 usage 决定旧预付费路径排除；找不到 task/绑定不符/MeasuredImage 与预付费矛盾均不允许 legacy fallback。该字段没有新增请求可控开关。
- 在锁内校验 pending、原 token、实际 DB 中同 token 的当前期限、固定计费身份/报价/测量快照以及精确 log。锁后、取得最后 log 锁后及写后检查新 DB clock，始终使用写入前的 deadline；同 token 合法续期兼容，不从 caller.NextPollAt 或下一次调度时间推断有效期。
- measurement 与 log、park/failed/completed 与 usage/token 的写入在一个短事务中完成，失败全回滚；不使用裸全行 Log.Save。日志 evidence 仍先于扣款写入，保留钱包回调立即读取的兼容；此时 log completed **不是**钱包已确认的证明。
- 普通钱包余额查询、扣款、measured-zero 的每个动作前原 claim guard；ExternalHTTP 每次实际 HTTP/内部重试、Sealos 的余额重试/实名查询/扣款 HTTP 与扣款缓存动作均检查同一 scope。无 scope 的既有非 image 入口保持兼容。网络不在 DB 锁内。
- 远端已接受但本地失租：旧 worker 停止，不换 owner、不再发新动作、不凭内存 BalanceConsumed 完成。replay-safe 后续合法领取者使用持久原 operation、金额和 pricingVersion 重放；非 replay-safe attempted 持久保留，未知结果进入原人工核对路径。没有重新报价或换幂等 key。
- 三处旧测试构造的虚拟 image task / 无持久 claim 已补成真实绑定 task 和有效 lease；未放宽防护。

## 验证证据

1. 不可变父版本复现：`/private/tmp/b2-accounting-before-yrp8jijy/before.jsonl`。真实 worker expired pending/complete 仍写入、余额响应后换 owner 仍扣款、首次 consume 后换 owner 仍发 4 次 HTTP；共 4 失败叶（6 个 fail 含父），failed 分支对照通过。修复后这些真实入口场景通过。
2. model 定向 race：`/private/tmp/b2-accounting-model-race-final.jsonl`，458 pass / 94 PG fixture skip / 0 fail。命令：`go -C core test -race ./model -run 'TestImage|TestAsyncUsage|TestMeasured|TestPostgresImage' -count=1 -json`。包含 existing image/poll/claim 回归；没有跑无关全 model SQL 矩阵。
3. task + balance 全包 race：`/private/tmp/b2-accounting-packages-race.jsonl`，task 186 pass / 5 skip，balance 63 pass。之后增加 Sealos guard，balance 全包最终 `.../b2-accounting-balance-final.jsonl` 64 pass / 0 skip。最后 scope/夹具调整的 focused race `.../b2-accounting-final-focused.jsonl` 141 pass / 1 wallet fixture skip。这些计数有重叠，不相加为独立用例总数。
4. 新 PG 真实锁场景：`/private/tmp/b2-accounting-pg-bc77vmns/tests.jsonl`，29 叶 / 35 pass 含父、0 skip。五类操作：measurement / park / evidence / complete / failed；覆盖 task 锁等待自然过期、最后 log 锁等待自然过期、同 token 续期跨原截止、换 owner、同步 measured 无 task 的 log 锁等待回滚。harness `/private/tmp/run-b2-accounting-pg.py`。postgres:16-alpine 本地缓存镜像、临时 tmpfs 数据库、随机 loopback 端口，容器及凭据已清理，见 cleanup.json。94 个 race PG skip 中本批 29 个已补跑；其余旧 PG 本轮未重跑，不能挪用历史证据冒称本轮全过。
5. 真实应用钱包：从 app `3e93cbe7883ea83d696b3feac642c67ec0438b38` git archive 隔离源码，复用只读 node_modules，启动已跟踪 `sec-wallet-server.ts`（真实 route/service + 内存 SQLite）。`/private/tmp/b2-accounting-wallet-pv_okh7s/gateway.jsonl`：首次 consume 已提交后换 owner，本地确认拒绝；新合法领取者同 operation 和 0.3 原金额重放，2 次请求只产生 1 条 -300000 微单位 ledger。未改真实 DB/账单/供应商。harness `/private/tmp/run-b2-accounting-wallet.py`；fixture 已停止，cleanup.json 可核对。首次 sandbox 不允许监听的尝试没有产生测试结论，后续授权 loopback 执行通过。
6. task 的 5 个 skip：原续租 PG stop / parent-cancel 两项、该 real-wallet fixture、SECWalletIntegration、UsageAttributionLedgerMatchesLogs。real-wallet 已按上项补跑；另外 4 项本轮未启动相应 fixture，不声称通过。
7. `go -C core build ./...` 通过，`/private/tmp/b2-accounting-build-final.log`。security-scan 脚本及人工 diff 检查在提交前执行，见 `/private/tmp/b2-accounting-security-staged.log`。

## 尚未闭合的边界

- 本批只保护领取仍有效期间的 ordinary accounting。预付费 `imageprepayment.Sync`、它的内部钱包动作/回执，以及 poll terminal 清 token 后即时退款的统一 sidecar owner runner 留到下一批。现有预付费 Sync 后内存 Price/BalanceConsumed 特殊兼容保持，不能拿本批通过宣称该边界已完成。
- 任意网络请求已经发出后，可能在远端执行期间发生失租；原 deadline 不撤销已接受扣款。处理方式是保留未知和同 operation 幂等恢复，不能声称网络 exactly-once。
- log evidence completed 不等于资金结算完成；所有 use 继续 open/unproven。没有完成全局 billing executor、历史覆盖、独立恢复水位、safe-disable 或完整 C2。
- 没有新增 schema/TTL，不改原三分钟领取期限、金额/报价/薄适配规则。MySQL 仅复用现有毫秒 DB-clock/deadline 语义，本批无 MySQL 实例验证。
- 发布必须依用户规则：独立分支→本地测试→PR审核→精确提交 staging 验收→合入 GitHub main→核对合并版本→用户新明确确认后才 production。子 Agent 不发布，原 A 会话继续暂停。

### Final compatibility checks

MySQL can return RowsAffected=0 for unchanged idempotent writes. While holding the existing row locks, an error-free zero-row UPDATE is accepted only after verifying the target still matches every written field; missing rows and suppressed writes reject. The final original-deadline DB-clock check still applies. UpdatedAt is normalized to milliseconds for the existing column precision. The recording test uses actual GORM/SQLite transactions and an affected-row callback, not a live MySQL instance.

Final focused race after this compatibility fix: `/private/tmp/b2-accounting-noop-final.jsonl`, 145 pass / 1 optional real-wallet fixture skip / 0 fail. This overlaps the tests above and is not an additive unique-test total. The real wallet test was separately executed as described above. Final build passed.

The existing measured-zero receipt-window fallback remains: after its window, the worker alerts and application catch-up handles the receipt. This batch does not claim that fallback proves receipt confirmation or use closure.

## Independent review fixes: ACCOUNTING-CLAIM-R1 / R2

The prior 58985a64 baseline was not accepted: the independent reviewer reproduced redirected HTTP actions after owner loss, and a persisted mismatched task/usage billing operation incorrectly selecting the prepaid exemption. This follow-up fixes those two defects without changing the earlier batch boundary.

- R1: ExternalHTTP balance/consume and Sealos real-name/balance/consume use `actionguard.DoHTTP`. With a scope it clones the shared HTTP client and wraps CheckRedirect. The existing redirect policy runs first, then the original scope is checked before the redirected request is sent. The default ten-redirect limit, explicit redirect denial and ErrUseLastResponse are preserved. Unscoped requests still call the original client directly. This guards application-visible redirects, not requests already sent or opaque internal Transport retries. MeasuredZero retains its existing no-redirect policy.
- R2: The prepaid exclusion additionally requires a nonempty durable task billing operation equal to the durable usage billing operation. Mismatched or both-empty operations use strict claim validation and cannot write a completed evidence log through legacy fallback. Existing task/group/token/usage identity checks remain; positive fixtures now carry a real nonempty frozen operation. This addresses corrupted/inconsistent persisted identities, not a claim that clients can directly mutate those rows.

Verification of this follow-up:

- Final race packages: actionguard 5 pass, balance 80 pass, task 186 pass / 5 fixture skips; `/private/tmp/accounting-r1-r2-final-packages.jsonl`.
- Final focused model race: 133 pass, zero skips/failures; `/private/tmp/accounting-r1-r2-final-model.jsonl`. Includes reviewer original operation-mismatch receipt regression and both-empty expired receipt rejection.
- Both balance backends: 307 and 308, live owner, lost owner and no scope. Live/legacy redirects preserve the exact POST body; lost owner emits only the original request. Existing callback policy, callback-induced loss, ErrUseLastResponse and default ten-hop limit also tested.
- Current app 3e93cbe real local wallet fixture: original accepted consume/local receipt rejection/recovery replay keeps one ledger debit and frozen operation. `/private/tmp/b2-accounting-wallet-bi2yzqie/gateway.jsonl`; cleanup.json confirms fixture stopped, in-memory SQLite only. This supplements the corresponding task skip. The other four skips are two existing PG stop/join cases plus SECWalletIntegration and UsageAttributionLedgerMatchesLogs, not run in this follow-up.
- Core build passed; `/private/tmp/accounting-r1-r2-final-build.log`. No new PostgreSQL/MySQL instance test claimed for this HTTP/persisted-predicate fix; earlier transaction evidence remains historical.
- Mandatory security script plus manual changed-file review passed; staged scan at `/private/tmp/accounting-r1-r2-security-staged.log`. No secrets or live configuration included.

Prepaid Sync/terminal refund ownership and complete executor/use closure remain out of this batch. No push, deployment, production access, provider invocation or paid generation. Parent performs independent exact-SHA review; release sequence remains local tests → PR review → exact staging verification → merge/main verification → fresh user confirmation before production.
