package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/justforlxz/drama-hub/internal/pgtest"
)

// newTestDB 用独立 schema 的测试库并建 settings 表。用真驱动、真库：
// settings 的读回/不落库断言必须看得到实际的库内容。
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := pgtest.Open(t)
	if err := EnsureSettings(context.Background(), db); err != nil {
		t.Fatalf("EnsureSettings: %v", err)
	}
	return db
}

// hasRow 报告 settings 表里是否存在 key 行。
func hasRow(t *testing.T, db *sql.DB, key SettingKey) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM settings WHERE key = $1`, string(key)).Scan(&n); err != nil {
		t.Fatalf("统计 %s: %v", key, err)
	}
	return n > 0
}

func TestEnsureSettingsIdempotent(t *testing.T) {
	db := newTestDB(t)
	s := NewSettings(db)
	ctx := context.Background()

	if err := s.Set(ctx, KeySegTargetSec, "20"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// 重复建表不得丢数据。
	if err := EnsureSettings(ctx, db); err != nil {
		t.Fatalf("第二次 EnsureSettings: %v", err)
	}
	got, err := s.Int(ctx, KeySegTargetSec)
	if err != nil {
		t.Fatalf("Int: %v", err)
	}
	if got != 20 {
		t.Fatalf("重复建表后读到 %d, 期望 20", got)
	}
}

// AC1：空库读默认值。
func TestDefaults(t *testing.T) {
	s := NewSettings(newTestDB(t))
	ctx := context.Background()

	ints := map[SettingKey]int{
		KeySegTargetSec: 10,
	}
	for key, want := range ints {
		got, err := s.Int(ctx, key)
		if err != nil {
			t.Fatalf("Int(%s): %v", key, err)
		}
		if got != want {
			t.Errorf("Int(%s) = %d, 期望默认 %d", key, got, want)
		}
	}

	prefix, err := s.String(ctx, KeyStrmPrefix)
	if err != nil {
		t.Fatalf("String(strm_prefix): %v", err)
	}
	if prefix != "" {
		t.Errorf("String(strm_prefix) = %q, 期望空串", prefix)
	}
}

// AC2：越界不落库。
func TestOutOfRangeNotPersisted(t *testing.T) {
	db := newTestDB(t)
	s := NewSettings(db)
	ctx := context.Background()

	err := s.Set(ctx, KeySegTargetSec, "50")
	if !errors.Is(err, ErrInvalidSetting) {
		t.Fatalf("Set(seg_target_sec, 50) 错误 = %v, 期望 ErrInvalidSetting", err)
	}
	if hasRow(t, db, KeySegTargetSec) {
		t.Fatal("越界值被写入了库")
	}
	got, err := s.Int(ctx, KeySegTargetSec)
	if err != nil {
		t.Fatalf("Int: %v", err)
	}
	if got != 10 {
		t.Fatalf("越界后读到 %d, 期望仍是默认 10", got)
	}
}

// AC3：非数字不落库。
func TestNonNumericNotPersisted(t *testing.T) {
	db := newTestDB(t)
	s := NewSettings(db)
	ctx := context.Background()

	err := s.Set(ctx, KeySegTargetSec, "abc")
	if !errors.Is(err, ErrInvalidSetting) {
		t.Fatalf("Set(seg_target_sec, abc) 错误 = %v, 期望 ErrInvalidSetting", err)
	}
	if hasRow(t, db, KeySegTargetSec) {
		t.Fatal("非数字值被写入了库")
	}
}

// AC4：改值后立即生效（没有内存缓存）。
func TestSetTakesEffectImmediately(t *testing.T) {
	s := NewSettings(newTestDB(t))
	ctx := context.Background()

	if err := s.Set(ctx, KeySegTargetSec, "20"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Int(ctx, KeySegTargetSec)
	if err != nil {
		t.Fatalf("Int: %v", err)
	}
	if got != 20 {
		t.Fatalf("改值后读到 %d, 期望 20", got)
	}
}

// 同一个 key 重复 Set 走 UPSERT，后写的值覆盖先写的值。
func TestSetUpdatesExistingRow(t *testing.T) {
	db := newTestDB(t)
	s := NewSettings(db)
	ctx := context.Background()

	for _, raw := range []string{"4", "30"} {
		if err := s.Set(ctx, KeySegTargetSec, raw); err != nil {
			t.Fatalf("Set(%s): %v", raw, err)
		}
	}
	got, err := s.Int(ctx, KeySegTargetSec)
	if err != nil {
		t.Fatalf("Int: %v", err)
	}
	if got != 30 {
		t.Fatalf("读到 %d, 期望 30", got)
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM settings WHERE key = $1`, string(KeySegTargetSec)).Scan(&rows); err != nil {
		t.Fatalf("统计行数: %v", err)
	}
	if rows != 1 {
		t.Fatalf("keys 行数 = %d, 期望 1（UPSERT 不新增行）", rows)
	}
}

