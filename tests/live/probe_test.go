//go:build live

// Package live_test 的 L1 探测项：记录 IMAP 能力与文件夹、发出主题标签携带一次性令牌的合成通知并找回各来源的实际投递 ID、
// 收取并脱敏各客户端的回复，以及分三个独立会话观测 IDLE 的推送与断开。
// 每封邮件发出前都经 /dev/tty 逐封确认，确认文本只显示主题标签之后的文字；对机器人邮箱只做只读访问，样本中只有结构特征。
package live_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chaoRookie/turncourier/internal/mail/imap"
	"github.com/chaoRookie/turncourier/internal/mail/smtp"
	"github.com/chaoRookie/turncourier/internal/security/token"
	"github.com/chaoRookie/turncourier/tests/live"
)

const (
	// copyWindow 是发信后查找「已发送」与抄送副本的时间窗。
	copyWindow = 60 * time.Second
	// copyPoll 是两次查找副本之间的间隔。
	copyPoll = 5 * time.Second
	// idleCheckTimeout 是预检与 FETCH 检查中一次 Idle 的期限。
	idleCheckTimeout = 10 * time.Second
	// selfSendDelay 是测量会话第一次 Idle 开始后、自发邮件发出的等待时间。
	selfSendDelay = 10 * time.Second
)

// capabilityRecord 是 TestL1Capabilities 写入样本的内容。
type capabilityRecord struct {
	Capabilities []string          `json:"capabilities"`
	Folders      []string          `json:"folders"`
	UIDValidity  map[string]uint32 `json:"uid_validity"`
	Messages     map[string]uint32 `json:"messages"`
}

// sendRecord 是 TestL1SendNotification 写入样本的内容：第几封、状态、两处副本的对照结果与脱敏后的 DATA 响应。
// SentCopyAfterReject 只在服务器于结束标记处拒收（smtp.ErrRejected）且「已发送」可读时设置：拒收之后「已发送」中
// 有没有这封邮件的副本，用来核对 D7 的前提「QQ 只为接受了的邮件写入「已发送」副本」；其余情况不输出该字段。
type sendRecord struct {
	Index               int          `json:"index"`
	Total               int          `json:"total"`
	Status              string       `json:"status"`
	Error               string       `json:"error,omitempty"`
	SentCopy            *copyRecord  `json:"sent_copy,omitempty"`
	SentCopyAfterReject *copyRecord  `json:"sent_copy_after_reject,omitempty"`
	CCCopy              *copyRecord  `json:"cc_copy,omitempty"`
	DataResponse        string       `json:"data_response,omitempty"`
	DataIDs             []string     `json:"data_ids,omitempty"`
	Cursors             []cursorInfo `json:"cursors,omitempty"`
}

// cursorInfo 记录发信前某个文件夹的游标，便于复核副本是在发信之后出现的。
type cursorInfo struct {
	Folder      string `json:"folder"`
	UIDValidity uint32 `json:"uid_validity"`
	LastUID     uint32 `json:"last_uid"`
}

// copyRecord 是「已发送」或抄送副本的对照结果：是否找到、其 Message-ID 的来源归类，以及 X-OQ-MSGID 是否等于我方 ID。
// Searched 表示补扫正常结束：找到了副本，或在等待窗口内每次补扫都成功而一直没有找到。补扫出错或探测被取消时为 false，
// 此时 found 为 false 不能说明副本不存在（例如不能当作 D7 前提成立的证据）。
type copyRecord struct {
	Searched            bool   `json:"searched"`
	Found               bool   `json:"found"`
	MessageIDSource     string `json:"message_id_source"`
	MessageIDEqualsOurs bool   `json:"message_id_equals_ours"`
	OQPresent           bool   `json:"x_oq_msgid_present"`
	OQEqualsOurs        bool   `json:"x_oq_msgid_equals_ours"`
}

// idleCheckRecord 是一次 10 秒 Idle 观测的原始值与判定。
type idleCheckRecord struct {
	Result         string         `json:"result"`
	Check          live.IdleCheck `json:"check"`
	UIDNextMissing bool           `json:"uidnext_missing,omitempty"`
	Error          string         `json:"error,omitempty"`
}

