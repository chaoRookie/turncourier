// Package imap 用离线假服务器验证长连接收取循环：交付与游标、处理失败时同一连接内重试、断开与半开后的退避重连与复位、
// 只在 IDLE 阶段出现的故障下登录间隔增长与登录频率上限、IDLE 降级为轮询、认证失败暂停、凭据不可用、
// Junk 缺失与暂时不可用、Junk 连续失败后降级而 INBOX 照常收信、取消。
package imap

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// testBackoff 返回测试用的短退避；登录窗口为秒级。
func testBackoff() Backoff {
	return Backoff{Initial: 100 * time.Millisecond, Max: 800 * time.Millisecond, AuthPause: 400 * time.Millisecond, MaxLogins: 12, LoginWindow: 5 * time.Second}
}

// harness 运行一个 Watcher：Handle 把批次写入切片并更新内存游标，Status 写入切片。
type harness struct {
	t  *testing.T
	fs *fakeServer
	w  *Watcher

	mu         sync.Mutex
	cursors    map[string]Cursor
	batches    []Batch
	statuses   []Status
	failFolder string // 该文件夹接下来的 failNext 次 Handle 返回错误
	failNext   int

	cancel context.CancelFunc
	done   chan error
}

// newHarness 以测试密码、短期限与给定退避构造 Watcher，尚未启动；用例结束时停止它。
func newHarness(t *testing.T, fs *fakeServer, timeouts Timeouts, backoff Backoff) *harness {
	t.Helper()
	setVar(t, &minAuthPause, 100*time.Millisecond)
	h := &harness{t: t, fs: fs, cursors: map[string]Cursor{}}
	h.w = &Watcher{
		Config:   fs.config(timeouts),
		Password: func(context.Context) (string, error) { return testPassword, nil },
		Cursor: func(_ context.Context, folder string) (Cursor, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.cursors[folder], nil
		},
		Handle: func(_ context.Context, b Batch) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if b.Folder == h.failFolder && h.failNext > 0 {
				h.failNext--
				h.batches = append(h.batches, b)
				return errors.New("database is busy")
			}
			h.batches = append(h.batches, b)
			h.cursors[b.Folder] = b.Next
			return nil
		},
		Status: func(s Status) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.statuses = append(h.statuses, s)
		},
		Backoff: backoff,
	}
	t.Cleanup(func() {
		if h.cancel != nil {
			h.cancel()
			<-h.done
		}
	})
	return h
}

// start 在后台运行 Watcher。
func (h *harness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan error, 1)
	go func() { h.done <- h.w.Run(ctx) }()
}

// stop 取消 ctx，断言 Run 在 1 秒内返回 context.Canceled。
func (h *harness) stop() {
	h.t.Helper()
	h.cancel()
	select {
	case err := <-h.done:
		if !errors.Is(err, context.Canceled) {
			h.t.Errorf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		h.t.Fatal("Run did not return within 1s after cancel")
	}
	h.done <- context.Canceled // 让 Cleanup 中的等待立即返回
}

// delivered 返回交给 Handle 的某文件夹邮件 UID（含处理失败后重新交付的），按交付顺序。
func (h *harness) delivered(folder string) []uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []uint32
	for _, b := range h.batches {
		if b.Folder == folder {
			out = append(out, uids(b)...)
		}
	}
	return out
}

// kinds 返回迄今收到的状态种类。
func (h *harness) kinds() []StatusKind {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []StatusKind
	for _, s := range h.statuses {
		out = append(out, s.Kind)
	}
	return out
}

// statusesOf 返回某种状态的全部通知。
func (h *harness) statusesOf(kind StatusKind) []Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Status
	for _, s := range h.statuses {
		if s.Kind == kind {
			out = append(out, s)
		}
	}
	return out
}

// waitStatus 等到某种状态至少出现 n 次。
func (h *harness) waitStatus(kind StatusKind, n int) {
	h.t.Helper()
	waitFor(h.t, string(kind)+" status", func() bool { return len(h.statusesOf(kind)) >= n })
}

// waitDelivered 等到某文件夹交付了 uid。
func (h *harness) waitDelivered(folder string, uid uint32) {
	h.t.Helper()
	waitFor(h.t, "delivery of "+folder+" message", func() bool { return slices.Contains(h.delivered(folder), uid) })
}

// loginTimes 返回代理记录的每次 LOGIN 的时刻。
func loginTimes(fs *fakeServer) []time.Time {
	var out []time.Time
	for _, cmd := range fs.commands() {
		if cmd.Name == "LOGIN" {
			out = append(out, cmd.At)
		}
	}
	return out
}

// assertNear 断言 d 在 want 的 ±20% 内。
func assertNear(t *testing.T, d, want time.Duration, what string) {
	t.Helper()
	if d < want*8/10 || d > want*12/10 {
		t.Errorf("%s = %v, want %v ±20%%", what, d, want)
	}
}

