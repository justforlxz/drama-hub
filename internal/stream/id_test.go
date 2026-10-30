package stream

import (
	"strings"
	"testing"
)

// goldenID 是按 docs/PLAYBACK-CONTRACT.md 的公式在测试侧独立算出的黄金值
// （不复用被测代码）：hex(HMAC-SHA256("s3cret", "e42"))[:12]。
const (
	testSecret = "unit-test-secret"
	goldenE42  = "e42-cf3df3bed6b6"
)

// TestID 钉住 id 的形态与黄金值：`uid + "-" + 12 位十六进制`，纯函数。
func TestID(t *testing.T) {
	if got := ID(testSecret, "e42"); got != goldenE42 {
		t.Fatalf("ID = %q, 期望黄金值 %q", got, goldenE42)
	}
	// 纯函数：重复调用逐字符相同（「不飘移」的第一条形式）。
	// 经变量转一手，避免静态检查把两边当成同一个表达式。
	first, second := ID(testSecret, "e42"), ID(testSecret, "e42")
	if first != second {
		t.Fatal("同一 uid 两次签出的 id 不同")
	}
	// uid 段原样保留、签名段恒 12 位。
	id := ID(testSecret, "m7")
	uid, sig, found := strings.Cut(id, "-")
	if !found || uid != "m7" || len(sig) != IDLen {
		t.Fatalf("id = %q，形态应为 <uid>-<12 位十六进制>", id)
	}
	// 换 secret 后签名段变（旧 URL 失效，ADR-0003 的既定后果）。
	if ID(testSecret+"x", "e42") == goldenE42 {
		t.Fatal("换 secret 后 id 没变")
	}
}

// TestParseID 覆盖契约状态码矩阵的第一行：合法 id 拆出 uid，其余一律 ok=false（401）。
func TestParseID(t *testing.T) {
	valid := ID(testSecret, "e42")
	cases := []struct {
		name   string
		secret string
		id     string
		wantOK bool
	}{
		{"合法", testSecret, valid, true},
		{"合并集合法", testSecret, ID(testSecret, "m3"), true},
		{"缺 -", testSecret, "e42", false},
		{"空 uid", testSecret, "-cf3df3bed6b6", false},
		{"空签名", testSecret, "e42-", false},
		{"签名改一位", testSecret, "e42-cf3df3bed6b7", false},
		{"签名截短", testSecret, "e42-cf3df3bed6b", false},
		{"换 secret", testSecret + "x", valid, false},
		{"签名段非十六进制", testSecret, "e42-zzzzzzzzzzzz", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uid, ok := ParseID(tc.secret, tc.id)
			if ok != tc.wantOK {
				t.Fatalf("ParseID(%q) ok = %v, 期望 %v", tc.id, ok, tc.wantOK)
			}
			if ok && uid == "" {
				t.Fatalf("ParseID(%q) 通过却拆出空 uid", tc.id)
			}
		})
	}
}

// TestParseIDReturnsUID 确认拆出的 uid 就是签名时用的那个（生产方与校验方同一算法）。
func TestParseIDReturnsUID(t *testing.T) {
	for _, uid := range []string{"e1", "e1234", "m3"} {
		got, ok := ParseID(testSecret, ID(testSecret, uid))
		if !ok || got != uid {
			t.Fatalf("ParseID(ID(%q)) = (%q, %v), 期望 (%q, true)", uid, got, ok, uid)
		}
	}
}
