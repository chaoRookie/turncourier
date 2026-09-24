// Package qqsim 测试离线 QQ 邮箱模拟器：用产品的 SMTP 客户端（internal/mail/smtp.Send）发信、用产品的 IMAP 会话
// （internal/mail/imap.Session）登录与补扫，核对 Message-ID 的改写、X-OQ-MSGID、「已发送」副本、收件人副本与合成的回复，
// 以及每一种故障注入产生的客户端错误分类；「已发送」副本的延迟写入由注入的时钟与放行操作控制，不做真实时间的长等待。
// 测试只连接本机回环地址上的模拟器，地址只用 example.invalid，授权码与令牌形状的文字都在运行时构造。
package qqsim

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	gosmtp "github.com/emersion/go-smtp"

	gateway "github.com/chaoRookie/turncourier/internal/mail"
	"github.com/chaoRookie/turncourier/internal/mail/imap"
	"github.com/chaoRookie/turncourier/internal/mail/parser"
	"github.com/chaoRookie/turncourier/internal/mail/smtp"
)

const (
	// botAddr 是模拟器的机器人地址；userAddr 是接收通知、发出回复的用户地址。都用保留的 example.invalid 域名。
	botAddr  = "bot@example.invalid"
	userAddr = "user@example.invalid"
	// taskID 是合成通知主题标签中的任务 ID（10 个字母表字符）。
	taskID = "q7m3k9x2pa"
	// waitLimit 是每一步网络操作与等待收件人副本的上限；正常情况下都在毫秒级完成。
	waitLimit = 5 * time.Second
	// separator 是 QQ 邮箱 App 回复中引用原文之前的分隔线。
	separator = "------------------ 原始邮件 ------------------"
)

var (
	// botPassword 是测试用的授权码，在运行时构造，源码中不出现形似机密的字面量。
	botPassword = strings.Repeat("qs", 8)
	// tencentID 匹配 L1 实测的改写后 Message-ID 形状：<tencent_ 加 40 个大写十六进制字符 @qq.com>。
	tencentID = regexp.MustCompile(`^<tencent_[0-9A-F]{40}@qq\.com>$`)
)

// fakeClock 是可以手动推进的测试时钟，供「已发送」副本的延迟写入使用；可并发读取。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// Now 返回当前的测试时刻。
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance 把测试时钟向前推进 d。
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newClock 返回停在固定时刻的测试时钟。
func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 24, 15, 0, 0, 0, time.UTC)}
}

// start 以机器人地址与测试授权码（opts 中没有给出时）启动模拟器，用例结束时关闭。
func start(t *testing.T, opts Options) *Server {
	t.Helper()
	if opts.Address == "" {
		opts.Address = botAddr
	}
	if opts.Password == "" {
		opts.Password = botPassword
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// setFaults 注入故障，失败即终止用例。
func setFaults(t *testing.T, srv *Server, f Faults) {
	t.Helper()
	if err := srv.SetFaults(f); err != nil {
		t.Fatalf("SetFaults: %v", err)
	}
}

// send 用产品的 smtp.Send 以机器人身份、以给定授权码把 msg 发给 userAddr。
func send(t *testing.T, srv *Server, password string, msg []byte) (smtp.Result, error) {
	t.Helper()
	cfg := srv.SMTPConfig()
	cfg.Timeouts = smtp.Timeouts{Command: waitLimit, Submission: waitLimit}
	ctx, cancel := context.WithTimeout(context.Background(), 2*waitLimit)
	defer cancel()
	return smtp.Send(ctx, cfg, password, smtp.Envelope{From: botAddr, To: []string{userAddr}}, msg)
}

// dial 用产品的 imap.Dial 以给定授权码登录模拟器；成功时用例结束前关闭会话。
func dial(t *testing.T, srv *Server, password string) (*imap.Session, error) {
	t.Helper()
	cfg := srv.IMAPConfig()
	cfg.Timeouts = imap.Timeouts{Dial: waitLimit, Greeting: waitLimit, Command: waitLimit, Fetch: waitLimit,
		IdleAck: waitLimit, IdleMax: waitLimit, IdleStop: waitLimit, Poll: waitLimit}
	ctx, cancel := context.WithTimeout(context.Background(), 2*waitLimit)
	defer cancel()
	s, err := imap.Dial(ctx, cfg, password)
	if err == nil {
		t.Cleanup(func() { _ = s.Close() })
	}
	return s, err
}

// mustDial 登录模拟器，失败即终止用例。
func mustDial(t *testing.T, srv *Server) *imap.Session {
	t.Helper()
	s, err := dial(t, srv, botPassword)
	if err != nil {
		t.Fatalf("imap.Dial: %v", err)
	}
	return s
}

// scanHeaders 以空游标补扫「已发送」的头部，失败即终止用例。
func scanHeaders(t *testing.T, s *imap.Session) imap.Batch {
	t.Helper()
	b, err := s.ScanHeaders(context.Background(), imap.FolderSent, imap.Cursor{})
	if err != nil {
		t.Fatalf("ScanHeaders: %v", err)
	}
	return b
}

// receive 取出下一封收件人副本，等待超过 waitLimit 即终止用例。
func receive(t *testing.T, srv *Server) Delivered {
	t.Helper()
	select {
	case d, ok := <-srv.Delivered():
		if !ok {
			t.Fatal("the Delivered channel is closed")
		}
		return d
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for a delivered copy")
	}
	return Delivered{}
}

// headerOf 用 net/mail 读出一封邮件的头部，失败即终止用例。
func headerOf(t *testing.T, raw []byte) mail.Header {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("net/mail cannot read the message: %v", err)
	}
	return msg.Header
}

// fakeToken 返回 48 个字母表字符的令牌形状文字，在运行时构造。
func fakeToken() string {
	return strings.Repeat("k7", 24)
}

// notification 构造一封形如 4b 通知的合成邮件（CRLF 换行），返回原始字节与解码后的主题：主题是原始 ASCII 的标签，
// 折行后接 B 编码的标题；multipart/alternative 的纯文本与 HTML 两部分都是 utf-8/quoted-printable，页脚含标记行、
// 回复说明与没有引用前缀的令牌行，另带 X-TurnCourier-ID。
func notification(ourID string) (raw []byte, subject string) {
	tag := "[TC " + taskID + " " + fakeToken() + "]"
	title := "回合完成：部署脚本已更新，请确认下一步"
	text := "任务 " + taskID + " 回合完成。\r\n部署脚本已更新。\r\n\r\n" + gateway.FooterMarker + "\r\n" + gateway.FooterNotice +
		"；请保留主题中的 [TC …] 标签，不要改动。\r\n令牌副本（仅供核对）：\r\n" + fakeToken() + "\r\n"
	var b bytes.Buffer
	b.WriteString("From: TurnCourier <" + botAddr + ">\r\nTo: " + userAddr + "\r\n")
	b.WriteString("Subject: " + tag + "\r\n " + mime.BEncoding.Encode("utf-8", title) + "\r\n")
	b.WriteString("Date: Wed, 24 Sep 2026 15:00:00 +0800\r\nMessage-Id: " + ourID + "\r\n" + gateway.IDHeader + ": " + ourID + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\nAuto-Submitted: auto-generated\r\nContent-Type: multipart/alternative; boundary=\"n1\"\r\n\r\n")
	for _, part := range []struct{ typ, body string }{
		{"text/plain", text},
		{"text/html", "<div style=\"white-space: pre-wrap\">" + html.EscapeString(text) + "</div>"},
	} {
		b.WriteString("--n1\r\nContent-Type: " + part.typ + "; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		w := quotedprintable.NewWriter(&b)
		_, _ = w.Write([]byte(part.body))
		_ = w.Close()
		b.WriteString("\r\n")
	}
	b.WriteString("--n1--\r\n")
	return b.Bytes(), tag + " " + title
}

// withoutFields 删去一封邮件头部中名为 names 之一的字段（不区分大小写，只处理不折行的字段），其余字节原样返回。
func withoutFields(raw []byte, names ...string) []byte {
	head, body, _ := bytes.Cut(raw, []byte("\r\n\r\n"))
	var out bytes.Buffer
	for _, line := range strings.Split(string(head), "\r\n") {
		name, _, _ := strings.Cut(line, ":")
		if !slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, name) }) {
			out.WriteString(line + "\r\n")
		}
	}
	out.WriteString("\r\n")
	out.Write(body)
	return out.Bytes()
}

