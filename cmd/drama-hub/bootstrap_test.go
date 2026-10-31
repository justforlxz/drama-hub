package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justforlxz/drama-hub/internal/store"
)

// testStreamSecret 是 32 字节整的合法 secret（minSecretBytes 的下界）。
const testStreamSecret = "0123456789abcdef0123456789abcdef"

// newBootstrapEnv 开一个临时库并给出 Settings 与原始连接（断言「有没有写行」）。
func newBootstrapEnv(t *testing.T) (*store.Settings, *sql.DB) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "bootstrap.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return store.NewSettings(db.DB), db.DB
}

// hasSettingRow 报告 settings 表里是否存在 key 行。
func hasSettingRow(t *testing.T, db *sql.DB, key store.SettingKey) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM settings WHERE key = ?`, string(key)).Scan(&n); err != nil {
		t.Fatalf("统计 %s: %v", key, err)
	}
	return n > 0
}

// 两个 env 的引导：空库写入、读回一致。
func TestBootstrapSettingsWritesMissingKeys(t *testing.T) {
	t.Setenv("DRAMA_HUB_STREAM_SECRET", testStreamSecret)
	t.Setenv("DRAMA_HUB_CACHE_DIR", "/data/cache")
	t.Setenv("DRAMA_HUB_WEBDAV_USER", "alice")
	t.Setenv("DRAMA_HUB_WEBDAV_PASS", "s3cret")

	settings, db := newBootstrapEnv(t)
	ctx := context.Background()
	bootstrapSettings(ctx, settings, slog.New(slog.DiscardHandler))

	secret, err := settings.String(ctx, store.KeyStreamSecret)
	if err != nil || secret != testStreamSecret {
		t.Errorf("stream_secret = %q, %v", secret, err)
	}
	cacheDir, err := settings.String(ctx, store.KeyCacheDir)
	if err != nil || cacheDir != "/data/cache" {
		t.Errorf("cache_dir = %q, %v", cacheDir, err)
	}
	// WebDAV 账号密码不走 env（lxz 2026-10-31 定，归 MKACG-622 的配置写入接口）：
	// 即使设了这两个 env 也不写库，避免两个真值。
	for _, key := range []store.SettingKey{store.KeyWebDAVUser, store.KeyWebDAVPass} {
		if hasSettingRow(t, db, key) {
			t.Errorf("%s 被 env 写入了（WebDAV 凭据不该走 env）", key)
		}
	}
}

// 幂等：重启时手改过的值不被 env 覆盖（AC：重启后手改的 settings 不被覆盖）。
func TestBootstrapSettingsDoesNotOverwrite(t *testing.T) {
	t.Setenv("DRAMA_HUB_STREAM_SECRET", testStreamSecret)
	t.Setenv("DRAMA_HUB_CACHE_DIR", "/env/cache")

	settings, _ := newBootstrapEnv(t)
	ctx := context.Background()
	bootstrapSettings(ctx, settings, slog.New(slog.DiscardHandler))

	// 模拟「重启前手改成别的值」。
	if err := settings.Set(ctx, store.KeyCacheDir, "/manual/cache"); err != nil {
		t.Fatalf("Set cache_dir: %v", err)
	}
	// 下次启动再跑一次引导。
	bootstrapSettings(ctx, settings, slog.New(slog.DiscardHandler))

	cacheDir, err := settings.String(ctx, store.KeyCacheDir)
	if err != nil {
		t.Fatalf("String: %v", err)
	}
	if cacheDir != "/manual/cache" {
		t.Fatalf("cache_dir = %q, 期望手改值 /manual/cache 不被 env 覆盖", cacheDir)
	}
}

// env 缺省：不写任何行（不留空值），也不报错。
func TestBootstrapSettingsEmptyEnvWritesNothing(t *testing.T) {
	for _, env := range []string{"DRAMA_HUB_STREAM_SECRET", "DRAMA_HUB_CACHE_DIR"} {
		t.Setenv(env, "")
	}
	settings, db := newBootstrapEnv(t)
	bootstrapSettings(context.Background(), settings, slog.New(slog.DiscardHandler))

	for _, key := range []store.SettingKey{store.KeyStreamSecret, store.KeyCacheDir} {
		if hasSettingRow(t, db, key) {
			t.Errorf("%s 在 env 缺省时被写了行", key)
		}
	}
	// 必填项仍是「未设置」，不是被写成了空串（否则 /video 会拿空密钥去签）。
	if _, err := settings.String(context.Background(), store.KeyStreamSecret); !errors.Is(err, store.ErrSettingMissing) {
		t.Errorf("stream_secret 错误 = %v, 期望 ErrSettingMissing", err)
	}
}

// 非法值：拒绝写入、不 panic，服务照常起（看板上显示「未设置」）。
func TestBootstrapSettingsRejectsInvalidValues(t *testing.T) {
	t.Setenv("DRAMA_HUB_STREAM_SECRET", "too-short")
	t.Setenv("DRAMA_HUB_WEBDAV_USER", "")
	t.Setenv("DRAMA_HUB_CACHE_DIR", "")

	settings, db := newBootstrapEnv(t)
	if err := settings.Set(context.Background(), store.KeyStreamSecret, testStreamSecret); err != nil {
		t.Fatalf("预置合法 secret: %v", err)
	}
	bootstrapSettings(context.Background(), settings, slog.New(slog.DiscardHandler))

	// 已有的合法值不被短 secret 覆盖。
	secret, err := settings.String(context.Background(), store.KeyStreamSecret)
	if err != nil {
		t.Fatalf("String: %v", err)
	}
	if secret != testStreamSecret {
		t.Fatalf("stream_secret = %q, 期望保留原值（短 secret 被拒）", secret)
	}
	// cache_dir 空串不是合法值：不写行（保留默认）。
	if hasSettingRow(t, db, store.KeyCacheDir) {
		t.Error("非法 cache_dir 被写进了库")
	}
}

// 拼错/废弃的 DRAMA_HUB_* 打一条 warn（只报名字、不报值），已知的四个不报。
func TestWarnUnknownEnv(t *testing.T) {
	t.Setenv("DRAMA_HUB_STREAM_SECRET", testStreamSecret)
	t.Setenv("DRAMA_HUB_WEBDAV_USR", "typo")

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	warnUnknownEnv(logger)

	out := buf.String()
	if !strings.Contains(out, "DRAMA_HUB_WEBDAV_USR") {
		t.Errorf("未知变量没有被点名：%s", out)
	}
	if strings.Contains(out, "env=DRAMA_HUB_STREAM_SECRET") {
		t.Errorf("已知变量被误报：%s", out)
	}
	// warn 里要列出支持的键（B4：让人一眼看见该用哪个）。
	if !strings.Contains(out, "supported=") || !strings.Contains(out, "DRAMA_HUB_STREAM_SECRET") {
		t.Errorf("warn 没有列出支持的 env：%s", out)
	}
	if strings.Contains(out, testStreamSecret) {
		t.Errorf("日志里出现了 secret 值：%s", out)
	}
}
