#!/bin/sh
# 门禁：测试代码不得访问真实上游站点。
#
# 测试只允许打到 127.0.0.1 / httptest.NewServer / testdata 里的 fixture：
#   * testdata/ 下的页面与 sitemap 是抓下来的 fixture（数据，不是请求目标），不扫；
#   * *_test.go 里出现真实上游站点域名即失败 —— 真实站点只能在插件生产代码里出现。
#
# 封面 CDN（byteimg）只出现在 fixture 的期望值里，不在扫描范围内：它不是上游站点，
# 测试拿到的是页面里解析出来的字符串，不会发起请求。
set -eu
cd "$(dirname "$0")/.."

if hits=$(grep -rn 'hongguoduanju\.com' --include='*_test.go' . 2>/dev/null); then
	printf '%s\n' "$hits" >&2
	echo "FAIL: 测试代码里出现真实上游域名（改用 127.0.0.1 / httptest / testdata fixture）" >&2
	exit 1
fi

echo "测试上游域名门禁: OK（测试代码不含真实上游域名）"
