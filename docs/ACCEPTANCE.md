# ACCEPTANCE — 真机验收清单

每阶段合并前跑一次，结果贴到对应 issue。不是自动化测试的替代品。
本轮（R2 收尾）的记录在文末「2026-10-30 记录」，逐条对应下面的清单项。

## 前置

- 一台装了 Emby 的机器；`rclone` 可执行；`ffmpeg` / `ffprobe` 可执行。
- 库里只放测试剧，避免污染主库。
- 配置齐全（`settings` 的 `webdav_user` / `webdav_pass` / `stream_secret` / `strm_prefix`），
  写入方式见 `docs/OPS.md` §2.4。

## P2（虚拟 WebDAV + NFO）

- [ ] `rclone lsf --max-depth 3 dramahub:/` 输出三级目录：剧 → Season 01 → strm/nfo
- [ ] `rclone size dramahub:/` 不报错（读不到长度会在这里暴露）
- [ ] `rclone cat dramahub:/短剧/<剧>/Season 01/<集>.strm` 输出一行 URL 且前缀是配置的 `strm_prefix`
- [ ] Emby 扫描后：剧名、年份、封面、集号连续（无跳号）、nfo 标题是「第 N 集」或「第 A-B 集」

## P3（单集出流）

- [ ] Emby 里点播单集：能播、有总时长、能拖到中间
- [ ] 播放时 `curl -sI <strm URL>` 返回 `Accept-Ranges: bytes` + 正确 `Content-Length`
- [ ] 断网重连后同一集续播不从头开始

## P4（缓存代理）

- [ ] 第一次播放后 `du -sh <缓存目录>` 增长
- [ ] 第二次播放同一集：代理日志无回源请求
- [ ] 强杀代理后重放：仍能播（回源兜底）

## P5（合并集）

- [ ] 合并集在 Emby 里显示为 1 集，标题「第 1-10 集」，时长 ≈ 各集之和（误差 < 2 s）
- [ ] 拖动到 10 分钟（应在第 4 集附近）能立即播放，画面无花屏
- [ ] 从合并集末尾回拖到开头，不黑屏、时长条不错乱
- [ ] 播放完毕 Emby 记录进度正确

## 记录格式

```
阶段：P5
日期：YYYY-MM-DD
Emby 版本：
结论：通过 / 不通过（列出失败项）
证据：命令 + 输出片段
```

---

# 2026-10-30 记录（R2 收尾 · MKACG-599）

## 环境

| 项 | 值 |
|---|---|
| 被测版本 | `main` @ `d0bb513`（`feat(web): 落地管理界面三页与管理 API (#13)`） |
| 产物 | `./scripts/ci.sh` 构建出的 runtime 镜像（`--target runtime`，二进制静态编译，版本锁 go1.27.3 / ffmpeg 6.1.2 / staticcheck 0.8.2） |
| 运行方式 | `podman run -d -p 18272:8080 -v /tmp/mk599/cdata:/data localhost/drama-hub-envcheck:latest` |
| 数据库 | 全新空库（首启自动建库并迁移） |
| 上游数据 | 真实官网剧目 `7503745441910017049`（《宴律，你的白月光回国了》，76 集） |
| rclone | v1.75.2（`webdav` 类型，指向 `/dav/`） |
| Emby | **本机无 Emby 实例**，Emby 客户端相关的 4 项未验（见下） |
| 宿主 ffmpeg | 9.0.2；本轮所有被验证的请求路径都**没有调用 ffmpeg**（切片在索引未就绪时就 503 返回），故版本差异不影响本轮结论 |

复跑前先按 `docs/OPS.md` §2.4 写 4 条 `settings`（本轮用 `sqlite3` 直写），否则 WebDAV 恒 401。

## 逐项结果

