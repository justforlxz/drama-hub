# testdata

测试夹具目录。生成物不进版本库，生成脚本进版本库。

- `media/` — 由 `scripts/make-fixtures.sh` 生成的小视频 fixture（约 5 秒、几百 KB），供 `internal/store` 与 `internal/segment` 的集成测试使用。
- SSR HTML 片段不放这里：官网页面 fixture 归 `internal/plugin/hongguo/testdata/html/`（解析逻辑在插件，fixture 跟着解析走）。

新增夹具时在对应测试文件里写一行说明它覆盖的场景，例如「两集 codec 签名不一致」。
