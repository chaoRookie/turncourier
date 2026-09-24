// Package sqlite 的收取游标与被拒来信测试用临时目录中的真实 SQLite 数据库验证：游标在同一 UIDVALIDITY 下只进不退、
// UIDVALIDITY 变化时重置、各文件夹互不影响；被拒来信按 (账户, 文件夹, UIDVALIDITY, UID) 去重、字段校验先于事务、
// 错误文本不回显地址与 Message-ID、按时间升序分页查询；Go 端的全部原因码都满足数据库的形状约束；
// 按 UID 与按 Message-ID（只认 UIDVALIDITY 不同的记录）查找被拒记录、按时间分批清理，以及导出的 Message-ID 与发件人校验
// 与存储实际接受的取值一致。
package sqlite

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// invalidMessageIDs 是存储拒绝的 Message-ID：空、2 个字符（含多字节字符）、含 NUL、非法 UTF-8（非续字节与续字节、
// 超长编码）与 999 个字符。
var invalidMessageIDs = []string{"", "<a", "<é", "<a\x00b>", "<\xff\xfe>", "<\x80>", "\xc0\x80\xc0\x80", "<" + strings.Repeat("a", 997) + ">"}

// mustAdvanceCursor 把 botAccount 在 folder 中的游标写为 c，失败时终止测试。
func mustAdvanceCursor(t *testing.T, s *Store, folder string, c Cursor) {
	t.Helper()
	if err := s.AdvanceCursor(t.Context(), botAccount, folder, c); err != nil {
		t.Fatalf("AdvanceCursor(%s, %+v) 返回错误: %v", folder, c, err)
	}
}

// requireCursor 断言 botAccount 在 folder 中的游标为 want。
func requireCursor(t *testing.T, s *Store, folder string, want Cursor) {
	t.Helper()
	got, err := s.FetchCursor(t.Context(), botAccount, folder)
	if err != nil || got != want {
		t.Errorf("FetchCursor(%s) = %+v, %v; want %+v", folder, got, err, want)
	}
}

// cursorRows 按 (account, folder) 返回 fetch_cursors 的全部行（含 updated_at），用于断言失败的写入没有改动任何列。
func cursorRows(t *testing.T, s *Store) []string {
	t.Helper()
	return dumpRows(t, s.db, "SELECT account, folder, uid_validity, last_uid, updated_at FROM fetch_cursors ORDER BY account, folder")
}

// TestAdvanceCursor 按契约的顺序验证游标：没有记录时 ErrNotFound；(7, 10) 写入后读回；(7, 12) 前进；(7, 11) 返回
// ErrCursorRegression 且整行不变；(7, 12) 重复写入成功并刷新 updated_at；(8, 3) 与之后的 (7, 1) 因 UIDVALIDITY 不同而整体重置，
// 此后 (7, 0) 仍是回退；新 UIDVALIDITY 下 LastUID 可以为 0 并可重复写入；UIDVALIDITY 与 UID 可取 uint32 最大值。
func TestAdvanceCursor(t *testing.T) {
	store, clock := openTaskStore(t, nil)
	if got, err := store.FetchCursor(t.Context(), botAccount, "INBOX"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("没有记录时 FetchCursor = %+v, %v; want ErrNotFound", got, err)
	}

	mustAdvanceCursor(t, store, "INBOX", Cursor{UIDValidity: 7, LastUID: 10})
	requireCursor(t, store, "INBOX", Cursor{UIDValidity: 7, LastUID: 10})
	*clock = clock.Add(time.Second)
	mustAdvanceCursor(t, store, "INBOX", Cursor{UIDValidity: 7, LastUID: 12})
	requireCursor(t, store, "INBOX", Cursor{UIDValidity: 7, LastUID: 12})

	before := cursorRows(t, store)
	*clock = clock.Add(time.Second)
	if err := store.AdvanceCursor(t.Context(), botAccount, "INBOX", Cursor{UIDValidity: 7, LastUID: 11}); !errors.Is(err, ErrCursorRegression) {
		t.Fatalf("(7, 11) 返回 %v; want ErrCursorRegression", err)
	}
	if after := cursorRows(t, store); !slices.Equal(after, before) {
		t.Errorf("回退被拒后游标行 = %q; want 不变 %q", after, before)
	}
	requireCursor(t, store, "INBOX", Cursor{UIDValidity: 7, LastUID: 12})

	*clock = clock.Add(time.Second)
	mustAdvanceCursor(t, store, "INBOX", Cursor{UIDValidity: 7, LastUID: 12})
	requireCursor(t, store, "INBOX", Cursor{UIDValidity: 7, LastUID: 12})
	want := []string{botAccount + "|INBOX|7|12|" + strconv.FormatInt(clock.UnixMilli(), 10)}
	if got := cursorRows(t, store); !slices.Equal(got, want) {
		t.Errorf("重复写入后游标行 = %q; want %q（updated_at 为本次写入的时间）", got, want)
	}

	mustAdvanceCursor(t, store, "INBOX", Cursor{UIDValidity: 8, LastUID: 3})
	requireCursor(t, store, "INBOX", Cursor{UIDValidity: 8, LastUID: 3})
	mustAdvanceCursor(t, store, "INBOX", Cursor{UIDValidity: 7, LastUID: 1})
	requireCursor(t, store, "INBOX", Cursor{UIDValidity: 7, LastUID: 1})
	if err := store.AdvanceCursor(t.Context(), botAccount, "INBOX", Cursor{UIDValidity: 7, LastUID: 0}); !errors.Is(err, ErrCursorRegression) {
		t.Errorf("(7, 0) 返回 %v; want ErrCursorRegression", err)
	}
	mustAdvanceCursor(t, store, "INBOX", Cursor{UIDValidity: 9, LastUID: 0})
	mustAdvanceCursor(t, store, "INBOX", Cursor{UIDValidity: 9, LastUID: 0})
	requireCursor(t, store, "INBOX", Cursor{UIDValidity: 9, LastUID: 0})
	if err := store.AdvanceCursor(t.Context(), botAccount, "INBOX", Cursor{UIDValidity: 4294967295, LastUID: 4294967295}); err != nil {
		t.Errorf("最大 UIDVALIDITY 与 UID 返回 %v; want nil", err)
	}
	requireCursor(t, store, "INBOX", Cursor{UIDValidity: 4294967295, LastUID: 4294967295})
}

