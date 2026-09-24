// Package sqlite 的待发通知测试用临时目录中的真实 SQLite 数据库验证通知的创建与加密、字段校验、领取顺序、
// 无法读出内容时不领取、投递结果、放弃、任务关闭前后的处理、即将过期的放弃、崩溃后的恢复、实际投递 Message-ID 的记录、
// 失败回滚、两个存储实例的串行领取，以及 4b 所需的查询：按 Message-ID 与实际投递 ID 查找、「最新通知」、核对为已送达不改变发出顺序、
// 线程引用、发信计数、放弃与缺少实际投递 ID 的列表及其游标分页。
package sqlite

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chaoRookie/turncourier/internal/queue"
	"github.com/chaoRookie/turncourier/internal/security/payload"
	"github.com/chaoRookie/turncourier/internal/task"
)

const (
	// notificationCanary 是合成通知内容中的金丝雀文本，用来断言内容不以明文落盘、不出现在错误文本中。
	notificationCanary = "NOTIFICATION-CANARY"
	// syntheticDeliveredID 是合成的实际投递 Message-ID，形如 QQ 改写后的 ID。
	syntheticDeliveredID = "<tencent_synthetic@example.invalid>"
)

// messageIDPattern 是我方 Message-ID 的形式：tc. 加 24 位小写 Crockford base32，右侧为测试使用的域名 example.invalid。
var messageIDPattern = regexp.MustCompile(`^<tc\.[0-9abcdefghjkmnpqrstvwxyz]{24}@example\.invalid>$`)

// testKeyCheck 是测试登记密钥时使用的 8 字节校验值；存储层不比对校验值。
var testKeyCheck = [8]byte{0x10, 0x21, 0x32, 0x43, 0x54, 0x65, 0x76, 0x87}

// newTestPayloadKey 返回密钥号为 kid 的测试正文密钥；密钥字节在运行时由 bytes.Repeat 构造，源码中没有密钥字面量。
func newTestPayloadKey(t *testing.T, kid uint8) *payload.Key {
	t.Helper()
	key, err := payload.NewKey(kid, bytes.Repeat([]byte{0x5a}, payload.KeyLen))
	if err != nil {
		t.Fatalf("构造测试正文密钥失败: %v", err)
	}
	return key
}

// testClock 返回位于 UTC+8、带毫秒的固定时刻，与 openTaskStore 相同，用来验证存储统一换算为 UTC 毫秒。
func testClock() *time.Time {
	now := time.Date(2026, 9, 18, 16, 30, 0, 123_000_000, time.FixedZone("UTC+8", 8*60*60))
	return &now
}

// openNotificationStore 在 dir 中打开存储：时钟读取 *clock，随机源为固定种子的 ChaCha8，正文密钥为 kid 1 的测试密钥；
// 测试结束时关闭，重复关闭的错误被忽略。同一种子的两个存储给出相同的随机字节，因此只能由其中一个创建任务与通知。
func openNotificationStore(t *testing.T, dir string, clock *time.Time) *Store {
	t.Helper()
	store, err := Open(t.Context(), dir, Options{
		Now:        func() time.Time { return *clock },
		Random:     rand.NewChaCha8([32]byte{}),
		PayloadKey: newTestPayloadKey(t, 1),
	})
	if err != nil {
		t.Fatalf("Open 返回错误: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// registerTestKeys 登记 (token, 1) 与 (payload, 1)，校验值为 testKeyCheck。
func registerTestKeys(t *testing.T, s *Store) {
	t.Helper()
	for _, purpose := range []KeyPurpose{KeyPurposeToken, KeyPurposePayload} {
		if err := s.RegisterKey(t.Context(), purpose, 1, testKeyCheck); err != nil {
			t.Fatalf("RegisterKey(%s, 1) 返回错误: %v", purpose, err)
		}
	}
}

// newNotificationStore 在新数据目录中以 openNotificationStore 打开存储，登记两把测试密钥并启动任务 T；
// 返回存储、时钟与 RUNNING 状态的任务 T。
func newNotificationStore(t *testing.T) (*Store, *time.Time, Task) {
	t.Helper()
	clock := testClock()
	store := openNotificationStore(t, dataDir(t), clock)
	registerTestKeys(t, store)
	return store, clock, startTask(t, store)
}

// notificationFor 返回任务 taskID 的一条合成通知输入：turn_completed、域名 example.invalid、有效期 168 小时，
// 内容含 label 与金丝雀文本，label 不同的通知内容不同。
func notificationFor(taskID, label string) NewNotification {
	return NewNotification{
		TaskID:  taskID,
		Event:   "turn_completed",
		Domain:  "example.invalid",
		TTL:     168 * time.Hour,
		Content: []byte("synthetic notification " + label + " " + notificationCanary),
	}
}

// mustCreateNotification 创建通知，失败时终止测试。
func mustCreateNotification(t *testing.T, s *Store, in NewNotification) Notification {
	t.Helper()
	created, err := s.CreateNotification(t.Context(), in)
	if err != nil {
		t.Fatalf("CreateNotification 返回错误: %v", err)
	}
	return created
}

// mustClaimNotification 领取下一条通知，失败时终止测试。
func mustClaimNotification(t *testing.T, s *Store) (Notification, []byte) {
	t.Helper()
	claimed, content, err := s.ClaimNextNotification(t.Context())
	if err != nil {
		t.Fatalf("ClaimNextNotification 返回错误: %v", err)
	}
	return claimed, content
}

// mustGetNotification 按 id 读取通知快照，失败时终止测试。
func mustGetNotification(t *testing.T, s *Store, id int64) Notification {
	t.Helper()
	n, err := getNotification(t.Context(), s.db, notificationByID, id)
	if err != nil {
		t.Fatalf("读取通知 %d 失败: %v", id, err)
	}
	return n
}

// must 返回断言通知操作成功的函数：调用方写作 must(t, name)(store.X(…))，把操作的两个返回值原样传入，失败时终止测试。
func must(t *testing.T, name string) func(Notification, error) Notification {
	return func(n Notification, err error) Notification {
		t.Helper()
		if err != nil {
			t.Fatalf("%s 返回错误: %v", name, err)
		}
		return n
	}
}

// allNotifications 按 id 返回全部通知快照，用于断言失败的操作没有改动通知。
func allNotifications(t *testing.T, s *Store) []Notification {
	t.Helper()
	var all []Notification
	for _, row := range dumpRows(t, s.db, "SELECT id FROM notifications ORDER BY id") {
		var id int64
		if _, err := fmt.Sscan(row, &id); err != nil {
			t.Fatalf("解析通知 id %q 失败: %v", row, err)
		}
		all = append(all, mustGetNotification(t, s, id))
	}
	return all
}

// payloadRows 返回 notification_payloads 的全部行（通知 id、key_id 与密文），用于断言正文行没有改动。
func payloadRows(t *testing.T, s *Store) []string {
	t.Helper()
	return dumpRows(t, s.db, "SELECT notification_id, key_id, hex(sealed) FROM notification_payloads ORDER BY notification_id")
}

// payloadCount 返回通知 id 的正文行数：待处理时为 1，进入 SENT 或 ABANDONED 后为 0。
func payloadCount(t *testing.T, s *Store, id int64) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM notification_payloads WHERE notification_id = ?", id).Scan(&n); err != nil {
		t.Fatalf("统计通知 %d 的正文行失败: %v", id, err)
	}
	return n
}

// requireNotification 断言通知 id 的状态、放弃原因与正文行数。
func requireNotification(t *testing.T, s *Store, id int64, state queue.OutboxState, reason string, payloads int) {
	t.Helper()
	got := mustGetNotification(t, s, id)
	if got.State != state || got.AbandonReason != reason {
		t.Errorf("通知 %d = %s/%q; want %s/%q", id, got.State, got.AbandonReason, state, reason)
	}
	if n := payloadCount(t, s, id); n != payloads {
		t.Errorf("通知 %d 的正文行数 = %d; want %d", id, n, payloads)
	}
}

// requireNoSendable 断言领取返回 ErrNoSendableNotification。
func requireNoSendable(t *testing.T, s *Store) {
	t.Helper()
	if claimed, _, err := s.ClaimNextNotification(t.Context()); !errors.Is(err, ErrNoSendableNotification) {
		t.Errorf("ClaimNextNotification = %+v, %v; want ErrNoSendableNotification", claimed, err)
	}
}

// storeDir 返回存储所用数据库文件所在的数据目录。
func storeDir(t *testing.T, s *Store) string {
	t.Helper()
	var seq int
	var name, file string
	if err := s.db.QueryRowContext(t.Context(), "PRAGMA database_list").Scan(&seq, &name, &file); err != nil {
		t.Fatalf("读取数据库文件路径失败: %v", err)
	}
	return filepath.Dir(file)
}

// requireWALEmpty 断言存储数据目录中的 WAL 文件为 0 字节，即操作提交后执行了 TRUNCATE 检查点。
func requireWALEmpty(t *testing.T, s *Store, step string) {
	t.Helper()
	if size := walSize(t, storeDir(t, s)); size != 0 {
		t.Errorf("%s 之后 WAL 大小 = %d; want 0（提交后应执行检查点）", step, size)
	}
}

// TestCreateNotification 验证创建：返回 PENDING、attempts 0、not_before 与创建时间为当前时间、令牌到期时间为当前时间加有效期
// （毫秒精度）、令牌 kid 1、owner 为 local；nid 等于随机源给出的前 12 字节，Message-ID 由其后 15 字节编码为 24 位
// Crockford base32；内容只以绑定到该行的密文落盘，用测试密钥能还原。NotificationByNID 取回同一快照，未知 nid 返回 ErrNotFound。
func TestCreateNotification(t *testing.T) {
	store, clock, running := newNotificationStore(t)
	ctx := t.Context()
	nid := make([]byte, 12)
	for i := range nid {
		nid[i] = byte(0xa0 + i)
	}
	// 这 15 个字节按 5 位分组依次为 0–23，编码后恰为字母表的前 24 个字符。
	messageRandom := []byte{0x00, 0x44, 0x32, 0x14, 0xc7, 0x42, 0x54, 0xb6, 0x35, 0xcf, 0x84, 0x65, 0x3a, 0x56, 0xd7}
	store.random = bytes.NewReader(slices.Concat(nid, messageRandom))
	in := notificationFor(running.ID, "create")
	created := mustCreateNotification(t, store, in)
	want := Notification{
		ID:             created.ID,
		TaskID:         running.ID,
		Owner:          "local",
		Event:          "turn_completed",
		NID:            [12]byte(nid),
		MessageID:      "<tc." + crockfordAlphabet[:24] + "@example.invalid>",
		TokenKeyID:     1,
		TokenExpiresAt: clock.Add(in.TTL).UTC(),
		State:          queue.OutboxPending,
		NotBefore:      clock.UTC(),
		CreatedAt:      clock.UTC(),
		UpdatedAt:      clock.UTC(),
	}
	if created.ID < 1 || created != want {
		t.Fatalf("CreateNotification = %+v; want %+v", created, want)
	}
	if !messageIDPattern.MatchString(created.MessageID) {
		t.Errorf("MessageID = %q; want 匹配 %s", created.MessageID, messageIDPattern)
	}
	var expiresAt, notBefore, createdAt int64
	if err := store.db.QueryRowContext(ctx, "SELECT token_expires_at, not_before, created_at FROM notifications WHERE id = ?", created.ID).
		Scan(&expiresAt, &notBefore, &createdAt); err != nil {
		t.Fatalf("读取时间列失败: %v", err)
	}
	if expiresAt != clock.UnixMilli()+in.TTL.Milliseconds() || notBefore != clock.UnixMilli() || createdAt != clock.UnixMilli() {
		t.Errorf("存储的时间 = %d, %d, %d; want %d, %d, %d", expiresAt, notBefore, createdAt,
			clock.UnixMilli()+in.TTL.Milliseconds(), clock.UnixMilli(), clock.UnixMilli())
	}

	var keyID int
	var sealed []byte
	if err := store.db.QueryRowContext(ctx, "SELECT key_id, sealed FROM notification_payloads WHERE notification_id = ?", created.ID).
		Scan(&keyID, &sealed); err != nil {
		t.Fatalf("读取正文行失败: %v", err)
	}
	if keyID != 1 || len(sealed) != len(in.Content)+payload.Overhead {
		t.Errorf("正文行 key_id = %d、密文长度 = %d; want 1、%d", keyID, len(sealed), len(in.Content)+payload.Overhead)
	}
	if bytes.Contains(sealed, []byte(notificationCanary)) {
		t.Error("密文含内容中的金丝雀文本")
	}
	opened, err := newTestPayloadKey(t, 1).Open(payload.KindNotification, running.ID, created.ID, sealed)
	if err != nil || !bytes.Equal(opened, in.Content) {
		t.Errorf("payload.Open = %q, %v; want %q", opened, err, in.Content)
	}

	if got, err := store.NotificationByNID(ctx, created.NID); err != nil || got != created {
		t.Errorf("NotificationByNID = %+v, %v; want %+v", got, err, created)
	}
	if got, err := store.NotificationByNID(ctx, [12]byte{0x01}); !errors.Is(err, ErrNotFound) {
		t.Errorf("未知 nid: NotificationByNID = %+v, %v; want ErrNotFound", got, err)
	}
	// Owner 取自所属任务的当前值，而不是常量。
	if _, err := store.db.ExecContext(ctx, "UPDATE tasks SET owner = 'owner-synthetic' WHERE id = ?", running.ID); err != nil {
		t.Fatalf("修改任务 owner 失败: %v", err)
	}
	if got, err := store.NotificationByNID(ctx, created.NID); err != nil || got.Owner != "owner-synthetic" {
		t.Errorf("修改 owner 后 NotificationByNID = %+v, %v; want Owner owner-synthetic", got, err)
	}

	// 其余字节取自随机源：Message-ID 同样合规，nid 与 Message-ID 各不相同。
	store.random = rand.NewChaCha8([32]byte{0x01})
	failed := notificationFor(running.ID, "second")
	failed.Event = "failed"
	second := mustCreateNotification(t, store, failed)
	if !messageIDPattern.MatchString(second.MessageID) || second.MessageID == created.MessageID || second.NID == created.NID || second.Event != "failed" {
		t.Errorf("第二条通知 = %+v; want 新的 nid 与合规的新 Message-ID", second)
	}
}

// TestCreateNotificationValidation 验证字段校验在事务开始前拒绝非法输入：错误文本含 "invalid notification" 并包装
// ErrInvalidArgument，上下文已取消时仍返回校验错误，且不写入任何行；各字段边界上的合法值可以创建。
func TestCreateNotificationValidation(t *testing.T) {
	store, _, running := newNotificationStore(t)
	tests := []struct {
		name   string
		modify func(*NewNotification)
	}{
		{"未知事件", func(n *NewNotification) { n.Event = "bogus" }},
		{"事件为空", func(n *NewNotification) { n.Event = "" }},
		{"大写事件", func(n *NewNotification) { n.Event = "TURN_COMPLETED" }},
		{"任务事件名", func(n *NewNotification) { n.Event = "close" }},
		{"域名为空", func(n *NewNotification) { n.Domain = "" }},
		{"域名含大写字母", func(n *NewNotification) { n.Domain = "Example.invalid" }},
		{"域名含下划线", func(n *NewNotification) { n.Domain = "mail_host.invalid" }},
		{"域名含 @", func(n *NewNotification) { n.Domain = "bot@example.invalid" }},
		{"域名含空格", func(n *NewNotification) { n.Domain = "example .invalid" }},
		{"域名含尖括号", func(n *NewNotification) { n.Domain = "example.invalid>" }},
		{"域名末尾换行", func(n *NewNotification) { n.Domain = "example.invalid\n" }},
		{"域名超过 253 字符", func(n *NewNotification) { n.Domain = strings.Repeat("a", 254) }},
		{"有效期为 59 分钟", func(n *NewNotification) { n.TTL = 59 * time.Minute }},
		{"有效期比 1 小时少 1 毫秒", func(n *NewNotification) { n.TTL = time.Hour - time.Millisecond }},
		{"有效期为 721 小时", func(n *NewNotification) { n.TTL = 721 * time.Hour }},
		{"有效期比 720 小时多 1 毫秒", func(n *NewNotification) { n.TTL = 720*time.Hour + time.Millisecond }},
		{"有效期为 0", func(n *NewNotification) { n.TTL = 0 }},
		{"有效期为负", func(n *NewNotification) { n.TTL = -time.Hour }},
		{"内容为空", func(n *NewNotification) { n.Content = nil }},
		{"内容超过上限", func(n *NewNotification) { n.Content = make([]byte, payload.MaxPlaintext+1) }},
		{"任务 ID 为空", func(n *NewNotification) { n.TaskID = "" }},
		{"任务 ID 9 个字符", func(n *NewNotification) { n.TaskID = "000000000" }},
		{"任务 ID 11 个字符", func(n *NewNotification) { n.TaskID = "00000000000" }},
		{"任务 ID 含 u", func(n *NewNotification) { n.TaskID = "000000000u" }},
		{"任务 ID 含大写字母", func(n *NewNotification) { n.TaskID = "ABCDEFGHJK" }},
		{"任务 ID 末尾换行", func(n *NewNotification) { n.TaskID = "000000000\n" }},
	}
	for _, tt := range tests {
		in := notificationFor(running.ID, "invalid")
		tt.modify(&in)
		got, err := store.CreateNotification(t.Context(), in)
		if err == nil || !strings.Contains(err.Error(), "invalid notification") || !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: CreateNotification = %+v, %v; want 包装 ErrInvalidArgument、含 \"invalid notification\" 的错误", tt.name, got, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	in := notificationFor(running.ID, "canceled")
	in.TTL = 0
	if got, err := store.CreateNotification(ctx, in); err == nil || !strings.Contains(err.Error(), "invalid notification") ||
		!errors.Is(err, ErrInvalidArgument) || errors.Is(err, context.Canceled) {
		t.Errorf("上下文已取消: CreateNotification = %+v, %v; want 含 \"invalid notification\" 的错误而不是 context.Canceled", got, err)
	}
	if n, p := countRows(t, store.db, "notifications"), countRows(t, store.db, "notification_payloads"); n != 0 || p != 0 {
		t.Fatalf("非法输入后通知 %d 行、正文 %d 行; want 0、0", n, p)
	}

	valid := []struct {
		name   string
		modify func(*NewNotification)
	}{
		{"有效期 1 小时", func(n *NewNotification) { n.TTL = time.Hour }},
		{"有效期 720 小时", func(n *NewNotification) { n.TTL = 720 * time.Hour }},
		{"域名 253 字符", func(n *NewNotification) { n.Domain = strings.Repeat("a", 253) }},
		{"域名含数字、点与连字符", func(n *NewNotification) { n.Domain = "mail-01.example.invalid" }},
		{"内容 1 字节", func(n *NewNotification) { n.Content = []byte{'x'} }},
		{"内容达到上限", func(n *NewNotification) { n.Content = bytes.Repeat([]byte{'x'}, payload.MaxPlaintext) }},
		{"waiting_input", func(n *NewNotification) { n.Event = "waiting_input" }},
		{"waiting_approval", func(n *NewNotification) { n.Event = "waiting_approval" }},
		{"failed", func(n *NewNotification) { n.Event = "failed" }},
	}
	for _, tt := range valid {
		in := notificationFor(running.ID, tt.name)
		tt.modify(&in)
		created, err := store.CreateNotification(t.Context(), in)
		if err != nil || created.Event != in.Event || !created.TokenExpiresAt.Equal(created.CreatedAt.Add(in.TTL)) ||
			!strings.HasSuffix(created.MessageID, "@"+in.Domain+">") {
			t.Errorf("%s: CreateNotification = %+v, %v; want 成功", tt.name, created, err)
		}
	}
}

// setTaskOwner 直接改写任务的 owner：存储没有修改 owner 的接口，只有外部工具能把它改成令牌签不出来的取值。
func setTaskOwner(t *testing.T, s *Store, taskID, owner string) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), "UPDATE tasks SET owner = ? WHERE id = ?", owner, taskID); err != nil {
		t.Fatalf("改写任务 owner 失败: %v", err)
	}
}

