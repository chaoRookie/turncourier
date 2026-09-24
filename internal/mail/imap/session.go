// Package imap 以只读方式从 IMAP 服务器收取新邮件：隐式 TLS 登录、EXAMINE、按 UID 补扫、BODY.PEEK 取信与 IDLE。
// 每条命令都有期限，超时即关闭连接，以应对 QQ 对不支持命令只回无标签 BAD、以及半开连接时库永久阻塞的问题。
// 本包不解析 MIME、不访问存储、不修改邮箱（不设 \Seen、不 MOVE、不 EXPUNGE、不 APPEND）。
package imap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const (
	// FolderInbox 是收件箱。
	FolderInbox = "INBOX"
	// FolderJunk 是 QQ 的垃圾箱名称；以 LIST 结果判断是否存在，不存在时跳过。
	FolderJunk = "Junk"
	// MaxBatch 是一次补扫取回的最多邮件数。
	MaxBatch = 50
	// MaxMessageSize 是取回正文的默认上限；更大的邮件只返回 UID 与大小。实现读取包内变量 maxMessageSize，
	// 其初值为本常量，测试可在包内降低它。
	MaxMessageSize = 2 << 20
	// MaxBatchBytes 是一批中已取回正文的合计上限；达到后本批提前结束并置 More，余下邮件留给下一批。
	// 实现读取包内变量 maxBatchBytes，其初值为本常量，测试可在包内降低它。
	MaxBatchBytes = 16 << 20
)

// maxMessageSize 与 maxBatchBytes 是实现实际使用的上限，初值为对应常量，测试可在包内降低。
var (
	maxMessageSize = MaxMessageSize
	maxBatchBytes  = MaxBatchBytes
)

// Config 是连接参数。
type Config struct {
	Host     string         // 例如 imap.qq.com
	Port     int            // 默认 993；端口可配置，TLS 模式固定为隐式 TLS
	Username string         // 机器人地址
	RootCAs  *x509.CertPool // nil 时使用系统根证书；tls.Config 只在包内构造：ServerName 固定为 Host，MinVersion 为 TLS 1.2
	Timeouts Timeouts
}

// Timeouts 是逐项期限；零值字段使用 DefaultTimeouts 中的对应值。
type Timeouts struct {
	Dial     time.Duration // TCP 连接与 TLS 握手，15 秒
	Greeting time.Duration // 服务器问候，15 秒
	Command  time.Duration // CAPABILITY、LOGIN、LIST、EXAMINE、UID SEARCH、LOGOUT，各 30 秒
	Fetch    time.Duration // 一批 UID FETCH 中的每封邮件，60 秒
	IdleAck  time.Duration // 发出 IDLE 到收到继续响应，30 秒
	IdleMax  time.Duration // 一次 IDLE 的最长持续时间，5 分钟；到期即结束并补扫
	IdleStop time.Duration // 发出 DONE 到 IDLE 完成，10 秒
	Poll     time.Duration // 服务器不支持 IDLE 时两次补扫的间隔，2 分钟
}

// DefaultTimeouts 返回生产期限。这些数值在 L1 测得 IDLE 推送延迟与服务器断开时间后可调整，调整须写入本清单。
func DefaultTimeouts() Timeouts {
	return Timeouts{
		Dial:     15 * time.Second,
		Greeting: 15 * time.Second,
		Command:  30 * time.Second,
		Fetch:    60 * time.Second,
		IdleAck:  30 * time.Second,
		IdleMax:  5 * time.Minute,
		IdleStop: 10 * time.Second,
		Poll:     2 * time.Minute,
	}
}

// withDefaults 把零值或负值字段替换为 DefaultTimeouts 中的对应值；负的期限会让每条命令立即超时。
func (t Timeouts) withDefaults() Timeouts {
	def := DefaultTimeouts()
	for _, f := range []struct{ v, d *time.Duration }{
		{&t.Dial, &def.Dial}, {&t.Greeting, &def.Greeting}, {&t.Command, &def.Command}, {&t.Fetch, &def.Fetch},
		{&t.IdleAck, &def.IdleAck}, {&t.IdleMax, &def.IdleMax}, {&t.IdleStop, &def.IdleStop}, {&t.Poll, &def.Poll},
	} {
		if *f.v <= 0 {
			*f.v = *f.d
		}
	}
	return t
}

