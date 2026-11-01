# 测试规范

适用于 `drama-hub`。目标只有一个：**协议正确性与缓存正确性不靠人眼，靠测试挡住。**

## 三层测试，各管各的

| 层 | 形式 | 管什么 | 不跑什么 |
|---|---|---|---|
| 单元 | `go test`，无 I/O | 纯逻辑：集号派生、segment 切点计算、`seq → (集号, 秒)`、HMAC 签名校验、settings 范围校验 | 不起 HTTP、不碰磁盘、不调 ffmpeg |
| 集成 | `go test`，`httptest` + 真 PostgreSQL（每个测试一个独立 schema）+ 真 ffmpeg | 端点契约：Range/206、Content-Length、token、playlist 合法性、并发闸 | 不访问外网、不连真 Emby |
| 验收 | 手工清单，真 Emby + 真 rclone | 播放、拖动、续播、电视墙 | 不做成自动化 |

单元与集成都在 `make test`（即 `scripts/ci.sh --test`）内，分两级：unit 不碰数据库、容器
`--network=none`；integration 起 `postgres:16` 服务容器，测试容器同网络、用
`DRAMA_HUB_TEST_DB_URL` 连它。测试的上游一律 mock（`httptest` / `testdata` fixture），
`scripts/check-test-upstream.sh` 会断言测试代码里没有真实上游域名。

验收清单在 `docs/ACCEPTANCE.md`，每阶段合并前跑一次并留记录。

## 必须真跑的（这些没测试不许合并）

1. **`Content-Length` 精确性**：`strm` / `nfo` / 图片 三个 GET 的 `Content-Length` 必须等于实际 body 长度，且 `PROPFIND` 里的 `getcontentlength` 与之一致。断言方式：比较 header 与 `len(body)`，不是断言魔数。
2. **Range 语义**：单集合成的 `206` + `Content-Range: bytes a-b/total`；越界返回 `416`；`HEAD` 返回相同的 `Content-Length` 但不返回 body。
3. **分片边界**：给定 fixture 的 `segment` 表，断言 `seq` 到 `(ep_no, start_sec, dur)` 的映射与总量等于各集时长之和（±一帧容差）。**这是合并集正确性的唯一守门测试，不允许跳过。**
4. **playlist 合法性**：自造 m3u8 必须能被 `ffprobe` 解析出预期总时长，且集边界处有 `#EXT-X-DISCONTINUITY`；分片串起来解码无错误（`ffmpeg -v error -i playlist -f null -` 返回 0）。
5. **出流签名**（ADR-0003）：正确签名放行；篡改或缺失 → `401`。用 `crypto/hmac.Equal` 常数时间比较，**不测过期时间、不测 token 表**——这两样都不存在，写了就是给自己造需求。
6. **settings**：非法值（越界、非数字）→ `400` 且不写入；合法值生效且下次读回一致。
   `PATCH /api/settings` 的批量写入（MKACG-622）：整批里有未知键或非法值时**一行都不写**；
   写进去的值返回归一化后的形态（`strm_prefix` 去尾 `/`）、密码与密钥回 `null` 不回显。
6b. **写操作令牌**（MKACG-622）：`POST` / `DELETE` / `PATCH /api/*` 无令牌、错令牌都 `401`
   且不产生副作用（没触发同步、没写库）；没配 `DRAMA_HUB_ADMIN_TOKEN` 时同样 `401`（fail
   closed）；`GET /api/*` 与三页界面不受影响；令牌本身不出现在任何响应体里。
7. **env 引导配置**（MKACG-616）：空库 + `DRAMA_HUB_STREAM_SECRET` / `DRAMA_HUB_CACHE_DIR`
   写入 settings；再跑一次不覆盖手改值；env 缺省不写空值；非法值（短 secret）不落库、
   不 panic；WebDAV 两个凭据 env **不写库**，并按未知变量 warn 列出支持的键。
8. **本地缓存**（`docs/CACHE-MODEL.md`「测试要求」的 6 条，ADR-0007 已把缓存收回本服务，
   淘汰有可测逻辑了）：命中不回源（第二次生成次数不变）、命中/未命中字节一致、生成失败
   不写缓存（无行、目录不变）、按 `cache_max_mb` 淘汰后容量回落且最久未用先走、
   `cache_lru_days` 过期后行与文件都不在、10 个并发同分片生成次数 = 1。
9. **缓存原子性与范围**（S7）：写入中途 kill 后三个内容目录里只有完整文件（半截文件只
   会留在 `tmp/`，由淘汰清扫）；m3u8 文本与源整片没有对应 kind、写不进缓存。

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

## 测试只有一条路径：容器

- `make test` 就是跑容器，**没有第二条路径**，因此不需要 `//go:build` 标签来区分本机与容器测试。
- 测试环境与 runtime 用**同一个 ffmpeg 二进制**（e2e 段 `FROM runtime`），切片时长与 TS 产物在测试与线上一致。
- 测试二进制在 build 段（自带 gcc）编译为 `-race`，再拷进 e2e 镜像；e2e 里没有 Go 工具链，镜像小、测试不用每次重新编译。
- `--network=none`：任何访问外网的测试会直接失败，这是刻意的。假上游用 `httptest.Server`（容器内 localhost 可用）。
- 测试产物写 `/tmp`（tmpfs），不写进只读挂载的源码目录。

## 提交前

`make check`（= `go vet` + 容器内测试）。合并前额外跑 `make env-check` 确认镜像未漂移。
