// Package imap 用离线假服务器验证单连接会话：收取与游标、只读、命令白名单、每条命令的期限、补扫中到达的 EXISTS、
// IDLE 推送、UIDVALIDITY 变化、批量与大小上限、认证与传输安全、文件夹错误；测试不连接任何真实服务器。
package imap

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
)

// testTimeouts 返回测试用的短期限（100–500ms 级别）；拨号含 TLS 握手，放宽到 1 秒以免在 -race 下误报。
func testTimeouts() Timeouts {
	return Timeouts{
		Dial:     time.Second,
		Greeting: 300 * time.Millisecond,
		Command:  300 * time.Millisecond,
		Fetch:    500 * time.Millisecond,
		IdleAck:  300 * time.Millisecond,
		IdleMax:  500 * time.Millisecond,
		IdleStop: 300 * time.Millisecond,
		Poll:     200 * time.Millisecond,
	}
}

// deadlineTimeouts 返回期限用例的期限：除拨号（1 秒）外都为 5 秒，再由 set 把被测步骤的期限改短。
// 步骤若换用了其他期限字段，耗时会明显超出被测期限，用例因此能发现。
func deadlineTimeouts(set func(*Timeouts)) Timeouts {
	tm := Timeouts{Dial: time.Second, Greeting: 5 * time.Second, Command: 5 * time.Second, Fetch: 5 * time.Second,
		IdleAck: 5 * time.Second, IdleMax: 5 * time.Second, IdleStop: 5 * time.Second, Poll: 5 * time.Second}
	set(&tm)
	return tm
}

// dial 以测试密码登录假服务器；用例结束时关闭会话。
func dial(t *testing.T, fs *fakeServer, timeouts Timeouts) *Session {
	t.Helper()
	s, err := Dial(context.Background(), fs.config(timeouts), testPassword)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// scan 调用 Scan，失败即终止用例。
func scan(t *testing.T, s *Session, folder string, cur Cursor) Batch {
	t.Helper()
	b, err := s.Scan(context.Background(), folder, cur)
	if err != nil {
		t.Fatalf("Scan(%s, %+v): %v", folder, cur, err)
	}
	return b
}

// uids 返回批次中各邮件的 UID。
func uids(b Batch) []uint32 {
	var out []uint32
	for _, m := range b.Messages {
		out = append(out, m.UID)
	}
	return out
}

// setVar 把包内变量改为 v，用例结束时恢复原值。
func setVar[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// assertWithin 断言从 start 起的耗时不超过 limit。
func assertWithin(t *testing.T, start time.Time, limit time.Duration, what string) {
	t.Helper()
	if elapsed := time.Since(start); elapsed > limit {
		t.Errorf("%s took %v, want at most %v", what, elapsed, limit)
	}
}

// assertClosed 断言会话的各个方法都返回 ErrClosed，且代理看到客户端关闭了连接。
func assertClosed(t *testing.T, fs *fakeServer, s *Session) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Scan(ctx, FolderInbox, Cursor{}); !errors.Is(err, ErrClosed) {
		t.Errorf("Scan after failure = %v, want ErrClosed", err)
	}
	if _, err := s.Examine(ctx, FolderInbox); !errors.Is(err, ErrClosed) {
		t.Errorf("Examine after failure = %v, want ErrClosed", err)
	}
	if _, err := s.ListFolders(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("ListFolders after failure = %v, want ErrClosed", err)
	}
	if _, err := s.Idle(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Idle after failure = %v, want ErrClosed", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close after failure = %v", err)
	}
	waitFor(t, "the client to close the connection", func() bool { return fs.openConns() == 0 })
}

// TestScanFetchesNewMessagesReadOnly 覆盖收取与游标以及只读：全量补扫、过滤服务器对 last+1:* 返回的最后一个 UID、
// 增量收取；全程没有设置 \Seen，每个正文数据项都是 BODY.PEEK[]<0.N>。
func TestScanFetchesNewMessagesReadOnly(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	var inbox [][]byte
	for i := 1; i <= 3; i++ {
		raw := testMessage(i, 200*i)
		inbox = append(inbox, raw)
		fs.appendMessage(FolderInbox, raw)
	}
	junk := testMessage(9, 300)
	fs.appendMessage(FolderJunk, junk)
	s := dial(t, fs, testTimeouts())
	uv := fs.status(FolderInbox).UIDValidity

	b := scan(t, s, FolderInbox, Cursor{})
	if b.Folder != FolderInbox || !b.Reset || b.UIDValidity != uv || b.More || b.Next != (Cursor{UIDValidity: uv, LastUID: 3}) {
		t.Fatalf("first batch = %+v", b)
	}
	if !slices.Equal(uids(b), []uint32{1, 2, 3}) {
		t.Fatalf("first batch UIDs = %v", uids(b))
	}
	for i, m := range b.Messages {
		if !bytes.Equal(m.Raw, inbox[i]) || m.Size != int64(len(inbox[i])) || m.TooLarge {
			t.Errorf("message %d = size %d, too large %v, raw %q", m.UID, m.Size, m.TooLarge, m.Raw)
		}
	}

	again := scan(t, s, FolderInbox, b.Next)
	if again.Reset || len(again.Messages) != 0 || again.More || again.Next != b.Next {
		t.Fatalf("rescan without new mail = %+v, want an empty batch", again)
	}
	fourth := testMessage(4, 100)
	fs.appendMessage(FolderInbox, fourth)
	next := scan(t, s, FolderInbox, b.Next)
	if next.Reset || !slices.Equal(uids(next), []uint32{4}) || !bytes.Equal(next.Messages[0].Raw, fourth) || next.Next.LastUID != 4 {
		t.Fatalf("incremental batch = %+v", next)
	}
	j := scan(t, s, FolderJunk, Cursor{})
	if j.Folder != FolderJunk || !slices.Equal(uids(j), []uint32{1}) || !bytes.Equal(j.Messages[0].Raw, junk) {
		t.Fatalf("junk batch = %+v", j)
	}

	for _, folder := range []string{FolderInbox, FolderJunk} {
		if st := fs.status(folder); *st.NumUnseen != *st.NumMessages {
			t.Errorf("%s has %d unseen of %d messages; the client set \\Seen", folder, *st.NumUnseen, *st.NumMessages)
		}
	}
	want := "BODY.PEEK[]<0." + strconv.Itoa(maxMessageSize+1) + ">"
	bodies := 0
	for _, cmd := range fs.commands() {
		for _, item := range cmd.Items {
			if strings.HasPrefix(item, "BODY") {
				bodies++
				if item != want {
					t.Errorf("body item = %q, want %q", item, want)
				}
			}
		}
	}
	if bodies != 5 {
		t.Errorf("body fetches = %d, want 5", bodies)
	}
}

