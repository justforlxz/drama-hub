# ADR-0005：SQLite 驱动选 modernc.org/sqlite

状态：已接受（2026-10-28）

## 背景

`database/sql` 是接口层，本身不含任何驱动。F2 的 settings 读写要落到
SQLite（AGENTS.md 硬约束 4：单二进制 + SQLite），因此必须引入第一个第三方依赖。
DEV-STANDARDS「依赖与技术选型」已把驱动列为 `modernc.org/sqlite`（纯 Go），
本 ADR 说明为什么标准库不够、以及为什么是它。

候选：

1. `github.com/mattn/go-sqlite3`：事实标准，但依赖 cgo。
2. `modernc.org/sqlite`：SQLite 源码经 cgo 剥离后的纯 Go 转译版。
3. 自己写 SQL 文件格式解析 / 用 `os/exec` 调 `sqlite3` 命令行：不成立。

## 决定

采用 **2**。

## 理由

- 方案 1 与「单二进制、`CGO_ENABLED=0`」冲突：cgo 会绑定目标平台的 libc，
  交叉编译要配整套交叉工具链，`-race` 之外还得维护 cgo 构建环境。
  Containerfile 的 build 段已经有 gcc，能把 cgo 编出来，但产物不再是纯静态二进制，
  部署形态随之变化。
- 方案 3 把数据库当外部进程：要多一个运行时依赖、要自己解析输出、
  拿不到事务与 `busy_timeout` 的语义，违背硬约束 4 的「没有第二个运行时」。
- 方案 2 的代价是编译产物更大（`modernc.org/libc` 约 300 MB 模块缓存）、
  首次构建更慢；换来 `CGO_ENABLED=0` 与零交叉编译负担。单机单用户的场景下值得。

## 后果

- `go.mod` / `go.sum` 出现第一批第三方依赖，必须入库：build 段的
  `COPY go.mod go.sum*` 依赖它。
- lint 段是独立 `FROM ${GOIMG}`、运行期 `--network=none` 且源码只读挂载，
  模块缓存必须在镜像构建期落进 `/go/pkg/mod`。因此 lint 段要补
  `COPY go.mod go.sum*` + `RUN go mod download`，否则离线 lint 必红。
- 驱动由 `internal/store` 在连接 SQLite 时注册（F3 的连接参数：WAL /
  `busy_timeout=5000` / `foreign_keys=ON`），其它包不 import 驱动。
- 版本随 `go.mod` 固定；升级驱动按普通依赖变更走，不需要新 ADR。
