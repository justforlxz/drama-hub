# drama-hub

短剧媒体管理器。把红果短剧的数据整理成 Emby 能直接识别的剧集库。

- 只读虚拟 WebDAV：按需渲染 `.strm` / `.nfo` / 封面，Emby 通过 `rclone mount` 扫描
- 出流服务：单集 MP4 透传（支持 Range），合并集自造 HLS 播放列表，按分片回源
- 合并集：N 集合成 1 条剧集，时间轴精确到关键帧，**不落物化文件**
- 缓存：分片级 LRU，上限与保留期后台可调

## 快速开始

```bash
make build                    # 产出 ./bin/drama-hub
DRAMA_HUB_DB=./data/drama.db DRAMA_HUB_ADDR=:8080 ./bin/drama-hub
```

首次启动自动建库与迁移。

## HTTP 接口（框架阶段）

框架阶段只有路由骨架，业务端点按阶段陆续接入。未实现的端点返回 **501 Not Implemented**
（已规划未实现），不是 404；未装配的路径返回 404。

| 端点 | 方法 | 状态 | 说明 |
|---|---|---|---|
| `/healthz` | GET（HEAD 隐式） | 200 `{"status":"ok"}` | 存活探测 |
| `/dav/` 及全部子路径 | 任意 | 501 | 只读 WebDAV 挂载点，后续接入 |
| `/s/` 及全部子路径 | 任意 | 501 | 出流转服务，后续接入 |
| `/api/` 及全部子路径 | 任意 | 501 | 管理 API，后续接入 |
| 其它路径（含 `/`） | 任意 | 404 | 未装配；管理界面属后续阶段 |

```bash
curl -i http://127.0.0.1:8080/healthz   # 200 {"status":"ok"}
curl -i http://127.0.0.1:8080/dav/      # 501
```


需要的运行环境：`ffmpeg`、`ffprobe` 在 `PATH` 里。

## 接到 Emby

```bash
rclone config create dramahub webdav url http://127.0.0.1:8080/dav vendor other \
  user <webdav_user> pass <webdav_pass>
rclone mount dramahub:/ /mnt/dramahub --vfs-cache-mode full --dir-cache-time 30s
```

Emby 库路径指向 `/mnt/dramahub/短剧`，类型选「电视节目」。

## 开发环境

生产与测试共用同一个镜像，ffmpeg 版本由镜像固定（见 `env.lock`）：

```bash
make env-check                  # 校验容器内 go/ffmpeg 版本与 env.lock 一致
make e2e-image                  # 构建测试镜像（runtime + Go 工具链）
make test-e2e                   # 容器内跑集成测试，--network=none
make run-container              # 容器内起服务，挂 ./data，映射 :8080
```

本机 `make build` / `make test` 只用于快速迭代：开发机 ffmpeg 与容器 ffmpeg 版本不同，
本机结果不能作为验收依据。升级基础镜像 = 改 `Containerfile` 的 digest + `env.lock` + 跑 `make env-check`。

podman 6 会把短名当远程仓库去解析，因此镜像名一律用全限定名 `localhost/drama-hub`，运行命令带 `--pull=never`。
宿主机 8080 被占用时用 `make run-container PORT=18081`。

## 文档

- `AGENTS.md` — 硬约束与目录约定（改代码前先读）
- `docs/DEV-STANDARDS.md` — 依赖、错误处理、配置、日志规范
- `docs/TEST-STANDARDS.md` — 测试分层、必须覆盖的协议断言
- `docs/ACCEPTANCE.md` — 真机验收清单
- `docs/adr/` — 架构决策记录

## 状态

实现中，分阶段推进：P1 数据与管理面 → P2 WebDAV+NFO → P3 单集出流 → P4 缓存 → P5 合并集 → P6 App 回退 → P7 缓存观测。

设计基线见 MKACG-580 附件 `DESIGN-drama-hub.md`。