// TestCapabilities 覆盖能力检查：Capabilities 返回登录后的大写、已排序的能力；没有 IDLE 时 Idle 不发送 IDLE；
// 公告 LOGINDISABLED 或没有 IMAP4rev1 时 Dial 不发送 LOGIN。
func TestCapabilities(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	s := dial(t, fs, testTimeouts())
	caps := s.Capabilities()
	if !slices.IsSorted(caps) || !slices.Contains(caps, "IMAP4REV1") || !slices.Contains(caps, "IDLE") {
		t.Errorf("Capabilities() = %q", caps)
	}
	for _, c := range caps {
		if c != strings.ToUpper(c) {
			t.Errorf("capability %q is not upper case", c)
		}
	}

	t.Run("no IDLE", func(t *testing.T) {
		fs := newFakeServer(t, proxyOptions{dropCaps: []string{"IDLE"}})
		s := dial(t, fs, testTimeouts())
		scan(t, s, FolderInbox, Cursor{})
		if _, err := s.Idle(context.Background()); !errors.Is(err, ErrCapability) {
			t.Errorf("Idle = %v, want ErrCapability", err)
		}
		if n := fs.count("IDLE"); n != 0 {
			t.Errorf("client sent %d IDLE commands", n)
		}
	})
	for _, tc := range []struct {
		name string
		opts proxyOptions
	}{
		{"LOGINDISABLED", proxyOptions{dropCaps: []string{"AUTH=PLAIN"}, addCaps: []string{"LOGINDISABLED"}}},
		{"no IMAP4rev1", proxyOptions{dropCaps: []string{"IMAP4rev1"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeServer(t, tc.opts)
			if _, err := Dial(context.Background(), fs.config(testTimeouts()), testPassword); !errors.Is(err, ErrCapability) {
				t.Errorf("Dial = %v, want ErrCapability", err)
			}
			if n := fs.count("LOGIN"); n != 0 {
				t.Errorf("client sent %d LOGIN commands", n)
			}
			waitFor(t, "the client to close the connection", func() bool { return fs.openConns() == 0 })
		})
	}
}

// TestCommandDeadlines 覆盖每条命令的期限：问候冻结、LOGIN 后冻结、EXAMINE 与 UID SEARCH 的无标签 BAD、
// 正文字面量传到一半冻结、IDLE 的无标签 BAD 与进入 IDLE 后半开。每个用例只把被测步骤的期限设短、其余都为 5 秒，
// 断言返回 ErrTimeout、耗时不超过被测期限加 500ms（步骤换用其他期限字段即超出），此后会话的调用都返回 ErrClosed。
func TestCommandDeadlines(t *testing.T) {
	const short, slack = 300 * time.Millisecond, 500 * time.Millisecond
	t.Run("greeting", func(t *testing.T) {
		fs := newFakeServer(t, proxyOptions{freezeGreeting: true})
		tm := deadlineTimeouts(func(x *Timeouts) { x.Greeting = short })
		start := time.Now()
		if _, err := Dial(context.Background(), fs.config(tm), testPassword); !errors.Is(err, ErrTimeout) {
			t.Fatalf("Dial = %v, want ErrTimeout", err)
		}
		assertWithin(t, start, short+slack, "Dial with a frozen greeting")
		waitFor(t, "the client to close the connection", func() bool { return fs.openConns() == 0 })
	})
	t.Run("login", func(t *testing.T) {
		fs := newFakeServer(t, proxyOptions{})
		fs.addRule(&rule{command: "LOGIN", kind: faultFreeze})
		tm := deadlineTimeouts(func(x *Timeouts) { x.Command = short })
		start := time.Now()
		if _, err := Dial(context.Background(), fs.config(tm), testPassword); !errors.Is(err, ErrTimeout) {
			t.Fatalf("Dial = %v, want ErrTimeout", err)
		}
		assertWithin(t, start, short+slack, "Dial with a frozen LOGIN")
		waitFor(t, "the client to close the connection", func() bool { return fs.openConns() == 0 })
	})
	command := deadlineTimeouts(func(x *Timeouts) { x.Command = short })
	fetch := deadlineTimeouts(func(x *Timeouts) { x.Fetch = short })
	idleAck := deadlineTimeouts(func(x *Timeouts) { x.IdleAck = short })
	// IdleStop 远短于 IdleMax：DONE 若换用 IdleMax，耗时约为 2 倍 IdleMax，超出上限。
	idleEnd := deadlineTimeouts(func(x *Timeouts) { x.IdleMax, x.IdleStop = time.Second, 100*time.Millisecond })
	for _, tc := range []struct {
		name  string
		rule  rule
		tm    Timeouts
		limit time.Duration
		idle  bool
	}{
		{"examine untagged BAD", rule{command: "EXAMINE", kind: faultBAD}, command, short, false},
		{"uid search untagged BAD", rule{command: "UID SEARCH", kind: faultBAD}, command, short, false},
		{"size fetch untagged BAD", rule{command: "UID FETCH", contains: "RFC822.SIZE", kind: faultBAD}, fetch, short, false},
		{"body literal stalls halfway", rule{command: "UID FETCH", contains: "BODY.PEEK", kind: faultFreezeAfter, bytes: 512}, fetch, short, false},
		{"idle untagged BAD", rule{command: "IDLE", kind: faultBAD}, idleAck, short, true},
		{"half-open during idle", rule{command: "IDLE", kind: faultFreezeAfter, bytes: len(idleContinuation)}, idleEnd, idleEnd.IdleMax + idleEnd.IdleStop, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeServer(t, proxyOptions{})
			fs.appendMessage(FolderInbox, testMessage(1, 4096))
			s := dial(t, fs, tc.tm)
			if tc.idle {
				scan(t, s, FolderInbox, Cursor{})
			}
			r := tc.rule
			fs.addRule(&r)
			start := time.Now()
			var err error
			if tc.idle {
				_, err = s.Idle(context.Background())
			} else {
				_, err = s.Scan(context.Background(), FolderInbox, Cursor{})
			}
			if !errors.Is(err, ErrTimeout) {
				t.Fatalf("err = %v, want ErrTimeout", err)
			}
			assertWithin(t, start, tc.limit+slack, tc.name)
			if tc.idle && tc.rule.kind == faultFreezeAfter && time.Since(start) < tc.tm.IdleMax {
				t.Errorf("Idle returned after %v, before IdleMax", time.Since(start))
			}
			assertClosed(t, fs, s)
		})
	}
}

