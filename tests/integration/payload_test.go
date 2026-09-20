// Package integration_test 组合示例配置、临时目录中的真实 SQLite 存储、回复令牌与正文加密，验证待发通知、令牌、键控摘要、
// 回复正文密文与派发的组合生命周期；只用合成数据，不访问网络与钥匙串。
package integration_test

import (
	"bytes"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/chaoRookie/turncourier/internal/queue"
	"github.com/chaoRookie/turncourier/internal/security/token"
	"github.com/chaoRookie/turncourier/internal/store/sqlite"
	"github.com/chaoRookie/turncourier/internal/task"
)

// syntheticDeliveredID 是合成的实际投递 Message-ID，形如 QQ 改写后的 ID。
const syntheticDeliveredID = "<tencent_synthetic@example.invalid>"

// countRows 以独立连接打开数据库文件 path，返回 table 的行数；表名只来自测试代码中的常量。存储不提供读取正文行的接口，
// 集成测试只能这样观察正文行是否已被删除。
func countRows(t *testing.T, path, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("统计 %s 行数失败: %v", table, err)
	}
	return n
}

// claimsOf 返回通知行给出的令牌 Claims：任务、owner 与令牌有效期。
func claimsOf(n sqlite.Notification) token.Claims {
	return token.Claims{TaskID: n.TaskID, Owner: n.Owner, ExpiresAt: n.TokenExpiresAt}
}

