package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// newTestDB 建一个临时文件 SQLite 并建表。用真驱动、真文件：
// settings 的读回/不落库断言必须看得到实际的库内容。
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开临时库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := EnsureSettings(context.Background(), db); err != nil {
		t.Fatalf("EnsureSettings: %v", err)
	}
	return db
}

// hasRow 报告 settings 表里是否存在 key 行。
func hasRow(t *testing.T, db *sql.DB, key SettingKey) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM settings WHERE key = ?`, string(key)).Scan(&n); err != nil {
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
		KeySegPrefetch:  2,
		KeyPinDays:      7,
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
	if err := db.QueryRow(`SELECT count(*) FROM settings WHERE key = ?`, string(KeySegTargetSec)).Scan(&rows); err != nil {
		t.Fatalf("统计行数: %v", err)
	}
	if rows != 1 {
		t.Fatalf("keys 行数 = %d, 期望 1（UPSERT 不新增行）", rows)
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

	const bogus SettingKey = "cache_max_mb"

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

	for _, raw := range []string{"/proxy", "proxy.local/stream", "not a url"} {
		if err := s.Set(ctx, KeyStrmPrefix, raw); !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("Set(strm_prefix, %q) 错误 = %v, 期望 ErrInvalidSetting", raw, err)
		}
	}

	const good = "http://10.0.0.2:3000/stream/"
	if err := s.Set(ctx, KeyStrmPrefix, good); err != nil {
		t.Fatalf("Set(strm_prefix, %q): %v", good, err)
	}
	got, err := s.String(ctx, KeyStrmPrefix)
	if err != nil {
		t.Fatalf("String: %v", err)
	}
	if got != good {
		t.Fatalf("读回 %q, 期望 %q", got, good)
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
		{string(KeySegPrefetch), 0, 10},
		{string(KeyPinDays), 0, 90},
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
