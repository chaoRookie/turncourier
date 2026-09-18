// Package sqlite 用嵌入的 SQL 脚本迁移数据库结构，并以 user_version 记录已应用的迁移编号。
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
)

// ErrSchemaTooNew 表示数据库由更新版本的 TurnCourier 创建，当前版本拒绝打开以免破坏数据。
var ErrSchemaTooNew = errors.New("database schema is newer than this build")

// embeddedMigrations 保存随二进制发布的迁移脚本。
//
//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// migrationFS 是去掉 migrations/ 前缀的迁移目录；目录名是常量，fs.Sub 不会失败。
var migrationFS, _ = fs.Sub(embeddedMigrations, "migrations")

// migrationName 匹配「四位数字_名称.sql」形式的迁移文件名，第一个分组是迁移编号。
var migrationName = regexp.MustCompile(`^([0-9]{4})_[a-z0-9_]+\.sql$`)

// migrate 按编号依次应用 fsys 根目录中尚未应用的迁移，每个迁移与 user_version 的更新在同一事务中提交。
// 文件名全部校验通过后才开始执行；数据库版本高于已知迁移数时返回 ErrSchemaTooNew，不做任何改动。
func migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	scripts, err := readMigrations(fsys)
	if err != nil {
		return err
	}
	for version := 1; version <= len(scripts); version++ {
		if err := applyMigration(ctx, db, scripts, version); err != nil {
			return err
		}
	}
	return nil
}

// readMigrations 读取全部迁移脚本；文件名须符合约定，且编号从 0001 起连续递增。
func readMigrations(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("cannot list migrations: %w", err)
	}
	scripts := make([]string, 0, len(entries))
	// fs.ReadDir 按文件名排序，编号连续时第 i 个文件的编号恰为 i+1。
	for i, entry := range entries {
		match := migrationName.FindStringSubmatch(entry.Name())
		if match == nil {
			return nil, fmt.Errorf("invalid migration file name %q", entry.Name())
		}
		if number, _ := strconv.Atoi(match[1]); number != i+1 {
			return nil, fmt.Errorf("migration %q is out of sequence: want number %04d", entry.Name(), i+1)
		}
		script, err := fs.ReadFile(fsys, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("cannot read migration %q: %w", entry.Name(), err)
		}
		scripts = append(scripts, string(script))
	}
	return scripts, nil
}

// applyMigration 在一个 IMMEDIATE 事务中应用编号为 version 的迁移。
// 事务内重新读取 user_version：其他进程已应用该迁移时直接跳过，因此多个进程同时打开也不会重复执行。
func applyMigration(ctx context.Context, db *sql.DB, scripts []string, version int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("cannot begin migration %04d: %w", version, err)
	}
	// 提交成功后 Rollback 只返回 sql.ErrTxDone，可以安全忽略。
	defer tx.Rollback()
	var current int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("cannot read schema version: %w", err)
	}
	if current > len(scripts) {
		return ErrSchemaTooNew
	}
	if current >= version {
		return nil
	}
	if _, err := tx.ExecContext(ctx, scripts[version-1]); err != nil {
		return fmt.Errorf("apply migration %04d: %w", version, err)
	}
	// PRAGMA 不支持参数绑定；version 是整数，直接格式化不存在注入风险。
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("cannot record migration %04d: %w", version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("cannot commit migration %04d: %w", version, err)
	}
	return nil
}