// TestContextCancellation 覆盖 ctx：已结束的 ctx 不发送命令；命令进行中取消 ctx 立即关闭连接并返回 ctx 的错误；IDLE 中取消同样如此。
func TestContextCancellation(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	s := dial(t, fs, testTimeouts())
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	before := len(fs.commands())
	if _, err := s.Scan(canceled, FolderInbox, Cursor{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Scan with a canceled ctx = %v, want context.Canceled", err)
	}
	if n := len(fs.commands()); n != before {
		t.Errorf("client sent %d commands with a canceled ctx", n-before)
	}
	if _, err := Dial(canceled, fs.config(testTimeouts()), testPassword); !errors.Is(err, context.Canceled) {
		t.Errorf("Dial with a canceled ctx = %v, want context.Canceled", err)
	}
	scan(t, s, FolderInbox, Cursor{})

	fs.addRule(&rule{command: "EXAMINE", kind: faultFreeze})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	if _, err := s.Scan(ctx, FolderInbox, Cursor{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Scan canceled mid-command = %v, want context.Canceled", err)
	}
	assertWithin(t, start, time.Second, "Scan canceled mid-command")
	assertClosed(t, fs, s)

	fs2 := newFakeServer(t, proxyOptions{})
	tm := testTimeouts()
	tm.IdleMax = 5 * time.Second
	s2 := dial(t, fs2, tm)
	scan(t, s2, FolderInbox, Cursor{})
	ctx2, cancel2 := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel2)
	start = time.Now()
	if _, err := s2.Idle(ctx2); !errors.Is(err, context.Canceled) {
		t.Fatalf("Idle canceled = %v, want context.Canceled", err)
	}
	assertWithin(t, start, time.Second, "Idle canceled")
	assertClosed(t, fs2, s2)
}

// TestServerDisconnect 覆盖服务器断开：下一次调用返回 ErrClosed；IDLE 进行中断开时 Idle 在 1 秒内返回 ErrClosed，不等到 IdleMax。
func TestServerDisconnect(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	s := dial(t, fs, testTimeouts())
	scan(t, s, FolderInbox, Cursor{})
	fs.disconnectAll()
	if _, err := s.Scan(context.Background(), FolderInbox, Cursor{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Scan after disconnect = %v, want ErrClosed", err)
	}
	assertClosed(t, fs, s)

	fs2 := newFakeServer(t, proxyOptions{})
	tm := testTimeouts()
	tm.IdleMax = 5 * time.Second
	s2 := dial(t, fs2, tm)
	scan(t, s2, FolderInbox, Cursor{})
	go func() {
		deadline := time.Now().Add(waitLimit)
		for fs2.count("IDLE") == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond) // 等客户端收到继续响应、进入等待
		fs2.disconnectAll()
	}()
	start := time.Now()
	if _, err := s2.Idle(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Idle when the server disconnects = %v, want ErrClosed", err)
	}
	assertWithin(t, start, time.Second, "Idle when the server disconnects")
	assertClosed(t, fs2, s2)
}

// TestExistsDuringScan 覆盖补扫中到达的 EXISTS：分别在 UID SEARCH 与 UID FETCH 的响应前注入，Scan 正常完成，
// 随后 Idle 不发送 IDLE、立即返回 true。
func TestExistsDuringScan(t *testing.T) {
	for _, tc := range []struct{ name, command, contains string }{
		{"uid search", "UID SEARCH", ""},
		{"uid fetch", "UID FETCH", "BODY.PEEK"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeServer(t, proxyOptions{})
			fs.appendMessage(FolderInbox, testMessage(1, 100))
			s := dial(t, fs, testTimeouts())
			fs.addRule(&rule{command: tc.command, contains: tc.contains, kind: faultInject, exists: []uint32{5}, limit: 1})
			if b := scan(t, s, FolderInbox, Cursor{}); len(b.Messages) != 1 {
				t.Fatalf("batch = %+v", b)
			}
			start := time.Now()
			newMail, err := s.Idle(context.Background())
			if err != nil || !newMail {
				t.Fatalf("Idle = %v, %v; want true, nil", newMail, err)
			}
			assertWithin(t, start, 100*time.Millisecond, "Idle with a pending EXISTS")
			if n := fs.count("IDLE"); n != 0 {
				t.Errorf("client sent %d IDLE commands", n)
			}
		})
	}
}

