# drama-hub

短剧媒体管理器：把红果短剧的数据变成一个 Emby 能识别的剧集库。

对外的三个面：只读虚拟 WebDAV（元数据）、出流服务（视频字节）、管理 API/WebUI。

设计基线是 MKACG-580 的附件 `DESIGN-drama-hub.md`。需要背景时读它，不要凭印象改架构。

## 做什么 / 不做什么

做：红果数据同步、虚拟文件树、NFO/STRM 生成、单集与合并集出流、**本地分片缓存与淘汰**、三页管理界面。

不做：播放器、下载器、转码流水线、字幕/弹幕、多用户权限、多租户。Emby 是唯一的播放客户端。

## 硬约束

改动前逐条对照。违反任一条的方案直接否决，不进入评审。

1. **虚拟，不落盘。** strm / nfo / 目录树全部按请求渲染。磁盘上只允许两类文件：图片缓存、分片缓存。禁止把剧集元数据导出成实体目录。
2. **合并集只有一种实现：虚拟 HLS 分片聚合。** 禁止物化合并文件、禁止字节拼接多集 MP4（`moov` 只描述首集，拼字节必然时长错、seek 崩）。这条是两个方案对比后定的，不要再试。
3. **每个虚拟文件必须给出精确 `Content-Length`。** WebDAV 的 `PROPFIND` 少了它，rclone mount 读不到文件、Emby 扫不出集。渲染 → 算长度 → 再返回，一次请求内完成。
4. **单二进制 + SQLite。** 没有 Node、没有前端构建步骤、没有第二个运行时、没有外部缓存服务。界面用 Go 模板 + 少量原生 JS，模板随二进制嵌入（`embed`）。
5. **可调参数进 `settings` 表。** strm 前缀、出流签名密钥、分片时长、预取数、pin 天数、缓存目录/上限/LRU 天数运行时可变，改完即时生效。代码里出现这类魔法数字视为缺陷。**缓存由本服务自己负责（ADR-0007）**：海报、分片、关键帧索引落 `<cache_dir>`，按 `cache_max_mb` 与 `cache_lru_days` 淘汰，规则与测试见 `docs/CACHE-MODEL.md`。其中 `seg_prefetch` / `pin_days` 目前只进 settings 与状态页、没有任何预取实现（接线属代码改动，见 `docs/DEV-STANDARDS.md`「分片缓存」）。
6. **对外集号只由 `emby_item` 派生表决定。** 合并调整只重建派生表，`drama` / `episode` 原始数据不动。
7. **出流用字节透传，不用 302。** 302 到签名 CDN 会让缓存键漂移、过期链接命中旧缓存返回错内容。
8. **出流 token 是 HMAC 常量签名，无过期、无状态、无 token 表**（ADR-0003）。带过期的 token 会破坏缓存键稳定性，与缓存目标直接冲突。
9. **不做理论性加固。** 新增任何防护/兜底/校验前，先说明具体的失败模式、发生可能性、复杂度成本，取得确认再写。单用户本机场景下概率接近零的边缘情况一律不写。

## 目录

```
cmd/drama-hub/          main，只做装配，不含业务逻辑
internal/plugin/        播放源插件契约（接口 / 5 条哨兵 / 注册表）与实现 hongguo/
internal/sync/          框架侧同步编排：插件取数 → 事务写库 → 重建派生表
internal/scheduler/     刷新调度：一轮三段、同源单飞、间隔热读、手动触发与状态
internal/store/         SQLite、迁移、派生表重建
internal/vfs/           虚拟文件树 → strm/nfo/图片 渲染
internal/webdav/        x/net/webdav 自定义 FileSystem
internal/stream/        播放 id 签名（HMAC）与请求时直链解析（不碰 HTTP）
internal/segment/       播放面 handler：关键帧索引、播放列表、分片转封装与缓存编排
internal/cache/         本地缓存（封面/关键帧索引/分片）+ 淘汰（ADR-0007）
internal/web/           管理 API + 页面模板
docs/                   规范与 ADR
```

依赖方向单向：`cmd` → `internal/web`/`webdav`/`stream` → 领域包 → `store`。领域包之间不互相 import，需要协作时由上层编排。反向 import 一律打回。

## 命令

```
make build        # 本机编译检查（不产出验收物）
make check        # lint + 容器内测试
make env-check    # 校验容器内 go/ffmpeg 版本与 env.lock 一致
make test         # 容器内跑全部测试（--network=none），唯一测试路径
make run          # 容器内跑服务，挂 ./data，PORT 可选
```

**运行与测试只有容器这一条路径**：开发机 ffmpeg 与容器 ffmpeg 不是同一个版本。
`ffmpeg` 只出现在 runtime 镜像里，e2e 段 `FROM runtime`，所以测试与线上用同一个二进制。
测试二进制在 build 段编译好再拷进 e2e，e2e 镜像里没有 Go 工具链。

## 规范

- 开发规范：`docs/DEV-STANDARDS.md`
- 测试规范：`docs/TEST-STANDARDS.md`
- 架构决策记录：`docs/adr/`（新决策先写 ADR，再改代码）

## 提交与分支

- 分支：`feat/<阶段>-<主题>`、`fix/<主题>`，从 `main` 切出。
- 提交信息：`<类型>(<范围>): <祈使句摘要>`，类型限 `feat|fix|refactor|test|docs|chore|perf`，范围用包名。
- 一个提交只做一件事；格式化改动单独提交。
- 合入前 `make check` 必须通过。

## 完成定义

功能算完成，需要同时满足：

1. `make check` 通过；
2. 该功能涉及的 HTTP 端点有集成测试（httptest 起真实 handler，不 mock 自己的 handler）；
3. 新行为在 README 或 `docs/` 有对应说明；
4. 涉及 `settings` 的，默认值和取值范围写进 `docs/DEV-STANDARDS.md` 的参数表。