// Cursor 是某个文件夹的收取游标；与 store/sqlite.Cursor 字段相同，但本包不依赖存储。
type Cursor struct {
	UIDValidity uint32
	LastUID     uint32
}

// Message 是取回的一封邮件；超过 MaxMessageSize 时 Raw 为 nil、TooLarge 为 true。
type Message struct {
	UID      uint32
	Size     int64
	Raw      []byte
	TooLarge bool
}

// Batch 是一次补扫的结果。Reset 为 true 表示 UIDVALIDITY 与游标不同（或没有游标），本批从 UID 1 开始全量补扫。
// Next 是本批全部处理完成后应持久化的游标；Messages 按 UID 升序。
type Batch struct {
	Folder      string
	UIDValidity uint32
	Reset       bool
	Messages    []Message
	Next        Cursor
	More        bool // 本批之外还有新邮件：UID SEARCH 结果超出 MaxBatch 封，或已取回正文合计达到 MaxBatchBytes 而提前结束；
	// 调用方处理本批后应以 Next 立即再次 Scan
}

var (
	// ErrAuthFailed 表示 LOGIN 被拒绝；授权码可能错误，或已因修改 QQ 密码而失效。
	ErrAuthFailed = errors.New("imap: authentication failed")
	// ErrTimeout 表示某条命令超过期限（包括无标签 BAD 造成的挂起与半开连接）；连接已被关闭。
	ErrTimeout = errors.New("imap: command timed out")
	// ErrClosed 表示连接已被服务器或本端关闭。
	ErrClosed = errors.New("imap: connection closed")
	// ErrCapability 表示服务器没有公告 IMAP4rev1、公告了 LOGINDISABLED（此时不发送凭据），或调用 Idle 时没有公告 IDLE。
	ErrCapability = errors.New("imap: required capability missing")
	// ErrNoFolder 表示 EXAMINE 被服务器以 NO 拒绝。原因可能是文件夹不存在，也可能是暂时不可用（例如 [UNAVAILABLE]），
	// 本包不区分；文件夹是否存在由调用方按 ListFolders 的结果判断。
	ErrNoFolder = errors.New("imap: folder cannot be examined")
)

// rejectedError 表示服务器以带标签的 NO 或 BAD（或问候中的 BYE）拒绝了一步；只保留步骤名与响应类型，不保留服务器文本，
// 以免错误中出现服务器回显的地址或其他内容。连接仍然可用。
type rejectedError struct {
	step string
	typ  imap.StatusResponseType
}

// Error 返回形如 "imap: examine rejected with NO" 的文本。
func (e *rejectedError) Error() string {
	return fmt.Sprintf("imap: %s rejected with %s", e.step, e.typ)
}

// isNo 判断 err 是否为服务器以 NO 拒绝。
func isNo(err error) bool {
	var rejected *rejectedError
	return errors.As(err, &rejected) && rejected.typ == imap.StatusResponseTypeNo
}

// Session 是一条已登录的连接；方法不可并发调用。
type Session struct {
	client *imapclient.Client
	t      Timeouts
	caps   imap.CapSet // 登录后读取的能力
	// exists 容量为 1，是「自上次 Scan 开始以来是否收到过 EXISTS」的唯一状态：解码协程以非阻塞发送留下信号，
	// Scan 在 EXAMINE 之前排空，Idle 取走。
	exists chan struct{}
	closed bool // 本端已关闭或已发现连接关闭；此后的调用都返回 ErrClosed
}

// Dial 以 imapclient.DialTLS 连接，等待问候，读取 CAPABILITY，确认 IMAP4rev1 且未公告 LOGINDISABLED 后 LOGIN，
// 登录后重新读取 CAPABILITY。LOGIN 被拒绝返回 ErrAuthFailed。
//
// 拨号与 TLS 握手受 Timeouts.Dial 约束但不响应 ctx（imapclient.DialTLS 不接受 ctx），其后每一步都响应 ctx。
// 错误文本不含地址、密码与服务器文本。
func Dial(ctx context.Context, cfg Config, password string) (*Session, error) {
	t := cfg.Timeouts.withDefaults()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("imap: dial: %w", err)
	}
	exists := make(chan struct{}, 1)
	client, err := imapclient.DialTLS(net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)), &imapclient.Options{
		TLSConfig: tlsConfig(cfg),
		Dialer:    &net.Dialer{Timeout: t.Dial, KeepAlive: 15 * time.Second},
		// imapclient 在解码协程中同步调用处理函数；这里从不阻塞，否则 client.Close 会与解码协程互相等待。
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(data *imapclient.UnilateralDataMailbox) {
				if data.NumMessages != nil {
					signal(exists)
				}
			},
		},
	})
	if err != nil {
		// 库的错误可能带地址或证书细节，只保留步骤名。
		return nil, errors.New("imap: dial failed")
	}
	s := &Session{client: client, t: t, exists: exists}
	if err := s.login(ctx, cfg.Username, password); err != nil {
		s.shutdown()
		return nil, err
	}
	return s, nil
}

