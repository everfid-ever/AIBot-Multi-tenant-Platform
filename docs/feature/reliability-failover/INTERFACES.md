# 接口、对象字段、配置与启动契约

代码基线为 `0f53489`。本文分清真实挂载的 HTTP 接口、内部 Go 合同和跨 Redis 的对象，不能把仓库中存在的 Gateway 接口误当成当前组合根已公开挂载。字段取自当前源码 AST；嵌入对象按其源类型展开。数据库字段及关系见 [DATABASE.md](DATABASE.md)，按消息顺序的事务与恢复见 [END-TO-END-DATA-FLOW.md](END-TO-END-DATA-FLOW.md)。

## 实际 HTTP 面与权限

| 入口 / 方法 | 输入与权限 | 输出及语义 | 实际落点 |
|---|---|---|---|
| 应用 `GET /livez` | 无业务 token | 200 仅表示进程存活，不是业务就绪 | webui_local_role.go |
| 应用 `GET /readyz` | 健康探测；drain 时拒绝接新业务 | 200/503；检查组件真实运行状态和 PG/Redis/ClamAV 探测 | runtime_health.go / webui_local_role.go |
| 应用 `/statusz` | 仅 StatusEnabled 时挂载 | 实例、启动身份、owners、组件状态、channel_mode=http_callback；不是凭据接口 | webui_local_role.go |
| `GET/POST /callbacks/wecom?route_key=...` | GET 协议验证；POST 渠道签名和 AES 身份校验，外部 body 受大小限制 | 渠道协议响应；可靠接收与任务完成分开；不接受用户自报 tenant 作为授权 | channels/wecom + ingress |
| `POST /webui/api/messages` | route_key 定位；X-WebUI-Timestamp/Nonce/Signature 对实际载荷验签 | 可靠接收结果，不同步等待模型最终答案 | channels/webui/http.go / protocol.go |
| `GET /webui/api/replies` | route_key、external_user_id、external_chat_id，加浏览器请求签名 | 本目标的持久邮箱回复；已接受发送可独立查询 | channels/webui/http.go |
| `GET /webui/api/progress` | 同一浏览器身份及签名 | SSE 进度；进度丢失不等于最终回复丢失 | channels/webui/http.go |
| `POST /webui/api/confirmations/actions` | 已验证浏览器身份、原确认记录和参数摘要 | 批准/拒绝原工具操作；不能跨用户/请求消费授权 | channels/webui/http.go |
| `GET /ops/session/lease` | 独立管理 token；tenant_id/agent_app_id/session_id | 观察 owner/Fence/期限，不返回 LeaseID 能力 | runtime_operations.go |
| `GET /ops/reliability` | 独立管理 token；tenant_id | 全租户有界工作快照与累计计数，不返回消息正文 | runtime_operations.go |
| `GET /ops/task/incident` | 同 token；tenant_id/request_id | 单语句 metadata snapshot + 可选独立解密检查点投影；标注两者版本匹配状态 | runtime_operations.go / postgres/task_incident.go |
| `GET /ops/task/records` | 同 token；tenant_id/request_id/kind；limit=1..100，默认50；可选cursor | tools/attempts/commits/outbox/deliveries 稳定主键分页；每页独立观察 | postgres/task_records.go |
| `GET /ops/delivery/incident` | 同 token；tenant_id/delivery_key/segment_no | Ledger、可选持久回执、最近100条处置，不保证多读原子快照 | postgres/incident.go |
| `POST /ops/delivery/resolve` | 同 token；严格 JSON、最多8KiB；完整 DTO 见后文 | 204；不同版本/证据409；有接受回执不能否认已发送或改 Provider ID | postgres/operations.go |
| `GET /webui/` / app.js / style.css | 浏览器资源 | 页面资源，不是租户身份凭据或恢复数据源 | channels/webui/assets.go |

管理接口仅在 `TRPC_RUNTIME_OPERATIONS_TOKEN` 非空时挂载；请求头 `X-TRPC-Local-Token`。token 至少32字符、不能与 WebUI token 相同，响应 `Cache-Control: no-store`。输入错误通常400、未授权401、无记录404、依赖/解密错误503；处置竞争或同键不同证据409。`http.Error` 为文本错误响应，不应假定所有失败都有统一 JSON envelope。actor 请求字段只是标签，服务端加 `deployment-admin/`；部署 token 不是个人身份认证。

### 稳定 WeCom 入口

`wecom-ha-entry` 只处理自己的 `/livez`、`/readyz`、`/statusz` 与 WeCom callback 代理，不把管理接口/WebUI 任意路径公开转发。后端依据真实 `/readyz` 健康、观察新鲜度与恢复成功阈值选择，转发保留签名参数及原 body。网络错误或502/503/504可尝试另一后端；重复回调靠 Inbox 原消息键去重。入口自身是额外进程，当前单入口不是入口层冗余或宿主机容灾。

### 故障暂停控制面（只用于测试设施）

仅 FailoverTestEnabled 时挂载 `/test/failover/<point>/arm`（POST）、`status`（GET）、`release`（POST）。使用 WebUI token 的 X-TRPC-Local-Token，与生产管理 token 区分；未授权403，错方法405。arm 可限定 tenant_id/request_id。内存 barrier 快照必须在 kill 前采集，不能把它当成 durable 业务回执。

| point | 精确边界 |
|---|---|
| p1 | 原任务可靠接收/派发之后，实际执行前 |
| p2 | 真实模型响应请求已进入执行，尚未完成结果 |
| p2-commit | 模型/工具阶段结束后、terminal 前 |
| tool-effect | 工具外部效果/调用返回后、成功结果提交前 |
| result-before-commit | 公开冻结结果持久化后、terminal 前 |
| p3 | terminal 与待发回复已提交，Reply Relay 发布前 |
| p4-before-send | Ledger 领取后、渠道调用前，不能代替 P4 |
| p4 | 供应商成功接受并返回 ID 后、持久回执落库前 |
| p4-receipt | delivery_receipt 已保存、Ledger sent 前 |

## 内部合同的职责

| 层 | 真正负责的动作 | 必须区分的结果 |
|---|---|---|
| IngressBindingResolver / Adapter | 先取得受限验证能力，再验证协议身份，最后提升 VerifiedBinding | 路由提示不是已授权租户 |
| Inbox/Payload/Prepared Store | 原消息持久去重、加密输入、规范预处理结果 | 接收成功不是执行成功 |
| TaskStore / AtomicSessionStore | 固定 envelope、顺序、取消意图，提交终态及 Outbox | 已提交终态不是渠道可见 |
| Broker / LeaseManager | 可靠派发/回收与同会话执行资格 | Redis pending owner 与实际会话 lease owner不是同一概念 |
| Taskflow Store | 原目标、计划、步骤、预算、可信完成检查点 | 步骤成功不证明未配置的任意自然语言需求都满足 |
| CompletionStore | 固定私有答案候选与原治理结算意图 | 私有候选不是公开结果或可发送载荷 |
| 工具恢复/业务核验 | 稳定原操作、真实查询、结果认证、独立业务读回 | not_found 不自动授权换键；未知不等于未发生 |
| DeliveryLedger / ReceiptStore | 分段 claim、发送/核对、接受证据和恢复确认 | 内部一行/供应商接受不单独证明客户端恰好一次 |

以下签名完整列出当前相关 Go 接口；这是进程内合同，不是额外公开 RPC。返回 ErrNotFound 要与后端故障区分；ErrVersionConflict/ErrStaleFence/ErrLeaseLost 不允许强制覆盖。ErrIdempotencyCollision 表示同键不同内容，不可换键掩盖。新草稿 ErrIncomplete 可在原预算内修复；已保存候选的矛盾按恢复冲突收敛。Delivery DeferredError 保留 pending/not_before，TerminalError 允许已持久失败后 ACK，ACK 本身不是成功交付证明。


### preprocess.Store