// TestSendRewritesMessageID 用 smtp.Send 发出一封通知：DATA 的 250 响应与 QQ 实测相同、不含任何 ID；收件人副本与「已发送」副本
// 相同，Message-Id 被改写为 <tencent_…@qq.com>，X-OQ-MSGID 等于原 ID，其余头部与正文原样保留。
func TestSendRewritesMessageID(t *testing.T) {
	srv := start(t, Options{})
	const ours = "<tc.notice.0001@example.invalid>"
	msg, _ := notification(ours)
	res, err := send(t, srv, botPassword, msg)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Response != "OK: queued as." {
		t.Errorf("DATA response = %q, want the text QQ returns without any ID", res.Response)
	}
	d := receive(t, srv)
	if !tencentID.MatchString(d.MessageID) || d.OriginalID != ours || strings.Contains(res.Response, "tencent") {
		t.Fatalf("delivered ID %q, original %q", d.MessageID, d.OriginalID)
	}
	if d.From != botAddr || !slices.Equal(d.To, []string{userAddr}) {
		t.Errorf("envelope %q -> %q", d.From, d.To)
	}
	h := headerOf(t, d.Raw)
	if h.Get("Message-Id") != d.MessageID || h.Get("X-OQ-MSGID") != ours || h.Get(gateway.IDHeader) != ours {
		t.Errorf("Message-Id %q, X-OQ-MSGID %q, %s %q", h.Get("Message-Id"), h.Get("X-OQ-MSGID"), gateway.IDHeader, h.Get(gateway.IDHeader))
	}
	if !bytes.Equal(withoutFields(d.Raw, "Message-Id", "X-OQ-MSGID"), withoutFields(msg, "Message-Id")) {
		t.Error("the delivered copy differs from the original beyond Message-Id and X-OQ-MSGID")
	}
	sent, err := srv.Messages(imap.FolderSent)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(sent) != 1 || sent[0].UID != 1 || !bytes.Equal(sent[0].Raw, d.Raw) {
		t.Fatalf("Sent holds %d messages, want the delivered copy", len(sent))
	}
	msg2, _ := notification("<tc.notice.0002@example.invalid>")
	if _, err := send(t, srv, botPassword, msg2); err != nil {
		t.Fatalf("second Send: %v", err)
	}
	if d2 := receive(t, srv); d2.MessageID == d.MessageID || !tencentID.MatchString(d2.MessageID) {
		t.Errorf("second delivered ID %q repeats or has the wrong shape", d2.MessageID)
	}
	if stats := srv.Stats(); stats.Accepted != 2 || stats.SMTPAuths != 2 {
		t.Errorf("Stats = %+v", stats)
	}
}

// TestIMAPSessionScansSentCopy 用 imap.Session 登录：LIST 恰有 INBOX、Junk 与「已发送」三个文件夹，三者的 UIDVALIDITY 相同
// （L1 实测）；ScanHeaders 只取回「已发送」副本的头部，其中的 Message-Id 与 X-OQ-MSGID 与收件人副本一致。
func TestIMAPSessionScansSentCopy(t *testing.T) {
	srv := start(t, Options{})
	msg, _ := notification("<tc.notice.0003@example.invalid>")
	if _, err := send(t, srv, botPassword, msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	d := receive(t, srv)
	s := mustDial(t, srv)
	folders, err := s.ListFolders(context.Background())
	if err != nil {
		t.Fatalf("ListFolders: %v", err)
	}
	slices.Sort(folders)
	if want := []string{imap.FolderInbox, imap.FolderJunk, imap.FolderSent}; !slices.Equal(folders, want) {
		t.Errorf("folders = %q, want %q", folders, want)
	}
	var validity []uint32
	for _, folder := range []string{imap.FolderInbox, imap.FolderJunk, imap.FolderSent} {
		box, err := s.Examine(context.Background(), folder)
		if err != nil {
			t.Fatalf("Examine(%s): %v", folder, err)
		}
		validity = append(validity, box.UIDValidity)
	}
	if validity[0] == 0 || validity[0] != validity[1] || validity[1] != validity[2] {
		t.Errorf("UIDVALIDITY per folder = %v, want one shared non-zero value", validity)
	}
	b := scanHeaders(t, s)
	if len(b.Messages) != 1 || b.UIDValidity != validity[0] || b.Next.LastUID != 1 {
		t.Fatalf("batch has %d messages, UIDVALIDITY %d, next %+v", len(b.Messages), b.UIDValidity, b.Next)
	}
	head := b.Messages[0].Raw
	if !bytes.HasSuffix(head, []byte("\r\n\r\n")) || bytes.Contains(head, []byte(gateway.FooterMarker)) {
		t.Error("ScanHeaders returned more than the header")
	}
	h := headerOf(t, head)
	if h.Get("Message-Id") != d.MessageID || h.Get("X-OQ-MSGID") != d.OriginalID {
		t.Errorf("Sent copy header: Message-Id %q, X-OQ-MSGID %q", h.Get("Message-Id"), h.Get("X-OQ-MSGID"))
	}
}

// TestSMTPFaults 逐个注入 AUTH、MAIL、RCPT、DATA 命令与结束标记之后的 4xx 与 5xx：smtp.Send 的错误分类都对应（AUTH 的 5xx 为
// ErrAuth，结束标记之后为 ErrRejected，其余为 ErrNotSent，都带着步骤与状态码的 *ReplyError）；被拒的邮件没有收件人副本，
// 也不进「已发送」。清除故障后照常发出。
func TestSMTPFaults(t *testing.T) {
	cases := []struct {
		name    string
		faults  Faults
		step    string
		code    int
		outcome error
	}{
		{"auth 535", Faults{AuthCode: 535}, "auth", 535, smtp.ErrNotSent},
		{"auth 454", Faults{AuthCode: 454}, "auth", 454, smtp.ErrNotSent},
		{"mail 451", Faults{MailCode: 451}, "mail", 451, smtp.ErrNotSent},
		{"mail 550", Faults{MailCode: 550}, "mail", 550, smtp.ErrNotSent},
		{"rcpt 450", Faults{RcptCode: 450}, "rcpt", 450, smtp.ErrNotSent},
		{"rcpt 550", Faults{RcptCode: 550}, "rcpt", 550, smtp.ErrNotSent},
		{"data 451", Faults{DataCode: 451}, "data", 451, smtp.ErrNotSent},
		{"data 554", Faults{DataCode: 554}, "data", 554, smtp.ErrNotSent},
		{"submission 451", Faults{SubmissionCode: 451}, "submission", 451, smtp.ErrRejected},
		{"submission 550", Faults{SubmissionCode: 550}, "submission", 550, smtp.ErrRejected},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := start(t, Options{})
			setFaults(t, srv, c.faults)
			msg, _ := notification("<tc.fault@example.invalid>")
			_, err := send(t, srv, botPassword, msg)
			if !errors.Is(err, c.outcome) {
				t.Fatalf("Send error %v, want %v", err, c.outcome)
			}
			if got := errors.Is(err, smtp.ErrAuth); got != (c.step == "auth" && c.code/100 == 5) {
				t.Errorf("errors.Is(err, ErrAuth) = %v", got)
			}
			var reply *smtp.ReplyError
			if !errors.As(err, &reply) || reply.Step != c.step || reply.Code != c.code || reply.Temporary() != (c.code/100 == 4) {
				t.Fatalf("reply error %+v, want step %s code %d", reply, c.step, c.code)
			}
			if reply.Enhanced != [3]int{} {
				t.Errorf("enhanced code %v, want none (QQ sends none)", reply.Enhanced)
			}
			srv.ReleaseSent()
			if sent, _ := srv.Messages(imap.FolderSent); len(sent) != 0 || srv.Stats().Accepted != 0 {
				t.Errorf("a rejected message was accepted: %d Sent copies", len(sent))
			}
			srv.ClearFaults()
			if _, err := send(t, srv, botPassword, msg); err != nil {
				t.Fatalf("Send after ClearFaults: %v", err)
			}
			receive(t, srv)
		})
	}
}

