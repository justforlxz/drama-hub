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

首次启动自动建库与迁移。打开 `http://127.0.0.1:8080/` 进入管理界面，先配 WebDAV 账号与 `strm_prefix`。

需要的运行环境：`ffmpeg`、`ffprobe` 在 `PATH` 里。

## 接到 Emby

```bash
rclone config create dramahub webdav url http://127.0.0.1:8080/dav vendor other \
  user <webdav_user> pass <webdav_pass>
rclone mount dramahub:/ /mnt/dramahub --vfs-cache-mode full --dir-cache-time 30s
```

Emby 库路径指向 `/mnt/dramahub/短剧`，类型选「电视节目」。

## 文档

- `AGENTS.md` — 硬约束与目录约定（改代码前先读）
- `docs/DEV-STANDARDS.md` — 依赖、错误处理、配置、日志规范
- `docs/TEST-STANDARDS.md` — 测试分层、必须覆盖的协议断言
- `docs/ACCEPTANCE.md` — 真机验收清单
- `docs/adr/` — 架构决策记录

## 状态

实现中，分阶段推进：P1 数据与管理面 → P2 WebDAV+NFO → P3 单集出流 → P4 缓存 → P5 合并集 → P6 App 回退 → P7 缓存观测。

设计基线见 MKACG-580 附件 `DESIGN-drama-hub.md`。