// TestCursorFoldersIndependent 验证游标按 (账户, 文件夹) 区分：INBOX 与 Junk、同一文件夹的两个账户各自前进、回退与重置，
// 互不影响；读取时两个文件夹各得到自己的游标。
func TestCursorFoldersIndependent(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	const other = "other@example.invalid"
	mustAdvanceCursor(t, store, "INBOX", Cursor{UIDValidity: 7, LastUID: 10})
	mustAdvanceCursor(t, store, "Junk", Cursor{UIDValidity: 9, LastUID: 2})
	if err := store.AdvanceCursor(t.Context(), other, "INBOX", Cursor{UIDValidity: 5, LastUID: 1}); err != nil {
		t.Fatalf("另一账户写入返回 %v", err)
	}
	requireCursor(t, store, "INBOX", Cursor{UIDValidity: 7, LastUID: 10})
	requireCursor(t, store, "Junk", Cursor{UIDValidity: 9, LastUID: 2})

	// Junk 的游标小于 INBOX 的 LastUID，只与 Junk 自己比较，因此照常前进；INBOX 的回退不受 Junk 影响。
	mustAdvanceCursor(t, store, "Junk", Cursor{UIDValidity: 9, LastUID: 3})
	if err := store.AdvanceCursor(t.Context(), botAccount, "INBOX", Cursor{UIDValidity: 7, LastUID: 9}); !errors.Is(err, ErrCursorRegression) {
		t.Errorf("INBOX (7, 9) 返回 %v; want ErrCursorRegression", err)
	}
	mustAdvanceCursor(t, store, "INBOX", Cursor{UIDValidity: 8, LastUID: 1})
	requireCursor(t, store, "INBOX", Cursor{UIDValidity: 8, LastUID: 1})
	requireCursor(t, store, "Junk", Cursor{UIDValidity: 9, LastUID: 3})
	if got, err := store.FetchCursor(t.Context(), other, "INBOX"); err != nil || got != (Cursor{UIDValidity: 5, LastUID: 1}) {
		t.Errorf("另一账户 FetchCursor = %+v, %v; want {5 1}", got, err)
	}
	if got, err := store.FetchCursor(t.Context(), other, "Junk"); !errors.Is(err, ErrNotFound) {
		t.Errorf("另一账户的 Junk FetchCursor = %+v, %v; want ErrNotFound", got, err)
	}
}

