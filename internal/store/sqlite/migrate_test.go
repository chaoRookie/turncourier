// Package sqlite 的迁移测试用临时目录中的真实 SQLite 数据库验证版本号、幂等、多个迁移依次应用、版本过新与失败回滚。
package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
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

// schemaObjects 按「类型:名称」排序返回用户定义的表、索引与触发器，排除 SQLite 内部对象与自动索引。
func schemaObjects(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(),
		"SELECT type || ':' || name FROM sqlite_schema WHERE type IN ('table', 'index', 'trigger') AND name NOT LIKE 'sqlite\\_%' ESCAPE '\\' ORDER BY 1")
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

// schemaV2 是 0001 加 0002 之后全部用户定义的表、索引与触发器，按 schemaObjects 的格式与顺序排列。
var schemaV2 = []string{
	"index:crypto_keys_one_active",
	"index:inbound_rejections_by_time",
	"index:notifications_by_task",
	"index:notifications_one_sending",
	"index:notifications_pending",
	"index:replies_by_task_state",
	"index:replies_one_in_flight",
	"index:task_events_by_task",
	"table:crypto_keys",
	"table:fetch_cursors",
	"table:inbound_messages",
	"table:inbound_rejections",
	"table:instance",
	"table:notification_payloads",
	"table:notifications",
	"table:replies",
	"table:reply_payloads",
	"table:task_events",
	"table:tasks",
	"trigger:notification_payloads_immutable",
	"trigger:notification_payloads_pending_only",
	"trigger:notifications_binding_immutable",
	"trigger:notifications_delivered_id_once",
	"trigger:notifications_drop_payload",
	"trigger:replies_drop_payload",
	"trigger:reply_payloads_immutable",
	"trigger:reply_payloads_queued_only",
}

// TestMigrateEmptyDatabase 验证空库迁移后版本为 2，0001 的四张表与三个索引、0002 新增的表、索引与触发器全部存在，
// 且没有多余对象（例如重建时残留的 inbound_messages_new、replies_new）。
func TestMigrateEmptyDatabase(t *testing.T) {
	db := openRawDB(t)
	if err := migrate(t.Context(), db, migrationFS); err != nil {
		t.Fatalf("migrate 返回错误: %v", err)
	}
	if got := userVersion(t, db); got != 2 {
		t.Errorf("user_version = %d; want 2", got)
	}
	if got := schemaObjects(t, db); !slices.Equal(got, schemaV2) {
		t.Errorf("schema = %v; want %v", got, schemaV2)
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
	if got := userVersion(t, db); got != 2 {
		t.Errorf("user_version = %d; want 2", got)
	}

	dir := dataDir(t)
	store := openStore(t, dir)
	insertTask(t, store.db, "0123456789")
	if err := store.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}
	store = openStore(t, dir)
	version, err := store.SchemaVersion(t.Context())
	if err != nil || version != 2 {
		t.Errorf("SchemaVersion = %d, %v; want 2, nil", version, err)
	}
	if got := countRows(t, store.db, "tasks"); got != 1 {
		t.Errorf("重新打开后 tasks 行数 = %d; want 1", got)
	}
}