// TestExistsHandlerNeverBlocks 在 UID SEARCH 的响应前连续注入两条 EXISTS，其间没有任何一方取走信号：
// 处理函数若阻塞发送，第二条会让解码协程停住，Scan 以 ErrTimeout 失败。
func TestExistsHandlerNeverBlocks(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	fs.appendMessage(FolderInbox, testMessage(1, 100))
	s := dial(t, fs, testTimeouts())
	fs.addRule(&rule{command: "UID SEARCH", kind: faultInject, exists: []uint32{5, 6}, limit: 1})
	if b := scan(t, s, FolderInbox, Cursor{}); len(b.Messages) != 1 {
		t.Fatalf("batch = %+v", b)
	}
	if newMail, err := s.Idle(context.Background()); err != nil || !newMail {
		t.Fatalf("Idle = %v, %v; want true, nil", newMail, err)
	}
	if n := fs.count("IDLE"); n != 0 {
		t.Errorf("client sent %d IDLE commands", n)
	}
}

// TestScanClearsStaleSignal 覆盖不残留旧信号：注入 EXISTS 后不调用 Idle 而再次 Scan，随后 Idle 发出 IDLE，
// 无新邮件时在 IdleMax 到期返回 false，连接仍可用。
func TestScanClearsStaleSignal(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	tm := testTimeouts()
	s := dial(t, fs, tm)
	fs.addRule(&rule{command: "UID SEARCH", kind: faultInject, exists: []uint32{5}, limit: 1})
	scan(t, s, FolderInbox, Cursor{})
	b := scan(t, s, FolderInbox, Cursor{})
	start := time.Now()
	newMail, err := s.Idle(context.Background())
	if err != nil || newMail {
		t.Fatalf("Idle = %v, %v; want false, nil", newMail, err)
	}
	if elapsed := time.Since(start); elapsed < tm.IdleMax || elapsed > tm.IdleMax+time.Second {
		t.Errorf("Idle returned after %v, want about IdleMax %v", elapsed, tm.IdleMax)
	}
	if n := fs.count("IDLE"); n != 1 {
		t.Errorf("client sent %d IDLE commands, want 1", n)
	}
	if n := fs.count("DONE"); n != 1 {
		t.Errorf("client sent %d DONE, want 1", n)
	}
	fs.appendMessage(FolderInbox, testMessage(1, 100))
	if next := scan(t, s, FolderInbox, b.Next); !slices.Equal(uids(next), []uint32{1}) {
		t.Errorf("batch after idle = %+v", next)
	}
}

// TestExistsProbeSequence 按 Task 13 TestL1Idle 的预检顺序（Examine、以 UIDNEXT−1 为游标 Scan 两次、再 Examine、Idle）
// 验证它能分辨两种服务器：每条 UID SEARCH 响应都附带邮件数不变的 EXISTS 时，两次 Examine 的状态相同，Idle 不发 IDLE
// 立即返回 true；不附带时 Idle 发出 IDLE，到 IdleMax 返回 false。两种情况都不取回已有邮件。
func TestExistsProbeSequence(t *testing.T) {
	for _, attach := range []bool{true, false} {
		t.Run(fmt.Sprintf("attach=%v", attach), func(t *testing.T) {
			fs := newFakeServer(t, proxyOptions{})
			fs.appendMessage(FolderInbox, testMessage(1, 100))
			s := dial(t, fs, testTimeouts())
			if attach {
				fs.addRule(&rule{command: "UID SEARCH", kind: faultInject, exists: []uint32{1}})
			}
			ctx := context.Background()
			before, err := s.Examine(ctx, FolderInbox)
			if err != nil {
				t.Fatalf("Examine: %v", err)
			}
			cur := Cursor{UIDValidity: before.UIDValidity, LastUID: before.UIDNext - 1}
			for range 2 {
				if b := scan(t, s, FolderInbox, cur); len(b.Messages) != 0 || b.Next != cur {
					t.Fatalf("batch = %+v", b)
				}
			}
			after, err := s.Examine(ctx, FolderInbox)
			if err != nil || after != before {
				t.Fatalf("Examine = %+v, %v; want %+v", after, err, before)
			}
			start := time.Now()
			newMail, err := s.Idle(ctx)
			if err != nil || newMail != attach {
				t.Fatalf("Idle = %v, %v; want %v, nil", newMail, err, attach)
			}
			wantIdle := 1
			if attach {
				assertWithin(t, start, 100*time.Millisecond, "Idle with EXISTS attached to UID SEARCH")
				wantIdle = 0
			}
			if n := fs.count("IDLE"); n != wantIdle {
				t.Errorf("client sent %d IDLE commands, want %d", n, wantIdle)
			}
			if n := fs.count("UID FETCH"); n != 0 {
				t.Errorf("client sent %d UID FETCH commands, want 0", n)
			}
		})
	}
}