// idleEvent 是测量会话中一次 Idle 返回：相对开始时间的秒数与返回类型。
type idleEvent struct {
	AtSeconds float64 `json:"at_s"`
	Type      string  `json:"type"`
	Error     string  `json:"error,omitempty"`
}

// selfSendRecord 是自发邮件的结果，以及从 250 响应到该次 Idle 返回 true 的延迟。
type selfSendRecord struct {
	Status        string `json:"status"`
	LatencyMillis int64  `json:"latency_ms,omitempty"`
	Error         string `json:"error,omitempty"`
}

// measureRecord 是测量会话的统计：时长、每次 Idle 返回的事件与自发邮件的结果。
type measureRecord struct {
	Minutes  int             `json:"minutes"`
	Events   []idleEvent     `json:"events"`
	SelfSend *selfSendRecord `json:"self_send,omitempty"`
}

// idleRecord 是 TestL1Idle 写入样本的内容：预检、测量、FETCH 检查各自的结果与综合结论。
type idleRecord struct {
	Preflight  idleCheckRecord  `json:"preflight"`
	Measure    measureRecord    `json:"measure"`
	FetchCheck *idleCheckRecord `json:"fetch_check,omitempty"`
	Conclusion string           `json:"exists_attached_to"`
}

// selfSendResult 是自发邮件在后台完成后送回的结果：250 响应的时刻与错误。
type selfSendResult struct {
	at  time.Time
	err error
}

// TestL1Capabilities 登录 IMAP，记录登录后的能力、文件夹列表，以及 INBOX、Junk、Sent Messages 的 UIDVALIDITY。
// 各文件夹的 UIDVALIDITY 是否相同只作记录：0002 已把文件夹纳入 inbound_messages 的 UID 唯一键，相同也不会撞键。本探测不发信。
func TestL1Capabilities(t *testing.T) {
	ctx, cancel := probeTimeout(3 * time.Minute)
	defer cancel()
	session := dialIMAP(ctx, t, imap.Timeouts{})
	folders, err := session.ListFolders(ctx)
	if err != nil {
		t.Fatalf("列出文件夹失败：%v", err)
	}
	data := capabilityRecord{
		Capabilities: session.Capabilities(),
		Folders:      folders,
		UIDValidity:  map[string]uint32{},
		Messages:     map[string]uint32{},
	}
	for _, folder := range []string{imap.FolderInbox, imap.FolderJunk, sentFolder} {
		box, err := session.Examine(ctx, folder)
		if err != nil {
			t.Logf("EXAMINE %s 失败（可能不存在）：%v", folder, err)
			continue
		}
		data.UIDValidity[folder], data.Messages[folder] = box.UIDValidity, box.Messages
	}
	t.Logf("能力：%s", strings.Join(data.Capabilities, " "))
	t.Logf("文件夹：%s", strings.Join(data.Folders, " | "))
	t.Logf("UIDVALIDITY：%v", data.UIDValidity)
	record(t, "capabilities", data)
}

// TestL1SendNotification 发出 TURNCOURIER_LIVE_SEND_COUNT（默认 1，至多 5）封合成通知给配置中的接收地址：
// 主题为 D4 的新形态标签 [TC <任务 ID> <令牌>] 加标题，页脚照旧保留一份令牌。每封发出前逐项确认，确认文本只显示标签之后的文字；
// 发出后在 60 秒内以只读方式查找「已发送」与（设置 TURNCOURIER_LIVE_CC_BOT=1 时）抄送副本，
// 把各来源的 ID 与一次性令牌写入 state.json（SubjectToken 记为 true），DATA 的 250 响应脱敏后写入样本。
// 服务器在结束标记处拒收（smtp.ErrRejected）时，同样在 60 秒内以只读方式查找「已发送」副本，对照结果记入样本的
// sent_copy_after_reject，用来核对 D7 的前提「QQ 只为接受了的邮件写入「已发送」副本」，随后照旧记录失败并终止。
func TestL1SendNotification(t *testing.T) {
	count := intEnv(t, sendCountEnv, 1, 1, 5)
	ccBot := os.Getenv(ccBotEnv) == "1"
	key := oneShotKey(t)
	state := loadState(t)
	for index := 1; index <= count; index++ {
		sendOne(t, &state, key, index, count, ccBot)
	}
}