// TestDropAfterData 注入「收到结束标记后不回响应即断开」：smtp.Send 得到 ErrUncertain（没有 *ReplyError）。DropAccepted 在断开之前
// 照常收下邮件（「已发送」副本与收件人副本都在，D7 可凭副本核对为已送达）；DropDiscarded 丢弃邮件，两处都没有它。
func TestDropAfterData(t *testing.T) {
	for _, c := range []struct {
		name     string
		drop     Drop
		accepted bool
	}{{"accepted", DropAccepted, true}, {"discarded", DropDiscarded, false}} {
		t.Run(c.name, func(t *testing.T) {
			srv := start(t, Options{})
			setFaults(t, srv, Faults{Drop: c.drop})
			msg, _ := notification("<tc.drop@example.invalid>")
			_, err := send(t, srv, botPassword, msg)
			var reply *smtp.ReplyError
			if !errors.Is(err, smtp.ErrUncertain) || errors.Is(err, smtp.ErrNotSent) || errors.As(err, &reply) {
				t.Fatalf("Send error %v, want ErrUncertain without a reply", err)
			}
			sent, _ := srv.Messages(imap.FolderSent)
			if got := len(sent) == 1 && srv.Stats().Accepted == 1; got != c.accepted {
				t.Errorf("accepted = %v (%d Sent copies), want %v", got, len(sent), c.accepted)
			}
			if c.accepted {
				if d := receive(t, srv); d.OriginalID != "<tc.drop@example.invalid>" || !bytes.Equal(d.Raw, sent[0].Raw) {
					t.Error("the delivered copy does not match the Sent copy")
				}
			}
		})
	}
}

