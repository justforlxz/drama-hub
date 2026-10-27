# 测试规范

适用于 `drama-hub`。目标只有一个：**协议正确性与缓存正确性不靠人眼，靠测试挡住。**

## 三层测试，各管各的

| 层 | 形式 | 管什么 | 不跑什么 |
|---|---|---|---|
| 单元 | `go test`，无 I/O | 纯逻辑：集号派生、segment 切点计算、`seq → (集号, 秒)`、LRU 淘汰顺序、settings 范围校验 | 不起 HTTP、不碰磁盘、不调 ffmpeg |
| 集成 | `go test`，`httptest` + 真 SQLite（临时文件）+ 真 ffmpeg | 端点契约：Range/206、Content-Length、token、playlist 合法性、并发闸 | 不访问外网、不连真 Emby |
| 验收 | 手工清单，真 Emby + 真 rclone | 播放、拖动、续播、电视墙 | 不做成自动化 |

单元与集成都在 `make test` 内；验收清单在 `docs/ACCEPTANCE.md`，每阶段合并前跑一次并留记录。

## 必须真跑的（这些没测试不许合并）

1. **`Content-Length` 精确性**：`strm` / `nfo` / 图片 三个 GET 的 `Content-Length` 必须等于实际 body 长度，且 `PROPFIND` 里的 `getcontentlength` 与之一致。断言方式：比较 header 与 `len(body)`，不是断言魔数。
2. **Range 语义**：单集合成的 `206` + `Content-Range: bytes a-b/total`；越界返回 `416`；`HEAD` 返回相同的 `Content-Length` 但不返回 body。
3. **分片边界**：给定 fixture 的 `segment` 表，断言 `seq` 到 `(ep_no, start_sec, dur)` 的映射与总量等于各集时长之和（±一帧容差）。**这是合并集正确性的唯一守门测试，不允许跳过。**
4. **playlist 合法性**：自造 m3u8 必须能被 `ffprobe` 解析出预期总时长，且集边界处有 `#EXT-X-DISCONTINUITY`；分片串起来解码无错误（`ffmpeg -v error -i playlist -f null -` 返回 0）。
5. **token**：过期 / 篡改 / 缺失 → `401`；有效 token 换一个 `uid` → `403`。
6. **缓存策略**：超过 `cache_max_mb` 触发淘汰；被 pin 的剧不淘汰；`last_used_at` 被访问更新。用假时钟，不用 `time.Sleep`。
7. **settings**：非法值（越界、非数字）→ `400` 且不写入；合法值生效且下次读回一致。

## 禁止的测试（写了要删）

- 断言 mock 被调用（测的是自己的假实现，不是代码行为）。
- 断言模板渲染出的字符串片段（`assert.Contains(body, "第1集")`）—— 该断言的是解析后的结构或协议字段。
- 断言 getter / setter / 结构体零值。
- 依赖真实外网：任何测试访问 `hongguoduanju.com` 或 CDN 都视为缺陷。
- 依赖真实时间：`time.Sleep` 超过 100ms、或断言「现在」的测试。
- 只断言「不报错」或「长度 > 0」。

## 测试数据

- fixture 放 `testdata/`，单个视频 fixture **超过 500 KB 视为缺陷**。
- 视频 fixture 由 `scripts/make-fixtures.sh` 用 ffmpeg 现场生成（5 秒、两个不同编码参数的小 mp4 + 一个 m3u8），生成物进 `.gitignore`，脚本进版本库。
- 上游 HTTP 全部用 `httptest.Server` 冒充：假官网返回固定的 SSR HTML 片段，假 CDN 支持 Range。**不要在测试里 mock 我们自己的 handler** —— handler 必须是真跑的。
- 每个 fixture 附带一段注释说明它覆盖的场景（如「两集 codec 签名不一致」）。

## 集成测试写法

- 一个测试函数包含：建临时 DB → 插入 fixture 数据 → 起 `httptest.Server` → 发真实 HTTP 请求 → 断言响应。
- 用 `t.Cleanup` 清理临时目录与子进程；ffmpeg 子进程必须被杀干净（测试结束检查没有残留进程）。
- 只对**响应**做断言，不对内部函数调用顺序做断言。
- 需要并发时用 `-race` 跑（`make test` 已带）。

## 回归测试

修 bug 必须配一个能在修复前失败的测试。PR 描述里写清：「修复前这条测试红在哪一行」。没有失败复现的修复视为未完成。

## 覆盖率

**不设数字目标。** 但以下路径必须有显式测试，缺一条打回：

- 上游返回空 / 非 200 / HTML 结构变了（选择器找不到 `vid_list`）
- 源直链 403（签名过期）→ 刷新一次；仍失败 → `502` 且不写脏数据
- 合并集里有集缺 `segment`（索引未完成）→ 该集播放返回 `503` + `Retry-After`，不返回半截流
- 分片转封装失败 → 返回 `502`，不缓存失败结果
- DB 写失败 → 请求失败且连接可用（断言后续请求仍 `200`）

## 提交前

`make check`（= `go vet` + `staticcheck` + `go test -race ./...`）。不通过不提交。
