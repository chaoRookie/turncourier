// Package imap 的长连接收取循环：登录后 LIST，每一轮依次补扫「已发送」（Watcher.Sent 为真时，只取头部）、INBOX 与 Junk，
// 再回到 INBOX 上 IDLE（不支持或已降级时按 Poll 轮询；Wake 的信号提前结束等待），连接失败时按退避重连，并限制登录频率；
// 认证失败暂停，本地处理失败在同一连接内按退避重试，Handle 可以把一批中的一部分延后到下一轮（ErrDefer）。
// 「已发送」与 Junk 的失败都不影响 INBOX：「已发送」是承重路径，失败只跳过本轮、从不降级；Junk 排在 INBOX 之后，本轮跳过即可，
// 连续失败到阈值后降级，满 relistInterval 后重新扫描。
package imap

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/emersion/go-imap/v2"
)

// Status 是 Watcher 的状态通知，不含地址、密码或邮件内容。
type Status struct {
	Kind StatusKind // connected、disconnected、auth_failed、backoff、credentials_unavailable、handle_failed、
	// idle_disabled、folder_missing、folder_unavailable、folder_disabled
	Delay  time.Duration // backoff、auth_failed、credentials_unavailable、handle_failed 时为下次尝试前的等待
	Folder string        // folder_missing、folder_unavailable 与 folder_disabled 时为文件夹名；「已发送」为 FolderSent
}

// StatusKind 是状态种类。
type StatusKind string

// Watcher 发出的状态种类。
const (
	StatusConnected              StatusKind = "connected"               // 登录成功
	StatusDisconnected           StatusKind = "disconnected"            // 已登录的连接因错误结束
	StatusAuthFailed             StatusKind = "auth_failed"             // LOGIN 被拒绝，暂停 AuthPause
	StatusBackoff                StatusKind = "backoff"                 // 重连前按退避或登录频率上限等待
	StatusCredentialsUnavailable StatusKind = "credentials_unavailable" // Password 返回错误，等待 Max 后再试
	StatusHandleFailed           StatusKind = "handle_failed"           // Cursor 或 Handle 返回错误，同一连接内按退避重试
	StatusIdleDisabled           StatusKind = "idle_disabled"           // IDLE 连续超时，本次 Run 改为轮询
	StatusFolderMissing          StatusKind = "folder_missing"          // LIST 中没有 Junk（「已发送」缺失按 folder_unavailable 报告）
	StatusFolderUnavailable      StatusKind = "folder_unavailable"      // 「已发送」或 Junk 本轮补扫未能完成，已跳过本轮；「已发送」不在 LIST 中时每轮都发出
	StatusFolderDisabled         StatusKind = "folder_disabled"         // Junk 连续失败达到 junkFailLimit，降级满 relistInterval 前不扫描它；「已发送」从不降级
)

// ErrDefer 由 Handle 包装返回，表示本批中有邮件须留到下一轮处理，Handle 已自行持久化可以前进的部分游标（可能一封也没有前进）。
// Watcher 结束该文件夹的本轮补扫：不发出 handle_failed、不退避、不计入 Junk 的失败次数、不调用 Scanned，照常补扫下一个文件夹
// 并等待；下一轮以 Cursor 重新补扫，被延后的邮件再次交付。INBOX 也如此；Handle 的其余错误仍按原有方式重试。
var ErrDefer = errors.New("imap: batch deferred to the next round")

// Backoff 是重连退避参数；零值字段使用默认值。
type Backoff struct {
	Initial     time.Duration // 15 秒；两次登录之间也至少间隔 Initial
	Max         time.Duration // 10 分钟
	AuthPause   time.Duration // 认证失败后的暂停，15 分钟；不得小于 10 分钟（测试可在包内降低下限）
	Jitter      float64       // ±20%
	MaxLogins   int           // LoginWindow 内至多登录的次数，12；达到后等到窗口内最早一次登录满 LoginWindow 再登录
	LoginWindow time.Duration // 1 小时
}

