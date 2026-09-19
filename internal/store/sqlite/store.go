// Package sqlite 用 SQLite 持久化任务、入站回复与回复队列；所有状态写入都在事务中经状态机校验。
package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	// 导入时注册纯 Go 实现的 "sqlite" 驱动，构建不依赖 CGO；错误类型与结果码用于识别 SQLITE_BUSY。
	moderncsqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Store 是可并发使用的 SQLite 存储；进程内所有操作串行经过单个连接。
type Store struct {
	db     *sql.DB
	now    func() time.Time
	random io.Reader
}

// Options 允许测试注入时钟与随机源；零值使用 time.Now 与 crypto/rand。
type Options struct {
	Now    func() time.Time
	Random io.Reader
}

const (
	// databaseFileName 是数据目录中的数据库文件名。
	databaseFileName = "turncourier.db"
	// connectionParams 设置外键、WAL、FULL 同步与 5 秒忙等待，并让事务以 BEGIN IMMEDIATE 开始：
	// 事务一开始就取得写锁，避免先读后写的事务在升级写锁时绕过忙等待直接返回 SQLITE_BUSY。
	connectionParams = "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)&_txlock=immediate"
	// openBusyTimeout 是打开数据库时重试 SQLITE_BUSY 的期限，与 connectionParams 中的 busy_timeout 一致。
	openBusyTimeout = 5 * time.Second
	// busyRetryInterval 是两次重试之间的间隔。
	busyRetryInterval = 10 * time.Millisecond
)

// uriEscaper 转义 SQLite URI 路径中有特殊含义的字符：% 引出转义序列，? 开始参数，# 开始片段。
var uriEscaper = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23")

// Open 在 dataDir 中打开或创建 turncourier.db 并执行未应用的迁移。
// dataDir 必须是绝对路径；目录权限须为 0700、数据库文件须为 0600（Unix），否则拒绝打开。
func Open(ctx context.Context, dataDir string, opts Options) (*Store, error) {
	if !filepath.IsAbs(dataDir) {
		return nil, errors.New("data directory must be an absolute path")
	}
	path := filepath.Join(dataDir, databaseFileName)
	if err := prepareFiles(dataDir, path); err != nil {
		return nil, err
	}
	db, err := openDB(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := migrate(ctx, db, migrationFS); err != nil {
		db.Close()
		return nil, err
	}
	store := &Store{db: db, now: opts.Now, random: opts.Random}
	if store.now == nil {
		store.now = time.Now
	}
	if store.random == nil {
		store.random = rand.Reader
	}
	return store, nil
}

// prepareFiles 创建缺失的数据目录（0700）与空数据库文件（0600），并拒绝权限更宽或类型不对的已有文件。
// 数据库文件由这里预先创建，SQLite 随后生成的 WAL 与共享内存文件沿用它的权限；错误文本不含本机路径。
func prepareFiles(dataDir, path string) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("cannot create data directory: %w", withoutPath(err))
	}
	info, err := os.Stat(dataDir)
	if err != nil {
		return fmt.Errorf("cannot read data directory: %w", withoutPath(err))
	}
	if err := checkPrivate(info, "data directory"); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		err = file.Close()
	}
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("cannot create database file: %w", withoutPath(err))
	}
	info, err = os.Stat(path)
	if err != nil {
		return fmt.Errorf("cannot read database file: %w", withoutPath(err))
	}
	if !info.Mode().IsRegular() {
		return errors.New("database file is not a regular file")
	}
	return checkPrivate(info, "database file")
}

// openDB 以固定连接参数打开 path 处的数据库，限制为单个连接并用 PingContext 触发实际打开。
// 多个进程首次同时打开新数据库时，空文件从回滚日志模式转为 WAL 的锁升级冲突不经 busy_timeout 等待，
// 直接返回 SQLITE_BUSY，因此打开在 openBusyTimeout 内遇到 SQLITE_BUSY 时重试。
func openDB(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+uriEscaper.Replace(path)+connectionParams)
	if err != nil {
		return nil, fmt.Errorf("cannot open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := retryWhileBusy(ctx, openBusyTimeout, db.PingContext); err != nil {
		db.Close()
		return nil, fmt.Errorf("cannot open database: %w", err)
	}
	return db, nil
}

// retryWhileBusy 调用 op，返回 SQLITE_BUSY 时每隔 busyRetryInterval 重试，直到 op 成功、返回其他错误或超过 timeout；
// ctx 在等待期间结束时返回 ctx.Err()。
func retryWhileBusy(ctx context.Context, timeout time.Duration, op func(context.Context) error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := op(ctx)
		if !isBusy(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(busyRetryInterval):
		}
	}
}

// isBusy 报告 err 是否为 SQLite 的 SQLITE_BUSY；扩展结果码的低 8 位是主结果码。
func isBusy(err error) bool {
	var sqliteErr *moderncsqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqlite3.SQLITE_BUSY
}

// Close 关闭数据库连接。
func (s *Store) Close() error {
	return s.db.Close()
}

// SchemaVersion 返回数据库当前的 user_version。
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("cannot read schema version: %w", err)
	}
	return version, nil
}

// withoutPath 去掉文件系统错误中的路径，保证面向用户的错误不暴露本机绝对路径。
func withoutPath(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}