// TestCreateNotificationChecksOwner 验证 owner 不在 token.Claims 要求的 1–255 字节内时不创建通知，错误包装 ErrInvalidArgument
// （重试也不会成功）：否则会建出一条永远签不出令牌、因而永远发不出去的通知。长度按字节计，255 个字节合法、256 个字节不合法。
func TestCreateNotificationChecksOwner(t *testing.T) {
	store, _, running := newNotificationStore(t)
	for _, owner := range []string{"", strings.Repeat("o", 256), strings.Repeat("代", 86)} {
		setTaskOwner(t, store, running.ID, owner)
		got, err := store.CreateNotification(t.Context(), notificationFor(running.ID, "owner"))
		if err == nil || !strings.Contains(err.Error(), "invalid notification") || !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("owner %d 字节: CreateNotification = %+v, %v; want 包装 ErrInvalidArgument、含 \"invalid notification\" 的错误", len(owner), got, err)
		}
	}
	if n, p := countRows(t, store.db, "notifications"), countRows(t, store.db, "notification_payloads"); n != 0 || p != 0 {
		t.Fatalf("owner 不合法时通知 %d 行、正文 %d 行; want 0、0", n, p)
	}
	for _, owner := range []string{"o", strings.Repeat("o", 255), strings.Repeat("代", 85)} {
		setTaskOwner(t, store, running.ID, owner)
		created := mustCreateNotification(t, store, notificationFor(running.ID, fmt.Sprintf("owner %d", len(owner))))
		if created.Owner != owner {
			t.Errorf("owner %d 字节: 通知的 Owner = %q; want 与任务一致", len(owner), created.Owner)
		}
	}
}

// TestCreateNotificationChecksTaskAndKeys 验证事务内的检查：任务不存在返回 ErrNotFound；任务 CLOSED 返回 ErrTaskNotNotifiable；
// CREATED 与 FAILED 任务可以创建；没有 active 令牌密钥返回包装 ErrNotFound 的错误；没有正文密钥、正文密钥的 kid 未登记、
// 已不是 active，或与登记的 kid 不符时返回 ErrPayloadKeyUnavailable。失败时不写入任何行。
func TestCreateNotificationChecksTaskAndKeys(t *testing.T) {
	t.Run("任务状态", func(t *testing.T) {
		store, _, running := newNotificationStore(t)
		if _, err := store.CreateNotification(t.Context(), notificationFor("zzzzzzzzzz", "missing")); !errors.Is(err, ErrNotFound) {
			t.Errorf("任务不存在: err = %v; want ErrNotFound", err)
		}
		closed := applyEvents(t, store, running, task.Close)
		if _, err := store.CreateNotification(t.Context(), notificationFor(closed.ID, "closed")); !errors.Is(err, ErrTaskNotNotifiable) {
			t.Errorf("任务已关闭: err = %v; want ErrTaskNotNotifiable", err)
		}
		if n := countRows(t, store.db, "notifications"); n != 0 {
			t.Fatalf("失败后通知行数 = %d; want 0", n)
		}
		failed := applyEvents(t, store, startTask(t, store), task.Fail)
		in := notificationFor(failed.ID, "failed")
		in.Event = "failed"
		if created, err := store.CreateNotification(t.Context(), in); err != nil || created.State != queue.OutboxPending {
			t.Errorf("任务 FAILED: CreateNotification = %+v, %v; want PENDING", created, err)
		}
		if created, err := store.CreateNotification(t.Context(), notificationFor(createTask(t, store).ID, "created")); err != nil {
			t.Errorf("任务 CREATED: CreateNotification = %+v, %v; want 成功", created, err)
		}
	})

	tests := []struct {
		name  string
		setup func(t *testing.T, s *Store)
		want  error
	}{
		{"没有 active 令牌密钥", func(t *testing.T, s *Store) {
			if err := s.RegisterKey(t.Context(), KeyPurposePayload, 1, testKeyCheck); err != nil {
				t.Fatalf("RegisterKey 返回错误: %v", err)
			}
		}, ErrNotFound},
		{"没有正文密钥", func(t *testing.T, s *Store) {
			registerTestKeys(t, s)
			s.payloadKey = nil
		}, ErrPayloadKeyUnavailable},
		{"正文密钥未登记", func(t *testing.T, s *Store) {
			if err := s.RegisterKey(t.Context(), KeyPurposeToken, 1, testKeyCheck); err != nil {
				t.Fatalf("RegisterKey 返回错误: %v", err)
			}
		}, ErrPayloadKeyUnavailable},
		{"正文密钥已不是 active", func(t *testing.T, s *Store) {
			registerTestKeys(t, s)
			if _, err := s.db.ExecContext(t.Context(), "UPDATE crypto_keys SET state = 'retired' WHERE purpose = 'payload'"); err != nil {
				t.Fatalf("修改密钥状态失败: %v", err)
			}
		}, ErrPayloadKeyUnavailable},
		{"正文密钥的 kid 与登记不符", func(t *testing.T, s *Store) {
			registerTestKeys(t, s)
			s.payloadKey = newTestPayloadKey(t, 2)
		}, ErrPayloadKeyUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openNotificationStore(t, dataDir(t), testClock())
			running := startTask(t, store)
			tt.setup(t, store)
			_, err := store.CreateNotification(t.Context(), notificationFor(running.ID, "keys"))
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v; want %v", err, tt.want)
			}
			if errors.Is(err, ErrNotFound) && !strings.Contains(err.Error(), "token") {
				t.Errorf("err = %v; want 指明缺少令牌密钥", err)
			}
			if n, p := countRows(t, store.db, "notifications"), countRows(t, store.db, "notification_payloads"); n != 0 || p != 0 {
				t.Errorf("失败后通知 %d 行、正文 %d 行; want 0、0", n, p)
			}
		})
	}
}

// TestCreateNotificationFailures 验证创建失败时整个事务回滚：正文行写入失败时通知行也不留下；随机源不足 27 字节时报错；
// nid 或 Message-ID 与已有通知冲突时报错且不重试（随机源只被读取一次 27 字节）。
func TestCreateNotificationFailures(t *testing.T) {
	store, _, running := newNotificationStore(t)
	ctx := t.Context()
	if _, err := store.db.ExecContext(ctx, "CREATE TRIGGER fault BEFORE INSERT ON notification_payloads BEGIN SELECT RAISE(ABORT, 'boom'); END"); err != nil {
		t.Fatalf("创建触发器失败: %v", err)
	}
	if _, err := store.CreateNotification(ctx, notificationFor(running.ID, "fault")); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("正文行写入失败: err = %v; want 含 boom 的错误", err)
	}
	if n := countRows(t, store.db, "notifications"); n != 0 {
		t.Errorf("正文行写入失败后通知行数 = %d; want 0", n)
	}
	if _, err := store.db.ExecContext(ctx, "DROP TRIGGER fault"); err != nil {
		t.Fatalf("删除触发器失败: %v", err)
	}

	store.random = bytes.NewReader(make([]byte, 26))
	if _, err := store.CreateNotification(ctx, notificationFor(running.ID, "short")); err == nil {
		t.Error("随机源不足 27 字节时应报错")
	}

	// sameNID 只与 first 的 nid 相同，sameMessageID 只与 first 的 Message-ID 随机部分相同，各自只触发一条唯一约束。
	first := bytes.Repeat([]byte{0x11}, 27)
	sameNID := slices.Concat(first[:12], bytes.Repeat([]byte{0x33}, 15))
	sameMessageID := slices.Concat(bytes.Repeat([]byte{0x22}, 12), first[12:])
	random := bytes.NewReader(slices.Concat(first, sameNID, sameMessageID, make([]byte, 5)))
	store.random = random
	mustCreateNotification(t, store, notificationFor(running.ID, "first"))
	if _, err := store.CreateNotification(ctx, notificationFor(running.ID, "same nid")); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: notifications.nid") {
		t.Errorf("nid 冲突: err = %v; want 违反 nid 唯一约束", err)
	}
	if _, err := store.CreateNotification(ctx, notificationFor(running.ID, "same message id")); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: notifications.message_id") {
		t.Errorf("Message-ID 冲突: err = %v; want 违反 message_id 唯一约束", err)
	}
	if random.Len() != 5 {
		t.Errorf("随机源剩余 %d 字节; want 5（冲突后不重试）", random.Len())
	}
	if n, p := countRows(t, store.db, "notifications"), countRows(t, store.db, "notification_payloads"); n != 1 || p != 1 {
		t.Errorf("通知 %d 行、正文 %d 行; want 1、1", n, p)
	}
}

// TestClaimNextNotification 验证领取：按 (not_before, id) 取最早到期的 PENDING 通知，改为 SENDING、attempts 加 1，
// 返回解密后的内容；已有 SENDING 通知时返回 ErrNoSendableNotification 且不改动数据；放回队列的通知在 not_before 之前不被领取，
// not_before 较早的通知即使 id 较大也先被领取。
func TestClaimNextNotification(t *testing.T) {
	store, clock, running := newNotificationStore(t)
	first := mustCreateNotification(t, store, notificationFor(running.ID, "first"))
	second := mustCreateNotification(t, store, notificationFor(running.ID, "second"))
	third := mustCreateNotification(t, store, notificationFor(running.ID, "third"))
	*clock = clock.Add(time.Second)

	claimed, content := mustClaimNotification(t, store)
	want := first
	want.State, want.Attempts, want.UpdatedAt = queue.OutboxSending, 1, clock.UTC()
	if claimed != want || !bytes.Equal(content, notificationFor(running.ID, "first").Content) {
		t.Fatalf("ClaimNextNotification = %+v, %q; want %+v 与第一条的内容", claimed, content, want)
	}
	requireNotification(t, store, first.ID, queue.OutboxSending, "", 1)
	before := allNotifications(t, store)
	requireNoSendable(t, store)
	if after := allNotifications(t, store); !slices.Equal(after, before) {
		t.Errorf("已有 SENDING 时领取改动了通知: %+v; want %+v", after, before)
	}

	requeued := must(t, "RequeueNotification")(store.RequeueNotification(t.Context(), first.ID, 30*time.Second))
	want.State, want.NotBefore = queue.OutboxPending, clock.Add(30*time.Second).UTC()
	if requeued != want {
		t.Errorf("RequeueNotification = %+v; want %+v", requeued, want)
	}
	if claimed, content := mustClaimNotification(t, store); claimed.ID != second.ID || !bytes.Equal(content, notificationFor(running.ID, "second").Content) {
		t.Errorf("放回后立即领取 = %+v, %q; want 第二条", claimed, content)
	}
	must(t, "MarkNotificationSent")(store.MarkNotificationSent(t.Context(), second.ID))
	if claimed, _ := mustClaimNotification(t, store); claimed.ID != third.ID {
		t.Errorf("领取 = 通知 %d; want 第三条 %d", claimed.ID, third.ID)
	}
	must(t, "MarkNotificationSent")(store.MarkNotificationSent(t.Context(), third.ID))
	*clock = clock.Add(30*time.Second - time.Millisecond)
	requireNoSendable(t, store)
	*clock = clock.Add(time.Millisecond)
	claimed, content = mustClaimNotification(t, store)
	if claimed.ID != first.ID || claimed.Attempts != 2 || !bytes.Equal(content, notificationFor(running.ID, "first").Content) {
		t.Errorf("时钟推进 30 秒后领取 = %+v, %q; want 第一条，attempts 2", claimed, content)
	}

	// not_before 优先于 id：第一条放回（not_before 为当前时间）后，更早到期的第四条先被领取。
	fourth := mustCreateNotification(t, store, notificationFor(running.ID, "fourth"))
	*clock = clock.Add(time.Second)
	must(t, "RequeueNotification")(store.RequeueNotification(t.Context(), first.ID, 0))
	if claimed, _ := mustClaimNotification(t, store); claimed.ID != fourth.ID {
		t.Errorf("领取 = 通知 %d; want not_before 较早的通知 %d", claimed.ID, fourth.ID)
	}
}