var (
	// minAuthPause 是 AuthPause 的下限（官方建议认证失败后暂停 10–15 分钟），测试可在包内降低。
	minAuthPause = 10 * time.Minute
	// relistInterval 是同一连接上重新 LIST 的间隔，也是 Junk 降级的恢复窗口（见 junkRetryAt）：
	// 改它会同时改变两者的节奏。测试可在包内降低。
	relistInterval = time.Hour
	// junkFailLimit 是 Junk 连续失败多少次后在 junkRetryAt 之前不再扫描它，也是一轮 Junk 补扫中允许的本地失败次数上限，
	// 测试可在包内降低。INBOX 没有这个上限：它按契约在同一连接上无限重试。
	junkFailLimit = 3
	// sentFailLimit 是一轮「已发送」补扫中允许的本地失败次数上限，达到即结束本轮（发出 folder_unavailable），下一轮照常重试。
	// 它不是降级阈值：「已发送」是承重路径，从不降级；设上限只是为了 Handle 持续失败时不挡住同一轮的 INBOX。
	sentFailLimit = 3
)

// withDefaults 把零值或负值字段替换为默认值，并把 AuthPause 提高到下限；Jitter 须在 (0, 1) 内，否则取默认值，
// 使退避等待不会变成负数。负的 MaxLogins 若原样使用，登录频率限制会越界 panic。
func (b Backoff) withDefaults() Backoff {
	if b.Initial <= 0 {
		b.Initial = 15 * time.Second
	}
	if b.Max <= 0 {
		b.Max = 10 * time.Minute
	}
	if b.AuthPause <= 0 {
		b.AuthPause = 15 * time.Minute
	}
	b.AuthPause = max(b.AuthPause, minAuthPause)
	if b.Jitter <= 0 || b.Jitter >= 1 {
		b.Jitter = 0.2
	}
	if b.MaxLogins <= 0 {
		b.MaxLogins = 12
	}
	if b.LoginWindow <= 0 {
		b.LoginWindow = time.Hour
	}
	return b
}

