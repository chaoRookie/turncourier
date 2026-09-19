// Package sqlite 的待发通知测试用临时目录中的真实 SQLite 数据库验证通知的创建与加密、字段校验、领取顺序、
// 无法读出内容时不领取、投递结果、放弃、任务关闭前后的处理、即将过期的放弃、崩溃后的恢复、实际投递 Message-ID 的记录、
// 失败回滚，以及两个存储实例的串行领取。
package sqlite

import (
	"bytes"
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

// TestCreateNotificationValidation 验证字段校验在事务开始前拒绝非法输入：错误文本含 "invalid notification"，
// 上下文已取消时仍返回校验错误，且不写入任何行；各字段边界上的合法值可以创建。
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
		if err == nil || !strings.Contains(err.Error(), "invalid notification") {
			t.Errorf("%s: CreateNotification = %+v, %v; want 含 \"invalid notification\" 的错误", tt.name, got, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	in := notificationFor(running.ID, "canceled")
	in.TTL = 0
	if got, err := store.CreateNotification(ctx, in); err == nil || !strings.Contains(err.Error(), "invalid notification") || errors.Is(err, context.Canceled) {
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
// queue.ErrInvalidOutboxTransition；各操作只接受契约规定的来源状态；UNCERTAIN 保留正文，核对为已投递时改为 SENT，
// 核对为未投递时回到 PENDING 并能再次领取到相同内容，任务已关闭时改为 ABANDONED(task_closed)；放回队列的延迟须在 0 到 24 小时。
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
	if resolved.State != queue.OutboxSent || resolved.SentAt != clock.UTC() {
		t.Errorf("ResolveUncertainNotification(true) = %+v; want SENT at %v", resolved, clock.UTC())
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
		if got, err := store.RequeueNotification(ctx, retried.ID, delay); err == nil || !strings.Contains(err.Error(), "invalid notification") {
			t.Errorf("延迟 %v: RequeueNotification = %+v, %v; want 含 \"invalid notification\" 的错误", delay, got, err)
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

// TestNotificationValidationBeforeTransaction 验证放回队列的延迟、放弃原因与实际投递 Message-ID 的校验先于开始事务：
// 上下文已取消时仍返回 "invalid notification" 校验错误，而不是 context.Canceled。
func TestNotificationValidationBeforeTransaction(t *testing.T) {
	store, _, _ := newNotificationStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := map[string]func() (Notification, error){
		"RequeueNotification":      func() (Notification, error) { return store.RequeueNotification(ctx, 1, -time.Second) },
		"AbandonNotification":      func() (Notification, error) { return store.AbandonNotification(ctx, 1, "expired") },
		"RecordDeliveredMessageID": func() (Notification, error) { return store.RecordDeliveredMessageID(ctx, 1, "<a") },
	}
	for name, call := range calls {
		if got, err := call(); err == nil || !strings.Contains(err.Error(), "invalid notification") || errors.Is(err, context.Canceled) {
			t.Errorf("%s = %+v, %v; want 含 \"invalid notification\" 的错误而不是 context.Canceled", name, got, err)
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
					if got, err := store.AbandonNotification(t.Context(), created.ID, bad); err == nil || !strings.Contains(err.Error(), "invalid notification") {
						t.Errorf("原因 %q: AbandonNotification = %+v, %v; want 含 \"invalid notification\" 的错误", bad, got, err)
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
// 返回 ErrDeliveredMessageIDConflict；非 SENT 通知返回 queue.ErrInvalidOutboxTransition；长度不在 3–998 个字符或含 NUL 时
// 在开始事务前报错，按字符计长。
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

	for _, bad := range []string{"", "<a", "<é", "<a\x00b>", "<" + strings.Repeat("a", 997) + ">"} {
		if got, err := store.RecordDeliveredMessageID(ctx, second.ID, bad); err == nil || !strings.Contains(err.Error(), "invalid notification") {
			t.Errorf("值 %q: RecordDeliveredMessageID = %+v, %v; want 含 \"invalid notification\" 的错误", bad, got, err)
		}
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

// TestNotificationOperationsCanceledContext 验证上下文已取消时各通知方法返回 context.Canceled，不改动任何数据。
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
		"RecoverSendingNotifications": func() error { _, err := store.RecoverSendingNotifications(ctx); return err },
		"NotificationByNID":           func() error { _, err := store.NotificationByNID(ctx, created.NID); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, context.Canceled) {
			t.Errorf("%s: err = %v; want context.Canceled", name, err)
		}
	}
	if got := allNotifications(t, store); !slices.Equal(got, []Notification{created}) {
		t.Errorf("通知 = %+v; want 保持 %+v", got, created)
	}
}