// sendOne 发出第 index 封合成通知并记录结果：先逐项确认（文本由 live.SendPrompt 渲染，不含主题标签，令牌不进入 /dev/tty），
// 再记下发信前两个文件夹的游标，发送后查找副本。结果不确定（ErrUncertain）时同样不重发，只记录状态。
// 服务器在结束标记处拒收（ErrRejected）时，仍从发信前记下的游标起以只读方式查找「已发送」副本（至多 60 秒），
// 结果记入 sent_copy_after_reject：找到即说明 D7 的前提「QQ 只为接受了的邮件写入副本」不成立。拒收的邮件没有送达，不写入 state.json。
// 令牌文本同时写入主题标签与页脚：主题标签是 D4 中唯一的验证输入，页脚只是给人看的副本。
func sendOne(t *testing.T, state *live.State, key *token.Key, index, total int, ccBot bool) {
	t.Helper()
	ctx, cancel := probeTimeout(10 * time.Minute)
	defer cancel()
	bot, recipient := input.cfg.Mailbox.Address, input.cfg.Recipient.Address
	mail := live.Mail{
		ProbeID:   randomID(t, 16),
		TaskID:    randomID(t, 10),
		MessageID: "<tc." + randomID(t, 24) + "@" + domainOf(bot) + ">",
	}
	nid, err := token.NewNID(rand.Reader)
	if err != nil {
		t.Fatalf("生成通知 ID 失败：%v", err)
	}
	issued, err := token.Issue(key, nid, token.Claims{TaskID: mail.TaskID, Owner: probeOwner, ExpiresAt: time.Now().Add(input.cfg.Security.TokenTTL)})
	if err != nil {
		t.Fatalf("签发合成令牌失败：%v", err)
	}
	mail.Token = issued.Reveal()
	mail.SubjectToken = true
	// 标题与 4b 通知的长度相近：B 编码后被拆成多个 encoded-word，L1b 复核因此也覆盖标题部分的折行。
	title := fmt.Sprintf("TurnCourier L1b 探测 %d/%d：新主题格式复核，请用不同客户端直接回复本邮件", index, total)
	notification := live.Notification{
		From: bot, To: recipient, Tag: live.SubjectTag(mail.TaskID, mail.Token), Title: title,
		MessageID: mail.MessageID, ProbeID: mail.ProbeID, Token: mail.Token, Date: time.Now(),
	}
	recipients := []string{recipient}
	if ccBot {
		notification.CC = bot
		recipients = append(recipients, bot)
	}
	message, err := live.ComposeNotification(notification)
	if err != nil {
		t.Fatalf("渲染通知失败：%v", err)
	}
	if !confirm(t, live.SendPrompt(bot, recipient, index, total, title, ccBot)) {
		record(t, "send", sendRecord{Index: index, Total: total, Status: "skipped"})
		t.Logf("第 %d 封已跳过", index)
		return
	}

	result := sendRecord{Index: index, Total: total, Status: "sent"}
	session := dialIMAP(ctx, t, imap.Timeouts{})
	// 每封邮件用一条连接，发完即关，不让连接在两次逐封确认之间长时间空闲。
	defer session.Close()
	sentCursor, sentOK := cursorAt(ctx, t, session, sentFolder)
	inboxCursor, inboxOK := live.Cursor{}, false
	if ccBot {
		inboxCursor, inboxOK = cursorAt(ctx, t, session, imap.FolderInbox)
	}
	if sentOK {
		result.Cursors = append(result.Cursors, cursorInfo{Folder: sentFolder, UIDValidity: sentCursor.UIDValidity, LastUID: sentCursor.LastUID})
	}
	if inboxOK {
		result.Cursors = append(result.Cursors, cursorInfo{Folder: imap.FolderInbox, UIDValidity: inboxCursor.UIDValidity, LastUID: inboxCursor.LastUID})
	}

	response, sendErr := smtp.Send(ctx, smtpConfig(), probePassword(t), smtp.Envelope{From: bot, To: recipients}, message)
	if sendErr != nil {
		result.Status, result.Error = "failed", sendErr.Error()
		switch {
		case errors.Is(sendErr, smtp.ErrUncertain):
			// 已进入提交阶段但没有得到响应，邮件可能已投递：先把我方 ID、探测 ID、任务 ID 与令牌记入 state.json，
			// 否则对方回复这封邮件时 TestL1Replies 认不出它，样本也就丢失。没有 DATA 响应，候选为空数组。
			result.Status = "uncertain"
			mail.DeliveredData = []string{}
			state.Mails = append(state.Mails, mail)
			saveState(t, *state)
		case errors.Is(sendErr, smtp.ErrRejected) && sentOK:
			// 服务器对结束标记回复了 4xx/5xx（例如收件人不存在），确定没有投递。D7 凭「已发送」副本把 UNCERTAIN 核对为已送达，
			// 前提是 QQ 只为接受了的邮件写入副本；这里照样以只读方式查找一次，找到即说明前提不成立。
			_, result.SentCopyAfterReject = findCopy(ctx, t, session, sentFolder, sentCursor, mail)
		case errors.Is(sendErr, smtp.ErrRejected):
			t.Logf("「已发送」无法读取，没有核对拒收之后是否出现副本")
		}
		record(t, "send", result)
		t.Fatalf("发送失败：%v", sendErr)
	}
	if sentOK {
		mail.DeliveredSent, result.SentCopy = findCopy(ctx, t, session, sentFolder, sentCursor, mail)
	}
	if inboxOK {
		mail.DeliveredCC, result.CCCopy = findCopy(ctx, t, session, imap.FolderInbox, inboxCursor, mail)
	}
	known := append(slices.Clone(state.Mails), mail)
	result.DataResponse, result.DataIDs = live.RedactDataResponse(response.Response, known)
	mail.DeliveredData = result.DataIDs
	state.Mails = append(state.Mails, mail)
	saveState(t, *state)
	record(t, "send", result)
	t.Logf("第 %d 封已发出：DATA 响应 %q，候选 ID %v", index, result.DataResponse, result.DataIDs)
}