// SetIfAbsent 只在键缺失时写：首次写入返回 true，第二次不改动已有值（env 引导的幂等）。
func TestSetIfAbsentWritesOnlyOnce(t *testing.T) {
	db := newTestDB(t)
	s := NewSettings(db)
	ctx := context.Background()

	written, err := s.SetIfAbsent(ctx, KeyWebDAVUser, "alice")
	if err != nil {
		t.Fatalf("首次 SetIfAbsent: %v", err)
	}
	if !written {
		t.Fatal("首次 SetIfAbsent 未写入")
	}
	written, err = s.SetIfAbsent(ctx, KeyWebDAVUser, "bob")
	if err != nil {
		t.Fatalf("第二次 SetIfAbsent: %v", err)
	}
	if written {
		t.Error("第二次 SetIfAbsent 报写入，期望跳过")
	}
	got, err := s.String(ctx, KeyWebDAVUser)
	if err != nil {
		t.Fatalf("String: %v", err)
	}
	if got != "alice" {
		t.Fatalf("读到 %q, 期望已有的 alice（不被覆盖）", got)
	}
}

// SetIfAbsent 与 Set 同一条校验：非法值不落库、不覆盖已有值。
func TestSetIfAbsentValidates(t *testing.T) {
	db := newTestDB(t)
	s := NewSettings(db)
	ctx := context.Background()

	written, err := s.SetIfAbsent(ctx, KeyStreamSecret, "太短")
	if !errors.Is(err, ErrInvalidSetting) {
		t.Fatalf("短 secret 错误 = %v, 期望 ErrInvalidSetting", err)
	}
	if written {
		t.Error("非法值被写入了")
	}
	if hasRow(t, db, KeyStreamSecret) {
		t.Fatal("非法值在库里留了行")
	}

	if _, err := s.SetIfAbsent(ctx, KeySegTargetSec, "50"); !errors.Is(err, ErrInvalidSetting) {
		t.Fatalf("越界值错误 = %v, 期望 ErrInvalidSetting", err)
	}
	if hasRow(t, db, KeySegTargetSec) {
		t.Fatal("越界值在库里留了行")
	}
	if _, err := s.SetIfAbsent(ctx, "不存在的键", "x"); !errors.Is(err, ErrUnknownSetting) {
		t.Fatalf("未知键错误 = %v, 期望 ErrUnknownSetting", err)
	}
}

// 2.0 不做缓存预热（MKACG-603 范围表），1.0 预留的两个死键已删除：
// 它们不再是合法配置项，读与写都按 ErrUnknownSetting 处理。
func TestRemovedPrefetchKeys(t *testing.T) {
	s := NewSettings(newTestDB(t))
	ctx := context.Background()

	for _, key := range []SettingKey{"seg_prefetch", "pin_days"} {
		if _, err := s.Int(ctx, key); !errors.Is(err, ErrUnknownSetting) {
			t.Errorf("Int(%s) 错误 = %v, 期望 ErrUnknownSetting", key, err)
		}
		if _, err := s.SetIfAbsent(ctx, key, "1"); !errors.Is(err, ErrUnknownSetting) {
			t.Errorf("SetIfAbsent(%s) 错误 = %v, 期望 ErrUnknownSetting", key, err)
		}
	}
}

