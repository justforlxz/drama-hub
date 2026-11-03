# ACCEPTANCE — 真机验收清单

每阶段合并前跑一次，结果贴到对应 issue。不是自动化测试的替代品。

## 前置

- 一台装了 Emby 的机器；`rclone` 可执行；`ffmpeg` / `ffprobe` 可执行。
- 库里只放测试剧，避免污染主库。
- 机器判定用 `scripts/acceptance.sh`（A–I 逐项 PASS/FAIL/SKIP，用法见 `docs/DEPLOY.md` 第 7 节）。

## P2（虚拟 WebDAV + NFO）

> `PROPFIND` 口径：数值深度 ≥2 由 `x/net/webdav` 直接回 `400`（不是缺陷）；
> **省略 `Depth` 头等价于 `Depth: infinity`，会递归整棵树**（21.6 万剧的分类目录实测
> 369 MB / 116 s / 服务端 RSS +501 MB）。验收一律逐层 `Depth: 1`（rclone / Emby /
> Jellyfin 本来就这么扫），手工 `curl` 也必须显式带 `-H 'Depth: 1'`，
> 见 `docs/DIRECTORY-CONTRACT.md`「单目录子项上限」。

- [ ] 挂载点是 `/webdav`（`rclone config create dramahub webdav url http://127.0.0.1:8080/webdav vendor other`）
- [ ] `rclone lsf --max-depth 4 dramahub:/` 输出四级：分类 → 剧 → Season 01 → strm/nfo
- [ ] 顶层只有分类目录（没有 `全部/`），分类数等于库里的 `SELECT DISTINCT genre`
- [ ] `rclone lsf --max-depth 1 dramahub:/<分类>` 只给出剧目录（**没有**两位十六进制的
      分片目录段；`00`–`7f` 已随 v5 下线，`grep -E '^[0-9a-f]{2}/$'` 必须零命中）
- [ ] `rclone size dramahub:/` 不报错（读不到长度会在这里暴露）
- [ ] `rclone cat "dramahub:/<分类>/<剧名> (<年份>)/Season 01/<集>.strm"` 输出一行
      `{strm_prefix}/video/{id}.m3u8`
- [ ] Emby 扫描后：剧名、年份、封面、集号连续（无跳号）、nfo 标题是「第 N 集」或「第 A-B 集」
- [ ] Emby / Jellyfin 库配法（**必须**按第一种，MKACG-651 实测）：
      1. 一个「剧集」库，媒体文件夹 = **每个分类目录各一个**（`/mnt/dramahub/爱情` 等，
         真实词表 68 个）。这时 Series ≈ 该分类的剧数、Episode ≈ 该分类的集数，
         不会出现「第 66 季」这类由中间目录层造成的假季。
      2. 库根挂 DAV 根（`/mnt/dramahub`）会**把分类目录当 Series**（nas 实测：1436 部剧
         被识别成 59 部），只用于记录行为、不作为通过条件。见
         `docs/DIRECTORY-CONTRACT.md`「被推翻的做法」与「单目录子项上限」。

## P3（单集出流）

- [ ] Emby 里点播单集：能播、有总时长、能拖到中间
- [ ] 播放时 `curl -sI <strm URL>` 返回 `Accept-Ranges: bytes` + 正确 `Content-Length`
- [ ] 断网重连后同一集续播不从头开始

## P4（缓存代理）

- [ ] 第一次播放后 `du -sh <缓存目录>` 增长
- [ ] 第二次播放同一集：代理日志无回源请求
- [ ] 强杀代理后重放：仍能播（回源兜底）

## P5（合并集：自动分块 + 运行时现算，ADR-0010 / ADR-0014）

默认 `merge_chunk_size=10`，不需要人工编排；`scripts/acceptance.sh` 的 G 段把下面几项
做成可判定的检查。

- [ ] 22 集的剧对外 3 集（10 + 10 + 2），每集 `.nfo` 标题分别是「第1-10集」「第11-20集」「第21-22集」
- [ ] 剧集数少于 N（如 3 集）→ 对外 1 集，标题「第1-3集」
- [ ] 集数是 N 的整倍数 → 不多出空组
- [ ] 合并集在 Emby 里显示为 1 集，时长 ≈ 各集之和（误差 < 2 s）
- [ ] 拖动到中段能立即播放，画面无花屏；末尾回拖到开头不黑屏、时长条不错乱
- [ ] 播放完毕 Emby 记录进度正确
- [ ] 后台把 `merge_chunk_size` 改成 5，**不触发任何同步/重建**，`ls` 立刻看到同剧对外
      5 集（5+5+5+5+2）——库里没有派生表，改完下一次列举就是新分块（ADR-0014）
- [ ] 改 N 后旧合并集的 `/video/{oldid}.m3u8` 返回 404（签名仍合法，不得返回别人的内容）
- [ ] 改回原 N（同样不同步）后对外集号与文件数复原；同一 N 下重复列举逐字符相同

## P6（每集一片 fMP4，ADR-0011 / MKACG-652）

- [ ] 单集 playlist 恰 **1** 个 `.m4s`，`#EXTINF` ≈ 该集真实时长（不是 10 秒）
- [ ] 单集分片落盘后 `head -c 8 | tail -c 4` 是 `ftyp`：`ffprobe` 单看这一片就能给出
      视频流与完整时长（自包含，不依赖 `#EXT-X-MAP`）
- [ ] 合并集整条 playlist 交给 ffmpeg 系播放端**不丢帧**：
      `ffprobe -select_streams v -count_frames -count_packets -show_entries
      stream=nb_read_frames,nb_read_packets <playlist>` 的 `frames` 必须等于 `packets`
      （集边界时间戳回退会让解码器丢帧，只算 `#EXTINF` 之和看不出来）
- [ ] **不取 playlist、直接请求合并集的分片**（模拟首播并发 / CDN 预取）：逐片
      `ffprobe -show_entries packet=pts_time` 的首 PTS 必须是「前面各集时长之和」而不是全 0
      （只请求分片时服务端要先补齐时长再平移，否则拼成 playlist 又会丢帧）
- [ ] 合并集（10 集）playlist 有 **10** 个分片、**9** 个 `#EXT-X-DISCONTINUITY`，
      `#EXTINF` 之和 ≈ Σ 各集时长；逐片单独 `ffprobe` 都成功
- [ ] 同一集连续播两次：第二次日志里没有新的 `msg="segment generated"`
- [ ] 连播 3 集后 `<cache_dir>/seg/` 下是 **3** 个 `.m4s`（不是 18 个）
- [ ] 浏览器（hls.js）与 Emby 各播一集：画面与声音都在。
      **若仍只有进度与声音**：先看播放端是否支持 HEVC（`isTypeSupported('video/mp4;
      codecs="hvc1.1.6.L120.B0"')`，或在 <https://ott.dolby.com/codec_test/> 上试）；
      流本身不是候选原因（同一片 `ffmpeg -i … -f null -` 无错误），转码是独立决策
- [ ] `scripts/acceptance.sh` 的 `F3` / `F7` 输出 `n/n 分片是自包含 fMP4 且含视频流`

## 记录格式

```
阶段：P5
日期：YYYY-MM-DD
Emby 版本：
结论：通过 / 不通过（列出失败项）
证据：命令 + 输出片段
```
