// Package sqlite 的打开测试用临时目录中的真实 SQLite 数据库验证连接参数、路径转义、文件权限与 STRICT 表。
package sqlite

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// openStore 在 dir 中打开存储并在测试结束时关闭；重复关闭的错误被忽略。
func openStore(t *testing.T, dir string) *Store {
	t.Helper()
	store, err := Open(t.Context(), dir, Options{})
	if err != nil {
		t.Fatalf("Open 返回错误: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// insertTask 绕过 API 直接插入一条 CREATED 任务，用于验证读写与表约束。
func insertTask(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(),
		"INSERT INTO tasks (id, agent, state, version, created_at, updated_at) VALUES (?, 'codex', 'CREATED', 1, 0, 0)", id)
	if err != nil {
		t.Fatalf("插入任务失败: %v", err)
	}
}

// countRows 返回指定表的行数；表名只来自测试代码中的常量。
func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("统计 %s 行数失败: %v", table, err)
	}
	return n
}

// dataDir 返回临时目录下尚不存在的数据目录路径，由 Open 以 0700 创建。
// t.TempDir 按 0777 减去 umask 创建目录（通常为 0755），不能直接作为数据目录。
func dataDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "data")
}

// existingDataDir 预先以 0700 创建数据目录，供需要先放入数据库文件的用例使用。
func existingDataDir(t *testing.T) string {
	t.Helper()
	dir := dataDir(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("创建数据目录失败: %v", err)
	}
	return dir
}

// requireErrorWithoutPath 断言返回了错误，且错误文本不含数据目录所在的临时目录路径。
func requireErrorWithoutPath(t *testing.T, err error, dir string) {
	t.Helper()
	if err == nil {
		t.Fatal("应当返回错误")
	}
	if strings.Contains(err.Error(), filepath.Dir(dir)) {
		t.Errorf("错误文本包含本机路径: %v", err)
	}
}

// fileMode 返回路径的权限位。
func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("读取文件信息失败: %v", err)
	}
	return info.Mode().Perm()
}

// TestOpenConnectionSettings 验证 WAL、外键、FULL 同步、忙等待与单连接设置已生效。
func TestOpenConnectionSettings(t *testing.T) {
	store := openStore(t, dataDir(t))
	tests := []struct {
		pragma string
		want   string
	}{
		{"journal_mode", "wal"},
		{"foreign_keys", "1"},
		{"synchronous", "2"},
		{"busy_timeout", "5000"},
	}
	for _, tt := range tests {
		var got string
		if err := store.db.QueryRowContext(t.Context(), "PRAGMA "+tt.pragma).Scan(&got); err != nil {
			t.Fatalf("读取 PRAGMA %s 失败: %v", tt.pragma, err)
		}
		if got != tt.want {
			t.Errorf("PRAGMA %s = %q; want %q", tt.pragma, got, tt.want)
		}
	}
	if got := store.db.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConnections = %d; want 1", got)
	}
}

// TestOpenBeginsImmediateTransactions 验证事务一开始就取得写锁：
// 存储持有未提交事务时，另一个不做忙等待的连接无法再开始 IMMEDIATE 事务。
func TestOpenBeginsImmediateTransactions(t *testing.T) {
	dir := dataDir(t)
	store := openStore(t, dir)
	tx, err := store.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("BeginTx 返回错误: %v", err)
	}
	defer tx.Rollback()

	// 未设置 busy_timeout 时 SQLite 不等待，锁被占用立即返回 SQLITE_BUSY。
	other, err := sql.Open("sqlite", "file:"+uriEscaper.Replace(filepath.Join(dir, databaseFileName))+"?_txlock=immediate")
	if err != nil {
		t.Fatalf("打开第二个连接失败: %v", err)
	}
	defer other.Close()
	otherTx, err := other.BeginTx(t.Context(), nil)
	if err == nil {
		otherTx.Rollback()
		t.Fatal("存储的事务未持有写锁，第二个连接不应能开始 IMMEDIATE 事务")
	}
}

// TestOpenOptions 验证零值 Options 使用 time.Now 与 crypto/rand，注入的时钟与随机源被原样保存。
func TestOpenOptions(t *testing.T) {
	store := openStore(t, dataDir(t))
	if store.now == nil || store.random != rand.Reader {
		t.Errorf("零值 Options 未使用默认时钟与 crypto/rand")
	}

	fixed := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	random := bytes.NewReader(nil)
	injected, err := Open(t.Context(), dataDir(t), Options{Now: func() time.Time { return fixed }, Random: random})
	if err != nil {
		t.Fatalf("Open 返回错误: %v", err)
	}
	defer injected.Close()
	if !injected.now().Equal(fixed) || injected.random != random {
		t.Errorf("注入的时钟或随机源未被保存")
	}
}

// TestOpenPathWithSpecialCharacters 验证目录名含空格、#、?、%、中文时可以打开、写入，并在重新打开后读回。
// ?、#、% 在 SQLite URI 中有特殊含义，未转义时会截断或改写路径，导致打开失败或悄悄打开另一个文件，
// 因此还要用 PRAGMA database_list 核对 SQLite 实际打开的文件。
func TestOpenPathWithSpecialCharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "数据 目录 #1 ?x=%20")
	store := openStore(t, dir)
	want, err := filepath.EvalSymlinks(filepath.Join(dir, databaseFileName))
	if err != nil {
		t.Fatalf("解析数据库文件路径失败: %v", err)
	}
	var seq int
	var name, file string
	if err := store.db.QueryRowContext(t.Context(), "PRAGMA database_list").Scan(&seq, &name, &file); err != nil {
		t.Fatalf("读取 PRAGMA database_list 失败: %v", err)
	}
	if file != want {
		t.Errorf("SQLite 打开的文件 = %q; want %q", file, want)
	}
	insertTask(t, store.db, "0123456789")
	if err := store.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}
	store = openStore(t, dir)
	if got := countRows(t, store.db, "tasks"); got != 1 {
		t.Errorf("重新打开后 tasks 行数 = %d; want 1", got)
	}
}