// Watcher 维护长连接收取循环。Password 在每次登录前调用；4b 装配时它返回进程启动时读出、缓存在内存中的授权码，
// 只在认证失败后才重新读取 Keychain（见「4b 与 4a 的衔接」），因此重连不会反复启动 security 或触发钥匙串弹窗。
// Password 返回错误时不连接，发出 credentials_unavailable 并等待 Max 后再试，不计入连接失败与登录次数。本包不知道凭据来自
// 何处，所以状态名是通用的 credentials_unavailable；4b 装配层把它报告为 keychain_unavailable（见「4b 与 4a 的衔接」）。
// Cursor 返回某文件夹已持久化的游标（没有时返回零值与 nil）；Handle 处理一批邮件并在成功后持久化 b.Next。
// Handle 返回错误（本地数据库忙、正文密钥不可用等）时本批游标不前进，Watcher 发出 handle_failed，按退避等待后
// 在同一连接上以 Cursor 重新补扫并再次交付同一批，不断开、不重新登录；连接在等待期间断开时按正常重连处理。
// Cursor 返回错误同样是本地错误，按 Handle 失败处理。Handle 返回包装 ErrDefer 的错误时按 ErrDefer 的说明处理。
// Cursor、Handle、Status、Scanned、SkipHistory 与 Now 都在 Run 所在的 goroutine 中依次调用，不会并发。
type Watcher struct {
	Config   Config
	Password func(ctx context.Context) (string, error)
	Cursor   func(ctx context.Context, folder string) (Cursor, error)
	Handle   func(ctx context.Context, b Batch) error
	Status   func(Status) // 可为 nil
	Backoff  Backoff

	// Sent 为真时每一轮最先以 ScanHeaders 补扫 FolderSent，批交给同一个 Handle（Batch.Folder 为 FolderSent）。它是承重路径（D6）：
	// 失败只跳过本轮、下一轮照常重试，不像 Junk 那样降级；每次失败发出 folder_unavailable（Folder 为 FolderSent），不因此拆掉连接。
	// LIST 中没有它时同样按失败处理：每轮都发出 folder_unavailable、不调用 Scanned（而不是像 Junk 那样只报一次 folder_missing）。
	Sent bool
	// Wake 可为 nil。等待新邮件时（IDLE 或 Poll 间隔）收到信号即正常结束等待——IDLE 发出 DONE 并按正常结束处理——开始新的一轮。
	// 只在等待时读取它：补扫期间到达的信号留在通道中，使下一次等待立即结束（此时不发出 IDLE）。发送方应非阻塞写入容量为 1 的通道。
	Wake <-chan struct{}
	// Scanned 可为 nil。某个文件夹本轮补扫成功完成（最后一批 More 为假，且 Handle 没有返回错误或 ErrDefer）时调用，
	// started 是本轮补扫该文件夹开始的时刻（第一次读取它的游标之前，本轮在同一连接内重试过也取第一次尝试之前），取自 Now。
	// D6 以它判断「副本确实不在」：开始时刻之前已在文件夹中的邮件，都已交给 Handle 并处理成功。
	Scanned func(folder string, started time.Time)
	// SkipHistory 可为 nil。文件夹没有持久化游标（Cursor 返回零值）且它返回真时，不补扫历史，只以 EXAMINE 得到的 UIDVALIDITY
	// 与 UIDNEXT−1 构造一个不含邮件的批（Next 即该游标）交给 Handle 持久化。它在 EXAMINE 返回之后才被询问：首次运行时，
	// 恰在 EXAMINE 途中发出的通知，其副本已计入 UIDNEXT，而通知在发出之前就已写入库中，此时询问的结果随之为假，副本不会被越过。
	// 已有游标或服务器没有报告 UIDNEXT 时不询问它，照常补扫。
	SkipHistory func(folder string) bool
	// Now 可为 nil（取 time.Now）。只用于 Scanned 的补扫开始时刻：调用方拿它和存储的时间戳比较，两者须出自同一个时钟。
	// 登录间隔、退避、Junk 的降级窗口与重新 LIST 等计时仍用真实时间。
	Now func() time.Time
}