// login 依次等待问候、检查登录前的能力、LOGIN 并读取登录后的能力。
func (s *Session) login(ctx context.Context, username, password string) error {
	if err := s.do(ctx, "greeting", s.t.Greeting, s.client.WaitGreeting); err != nil {
		return err
	}
	var caps imap.CapSet
	// 问候带有能力列表时 Caps 直接返回它，否则等待库在收到问候后自动发出的 CAPABILITY。
	// Caps 返回 nil 表示连接失败或服务器没有给出能力，都无法继续登录，按连接失败处理。
	if err := s.do(ctx, "capability", s.t.Command, func() error {
		if caps = s.client.Caps(); caps == nil {
			return errors.New("capabilities unavailable")
		}
		return nil
	}); err != nil {
		return err
	}
	if !caps.Has(imap.CapIMAP4rev1) || caps.Has(imap.CapLoginDisabled) {
		return ErrCapability
	}
	if err := s.do(ctx, "login", s.t.Command, func() error {
		return s.client.Login(username, password).Wait()
	}); err != nil {
		// 任何带标签的 NO 都按认证失败处理。本包不保留服务器响应文本与响应码，无法区分授权码错误与「暂时不可用」等原因；
		// 而调用方对认证失败的处置（暂停 AuthPause 再重试）在两种原因下都安全，反过来把认证失败当作临时错误会反复登录。
		if isNo(err) {
			return ErrAuthFailed
		}
		return err
	}
	// 显式发送 CAPABILITY：LOGIN 的响应不带能力列表时，库在后台重新请求，Caps 可能读到登录前的旧值。
	var after imap.CapSet
	if err := s.do(ctx, "capability", s.t.Command, func() (err error) {
		after, err = s.client.Capability().Wait()
		return err
	}); err != nil {
		return err
	}
	s.caps = after
	return nil
}

// tlsConfig 构造隐式 TLS 的客户端配置：校验证书链与主机名，最低 TLS 1.2；调用方只能影响根证书池。
func tlsConfig(cfg Config) *tls.Config {
	return &tls.Config{ServerName: cfg.Host, RootCAs: cfg.RootCAs, MinVersion: tls.VersionTLS12}
}

// signal 以非阻塞方式在 ch 中留下一个信号；已有信号时直接丢弃（一个待处理的信号已足以触发补扫），从不阻塞。
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// do 在期限 d 内执行一步：fn 在独立 goroutine 中运行，期限到、ctx 结束或连接关闭时关闭连接并返回 ErrTimeout、ctx 的错误或 ErrClosed。
// 服务器带标签的 NO/BAD 返回 *rejectedError，连接仍可用；其他失败都来自连接层（库在读写失败时关闭连接），返回 ErrClosed。
// 期限到期后 fn 仍在运行，直到连接关闭使它返回；它只写自己的局部变量，调用方在 do 返回错误后不读取这些变量。
func (s *Session) do(ctx context.Context, step string, d time.Duration, fn func() error) error {
	if s.closed {
		return fmt.Errorf("%w (%s)", ErrClosed, step)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("imap: %s: %w", step, err)
	}
	done := make(chan error, 1)
	go func() { done <- fn() }()
	timer := time.NewTimer(d)
	defer timer.Stop()
	var err error
	select {
	case err = <-done:
	case <-timer.C:
		s.shutdown()
		return fmt.Errorf("%w (%s)", ErrTimeout, step)
	case <-ctx.Done():
		s.shutdown()
		return fmt.Errorf("imap: %s: %w", step, ctx.Err())
	case <-s.client.Closed():
		// 库在关闭 Closed 之前已以错误结束全部待完成命令，fn 随即返回。服务器可能回带标签的 NO 后立即断开（例如拒绝 LOGIN），
		// 这时两个分支同时就绪、select 随机选择，所以仍按 fn 的结果分类，认证失败才不会被当成普通断开；至多等待 IdleStop。
		s.shutdown()
		select {
		case err = <-done:
		case <-time.After(s.t.IdleStop):
			return fmt.Errorf("%w (%s)", ErrClosed, step)
		}
	}
	var imapErr *imap.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &imapErr):
		return &rejectedError{step: step, typ: imapErr.Type}
	default:
		s.shutdown()
		return fmt.Errorf("%w (%s)", ErrClosed, step)
	}
}