// AC5：必填项缺失报错，写入后读回一致。
func TestRequiredSettings(t *testing.T) {
	s := NewSettings(newTestDB(t))
	ctx := context.Background()

	if _, err := s.String(ctx, KeyStreamSecret); !errors.Is(err, ErrSettingMissing) {
		t.Fatalf("空库读 stream_secret 错误 = %v, 期望 ErrSettingMissing", err)
	}

	secret := strings.Repeat("k", 32)
	user := "alice"
	pass := "s3cret"

	for key, raw := range map[SettingKey]string{
		KeyStreamSecret: secret,
		KeyWebDAVUser:   user,
		KeyWebDAVPass:   pass,
	} {
		if err := s.Set(ctx, key, raw); err != nil {
			t.Fatalf("Set(%s): %v", key, err)
		}
		got, err := s.String(ctx, key)
		if err != nil {
			t.Fatalf("String(%s): %v", key, err)
		}
		if got != raw {
			t.Errorf("String(%s) = %q, 期望 %q", key, got, raw)
		}
	}
}

// 必填项不得接受空串。
func TestRequiredSettingsRejectEmpty(t *testing.T) {
	s := NewSettings(newTestDB(t))
	ctx := context.Background()

	for _, key := range []SettingKey{KeyWebDAVUser, KeyWebDAVPass, KeyStreamSecret} {
		if err := s.Set(ctx, key, ""); !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("Set(%s, 空串) 错误 = %v, 期望 ErrInvalidSetting", key, err)
		}
	}
}

// AC6：secret 长度边界 31 / 32 字节。
func TestStreamSecretLengthBoundary(t *testing.T) {
	db := newTestDB(t)
	s := NewSettings(db)
	ctx := context.Background()

	if err := s.Set(ctx, KeyStreamSecret, strings.Repeat("a", 31)); !errors.Is(err, ErrInvalidSetting) {
		t.Fatalf("31 字节错误 = %v, 期望 ErrInvalidSetting", err)
	}
	if hasRow(t, db, KeyStreamSecret) {
		t.Fatal("过短的 secret 被写入了库")
	}

	if err := s.Set(ctx, KeyStreamSecret, strings.Repeat("a", 32)); err != nil {
		t.Fatalf("32 字节 Set: %v", err)
	}
	got, err := s.String(ctx, KeyStreamSecret)
	if err != nil {
		t.Fatalf("String: %v", err)
	}
	if len(got) != 32 {
		t.Fatalf("读回长度 %d, 期望 32", len(got))
	}
}

// AC7：未知 key。
func TestUnknownKey(t *testing.T) {
	db := newTestDB(t)
	s := NewSettings(db)
	ctx := context.Background()

	const bogus SettingKey = "cache_max_gb"

	if err := s.Set(ctx, bogus, "1"); !errors.Is(err, ErrUnknownSetting) {
		t.Errorf("Set(未知 key) 错误 = %v, 期望 ErrUnknownSetting", err)
	}
	if _, err := s.String(ctx, bogus); !errors.Is(err, ErrUnknownSetting) {
		t.Errorf("String(未知 key) 错误 = %v, 期望 ErrUnknownSetting", err)
	}
	if _, err := s.Int(ctx, bogus); !errors.Is(err, ErrUnknownSetting) {
		t.Errorf("Int(未知 key) 错误 = %v, 期望 ErrUnknownSetting", err)
	}
	if hasRow(t, db, bogus) {
		t.Fatal("未知 key 被写入了库")
	}
}

