// Package sqlite 的检查点测试用临时目录中的真实 SQLite 数据库验证 TRUNCATE 检查点清空 WAL、忙时立即放弃且恢复忙等待，
// 恢复忙等待失败时作废连接，以及打开数据库时执行一次检查点。
package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	moderncsqlite "modernc.org/sqlite"
)

// walSize 返回数据目录中 WAL 文件的大小；文件不存在时视为 0。
func walSize(t *testing.T, dir string) int64 {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, databaseFileName+"-wal"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("读取 WAL 文件信息失败: %v", err)
	}
	return info.Size()
}

// insertTasks 绕过 API 插入 n 条 id 以 prefix 开头的任务，使 WAL 中留下新的帧；prefix 与序号合起来须为 10 个字符。
func insertTasks(t *testing.T, db *sql.DB, prefix string, n int) {
	t.Helper()
	for i := range n {
		insertTask(t, db, fmt.Sprintf("%s%02d", prefix, i))
	}
}

// TestTruncateWAL 验证 truncateWAL：写入后 WAL 非空，检查点完成时返回 true 且 WAL 被截断为 0 字节；
// 另一个连接持有读事务时不等待忙等待期限，在 200ms 内返回 false，之后同一存储的 busy_timeout 仍为 5000；
// 读事务结束后再次调用返回 true 且 WAL 为 0 字节。
func TestTruncateWAL(t *testing.T) {
	dir := dataDir(t)
	store := openStore(t, dir)
	insertTasks(t, store.db, "wal00000", 5)
	if size := walSize(t, dir); size == 0 {
		t.Fatal("写入后 WAL 应当非空")
	}
	if !truncateWAL(t.Context(), store.db) {
		t.Fatal("没有读者时 truncateWAL 应返回 true")
	}
	if size := walSize(t, dir); size != 0 {
		t.Errorf("检查点后 WAL 大小 = %d; want 0", size)
	}

	insertTasks(t, store.db, "wal00001", 5)
	reader, err := sql.Open("sqlite", "file:"+uriEscaper.Replace(filepath.Join(dir, databaseFileName)))
	if err != nil {
		t.Fatalf("打开读者连接失败: %v", err)
	}
	defer reader.Close()
	tx, err := reader.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("开始读事务失败: %v", err)
	}
	defer tx.Rollback()
	// 读取一次才真正取得读快照；此时 WAL 中有未截断的帧，读者会一直引用它们。
	var n int
	if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM tasks").Scan(&n); err != nil {
		t.Fatalf("读事务查询失败: %v", err)
	}
	began := time.Now()
	if truncateWAL(t.Context(), store.db) {
		t.Error("有读者引用 WAL 时 truncateWAL 应返回 false")
	}
	if elapsed := time.Since(began); elapsed > 200*time.Millisecond {
		t.Errorf("有读者时 truncateWAL 用时 %v; want 200ms 内立即返回", elapsed)
	}
	var timeout string
	if err := store.db.QueryRowContext(t.Context(), "PRAGMA busy_timeout").Scan(&timeout); err != nil || timeout != "5000" {
		t.Errorf("检查点后 busy_timeout = %q, %v; want 5000", timeout, err)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("结束读事务失败: %v", err)
	}
	if !truncateWAL(t.Context(), store.db) {
		t.Error("读事务结束后 truncateWAL 应返回 true")
	}
	if size := walSize(t, dir); size != 0 {
		t.Errorf("读事务结束后检查点 WAL 大小 = %d; want 0", size)
	}
}

// restoreFailConnector 用 modernc 驱动打开 dsn，把连接包装为 restoreFailConn，并统计被关闭的连接数。
type restoreFailConnector struct {
	dsn    string
	closed atomic.Int32
}

// Connect 打开一个真实的 SQLite 连接并包装为 restoreFailConn。
func (c *restoreFailConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &restoreFailConn{Conn: conn, closed: &c.closed}, nil
}

// Driver 返回 modernc 的 SQLite 驱动；零值即可使用。
func (c *restoreFailConnector) Driver() driver.Driver {
	return &moderncsqlite.Driver{}
}

// restoreFailConn 让 truncateWAL 恢复 busy_timeout 的语句失败，其余语句原样转发给真实连接，关闭时计数。
type restoreFailConn struct {
	driver.Conn
	closed *atomic.Int32
}

// ExecContext 对恢复 busy_timeout 的语句返回注入的错误，其余语句转发给真实连接。
func (c *restoreFailConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if query == "PRAGMA busy_timeout = "+busyTimeoutMillis {
		return nil, errors.New("injected restore failure")
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

// QueryContext 把查询原样转发给真实连接。
func (c *restoreFailConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

// Close 记录一次关闭并关闭真实连接。
func (c *restoreFailConn) Close() error {
	c.closed.Add(1)
	return c.Conn.Close()
}

// TestTruncateWALDiscardsConnOnRestoreFailure 验证恢复 busy_timeout 失败时 truncateWAL 让连接作废：包装驱动使恢复语句失败，
// truncateWAL 返回 false，这唯一的连接恰好被关闭一次；之后的查询按 connectionParams 新建连接，busy_timeout 读回 5000，
// 而不是沿用忙等待为 0 的旧连接。
func TestTruncateWALDiscardsConnOnRestoreFailure(t *testing.T) {
	connector := &restoreFailConnector{dsn: "file:" + uriEscaper.Replace(filepath.Join(t.TempDir(), databaseFileName)) + connectionParams}
	db := sql.OpenDB(connector)
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	if truncateWAL(t.Context(), db) {
		t.Error("恢复 busy_timeout 失败时 truncateWAL 应返回 false")
	}
	if got := connector.closed.Load(); got != 1 {
		t.Errorf("连接关闭次数 = %d; want 1", got)
	}
	var timeout string
	if err := db.QueryRowContext(t.Context(), "PRAGMA busy_timeout").Scan(&timeout); err != nil || timeout != "5000" {
		t.Errorf("恢复失败后 busy_timeout = %q, %v; want 5000", timeout, err)
	}
}

// TestOpenTruncatesWAL 验证 Open 在迁移完成后执行一次检查点：新目录打开后迁移写下的帧已被截断，WAL 为 0 字节；
// 存储 A 写入若干行并保持打开，打开同一目录的存储 B 之后 WAL 为 0 字节。
func TestOpenTruncatesWAL(t *testing.T) {
	fresh := dataDir(t)
	openStore(t, fresh)
	if size := walSize(t, fresh); size != 0 {
		t.Errorf("新目录迁移后 WAL 大小 = %d; want 0（检查点须在迁移之后）", size)
	}
	dir := dataDir(t)
	first := openStore(t, dir)
	insertTasks(t, first.db, "open0000", 5)
	if size := walSize(t, dir); size == 0 {
		t.Fatal("写入后 WAL 应当非空")
	}
	openStore(t, dir)
	if size := walSize(t, dir); size != 0 {
		t.Errorf("打开存储 B 后 WAL 大小 = %d; want 0", size)
	}
}