// TestCursorValidation 验证账户为空、过短、过长或含空白，文件夹为空、256 个字符或含 NUL，账户或文件夹含非法 UTF-8，
// UIDVALIDITY 为 0 时，AdvanceCursor 在开始事务前返回包装 ErrInvalidArgument 的错误（上下文已取消仍返回校验错误）且不写入；
// FetchCursor 对同样的账户与文件夹报同样的错。
// 非法 UTF-8 的账户 "\xc0\x80\xc0\x80" 按 Go 计为 4 个字符、按 SQLite 的 length() 计为 2 个，须由 Go 端拒绝而不是留给 CHECK 约束。
// 文件夹含空格、恰为 255 个字符（含非 ASCII 字符，按字符计）时可以写入与读取。
func TestCursorValidation(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	valid := Cursor{UIDValidity: 7, LastUID: 1}
	tests := []struct {
		name            string
		account, folder string
		cursor          Cursor
	}{
		{"账户为空", "", "INBOX", valid},
		{"账户少于 3 个字符", "a@", "INBOX", valid},
		{"账户超过 254 个字符", strings.Repeat("a", 243) + "@example.com", "INBOX", valid},
		{"账户含空格", "bot @example.invalid", "INBOX", valid},
		{"账户末尾换行", botAccount + "\n", "INBOX", valid},
		{"账户含 NUL", "bot\x00@example.invalid", "INBOX", valid},
		{"文件夹为空", botAccount, "", valid},
		{"文件夹为 256 个字符", botAccount, strings.Repeat("f", 256), valid},
		{"文件夹含 NUL", botAccount, "IN\x00BOX", valid},
		{"账户含非法 UTF-8", "\xc0\x80\xc0\x80", "INBOX", valid},
		{"文件夹含非法 UTF-8", botAccount, "IN\xffBOX", valid},
		{"UIDVALIDITY 为 0", botAccount, "INBOX", Cursor{UIDValidity: 0, LastUID: 1}},
	}
	for _, tt := range tests {
		err := store.AdvanceCursor(ctx, tt.account, tt.folder, tt.cursor)
		if err == nil || !strings.Contains(err.Error(), "invalid fetch cursor") || !errors.Is(err, ErrInvalidArgument) || errors.Is(err, context.Canceled) {
			t.Errorf("%s: AdvanceCursor 返回 %v; want 包装 ErrInvalidArgument、含 \"invalid fetch cursor\" 的错误而不是 context.Canceled", tt.name, err)
		}
		if tt.cursor.UIDValidity == 0 {
			continue
		}
		if got, err := store.FetchCursor(ctx, tt.account, tt.folder); err == nil || !strings.Contains(err.Error(), "invalid fetch cursor") ||
			!errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: FetchCursor = %+v, %v; want 包装 ErrInvalidArgument、含 \"invalid fetch cursor\" 的错误", tt.name, got, err)
		}
	}
	if got := countRows(t, store.db, "fetch_cursors"); got != 0 {
		t.Errorf("校验失败后 fetch_cursors 有 %d 行; want 0", got)
	}

	for _, folder := range []string{"Sent Messages", strings.Repeat("f", 255), strings.Repeat("垃", 255), "J"} {
		mustAdvanceCursor(t, store, folder, valid)
		requireCursor(t, store, folder, valid)
	}
	for _, account := range []string{"a@b", strings.Repeat("a", 242) + "@example.com"} {
		if err := store.AdvanceCursor(t.Context(), account, "INBOX", valid); err != nil {
			t.Errorf("账户边界值 %d 个字符: AdvanceCursor 返回 %v", len(account), err)
		}
	}
}

// rejectionFor 返回一条合成被拒来信：botAccount、INBOX、UIDVALIDITY 7、给定 UID、Message-ID <r<uid>@example.invalid>、
// 发件人 user@example.invalid、原因 thread_mismatch，不关联任务；测试按需修改字段。
func rejectionFor(uid uint32) Rejection {
	return Rejection{
		Account:     botAccount,
		Folder:      "INBOX",
		UIDValidity: 7,
		UID:         uid,
		MessageID:   "<r" + strconv.FormatUint(uint64(uid), 10) + "@example.invalid>",
		Sender:      "user@example.invalid",
		Reason:      RejectThreadMismatch,
	}
}

// mustRecordRejection 写入被拒来信并断言它是新记录，返回新 ID；失败时终止测试。
func mustRecordRejection(t *testing.T, s *Store, r Rejection) int64 {
	t.Helper()
	id, duplicate, err := s.RecordRejection(t.Context(), r)
	if err != nil || duplicate || id <= 0 {
		t.Fatalf("RecordRejection(uid %d) = %d, %t, %v; want 新 ID、false、nil", r.UID, id, duplicate, err)
	}
	return id
}

// mustRejections 查询被拒来信，失败时终止测试。
func mustRejections(t *testing.T, s *Store, since time.Time, limit int) []Rejection {
	t.Helper()
	got, err := s.Rejections(t.Context(), since, limit)
	if err != nil {
		t.Fatalf("Rejections(%v, %d) 返回错误: %v", since, limit, err)
	}
	return got
}