// 用错类型读取要报 ErrInvalidSetting，而不是静默返回零值。
func TestReadWithWrongType(t *testing.T) {
	s := NewSettings(newTestDB(t))
	ctx := context.Background()

	if _, err := s.Int(ctx, KeyStrmPrefix); !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("Int(strm_prefix) 错误 = %v, 期望 ErrInvalidSetting", err)
	}
	if _, err := s.String(ctx, KeySegTargetSec); !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("String(seg_target_sec) 错误 = %v, 期望 ErrInvalidSetting", err)
	}
}

// strm_prefix 只接受空串或带 scheme 的绝对 URL。
func TestStrmPrefixValidation(t *testing.T) {
	s := NewSettings(newTestDB(t))
	ctx := context.Background()

	for _, raw := range []string{
		"/proxy", "proxy.local/stream", "not a url",
		// 归一化只去尾 `/`，去掉之后仍是「带 scheme 但没有 host」：前者剩 `http:`、
		// 后者 host 为空，拼出的 .strm 都是 `http:/video/…`，Emby 拉不到（MKACG-634）。
		"http://", "http:///path",
	} {
		if err := s.Set(ctx, KeyStrmPrefix, raw); !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("Set(strm_prefix, %q) 错误 = %v, 期望 ErrInvalidSetting", raw, err)
		}
	}

	// B2：尾 `/` 写入时归一化（`{strm_prefix}/video/…` 是字符串拼接，留着会拼出
	// `//video/`，MKACG-617 实测）。归一不改语义，非法值照旧报错。
	const given = "http://10.0.0.2:3000/stream/"
	const want = "http://10.0.0.2:3000/stream"
	if err := s.Set(ctx, KeyStrmPrefix, given); err != nil {
		t.Fatalf("Set(strm_prefix, %q): %v", given, err)
	}
	got, err := s.String(ctx, KeyStrmPrefix)
	if err != nil {
		t.Fatalf("String: %v", err)
	}
	if got != want {
		t.Fatalf("读回 %q, 期望 %q（尾斜杠已归一）", got, want)
	}
	// 只有 `/` 的值归一成空串 = 按请求 Host 推导（不是非法值）。
	if err := s.Set(ctx, KeyStrmPrefix, "http://h:1///"); err != nil {
		t.Fatalf("Set(strm_prefix, 多尾斜杠): %v", err)
	}
	if got, err := s.String(ctx, KeyStrmPrefix); err != nil || got != "http://h:1" {
		t.Fatalf("多尾斜杠读回 %q, %v，期望 http://h:1", got, err)
	}
}

// AC8：纯校验，无 I/O。四个整型键的 min-1 / min / max / max+1 边界。
func TestParseSettingBoundaries(t *testing.T) {
	tests := []struct {
		key string
		min int
		max int
	}{
		{string(KeySegTargetSec), 4, 30},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			key := SettingKey(tt.key)
			inRange := []int{tt.min, tt.max}
			if tt.min > 0 {
				inRange = append(inRange, tt.min-1, tt.max+1)
			} else {
				inRange = append(inRange, tt.max+1, -1)
			}

			for _, n := range inRange {
				raw := strconv.Itoa(n)
				got, err := parseSetting(key, raw)
				ok := n >= tt.min && n <= tt.max
				switch {
				case ok && err != nil:
					t.Errorf("%s=%d 应通过, 实际报错 %v", tt.key, n, err)
				case ok && got != n:
					t.Errorf("%s=%d 解析结果 = %v", tt.key, n, got)
				case !ok && !errors.Is(err, ErrInvalidSetting):
					t.Errorf("%s=%d 应报 ErrInvalidSetting, 实际 %v", tt.key, n, err)
				}
			}
		})
	}
}

