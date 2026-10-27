# ACCEPTANCE — 真机验收清单

每阶段合并前跑一次，结果贴到对应 issue。不是自动化测试的替代品。

## 前置

- 一台装了 Emby 的机器；`rclone` 可执行；`ffmpeg` / `ffprobe` 可执行。
- 库里只放测试剧，避免污染主库。

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