// TestNotificationReplyPayloadLifecycle 按 4b 的调用方式串起通知与回复：创建 turn_completed 通知，用通知行的 nid 与 Claims 签发令牌，
// 令牌在当前时间有效、在有效期时刻过期；领取通知得到原内容，标记 SENT 并记录实际投递 ID 后，按 nid 取回的 Claims 与签发时一致；
// 以令牌密钥的键控摘要记录回复，派发得到原正文，确认后正文行消失；同一封信再次记录为重复，同一 Message-ID 换了正文则冲突；
// 任务关闭后令牌本身仍能通过验证，而新回复记为 REJECTED(task_closed)，撤销由任务状态承担。
func TestNotificationReplyPayloadLifecycle(t *testing.T) {
	ctx := t.Context()
	cfg := loadExampleConfig(t)
	store := openStore(t, cfg.Paths.DataDir)
	if _, _, err := store.EnsureInstance(ctx); err != nil {
		t.Fatalf("EnsureInstance 返回错误: %v", err)
	}
	registerKeys(t, store)
	tokenKey := testTokenKey()
	created, err := store.CreateTask(ctx, sqlite.AgentCodex)
	if err != nil {
		t.Fatalf("CreateTask 返回错误: %v", err)
	}
	id := created.ID
	started, err := store.StartTask(ctx, id, created.Version, "thread-synthetic-0001")
	if err != nil {
		t.Fatalf("StartTask 返回错误: %v", err)
	}
	applyEvent(t, store, started, task.TurnCompleted, task.Completed)

	content := []byte("synthetic notification content")
	notification, err := store.CreateNotification(ctx, sqlite.NewNotification{
		TaskID: id, Event: "turn_completed", Domain: "example.invalid", TTL: cfg.Security.TokenTTL, Content: content,
	})
	if err != nil {
		t.Fatalf("CreateNotification 返回错误: %v", err)
	}
	if notification.TokenKeyID != tokenKey.ID() {
		t.Fatalf("通知的令牌 kid = %d; want %d", notification.TokenKeyID, tokenKey.ID())
	}
	claims := claimsOf(notification)
	issued, err := token.Issue(tokenKey, token.NID(notification.NID), claims)
	if err != nil {
		t.Fatalf("token.Issue 返回错误: %v", err)
	}
	parsed, err := token.Parse(issued.Reveal())
	if err != nil || parsed != issued {
		t.Fatalf("token.Parse(Reveal()) = %v, %v; want 与签发的令牌相同", parsed, err)
	}
	if err := token.Verify(tokenKey, parsed, claims, time.Now()); err != nil {
		t.Errorf("当前时间 token.Verify 返回错误: %v", err)
	}
	if err := token.Verify(tokenKey, parsed, claims, notification.TokenExpiresAt); !errors.Is(err, token.ErrExpired) {
		t.Errorf("有效期时刻 token.Verify = %v; want token.ErrExpired", err)
	}

	claimed, got, err := store.ClaimNextNotification(ctx)
	if err != nil || claimed.ID != notification.ID || !bytes.Equal(got, content) {
		t.Fatalf("ClaimNextNotification = %+v, %q, %v; want 通知 %d 与原内容", claimed, got, err, notification.ID)
	}
	if _, err := store.MarkNotificationSent(ctx, notification.ID); err != nil {
		t.Fatalf("MarkNotificationSent 返回错误: %v", err)
	}
	if _, err := store.RecordDeliveredMessageID(ctx, notification.ID, syntheticDeliveredID); err != nil {
		t.Fatalf("RecordDeliveredMessageID 返回错误: %v", err)
	}
	byNID, err := store.NotificationByNID(ctx, parsed.NID())
	if err != nil {
		t.Fatalf("NotificationByNID 返回错误: %v", err)
	}
	if got := claimsOf(byNID); got.TaskID != claims.TaskID || got.Owner != claims.Owner || !got.ExpiresAt.Equal(claims.ExpiresAt) {
		t.Errorf("按 nid 取回的 Claims = %+v; want 签发时的 %+v", got, claims)
	}
	if byNID.ID != notification.ID || byNID.TokenKeyID != parsed.KeyID() || byNID.DeliveredMessageID != syntheticDeliveredID {
		t.Errorf("NotificationByNID = %+v; want 通知 %d、令牌 kid %d、实际投递 ID %s", byNID, notification.ID, parsed.KeyID(), syntheticDeliveredID)
	}

	first := syntheticReply(id, cfg.Mailbox.Address, 1, "<first@example.invalid>", "first synthetic reply")
	if want := tokenKey.BodyDigest([]byte("first synthetic reply")); first.BodyDigest != want {
		t.Fatalf("合成回复的摘要不是令牌密钥的键控摘要")
	}
	recorded, err := store.RecordReply(ctx, first)
	if err != nil || recorded.Duplicate || recorded.Reply.State != queue.Queued {
		t.Fatalf("RecordReply = %+v, %v; want 新入队 QUEUED", recorded, err)
	}
	reply, _, body, err := store.ClaimNextReply(ctx, id)
	if err != nil || reply.Seq != recorded.Reply.Seq || !bytes.Equal(body, first.Body) {
		t.Fatalf("ClaimNextReply = %+v, %q, %v; want 回复 %d 与原正文", reply, body, err, recorded.Reply.Seq)
	}
	if countRows(t, cfg.Paths.Database, "reply_payloads") != 1 {
		t.Errorf("派发后 reply_payloads 应保留 1 行正文")
	}
	if _, _, err := store.AcknowledgeReply(ctx, reply.Seq); err != nil {
		t.Fatalf("AcknowledgeReply 返回错误: %v", err)
	}
	if n := countRows(t, cfg.Paths.Database, "reply_payloads"); n != 0 {
		t.Errorf("确认后 reply_payloads 行数 = %d; want 0", n)
	}

	if again, err := store.RecordReply(ctx, first); err != nil || !again.Duplicate || again.Reply.Seq != recorded.Reply.Seq {
		t.Errorf("再次记录同一封信 = %+v, %v; want Duplicate=true、seq %d", again, err, recorded.Reply.Seq)
	}
	changed := syntheticReply(id, cfg.Mailbox.Address, 1, "<first@example.invalid>", "changed synthetic reply")
	if got, err := store.RecordReply(ctx, changed); !errors.Is(err, sqlite.ErrMessageConflict) {
		t.Errorf("同一 Message-ID 换了正文: RecordReply = %+v, %v; want ErrMessageConflict", got, err)
	}

	current, err := store.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask 返回错误: %v", err)
	}
	applyEvent(t, store, current, task.Close, task.Closed)
	if err := token.Verify(tokenKey, parsed, claimsOf(byNID), time.Now()); err != nil {
		t.Errorf("任务关闭后 token.Verify 返回错误: %v; want 令牌本身仍有效", err)
	}
	late, err := store.RecordReply(ctx, syntheticReply(id, cfg.Mailbox.Address, 2, "<late@example.invalid>", "late synthetic reply"))
	if err != nil || late.Duplicate || late.Reply.State != queue.Rejected || late.Reply.RejectReason != "task_closed" {
		t.Fatalf("任务关闭后 RecordReply = %+v, %v; want REJECTED(task_closed)", late, err)
	}
	if n := countRows(t, cfg.Paths.Database, "reply_payloads"); n != 0 {
		t.Errorf("被拒绝的回复写入了正文: reply_payloads 行数 = %d; want 0", n)
	}
}