// oneShotKey 生成一把用后即弃的令牌签名密钥：它只存在于本次探测进程内，签出的令牌不能用于任何真实验证。
func oneShotKey(t *testing.T) *token.Key {
	t.Helper()
	secret := make([]byte, token.KeyLen)
	if _, err := rand.Read(secret); err != nil {
		t.Fatalf("读取随机数失败：%v", err)
	}
	key, err := token.NewKey(1, secret)
	if err != nil {
		t.Fatalf("构造合成密钥失败：%v", err)
	}
	return key
}

// domainOf 取地址中 @ 之后的域名，用于拼出我方 Message-ID。
func domainOf(address string) string {
	_, domain, _ := strings.Cut(address, "@")
	return domain
}

// cursorAt 以 EXAMINE 读取文件夹当前的 UIDVALIDITY 与 UIDNEXT，构造只覆盖此后新邮件的游标；
// 文件夹不存在或服务器未报告 UIDNEXT 时从 UID 1 开始（服务器未报告时无法只取新邮件）。
func cursorAt(ctx context.Context, t *testing.T, session *imap.Session, folder string) (live.Cursor, bool) {
	t.Helper()
	box, err := session.Examine(ctx, folder)
	if err != nil {
		t.Logf("EXAMINE %s 失败（可能不存在）：%v", folder, err)
		return live.Cursor{}, false
	}
	cursor := live.Cursor{UIDValidity: box.UIDValidity}
	if box.UIDNext > 0 {
		cursor.LastUID = box.UIDNext - 1
	}
	return cursor, true
}