// TestWatcherDelivers 覆盖正常收取：依次交付 INBOX 与 Junk 的批次，IDLE 期间放入的邮件在 1 秒内交付，每封只交付一次。
func TestWatcherDelivers(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	fs.appendMessage(FolderJunk, testMessage(1, 100))
	for i := 1; i <= 3; i++ {
		fs.appendMessage(FolderInbox, testMessage(i, 100))
	}
	tm := testTimeouts()
	tm.IdleMax = 5 * time.Second
	h := newHarness(t, fs, tm, testBackoff())
	h.start()
	h.waitDelivered(FolderInbox, 3)
	// Junk 排在 INBOX 之后，等到 INBOX 的最后一封并不意味着 Junk 的批次已经入列，必须单独等它。
	h.waitDelivered(FolderJunk, 1)
	h.mu.Lock()
	first := slices.Clone(h.batches)
	h.mu.Unlock()
	if len(first) != 2 || first[0].Folder != FolderInbox || first[1].Folder != FolderJunk || !slices.Equal(uids(first[0]), []uint32{1, 2, 3}) {
		t.Fatalf("first batches = %+v", first)
	}
	waitFor(t, "IDLE", func() bool { return fs.count("IDLE") == 1 })
	time.Sleep(50 * time.Millisecond)
	pushed := time.Now()
	fs.appendMessage(FolderInbox, testMessage(4, 100))
	h.waitDelivered(FolderInbox, 4)
	if d := time.Since(pushed); d > time.Second {
		t.Errorf("pushed message delivered after %v", d)
	}
	h.stop()
	if got := h.delivered(FolderInbox); !slices.Equal(got, []uint32{1, 2, 3, 4}) {
		t.Errorf("INBOX deliveries = %v, want each message once", got)
	}
	if got := h.delivered(FolderJunk); !slices.Equal(got, []uint32{1}) {
		t.Errorf("Junk deliveries = %v", got)
	}
	if n := fs.count("LOGIN"); n != 1 {
		t.Errorf("LOGIN count = %d, want 1", n)
	}
	if k := h.kinds(); len(k) == 0 || k[0] != StatusConnected {
		t.Errorf("statuses = %v, want connected first", k)
	}
}

// TestWatcherRetriesHandleOnSameConnection 覆盖处理失败：发出 handle_failed，按退避等待后在同一连接上再次交付同一批，不重新登录。
func TestWatcherRetriesHandleOnSameConnection(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	fs.appendMessage(FolderInbox, testMessage(1, 100))
	fs.appendMessage(FolderInbox, testMessage(2, 100))
	h := newHarness(t, fs, testTimeouts(), testBackoff())
	h.failFolder, h.failNext = FolderInbox, 2
	h.start()
	h.waitDelivered(FolderInbox, 2)
	waitFor(t, "a successful INBOX batch", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.cursors[FolderInbox].LastUID == 2
	})
	// 成功处理后退避复位：下一次失败重新从 Initial 开始。
	h.mu.Lock()
	h.failNext = 1
	h.mu.Unlock()
	fs.appendMessage(FolderInbox, testMessage(3, 100))
	waitFor(t, "the third message to be handled", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.cursors[FolderInbox].LastUID == 3
	})
	h.stop()
	failed := h.statusesOf(StatusHandleFailed)
	if len(failed) != 3 {
		t.Fatalf("handle_failed statuses = %+v, want 3", failed)
	}
	assertNear(t, failed[0].Delay, 100*time.Millisecond, "first handle_failed delay")
	assertNear(t, failed[1].Delay, 200*time.Millisecond, "second handle_failed delay")
	assertNear(t, failed[2].Delay, 100*time.Millisecond, "handle_failed delay after a success")
	h.mu.Lock()
	var inbox [][]uint32
	for _, b := range h.batches {
		if b.Folder == FolderInbox {
			inbox = append(inbox, uids(b))
		}
	}
	h.mu.Unlock()
	if len(inbox) < 3 || !slices.Equal(inbox[0], []uint32{1, 2}) || !slices.Equal(inbox[1], []uint32{1, 2}) || !slices.Equal(inbox[2], []uint32{1, 2}) {
		t.Errorf("INBOX batches = %v, want the same batch delivered three times", inbox)
	}
	if n := fs.count("LOGIN"); n != 1 {
		t.Errorf("LOGIN count = %d, want 1", n)
	}
	if n := len(h.statusesOf(StatusDisconnected)); n != 0 {
		t.Errorf("disconnected %d times", n)
	}
}

// TestWatcherReconnectsWithBackoff 覆盖断开与半开：退避间隔依次约为 Initial、2×Initial；一次 IDLE 正常结束（收到 EXISTS）后
// 复位，此时连接的存活时间远小于 IdleMax，所以复位只能来自 IDLE 正常结束；重连后继续交付新邮件。
func TestWatcherReconnectsWithBackoff(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	tm := testTimeouts()
	tm.IdleMax = 1500 * time.Millisecond
	b := testBackoff()
	h := newHarness(t, fs, tm, b)
	h.start()
	waitFor(t, "first IDLE", func() bool { return fs.count("IDLE") == 1 })

	fs.disconnectAll()
	h.waitStatus(StatusDisconnected, 1)
	waitFor(t, "second IDLE", func() bool { return fs.count("IDLE") == 2 })

	fs.freezeAll() // IDLE 进行中半开：Idle 超时后重连
	h.waitStatus(StatusDisconnected, 2)
	waitFor(t, "third IDLE", func() bool { return fs.count("IDLE") == 3 })

	time.Sleep(50 * time.Millisecond)
	fs.appendMessage(FolderInbox, testMessage(1, 100)) // IDLE 收到 EXISTS 正常结束，退避复位
	h.waitDelivered(FolderInbox, 1)
	waitFor(t, "fourth IDLE", func() bool { return fs.count("IDLE") == 4 })
	fs.disconnectAll()
	h.waitStatus(StatusDisconnected, 3)
	fs.appendMessage(FolderInbox, testMessage(2, 100))
	h.waitDelivered(FolderInbox, 2)
	h.stop()

	backoffs := h.statusesOf(StatusBackoff)
	if len(backoffs) != 3 {
		t.Fatalf("backoff statuses = %+v, want 3", backoffs)
	}
	assertNear(t, backoffs[0].Delay, b.Initial, "first backoff")
	assertNear(t, backoffs[1].Delay, 2*b.Initial, "second backoff")
	assertNear(t, backoffs[2].Delay, b.Initial, "backoff after a normal IDLE")
	if n := fs.count("LOGIN"); n != 4 {
		t.Errorf("LOGIN count = %d, want 4", n)
	}
}

