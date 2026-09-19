// Package sqlite 的收取游标与被拒来信测试用临时目录中的真实 SQLite 数据库验证：游标在同一 UIDVALIDITY 下只进不退、
// UIDVALIDITY 变化时重置、各文件夹互不影响；被拒来信按 (账户, 文件夹, UIDVALIDITY, UID) 去重、字段校验先于事务、
// 错误文本不回显地址与 Message-ID、按时间升序分页查询；Go 端的全部原因码都满足数据库的形状约束。
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
// UIDVALIDITY 为 0 时，AdvanceCursor 在开始事务前报错（上下文已取消仍返回校验错误）且不写入；FetchCursor 对同样的账户与文件夹报错。
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
		if err == nil || !strings.Contains(err.Error(), "invalid fetch cursor") || errors.Is(err, context.Canceled) {
			t.Errorf("%s: AdvanceCursor 返回 %v; want 含 \"invalid fetch cursor\" 的错误而不是 context.Canceled", tt.name, err)
		}
		if tt.cursor.UIDValidity == 0 {
			continue
		}
		if got, err := store.FetchCursor(ctx, tt.account, tt.folder); err == nil || !strings.Contains(err.Error(), "invalid fetch cursor") {
			t.Errorf("%s: FetchCursor = %+v, %v; want 含 \"invalid fetch cursor\" 的错误", tt.name, got, err)
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

// TestRecordRejectionValidation 验证原因码不在列表中、各字段长度、空白、NUL、非法 UTF-8 或取值范围不符时，RecordRejection
// 在开始事务前报错（上下文已取消仍返回校验错误）且不写入，错误文本不含地址与 Message-ID；边界值可以写入。
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
	}
	for _, tt := range tests {
		r := rejectionFor(1)
		tt.modify(&r)
		id, duplicate, err := store.RecordRejection(ctx, r)
		if err == nil || !strings.Contains(err.Error(), "invalid rejection") || errors.Is(err, context.Canceled) || id != 0 || duplicate {
			t.Errorf("%s: RecordRejection = %d, %t, %v; want 含 \"invalid rejection\" 的错误而不是 context.Canceled", tt.name, id, duplicate, err)
			continue
		}
		for _, value := range []string{r.Account, r.MessageID, r.Sender} {
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
// 只返回 received_at 晚于 since 的记录（恰在 since 的不返回），limit 生效且须为 1–1000。
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
		if got, err := store.Rejections(t.Context(), time.Time{}, limit); err == nil || got != nil {
			t.Errorf("limit %d: Rejections = %+v, %v; want 错误", limit, got, err)
		}
	}
}

// TestRejectReasonsMatchDatabase 验证 Go 端的原因码列表恰为契约列出的 13 个常量，且逐个经 RecordRejection 写入成功，
// 即每个都满足 inbound_rejections.reason 的形状约束（1–40 个 [a-z_] 字符）；4b 增补原因码时本测试随之要求更新。
func TestRejectReasonsMatchDatabase(t *testing.T) {
	want := []RejectReason{
		RejectAutoReply, RejectBounce, RejectSenderNotAllowed, RejectTooLarge, RejectMalformed, RejectParseUncertain,
		RejectThreadMismatch, RejectSubjectTag, RejectTokenMissing, RejectTokenInvalid, RejectTokenExpired, RejectTaskUnknown,
		RejectMessageConflict,
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

// TestMailboxCanceledContext 验证输入合法而上下文已取消时，游标与被拒来信的四个方法都返回 context.Canceled，且不写入任何行。
func TestMailboxCanceledContext(t *testing.T) {
	store, _ := openTaskStore(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := map[string]func() error{
		"FetchCursor":     func() error { _, err := store.FetchCursor(ctx, botAccount, "INBOX"); return err },
		"AdvanceCursor":   func() error { return store.AdvanceCursor(ctx, botAccount, "INBOX", Cursor{UIDValidity: 7, LastUID: 1}) },
		"RecordRejection": func() error { _, _, err := store.RecordRejection(ctx, rejectionFor(1)); return err },
		"Rejections":      func() error { _, err := store.Rejections(ctx, time.Time{}, 10); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, context.Canceled) {
			t.Errorf("%s: err = %v; want context.Canceled", name, err)
		}
	}
	for _, table := range []string{"fetch_cursors", "inbound_rejections"} {
		if got := countRows(t, store.db, table); got != 0 {
			t.Errorf("%s 有 %d 行; want 0", table, got)
		}
	}
}