// 字符串类 key 的纯校验边界：secret 31/32 字节、URL 空/绝对/相对、必填项空串。
func TestParseSettingStringKinds(t *testing.T) {
	cases := []struct {
		name    string
		key     SettingKey
		raw     string
		wantErr bool
	}{
		{"secret-31", KeyStreamSecret, strings.Repeat("x", 31), true},
		{"secret-32", KeyStreamSecret, strings.Repeat("x", 32), false},
		{"url-empty", KeyStrmPrefix, "", false},
		{"url-absolute", KeyStrmPrefix, "https://example.com/stream", false},
		{"url-relative", KeyStrmPrefix, "/stream", true},
		{"user-empty", KeyWebDAVUser, "", true},
		{"user-set", KeyWebDAVUser, "alice", false},
		{"pass-empty", KeyWebDAVPass, "", true},
		{"pass-set", KeyWebDAVPass, "s3cret", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseSetting(c.key, c.raw)
			switch {
			case c.wantErr && !errors.Is(err, ErrInvalidSetting):
				t.Errorf("期望 ErrInvalidSetting, 实际 %v", err)
			case !c.wantErr && err != nil:
				t.Errorf("期望通过, 实际 %v", err)
			}
		})
	}
}

// 插件配置键 plugin.<源>.<项>：默认值来自 docs/PLUGIN-CONTRACT.md「配置」。
//
// 内置源默认启用（升级到 2.0 后红果不需要先手工开一下）；base_url 空 = 用插件
// 内置地址；请求超时默认 30 秒。
func TestPluginSettingDefaults(t *testing.T) {
	s := NewSettings(newTestDB(t))
	ctx := context.Background()

	enabled, err := s.Bool(ctx, PluginSettingKey("web", PluginItemEnabled))
	if err != nil {
		t.Fatalf("Bool(plugin.web.enabled): %v", err)
	}
	if !enabled {
		t.Error("plugin.web.enabled 默认应为 true")
	}
	baseURL, err := s.String(ctx, PluginSettingKey("web", PluginItemBaseURL))
	if err != nil {
		t.Fatalf("String(plugin.web.base_url): %v", err)
	}
	if baseURL != "" {
		t.Errorf("plugin.web.base_url 默认应为空（用插件内置地址），实际 %q", baseURL)
	}
	timeout, err := s.Int(ctx, PluginSettingKey("web", PluginItemRequestTimeout))
	if err != nil {
		t.Fatalf("Int(plugin.web.request_timeout_sec): %v", err)
	}
	if timeout != 30 {
		t.Errorf("plugin.web.request_timeout_sec 默认应为 30，实际 %d", timeout)
	}
}

// 插件键的合法值与非法值：未知项、非法源名、越界、类型不符都拒绝且不落库。
func TestPluginSettingValidation(t *testing.T) {
	db := newTestDB(t)
	s := NewSettings(db)
	ctx := context.Background()

	valid := []struct {
		key SettingKey
		raw string
	}{
		{PluginSettingKey("web", PluginItemEnabled), "false"},
		{PluginSettingKey("web", PluginItemEnabled), "true"},
		{PluginSettingKey("web", PluginItemBaseURL), ""},
		{PluginSettingKey("web", PluginItemBaseURL), "http://127.0.0.1:18080"},
		{PluginSettingKey("web", PluginItemRequestTimeout), "5"},
		{PluginSettingKey("web", PluginItemRequestTimeout), "300"},
		{PluginSettingKey("src_2", PluginItemEnabled), "false"},
	}
	for _, c := range valid {
		if err := s.Set(ctx, c.key, c.raw); err != nil {
			t.Errorf("Set(%s, %q) = %v, 期望通过", c.key, c.raw, err)
		}
	}

	invalid := []struct {
		key SettingKey
		raw string
	}{
		// 用另一个源名：同一 key 在上面已经写过合法值，hasRow 就没法证明「没落库」。
		{PluginSettingKey("bad", PluginItemRequestTimeout), "4"},   // 越界
		{PluginSettingKey("bad", PluginItemRequestTimeout), "301"}, // 越界
		{PluginSettingKey("bad", PluginItemRequestTimeout), "30s"},
		{PluginSettingKey("bad", PluginItemBaseURL), "/player"}, // 相对 URL
		{PluginSettingKey("bad", PluginItemEnabled), "yes"},
		{PluginSettingKey("web", "refresh_batch"), "10"},      // 未登记的项
		{PluginSettingKey("", PluginItemEnabled), "true"},     // 空源名
		{PluginSettingKey("-web", PluginItemEnabled), "true"}, // 首字符是分隔符
		{PluginSettingKey("WEB", PluginItemEnabled), "true"},  // 大写不在允许字符集
		{PluginSettingKey("web.x", PluginItemEnabled), "true"},
	}
	for _, c := range invalid {
		if err := s.Set(ctx, c.key, c.raw); !errors.Is(err, ErrInvalidSetting) && !errors.Is(err, ErrUnknownSetting) {
			t.Errorf("Set(%s, %q) = %v, 期望被拒", c.key, c.raw, err)
		}
		if hasRow(t, db, c.key) {
			t.Errorf("非法插件配置 %s 被写入了库", c.key)
		}
	}

	// 用错类型读取同样报 ErrInvalidSetting。
	if _, err := s.String(ctx, PluginSettingKey("web", PluginItemEnabled)); !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("String(plugin.web.enabled) 错误 = %v, 期望 ErrInvalidSetting", err)
	}
	if _, err := s.Bool(ctx, PluginSettingKey("web", PluginItemBaseURL)); !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("Bool(plugin.web.base_url) 错误 = %v, 期望 ErrInvalidSetting", err)
	}
}