// TestWatcherResetsBackoffAfterHealthyPeriod 覆盖另一条复位规则：服务器不支持 IDLE 而按 Poll 补扫时，
// 连接自登录起保持健康达到 IdleMax 后断开，退避回到 Initial；存活时间不足 IdleMax 的连接断开则继续加倍。
func TestWatcherResetsBackoffAfterHealthyPeriod(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{dropCaps: []string{"IDLE"}})
	tm := testTimeouts()
	tm.IdleMax, tm.Poll = 400*time.Millisecond, 50*time.Millisecond
	b := testBackoff()
	h := newHarness(t, fs, tm, b)
	h.start()
	for i := 1; i <= 2; i++ {
		// 只在登录完成、新连接上已有补扫之后断开；登录过程中断开属于拨号失败，不发出 disconnected。
		searches := fs.count("UID SEARCH")
		h.waitStatus(StatusConnected, i)
		waitFor(t, "a scan on the new connection", func() bool { return fs.count("UID SEARCH") > searches })
		fs.disconnectAll()
		h.waitStatus(StatusDisconnected, i)
	}
	h.waitStatus(StatusConnected, 3)
	time.Sleep(tm.IdleMax + 200*time.Millisecond)
	fs.disconnectAll()
	h.waitStatus(StatusDisconnected, 3)
	waitFor(t, "fourth login", func() bool { return fs.count("LOGIN") == 4 })
	h.stop()
	backoffs := h.statusesOf(StatusBackoff)
	if len(backoffs) != 3 {
		t.Fatalf("backoff statuses = %+v, want 3", backoffs)
	}
	assertNear(t, backoffs[0].Delay, b.Initial, "first backoff")
	assertNear(t, backoffs[1].Delay, 2*b.Initial, "second backoff")
	assertNear(t, backoffs[2].Delay, b.Initial, "backoff after a healthy period")
}

// TestWatcherIdlePhaseFaults 覆盖只在 IDLE 阶段出现的故障：每次进入 IDLE 后代理即断开而补扫每次都成功。
// 补扫成功不复位退避，登录间隔按倍数增长直到 Max，此后不回落；任一 LoginWindow 内的登录不超过 MaxLogins，两次登录至少间隔 Initial。
func TestWatcherIdlePhaseFaults(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	fs.addRule(&rule{command: "IDLE", kind: faultDisconnect})
	b := Backoff{Initial: 100 * time.Millisecond, Max: 400 * time.Millisecond, AuthPause: time.Second, Jitter: 0.01, MaxLogins: 4, LoginWindow: 1500 * time.Millisecond}
	h := newHarness(t, fs, testTimeouts(), b)
	h.start()
	// 7 次登录（6 个间隔，约 100、200、400、800、400、400ms）约需 2.5 秒，接近 waitLimit，这里放宽。
	waitForWithin(t, "7 logins", 2*waitLimit, func() bool { return fs.count("LOGIN") >= 7 })
	h.stop()

	logins := loginTimes(fs)
	var gaps []time.Duration
	for i := 1; i < len(logins); i++ {
		gaps = append(gaps, logins[i].Sub(logins[i-1]))
	}
	// Watcher 在拨号返回后登记登录时刻，它不早于代理收到 LOGIN 的时刻，所以按代理记录比较间隔与窗口不需要余量。
	for i, gap := range gaps {
		if gap < b.Initial {
			t.Errorf("gap %d = %v, below Initial", i, gap)
		}
	}
	// 前三个间隔约为 100、200、400ms：每次至少增长 1.5 倍，直到 Max。
	for i := 1; i < 3; i++ {
		if gaps[i] < gaps[i-1]*3/2 {
			t.Errorf("login gaps %v do not grow until Max", gaps)
			break
		}
	}
	// 每个间隔都不低于前一个间隔与 Max 中较小者（扣除抖动与 50ms 调度余量）：到达 Max 之后退避不回落到 Initial。
	for i := 1; i < len(gaps); i++ {
		if floor := time.Duration(float64(min(gaps[i-1], b.Max))*(1-b.Jitter)) - 50*time.Millisecond; gaps[i] < floor {
			t.Errorf("login gap %d = %v, below %v after reaching it; gaps %v", i, gaps[i], floor, gaps)
		}
	}
	for i := range logins {
		n := 0
		for _, at := range logins[i:] {
			if at.Sub(logins[i]) < b.LoginWindow {
				n++
			}
		}
		if n > b.MaxLogins {
			t.Errorf("%d logins within %v starting at login %d, gaps %v", n, b.LoginWindow, i, gaps)
		}
	}
	if n := fs.count("UID SEARCH"); n < 7 {
		t.Errorf("UID SEARCH count = %d; every round should have scanned", n)
	}
}

// TestWatcherLoginWindowUsesLoginTime 覆盖登录频率上限按 LOGIN 发出的时刻计算：第一个连接的握手很慢，LOGIN 远晚于拨号开始，
// 任一 LoginWindow 内代理收到的 LOGIN 仍不超过 MaxLogins。若按拨号开始的时刻计算，窗口会提前结束，第三次 LOGIN 落在窗口内。
func TestWatcherLoginWindowUsesLoginTime(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{slowHandshake: 400 * time.Millisecond})
	fs.addRule(&rule{command: "LOGIN", kind: faultDisconnect})
	b := Backoff{Initial: 50 * time.Millisecond, Max: 50 * time.Millisecond, AuthPause: time.Second, Jitter: 0.01, MaxLogins: 2, LoginWindow: 600 * time.Millisecond}
	h := newHarness(t, fs, testTimeouts(), b)
	h.start()
	waitFor(t, "3 logins", func() bool { return fs.count("LOGIN") >= 3 })
	h.stop()
	logins := loginTimes(fs)
	if d := logins[2].Sub(logins[0]); d < b.LoginWindow {
		t.Errorf("3 logins within %v, want at most %d in any %v", d, b.MaxLogins, b.LoginWindow)
	}
}

