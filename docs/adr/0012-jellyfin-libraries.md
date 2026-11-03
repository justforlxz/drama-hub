# ADR-0012：Jellyfin 一分类一媒体库，由本服务自动创建与维护

状态：**已接受**（2026-11-02，MKACG-654）

## 背景

Jellyfin（本部署 12.1.0）只把**库根的直接子目录**认作剧集（Series）。现网把单个库的根
挂在 rclone 挂载根 `/data/video/dramahub` 上，而挂载根下面是分类目录、再下面是剧目录，
于是 `jellyfin.db` 里出现：

- 59 条 `TV.Series`（= 分类目录），
- 5095 条 `TV.Season`（= 分片目录 `00`–`7f`），
- 5329 个 `Folder`（= 真剧目录，全部没被识别成剧），
- 44196 条 `Episode`。

也就是说：**库根与剧目录之间多隔了一层，库就整体失效**。修法只有一个方向 —— 库根收窄到
`<挂载根>/<分类>`，一个分类一个库。分类有 58–65 个（随上游变），手点在运维上不现实。

## 决定

1. **一分类一库**：`name = <jellyfin_library_prefix><分类目录名>`，`collectionType =
   tvshows`，`paths = <jellyfin_library_root>/<分类目录名>`。
   - 分类目录名取**虚拟文件树根目录列出的名字**（已转义，含兜底名「其他」），不取
     `drama.genre` 原值：库路径必须与真实目录逐字相等，差一个转义字符库就是空的。
   - `jellyfin_library_root` 是 **Jellyfin 容器内**看到的挂载根（本部署
     `/data/video/dramahub`），不是宿主路径、也不是本服务的挂载点。
2. **自管集合 = 三条同时成立**：名字带 prefix、`Locations` 只有一个、那个位置正好是
   `root` 的直接子目录（`path.Clean` 比较）。任一条单独成立都可能是别人的库，一律不动。
3. **库集合跟随「启用的分类」，开关即删除开关**（2026-11-04 由 MKACG-668 修订，取代
   最初的「只增不删 + 显式 prune」）。页面给出一行一个分类的开关，默认全部开启；关掉的
   分类不建库，已有的自管库在**下一轮同步**里删掉（拨开关本身不发任何 Jellyfin 请求）：
   - 周期同步：轮末钩子读到开关关着，这一轮删；
   - 点「同步媒体库」：本来就是立刻跑一轮，所以当场删。
   删除范围仍严格限于自管库（决定 2 的三条），别人的库永不删。删除失败不静默：这一项进
   `delete_failed[]` 与 `last_error`，整轮继续，下一轮自动重试；目标库已经不在了算这一项
   完成（幂等）。
   分类从树里整个消失（没有开关行）仍然只报告「孤儿库」，不删 —— 删除只由显式关闭触发，
   一次读不到某个分类不该顺手删库。
4. **失败隔离**。Jellyfin 不可达 / 非 2xx / 超时：只写内存里的 `last_error`，不影响同步轮
   收尾（`source_state` 一个字都不改）、不影响 `/healthz`、WebDAV 与播放。
5. **凭据只写不读**。`jellyfin_api_key` 是 settings 里的密钥项：`PATCH /api/settings`
   回 `null`，`/api/status` 只给 `api_key_set`，错误文本只含方法、端点路径与状态码。
6. **删除开关落库，其余状态不落库**。分类开关（`jellyfin_category_switch` 表，行不存在 =
   启用）必须跨重启保持：关掉的库不能因为进程重启就被下一轮同步悄悄重建。reconcile 的
   结果本身仍然幂等，不需要接续；`created` / `last_*` / 孤儿库只存在进程内存里，重启后
   回到「未运行」。
7. **触发面两个，语义相同**：手动 `POST /api/jellyfin/libraries`（202 / 409 / 400）与同步
   轮末钩子（`scheduler.runRound` 的 `saveState` 之后，一轮一次，撞车静默跳过）。两者共用
   一把单飞锁，一轮最多跑一个 reconcile。
8. **扫描只在有变化时触发**：`created > 0` 或 `removed > 0` 或 `uncertain > 0` 才调一次
   `POST /Library/Refresh`。建库时带 `refreshLibrary=false` —— 否则 65 个库各触发一次全库
   扫描。0 变化时**一个写请求都不发**，这就是「再点一次 0 新增」的可验证点。
