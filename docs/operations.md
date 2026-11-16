# 运行与恢复

生产启动前构建 `go build -o atlas ./cmd/atlas` 和 `go build -o atlasctl ./cmd/atlasctl`。通过ATLAS_STATE指定独立可写状态路径，通过ATLAS_ADDR指定受信环境的监听地址。状态目录应由运行进程拥有；临时写文件与最终文件位于同一个目录。ATLAS_WRITER可选，标识本次进程身份，默认`host:pid`，出现在`GET /v1/status`的`writer`字段和`process.claimed`事件中。

同一状态路径的第二个进程会因为独占文件锁拒绝启动。锁文件可长期存在，锁的有效性由操作系统进程锁决定。不要通过删除锁文件启动第二个写进程。文件锁只在共享同一文件的进程之间有效，因此每次写入都会在文件内发布`generation`（单调递增）与`writer`；进程在每次提交前后和每250ms检查一次磁盘头部，发现自己不再是当前generation时立即停止渲染并以`503 fenced`拒绝所有路由（含`/healthz`和只读接口），避免旧主机继续返回过期事实。

SIGTERM或中断信号会停止领取任务，取消执行中的计算，等待HTTP请求和执行循环结束，随后在仍持有状态所有权时写入`process.stopped`事件。执行中的任务保留running，下一次启动会重新入队。SIGKILL恢复用 `python3 scripts/walkthrough.py --workflow recover-render` 演练；只会结束该脚本自己启动的实例。被fence的进程不会提交执行结果，下一次以新的generation 启动时按固定输入恢复。

备份时先正常停止实例，复制状态JSON；恢复时向空的独立状态路径复制，再启动。不要把运行中手工编辑过的JSON直接作为新事实。启动校验拒绝不支持版本、损坏摘要、孤立关联和非法文档。备份包含PNG字节与全部快照引用闭包，无需从外部素材服务恢复。

跨主机迁移单写实例必须停写、按清单迁移并fence旧主机，不能靠复制锁文件或直接重启旧主机。`atlasctl cutover` 在持锁状态下完成捕获、复制、校验和fence，清单记录`factsDigest`（固定任务输入、任务承诺、幂等收据、已清理结果）与`generation`。目标主机用`ATLAS_MANIFEST`启动，只接受与清单一致的字节，随后递增generation。旧路径旁的`state.json.superseded`使旧主机拒绝启动和重启；删除锁文件不解除该状态。完整步骤、回退演练和失败处理见`docs/handoff.md`，自动演练为：

```sh
python3 scripts/handoff_drill.py --keep
```

容量观察入口是GET /v1/status。它报告未压缩的持久结果PNG字节总量、各任务状态数量、当前`writer`、`generation`和`adoptedBy`，不代表堆内存峰值。完整状态在事务中复制并编码，因此长期积累会增加提交成本。当前未提供任务或快照删除；部署应按预期数据量规划状态盘和实例周期。结果清理入口仅删除PNG字节。历史事件超过500条会截断，但sequence继续增加。

故障时保留当前状态文件和服务日志，在副本上检查。500表示事务未能完成；客户端可先读取原请求目标确认状态。提交任务时使用稳定幂等键，不能因为连接中断盲目换键。409应结合当前场景revision、快照state或收据语义判断。410说明成功结果字节已清理，任务成功事实仍保留。503 `fenced` 表示该进程已失去状态所有权，应停止该主机并从清单核对目标主机，而不是重试或删除标记。

`deploy/systemd/atlas.service` 是Linux systemd配置示例，需按实际用户、二进制和状态路径调整；其中`ConditionPathExistsNot`用于阻止迁移后的旧主机被systemd自动拉起。macOS可直接运行同一二进制。当前未验证容器部署、跨进程共享写入、跨机器文件系统锁或网络存储；在这些环境中generation检查是最后的防线，发生fence后必须人工确认唯一写入者。