// TestRecordRejection 验证被拒来信的写入与去重：首次写入返回新 ID 与 false，全部字段按原样读回，ReceivedAt 取存储时钟、
// ID 由数据库分配（输入中的 ID 与 ReceivedAt 被忽略）；同一 (账户, 文件夹, UIDVALIDITY, UID) 再次写入返回同一 ID 与 true，
// 其余字段不同也不覆盖；四个键中任一不同都是新记录；Message-ID、发件人与任务可为空。
func TestRecordRejection(t *testing.T) {
	store, clock := openTaskStore(t, nil)
	linked := createTask(t, store)

	first := rejectionFor(1)
	first.TaskID = linked.ID
	first.ID, first.ReceivedAt = 999, clock.Add(-time.Hour)
	id := mustRecordRejection(t, store, first)
	first.ID, first.ReceivedAt = id, clock.UTC()
	if got := mustRejections(t, store, time.Time{}, 10); !slices.Equal(got, []Rejection{first}) {
		t.Fatalf("Rejections = %+v; want %+v", got, []Rejection{first})
	}

	*clock = clock.Add(time.Minute)
	again := rejectionFor(1)
	again.MessageID, again.Sender, again.Reason = "<other@example.invalid>", "other@example.invalid", RejectBounce
	if gotID, duplicate, err := store.RecordRejection(t.Context(), again); gotID != id || !duplicate || err != nil {
		t.Errorf("再次写入同一邮件 = %d, %t, %v; want %d, true, nil", gotID, duplicate, err, id)
	}
	if got := mustRejections(t, store, time.Time{}, 10); !slices.Equal(got, []Rejection{first}) {
		t.Errorf("再次写入后 Rejections = %+v; want 原记录不变 %+v", got, []Rejection{first})
	}

	others := []Rejection{rejectionFor(2), rejectionFor(1), rejectionFor(1), rejectionFor(1)}
	others[1].Folder = "Junk"
	others[2].UIDValidity = 8
	others[3].Account = "other@example.invalid"
	ids := map[int64]bool{id: true}
	for _, r := range others {
		ids[mustRecordRejection(t, store, r)] = true
	}
	if len(ids) != 5 {
		t.Errorf("五封不同键的邮件得到 %d 个不同 ID; want 5", len(ids))
	}

	bare := rejectionFor(3)
	bare.MessageID, bare.Sender = "", ""
	bare.ID = mustRecordRejection(t, store, bare)
	bare.ReceivedAt = clock.UTC()
	got := mustRejections(t, store, time.Time{}, 10)
	if len(got) != 6 || got[5] != bare {
		t.Errorf("Rejections 的最后一条 = %+v; want Message-ID、发件人与任务为空的 %+v", got, bare)
	}
	if got := dumpRows(t, store.db, "SELECT message_id IS NULL, sender IS NULL, task_id IS NULL FROM inbound_rejections WHERE id = ?", bare.ID); !slices.Equal(got, []string{"1|1|1"}) {
		t.Errorf("空的 Message-ID、发件人与任务写入为 %q; want 都为 NULL", got)
	}
}

// TestRecordRejectionUnknownTask 验证 TaskID 不存在时返回 ErrNotFound 且不写入；已有记录的邮件再次写入时按重复返回，
// 不再检查任务。
func TestRecordRejectionUnknownTask(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	r := rejectionFor(1)
	r.TaskID = "0000000000"
	if id, duplicate, err := store.RecordRejection(t.Context(), r); !errors.Is(err, ErrNotFound) || id != 0 || duplicate {
		t.Errorf("TaskID 不存在: RecordRejection = %d, %t, %v; want 0, false, ErrNotFound", id, duplicate, err)
	}
	if got := countRows(t, store.db, "inbound_rejections"); got != 0 {
		t.Errorf("inbound_rejections 有 %d 行; want 0", got)
	}
	id := mustRecordRejection(t, store, rejectionFor(1))
	if gotID, duplicate, err := store.RecordRejection(t.Context(), r); gotID != id || !duplicate || err != nil {
		t.Errorf("已有记录的邮件带不存在的 TaskID = %d, %t, %v; want %d, true, nil", gotID, duplicate, err, id)
	}
}

