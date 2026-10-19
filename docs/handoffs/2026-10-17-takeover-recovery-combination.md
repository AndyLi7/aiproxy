# 主控 A：原领取回执与视频恢复组合验收

2026-10-17，用户要求优先完成 A，旧渠道清理后置。本批只本地开发与验收，不推送、部署、执行线上 SQL 或付费生成。原 A 会话保持暂停。

## 精确来源与结论

- 基线 `8dde1d4b78c765a85821495584f015def63ce01c`：已验收续租精度修复。
- `c2cad26f054a4aece56f1d8d3c83156697e706fd`：原 async claim 回执与图片超时终态事务，主控本次独立审核通过。
- `929aeb1fa9ea2dc50bc5d1bc57e05b42dc5cb9f0`：B 视频交付与 A 恢复/延期接口组合，独立子 Agent 审核通过，主控核对来源、边界与组合结果。
- 两批无冲突组合于独立 `codex/b2-migration-takeover`。不改原工程师工作区。B 已发布的流式转存/Range/媒体类型边界得到保留。

## 实际验证

原 claim 精确源码副本 `/private/tmp/b2-claim-primary-20261017/source`：
- 定向 model race 140 pass / 28 skip；task 全包 141 pass / 4 skip。真实临时 PostgreSQL16 另跑 2 根/27 项通过，包括同 token 合法延期、锁等待失租、写后过期以及最后日志锁等待失败的整笔回滚；容器清理记录 cleanup.json。未把 PG 跳过当通过。
- `go build ./core/...` 通过。初始顶层 `go build ./...` 因 go.work 根不属于模块失败，修正命令后通过；不是代码修复。
- 初始全 model 探索在无关大型 `TestCallStatusSQLMatchesGo/sqlite` 矩阵处中止，不能称全 model 测试通过。测试不连接现有业务 DB。

视频精确源码副本 `/private/tmp/a-video-review-BdSCmK/`：
- race 定向 44 根/195 项，154 pass / 41 fixture skip；两根新增 PG 用例随后真实 PG 补跑。
- PostgreSQL TestVideoMigration 4 根/13 项通过；实际 RecoverOnce→Poll→Deliver 延期/锁等待/迟到回执旧 owner 保护。
- 独立追加冻结合同、原 quote 未知字段/大整数、输入与指纹保持，输出未知非 URL 字段保留且不二次生成；build 通过。
- 未实等10分钟下载，无真实 bucket/钱包/供应商调用，无 MySQL 实例锁验证。详情 acceptance.md。

最终本地组合：
- nativeresult、ownedartifact、nativetask、controller、task 五包 race：547 根/1179 项，1153 pass /26 fixture skip，0 fail。
- model 相关定向 race：24 根/141 项全部通过，含 ExpireImageGeneration 重复 CAS、async receipts、续租及相关结果场景。
- 合计1294 pass /26 skip；根与子项包含嵌套，不表示独立业务场景数。未重跑的外部 fixture 保留限制。PG证据是上述精确来源分别实测，不冒充本组合重复PG实测。
- `go build ./core/...` 通过；security-scan 37个改动文件 clean、人工身份/租约/事务/下载边界审查与 whitespace 通过。此记录加入后再次扫描。
- 日志 `/private/tmp/b2-claim-primary-20261017/combined-runtime.jsonl`、combined-model.jsonl、combined-build.log。

## 仍未完成

普通图片 poll 的上游动作/非超时任务回执/普通日志和钱包动作还需原 owner 保护。原 claim 清理不证明 executor 退出，外部钱包不与本地 DB 原子。所有未证明 use/continuation 继续 open/unproven，不关闭旧 active/未知/崩溃记录。

完整后台执行与退出事实、联合关闭/safe-disable、C2 实际 adapter/API/UI/drain/revert、真实 issuer/全副本围栏/独立恢复水位仍是完整 A 门槛。SQL 和生产启用另审。本批不恢复火山中国版，不引入新 TTL，不改变冻结报价及薄适配。