// TestClaimNextNotificationUnreadablePayload 验证内容无法读出时不领取：密文被篡改返回包装 payload.ErrDecrypt 的错误，
// 正文行缺失返回 ErrPayloadMissing，没有正文密钥、密文的 key_id 与当前密钥不符或该 kid 已不是 active 时返回
// ErrPayloadKeyUnavailable；各情况都不改动任何行，通知仍为 PENDING，错误文本不含内容；领取返回该通知未改动的快照，
// 调用方据此取得 id 以便告警或放弃。
func TestClaimNextNotificationUnreadablePayload(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, s *Store, id int64)
		want  error
	}{
		{"密文被篡改", func(t *testing.T, s *Store, id int64) {
			var sealed []byte
			if err := s.db.QueryRowContext(t.Context(), "SELECT sealed FROM notification_payloads WHERE notification_id = ?", id).Scan(&sealed); err != nil {
				t.Fatalf("读取密文失败: %v", err)
			}
			sealed[20] ^= 0x01
			if _, err := s.db.ExecContext(t.Context(), "DROP TRIGGER notification_payloads_immutable"); err != nil {
				t.Fatalf("删除不可改写触发器失败: %v", err)
			}
			if _, err := s.db.ExecContext(t.Context(), "UPDATE notification_payloads SET sealed = ? WHERE notification_id = ?", sealed, id); err != nil {
				t.Fatalf("改写密文失败: %v", err)
			}
		}, payload.ErrDecrypt},
		{"正文行缺失", func(t *testing.T, s *Store, id int64) {
			if _, err := s.db.ExecContext(t.Context(), "DELETE FROM notification_payloads WHERE notification_id = ?", id); err != nil {
				t.Fatalf("删除正文行失败: %v", err)
			}
		}, ErrPayloadMissing},
		{"没有正文密钥", func(t *testing.T, s *Store, _ int64) { s.payloadKey = nil }, ErrPayloadKeyUnavailable},
		{"密文的 key_id 与当前密钥不符", func(t *testing.T, s *Store, _ int64) {
			for _, query := range []string{
				"UPDATE crypto_keys SET state = 'retired' WHERE purpose = 'payload'",
				"INSERT INTO crypto_keys (purpose, kid, state, key_check, created_at, updated_at) SELECT 'payload', 2, 'active', key_check, 0, 0 FROM crypto_keys WHERE purpose = 'payload'",
			} {
				if _, err := s.db.ExecContext(t.Context(), query); err != nil {
					t.Fatalf("%s 失败: %v", query, err)
				}
			}
			s.payloadKey = newTestPayloadKey(t, 2)
		}, ErrPayloadKeyUnavailable},
		{"kid 已不是 active", func(t *testing.T, s *Store, _ int64) {
			if _, err := s.db.ExecContext(t.Context(), "UPDATE crypto_keys SET state = 'retired' WHERE purpose = 'payload'"); err != nil {
				t.Fatalf("修改密钥状态失败: %v", err)
			}
		}, ErrPayloadKeyUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, _, running := newNotificationStore(t)
			created := mustCreateNotification(t, store, notificationFor(running.ID, "unreadable"))
			tt.setup(t, store, created.ID)
			notifications, payloads := allNotifications(t, store), payloadRows(t, store)
			claimed, content, err := store.ClaimNextNotification(t.Context())
			if !errors.Is(err, tt.want) || content != nil || claimed != notifications[0] {
				t.Errorf("ClaimNextNotification = %+v, %q, %v; want 未改动的快照 %+v、nil 与 %v", claimed, content, err, notifications[0], tt.want)
			}
			if err != nil && strings.Contains(err.Error(), notificationCanary) {
				t.Errorf("错误文本含通知内容: %v", err)
			}
			if after := allNotifications(t, store); !slices.Equal(after, notifications) || after[0].State != queue.OutboxPending {
				t.Errorf("通知被改动: %+v; want %+v", after, notifications)
			}
			if after := payloadRows(t, store); !slices.Equal(after, payloads) {
				t.Errorf("正文行被改动: %q; want %q", after, payloads)
			}
		})
	}
}

// TestNotificationPayloadUsesCurrentKey 验证正文行记录加密所用密钥的 kid：payload 的 active 密钥换为 kid 2 后，
// 新通知的正文行 key_id 为 2，并能用 kid 2 的密钥领取到原内容。
func TestNotificationPayloadUsesCurrentKey(t *testing.T) {
	store, _, running := newNotificationStore(t)
	for _, query := range []string{
		"UPDATE crypto_keys SET state = 'retired' WHERE purpose = 'payload'",
		"INSERT INTO crypto_keys (purpose, kid, state, key_check, created_at, updated_at) SELECT 'payload', 2, 'active', key_check, 0, 0 FROM crypto_keys WHERE purpose = 'payload'",
	} {
		if _, err := store.db.ExecContext(t.Context(), query); err != nil {
			t.Fatalf("%s 失败: %v", query, err)
		}
	}
	store.payloadKey = newTestPayloadKey(t, 2)
	in := notificationFor(running.ID, "kid 2")
	created := mustCreateNotification(t, store, in)
	var keyID int
	if err := store.db.QueryRowContext(t.Context(), "SELECT key_id FROM notification_payloads WHERE notification_id = ?", created.ID).Scan(&keyID); err != nil || keyID != 2 {
		t.Errorf("正文行 key_id = %d, %v; want 2", keyID, err)
	}
	if claimed, content := mustClaimNotification(t, store); claimed.ID != created.ID || !bytes.Equal(content, in.Content) {
		t.Errorf("领取 = %+v, %q; want 通知 %d 与原内容", claimed, content, created.ID)
	}
}

// TestNotificationOutcomes 验证投递结果：SENT 记录 sent_at、删除正文行并执行检查点，终态上再次标记返回
// queue.ErrInvalidOutboxTransition；各操作只接受契约规定的来源状态；UNCERTAIN 保留正文，核对为已投递时改为 SENT（sent_at 为进入
// UNCERTAIN 的时刻，不是核对时刻），核对为未投递时回到 PENDING 并能再次领取到相同内容，任务已关闭时改为 ABANDONED(task_closed)；
// 放回队列的延迟须在 0 到 24 小时。
func TestNotificationOutcomes(t *testing.T) {
	store, clock, running := newNotificationStore(t)
	ctx := t.Context()
	sent := mustCreateNotification(t, store, notificationFor(running.ID, "sent"))
	claimed, _ := mustClaimNotification(t, store)
	*clock = clock.Add(time.Second)
	got := must(t, "MarkNotificationSent")(store.MarkNotificationSent(ctx, sent.ID))
	want := claimed
	want.State, want.SentAt, want.UpdatedAt = queue.OutboxSent, clock.UTC(), clock.UTC()
	if got != want {
		t.Errorf("MarkNotificationSent = %+v; want %+v", got, want)
	}
	requireNotification(t, store, sent.ID, queue.OutboxSent, "", 0)
	requireWALEmpty(t, store, "MarkNotificationSent")
	if _, err := store.MarkNotificationSent(ctx, sent.ID); !errors.Is(err, queue.ErrInvalidOutboxTransition) {
		t.Errorf("再次 MarkNotificationSent: err = %v; want queue.ErrInvalidOutboxTransition", err)
	}
	if _, err := store.MarkNotificationSent(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的通知: err = %v; want ErrNotFound", err)
	}

	// PENDING 通知只能被领取或放弃。
	pending := mustCreateNotification(t, store, notificationFor(running.ID, "uncertain"))
	for name, op := range map[string]func() (Notification, error){
		"MarkNotificationSent":         func() (Notification, error) { return store.MarkNotificationSent(ctx, pending.ID) },
		"MarkNotificationUncertain":    func() (Notification, error) { return store.MarkNotificationUncertain(ctx, pending.ID) },
		"RequeueNotification":          func() (Notification, error) { return store.RequeueNotification(ctx, pending.ID, 0) },
		"ResolveUncertainNotification": func() (Notification, error) { return store.ResolveUncertainNotification(ctx, pending.ID, true) },
	} {
		if got, err := op(); !errors.Is(err, queue.ErrInvalidOutboxTransition) {
			t.Errorf("PENDING 上的 %s = %+v, %v; want queue.ErrInvalidOutboxTransition", name, got, err)
		}
	}
	requireNotification(t, store, pending.ID, queue.OutboxPending, "", 1)

	mustClaimNotification(t, store)
	if _, err := store.ResolveUncertainNotification(ctx, pending.ID, false); !errors.Is(err, queue.ErrInvalidOutboxTransition) {
		t.Errorf("SENDING 上的 ResolveUncertainNotification: err = %v; want queue.ErrInvalidOutboxTransition", err)
	}
	uncertain := must(t, "MarkNotificationUncertain")(store.MarkNotificationUncertain(ctx, pending.ID))
	if uncertain.State != queue.OutboxUncertain || !uncertain.SentAt.IsZero() {
		t.Errorf("MarkNotificationUncertain = %+v; want UNCERTAIN", uncertain)
	}
	requireNotification(t, store, pending.ID, queue.OutboxUncertain, "", 1)
	for name, op := range map[string]func() (Notification, error){
		"MarkNotificationSent":      func() (Notification, error) { return store.MarkNotificationSent(ctx, pending.ID) },
		"MarkNotificationUncertain": func() (Notification, error) { return store.MarkNotificationUncertain(ctx, pending.ID) },
		"RequeueNotification":       func() (Notification, error) { return store.RequeueNotification(ctx, pending.ID, 0) },
	} {
		if got, err := op(); !errors.Is(err, queue.ErrInvalidOutboxTransition) {
			t.Errorf("UNCERTAIN 上的 %s = %+v, %v; want queue.ErrInvalidOutboxTransition", name, got, err)
		}
	}
	*clock = clock.Add(time.Second)
	resolved := must(t, "ResolveUncertainNotification(true)")(store.ResolveUncertainNotification(ctx, pending.ID, true))
	if resolved.State != queue.OutboxSent || !resolved.SentAt.Equal(uncertain.UpdatedAt) || !resolved.UpdatedAt.Equal(clock.UTC()) {
		t.Errorf("ResolveUncertainNotification(true) = %+v; want SENT、sent_at 为进入 UNCERTAIN 的时刻 %v、updated_at 为核对时刻 %v",
			resolved, uncertain.UpdatedAt, clock.UTC())
	}
	requireNotification(t, store, pending.ID, queue.OutboxSent, "", 0)
	requireWALEmpty(t, store, "ResolveUncertainNotification(true)")

	retried := mustCreateNotification(t, store, notificationFor(running.ID, "retried"))
	mustClaimNotification(t, store)
	must(t, "MarkNotificationUncertain")(store.MarkNotificationUncertain(ctx, retried.ID))
	*clock = clock.Add(time.Second)
	back := must(t, "ResolveUncertainNotification(false)")(store.ResolveUncertainNotification(ctx, retried.ID, false))
	if back.State != queue.OutboxPending || back.NotBefore != clock.UTC() || back.Attempts != 1 {
		t.Errorf("ResolveUncertainNotification(false) = %+v; want PENDING，not_before 为当前时间", back)
	}
	requireNotification(t, store, retried.ID, queue.OutboxPending, "", 1)
	if claimed, content := mustClaimNotification(t, store); claimed.ID != retried.ID || claimed.Attempts != 2 ||
		!bytes.Equal(content, notificationFor(running.ID, "retried").Content) {
		t.Errorf("核对为未投递后领取 = %+v, %q; want 同一通知与相同内容", claimed, content)
	}

	for _, delay := range []time.Duration{-time.Second, -time.Millisecond, 24*time.Hour + time.Millisecond, 25 * time.Hour} {
		if got, err := store.RequeueNotification(ctx, retried.ID, delay); err == nil || !strings.Contains(err.Error(), "invalid notification") ||
			!errors.Is(err, ErrInvalidArgument) {
			t.Errorf("延迟 %v: RequeueNotification = %+v, %v; want 包装 ErrInvalidArgument、含 \"invalid notification\" 的错误", delay, got, err)
		}
	}
	requireNotification(t, store, retried.ID, queue.OutboxSending, "", 1)
	if got := must(t, "RequeueNotification")(store.RequeueNotification(ctx, retried.ID, 24*time.Hour)); got.NotBefore != clock.Add(24*time.Hour).UTC() {
		t.Errorf("延迟 24 小时: NotBefore = %v; want %v", got.NotBefore, clock.Add(24*time.Hour).UTC())
	}

	closing := mustCreateNotification(t, store, notificationFor(running.ID, "closing"))
	if claimed, _ := mustClaimNotification(t, store); claimed.ID != closing.ID {
		t.Fatalf("领取 = 通知 %d; want %d", claimed.ID, closing.ID)
	}
	must(t, "MarkNotificationUncertain")(store.MarkNotificationUncertain(ctx, closing.ID))
	applyEvents(t, store, mustGetTask(t, store, running.ID), task.Close)
	abandoned := must(t, "ResolveUncertainNotification(false)")(store.ResolveUncertainNotification(ctx, closing.ID, false))
	if abandoned.State != queue.OutboxAbandoned || abandoned.AbandonReason != "task_closed" {
		t.Errorf("任务关闭后 ResolveUncertainNotification(false) = %+v; want ABANDONED(task_closed)", abandoned)
	}
	requireNotification(t, store, closing.ID, queue.OutboxAbandoned, "task_closed", 0)
	requireWALEmpty(t, store, "ResolveUncertainNotification(false) 放弃")
}

// TestNotificationValidationBeforeTransaction 验证放回队列的延迟、放弃原因与实际投递 Message-ID（记录与凭副本核对两处）的校验
// 先于开始事务：上下文已取消时仍返回包装 ErrInvalidArgument 的 "invalid notification" 校验错误，而不是 context.Canceled。
func TestNotificationValidationBeforeTransaction(t *testing.T) {
	store, _, _ := newNotificationStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := map[string]func() (Notification, error){
		"RequeueNotification":         func() (Notification, error) { return store.RequeueNotification(ctx, 1, -time.Second) },
		"AbandonNotification":         func() (Notification, error) { return store.AbandonNotification(ctx, 1, "expired") },
		"RecordDeliveredMessageID":    func() (Notification, error) { return store.RecordDeliveredMessageID(ctx, 1, "<a") },
		"ResolveUncertainAsDelivered": func() (Notification, error) { return store.ResolveUncertainAsDelivered(ctx, 1, "<a") },
	}
	for name, call := range calls {
		if got, err := call(); err == nil || !strings.Contains(err.Error(), "invalid notification") || !errors.Is(err, ErrInvalidArgument) ||
			errors.Is(err, context.Canceled) {
			t.Errorf("%s = %+v, %v; want 包装 ErrInvalidArgument、含 \"invalid notification\" 的错误而不是 context.Canceled", name, got, err)
		}
	}
}