// TestIdlePush 覆盖 IDLE 推送：IDLE 期间放入 1 封，Idle 在 1 秒内返回 true。
func TestIdlePush(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	tm := testTimeouts()
	tm.IdleMax = 5 * time.Second
	s := dial(t, fs, tm)
	b := scan(t, s, FolderInbox, Cursor{})
	go func() {
		deadline := time.Now().Add(waitLimit)
		for fs.count("IDLE") == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		_, _ = fs.put(FolderInbox, testMessage(1, 100))
	}()
	start := time.Now()
	newMail, err := s.Idle(context.Background())
	if err != nil || !newMail {
		t.Fatalf("Idle = %v, %v; want true, nil", newMail, err)
	}
	assertWithin(t, start, time.Second, "Idle with a pushed message")
	if next := scan(t, s, FolderInbox, b.Next); !slices.Equal(uids(next), []uint32{1}) {
		t.Errorf("batch after push = %+v", next)
	}
}

// TestScanUIDValidityChange 覆盖 UIDVALIDITY 变化：删除并重建 Junk 后，以旧游标补扫得到 Reset 与从 UID 1 开始的邮件。
func TestScanUIDValidityChange(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	s := dial(t, fs, testTimeouts())
	old := fs.status(FolderJunk).UIDValidity
	if err := fs.user.Delete(FolderJunk); err != nil {
		t.Fatal(err)
	}
	if err := fs.user.Create(FolderJunk, nil); err != nil {
		t.Fatal(err)
	}
	fs.appendMessage(FolderJunk, testMessage(1, 100))
	fs.appendMessage(FolderJunk, testMessage(2, 100))
	b := scan(t, s, FolderJunk, Cursor{UIDValidity: old, LastUID: 5})
	uv := fs.status(FolderJunk).UIDValidity
	if uv == old || !b.Reset || b.UIDValidity != uv || !slices.Equal(uids(b), []uint32{1, 2}) || b.Next != (Cursor{UIDValidity: uv, LastUID: 2}) {
		t.Fatalf("batch = %+v (old uidvalidity %d, new %d)", b, old, uv)
	}
	if again := scan(t, s, FolderJunk, Cursor{UIDValidity: uv}); again.Reset || len(again.Messages) != 2 {
		t.Errorf("scan with a matching zero cursor = %+v", again)
	}
}

// TestScanBatches 覆盖批量：120 封邮件依次得到 50、50、20 封，前两批 More 为 true；恰为 50 封时一批取完，51 封时分为 50 与 1。
func TestScanBatches(t *testing.T) {
	for _, tc := range []struct {
		total int
		sizes []int
		more  []bool
	}{
		{120, []int{50, 50, 20}, []bool{true, true, false}},
		{51, []int{50, 1}, []bool{true, false}},
		{50, []int{50}, []bool{false}},
	} {
		t.Run(strconv.Itoa(tc.total), func(t *testing.T) {
			fs := newFakeServer(t, proxyOptions{})
			for i := 1; i <= tc.total; i++ {
				fs.appendMessage(FolderInbox, testMessage(i, 100))
			}
			s := dial(t, fs, testTimeouts())
			cur := Cursor{}
			var sizes []int
			var more []bool
			first := uint32(1)
			for range tc.sizes {
				b := scan(t, s, FolderInbox, cur)
				sizes = append(sizes, len(b.Messages))
				more = append(more, b.More)
				for i, uid := range uids(b) {
					if uid != first+uint32(i) {
						t.Fatalf("batch UIDs = %v, want consecutive from %d", uids(b), first)
					}
				}
				first += uint32(len(b.Messages))
				cur = b.Next
			}
			if !slices.Equal(sizes, tc.sizes) || !slices.Equal(more, tc.more) {
				t.Errorf("batch sizes = %v, more = %v; want %v, %v", sizes, more, tc.sizes, tc.more)
			}
		})
	}
}

// TestScanTooLarge 覆盖大小上限：声明超过上限的邮件不请求正文；服务器少报大小时字面量流式读取至多 N+1 字节，结果仍为 TooLarge。
func TestScanTooLarge(t *testing.T) {
	setVar(t, &maxMessageSize, 1024)
	for _, tc := range []struct {
		name      string
		opts      proxyOptions
		wantFetch bool
	}{
		{"declared size", proxyOptions{}, false},
		{"under-reported size", proxyOptions{sizeOverride: 100}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeServer(t, tc.opts)
			small := testMessage(1, 500)
			fs.appendMessage(FolderInbox, small)
			fs.appendMessage(FolderInbox, testMessage(2, 4096))
			exact := testMessage(3, 1024) // 恰为上限：照常取回
			fs.appendMessage(FolderInbox, exact)
			s := dial(t, fs, testTimeouts())
			b := scan(t, s, FolderInbox, Cursor{})
			if len(b.Messages) != 3 || b.More || b.Next.LastUID != 3 {
				t.Fatalf("batch = %+v", b)
			}
			if m := b.Messages[0]; m.TooLarge || !bytes.Equal(m.Raw, small) {
				t.Errorf("small message = %+v", m)
			}
			if m := b.Messages[2]; len(exact) != 1024 || m.TooLarge || !bytes.Equal(m.Raw, exact) {
				t.Errorf("message of exactly maxMessageSize: too large %v, %d raw bytes", m.TooLarge, len(m.Raw))
			}
			if m := b.Messages[1]; !m.TooLarge || m.Raw != nil {
				t.Errorf("large message: too large %v, %d raw bytes; want TooLarge and nil Raw", m.TooLarge, len(m.Raw))
			}
			fetched := false
			for _, cmd := range fs.commands() {
				if cmd.Name == "UID FETCH" && strings.HasPrefix(cmd.Args, "2 ") && slices.Contains(cmd.Items, "BODY.PEEK[]<0.1025>") {
					fetched = true
				}
			}
			if fetched != tc.wantFetch {
				t.Errorf("body of UID 2 requested = %v, want %v", fetched, tc.wantFetch)
			}
		})
	}
}