// 刷新调度的三项有默认值；源级间隔覆盖没有默认值（空 = 继承全局），必须用
// OptionalInt 读——用 Int 读「未设置」要报 ErrSettingMissing，而不是编一个数字。
func TestRefreshSettingDefaults(t *testing.T) {
	s := NewSettings(newTestDB(t))
	ctx := context.Background()

	for key, want := range map[SettingKey]int{
		KeyRefreshIntervalSec: 3600,
		KeyRefreshBatch:       1000,
		KeyRefreshTimeoutSec:  900,
	} {
		got, err := s.Int(ctx, key)
		if err != nil {
			t.Fatalf("Int(%s): %v", key, err)
		}
		if got != want {
			t.Errorf("Int(%s) = %d, 期望默认 %d", key, got, want)
		}
	}

	override := PluginSettingKey("web", PluginItemRefreshInterval)
	value, found, err := s.OptionalInt(ctx, override)
	if err != nil {
		t.Fatalf("OptionalInt(%s): %v", override, err)
	}
	if found || value != 0 {
		t.Errorf("未设置时 OptionalInt(%s) = (%d, %v), 期望 (0, false)（继承全局）", override, value, found)
	}
	if _, err := s.Int(ctx, override); !errors.Is(err, ErrSettingMissing) {
		t.Errorf("Int(%s) 错误 = %v, 期望 ErrSettingMissing", override, err)
	}

	// 写下去之后 OptionalInt 能读到值。
	if err := s.Set(ctx, override, "900"); err != nil {
		t.Fatalf("Set(%s): %v", override, err)
	}
	if value, found, err := s.OptionalInt(ctx, override); err != nil || !found || value != 900 {
		t.Errorf("OptionalInt(%s) = (%d, %v, %v), 期望 (900, true, nil)", override, value, found, err)
	}
}