// Run 循环直到 ctx 结束并返回 ctx.Err()：连接并登录 → LIST 确定 Junk 与「已发送」是否存在 → 每一轮依次补扫「已发送」
// （Sent 为真时）、INBOX、Junk（存在且未降级时），各自直到 More 为 false 或 Handle 延后 → 回到 INBOX 上 IDLE（不支持或已降级时
// 等待 Poll；Wake 提前结束等待）→ 开始下一轮。连接与命令错误关闭连接并按退避重连；认证失败暂停 AuthPause 并发出 auth_failed 状态。
// 同一连接上每小时重新 LIST 一次。Watcher 在一次 Run 中记住上一次 LIST 对 Junk 是否存在的结论，初值为「存在」：因此首次 LIST
// 就没有 Junk 时发出一次 folder_missing，此后只在由存在变为缺失时再发出一次；重连不重置这一结论，Junk 一直缺失时不会每次重连
// 都重复发出。
//
// 「已发送」本轮补扫失败（不在 LIST 中、EXAMINE 被拒绝、命令失败或超时、连接断开、本轮累计 sentFailLimit 次本地处理失败）时
// 只发出 folder_unavailable 并跳过本轮，照常补扫 INBOX 与 Junk；从不降级，下一轮照常重试。失败时连接已被关闭的（命令超时或
// 服务器断开），重连后的那一轮从 INBOX 继续，不再先补扫「已发送」：否则「已发送」每次都拆掉连接时 INBOX 永远轮不到补扫。
//
// INBOX 排在 Junk 之前，Junk 的任何失败都不影响本次连接已交付的 INBOX 新邮件。Junk 本轮补扫失败（EXAMINE 被拒绝、
// 命令失败或超时、本轮累计 junkFailLimit 次本地处理失败）时只发出 folder_unavailable 并跳过本轮，不因此断开连接；
// 连续失败达到 junkFailLimit 次后发出一次 folder_disabled，降级满 relistInterval 后才重新扫描 Junk，成功一次即清零计数。
// 连接层失败（ErrClosed）不计入这个次数：它与 Junk 是否可用无关，重连后 Junk 照常补扫。
// INBOX 的失败仍按原有方式处理：命令失败关闭连接并重连，本地处理失败在同一连接内按退避无限重试。
//
// 退避只在连接自登录起保持健康达到 IdleMax、或一次 IDLE 正常结束（含被 Wake 结束）后复位；补扫成功不复位。每次等待取退避与
// 登录频率限制（两次登录至少间隔 Initial，任意 LoginWindow 内至多 MaxLogins 次）中较长者，只发出一个状态。
func (w *Watcher) Run(ctx context.Context) error {
	b := w.Backoff.withDefaults()
	r := &runner{w: w, b: b, t: w.Config.Timeouts.withDefaults(), junk: true, reconnect: retry{b: b}, local: retry{b: b}}
	kind, pending := StatusBackoff, time.Duration(0)
	for {
		if d := max(pending, r.loginWait(time.Now())); d > 0 {
			r.emit(Status{Kind: kind, Delay: d})
			if err := sleep(ctx, d); err != nil {
				return err
			}
		}
		kind, pending = StatusBackoff, 0
		password, err := w.Password(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			kind, pending = StatusCredentialsUnavailable, b.Max
			continue
		}
		s, err := Dial(ctx, w.Config, password)
		// 在拨号返回后登记：此刻不早于 LOGIN 发出的时刻，按它计算的间隔与窗口不会因拨号、问候较慢而少算；拨号失败也计入。
		r.logins = append(r.logins, time.Now())
		switch {
		case ctx.Err() != nil:
			if s != nil {
				s.shutdown()
			}
			return ctx.Err()
		case errors.Is(err, ErrAuthFailed):
			kind, pending = StatusAuthFailed, b.AuthPause
			continue
		case err != nil:
			pending = r.reconnect.next()
			continue
		}
		r.emit(Status{Kind: StatusConnected})
		_ = r.serve(ctx, s, time.Now())
		if ctx.Err() != nil {
			s.shutdown()
			return ctx.Err()
		}
		s.Close()
		r.emit(Status{Kind: StatusDisconnected})
		pending = r.reconnect.next()
	}
}

// runner 是一次 Run 的状态。
type runner struct {
	w            *Watcher
	b            Backoff
	t            Timeouts
	sent         bool        // 上一次 LIST 是否含「已发送」；每条连接在第一轮之前都已 LIST，所以不需要初值
	resumeInbox  bool        // 上一条连接在补扫「已发送」失败时已被关闭：重连后的那一轮从 INBOX 继续，不再先补扫「已发送」
	junk         bool        // 上一次 LIST 是否含 Junk，初值为存在
	junkOff      bool        // Junk 已连续失败 junkFailLimit 次，在 junkRetryAt 之前不再扫描它
	junkFails    int         // Junk 连续补扫失败的次数（跨重连累计），成功一次即清零
	junkRetryAt  time.Time   // 降级解除的时刻：降级时置为 now+relistInterval，到点后清零计数并恢复补扫
	idleOff      bool        // IDLE 已连续超时 2 次，本次 Run 余下时间改为轮询
	idleTimeouts int         // Idle 连续返回 ErrTimeout 的次数
	logins       []time.Time // 登录时刻（拨号返回时），只保留最近一个 LoginWindow 内的
	reconnect    retry       // 重连退避
	local        retry       // Cursor 与 Handle 失败后的同连接重试退避
}

// emit 发出状态通知；Status 为 nil 时忽略。
func (r *runner) emit(s Status) {
	if r.w.Status != nil {
		r.w.Status(s)
	}
}