// TestSentCopyDelayedByClock 注入「已发送」副本延迟 30 秒写入：收件人副本立即交付，而 IMAP 补扫在注入的时钟走满 30 秒之前
// 都看不到副本，走满之后下一次 EXAMINE 即看到；等待只推进测试时钟，不做真实时间的等待。
func TestSentCopyDelayedByClock(t *testing.T) {
	clock := newClock()
	srv := start(t, Options{Now: clock.Now})
	setFaults(t, srv, Faults{SentDelay: 30 * time.Second})
	msg, _ := notification("<tc.delay@example.invalid>")
	if _, err := send(t, srv, botPassword, msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	d := receive(t, srv)
	s := mustDial(t, srv)
	if b := scanHeaders(t, s); len(b.Messages) != 0 {
		t.Fatal("the delayed copy is visible at once")
	}
	clock.Advance(29 * time.Second)
	if b := scanHeaders(t, s); len(b.Messages) != 0 {
		t.Fatal("the delayed copy is visible before its delay")
	}
	if sent, _ := srv.Messages(imap.FolderSent); len(sent) != 0 {
		t.Fatal("Messages shows the delayed copy before its delay")
	}
	clock.Advance(time.Second)
	b := scanHeaders(t, s)
	if len(b.Messages) != 1 || headerOf(t, b.Messages[0].Raw).Get("Message-Id") != d.MessageID {
		t.Fatalf("after the delay the scan returned %d messages", len(b.Messages))
	}
	if n := srv.ReleaseSent(); n != 0 {
		t.Errorf("ReleaseSent released %d copies after the copy was written", n)
	}
}

// TestReleaseSent 注入 1 小时的延迟后连发两封：ReleaseSent 不等时钟即按收下的顺序写入两份副本，第二次调用没有可放行的副本；
// 放行之前 Messages 看不到它们。延迟的计时默认取真实时间（Options.Now 为空）。
func TestReleaseSent(t *testing.T) {
	srv := start(t, Options{})
	setFaults(t, srv, Faults{SentDelay: time.Hour})
	var ids []string
	for _, ours := range []string{"<tc.release.1@example.invalid>", "<tc.release.2@example.invalid>"} {
		msg, _ := notification(ours)
		if _, err := send(t, srv, botPassword, msg); err != nil {
			t.Fatalf("Send: %v", err)
		}
		ids = append(ids, receive(t, srv).MessageID)
	}
	if sent, _ := srv.Messages(imap.FolderSent); len(sent) != 0 {
		t.Fatal("delayed copies are visible before ReleaseSent")
	}
	if n := srv.ReleaseSent(); n != 2 {
		t.Fatalf("ReleaseSent released %d copies, want 2", n)
	}
	if n := srv.ReleaseSent(); n != 0 {
		t.Errorf("second ReleaseSent released %d copies", n)
	}
	b := scanHeaders(t, mustDial(t, srv))
	if len(b.Messages) != 2 {
		t.Fatalf("Sent holds %d copies after release, want 2", len(b.Messages))
	}
	for i, m := range b.Messages {
		if got := headerOf(t, m.Raw).Get("Message-Id"); got != ids[i] || m.UID != uint32(i+1) {
			t.Errorf("copy %d: UID %d, Message-Id %q, want %q", i, m.UID, got, ids[i])
		}
	}
}

// TestNoSentCopy 注入「不写入」（模拟关闭「保存到已发送」）：邮件照常交付，「已发送」中一直没有副本，放行也不会出现。
func TestNoSentCopy(t *testing.T) {
	srv := start(t, Options{})
	setFaults(t, srv, Faults{NoSentCopy: true, SentDelay: time.Minute})
	msg, _ := notification("<tc.nocopy@example.invalid>")
	if _, err := send(t, srv, botPassword, msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	receive(t, srv)
	if n := srv.ReleaseSent(); n != 0 {
		t.Errorf("ReleaseSent released %d copies", n)
	}
	if b := scanHeaders(t, mustDial(t, srv)); len(b.Messages) != 0 {
		t.Errorf("Sent holds %d copies", len(b.Messages))
	}
}

// TestIMAPFaults 注入 IMAP 的三种故障：登录失败时 imap.Dial 返回 ErrAuthFailed（授权码错误同样如此，两者都计入登录次数）；
// 「已发送」的 EXAMINE 被拒绝时 Examine 返回 ErrNoFolder、连接仍可用、INBOX 照常；LIST 中没有「已发送」时 ListFolders
// 只列出 INBOX 与 Junk，而只注入这一条时 EXAMINE 照常。清除故障后一切恢复；不存在的文件夹同样得到 ErrNoFolder。
func TestIMAPFaults(t *testing.T) {
	srv := start(t, Options{})
	setFaults(t, srv, Faults{IMAPLoginFails: true})
	if _, err := dial(t, srv, botPassword); !errors.Is(err, imap.ErrAuthFailed) {
		t.Fatalf("Dial with the login fault: %v, want ErrAuthFailed", err)
	}
	srv.ClearFaults()
	if _, err := dial(t, srv, botPassword+"x"); !errors.Is(err, imap.ErrAuthFailed) {
		t.Fatalf("Dial with a wrong password: %v, want ErrAuthFailed", err)
	}
	if got := srv.Stats().IMAPLogins; got != 2 {
		t.Errorf("IMAPLogins = %d, want 2", got)
	}
	s := mustDial(t, srv)
	ctx := context.Background()
	setFaults(t, srv, Faults{SentExamineFails: true, SentHidden: true})
	if _, err := s.Examine(ctx, imap.FolderSent); !errors.Is(err, imap.ErrNoFolder) {
		t.Errorf("Examine(Sent) = %v, want ErrNoFolder", err)
	}
	if _, err := s.ScanHeaders(ctx, imap.FolderSent, imap.Cursor{}); !errors.Is(err, imap.ErrNoFolder) {
		t.Errorf("ScanHeaders(Sent) = %v, want ErrNoFolder", err)
	}
	if _, err := s.Examine(ctx, imap.FolderInbox); err != nil {
		t.Errorf("Examine(INBOX) after the rejection: %v", err)
	}
	folders, err := s.ListFolders(ctx)
	slices.Sort(folders)
	if err != nil || !slices.Equal(folders, []string{imap.FolderInbox, imap.FolderJunk}) {
		t.Errorf("ListFolders with Sent hidden = %q (err %v)", folders, err)
	}
	setFaults(t, srv, Faults{SentHidden: true})
	if folders, err := s.ListFolders(ctx); err != nil || slices.Contains(folders, imap.FolderSent) {
		t.Errorf("ListFolders with only SentHidden = %q (err %v)", folders, err)
	}
	if _, err := s.Examine(ctx, imap.FolderSent); err != nil {
		t.Errorf("SentHidden alone made EXAMINE fail: %v", err)
	}
	srv.ClearFaults()
	if _, err := s.Examine(ctx, imap.FolderSent); err != nil {
		t.Errorf("Examine(Sent) after ClearFaults: %v", err)
	}
	if folders, err := s.ListFolders(ctx); err != nil || !slices.Contains(folders, imap.FolderSent) {
		t.Errorf("ListFolders after ClearFaults = %q (err %v)", folders, err)
	}
	if _, err := s.Examine(ctx, "Drafts"); !errors.Is(err, imap.ErrNoFolder) {
		t.Errorf("Examine of a folder that does not exist = %v, want ErrNoFolder", err)
	}
}

// TestDisconnectAll 立即断开当前全部连接：已登录的 IMAP 会话下一条命令得到 ErrClosed，已问候的 SMTP 连接下一条命令失败；
// 之后的新连接照常工作（断网恢复）。
func TestDisconnectAll(t *testing.T) {
	srv := start(t, Options{})
	s := mustDial(t, srv)
	client, err := gosmtp.DialTLS(net.JoinHostPort(srv.Host(), strconv.Itoa(srv.SMTPPort())),
		&tls.Config{ServerName: srv.Host(), RootCAs: srv.RootCAs()})
	if err != nil {
		t.Fatalf("DialTLS: %v", err)
	}
	defer client.Close()
	if err := client.Hello("localhost"); err != nil {
		t.Fatalf("EHLO: %v", err)
	}
	if n := srv.DisconnectAll(); n != 2 {
		t.Errorf("DisconnectAll closed %d connections, want 2", n)
	}
	if _, err := s.ListFolders(context.Background()); !errors.Is(err, imap.ErrClosed) {
		t.Errorf("ListFolders after DisconnectAll = %v, want ErrClosed", err)
	}
	if err := client.Noop(); err == nil {
		t.Error("the SMTP connection still works after DisconnectAll")
	}
	if n := srv.DisconnectAll(); n != 0 {
		t.Errorf("second DisconnectAll closed %d connections", n)
	}
	if _, err := mustDial(t, srv).ListFolders(context.Background()); err != nil {
		t.Errorf("IMAP after reconnect: %v", err)
	}
	msg, _ := notification("<tc.reconnect@example.invalid>")
	if _, err := send(t, srv, botPassword, msg); err != nil {
		t.Errorf("SMTP after reconnect: %v", err)
	}
}

// TestDeliver 把字节放进 INBOX、Junk 与「已发送」：各文件夹各自从 UID 1 编号（与相同的 UIDVALIDITY 一起构成跨文件夹撞键的情形）；
// 头部可以解析的畸形来信（非法 UTF-8、<> 与超长的 Message-ID）经 IMAP 取回时逐字节相同；头部无法解析的字节经 IMAP 取回为空，
// Messages 仍给出原样的字节且不与内部记录共用内存；不存在的文件夹被拒绝。
func TestDeliver(t *testing.T) {
	srv := start(t, Options{})
	odd := []byte("From: " + userAddr + "\r\nSubject: \xff\xfe bad\r\nMessage-Id: <>\r\nX-Long: " + strings.Repeat("x", 1200) +
		"\r\n\r\nbody \xff\xfe\r\n")
	history := []byte("From: " + botAddr + "\r\nMessage-Id: <tencent_OLD@qq.com>\r\n\r\nold\r\n")
	garbage := []byte("not even a mail message")
	for _, c := range []struct {
		folder string
		raw    []byte
	}{{imap.FolderInbox, odd}, {imap.FolderJunk, garbage}, {imap.FolderSent, history}} {
		uid, err := srv.Deliver(c.folder, c.raw)
		if err != nil || uid != 1 {
			t.Fatalf("Deliver(%s) = %d, %v", c.folder, uid, err)
		}
	}
	s := mustDial(t, srv)
	for folder, want := range map[string][]byte{imap.FolderInbox: odd, imap.FolderJunk: {}, imap.FolderSent: history} {
		b, err := s.Scan(context.Background(), folder, imap.Cursor{})
		if err != nil || len(b.Messages) != 1 || !bytes.Equal(b.Messages[0].Raw, want) {
			t.Errorf("Scan(%s): %d messages, err %v", folder, len(b.Messages), err)
		}
	}
	for folder, want := range map[string][]byte{imap.FolderInbox: odd, imap.FolderJunk: garbage, imap.FolderSent: history} {
		stored, err := srv.Messages(folder)
		if err != nil || len(stored) != 1 || stored[0].UID != 1 || !bytes.Equal(stored[0].Raw, want) {
			t.Fatalf("Messages(%s) = %d messages, err %v", folder, len(stored), err)
		}
		stored[0].Raw[0] ^= 0xff
		if again, _ := srv.Messages(folder); !bytes.Equal(again[0].Raw, want) {
			t.Errorf("Messages(%s) returned bytes shared with the store", folder)
		}
	}
	if _, err := srv.Deliver("Drafts", odd); err == nil {
		t.Error("Deliver accepted a folder that does not exist")
	}
	if _, err := srv.Messages("Drafts"); err == nil {
		t.Error("Messages accepted a folder that does not exist")
	}
}

// TestDeliverWakesIdle 确认放入 INBOX 的邮件像真实服务器一样推送 EXISTS：在 INBOX 上 IDLE 的会话被唤醒。
func TestDeliverWakesIdle(t *testing.T) {
	srv := start(t, Options{})
	s := mustDial(t, srv)
	if _, err := s.Examine(context.Background(), imap.FolderInbox); err != nil {
		t.Fatalf("Examine: %v", err)
	}
	time.AfterFunc(50*time.Millisecond, func() {
		_, _ = srv.Deliver(imap.FolderInbox, []byte("From: "+userAddr+"\r\n\r\nwake\r\n"))
	})
	if newMail, err := s.Idle(context.Background()); err != nil || !newMail {
		t.Fatalf("Idle = %v, %v; want woken by the delivery", newMail, err)
	}
}

// decodedPart 是回复中按标准库解码的一个叶子部件。
type decodedPart struct {
	mediaType, charset, encoding, text string
}

// decodeParts 用 mime/multipart 与 encoding/base64 独立解码一封 multipart 回复的各叶子部件（只处理一层 multipart）。
func decodeParts(t *testing.T, raw []byte) (string, []decodedPart) {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("Content-Type: %v", err)
	}
	var parts []decodedPart
	r := multipart.NewReader(msg.Body, params["boundary"])
	for {
		p, err := r.NextRawPart()
		if err == io.EOF {
			return mediaType, parts
		}
		if err != nil {
			t.Fatalf("NextRawPart: %v", err)
		}
		data, _ := io.ReadAll(p)
		typ, typParams, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		encoding := p.Header.Get("Content-Transfer-Encoding")
		if encoding == "base64" {
			if data, err = base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(data)), "")); err != nil {
				t.Fatalf("base64: %v", err)
			}
		}
		parts = append(parts, decodedPart{mediaType: typ, charset: typParams["charset"], encoding: encoding, text: string(data)})
	}
}

