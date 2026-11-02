# 故障恢复后继续对话 — 当前实现基线

代码基线 `0f53489`；迁移至000008，当前源码中的新增业务合同/引用字段位于原加密Plan内，没有000009。

## 当前权威链路

- 可信 Binding 固定 tenant/app/account；身份/会话密钥和渠道坐标派生稳定 user/session。
- ingress.Pipeline 先保存加密输入，再同事务建立 Inbox 与预处理工作；原消息键不因重试变化。
- 平台 session_head/commit 负责输入顺序、Fence和终态；官方 SDK session_events/state 保存原执行转写，两域不是一个事务。
- Worker 读取最近最多100个原会话终态，认证原成功输入、原用户/会话、固定结果和密钥版本；未提交/被拒绝草稿不当作历史答案。
- 新消息完成后，原执行配置和 Inbox 目标构成 ReplyRoute；跨租户或目标不匹配拒绝，旧结果/候选不被重新生成覆盖。

## 接管与边界

callback 入口只向 ready 后端转发；Worker reclaim、会话 lease/Fence 与 Delivery Ledger 各自负责旧工作。单实例恢复不需要换会话或用户重新发送已可靠接收的原输入。模型/HTTP/工具尝试可增加，原有效终态与逻辑回复仍保持原坐标。

P4 已保存匹配回执时只补 sent；未保存回执且渠道不可可靠查询时保留不确定并走审计处置。这里不声称无条件最终可见恰好一次、无限记忆或已完成真实环境验收。

## 逐层核对

[完整数据链路](../END-TO-END-DATA-FLOW.md) · [实现历程](../IMPLEMENTATION-HISTORY.md) · [表字段](../DATABASE.md) · [接口与配置](../INTERFACES.md) · [机制](FULL-GUIDE.md) · [运行走读](implementation-walkthrough.md) · [状态设计](design.md) · [代码](CODE-APPENDIX.md) · [精确契约](REFERENCE-IMPLEMENTATION.md) · [验收](testing-and-acceptance.md) · [运维](release-and-operations.md)。
