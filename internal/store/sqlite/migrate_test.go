// Package sqlite 的迁移测试用临时目录中的真实 SQLite 数据库验证版本号、幂等、多个迁移依次应用、版本过新与失败回滚。
package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"testing/fstest"
)

// openRawDB 在独立的临时目录创建尚未迁移的数据库连接，测试结束时关闭。
func openRawDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openDB(t.Context(), filepath.Join(t.TempDir(), databaseFileName))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// userVersion 读取数据库当前的 user_version。
func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var version int
	if err := db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("读取 user_version 失败: %v", err)
	}
	return version
}

// schemaObjects 按「类型:名称」排序返回用户定义的表与索引，排除 SQLite 内部对象与自动索引。
func schemaObjects(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(),
		"SELECT type || ':' || name FROM sqlite_schema WHERE type IN ('table', 'index') AND name NOT LIKE 'sqlite\\_%' ESCAPE '\\' ORDER BY 1")
	if err != nil {
		t.Fatalf("查询 sqlite_schema 失败: %v", err)
	}
	defer rows.Close()
	var objects []string
	for rows.Next() {
		var object string
		if err := rows.Scan(&object); err != nil {
			t.Fatalf("读取 sqlite_schema 失败: %v", err)
		}
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 sqlite_schema 失败: %v", err)
	}
	return objects
}

// TestMigrateEmptyDatabase 验证空库迁移后版本为 1，四张表与三个索引全部存在。
func TestMigrateEmptyDatabase(t *testing.T) {
	db := openRawDB(t)
	if err := migrate(t.Context(), db, migrationFS); err != nil {
		t.Fatalf("migrate 返回错误: %v", err)
	}
	if got := userVersion(t, db); got != 1 {
		t.Errorf("user_version = %d; want 1", got)
	}
	want := []string{
		"index:replies_by_task_state",
		"index:replies_one_in_flight",
		"index:task_events_by_task",
		"table:inbound_messages",
		"table:replies",
		"table:task_events",
		"table:tasks",
	}
	if got := schemaObjects(t, db); !slices.Equal(got, want) {
		t.Errorf("schema = %v; want %v", got, want)
	}
}

// TestMigrateTwiceIsNoop 验证已迁移的数据库再次迁移与重新打开都不会重复执行脚本，已有数据保持不变。
func TestMigrateTwiceIsNoop(t *testing.T) {
	db := openRawDB(t)
	for range 2 {
		if err := migrate(t.Context(), db, migrationFS); err != nil {
			t.Fatalf("migrate 返回错误: %v", err)
		}
	}
	if got := userVersion(t, db); got != 1 {
		t.Errorf("user_version = %d; want 1", got)
	}

	dir := dataDir(t)
	store := openStore(t, dir)
	insertTask(t, store.db, "0123456789")
	if err := store.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}
	store = openStore(t, dir)
	version, err := store.SchemaVersion(t.Context())
	if err != nil || version != 1 {
		t.Errorf("SchemaVersion = %d, %v; want 1, nil", version, err)
	}
	if got := countRows(t, store.db, "tasks"); got != 1 {
		t.Errorf("重新打开后 tasks 行数 = %d; want 1", got)
	}
}

// TestOpenRejectsNewerSchema 验证 user_version 高于已知迁移时 Open 返回 ErrSchemaTooNew，且不改动数据库。
// 版本恰好比已知迁移多 1 是旧版程序打开新版数据库的典型情形，用来钉住版本比较的边界；
// 设置版本后 SchemaVersion 须读回同一个值，证明它读取的是真实的 user_version。
func TestOpenRejectsNewerSchema(t *testing.T) {
	scripts, err := readMigrations(migrationFS)
	if err != nil {
		t.Fatalf("读取迁移失败: %v", err)
	}
	for _, newer := range []int{len(scripts) + 1, 99} {
		t.Run(fmt.Sprint(newer), func(t *testing.T) {
			dir := dataDir(t)
			store := openStore(t, dir)
			insertTask(t, store.db, "0123456789")
			// PRAGMA 不支持参数绑定；newer 是测试给定的整数，直接格式化即可。
			if _, err := store.db.ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version = %d", newer)); err != nil {
				t.Fatalf("设置 user_version 失败: %v", err)
			}
			if version, err := store.SchemaVersion(t.Context()); err != nil || version != newer {
				t.Errorf("SchemaVersion = %d, %v; want %d, nil", version, err, newer)
			}
			before := schemaObjects(t, store.db)
			if err := store.Close(); err != nil {
				t.Fatalf("Close 返回错误: %v", err)
			}

			got, err := Open(t.Context(), dir, Options{})
			if !errors.Is(err, ErrSchemaTooNew) {
				t.Fatalf("Open 错误 = %v; want ErrSchemaTooNew", err)
			}
			if got != nil {
				t.Errorf("Open 失败时返回了非空 Store")
			}
			requireErrorWithoutPath(t, err, dir)

			db, err := openDB(t.Context(), filepath.Join(dir, databaseFileName))
			if err != nil {
				t.Fatalf("重新打开数据库失败: %v", err)
			}
			defer db.Close()
			if version := userVersion(t, db); version != newer {
				t.Errorf("user_version = %d; want %d", version, newer)
			}
			if after := schemaObjects(t, db); !slices.Equal(after, before) {
				t.Errorf("schema = %v; want %v", after, before)
			}
			if rows := countRows(t, db, "tasks"); rows != 1 {
				t.Errorf("tasks 行数 = %d; want 1", rows)
			}
		})
	}
}

