# ADR-0009：PostgreSQL 作为唯一存储后端

状态：已接受（2026-10-31，lxz 定稿）
取代：ADR-0002 的存储部分（单二进制内嵌数据库）、ADR-0005（内嵌数据库驱动的选型）

## 背景

2.0 起这条线要走到多副本（MKACG-619：同机/多机跑 ≥2 个实例共用一份数据）。
内嵌数据库的写者是进程自己：第二个实例要么拿不到同一份数据，要么在共享文件系统上
靠文件锁硬撑——两种都不成立。单进程写者同时也是吞吐上限（同步、合并、缓存淘汰
全在同一条写队列后面排队）。

同时期这条线的部署形态已经明确：一台机器上跑容器，数据库就在同一台机器（或同一
内网）上，不存在「不想装数据库」这个约束——它本来就要装 Emby、rclone、ffmpeg。

## 决定

- **PostgreSQL 是唯一存储后端**（单后端，不留内嵌数据库代码路径，不做双后端开关）。
- 驱动：`github.com/jackc/pgx/v5` 的 `stdlib` 适配层（`database/sql` 接口不变，
  `internal/store` 的调用形态不动）。选它是因为 v5 是活跃维护的主线，池、协议、
  SQLSTATE 都是原生支持；`lib/pq` 已进入维护模式。
- 连接串由 `DRAMA_HUB_DB_URL`（`postgres://…`）给，是**必填项**；连接池参数
  （`DRAMA_HUB_DB_MAX_OPEN_CONNS` / `DRAMA_HUB_DB_MAX_IDLE_CONNS` /
  `DRAMA_HUB_DB_CONN_MAX_LIFETIME_SEC`）走 env。
- 占位符改为 PostgreSQL 的 `$n`（含 `IN (…)` 的序号偏移），不再用 `?`。
- 迁移版本真值从 `PRAGMA user_version` 换成 `schema_migrations(version INT PK,
  applied_at TIMESTAMPTZ)`；现有 4 份迁移文件**文件名与序号不变**，内容改写为 PG DDL。
- 启动时自动迁移；并发保护用 `pg_advisory_lock`（会话级，锁与迁移共用一条连接）：
  多副本同时启动只有一个真正执行，其余等锁后读到新版本、全部跳过。
- 不提供 down 迁移（只向前）；不写存量数据迁移工具（2.0 没有需要迁移的存量数据）。
- 迁移失败**立即退出**，不允许吞错继续提供半迁移服务；错误文本带失败文件名与失败前版本。
- 业务时间列（`sync_at` / `last_used_at` / `source_state.*` / `settings.updated_at`）
  **仍然是 UTC RFC3339 的不透明 TEXT**：它们靠「空串 = 还没发生过」表达语义，改成
  `TIMESTAMPTZ` 属于业务语义变更，不在本次范围。新增的 `schema_migrations.applied_at`
  才用 `TIMESTAMPTZ`。
- 排序敏感的 `title` / `genre` 显式 `COLLATE "C"`：保留原内嵌库的字节序语义，
  目录分页顺序与索引排序不随数据库 locale 漂移。

## 后果

- 部署多一个必选依赖：没有可连的 PostgreSQL 就启动失败（`DRAMA_HUB_DB_URL` 缺失或连不上）。
  `AGENTS.md` 硬约束 4 已改写，不再宣称「无外部依赖」。
- 两处语义变化必须记住，都来自「数据库自己会做守门」：
  - 唯一/主键冲突统一是 SQLSTATE `23505`，`wrapWriteErr` 一条规则映射到 `ErrConflict`；
  - 写事务的锁序必须全仓统一为「drama → 子表」：两条事务以相反顺序请求同两行就是
    PostgreSQL 的 40P01 死锁（内嵌库单写者时不存在这条失败路径）。`lockDrama`
    （`internal/store/lock.go`）是唯一加锁点：同步事务的第一步 `UpsertDrama` 就已
    锁住 drama 行，首播回填因此先锁 drama 再 `UPDATE episode`；合并编排、派生表
    重建同理。派生表重建的「先删后插」也靠这把锁串行（READ COMMITTED 下两个并发
    重建各自按快照删、再插入同一批行，否则撞唯一约束）。
  - 已接受的失败模式：并发删剧与「按剧写入子表」相遇时，后拿到锁的事务可能因
    外键（23503）或以死锁检测（40P01）失败。两者都是「整笔回滚、数据不脏、重试
    即可」；这是显式取舍，不是遗漏（见 `docs/DEV-STANDARDS.md`「数据库」的锁序条款）。
- CI 分两级（`scripts/ci.sh`）：
  - `unit`：`--network=none`，纯函数与不碰数据库的包；
  - `integration`：起 `postgres:16` 服务容器（带 healthcheck，镜像 digest 锁在
    `env.lock`），测试容器同自定义网络，用 `DRAMA_HUB_TEST_DB_URL` 连它；每个测试
    拿一个独立 schema（`internal/pgtest`），结束 DROP。
  测试的上游一律 mock（`httptest` / `testdata` fixture），由
  `scripts/check-test-upstream.sh` 断言「测试代码里没有真实上游域名」。
- 实现「多副本」剩下的只是无状态化（MKACG-619），数据库侧不再有单写者瓶颈。
