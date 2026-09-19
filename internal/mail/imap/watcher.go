// Package imap 的长连接收取循环：登录后 LIST，依次补扫 Junk 与 INBOX，在 INBOX 上 IDLE（不支持或已降级时按 Poll 轮询），
// 连接失败时按退避重连，并限制登录频率；认证失败暂停，本地处理失败在同一连接内按退避重试。
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
	// idle_disabled、folder_missing、folder_unavailable
	Delay  time.Duration // backoff、auth_failed、credentials_unavailable、handle_failed 时为下次尝试前的等待
	Folder string        // folder_missing 与 folder_unavailable 时为文件夹名
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
	StatusFolderMissing          StatusKind = "folder_missing"          // LIST 中没有 Junk
	StatusFolderUnavailable      StatusKind = "folder_unavailable"      // Junk 在 LIST 中但 EXAMINE 被拒绝
)

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
	// relistInterval 是同一连接上重新 LIST 的间隔，测试可在包内降低。
	relistInterval = time.Hour
)

// withDefaults 把零值字段替换为默认值，并把 AuthPause 提高到下限。
func (b Backoff) withDefaults() Backoff {
	if b.Initial == 0 {
		b.Initial = 15 * time.Second
	}
	if b.Max == 0 {
		b.Max = 10 * time.Minute
	}
	if b.AuthPause == 0 {
		b.AuthPause = 15 * time.Minute
	}
	b.AuthPause = max(b.AuthPause, minAuthPause)
	if b.Jitter == 0 {
		b.Jitter = 0.2
	}
	if b.MaxLogins == 0 {
		b.MaxLogins = 12
	}
	if b.LoginWindow == 0 {
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
// Cursor 返回错误同样是本地错误，按 Handle 失败处理。
type Watcher struct {
	Config   Config
	Password func(ctx context.Context) (string, error)
	Cursor   func(ctx context.Context, folder string) (Cursor, error)
	Handle   func(ctx context.Context, b Batch) error
	Status   func(Status) // 可为 nil
	Backoff  Backoff
}

// Run 循环直到 ctx 结束并返回 ctx.Err()：连接并登录 → LIST 确定 Junk 是否存在 → 依次对 Junk（存在时）、INBOX
// 补扫直到 More 为 false → 在 INBOX 上 IDLE（不支持或已降级时等待 Poll）→ 回到补扫。连接与命令错误关闭连接并按退避重连；
// 认证失败暂停 AuthPause 并发出 auth_failed 状态。同一连接上每小时重新 LIST 一次。Watcher 在一次 Run 中记住上一次 LIST
// 对 Junk 是否存在的结论，初值为「存在」：因此首次 LIST 就没有 Junk 时发出一次 folder_missing，此后只在由存在变为缺失时
// 再发出一次；重连不重置这一结论，Junk 一直缺失时不会每次重连都重复发出。LIST 中存在但 EXAMINE 返回 ErrNoFolder 时
// 本轮跳过 Junk 并发出 folder_unavailable，下一轮照常重试。
//
// 退避只在连接自登录起保持健康达到 IdleMax、或一次 IDLE 正常结束后复位；补扫成功不复位。每次等待取退避与登录频率限制
// （两次登录至少间隔 Initial，任意 LoginWindow 内至多 MaxLogins 次）中较长者，只发出一个状态。
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
		r.logins = append(r.logins, time.Now())
		s, err := Dial(ctx, w.Config, password)
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
	junk         bool        // 上一次 LIST 是否含 Junk，初值为存在
	idleOff      bool        // IDLE 已连续超时 2 次，本次 Run 余下时间改为轮询
	idleTimeouts int         // Idle 连续返回 ErrTimeout 的次数
	logins       []time.Time // 登录（拨号）时刻，只保留最近一个 LoginWindow 内的
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
		if r.junk {
			if err := r.drain(ctx, s, FolderJunk); errors.Is(err, ErrNoFolder) {
				r.emit(Status{Kind: StatusFolderUnavailable, Folder: FolderJunk})
			} else if err != nil {
				return err
			}
		}
		if err := r.drain(ctx, s, FolderInbox); err != nil {
			return err
		}
		if time.Since(loginAt) >= r.t.IdleMax {
			r.reconnect.reset() // 连接自登录起已保持健康达到 IdleMax
		}
		if err := r.wait(ctx, s); err != nil {
			return err
		}
	}
}

// list 执行 LIST 并更新 Junk 是否存在的结论；由存在变为缺失时发出 folder_missing。
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
	return nil
}

// drain 以持久化的游标反复补扫 folder，把有内容或游标有变化的批次交给 Handle，直到 More 为 false。
// Cursor 或 Handle 失败时发出 handle_failed，按退避等待后在同一连接上重新补扫；Scan 的错误原样返回。
func (r *runner) drain(ctx context.Context, s *Session, folder string) error {
	for {
		cur, err := r.w.Cursor(ctx, folder)
		if err == nil {
			var b Batch
			if b, err = s.Scan(ctx, folder, cur); err != nil {
				return err
			}
			if len(b.Messages) > 0 || b.Next != cur {
				err = r.w.Handle(ctx, b)
			}
			if err == nil {
				r.local.reset()
				if !b.More {
					return nil
				}
				continue
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d := r.local.next()
		r.emit(Status{Kind: StatusHandleFailed, Delay: d})
		if err := s.sleep(ctx, d); err != nil {
			return err
		}
	}
}

// wait 等待新邮件：在 INBOX 上 IDLE，服务器不支持或本次 Run 已降级时等待 Poll。
// Idle 连续 2 次超时后降级并发出一次 idle_disabled；一次 IDLE 正常结束即复位重连退避。
func (r *runner) wait(ctx context.Context, s *Session) error {
	if r.idleOff || !s.caps.Has(imap.CapIdle) {
		return s.sleep(ctx, r.t.Poll)
	}
	_, err := s.Idle(ctx)
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
