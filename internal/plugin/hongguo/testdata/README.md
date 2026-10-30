# hongguo/testdata

官网 SSR 页面 fixture。**手工裁剪后入库，不放整页 HTML**（整页 200+ KB，且含无关脚本）。

- `html/` — `internal/plugin/hongguo` 的解析 / 插件测试用固定片段，裁剪自 2026-10-29/30 抓取的真实页面
  （`internal/sync` 的入库一致性测试读同一份文件，保证「迁移前后逐字段一致」用的是同一页面）。
  每个文件头部注释写清：来源 URL、裁剪范围、改造点、覆盖的 AC 编号。
  - `page-*.html`：列表页（`/player/{id}`）与单集页，覆盖解析与哨兵映射。
  - `sitemap-index.xml` / `sitemap-1.xml` / `sitemap-2.xml` / `sitemap-seo.xml`：目录发现
    （`Enumerate` 的 sitemap 三段），只留少数 loc，**loc 保留真实域名**——测试里把
    `DefaultBaseURL` 换成假站点地址，这样「索引给绝对 URL、插件照 loc 取文件」这条真实
    行为在 `--network=none` 下也被测到。
  - `category-page1.html` / `category-page2.html`：`/category` 的两页（`totalPages` 从 34
    改成 2），真实条目、真实字段，覆盖 `series_id` 提取与末页判定。

刷新方式（上游改版导致 CI 红时，人工一次性重抓）：

```
curl -sS 'https://hongguoduanju.com/player/{sid}' -o /tmp/page.html
curl -sS 'https://hongguoduanju.com/player/{sid}/{vid}' -o /tmp/ep.html
curl -sS 'https://hongguoduanju.com/sitemap/hongguoduanju/index.xml' -o /tmp/index.xml
curl -sS 'https://hongguoduanju.com/sitemap/hongguoduanju/index1.xml' -o /tmp/index1.xml
curl -sS 'https://hongguoduanju.com/seo-important.xml' -o /tmp/seo.xml
curl -sS 'https://hongguoduanju.com/category' -o /tmp/cat1.html
curl -sS 'https://hongguoduanju.com/category?page=2' -o /tmp/cat2.html
```

然后按各 fixture 头部注释的范围重新裁剪。**不要把真实可下载直链放进 fixture**：测试必须能在
`--network=none` 下通过，fixture 里出现真链会诱使后续测试去访问外网。