// TestMigrateAppliesMultipleMigrations 验证多个迁移按编号各执行一次：空库一次应用 0001 与 0002，
// 已在版本 1 的库只追加执行 0002。任一迁移被重复执行或执行错脚本，都会因表已存在而失败。
func TestMigrateAppliesMultipleMigrations(t *testing.T) {
	first := &fstest.MapFile{Data: []byte("CREATE TABLE a (id INTEGER PRIMARY KEY) STRICT;")}
	second := &fstest.MapFile{Data: []byte("CREATE TABLE b (id INTEGER PRIMARY KEY) STRICT;")}
	tests := []struct {
		name    string
		initial fstest.MapFS
	}{
		{"空库一次应用两个迁移", fstest.MapFS{}},
		{"版本 1 只追加新迁移", fstest.MapFS{"0001_a.sql": first}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openRawDB(t)
			if err := migrate(t.Context(), db, tt.initial); err != nil {
				t.Fatalf("初始迁移返回错误: %v", err)
			}
			if err := migrate(t.Context(), db, fstest.MapFS{"0001_a.sql": first, "0002_b.sql": second}); err != nil {
				t.Fatalf("migrate 返回错误: %v", err)
			}
			if got := userVersion(t, db); got != 2 {
				t.Errorf("user_version = %d; want 2", got)
			}
			if got, want := schemaObjects(t, db), []string{"table:a", "table:b"}; !slices.Equal(got, want) {
				t.Errorf("schema = %v; want %v", got, want)
			}
		})
	}
}

// TestMigrateRollsBackFailedMigration 验证失败的迁移整体回滚：版本停在上一个迁移，脚本中已执行的语句也被撤销。
func TestMigrateRollsBackFailedMigration(t *testing.T) {
	tests := []struct {
		name string
		bad  string
	}{
		{"单条语句语法错误", "CREATE TABLE broken ("},
		// 第一条语句已成功执行，只有整个迁移在一个事务中时 partial 表才会随回滚消失。
		{"部分语句已执行", "CREATE TABLE partial (id INTEGER) STRICT; CREATE TABLE broken ("},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openRawDB(t)
			fsys := fstest.MapFS{
				"0001_init.sql": {Data: []byte("CREATE TABLE first (id INTEGER PRIMARY KEY) STRICT;")},
				"0002_bad.sql":  {Data: []byte(tt.bad)},
			}
			if err := migrate(t.Context(), db, fsys); err == nil {
				t.Fatal("migrate 应当返回错误")
			}
			if got := userVersion(t, db); got != 1 {
				t.Errorf("user_version = %d; want 1", got)
			}
			if got, want := schemaObjects(t, db), []string{"table:first"}; !slices.Equal(got, want) {
				t.Errorf("schema = %v; want %v", got, want)
			}
		})
	}
}

// TestMigrateRejectsInvalidFileNames 验证文件名不符合「四位数字_名称.sql」或编号不从 1 连续递增时，
// 在执行任何迁移之前报错。
func TestMigrateRejectsInvalidFileNames(t *testing.T) {
	const valid = "CREATE TABLE first (id INTEGER PRIMARY KEY) STRICT;"
	tests := []struct {
		name  string
		files []string
	}{
		{"编号不连续", []string{"0001_init.sql", "0003_next.sql"}},
		{"编号不从 1 开始", []string{"0002_init.sql"}},
		{"编号重复", []string{"0001_init.sql", "0001_again.sql"}},
		{"编号不足四位", []string{"001_init.sql"}},
		{"缺少下划线", []string{"0001-init.sql"}},
		{"名称为空", []string{"0001_.sql"}},
		{"扩展名错误", []string{"0001_init.txt"}},
		// 以下用例钉住文件名正则的首尾锚点与名称字符集。
		{"编号超过四位", []string{"00001_init.sql"}},
		{"编号前有前缀", []string{"x0001_init.sql"}},
		{"名称含点", []string{"0001_init.v2.sql"}},
		{"扩展名后有后缀", []string{"0001_init.sql.bak"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openRawDB(t)
			fsys := fstest.MapFS{}
			for _, name := range tt.files {
				fsys[name] = &fstest.MapFile{Data: []byte(valid)}
			}
			if err := migrate(t.Context(), db, fsys); err == nil {
				t.Fatal("migrate 应当返回错误")
			}
			if got := userVersion(t, db); got != 0 {
				t.Errorf("user_version = %d; want 0", got)
			}
			if got := schemaObjects(t, db); len(got) != 0 {
				t.Errorf("schema = %v; want 空", got)
			}
		})
	}
}