// TestRecordRejectionValidation 验证原因码不在列表中（包括形状合法的未知原因码）、各字段长度、空白、NUL、非法 UTF-8 或取值范围
// 不符时，RecordRejection 在开始事务前返回包装 ErrInvalidArgument 的错误（上下文已取消仍返回校验错误）且不写入，错误文本不含地址、
// Message-ID 与任务 ID；任务 ID 须为空或任务 ID 的形式；边界值可以写入。
func TestRecordRejectionValidation(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	tests := []struct {
		name   string
		modify func(*Rejection)
	}{
		{"原因码为 Bad", func(r *Rejection) { r.Reason = "Bad" }},
		{"原因码为空", func(r *Rejection) { r.Reason = "" }},
		{"原因码形状合法但不在列表中", func(r *Rejection) { r.Reason = "synthetic" }},
		{"账户为空", func(r *Rejection) { r.Account = "" }},
		{"账户含空格", func(r *Rejection) { r.Account = "bot @example.invalid" }},
		{"账户超过 254 个字符", func(r *Rejection) { r.Account = strings.Repeat("a", 243) + "@example.com" }},
		{"账户含 NUL", func(r *Rejection) { r.Account = "bot\x00@example.invalid" }},
		{"文件夹为空", func(r *Rejection) { r.Folder = "" }},
		{"文件夹为 256 个字符", func(r *Rejection) { r.Folder = strings.Repeat("f", 256) }},
		{"文件夹含 NUL", func(r *Rejection) { r.Folder = "IN\x00BOX" }},
		{"UIDVALIDITY 为 0", func(r *Rejection) { r.UIDValidity = 0 }},
		{"UID 为 0", func(r *Rejection) { r.UID = 0 }},
		{"Message-ID 为 2 个字符", func(r *Rejection) { r.MessageID = "<a" }},
		{"Message-ID 为 2 个非 ASCII 字符", func(r *Rejection) { r.MessageID = "<é" }},
		{"Message-ID 超过 998 个字符", func(r *Rejection) { r.MessageID = "<" + strings.Repeat("a", 997) + ">" }},
		{"Message-ID 含 NUL", func(r *Rejection) { r.MessageID = "<a\x00b@example.invalid>" }},
		{"Message-ID 含非法 UTF-8", func(r *Rejection) { r.MessageID = "\xc0\x80\xc0\x80" }},
		{"发件人为 2 个字符", func(r *Rejection) { r.Sender = "u@" }},
		{"发件人超过 254 个字符", func(r *Rejection) { r.Sender = strings.Repeat("u", 243) + "@example.com" }},
		{"发件人含空格", func(r *Rejection) { r.Sender = "user @example.invalid" }},
		{"发件人含 NUL", func(r *Rejection) { r.Sender = "user\x00@example.invalid" }},
		{"发件人含非法 UTF-8", func(r *Rejection) { r.Sender = "\xc0\x80\xc0\x80" }},
		{"任务不存在时仍先校验字段", func(r *Rejection) { r.TaskID, r.UID = "0000000000", 0 }},
		{"任务 ID 含大写字母", func(r *Rejection) { r.TaskID = "000000000A" }},
		{"任务 ID 形似地址", func(r *Rejection) { r.TaskID = "attacker@example.invalid" }},
	}
	for _, tt := range tests {
		r := rejectionFor(1)
		tt.modify(&r)
		id, duplicate, err := store.RecordRejection(ctx, r)
		if err == nil || !strings.Contains(err.Error(), "invalid rejection") || !errors.Is(err, ErrInvalidArgument) || errors.Is(err, context.Canceled) ||
			id != 0 || duplicate {
			t.Errorf("%s: RecordRejection = %d, %t, %v; want 包装 ErrInvalidArgument、含 \"invalid rejection\" 的错误而不是 context.Canceled",
				tt.name, id, duplicate, err)
			continue
		}
		for _, value := range []string{r.Account, r.MessageID, r.Sender, r.TaskID} {
			if len(value) >= 3 && strings.Contains(err.Error(), value) {
				t.Errorf("%s: 错误文本 %q 回显了 %q", tt.name, err, value)
			}
		}
	}
	if got := countRows(t, store.db, "inbound_rejections"); got != 0 {
		t.Errorf("校验失败后 inbound_rejections 有 %d 行; want 0", got)
	}

	boundaries := []func(*Rejection){
		func(r *Rejection) { r.MessageID, r.Sender = "<a>", "a@b" },
		func(r *Rejection) { r.MessageID = "<" + strings.Repeat("b", 996) + ">" },
		func(r *Rejection) { r.Sender = strings.Repeat("u", 242) + "@example.com" },
		func(r *Rejection) { r.Folder = "Sent Messages" },
		func(r *Rejection) { r.Folder = strings.Repeat("垃", 255) },
		func(r *Rejection) { r.UIDValidity, r.UID = 4294967295, 4294967295 },
		func(r *Rejection) { r.Account = "a@b" },
	}
	for i, modify := range boundaries {
		r := rejectionFor(uint32(10 + i))
		modify(&r)
		mustRecordRejection(t, store, r)
	}
}

// TestRejectionsOrderAndLimit 验证 Rejections 按 (received_at, id) 升序返回（写入顺序与时间顺序不同，同一毫秒按 ID），
// 只返回 received_at 晚于 since 的记录（恰在 since 的不返回），limit 生效且须为 1–1000，越界时返回包装 ErrInvalidArgument 的错误。
func TestRejectionsOrderAndLimit(t *testing.T) {
	store, clock := openTaskStore(t, nil)
	start := *clock
	at := func(offset time.Duration, uid uint32) Rejection {
		*clock = start.Add(offset)
		r := rejectionFor(uid)
		r.ID = mustRecordRejection(t, store, r)
		r.ReceivedAt = clock.UTC()
		return r
	}
	a := at(2*time.Second, 1)
	b := at(0, 2)
	c := at(0, 3)
	d := at(time.Second, 4)

	tests := []struct {
		name  string
		since time.Time
		limit int
		want  []Rejection
	}{
		{"全部", time.Time{}, 1000, []Rejection{b, c, d, a}},
		{"limit 2", time.Time{}, 2, []Rejection{b, c}},
		{"limit 1", time.Time{}, 1, []Rejection{b}},
		{"since 早 1 毫秒", start.Add(-time.Millisecond), 1000, []Rejection{b, c, d, a}},
		{"since 恰为 b 与 c 的时间", start, 1000, []Rejection{d, a}},
		{"since 为 d 的时间", start.Add(time.Second), 1, []Rejection{a}},
		{"since 晚于全部", start.Add(time.Hour), 1000, nil},
	}
	for _, tt := range tests {
		if got := mustRejections(t, store, tt.since, tt.limit); !slices.Equal(got, tt.want) {
			t.Errorf("%s: Rejections = %+v; want %+v", tt.name, got, tt.want)
		}
	}
	for _, limit := range []int{0, -1, 1001} {
		if got, err := store.Rejections(t.Context(), time.Time{}, limit); err == nil || got != nil || !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("limit %d: Rejections = %+v, %v; want 包装 ErrInvalidArgument 的错误", limit, got, err)
		}
	}
}