| 项 | 结果 | 证据 |
|---|---|---|
| P2-1 三级目录 | ✅ | `rclone lsf --max-depth 3 dramahub:/` → `宴律，你的白月光回国了 (2025)/`、`…/Season 01/`、`…/tvshow.nfo`；`-R` 递归共 155 条（2 目录 + 1 tvshow.nfo + 76×2 文件） |
| P2-2 `rclone size` | ✅ | `Total objects: 153` / `Total size: 19.899 KiB (20377 Byte)`（153 = 1 + 76×2，长度来自渲染字节数） |
| P2-3 strm 前缀 | ✅ | `rclone cat … - S01E01 - 第1集.strm` → `http://127.0.0.1:18272/s/W6bwVny-…/e/e1`，前缀等于配置的 `strm_prefix`，一行 URL + 尾换行 |
| P2-4 Emby 扫描 | ❌ 未验证 | 本机无 Emby 实例。可核对的产物面：nfo 标题为 `第1集`、`<season>1</season>`、`<episode>1</episode>`（对外集号连续、无跳号；建合并组后 76 源集 → 74 对外集，`S01E01` 变 `第1-3集`）。但**封面不会被渲染**（虚拟树只产出 `.strm`/`.nfo`），`<runtime>` 为 `0`（同步不采集时长），故即使装 Emby 也拿不到封面与正确时长 |
| P3-1 Emby 点播 | ❌ 不通过 | 同一 URL 的 `curl -I` → `HTTP/1.1 502 / Content-Length: 75`，body `{"code":"upstream",…}`；库内 `select count(*) from episode where play_url<>''` = 0。原因：同步不写 `play_url`，`provider.FetchDirectURL` 无生产调用方；上游只公开首集预览片（见下条） |
| P3-2 `Accept-Ranges` + `Content-Length` | ❌ 不通过 | 502 响应没有 `Accept-Ranges`（上游未定状态码之前不会写视频头）：`status=502 bytes=75`（带 `Range: bytes=0-1023` 也一样） |
| P3-3 断网续播 | ❌ 无法验证 | 无字节可播，续播语义无从产生 |
| P4-1 首次播放后缓存增长 | ❌ 无法验证 | 播放未成立（P3 502），缓存代理目录不会增长 |
| P4-2 二次播放无回源 | ❌ 无法验证 | 同上：无分片请求，代理侧无从产生「回源 / 命中」之分。另外**代理接法本身也没验证通过**：按 ADR-0004 的候选接法（`http` 远端 + `rclone serve http --vfs-cache-mode full`）实测，`rclone lsl dramahub-cdn:/healthz` 能取到 16 字节（远端是通的），但 `serve http` 下所有路径都返回 `404 File not found`（日志 `healthz: … File not found`；`http` 远端不支持目录，而服务根 `HEAD /` 返回 200，rclone 把根判成一个文件） |
| P4-3 强杀代理后仍能播 | ❌ 无法验证 | 同上（无字节可播，也没有验证通过的代理可杀） |
| P5-1 合并集显示 1 集 / 标题 / 时长 | ⚠️ 部分 | 编排与派生表成立：`POST /api/dramas/1/merge-groups {"episode_ids":[1,2,3]}` → 201，`emby_item` 变成 `1|m1|merged`、`2|e4`、`3|e5`，对外集 76→74，`S01E01` 文件名变 `第1-3集`。Emby 侧显示未验（无实例），时长校验无从谈起（`<runtime>` 为 0 且无字节） |
| P5-2 拖动到 10 分钟能播 | ❌ 不通过 | `GET /s/{token}/e/m1` → `503` + `Retry-After: 5`，body `{"code":"unavailable","message":"合并组成员剧集 1 的分片索引未就绪: 暂不可用"}`；`GET /s/{token}/seg/m1/0.ts` 同为 503。原因：`segment.Cutter.Index` 没有生产调用方（无端点、无 CLI、管理面无按钮），`segment` 表恒空 |
| P5-3 末尾回拖不黑屏 | ❌ 无法验证 | playlist 未生成（503），无时间轴可拖 |
| P5-4 Emby 进度记录 | ❌ 无法验证 | 无 Emby 实例，且播放未成立 |

## 结论