// loginWait 返回下次登录前还需等待多久：距上次登录至少 Initial，LoginWindow 内已有 MaxLogins 次时等到最早一次满窗口。
func (r *runner) loginWait(now time.Time) time.Duration {
	r.logins = slices.DeleteFunc(r.logins, func(at time.Time) bool { return now.Sub(at) >= r.b.LoginWindow })
	var d time.Duration
	if n := len(r.logins); n > 0 {
		d = r.logins[n-1].Add(r.b.Initial).Sub(now)
		if n >= r.b.MaxLogins {
			d = max(d, r.logins[n-r.b.MaxLogins].Add(r.b.LoginWindow).Sub(now))
		}
	}
	return max(d, 0)
}

// serve 在一条已登录的连接上循环补扫与等待新邮件，直到出错；返回时连接可能仍然打开（例如服务器拒绝了 INBOX）。
func (r *runner) serve(ctx context.Context, s *Session, loginAt time.Time) error {
	if err := r.list(ctx, s); err != nil {
		return err
	}
	listedAt := time.Now()
	for {
		if time.Since(listedAt) >= relistInterval {
			if err := r.list(ctx, s); err != nil {
				return err
			}
			listedAt = time.Now()
		}
		// 降级满 relistInterval 后解除：Junk 的失败可能是暂时的（服务器忙、NO [UNAVAILABLE]、命令超时），
		// 否则本次 Run 余下时间再也不扫描 Junk，被误判为垃圾邮件的回复会被静默丢弃。闸门按时刻判断而不是按
		// 「又 LIST 了一次」：重连会让本次连接的 LIST 计时从头开始，连接活不到 relistInterval 时就永远轮不到复位。
		// 重试仍被闸门压在每 relistInterval 至多一簇（junkFailLimit 次），不会形成命令或重连风暴。
		if r.junkOff && !time.Now().Before(r.junkRetryAt) {
			r.junkFails, r.junkOff = 0, false
		}
		// 每一轮最先补扫「已发送」：通知的副本在发信时就已存在，回复必然更晚到达，先记下实际投递 ID 再处理回复（D6）。
		// 上一条连接正是在补扫它时被关闭的，这一轮从 INBOX 继续（见 drainSent）。补扫它之后不必重新 EXAMINE INBOX：
		// 紧接着的 INBOX 补扫本身就会选中 INBOX。
		if r.w.Sent {
			if r.resumeInbox {
				r.resumeInbox = false
			} else if err := r.drainSent(ctx, s); err != nil {
				return err
			}
		}
		if err := r.drain(ctx, s, FolderInbox, 0); err != nil {
			return err
		}
		if r.junk && !r.junkOff {
			if err := r.drainJunk(ctx, s); err != nil {
				return err
			}
			// Junk 的 EXAMINE 无论成功还是被拒绝，选中的文件夹都不再是 INBOX（EXAMINE 失败时没有选中任何文件夹），
			// 而 IDLE 只在选中的文件夹上等待新邮件，所以回到 INBOX；这一步失败是 INBOX 的失败，按原有方式重连。
			if _, err := s.Examine(ctx, FolderInbox); err != nil {
				return err
			}
		}
		if time.Since(loginAt) >= r.t.IdleMax {
			r.reconnect.reset() // 连接自登录起已保持健康达到 IdleMax
		}
		if err := r.wait(ctx, s); err != nil {
			return err
		}
	}
}

// list 执行 LIST 并更新 Junk 与「已发送」是否存在的结论；Junk 由存在变为缺失时发出 folder_missing。
// 「已发送」缺失不在这里报告，而是每轮补扫它时按失败发出 folder_unavailable（见 drainSent）。
func (r *runner) list(ctx context.Context, s *Session) error {
	folders, err := s.ListFolders(ctx)
	if err != nil {
		return err
	}
	present := slices.Contains(folders, FolderJunk)
	if r.junk && !present {
		r.emit(Status{Kind: StatusFolderMissing, Folder: FolderJunk})
	}
	r.junk = present
	r.sent = slices.Contains(folders, FolderSent)
	return nil
}

