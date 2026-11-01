# ACCEPTANCE — 真机验收清单

每阶段合并前跑一次，结果贴到对应 issue。不是自动化测试的替代品。

## 前置

- 一台装了 Emby 的机器；`rclone` 可执行；`ffmpeg` / `ffprobe` 可执行。
- 库里只放测试剧，避免污染主库。

## P2（虚拟 WebDAV + NFO）

> `PROPFIND` 口径：只支持 `Depth: 0` / `Depth: 1`。数值深度 ≥2 由 `x/net/webdav`
> 直接回 `400`，这不是缺陷；验收只按逐层 `Depth: 1` 走（rclone / Emby 本来就这么扫）。

- [ ] 挂载点是 `/webdav`（`rclone config create dramahub webdav url http://127.0.0.1:8080/webdav vendor other`）
- [ ] `rclone lsf --max-depth 5 dramahub:/` 输出五级：分类 → 分片 → 剧 → Season 01 → strm/nfo
- [ ] 顶层只有分类目录（没有 `全部/`），分类数等于库里的 `SELECT DISTINCT genre`
- [ ] `rclone lsf --max-depth 2 dramahub:/<分类>` 只给出分片目录：`00`–`7f` 全部 128 个
      （空分片是空目录，列它不产生查询）
- [ ] `rclone size dramahub:/` 不报错（读不到长度会在这里暴露）
- [ ] `rclone cat "dramahub:/<分类>/<分片>/<剧名> (<年份>)/Season 01/<集>.strm"` 输出一行
      `{strm_prefix}/video/{id}.m3u8`
- [ ] Emby 扫描后：剧名、年份、封面、集号连续（无跳号）、nfo 标题是「第 N 集」或「第 A-B 集」
- [ ] Emby 库配法（两种都支持，先试第一种）：
      1. 一个「剧集」库，媒体文件夹 = DAV 根（`/mnt/dramahub`），Emby 逐层往下扫；
      2. 若 Emby 在 DAV 根上把分类当成剧，退一步把具体分类目录（`/webdav/爱情` 等，
         真实词表 68 个）逐个加成媒体文件夹。

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