[源码](../../../trpcservice/preprocess/preprocess.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `ClaimInboxAndSchedule` | `func(context.Context, ClaimRequest) (messaging.InboxRecord, Job, error)` |
| `ClaimJobs` | `func(context.Context, ClaimOptions) ([]Job, error)` |
| `FinishReady` | `func(context.Context, Job) (Job, error)` |
| `FinishRetry` | `func(context.Context, Job, time.Time, string) (Job, error)` |
| `FinishRejected` | `func(context.Context, Job, string) (Job, error)` |
| `ListReadyForDispatch` | `func(context.Context, int) ([]Job, error)` |
| `ClaimReadyForDispatch` | `func(context.Context, ClaimOptions) ([]Job, error)` |
| `MarkDispatched` | `func(context.Context, Job, time.Time) (Job, error)` |

### gateway.Dispatcher

[源码](../../../trpcservice/gateway/dispatch.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `Dispatch` | `func(context.Context, DispatchRequest) (ExecutionHandle, error)` |

### gateway.Executor

[源码](../../../trpcservice/gateway/dispatch.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `Execute` | `func(context.Context, runtime.ExecutionEnvelope) error` |

### ingress.IdentityMapper

[源码](../../../trpcservice/channels/ingress/pipeline.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `Map` | `func(context.Context, channel.VerifiedBinding, channel.ProviderEvent) (identity.Result, error)` |

### ingress.Verifier

[源码](../../../trpcservice/channels/ingress/pipeline.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `VerifyAndDecode` | `func(context.Context, channel.CallbackRequest) (VerifiedIngress, error)` |

### ingress.ConfigSelector

[源码](../../../trpcservice/channels/ingress/pipeline.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `SelectEffective` | `func(context.Context, string, int64) (config.Snapshot, error)` |

### worker.LeaseExecutor

[源码](../../../trpcservice/worker/consumer.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `ExecuteWithLease` | `func(context.Context, runtime.ExecutionEnvelope, uint64, func(context.Context) error) error` |

### worker.CancellationExecutor

[源码](../../../trpcservice/worker/consumer.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `CancelWithLease` | `func(context.Context, runtime.ExecutionEnvelope, uint64, func(context.Context) error) error` |

### worker.OutboundRenderer

[源码](../../../trpcservice/worker/outbound.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `RenderOutbound` | `func(context.Context, runtime.ExecutionEnvelope, string) (OutboundContent, error)` |

### governance.DecisionRecorder

[源码](../../../trpcservice/governance/service.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `RecordDecision` | `func(context.Context, Decision) error` |

### governance.RunGuard

[源码](../../../trpcservice/governance/service.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `Begin` | `func(context.Context, runtime.ExecutionEnvelope, VersionedRef, []byte) (RunPermit, error)` |
| `Finish` | `func(context.Context, RunPermit, Usage, []byte) (Decision, error)` |
| `Refund` | `func(context.Context, RunPermit, string) error` |
| `Abort` | `func(context.Context, runtime.ExecutionEnvelope, VersionedRef, string) error` |
| `Record` | `func(context.Context, Decision) error` |

### governance.ModelCandidateGuard

[源码](../../../trpcservice/governance/service.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `ValidateModelCandidates` | `func(context.Context, RunPermit, []VersionedRef) (Decision, error)` |

### governance.MultiModelRunGuard

[源码](../../../trpcservice/governance/service.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `FinishModels` | `func(context.Context, RunPermit, []ModelUsage, []byte) (Decision, error)` |

### governance.PreparedRunGuard

[源码](../../../trpcservice/governance/service.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `PrepareFinalization` | `func(context.Context, RunPermit, []ModelUsage, []byte) (Finalization, error)` |
| `CommitFinalization` | `func(context.Context, Finalization) (Decision, error)` |

### governance.Ledger

[源码](../../../trpcservice/governance/budget.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `Reserve` | `func(context.Context, ReserveRequest) (Reservation, error)` |
| `Settle` | `func(context.Context, SettleRequest) (Reservation, error)` |
| `Refund` | `func(context.Context, string, string, int64, string) (Reservation, error)` |
| `GetReservation` | `func(context.Context, string, string) (Reservation, error)` |

### governance.Repository

[源码](../../../trpcservice/governance/snapshot.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `GetPolicy` | `func(context.Context, string, int64) (PolicySnapshot, error)` |
| `GetPricing` | `func(context.Context, string, int64) (PricingSnapshot, error)` |

### governance.VersionedTool

[源码](../../../trpcservice/governance/snapshot.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `GovernanceToolRef` | `func() VersionedRef` |

### contract.ScopedVerifierHandle

[源码](../../../trpcservice/channels/contract/adapter.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `Verify` | `func(context.Context, CallbackRequest, ProtocolVerifier) (VerifiedCallback, VerificationReceipt, error)` |
| `Close` | `func() error` |

### contract.IngressBindingResolver

[源码](../../../trpcservice/channels/contract/adapter.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `ResolveCandidate` | `func(context.Context, PublicRouteHint) (CandidateBindingContext, error)` |
| `AcquireVerifier` | `func(context.Context, CandidateBindingContext) (ScopedVerifierHandle, error)` |
| `PromoteVerified` | `func(context.Context, CandidateBindingContext, VerificationReceipt) (VerifiedBinding, error)` |

### contract.TextSegmentLimit

[源码](../../../trpcservice/channels/contract/adapter.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `MaxTextBytes` | `func() int` |

### contract.ReplyPublisher

[源码](../../../trpcservice/channels/contract/adapter.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `PublishReply` | `func(context.Context, ReplyDestination, ReplyEvent) error` |

### contract.ReplyQueue

[源码](../../../trpcservice/channels/contract/adapter.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `ConsumeReplies` | `func(context.Context, ReplyDestination, ReplyConsumerOptions, func(context.Context, ReplyDelivery) error) error` |
| `AckReply` | `func(context.Context, ReplyDestination, ReplyDelivery) error` |
| `ReclaimReplies` | `func(context.Context, ReplyDestination, ReplyConsumerOptions) ([]ReplyDelivery, error)` |

### contract.DeliveryReconciler

[源码](../../../trpcservice/channels/contract/adapter.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `ReconcileDelivery` | `func(context.Context, ReconciliationRequest) (ReconciliationResult, error)` |

### contract.HTTPAdapter

[源码](../../../trpcservice/channels/contract/adapter.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `(embed)` | `Adapter` |
| `IsChallenge` | `func(CallbackRequest) bool` |
| `PublicChallengeRoute` | `func(context.Context, CallbackRequest) (PublicRouteHint, error)` |
| `VerifyChallenge` | `func(context.Context, CallbackRequest, ScopedVerifierHandle) (HTTPResponse, VerificationReceipt, error)` |
| `CallbackACK` | `func() HTTPResponse` |

### contract.Adapter

[源码](../../../trpcservice/channels/contract/adapter.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `ID` | `func() string` |
| `Run` | `func(context.Context) error` |
| `PublicRoute` | `func(context.Context, CallbackRequest) (PublicRouteHint, error)` |
| `Verify` | `func(context.Context, CallbackRequest, ScopedVerifierHandle) (VerifiedCallback, VerificationReceipt, error)` |
| `Decode` | `func(context.Context, VerifiedCallback) ([]ProviderEvent, error)` |
| `Deliver` | `func(context.Context, DeliveryRequest) (DeliveryResult, error)` |
| `Capabilities` | `func() Capabilities` |

### messaging.InboxClaimer

[源码](../../../trpcservice/storage/messaging/messaging.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `ClaimInbox` | `func(context.Context, ClaimInboxRequest) (InboxRecord, error)` |
| `GetInbox` | `func(context.Context, InboxKey) (InboxRecord, error)` |

### messaging.PayloadKeyResolver

[源码](../../../trpcservice/storage/messaging/messaging.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `ResolvePayloadKey` | `func(context.Context, string, int64) (PayloadCipherKey, error)` |

### messaging.PayloadStore

[源码](../../../trpcservice/storage/messaging/messaging.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `PutPayload` | `func(context.Context, PayloadRecord) error` |
| `GetPayload` | `func(context.Context, string, string) (PayloadRecord, error)` |

### messaging.PreparedPayloadStore

[源码](../../../trpcservice/storage/messaging/messaging.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `PutPreparedPayload` | `func(context.Context, PreparedPayloadRecord) error` |
| `GetPreparedPayload` | `func(context.Context, string, string, string) (PayloadRecord, error)` |

### messaging.ResultStore

[源码](../../../trpcservice/storage/messaging/messaging.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `PutResult` | `func(context.Context, ResultRecord) error` |
| `GetResult` | `func(context.Context, string, string) (ResultRecord, error)` |

### messaging.ToolResultStore

[源码](../../../trpcservice/storage/messaging/messaging.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `PutToolResult` | `func(context.Context, ToolResultRecord) error` |
| `GetToolResult` | `func(context.Context, string, string) (ToolResultRecord, error)` |

### messaging.InteractionStore

[源码](../../../trpcservice/storage/messaging/messaging.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `PutInteraction` | `func(context.Context, InteractionRecord) error` |
| `GetReplyContent` | `func(context.Context, string, string, string) (ResultRecord, error)` |

### messaging.ReplyRouteStore

[源码](../../../trpcservice/storage/messaging/messaging.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `ResolveReplyRoute` | `func(context.Context, string, string) (ReplyRoute, error)` |

### messaging.OutboxStore

[源码](../../../trpcservice/storage/messaging/messaging.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `ClaimOutbox` | `func(context.Context, string, int, string, time.Time) ([]OutboxRecord, error)` |
| `RenewOutboxClaim` | `func(context.Context, string, string, uint64, string, time.Time) (uint64, error)` |
| `MarkPublished` | `func(context.Context, string, string, uint64) error` |
| `MarkRetry` | `func(context.Context, string, string, uint64, time.Time) error` |

### messaging.ReconciliationStore

[源码](../../../trpcservice/storage/messaging/messaging.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `FindReconciliationIssues` | `func(context.Context, time.Time, int) ([]ReconciliationIssue, error)` |

### messaging.DeliveryLedger

[源码](../../../trpcservice/storage/messaging/messaging.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `GetDelivery` | `func(context.Context, DeliveryKey) (DeliveryRecord, error)` |
| `ClaimDelivery` | `func(context.Context, DeliveryKey, DeliveryPlan, DeliveryClaim) (DeliveryRecord, bool, error)` |
| `RenewDeliveryClaim` | `func(context.Context, DeliveryRecord, time.Duration) (DeliveryRecord, error)` |
| `FinishDelivery` | `func(context.Context, DeliveryRecord, int64) (DeliveryRecord, error)` |
| `DeferDeliveryReconciliation` | `func(context.Context, DeliveryRecord, int64) (DeliveryRecord, error)` |
| `ReconcileDelivery` | `func(context.Context, DeliveryRecord, int64) (DeliveryRecord, error)` |

### messaging.CompletionStore

[源码](../../../trpcservice/storage/messaging/completion.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `PutCompletion` | `func(context.Context, CompletionRecord) error` |
| `GetCompletion` | `func(context.Context, string, string) (CompletionRecord, error)` |

### messaging.ProviderReceiptStore

[源码](../../../trpcservice/storage/messaging/receipt.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `PutProviderReceipt` | `func(context.Context, DeliveryRecord, ProviderReceipt) error` |
| `GetProviderReceipt` | `func(context.Context, DeliveryKey) (ProviderReceipt, error)` |

### session.AtomicSessionStore

[源码](../../../trpcservice/storage/session/atomic.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `OpenForRun` | `func(context.Context, OpenForRunRequest) (SessionHead, error)` |
| `CommitTurn` | `func(context.Context, CommitTurnRequest) (CommitTurnResult, error)` |
| `GetTerminalByInputSeq` | `func(context.Context, TerminalKey) (CommitTurnResult, error)` |
| `ReadLastFence` | `func(context.Context, SessionKey) (uint64, error)` |
| `LoadSession` | `func(context.Context, SessionKey) (SessionSnapshot, error)` |

### session.TerminalHistoryReader

[源码](../../../trpcservice/storage/session/terminal_history.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `ListTerminalHistory` | `func(context.Context, SessionKey, uint64, int) ([]TerminalHistory, error)` |

### coordination.LeaseManager

[源码](../../../trpcservice/coordination/lease.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `Acquire` | `func(context.Context, SessionKey, string, time.Duration) (Lease, error)` |
| `Renew` | `func(context.Context, Lease, time.Duration) (Lease, error)` |
| `Release` | `func(context.Context, Lease) error` |
| `EnsureFenceAtLeast` | `func(context.Context, SessionKey, uint64) error` |

### coordination.LeaseInspector

[源码](../../../trpcservice/coordination/lease.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `Inspect` | `func(context.Context, SessionKey) (LeaseObservation, error)` |

### broker.Broker

[源码](../../../trpcservice/broker/broker.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `Publish` | `func(context.Context, Shard, runtime.ExecutionEnvelope) error` |
| `Consume` | `func(context.Context, ConsumerOptions, func(context.Context, Delivery) error) error` |
| `Ack` | `func(context.Context, Delivery) error` |
| `Reclaim` | `func(context.Context, ReclaimOptions) ([]Delivery, error)` |

### gateway.InputParker

[源码](../../../trpcservice/gateway/taskstore.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `ParkInput` | `func(context.Context, ParkRequest) (ParkResult, error)` |

### gateway.WakeupStore

[源码](../../../trpcservice/gateway/taskstore.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `InspectWakeup` | `func(context.Context, ExecutionKey) (WakeupCandidate, error)` |
| `MarkWoken` | `func(context.Context, ExecutionKey, int64) error` |

### gateway.ParkedInputStore

[源码](../../../trpcservice/gateway/taskstore.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `(embed)` | `WakeupStore` |
| `ListActionableParkedInputs` | `func(context.Context, time.Time, int) ([]ExecutionKey, error)` |

### gateway.ExecutionReader

[源码](../../../trpcservice/gateway/taskstore.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `GetExecution` | `func(context.Context, ExecutionKey) (ExecutionStatus, error)` |

### gateway.TaskStore

[源码](../../../trpcservice/gateway/taskstore.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `PrepareDispatch` | `func(context.Context, PrepareDispatchRequest) (PreparedDispatch, error)` |
| `(embed)` | `ExecutionReader` |
| `RequestCancel` | `func(context.Context, CancelRequest) (CancelResult, error)` |
| `(embed)` | `InputParker` |

### gateway.ModelCallCounter

[源码](../../../trpcservice/gateway/taskstore.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `RecordModelCall` | `func(context.Context, ExecutionKey) error` |

### gateway.ModelRequestCounter

[源码](../../../trpcservice/gateway/taskstore.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `RecordModelRequest` | `func(context.Context, ExecutionKey) error` |

### gateway.ExecutionAttemptStore

[源码](../../../trpcservice/gateway/taskstore.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `BeginExecutionAttempt` | `func(context.Context, ExecutionKey, string, uint64) (ExecutionAttempt, error)` |
| `FinishExecutionAttempt` | `func(context.Context, ExecutionAttempt, bool) error` |

### taskflow.Store

[源码](../../../trpcservice/taskflow/taskflow.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `Open` | `func(context.Context, Scope, string, int64) (Plan, error)` |
| `Get` | `func(context.Context, Key) (Plan, error)` |
| `Save` | `func(context.Context, Scope, int64, Plan) (Plan, error)` |

### taskflow.EvidenceDescriptor

[源码](../../../trpcservice/taskflow/policy.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `TaskEvidenceCheck` | `func() *ResultCheck` |

### tool.RecoveryLookup

[源码](../../../trpcservice/tool/recovery.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `RecoveryLookupEnabled` | `func() bool` |
| `LookupRecovery` | `func(context.Context, string) (RecoveryResult, error)` |

### tool.RecoveryDescriptor

[源码](../../../trpcservice/tool/recovery.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `RecoveryPolicy` | `func() RecoveryPolicy` |
| `RecoveryKey` | `func(context.Context) (string, error)` |

### tool.RecoveryQuerier

[源码](../../../trpcservice/tool/recovery.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `(embed)` | `RecoveryDescriptor` |
| `QueryRecovery` | `func(context.Context, string) (result any, completed bool, err error)` |

### tool.BusinessResultVerifier

[源码](../../../trpcservice/tool/business_verification.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `VerifyBusinessResult` | `func(context.Context, []byte, []byte) error` |

### taskflow.FinalEvidenceDescriptor

[源码](../../../trpcservice/taskflow/final_evidence.go)

| 方法 / 嵌入合同 | 精确 Go 签名 |
|---|---|
| `TaskFinalEvidenceChecks` | `func() []*ResultCheck` |

## 对象与逐字段合同

下表保留 JSON tag；未声明 tag 的内部对象若直接 json.Marshal，默认使用导出字段名。不能为了文档好看擅自把它们全部改成 snake_case。ExecutionEnvelope 另有自定义 MarshalJSON：全零 ExecutionBudget 省略，避免旧 schema-v1 读取器遇到未知字段；新增预算须按升级顺序启用。ReplyEvent 在当前 Redis Publisher 中直接 JSON 编码，外层另保存校验用路由字段。

Scope/KeyVersion 等标记 `json:"-"` 的字段不进入加密正文，读取时由受信数据库行/当前上下文补回；省略不是可以不校验。

### preprocess.ClaimRequest

[源码](../../../trpcservice/preprocess/preprocess.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Inbox` | `messaging.ClaimInboxRequest` | 未单独声明 | 原消息的可信 ClaimInboxRequest，不能用模型输入替换tenant/account/message身份。 |
| `TenantVersion` | `int64` | 未单独声明 | 接收/执行时固定的租户控制版本，拒绝错误版本接管。 |
| `ConfigVersion` | `int64` | 未单独声明 | 原接收/路由冻结的配置快照版本，投递也按原版本解析。 |
| `ChannelBindingID` | `string` | 未单独声明 | 本输入/确认/回复实际关联的 Bot Binding。 |
| `UserID` | `string` | 未单独声明 | 服务端稳定用户身份，不能拿另一个租户的派生结果复用。 |
| `TraceParent` | `string` | 未单独声明 | 仅用于观测的 W3C 追踪传播头。 |

### preprocess.Job

[源码](../../../trpcservice/preprocess/preprocess.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `JobID` | `string` | 未单独声明 | 预处理任务稳定 ID。 |
| `PayloadRef` | `string` | 未单独声明 | 原输入/事件载荷的持久引用，不是发送成功证据。 |
| `PreparedPayloadRef` | `string` | 未单独声明 | 预处理成功后的规范输入引用。 |
| `AgentAppID` | `string` | 未单独声明 | 租户内应用身份，参与会话、执行及发布版本隔离。 |
| `SessionID` | `string` | 未单独声明 | 服务端稳定会话身份，故障接管保留该坐标。 |
| `UserID` | `string` | 未单独声明 | 服务端稳定用户身份，不能拿另一个租户的派生结果复用。 |
| `Channel` | `string` | 未单独声明 | 渠道类型，和外部账号、消息 ID 共同确定接收去重边界。 |
| `ChannelBindingID` | `string` | 未单独声明 | 本输入/确认/回复实际关联的 Bot Binding。 |
| `TraceParent` | `string` | 未单独声明 | 仅用于观测的 W3C 追踪传播头。 |
| `TenantVersion` | `int64` | 未单独声明 | 接收/执行时固定的租户控制版本，拒绝错误版本接管。 |
| `ConfigVersion` | `int64` | 未单独声明 | 原接收/路由冻结的配置快照版本，投递也按原版本解析。 |
| `State` | `State` | 未单独声明 | 本表状态或 SDK 状态 JSON；实际类型、约束和本表职责共同解释。 |
| `Attempt` | `int` | 未单独声明 | 本表工作尝试计数；不是模型调用次数或用户可见回复次数。 |
| `Version` | `int64` | 未单独声明 | 本行的乐观锁/控制版本；不可与 fence、修订、配置版本混用。 |
| `LeaseOwner` | `string` | 未单独声明 | 当前租约所有者；不以节点名称猜测实际执行者。 |
| `RejectReason` | `string` | 未单独声明 | 预处理拒绝原因，不能等同于外部操作未发生。 |
| `LeaseUntil` | `time.Time` | 未单独声明 | 数据库工具/预处理工作租约期限。 |
| `NotBefore` | `time.Time` | 未单独声明 | 该记录下次允许处理/核对的最早时间。 |
| `CreatedAt` | `time.Time` | 未单独声明 | 该持久记录的创建时间；不是下游最终可见时刻。 |
| `UpdatedAt` | `time.Time` | 未单独声明 | 该持久记录最近更新时刻。 |
| `DispatchedAt` | `time.Time` | 未单独声明 | 预处理输入完成可靠派发的时间。 |

### preprocess.ClaimOptions

[源码](../../../trpcservice/preprocess/preprocess.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Owner` | `string` | 未单独声明 | 当前数据库领取者/角色身份。 |
| `Now` | `time.Time` | 未单独声明 | 本次数据库领取/重试的观察时刻，和固定业务deadline分别解释。 |
| `TTL` | `time.Duration` | 未单独声明 | 本次租约/claim 的存活期限，非整个任务业务期限。 |
| `Limit` | `int` | 未单独声明 | 本次有界查询上限；分页默认 50，最大 100。 |

### gateway.ExecutionHandle

[源码](../../../trpcservice/gateway/dispatch.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `Status` | `string` | 未单独声明 | 控制面对象的当前状态，disabled/suspended 等不等于节点死亡。 |
| `ReplyCursor` | `string` | 未单独声明 | 平台回复进度引用；不能单独证明渠道最终可见。 |

### gateway.DispatchRequest

[源码](../../../trpcservice/gateway/dispatch.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Tenant` | `tenant.Context` | 未单独声明 | 可信租户上下文，携带原租户/配置版本及授权边界。 |
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `SessionID` | `string` | 未单独声明 | 服务端稳定会话身份，故障接管保留该坐标。 |
| `UserID` | `string` | 未单独声明 | 服务端稳定用户身份，不能拿另一个租户的派生结果复用。 |
| `PayloadRef` | `string` | 未单独声明 | 原输入/事件载荷的持久引用，不是发送成功证据。 |
| `TraceParent` | `string` | 未单独声明 | 仅用于观测的 W3C 追踪传播头。 |
| `ConfigVersion` | `int64` | 未单独声明 | 原接收/路由冻结的配置快照版本，投递也按原版本解析。 |

### ingress.AcceptedEvent

[源码](../../../trpcservice/channels/ingress/pipeline.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `PayloadRef` | `string` | 未单独声明 | 原输入/事件载荷的持久引用，不是发送成功证据。 |
| `PreprocessJobID` | `string` | 未单独声明 | 原输入可回收的预处理任务ID。 |

### ingress.VerifiedIngress

[源码](../../../trpcservice/channels/ingress/service.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Binding` | `channel.VerifiedBinding` | 未单独声明 | 经过验证或版本化解析的固定Bot/应用绑定，不是公开路由提示。 |
| `Events` | `[]channel.ProviderEvent` | 未单独声明 | 平台协调事件或 SDK 快照事件数组；以该对象用途为准，当前组合根不镜像 SDK 原事件。 |
| `TraceParent` | `string` | 未单独声明 | 仅用于观测的 W3C 追踪传播头。 |

### runtime.ExecutionBudget

[源码](../../../trpcservice/runtime/execution_budget.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `MaxLLMCalls` | `int` | ``json:"max_llm_calls,omitempty"`` | 服务端模型逻辑调用预算，0 的含义由执行预算合并规则决定。 |
| `MaxToolCalls` | `int` | ``json:"max_tool_calls,omitempty"`` | 服务端工具调用预算，含受预算控制的检查点工具。 |
| `MaxParallelTools` | `int` | ``json:"max_parallel_tools,omitempty"`` | 同时执行工具数的预算上限。 |
| `ExecutionTimeoutSeconds` | `int` | ``json:"execution_timeout_seconds,omitempty"`` | 服务端固定执行时限；与任务整体年龄预算分别约束。 |

### worker.completionCandidate

[源码](../../../trpcservice/worker/completion.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `SchemaVersion` | `int` | 未单独声明 | 对象序列化格式版本，不是数据库迁移版本。 |
| `Envelope` | `runtime.ExecutionEnvelope` | 未单独声明 | 原始固定 ExecutionEnvelope；候选恢复完整交叉核对，不能只比较 request ID。 |
| `InputDigest` | `string` | 未单独声明 | 本任务原始持久输入完整字节 SHA-256。 |
| `Permit` | `*governance.RunPermit` | 未单独声明 | 原预算预约和允许执行的治理凭据，不在恢复时重新申请。 |
| `Prepared` | `*governance.Finalization` | 未单独声明 | 原只读审核结果与结算意图，候选先存后才提交结算。 |
| `Usage` | `[]governance.ModelUsage` | 未单独声明 | 按实际模型分组的用量，恢复按原用量幂等结算。 |
| `Output` | `OutboundContent` | 未单独声明 | 已经准备/审核的最终内容和类型，候选阶段仍非公开结果。 |
| `PlanDigest` | `string` | 未单独声明 | 候选所绑定的原输入、合同、计划及完成证据摘要。 |

### worker.OutboundContent

[源码](../../../trpcservice/worker/outbound.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Content` | `[]byte` | 未单独声明 | SDK 事件/摘要内容或审计载荷，依本表用途解释，不等于最终已发送答案。 |
| `ContentType` | `string` | 未单独声明 | text/plain、卡片 JSON 或受支持图片类型，恢复保持原类型。 |

### preprocess.NormalizedInput

[源码](../../../trpcservice/preprocess/worker.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `ExternalMessageID` | `string` | ``json:"external_message_id"`` | 渠道原消息 ID；相同账号内重复回调用它去重。 |
| `ExternalUserID` | `string` | ``json:"external_user_id"`` | 渠道侧用户标识；内部 user_id 由租户密钥派生。 |
| `ExternalChatID` | `string` | ``json:"external_chat_id"`` | 渠道侧会话/群标识；与外部用户一起构造回复目的地。 |
| `ChannelBindingID` | `string` | ``json:"channel_binding_id,omitempty"`` | 本输入/确认/回复实际关联的 Bot Binding。 |
| `ExternalAccountID` | `string` | ``json:"external_account_id,omitempty"`` | 渠道侧 Bot/应用账号，既是身份域也是回送目标组成部分。 |
| `ConfigVersion` | `int64` | ``json:"config_version,omitempty"`` | 原接收/路由冻结的配置快照版本，投递也按原版本解析。 |
| `MessageType` | `string` | ``json:"message_type,omitempty"`` | 规范文本/图片/文件等消息类型。 |
| `Text` | `string` | ``json:"text,omitempty"`` | 本消息的文本内容，仅在已验证/已准备作用域中交给模型。 |
| `MediaRefs` | `[]channel.MediaRef` | ``json:"media_refs,omitempty"`` | 原渠道媒体引用，仅入口/预处理接收；Worker 拒绝未经准备的原始媒体引用。 |

### preprocess.PreparedMedia

[源码](../../../trpcservice/preprocess/worker.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `ArtifactID` | `string` | ``json:"artifact_id"`` | 媒体/附件持久对象身份。 |
| `ArtifactRef` | `string` | ``json:"artifact_ref"`` | 解密/规范输入使用的附件引用。 |
| `Kind` | `string` | ``json:"kind"`` | Outbox、后端或事件类型；具体表内枚举约束为准。 |
| `MediaType` | `string` | ``json:"media_type"`` | 媒体 MIME 类型。 |
| `ContentDigest` | `string` | ``json:"content_digest"`` | 该表内容的 SHA-256；范围以表的编码契约为准。 |
| `Size` | `int64` | ``json:"size"`` | 已准备媒体大小，单位字节；必须为正数。 |

### preprocess.PreparedInput

[源码](../../../trpcservice/preprocess/worker.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `ExternalMessageID` | `string` | ``json:"external_message_id"`` | 渠道原消息 ID；相同账号内重复回调用它去重。 |
| `ExternalUserID` | `string` | ``json:"external_user_id"`` | 渠道侧用户标识；内部 user_id 由租户密钥派生。 |
| `ExternalChatID` | `string` | ``json:"external_chat_id"`` | 渠道侧会话/群标识；与外部用户一起构造回复目的地。 |
| `MessageType` | `string` | ``json:"message_type,omitempty"`` | 规范文本/图片/文件等消息类型。 |
| `Text` | `string` | ``json:"text,omitempty"`` | 本消息的文本内容，仅在已验证/已准备作用域中交给模型。 |
| `Media` | `[]PreparedMedia` | ``json:"media,omitempty"`` | 只包含已扫描/已持久化的规范媒体引用，Worker 拒绝原始媒体 URL。 |

### governance.RunPermit

[源码](../../../trpcservice/governance/service.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Policy` | `PolicySnapshot` | 未单独声明 | 原执行固定的治理策略快照。 |
| `Model` | `VersionedRef` | 未单独声明 | 实际使用的精确模型配置引用。 |
| `Decision` | `Decision` | 未单独声明 | 原治理决定或本阶段的审核结论，必须与 tenant/request/policy 一致。 |
| `Reservation` | `Reservation` | 未单独声明 | 原请求已有预算预约，接管重放同预约。 |

### governance.ModelUsage

[源码](../../../trpcservice/governance/service.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Model` | `VersionedRef` | 未单独声明 | 实际使用的精确模型配置引用。 |
| `Usage` | `Usage` | 未单独声明 | 按实际模型分组的用量，恢复按原用量幂等结算。 |

### governance.Finalization

[源码](../../../trpcservice/governance/service.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Settlement` | `SettleRequest` | 未单独声明 | 固定结算请求，先存候选后才写治理账本。 |
| `Decision` | `Decision` | 未单独声明 | 原治理决定或本阶段的审核结论，必须与 tenant/request/policy 一致。 |
| `OutputDigest` | `string` | 未单独声明 | 已审核的最终答案字节摘要。 |

### governance.Reservation

[源码](../../../trpcservice/governance/budget.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `ReservationID` | `string` | 未单独声明 | 本次原请求的预算预约 ID。 |
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `ResourceID` | `string` | 未单独声明 | 治理或计量资源 ID，例如固定模型配置。 |
| `AttemptClass` | `string` | 未单独声明 | 用量对应的调用/重试分类。 |
| `PolicyVersion` | `int64` | 未单独声明 | 本次请求固定的治理策略版本。 |
| `PricingVersion` | `int64` | 未单独声明 | 本次计费固定的价格版本。 |
| `ReservedCostMicros` | `int64` | 未单独声明 | 原预算预约的费用额度。 |
| `ReservedTokens` | `int64` | 未单独声明 | 原预算预约冻结的 token 数量。 |
| `ActualCostMicros` | `int64` | 未单独声明 | 预约结算时的实际费用。 |
| `InputTokens` | `int64` | 未单独声明 | 本条记录对应输入 token 数量。 |
| `OutputTokens` | `int64` | 未单独声明 | 本条记录对应输出 token 数量。 |
| `State` | `ReservationState` | 未单独声明 | 本表状态或 SDK 状态 JSON；实际类型、约束和本表职责共同解释。 |
| `Version` | `int64` | 未单独声明 | 本行的乐观锁/控制版本；不可与 fence、修订、配置版本混用。 |

### governance.Usage

[源码](../../../trpcservice/governance/budget.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `InputTokens` | `int64` | 未单独声明 | 本条记录对应输入 token 数量。 |
| `OutputTokens` | `int64` | 未单独声明 | 本条记录对应输出 token 数量。 |
| `CachedInputTokens` | `int64` | 未单独声明 | 供应商报告的缓存命中输入 token 数量。 |

### governance.SettleRequest

[源码](../../../trpcservice/governance/budget.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `ReservationID` | `string` | 未单独声明 | 本次原请求的预算预约 ID。 |
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `Stage` | `string` | 未单独声明 | waiting 或 terminal 等提交/治理阶段。 |
| `UsageKind` | `string` | 未单独声明 | 用量账本中该行的计量类型。 |
| `ExpectedVersion` | `int64` | 未单独声明 | 人工处置针对的 Ledger 版本，防止并发裁决覆盖。 |
| `Usage` | `Usage` | 未单独声明 | 按实际模型分组的用量，恢复按原用量幂等结算。 |
| `ActualCostMicros` | `int64` | 未单独声明 | 预约结算时的实际费用。 |

### governance.Decision

[源码](../../../trpcservice/governance/snapshot.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `DecisionID` | `string` | 未单独声明 | 同一治理阶段的稳定决策幂等 ID。 |
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `Stage` | `string` | 未单独声明 | waiting 或 terminal 等提交/治理阶段。 |
| `Action` | `Action` | 未单独声明 | 治理 allow/deny/ask 等动作，不是任务最终执行结果。 |
| `ReasonCode` | `string` | 未单独声明 | 稳定控制或治理原因码，不是任意模型解释。 |
| `PolicyVersion` | `int64` | 未单独声明 | 本次请求固定的治理策略版本。 |
| `RuleIDs` | `[]string` | 未单独声明 | 本次原治理决定命中的固定规则 ID 列表。 |
| `ReservationID` | `string` | 未单独声明 | 本次原请求的预算预约 ID。 |

### runtime.ExecutionEnvelope

[源码](../../../trpcservice/runtime/envelope.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `SchemaVersion` | `uint16` | ``json:"schema_version"`` | 对象序列化格式版本，不是数据库迁移版本。 |
| `TenantID` | `string` | ``json:"tenant_id"`` | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `TenantVersion` | `int64` | ``json:"tenant_version"`` | 接收/执行时固定的租户控制版本，拒绝错误版本接管。 |
| `AgentAppID` | `string` | ``json:"agent_app_id"`` | 租户内应用身份，参与会话、执行及发布版本隔离。 |
| `AgentAppVersion` | `int64` | ``json:"agent_app_version"`` | 接收时固定的应用控制版本。 |
| `AgentAppRevision` | `int64` | ``json:"agent_app_revision"`` | 实际执行的已发布应用内容修订号。 |
| `AgentContentDigest` | `string` | ``json:"agent_content_digest"`` | 已发布应用行为内容摘要，防止相同版本引用被替换。 |
| `ConfigVersion` | `int64` | ``json:"config_version"`` | 原接收/路由冻结的配置快照版本，投递也按原版本解析。 |
| `PolicyVersion` | `int64` | ``json:"policy_version"`` | 本次请求固定的治理策略版本。 |
| `ExecutionBudget` | `ExecutionBudget` | ``json:"execution_budget"`` | 已发布并冻结的模型/工具/并行/执行时间预算。 |
| `RequestID` | `string` | ``json:"request_id"`` | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `SessionID` | `string` | ``json:"session_id"`` | 服务端稳定会话身份，故障接管保留该坐标。 |
| `UserID` | `string` | ``json:"user_id"`` | 服务端稳定用户身份，不能拿另一个租户的派生结果复用。 |
| `Channel` | `string` | ``json:"channel"`` | 渠道类型，和外部账号、消息 ID 共同确定接收去重边界。 |
| `InputSeq` | `uint64` | ``json:"input_seq"`` | 平台同一会话内分配的输入顺序号。 |
| `PayloadRef` | `string` | ``json:"payload_ref"`` | 原输入/事件载荷的持久引用，不是发送成功证据。 |
| `TraceParent` | `string` | ``json:"traceparent,omitempty"`` | 仅用于观测的 W3C 追踪传播头。 |
| `CreatedAt` | `time.Time` | ``json:"created_at"`` | 该持久记录的创建时间；不是下游最终可见时刻。 |

### runtime.ExecutionContext

[源码](../../../trpcservice/runtime/context.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `SubjectID` | `string` | 未单独声明 | 该工具确认或授权对应的服务端用户主体。 |
| `PolicyVersion` | `int64` | 未单独声明 | 本次请求固定的治理策略版本。 |
| `GrantID` | `string` | 未单独声明 | 一次性工具执行授权身份，必须核对原请求/参数/用户/策略。 |
| `GrantVersion` | `int64` | 未单独声明 | 本次已批准工具授权的预期控制版本。 |
| `ToolCallID` | `string` | 未单独声明 | 工具逻辑操作身份；普通 allow 调用稳定派生，ask 沿原确认 ID。 |
| `ArgsDigest` | `string` | 未单独声明 | 工具规范 JSON 参数摘要，改变参数不能冒充原操作。 |
| `PayloadKeyVersion` | `int64` | 未单独声明 | 本原请求固定的租户载荷加密密钥版本。 |

### contract.VerifiedBinding

[源码](../../../trpcservice/channels/contract/adapter.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `AgentAppID` | `string` | 未单独声明 | 租户内应用身份，参与会话、执行及发布版本隔离。 |
| `ChannelBindingID` | `string` | 未单独声明 | 本输入/确认/回复实际关联的 Bot Binding。 |
| `Channel` | `string` | 未单独声明 | 渠道类型，和外部账号、消息 ID 共同确定接收去重边界。 |
| `ExternalAccountID` | `string` | 未单独声明 | 渠道侧 Bot/应用账号，既是身份域也是回送目标组成部分。 |
| `TenantVersion` | `int64` | 未单独声明 | 接收/执行时固定的租户控制版本，拒绝错误版本接管。 |
| `BindingVersion` | `int64` | 未单独声明 | 绑定自身的控制版本，用于防止过期接入能力生效。 |
| `IdentitySecretRef` | `secrets.SecretRef` | 未单独声明 | 用于内部用户身份派生的租户密钥引用。 |
| `SessionSecretRef` | `secrets.SecretRef` | 未单独声明 | 用于内部会话身份派生的租户密钥引用。 |

### contract.ProviderEvent

[源码](../../../trpcservice/channels/contract/adapter.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `SchemaVersion` | `uint16` | 未单独声明 | 对象序列化格式版本，不是数据库迁移版本。 |
| `Channel` | `string` | 未单独声明 | 渠道类型，和外部账号、消息 ID 共同确定接收去重边界。 |
| `ExternalAccountID` | `string` | 未单独声明 | 渠道侧 Bot/应用账号，既是身份域也是回送目标组成部分。 |
| `ExternalMessageID` | `string` | 未单独声明 | 渠道原消息 ID；相同账号内重复回调用它去重。 |
| `ConversationType` | `string` | 未单独声明 | dm/p2p/group 等外部会话形态，决定内部会话键派生。 |
| `ExternalUserID` | `string` | 未单独声明 | 渠道侧用户标识；内部 user_id 由租户密钥派生。 |
| `ExternalChatID` | `string` | 未单独声明 | 渠道侧会话/群标识；与外部用户一起构造回复目的地。 |
| `MessageType` | `string` | 未单独声明 | 规范文本/图片/文件等消息类型。 |
| `Text` | `string` | 未单独声明 | 本消息的文本内容，仅在已验证/已准备作用域中交给模型。 |
| `MediaRefs` | `[]MediaRef` | 未单独声明 | 原渠道媒体引用，仅入口/预处理接收；Worker 拒绝未经准备的原始媒体引用。 |
| `OccurredAt` | `time.Time` | 未单独声明 | 事件发生时刻，与持久化创建时间分别记录。 |
| `TraceParent` | `string` | 未单独声明 | 仅用于观测的 W3C 追踪传播头。 |

### contract.ChannelEvent

[源码](../../../trpcservice/channels/contract/adapter.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `SchemaVersion` | `uint16` | 未单独声明 | 对象序列化格式版本，不是数据库迁移版本。 |
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `AgentAppID` | `string` | 未单独声明 | 租户内应用身份，参与会话、执行及发布版本隔离。 |
| `ChannelBindingID` | `string` | 未单独声明 | 本输入/确认/回复实际关联的 Bot Binding。 |
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `UserID` | `string` | 未单独声明 | 服务端稳定用户身份，不能拿另一个租户的派生结果复用。 |
| `SessionID` | `string` | 未单独声明 | 服务端稳定会话身份，故障接管保留该坐标。 |
| `Channel` | `string` | 未单独声明 | 渠道类型，和外部账号、消息 ID 共同确定接收去重边界。 |
| `ExternalMessageID` | `string` | 未单独声明 | 渠道原消息 ID；相同账号内重复回调用它去重。 |
| `MessageType` | `string` | 未单独声明 | 规范文本/图片/文件等消息类型。 |
| `Text` | `string` | 未单独声明 | 本消息的文本内容，仅在已验证/已准备作用域中交给模型。 |
| `MediaRefs` | `[]MediaRef` | 未单独声明 | 原渠道媒体引用，仅入口/预处理接收；Worker 拒绝未经准备的原始媒体引用。 |
| `PayloadRef` | `string` | 未单独声明 | 原输入/事件载荷的持久引用，不是发送成功证据。 |
| `TraceParent` | `string` | 未单独声明 | 仅用于观测的 W3C 追踪传播头。 |

### contract.ReplyEvent

[源码](../../../trpcservice/channels/contract/adapter.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `SchemaVersion` | `uint16` | 未单独声明 | 对象序列化格式版本，不是数据库迁移版本。 |
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `ChannelBindingID` | `string` | 未单独声明 | 本输入/确认/回复实际关联的 Bot Binding。 |
| `DeliveryKey` | `string` | 未单独声明 | 同一逻辑回复的稳定投递键，分段共用此键。 |
| `ConfigVersion` | `int64` | 未单独声明 | 原接收/路由冻结的配置快照版本，投递也按原版本解析。 |
| `EventSeq` | `uint64` | 未单独声明 | 该聚合/事件内的稳定序号。 |
| `Kind` | `string` | 未单独声明 | Outbox、后端或事件类型；具体表内枚举约束为准。 |
| `ContentRef` | `string` | 未单独声明 | 回复、摘要或业务内容引用，按租户解析对应持久材料。 |
| `ContentType` | `string` | 未单独声明 | text/plain、卡片 JSON 或受支持图片类型，恢复保持原类型。 |
| `Target` | `DeliveryTarget` | 未单独声明 | 固定渠道账号、用户、会话及适用原消息的回送目标。 |
| `Final` | `bool` | 未单独声明 | 该 ReplyEvent 是否代表最终语义，流式更新不冒充最终答案计数。 |
| `TraceParent` | `string` | 未单独声明 | 仅用于观测的 W3C 追踪传播头。 |

### contract.ReplyDestination

[源码](../../../trpcservice/channels/contract/adapter.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `Channel` | `string` | 未单独声明 | 渠道类型，和外部账号、消息 ID 共同确定接收去重边界。 |
| `ChannelBindingID` | `string` | 未单独声明 | 本输入/确认/回复实际关联的 Bot Binding。 |
| `ExternalAccountID` | `string` | 未单独声明 | 渠道侧 Bot/应用账号，既是身份域也是回送目标组成部分。 |
| `ConfigVersion` | `int64` | 未单独声明 | 原接收/路由冻结的配置快照版本，投递也按原版本解析。 |

### contract.DeliveryRequest

[源码](../../../trpcservice/channels/contract/adapter.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Event` | `ReplyEvent` | 未单独声明 | 官方 SDK 事件 JSON，原始草稿/工具轨迹不等于已批准答案。 |
| `ClientRequestID` | `string` | 未单独声明 | 发送本 segment 时使用的稳定供应商请求键。 |
| `Target` | `DeliveryTarget` | 未单独声明 | 固定渠道账号、用户、会话及适用原消息的回送目标。 |
| `Content` | `[]byte` | 未单独声明 | SDK 事件/摘要内容或审计载荷，依本表用途解释，不等于最终已发送答案。 |
| `ContentDigest` | `string` | 未单独声明 | 该表内容的 SHA-256；范围以表的编码契约为准。 |
| `ContentType` | `string` | 未单独声明 | text/plain、卡片 JSON 或受支持图片类型，恢复保持原类型。 |

### contract.DeliveryTarget

[源码](../../../trpcservice/channels/contract/adapter.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Channel` | `string` | 未单独声明 | 渠道类型，和外部账号、消息 ID 共同确定接收去重边界。 |
| `ExternalAccountID` | `string` | 未单独声明 | 渠道侧 Bot/应用账号，既是身份域也是回送目标组成部分。 |
| `ExternalMessageID` | `string` | 未单独声明 | 渠道原消息 ID；相同账号内重复回调用它去重。 |
| `ExternalChatID` | `string` | 未单独声明 | 渠道侧会话/群标识；与外部用户一起构造回复目的地。 |
| `ExternalUserID` | `string` | 未单独声明 | 渠道侧用户标识；内部 user_id 由租户密钥派生。 |

### contract.DeliveryResult

[源码](../../../trpcservice/channels/contract/adapter.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `ProviderMessageID` | `string` | 未单独声明 | 渠道正常接受后返回的消息 ID，不单独证明客户端展示。 |
| `Delivered` | `bool` | 未单独声明 | 适配器明确返回渠道已接受的成功标志，不等同于客户端已展示。 |

### contract.ReconciliationRequest

[源码](../../../trpcservice/channels/contract/adapter.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Event` | `ReplyEvent` | 未单独声明 | 官方 SDK 事件 JSON，原始草稿/工具轨迹不等于已批准答案。 |
| `ClientRequestID` | `string` | 未单独声明 | 发送本 segment 时使用的稳定供应商请求键。 |
| `ContentDigest` | `string` | 未单独声明 | 该表内容的 SHA-256；范围以表的编码契约为准。 |

### contract.ReconciliationResult

[源码](../../../trpcservice/channels/contract/adapter.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Status` | `ReconciliationStatus` | 未单独声明 | 控制面对象的当前状态，disabled/suspended 等不等于节点死亡。 |
| `ProviderMessageID` | `string` | 未单独声明 | 渠道正常接受后返回的消息 ID，不单独证明客户端展示。 |

### messaging.InboxKey

[源码](../../../trpcservice/storage/messaging/messaging.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `Channel` | `string` | 未单独声明 | 渠道类型，和外部账号、消息 ID 共同确定接收去重边界。 |
| `ExternalAccountID` | `string` | 未单独声明 | 渠道侧 Bot/应用账号，既是身份域也是回送目标组成部分。 |
| `ExternalMessageID` | `string` | 未单独声明 | 渠道原消息 ID；相同账号内重复回调用它去重。 |

### messaging.ClaimInboxRequest

[源码](../../../trpcservice/storage/messaging/messaging.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `(embed)` | `InboxKey` | 未单独声明 | 嵌入 `InboxKey` 的完整字段；按源类型展开，不能忽略其租户/请求作用域。 |
| `AgentAppID` | `string` | 未单独声明 | 租户内应用身份，参与会话、执行及发布版本隔离。 |
| `SessionID` | `string` | 未单独声明 | 服务端稳定会话身份，故障接管保留该坐标。 |
| `ExternalChatID` | `string` | 未单独声明 | 渠道侧会话/群标识；与外部用户一起构造回复目的地。 |
| `ExternalUserID` | `string` | 未单独声明 | 渠道侧用户标识；内部 user_id 由租户密钥派生。 |
| `PayloadDigest` | `string` | 未单独声明 | 原持久输入内容摘要，防止重复消息 ID 携带不同内容。 |
| `KeyVersion` | `int64` | 未单独声明 | 原载荷的租户加密密钥版本；恢复须仍能解析该版本。 |
| `InitialState` | `InboxState` | 未单独声明 | 创建 Inbox 时的初始工作状态，须与输入处理流程匹配。 |

### messaging.InboxRecord

[源码](../../../trpcservice/storage/messaging/messaging.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `(embed)` | `InboxKey` | 未单独声明 | 嵌入 `InboxKey` 的完整字段；按源类型展开，不能忽略其租户/请求作用域。 |
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `AgentAppID` | `string` | 未单独声明 | 租户内应用身份，参与会话、执行及发布版本隔离。 |
| `SessionID` | `string` | 未单独声明 | 服务端稳定会话身份，故障接管保留该坐标。 |
| `ExternalChatID` | `string` | 未单独声明 | 渠道侧会话/群标识；与外部用户一起构造回复目的地。 |
| `ExternalUserID` | `string` | 未单独声明 | 渠道侧用户标识；内部 user_id 由租户密钥派生。 |
| `InputSeq` | `uint64` | 未单独声明 | 平台同一会话内分配的输入顺序号。 |
| `State` | `InboxState` | 未单独声明 | 本表状态或 SDK 状态 JSON；实际类型、约束和本表职责共同解释。 |
| `PayloadRef` | `string` | 未单独声明 | 原输入/事件载荷的持久引用，不是发送成功证据。 |
| `PayloadDigest` | `string` | 未单独声明 | 原持久输入内容摘要，防止重复消息 ID 携带不同内容。 |
| `KeyVersion` | `int64` | 未单独声明 | 原载荷的租户加密密钥版本；恢复须仍能解析该版本。 |
| `Version` | `int64` | 未单独声明 | 本行的乐观锁/控制版本；不可与 fence、修订、配置版本混用。 |
| `TerminalReason` | `string` | 未单独声明 | 接收/派发阶段确定的终结原因。 |
| `ResultRef` | `string` | 未单独声明 | 不可变结果或工具结果的引用，必须核对同作用域正文摘要。 |
| `CreatedAt` | `time.Time` | 未单独声明 | 该持久记录的创建时间；不是下游最终可见时刻。 |
| `UpdatedAt` | `time.Time` | 未单独声明 | 该持久记录最近更新时刻。 |

### messaging.PayloadRecord

[源码](../../../trpcservice/storage/messaging/messaging.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `PayloadRef` | `string` | 未单独声明 | 原输入/事件载荷的持久引用，不是发送成功证据。 |
| `ContentDigest` | `string` | 未单独声明 | 该表内容的 SHA-256；范围以表的编码契约为准。 |
| `Content` | `[]byte` | 未单独声明 | SDK 事件/摘要内容或审计载荷，依本表用途解释，不等于最终已发送答案。 |
| `KeyVersion` | `int64` | 未单独声明 | 原载荷的租户加密密钥版本；恢复须仍能解析该版本。 |
| `CreatedAt` | `time.Time` | 未单独声明 | 该持久记录的创建时间；不是下游最终可见时刻。 |

### messaging.PreparedPayloadRecord

[源码](../../../trpcservice/storage/messaging/messaging.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `PayloadRef` | `string` | 未单独声明 | 原输入/事件载荷的持久引用，不是发送成功证据。 |
| `SourcePayloadRef` | `string` | 未单独声明 | 该规范输入源自的原始载荷引用。 |
| `ContentDigest` | `string` | 未单独声明 | 该表内容的 SHA-256；范围以表的编码契约为准。 |
| `Content` | `[]byte` | 未单独声明 | SDK 事件/摘要内容或审计载荷，依本表用途解释，不等于最终已发送答案。 |
| `KeyVersion` | `int64` | 未单独声明 | 原载荷的租户加密密钥版本；恢复须仍能解析该版本。 |
| `ArtifactRetention` | `time.Duration` | 未单独声明 | 该准备输入要求的附件保留期限。 |
| `ArtifactReferences` | `[]PreparedArtifactReference` | 未单独声明 | 准备结果持有的附件引用，恢复前保持有效。 |
| `CreatedAt` | `time.Time` | 未单独声明 | 该持久记录的创建时间；不是下游最终可见时刻。 |

### messaging.ResultRecord

[源码](../../../trpcservice/storage/messaging/messaging.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `ResultRef` | `string` | 未单独声明 | 不可变结果或工具结果的引用，必须核对同作用域正文摘要。 |
| `ContentDigest` | `string` | 未单独声明 | 该表内容的 SHA-256；范围以表的编码契约为准。 |
| `ContentType` | `string` | 未单独声明 | text/plain、卡片 JSON 或受支持图片类型，恢复保持原类型。 |
| `Content` | `[]byte` | 未单独声明 | SDK 事件/摘要内容或审计载荷，依本表用途解释，不等于最终已发送答案。 |
| `KeyVersion` | `int64` | 未单独声明 | 原载荷的租户加密密钥版本；恢复须仍能解析该版本。 |
| `CreatedAt` | `time.Time` | 未单独声明 | 该持久记录的创建时间；不是下游最终可见时刻。 |

### messaging.InteractionRecord

[源码](../../../trpcservice/storage/messaging/messaging.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `ContentRef` | `string` | 未单独声明 | 回复、摘要或业务内容引用，按租户解析对应持久材料。 |
| `ContentDigest` | `string` | 未单独声明 | 该表内容的 SHA-256；范围以表的编码契约为准。 |
| `ContentType` | `string` | 未单独声明 | text/plain、卡片 JSON 或受支持图片类型，恢复保持原类型。 |
| `Content` | `[]byte` | 未单独声明 | SDK 事件/摘要内容或审计载荷，依本表用途解释，不等于最终已发送答案。 |
| `KeyVersion` | `int64` | 未单独声明 | 原载荷的租户加密密钥版本；恢复须仍能解析该版本。 |
| `CreatedAt` | `time.Time` | 未单独声明 | 该持久记录的创建时间；不是下游最终可见时刻。 |

### messaging.ReplyCoordinate

[源码](../../../trpcservice/storage/messaging/messaging.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `Stage` | `string` | 未单独声明 | waiting 或 terminal 等提交/治理阶段。 |
| `InputSeq` | `uint64` | 未单独声明 | 平台同一会话内分配的输入顺序号。 |
| `Ordinal` | `uint64` | 未单独声明 | 同一父对象内的稳定排序号。 |

### messaging.ReplyRoute

[源码](../../../trpcservice/storage/messaging/messaging.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `Channel` | `string` | 未单独声明 | 渠道类型，和外部账号、消息 ID 共同确定接收去重边界。 |
| `ChannelBindingID` | `string` | 未单独声明 | 本输入/确认/回复实际关联的 Bot Binding。 |
| `ExternalAccountID` | `string` | 未单独声明 | 渠道侧 Bot/应用账号，既是身份域也是回送目标组成部分。 |
| `ExternalMessageID` | `string` | 未单独声明 | 渠道原消息 ID；相同账号内重复回调用它去重。 |
| `ExternalChatID` | `string` | 未单独声明 | 渠道侧会话/群标识；与外部用户一起构造回复目的地。 |
| `ExternalUserID` | `string` | 未单独声明 | 渠道侧用户标识；内部 user_id 由租户密钥派生。 |
| `ConfigVersion` | `int64` | 未单独声明 | 原接收/路由冻结的配置快照版本，投递也按原版本解析。 |

### messaging.OutboxRecord

[源码](../../../trpcservice/storage/messaging/messaging.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `OutboxID` | `string` | 未单独声明 | 持久待发布事件的主键，不是 Redis Stream ID。 |
| `Kind` | `string` | 未单独声明 | Outbox、后端或事件类型；具体表内枚举约束为准。 |
| `AggregateID` | `string` | 未单独声明 | Outbox 所属聚合对象；任务回复通常关联原 request_id。 |
| `IdempotencyKey` | `string` | 未单独声明 | 本表业务去重键；不能任意改变它绕过不确定效果。 |
| `PayloadRef` | `string` | 未单独声明 | 原输入/事件载荷的持久引用，不是发送成功证据。 |
| `EventSeq` | `uint64` | 未单独声明 | 该聚合/事件内的稳定序号。 |
| `Version` | `uint64` | 未单独声明 | 本行的乐观锁/控制版本；不可与 fence、修订、配置版本混用。 |
| `State` | `OutboxState` | 未单独声明 | 本表状态或 SDK 状态 JSON；实际类型、约束和本表职责共同解释。 |
| `Attempt` | `int` | 未单独声明 | 本表工作尝试计数；不是模型调用次数或用户可见回复次数。 |
| `NextAttemptAt` | `time.Time` | 未单独声明 | Outbox 下次允许发布的时间。 |
| `ClaimUntil` | `time.Time` | 未单独声明 | 本次数据库领取的有效期限。 |
| `CreatedAt` | `time.Time` | 未单独声明 | 该持久记录的创建时间；不是下游最终可见时刻。 |
| `ClaimOwner` | `string` | 未单独声明 | 当前可回收数据库工作领取者。 |
| `TraceParent` | `string` | 未单独声明 | 仅用于观测的 W3C 追踪传播头。 |

### messaging.DeliveryKey

[源码](../../../trpcservice/storage/messaging/messaging.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `DeliveryKey` | `string` | 未单独声明 | 同一逻辑回复的稳定投递键，分段共用此键。 |
| `SegmentNo` | `int` | 未单独声明 | 本条最终答案的分段序号，零起始。 |

### messaging.DeliveryPlan

[源码](../../../trpcservice/storage/messaging/messaging.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `RendererVersion` | `string` | 未单独声明 | 冻结回复渲染器版本，接管不能改版重切答案。 |
| `FormatVersion` | `string` | 未单独声明 | 冻结渠道格式版本。 |
| `ContentDigest` | `string` | 未单独声明 | 该表内容的 SHA-256；范围以表的编码契约为准。 |
| `SegmentCount` | `int` | 未单独声明 | 冻结渲染计划的总分段数。 |

### messaging.DeliveryRecord

[源码](../../../trpcservice/storage/messaging/messaging.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `(embed)` | `DeliveryKey` | 未单独声明 | 嵌入 `DeliveryKey` 的完整字段；按源类型展开，不能忽略其租户/请求作用域。 |
| `ProviderMessageID` | `string` | 未单独声明 | 渠道正常接受后返回的消息 ID，不单独证明客户端展示。 |
| `LastErrorClass` | `string` | 未单独声明 | 稳定错误类别；不是原始业务正文。 |
| `ClientRequestID` | `string` | 未单独声明 | 发送本 segment 时使用的稳定供应商请求键。 |
| `ClaimOwner` | `string` | 未单独声明 | 当前可回收数据库工作领取者。 |
| `State` | `DeliveryState` | 未单独声明 | 本表状态或 SDK 状态 JSON；实际类型、约束和本表职责共同解释。 |
| `Plan` | `DeliveryPlan` | 未单独声明 | 冻结的投递渲染计划，或当前任务计划；以所属类型区分。 |
| `Attempt` | `int` | 未单独声明 | 本表工作尝试计数；不是模型调用次数或用户可见回复次数。 |
| `ReconcileAttempt` | `int` | 未单独声明 | 不确定投递的核对次数，与实际发送 attempt 分开。 |
| `Version` | `int64` | 未单独声明 | 本行的乐观锁/控制版本；不可与 fence、修订、配置版本混用。 |
| `NotBefore` | `time.Time` | 未单独声明 | 该记录下次允许处理/核对的最早时间。 |
| `ClaimUntil` | `time.Time` | 未单独声明 | 本次数据库领取的有效期限。 |
| `UpdatedAt` | `time.Time` | 未单独声明 | 该持久记录最近更新时刻。 |

### messaging.DeliveryClaim

[源码](../../../trpcservice/storage/messaging/messaging.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Owner` | `string` | 未单独声明 | 当前数据库领取者/角色身份。 |
| `TTL` | `time.Duration` | 未单独声明 | 本次租约/claim 的存活期限，非整个任务业务期限。 |

### messaging.CompletionRecord

[源码](../../../trpcservice/storage/messaging/completion.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `EnvelopeDigest` | `string` | 未单独声明 | 原 ExecutionEnvelope 的规范摘要，固定路由和执行版本。 |
| `ContentDigest` | `string` | 未单独声明 | 该表内容的 SHA-256；范围以表的编码契约为准。 |
| `Content` | `[]byte` | 未单独声明 | SDK 事件/摘要内容或审计载荷，依本表用途解释，不等于最终已发送答案。 |
| `KeyVersion` | `int64` | 未单独声明 | 原载荷的租户加密密钥版本；恢复须仍能解析该版本。 |
| `Fence` | `uint64` | 未单独声明 | 持久执行资格序号，旧 owner 的较小值不能覆盖新 owner。 |

### messaging.ProviderReceipt

[源码](../../../trpcservice/storage/messaging/receipt.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `(embed)` | `DeliveryKey` | 未单独声明 | 嵌入 `DeliveryKey` 的完整字段；按源类型展开，不能忽略其租户/请求作用域。 |
| `ClientRequestID` | `string` | 未单独声明 | 发送本 segment 时使用的稳定供应商请求键。 |
| `ProviderMessageID` | `string` | 未单独声明 | 渠道正常接受后返回的消息 ID，不单独证明客户端展示。 |
| `PlanDigest` | `string` | 未单独声明 | 候选所绑定的原输入、合同、计划及完成证据摘要。 |
| `RouteDigest` | `string` | 未单独声明 | 原 tenant/request/binding/config/target 等发送语义摘要。 |
| `SegmentDigest` | `string` | 未单独声明 | 本次真实发送 segment 字节 SHA-256。 |
| `CreatedAt` | `time.Time` | 未单独声明 | 该持久记录的创建时间；不是下游最终可见时刻。 |

### session.SessionKey

[源码](../../../trpcservice/storage/session/atomic.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `AgentAppID` | `string` | 未单独声明 | 租户内应用身份，参与会话、执行及发布版本隔离。 |
| `SessionID` | `string` | 未单独声明 | 服务端稳定会话身份，故障接管保留该坐标。 |

### session.TerminalKey

[源码](../../../trpcservice/storage/session/atomic.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `(embed)` | `SessionKey` | 未单独声明 | 嵌入 `SessionKey` 的完整字段；按源类型展开，不能忽略其租户/请求作用域。 |
| `InputSeq` | `uint64` | 未单独声明 | 平台同一会话内分配的输入顺序号。 |

### session.OpenForRunRequest

[源码](../../../trpcservice/storage/session/atomic.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `(embed)` | `SessionKey` | 未单独声明 | 嵌入 `SessionKey` 的完整字段；按源类型展开，不能忽略其租户/请求作用域。 |
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `InputSeq` | `uint64` | 未单独声明 | 平台同一会话内分配的输入顺序号。 |
| `Fence` | `uint64` | 未单独声明 | 持久执行资格序号，旧 owner 的较小值不能覆盖新 owner。 |

### session.SessionHead

[源码](../../../trpcservice/storage/session/atomic.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `(embed)` | `SessionKey` | 未单独声明 | 嵌入 `SessionKey` 的完整字段；按源类型展开，不能忽略其租户/请求作用域。 |
| `Version` | `int64` | 未单独声明 | 本行的乐观锁/控制版本；不可与 fence、修订、配置版本混用。 |
| `LastFence` | `uint64` | 未单独声明 | 会话已接受的最高 Fence，跨 Redis 重建仍需保持。 |
| `LastSessionSeq` | `uint64` | 未单独声明 | 平台已经提交的事件高水位。 |
| `NextInputSeq` | `uint64` | 未单独声明 | 当前允许执行的输入顺序号。 |
| `State` | `map[string]any` | 未单独声明 | 本表状态或 SDK 状态 JSON；实际类型、约束和本表职责共同解释。 |

### session.BufferedEvent

[源码](../../../trpcservice/storage/session/atomic.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `EventID` | `string` | 未单独声明 | 持久事件的主键/标识；不同事件表粒度不同。 |
| `EventType` | `string` | 未单独声明 | 平台/SDK 事件类别。 |
| `PayloadRef` | `string` | 未单独声明 | 原输入/事件载荷的持久引用，不是发送成功证据。 |
| `EventSeq` | `uint64` | 未单独声明 | 该聚合/事件内的稳定序号。 |
| `Payload` | `json.RawMessage` | 未单独声明 | 版本化配置/事件的 JSON 载荷，具体对象在本表说明中限定。 |

### session.SessionSnapshot

[源码](../../../trpcservice/storage/session/atomic.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Head` | `SessionHead` | 未单独声明 | 平台会话头，包含顺序、版本及 Fence 高水位。 |
| `Events` | `[]json.RawMessage` | 未单独声明 | 平台协调事件或 SDK 快照事件数组；以该对象用途为准，当前组合根不镜像 SDK 原事件。 |

### session.SummaryCandidate

[源码](../../../trpcservice/storage/session/atomic.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `SummaryID` | `string` | 未单独声明 | 平台摘要内容的稳定身份。 |
| `BaseSessionSeq` | `uint64` | 未单独声明 | 摘要覆盖的平台会话事件边界。 |
| `LastEventID` | `string` | 未单独声明 | SDK 状态引用的最后一个事件，用于 SDK 自身状态关联。 |
| `CutoffAt` | `time.Time` | 未单独声明 | 保留/清理或压缩流程的截止观察时间。 |
| `ContentRef` | `string` | 未单独声明 | 回复、摘要或业务内容引用，按租户解析对应持久材料。 |

### session.OutboxEvent

[源码](../../../trpcservice/storage/session/atomic.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Kind` | `string` | 未单独声明 | Outbox、后端或事件类型；具体表内枚举约束为准。 |
| `IdempotencyKey` | `string` | 未单独声明 | 本表业务去重键；不能任意改变它绕过不确定效果。 |
| `PayloadRef` | `string` | 未单独声明 | 原输入/事件载荷的持久引用，不是发送成功证据。 |
| `TraceParent` | `string` | 未单独声明 | 仅用于观测的 W3C 追踪传播头。 |
| `EventSeq` | `uint64` | 未单独声明 | 该聚合/事件内的稳定序号。 |

### session.CommitTurnRequest

[源码](../../../trpcservice/storage/session/atomic.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `(embed)` | `SessionKey` | 未单独声明 | 嵌入 `SessionKey` 的完整字段；按源类型展开，不能忽略其租户/请求作用域。 |
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `CommitID` | `string` | 未单独声明 | 同一请求某个提交阶段的幂等提交 ID。 |
| `Stage` | `string` | 未单独声明 | waiting 或 terminal 等提交/治理阶段。 |
| `InputSeq` | `uint64` | 未单独声明 | 平台同一会话内分配的输入顺序号。 |
| `Fence` | `uint64` | 未单独声明 | 持久执行资格序号，旧 owner 的较小值不能覆盖新 owner。 |
| `ExpectedVersion` | `int64` | 未单独声明 | 人工处置针对的 Ledger 版本，防止并发裁决覆盖。 |
| `Outcome` | `runtime.Outcome` | 未单独声明 | 执行或会话提交结果；仅固定终态可进入历史权威投影。 |
| `Events` | `[]BufferedEvent` | 未单独声明 | 平台协调事件或 SDK 快照事件数组；以该对象用途为准，当前组合根不镜像 SDK 原事件。 |
| `StateDelta` | `StateDelta` | 未单独声明 | 平台兼容状态增量；不回滚已经写入的官方 SDK 事件。 |
| `SummaryCandidate` | `*SummaryCandidate` | 未单独声明 | 平台摘要候选，和 execution_completion 完成候选不是同一对象。 |
| `ResultRef` | `string` | 未单独声明 | 不可变结果或工具结果的引用，必须核对同作用域正文摘要。 |
| `ReplyCursor` | `string` | 未单独声明 | 平台回复进度引用；不能单独证明渠道最终可见。 |
| `Outbox` | `[]OutboxEvent` | 未单独声明 | 本次平台提交同时建立的待发布事件数组。 |

### session.CommitTurnResult

[源码](../../../trpcservice/storage/session/atomic.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `CommitID` | `string` | 未单独声明 | 同一请求某个提交阶段的幂等提交 ID。 |
| `Outcome` | `runtime.Outcome` | 未单独声明 | 执行或会话提交结果；仅固定终态可进入历史权威投影。 |
| `InputSeq` | `uint64` | 未单独声明 | 平台同一会话内分配的输入顺序号。 |
| `SessionVersion` | `int64` | 未单独声明 | 提交推进后的平台 session_head 版本。 |
| `ResultRef` | `string` | 未单独声明 | 不可变结果或工具结果的引用，必须核对同作用域正文摘要。 |
| `ReplyCursor` | `string` | 未单独声明 | 平台回复进度引用；不能单独证明渠道最终可见。 |

### session.TerminalHistory

[源码](../../../trpcservice/storage/session/terminal_history.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `CommitID` | `string` | 未单独声明 | 同一请求某个提交阶段的幂等提交 ID。 |
| `ResultRef` | `string` | 未单独声明 | 不可变结果或工具结果的引用，必须核对同作用域正文摘要。 |
| `InputSeq` | `uint64` | 未单独声明 | 平台同一会话内分配的输入顺序号。 |
| `Fence` | `uint64` | 未单独声明 | 持久执行资格序号，旧 owner 的较小值不能覆盖新 owner。 |
| `Outcome` | `runtime.Outcome` | 未单独声明 | 执行或会话提交结果；仅固定终态可进入历史权威投影。 |
| `CommittedAt` | `time.Time` | 未单独声明 | 平台终态提交时刻，非渠道显示时刻。 |

### coordination.SessionKey

[源码](../../../trpcservice/coordination/lease.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `AgentAppID` | `string` | 未单独声明 | 租户内应用身份，参与会话、执行及发布版本隔离。 |
| `SessionID` | `string` | 未单独声明 | 服务端稳定会话身份，故障接管保留该坐标。 |

### coordination.Lease

[源码](../../../trpcservice/coordination/lease.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Session` | `SessionKey` | 未单独声明 | 该租约绑定的 tenant/app/session 三元组。 |
| `WorkerID` | `string` | 未单独声明 | 实际执行尝试的进程角色身份，含启动代次。 |
| `LeaseID` | `string` | 未单独声明 | 随机租约能力值，Renew/Release 必须精确持有；检查接口不返回它。 |
| `Fence` | `uint64` | 未单独声明 | 持久执行资格序号，旧 owner 的较小值不能覆盖新 owner。 |
| `ExpiresAt` | `time.Time` | 未单独声明 | 能力/令牌/SDK 对象的过期时间；语义以本表为界。 |

### gateway.PrepareDispatchRequest

[源码](../../../trpcservice/gateway/taskstore.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Tenant` | `tenant.Context` | 未单独声明 | 可信租户上下文，携带原租户/配置版本及授权边界。 |
| `Binding` | `tenant.ExecutionBinding` | 未单独声明 | 经过验证或版本化解析的固定Bot/应用绑定，不是公开路由提示。 |
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `SessionID` | `string` | 未单独声明 | 服务端稳定会话身份，故障接管保留该坐标。 |
| `UserID` | `string` | 未单独声明 | 服务端稳定用户身份，不能拿另一个租户的派生结果复用。 |
| `PayloadRef` | `string` | 未单独声明 | 原输入/事件载荷的持久引用，不是发送成功证据。 |
| `TraceParent` | `string` | 未单独声明 | 仅用于观测的 W3C 追踪传播头。 |

### gateway.PreparedDispatch

[源码](../../../trpcservice/gateway/taskstore.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Envelope` | `runtime.ExecutionEnvelope` | 未单独声明 | 原始固定 ExecutionEnvelope；候选恢复完整交叉核对，不能只比较 request ID。 |
| `Accepted` | `bool` | 未单独声明 | 本次派发是否被可靠接受；不是业务最终完成。 |
| `TerminalReason` | `string` | 未单独声明 | 接收/派发阶段确定的终结原因。 |

### taskflow.Key

[源码](../../../trpcservice/taskflow/taskflow.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |

### taskflow.Scope

[源码](../../../trpcservice/taskflow/taskflow.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `(embed)` | `Key` | 未单独声明 | 嵌入 `Key` 的完整字段；按源类型展开，不能忽略其租户/请求作用域。 |
| `Fence` | `uint64` | 未单独声明 | 持久执行资格序号，旧 owner 的较小值不能覆盖新 owner。 |

### taskflow.Step

[源码](../../../trpcservice/taskflow/taskflow.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TrustedOutputs` | `[]*ResultCheck` | ``json:"trusted_outputs,omitempty"`` | 由工具代码提供、首次绑定操作时冻结的最终引用条件。 |
| `RequirementIDs` | `[]string` | ``json:"requirement_ids,omitempty"`` | 该步骤覆盖的冻结交付物 ID，声明后不可重写旧步骤。 |
| `ID` | `string` | ``json:"id"`` | SDK 内部行 ID；实际对外作用域仍需 app_name/user/session。 |
| `Title` | `string` | ``json:"title"`` | 步骤或业务笔记标题，不是权限坐标。 |
| `Criterion` | `string` | ``json:"criterion"`` | 该步骤显式完成准则说明，不单凭这段文字认定成功。 |
| `Check` | `*ResultCheck` | ``json:"check,omitempty"`` | 计划声明的业务结果断言，固定路径/期望值或非空条件。 |
| `TrustedCheck` | `*ResultCheck` | ``json:"trusted_check,omitempty"`` | 由实际审核工具提供的可信完成断言，不由模型指定。 |
| `Kind` | `string` | ``json:"kind"`` | Outbox、后端或事件类型；具体表内枚举约束为准。 |
| `DependsOn` | `[]string` | ``json:"depends_on,omitempty"`` | 步骤声明时固定的拓扑依赖列表。 |
| `ToolID` | `string` | ``json:"tool_id,omitempty"`` | 审核注册的工具标识；须与固定版本及租户一起解析。 |
| `ToolVersion` | `int64` | ``json:"tool_version,omitempty"`` | 该工具的精确版本，参与执行、幂等和证据校验。 |
| `State` | `string` | ``json:"state"`` | 本表状态或 SDK 状态 JSON；实际类型、约束和本表职责共同解释。 |
| `ToolCallID` | `string` | ``json:"tool_call_id,omitempty"`` | 工具逻辑操作身份；普通 allow 调用稳定派生，ask 沿原确认 ID。 |
| `ArgsDigest` | `string` | ``json:"args_digest,omitempty"`` | 工具规范 JSON 参数摘要，改变参数不能冒充原操作。 |
| `Arguments` | `json.RawMessage` | ``json:"arguments,omitempty"`` | 本步骤已绑定的规范工具参数，原密文快照中保存，不经管理 API 回显。 |
| `ResultRef` | `string` | ``json:"result_ref,omitempty"`` | 不可变结果或工具结果的引用，必须核对同作用域正文摘要。 |
| `EvidenceDigest` | `string` | ``json:"evidence_digest,omitempty"`` | 已核验笔记或工具结果的摘要，原始模型自报不能替代它。 |
| `Note` | `string` | ``json:"note,omitempty"`` | 分析步骤实际保存的分析笔记，不是模型隐藏推理栈。 |

### taskflow.Plan

[源码](../../../trpcservice/taskflow/taskflow.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `CompletionIssue` | `string` | ``json:"completion_issue,omitempty"`` | 上次新答案缺项的持久修复原因，不算有效进展。 |
| `Acceptance` | `*Acceptance` | ``json:"acceptance,omitempty"`` | 本请求首次冻结的目标与服务端必需交付物合同，模型不能重写。 |
| `FinalDigest` | `string` | ``json:"final_digest,omitempty"`` | SealFinal 固定的最终渲染字节摘要。 |
| `(embed)` | `Scope` | ``json:"-"`` | 嵌入 `Scope` 的完整字段；按源类型展开，不能忽略其租户/请求作用域。 |
| `Version` | `int64` | ``json:"version"`` | 本行的乐观锁/控制版本；不可与 fence、修订、配置版本混用。 |
| `InputDigest` | `string` | ``json:"input_digest"`` | 本任务原始持久输入完整字节 SHA-256。 |
| `KeyVersion` | `int64` | ``json:"-"`` | 原载荷的租户加密密钥版本；恢复须仍能解析该版本。 |
| `Phase` | `string` | ``json:"phase"`` | 任务计划 open/sealed/failed 等阶段，不是单次工具状态。 |
| `Steps` | `[]Step` | ``json:"steps"`` | 只追加的原任务步骤与证据列表，恢复不删除旧义务。 |
| `FinalRef` | `string` | ``json:"final_ref,omitempty"`` | 任务计划封存时固定的公开最终结果引用。 |
| `Contract` | `Contract` | ``json:"contract"`` | 服务端冻结的任务预算与证据门禁规则。 |
| `StartedAt` | `time.Time` | ``json:"started_at,omitempty"`` | 本次执行尝试或保留记录起点。 |
| `Attempts` | `int` | ``json:"attempts"`` | 原任务累计模型运行机会次数，换 owner 不清零。 |
| `NoProgress` | `int` | ``json:"no_progress"`` | 连续无新增已验证完成步骤的尝试计数。 |
| `LastProgressDigest` | `string` | ``json:"last_progress_digest,omitempty"`` | 已成功步骤及其证据的规范摘要，只有新可信进展才能重置停滞计数。 |
| `FailureReason` | `string` | ``json:"failure_reason,omitempty"`` | 任务已收敛失败的稳定原因码。 |

### taskflow.Requirement

[源码](../../../trpcservice/taskflow/acceptance.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `ArgsDigest` | `string` | ``json:"args_digest,omitempty"`` | 工具规范 JSON 参数摘要，改变参数不能冒充原操作。 |
| `ID` | `string` | ``json:"id"`` | SDK 内部行 ID；实际对外作用域仍需 app_name/user/session。 |
| `Description` | `string` | ``json:"description"`` | 对象说明文本，不参与节点执行权的判定。 |
| `ToolID` | `string` | ``json:"tool_id,omitempty"`` | 审核注册的工具标识；须与固定版本及租户一起解析。 |
| `ToolVersion` | `int64` | ``json:"tool_version,omitempty"`` | 该工具的精确版本，参与执行、幂等和证据校验。 |
| `OutputContains` | `[]string` | ``json:"output_contains,omitempty"`` | 最终渲染字节必须包含的字面内容列表，不是通用语义正确证明。 |
| `EvidencePath` | `[]string` | ``json:"evidence_path,omitempty"`` | 从真实工具回执抽取最终必须引用的标量值的对象路径。 |

### taskflow.Acceptance

[源码](../../../trpcservice/taskflow/acceptance.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Version` | `int` | ``json:"version"`` | 本行的乐观锁/控制版本；不可与 fence、修订、配置版本混用。 |
| `InputDigest` | `string` | ``json:"input_digest"`` | 本任务原始持久输入完整字节 SHA-256。 |
| `Objective` | `string` | ``json:"objective"`` | 原持久输入解码后的目标文本或明确标注的截段/多模态说明。 |
| `Requirements` | `[]Requirement` | ``json:"requirements"`` | 首次冻结的必需交付物列表。 |

### taskflow.Contract

[源码](../../../trpcservice/taskflow/policy.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Version` | `int` | ``json:"version"`` | 本行的乐观锁/控制版本；不可与 fence、修订、配置版本混用。 |
| `RequirePlan` | `bool` | ``json:"require_plan"`` | 成功前是否强制非空持久任务计划。 |
| `RequireToolChecks` | `bool` | ``json:"require_tool_checks"`` | 工具步骤及效果型工具的可信完成断言是否强制。 |
| `MaxAttempts` | `int` | ``json:"max_attempts"`` | 原任务或组件允许的尝试上限，具体计数域见调用者。 |
| `MaxNoProgress` | `int` | ``json:"max_no_progress"`` | 连续无新增已验证成功步骤的上限。 |
| `MaxAgeSeconds` | `int` | ``json:"max_age_seconds"`` | 从原任务 StartedAt 起固定的总体年龄上限，不随重试延长。 |

### taskflow.ResultCheck

[源码](../../../trpcservice/taskflow/evidence.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Path` | `[]string` | ``json:"path"`` | 确定性业务断言的 JSON 对象字段路径。 |
| `EqualsJSON` | `string` | ``json:"equals_json,omitempty"`` | 结果路径的 JSON 编码期望值，与 NonEmpty 互斥。 |
| `NonEmpty` | `bool` | ``json:"non_empty,omitempty"`` | 路径处要求非空；工具最终业务引用还必须是字符串或精确数字。 |

### taskflow.StepProgress

[源码](../../../trpcservice/taskflow/recovery_summary.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `ID` | `string` | ``json:"id"`` | SDK 内部行 ID；实际对外作用域仍需 app_name/user/session。 |
| `Kind` | `string` | ``json:"kind"`` | Outbox、后端或事件类型；具体表内枚举约束为准。 |
| `State` | `string` | ``json:"state"`` | 本表状态或 SDK 状态 JSON；实际类型、约束和本表职责共同解释。 |
| `ToolID` | `string` | ``json:"tool_id,omitempty"`` | 审核注册的工具标识；须与固定版本及租户一起解析。 |
| `ToolVersion` | `int64` | ``json:"tool_version,omitempty"`` | 该工具的精确版本，参与执行、幂等和证据校验。 |
| `ResultRef` | `string` | ``json:"result_ref,omitempty"`` | 不可变结果或工具结果的引用，必须核对同作用域正文摘要。 |
| `EvidenceDigest` | `string` | ``json:"evidence_digest,omitempty"`` | 已核验笔记或工具结果的摘要，原始模型自报不能替代它。 |

### taskflow.BlockedStep

[源码](../../../trpcservice/taskflow/recovery_summary.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `ID` | `string` | ``json:"id"`` | SDK 内部行 ID；实际对外作用域仍需 app_name/user/session。 |
| `Reason` | `string` | ``json:"reason"`` | 稳定阻塞/停止原因，不作为外部未执行证据。 |
| `Dependencies` | `[]string` | ``json:"dependencies,omitempty"`` | 尚未完成的依赖步骤 ID 列表。 |

### taskflow.RecoverySummary

[源码](../../../trpcservice/taskflow/recovery_summary.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Failed` | `[]StepProgress` | ``json:"failed"`` | 失败步骤投影，不得据此继续未确认的外部写入。 |
| `NeedsFinalRepair` | `bool` | ``json:"needs_final_repair"`` | 检查点中存在 completion_issue 的布尔投影，不回显正文。 |
| `PlanVersion` | `int64` | ``json:"plan_version"`` | 该恢复摘要对应的 task_plan.version。 |
| `Fence` | `uint64` | ``json:"fence"`` | 持久执行资格序号，旧 owner 的较小值不能覆盖新 owner。 |
| `Phase` | `string` | ``json:"phase"`` | 任务计划 open/sealed/failed 等阶段，不是单次工具状态。 |
| `InputDigest` | `string` | ``json:"input_digest"`` | 本任务原始持久输入完整字节 SHA-256。 |
| `Completed` | `[]StepProgress` | ``json:"completed"`` | 已完成步骤及其已保存结果引用/摘要。 |
| `Active` | `[]StepProgress` | ``json:"active"`` | 当前活动步骤的投影，不代表新 owner 已取得执行资格。 |
| `Ready` | `[]string` | ``json:"ready"`` | 静态依赖就绪的步骤 ID，仍须当前执行资格和工具授权。 |
| `Blocked` | `[]BlockedStep` | ``json:"blocked"`` | 不能开始的 pending 步骤及阻塞原因。 |
| `UnplannedRequirements` | `[]string` | ``json:"unplanned_requirements"`` | 尚未有声明步骤覆盖的冻结要求。 |
| `UnfinishedRequirements` | `[]string` | ``json:"unfinished_requirements"`` | 未规划或关联步骤尚未全部成功的冻结要求。 |
| `UnknownEffects` | `[]string` | ``json:"unknown_effects"`` | 效果未知的步骤 ID，不进入可执行写操作集合。 |
| `NextAction` | `string` | ``json:"next_action"`` | 由持久步骤、合同和期限推导的下一步提示，不授予执行权限。 |
| `FailureReason` | `string` | ``json:"failure_reason,omitempty"`` | 任务已收敛失败的稳定原因码。 |
| `Attempts` | `int` | ``json:"attempts"`` | 原任务累计模型运行机会次数，换 owner 不清零。 |
| `NoProgress` | `int` | ``json:"no_progress"`` | 连续无新增已验证完成步骤的尝试计数。 |
| `RemainingAttempts` | `int` | ``json:"remaining_attempts"`` | 未来新尝试余额；不能误禁当前最后一次合法执行。 |
| `Deadline` | `time.Time` | ``json:"deadline,omitempty"`` | 由首次 StartedAt 和固定任务年龄计算的截止时刻。 |

### postgres.DeliveryResolution

[源码](../../../trpcservice/storage/messaging/postgres/operations.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TenantID` | `string` | ``json:"tenant_id"`` | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `DeliveryKey` | `string` | ``json:"delivery_key"`` | 同一逻辑回复的稳定投递键，分段共用此键。 |
| `SegmentNo` | `int` | ``json:"segment_no"`` | 本条最终答案的分段序号，零起始。 |
| `ExpectedVersion` | `int64` | ``json:"expected_version"`` | 人工处置针对的 Ledger 版本，防止并发裁决覆盖。 |
| `Actor` | `string` | ``json:"actor"`` | 部署管理员命名空间下的操作者标签，不是个人认证证据。 |
| `EvidenceRef` | `string` | ``json:"evidence_ref"`` | 外部投递核验材料的引用，不接受纯超时猜测。 |
| `Decision` | `string` | ``json:"decision"`` | 原治理决定或本阶段的审核结论，必须与 tenant/request/policy 一致。 |
| `ProviderMessageID` | `string` | ``json:"provider_message_id"`` | 渠道正常接受后返回的消息 ID，不单独证明客户端展示。 |

### postgres.TaskRecordsQuery

[源码](../../../trpcservice/storage/messaging/postgres/task_records.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TenantID` | `string` | 未单独声明 | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `RequestID` | `string` | 未单独声明 | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `Kind` | `string` | 未单独声明 | Outbox、后端或事件类型；具体表内枚举约束为准。 |
| `Cursor` | `string` | 未单独声明 | 原 scope/kind 的 Base64URL 分页位置，不是授权凭据。 |
| `Limit` | `int` | 未单独声明 | 本次有界查询上限；分页默认 50，最大 100。 |

### postgres.TaskRecordsPage

[源码](../../../trpcservice/storage/messaging/postgres/task_records.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `TenantID` | `string` | ``json:"tenant_id"`` | 租户隔离坐标，查询、联合键及密钥解析必须使用同一租户。 |
| `RequestID` | `string` | ``json:"request_id"`` | 同一原始输入的服务端请求身份；故障接管不新建 request。 |
| `Kind` | `string` | ``json:"kind"`` | Outbox、后端或事件类型；具体表内枚举约束为准。 |
| `Records` | `[]json.RawMessage` | ``json:"records"`` | 本页只读元数据数组，不能当作完整历史快照。 |
| `NextCursor` | `string` | ``json:"next_cursor,omitempty"`` | 下一页稳定位置；缺失表示本观察页未检测到更多记录。 |
| `ObservedAt` | `time.Time` | ``json:"observed_at"`` | 该查询的数据库观察时间，不保证多页或独立解密读取同一快照。 |

### postgres.taskRecordsCursor

[源码](../../../trpcservice/storage/messaging/postgres/task_records.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `Version` | `int` | ``json:"version"`` | 本行的乐观锁/控制版本；不可与 fence、修订、配置版本混用。 |
| `Tenant` | `string` | ``json:"tenant"`` | 游标绑定的租户 ID。 |
| `Request` | `string` | ``json:"request"`` | 游标绑定的原 request_id。 |
| `Kind` | `string` | ``json:"kind"`` | Outbox、后端或事件类型；具体表内枚举约束为准。 |
| `After` | `[]string` | ``json:"after"`` | 分页上次最后一条真实返回记录的稳定主键位置。 |

### tool.RecoveryResult

[源码](../../../trpcservice/tool/recovery.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `State` | `RecoveryState` | 未单独声明 | 本表状态或 SDK 状态 JSON；实际类型、约束和本表职责共同解释。 |
| `Result` | `any` | 未单独声明 | 供应商查询得到的原操作结果，须按完整身份及业务断言核验。 |

### webui.inboundEnvelope

[源码](../../../trpcservice/channels/webui/protocol.go)

| Go 字段 | 类型 | JSON tag / 嵌入 | 用途与校验边界 |
|---|---|---|---|
| `SchemaVersion` | `uint16` | ``json:"schema_version"`` | 对象序列化格式版本，不是数据库迁移版本。 |
| `ExternalAccountID` | `string` | ``json:"external_account_id"`` | 渠道侧 Bot/应用账号，既是身份域也是回送目标组成部分。 |
| `ExternalMessageID` | `string` | ``json:"external_message_id"`` | 渠道原消息 ID；相同账号内重复回调用它去重。 |
| `ExternalUserID` | `string` | ``json:"external_user_id"`` | 渠道侧用户标识；内部 user_id 由租户密钥派生。 |
| `ExternalChatID` | `string` | ``json:"external_chat_id"`` | 渠道侧会话/群标识；与外部用户一起构造回复目的地。 |
| `ConversationType` | `string` | ``json:"conversation_type"`` | dm/p2p/group 等外部会话形态，决定内部会话键派生。 |
| `MessageType` | `string` | ``json:"message_type"`` | 规范文本/图片/文件等消息类型。 |
| `Text` | `string` | ``json:"text"`` | 本消息的文本内容，仅在已验证/已准备作用域中交给模型。 |
| `OccurredAt` | `time.Time` | ``json:"occurred_at"`` | 事件发生时刻，与持久化创建时间分别记录。 |

## 环境配置与固定运行参数

环境变量来自 loadWebUILocalConfig/loadWeComHAEntryConfig；下表默认是代码默认，不是对所有部署的建议值。Compose 的实际默认端口和挂载见源文件，运行前固定最终展开配置。SDK 版本由 go.mod 固定为 trpc-agent-go v1.11.2，不能在复刻时静默升级并假定序列化/存储语义不变。

| 变量 | 代码默认 / 要求 | 作用与限制 |
|---|---|---|
| TRPC_POSTGRES_DSN | 必填 | 同一恢复域实例使用同一权威数据库；不在文档/日志填实际凭据 |
| TRPC_REDIS_ADDRESS | 必填 | 同一恢复域使用相同 Redis 服务 |
| TRPC_REDIS_ENVIRONMENT | local-runtime | Streams/lease 命名空间；隔离环境可不同，同一 HA 域必须一致 |
| TRPC_LISTEN_ADDRESS | :8080 | 容器内部监听；同网络命名空间中须避免端口冲突 |
| TRPC_WEBUI_LOCAL_INSTANCE_ID | standalone；1..48，字母小写/数字/连字符 | 部署稳定实例名；Compose 对 A/B 显式设置不同值 |
| TRPC_WEBUI_LOCAL_EXCLUSIVE_RUNTIME | 默认 false | 单实例模式可取全局 PG advisory lock；多节点必须关闭，不能误当成分布式每 Bot 主选举 |
| TRPC_WEBUI_LOCAL_SECRET_ROOT | /tmp/trpc-webui-secrets，绝对路径 | 当前本地模式秘密文件目录；多容器各自 tmpfs，材料版本/内容仍须一致 |
| TRPC_WEBUI_LOCAL_SKILL_STAGING_ROOT | /tmp/trpc-webui-skills，绝对规范路径 | 已发布技能文件目录，不能与 SecretRoot 同路径 |
| TRPC_WEBUI_LOCAL_SKILLS_READ_ONLY | 默认 false | 只约束技能文件读取/发布权限，不代表数据库连接只读或禁用所有幂等 bootstrap |
| TRPC_WEBUI_DEEPSEEK_KEY_FILE | /run/secrets/deepseek_api_key | 部署注入模型凭据文件，不能写进文档/日志 |
| TRPC_WEBUI_LOCAL_ROUTE_KEY | local-webui | 本地 WebUI 路由定位，不是单独的租户授权 |
| TRPC_WEBUI_LOCAL_TOKEN | 内建本地示例值；运行要求无首尾空白且至少16字符 | WebUI 签名/本地材料派生来源，两个实例保持相同；不是运维 token |
| TRPC_RUNTIME_OPERATIONS_TOKEN | 默认空，不挂管理 API | 独立且至少32字符，无首尾空白，不等于 WebUI token；须真正注入应用容器环境 |
| TRPC_RUNTIME_DRAIN_TIMEOUT | 30s，合法1s..5min | 全局排空上限，超时工作留给原可靠恢复机制 |
| TRPC_WEBUI_LOCAL_STATUS_ENABLED | 默认 false | 挂载应用 /statusz；Compose 多节点模式启用 |
| TRPC_WEBUI_LOCAL_FAILOVER_TEST_ENABLED | 默认 false | 挂载可控 barrier，仅为验证设施 |
| TRPC_WEBUI_LOCAL_CLAMAV_ADDRESS | clamav:3310 | 当前本地组合根启动/ready 需要扫描依赖就绪 |
| TRPC_WEBUI_LOCAL_QDRANT_ENDPOINT | http://qdrant:6333 | 当前本地模式校验固定该端点，不是可任意改 URL 的通用部署配置 |
| TRPC_WECOM_LOCAL_ENABLED | 默认 false | 启用主 Bot callback 与固定绑定材料 |
| WECOM_CORP_ID / WECOM_AGENT_ID | 主 Bot 启用时必填；AgentID>0 | 当前 external_account_id 为 CorpID，AgentID 在验证/发送材料中固定 |
| WECOM_APP_SECRET / WECOM_CALLBACK_TOKEN / WECOM_ENCODING_AES_KEY | 主 Bot 启用时必填 | 部署注入原应用发送/验签材料，不写实际值 |
| TRPC_WECOM_SECONDARY_LOCAL_ENABLED | 默认 false，须先启用主 Bot | 次 Bot 独立 tenant/app/binding；不能仅设置次 Bot 凭据而未显式启用 |
| WECOM_SECONDARY_CORP_ID / WECOM_SECONDARY_AGENT_ID | 次 Bot 启用时必填 | 同 Corp 不同 Agent 可用；同 Corp 同 Agent 被拒绝 |
| WECOM_SECONDARY_APP_SECRET / WECOM_SECONDARY_CALLBACK_TOKEN / WECOM_SECONDARY_ENCODING_AES_KEY | 次 Bot 启用时必填 | 次租户的独立秘密作用域 |
| TRPC_FEISHU_LOCAL_ENABLED | 当前为 true 时直接拒绝 | 历史适配器代码存在不表示本组合根仍启用飞书本地运行面 |
| TRPC_WECOM_HA_ENTRY_BACKENDS | 至少两个不同 http origin | 不接受 userinfo/query/fragment/path；这里列应用 callback 后端 |
| TRPC_WECOM_HA_ENTRY_PROBE_INTERVAL | 1s，100ms..1min | 后端业务就绪探测周期 |
| TRPC_WECOM_HA_ENTRY_PROBE_TIMEOUT | 1s，100ms..30s | 单次 ready 探测超时 |
| TRPC_WECOM_HA_ENTRY_REQUEST_TIMEOUT | 2s，100ms..30s | 每次 callback 转发期限，外部响应错误仍需去重边界 |
| TRPC_WECOM_HA_ENTRY_RECOVERY_SUCCESSES | 2，1..10 | 摘除后重新加入所需连续成功次数 |

入口健康观察 TTL 在代码中计算为 `3*(ProbeInterval+ProbeTimeout)`，不是一个额外环境开关。ProcessStartID 在进程内随机生成，不能配置旧值冒充老进程。owner 为 `webui-local-<component>-<instance>-<processStartID>`；实例 ID 格式校验不等于数据库中有实例名唯一约束。

### 运行时装配值：不是环境变量

| 对象 | 当前值 | 如何影响恢复 |
|---|---|---|
| Redis 分片 / Worker group | 4 / webui-workers | 同会话稳定分片；多个实例消费同一工作域 |
| 执行 stream read/reclaim idle | 250ms / 30s | 断线后原 PEL 条目的回收条件 |
| Worker 会话 lease / renew | 30s / 10s | 资格续期与过期接管；不能视作确定 RTO |
| Worker reclaim 轮询 / limit | 5s / 100 | 批量发现过期 pending；容量不足会增加延迟 |
| Worker cancel 观察 | 未显式装配时100ms | 数据库取消意图轮询，不能撤销已发生副作用 |
| 预处理 claim / retry / max | 30s / 1s / 8次 | 同 job 的重试与失败收敛 |
| Relay claim / renew / poll | 30s / 10s / 100ms | Dispatch、Reply、Wakeup 等 Outbox 发布回收 |
| Delivery claim / renew | 30s / 10s | segment 执行权；Provider 接受与本地状态仍非原子 |
| Delivery 重试延迟 / 上限 | 1s / 1min | 仅明确可重试拒绝使用发送重试，未知走核对 |
| Delivery 发送上限 / 核对上限 | 8 / 8 | 发送 attempt 与 reconcile_attempt 分开累计 |
| Delivery catalog refresh | 1s | 新账号/绑定的消费者同步；它不是 Bot 主连接选举 |
| confirmation expiry 轮询 | 1s，batch100 | 用户未决定的到期流程自动收敛 |
| Task Contract | version1，RequirePlan/RequireToolChecks=true，MaxAttempts8，MaxNoProgress3，MaxAgeSeconds1800 | 写入原计划后跨节点保持，最长可接受年龄参数校验不超过86400秒 |
| 终态历史读取 | 最近最多100个输入终态 | 有序连续校验；不是无限上下文或所有模型都能完整记住 |
| 工具结果内联 / 分页 | <=8192字节内联，超出走 read_result | 不截断大结果来冒充完整业务证据 |
| Plan / Completion 大小 | JSON加密前256KiB / 候选加密前2MiB | 达到上限拒绝，不因拆分字段绕过总大小 |
| 应用 HTTP | Header5s，Read/Write10s，Idle60s，Header最大16KiB | 本地服务装配值；SSE 使用专门写期限刷新 |
| 应用 ready / 管理读 | ready2s，管理通常2s或3s context | 依赖失败报告不可用，不以超时判定业务未发生 |

### 持久化业务条件配置

`AgentAppRevision.RuntimePolicy["task_requirements"]` 是发布配置，不是模型工具入参。最多32项，结构与字段见 Requirement；配置整体最多64KiB。工具要求必须出现在当前修订的精确 Required ToolRefs，发布还需要部署工具目录校验器及可信完成断言。对已有计划只复用原合同，不重新读取后续配置替换它。

```json
{"task_requirements":[{"id":"receipt","description":"返回真实记录编号","tool_id":"create_record","tool_version":1,"evidence_path":["record_id"]}]}
```

示例工具不是当前本地默认注册工具；实际本地注册为两个租户各自的 webui_create_note@1。明确 `/note` JSON 命令由代码 TaskRequirements 生成原参数摘要与 note_id 条件，仍需原用户确认。普通聊天不通过关键词自动获得完整业务验收合同。

### MCP 扩展契约（默认本地工具目录不等于已接入所有 MCP）

| 配置 | 真实约束与含义 |
|---|---|
| Transport / ServerURL | 审核的 HTTPS SSE/streamable HTTP；不允许模型选 URL 或任意 stdio 进程 |
| RemoteToolName / ExpectedDeclarationDigest | 本地 ID 与远端工具名称对应，声明摘要在构建与连接时固定核对 |
| Timeout / SecretHeader / SecretPrefix | 部署代码提供超时和秘密注入方式，不接受模型保留字段覆盖 |
| ReadOnly | 仅已审核只读操作可声明 replay_safe，不凭远端 annotations 自动信任 |
| IdempotencyParameter | 供应商可靠幂等参数名，服务端派生原业务键；需 CompletionCheck |
| CompletionCheck | 固定业务成功断言；传输成功不能代替它 |
| Lookup.ToolName / ExpectedDeclarationDigest / KeyParameter | 同 endpoint 的审核只读查询，按原操作键返回固定状态协议 |
| JSONTextResult | 启用时解析单个业务 JSON 对象，便于对象路径断言；默认false保留原 content 输出 |

completed 需原 operation_key 与非空结果，not_found 仍仅能用同幂等键重试；pending/unknown/异常不降级为未发生，failed 不复活。JSON数字按精确 Number 保留，重复键、未知状态、错误 operation_key、多个文本块和错误结果均拒绝。通用 code_exec 仍 manual。

## 启动顺序与退出顺序

1. 执行000001..000008，固定镜像、SDK、配置、凭据/密钥版本和共享环境；不要让 SDK 自建另一份 schema。
2. 依赖服务就绪，bootstrap 发布或核对控制面、秘密作用域和不可变技能文件。运行实例仍执行幂等检查；SKILLS_READ_ONLY 仅限制技能文件。
3. 建立 PG/Redis/秘密解析、协议适配器、预处理 Store、执行 Broker/Lease、版本化 profile/模型、治理、工具目录、SDK session/memory、计划/结果 Store。
4. 为各角色建立进程唯一 owner，启动预处理、Relay、Worker、Wakeup、Delivery、确认到期和 HTTP。组件必须真正访问工作依赖后才能 ready。
5. 稳定入口通过业务 ready 和连续恢复阈值加入后端；用户仍用同一 callback 地址。
6. SIGTERM 时 health.beginDrain 与 Gate.Begin 先关闭准入，HTTP Shutdown 和角色排空并行；停止新批次/新消费，不立即取消活动 workCtx。
7. 全局期限内完成已进入工作；之后取消进程 context 并退出。超时留下持久工作给存活实例。SIGKILL 完全不运行该清理逻辑。

## 本地模式秘密与隔离的实际含义

当前本地组合根按部署 Token 和不同用途/租户标签派生可重建的示例材料，各容器的目录私有但材料必须一致，才可解密对端写入的原版本。改变该 Token 会影响这些材料，不能当作一次无影响的浏览器令牌替换。生产式秘密服务的轮换/分发需要保留历史 key_version 并维持身份/会话映射；不能由本地临时目录存在推导其已实现。

Compose 应用 root只读，/tmp 是每容器私有 tmpfs，技能版本由一次 bootstrap 原子发布并只读共享；业务输入、计划、结果和会话不依赖这些本地临时目录。A/B 容器内部都可监听8080，宿主映射默认58088/58089，稳定入口默认58087；宿主或同一网络命名空间内运行多进程时须另分监听端口。以上是单机应用冗余，不是共享依赖和稳定入口本身的冗余。

## SDK事件扩展与历史投影的对象边界

当前SDK事件的可信扩展名为 `trpc.execution.v1`，序列化字段为 `request_id` 与 `fence`。它由运行上下文写入；运行期 PreserveCallID 不序列化，只用于恢复准确批准的确认调用。配对工具还需原请求/尝试/调用分支，不把同名工具结果接到另一调用。

历史投影对象由终态、原成功输入和固定结果组成读取副本，不删除或覆写SDK原始事件；当前尝试可保留准确授权调用，过去轮次不按最高Fence草稿推断成功答案。SDK事件完整格式由go.mod固定版本的官方事件模型负责，这些JSON不是新增平台SQL列。

## HTTP回调的实际协议参数

企业微信 GET 验证请求使用 route_key 定位绑定，以及 msg_signature、timestamp、nonce、echostr；服务端核对固定材料、解密并返回协议挑战响应。POST 使用相同验签参数及渠道加密XML/body，解密后的身份必须与绑定材料一致，再交给 ProviderEvent 解码。当前应用回调装配的 body 上限为1MiB，稳定入口读取上限2MiB不放宽应用本身限制。公开参数不允许直接指定可信tenant或替换发送秘密。

WebUI消息JSON的逐字段结构见 webui.inboundEnvelope；浏览器GET回复/进度还需 route_key、external_user_id、external_chat_id。X-WebUI-Timestamp、X-WebUI-Nonce、X-WebUI-Signature的实际规范编码及校验由 channels/webui/protocol.go/http.go 负责，默认允许时间偏差5分钟；不得把管理token当作渠道签名。这些协议验证与服务端原消息幂等是两层不同职责。