// drainSent 在每轮最先补扫「已发送」（只取头部）。它是承重路径（D6）：本轮失败只发出 folder_unavailable 并跳过本轮，
// 不把错误交给 serve，因此不因它拆掉连接；从不计数、从不降级，下一轮照常重试。LIST 中没有它同样按失败处理，且不 EXAMINE 它。
// 连接层失败（命令超时或服务器断开，连接已被关闭）也算本轮失败：对 D6 而言它同样意味着本轮拿不到「副本确实不在」的证据；
// 此时置 resumeInbox，紧随其后的 INBOX 补扫发现连接已关闭而按原有方式重连，重连后的那一轮从 INBOX 继续。否则「已发送」
// 每次都拆掉连接时（例如对它的 EXAMINE 一直只回无标签 BAD），INBOX 永远轮不到补扫。只有 ctx 结束时才返回错误。
func (r *runner) drainSent(ctx context.Context, s *Session) error {
	if !r.sent {
		r.emit(Status{Kind: StatusFolderUnavailable, Folder: FolderSent})
		return nil
	}
	err := r.drain(ctx, s, FolderSent, sentFailLimit)
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return err
	}
	r.emit(Status{Kind: StatusFolderUnavailable, Folder: FolderSent})
	r.resumeInbox = s.closed
	return nil
}

// drainJunk 在 INBOX 之后补扫 Junk（调用方已确认 Junk 存在且本次 Run 未降级）。本轮失败只发出 folder_unavailable
// 并跳过本轮，不把错误交给 serve，因此不会拆掉本次连接；连接确实已断开时，紧接着重新 EXAMINE INBOX 就会发现并按原有方式重连。
// 连续失败达到 junkFailLimit 次后发出一次 folder_disabled，降级满 relistInterval 后才重新扫描 Junk；成功一次即清零计数。
// Handle 延后本批（ErrDefer）不是失败：drain 照常返回 nil，Junk 确实可以访问，计数同样清零。
// 连接层失败（ErrClosed）不算 Junk 的失败：它与 Junk 是否可用无关，重连后 Junk 照常补扫，否则几次恰好落在补扫 Junk
// 窗口内的断连就会让 Junk 被反复误判为不可用，而降级的解除要等到 junkRetryAt。这类失败也不发 folder_unavailable，
// 由紧随其后的 Examine(INBOX) 触发原有的重连与 disconnected 状态。只有 ctx 结束时才返回错误。
func (r *runner) drainJunk(ctx context.Context, s *Session) error {
	// INBOX 补扫期间到达的 EXISTS 留在通道里，而 Junk 的 Scan 会在 EXAMINE 之前排空它。补扫结束后放回这个信号，
	// wait 才能立即回到 INBOX 再补扫一轮，INBOX 的新邮件不必等到 IdleMax。
	if len(s.exists) > 0 {
		defer signal(s.exists)
	}
	err := r.drain(ctx, s, FolderJunk, junkFailLimit)
	switch {
	case err == nil:
		r.junkFails = 0
		return nil
	case ctx.Err() != nil:
		return err
	case errors.Is(err, ErrClosed):
		return nil
	}
	r.junkFails++
	r.emit(Status{Kind: StatusFolderUnavailable, Folder: FolderJunk})
	if r.junkFails >= junkFailLimit {
		r.junkOff, r.junkRetryAt = true, time.Now().Add(relistInterval)
		r.emit(Status{Kind: StatusFolderDisabled, Folder: FolderJunk})
	}
	return nil
}

