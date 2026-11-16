# Atlas Render

Go 静态图形合成服务。使用有序场景图描述矩形、椭圆、多边形和位图，在服务端输出 PNG。支持局部变换、嵌套裁剪、线性渐变、透明叠加、冻结快照、异步执行和快照包交换。仅使用 Go 标准库。

## 启动

需要 Go 1.22 或以上；持久存储的进程独占锁适用于 macOS 和 Linux。

```sh
go build ./...
go run ./cmd/atlas
```

默认监听 `127.0.0.1:8763`，状态写入 `var/atlas-state.json`。可用 `ATLAS_ADDR`、`ATLAS_STATE` 改变监听地址和状态路径；`ATLAS_WRITER` 标识进程身份。同一个状态路径只能由一个进程打开。服务启动会验证持久数据、发布新的状态generation并恢复中断的执行任务；发现状态已被其他进程接管时，进程会以 `503 fenced` 停止服务所有路由，而不是继续返回旧事实。默认部署面向受信网络中的单实例生产流程。

## 运行真实工作流

```sh
python3 scripts/walkthrough.py --workflow submit-render --out var/demo
python3 scripts/walkthrough.py --workflow retire-asset
python3 scripts/walkthrough.py --workflow cancel-render
python3 scripts/walkthrough.py --workflow retry-render
python3 scripts/walkthrough.py --workflow recover-render
python3 scripts/walkthrough.py --workflow import-package
python3 scripts/walkthrough.py --workflow compare-renders
```

脚本在临时状态目录启动实际服务，通过 HTTP 完成操作后停止服务，输出任务状态和 PNG 摘要；`--out` 保存可查看的合成示例。恢复流程会强制结束脚本自己启动的服务进程，再验证恢复结果。所有工作流编号见 `.project-manifest.json`。

## 主机迁移演练

```sh
go build -o atlas ./cmd/atlas
go build -o atlasctl ./cmd/atlasctl
python3 scripts/handoff_drill.py --keep
```

脚本用两个独立状态目录模拟两台主机，完成一次真实中断（SIGKILL）后的单写切换和回退：捕获并校验固定输入、任务承诺和已清理结果的 `factsDigest`，复制并逐字节校验目标状态，用 `state.json.superseded` fence 旧主机（删锁文件和重启均无效），在目标主机按原幂等键重放并恢复 running 任务，比较像素确认无分叉，再回退到原路径。步骤和失败处理见 `docs/handoff.md`。

## 场景到结果

1. `PUT /v1/scenes/emblem`，提交名称、`expectedRevision: 0` 和 `document`。示例见 `examples/emblem.json`。
2. `POST /v1/scenes/emblem/snapshots`，提交 `expectedRevision`，取得快照 ID。
3. `POST /v1/jobs`，提交 `snapshotId`、`scale: 1`，并带唯一的 `Idempotency-Key` 请求头。
4. `GET /v1/jobs/{id}` 等待 `succeeded`，再读取 `GET /v1/artifacts/{artifactId}`。

同一幂等键和同一请求返回原任务；同键不同请求返回 409。快照包含引用素材的字节，后续场景编辑或素材退役不改变它。撤回快照会阻止新工作，已接受的任务仍使用固定输入。任务取消和结果提交竞争同一个持久状态；终态完成后不可取消。

## 范围与限制

画布每边最多 1536 像素，输出缩放为 1 或 2；场景不超过 200 图层、8 层嵌套。PNG 素材每边最多 1024 像素、输入最多 2 MiB。编译后的工作量不得超过四千万个图层像素，队列最多接受 64 个未结束任务。

透明度为 0–1，变换中的 `scaleX`、`scaleY` 必须显式设置；省略会成为零而被拒绝。组的透明度乘到每个后代，属于分布式透明语义；当前没有离屏隔离组。缩放 2 表示两倍尺寸渲染，不表示输出降采样。位图按最近邻采样，颜色直接在编码通道上混合。

持久化使用完整 JSON 状态的原子替换，保留全部任务与快照，事件只保留最近 500 条。适合中小规模、单写实例工作负载。清理结果只释放 PNG 字节，仍保留任务事实和摘要。进程重启可恢复任务；突然断电的文件系统耐久性不作额外保证。

测试处于 deferred 阶段，基线未包含自动化测试套件或 `test_command`。生产工作流检查、构建与静态检查已单独提供。接口和持久状态的规则详见 `PROJECT_SPEC.md`、`docs/api.md`、`docs/operations.md`。