// TestAbandonNotification 验证放弃：PENDING、SENDING、UNCERTAIN 通知以 rejected 或 manual 放弃成功，正文行被删除并执行检查点；
// 原因为 task_closed、expired 或其他值时在开始事务前报错；SENT 与已放弃的通知返回 queue.ErrInvalidOutboxTransition。
func TestAbandonNotification(t *testing.T) {
	// prepare 把新建的通知推进到指定状态。
	prepare := map[queue.OutboxState]func(t *testing.T, s *Store, id int64){
		queue.OutboxPending: func(*testing.T, *Store, int64) {},
		queue.OutboxSending: func(t *testing.T, s *Store, _ int64) { mustClaimNotification(t, s) },
		queue.OutboxUncertain: func(t *testing.T, s *Store, id int64) {
			mustClaimNotification(t, s)
			must(t, "MarkNotificationUncertain")(s.MarkNotificationUncertain(t.Context(), id))
		},
	}
	for _, state := range []queue.OutboxState{queue.OutboxPending, queue.OutboxSending, queue.OutboxUncertain} {
		for _, reason := range []string{"rejected", "manual"} {
			t.Run(string(state)+"/"+reason, func(t *testing.T) {
				store, clock, running := newNotificationStore(t)
				created := mustCreateNotification(t, store, notificationFor(running.ID, "abandon"))
				prepare[state](t, store, created.ID)
				for _, bad := range []string{"task_closed", "expired", "bogus", "", "REJECTED"} {
					if got, err := store.AbandonNotification(t.Context(), created.ID, bad); err == nil || !strings.Contains(err.Error(), "invalid notification") ||
						!errors.Is(err, ErrInvalidArgument) {
						t.Errorf("原因 %q: AbandonNotification = %+v, %v; want 包装 ErrInvalidArgument、含 \"invalid notification\" 的错误", bad, got, err)
					}
				}
				requireNotification(t, store, created.ID, state, "", 1)
				before := mustGetNotification(t, store, created.ID)
				*clock = clock.Add(time.Second)
				got := must(t, "AbandonNotification")(store.AbandonNotification(t.Context(), created.ID, reason))
				want := before
				want.State, want.AbandonReason, want.UpdatedAt = queue.OutboxAbandoned, reason, clock.UTC()
				if got != want {
					t.Errorf("AbandonNotification = %+v; want %+v", got, want)
				}
				requireNotification(t, store, created.ID, queue.OutboxAbandoned, reason, 0)
				requireWALEmpty(t, store, "AbandonNotification")
				if _, err := store.AbandonNotification(t.Context(), created.ID, reason); !errors.Is(err, queue.ErrInvalidOutboxTransition) {
					t.Errorf("再次放弃: err = %v; want queue.ErrInvalidOutboxTransition", err)
				}
			})
		}
	}
	store, _, running := newNotificationStore(t)
	sent := mustCreateNotification(t, store, notificationFor(running.ID, "sent"))
	mustClaimNotification(t, store)
	must(t, "MarkNotificationSent")(store.MarkNotificationSent(t.Context(), sent.ID))
	if _, err := store.AbandonNotification(t.Context(), sent.ID, "manual"); !errors.Is(err, queue.ErrInvalidOutboxTransition) {
		t.Errorf("SENT 通知: err = %v; want queue.ErrInvalidOutboxTransition", err)
	}
	requireNotification(t, store, sent.ID, queue.OutboxSent, "", 0)
}

// TestCloseTaskAbandonsPendingNotifications 验证关闭任务：同一事务中该任务的 PENDING 通知改为 ABANDONED(task_closed) 且正文行被删除，
// SENDING 与 UNCERTAIN 通知不变，排队回复按 Phase 3 规则被拒绝，提交后执行检查点；另一个任务执行 fail 后它的 PENDING 通知不变，
// fail 提交后同样执行检查点。
func TestCloseTaskAbandonsPendingNotifications(t *testing.T) {
	store, clock, running := newNotificationStore(t)
	uncertain := mustCreateNotification(t, store, notificationFor(running.ID, "uncertain"))
	mustClaimNotification(t, store)
	must(t, "MarkNotificationUncertain")(store.MarkNotificationUncertain(t.Context(), uncertain.ID))
	sending := mustCreateNotification(t, store, notificationFor(running.ID, "sending"))
	mustClaimNotification(t, store)
	pending := mustCreateNotification(t, store, notificationFor(running.ID, "pending"))
	replies := enqueueReplies(t, store, running.ID, 2)
	other := startTask(t, store)
	otherPending := mustCreateNotification(t, store, notificationFor(other.ID, "other"))
	inFlight := []Notification{mustGetNotification(t, store, uncertain.ID), mustGetNotification(t, store, sending.ID)}

	*clock = clock.Add(time.Second)
	applyEvents(t, store, mustGetTask(t, store, running.ID), task.Close)
	got := mustGetNotification(t, store, pending.ID)
	want := pending
	want.State, want.AbandonReason, want.UpdatedAt = queue.OutboxAbandoned, "task_closed", clock.UTC()
	if got != want {
		t.Errorf("关闭后 PENDING 通知 = %+v; want %+v", got, want)
	}
	requireNotification(t, store, pending.ID, queue.OutboxAbandoned, "task_closed", 0)
	for _, n := range inFlight {
		if got := mustGetNotification(t, store, n.ID); got != n || payloadCount(t, store, n.ID) != 1 {
			t.Errorf("关闭后在途通知 = %+v; want 不变 %+v 且保留正文", got, n)
		}
	}
	for _, reply := range replies {
		if got := mustGetReply(t, store, reply.Seq); got.State != queue.Rejected || got.RejectReason != "task_closed" {
			t.Errorf("关闭后回复 %d = %s/%q; want REJECTED/task_closed", reply.Seq, got.State, got.RejectReason)
		}
	}
	requireWALEmpty(t, store, "close")

	applyEvents(t, store, other, task.Fail)
	if got := mustGetNotification(t, store, otherPending.ID); got != otherPending || payloadCount(t, store, otherPending.ID) != 1 {
		t.Errorf("fail 后另一任务的 PENDING 通知 = %+v; want 不变 %+v", got, otherPending)
	}
	requireWALEmpty(t, store, "fail")
}

// TestCloseTaskRollsBackOnNotificationFailure 与 TestTaskEventsRollBackOnFailure 同一写法：在 notifications 上创建
// BEFORE UPDATE … RAISE(ABORT) 触发器后执行 close 失败，任务、回复、事件记录与通知都保持原状；fail 不改动通知，
// 同一触发器下照常成功。
func TestCloseTaskRollsBackOnNotificationFailure(t *testing.T) {
	store, _, running := newNotificationStore(t)
	mustCreateNotification(t, store, notificationFor(running.ID, "pending"))
	enqueueReplies(t, store, running.ID, 2)
	if _, err := store.db.ExecContext(t.Context(), "CREATE TRIGGER fault BEFORE UPDATE ON notifications BEGIN SELECT RAISE(ABORT, 'boom'); END"); err != nil {
		t.Fatalf("创建触发器失败: %v", err)
	}
	replies, notifications, payloads := allReplies(t, store), allNotifications(t, store), payloadRows(t, store)
	before, events := mustGetTask(t, store, running.ID), countRows(t, store.db, "task_events")
	if _, err := store.ApplyTaskEvent(t.Context(), running.ID, running.Version, task.Close); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("close: err = %v; want 含 boom 的错误", err)
	}
	if after := allReplies(t, store); !slices.Equal(after, replies) {
		t.Errorf("回复队列未回滚: %+v; want %+v", after, replies)
	}
	if after := allNotifications(t, store); !slices.Equal(after, notifications) {
		t.Errorf("通知未回滚: %+v; want %+v", after, notifications)
	}
	if after := payloadRows(t, store); !slices.Equal(after, payloads) {
		t.Errorf("正文行未回滚: %q; want %q", after, payloads)
	}
	requireUnchanged(t, store, before, events)

	if _, err := store.ApplyTaskEvent(t.Context(), running.ID, running.Version, task.Fail); err != nil {
		t.Errorf("fail 不应改动通知，却返回错误: %v", err)
	}
	if after := allNotifications(t, store); !slices.Equal(after, notifications) {
		t.Errorf("fail 改动了通知: %+v; want %+v", after, notifications)
	}
}

// TestNotificationsAfterTaskClosed 验证任务关闭后通知不再回到 PENDING：发送中的通知在关闭后确认未投递时改为 ABANDONED(task_closed)，
// 正文行被删除并执行检查点，之后没有可领取的通知；FAILED 任务的通知照常放回；发送中关闭的通知照样记为 SENT；
// 绕过 API 改回 PENDING 的已关闭任务的通知在领取前被放弃，领取返回 ErrNoSendableNotification。
func TestNotificationsAfterTaskClosed(t *testing.T) {
	t.Run("放回", func(t *testing.T) {
		store, _, running := newNotificationStore(t)
		created := mustCreateNotification(t, store, notificationFor(running.ID, "requeue"))
		mustClaimNotification(t, store)
		applyEvents(t, store, running, task.Close)
		got := must(t, "RequeueNotification")(store.RequeueNotification(t.Context(), created.ID, 0))
		if got.State != queue.OutboxAbandoned || got.AbandonReason != "task_closed" {
			t.Errorf("RequeueNotification = %+v; want ABANDONED(task_closed)", got)
		}
		requireNotification(t, store, created.ID, queue.OutboxAbandoned, "task_closed", 0)
		requireWALEmpty(t, store, "RequeueNotification 放弃")
		requireNoSendable(t, store)
	})
	t.Run("FAILED 任务放回", func(t *testing.T) {
		store, _, running := newNotificationStore(t)
		created := mustCreateNotification(t, store, notificationFor(running.ID, "failed"))
		mustClaimNotification(t, store)
		applyEvents(t, store, running, task.Fail)
		if got := must(t, "RequeueNotification")(store.RequeueNotification(t.Context(), created.ID, 0)); got.State != queue.OutboxPending {
			t.Errorf("RequeueNotification = %+v; want PENDING", got)
		}
	})
	t.Run("发送中关闭", func(t *testing.T) {
		store, _, running := newNotificationStore(t)
		created := mustCreateNotification(t, store, notificationFor(running.ID, "sent"))
		mustClaimNotification(t, store)
		applyEvents(t, store, running, task.Close)
		if got := must(t, "MarkNotificationSent")(store.MarkNotificationSent(t.Context(), created.ID)); got.State != queue.OutboxSent {
			t.Errorf("MarkNotificationSent = %+v; want SENT", got)
		}
	})
	t.Run("绕过 API 改回 PENDING", func(t *testing.T) {
		store, _, running := newNotificationStore(t)
		created := mustCreateNotification(t, store, notificationFor(running.ID, "bypass"))
		applyEvents(t, store, running, task.Close)
		if _, err := store.db.ExecContext(t.Context(), "UPDATE notifications SET state = 'PENDING', abandon_reason = NULL WHERE id = ?", created.ID); err != nil {
			t.Fatalf("改回 PENDING 失败: %v", err)
		}
		requireNoSendable(t, store)
		requireNotification(t, store, created.ID, queue.OutboxAbandoned, "task_closed", 0)
		requireWALEmpty(t, store, "领取前放弃")
	})
}

// TestClaimNotificationsOfOpenTasks 验证领取前的 task_closed 兜底只放弃 CLOSED 任务的通知：任务进入 WAITING_INPUT、
// WAITING_APPROVAL、COMPLETED 或 FAILED 后创建的对应事件通知，COMPLETED 时创建、任务随后因回复不确定进入 DELIVERY_UNCERTAIN
// 的 turn_completed 通知，以及 CREATED 任务的通知，按创建顺序依次被领取，标记 SENT 后正文行被删除。
func TestClaimNotificationsOfOpenTasks(t *testing.T) {
	store, _, _ := newNotificationStore(t)
	var ids []int64
	var taskIDs []string
	notify := func(current Task, event string) {
		in := notificationFor(current.ID, event)
		in.Event = event
		ids = append(ids, mustCreateNotification(t, store, in).ID)
		taskIDs = append(taskIDs, current.ID)
	}
	cases := []struct {
		event        task.Event
		notification string
	}{
		{task.InputRequested, "waiting_input"},
		{task.ApprovalRequested, "waiting_approval"},
		{task.TurnCompleted, "turn_completed"},
		{task.Fail, "failed"},
	}
	for _, c := range cases {
		notify(applyEvents(t, store, startTask(t, store), c.event), c.notification)
	}
	completed, _ := completedTask(t, store, 1)
	notify(completed, "turn_completed")
	reply, _ := claim(t, store, completed.ID)
	if _, current := mustMarkUncertain(t, store, reply.Seq); current.State != task.DeliveryUncertain {
		t.Fatalf("MarkReplyUncertain 后任务状态 = %s; want %s", current.State, task.DeliveryUncertain)
	}
	notify(createTask(t, store), "turn_completed")
	for i, id := range ids {
		if claimed, _ := mustClaimNotification(t, store); claimed.ID != id {
			t.Fatalf("第 %d 次领取 = 通知 %d; want %s 任务的通知 %d", i+1, claimed.ID, mustGetTask(t, store, taskIDs[i]).State, id)
		}
		must(t, "MarkNotificationSent")(store.MarkNotificationSent(t.Context(), id))
		requireNotification(t, store, id, queue.OutboxSent, "", 0)
	}
	requireNoSendable(t, store)
}

// TestClaimAbandonsExpiringNotifications 验证领取前放弃令牌将在 10 分钟内过期的 PENDING 通知：有效期 1 小时的通知在时钟推进
// 49 分 59 秒后仍被领取；推进 50 分钟后（距过期恰 10 分钟）改为 ABANDONED(expired)、正文行被删除并执行检查点，
// 领取返回之后创建的通知，只有它时返回 ErrNoSendableNotification。第一个事务读到的时刻距过期还多 1 毫秒、第二个事务读到恰 10 分钟时，
// 该通知既不被放弃也不被领取，下一次领取时放弃。
func TestClaimAbandonsExpiringNotifications(t *testing.T) {
	// hourly 构造任务 taskID 的一条合成通知：令牌有效期 1 小时，label 区分内容。
	hourly := func(taskID, label string) NewNotification {
		in := notificationFor(taskID, label)
		in.TTL = time.Hour
		return in
	}
	t.Run("49 分 59 秒", func(t *testing.T) {
		store, clock, running := newNotificationStore(t)
		created := mustCreateNotification(t, store, hourly(running.ID, "fresh"))
		*clock = clock.Add(49*time.Minute + 59*time.Second)
		if claimed, _ := mustClaimNotification(t, store); claimed.ID != created.ID {
			t.Errorf("领取 = 通知 %d; want %d", claimed.ID, created.ID)
		}
	})
	t.Run("有后来的通知", func(t *testing.T) {
		store, clock, running := newNotificationStore(t)
		expiring := mustCreateNotification(t, store, hourly(running.ID, "expiring"))
		*clock = clock.Add(50 * time.Minute)
		later := mustCreateNotification(t, store, hourly(running.ID, "later"))
		if claimed, _ := mustClaimNotification(t, store); claimed.ID != later.ID {
			t.Errorf("领取 = 通知 %d; want 之后创建的 %d", claimed.ID, later.ID)
		}
		requireNotification(t, store, expiring.ID, queue.OutboxAbandoned, "expired", 0)
		if got := mustGetNotification(t, store, expiring.ID); got.UpdatedAt != clock.UTC() {
			t.Errorf("放弃时间 = %v; want %v", got.UpdatedAt, clock.UTC())
		}
	})
	t.Run("只有将过期的通知", func(t *testing.T) {
		store, clock, running := newNotificationStore(t)
		expiring := mustCreateNotification(t, store, hourly(running.ID, "expiring"))
		*clock = clock.Add(50 * time.Minute)
		requireNoSendable(t, store)
		requireNotification(t, store, expiring.ID, queue.OutboxAbandoned, "expired", 0)
		requireWALEmpty(t, store, "领取前放弃")
	})
	t.Run("两个事务之间进入 10 分钟", func(t *testing.T) {
		store, clock, running := newNotificationStore(t)
		boundary := mustCreateNotification(t, store, hourly(running.ID, "boundary"))
		// 每次读取时钟前进 1 毫秒：领取的第一个事务读到 50 分钟差 1 毫秒，第二个事务读到恰 50 分钟。
		*clock = clock.Add(50*time.Minute - 2*time.Millisecond)
		store.now = func() time.Time {
			*clock = clock.Add(time.Millisecond)
			return *clock
		}
		requireNoSendable(t, store)
		requireNotification(t, store, boundary.ID, queue.OutboxPending, "", 1)
		requireNoSendable(t, store)
		requireNotification(t, store, boundary.ID, queue.OutboxAbandoned, "expired", 0)
	})
}