// TestScanIgnoredPartialBoundsMemory 覆盖服务器不遵守部分取回：正文 FETCH 回来 32 MiB 的字面量时结果为 TooLarge、Raw 为 nil，
// 且 Scan 期间的内存分配远小于字面量，证明正文至多读取 maxMessageSize+1 字节，其余部分读出丢弃。
func TestScanIgnoredPartialBoundsMemory(t *testing.T) {
	setVar(t, &maxMessageSize, 1024)
	const literal = 32 << 20
	fs := newFakeServer(t, proxyOptions{})
	fs.appendMessage(FolderInbox, testMessage(1, 100))
	tm := testTimeouts()
	tm.Fetch = 5 * time.Second // 32 MiB 要经 TLS 读出丢弃，-race 下也留足时间
	s := dial(t, fs, tm)
	fs.addRule(&rule{command: "UID FETCH", contains: "BODY.PEEK", kind: faultWholeBody, bytes: literal})
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	b := scan(t, s, FolderInbox, Cursor{})
	runtime.ReadMemStats(&after)
	if len(b.Messages) != 1 || !b.Messages[0].TooLarge || b.Messages[0].Raw != nil || b.Next.LastUID != 1 {
		t.Fatalf("batch = %+v, want one TooLarge message without Raw", b)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc >= literal/2 {
		t.Errorf("Scan allocated %d bytes for a %d-byte literal; the body read is not bounded", alloc, literal)
	}
}

// TestScanBodyLiteralCutShort 模拟服务器在正文字面量中途以 close_notify 正常关闭连接。go-imap 的字面量读取器把这时的
// io.EOF 当作字面量正常结束：ReadAll 带着不完整的正文「成功」返回，解码协程也被放行、继续读同一个读缓冲。
// 此时若再调用 Next，它会丢弃剩余字面量而再次读取读缓冲，与解码协程争用（make test 的 -race 会报告数据竞争）。
// Scan 必须返回 ErrClosed，不交付不完整的正文。
func TestScanBodyLiteralCutShort(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	fs.appendMessage(FolderInbox, testMessage(1, 4096))
	s := dial(t, fs, testTimeouts())
	fs.addRule(&rule{command: "UID FETCH", contains: "BODY.PEEK", kind: faultCloseAfter, bytes: 512})
	b, err := s.Scan(context.Background(), FolderInbox, Cursor{})
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
	if len(b.Messages) != 0 {
		t.Fatalf("Scan returned %d messages after the literal was cut short", len(b.Messages))
	}
}

// TestScanBatchBytes 覆盖正文合计上限：已取回正文加下一封的大小超过上限时本批提前结束并置 More，下一批从未交付的第一封开始；
// 恰好等于上限时仍在本批内。
func TestScanBatchBytes(t *testing.T) {
	setVar(t, &maxBatchBytes, 2000)
	fs := newFakeServer(t, proxyOptions{})
	for i := 1; i <= 5; i++ {
		fs.appendMessage(FolderInbox, testMessage(i, 1000))
	}
	s := dial(t, fs, testTimeouts())
	cur := Cursor{}
	var got [][]uint32
	var more []bool
	for range 3 {
		b := scan(t, s, FolderInbox, cur)
		got = append(got, uids(b))
		more = append(more, b.More)
		cur = b.Next
	}
	want := [][]uint32{{1, 2}, {3, 4}, {5}}
	if !slices.EqualFunc(got, want, slices.Equal) || !slices.Equal(more, []bool{true, true, false}) {
		t.Errorf("batches = %v, more = %v; want %v, [true true false]", got, more, want)
	}

	// 单封就超过上限时每批仍交付一封，保证前进。
	maxBatchBytes = 10
	if b := scan(t, s, FolderInbox, Cursor{}); !slices.Equal(uids(b), []uint32{1}) || !b.More || b.Next.LastUID != 1 {
		t.Errorf("batch with a tiny byte limit = %+v", b)
	}
}

// TestScanSkipsVanishedMessages 覆盖补扫期间被删除的邮件：UID FETCH 没有返回数据时跳过该邮件，游标越过它。
func TestScanSkipsVanishedMessages(t *testing.T) {
	for _, tc := range []struct{ name, contains string }{
		{"size fetch", "RFC822.SIZE"},
		{"body fetch", "BODY.PEEK"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeServer(t, proxyOptions{})
			fs.appendMessage(FolderInbox, testMessage(1, 100))
			s := dial(t, fs, testTimeouts())
			fs.addRule(&rule{command: "UID FETCH", contains: tc.contains, kind: faultEmpty, limit: 1})
			b := scan(t, s, FolderInbox, Cursor{})
			if len(b.Messages) != 0 || b.More || b.Next.LastUID != 1 {
				t.Errorf("batch = %+v, want no messages and LastUID 1", b)
			}
		})
	}
}