// TestRejectReasonsMatchDatabase 验证 Go 端的原因码列表恰为 4a 的 13 个与 4b 增补的 7 个常量，4b 增补的常量取值与契约逐字相同，
// 且逐个经 RecordRejection 写入并按原样读回，即每个都满足 inbound_rejections.reason 的形状约束（1–40 个 [a-z_] 字符）；
// 以后增补原因码时本测试随之要求更新。未知原因码被拒绝见 TestRecordRejectionValidation。
func TestRejectReasonsMatchDatabase(t *testing.T) {
	added := map[RejectReason]string{
		RejectSubjectTagMissing:  "subject_tag_missing",
		RejectSubjectTagMultiple: "subject_tag_multiple",
		RejectSubjectTagDamaged:  "subject_tag_damaged",
		RejectTokenSuperseded:    "token_superseded",
		RejectRateLimited:        "rate_limited",
		RejectMailPaused:         "mail_paused",
		RejectEmptyBody:          "empty_body",
	}
	for reason, literal := range added {
		if string(reason) != literal {
			t.Errorf("原因码常量 = %q; want %q", reason, literal)
		}
	}
	want := []RejectReason{
		RejectAutoReply, RejectBounce, RejectSenderNotAllowed, RejectTooLarge, RejectMalformed, RejectParseUncertain,
		RejectThreadMismatch, RejectSubjectTag, RejectTokenMissing, RejectTokenInvalid, RejectTokenExpired, RejectTaskUnknown,
		RejectMessageConflict, RejectSubjectTagMissing, RejectSubjectTagMultiple, RejectSubjectTagDamaged, RejectTokenSuperseded,
		RejectRateLimited, RejectMailPaused, RejectEmptyBody,
	}
	known := slices.Sorted(maps.Keys(knownRejectReasons))
	if sortedWant := slices.Sorted(slices.Values(want)); !slices.Equal(known, sortedWant) {
		t.Errorf("knownRejectReasons = %q; want %q", known, sortedWant)
	}
	store, _ := openTaskStore(t, nil)
	for i, reason := range want {
		r := rejectionFor(uint32(i + 1))
		r.Reason = reason
		if _, _, err := store.RecordRejection(t.Context(), r); err != nil {
			t.Errorf("原因码 %q 写入失败: %v", reason, err)
		}
	}
	got := mustRejections(t, store, time.Time{}, 1000)
	if len(got) != len(want) {
		t.Fatalf("写入 %d 条; want %d", len(got), len(want))
	}
	for i, r := range got {
		if r.Reason != want[i] {
			t.Errorf("第 %d 条原因码读回 %q; want %q", i, r.Reason, want[i])
		}
	}
}

// TestMailboxCanceledContext 验证输入合法而上下文已取消时，游标与被拒来信的各方法都返回 context.Canceled（不是
// ErrInvalidArgument），且不写入任何行。
func TestMailboxCanceledContext(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := map[string]func() error{
		"FetchCursor":     func() error { _, err := store.FetchCursor(ctx, botAccount, "INBOX"); return err },
		"AdvanceCursor":   func() error { return store.AdvanceCursor(ctx, botAccount, "INBOX", Cursor{UIDValidity: 7, LastUID: 1}) },
		"RecordRejection": func() error { _, _, err := store.RecordRejection(ctx, rejectionFor(1)); return err },
		"Rejections":      func() error { _, err := store.Rejections(ctx, time.Time{}, 10); return err },
		"RejectionExists": func() error { _, err := store.RejectionExists(ctx, botAccount, "INBOX", 7, 1); return err },
		"RejectedBeforeReset": func() error {
			_, err := store.RejectedBeforeReset(ctx, botAccount, "INBOX", 7, "<r1@example.invalid>")
			return err
		},
		"PruneRejections": func() error { _, err := store.PruneRejections(ctx, time.UnixMilli(1)); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, context.Canceled) || errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v; want context.Canceled", name, err)
		}
	}
	for _, table := range []string{"fetch_cursors", "inbound_rejections"} {
		if got := countRows(t, store.db, table); got != 0 {
			t.Errorf("%s 有 %d 行; want 0", table, got)
		}
	}
}

// TestRejectionExists 验证按 (账户, 文件夹, UIDVALIDITY, UID) 判断被拒记录是否存在：四个键全部相同才为真，任一不同为假。
func TestRejectionExists(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	mustRecordRejection(t, store, rejectionFor(1))
	for _, tt := range []struct {
		name            string
		account, folder string
		uidValidity     uint32
		uid             uint32
		want            bool
	}{
		{"同一封", botAccount, "INBOX", 7, 1, true},
		{"另一 UID", botAccount, "INBOX", 7, 2, false},
		{"另一文件夹", botAccount, "Junk", 7, 1, false},
		{"另一 UIDVALIDITY", botAccount, "INBOX", 8, 1, false},
		{"另一账户", "other@example.invalid", "INBOX", 7, 1, false},
	} {
		if got, err := store.RejectionExists(t.Context(), tt.account, tt.folder, tt.uidValidity, tt.uid); err != nil || got != tt.want {
			t.Errorf("%s: RejectionExists = %t, %v; want %t", tt.name, got, err, tt.want)
		}
	}
}