// TestWatcherPendingExistsIsNotIdle 覆盖补扫期间收到的 EXISTS：Watcher 不发送 IDLE 就回到补扫，这不是一次 IDLE 正常结束，
// 既不复位重连退避，也不打断 IDLE 连续超时的计数。每个连接的 UID SEARCH 响应前都注入一条 EXISTS，IDLE 只回无标签 BAD：
// 退避依次约为 Initial、2×Initial，两次 IDLE 超时后发出 idle_disabled。
func TestWatcherPendingExistsIsNotIdle(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{noJunk: true})
	inject := &rule{command: "UID SEARCH", kind: faultInject, exists: []uint32{7}, limit: 1}
	fs.addRule(inject)
	fs.addRule(&rule{command: "IDLE", kind: faultBAD, hook: func() {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		inject.used = 0 // 下一个连接的第一次 UID SEARCH 再注入一次
	}})
	b := testBackoff()
	h := newHarness(t, fs, testTimeouts(), b)
	h.start()
	h.waitStatus(StatusIdleDisabled, 1)
	h.waitStatus(StatusBackoff, 2)
	h.stop()
	backoffs := h.statusesOf(StatusBackoff)
	assertNear(t, backoffs[0].Delay, b.Initial, "first backoff")
	assertNear(t, backoffs[1].Delay, 2*b.Initial, "second backoff")
	if n := fs.count("IDLE"); n != 2 {
		t.Errorf("IDLE count = %d, want 2", n)
	}
}

// TestWatcherDisablesIdleAfterTimeouts 覆盖 IDLE 降级：IDLE 两次只回无标签 BAD 而超时后发出一次 idle_disabled，
// 此后不再发送 IDLE，新邮件在 Poll 间隔内交付。
func TestWatcherDisablesIdleAfterTimeouts(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	fs.addRule(&rule{command: "IDLE", kind: faultBAD})
	tm := testTimeouts()
	h := newHarness(t, fs, tm, testBackoff())
	h.start()
	h.waitStatus(StatusIdleDisabled, 1)
	waitFor(t, "reconnect after idle is disabled", func() bool { return fs.count("LOGIN") == 3 })
	fs.appendMessage(FolderInbox, testMessage(1, 100))
	arrived := time.Now()
	h.waitDelivered(FolderInbox, 1)
	if d := time.Since(arrived); d > tm.Poll+500*time.Millisecond {
		t.Errorf("message delivered after %v in poll mode", d)
	}
	time.Sleep(3 * tm.Poll)
	h.stop()
	if n := fs.count("IDLE"); n != 2 {
		t.Errorf("IDLE count = %d, want 2", n)
	}
	if n := len(h.statusesOf(StatusIdleDisabled)); n != 1 {
		t.Errorf("idle_disabled count = %d, want 1", n)
	}
}

// TestWatcherIdleTimeoutsMustBeConsecutive 覆盖「连续 2 次」：两次 IDLE 超时之间有一次 IDLE 正常结束时不降级。
func TestWatcherIdleTimeoutsMustBeConsecutive(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	fs.addRule(&rule{command: "IDLE", kind: faultBAD, limit: 1})
	h := newHarness(t, fs, testTimeouts(), testBackoff())
	h.start()
	h.waitStatus(StatusDisconnected, 1)
	waitFor(t, "an IDLE to end normally", func() bool { return fs.count("DONE") >= 1 })
	fs.addRule(&rule{command: "IDLE", kind: faultBAD, limit: 1})
	h.waitStatus(StatusDisconnected, 2)
	waitFor(t, "IDLE after the second timeout", func() bool { return fs.count("DONE") >= 2 })
	h.stop()
	if n := len(h.statusesOf(StatusIdleDisabled)); n != 0 {
		t.Errorf("idle_disabled count = %d, want 0", n)
	}
}

// TestWatcherPollsWithoutIdleCapability 覆盖服务器不公告 IDLE：直接按 Poll 间隔补扫，不发出 idle_disabled。
func TestWatcherPollsWithoutIdleCapability(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{dropCaps: []string{"IDLE"}})
	h := newHarness(t, fs, testTimeouts(), testBackoff())
	h.start()
	waitFor(t, "a few polls", func() bool { return fs.count("UID SEARCH") >= 6 })
	fs.appendMessage(FolderInbox, testMessage(1, 100))
	h.waitDelivered(FolderInbox, 1)
	h.stop()
	if n := fs.count("IDLE"); n != 0 {
		t.Errorf("IDLE count = %d, want 0", n)
	}
	if n := len(h.statusesOf(StatusIdleDisabled)); n != 0 {
		t.Errorf("idle_disabled count = %d, want 0", n)
	}
	if n := fs.count("LOGIN"); n != 1 {
		t.Errorf("LOGIN count = %d, want 1", n)
	}
}

// TestWatcherRecoversFromCommandBAD 覆盖 LIST 与 UID SEARCH 的无标签 BAD：超时断开并重连，故障解除后恢复交付。
func TestWatcherRecoversFromCommandBAD(t *testing.T) {
	for _, command := range []string{"LIST", "UID SEARCH"} {
		t.Run(command, func(t *testing.T) {
			fs := newFakeServer(t, proxyOptions{})
			fs.appendMessage(FolderInbox, testMessage(1, 100))
			fs.addRule(&rule{command: command, kind: faultBAD, limit: 1})
			h := newHarness(t, fs, testTimeouts(), testBackoff())
			h.start()
			h.waitDelivered(FolderInbox, 1)
			h.stop()
			if n := len(h.statusesOf(StatusDisconnected)); n != 1 {
				t.Errorf("disconnected count = %d, want 1", n)
			}
			if n := fs.count("LOGIN"); n != 2 {
				t.Errorf("LOGIN count = %d, want 2", n)
			}
		})
	}
}

// TestWatcherBacksOffWhenDialFails 覆盖登录前失败（问候冻结）：不发出 connected，退避间隔依次约为 Initial、2×Initial、4×Initial。
func TestWatcherBacksOffWhenDialFails(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{freezeGreeting: true})
	b := testBackoff()
	h := newHarness(t, fs, testTimeouts(), b)
	h.start()
	h.waitStatus(StatusBackoff, 3)
	h.stop()
	backoffs := h.statusesOf(StatusBackoff)
	jittered := false
	for i, want := range []time.Duration{b.Initial, 2 * b.Initial, 4 * b.Initial} {
		assertNear(t, backoffs[i].Delay, want, fmt.Sprintf("backoff %d", i+1))
		jittered = jittered || backoffs[i].Delay != want
	}
	if !jittered {
		t.Errorf("backoff delays %+v carry no jitter", backoffs)
	}
	if n := len(h.statusesOf(StatusConnected)); n != 0 {
		t.Errorf("connected %d times", n)
	}
}