// TestDialAuthFailed 覆盖认证失败：返回 ErrAuthFailed，错误文本不含密码。服务器回 NO 后立即断开时同样返回 ErrAuthFailed：
// 命令结束与连接关闭几乎同时发生，不能按先后误判为普通断开，所以重复多次以覆盖两种先后。
func TestDialAuthFailed(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	wrong := strings.Repeat("qx", 8)
	_, err := Dial(context.Background(), fs.config(testTimeouts()), wrong)
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("Dial = %v, want ErrAuthFailed", err)
	}
	if strings.Contains(err.Error(), wrong) || strings.Contains(err.Error(), testPassword) {
		t.Errorf("error text %q contains a password", err)
	}
	waitFor(t, "the client to close the connection", func() bool { return fs.openConns() == 0 })

	fs.addRule(&rule{command: "LOGIN", kind: faultRejectClose})
	for i := range 100 {
		if _, err := Dial(context.Background(), fs.config(testTimeouts()), wrong); !errors.Is(err, ErrAuthFailed) {
			t.Fatalf("Dial %d against a server that closes after NO = %v, want ErrAuthFailed", i, err)
		}
	}
}

// TestDialTransportSecurity 覆盖传输安全：明文入口、证书不受信任、主机名不在证书中与只支持旧版 TLS 的服务器都在发送命令前失败。
func TestDialTransportSecurity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opts   proxyOptions
		adjust func(*Config)
	}{
		{"plaintext", proxyOptions{plaintext: true}, nil},
		{"untrusted certificate", proxyOptions{}, func(c *Config) { c.RootCAs = x509.NewCertPool() }},
		{"hostname not in certificate", proxyOptions{}, func(c *Config) { c.Host = "localhost" }},
		{"TLS 1.1 only", proxyOptions{maxTLSVersion: tls.VersionTLS11}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeServer(t, tc.opts)
			if tc.opts.maxTLSVersion != 0 {
				assertServerSpeaksOldTLS(t, fs)
			}
			cfg := fs.config(testTimeouts())
			if tc.adjust != nil {
				tc.adjust(&cfg)
			}
			_, err := Dial(context.Background(), cfg, testPassword)
			if err == nil {
				t.Fatal("Dial succeeded")
			}
			if strings.Contains(err.Error(), testPassword) {
				t.Errorf("error text %q contains the password", err)
			}
			if fs.acceptedConns() == 0 {
				t.Error("the client never connected; the case did not exercise the transport check")
			}
			if cmds := fs.commands(); len(cmds) != 0 {
				t.Errorf("proxy received commands %+v", cmds)
			}
		})
	}
}

// assertServerSpeaksOldTLS 用一条允许 TLS 1.0 的对照连接确认代理确实能协商出 TLS 1.1，避免 Go 移除旧版本支持后用例空转通过。
func assertServerSpeaksOldTLS(t *testing.T, fs *fakeServer) {
	t.Helper()
	conn, err := tls.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(fs.port)), &tls.Config{RootCAs: fs.pool, MinVersion: tls.VersionTLS10, NextProtos: []string{"imap"}})
	if err != nil {
		t.Fatalf("control connection with TLS 1.0 allowed: %v", err)
	}
	defer conn.Close()
	if v := conn.ConnectionState().Version; v != tls.VersionTLS11 {
		t.Fatalf("control connection negotiated %#x, want TLS 1.1", v)
	}
}

// TestFolders 覆盖文件夹：不存在的文件夹在 Examine 与 Scan 中都返回 ErrNoFolder 且连接仍可用；
// EXAMINE Junk 被回 NO [UNAVAILABLE] 时同样如此；Examine(INBOX) 的状态与 imapmemserver 一致；ListFolders 返回全部文件夹。
func TestFolders(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	fs.appendMessage(FolderInbox, testMessage(1, 100))
	fs.appendMessage(FolderInbox, testMessage(2, 100))
	s := dial(t, fs, testTimeouts())
	ctx := context.Background()

	folders, err := s.ListFolders(ctx)
	if err != nil || !slices.Contains(folders, FolderInbox) || !slices.Contains(folders, FolderJunk) {
		t.Fatalf("ListFolders = %q, %v", folders, err)
	}
	if _, err := s.Examine(ctx, "Missing"); !errors.Is(err, ErrNoFolder) {
		t.Errorf("Examine(Missing) = %v, want ErrNoFolder", err)
	}
	if _, err := s.Scan(ctx, "Missing", Cursor{}); !errors.Is(err, ErrNoFolder) {
		t.Errorf("Scan(Missing) = %v, want ErrNoFolder", err)
	}
	fs.addRule(&rule{command: "EXAMINE", contains: FolderJunk, kind: faultReject, limit: 1})
	if _, err := s.Examine(ctx, FolderJunk); !errors.Is(err, ErrNoFolder) {
		t.Errorf("Examine(Junk) rejected with UNAVAILABLE = %v, want ErrNoFolder", err)
	}
	if _, err := s.Examine(ctx, FolderJunk); err != nil {
		t.Errorf("Examine(Junk) after the rejection = %v", err)
	}
	box, err := s.Examine(ctx, FolderInbox)
	if err != nil {
		t.Fatal(err)
	}
	st := fs.status(FolderInbox)
	if box != (Mailbox{UIDValidity: st.UIDValidity, UIDNext: uint32(st.UIDNext), Messages: *st.NumMessages}) || box.Messages != 2 {
		t.Errorf("Examine(INBOX) = %+v, status %+v", box, st)
	}
	if b := scan(t, s, FolderInbox, Cursor{}); len(b.Messages) != 2 {
		t.Errorf("batch after folder errors = %+v", b)
	}
}