// TestReply 对「已发送」改写过的通知合成 QQ 邮箱 App 式的回复并交给产品的解析器：发件人、收件人（机器人）、主题（前缀加原主题，
// 含标签）、新的 tencent Message-ID、In-Reply-To 与 References 都指向实际投递 ID；新正文剥离引用后正好是给出的正文；判定为真人回复。
// 标准库独立解码核对结构：multipart/alternative 两部分都是 utf-8/base64，纯文本带「原始邮件」分隔线、引用头块的主题行与
// 没有引用前缀的页脚令牌行。换成自动回复的前缀后判定为自动回复。
func TestReply(t *testing.T) {
	srv := start(t, Options{})
	msg, subject := notification("<tc.reply@example.invalid>")
	if _, err := send(t, srv, botPassword, msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	d := receive(t, srv)
	body := "继续执行集成测试。\r\n失败的话把日志发给我。"
	raw, err := Reply(d, userAddr, "Re: ", body)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	m, err := parser.Parse(raw)
	if err != nil {
		t.Fatalf("parser.Parse: %v", err)
	}
	if m.From != userAddr || m.Subject != "Re: "+subject || !tencentID.MatchString(m.MessageID) || m.MessageID == d.MessageID {
		t.Errorf("From %q, subject matches %v, Message-ID %q", m.From, m.Subject == "Re: "+subject, m.MessageID)
	}
	if !slices.Equal(m.InReplyTo, []string{d.MessageID}) || !slices.Equal(m.References, []string{d.MessageID}) {
		t.Errorf("In-Reply-To %q, References %q, want the delivered ID", m.InReplyTo, m.References)
	}
	if to, err := mail.ParseAddress(headerOf(t, raw).Get("To")); err != nil || to.Address != botAddr {
		t.Errorf("To = %v (err %v), want the bot address", to, err)
	}
	if text, err := m.NewText(); err != nil || text != "继续执行集成测试。\n失败的话把日志发给我。" {
		t.Errorf("NewText = %q, %v", text, err)
	}
	plain, markup, err := m.BodyPrefix()
	if err != nil {
		t.Fatalf("BodyPrefix: %v", err)
	}
	if v := parser.Classify(m, plain, markup); v.Kind != parser.Human {
		t.Errorf("Classify = %+v, want a human reply", v)
	}
	mediaType, parts := decodeParts(t, raw)
	if mediaType != "multipart/alternative" || len(parts) != 2 {
		t.Fatalf("%s with %d parts", mediaType, len(parts))
	}
	for i, want := range []string{"text/plain", "text/html"} {
		if p := parts[i]; p.mediaType != want || p.charset != "utf-8" || p.encoding != "base64" {
			t.Errorf("part %d is %s %s/%s, want %s utf-8/base64", i, p.mediaType, p.charset, p.encoding, want)
		}
	}
	lines := strings.Split(parts[0].text, "\r\n")
	if !slices.Contains(lines, separator) || !slices.Contains(lines, "主题: "+subject) || !slices.Contains(lines, fakeToken()) ||
		!slices.Contains(lines, gateway.FooterMarker) {
		t.Error("the plain text lacks the separator, the quoted subject line or the unprefixed footer lines")
	}
	if strings.Contains(parts[1].text, "<blockquote") || !strings.Contains(parts[1].text, "失败的话把日志发给我。") {
		t.Error("the HTML part uses blockquote or lacks the body")
	}
	auto, err := Reply(d, userAddr, "自动回复: ", "我正在休假。")
	if err != nil {
		t.Fatalf("Reply with an auto-reply prefix: %v", err)
	}
	am, err := parser.Parse(auto)
	if err != nil {
		t.Fatalf("parser.Parse: %v", err)
	}
	plain, markup, _ = am.BodyPrefix()
	if v := parser.Classify(am, plain, markup); v.Kind != parser.AutoReply || am.MessageID == m.MessageID {
		t.Errorf("Classify = %+v with Message-ID reused %v, want an auto-reply with its own ID", v, am.MessageID == m.MessageID)
	}
}

// deliveredWith 构造一个收件人副本：信封与实际投递 ID 合法，原始字节由 raw 给出。
func deliveredWith(raw string) Delivered {
	return Delivered{From: botAddr, To: []string{userAddr}, MessageID: "<tencent_ABCDEF@qq.com>", Raw: []byte(raw)}
}

// TestReplyQuotesPlainText 确认 Reply 按 MIME 结构找到第一个非附件的 text/plain 部件并按传输编码解码：单部件的 7bit 与 base64、
// 嵌套的 multipart 中排在附件之后的纯文本；引用中的换行统一为 CRLF，没有主题时主题只有前缀；原文、前缀与正文中形如占位符的
// 文字原样保留。
func TestReplyQuotesPlainText(t *testing.T) {
	cases := map[string]string{
		"single 7bit": "From: " + botAddr + "\r\nSubject: =?utf-8?b?5rWL6K+V?=\r\n\r\n原文一\nquoted-line\n\n",
		"single base64": "From: " + botAddr + "\r\nSubject: =?utf-8?b?5rWL6K+V?=\r\nContent-Type: text/plain; charset=UTF-8\r\n" +
			"Content-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte("原文一\r\nquoted-line\r\n")) + "\r\n",
		"attachment first": "From: " + botAddr + "\r\nSubject: =?utf-8?b?5rWL6K+V?=\r\nContent-Type: multipart/mixed; boundary=m\r\n\r\n" +
			"--m\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=a.txt\r\n\r\nattached\r\n" +
			"--m\r\nContent-Type: multipart/alternative; boundary=a\r\n\r\n--a\r\nContent-Type: text/html\r\n\r\n<p>x</p>\r\n" +
			"--a\r\nContent-Type: text/plain; charset=us-ascii\r\nContent-Transfer-Encoding: 8bit\r\n\r\n原文一\r\nquoted-line\r\n--a--\r\n--m--\r\n",
	}
	for name, raw := range cases {
		reply, err := Reply(deliveredWith(raw), userAddr, "Re: ", "好的")
		if err != nil {
			t.Errorf("%s: Reply: %v", name, err)
			continue
		}
		_, parts := decodeParts(t, reply)
		if !strings.Contains(parts[0].text, "主题: 测试\r\n\r\n原文一\r\nquoted-line\r\n") || strings.Contains(parts[0].text, "attached") {
			t.Errorf("%s: the quote is %q", name, parts[0].text)
		}
	}
	reply, err := Reply(deliveredWith("From: "+botAddr+"\r\n\r\ntext\r\n"), userAddr, "Re: ", "好的")
	if err != nil {
		t.Fatalf("Reply without a subject: %v", err)
	}
	if m, err := parser.Parse(reply); err != nil || m.Subject != "Re: " {
		t.Errorf("subject of a reply to a subjectless message: %v", err)
	}
	braces, err := Reply(deliveredWith("From: "+botAddr+"\r\nSubject: {{TOKEN}}\r\n\r\n{{.Name}}\r\n"), userAddr, "{{PREFIX}} ", "{{BODY}}")
	if err != nil {
		t.Fatalf("Reply with braces: %v", err)
	}
	m, err := parser.Parse(braces)
	if err != nil || m.Subject != "{{PREFIX}} {{TOKEN}}" {
		t.Fatalf("subject with braces: %v", err)
	}
	if _, parts := decodeParts(t, braces); !strings.HasPrefix(parts[0].text, "{{BODY}}\r\n") ||
		!strings.HasSuffix(parts[0].text, "主题: {{TOKEN}}\r\n\r\n{{.Name}}\r\n") {
		t.Errorf("plain text with braces = %q", parts[0].text)
	}
}

// TestReplyRejects 确认 Reply 在无法按 QQ 邮箱 App 的结构回复时返回错误，且错误文本不含主题、正文与地址：原信无法解析、
// 主题的编码词无法解码、没有 text/plain 部件、字符集或传输编码不支持、base64 或 quoted-printable 损坏、multipart 缺少分隔串或
// 没有结束、嵌套过深、实际投递 ID 缺失；发件人含换行、正文含单独的 CR。
func TestReplyRejects(t *testing.T) {
	const canary = "CANARY-SUBJECT"
	head := "From: " + botAddr + "\r\nSubject: " + canary + "\r\n"
	nested := head + "Content-Type: multipart/mixed; boundary=b0\r\n\r\n"
	for i := 1; i <= 10; i++ {
		nested += "--b" + strconv.Itoa(i-1) + "\r\nContent-Type: multipart/mixed; boundary=b" + strconv.Itoa(i) + "\r\n\r\n"
	}
	nested += "--b10\r\nContent-Type: text/plain\r\n\r\ndeep\r\n"
	cases := map[string]Delivered{
		"not a message":    deliveredWith("no header separator " + canary),
		"bad subject":      deliveredWith("From: " + botAddr + "\r\nSubject: =?x-unknown?B?Q0FOQVJZ?=\r\n\r\ntext\r\n"),
		"html only":        deliveredWith(head + "Content-Type: text/html\r\n\r\n<p>" + canary + "</p>\r\n"),
		"bad content type": deliveredWith(head + "Content-Type: text/plain; charset\r\n\r\ntext\r\n"),
		"unknown charset":  deliveredWith(head + "Content-Type: text/plain; charset=big5\r\n\r\ntext\r\n"),
		"invalid utf-8":    deliveredWith(head + "Content-Type: text/plain; charset=utf-8\r\n\r\n\xff\xfe\r\n"),
		"unknown encoding": deliveredWith(head + "Content-Transfer-Encoding: uuencode\r\n\r\ntext\r\n"),
		"bad base64":       deliveredWith(head + "Content-Transfer-Encoding: base64\r\n\r\n!!!!\r\n"),
		"bad qp":           deliveredWith(head + "Content-Transfer-Encoding: quoted-printable\r\n\r\ncontrol \x01 byte\r\n"),
		"no boundary":      deliveredWith(head + "Content-Type: multipart/mixed\r\n\r\n--x\r\n\r\ntext\r\n--x--\r\n"),
		"unterminated":     deliveredWith(head + "Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/html\r\n\r\n<p>"),
		"too deep":         deliveredWith(nested),
		"no delivered id":  {From: botAddr, Raw: []byte(head + "\r\ntext\r\n")},
		"empty multipart":  deliveredWith(head + "Content-Type: multipart/mixed; boundary=x\r\n\r\n--x--\r\n"),
		"attachment only":  deliveredWith(head + "Content-Disposition: attachment\r\n\r\ntext\r\n"),
		"not a text part":  deliveredWith(head + "Content-Type: application/octet-stream\r\n\r\ntext\r\n"),
		"quoted bare cr":   deliveredWith(head + "\r\nline\rline\r\n"),
	}
	for name, d := range cases {
		reply, err := Reply(d, userAddr, "Re: ", "正文 BODY-CANARY")
		if err == nil || reply != nil {
			t.Errorf("%s: Reply succeeded", name)
			continue
		}
		for _, leak := range []string{canary, "BODY-CANARY", userAddr, botAddr, "example.invalid"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s: error %q leaks %q", name, err, leak)
			}
		}
	}
	valid := deliveredWith(head + "\r\ntext\r\n")
	for name, args := range map[string][3]string{
		"from with line break":   {"a@example.invalid\r\nBcc: b@example.invalid", "Re: ", "正文"},
		"prefix with line break": {userAddr, "Re:\n", "正文"},
		"body with bare cr":      {userAddr, "Re: ", "a\rb"},
	} {
		if reply, err := Reply(valid, args[0], args[1], args[2]); err == nil || reply != nil {
			t.Errorf("%s: Reply succeeded", name)
		}
	}
}