// TestMigrateRereadsVersionInsideTransaction 验证迁移在事务内重新读取 user_version：另一连接已在 IMMEDIATE 事务中
// 应用 0001 但尚未提交时开始迁移，等它提交后本次迁移须跳过已提交的 0001、再执行 0002，不重复执行脚本。
// 只在事务外读取版本的实现会读到提交前的 0，随后以 table already exists 失败。
func TestMigrateRereadsVersionInsideTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), databaseFileName)
	var dbs [2]*sql.DB
	for i := range dbs {
		db, err := openDB(t.Context(), path)
		if err != nil {
			t.Fatalf("打开第 %d 个连接失败: %v", i+1, err)
		}
		t.Cleanup(func() { db.Close() })
		dbs[i] = db
	}
	scripts, err := readMigrations(migrationFS)
	if err != nil {
		t.Fatalf("readMigrations 返回错误: %v", err)
	}
	holder, err := dbs[1].BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("BeginTx 返回错误: %v", err)
	}
	defer holder.Rollback()
	for _, stmt := range []string{scripts[0], "PRAGMA user_version = 1"} {
		if _, err := holder.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("在持锁事务中执行迁移失败: %v", err)
		}
	}

	done := make(chan error, 1)
	go func() { done <- migrate(t.Context(), dbs[0], migrationFS) }()
	// 给迁移足够时间读到版本并在 busy_timeout 内等待写锁；即使调度更慢，正确实现也会在提交后读到 1 而通过。
	time.Sleep(200 * time.Millisecond)
	if err := holder.Commit(); err != nil {
		t.Fatalf("提交持锁事务失败: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("并发迁移返回错误: %v", err)
	}
	if got := userVersion(t, dbs[0]); got != 2 {
		t.Errorf("user_version = %d; want 2", got)
	}
	if got := schemaObjects(t, dbs[0]); !slices.Equal(got, schemaV2) {
		t.Errorf("schema = %v; want %v", got, schemaV2)
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

// TestMigrateStopsAtVersion2OnFailedMigration 验证在真实的 0001、0002 之后注入失败的 0003_bad.sql 时，
// 版本停在 2，0003 中已执行的语句随事务回滚，结构仍与版本 2 相同。
func TestMigrateStopsAtVersion2OnFailedMigration(t *testing.T) {
	fsys := fstest.MapFS{"0003_bad.sql": {Data: []byte("CREATE TABLE partial (id INTEGER) STRICT; CREATE TABLE broken (")}}
	entries, err := fs.ReadDir(migrationFS, ".")
	if err != nil {
		t.Fatalf("列出迁移失败: %v", err)
	}
	for _, entry := range entries {
		script, err := fs.ReadFile(migrationFS, entry.Name())
		if err != nil {
			t.Fatalf("读取迁移 %s 失败: %v", entry.Name(), err)
		}
		fsys[entry.Name()] = &fstest.MapFile{Data: script}
	}
	db := openRawDB(t)
	if err := migrate(t.Context(), db, fsys); err == nil {
		t.Fatal("migrate 应当返回错误")
	}
	if got := userVersion(t, db); got != 2 {
		t.Errorf("user_version = %d; want 2", got)
	}
	if got := schemaObjects(t, db); !slices.Equal(got, schemaV2) {
		t.Errorf("schema = %v; want %v", got, schemaV2)
	}
}

// openVersion1 在新的数据目录中按 Open 的权限要求创建数据库，只应用嵌入的 0001_init.sql，
// 模拟 Phase 3 发布的版本 1 数据库；返回数据目录与连接，连接在测试结束时关闭（提前关闭后再次关闭无害）。
func openVersion1(t *testing.T) (string, *sql.DB) {
	t.Helper()
	script, err := fs.ReadFile(migrationFS, "0001_init.sql")
	if err != nil {
		t.Fatalf("读取 0001_init.sql 失败: %v", err)
	}
	dir := dataDir(t)
	path := filepath.Join(dir, databaseFileName)
	if err := prepareFiles(dir, path); err != nil {
		t.Fatalf("准备数据目录失败: %v", err)
	}
	db, err := openDB(t.Context(), path)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate(t.Context(), db, fstest.MapFS{"0001_init.sql": {Data: script}}); err != nil {
		t.Fatalf("迁移到版本 1 失败: %v", err)
	}
	if got := userVersion(t, db); got != 1 {
		t.Fatalf("user_version = %d; want 1", got)
	}
	return dir, db
}

// dumpRows 执行查询 query，把每行各列的值格式化后以 | 连接，按结果顺序返回；用于逐行逐字段比较升级前后的数据。
func dumpRows(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query, args...)
	if err != nil {
		t.Fatalf("查询 %q 失败: %v", query, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("读取 %q 的列失败: %v", query, err)
	}
	var dump []string
	for rows.Next() {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			t.Fatalf("读取 %q 的结果失败: %v", query, err)
		}
		fields := make([]string, len(values))
		for i, value := range values {
			fields[i] = fmt.Sprint(value)
		}
		dump = append(dump, strings.Join(fields, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 %q 的结果失败: %v", query, err)
	}
	return dump
}

// insertV1Reply 在版本 1 的库中直接插入一条入站记录（没有 folder 列）及其回复，返回入站记录 id 与回复序号。
// resumeState 与 rejectReason 为空时写入 NULL。
func insertV1Reply(t *testing.T, db *sql.DB, taskID string, uid int, digest []byte, state, resumeState, rejectReason string) (int64, int64) {
	t.Helper()
	var inboundID, seq int64
	if err := db.QueryRowContext(t.Context(),
		"INSERT INTO inbound_messages (account, uid_validity, uid, message_id, body_sha256, task_id, received_at) VALUES (?, 7, ?, ?, ?, ?, ?) RETURNING id",
		botAccount, uid, fmt.Sprintf("<v1-%d@example.invalid>", uid), digest, taskID, 1000+uid).Scan(&inboundID); err != nil {
		t.Fatalf("插入版本 1 的入站记录失败: %v", err)
	}
	if err := db.QueryRowContext(t.Context(),
		"INSERT INTO replies (inbound_id, task_id, state, resume_state, reject_reason, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING seq",
		inboundID, taskID, state, sql.NullString{String: resumeState, Valid: resumeState != ""},
		sql.NullString{String: rejectReason, Valid: rejectReason != ""}, 2000+uid, 3000+uid).Scan(&seq); err != nil {
		t.Fatalf("插入版本 1 的回复失败: %v", err)
	}
	return inboundID, seq
}

// deleteV1Reply 删除回复及其入站记录（先删引用方），使两张表的 AUTOINCREMENT 序列大于各自现有的最大 id。
func deleteV1Reply(t *testing.T, db *sql.DB, inboundID, seq int64) {
	t.Helper()
	for _, stmt := range []struct {
		query string
		id    int64
	}{{"DELETE FROM replies WHERE seq = ?", seq}, {"DELETE FROM inbound_messages WHERE id = ?", inboundID}} {
		if _, err := db.ExecContext(t.Context(), stmt.query, stmt.id); err != nil {
			t.Fatalf("执行 %q 失败: %v", stmt.query, err)
		}
	}
}

// sequenceOf 返回表在 sqlite_sequence 中的值；没有记录时返回 0。
func sequenceOf(t *testing.T, db *sql.DB, table string) int64 {
	t.Helper()
	var seq int64
	err := db.QueryRowContext(t.Context(), "SELECT seq FROM sqlite_sequence WHERE name = ?", table).Scan(&seq)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("读取 %s 的序列失败: %v", table, err)
	}
	return seq
}

// rebuiltSchemaSQL 按名称读取 inbound_messages、replies 与 replies 两个索引在 sqlite_schema.sql 中的建表文本。
func rebuiltSchemaSQL(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	texts := make(map[string]string)
	for _, name := range []string{"inbound_messages", "replies", "replies_one_in_flight", "replies_by_task_state"} {
		var text string
		if err := db.QueryRowContext(t.Context(), "SELECT sql FROM sqlite_schema WHERE name = ?", name).Scan(&text); err != nil {
			t.Fatalf("读取 %s 的建表文本失败: %v", name, err)
		}
		texts[name] = text
	}
	return texts
}

// replaceOnce 把 text 中恰好出现一次的 old 替换为 replacement；出现次数不为 1 时终止测试，防止期望值构造得不对却悄悄通过。
func replaceOnce(t *testing.T, text, old, replacement string) string {
	t.Helper()
	if n := strings.Count(text, old); n != 1 {
		t.Fatalf("%q 在建表文本中出现 %d 次; want 恰好 1 次", old, n)
	}
	return strings.Replace(text, old, replacement, 1)
}

// TestMigrateUpgradesVersion1Data 在只含 0001 的版本 1 库中写入多个任务、多条入站记录与五种状态的回复，
// 再删除最新的一条回复及其入站记录，使两张表的序列都大于最大 id；随后以真实迁移 Open，验证 0002 重建
// inbound_messages 与 replies 时：数据逐行逐字段保留（folder 为 INBOX），外键与在途唯一索引照常生效，
// AUTOINCREMENT 序列不回退，两张表的建表文本除 folder 列与 UID 唯一键外与 0001 逐字相同，旧行按 INBOX 参与去重。
func TestMigrateUpgradesVersion1Data(t *testing.T) {
	dir, db := openVersion1(t)
	tasks := []string{"0000000001", "0000000002", "0000000003"}
	for _, id := range tasks {
		insertTask(t, db, id)
	}
	if _, err := db.ExecContext(t.Context(),
		"UPDATE tasks SET state = 'RUNNING', session_id = 'thread-synthetic-0002', version = 2, updated_at = 5 WHERE id = ?", tasks[1]); err != nil {
		t.Fatalf("更新任务失败: %v", err)
	}
	// v1Reply 描述一条要在版本 1 的库中直接插入的回复。
	type v1Reply struct {
		taskID, state, resumeState, rejectReason string
		digest                                   []byte
	}
	replies := []v1Reply{
		{tasks[0], "QUEUED", "", "", digestA[:]},
		{tasks[0], "DISPATCHING", "COMPLETED", "", digestB[:]},
		{tasks[1], "UNCERTAIN", "WAITING_INPUT", "", digestA[:]},
		{tasks[1], "ACKNOWLEDGED", "", "", digestB[:]},
		{tasks[2], "REJECTED", "", "task_closed", digestA[:]},
		{tasks[2], "QUEUED", "", "", digestB[:]},
	}
	var lastInbound, lastSeq int64
	for i, r := range replies {
		lastInbound, lastSeq = insertV1Reply(t, db, r.taskID, i+1, r.digest, r.state, r.resumeState, r.rejectReason)
	}
	deleteV1Reply(t, db, lastInbound, lastSeq)
	inboundSeq, replySeq := sequenceOf(t, db, "inbound_messages"), sequenceOf(t, db, "replies")
	var maxInbound, maxSeq int64
	if err := db.QueryRowContext(t.Context(), "SELECT (SELECT max(id) FROM inbound_messages), (SELECT max(seq) FROM replies)").Scan(&maxInbound, &maxSeq); err != nil {
		t.Fatalf("读取最大 id 失败: %v", err)
	}
	if inboundSeq <= maxInbound || replySeq <= maxSeq {
		t.Fatalf("前提不成立: 序列 %d/%d 应大于最大 id %d/%d", inboundSeq, replySeq, maxInbound, maxSeq)
	}

	const (
		tasksQuery    = "SELECT * FROM tasks ORDER BY id"
		repliesQuery  = "SELECT * FROM replies ORDER BY seq"
		sequenceQuery = "SELECT name, seq FROM sqlite_sequence ORDER BY name"
	)
	tasksBefore := dumpRows(t, db, tasksQuery)
	repliesBefore := dumpRows(t, db, repliesQuery)
	// 升级前以常量 'INBOX' 占据 folder 的位置，与升级后读取的真实 folder 列逐行比较。
	inboundBefore := dumpRows(t, db,
		"SELECT id, account, 'INBOX', uid_validity, uid, message_id, body_sha256, task_id, received_at FROM inbound_messages ORDER BY id")
	sequenceBefore := dumpRows(t, db, sequenceQuery)
	schemaBefore := rebuiltSchemaSQL(t, db)
	var queued struct {
		seq       int64
		uid       uint32
		messageID string
		digest    []byte
	}
	if err := db.QueryRowContext(t.Context(),
		"SELECT r.seq, i.uid, i.message_id, i.body_sha256 FROM replies r JOIN inbound_messages i ON i.id = r.inbound_id WHERE r.task_id = ? AND r.state = 'QUEUED'",
		tasks[0]).Scan(&queued.seq, &queued.uid, &queued.messageID, &queued.digest); err != nil {
		t.Fatalf("读取 QUEUED 回复失败: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭版本 1 的库失败: %v", err)
	}

	store := openStore(t, dir)
	if version, err := store.SchemaVersion(t.Context()); err != nil || version != 2 {
		t.Fatalf("SchemaVersion = %d, %v; want 2, nil", version, err)
	}
	if got := schemaObjects(t, store.db); !slices.Equal(got, schemaV2) {
		t.Errorf("schema = %v; want %v", got, schemaV2)
	}
	for _, check := range []struct {
		name   string
		before []string
		after  []string
	}{
		{"tasks", tasksBefore, dumpRows(t, store.db, tasksQuery)},
		{"replies", repliesBefore, dumpRows(t, store.db, repliesQuery)},
		{"inbound_messages", inboundBefore, dumpRows(t, store.db,
			"SELECT id, account, folder, uid_validity, uid, message_id, body_sha256, task_id, received_at FROM inbound_messages ORDER BY id")},
		{"sqlite_sequence", sequenceBefore, dumpRows(t, store.db, sequenceQuery)},
	} {
		if !slices.Equal(check.after, check.before) {
			t.Errorf("升级后 %s = %q; want %q", check.name, check.after, check.before)
		}
	}
	if got := countRows(t, store.db, "reply_payloads"); got != 0 {
		t.Errorf("reply_payloads 行数 = %d; want 0", got)
	}
	if got := dumpRows(t, store.db, "PRAGMA foreign_key_check"); len(got) != 0 {
		t.Errorf("PRAGMA foreign_key_check = %q; want 空", got)
	}
	wantForeignKeys := []string{"inbound_id|inbound_messages|id", "task_id|tasks|id"}
	if got := dumpRows(t, store.db, `SELECT "from", "table", "to" FROM pragma_foreign_key_list('replies') ORDER BY "from"`); !slices.Equal(got, wantForeignKeys) {
		t.Errorf("replies 的外键 = %q; want %q", got, wantForeignKeys)
	}
	if got := dumpRows(t, store.db,
		"SELECT name FROM sqlite_schema WHERE instr(name, '_new') > 0 OR instr(sql, 'inbound_messages_new') > 0 OR instr(sql, 'replies_new') > 0"); len(got) != 0 {
		t.Errorf("sqlite_schema 残留重建用的临时表或引用: %q", got)
	}

	// RENAME 把表名改写为带双引号的形式；比较前只去掉这两个表名两侧的双引号。
	unquote := strings.NewReplacer(`"inbound_messages"`, "inbound_messages", `"replies"`, "replies")
	want := maps.Clone(schemaBefore)
	want["inbound_messages"] = replaceOnce(t, want["inbound_messages"],
		"    account       TEXT NOT NULL CHECK (length(account) BETWEEN 3 AND 254),\n",
		"    account       TEXT NOT NULL CHECK (length(account) BETWEEN 3 AND 254),\n    folder        TEXT NOT NULL CHECK (length(folder) BETWEEN 1 AND 255),\n")
	want["inbound_messages"] = replaceOnce(t, want["inbound_messages"], "UNIQUE (account, uid_validity, uid)", "UNIQUE (account, folder, uid_validity, uid)")
	for name, text := range rebuiltSchemaSQL(t, store.db) {
		if got := unquote.Replace(text); got != want[name] {
			t.Errorf("升级后 %s 的建表文本 =\n%s\nwant\n%s", name, got, want[name])
		}
	}

	var inboundID int64
	if err := store.db.QueryRowContext(t.Context(),
		"INSERT INTO inbound_messages (account, folder, uid_validity, uid, message_id, body_sha256, task_id, received_at) VALUES (?, 'INBOX', 7, 100, '<in-flight@example.invalid>', ?, ?, 0) RETURNING id",
		botAccount, digestA[:], tasks[0]).Scan(&inboundID); err != nil {
		t.Fatalf("插入入站记录失败: %v", err)
	}
	if inboundID <= inboundSeq {
		t.Errorf("升级后新入站记录 id = %d; want 大于升级前的序列 %d", inboundID, inboundSeq)
	}
	if _, err := store.db.ExecContext(t.Context(),
		"INSERT INTO replies (inbound_id, task_id, state, resume_state, created_at, updated_at) VALUES (?, ?, 'DISPATCHING', 'COMPLETED', 0, 0)",
		inboundID, tasks[0]); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: replies.task_id") {
		t.Errorf("同一任务第二条 DISPATCHING: err = %v; want 违反 replies_one_in_flight", err)
	}

	in := InboundReply{TaskID: tasks[0], Account: botAccount, Folder: "INBOX", UIDValidity: 7, UID: queued.uid, MessageID: queued.messageID}
	copy(in.BodySHA256[:], queued.digest)
	if got, err := store.RecordReply(t.Context(), in); err != nil || !got.Duplicate || got.Reply != mustGetReply(t, store, queued.seq) {
		t.Errorf("以 INBOX 重复记录旧行: RecordReply = %+v, %v; want 原回复 %d 且 Duplicate=true", got, err, queued.seq)
	}
	in.UID, in.MessageID, in.BodySHA256 = 200, "<after-upgrade@example.invalid>", digestB
	if got := recordReply(t, store, in); got.Duplicate || got.Reply.Seq <= replySeq {
		t.Errorf("升级后新回复 = %+v; want 新入队且序号大于升级前的序列 %d", got, replySeq)
	}
}

// TestMigrateUpgradesVersion1WithoutRows 验证没有数据行的版本 1 库同样可以升级：库中从未有过行时两张表在
// sqlite_sequence 中始终没有记录；行已全部删除而 sqlite_sequence 仍有记录时，升级后序列值不变，
// 新插入行的 id 与 seq 大于它（只用 UPDATE 复制序列的实现会在这里丢失序列）。
func TestMigrateUpgradesVersion1WithoutRows(t *testing.T) {
	for _, tt := range []struct {
		name    string
		deleted int // 插入后又删除的回复数
		records int // 两张表在 sqlite_sequence 中的记录数
	}{
		{"从未有过行", 0, 0},
		{"行已全部删除", 3, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir, db := openVersion1(t)
			insertTask(t, db, "0000000001")
			for i := range tt.deleted {
				inboundID, seq := insertV1Reply(t, db, "0000000001", i+1, digestA[:], "QUEUED", "", "")
				deleteV1Reply(t, db, inboundID, seq)
			}
			const query = "SELECT name, seq FROM sqlite_sequence WHERE name IN ('inbound_messages', 'replies') ORDER BY name"
			before := dumpRows(t, db, query)
			if len(before) != tt.records {
				t.Fatalf("前提不成立: 升级前 sqlite_sequence = %q", before)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("关闭版本 1 的库失败: %v", err)
			}

			store := openStore(t, dir)
			if version, err := store.SchemaVersion(t.Context()); err != nil || version != 2 {
				t.Fatalf("SchemaVersion = %d, %v; want 2, nil", version, err)
			}
			if after := dumpRows(t, store.db, query); !slices.Equal(after, before) {
				t.Errorf("升级后 sqlite_sequence = %q; want %q", after, before)
			}
			got := recordReply(t, store, inbound("0000000001"))
			var inboundID int64
			if err := store.db.QueryRowContext(t.Context(), "SELECT inbound_id FROM replies WHERE seq = ?", got.Reply.Seq).Scan(&inboundID); err != nil {
				t.Fatalf("读取入站记录 id 失败: %v", err)
			}
			if got.Reply.Seq <= int64(tt.deleted) || inboundID <= int64(tt.deleted) {
				t.Errorf("升级后新行 seq = %d、入站 id = %d; want 都大于 %d", got.Reply.Seq, inboundID, tt.deleted)
			}
		})
	}
}