// TestWatcherHandleWaitSurvivesDisconnect 覆盖处理失败后的等待期间连接断开：按正常重连处理，恢复后交付。
func TestWatcherHandleWaitSurvivesDisconnect(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	fs.appendMessage(FolderInbox, testMessage(1, 100))
	b := testBackoff()
	b.Initial = 300 * time.Millisecond
	h := newHarness(t, fs, testTimeouts(), b)
	h.failFolder, h.failNext = FolderInbox, 1
	h.start()
	h.waitStatus(StatusHandleFailed, 1)
	fs.disconnectAll()
	h.waitStatus(StatusDisconnected, 1)
	waitFor(t, "the batch to be handled", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.cursors[FolderInbox].LastUID == 1
	})
	h.stop()
	if n := fs.count("LOGIN"); n != 2 {
		t.Errorf("LOGIN count = %d, want 2", n)
	}
}

// TestWatcherDrainsMoreBatches 覆盖 More：正文合计上限在测试中降低后，同一轮内依次交付多个批次，全部交付之前不进入 IDLE。
func TestWatcherDrainsMoreBatches(t *testing.T) {
	setVar(t, &maxBatchBytes, 150)
	fs := newFakeServer(t, proxyOptions{})
	for i := 1; i <= 3; i++ {
		fs.appendMessage(FolderInbox, testMessage(i, 100))
	}
	h := newHarness(t, fs, testTimeouts(), testBackoff())
	h.start()
	waitFor(t, "IDLE", func() bool { return fs.count("IDLE") >= 1 })
	h.stop()
	h.mu.Lock()
	defer h.mu.Unlock()
	var got [][]uint32
	var more []bool
	for _, b := range h.batches {
		if b.Folder == FolderInbox && len(b.Messages) > 0 {
			got = append(got, uids(b))
			more = append(more, b.More)
		}
	}
	if !slices.EqualFunc(got, [][]uint32{{1}, {2}, {3}}, slices.Equal) || !slices.Equal(more, []bool{true, true, false}) {
		t.Errorf("INBOX batches = %v, more = %v", got, more)
	}
}

// TestWatcherExistsDuringFetch 覆盖补扫的 UID FETCH 进行中到达的新邮件：下一轮立即交付，不等 IdleMax；随后取消，Run 在 1 秒内返回。
func TestWatcherExistsDuringFetch(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	fs.appendMessage(FolderInbox, testMessage(1, 100))
	tm := testTimeouts()
	tm.IdleMax = 5 * time.Second
	arrived := make(chan time.Time, 1)
	fs.addRule(&rule{command: "UID FETCH", contains: "BODY.PEEK", kind: faultInject, exists: []uint32{2}, limit: 1, hook: func() {
		arrived <- time.Now()
		_, _ = fs.put(FolderInbox, testMessage(2, 100))
	}})
	h := newHarness(t, fs, tm, testBackoff())
	h.start()
	h.waitDelivered(FolderInbox, 2)
	if d := time.Since(<-arrived); d > time.Second {
		t.Errorf("message arriving during the fetch was delivered after %v", d)
	}
	h.stop()
	if got := h.delivered(FolderInbox); !slices.Equal(got, []uint32{1, 2}) {
		t.Errorf("INBOX deliveries = %v", got)
	}
}

// TestWatcherAuthFailure 覆盖认证失败：发出 auth_failed，Delay 等于 AuthPause，暂停期间没有新的 LOGIN，也不按普通断开退避。
// 服务器回 NO 后立即断开时同样如此。
func TestWatcherAuthFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close bool // 服务器回 NO 后立即关闭连接
	}{{"rejected", false}, {"rejected then closed", true}} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeServer(t, proxyOptions{})
			if tc.close {
				fs.addRule(&rule{command: "LOGIN", kind: faultRejectClose})
			}
			b := testBackoff()
			h := newHarness(t, fs, testTimeouts(), b)
			wrong := strings.Repeat("qx", 8)
			h.w.Password = func(context.Context) (string, error) { return wrong, nil }
			h.start()
			h.waitStatus(StatusAuthFailed, 1)
			time.Sleep(b.AuthPause - 150*time.Millisecond)
			if n := fs.count("LOGIN"); n != 1 {
				t.Errorf("LOGIN count during AuthPause = %d, want 1", n)
			}
			h.waitStatus(StatusAuthFailed, 2)
			h.stop()
			for _, s := range h.statusesOf(StatusAuthFailed) {
				if s.Delay != b.AuthPause {
					t.Errorf("auth_failed delay = %v, want %v", s.Delay, b.AuthPause)
				}
			}
			if n := len(h.statusesOf(StatusBackoff)); n != 0 {
				t.Errorf("backoff statuses = %d; a rejected LOGIN must pause, not back off", n)
			}
			logins := loginTimes(fs)
			if len(logins) < 2 || logins[1].Sub(logins[0]) < b.AuthPause {
				t.Errorf("logins %v are closer than AuthPause", logins)
			}
		})
	}
}

