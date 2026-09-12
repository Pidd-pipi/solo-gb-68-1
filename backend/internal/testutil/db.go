// Package testutil 提供集成测试公用的数据库环境。
// 仅被各包的 _test.go 引用，不会进入正式二进制。
package testutil

import (
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"irrigation/pkg/database"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var dbNamePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// SetupPostgresDB 准备测试数据库环境：
//  1. 连接测试 PostgreSQL（可用 TEST_PG_HOST/PORT/USER/PASSWORD/DB 环境变量覆盖）；
//  2. 自动创建专用测试库（默认 irrigation_test，与开发库隔离，可安全重置）；
//  3. 重置 public schema 并加载 database/init.sql，保证干净环境下反复执行结果一致；
//  4. 设置全局 database.DB 供被测服务使用。
//
// 数据库不可达时调用 t.Skip 跳过，无数据库环境下 go test 仍然通过。
func SetupPostgresDB(t *testing.T) {
	t.Helper()

	host := envOr("TEST_PG_HOST", "127.0.0.1")
	port := envOr("TEST_PG_PORT", "55432")
	user := envOr("TEST_PG_USER", "postgres")
	password := os.Getenv("TEST_PG_PASSWORD")
	dbName := envOr("TEST_PG_DB", "irrigation_test")

	if !dbNamePattern.MatchString(dbName) {
		t.Fatalf("非法测试数据库名: %q", dbName)
	}

	maintDSN := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=postgres sslmode=disable",
		host, port, user, password)
	maint, err := sql.Open("pgx", maintDSN)
	if err != nil {
		t.Skipf("无法连接测试数据库，跳过集成测试: %v", err)
	}
	defer maint.Close()
	if err := maint.Ping(); err != nil {
		t.Skipf("无法连接测试数据库，跳过集成测试: %v", err)
	}

	var exists int
	err = maint.QueryRow("SELECT 1 FROM pg_database WHERE datname = $1", dbName).Scan(&exists)
	if err == sql.ErrNoRows {
		if _, err := maint.Exec(fmt.Sprintf("CREATE DATABASE %s", dbName)); err != nil {
			t.Fatalf("创建测试数据库失败: %v", err)
		}
	} else if err != nil {
		t.Fatalf("检查测试数据库失败: %v", err)
	}

	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		host, port, user, password, dbName)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("连接测试数据库失败: %v", err)
	}

	// 重置 schema，保证每次运行在干净环境执行
	if err := db.Exec("DROP SCHEMA public CASCADE").Error; err != nil {
		t.Fatalf("重置 schema 失败: %v", err)
	}
	if err := db.Exec("CREATE SCHEMA public").Error; err != nil {
		t.Fatalf("重建 schema 失败: %v", err)
	}

	sqlBytes, err := os.ReadFile("../../../database/init.sql")
	if err != nil {
		t.Fatalf("读取 init.sql 失败: %v", err)
	}
	for _, stmt := range strings.Split(string(sqlBytes), ";\n") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("执行 init.sql 语句失败: %v\n语句: %s", err, stmt)
		}
	}

	database.DB = db
	t.Cleanup(func() { database.DB = nil })
}