// TestOpenRejectsRelativePath 验证相对路径被拒绝，且不会在当前目录创建任何内容。
func TestOpenRejectsRelativePath(t *testing.T) {
	store, err := Open(t.Context(), filepath.Join("relative", "data"), Options{})
	if err == nil {
		store.Close()
		t.Fatal("Open 应当拒绝相对路径")
	}
	if _, statErr := os.Stat("relative"); !os.IsNotExist(statErr) {
		t.Errorf("相对路径被创建: %v", statErr)
	}
}

// TestOpenRejectsFileAsDataDir 验证数据目录位置已是普通文件时拒绝打开，且文件系统错误中的路径被剥离。
func TestOpenRejectsFileAsDataDir(t *testing.T) {
	dir := dataDir(t)
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatalf("创建文件失败: %v", err)
	}
	_, err := Open(t.Context(), dir, Options{})
	requireErrorWithoutPath(t, err, dir)
}

// TestOpenRejectsInvalidDatabaseFile 验证数据库文件内容为随机字节或不是常规文件时拒绝打开，错误文本不含本机路径。
func TestOpenRejectsInvalidDatabaseFile(t *testing.T) {
	t.Run("随机字节", func(t *testing.T) {
		dir := existingDataDir(t)
		path := filepath.Join(dir, databaseFileName)
		if err := os.WriteFile(path, randomBytes(t, 4096), 0o600); err != nil {
			t.Fatalf("写入数据库文件失败: %v", err)
		}
		_, err := Open(t.Context(), dir, Options{})
		requireErrorWithoutPath(t, err, dir)
	})
	t.Run("目录", func(t *testing.T) {
		dir := existingDataDir(t)
		if err := os.Mkdir(filepath.Join(dir, databaseFileName), 0o700); err != nil {
			t.Fatalf("创建目录失败: %v", err)
		}
		_, err := Open(t.Context(), dir, Options{})
		requireErrorWithoutPath(t, err, dir)
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("错误 = %v; want 常规文件检查失败", err)
		}
	})
}

// randomBytes 返回 n 个来自 crypto/rand 的随机字节。
func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("生成随机字节失败: %v", err)
	}
	return buf
}

// TestOpenCreatesPrivateFiles 仅在 Unix 上验证新建的数据目录为 0700，数据库及其 WAL、共享内存文件为 0600。
func TestOpenCreatesPrivateFiles(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skip("目录与文件权限检查只在 Unix 生效")
	}
	dir := dataDir(t)
	store := openStore(t, dir)
	insertTask(t, store.db, "0123456789")
	if got := fileMode(t, dir); got != 0o700 {
		t.Errorf("数据目录权限 = %o; want 700", got)
	}
	for _, name := range []string{databaseFileName, databaseFileName + "-wal", databaseFileName + "-shm"} {
		if got := fileMode(t, filepath.Join(dir, name)); got != 0o600 {
			t.Errorf("%s 权限 = %o; want 600", name, got)
		}
	}
}

// TestOpenRejectsWidePermissions 仅在 Unix 上验证已有目录或数据库文件对组或其他用户开放任何权限时拒绝打开。
func TestOpenRejectsWidePermissions(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skip("目录与文件权限检查只在 Unix 生效")
	}
	// 0o750 与 0o705（文件 0o640 与 0o604）分别只开放组或其他用户，用来钉住权限掩码同时覆盖两者。
	for _, mode := range []os.FileMode{0o755, 0o750, 0o705} {
		dir := dataDir(t)
		if err := os.Mkdir(dir, mode); err != nil {
			t.Fatalf("创建目录失败: %v", err)
		}
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatalf("设置目录权限失败: %v", err)
		}
		_, err := Open(t.Context(), dir, Options{})
		requireErrorWithoutPath(t, err, dir)
	}
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604} {
		dir := existingDataDir(t)
		path := filepath.Join(dir, databaseFileName)
		if err := os.WriteFile(path, nil, mode); err != nil {
			t.Fatalf("创建数据库文件失败: %v", err)
		}
		// os.WriteFile 会受 umask 影响，权限用例必须显式设置。
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("设置文件权限失败: %v", err)
		}
		_, err := Open(t.Context(), dir, Options{})
		requireErrorWithoutPath(t, err, dir)
	}
	// 权限恰好为 0700 与 0600 的已有目录和空文件可以打开。
	dir := existingDataDir(t)
	if err := os.WriteFile(filepath.Join(dir, databaseFileName), nil, 0o600); err != nil {
		t.Fatalf("创建数据库文件失败: %v", err)
	}
	openStore(t, dir)
}

// TestStrictTables 验证 STRICT 表拒绝与列类型不符的值，而同一行使用正确类型时可以写入。
func TestStrictTables(t *testing.T) {
	store := openStore(t, dataDir(t))
	_, err := store.db.ExecContext(t.Context(),
		"INSERT INTO tasks (id, agent, state, version, created_at, updated_at) VALUES ('0123456789', 'codex', 'CREATED', 'x', 0, 0)")
	if err == nil {
		t.Fatal("STRICT 表应当拒绝把 'x' 写入 INTEGER 列")
	}
	insertTask(t, store.db, "0123456789")
}
