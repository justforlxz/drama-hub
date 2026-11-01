// Package pgtest 给测试提供一次性的 PostgreSQL schema。
//
// 测试库由 DRAMA_HUB_TEST_DB_URL 指定（postgres:// URL）；每个测试拿到一个
// 独立 schema，互不干扰，结束后 DROP。CI 的 integration 级起 postgres:16 并注入
// 这个环境变量，unit 级不跑任何碰数据库的包。
package pgtest

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// EnvDBURL 是测试库连接串的环境变量名。
const EnvDBURL = "DRAMA_HUB_TEST_DB_URL"

// DSN 在测试库上建一个独立 schema，返回 search_path 指向它的连接串。
//
// 没设置 DRAMA_HUB_TEST_DB_URL 时直接失败而不是 skip：数据库测试被静默跳过
// 等于给绿灯，而这批测试是 store/迁移的验收本体。
func DSN(t testing.TB) string {
	t.Helper()
	base := os.Getenv(EnvDBURL)
	if base == "" {
		t.Fatalf("%s 未设置：数据库测试需要 PostgreSQL（本地起 postgres:16 容器）", EnvDBURL)
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "postgres" && u.Scheme != "postgresql" {
		t.Fatalf("%s=%q 不是 postgres:// URL", EnvDBURL, base)
	}

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("连接测试库: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	schema := "pgtest_" + randomHex(t)
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatalf("建测试 schema %s: %v", schema, err)
	}
	// LIFO：调用方的连接池先 Close，再 DROP schema。
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Logf("DROP SCHEMA %s 失败（不影响断言）: %v", schema, err)
		}
	})

	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// randomHex 返回 8 字节随机数的十六进制串，作为 schema 名的唯一后缀。
func randomHex(t testing.TB) string {
	t.Helper()
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("生成 schema 名: %v", err)
	}
	return hex.EncodeToString(buf[:])
}

// Open 打开一个指向新 schema 的裸 *sql.DB（不跑迁移）。
func Open(t testing.TB) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", DSN(t))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