// findCopy 在 60 秒内以只读方式补扫 folder，按探测 ID 头找回同一封邮件的副本，返回它的 Message-ID 与对照结果。
// 只读取头部用于对照，不输出邮件内容。
func findCopy(ctx context.Context, t *testing.T, session *imap.Session, folder string, cursor live.Cursor, mail live.Mail) (string, *copyRecord) {
	t.Helper()
	deadline := time.Now().Add(copyWindow)
	for {
		batch, err := session.Scan(ctx, folder, imap.Cursor(cursor))
		if err != nil {
			t.Logf("补扫 %s 失败：%v", folder, err)
			return "", &copyRecord{}
		}
		for _, message := range batch.Messages {
			if message.Raw == nil {
				continue
			}
			copied, err := live.CopyHeaders(message.Raw)
			if err != nil || copied.ProbeID != mail.ProbeID {
				continue
			}
			return copied.MessageID, &copyRecord{
				Searched:            true,
				Found:               true,
				MessageIDSource:     live.ClassifyID(copied.MessageID, []live.Mail{mail}),
				MessageIDEqualsOurs: copied.MessageID == mail.MessageID,
				OQPresent:           copied.OQMsgID != "",
				OQEqualsOurs:        copied.OQMsgID == mail.MessageID,
			}
		}
		cursor = live.Cursor(batch.Next)
		if batch.More {
			continue
		}
		if time.Now().After(deadline) {
			t.Logf("%s 中未找到本封邮件的副本", folder)
			return "", &copyRecord{Searched: true}
		}
		select {
		case <-time.After(copyPoll):
		case <-ctx.Done():
			return "", &copyRecord{}
		}
	}
}

// TestL1Replies 读取 state.json，以只读方式补扫 Junk 与 INBOX（游标存于输出目录），
// 对引用了探测邮件的来信输出脱敏样本；自动回复与退信同样输出，kind 为 auto 或 bounce。本探测不发信。
func TestL1Replies(t *testing.T) {
	state := loadState(t)
	if len(state.Mails) == 0 {
		t.Skip("state.json 中没有探测邮件，请先运行 TestL1SendNotification")
	}
	ctx, cancel := probeTimeout(15 * time.Minute)
	defer cancel()
	session := dialIMAP(ctx, t, imap.Timeouts{})
	folders, err := session.ListFolders(ctx)
	if err != nil {
		t.Fatalf("列出文件夹失败：%v", err)
	}
	roles := live.Roles{
		Bot:       input.cfg.Mailbox.Address,
		Recipient: input.cfg.Recipient.Address,
		Allowed:   input.cfg.Recipient.AllowedSenders,
	}
	targets := []string{}
	if slices.Contains(folders, imap.FolderJunk) {
		targets = append(targets, imap.FolderJunk)
	}
	targets = append(targets, imap.FolderInbox)
	total := 0
	for _, folder := range targets {
		total += scanFolder(ctx, t, session, &state, folder, roles)
	}
	t.Logf("本次输出 %d 条来信样本", total)
}

// scanFolder 补扫一个文件夹并输出样本，每批处理完就把游标写回状态文件；返回本文件夹输出的样本数。
func scanFolder(ctx context.Context, t *testing.T, session *imap.Session, state *live.State, folder string, roles live.Roles) int {
	t.Helper()
	count := 0
	cursor := state.Cursors[folder]
	for {
		batch, err := session.Scan(ctx, folder, imap.Cursor(cursor))
		if err != nil {
			t.Fatalf("补扫 %s 失败：%v", folder, err)
		}
		for _, message := range batch.Messages {
			if message.Raw == nil {
				t.Logf("%s 中 UID %d 的邮件过大（%d 字节），跳过", folder, message.UID, message.Size)
				continue
			}
			sample, ok, err := live.Analyze(message.Raw, *state, roles)
			if err != nil {
				t.Logf("%s 中 UID %d 无法解析：%v", folder, message.UID, err)
				continue
			}
			if !ok {
				continue
			}
			sample.Folder = folder
			if err := live.AppendSample(input.out, sample); err != nil {
				t.Fatalf("写入样本失败：%v", err)
			}
			count++
		}
		cursor = live.Cursor(batch.Next)
		if state.Cursors == nil {
			state.Cursors = map[string]live.Cursor{}
		}
		state.Cursors[folder] = cursor
		saveState(t, *state)
		if !batch.More {
			return count
		}
	}
}