// TestBackoffDefaults 覆盖生产默认值：Backoff{} 的有效 AuthPause 为 15 分钟，5 分钟按 10 分钟下限处理，MaxLogins 12，LoginWindow 1 小时；
// 负值与越界的 Jitter 同样取默认值。
func TestBackoffDefaults(t *testing.T) {
	want := Backoff{Initial: 15 * time.Second, Max: 10 * time.Minute, AuthPause: 15 * time.Minute, Jitter: 0.2, MaxLogins: 12, LoginWindow: time.Hour}
	if got := (Backoff{}).withDefaults(); got != want {
		t.Errorf("Backoff{} with defaults = %+v, want %+v", got, want)
	}
	if got := (Backoff{AuthPause: 5 * time.Minute}).withDefaults().AuthPause; got != 10*time.Minute {
		t.Errorf("AuthPause 5m with defaults = %v, want the 10m floor", got)
	}
	custom := Backoff{Initial: 1, Max: 2, AuthPause: time.Hour, Jitter: 0.1, MaxLogins: 3, LoginWindow: 4}
	if got := custom.withDefaults(); got != custom {
		t.Errorf("custom Backoff with defaults = %+v", got)
	}
	// 负值与越界的 Jitter 按未设置处理，避免登录频率限制越界 panic 或退避等待为负。
	negative := Backoff{Initial: -1, Max: -1, AuthPause: -1, Jitter: -0.5, MaxLogins: -1, LoginWindow: -1}
	if got := negative.withDefaults(); got != want {
		t.Errorf("negative Backoff with defaults = %+v, want %+v", got, want)
	}
	if got := (Backoff{Jitter: 1.5}).withDefaults().Jitter; got != 0.2 {
		t.Errorf("Jitter 1.5 with defaults = %v, want 0.2", got)
	}
}

// TestWatcherCredentialsUnavailable 覆盖凭据不可用：不连接，发出 credentials_unavailable，Delay 等于 Max，不计入登录次数。
func TestWatcherCredentialsUnavailable(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	b := testBackoff()
	b.MaxLogins, b.LoginWindow = 1, time.Minute
	h := newHarness(t, fs, testTimeouts(), b)
	var mu sync.Mutex
	calls := 0
	h.w.Password = func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls <= 2 {
			return "", errors.New("keychain is locked")
		}
		return testPassword, nil
	}
	h.start()
	h.waitStatus(StatusCredentialsUnavailable, 2)
	if n := fs.acceptedConns(); n != 0 {
		t.Errorf("connections while credentials are unavailable = %d", n)
	}
	h.waitStatus(StatusConnected, 1)
	h.stop()
	for _, s := range h.statusesOf(StatusCredentialsUnavailable) {
		if s.Delay != b.Max {
			t.Errorf("credentials_unavailable delay = %v, want %v", s.Delay, b.Max)
		}
	}
	if n := len(h.statusesOf(StatusBackoff)); n != 0 {
		t.Errorf("backoff statuses = %d; failed credential reads must not count as logins", n)
	}
}

// TestWatcherJunkFolder 覆盖 Junk 缺失与暂时不可用：首次缺失恰好发出一次 folder_missing，重连不重复；创建后开始扫描且不发出；
// 删除后再发出一次；EXAMINE Junk 被拒绝时发出 folder_unavailable，下一轮照常扫描并交付其中的邮件。
func TestWatcherJunkFolder(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{noJunk: true})
	h := newHarness(t, fs, testTimeouts(), testBackoff())
	h.start()
	waitFor(t, "IDLE", func() bool { return fs.count("IDLE") >= 1 })
	fs.disconnectAll()
	waitFor(t, "second login", func() bool { return fs.count("LIST") >= 2 && fs.count("IDLE") >= 2 })
	if n := len(h.statusesOf(StatusFolderMissing)); n != 1 {
		t.Fatalf("folder_missing count = %d, want 1", n)
	}
	for _, cmd := range fs.commands() {
		if cmd.Name == "EXAMINE" && strings.Contains(cmd.Args, FolderJunk) {
			t.Fatalf("client examined Junk while it was missing")
		}
	}

	if err := fs.user.Create(FolderJunk, nil); err != nil {
		t.Fatal(err)
	}
	fs.appendMessage(FolderJunk, testMessage(1, 100))
	fs.disconnectAll()
	h.waitDelivered(FolderJunk, 1)
	if n := len(h.statusesOf(StatusFolderMissing)); n != 1 {
		t.Errorf("folder_missing count after Junk appeared = %d, want 1", n)
	}

	if err := fs.user.Delete(FolderJunk); err != nil {
		t.Fatal(err)
	}
	fs.disconnectAll()
	h.waitStatus(StatusFolderMissing, 2)

	if err := fs.user.Create(FolderJunk, nil); err != nil {
		t.Fatal(err)
	}
	fs.appendMessage(FolderJunk, testMessage(2, 100))
	fs.addRule(&rule{command: "EXAMINE", contains: FolderJunk, kind: faultReject, limit: 1})
	fs.disconnectAll()
	h.waitStatus(StatusFolderUnavailable, 1)
	waitFor(t, "delivery from the recreated Junk", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.cursors[FolderJunk].LastUID == 1 && h.cursors[FolderJunk].UIDValidity == fs.status(FolderJunk).UIDValidity
	})
	h.stop()
	if n := len(h.statusesOf(StatusFolderMissing)); n != 2 {
		t.Errorf("folder_missing count = %d, want 2", n)
	}
	if n := len(h.statusesOf(StatusFolderUnavailable)); n != 1 {
		t.Errorf("folder_unavailable count = %d, want 1", n)
	}
	for _, s := range h.statusesOf(StatusFolderMissing) {
		if s.Folder != FolderJunk {
			t.Errorf("folder_missing folder = %q", s.Folder)
		}
	}
}

// examineCount 返回代理记录的、对 folder 发出的 EXAMINE 次数。
func examineCount(fs *fakeServer, folder string) int {
	n := 0
	for _, cmd := range fs.commands() {
		if cmd.Name == "EXAMINE" && strings.Contains(cmd.Args, folder) {
			n++
		}
	}
	return n
}

// assertInboxFirst 断言本次运行中第一条 EXAMINE 是对 INBOX 发出的：INBOX 先补扫，Junk 的失败才不会连累本轮 INBOX 的交付。
func assertInboxFirst(t *testing.T, fs *fakeServer) {
	t.Helper()
	for _, cmd := range fs.commands() {
		if cmd.Name != "EXAMINE" {
			continue
		}
		if !strings.Contains(cmd.Args, FolderInbox) {
			t.Errorf("first EXAMINE was %q, want INBOX before Junk", cmd.Args)
		}
		return
	}
	t.Error("no EXAMINE was sent")
}

