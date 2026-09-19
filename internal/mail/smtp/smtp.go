// Package smtp 经隐式 TLS 向 SMTP 服务器提交单封邮件，把结果分为：已接受、确定未投递、被拒绝与结果不确定。
// 只调用 go-smtp 的 DialTLS，不支持 STARTTLS 与明文，不开启调试输出（它会写出凭据）。
// 本包不渲染邮件、不重试、不访问存储；重试与状态记录由调用方按 queue 的待发通知状态机处理。
package smtp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
)

// Config 是一次发送的连接参数。
type Config struct {
	Host     string         // 例如 smtp.qq.com
	Port     int            // 默认 465；端口可配置，TLS 模式固定为隐式 TLS
	Username string         // 机器人地址，也是信封发件人
	RootCAs  *x509.CertPool // nil 时使用系统根证书；测试注入自签 CA。tls.Config 只在包内构造：ServerName 固定为 Host，MinVersion 为 TLS 1.2
	Timeouts Timeouts
}

// Timeouts 是逐步期限；零值字段使用 DefaultTimeouts 中的对应值。
type Timeouts struct {
	Command    time.Duration // 问候、EHLO、AUTH、MAIL、RCPT、DATA（至 354）各自的期限，也是写正文时每个 64 KiB 分块的期限；默认 30 秒
	Submission time.Duration // 从调用 CloseWithResponse（flush 剩余正文、写结束标记）到收到响应的期限，默认 120 秒
}

// DefaultTimeouts 返回生产期限；拨号与 TLS 握手的 30 秒由 go-smtp 的 DialTLS 固定，不在此配置。
func DefaultTimeouts() Timeouts {
	return Timeouts{Command: 30 * time.Second, Submission: 120 * time.Second}
}

// withDefaults 把零值字段替换为 DefaultTimeouts 中的对应值。
func (t Timeouts) withDefaults() Timeouts {
	def := DefaultTimeouts()
	if t.Command == 0 {
		t.Command = def.Command
	}
	if t.Submission == 0 {
		t.Submission = def.Submission
	}
	return t
}

// Envelope 是信封；From 必须等于 Config.Username（QQ 要求信封发件人与登录用户一致）。
type Envelope struct {
	From string
	To   []string
}

// Result 是服务器接受邮件后的结果。
type Result struct {
	Response string // 结束标记后 250 响应的文本（不含状态码），至多 512 字节；L1 用它查找服务器分配的 ID
}

// MaxMessageSize 是单封邮件的字节上限。
const MaxMessageSize = 4 << 20

const (
	// chunkSize 是写正文的分块大小，每块各有一个 Command 期限。
	chunkSize = 64 << 10
	// maxResponse 是 Result.Response 保留的字节数上限。
	maxResponse = 512
	// ehloName 是 EHLO 使用的名字，与 go-smtp 的默认值相同，不透露本机主机名。
	ehloName = "localhost"
)

var (
	// ErrNotSent 表示进入提交阶段（调用 CloseWithResponse）之前失败：结束标记尚未开始写出，服务器不可能已接受这封邮件。
	ErrNotSent = errors.New("smtp: message was not sent")
	// ErrAuth 表示认证被拒绝；返回的错误同时满足 errors.Is(err, ErrNotSent)。
	ErrAuth = errors.New("smtp: authentication failed")
	// ErrRejected 表示服务器对结束标记回复了 4xx 或 5xx，确定未投递。
	ErrRejected = errors.New("smtp: message was rejected")
	// ErrUncertain 表示已进入提交阶段（flush 剩余至多 4 KiB 正文、写结束标记、读响应），但没有得到任何响应：可能已投递，
	// 调用方不得自动重发。flush 剩余正文时失败也归入此类：此时结束标记可能尚未写出，但本包无法区分，按保守方向处理。
	ErrUncertain = errors.New("smtp: delivery outcome is uncertain")
)

// closeData 是提交阶段调用的 CloseWithResponse；测试替换它来构造 flush 剩余正文时阻塞的情形。
var closeData = (*gosmtp.DataCommand).CloseWithResponse

// writeChunk 是写正文一个分块的调用；测试替换它来记录各分块的长度。
var writeChunk = (*gosmtp.DataCommand).Write

// ReplyError 携带服务器拒绝时的步骤与状态码，不含服务器响应文本。
type ReplyError struct {
	Step     string // auth、mail、rcpt、data、submission
	Code     int
	Enhanced [3]int // 服务器未给出时为零值
}

// Error 返回形如 "smtp: rcpt rejected with 550 5.1.1" 的文本。
func (e *ReplyError) Error() string {
	text := fmt.Sprintf("smtp: %s rejected with %d", e.Step, e.Code)
	if e.Enhanced != [3]int{} {
		text += fmt.Sprintf(" %d.%d.%d", e.Enhanced[0], e.Enhanced[1], e.Enhanced[2])
	}
	return text
}

// Temporary 在 4xx 时返回 true。
func (e *ReplyError) Temporary() bool {
	return e.Code/100 == 4
}