9. **整轮预算按请求数给**：`jellyfin_timeout_sec × (待发请求数 + 1)`。一轮串行发
   「列库 1 + 建库 N + 扫描 1」个请求，每个都有自己的单次超时；把整轮钉死成固定的
   `2 × timeout` 时，实测 58 个分类（单次建库 ~1.1s，Jellyfin 扫库期间单次 >10s）在默认
   10s 超时下点一次只建 15 个就 `context deadline exceeded`，要连点 3–4 次才收敛。
   这是默认配置下的必现故障，不是极端情况。
10. **单点失败不中止整轮**。两类失败只记账跳过，继续处理其余分类：
    - **请求超时**（`uncertain`）：Jellyfin 侧结果未知 —— 既不能算建成（没收到 204），
      也不能算没建成；下一轮列库幂等收口。
    - **4xx 被拒**（`failed`）：实测 12.1 库路径在容器内不存在时回 400
      （`ArgumentException: The specified path does not exist`）。分类目录由 vfs 现算，
      与 Jellyfin 容器里的实际挂载之间必然存在窗口（rclone 还没刷出目录、挂载漏分类、
      `library_root` 填错一层），所以这是常态而非异常。一个坏路径让其余几十个分类都建
      不出来，是比原症状更糟的故障。
    连不上 / 5xx / 认证失败才是系统性的，整轮中止留一条 `last_error`。

## API 事实（12.1 实测，不要按记忆写）

| 动作 | 端点 | 结论 |
|---|---|---|
| 列库 | `GET /Library/VirtualFolders` | 200，元素含 `Name` / `Locations[]` / `CollectionType` / `ItemId` |
| 建库 | `POST /Library/VirtualFolders?name=&collectionType=tvshows&paths=&refreshLibrary=false` | **204，无响应体，拿不到库 id**；`paths` 由 `CommaDelimitedCollectionModelBinder` 绑定（重复参数或逗号分隔都行） |
| 删库 | `DELETE /Library/VirtualFolders?name=&refreshLibrary=false` | 204；名字不存在时按异常处理（可能 500），不能当存在性探测用 |
| 触发扫描 | `POST /Library/Refresh` | 204，扫全部库 |
| 认证 | `Authorization: MediaBrowser Token="<key>"` | **12.1 唯一可用方式**：迁移 `20260531160000_DisableLegacyAuthorization` 把 `EnableLegacyAuthorization` 置 false，`X-Emby-Token` 不再生效；`?ApiKey=` 仍可用但会把 key 写进 Jellyfin 访问日志，不用 |

库 id 本任务不需要（删库按 name、扫描用全局端点），所以「建库拿不到 id」不构成障碍。

## 配置

五项全部走 settings（不新增 env，见 `docs/DEV-STANDARDS.md`「配置」表）：

| key | 默认 | 范围 | 说明 |
|---|---|---|---|
| `jellyfin_url` | `""` | 带 host 的绝对 URL 或空 | 空 = 关闭 |
| `jellyfin_api_key` | 无行 | 空或非空字符串（密钥） | 空 = 关闭；只写不读 |
| `jellyfin_library_root` | `""` | 容器内绝对路径（非 `/`） | 空 = 关闭 |
| `jellyfin_library_prefix` | `短剧 - ` | 非空字符串 | 库名前缀，也是归属判据的第一条 |
| `jellyfin_timeout_sec` | `10` | 1–60 | 单次出站超时 |

启用条件 = 前三项都非空。**不设 `jellyfin_enabled`**：多一个开关就多一种「显示已配置但
不跑」。前缀不接受空串：空前缀会让所有库都算自管，删库时可能删到别人的库。

分类开关不在 settings 里（它是按分类的多行状态，不是单值键）：`jellyfin_category_switch`
一张表，`category` 主键，行不存在即启用。页面 `GET/PATCH /api/jellyfin/categories`
读写它，Manager 每轮开始现读。

## 后果

- 65 个分类 → 65 个库，每个库根下直接是剧目录，Jellyfin 才能把剧识别成 Series。
- 部署多两步：给本服务一个能连到 Jellyfin 的地址、在 Jellyfin 里建一个 API key
  （`docs/DEPLOY.md`）。
- 库里出现同类需求（Emby）时复用差异算法，但**不做**在本服务里改 Jellyfin 的转码/元数据。

## 被否决的替代方案

- **一个库挂着整个挂载根**：就是现状的故障（59 个分类成了 59 部 Series）。
- **手工在 Jellyfin 里点 65 次**：分类随上游变，每次都要重做，且无法幂等。
- **本服务自己写 `jellyfin.db`**：跨版本 schema，且绕过 Jellyfin 的索引流程。
- **按剧建库**：一分类一库就能让剧成为库根的直接子目录；按剧建库是 1436 个库。
