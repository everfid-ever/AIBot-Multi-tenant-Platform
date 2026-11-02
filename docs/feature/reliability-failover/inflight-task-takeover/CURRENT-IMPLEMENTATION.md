# 在途任务接管与幂等投递 — 当前实现摘要

本轮接入具体持久化笔记业务：明确 `/note` 命令冻结原参数，原确认后写入并独立读回核验，最终答案引用真实编号；自然聊天不强制写入。新增 `/ops/task/records`，按原租户/请求对五类记录进行稳定主键分页。详见[业务与分页设计](design.md#具体业务闭环原始笔记参数独立读回与真实编号)。

本轮补齐**发布前工具条件校验、结构化恢复摘要和原任务详情查询**。完成条件须引用精确且强制的工具，并通过部署目录及可信完成断言检查；恢复摘要只投影持久事实。`GET /ops/task/incident` 按租户与原请求关联 Inbox 到投递元数据，区分 SQL 快照与独立检查点读取，不授予重放权限。详见 [设计](design.md#发布前校验恢复摘要与任务详情)。

本轮新增**原始目标与服务端必需交付物合同**：新任务冻结原输入目标及版本化 `runtime_policy.task_requirements`，步骤必须覆盖配置要求，最终渲染答案通过内容和可信回执引用检查后才能成功。新草稿缺项保留进度并在原预算内修复，已保存候选矛盾仍走审计失败；未配置要求不等于通用语义正确保证。详见 [设计](design.md#原始目标必需交付物与最终输出校验)。

本文是阅读摘要，详细机制、字段、代码及验收分别在同目录L1–L4文档中展开，所有正文使用同一当前实现口径。

当前部署增加 000008_delivery_receipt：正常成功响应先保存不可变发送回执，再确认 Ledger；回执落库后退出可由新 owner 核对计划/路由/分段摘要自动补 sent，不重发。没有持久证据的窗口仍按真实渠道查询或人工证据处置；未知错误、取消、续租失败、HTTP 服务端错误不再盲重试，failed 分段阻断后续分段。新增 GET /ops/delivery/incident，resolve 支持原请求幂等重试，且不能用负向处置覆盖已存成功回执。

`000007_execution_completion` 将输出审核与治理写入分开：完整 envelope、输入摘要、原预约、分模型用量、确定的结算参数、输出审核决定与最终渲染内容先加密保存。候选不可改写且不是可投递结果；接管者跳过新的模型运行，重放原结算决定，再保存 result_payload 和 terminal/Outbox。即使预约已 settled，也不重新 Begin 而误判为 reservation_closed。读取顺序是先核对完成候选及其关联公开结果，再兼容无候选的旧冻结结果，最后才考虑新模型运行。新候选 SchemaVersion=2 额外绑定 PlanDigest，矛盾状态提交明确 failed/audit 并保留证据，不盲目重算或发送。SQL 候选写入还与 execution_attempt 的执行记录锁串行，拒绝已被新 Fence 超越的写入。

MCP 恢复默认 manual；经代码审核的 ReadOnly 才允许重放，经审核的 IdempotencyParameter 配合 CompletionCheck 才开启同业务键重试。参数由可信 tenant/request/工具/稳定调用/参数摘要派生并注入，模型不能覆盖。每次连接重新核对声明摘要，MCP isError 不成为成功证据。代码执行提供 status=completed 可信断言，非零退出、超时及 SDK 参数校验不算完成；执行中断仍采用 manual，因为不能从所有沙箱后端推断外部写操作幂等。

当前新增 `000005_task_checkpoints`：原输入摘要绑定加密计划、只追加步骤修订、依赖闸门、工具操作/参数固定、结果证据核对与封存提交。步骤计划在本地 WebUI/WeCom 组合根启用；原始目标仍取自持久输入，不让模型改写 tenant/request。大结果按原引用分块读取。原 `000004` 的计数、工具结果和恢复机制继续复用。

当前合同由 `000006_task_failure_convergence` 进一步收敛：空计划不能成功，工具检查必需，副作用工具需实现可信输出契约；任务预算默认最多 8 次运行机会、连续 3 次无进展、首次运行后 1800 秒，换节点不重置。失败计划、原因和固定失败结果可跨节点续提交原 failed terminal 与 reply/audit Outbox。部署须具备 000006 新列与约束，明细接口增加 task_failures 原因码。

保护系统已经持久接收、尚未完成最终回复的同一原始输入。执行或发送节点故障后，用户不重发，由存活节点回收工作、复用结果或按协议安全重试。

| 测试路径名 | 实际边界 | 证据 / 使用限制 |
|---|---|---|
| p1 | 预处理持久领取后、执行前 | 原输入/job 已持久化；不能代替所有 P1 子窗口。 |
| p2 | 模型成功响应头返回后、SDK 消费结果前 | 请求尝试与成功头状态；不证明 provider 内部仍在生成。 |
| p2-commit | 模型/工具完成、terminal 结果提交前 | 旧边界，不能代替执行中的 P2。 |
| result-before-commit | 冻结结果保存后、terminal/Outbox 提交前 | 原 result_ref/digest 不变，接管不再调用模型。 |
| p3 | terminal 已提交、reply PublishReply 前 | session_commit、result_payload、已领取 reply Outbox。 |
| p4-before-send | Ledger claim 后、provider 调用前 | 旧发送前边界，不能证明 P4 已接受。 |
| p4 | provider 成功接受且返回消息 ID 后、回执落库前 | barrier 快照 Evidence；进程内快照需在 kill 前采集，非 durable receipt。 |
| p4-receipt | delivery_receipt 已落库、Ledger sent 前 | 接管后核对摘要，只补 sent，不再次调用渠道。 |
| tool-effect | 工具业务效果完成后、工具结果提交前 | 按业务键查外部效果，恢复不能增加效果数。 |

仅 `TRPC_WEBUI_LOCAL_FAILOVER_TEST_ENABLED=true` 注册 `/test/failover/<point>/arm|status|release`，用本地 WebUI token 鉴权。arm 可带 tenant_id/request_id 过滤；status 回显命中身份/evidence。暂停控制面是进程内测试设施，不续 lease、不修改业务状态、不替系统重投。标准接管在命中后杀 owner，不对 survivor release。

## 工具恢复的执行顺序

当前 idempotent_key 工具可额外提供 RecoveryLookup：接管先查原业务键，completed 复用结果，not_found 同键继续，pending/unknown/异常不回退写入，failed 终结工具。实际 local_note 已接上业务表查询，Create 重试也会认证并返回原密文结果；MCP 提供显式审核的 LookupConfig 和 JSONTextResult，未知提供商没有自动获得查询/重放能力。代码自有完成回执在 Complete 前及旧结果复用时都校验，大整数 ID 保留精度。见[工具设计](design.md#状态查询优先与可信回执前置校验)。

1. `GuardedCallable.callDurable` 校验租户执行上下文、payload key version、规范化参数及工具版本。普通 allow 的 operation ID 是 tenant/request/tool/version/参数摘要的 SHA-256；同一输入的同工具同参数视为同一逻辑操作。需要不同效果时必须带不同业务参数/键。ask 使用确认中的持久化调用 ID。
2. 读取 tool_execution。已 succeeded 且不可变字段一致时，LoadResult 解密、校验结果引用摘要与代码自有可信断言后复用；failed 拒绝继续。未知新工具默认 manual。
3. Claim 首次创建 running，或领取 pending/effect_unknown/过期 running；attempt 和工具 fence 增加。未过期 running 不可抢占。
4. 启动工具续租。ask 消费 grant；遇到已消费冲突时仅允许恢复同 request/tool-call 且 effect_unknown 的 attempt，不再次消费授权。
5. idempotent_key 若启用 RecoveryLookup，恢复先查原业务键：completed 复用回执，not_found 同键继续，pending/unknown/查询错误不写，failed 终结工具；未配置时保持原同键调用。旧 queryable 协议仍要求 false 证明未接受，未知不得当 false；replay_safe 重放；manual 接管保留 effect_unknown 并安全停止。
6. 外部调用错误可将 tool_execution 标为 effect_unknown；lease 丢失立即取消 call context。外部系统若不响应取消，仍由其幂等/查询协议避免重复业务效果。
7. 实际调用或查询补回结果先验证 JSON 与代码自有可信完成断言；拒绝回执标为 failed/tool_receipt_rejected，不创建成功结果。校验通过后停止续租，Complete 在同一 PG 事务中验证 owner/fence/有效租期并写 succeeded 和加密结果。失败/未知状态落库也先停止续租。
8. ask 再把结果镜像至 tool_result_payload 并完成 tool_attempt；镜像前故障可从 tool_execution_result 重建。业务效果与平台结果是两个事务；local_note 用 (tenant_id,business_key) 唯一键覆盖这段窗口。

恢复的是已保存结果或外部操作的协议状态，不会恢复工具函数体的内存/指令位置。未冻结最终结果时模型可能重新推理；相同逻辑操作重新进入 GuardedCallable 时复用或安全恢复。任意 Graph 检查点也不能据此推断为自动恢复整张图的执行栈。

## 共同计数与投递边界

| 计数 | 权威记录 | 解释 |
|---|---|---|
| Worker 尝试 | `execution_attempt` 的行数 / `attempt_no` | 每次进入执行器一行；新尝试把旧 `running` 标为 `superseded`。completed 仅表示执行器正常返回。 |
| 逻辑模型调用 | `execution_record.model_call_count` | 进入模型适配器的逻辑调用，可因恢复增加。 |
| 模型 HTTP 尝试 | `execution_record.model_request_count` | transport 尝试，含连接失败及内部重试；不等于接受数或计费数。 |
| 工具尝试 / 业务效果 | `tool_execution.attempt` / 外部业务幂等记录 | 同键工具重试可大于一次，业务效果应按工具契约去重。 |
| 回复发送 / 用户可见答案 | Ledger `attempt`、`segment_no/count` / 客户端证据 | 一条答案可有多个分段；唯一 Ledger 行不能单独证明用户只见一次。 |

内部保证与外部交付分别判定：同一输入只有一个有效 terminal commit；每个回复 segment 在 Ledger 中只有一行权威状态，`sent` 后自动路径只跳过发送。该行唯一不等于渠道只接收一次。P4 中若匹配的 provider receipt 已落库，可直接补齐 Ledger；回执也未落库的剩余窗口仍需下游可靠去重/查询或外部处置证据。当前 WeCom 没有请求键查询适配器，内容级有限时间去重不足以提供永久请求级幂等；取消、HTTP 5xx 和响应丢失均可能留下不确定结果。系统不盲目补发，先保留 `ambiguous`；核对次数耗尽可转为 `failed/reconcile_exhausted`，仍需运维查证。

## 详细文档

见 [概念](OVERVIEW.md)、[机制](FULL-GUIDE.md)、[字段与状态](design.md)、[运行走读](implementation-walkthrough.md)、[代码](CODE-APPENDIX.md)、[参考实现](REFERENCE-IMPLEMENTATION.md)、[验收](testing-and-acceptance.md)、[运维](release-and-operations.md)。