// TestL1Idle 分三个独立会话观测 IDLE：预检会话判断 UID SEARCH 的响应是否附带 EXISTS；测量会话连续 IDLE，
// 记录每次推送与服务器断开的时刻；设置 TURNCOURIER_LIVE_IDLE_SELF_SEND=1 时，测量中自发一封邮件并记录推送延迟，
// 测量结束后再用 FETCH 检查会话分辨 EXISTS 是随 UID SEARCH 还是随 UID FETCH 附带。
func TestL1Idle(t *testing.T) {
	minutes := intEnv(t, idleMinutesEnv, 30, 1, 60)
	selfSend := os.Getenv(idleSelfSendEnv) == "1"
	preflight, cursor, hasCursor := idlePreflight(t)
	t.Logf("预检结论：%s（%+v）", preflight.Result, preflight.Check)

	data := idleRecord{Preflight: preflight, Measure: idleMeasure(t, minutes, selfSend)}
	if selfSend && data.Measure.SelfSend != nil && data.Measure.SelfSend.Status == "sent" {
		check := idleFetchCheck(t, cursor, hasCursor)
		data.FetchCheck = &check
		data.Conclusion = live.IdleConclusion(preflight.Result, check.Result)
	} else {
		data.Conclusion = live.IdleConclusion(preflight.Result, "")
	}
	record(t, "idle", data)
	t.Logf("IDLE 结论：%s；推送事件 %d 次", data.Conclusion, len(data.Measure.Events))
}

// idlePreflight 是预检会话：Examine 记下邮件数 N0 与 UIDNEXT U0，以 U0−1 构造游标（不取回已有邮件）后连续 Scan 两次，
// 再 Examine 记下 N1、U1，随后以 10 秒期限调用一次 Idle 并判定。U0 为 0 时记为 uidnext_missing，结论为未知，
// 并跳过其余预检步骤，不以回绕的游标继续。返回的游标供 FETCH 检查会话使用。
func idlePreflight(t *testing.T) (idleCheckRecord, live.Cursor, bool) {
	t.Helper()
	ctx, cancel := probeTimeout(5 * time.Minute)
	defer cancel()
	session := dialIMAP(ctx, t, imap.Timeouts{})
	defer session.Close()
	before, err := session.Examine(ctx, imap.FolderInbox)
	if err != nil {
		return idleCheckRecord{Result: "unknown", Error: err.Error()}, live.Cursor{}, false
	}
	if before.UIDNext == 0 {
		return idleCheckRecord{Result: "unknown", UIDNextMissing: true}, live.Cursor{}, false
	}
	cursor := live.Cursor{UIDValidity: before.UIDValidity, LastUID: before.UIDNext - 1}
	scanned := cursor
	for range 2 {
		batch, err := session.Scan(ctx, imap.FolderInbox, imap.Cursor(scanned))
		if err != nil {
			return idleCheckRecord{Result: "unknown", Error: err.Error()}, cursor, true
		}
		scanned = live.Cursor(batch.Next)
	}
	check, err := idleAfterScan(ctx, t, session, before)
	result := idleCheckRecord{Result: live.JudgeIdleCheck(check), Check: check}
	if err != nil && !check.DeadlineExceeded {
		result.Error = err.Error()
	}
	return result, cursor, true
}