// 刷新间隔的边界：300 与 86400 是闭区间端点，越界一律拒绝且不落库
// （docs/SCHEDULER-MODEL.md「间隔范围为什么是 300–86400」）。
func TestRefreshIntervalBounds(t *testing.T) {
	db := newTestDB(t)
	s := NewSettings(db)
	ctx := context.Background()

	keys := []SettingKey{KeyRefreshIntervalSec, PluginSettingKey("web", PluginItemRefreshInterval)}
	for _, key := range keys {
		for _, raw := range []string{"300", "86400"} {
			if err := s.Set(ctx, key, raw); err != nil {
				t.Errorf("Set(%s, %s) = %v, 期望通过（闭区间端点）", key, raw, err)
			}
		}
		value, found, err := s.OptionalInt(ctx, key)
		if err != nil {
			t.Fatalf("OptionalInt(%s): %v", key, err)
		}
		if key == KeyRefreshIntervalSec && (!found || value != 86400) {
			t.Errorf("OptionalInt(%s) = (%d, %v), 期望 (86400, true)", key, value, found)
		}
	}
	// hasRow 断言用另一个源名，避免上面写过的合法值干扰。
	for _, raw := range []string{"299", "86401", "0", "-1", "1h"} {
		if err := s.Set(ctx, PluginSettingKey("bad", PluginItemRefreshInterval), raw); !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("Set(plugin.bad.refresh_interval_sec, %q) = %v, 期望 ErrInvalidSetting", raw, err)
		}
		if hasRow(t, db, PluginSettingKey("bad", PluginItemRefreshInterval)) {
			t.Errorf("越界的间隔 %q 被写入了库", raw)
		}
	}
}

// 存量批量 refresh_batch 的边界：0（关闭轮转）到 10000。
func TestRefreshBatchBounds(t *testing.T) {
	db := newTestDB(t)
	s := NewSettings(db)
	ctx := context.Background()

	for _, raw := range []string{"0", "10000"} {
		if err := s.Set(ctx, KeyRefreshBatch, raw); err != nil {
			t.Errorf("Set(refresh_batch, %s) = %v, 期望通过", raw, err)
		}
	}
	for _, raw := range []string{"-1", "10001"} {
		if err := s.Set(ctx, KeyRefreshBatch, raw); !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("Set(refresh_batch, %s) = %v, 期望 ErrInvalidSetting", raw, err)
		}
	}
}

// 缓存淘汰两项的默认值与范围（docs/CACHE-MODEL.md「淘汰」）：
// cache_max_mb 0–1048576（0 = 不按大小淘汰），cache_lru_days 1–365。
func TestCacheSettingDefaultsAndBounds(t *testing.T) {
	db := newTestDB(t)
	s := NewSettings(db)
	ctx := context.Background()

	for key, want := range map[SettingKey]int{
		KeyCacheMaxMB:   8192,
		KeyCacheLRUDays: 30,
	} {
		got, err := s.Int(ctx, key)
		if err != nil {
			t.Fatalf("Int(%s): %v", key, err)
		}
		if got != want {
			t.Errorf("Int(%s) = %d, 期望默认 %d", key, got, want)
		}
	}

	for _, raw := range []string{"0", "1048576"} {
		if err := s.Set(ctx, KeyCacheMaxMB, raw); err != nil {
			t.Errorf("Set(cache_max_mb, %s) = %v, 期望通过（闭区间端点）", raw, err)
		}
	}
	for _, raw := range []string{"1", "365"} {
		if err := s.Set(ctx, KeyCacheLRUDays, raw); err != nil {
			t.Errorf("Set(cache_lru_days, %s) = %v, 期望通过（闭区间端点）", raw, err)
		}
	}

	for _, tc := range []struct {
		key SettingKey
		raw string
	}{
		{KeyCacheMaxMB, "-1"}, {KeyCacheMaxMB, "1048577"},
		{KeyCacheLRUDays, "0"}, {KeyCacheLRUDays, "366"},
	} {
		if err := s.Set(ctx, tc.key, tc.raw); !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("Set(%s, %s) = %v, 期望 ErrInvalidSetting", tc.key, tc.raw, err)
		}
	}
}

