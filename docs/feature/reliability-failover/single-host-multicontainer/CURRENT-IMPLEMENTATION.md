# 单主机多容器实例高可用 — 当前实现基线

代码基线 `0f53489`；迁移至000008，当前源码中的新增业务合同/引用字段位于原加密Plan内，没有000009。

## 当前部署与身份

同一Compose项目运行WeCom稳定callback入口和A/B两个应用实例；PG/Redis工作域及已发布配置共享，存活实例具备两个租户配置。容器root只读、私有/tmp，技能卷只读共享，原业务状态不存本地临时目录。

InstanceID为部署名，ProcessStartID每次启动随机生成，role owner包含两者。没有专门的全局node表或独占Bot长连接；Redis消费者组、会话lease和数据库segment claim各管自己的工作。单实例ExclusiveRuntime全局锁在多节点模式关闭。

## 生命周期闭环

| 事件 | 代码行为 |
|---|---|
| 加入 | 引导/校验共享内容与依赖，组件真实运行后ready，入口以恢复阈值加入 |
| 强杀 | 进程退出不执行清理；入口切后端，工作/租约超时按原request回收 |
| 重新加入 | 新启动代次，不继承旧owner能力；原业务与结果继续按共享事实核对 |
| 反向故障 | 同一套对称领取/回收机制，不依赖固定主节点名 |
| SIGTERM | 关闭准入/新读取，保留活动context，默认30s后取消/退出 |
| drain超时 | 报告未排空；原持久工作交给存活实例，不删lease冒充成功 |

业务续聊与原在途消息分别沿能力一/二判断；同主机和共享依赖故障不在本双应用副本承诺内。脚本提供生命周期/负载检查路径，但本基线不填写未执行的验收结果。

## 逐层核对

[完整数据链路](../END-TO-END-DATA-FLOW.md) · [实现历程](../IMPLEMENTATION-HISTORY.md) · [表字段](../DATABASE.md) · [接口与配置](../INTERFACES.md) · [机制](FULL-GUIDE.md) · [运行走读](implementation-walkthrough.md) · [状态设计](design.md) · [代码](CODE-APPENDIX.md) · [精确契约](REFERENCE-IMPLEMENTATION.md) · [验收](testing-and-acceptance.md) · [运维](release-and-operations.md)。