// idleAfterScan 在补扫之后再 Examine 一次（同时测出一次普通命令的往返耗时），随后以 10 秒期限调用 Idle，
// 返回可供判定的原始观测值。
func idleAfterScan(ctx context.Context, t *testing.T, session *imap.Session, before imap.Mailbox) (live.IdleCheck, error) {
	t.Helper()
	start := time.Now()
	after, err := session.Examine(ctx, imap.FolderInbox)
	roundTrip := time.Since(start)
	if err != nil {
		return live.IdleCheck{Messages0: before.Messages, UIDNext0: before.UIDNext}, err
	}
	idleCtx, cancel := context.WithTimeout(ctx, idleCheckTimeout)
	defer cancel()
	begin := time.Now()
	newMail, idleErr := session.Idle(idleCtx)
	return live.IdleCheck{
		Messages0:        before.Messages,
		Messages1:        after.Messages,
		UIDNext0:         before.UIDNext,
		UIDNext1:         after.UIDNext,
		NewMail:          newMail,
		Returned:         idleErr == nil,
		DeadlineExceeded: errors.Is(idleErr, context.DeadlineExceeded),
		Elapsed:          time.Since(begin),
		RoundTrip:        roundTrip,
	}, idleErr
}

// idleMeasure 是测量会话：新建连接，Examine 后连续调用 Idle，时长为 minutes 分钟，IdleMax 放宽到该时长；
// 各次 Idle 之间不调用 Scan，每次返回都计入推送统计。selfSend 为真时，自发邮件在开始测量之前逐封确认，
// 确认后于第一次 Idle 开始 10 秒时发给机器人自己，并记录从 250 响应到该次 Idle 返回 true 的延迟。
func idleMeasure(t *testing.T, minutes int, selfSend bool) measureRecord {
	t.Helper()
	duration := time.Duration(minutes) * time.Minute
	ctx, cancel := probeTimeout(duration + 5*time.Minute)
	defer cancel()
	data := measureRecord{Minutes: minutes, Events: []idleEvent{}}
	var message []byte
	if selfSend {
		message, data.SelfSend = prepareSelfSend(t)
	}
	session := dialIMAP(ctx, t, imap.Timeouts{IdleMax: duration})
	defer session.Close()
	if _, err := session.Examine(ctx, imap.FolderInbox); err != nil {
		t.Fatalf("EXAMINE INBOX 失败：%v", err)
	}

	sent := make(chan selfSendResult, 1)
	start := time.Now()
	deadline := start.Add(duration)
	var firstPush time.Time
	for round := 0; time.Now().Before(deadline); round++ {
		if round == 0 && message != nil {
			password := probePassword(t)
			go sendSelfAfter(ctx, password, message, sent)
		}
		idleCtx, idleCancel := context.WithDeadline(ctx, deadline)
		newMail, err := session.Idle(idleCtx)
		idleCancel()
		event := idleEvent{AtSeconds: time.Since(start).Seconds()}
		switch {
		case err == nil && newMail:
			event.Type = "exists"
			if firstPush.IsZero() {
				firstPush = time.Now()
			}
		case err == nil:
			event.Type = "idle_max"
		case errors.Is(err, context.DeadlineExceeded):
			event.Type = "deadline"
		case errors.Is(err, imap.ErrClosed):
			event.Type = "closed"
			event.Error = err.Error()
		default:
			event.Type = "error"
			event.Error = err.Error()
		}
		data.Events = append(data.Events, event)
		t.Logf("第 %d 次 Idle 于 %.1f 秒返回：%s", round+1, event.AtSeconds, event.Type)
		if err != nil {
			break
		}
	}
	if message != nil {
		finishSelfSend(t, sent, firstPush, data.SelfSend)
	}
	return data
}

// prepareSelfSend 在开始测量之前渲染自发邮件并逐封确认；确认被拒绝时返回的记录状态为 skipped，邮件为 nil。
// 自发邮件只用于测量推送延迟，不写入 state.json，因此用旧形态标签 [TC <任务 ID>]、不带令牌。
func prepareSelfSend(t *testing.T) ([]byte, *selfSendRecord) {
	t.Helper()
	bot := input.cfg.Mailbox.Address
	message, err := live.ComposeNotification(live.Notification{
		From: bot, To: bot,
		Tag:       live.SubjectTag(randomID(t, 10), ""),
		Title:     "TurnCourier L1 IDLE 探测",
		MessageID: "<tc." + randomID(t, 24) + "@" + domainOf(bot) + ">",
		ProbeID:   randomID(t, 16),
		Date:      time.Now(),
	})
	if err != nil {
		t.Fatalf("渲染自发邮件失败：%v", err)
	}
	if !confirm(t, fmt.Sprintf("将在测量开始 10 秒后用 %s 给机器人自己发送一封合成邮件，用于测量 IDLE 推送延迟。", bot)) {
		t.Logf("已跳过自发邮件")
		return nil, &selfSendRecord{Status: "skipped"}
	}
	return message, &selfSendRecord{Status: "sent"}
}