// drain 以持久化的游标反复补扫 folder（「已发送」只取头部），把有内容或游标有变化的批次交给 Handle，直到 More 为 false，
// 此时本轮补扫成功完成，调用 Scanned。文件夹没有游标且 SkipHistory 返回真时不补扫历史，只交付落在 UIDNEXT−1 的游标。
// Handle 返回包装 ErrDefer 的错误时结束本轮补扫并返回 nil：不重试、不退避、不调用 Scanned，调用方照常进入下一个文件夹。
// Cursor 或 Handle 的其他失败发出 handle_failed，按退避等待后在同一连接上重新补扫；补扫的错误原样返回。
// maxLocal 是本轮允许的本地失败次数（本次调用内累计，成功不清零，只复位退避），达到即返回最后一个错误；
// 0 表示不限，INBOX 按契约取 0。
func (r *runner) drain(ctx context.Context, s *Session, folder string, maxLocal int) error {
	started := r.scanStart()
	local := 0
	for {
		cur, err := r.w.Cursor(ctx, folder)
		if err == nil {
			opts := scanOptions{headers: folder == FolderSent}
			if cur == (Cursor{}) && r.w.SkipHistory != nil {
				opts.skipHistory = func() bool { return r.w.SkipHistory(folder) }
			}
			var b Batch
			if b, err = s.scan(ctx, folder, cur, opts); err != nil {
				return err
			}
			if len(b.Messages) > 0 || b.Next != cur {
				err = r.w.Handle(ctx, b)
			}
			if errors.Is(err, ErrDefer) {
				return nil
			}
			if err == nil {
				r.local.reset()
				if !b.More {
					if r.w.Scanned != nil {
						r.w.Scanned(folder, started)
					}
					return nil
				}
				continue
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if local++; maxLocal > 0 && local >= maxLocal {
			return err
		}
		d := r.local.next()
		r.emit(Status{Kind: StatusHandleFailed, Delay: d})
		if err := s.sleep(ctx, d, nil); err != nil {
			return err
		}
	}
}

// scanStart 返回本轮补扫某文件夹开始的时刻，取自 Watcher.Now（为 nil 时取 time.Now）；Scanned 为 nil 时用不到它，返回零值。
func (r *runner) scanStart() time.Time {
	switch {
	case r.w.Scanned == nil:
		return time.Time{}
	case r.w.Now != nil:
		return r.w.Now()
	default:
		return time.Now()
	}
}

// wait 等待新邮件：在 INBOX 上 IDLE，服务器不支持或本次 Run 已降级时等待 Poll；Wake 收到信号时提前正常结束。
// Idle 连续 2 次超时后降级并发出一次 idle_disabled；一次 IDLE 正常结束（含被 Wake 结束、发出 DONE）即复位重连退避。
// 补扫期间已收到 EXISTS 或唤醒信号时不发送 IDLE，直接回到补扫（下一次 Scan 排空 EXISTS 信号，唤醒信号在这里取走）；
// 这不是一次 IDLE 正常结束，所以不复位退避，也不改变 IDLE 连续超时的计数。
func (r *runner) wait(ctx context.Context, s *Session) error {
	if r.idleOff || !s.caps.Has(imap.CapIdle) {
		return s.sleep(ctx, r.t.Poll, r.w.Wake)
	}
	if woken(r.w.Wake) || len(s.exists) > 0 {
		return nil
	}
	_, err := s.idle(ctx, r.w.Wake)
	if errors.Is(err, ErrTimeout) {
		r.idleTimeouts++
		if r.idleTimeouts >= 2 {
			r.idleOff = true
			r.emit(Status{Kind: StatusIdleDisabled})
		}
	} else {
		r.idleTimeouts = 0
	}
	if err != nil {
		return err
	}
	r.reconnect.reset()
	return nil
}

// woken 以非阻塞方式取走 wake 中已有的信号，报告是否取到；wake 为 nil 时返回 false。
func woken(wake <-chan struct{}) bool {
	select {
	case <-wake:
		return true
	default:
		return false
	}
}

// retry 是一串按倍数 2 增长、上限为 Max、带 ±Jitter 抖动的退避等待。
type retry struct {
	b    Backoff
	base time.Duration // 下一次等待的基准；0 表示 Initial
}

// next 返回下一次等待并把基准加倍。
func (r *retry) next() time.Duration {
	d := r.base
	if d == 0 {
		d = r.b.Initial
	}
	r.base = min(2*d, r.b.Max)
	return time.Duration(float64(d) * (1 + r.b.Jitter*(2*rand.Float64()-1)))
}

// reset 把下一次等待恢复为 Initial。
func (r *retry) reset() {
	r.base = 0
}

// sleep 等待 d 或 ctx 结束。
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