// Send 校验参数后连接、认证并提交一封邮件；ctx 结束时关闭连接。
// ctx 在进入提交阶段之前结束返回 ErrNotSent，进入之后（包括 flush 剩余正文期间）返回 ErrUncertain。错误文本只含步骤名、状态码与增强状态码，
// 不含服务器响应文本、地址、密码或邮件内容；服务器以 4xx/5xx 拒绝时错误链中含 *ReplyError。
//
// 参数不合法时返回包装 ErrNotSent 的错误且不联网。认证步骤的 5xx 回复都视为认证被拒绝（ErrAuth），4xx 只是 ErrNotSent。
// 问候与 EHLO 失败不附 *ReplyError：那是服务器对连接而不是对这封邮件的拒绝。
func Send(ctx context.Context, cfg Config, password string, env Envelope, msg []byte) (Result, error) {
	timeouts, err := validate(cfg, password, env, msg)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrNotSent, err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("%w: dial: %w", ErrNotSent, err)
	}
	// DialTLS 的拨号与握手期限由库固定为 30 秒且不响应 ctx；已确认的决策只允许 DialTLS，不为此改用其他拨号方式。
	client, err := gosmtp.DialTLS(net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)), tlsConfig(cfg))
	if err != nil {
		return Result{}, fmt.Errorf("%w: dial failed", ErrNotSent)
	}
	defer client.Close()
	// 库自带的期限只作第二道保险：它不覆盖写正文与 flush，第一道是下面的看门狗。
	client.CommandTimeout = timeouts.Command
	client.SubmissionTimeout = timeouts.Submission
	w := startWatchdog(ctx, func() { client.Close() })
	defer w.halt()

	if err := w.step(timeouts.Command, func() error { return client.Hello(ehloName) }); err != nil {
		return Result{}, w.fail(ErrNotSent, "ehlo", err)
	}
	if _, isTLS := client.TLSConnectionState(); !isTLS || !client.SupportsAuth("PLAIN") {
		return Result{}, fmt.Errorf("%w: server does not offer AUTH PLAIN over TLS", ErrNotSent)
	}
	if err := w.step(timeouts.Command, func() error {
		return client.Auth(sasl.NewPlainClient("", cfg.Username, password))
	}); err != nil {
		if reply := replyOf("auth", err); reply != nil && reply.Code/100 == 5 {
			return Result{}, fmt.Errorf("%w: %w: %w", ErrNotSent, ErrAuth, reply)
		}
		return Result{}, w.fail(ErrNotSent, "auth", err)
	}
	if err := w.step(timeouts.Command, func() error { return client.Mail(env.From, nil) }); err != nil {
		return Result{}, w.fail(ErrNotSent, "mail", err)
	}
	for _, to := range env.To {
		if err := w.step(timeouts.Command, func() error { return client.Rcpt(to, nil) }); err != nil {
			return Result{}, w.fail(ErrNotSent, "rcpt", err)
		}
	}
	var data *gosmtp.DataCommand
	if err := w.step(timeouts.Command, func() (err error) {
		data, err = client.Data()
		return err
	}); err != nil {
		return Result{}, w.fail(ErrNotSent, "data", err)
	}
	for off := 0; off < len(msg); off += chunkSize {
		chunk := msg[off:min(off+chunkSize, len(msg))]
		if err := w.step(timeouts.Command, func() error {
			_, err := writeChunk(data, chunk)
			return err
		}); err != nil {
			return Result{}, w.fail(ErrNotSent, "body", err)
		}
	}
	// 提交阶段：从这里起连接中断都可能发生在服务器接受之后，只能报告结果不确定。
	var resp *gosmtp.DataResponse
	if err := w.step(timeouts.Submission, func() (err error) {
		resp, err = closeData(data)
		return err
	}); err != nil {
		if reply := replyOf("submission", err); reply != nil {
			return Result{}, fmt.Errorf("%w: %w", ErrRejected, reply)
		}
		return Result{}, w.fail(ErrUncertain, "submission", err)
	}
	// QUIT 的结果不影响已确定的投递结果。
	_ = w.step(timeouts.Command, client.Quit)
	return Result{Response: truncate(resp.StatusText, maxResponse)}, nil
}

// validate 校验 Send 的参数并返回补齐默认值的期限；错误文本不含地址与密码。
func validate(cfg Config, password string, env Envelope, msg []byte) (Timeouts, error) {
	switch {
	case cfg.Host == "":
		return Timeouts{}, errors.New("invalid config: host is empty")
	case cfg.Port < 1 || cfg.Port > 65535:
		return Timeouts{}, errors.New("invalid config: port must be 1-65535")
	case cfg.Timeouts.Command < 0 || cfg.Timeouts.Submission < 0:
		return Timeouts{}, errors.New("invalid config: timeouts must not be negative")
	case env.From != cfg.Username:
		return Timeouts{}, errors.New("invalid envelope: sender must equal the login username")
	case !validLine(env.From):
		return Timeouts{}, errors.New("invalid envelope: sender must be non-empty without CR or LF")
	case len(env.To) == 0:
		return Timeouts{}, errors.New("invalid envelope: no recipients")
	case len(msg) == 0 || len(msg) > MaxMessageSize:
		return Timeouts{}, errors.New("invalid message: size must be 1 byte to 4 MiB")
	case !validPassword(password):
		return Timeouts{}, errors.New("invalid password: must be printable ASCII without whitespace")
	}
	for _, to := range env.To {
		if !validLine(to) {
			return Timeouts{}, errors.New("invalid envelope: recipients must be non-empty without CR or LF")
		}
	}
	return cfg.Timeouts.withDefaults(), nil
}

