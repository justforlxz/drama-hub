# hongguo/testdata

官网 SSR 页面 fixture。**手工裁剪后入库，不放整页 HTML**（整页 200+ KB，且含无关脚本）。

- `html/` — `internal/plugin/hongguo` 的解析 / 插件测试用固定片段，裁剪自 2026-10-29 抓取的真实页面
  （`internal/sync` 的入库一致性测试读同一份文件，保证「迁移前后逐字段一致」用的是同一页面）。
  每个文件头部注释写清：来源 URL、裁剪范围、改造点、覆盖的 AC 编号。

刷新方式（上游改版导致 CI 红时，人工一次性重抓）：

```
curl -sS 'https://hongguoduanju.com/player/{sid}' -o /tmp/page.html
curl -sS 'https://hongguoduanju.com/player/{sid}/{vid}' -o /tmp/ep.html
```

然后按各 fixture 头部注释的范围重新裁剪。**不要把真实可下载直链放进 fixture**：测试必须能在
`--network=none` 下通过，fixture 里出现真链会诱使后续测试去访问外网。
