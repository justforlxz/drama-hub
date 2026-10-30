# L1 口径：「获取全部的剧集」的可验证定义

状态：定稿（2026-10-30，MKACG-604 / stage 1）
取证时间：2026-10-30 09:08–09:2x UTC，来源 `https://hongguoduanju.com`
取证方式：下文每条数字都附「怎么取到的」，可直接复跑。

## 一句话口径（L1）

> **「获取全部的剧集」= 对源可枚举出的每一部剧目，入库其声明的全部集标识
> （`ep_no` = 上游集序下标 + 1，集数 = 上游声明集数），不截断、不抽样；
> 「全部」的边界由「源可枚举出的剧目集合」定义，实测该集合为 608,372 部。**

拆成三条可独立验证的子句：

| # | 子句 | 验证方式 |
|---|---|---|
| L1-a | 源可枚举全部剧目：`sitemap 并集 ∪ /category 全部页` | 跑下面「取证 A/B」，得到 608,319 ∪ 800 = **608,372** |
| L1-b | 每部剧的集标识一次性拿全：`len(vid_list) == episode_cnt` | 948 部样本 **100%** 相等（见取证 C） |
| L1-c | 入库不截断：`COUNT(episode)` 逐剧等于 `episode_cnt` | 空库跑一次后 SQL 对账 |

`0` 集或空 `vid_list` 的剧目不在本口径的「全部」里：实测 948 部样本最小 3 集。

## 取证 A：`/category` 是**封顶 800** 的视图，不是全库

```
curl -sS 'https://hongguoduanju.com/category' | (解 _ROUTER_DATA 的 category_page.pagination)
```
返回 `{"total":800,"pageNum":1,"pageSize":24,"totalPages":34}`。

- 第 34 页只返回 8 条（33×24+8 = 800），`?page=35` 与 `?page=100` 均 **HTTP 404**。
- 传 `?category_json_ids=5000`（= 爱情）时 `pagination` **仍是 800 / 34 页**，且第 1 页
  24 个 `series_id` 与不带筛选时**逐个相同**（实测 `identical: True`）。
- 传 `?gender=` / `?sort_type=` / `?time=` / `?tab=` 时页面不返回 `category_page` 载荷
  （SSR 只出 `category_layout` + 空壳）。

结论：800 是这个 SSR 视图的**硬上限**，不是源侧剧目总数；分类筛选在这个页面上不生效。
robots.txt 也把 `/category?*<参数>` 逐个 Disallow，只有裸 `/category` 与 `?page=` 允许。

## 取证 B：sitemap 给出 608,319 个剧目标识

`robots.txt` 声明三处：`seo-important.xml`、`seo-static.xml`、`sitemap/hongguoduanju/index.xml`。

- `sitemap/hongguoduanju/index.xml` 是索引，列 **29** 个子 sitemap（`index1..index29.xml`）。
- 子 sitemap 每份 22,915–43,556 个 `<loc>`；URL 形状只有两种：`/player/{id}`、`/detail?series_id={id}`。
  两者都是**剧目**页（不是集页），因此 `id` 就是剧目标识。
- 29 份去重并集 = **608,273**；`seo-important.xml` 4,000 个 `<loc>`（2,000 `/detail` + 2,000 `/player`）
  去重并集 = 2,000，其中 1,954 已含在前者内。总并集 = **608,319**。

```
# 取证命令（约 30 个请求，~30s）
for i in $(seq 1 29); do curl -sS https://hongguoduanju.com/sitemap/hongguoduanju/index$i.xml; done \
  | grep -o '/player/[0-9]\+' | cut -d/ -f3 | sort -u | wc -l
```

## 取证 B2：sitemap 与 `/category` 互不包含，并集才是全量

`/category` 的 800 部里，**747 部**在 sitemap 中，**53 部不在**（sitemap 生成滞后于最新上架）。

因此「全部」= 两个集合的并集 = 608,319 + 53 = **608,372**。
任何单一入口都不完备：只用 sitemap 会漏掉最新 53 部，只用 `/category` 只剩 800 部。

## 取证 C：单剧集数分布（948 部样本）

抽样口径：从 29 份 sitemap 均匀抽取（`seed` 固定，可复跑），逐部 `GET /player/{id}`
解析 `_ROUTER_DATA → player_(series_id)/page → seriesDetail`。

| 样本 | 样本数 | `isSuccess=true` | `len(vid_list)==episode_cnt` | 有 `tags` |
|---|---|---|---|---|
| sitemap 均匀抽样 | 120 | 120 (100%) | 120 (100%) | 120 (100%) |
| sitemap 均匀抽样 | 28 | 28 (100%) | 28 (100%) | 28 (100%) |
| `/category` 全 34 页 | 800 | 800 (100%) | 800 (100%) | 800 (100%) |

120 部样本的 `episode_cnt`：**min 3 / 中位 40 / 均值 46.8 / max 184**；
上述 948 部里出现的最大值是 **184**；另一次定向抽查里见到 **252** 集（`镇国驸马爷3D版第五季`），
说明集数上限不是 184，采样均值比真实均值略低。
`/category` 800 部样本的 `episode_cnt` 合计 59,959（均值 74.9，那是「最新上架」的偏置，
不代表全库）。