// validLine 判断地址非空且不含 CR、LF，不能借换行注入 SMTP 命令。
func validLine(s string) bool {
	return s != "" && !strings.ContainsAny(s, "\r\n")
}

// validPassword 判断密码非空且只由可打印 ASCII 组成（不含空格与控制字符）。
func validPassword(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '!' || s[i] > '~' {
			return false
		}
	}
	return s != ""
}

// tlsConfig 构造隐式 TLS 的客户端配置：校验证书链与主机名，最低 TLS 1.2；调用方只能影响根证书池。
func tlsConfig(cfg Config) *tls.Config {
	return &tls.Config{ServerName: cfg.Host, RootCAs: cfg.RootCAs, MinVersion: tls.VersionTLS12}
}

// replyOf 把服务器的 4xx/5xx 回复转为 *ReplyError 并丢弃响应文本；其他错误（含意外的 2xx、3xx 回复）返回 nil。
func replyOf(step string, err error) *ReplyError {
	var smtpErr *gosmtp.SMTPError
	if !errors.As(err, &smtpErr) || smtpErr.Code < 400 || smtpErr.Code > 599 {
		return nil
	}
	return &ReplyError{Step: step, Code: smtpErr.Code, Enhanced: [3]int(smtpErr.EnhancedCode)}
}

// truncate 把 s 截到至多 n 字节。
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// watchdog 在一个后台 goroutine 中等待 ctx 结束或当前步骤的计时器到期，任一发生即关闭连接，使阻塞的读写返回；
// 它记得自己为何关闭连接，供 fail 给失败分类。go-smtp 在写正文与 flush 期间不设连接期限，服务器停止读取或连接半开时
// 这两段会永久阻塞，只有关闭连接能让它们返回。是否已进入提交阶段由 Send 按失败所在的步骤决定，看门狗不需要知道。
type watchdog struct {
	ctx       context.Context    // 结束时关闭连接
	deadlines chan time.Duration // 进入一步时发送该步的期限，离开时发送 0 停止计时
	quit      chan struct{}      // halt 关闭它，通知 goroutine 退出
	done      chan struct{}      // goroutine 退出时关闭
	expired   atomic.Bool        // 计时器到期（而非 ctx 结束）关闭了连接
}

// startWatchdog 启动看门狗；此时不计时，只监视 ctx。
func startWatchdog(ctx context.Context, closeConn func()) *watchdog {
	w := &watchdog{ctx: ctx, deadlines: make(chan time.Duration), quit: make(chan struct{}), done: make(chan struct{})}
	go w.run(closeConn)
	return w
}

// run 是看门狗的循环；关闭连接后即退出，之后的步骤在已关闭的连接上立即失败。
func (w *watchdog) run(closeConn func()) {
	defer close(w.done)
	// Go 1.23 起 Stop 之后不会再从 C 收到旧的到期值，因此可以从一个已停止的计时器开始。
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	for {
		select {
		case d := <-w.deadlines:
			timer.Stop()
			if d > 0 {
				timer.Reset(d)
			}
		case <-timer.C:
			w.expired.Store(true)
			closeConn()
			return
		case <-w.ctx.Done():
			closeConn()
			return
		case <-w.quit:
			return
		}
	}
}

// step 在期限 d 内执行 fn：进入前以 d 重置计时器，fn 返回后停止计时；步骤之间没有网络读写，不需要计时。
func (w *watchdog) step(d time.Duration, fn func() error) error {
	w.set(d)
	err := fn()
	w.set(0)
	return err
}

// set 把新期限交给看门狗；看门狗已退出时直接返回。
func (w *watchdog) set(d time.Duration) {
	select {
	case w.deadlines <- d:
	case <-w.done:
	}
}

// fail 把某一步的失败包装为 outcome（ErrNotSent 或 ErrUncertain）：服务器 4xx/5xx 回复附 *ReplyError（问候与 EHLO 除外），
// 看门狗因 ctx 结束关闭连接时附 ctx 的错误；看门狗计时器或库自带的连接期限（期限相同，谁先到都可能）到期时注明超时；
// 库返回的其他错误可能带地址或服务器文本，只保留步骤名。
func (w *watchdog) fail(outcome error, step string, err error) error {
	if reply := replyOf(step, err); reply != nil && step != "ehlo" {
		return fmt.Errorf("%w: %w", outcome, reply)
	}
	if ctxErr := w.ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%w: %s: %w", outcome, step, ctxErr)
	}
	if w.expired.Load() || errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("%w: %s timed out", outcome, step)
	}
	return fmt.Errorf("%w: %s failed", outcome, step)
}

// halt 停止看门狗并等待其 goroutine 退出。
func (w *watchdog) halt() {
	close(w.quit)
	<-w.done
}