// TestRecoverSendingNotifications 模拟发送中崩溃：领取后直接关闭存储，重新打开后 RecoverSendingNotifications 把 SENDING 改为
// UNCERTAIN，按 id 返回全部 UNCERTAIN 通知（含崩溃前已不确定的一条），内容保留；再次调用结果相同；UNCERTAIN 不阻塞发送，
// 反复领取直到 ErrNoSendableNotification 的过程中从不返回 UNCERTAIN 通知。
func TestRecoverSendingNotifications(t *testing.T) {
	store, clock, running := newNotificationStore(t)
	earlier := mustCreateNotification(t, store, notificationFor(running.ID, "earlier"))
	mustClaimNotification(t, store)
	must(t, "MarkNotificationUncertain")(store.MarkNotificationUncertain(t.Context(), earlier.ID))
	crashed := mustCreateNotification(t, store, notificationFor(running.ID, "crashed"))
	var pending []int64
	for i := range 2 {
		pending = append(pending, mustCreateNotification(t, store, notificationFor(running.ID, fmt.Sprint("pending ", i))).ID)
	}
	if claimed, _ := mustClaimNotification(t, store); claimed.ID != crashed.ID {
		t.Fatalf("领取 = 通知 %d; want %d", claimed.ID, crashed.ID)
	}
	dir := storeDir(t, store)
	if err := store.Close(); err != nil {
		t.Fatalf("关闭存储失败: %v", err)
	}

	reopened := openNotificationStore(t, dir, clock)
	*clock = clock.Add(time.Minute)
	want := []Notification{mustGetNotification(t, reopened, earlier.ID), mustGetNotification(t, reopened, crashed.ID)}
	want[1].State, want[1].UpdatedAt = queue.OutboxUncertain, clock.UTC()
	for i := range 2 {
		recovered, err := reopened.RecoverSendingNotifications(t.Context())
		if err != nil || !slices.Equal(recovered, want) {
			t.Fatalf("第 %d 次 RecoverSendingNotifications = %+v, %v; want %+v", i+1, recovered, err, want)
		}
		*clock = clock.Add(time.Minute)
	}
	requireNotification(t, reopened, crashed.ID, queue.OutboxUncertain, "", 1)

	var claimedIDs []int64
	for {
		claimed, _, err := reopened.ClaimNextNotification(t.Context())
		if errors.Is(err, ErrNoSendableNotification) {
			break
		}
		if err != nil {
			t.Fatalf("ClaimNextNotification 返回错误: %v", err)
		}
		claimedIDs = append(claimedIDs, claimed.ID)
		must(t, "MarkNotificationSent")(reopened.MarkNotificationSent(t.Context(), claimed.ID))
	}
	if !slices.Equal(claimedIDs, pending) {
		t.Errorf("领取到的通知 = %v; want %v（从不领取 UNCERTAIN）", claimedIDs, pending)
	}
	requireNotification(t, reopened, crashed.ID, queue.OutboxUncertain, "", 1)
	if recovered, err := reopened.RecoverSendingNotifications(t.Context()); err != nil || len(recovered) != 2 {
		t.Errorf("RecoverSendingNotifications = %+v, %v; want 仍为两条 UNCERTAIN", recovered, err)
	}
}