**不通过。** 阻塞点不在验收动作，而在产品能力，三条都是**代码里没有任何调用方**的既定缺口
（`grep` 非测试代码可复现）：

1. **整集字节来源缺失**：`episode.play_url` 永远为空 → 单集出流恒 502。
   - `provider.SyncSeries` 按契约把 `play_url` 留空；`Source.FetchDirectURL` 无生产调用方。
   - 上游真实情况（2026-10-30 实测）：首集页面有 `contentUrl = …/video/public/<vid>/360p.mp4`，
     `curl -I` → 200、`content-length: 750269`、`accept-ranges: bytes`；
     `ffprobe` → `duration=10.066667`（即 10 秒预览片，不是整集）；
     第 2 集页面 `contentUrl` 匹配数为 0（没有直链）。
   - 整集直链属 P6（App 回退）范围，**尚未实现**，且在 MKACG-592 / MKACG-595 里是留给 PL 的决策项。
2. **分片索引无触发点**：`segment.Cutter.Index` 无生产调用方 → `segment` 表恒空 →
   合并集 playlist 与分片端点恒 503（R1 交付时也记录了「没加这个死按钮」）。
3. **封面与时长不产出**：虚拟树只渲染 `.strm`/`.nfo`，`internal/cache` 仍是空壳；
   同步不采集 `duration_sec`，集 `.nfo` 的 `<runtime>` 恒为 0。
4. **缓存代理接法未验证**：ADR-0004 指定 rclone `serve http`，但 `http` 远端 + `serve http`
   实测所有路径 404（远端本身通，VFS 把根判成文件）。P4 的三条因此连"代理在链路里"这一前提都没立起来。
5. **配置无写入 API**：`settings` 只有 `Settings.Set` 进程内写入，没有 HTTP 入口；本轮引导配置
   靠 `sqlite3` 直写（写法见 `docs/OPS.md` §2.4），也绕过了范围校验。

补充：本机无 Emby 实例，P2-4 与 P5-1 的 Emby 侧显示、P3-1 的点播、P5-4 的进度记录
没有在 Emby 里验过。**但即使补上 Emby 也过不了**——P3/P5 的失败发生在字节层（502/503），
与客户端无关。

## 复跑方法

缺口 1、2 被决策并落地后，按下列顺序复跑（每步带超时，别用无界命令）：

```bash
# 0) 起服务（容器或本机二进制皆可），确认配置齐全
curl -s localhost:8080/api/status | jq '.config'
# 1) 同步剧目
curl -sS -X POST -H 'Content-Type: application/json' \
  -d '{"series_id":"7503745441910017049"}' localhost:8080/api/dramas
# 2) P2：目录 / 长度 / strm 前缀
rclone lsf --max-depth 3 dramahub:/ ; rclone size dramahub:/
rclone cat "dramahub:/<剧>/Season 01/<集>.strm"
# 3) P3：长度与 Range（用上一步的 URL）
URL=$(rclone cat "dramahub:/<剧>/Season 01/<集>.strm" | tr -d '\n')
curl -sI "$URL"                               # 期望 200/206 + Accept-Ranges + Content-Length
curl -s -o /dev/null -w '%{http_code} %{size_download}\n' -H 'Range: bytes=1000000-1001023' "$URL"
# 4) P4：缓存代理（strm_prefix 指向代理）
du -sh <rclone vfs 缓存目录>                   # 播放后增长
# 5) P5：建合并组 → playlist → 时间轴断言（宿主 ffmpeg 会因版本不同产生差异，用镜像内 ffmpeg）
curl -s localhost:8080/s/<token>/e/m1 | head -20        # 期望 200 + m3u8 + DISCONTINUITY
ffprobe -v error -show_entries format=duration -of default=nw=1 <playlist 路径>
ffmpeg -v error -ss 600 -i <playlist 路径> -f null -    # 10 分钟处解码无错误
```

Emby 侧（P2-4、P3-1、P5-1、P5-4）只能在装有 Emby 的机器上补：库路径指向
`rclone mount` 的挂载点，类型「电视节目」，扫描后按上方清单逐条看。