// TestRewriteMessageID 覆盖改写头部的边界：Message-Id 折行、名称大小写不同、出现两次、原有 X-OQ-MSGID、没有 Message-Id、
// 只有 LF 的换行、没有正文、头部之前就是空行。每种情况下改写后的头部只有一个 Message-Id，X-OQ-MSGID 只在原信有 ID 时出现。
func TestRewriteMessageID(t *testing.T) {
	const id = "<tencent_NEW@qq.com>"
	cases := []struct {
		name, raw, want, original string
	}{
		{"folded", "From: a\r\nMessage-Id:\r\n <old@example.invalid>\r\nTo: b\r\n\r\nbody\r\n",
			"From: a\r\nMessage-Id: " + id + "\r\nX-OQ-MSGID: <old@example.invalid>\r\nTo: b\r\n\r\nbody\r\n", "<old@example.invalid>"},
		{"case and duplicate", "MESSAGE-ID: <one@example.invalid>\r\nmessage-id: <two@example.invalid>\r\nX-OQ-MSGID: <stale@example.invalid>\r\n\r\nbody",
			"Message-Id: " + id + "\r\nX-OQ-MSGID: <one@example.invalid>\r\n\r\nbody", "<one@example.invalid>"},
		{"missing", "From: a\r\nSubject: s\r\n\r\nbody\r\n", "Message-Id: " + id + "\r\nFrom: a\r\nSubject: s\r\n\r\nbody\r\n", ""},
		{"empty value", "Message-Id:  \r\n\r\nbody\r\n", "Message-Id: " + id + "\r\n\r\nbody\r\n", ""},
		{"bare LF", "From: a\nMessage-Id: <lf@example.invalid>\n\nbody\n",
			"From: a\nMessage-Id: " + id + "\nX-OQ-MSGID: <lf@example.invalid>\n\nbody\n", "<lf@example.invalid>"},
		{"header only", "Message-Id: <h@example.invalid>\r\nX-Other: 1", "Message-Id: " + id + "\r\nX-OQ-MSGID: <h@example.invalid>\r\nX-Other: 1", "<h@example.invalid>"},
		{"no header", "\r\nbody\r\n", "Message-Id: " + id + "\r\n\r\nbody\r\n", ""},
		{"leading continuation", " stray\r\nMessage-Id: <c@example.invalid>\r\n\r\n",
			" stray\r\nMessage-Id: " + id + "\r\nX-OQ-MSGID: <c@example.invalid>\r\n\r\n", "<c@example.invalid>"},
	}
	for _, c := range cases {
		out, original := rewriteID([]byte(c.raw), id)
		if string(out) != c.want || original != c.original {
			t.Errorf("%s: rewriteID = %q, %q; want %q, %q", c.name, out, original, c.want, c.original)
		}
	}
}