// TestRecordDeliveredMessageID 验证实际投递 Message-ID 的记录：SENT 通知记录成功，同值再次记录不改动，另一个值或已属于另一条通知的值
// 返回 ErrDeliveredMessageIDConflict；非 SENT 通知返回 queue.ErrInvalidOutboxTransition；长度不在 3–998 个字符、含 NUL 或
// 非法 UTF-8 时在开始事务前返回包装 ErrInvalidArgument 的错误，按字符计长。
func TestRecordDeliveredMessageID(t *testing.T) {
	store, clock, running := newNotificationStore(t)
	ctx := t.Context()
	// sentNotification 创建一条通知，领取并标记为 SENT。
	sentNotification := func(label string) Notification {
		created := mustCreateNotification(t, store, notificationFor(running.ID, label))
		mustClaimNotification(t, store)
		return must(t, "MarkNotificationSent")(store.MarkNotificationSent(ctx, created.ID))
	}
	first := sentNotification("first")
	*clock = clock.Add(time.Second)
	recorded := must(t, "RecordDeliveredMessageID")(store.RecordDeliveredMessageID(ctx, first.ID, syntheticDeliveredID))
	want := first
	want.DeliveredMessageID, want.UpdatedAt = syntheticDeliveredID, clock.UTC()
	if recorded != want {
		t.Errorf("RecordDeliveredMessageID = %+v; want %+v", recorded, want)
	}
	*clock = clock.Add(time.Second)
	if again := must(t, "再次 RecordDeliveredMessageID")(store.RecordDeliveredMessageID(ctx, first.ID, syntheticDeliveredID)); again != recorded {
		t.Errorf("同值再次记录 = %+v; want 不改动 %+v", again, recorded)
	}
	if _, err := store.RecordDeliveredMessageID(ctx, first.ID, "<tencent_other@example.invalid>"); !errors.Is(err, ErrDeliveredMessageIDConflict) {
		t.Errorf("另一个值: err = %v; want ErrDeliveredMessageIDConflict", err)
	}
	second := sentNotification("second")
	if _, err := store.RecordDeliveredMessageID(ctx, second.ID, syntheticDeliveredID); !errors.Is(err, ErrDeliveredMessageIDConflict) {
		t.Errorf("已属于另一条通知的值: err = %v; want ErrDeliveredMessageIDConflict", err)
	}
	if got := mustGetNotification(t, store, second.ID); got != second {
		t.Errorf("冲突后第二条通知 = %+v; want 不变 %+v", got, second)
	}
	if got := mustGetNotification(t, store, first.ID); got != recorded {
		t.Errorf("第一条通知 = %+v; want 不变 %+v", got, recorded)
	}

	pending := mustCreateNotification(t, store, notificationFor(running.ID, "pending"))
	if _, err := store.RecordDeliveredMessageID(ctx, pending.ID, "<pending@example.invalid>"); !errors.Is(err, queue.ErrInvalidOutboxTransition) {
		t.Errorf("PENDING 通知: err = %v; want queue.ErrInvalidOutboxTransition", err)
	}
	mustClaimNotification(t, store)
	must(t, "MarkNotificationUncertain")(store.MarkNotificationUncertain(ctx, pending.ID))
	if _, err := store.RecordDeliveredMessageID(ctx, pending.ID, "<uncertain@example.invalid>"); !errors.Is(err, queue.ErrInvalidOutboxTransition) {
		t.Errorf("UNCERTAIN 通知: err = %v; want queue.ErrInvalidOutboxTransition", err)
	}
	if _, err := store.RecordDeliveredMessageID(ctx, 999, "<missing@example.invalid>"); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的通知: err = %v; want ErrNotFound", err)
	}

	// 非法 UTF-8 的两种情形：0xff 不是续字节，SQLite 的 length() 与 Go 的字符数相同，CHECK 约束放行，值会落盘；
	// 0x80 是续字节，length() 不计入，事务开始之后才被 CHECK 拒绝。两者都应在开始事务之前拒绝。
	for _, bad := range invalidMessageIDs {
		if got, err := store.RecordDeliveredMessageID(ctx, second.ID, bad); err == nil || !strings.Contains(err.Error(), "invalid notification") ||
			!errors.Is(err, ErrInvalidArgument) {
			t.Errorf("值 %q: RecordDeliveredMessageID = %+v, %v; want 包装 ErrInvalidArgument、含 \"invalid notification\" 的错误", bad, got, err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if got, err := store.RecordDeliveredMessageID(canceled, second.ID, "<\xff\xfe>"); err == nil ||
		!strings.Contains(err.Error(), "invalid notification") || errors.Is(err, context.Canceled) {
		t.Errorf("上下文已取消: RecordDeliveredMessageID = %+v, %v; want 含 \"invalid notification\" 的错误而不是 context.Canceled", got, err)
	}
	if got := mustGetNotification(t, store, second.ID); got != second {
		t.Errorf("非法取值后第二条通知 = %+v; want 不变 %+v", got, second)
	}
	for _, good := range []string{"<a>", "<" + strings.Repeat("é", 996) + ">"} {
		next := sentNotification(good[:3])
		if got, err := store.RecordDeliveredMessageID(ctx, next.ID, good); err != nil || got.DeliveredMessageID != good {
			t.Errorf("边界值（%d 个字符）: RecordDeliveredMessageID = %+v, %v; want 成功", len([]rune(good)), got, err)
		}
	}
}

// TestClaimNextNotificationAcrossProcesses 用两个独立打开同一数据目录的存储模拟两个进程：每轮 20 个 goroutine 同时领取，
// 恰好 1 个成功，其余都返回 ErrNoSendableNotification，不泄漏 SQLITE_BUSY 等错误；循环 20 轮。
// 最后绕过 API 直接插入第二条 SENDING 通知，违反 notifications_one_sending。
func TestClaimNextNotificationAcrossProcesses(t *testing.T) {
	dir := dataDir(t)
	clock := testClock()
	stores := []*Store{openNotificationStore(t, dir, clock), openNotificationStore(t, dir, clock)}
	registerTestKeys(t, stores[0])
	running := startTask(t, stores[0])
	for round := range 20 {
		// 两个存储的随机源种子相同，只由 stores[0] 创建通知，避免 nid 重复。
		created := mustCreateNotification(t, stores[0], notificationFor(running.ID, fmt.Sprint("round ", round)))
		start := make(chan struct{})
		// result 是一次领取的返回值。
		type result struct {
			id  int64
			err error
		}
		results := make(chan result, 20)
		var wg sync.WaitGroup
		for i := range 20 {
			wg.Go(func() {
				<-start
				claimed, _, err := stores[i%2].ClaimNextNotification(t.Context())
				results <- result{claimed.ID, err}
			})
		}
		close(start)
		wg.Wait()
		close(results)
		succeeded := 0
		for r := range results {
			switch {
			case r.err == nil && r.id == created.ID:
				succeeded++
			case !errors.Is(r.err, ErrNoSendableNotification):
				t.Errorf("第 %d 轮: ClaimNextNotification 返回意外结果: 通知 %d, %v", round, r.id, r.err)
			}
		}
		if succeeded != 1 {
			t.Fatalf("第 %d 轮: %d 次领取成功; want 恰好 1 次", round, succeeded)
		}
		must(t, "MarkNotificationSent")(stores[(round+1)%2].MarkNotificationSent(t.Context(), created.ID))
	}
	mustCreateNotification(t, stores[0], notificationFor(running.ID, "sending"))
	mustClaimNotification(t, stores[1])
	if err := notificationRow(running.ID, 99).with("state", "SENDING").insert(t, stores[0].db, "notifications"); err == nil ||
		!strings.Contains(err.Error(), "UNIQUE constraint failed: notifications.state") {
		t.Errorf("直接插入第二条 SENDING: err = %v; want 违反 notifications_one_sending", err)
	}
}

// TestNotificationOperationsRollBackOnFailure 用测试内创建的触发器让各通知操作的更新失败，或让带状态条件的更新不影响任何行，
// 验证方法返回错误且整个事务回滚：通知与正文行都保持操作前的样子。
func TestNotificationOperationsRollBackOnFailure(t *testing.T) {
	faults := []struct {
		name    string
		trigger string
		want    string
	}{
		{"通知更新失败", "BEFORE UPDATE ON notifications BEGIN SELECT RAISE(ABORT, 'boom'); END", "boom"},
		{"通知更新未命中", "BEFORE UPDATE ON notifications BEGIN SELECT RAISE(IGNORE); END", "changed during update"},
	}
	// claimed 创建并领取一条通知，返回其 id。
	claimed := func(t *testing.T, s *Store, taskID string) int64 {
		created := mustCreateNotification(t, s, notificationFor(taskID, "fault"))
		mustClaimNotification(t, s)
		return created.ID
	}
	// uncertain 创建、领取一条通知并标记为不确定，返回其 id。
	uncertain := func(t *testing.T, s *Store, taskID string) int64 {
		id := claimed(t, s, taskID)
		must(t, "MarkNotificationUncertain")(s.MarkNotificationUncertain(t.Context(), id))
		return id
	}
	operations := []struct {
		name string
		run  func(t *testing.T, s *Store, taskID string) func() error
	}{
		{"ClaimNextNotification", func(t *testing.T, s *Store, taskID string) func() error {
			mustCreateNotification(t, s, notificationFor(taskID, "fault"))
			return func() error { _, _, err := s.ClaimNextNotification(t.Context()); return err }
		}},
		{"MarkNotificationSent", func(t *testing.T, s *Store, taskID string) func() error {
			id := claimed(t, s, taskID)
			return func() error { _, err := s.MarkNotificationSent(t.Context(), id); return err }
		}},
		{"MarkNotificationUncertain", func(t *testing.T, s *Store, taskID string) func() error {
			id := claimed(t, s, taskID)
			return func() error { _, err := s.MarkNotificationUncertain(t.Context(), id); return err }
		}},
		{"RequeueNotification", func(t *testing.T, s *Store, taskID string) func() error {
			id := claimed(t, s, taskID)
			return func() error { _, err := s.RequeueNotification(t.Context(), id, time.Second); return err }
		}},
		{"AbandonNotification", func(t *testing.T, s *Store, taskID string) func() error {
			id := mustCreateNotification(t, s, notificationFor(taskID, "fault")).ID
			return func() error { _, err := s.AbandonNotification(t.Context(), id, "manual"); return err }
		}},
		{"ResolveUncertainNotification(true)", func(t *testing.T, s *Store, taskID string) func() error {
			id := uncertain(t, s, taskID)
			return func() error { _, err := s.ResolveUncertainNotification(t.Context(), id, true); return err }
		}},
		{"ResolveUncertainNotification(false)", func(t *testing.T, s *Store, taskID string) func() error {
			id := uncertain(t, s, taskID)
			return func() error { _, err := s.ResolveUncertainNotification(t.Context(), id, false); return err }
		}},
		{"RecordDeliveredMessageID", func(t *testing.T, s *Store, taskID string) func() error {
			id := claimed(t, s, taskID)
			must(t, "MarkNotificationSent")(s.MarkNotificationSent(t.Context(), id))
			return func() error { _, err := s.RecordDeliveredMessageID(t.Context(), id, syntheticDeliveredID); return err }
		}},
		{"ResolveUncertainAsDelivered", func(t *testing.T, s *Store, taskID string) func() error {
			id := uncertain(t, s, taskID)
			return func() error {
				_, err := s.ResolveUncertainAsDelivered(t.Context(), id, syntheticDeliveredID)
				return err
			}
		}},
		{"RecoverSendingNotifications", func(t *testing.T, s *Store, taskID string) func() error {
			claimed(t, s, taskID)
			return func() error { _, err := s.RecoverSendingNotifications(t.Context()); return err }
		}},
	}
	for _, op := range operations {
		for _, fault := range faults {
			t.Run(op.name+"/"+fault.name, func(t *testing.T) {
				store, _, running := newNotificationStore(t)
				run := op.run(t, store, running.ID)
				if _, err := store.db.ExecContext(t.Context(), "CREATE TRIGGER fault "+fault.trigger); err != nil {
					t.Fatalf("创建触发器失败: %v", err)
				}
				notifications, payloads := allNotifications(t, store), payloadRows(t, store)
				if err := run(); err == nil || !strings.Contains(err.Error(), fault.want) {
					t.Errorf("err = %v; want 含 %q 的错误", err, fault.want)
				}
				if after := allNotifications(t, store); !slices.Equal(after, notifications) {
					t.Errorf("通知未回滚: %+v; want %+v", after, notifications)
				}
				if after := payloadRows(t, store); !slices.Equal(after, payloads) {
					t.Errorf("正文行未回滚: %q; want %q", after, payloads)
				}
			})
		}
	}
}

// TestClaimAbandonRollsBackOnFailure 用只在放弃原因为 expired 或 task_closed 时触发的故障触发器，让领取前放弃（第一个事务）的
// 对应一步失败：领取返回注入的错误且不进入第二个事务；在 task_closed 一步失败时，已由 expired 一步放弃的通知随事务回滚。
// 准备一条距过期恰 10 分钟的通知、一条被绕过 API 改回 PENDING 的已关闭任务的通知和一条正常通知，所有通知与正文行都保持原状。
func TestClaimAbandonRollsBackOnFailure(t *testing.T) {
	for _, reason := range []string{"expired", "task_closed"} {
		t.Run(reason, func(t *testing.T) {
			store, clock, running := newNotificationStore(t)
			expiring := notificationFor(running.ID, "expiring")
			expiring.TTL = time.Hour
			mustCreateNotification(t, store, expiring)
			closed := startTask(t, store)
			bypassed := mustCreateNotification(t, store, notificationFor(closed.ID, "bypass"))
			applyEvents(t, store, closed, task.Close)
			if _, err := store.db.ExecContext(t.Context(), "UPDATE notifications SET state = 'PENDING', abandon_reason = NULL WHERE id = ?", bypassed.ID); err != nil {
				t.Fatalf("改回 PENDING 失败: %v", err)
			}
			*clock = clock.Add(50 * time.Minute)
			mustCreateNotification(t, store, notificationFor(running.ID, "normal"))
			if _, err := store.db.ExecContext(t.Context(), "CREATE TRIGGER fault BEFORE UPDATE ON notifications WHEN NEW.abandon_reason = '"+reason+
				"' BEGIN SELECT RAISE(ABORT, 'boom'); END"); err != nil {
				t.Fatalf("创建触发器失败: %v", err)
			}
			notifications, payloads := allNotifications(t, store), payloadRows(t, store)
			if claimed, content, err := store.ClaimNextNotification(t.Context()); err == nil || !strings.Contains(err.Error(), "boom") {
				t.Errorf("ClaimNextNotification = %+v, %q, %v; want 含 boom 的错误", claimed, content, err)
			}
			if after := allNotifications(t, store); !slices.Equal(after, notifications) {
				t.Errorf("通知未回滚: %+v; want %+v", after, notifications)
			}
			if after := payloadRows(t, store); !slices.Equal(after, payloads) {
				t.Errorf("正文行未回滚: %q; want %q", after, payloads)
			}
		})
	}
}

// TestNotificationOperationsCanceledContext 验证输入合法而上下文已取消时各通知方法返回 context.Canceled（不是
// ErrInvalidArgument：取消不是校验失败），不改动任何数据。
func TestNotificationOperationsCanceledContext(t *testing.T) {
	store, _, running := newNotificationStore(t)
	created := mustCreateNotification(t, store, notificationFor(running.ID, "canceled"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := map[string]func() error{
		"CreateNotification":           func() error { _, err := store.CreateNotification(ctx, notificationFor(running.ID, "x")); return err },
		"ClaimNextNotification":        func() error { _, _, err := store.ClaimNextNotification(ctx); return err },
		"MarkNotificationSent":         func() error { _, err := store.MarkNotificationSent(ctx, created.ID); return err },
		"RequeueNotification":          func() error { _, err := store.RequeueNotification(ctx, created.ID, 0); return err },
		"AbandonNotification":          func() error { _, err := store.AbandonNotification(ctx, created.ID, "manual"); return err },
		"ResolveUncertainNotification": func() error { _, err := store.ResolveUncertainNotification(ctx, created.ID, true); return err },
		"RecordDeliveredMessageID": func() error {
			_, err := store.RecordDeliveredMessageID(ctx, created.ID, syntheticDeliveredID)
			return err
		},
		"ResolveUncertainAsDelivered": func() error {
			_, err := store.ResolveUncertainAsDelivered(ctx, created.ID, syntheticDeliveredID)
			return err
		},
		"RecoverSendingNotifications": func() error { _, err := store.RecoverSendingNotifications(ctx); return err },
		"NotificationByNID":           func() error { _, err := store.NotificationByNID(ctx, created.NID); return err },
		"NotificationByMessageID":     func() error { _, err := store.NotificationByMessageID(ctx, created.MessageID); return err },
		"NotificationByDeliveredID":   func() error { _, err := store.NotificationByDeliveredID(ctx, syntheticDeliveredID); return err },
		"LatestAttemptedNotification": func() error { _, err := store.LatestAttemptedNotification(ctx, running.ID, time.Time{}); return err },
		"LatestSentNotification":      func() error { _, err := store.LatestSentNotification(ctx, running.ID); return err },
		"ThreadReferences":            func() error { _, err := store.ThreadReferences(ctx, running.ID, created.ID, 20); return err },
		"CountNotificationsSince":     func() error { _, err := store.CountNotificationsSince(ctx, time.Time{}); return err },
		"AbandonedSince":              func() error { _, err := store.AbandonedSince(ctx, time.Time{}, 0); return err },
		"SentWithoutDeliveredID":      func() error { _, err := store.SentWithoutDeliveredID(ctx, created.CreatedAt, 0); return err },
		"HasNotifications":            func() error { _, err := store.HasNotifications(ctx); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, context.Canceled) || errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v; want context.Canceled", name, err)
		}
	}
	if got := allNotifications(t, store); !slices.Equal(got, []Notification{created}) {
		t.Errorf("通知 = %+v; want 保持 %+v", got, created)
	}
}

// settleNotification 创建任务 taskID 的一条通知并立即领取，再在当前时钟下标记为 state（SENT 或 UNCERTAIN），返回标记后的快照；
// 调用方须保证没有更早到期的 PENDING 通知，也没有 SENDING 通知，否则领取到的不是它。
func settleNotification(t *testing.T, s *Store, taskID, label string, state queue.OutboxState) Notification {
	t.Helper()
	created := mustCreateNotification(t, s, notificationFor(taskID, label))
	if claimed, _ := mustClaimNotification(t, s); claimed.ID != created.ID {
		t.Fatalf("领取 = 通知 %d; want 刚创建的 %d", claimed.ID, created.ID)
	}
	if state == queue.OutboxSent {
		return must(t, "MarkNotificationSent")(s.MarkNotificationSent(t.Context(), created.ID))
	}
	return must(t, "MarkNotificationUncertain")(s.MarkNotificationUncertain(t.Context(), created.ID))
}

// insertNotificationAs 绕过 API 直接插入任务 taskID 的第 n 条通知行（notificationRow），并按 set 改写其中的列，返回其 id；
// 用于精确构造状态与各时间列。
func insertNotificationAs(t *testing.T, s *Store, taskID string, n int, set columns) int64 {
	t.Helper()
	row := notificationRow(taskID, n)
	for name, value := range set {
		row = row.with(name, value)
	}
	if err := row.insert(t, s.db, "notifications"); err != nil {
		t.Fatalf("插入第 %d 条通知失败: %v", n, err)
	}
	var id int64
	if err := s.db.QueryRowContext(t.Context(), "SELECT id FROM notifications WHERE message_id = ?", fmt.Sprintf("<tc.%d@example.invalid>", n)).Scan(&id); err != nil {
		t.Fatalf("读取第 %d 条通知的 id 失败: %v", n, err)
	}
	return id
}

// TestNotificationByMessageIDAndDeliveredID 验证按我方 Message-ID 与按实际投递 ID 读取通知：命中时返回与按 id 读取相同的快照，
// 未命中返回 ErrNotFound（不是 ErrInvalidArgument）；实际投递 ID 记下之前按它查不到，两个查找都不会命中对方的列。
func TestNotificationByMessageIDAndDeliveredID(t *testing.T) {
	store, _, running := newNotificationStore(t)
	ctx := t.Context()
	sent := settleNotification(t, store, running.ID, "sent", queue.OutboxSent)
	pending := mustCreateNotification(t, store, notificationFor(running.ID, "pending"))
	// requireFound 断言查找返回通知 id 的当前快照。
	requireFound := func(name string, id int64, got Notification, err error) {
		t.Helper()
		if want := mustGetNotification(t, store, id); err != nil || got != want {
			t.Errorf("%s = %+v, %v; want %+v", name, got, err, want)
		}
	}
	// requireNotFound 断言查找返回 ErrNotFound 与零值。
	requireNotFound := func(name string, got Notification, err error) {
		t.Helper()
		if !errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalidArgument) || got != (Notification{}) {
			t.Errorf("%s = %+v, %v; want ErrNotFound", name, got, err)
		}
	}
	got, err := store.NotificationByMessageID(ctx, pending.MessageID)
	requireFound("按 PENDING 通知的 Message-ID", pending.ID, got, err)
	got, err = store.NotificationByMessageID(ctx, sent.MessageID)
	requireFound("按 SENT 通知的 Message-ID", sent.ID, got, err)
	got, err = store.NotificationByDeliveredID(ctx, syntheticDeliveredID)
	requireNotFound("记下实际投递 ID 之前按它查找", got, err)

	must(t, "RecordDeliveredMessageID")(store.RecordDeliveredMessageID(ctx, sent.ID, syntheticDeliveredID))
	got, err = store.NotificationByDeliveredID(ctx, syntheticDeliveredID)
	requireFound("按实际投递 ID", sent.ID, got, err)
	got, err = store.NotificationByDeliveredID(ctx, sent.MessageID)
	requireNotFound("以我方 Message-ID 按实际投递 ID 查找", got, err)
	got, err = store.NotificationByMessageID(ctx, syntheticDeliveredID)
	requireNotFound("以实际投递 ID 按我方 Message-ID 查找", got, err)
	got, err = store.NotificationByMessageID(ctx, "<missing@example.invalid>")
	requireNotFound("未知的 Message-ID", got, err)
}

// TestLatestNotifications 按发出的先后构造同一任务的通知，逐步验证 LatestAttemptedNotification 与 LatestSentNotification：
// 按发出时刻而不是 id 取最晚的一条（较早创建、因重新排队而较晚发出的通知胜出），发出时刻相同时取 id 较大者；UNCERTAIN 按进入
// UNCERTAIN 的时刻参与前者（updated_at 恰为 uncertainAfter 时计入，早于它即不计入，零值时全部计入），从不参与后者；更晚变化的
// PENDING、SENDING 与 ABANDONED 都跳过，其他任务的通知不计入；较新的通知发出之后再凭副本把较早的 UNCERTAIN 通知核对为已送达，
// 两者的结果仍是较新的那条。
func TestLatestNotifications(t *testing.T) {
	store, clock, running := newNotificationStore(t)
	ctx := t.Context()
	// requireLatest 断言 step 这一步两个查询分别返回通知 attempted 与 sent 的当前快照；id 为 0 表示应返回 ErrNotFound。
	requireLatest := func(step string, uncertainAfter time.Time, attempted, sent int64) {
		t.Helper()
		for _, check := range []struct {
			name string
			want int64
			get  func() (Notification, error)
		}{
			{"LatestAttemptedNotification", attempted, func() (Notification, error) {
				return store.LatestAttemptedNotification(ctx, running.ID, uncertainAfter)
			}},
			{"LatestSentNotification", sent, func() (Notification, error) { return store.LatestSentNotification(ctx, running.ID) }},
		} {
			got, err := check.get()
			if check.want == 0 {
				if !errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalidArgument) || got != (Notification{}) {
					t.Errorf("%s: %s = %+v, %v; want ErrNotFound", step, check.name, got, err)
				}
				continue
			}
			if want := mustGetNotification(t, store, check.want); err != nil || got != want {
				t.Errorf("%s: %s = 通知 %d %+v, %v; want 通知 %d", step, check.name, got.ID, got, err, check.want)
			}
		}
	}
	requireLatest("没有通知", time.Time{}, 0, 0)

	// 较早创建的 A 被放回队列，较晚创建的 B 先发出，A 一分钟后才发出：A 的发出时刻最晚。
	a := mustCreateNotification(t, store, notificationFor(running.ID, "a"))
	b := mustCreateNotification(t, store, notificationFor(running.ID, "b"))
	mustClaimNotification(t, store)
	must(t, "RequeueNotification")(store.RequeueNotification(ctx, a.ID, time.Minute))
	if claimed, _ := mustClaimNotification(t, store); claimed.ID != b.ID {
		t.Fatalf("领取 = 通知 %d; want B %d", claimed.ID, b.ID)
	}
	*clock = clock.Add(time.Second)
	must(t, "MarkNotificationSent")(store.MarkNotificationSent(ctx, b.ID))
	requireLatest("只有 B 已发出", time.Time{}, b.ID, b.ID)
	*clock = clock.Add(time.Minute)
	if claimed, _ := mustClaimNotification(t, store); claimed.ID != a.ID {
		t.Fatalf("领取 = 通知 %d; want A %d", claimed.ID, a.ID)
	}
	must(t, "MarkNotificationSent")(store.MarkNotificationSent(ctx, a.ID))
	requireLatest("较早创建的 A 较晚发出", time.Time{}, a.ID, a.ID)

	// C 与 D 在同一毫秒发出：取 id 较大的 D。
	*clock = clock.Add(time.Second)
	settleNotification(t, store, running.ID, "c", queue.OutboxSent)
	d := settleNotification(t, store, running.ID, "d", queue.OutboxSent)
	requireLatest("C 与 D 同时发出", time.Time{}, d.ID, d.ID)

	// E 进入 UNCERTAIN：尚无缺失证据时是「最新通知」，但从不是最新的 SENT 通知。
	*clock = clock.Add(time.Second)
	e := settleNotification(t, store, running.ID, "e", queue.OutboxUncertain)
	enteredAt := e.UpdatedAt
	requireLatest("E 不确定、还没有成功补扫（零值）", time.Time{}, e.ID, d.ID)
	requireLatest("uncertainAfter 早于 E 进入 UNCERTAIN 1 毫秒", enteredAt.Add(-time.Millisecond), e.ID, d.ID)
	requireLatest("uncertainAfter 恰为 E 进入 UNCERTAIN 的时刻", enteredAt, e.ID, d.ID)
	requireLatest("uncertainAfter 晚 1 毫秒（E 已有缺失证据）", enteredAt.Add(time.Millisecond), d.ID, d.ID)

	// 更晚变化的 SENDING、PENDING 与 ABANDONED 都跳过；其他任务更晚发出或不确定的通知不计入。
	*clock = clock.Add(time.Second)
	f := mustCreateNotification(t, store, notificationFor(running.ID, "f"))
	if claimed, _ := mustClaimNotification(t, store); claimed.ID != f.ID {
		t.Fatalf("领取 = 通知 %d; want F %d", claimed.ID, f.ID)
	}
	mustCreateNotification(t, store, notificationFor(running.ID, "g"))
	h := mustCreateNotification(t, store, notificationFor(running.ID, "h"))
	must(t, "AbandonNotification")(store.AbandonNotification(ctx, h.ID, "manual"))
	other := startTask(t, store)
	later := clock.Add(time.Hour).UnixMilli()
	insertNotificationAs(t, store, other.ID, 90, columns{"state": "SENT", "sent_at": later, "updated_at": later})
	insertNotificationAs(t, store, other.ID, 91, columns{"state": "UNCERTAIN", "updated_at": later})
	requireLatest("更晚的 SENDING、PENDING、ABANDONED 与其他任务的通知", time.Time{}, e.ID, d.ID)

	// F 发出后成为最新；此后才把较早的 E 凭副本核对为已送达：E 的 sent_at 沿用进入 UNCERTAIN 的时刻，结果仍是 F。
	*clock = clock.Add(time.Second)
	f = must(t, "MarkNotificationSent")(store.MarkNotificationSent(ctx, f.ID))
	requireLatest("F 发出", time.Time{}, f.ID, f.ID)
	*clock = clock.Add(time.Minute)
	resolved := must(t, "ResolveUncertainAsDelivered")(store.ResolveUncertainAsDelivered(ctx, e.ID, syntheticDeliveredID))
	if resolved.State != queue.OutboxSent || !resolved.SentAt.Equal(enteredAt) {
		t.Fatalf("ResolveUncertainAsDelivered = %+v; want SENT、sent_at 为 %v", resolved, enteredAt)
	}
	requireLatest("较新的 F 发出之后再核对较早的 E", time.Time{}, f.ID, f.ID)
	requireLatest("核对之后缺失证据不再影响 E", clock.Add(time.Hour), f.ID, f.ID)
}

// TestLatestAttemptedNotificationOnlyUncertain 验证任务只有 UNCERTAIN 通知时：尚无缺失证据（零值或 uncertainAfter 恰在进入
// UNCERTAIN 的时刻）返回它，缺失证据成立后返回 ErrNotFound；LatestSentNotification 始终返回 ErrNotFound；其他任务的 SENT 通知
// 不填补这一空缺。
func TestLatestAttemptedNotificationOnlyUncertain(t *testing.T) {
	store, _, running := newNotificationStore(t)
	ctx := t.Context()
	u := settleNotification(t, store, running.ID, "u", queue.OutboxUncertain)
	other := startTask(t, store)
	insertNotificationAs(t, store, other.ID, 90, columns{"state": "SENT", "sent_at": u.UpdatedAt.UnixMilli(), "updated_at": u.UpdatedAt.UnixMilli()})
	for _, after := range []time.Time{{}, u.UpdatedAt} {
		if got, err := store.LatestAttemptedNotification(ctx, running.ID, after); err != nil || got != u {
			t.Errorf("uncertainAfter %v: LatestAttemptedNotification = %+v, %v; want %+v", after, got, err, u)
		}
	}
	if got, err := store.LatestAttemptedNotification(ctx, running.ID, u.UpdatedAt.Add(time.Millisecond)); !errors.Is(err, ErrNotFound) {
		t.Errorf("缺失证据成立后 LatestAttemptedNotification = %+v, %v; want ErrNotFound", got, err)
	}
	if got, err := store.LatestSentNotification(ctx, running.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("LatestSentNotification = %+v, %v; want ErrNotFound", got, err)
	}
}

// TestLatestAttemptedNotificationMixedTie 验证 SENT 与 UNCERTAIN 的发出时刻相同时只按 id 取较大者，不按状态：
// 同一任务中 SENT 在前、UNCERTAIN 在后时取 UNCERTAIN；另一任务中 UNCERTAIN 在前、SENT 在后时取 SENT。
func TestLatestAttemptedNotificationMixedTie(t *testing.T) {
	store, _, running := newNotificationStore(t)
	ctx := t.Context()
	const at = int64(1_000_000)
	insertNotificationAs(t, store, running.ID, 1, columns{"state": "SENT", "sent_at": at, "updated_at": at})
	uncertain := insertNotificationAs(t, store, running.ID, 2, columns{"state": "UNCERTAIN", "updated_at": at})
	if got, err := store.LatestAttemptedNotification(ctx, running.ID, time.Time{}); err != nil || got.ID != uncertain {
		t.Errorf("SENT 在前、UNCERTAIN 在后: LatestAttemptedNotification = 通知 %d, %v; want %d", got.ID, err, uncertain)
	}
	other := startTask(t, store)
	insertNotificationAs(t, store, other.ID, 3, columns{"state": "UNCERTAIN", "updated_at": at})
	sent := insertNotificationAs(t, store, other.ID, 4, columns{"state": "SENT", "sent_at": at, "updated_at": at})
	if got, err := store.LatestAttemptedNotification(ctx, other.ID, time.Time{}); err != nil || got.ID != sent {
		t.Errorf("UNCERTAIN 在前、SENT 在后: LatestAttemptedNotification = 通知 %d, %v; want %d", got.ID, err, sent)
	}
}

// TestLatestAttemptedNotificationUsesEnteredUncertain 验证 UNCERTAIN 通知按进入 UNCERTAIN 的时刻（updated_at）参与排序与缺失证据的判断，
// 而不是创建时刻：A 创建得早、进入 UNCERTAIN 晚于 B 发出，A 是「最新通知」；uncertainAfter 恰为 A 进入 UNCERTAIN 的时刻时 A 仍计入。
func TestLatestAttemptedNotificationUsesEnteredUncertain(t *testing.T) {
	store, _, running := newNotificationStore(t)
	const at = int64(1_000_000)
	a := insertNotificationAs(t, store, running.ID, 1, columns{"state": "UNCERTAIN", "created_at": 10, "token_expires_at": 10 * at, "updated_at": at + 10})
	insertNotificationAs(t, store, running.ID, 2, columns{"state": "SENT", "created_at": 20, "token_expires_at": 10 * at, "sent_at": at, "updated_at": at})
	for _, after := range []time.Time{{}, time.UnixMilli(at + 10)} {
		if got, err := store.LatestAttemptedNotification(t.Context(), running.ID, after); err != nil || got.ID != a {
			t.Errorf("uncertainAfter %v: LatestAttemptedNotification = 通知 %d, %v; want A %d", after, got.ID, err, a)
		}
	}
}

// TestResolveUncertainAsDelivered 验证凭副本核对：UNCERTAIN 通知在一个事务中改为 SENT 并记下实际投递 ID，sent_at 等于进入
// UNCERTAIN 的时刻（核对前的 updated_at）而不是核对时刻，updated_at 为核对时刻；正文行被删除并执行检查点；此后按实际投递 ID
// 能查到它，发信计数按进入 UNCERTAIN 的时刻计。通知不是 UNCERTAIN（PENDING、SENDING、SENT、ABANDONED）时返回
// queue.ErrInvalidOutboxTransition，实际投递 ID 已属于别的通知时返回 ErrDeliveredMessageIDConflict，通知不存在时返回
// ErrNotFound，这些情况都不改动任何通知与正文行；记录实际投递 ID 的一步失败时，状态的改动随事务回滚。
func TestResolveUncertainAsDelivered(t *testing.T) {
	store, clock, running := newNotificationStore(t)
	ctx := t.Context()
	const taken, fresh = "<taken@example.invalid>", "<fresh@example.invalid>"
	// refuse 调用 ResolveUncertainAsDelivered(id, deliveredID)，断言返回零值且全部通知与正文行都没有改动，返回错误供调用方判定。
	refuse := func(name string, id int64, deliveredID string) error {
		t.Helper()
		notifications, payloads := allNotifications(t, store), payloadRows(t, store)
		got, err := store.ResolveUncertainAsDelivered(ctx, id, deliveredID)
		if got != (Notification{}) {
			t.Errorf("%s: ResolveUncertainAsDelivered = %+v; want 零值", name, got)
		}
		if after := allNotifications(t, store); !slices.Equal(after, notifications) {
			t.Errorf("%s: 通知被改动: %+v; want %+v", name, after, notifications)
		}
		if after := payloadRows(t, store); !slices.Equal(after, payloads) {
			t.Errorf("%s: 正文行被改动: %q; want %q", name, after, payloads)
		}
		return err
	}
	// requireRefused 在 refuse 之外断言错误包装 want，返回错误供调用方进一步断言。
	requireRefused := func(name string, id int64, deliveredID string, want error) error {
		t.Helper()
		err := refuse(name, id, deliveredID)
		if !errors.Is(err, want) {
			t.Errorf("%s: err = %v; want %v", name, err, want)
		}
		return err
	}
	// requireCounts 断言发信计数：从进入 UNCERTAIN 的时刻起为 1，晚 1 毫秒起为 0。
	requireCounts := func(step string, enteredAt time.Time) {
		t.Helper()
		for since, want := range map[time.Time]int{enteredAt: 1, enteredAt.Add(time.Millisecond): 0} {
			if got, err := store.CountNotificationsSince(ctx, since); err != nil || got != want {
				t.Errorf("%s: CountNotificationsSince(%v) = %d, %v; want %d", step, since, got, err, want)
			}
		}
	}

	sent := settleNotification(t, store, running.ID, "sent", queue.OutboxSent)
	must(t, "RecordDeliveredMessageID")(store.RecordDeliveredMessageID(ctx, sent.ID, taken))
	*clock = clock.Add(time.Second)
	uncertain := settleNotification(t, store, running.ID, "uncertain", queue.OutboxUncertain)
	enteredAt := uncertain.UpdatedAt
	abandoned := mustCreateNotification(t, store, notificationFor(running.ID, "abandoned"))
	must(t, "AbandonNotification")(store.AbandonNotification(ctx, abandoned.ID, "manual"))
	sending := mustCreateNotification(t, store, notificationFor(running.ID, "sending"))
	mustClaimNotification(t, store)
	pending := mustCreateNotification(t, store, notificationFor(running.ID, "pending"))
	for _, tt := range []struct {
		name string
		id   int64
	}{{"PENDING", pending.ID}, {"SENDING", sending.ID}, {"SENT", sent.ID}, {"ABANDONED", abandoned.ID}} {
		requireRefused(tt.name, tt.id, fresh, queue.ErrInvalidOutboxTransition)
	}
	if err := requireRefused("实际投递 ID 已属于别的通知", uncertain.ID, taken, ErrDeliveredMessageIDConflict); err != nil &&
		strings.Contains(err.Error(), taken) {
		t.Errorf("冲突的错误文本回显了 Message-ID: %v", err)
	}
	requireRefused("通知不存在", 999, fresh, ErrNotFound)
	requireCounts("核对之前", enteredAt)

	if _, err := store.db.ExecContext(ctx, "CREATE TRIGGER fault BEFORE UPDATE OF delivered_message_id ON notifications BEGIN SELECT RAISE(ABORT, 'boom'); END"); err != nil {
		t.Fatalf("创建触发器失败: %v", err)
	}
	if err := refuse("记录实际投递 ID 的一步失败", uncertain.ID, fresh); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("记录实际投递 ID 的一步失败: err = %v; want 含 boom 的错误", err)
	}
	if _, err := store.db.ExecContext(ctx, "DROP TRIGGER fault"); err != nil {
		t.Fatalf("删除触发器失败: %v", err)
	}

	*clock = clock.Add(time.Minute)
	got := must(t, "ResolveUncertainAsDelivered")(store.ResolveUncertainAsDelivered(ctx, uncertain.ID, syntheticDeliveredID))
	want := uncertain
	want.State, want.DeliveredMessageID, want.SentAt, want.UpdatedAt = queue.OutboxSent, syntheticDeliveredID, enteredAt, clock.UTC()
	if got != want {
		t.Errorf("ResolveUncertainAsDelivered = %+v; want %+v", got, want)
	}
	requireNotification(t, store, uncertain.ID, queue.OutboxSent, "", 0)
	requireWALEmpty(t, store, "ResolveUncertainAsDelivered")
	if byDelivered, err := store.NotificationByDeliveredID(ctx, syntheticDeliveredID); err != nil || byDelivered != want {
		t.Errorf("NotificationByDeliveredID = %+v, %v; want %+v", byDelivered, err, want)
	}
	requireCounts("核对之后", enteredAt)
	requireRefused("已核对为送达的通知", uncertain.ID, fresh, queue.ErrInvalidOutboxTransition)
}