// sendSelfAfter 等待 10 秒后把合成邮件发给机器人自己，并把 250 响应的时刻送回通道；它在后台运行，
// 因此不调用 t 的任何方法，错误经通道返回。
func sendSelfAfter(ctx context.Context, password string, message []byte, done chan<- selfSendResult) {
	timer := time.NewTimer(selfSendDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		done <- selfSendResult{err: ctx.Err()}
		return
	}
	bot := input.cfg.Mailbox.Address
	_, err := smtp.Send(ctx, smtpConfig(), password, smtp.Envelope{From: bot, To: []string{bot}}, message)
	done <- selfSendResult{at: time.Now(), err: err}
}

// finishSelfSend 取回自发邮件的结果并计算推送延迟：250 响应之后的第一次 EXISTS 推送才算这封邮件的延迟。
func finishSelfSend(t *testing.T, sent <-chan selfSendResult, firstPush time.Time, data *selfSendRecord) {
	t.Helper()
	var result selfSendResult
	select {
	case result = <-sent:
	case <-time.After(2 * time.Minute):
		data.Status, data.Error = "failed", "自发邮件没有在测量结束后及时返回结果"
		return
	}
	switch {
	case result.err != nil:
		data.Status, data.Error = "failed", result.err.Error()
		t.Logf("自发邮件失败：%v", result.err)
	case firstPush.IsZero() || firstPush.Before(result.at):
		t.Logf("自发邮件已发出，但没有观测到它引发的推送")
	default:
		data.LatencyMillis = firstPush.Sub(result.at).Milliseconds()
		t.Logf("从 250 响应到 EXISTS 推送：%d 毫秒", data.LatencyMillis)
	}
}

// idleFetchCheck 是 FETCH 检查会话：新建连接，Examine 后以预检的游标 Scan 一次（取回自发邮件，不输出其内容），
// 再 Examine，随后以 10 秒期限调用 Idle，按预检的规则判定并单独记录。
// 预检未能进行时改用本会话 Examine 得到的 UIDNEXT 构造只取回最新一封的游标，取不到 UIDNEXT 时记为未知并跳过补扫。
func idleFetchCheck(t *testing.T, cursor live.Cursor, hasCursor bool) idleCheckRecord {
	t.Helper()
	ctx, cancel := probeTimeout(5 * time.Minute)
	defer cancel()
	session := dialIMAP(ctx, t, imap.Timeouts{})
	defer session.Close()
	before, err := session.Examine(ctx, imap.FolderInbox)
	if err != nil {
		return idleCheckRecord{Result: "unknown", Error: err.Error()}
	}
	if !hasCursor {
		// 不退化为零游标：那会从真实收件箱取回至多 MaxBatch 封无关邮件的完整正文。
		// UIDNEXT−2 只让 Scan 取回 UID 最大的那一封，也就是刚刚发出的自发邮件。
		if before.UIDNext < 2 {
			return idleCheckRecord{Result: "unknown", UIDNextMissing: before.UIDNext == 0,
				Error: "预检未能进行，本会话也没有可用的 UIDNEXT，跳过补扫"}
		}
		cursor = live.Cursor{UIDValidity: before.UIDValidity, LastUID: before.UIDNext - 2}
	}
	if _, err := session.Scan(ctx, imap.FolderInbox, imap.Cursor(cursor)); err != nil {
		return idleCheckRecord{Result: "unknown", Error: err.Error()}
	}
	check, err := idleAfterScan(ctx, t, session, before)
	result := idleCheckRecord{Result: live.JudgeIdleCheck(check), Check: check, UIDNextMissing: before.UIDNext == 0}
	if err != nil && !check.DeadlineExceeded {
		result.Error = err.Error()
	}
	return result
}