// TestWatcherDegradesJunkAfterRepeatedFailures 覆盖 Junk 持续失败时的降级：每轮先补扫 INBOX 再补扫 Junk，
// Junk 连续失败 junkFailLimit 次后在本次 Run 内跳过它并发出一次 folder_disabled，INBOX 的新邮件照常交付。
// 三种持续失败各一例：服务器以带标签 BAD 拒绝 EXAMINE（连接仍可用）、无标签 BAD 使 EXAMINE 超时（连接被关闭）、
// Junk 批次的 Handle 一直失败（本地失败，同一连接内重试有上限）。第四例是反面：会自行消失的连接层失败不触发降级。
func TestWatcherDegradesJunkAfterRepeatedFailures(t *testing.T) {
	t.Run("tagged BAD", func(t *testing.T) {
		fs := newFakeServer(t, proxyOptions{})
		fs.appendMessage(FolderInbox, testMessage(1, 100))
		fs.addRule(&rule{command: "EXAMINE", contains: FolderJunk, kind: faultRejectBAD})
		tm := testTimeouts()
		tm.IdleMax = 150 * time.Millisecond
		h := newHarness(t, fs, tm, testBackoff())
		h.start()
		h.waitDelivered(FolderInbox, 1)
		h.waitStatus(StatusFolderDisabled, 1)
		fs.appendMessage(FolderInbox, testMessage(2, 100))
		h.waitDelivered(FolderInbox, 2)
		h.stop()
		assertInboxFirst(t, fs)
		if n := examineCount(fs, FolderJunk); n != junkFailLimit {
			t.Errorf("EXAMINE Junk count = %d, want %d; Junk must not be examined after the degradation", n, junkFailLimit)
		}
		if n := len(h.statusesOf(StatusFolderUnavailable)); n != junkFailLimit {
			t.Errorf("folder_unavailable count = %d, want %d", n, junkFailLimit)
		}
		if n := fs.count("LOGIN"); n != 1 {
			t.Errorf("LOGIN count = %d, want 1; a rejected Junk must not tear the connection down", n)
		}
		if n := len(h.statusesOf(StatusDisconnected)); n != 0 {
			t.Errorf("disconnected %d times", n)
		}
		for _, s := range h.statusesOf(StatusFolderDisabled) {
			if s.Folder != FolderJunk {
				t.Errorf("folder_disabled folder = %q", s.Folder)
			}
		}
	})

	t.Run("untagged BAD", func(t *testing.T) {
		fs := newFakeServer(t, proxyOptions{})
		fs.appendMessage(FolderInbox, testMessage(1, 100))
		fs.addRule(&rule{command: "EXAMINE", contains: FolderJunk, kind: faultBAD})
		h := newHarness(t, fs, testTimeouts(), testBackoff())
		h.start()
		h.waitDelivered(FolderInbox, 1)
		// Junk 的 EXAMINE 要到 Command 期限才超时，此时本轮 INBOX 早已交付，仍是第一条连接。
		if n := fs.count("LOGIN"); n != 1 {
			t.Errorf("LOGIN count at the first INBOX delivery = %d, want 1; INBOX must be drained before Junk", n)
		}
		h.waitStatus(StatusFolderDisabled, 1)
		// 降级前每次 Junk 超时都关闭连接，降级后连接稳定：新邮件在同一连接上交付，不再登录，也不再碰 Junk。
		fs.appendMessage(FolderInbox, testMessage(2, 100))
		h.waitDelivered(FolderInbox, 2)
		logins, examines := fs.count("LOGIN"), examineCount(fs, FolderJunk)
		fs.appendMessage(FolderInbox, testMessage(3, 100))
		h.waitDelivered(FolderInbox, 3)
		h.stop()
		assertInboxFirst(t, fs)
		if examines != junkFailLimit {
			t.Errorf("EXAMINE Junk count = %d, want %d", examines, junkFailLimit)
		}
		if n := examineCount(fs, FolderJunk); n != examines {
			t.Errorf("EXAMINE Junk count grew to %d after the degradation", n)
		}
		if n := fs.count("LOGIN"); n != logins {
			t.Errorf("LOGIN count grew from %d to %d after the degradation", logins, n)
		}
		if got := h.delivered(FolderInbox); !slices.Equal(got, []uint32{1, 2, 3}) {
			t.Errorf("INBOX deliveries = %v, want each message once", got)
		}
	})

	t.Run("handle keeps failing", func(t *testing.T) {
		fs := newFakeServer(t, proxyOptions{})
		fs.appendMessage(FolderJunk, testMessage(1, 100))
		fs.appendMessage(FolderInbox, testMessage(1, 100))
		b := testBackoff()
		b.Initial, b.Max = 20*time.Millisecond, 40*time.Millisecond
		h := newHarness(t, fs, testTimeouts(), b)
		h.failFolder, h.failNext = FolderJunk, 1000
		h.start()
		h.waitDelivered(FolderInbox, 1)
		h.waitStatus(StatusFolderDisabled, 1)
		fs.appendMessage(FolderInbox, testMessage(2, 100))
		h.waitDelivered(FolderInbox, 2)
		h.stop()
		// 每轮至多 junkFailLimit 次本地失败：前 junkFailLimit-1 次发出 handle_failed 并在同一连接内重试，最后一次结束本轮。
		if n, want := len(h.statusesOf(StatusHandleFailed)), junkFailLimit*(junkFailLimit-1); n != want {
			t.Errorf("handle_failed count = %d, want %d", n, want)
		}
		if n := len(h.statusesOf(StatusFolderUnavailable)); n != junkFailLimit {
			t.Errorf("folder_unavailable count = %d, want %d", n, junkFailLimit)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if cur := h.cursors[FolderJunk]; cur != (Cursor{}) {
			t.Errorf("Junk cursor = %+v, want no progress while Handle fails", cur)
		}
		if n := fs.count("LOGIN"); n != 1 {
			t.Errorf("LOGIN count = %d, want 1", n)
		}
	})

	t.Run("transient disconnects", func(t *testing.T) {
		fs := newFakeServer(t, proxyOptions{})
		fs.appendMessage(FolderInbox, testMessage(1, 100))
		fs.appendMessage(FolderJunk, testMessage(1, 100))
		// 只在补扫 Junk 时断连，且只断 junkFailLimit 次：断连与 Junk 是否可用无关，故障耗尽后 Junk 必须照常被补扫。
		fs.addRule(&rule{command: "EXAMINE", contains: FolderJunk, kind: faultDisconnect, limit: junkFailLimit})
		tm := testTimeouts()
		tm.IdleMax = 150 * time.Millisecond
		h := newHarness(t, fs, tm, testBackoff())
		h.start()
		h.waitDelivered(FolderInbox, 1)
		h.waitDelivered(FolderJunk, 1)
		h.stop()
		if n := len(h.statusesOf(StatusDisconnected)); n != junkFailLimit {
			t.Errorf("disconnected count = %d, want %d; the faults must be connection-layer failures", n, junkFailLimit)
		}
		if n := len(h.statusesOf(StatusFolderDisabled)); n != 0 {
			t.Errorf("folder_disabled count = %d, want 0; a dropped connection is not a Junk failure", n)
		}
		if n := len(h.statusesOf(StatusFolderUnavailable)); n != 0 {
			t.Errorf("folder_unavailable count = %d, want 0", n)
		}
	})
}

// TestWatcherRetriesJunkAfterRelist 覆盖降级的解除：Junk 的 EXAMINE 先被持续拒绝而降级，故障消失后，
// 同一连接上的下一次重新 LIST 解除降级，Junk 重新被补扫并交付；期间不重新登录。
func TestWatcherRetriesJunkAfterRelist(t *testing.T) {
	// 重新 LIST 的间隔要长于攒满 junkFailLimit 次失败所需的时间，否则降级还没发生就被复位。
	setVar(t, &relistInterval, 800*time.Millisecond)
	fs := newFakeServer(t, proxyOptions{})
	fs.appendMessage(FolderInbox, testMessage(1, 100))
	fs.appendMessage(FolderJunk, testMessage(1, 100))
	fs.addRule(&rule{command: "EXAMINE", contains: FolderJunk, kind: faultRejectBAD})
	tm := testTimeouts()
	tm.IdleMax = 50 * time.Millisecond
	h := newHarness(t, fs, tm, testBackoff())
	h.start()
	h.waitDelivered(FolderInbox, 1)
	h.waitStatus(StatusFolderDisabled, 1)
	fs.clearRules()
	h.waitDelivered(FolderJunk, 1)
	h.stop()
	if n := fs.count("LOGIN"); n != 1 {
		t.Errorf("LOGIN count = %d, want 1; the retry must stay on the same connection", n)
	}
}

// TestWatcherRelists 覆盖同一连接上定期重新 LIST：间隔在测试中降低后，LIST 次数增加而没有重新登录。
func TestWatcherRelists(t *testing.T) {
	setVar(t, &relistInterval, 200*time.Millisecond)
	fs := newFakeServer(t, proxyOptions{})
	tm := testTimeouts()
	tm.IdleMax = 100 * time.Millisecond
	h := newHarness(t, fs, tm, testBackoff())
	h.start()
	waitFor(t, "a second LIST", func() bool { return fs.count("LIST") >= 2 })
	h.stop()
	if n := fs.count("LOGIN"); n != 1 {
		t.Errorf("LOGIN count = %d, want 1", n)
	}
	// 两个空文件夹各交付一次 Reset 批次以持久化游标，此后游标不变的空批不交给 Handle。
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.batches) != 2 || !h.batches[0].Reset || !h.batches[1].Reset || h.batches[0].Next.UIDValidity == 0 || h.batches[1].Next.UIDValidity == 0 {
		t.Errorf("batches = %+v, want one Reset batch per folder", h.batches)
	}
}