// TestResolveUncertainAsDeliveredKeepsEnteredTime 验证核对为已送达时 sent_at 取进入 UNCERTAIN 的时刻，而不是创建时刻、
// 领取时刻或核对时刻：四个时刻各相差一分钟。
func TestResolveUncertainAsDeliveredKeepsEnteredTime(t *testing.T) {
	store, clock, running := newNotificationStore(t)
	ctx := t.Context()
	created := mustCreateNotification(t, store, notificationFor(running.ID, "late"))
	*clock = clock.Add(time.Minute)
	mustClaimNotification(t, store)
	*clock = clock.Add(time.Minute)
	uncertain := must(t, "MarkNotificationUncertain")(store.MarkNotificationUncertain(ctx, created.ID))
	*clock = clock.Add(time.Minute)
	got := must(t, "ResolveUncertainAsDelivered")(store.ResolveUncertainAsDelivered(ctx, created.ID, syntheticDeliveredID))
	if !got.SentAt.Equal(uncertain.UpdatedAt) || got.SentAt.Equal(created.CreatedAt) {
		t.Errorf("sent_at = %v; want 进入 UNCERTAIN 的时刻 %v（创建于 %v）", got.SentAt, uncertain.UpdatedAt, created.CreatedAt)
	}
}

// TestResolveUncertainNotificationKeepsSendOrder 验证本地核对为已送达时与 D7 的副本核对一样，sent_at 取进入 UNCERTAIN 的时刻而不是
// 创建时刻或核对时刻，发出顺序与发信计数都不随核对而变：A 创建一分钟后进入 UNCERTAIN，再过一分钟 B 发出，又过一分钟人工把 A 核对为
// 已送达；此后最新的 SENT 通知与「最新通知」仍是 B（对 B 的回复不会被误判为 token_superseded），A 的 updated_at 为核对时刻，
// 从核对时刻起的发信计数为 0（A 不在核对时刻再计一次），从 A 进入 UNCERTAIN 的时刻起为 2。
func TestResolveUncertainNotificationKeepsSendOrder(t *testing.T) {
	store, clock, running := newNotificationStore(t)
	ctx := t.Context()
	a := mustCreateNotification(t, store, notificationFor(running.ID, "a"))
	*clock = clock.Add(time.Minute)
	mustClaimNotification(t, store)
	a = must(t, "MarkNotificationUncertain")(store.MarkNotificationUncertain(ctx, a.ID))
	*clock = clock.Add(time.Minute)
	b := settleNotification(t, store, running.ID, "b", queue.OutboxSent)
	*clock = clock.Add(time.Minute)
	resolvedAt := clock.UTC()
	resolved := must(t, "ResolveUncertainNotification(true)")(store.ResolveUncertainNotification(ctx, a.ID, true))
	if resolved.State != queue.OutboxSent || !resolved.SentAt.Equal(a.UpdatedAt) || !resolved.UpdatedAt.Equal(resolvedAt) {
		t.Errorf("ResolveUncertainNotification(true) = %+v; want SENT、sent_at 为进入 UNCERTAIN 的时刻 %v、updated_at 为核对时刻 %v",
			resolved, a.UpdatedAt, resolvedAt)
	}
	for name, latest := range map[string]func() (Notification, error){
		"LatestSentNotification":      func() (Notification, error) { return store.LatestSentNotification(ctx, running.ID) },
		"LatestAttemptedNotification": func() (Notification, error) { return store.LatestAttemptedNotification(ctx, running.ID, time.Time{}) },
	} {
		if got, err := latest(); err != nil || got.ID != b.ID {
			t.Errorf("人工核对较早的 A 之后 %s = 通知 %d, %v; want B %d", name, got.ID, err, b.ID)
		}
	}
	for since, want := range map[time.Time]int{a.UpdatedAt: 2, resolvedAt: 0} {
		if got, err := store.CountNotificationsSince(ctx, since); err != nil || got != want {
			t.Errorf("CountNotificationsSince(%v) = %d, %v; want %d", since, got, err, want)
		}
	}
}

// TestThreadReferences 用直接插入的通知验证线程引用：只取该任务中 id 小于 beforeID、已记下实际投递 ID 的 SENT 通知，按 sent_at
// 升序（相同时按 id）返回最后 limit 个；没有实际投递 ID 的 SENT 通知、UNCERTAIN 通知与其他任务的通知都不在其中；
// 有 21 个可引用的通知时 limit 20 返回最近的 20 个。
func TestThreadReferences(t *testing.T) {
	store, _, running := newNotificationStore(t)
	ctx := t.Context()
	other := startTask(t, store)
	// sent 直接插入任务 taskID 的第 n 条 SENT 通知，发出时刻为 sentAt；delivered 为空时不记实际投递 ID。返回其 id。
	sent := func(t *testing.T, s *Store, taskID string, n int, sentAt int64, delivered string) int64 {
		t.Helper()
		set := columns{"state": "SENT", "sent_at": sentAt}
		if delivered != "" {
			set["delivered_message_id"] = delivered
		}
		return insertNotificationAs(t, s, taskID, n, set)
	}
	const d1, d2, d5, d6, d7, d8 = "<d1@example.invalid>", "<d2@example.invalid>", "<d5@example.invalid>", "<d6@example.invalid>",
		"<d7@example.invalid>", "<d8@example.invalid>"
	id1 := sent(t, store, running.ID, 1, 300, d1)
	sent(t, store, running.ID, 2, 100, d2)
	sent(t, store, running.ID, 3, 200, "")
	insertNotificationAs(t, store, running.ID, 4, columns{"state": "UNCERTAIN", "updated_at": 500})
	id5 := sent(t, store, running.ID, 5, 200, d5)
	sent(t, store, running.ID, 6, 400, d6)
	sent(t, store, other.ID, 7, 250, d7)
	id8 := sent(t, store, running.ID, 8, 400, d8)
	tests := []struct {
		name     string
		taskID   string
		beforeID int64
		limit    int
		want     []string
	}{
		{"全部：按 sent_at 升序，相同时按 id", running.ID, id8 + 1, 20, []string{d2, d5, d1, d6, d8}},
		{"不含 beforeID 本身", running.ID, id8, 20, []string{d2, d5, d1, d6}},
		{"只取 id 更小的", running.ID, id5, 20, []string{d2, d1}},
		{"limit 2 取最后两个", running.ID, id8 + 1, 2, []string{d6, d8}},
		{"limit 1", running.ID, id8 + 1, 1, []string{d8}},
		{"最早的通知之前没有引用", running.ID, id1, 20, nil},
		{"其他任务", other.ID, id8 + 1, 20, []string{d7}},
		{"没有通知的任务", "0000000000", id8 + 1, 20, nil},
	}
	for _, tt := range tests {
		if got, err := store.ThreadReferences(ctx, tt.taskID, tt.beforeID, tt.limit); err != nil || !slices.Equal(got, tt.want) {
			t.Errorf("%s: ThreadReferences = %q, %v; want %q", tt.name, got, err, tt.want)
		}
	}

	t.Run("至多 20 个", func(t *testing.T) {
		store, _, running := newNotificationStore(t)
		var want []string
		for n := 1; n <= 21; n++ {
			delivered := fmt.Sprintf("<m%d@example.invalid>", n)
			sent(t, store, running.ID, n, int64(n)*10, delivered)
			if n > 1 {
				want = append(want, delivered)
			}
		}
		if got, err := store.ThreadReferences(t.Context(), running.ID, 100, 20); err != nil || !slices.Equal(got, want) {
			t.Errorf("ThreadReferences = %q, %v; want 最近的 20 个 %q", got, err, want)
		}
	})
}

