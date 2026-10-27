# drama-hub

短剧媒体管理器：把红果短剧的数据变成一个 Emby 能识别的剧集库。

对外的三个面：只读虚拟 WebDAV（元数据）、出流服务（视频字节）、管理 API/WebUI。

设计基线是 MKACG-580 的附件 `DESIGN-drama-hub.md`。需要背景时读它，不要凭印象改架构。

## 做什么 / 不做什么

做：红果数据同步、虚拟文件树、NFO/STRM 生成、单集与合并集出流、分片缓存策略、三页管理界面。

不做：播放器、下载器、转码流水线、字幕/弹幕、多用户权限、多租户。Emby 是唯一的播放客户端。

## 硬约束

改动前逐条对照。违反任一条的方案直接否决，不进入评审。

1. **虚拟，不落盘。** strm / nfo / 目录树全部按请求渲染。磁盘上只允许两类文件：图片缓存、分片缓存。禁止把剧集元数据导出成实体目录。
2. **合并集只有一种实现：虚拟 HLS 分片聚合。** 禁止物化合并文件、禁止字节拼接多集 MP4（`moov` 只描述首集，拼字节必然时长错、seek 崩）。这条是两个方案对比后定的，不要再试。
3. **每个虚拟文件必须给出精确 `Content-Length`。** WebDAV 的 `PROPFIND` 少了它，rclone mount 读不到文件、Emby 扫不出集。渲染 → 算长度 → 再返回，一次请求内完成。
4. **单二进制 + SQLite。** 没有 Node、没有前端构建步骤、没有第二个运行时、没有外部缓存服务。界面用 Go 模板 + 少量原生 JS，模板随二进制嵌入（`embed`）。
5. **可调参数进 `settings` 表。** strm 前缀、缓存上限、LRU 天数、分片时长、预取数、pin 天数全部运行时可变，改完即时生效。代码里出现这类魔法数字视为缺陷。
6. **对外集号只由 `emby_item` 派生表决定。** 合并调整只重建派生表，`drama` / `episode` 原始数据不动。
7. **出流用字节透传，不用 302。** 302 到签名 CDN 会让缓存键漂移、过期链接命中旧缓存返回错内容。

## 目录

```
cmd/drama-hub/          main，只做装配，不含业务逻辑
internal/provider/      数据源适配（官网 SSR 主力、App 接口回退）
internal/store/         SQLite、迁移、派生表重建
internal/vfs/           虚拟文件树 → strm/nfo/图片 渲染
internal/webdav/        x/net/webdav 自定义 FileSystem
internal/stream/        出流：HEAD/GET/Range/token
internal/segment/       关键帧索引、播放列表生成、分片转封装
internal/cache/         分片缓存策略、LRU、pin
internal/web/           管理 API + 页面模板
docs/                   规范与 ADR
```

依赖方向单向：`cmd` → `internal/web`/`webdav`/`stream` → 领域包 → `store`。领域包之间不互相 import，需要协作时由上层编排。反向 import 一律打回。

## 命令

```
make build      # 编译到 ./bin/drama-hub
make run        # 本地启动（默认 :8080，DB 在 ./data/）
make test       # go test -race ./...
make lint       # go vet + staticcheck
make check      # lint + test，提交前跑这个
```

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