// TestRejectedBeforeReset 验证按 Message-ID 认出 UIDVALIDITY 重置前被拒的来信：同一账户与文件夹中 Message-ID 相同、UIDVALIDITY
// 不同的被拒记录才算；UIDVALIDITY 相同的不算（伪造者不能借一条被拒记录挡掉同一 UIDVALIDITY 下 Message-ID 相同的合法回复），
// 另一文件夹、另一账户与另一 Message-ID 也不算。同一 Message-ID 在新 UIDVALIDITY 下再被拒绝后，查询任一 UIDVALIDITY 都为真。
// 查询使用 0003 的 inbound_rejections_by_message 索引。
func TestRejectedBeforeReset(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	ctx := t.Context()
	const rejected = "<r1@example.invalid>"
	mustRecordRejection(t, store, rejectionFor(1))
	bare := rejectionFor(2)
	bare.MessageID = ""
	mustRecordRejection(t, store, bare)
	// check 断言 RejectedBeforeReset(account, folder, uidValidity, messageID) 的结果为 want。
	check := func(name, account, folder string, uidValidity uint32, messageID string, want bool) {
		t.Helper()
		if got, err := store.RejectedBeforeReset(ctx, account, folder, uidValidity, messageID); err != nil || got != want {
			t.Errorf("%s: RejectedBeforeReset = %t, %v; want %t", name, got, err, want)
		}
	}
	check("UIDVALIDITY 不同", botAccount, "INBOX", 8, rejected, true)
	check("UIDVALIDITY 相同", botAccount, "INBOX", 7, rejected, false)
	check("另一文件夹", botAccount, "Junk", 8, rejected, false)
	check("另一账户", "other@example.invalid", "INBOX", 8, rejected, false)
	check("另一 Message-ID", botAccount, "INBOX", 8, "<r9@example.invalid>", false)

	reset := rejectionFor(5)
	reset.UIDValidity, reset.MessageID = 8, rejected
	mustRecordRejection(t, store, reset)
	check("重置后再被拒，查询旧 UIDVALIDITY", botAccount, "INBOX", 7, rejected, true)
	check("重置后再被拒，查询新 UIDVALIDITY", botAccount, "INBOX", 8, rejected, true)
	check("第三个 UIDVALIDITY", botAccount, "INBOX", 9, rejected, true)

	plan := dumpRows(t, store.db, "EXPLAIN QUERY PLAN "+rejectedBeforeResetQuery, botAccount, "INBOX", rejected, 9)
	if !slices.ContainsFunc(plan, func(row string) bool { return strings.Contains(row, "USING INDEX inbound_rejections_by_message") }) {
		t.Errorf("RejectedBeforeReset 的查询计划 = %q; want 使用 inbound_rejections_by_message", plan)
	}
}

// TestPruneRejections 验证清理：只删除 received_at 早于 before 的被拒记录（按毫秒，恰在 before 的保留），返回删除条数，
// 没有可删的记录时返回 0；一次至多删除 10000 条且先删最早的，多出的留给下一次。
func TestPruneRejections(t *testing.T) {
	store, clock := openTaskStore(t, nil)
	start := *clock
	for i, offset := range []time.Duration{-time.Millisecond, 0, time.Millisecond} {
		*clock = start.Add(offset)
		mustRecordRejection(t, store, rejectionFor(uint32(i+1)))
	}
	// prune 调用 PruneRejections(before)，断言删除 want 条，且剩余记录的 UID 依次为 remaining。
	prune := func(t *testing.T, s *Store, step string, before time.Time, want int, remaining ...string) {
		t.Helper()
		if got, err := s.PruneRejections(t.Context(), before); err != nil || got != want {
			t.Errorf("%s: PruneRejections = %d, %v; want %d", step, got, err, want)
		}
		if got := dumpRows(t, s.db, "SELECT uid FROM inbound_rejections ORDER BY uid"); !slices.Equal(got, remaining) {
			t.Errorf("%s 之后剩余 UID = %q; want %q", step, got, remaining)
		}
	}
	prune(t, store, "早于 before 的一条", start, 1, "2", "3")
	prune(t, store, "再次清理", start, 0, "2", "3")
	prune(t, store, "晚 2 毫秒", start.Add(2*time.Millisecond), 2)
	prune(t, store, "空表", start.Add(time.Hour), 0)

	t.Run("一次至多 10000 条", func(t *testing.T) {
		store, _ := openTaskStore(t, nil)
		if _, err := store.db.ExecContext(t.Context(),
			"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 10001) "+
				"INSERT INTO inbound_rejections (account, folder, uid_validity, uid, reason, received_at) SELECT ?, 'INBOX', 7, i, 'malformed', 1000 + i FROM n",
			botAccount); err != nil {
			t.Fatalf("插入 10001 条被拒记录失败: %v", err)
		}
		before := time.UnixMilli(1_000_000)
		prune(t, store, "第一次", before, 10000, "10001")
		prune(t, store, "第二次", before, 1)
		prune(t, store, "第三次", before, 0)
	})
}