// TestCountNotificationsSince 用直接插入的通知验证发信计数：只计 SENT 与 UNCERTAIN，按 COALESCE(sent_at, updated_at) 不早于 since
// 计（按毫秒，恰在 since 的计入）；SENT 按 sent_at 而不是 updated_at 计（记下实际投递 ID 会改 updated_at）；PENDING、SENDING 与
// ABANDONED 不论何时变化都不计。
func TestCountNotificationsSince(t *testing.T) {
	store, _, running := newNotificationStore(t)
	const at = int64(1_000_000)
	for n, set := range map[int]columns{
		1: {"state": "SENT", "sent_at": at, "updated_at": at + 5000},
		2: {"state": "SENT", "sent_at": at - 1, "updated_at": at + 10},
		3: {"state": "UNCERTAIN", "updated_at": at},
		4: {"updated_at": at + 100},
		5: {"state": "SENDING", "updated_at": at + 100},
		6: {"state": "ABANDONED", "abandon_reason": "expired", "updated_at": at + 100},
	} {
		insertNotificationAs(t, store, running.ID, n, set)
	}
	for _, tt := range []struct {
		name  string
		since time.Time
		want  int
	}{
		{"零值", time.Time{}, 3},
		{"早 1 毫秒", time.UnixMilli(at - 1), 3},
		{"恰在边界", time.UnixMilli(at), 2},
		{"晚 1 毫秒", time.UnixMilli(at + 1), 0},
	} {
		if got, err := store.CountNotificationsSince(t.Context(), tt.since); err != nil || got != tt.want {
			t.Errorf("%s: CountNotificationsSince = %d, %v; want %d", tt.name, got, err, tt.want)
		}
	}
}

// bulkNotification 绕过 API 直接插入任务 taskID 的第 n 条通知并按 set 改写其中的列，返回其 id；nid 改为 n 的 12 位十进制文本，
// 使 n 可以超过 255（notificationRow 以 n 的一个字节重复 12 次作为 nid）。set 不能为 nil。
func bulkNotification(t *testing.T, s *Store, taskID string, n int, set columns) int64 {
	t.Helper()
	return insertNotificationAs(t, s, taskID, n, set.with("nid", []byte(fmt.Sprintf("%012d", n))))
}

// drainAbandoned 按发送循环的做法逐页取完 AbandonedSince：从游标 (since, afterID) 起，以每页最后一行的 (updated_at, id) 作为下一页的
// 游标，返回满 100 条时继续取，直到不足 100 条；返回各页的条数与按顺序拼接的通知 id。取了 10 页仍未取完时终止测试，
// 游标不前进的实现因此不会让测试一直循环。
func drainAbandoned(t *testing.T, s *Store, since time.Time, afterID int64) (sizes []int, ids []int64) {
	t.Helper()
	for {
		if len(sizes) == 10 {
			t.Fatalf("AbandonedSince 取了 10 页仍未取完: 各页条数 %v", sizes)
		}
		page, err := s.AbandonedSince(t.Context(), since, afterID)
		if err != nil {
			t.Fatalf("AbandonedSince 返回错误: %v", err)
		}
		sizes = append(sizes, len(page))
		for _, n := range page {
			ids = append(ids, n.ID)
		}
		if len(page) < 100 {
			return sizes, ids
		}
		since, afterID = page[len(page)-1].UpdatedAt, page[len(page)-1].ID
	}
}

// drainSentWithoutDelivered 按「已发送」副本处理的做法逐页取完 SentWithoutDeliveredID(sentBefore, afterID)：afterID 从 0 起，以每页
// 最后一行的 id 作为下一页的 afterID，返回满 100 条时继续取，直到不足 100 条；返回各页的条数与按顺序拼接的通知 id。
// 取了 10 页仍未取完时终止测试。
func drainSentWithoutDelivered(t *testing.T, s *Store, sentBefore time.Time) (sizes []int, ids []int64) {
	t.Helper()
	var afterID int64
	for {
		if len(sizes) == 10 {
			t.Fatalf("SentWithoutDeliveredID 取了 10 页仍未取完: 各页条数 %v", sizes)
		}
		page, err := s.SentWithoutDeliveredID(t.Context(), sentBefore, afterID)
		if err != nil {
			t.Fatalf("SentWithoutDeliveredID 返回错误: %v", err)
		}
		sizes = append(sizes, len(page))
		for _, n := range page {
			ids = append(ids, n.ID)
		}
		if len(page) < 100 {
			return sizes, ids
		}
		afterID = page[len(page)-1].ID
	}
}

// TestAbandonedSince 用直接插入的通知验证：只返回原因为 expired 或 task_closed 的 ABANDONED 通知中位于游标 (since, afterID) 之后的——
// updated_at 晚于 since，或恰为 since（按毫秒）且 id 大于 afterID——按 (updated_at, id) 升序而不是按 id，快照与按 id 读取的相同；
// afterID 只作用于 since 那一毫秒：等于其中某行的 id 时不再返回该行，大于其中全部的 id 时那一毫秒一行都不返回，而更晚放弃的通知
// 不论 id 大小都返回；rejected、manual 与其他状态的通知不返回。另按调用方的做法逐页取完：250 条分三页、关闭任务时同一毫秒放弃的
// 150 条分两页，都不重不漏。
func TestAbandonedSince(t *testing.T) {
	store, _, running := newNotificationStore(t)
	ctx := t.Context()
	const at = int64(1_000_000)
	// abandoned 直接插入第 n 条原因为 reason、放弃于 updatedAt 的通知，返回其 id。
	abandoned := func(n int, reason string, updatedAt int64) int64 {
		t.Helper()
		return insertNotificationAs(t, store, running.ID, n, columns{"state": "ABANDONED", "abandon_reason": reason, "updated_at": updatedAt})
	}
	closed := abandoned(1, "task_closed", at+1)
	expired := abandoned(2, "expired", at)
	abandoned(3, "rejected", at+1)
	abandoned(4, "manual", at+1)
	older := abandoned(5, "expired", at-1)
	insertNotificationAs(t, store, running.ID, 6, columns{"updated_at": at + 1})
	insertNotificationAs(t, store, running.ID, 7, columns{"state": "SENT", "sent_at": at + 1, "updated_at": at + 1})
	sameMillisecond := abandoned(8, "task_closed", at)
	for _, tt := range []struct {
		name    string
		since   time.Time
		afterID int64
		want    []int64
	}{
		{"零值：按 (updated_at, id) 而不是按 id 排序", time.Time{}, 0, []int64{older, expired, sameMillisecond, closed}},
		{"恰在 since 的那一毫秒全部返回", time.UnixMilli(at), 0, []int64{expired, sameMillisecond, closed}},
		{"afterID 等于那一毫秒中一行的 id：不再返回该行", time.UnixMilli(at), expired, []int64{sameMillisecond, closed}},
		{"afterID 等于那一毫秒中最大的 id", time.UnixMilli(at), sameMillisecond, []int64{closed}},
		{"afterID 大于那一毫秒中全部的 id：更晚放弃、id 更小的照样返回", time.UnixMilli(at), sameMillisecond + 100, []int64{closed}},
		{"晚 1 毫秒", time.UnixMilli(at + 1), 0, []int64{closed}},
		{"晚 1 毫秒且 afterID 等于其 id", time.UnixMilli(at + 1), closed, nil},
		{"晚于全部", time.UnixMilli(at + 2), 0, nil},
	} {
		var want []Notification
		for _, id := range tt.want {
			want = append(want, mustGetNotification(t, store, id))
		}
		if got, err := store.AbandonedSince(ctx, tt.since, tt.afterID); err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: AbandonedSince = %+v, %v; want %+v", tt.name, got, err, want)
		}
	}

	t.Run("逐页取完 250 条", func(t *testing.T) {
		store, _, running := newNotificationStore(t)
		// row 是一条应返回的通知：放弃时刻与 id。
		type row struct{ updatedAt, id int64 }
		var want []row
		for n := 1; n <= 300; n++ {
			// 放弃时刻分散在 60 个毫秒中，每一毫秒 5 条，先后与 id 的先后不同；每 6 条中有 1 条原因为 rejected，不返回。
			updatedAt, reason := at+int64(n*37%60), "expired"
			switch {
			case n%6 == 0:
				reason = "rejected"
			case n%2 == 0:
				reason = "task_closed"
			}
			id := bulkNotification(t, store, running.ID, n, columns{"state": "ABANDONED", "abandon_reason": reason, "updated_at": updatedAt})
			if reason != "rejected" {
				want = append(want, row{updatedAt, id})
			}
		}
		slices.SortFunc(want, func(a, b row) int { return cmp.Or(cmp.Compare(a.updatedAt, b.updatedAt), cmp.Compare(a.id, b.id)) })
		var wantIDs []int64
		for _, r := range want {
			wantIDs = append(wantIDs, r.id)
		}
		if sizes, got := drainAbandoned(t, store, time.Time{}, 0); !slices.Equal(sizes, []int{100, 100, 50}) || !slices.Equal(got, wantIDs) {
			t.Errorf("各页条数 %v，id %v; want [100 100 50]，按 (updated_at, id) 排列的 %v", sizes, got, wantIDs)
		}
	})

	t.Run("关闭任务时同一毫秒放弃的 150 条", func(t *testing.T) {
		store, _, running := newNotificationStore(t)
		var want []int64
		for i := range 150 {
			want = append(want, mustCreateNotification(t, store, notificationFor(running.ID, fmt.Sprint(i))).ID)
		}
		applyEvents(t, store, running, task.Close)
		if got := dumpRows(t, store.db, "SELECT DISTINCT state, abandon_reason, updated_at FROM notifications"); len(got) != 1 {
			t.Fatalf("关闭任务后的通知 = %q; want 同一毫秒放弃的 ABANDONED(task_closed)", got)
		}
		if sizes, got := drainAbandoned(t, store, time.Time{}, 0); !slices.Equal(sizes, []int{100, 50}) || !slices.Equal(got, want) {
			t.Errorf("各页条数 %v，id %v; want [100 50]，按 id 排列的 %v", sizes, got, want)
		}
	})
}

// TestSentWithoutDeliveredID 用直接插入的通知验证：只返回 sent_at 早于 sentBefore（恰在 sentBefore 的不返回）、仍没有实际投递 ID、
// id 大于 afterID 的 SENT 通知，按 id 升序而不是按 sent_at，快照与按 id 读取的相同；afterID 等于某行的 id 时不再返回该行，等于或
// 大于最大的 id 时返回空；已有实际投递 ID 的、UNCERTAIN 与 PENDING 通知不返回。另按调用方的做法逐页取完：250 条分三页、同一毫秒
// 发出的 150 条分两页，都不重不漏。
func TestSentWithoutDeliveredID(t *testing.T) {
	store, _, running := newNotificationStore(t)
	ctx := t.Context()
	const at = int64(1_000_000)
	// sentAt 直接插入第 n 条发出于 when 的 SENT 通知（delivered 为空时不记实际投递 ID），返回其 id。
	sentAt := func(n int, when int64, delivered string) int64 {
		t.Helper()
		set := columns{"state": "SENT", "sent_at": when, "updated_at": when}
		if delivered != "" {
			set["delivered_message_id"] = delivered
		}
		return insertNotificationAs(t, store, running.ID, n, set)
	}
	late := sentAt(1, at-1, "")
	early := sentAt(2, at-100, "")
	boundary := sentAt(3, at, "")
	sentAt(4, at-10, syntheticDeliveredID)
	insertNotificationAs(t, store, running.ID, 5, columns{"state": "UNCERTAIN", "updated_at": at - 10})
	insertNotificationAs(t, store, running.ID, 6, columns{"updated_at": at - 10})
	for _, tt := range []struct {
		name    string
		before  time.Time
		afterID int64
		want    []int64
	}{
		{"恰在边界的不返回，按 id 而不是按 sent_at 升序", time.UnixMilli(at), 0, []int64{late, early}},
		{"晚 1 毫秒", time.UnixMilli(at + 1), 0, []int64{late, early, boundary}},
		{"只剩更早的", time.UnixMilli(at - 1), 0, []int64{early}},
		{"早于全部", time.UnixMilli(at - 100), 0, nil},
		{"afterID 等于一行的 id：不再返回该行", time.UnixMilli(at + 1), late, []int64{early, boundary}},
		{"afterID 与 sentBefore 同时生效", time.UnixMilli(at), late, []int64{early}},
		{"afterID 等于最大的 id", time.UnixMilli(at + 1), boundary, nil},
		{"afterID 大于全部的 id", time.UnixMilli(at + 1), boundary + 100, nil},
	} {
		var want []Notification
		for _, id := range tt.want {
			want = append(want, mustGetNotification(t, store, id))
		}
		if got, err := store.SentWithoutDeliveredID(ctx, tt.before, tt.afterID); err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: SentWithoutDeliveredID = %+v, %v; want %+v", tt.name, got, err, want)
		}
	}

	t.Run("逐页取完 250 条", func(t *testing.T) {
		store, _, running := newNotificationStore(t)
		var want []int64
		for n := 1; n <= 300; n++ {
			// 发出时刻分散在 60 个毫秒中，先后与 id 的先后不同；每 6 条中有 1 条已记下实际投递 ID，不返回。
			set := columns{"state": "SENT", "sent_at": at - int64(n*37%60), "updated_at": at}
			if n%6 == 0 {
				set["delivered_message_id"] = fmt.Sprintf("<d%d@example.invalid>", n)
			}
			if id := bulkNotification(t, store, running.ID, n, set); n%6 != 0 {
				want = append(want, id)
			}
		}
		if sizes, got := drainSentWithoutDelivered(t, store, time.UnixMilli(at+1)); !slices.Equal(sizes, []int{100, 100, 50}) || !slices.Equal(got, want) {
			t.Errorf("各页条数 %v，id %v; want [100 100 50]，按 id 排列的 %v", sizes, got, want)
		}
	})

	t.Run("同一毫秒发出的 150 条", func(t *testing.T) {
		store, _, running := newNotificationStore(t)
		var want []int64
		for n := 1; n <= 150; n++ {
			want = append(want, bulkNotification(t, store, running.ID, n, columns{"state": "SENT", "sent_at": at, "updated_at": at}))
		}
		if sizes, got := drainSentWithoutDelivered(t, store, time.UnixMilli(at+1)); !slices.Equal(sizes, []int{100, 50}) || !slices.Equal(got, want) {
			t.Errorf("各页条数 %v，id %v; want [100 50]，按 id 排列的 %v", sizes, got, want)
		}
	})
}

// TestHasNotifications 验证库中从未有过通知时为 false；创建一条之后为 true，它被放弃之后仍为 true（「有过」而不是「现有待发」）。
func TestHasNotifications(t *testing.T) {
	store, _, running := newNotificationStore(t)
	ctx := t.Context()
	if has, err := store.HasNotifications(ctx); err != nil || has {
		t.Fatalf("空库 HasNotifications = %t, %v; want false", has, err)
	}
	created := mustCreateNotification(t, store, notificationFor(running.ID, "first"))
	if has, err := store.HasNotifications(ctx); err != nil || !has {
		t.Errorf("创建后 HasNotifications = %t, %v; want true", has, err)
	}
	must(t, "AbandonNotification")(store.AbandonNotification(ctx, created.ID, "manual"))
	if has, err := store.HasNotifications(ctx); err != nil || !has {
		t.Errorf("放弃后 HasNotifications = %t, %v; want true", has, err)
	}
}