// shutdown 关闭连接。client.Close 要等解码协程退出，所以在独立 goroutine 中调用它，至多等待 IdleStop，
// 超时即视为已关闭并返回，不让关闭本身再次挂起。
func (s *Session) shutdown() {
	if s.closed {
		return
	}
	s.closed = true
	done := make(chan struct{})
	go func() {
		_ = s.client.Close()
		close(done)
	}()
	timer := time.NewTimer(s.t.IdleStop)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

// sleep 等待 d；期间连接关闭返回 ErrClosed，ctx 结束返回 ctx 的错误。不发送任何命令。
func (s *Session) sleep(ctx context.Context, d time.Duration) error {
	if s.closed {
		return ErrClosed
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.client.Closed():
		s.shutdown()
		return ErrClosed
	}
}

// Capabilities 返回登录后的能力名称（大写，排序），供 L1 记录。
func (s *Session) Capabilities() []string {
	names := make([]string, 0, len(s.caps))
	for c := range s.caps {
		names = append(names, strings.ToUpper(string(c)))
	}
	slices.Sort(names)
	return names
}

// ListFolders 执行 LIST "" "*"，返回文件夹名称，供 L1 记录与确认 Junk 是否存在。
// imapclient 解析 LIST 响应时已把修改版 UTF-7 解码为 UTF-8，EXAMINE 时再编码回去，因此名称可以原样传给 Examine 与 Scan。
func (s *Session) ListFolders(ctx context.Context) ([]string, error) {
	var names []string
	if err := s.do(ctx, "list", s.t.Command, func() error {
		cmd := s.client.List("", "*", nil)
		for data := cmd.Next(); data != nil; data = cmd.Next() {
			names = append(names, data.Mailbox)
		}
		return cmd.Close()
	}); err != nil {
		return nil, err
	}
	return names, nil
}

// Mailbox 是 EXAMINE 返回的文件夹状态。
type Mailbox struct {
	UIDValidity uint32
	UIDNext     uint32
	Messages    uint32
}

// Examine 以只读方式选中 folder 并返回其状态；EXAMINE 被拒绝时返回 ErrNoFolder。L1 用它记录各文件夹的 UIDVALIDITY。
func (s *Session) Examine(ctx context.Context, folder string) (Mailbox, error) {
	var data *imap.SelectData
	if err := s.do(ctx, "examine", s.t.Command, func() (err error) {
		data, err = s.client.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
		return err
	}); err != nil {
		if isNo(err) {
			return Mailbox{}, ErrNoFolder
		}
		return Mailbox{}, err
	}
	return Mailbox{UIDValidity: data.UIDValidity, UIDNext: uint32(data.UIDNext), Messages: data.NumMessages}, nil
}

// Scan 对 folder 执行 EXAMINE；UIDVALIDITY 与 cur 不同则从 UID 1 全量补扫（Reset）。
// 执行 UID SEARCH UID <LastUID+1>:*，丢弃 uid ≤ LastUID 的结果（没有新邮件时服务器仍返回最后一个 UID），
// 取最小的 MaxBatch 个，先 UID FETCH (UID RFC822.SIZE)，再对声明大小不超过 maxMessageSize 的逐封
// UID FETCH (BODY.PEEK[]<0.maxMessageSize+1>)。字面量以流式读取，至多读 maxMessageSize+1 字节：超出即判为 TooLarge、
// 其余部分读出丢弃（受 Fetch 期限约束），因此服务器少报 RFC822.SIZE 或不遵守部分取回时内存仍有上界。
// 已取回正文合计达到 MaxBatchBytes 时本批提前结束并置 More。
//
// 合计上限在取下一封正文之前检查：已取回的正文加下一封声明的大小超过 maxBatchBytes 时本批结束，因此服务器如实报告大小时
// 合计不超过上限；每批至少交付一封，单封大于上限时也能前进。UID SEARCH 返回、但 FETCH 没有返回数据（或没有正文）的邮件
// 已在两条命令之间被删除，跳过它，游标越过它。
func (s *Session) Scan(ctx context.Context, folder string, cur Cursor) (Batch, error) {
	// 排空发生在 EXAMINE 之前：此后到达的 EXISTS 都会留下信号，新邮件要么被本次 UID SEARCH 覆盖，要么让下一次 Idle 立即返回。
	select {
	case <-s.exists:
	default:
	}
	box, err := s.Examine(ctx, folder)
	if err != nil {
		return Batch{}, err
	}
	b := Batch{Folder: folder, UIDValidity: box.UIDValidity}
	last := cur.LastUID
	if cur.UIDValidity != box.UIDValidity {
		b.Reset, last = true, 0
	}
	b.Next = Cursor{UIDValidity: box.UIDValidity, LastUID: last}
	if last == math.MaxUint32 {
		return b, nil // UID 已到上限，不可能再有新邮件；last+1 会回绕成非法的 0
	}
	uids, err := s.search(ctx, last)
	if err != nil {
		return Batch{}, err
	}
	if len(uids) > MaxBatch {
		uids, b.More = uids[:MaxBatch], true
	}
	if len(uids) == 0 {
		return b, nil
	}
	sizes, err := s.sizes(ctx, uids)
	if err != nil {
		return Batch{}, err
	}
	var total int64
	for _, uid := range uids {
		size, ok := sizes[uid]
		switch {
		case !ok:
			// 已被删除，跳过。
		case size > int64(maxMessageSize):
			b.Messages = append(b.Messages, Message{UID: uint32(uid), Size: size, TooLarge: true})
		case len(b.Messages) > 0 && total+size > int64(maxBatchBytes):
			b.More = true
			return b, nil
		default:
			raw, tooLarge, found, err := s.body(ctx, uid)
			if err != nil {
				return Batch{}, err
			}
			if found {
				b.Messages = append(b.Messages, Message{UID: uint32(uid), Size: size, Raw: raw, TooLarge: tooLarge})
				total += int64(len(raw))
			}
		}
		b.Next.LastUID = uint32(uid)
	}
	return b, nil
}

// search 执行 UID SEARCH UID <last+1>:*，返回大于 last 的 UID，升序。
func (s *Session) search(ctx context.Context, last uint32) ([]imap.UID, error) {
	var found []imap.UID
	if err := s.do(ctx, "uid search", s.t.Command, func() error {
		data, err := s.client.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{{{Start: imap.UID(last + 1), Stop: 0}}}}, nil).Wait()
		if err == nil {
			found = data.AllUIDs()
		}
		return err
	}); err != nil {
		return nil, err
	}
	uids := slices.DeleteFunc(found, func(uid imap.UID) bool { return uint32(uid) <= last })
	slices.Sort(uids)
	return uids, nil
}