// TestValidMessageIDAndSender 验证导出的校验函数与存储实际接受的取值一致。Message-ID：3 个字符（含多字节字符）与 998 个字符
// （含多字节字符）合法，2 与 999 个字符、NUL 与非法 UTF-8 不合法；ValidMessageID 为真当且仅当 RecordRejection 与 RecordReply
// 接受它，且 RecordDeliveredMessageID、ResolveUncertainAsDelivered、NotificationByMessageID、NotificationByDeliveredID、
// InboundByMessageID 与 RejectedBeforeReset 不以 ErrInvalidArgument 拒绝它。发件人：3 与 254 个字符（含多字节字符）合法，
// 2 与 255 个字符、空白（含全角空格）、NUL 与非法 UTF-8 不合法；ValidSender 为真当且仅当 RecordRejection 接受它。
// 空字符串对两者都不合法：被拒记录把空值当作缺失，不经这两条规则。
func TestValidMessageIDAndSender(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	ctx := t.Context()
	running := startTask(t, store)
	pending, err := store.CreateNotification(ctx, notificationFor(running.ID, "validity"))
	if err != nil {
		t.Fatalf("CreateNotification 返回错误: %v", err)
	}
	if ValidMessageID("") || ValidSender("") {
		t.Errorf("ValidMessageID(\"\") = %t, ValidSender(\"\") = %t; want 都为 false", ValidMessageID(""), ValidSender(""))
	}
	messageIDs := []struct {
		id    string
		valid bool
	}{
		{"<a>", true},
		{"<é>", true},
		{strings.Repeat("a", 998), true},
		{"<" + strings.Repeat("é", 996) + ">", true},
		{"<" + strings.Repeat("é", 997) + ">", false},
	}
	for _, id := range invalidMessageIDs[1:] {
		messageIDs = append(messageIDs, struct {
			id    string
			valid bool
		}{id, false})
	}
	uid := uint32(0)
	for _, tt := range messageIDs {
		if got := ValidMessageID(tt.id); got != tt.valid {
			t.Errorf("ValidMessageID(%q) = %t; want %t", tt.id, got, tt.valid)
		}
		uid++
		rejection := rejectionFor(uid)
		rejection.MessageID = tt.id
		_, _, rejectionErr := store.RecordRejection(ctx, rejection)
		reply := withBody(inbound(running.ID), "synthetic reply "+strconv.Itoa(int(uid)))
		reply.UID, reply.MessageID = uid, tt.id
		_, replyErr := store.RecordReply(ctx, reply)
		for name, err := range map[string]error{"RecordRejection": rejectionErr, "RecordReply": replyErr} {
			if (err == nil) != tt.valid || (err != nil && !errors.Is(err, ErrInvalidArgument)) {
				t.Errorf("%s 对 %q 返回 %v; want 接受 = %t，拒绝时包装 ErrInvalidArgument", name, tt.id, err, tt.valid)
			}
		}
		_, deliveredErr := store.RecordDeliveredMessageID(ctx, pending.ID, tt.id)
		_, resolveErr := store.ResolveUncertainAsDelivered(ctx, pending.ID, tt.id)
		_, byMessageErr := store.NotificationByMessageID(ctx, tt.id)
		_, byDeliveredErr := store.NotificationByDeliveredID(ctx, tt.id)
		_, inboundErr := store.InboundByMessageID(ctx, botAccount, tt.id)
		_, resetErr := store.RejectedBeforeReset(ctx, botAccount, "INBOX", 9, tt.id)
		for name, err := range map[string]error{
			"RecordDeliveredMessageID": deliveredErr, "ResolveUncertainAsDelivered": resolveErr, "NotificationByMessageID": byMessageErr,
			"NotificationByDeliveredID": byDeliveredErr, "InboundByMessageID": inboundErr, "RejectedBeforeReset": resetErr,
		} {
			if errors.Is(err, ErrInvalidArgument) == tt.valid {
				t.Errorf("%s 对 %q 返回 %v; want 以 ErrInvalidArgument 拒绝 = %t", name, tt.id, err, !tt.valid)
			}
		}
	}

	for _, tt := range []struct {
		sender string
		valid  bool
	}{
		{"a@b", true},
		{strings.Repeat("u", 242) + "@example.com", true},
		{strings.Repeat("é", 242) + "@example.com", true},
		{"u@", false},
		{strings.Repeat("u", 243) + "@example.com", false},
		{"user @example.invalid", false},
		{"user\t@example.invalid", false},
		{"user　@example.invalid", false},
		{"user\x00@example.invalid", false},
		{"\xc0\x80\xc0\x80", false},
		{"user\xff@example.invalid", false},
	} {
		if got := ValidSender(tt.sender); got != tt.valid {
			t.Errorf("ValidSender(%q) = %t; want %t", tt.sender, got, tt.valid)
		}
		uid++
		rejection := rejectionFor(uid)
		rejection.Sender = tt.sender
		if _, _, err := store.RecordRejection(ctx, rejection); (err == nil) != tt.valid || (err != nil && !errors.Is(err, ErrInvalidArgument)) {
			t.Errorf("RecordRejection 对发件人 %q 返回 %v; want 接受 = %t，拒绝时包装 ErrInvalidArgument", tt.sender, err, tt.valid)
		}
	}
}
