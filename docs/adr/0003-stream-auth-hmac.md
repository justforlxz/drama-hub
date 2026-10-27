# drama-hub 出流鉴权（ADR-0003）

## 决策

出流接口 `/s/{token}/...` 的 `token` 用 **HMAC 常量签名**，无状态、无过期、无 token 表。

```
token = base64url( HMAC-SHA256( secret, "stream:" + uid ) )
```

secret 是配置项 `stream_secret`（settings 表，必填，长度 ≥ 32 字节）。校验时用同一 secret 重算比对，
`crypto/hmac.Equal` 做常数时间比较。

## 为什么（对应审计确认）

- **无过期**：设计 §5.2 要求缓存键稳定（`/s/{token}/e/{uid}` 要能被 rclone 缓存代理命中）。
  过期 token 会让 rclone VFS 缓存里的 URL 在过期后失效，缓存键直接漂移 —— 与缓存目标冲突。
  因此不能有过期、不能有状态、不能有表。
- **无状态**：不需要 `token` 表、不需要轮转、不需要时钟；校验是纯函数，O(1)。
- **代价**：泄漏即长期可用，直到换 secret。本场景单用户、本机/局域网，泄漏概率接近 0；
  换 secret 只需改 settings 一处，旧 strm 里的 URL 会失效（重新同步即可）。

## 范围边界

- 这个 token 只保护「知道 uid 就能看这个剧集内容」这层。**不**用于：
  - 用户登录/权限（单用户，不做多用户）
  - 防脚本抓取整个库（本机/局域网无此威胁）
- WebDAV 仍用独立的 `webdav_user` / `webdav_pass` Basic 认证，两者分开，不混用。

## 后果

- 缓存代理（rclone serve http）缓存的是签名 URL，key 长期稳定，命中不会因过期失效。
- 401/403 矩阵测试简化为：正确签名放行、错误签名拒绝（`crypto/hmac.Equal` 常数时间）。
- 无时钟偏移、无轮转、无表，少一个整类测试。