// TestSMTPSessionRules 确认模拟器照 QQ 的规矩接收 SMTP：没有认证就 MAIL 被拒；信封发件人必须等于登录账户；授权码错误、
// 用户名不符或授权身份不符时 AUTH 以 535 失败（smtp.Send 得到 ErrAuth）；只提供 AUTH PLAIN。
func TestSMTPSessionRules(t *testing.T) {
	srv := start(t, Options{})
	msg, _ := notification("<tc.rules@example.invalid>")
	if _, err := send(t, srv, botPassword+"x", msg); !errors.Is(err, smtp.ErrAuth) {
		t.Errorf("Send with a wrong password = %v, want ErrAuth", err)
	}
	client, err := gosmtp.DialTLS(net.JoinHostPort(srv.Host(), strconv.Itoa(srv.SMTPPort())),
		&tls.Config{ServerName: srv.Host(), RootCAs: srv.RootCAs()})
	if err != nil {
		t.Fatalf("DialTLS: %v", err)
	}
	defer client.Close()
	var smtpErr *gosmtp.SMTPError
	if err := client.Mail(botAddr, nil); !errors.As(err, &smtpErr) || smtpErr.Code != 503 {
		t.Errorf("MAIL before AUTH = %v, want 503", err)
	}
	if ok, mechs := client.Extension("AUTH"); !ok || mechs != "PLAIN" {
		t.Errorf("AUTH mechanisms = %q", mechs)
	}
	for _, c := range []struct{ identity, user string }{{"", userAddr}, {userAddr, botAddr}} {
		if err := client.Auth(plainClient(c.identity, c.user, botPassword)); !errors.As(err, &smtpErr) || smtpErr.Code != 535 {
			t.Errorf("AUTH as %q/%q = %v, want 535", c.identity, c.user, err)
		}
	}
	if err := client.Auth(loginClient{}); !errors.As(err, &smtpErr) || smtpErr.Code != 504 {
		t.Errorf("AUTH LOGIN = %v, want 504", err)
	}
	if err := client.Auth(plainClient(botAddr, botAddr, botPassword)); err != nil {
		t.Fatalf("AUTH with the authorization identity: %v", err)
	}
	if err := client.Mail(userAddr, nil); !errors.As(err, &smtpErr) || smtpErr.Code != 501 {
		t.Errorf("MAIL FROM another address = %v, want 501", err)
	}
	if err := client.Mail(botAddr, nil); err != nil {
		t.Errorf("MAIL FROM the login address: %v", err)
	}
	if got := srv.Stats().SMTPAuths; got != 4 {
		t.Errorf("SMTPAuths = %d, want 4 (LOGIN never reaches the authenticator)", got)
	}
}

// TestIMAPListPatterns 用 imapclient 直接发 LIST：空模式只返回层级分隔符（不可选的根），通配符按模式筛选文件夹。
func TestIMAPListPatterns(t *testing.T) {
	srv := start(t, Options{})
	c, err := imapclient.DialTLS(net.JoinHostPort(srv.Host(), strconv.Itoa(srv.IMAPPort())),
		&imapclient.Options{TLSConfig: &tls.Config{ServerName: srv.Host(), RootCAs: srv.RootCAs()}})
	if err != nil {
		t.Fatalf("DialTLS: %v", err)
	}
	defer c.Close()
	if err := c.Login(botAddr, botPassword).Wait(); err != nil {
		t.Fatalf("LOGIN: %v", err)
	}
	root, err := c.List("", "", nil).Collect()
	if err != nil || len(root) != 1 || root[0].Mailbox != "" || root[0].Delim != '/' ||
		!slices.Contains(root[0].Attrs, goimap.MailboxAttrNoSelect) {
		t.Errorf("LIST \"\" \"\" = %+v (err %v)", root, err)
	}
	sent, err := c.List("", "S*", nil).Collect()
	if err != nil || len(sent) != 1 || sent[0].Mailbox != imap.FolderSent {
		t.Errorf("LIST \"\" \"S*\" = %+v (err %v)", sent, err)
	}
	status, err := c.Status(imap.FolderSent, &goimap.StatusOptions{UIDValidity: true, NumMessages: true}).Wait()
	if err != nil || status.UIDValidity != srv.uidValidity || status.NumMessages == nil || *status.NumMessages != 0 {
		t.Errorf("STATUS = %+v (err %v)", status, err)
	}
}

// TestIMAPSessionIsReadOnly 确认会修改邮箱的命令一律以 NO 拒绝：模拟器记下的邮件与文件夹因此总是与 imapmemserver 一致，
// 产品若开始写邮箱也会在这里暴露。直接调用会话的方法，参数中的写出器不会被用到。
func TestIMAPSessionIsReadOnly(t *testing.T) {
	srv := start(t, Options{})
	sess := &imapSession{srv: srv}
	if err := sess.Login(botAddr, botPassword); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if _, err := sess.Select(imap.FolderInbox, &goimap.SelectOptions{}); err != nil {
		t.Fatalf("Select: %v", err)
	}
	calls := map[string]func() error{
		"create":      func() error { return sess.Create("New", nil) },
		"delete":      func() error { return sess.Delete(imap.FolderJunk) },
		"rename":      func() error { return sess.Rename(imap.FolderJunk, "Old", nil) },
		"subscribe":   func() error { return sess.Subscribe(imap.FolderJunk) },
		"unsubscribe": func() error { return sess.Unsubscribe(imap.FolderJunk) },
		"append": func() error {
			_, err := sess.Append(imap.FolderInbox, bytes.NewReader([]byte("x")), &goimap.AppendOptions{})
			return err
		},
		"expunge": func() error { return sess.Expunge(nil, nil) },
		"store":   func() error { return sess.Store(nil, nil, nil, nil) },
		"copy": func() error {
			_, err := sess.Copy(nil, imap.FolderJunk)
			return err
		},
		"move": func() error { return sess.Move(nil, nil, imap.FolderJunk) },
	}
	for name, call := range calls {
		var imapErr *goimap.Error
		if err := call(); !errors.As(err, &imapErr) || imapErr.Type != goimap.StatusResponseTypeNo {
			t.Errorf("%s = %v, want NO", name, err)
		}
	}
	if stored, err := srv.Messages(imap.FolderInbox); err != nil || len(stored) != 0 {
		t.Errorf("INBOX changed: %d messages, err %v", len(stored), err)
	}
	if _, err := sess.Status("Drafts", &goimap.StatusOptions{UIDValidity: true}); err == nil {
		t.Error("Status accepted a folder that does not exist")
	}
	if err := sess.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := (&imapSession{srv: srv}).Close(); err != nil {
		t.Errorf("Close before login: %v", err)
	}
}

