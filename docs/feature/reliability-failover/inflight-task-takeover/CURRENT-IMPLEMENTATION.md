# 在途任务接管与幂等投递 — 当前实现基线

代码基线 `0f53489`；迁移至000008，当前源码中的新增业务合同/引用字段位于原加密Plan内，没有000009。

## 当前已经连通的恢复责任

| 层 | 当前机制 |
|---|---|
| 原消息 | Inbox、加密原输入、预处理job、execution/dispatch Outbox及原会话顺序 |
| 执行资格 | Redis会话lease、数据库Fence高水位、Worker尝试流水、PEL回收 |
| 任务进度 | 原目标/Requirement、只追加步骤、检查点、预算/期限、失败收敛、恢复摘要 |
| 工具效果 | 普通/确认统一Guard、原逻辑操作/参数、可信断言、原结果认证、查询优先和适用业务读回 |
| 完成与结算 | 完成证据闸门、原PlanDigest候选、原Permit/Usage/价格重放、公开不可变结果及SealFinal |
| 终态与发送 | 唯一平台terminal+Outbox、原ReplyRoute、分段Ledger、不可变接受回执、证据化处置 |
| 发布与诊断 | 精确强制工具/声明验证、原任务incident、五类稳定主键分页，管理查询不修改业务 |

## 具体业务与外部边界

已落到真实共享业务表的笔记支持 `/note` 原参数绑定、用户确认、独立读回和最终真实编号。泛化MCP框架支持审核的键/查询/JSON结果协议，不等于已接入所有供应商；通用代码执行继续manual。未配置的自然语言业务要求不由模型计划自动保证完整。

P4回执落库后可自动补sent；接受到回执落库之前仍取决于下游协议。未知保留ambiguous或效果不确定，核对耗尽可终结并有证据裁决；安全停止不等于任务成功。管理token为部署身份，actor是标签；不提供强制复活终态或任意工具重放API。

## 逐层核对

[完整数据链路](../END-TO-END-DATA-FLOW.md) · [实现历程](../IMPLEMENTATION-HISTORY.md) · [表字段](../DATABASE.md) · [接口与配置](../INTERFACES.md) · [机制](FULL-GUIDE.md) · [运行走读](implementation-walkthrough.md) · [状态设计](design.md) · [代码](CODE-APPENDIX.md) · [精确契约](REFERENCE-IMPLEMENTATION.md) · [验收](testing-and-acceptance.md) · [运维](release-and-operations.md)。