// sizes 执行 UID FETCH <uids> (UID RFC822.SIZE)，返回各邮件声明的大小；服务器没有返回的 UID 不在结果中。
// 这条命令只取元数据，与逐封取正文同用 Fetch 期限。
func (s *Session) sizes(ctx context.Context, uids []imap.UID) (map[imap.UID]int64, error) {
	sizes := make(map[imap.UID]int64, len(uids))
	if err := s.do(ctx, "uid fetch", s.t.Fetch, func() error {
		cmd := s.client.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{RFC822Size: true})
		for msg := cmd.Next(); msg != nil; msg = cmd.Next() {
			var uid imap.UID
			var size int64
			for item := msg.Next(); item != nil; item = msg.Next() {
				switch item := item.(type) {
				case imapclient.FetchItemDataUID:
					uid = item.UID
				case imapclient.FetchItemDataRFC822Size:
					size = item.Size
				}
			}
			if uid != 0 {
				sizes[uid] = size
			}
		}
		return cmd.Close()
	}); err != nil {
		return nil, err
	}
	return sizes, nil
}

// body 执行 UID FETCH <uid> (BODY.PEEK[]<0.maxMessageSize+1>)，流式读取至多 maxMessageSize+1 字节：
// 超出 maxMessageSize 时 tooLarge 为 true、raw 为 nil，其余部分由库读出丢弃。found 为 false 表示服务器没有返回正文。
func (s *Session) body(ctx context.Context, uid imap.UID) (raw []byte, tooLarge, found bool, err error) {
	limit := int64(maxMessageSize)
	var data []byte
	var large, ok bool
	err = s.do(ctx, "uid fetch", s.t.Fetch, func() error {
		cmd := s.client.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{
			BodySection: []*imap.FetchItemBodySection{{Peek: true, Partial: &imap.SectionPartial{Offset: 0, Size: limit + 1}}},
		})
		for msg := cmd.Next(); msg != nil; msg = cmd.Next() {
			for item := msg.Next(); item != nil; item = msg.Next() {
				section, isBody := item.(imapclient.FetchItemDataBodySection)
				if !isBody || section.Literal == nil || ok {
					continue
				}
				ok = true
				var err error
				if data, err = io.ReadAll(io.LimitReader(section.Literal, limit+1)); err != nil {
					// 读取失败说明连接已断开。此时不能再调用 Next：它会丢弃同一字面量而再次读取，与解码协程争用读缓冲。
					// 连接断开后解码协程自行退出并结束本命令，不会因为没人取走数据而阻塞。
					return err
				}
				// 读到的字节少于字面量声明的长度（且未到上限），说明连接在字面量中途断开：服务器以 close_notify 正常关闭时，
				// go-imap 的字面量读取器把 io.EOF 当作字面量结束，ReadAll 不报错，解码协程也已被放行、继续读同一个读缓冲。
				// 这与读取失败是同一种情况，同样不能再调用 Next，只能按连接断开结束本命令。
				if int64(len(data)) < min(section.Literal.Size(), limit+1) {
					return io.ErrUnexpectedEOF
				}
				if int64(len(data)) > limit {
					data, large = nil, true
				}
			}
		}
		return cmd.Close()
	})
	if err != nil {
		return nil, false, false, err
	}
	return data, large, ok, nil
}