// TestSMTPConnReadsLines 覆盖 SMTP 连接包装的读取边界：命令按行交出、一次读不完的行分几次交出、连接在行中途结束时先交出残行
// 再报告错误、超长而没有行尾的数据按上限分段交出；DATA 故障只拦截恰为 DATA 的命令行。
func TestSMTPConnReadsLines(t *testing.T) {
	srv := start(t, Options{})
	setFaults(t, srv, Faults{DataCode: 451})
	client, server := net.Pipe()
	c := &smtpConn{Conn: server, srv: srv}
	long := strings.Repeat("x", maxCommandLine+10)
	go func() {
		_, _ = client.Write([]byte("NOOP\r\nDATA-ish\r\n" + long + "\r\nTAIL"))
		_ = client.Close()
	}()
	var got []string
	buf := make([]byte, 4)
	var data []byte
	for {
		n, err := c.Read(buf)
		data = append(data, buf[:n]...)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("Read: %v", err)
			}
			break
		}
	}
	got = strings.SplitAfter(string(data), "\r\n")
	if want := []string{"NOOP\r\n", "DATA-ish\r\n", long + "\r\n", "TAIL"}; !slices.Equal(got, want) {
		t.Errorf("lines = %q", got)
	}
}

// TestNewValidatesOptions 确认 New 拒绝缺少或含空白、换行的机器人地址与授权码，错误文本不回显它们。
func TestNewValidatesOptions(t *testing.T) {
	for name, opts := range map[string]Options{
		"no address":         {Password: botPassword},
		"address without @":  {Address: "bot", Password: botPassword},
		"address with space": {Address: "bot @example.invalid", Password: botPassword},
		"address with CRLF":  {Address: botAddr + "\r\n", Password: botPassword},
		"no password":        {Address: botAddr},
		"password with tab":  {Address: botAddr, Password: botPassword + "\t"},
		"non-ascii password": {Address: botAddr, Password: botPassword + "é"},
	} {
		srv, err := New(opts)
		if err == nil {
			_ = srv.Close()
			t.Errorf("%s: New succeeded", name)
			continue
		}
		if strings.Contains(err.Error(), botPassword) || strings.Contains(err.Error(), "example.invalid") {
			t.Errorf("%s: error %q echoes an option", name, err)
		}
	}
}

// TestSetFaultsValidates 确认故障的状态码只接受 4xx 与 5xx，延迟不能为负，Drop 只能取已定义的值；非法时原有故障不变。
func TestSetFaultsValidates(t *testing.T) {
	srv := start(t, Options{})
	setFaults(t, srv, Faults{RcptCode: 550})
	for name, f := range map[string]Faults{
		"2xx":            {MailCode: 250},
		"3xx":            {DataCode: 354},
		"6xx":            {AuthCode: 600},
		"negative code":  {SubmissionCode: -1},
		"negative delay": {SentDelay: -time.Second},
		"unknown drop":   {Drop: DropDiscarded + 1},
		"negative drop":  {Drop: -1},
	} {
		if err := srv.SetFaults(f); err == nil {
			t.Errorf("%s: SetFaults accepted the faults", name)
		}
	}
	msg, _ := notification("<tc.validate@example.invalid>")
	var reply *smtp.ReplyError
	if _, err := send(t, srv, botPassword, msg); !errors.As(err, &reply) || reply.Step != "rcpt" {
		t.Errorf("the original fault was lost: %v", err)
	}
}

// TestClose 确认 Close 可重复调用；关闭后 Delivered 通道关闭、Deliver 失败、新连接被拒绝，未交出的收件人副本随之丢弃。
func TestClose(t *testing.T) {
	srv, err := New(Options{Address: botAddr, Password: botPassword})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	msg, _ := notification("<tc.close@example.invalid>")
	if _, err := send(t, srv, botPassword, msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	s := mustDial(t, srv)
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	for range srv.Delivered() {
		// 关闭之前可能已经交出一封；通道随后关闭，循环结束。
	}
	if _, err := srv.Deliver(imap.FolderInbox, []byte("x")); err == nil {
		t.Error("Deliver succeeded after Close")
	}
	if _, err := s.ListFolders(context.Background()); !errors.Is(err, imap.ErrClosed) {
		t.Errorf("an open session survived Close: %v", err)
	}
	if _, err := dial(t, srv, botPassword); err == nil {
		t.Error("a new IMAP connection succeeded after Close")
	}
	if _, err := send(t, srv, botPassword, msg); !errors.Is(err, smtp.ErrNotSent) {
		t.Errorf("Send after Close = %v, want ErrNotSent", err)
	}
	if n := srv.DisconnectAll(); n != 0 {
		t.Errorf("DisconnectAll after Close closed %d connections", n)
	}
}

// TestConfigs 确认 IMAPConfig 与 SMTPConfig 指向本机回环地址上的两个不同端口、登录名为机器人地址、根证书池只信任模拟器的 CA。
func TestConfigs(t *testing.T) {
	srv := start(t, Options{})
	ic, sc := srv.IMAPConfig(), srv.SMTPConfig()
	if ic.Host != "127.0.0.1" || sc.Host != "127.0.0.1" || ic.Port != srv.IMAPPort() || sc.Port != srv.SMTPPort() ||
		ic.Port == sc.Port || ic.Username != botAddr || sc.Username != botAddr || ic.RootCAs != srv.RootCAs() || sc.RootCAs != srv.RootCAs() {
		t.Errorf("IMAP %+v, SMTP %+v", ic, sc)
	}
	other := start(t, Options{})
	if _, err := tls.Dial("tcp", net.JoinHostPort(other.Host(), strconv.Itoa(other.IMAPPort())),
		&tls.Config{ServerName: other.Host(), RootCAs: srv.RootCAs()}); err == nil {
		t.Error("one simulator's CA verifies another simulator's certificate")
	}
	conn, err := tls.Dial("tcp", net.JoinHostPort(srv.Host(), strconv.Itoa(srv.SMTPPort())),
		&tls.Config{ServerName: "localhost", RootCAs: srv.RootCAs()})
	if err != nil {
		t.Fatalf("TLS with the name localhost: %v", err)
	}
	_ = conn.Close()
}

// plainClient 返回以给定授权身份、用户名与授权码做 AUTH PLAIN 的客户端。
func plainClient(identity, username, password string) plainAuth {
	return plainAuth{identity: identity, username: username, password: password}
}

// plainAuth 是测试用的 AUTH PLAIN 客户端：可以带授权身份（产品的客户端总是留空）。
type plainAuth struct {
	identity, username, password string
}

// Start 返回 PLAIN 机制与初始响应。
func (a plainAuth) Start() (string, []byte, error) {
	return "PLAIN", []byte(a.identity + "\x00" + a.username + "\x00" + a.password), nil
}

// Next 不应被调用：PLAIN 只有一轮。
func (a plainAuth) Next([]byte) ([]byte, error) {
	return nil, errors.New("unexpected challenge")
}

// loginClient 是测试用的 AUTH LOGIN 客户端，用来确认模拟器只接受 PLAIN。
type loginClient struct{}

// Start 返回 LOGIN 机制，没有初始响应。
func (loginClient) Start() (string, []byte, error) {
	return "LOGIN", nil, nil
}

// Next 不应被调用：服务器拒绝 LOGIN。
func (loginClient) Next([]byte) ([]byte, error) {
	return nil, errors.New("unexpected challenge")
}

// TestHeaderHelpers 钉住测试辅助 withoutFields 与 headerOf 依赖的格式：无关的字段原样保留。
func TestHeaderHelpers(t *testing.T) {
	raw := []byte("A: 1\r\nMessage-Id: <x@example.invalid>\r\nB: 2\r\n\r\nbody")
	if got := string(withoutFields(raw, "message-id")); got != "A: 1\r\nB: 2\r\n\r\nbody" {
		t.Errorf("withoutFields = %q", got)
	}
	if h := headerOf(t, raw); textproto.MIMEHeader(h).Get("B") != "2" {
		t.Error("headerOf lost a field")
	}
}