// SetMany 是管理面「一次改多项」的落点：整批校验通过才写、返回归一化后的类型化值。
func TestSetManyWritesAllAndNormalizes(t *testing.T) {
	db := newTestDB(t)
	s := NewSettings(db)
	ctx := context.Background()

	got, err := s.SetMany(ctx, map[SettingKey]string{
		KeyStrmPrefix:   "http://media.example:9000/", // 尾 `/` 写入时归一
		KeyWebDAVUser:   "alice",
		KeyCacheMaxMB:   "4096",
		KeyRefreshBatch: "0",
	})
	if err != nil {
		t.Fatalf("SetMany: %v", err)
	}
	if got[KeyStrmPrefix] != "http://media.example:9000" {
		t.Errorf("归一化后的 strm_prefix = %v, 期望去尾 /", got[KeyStrmPrefix])
	}
	if got[KeyCacheMaxMB] != 4096 || got[KeyRefreshBatch] != 0 {
		t.Errorf("整型返回值 = %v / %v, 期望 4096 / 0", got[KeyCacheMaxMB], got[KeyRefreshBatch])
	}

	// 落库的就是归一化后的值：下一次读直接可用。
	if prefix, err := s.String(ctx, KeyStrmPrefix); err != nil || prefix != "http://media.example:9000" {
		t.Errorf("读回 strm_prefix = %q err=%v", prefix, err)
	}
	if user, err := s.String(ctx, KeyWebDAVUser); err != nil || user != "alice" {
		t.Errorf("读回 webdav_user = %q err=%v", user, err)
	}
	if n, err := s.Int(ctx, KeyCacheMaxMB); err != nil || n != 4096 {
		t.Errorf("读回 cache_max_mb = %d err=%v", n, err)
	}
}

// 一批里只要有一项未知或非法，整批一行都不写 —— 半批生效比整批失败更难排查。
func TestSetManyRejectsWholeBatch(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		values map[SettingKey]string
		want   error
	}{
		{
			name:   "未知键",
			values: map[SettingKey]string{KeyWebDAVUser: "alice", SettingKey("nope"): "1"},
			want:   ErrUnknownSetting,
		},
		{
			name:   "越界值",
			values: map[SettingKey]string{KeyWebDAVUser: "alice", KeyCacheMaxMB: "1048577"},
			want:   ErrInvalidSetting,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			s := NewSettings(db)
			if _, err := s.SetMany(ctx, tc.values); !errors.Is(err, tc.want) {
				t.Fatalf("SetMany 错误 = %v, 期望 %v", err, tc.want)
			}
			if got, err := s.String(ctx, KeyWebDAVUser); !errors.Is(err, ErrSettingMissing) {
				t.Fatalf("批次被拒后 webdav_user = %q err=%v, 期望仍是「未设置」", got, err)
			}
			if hasRow(t, db, KeyCacheMaxMB) {
				t.Error("批次被拒后 cache_max_mb 仍被写入")
			}
		})
	}
}

// 海报下载超时（MKACG-620）：默认 5 秒、范围 1–60，越界不落库。
// 默认值与范围的真值在 docs/DEV-STANDARDS.md「配置」表。
func TestCoverTimeoutDefaultAndBounds(t *testing.T) {
	db := newTestDB(t)
	s := NewSettings(db)
	ctx := context.Background()

	got, err := s.Int(ctx, KeyCoverTimeoutSec)
	if err != nil {
		t.Fatalf("Int(cover_timeout_sec): %v", err)
	}
	if got != 5 {
		t.Fatalf("cover_timeout_sec 默认 = %d, 期望 5", got)
	}

	for _, raw := range []string{"1", "60"} {
		if err := s.Set(ctx, KeyCoverTimeoutSec, raw); err != nil {
			t.Errorf("Set(cover_timeout_sec, %s) = %v, 期望通过（闭区间端点）", raw, err)
		}
	}
	for _, raw := range []string{"0", "61", "-1", "5s"} {
		if err := s.Set(ctx, KeyCoverTimeoutSec, raw); !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("Set(cover_timeout_sec, %q) = %v, 期望 ErrInvalidSetting", raw, err)
		}
	}
	// 越界值一个都没落库：库里留着的还是最后一次合法写入的 60。
	kept, err := s.Int(ctx, KeyCoverTimeoutSec)
	if err != nil {
		t.Fatalf("Int(cover_timeout_sec)（越界写入后）: %v", err)
	}
	if kept != 60 {
		t.Fatalf("越界写入后 cover_timeout_sec = %d, 期望仍是 60", kept)
	}
}