// Idle 若发现自上次 Scan 开始以来已收到 EXISTS（例如补扫的 UID FETCH 进行中新邮件到达；以 EXISTS 通道中是否有信号判断），
// 不发送 IDLE，取走信号并立即返回 true。
// 否则在已 EXAMINE 的文件夹上执行 IDLE，直到收到 EXISTS（返回 true）、达到 IdleMax（返回 false）或 ctx 结束；
// 然后发出 DONE 并在 IdleStop 内等待完成，超时即关闭连接并返回 ErrTimeout。服务器未公告 IDLE 时返回 ErrCapability。
// ctx 结束时不再发送 DONE，直接关闭连接并返回 ctx 的错误。
func (s *Session) Idle(ctx context.Context) (newMail bool, err error) {
	if s.closed {
		return false, ErrClosed
	}
	if !s.caps.Has(imap.CapIdle) {
		return false, ErrCapability
	}
	select {
	case <-s.exists:
		return true, nil
	default:
	}
	var idle *imapclient.IdleCommand
	if err := s.do(ctx, "idle", s.t.IdleAck, func() (err error) {
		idle, err = s.client.Idle()
		return err
	}); err != nil {
		return false, err
	}
	timer := time.NewTimer(s.t.IdleMax)
	defer timer.Stop()
	select {
	case <-s.exists:
		newMail = true
	case <-timer.C:
	case <-ctx.Done():
		s.shutdown()
		return false, fmt.Errorf("imap: idle: %w", ctx.Err())
	case <-s.client.Closed():
		s.shutdown()
		return false, fmt.Errorf("%w (idle)", ErrClosed)
	}
	if err := s.do(ctx, "done", s.t.IdleStop, func() error {
		if err := idle.Close(); err != nil {
			return err
		}
		return idle.Wait()
	}); err != nil {
		return false, err
	}
	return newMail, nil
}

// Close 在 Command 期限内尽力发送 LOGOUT，然后关闭连接；连接已关闭时什么也不做。总是返回 nil。
func (s *Session) Close() error {
	if s.closed {
		return nil
	}
	_ = s.do(context.Background(), "logout", s.t.Command, func() error { return s.client.Logout().Wait() })
	s.shutdown()
	return nil
}