**全库集数估算**：608,372 × 46.8 ≈ **2,847 万集**（用 sitemap 均匀抽样的均值，不用
`/category` 偏置样本）。

## 取证 D：直链只有第 1 集，且是 10 秒预览

逐集抓 `GET /player/{id}/{ep_vid}`，取 JSON-LD `VideoObject.contentUrl`：

| 集 | 结果 |
|---|---|
| 第 1 集 | `contentUrl = https://hongguoduanju.com/video/public/{ep_vid}/360p.mp4` |
| 第 2、3 集 | `VideoObject` 在，`contentUrl` 缺失，`publicVideoSeo.available=false` |
| 第 4 集起 | **HTTP 404** |

抽 4 部剧逐一验证，形态完全一致。第 1 集那条链实测：

```
curl -sS -r 0-200000 '<contentUrl>'   # content-range: bytes 0-200000/651433
ffprobe -v error -show_entries format=duration  # duration=10.066667
```

即：**公开直链是 10.07 秒 / 651 KB 的 360p 预览片**，而 JSON-LD 自己声明 `duration: PT3M`。
另外抽 88 部剧的 `seriesDetail`：`protectedPlayback=true`、`pay_type=0`、
`accessible_episode_cnt=3` 三项 **88/88 全部一致**，没有任何一部是整集可播的。

这与 `docs/DEV-STANDARDS.md`「`play_url` 留空的契约」的 2026-10-29 结论一致，
并补上了两点原文档没写的：**第 2..N 集不是「暂缺」而是结构性不公开**；
**第 4 集起连页面本身都 404**。

→ 因此 L1 的「全部集」只能是**元数据全集**，不是「全部可播」。
`docs/DATA-MODEL.md`「剧集信息」据此把 `episode.title` / `duration_sec` 钉成恒定空值。

## 取证 E：全量抓取的代价（实测吞吐）

串行 15 次 `GET /player/{id}`：均值 **0.98 s**、中位 0.86 s、平均响应体 **213 KB**。
4 并发跑 120 次：**27.1 s**，约 **4.4 req/s**。

| 场景 | 请求数 | 串行 | 4 并发（实测吞吐） |
|---|---|---|---|
| 全量首次灌库（608,372 部 × 1 次 `Work`） | 608,372 | ≈ 166 h | ≈ **38 h** |
| sitemap 增量枚举（29 份 + 1 份） | 30 | ≈ 30 s | ≈ 30 s |
| `/category` 全部页（只覆盖最新 800 部） | 34 | ≈ 30 s | ≈ 30 s |

→ 「首次全量 38 小时、之后增量 30 秒」是设计基线，`docs/SCHEDULER-MODEL.md` 据此
把「目录枚举」与「存量轮转」拆成两件事，不放进同一个周期里。

## 由此确定的、必须写进契约的三件事

1. **枚举能力必须能返回「只有标识」的作品。** sitemap 只给 id，给的还是 60 万个；
   如果接口要求枚举时返回完整元数据，608,372 部就必须一次性全抓，38 小时的活会被
   塞进一个不可能完成的调用里。→ `docs/PLUGIN-CONTRACT.md` 的 `Enumerate` 返回
   `WorkRef`（只有标识），元数据由 `Work` 一部一部取。
2. **枚举必须可分页续跑。** 38 小时跨多次重启、跨多次配置变更，断点必须落库。
   → `source_state.cursor`（`docs/DATA-MODEL.md` 0002）。
3. **「全部可播」不作为 2.0 的验收项。** 源侧就只公开第 1 集的 10 秒。
   播放侧的可验证预期只能是「解析不到就 502、不写库」，不能是「每集都能播」。

## 规模后果（不改口径，但必须在 stage 4 之前给结论）

按 L1 字面执行，库与对外的量级是：

| 量 | 估算 | 依据 |
|---|---|---|
| `drama` 行 | 608,372 | 取证 A+B |
| `episode` 行 | ≈ 2,847 万 | 608,372 × 46.8 |
| `emby_item` 行（派生） | ≈ 2,847 万 | 每集一条对外剧集 |
| SQLite 文件 | ≈ 6–9 GB | 每行含索引约 155–190 B |
| WebDAV 每类目录子项 | 最大类 ≈ 21.6 万 | 800 样本里「爱情」占 35.5% |

`/全部/` 一级目录已按 lxz 2026-10-30 口径删除（`docs/DIRECTORY-CONTRACT.md` v2），
但单个分类目录仍有约 21.6 万个子目录。`x/net/webdav` 的 `walkFS` 对一次 `PROPFIND`
调用 `Readdir(0)` 把**全部**子项读进内存再逐个 `Stat`
（`x/net/webdav@v0.62.0/webdav/file.go:787`），所以 20 万级子项的目录不是「慢」而是
「不可列举」。对策已定（`docs/DIRECTORY-CONTRACT.md`「D2」）：分类目录下加一层纯函数分片
（`shard = fnv32a(provider_id) % N`，默认 `N=128`），把最大分类从 21.6 万摊到 ~1.7k。
具体 `N` 由 S4 全量跑完后的 `SELECT genre, COUNT(*) FROM drama GROUP BY genre` 校准，
不改 schema、不需要再决策。