// TestCloseLogsOut 覆盖 Close：发送 LOGOUT 并关闭连接，重复调用无害，此后调用返回 ErrClosed。
func TestCloseLogsOut(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	s := dial(t, fs, testTimeouts())
	if err := s.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if n := fs.count("LOGOUT"); n != 1 {
		t.Errorf("client sent %d LOGOUT, want 1", n)
	}
	assertClosed(t, fs, s)
	if n := fs.count("LOGOUT"); n != 1 {
		t.Errorf("client sent %d LOGOUT after closing twice, want 1", n)
	}
	if err := s.sleep(context.Background(), time.Millisecond); !errors.Is(err, ErrClosed) {
		t.Errorf("sleep after Close = %v, want ErrClosed", err)
	}
}

// TestScanCursorAtMaxUID 覆盖 UID 已到上限的游标：不发送 UID SEARCH（last+1 会回绕成非法的 0），返回空批。
func TestScanCursorAtMaxUID(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	s := dial(t, fs, testTimeouts())
	cur := Cursor{UIDValidity: fs.status(FolderInbox).UIDValidity, LastUID: math.MaxUint32}
	if b := scan(t, s, FolderInbox, cur); b.Reset || len(b.Messages) != 0 || b.More || b.Next != cur {
		t.Errorf("batch = %+v", b)
	}
	if n := fs.count("UID SEARCH"); n != 0 {
		t.Errorf("client sent %d UID SEARCH commands", n)
	}
}

// TestShutdownBoundedWait 覆盖关闭本身的期限：解码协程停在处理函数里时 client.Close 不会返回，shutdown 至多等待 IdleStop。
// 本包的处理函数从不阻塞，这里用测试自建的阻塞处理函数构造这种情形。
func TestShutdownBoundedWait(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	client := imapclient.New(clientConn, &imapclient.Options{UnilateralDataHandler: &imapclient.UnilateralDataHandler{
		Mailbox: func(*imapclient.UnilateralDataMailbox) {
			entered <- struct{}{}
			<-release
		},
	}})
	t.Cleanup(func() {
		close(release)
		serverConn.Close()
	})
	go func() { _, _ = io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n* 1 EXISTS\r\n") }()
	select {
	case <-entered:
	case <-time.After(waitLimit):
		t.Fatal("handler was not called")
	}
	s := &Session{client: client, t: Timeouts{IdleStop: 100 * time.Millisecond}.withDefaults(), exists: make(chan struct{}, 1)}
	start := time.Now()
	s.shutdown()
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond || elapsed > time.Second {
		t.Errorf("shutdown took %v, want about IdleStop", elapsed)
	}
	if _, err := s.Idle(context.Background()); !errors.Is(err, ErrClosed) {
		t.Errorf("Idle after shutdown = %v, want ErrClosed", err)
	}
}

// TestTimeoutsDefaults 覆盖默认期限与零值、负值字段的补齐。
func TestTimeoutsDefaults(t *testing.T) {
	want := Timeouts{
		Dial: 15 * time.Second, Greeting: 15 * time.Second, Command: 30 * time.Second, Fetch: 60 * time.Second,
		IdleAck: 30 * time.Second, IdleMax: 5 * time.Minute, IdleStop: 10 * time.Second, Poll: 2 * time.Minute,
	}
	if got := DefaultTimeouts(); got != want {
		t.Errorf("DefaultTimeouts() = %+v, want %+v", got, want)
	}
	if got := (Timeouts{}).withDefaults(); got != want {
		t.Errorf("zero Timeouts with defaults = %+v", got)
	}
	custom := Timeouts{Dial: 1, Greeting: 2, Command: 3, Fetch: 4, IdleAck: 5, IdleMax: 6, IdleStop: 7, Poll: 8}
	if got := custom.withDefaults(); got != custom {
		t.Errorf("custom Timeouts with defaults = %+v", got)
	}
	negative := Timeouts{Dial: -1, Greeting: -1, Command: -1, Fetch: -1, IdleAck: -1, IdleMax: -1, IdleStop: -1, Poll: -1}
	if got := negative.withDefaults(); got != want {
		t.Errorf("negative Timeouts with defaults = %+v, want %+v", got, want)
	}
}

// TestErrorTextHasNoServerText 覆盖错误文本：服务器拒绝时只报告步骤与响应类型，不带服务器文本。
func TestErrorTextHasNoServerText(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	s := dial(t, fs, testTimeouts())
	fs.addRule(&rule{command: "EXAMINE", kind: faultReject})
	_, err := s.Examine(context.Background(), FolderInbox)
	if !errors.Is(err, ErrNoFolder) || strings.Contains(err.Error(), "temporarily") {
		t.Errorf("Examine = %v", err)
	}
	fs.clearRules()
	fs.addRule(&rule{command: "UID SEARCH", kind: faultReject})
	_, err = s.Scan(context.Background(), FolderInbox, Cursor{})
	if err == nil || errors.Is(err, ErrNoFolder) || strings.Contains(err.Error(), "temporarily") || !strings.Contains(err.Error(), "uid search") {
		t.Errorf("Scan with a rejected UID SEARCH = %v", err)
	}
	if got := fmt.Sprint(err); strings.Contains(got, testPassword) {
		t.Errorf("error text %q contains the password", got)
	}
}