// TestWatcherLoginSpacing 覆盖两次登录至少间隔 Initial：抖动很大、每次登录即失败时，退避可能短于 Initial，登录间隔仍不低于 Initial。
func TestWatcherLoginSpacing(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	fs.addRule(&rule{command: "LOGIN", kind: faultDisconnect})
	b := Backoff{Initial: 100 * time.Millisecond, Max: 100 * time.Millisecond, AuthPause: time.Second, Jitter: 0.9, MaxLogins: 100, LoginWindow: time.Minute}
	h := newHarness(t, fs, testTimeouts(), b)
	h.start()
	waitFor(t, "8 logins", func() bool { return fs.count("LOGIN") >= 8 })
	h.stop()
	logins := loginTimes(fs)
	for i := 1; i < len(logins); i++ {
		if gap := logins[i].Sub(logins[i-1]); gap < b.Initial {
			t.Errorf("gap between logins %d and %d = %v, below Initial %v", i-1, i, gap, b.Initial)
		}
	}
	if n := len(h.statusesOf(StatusConnected)); n != 0 {
		t.Errorf("connected %d times", n)
	}
}

// TestWatcherCancelDuringIdle 覆盖取消：IDLE 中取消 ctx，Run 在 1 秒内返回 context.Canceled，服务端会话已结束。
func TestWatcherCancelDuringIdle(t *testing.T) {
	fs := newFakeServer(t, proxyOptions{})
	tm := testTimeouts()
	tm.IdleMax = 5 * time.Second
	h := newHarness(t, fs, tm, testBackoff())
	h.start()
	waitFor(t, "IDLE", func() bool { return fs.count("IDLE") == 1 })
	time.Sleep(50 * time.Millisecond)
	h.stop()
	waitFor(t, "the session to end", func() bool { return fs.openConns() == 0 })
}
