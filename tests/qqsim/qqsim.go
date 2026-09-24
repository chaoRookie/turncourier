// Package qqsim 是离线的 QQ 邮箱行为模拟器，供 internal/app、internal/cli 与 tests/e2e 的测试共用。它在本机回环地址的两个随机端口上
// 以隐式 TLS 提供 SMTP（go-smtp 的服务端，AUTH PLAIN 用 go-sasl 的服务端实现）与 IMAP（go-imap/v2 的 imapserver 加 imapmemserver），
// 证书由启动时生成的自签 CA 签发，调用方经 RootCAs 信任它。
//
// 它按 L1 实测复现 QQ 的行为：一个机器人账户，文件夹 INBOX、Junk 与「已发送」（Sent Messages）共用同一个 UIDVALIDITY；SMTP 收下邮件时
// 把 Message-Id 改写为 <tencent_…@qq.com>、原 ID 写入 X-OQ-MSGID，改写后的副本追加到「已发送」、交给 Delivered 通道（收件人一侧的副本），
// DATA 的 250 响应与 QQ 一样不含任何 ID。测试辅助 Reply 按 QQ 邮箱 App 的结构合成回复，Deliver 把任意字节放进文件夹；SetFaults 注入
// 4b 契约列出的各种故障，DisconnectAll 立即断开当前全部连接（断网恢复用）。
//
// 本包是测试基础设施，不进入产品二进制；不联网、不读写文件，也不写任何日志：两个服务端的调试输出会带出授权码，一律关闭。
// 错误文本不回显授权码、地址、主题与正文。
package qqsim

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"

	"github.com/chaoRookie/turncourier/internal/mail/imap"
	"github.com/chaoRookie/turncourier/internal/mail/smtp"
	mailfixture "github.com/chaoRookie/turncourier/tests/fixtures/mail"
)

const (
	// host 是模拟器监听的地址：本机回环，端口随机。证书的 SAN 含它与 localhost。
	host = "127.0.0.1"
	// delim 是文件夹层级的分隔符，与 imapmemserver 相同。
	delim = '/'
	// queuedText 是 DATA 结束标记之后的 250 响应文本，与 L1 实测的 QQ 响应相同，不含任何 ID。
	queuedText = "OK: queued as."
	// injectedText 是注入的失败回复的文本。L1 中 QQ 的拒绝回复不带增强状态码，模拟器的回复同样不带。
	injectedText = "qqsim: injected failure"
	// oqHeader 是 QQ 在「已发送」副本中保存原 Message-ID 的头名（L1 实测）。
	oqHeader = "X-OQ-MSGID"
	// maxCommandLine 是 SMTP 连接包装在命令模式下一次交出的最长一段；更长的行分几次交出，行长上限由 go-smtp 自己执行。
	maxCommandLine = 4096
	// maxDepth 是 Reply 在原信中查找纯文本部件时 MIME 嵌套深度的上限。
	maxDepth = 8
	// maxMessageBytes 是 SMTP 接受的单封邮件上限，远大于产品的 4 MiB。
	maxMessageBytes = 32 << 20
	// replySample 是 Reply 使用的 mailfixture 样本：L1 实测的 QQ 邮箱 App 回复结构。
	replySample = "qq-app-reply"
)

var (
	// errClosed 表示模拟器已经关闭。
	errClosed = errors.New("qqsim: the simulator is closed")
	// errDropped 是注入「收到结束标记后不回响应即断开」时 Data 的返回值；连接此时已关闭，go-smtp 写不出任何回复。
	errDropped = errors.New("qqsim: connection dropped after the end of data")
	// errReadOnly 是修改邮箱的 IMAP 命令得到的 NO 回复：产品只读邮箱，模拟器的记录也因此与 imapmemserver 始终一致。
	errReadOnly = &goimap.Error{Type: goimap.StatusResponseTypeNo, Text: "qqsim: the simulated mailbox is read-only for clients"}
	// errUnreadable 表示 Reply 无法解析收件人副本（头部、主题的编码词、MIME 结构或传输编码）。
	errUnreadable = errors.New("qqsim: cannot read the delivered copy")
	// errNoPlain 表示收件人副本中没有可引用的非附件 text/plain 部件。
	errNoPlain = errors.New("qqsim: the delivered copy has no text/plain part to quote")
	// errCharset 表示要引用的纯文本部件不是 utf-8 或 us-ascii，或者解码后不是合法的 UTF-8。
	errCharset = errors.New("qqsim: the quoted text/plain part is not utf-8")
)

// Options 是模拟器的参数。
type Options struct {
	Address  string           // 机器人地址：IMAP 与 SMTP 的登录名，也是唯一接受的信封发件人
	Password string           // 授权码，由测试在运行时构造；可打印 ASCII，不含空白
	Now      func() time.Time // 可为 nil（取 time.Now）；「已发送」副本的延迟与 INTERNALDATE 按它计时，测试注入与被测代码相同的时钟
}

// Drop 描述「收到结束标记后不回响应即断开」时服务器是否已经收下邮件。客户端两种情况下都只能报告结果不确定（smtp.ErrUncertain）。
type Drop int

const (
	// NoDrop 是零值：照常回复。
	NoDrop Drop = iota
	// DropAccepted 先照常收下邮件（改写 ID、按设置写「已发送」副本、交给收件人），再不回响应即断开：实际已送达。
	DropAccepted
	// DropDiscarded 丢弃邮件后不回响应即断开：实际未送达，「已发送」与收件人都没有它。
	DropDiscarded
)

// Faults 是当前注入的故障；零值表示没有故障。SetFaults 整体替换它，对之后的每一步生效。状态码不为 0 时该步以它回复，
// 回复不带增强状态码（与 L1 实测的 QQ 相同）：
//
//   - AuthCode：AUTH 失败。5xx 时客户端得到 smtp.ErrAuth，4xx 只是暂时失败；
//   - MailCode、RcptCode：MAIL FROM、RCPT TO 被拒；
//   - DataCode：DATA 命令被拒（354 之前，邮件还没开始传）；
//   - SubmissionCode：结束标记之后被拒，邮件不被收下（550 即 L1 实测的同域收件人不存在）。
type Faults struct {
	AuthCode       int
	MailCode       int
	RcptCode       int
	DataCode       int
	SubmissionCode int
	// Drop 不为 NoDrop 时，收到结束标记后不回任何响应即断开连接；优先于 SubmissionCode。
	Drop Drop
	// SentDelay 为正时，收下的邮件的「已发送」副本在这么久之后（按 Options.Now）才写入：到期后下一次 EXAMINE 或 STATUS「已发送」、
	// 或调用 Messages 时写入；ReleaseSent 不等到期立即写入。延迟按收下时的设置计算。
	SentDelay time.Duration
	// NoSentCopy 为真时不写「已发送」副本（模拟关闭「保存到已发送」），优先于 SentDelay；邮件照常交给收件人。
	NoSentCopy bool
	// IMAPLoginFails 为真时 IMAP 的 LOGIN 一律以 NO [AUTHENTICATIONFAILED] 失败（客户端得到 imap.ErrAuthFailed）。
	IMAPLoginFails bool
	// SentExamineFails 为真时对「已发送」的 SELECT 与 EXAMINE 以 NO [UNAVAILABLE] 拒绝（客户端得到 imap.ErrNoFolder），连接仍可用。
	SentExamineFails bool
	// SentHidden 为真时 LIST 不列出「已发送」；只影响 LIST，EXAMINE 照常（要它也失败，同时设 SentExamineFails）。
	SentHidden bool
}

// Stats 是模拟器迄今的计数，供测试断言「没有登录」「没有尝试认证」这类否定性质。
type Stats struct {
	IMAPLogins int // IMAP LOGIN 尝试次数，含失败
	SMTPAuths  int // SMTP AUTH PLAIN 尝试次数，含失败（其他机制在到达认证之前就被拒绝，不计入）
	Accepted   int // 收下的邮件数（含 DropAccepted）
}

// Delivered 是 SMTP 收下的一封邮件在收件人一侧的副本：Message-Id 已改写，X-OQ-MSGID 保留原 ID，字节与「已发送」副本相同。
type Delivered struct {
	From       string   // 信封发件人（机器人地址）
	To         []string // 信封收件人
	MessageID  string   // 改写后的实际投递 ID，形如 <tencent_…@qq.com>
	OriginalID string   // 原信的 Message-Id（即 X-OQ-MSGID 的取值）；原信没有时为空
	Raw        []byte   // 改写后的完整邮件字节
}

// Message 是文件夹中的一封邮件：UID 与原始字节。
type Message struct {
	UID uint32
	Raw []byte
}

// pendingCopy 是一份延迟写入的「已发送」副本：到期时刻（按 Options.Now）与改写后的字节。
type pendingCopy struct {
	due time.Time
	raw []byte
}

// Server 是运行中的模拟器。方法可并发调用。
type Server struct {
	addr        string
	password    string
	now         func() time.Time
	pool        *x509.CertPool
	uidValidity uint32 // 三个文件夹共用的 UIDVALIDITY（L1 实测相同），非零
	imapPort    int
	smtpPort    int

	user       *imapmemserver.User
	imapServer *imapserver.Server
	smtpServer *gosmtp.Server
	listeners  []net.Listener // 两个 TCP 监听器；Close 自己也关闭它们，见 Close 的说明

	delivered chan Delivered // Delivered 返回它；forward 是唯一的发送方，退出时关闭它
	notify    chan struct{}  // 容量为 1：queue 有新成员时留下信号，唤醒 forward
	done      chan struct{}  // Close 时关闭
	wg        sync.WaitGroup // 两个 Serve 协程与 forward

	mu      sync.Mutex // 保护以下字段
	faults  Faults
	stats   Stats
	queue   []Delivered          // 尚未交给 Delivered 通道的收件人副本，不设上限
	pending []pendingCopy        // 尚未写入的延迟副本，按收下的顺序
	folders map[string][]Message // 各文件夹的邮件，与 imapmemserver 中的一致（客户端的写命令都被拒绝）
	conns   map[*trackedConn]struct{}
	closed  bool
}

// 编译期确认两个会话类型满足服务端要求的接口。
var (
	_ imapserver.Session = (*imapSession)(nil)
	_ gosmtp.AuthSession = (*smtpSession)(nil)
)

// New 校验参数，生成自签 CA 与服务器证书，在 127.0.0.1 的两个随机端口上启动隐式 TLS 的 SMTP 与 IMAP 服务。用完须调用 Close。
// 地址与授权码不合规、证书或监听失败时返回错误，错误文本不回显地址与授权码。
func New(opts Options) (*Server, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	cert, pool, err := newCertificates()
	if err != nil {
		return nil, err
	}
	imapLn, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return nil, fmt.Errorf("qqsim: listen: %w", err)
	}
	smtpLn, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		_ = imapLn.Close()
		return nil, fmt.Errorf("qqsim: listen: %w", err)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	s := &Server{
		addr: opts.Address, password: opts.Password, now: now, pool: pool, uidValidity: randomUIDValidity(),
		imapPort: imapLn.Addr().(*net.TCPAddr).Port, smtpPort: smtpLn.Addr().(*net.TCPAddr).Port, listeners: []net.Listener{imapLn, smtpLn},
		user:      imapmemserver.NewUser(opts.Address, opts.Password),
		delivered: make(chan Delivered), notify: make(chan struct{}, 1), done: make(chan struct{}),
		folders: map[string][]Message{}, conns: map[*trackedConn]struct{}{},
	}
	for _, name := range []string{imap.FolderInbox, imap.FolderJunk, imap.FolderSent} {
		// 新账户中没有文件夹，不会重名，Create 不会失败。
		_ = s.user.Create(name, nil)
		s.folders[name] = nil
	}
	// 不设 NextProtos：imapclient 协商 ALPN "imap"，服务端声明其他协议会让握手失败。
	serverTLS := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	s.imapServer = imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &imapSession{srv: s}, nil, nil
		},
		Caps:   goimap.CapSet{goimap.CapIMAP4rev1: {}},
		Logger: log.New(io.Discard, "", 0),
	})
	s.smtpServer = gosmtp.NewServer(smtpBackend{srv: s})
	s.smtpServer.Domain = "smtp.example.invalid"
	s.smtpServer.MaxMessageBytes = maxMessageBytes
	// go-smtp 只把 *tls.Conn 当作加密连接，而它看到的是包在 TLS 之外的 smtpConn；传输层仍然只有隐式 TLS，没有明文入口。
	s.smtpServer.AllowInsecureAuth = true
	s.smtpServer.ErrorLog = log.New(io.Discard, "", 0)
	s.wg.Add(3)
	go func() {
		defer s.wg.Done()
		_ = s.imapServer.Serve(tls.NewListener(&trackingListener{Listener: imapLn, srv: s}, serverTLS))
	}()
	go func() {
		defer s.wg.Done()
		_ = s.smtpServer.Serve(&smtpListener{Listener: &trackingListener{Listener: smtpLn, srv: s}, srv: s, config: serverTLS})
	}()
	go s.forward()
	return s, nil
}

// validate 检查机器人地址与授权码：地址非空、含 @、不含空白与控制字符；授权码非空，只由可打印 ASCII 组成且不含空白
// （与产品的要求相同）。错误文本不回显它们。
func (o Options) validate() error {
	invalid := func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }
	if o.Address == "" || !strings.Contains(o.Address, "@") || strings.ContainsFunc(o.Address, invalid) {
		return errors.New("qqsim: the bot address must be non-empty, contain @ and have no white space or control characters")
	}
	if o.Password == "" || strings.ContainsFunc(o.Password, func(r rune) bool { return r <= ' ' || r > '~' }) {
		return errors.New("qqsim: the authorization code must be non-empty printable ASCII without white space")
	}
	return nil
}

// newCertificates 生成一张自签 CA 与由它签发的服务器证书（ECDSA P-256，SAN 为 127.0.0.1 与 localhost），返回服务器证书与只含该 CA 的
// 证书池。私钥只在内存中，不落盘。有效期按真实时间计（客户端的 TLS 校验用真实时间，与注入的时钟无关），覆盖测试运行的时长。
func newCertificates() (tls.Certificate, *x509.CertPool, error) {
	caKey, err1 := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafKey, err2 := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err := errors.Join(err1, err2); err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("qqsim: generate keys: %w", err)
	}
	notBefore, notAfter := time.Now().Add(-time.Hour), time.Now().Add(7*24*time.Hour)
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "qqsim test CA"}, NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("qqsim: create the CA certificate: %w", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("qqsim: parse the CA certificate: %w", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: host}, NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP(host)}, DNSNames: []string{"localhost"},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("qqsim: create the server certificate: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}, pool, nil
}

// randomUIDValidity 返回一个随机的非零 UIDVALIDITY（不超过 2^31，照顾把它当作有符号数的实现）。
func randomUIDValidity() uint32 {
	var b [4]byte
	// crypto/rand.Read 不会返回错误：取随机数失败时直接终止进程。
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])>>1 | 1
}

// newMessageID 返回 L1 实测形状的 QQ Message-ID：<tencent_ 加 40 个大写十六进制字符 @qq.com>，十六进制取自 crypto/rand。
func newMessageID() string {
	var b [20]byte
	// crypto/rand.Read 不会返回错误：取随机数失败时直接终止进程。
	_, _ = rand.Read(b[:])
	return "<tencent_" + strings.ToUpper(hex.EncodeToString(b[:])) + "@qq.com>"
}

// Host 返回模拟器监听的地址 127.0.0.1。
func (s *Server) Host() string {
	return host
}

// IMAPPort 返回 IMAP 服务（隐式 TLS）的端口。
func (s *Server) IMAPPort() int {
	return s.imapPort
}

// SMTPPort 返回 SMTP 服务（隐式 TLS）的端口。
func (s *Server) SMTPPort() int {
	return s.smtpPort
}

// RootCAs 返回只含模拟器 CA 的证书池；调用方把它放进客户端的 RootCAs，不要修改它。
func (s *Server) RootCAs() *x509.CertPool {
	return s.pool
}

// IMAPConfig 返回连接模拟器 IMAP 服务的 imap.Config：登录名为机器人地址，期限取默认值（调用方可改）。
func (s *Server) IMAPConfig() imap.Config {
	return imap.Config{Host: host, Port: s.imapPort, Username: s.addr, RootCAs: s.pool}
}

// SMTPConfig 返回连接模拟器 SMTP 服务的 smtp.Config：登录名为机器人地址，期限取默认值（调用方可改）。
func (s *Server) SMTPConfig() smtp.Config {
	return smtp.Config{Host: host, Port: s.smtpPort, Username: s.addr, RootCAs: s.pool}
}

// Delivered 返回收件人副本的通道：SMTP 每收下一封邮件，按收下的顺序交出一份。排队不设上限，测试不读取也不会阻塞 SMTP 会话；
// Close 之后通道关闭，尚未交出的副本丢弃。
func (s *Server) Delivered() <-chan Delivered {
	return s.delivered
}

// SetFaults 整体替换当前注入的故障（零值表示没有故障），对之后的每一步生效；已经在延迟中的「已发送」副本不受影响。
// 状态码只接受 400–599，延迟不能为负，Drop 只能取已定义的值；不合法时返回错误，原有故障不变。
func (s *Server) SetFaults(f Faults) error {
	for _, code := range []int{f.AuthCode, f.MailCode, f.RcptCode, f.DataCode, f.SubmissionCode} {
		if code != 0 && (code < 400 || code > 599) {
			return errors.New("qqsim: fault reply codes must be 4xx or 5xx")
		}
	}
	switch {
	case f.SentDelay < 0:
		return errors.New("qqsim: SentDelay must not be negative")
	case f.Drop < NoDrop || f.Drop > DropDiscarded:
		return errors.New("qqsim: unknown Drop value")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = f
	return nil
}

// ClearFaults 清除全部故障，等同于 SetFaults(Faults{})。
func (s *Server) ClearFaults() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = Faults{}
}

// currentFaults 返回当前注入的故障。
func (s *Server) currentFaults() Faults {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.faults
}

// countLogin 计入一次 IMAP 登录尝试并返回当前故障。
func (s *Server) countLogin() Faults {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.IMAPLogins++
	return s.faults
}

// countAuth 计入一次 SMTP 认证尝试并返回当前故障。
func (s *Server) countAuth() Faults {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.SMTPAuths++
	return s.faults
}

// Stats 返回迄今的计数。
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Deliver 把任意字节原样追加到文件夹，返回分配的 UID：通常是 INBOX 或 Junk（模拟到达的来信），也可以向「已发送」放入历史或外来的
// 副本。在该文件夹上 IDLE 的连接会收到 EXISTS。文件夹不存在或模拟器已关闭时返回错误。
//
// Messages 与 RFC822.SIZE 总是原样的字节；IMAP 取回的 BODY[] 与 BODY[HEADER] 则经 imapmemserver 重新解析头部后写出：头部能被
// go-message 解析时，各头部行统一为 CRLF 换行、头部之后是一个 CRLF 空行，其余字节不变，所以 CRLF 换行的正常邮件逐字节相同（头部中
// 的非法 UTF-8、超长的行、<> 这样的 Message-ID 都原样保留）；头部无法解析时（某行没有冒号、字段名含非法字符、第一行以空白开头）
// 取回的是空字节，产品的解析器会把它判为畸形来信。
func (s *Server) Deliver(folder string, raw []byte) (uint32, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, errClosed
	}
	return s.appendLocked(folder, raw, now)
}

// appendLocked 把 raw 的副本追加到文件夹（imapmemserver 与模拟器的记录各存一份），INTERNALDATE 取 at，返回 UID。调用方持有 s.mu。
func (s *Server) appendLocked(folder string, raw []byte, at time.Time) (uint32, error) {
	data, err := s.user.Append(folder, bytes.NewReader(raw), &goimap.AppendOptions{Time: at})
	if err != nil {
		return 0, fmt.Errorf("qqsim: folder %q does not exist", folder)
	}
	uid := uint32(data.UID)
	s.folders[folder] = append(s.folders[folder], Message{UID: uid, Raw: bytes.Clone(raw)})
	return uid, nil
}

// Messages 返回文件夹中的全部邮件，按 UID 升序，字节是副本；取「已发送」时先写入到期的延迟副本。文件夹不存在时返回错误。
func (s *Server) Messages(folder string) ([]Message, error) {
	if folder == imap.FolderSent {
		s.release(false)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.folders[folder]
	if !ok {
		return nil, fmt.Errorf("qqsim: folder %q does not exist", folder)
	}
	out := make([]Message, len(stored))
	for i, m := range stored {
		out[i] = Message{UID: m.UID, Raw: bytes.Clone(m.Raw)}
	}
	return out, nil
}

// ReleaseSent 不看时钟，立即把全部延迟中的「已发送」副本按收下的顺序写入，返回写入的份数；测试以它代替等待。
func (s *Server) ReleaseSent() int {
	return s.release(true)
}

// release 把到期的（all 为真时全部）延迟副本按收下的顺序写入「已发送」，返回写入的份数。
func (s *Server) release(all bool) int {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.pending[:0]
	n := 0
	for _, p := range s.pending {
		if !all && now.Before(p.due) {
			kept = append(kept, p)
			continue
		}
		// 「已发送」总是存在，追加不会失败。
		_, _ = s.appendLocked(imap.FolderSent, p.raw, now)
		n++
	}
	s.pending = kept
	return n
}

// accept 按 L1 实测的 QQ 行为收下一封邮件：改写 Message-Id、原 ID 写入 X-OQ-MSGID，按当前故障把副本写入「已发送」
// （立即、延迟或不写），再把收件人一侧的副本排进 Delivered。模拟器已关闭时丢弃。
func (s *Server) accept(from string, to []string, raw []byte) {
	id := newMessageID()
	rewritten, original := rewriteID(raw, id)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.stats.Accepted++
	switch f := s.faults; {
	case f.NoSentCopy:
	case f.SentDelay > 0:
		s.pending = append(s.pending, pendingCopy{due: now.Add(f.SentDelay), raw: rewritten})
	default:
		// 「已发送」总是存在，追加不会失败。
		_, _ = s.appendLocked(imap.FolderSent, rewritten, now)
	}
	s.queue = append(s.queue, Delivered{From: from, To: slices.Clone(to), MessageID: id, OriginalID: original, Raw: bytes.Clone(rewritten)})
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// forward 把排队的收件人副本依次交给 Delivered 通道。它是通道唯一的发送方；模拟器关闭时退出并关闭通道，尚未交出的副本随之丢弃。
func (s *Server) forward() {
	defer s.wg.Done()
	defer close(s.delivered)
	for {
		s.mu.Lock()
		ready := len(s.queue) > 0
		var next Delivered
		if ready {
			next = s.queue[0]
		}
		s.mu.Unlock()
		if !ready {
			select {
			case <-s.notify:
				continue
			case <-s.done:
				return
			}
		}
		select {
		case s.delivered <- next:
			s.mu.Lock()
			s.queue[0] = Delivered{}
			s.queue = s.queue[1:]
			s.mu.Unlock()
		case <-s.done:
			return
		}
	}
}

// DisconnectAll 立即关闭当前全部 IMAP 与 SMTP 连接（直接关闭底层 TCP 连接，不发 BYE 或 421），返回关闭的连接数；模拟断网，
// 之后的新连接照常接受。
func (s *Server) DisconnectAll() int {
	s.mu.Lock()
	conns := make([]*trackedConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	return len(conns)
}

// Close 关闭两个服务与全部连接，等待服务协程退出；Delivered 通道随后关闭，尚未交出的副本丢弃。可重复调用，之后的调用直接返回 nil。
// 两个 TCP 监听器由这里再关闭一次：go-smtp 的 Serve 在开始时才登记监听器，New 之后立即 Close 时它还没有登记，Server.Close 关不到它，
// Serve 就会永远阻塞在 Accept 上（imapserver 的 Serve 会先检查是否已关闭，没有这个问题）。重复关闭的错误忽略。
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.done)
	s.mu.Unlock()
	err := errors.Join(s.imapServer.Close(), s.smtpServer.Close())
	for _, l := range s.listeners {
		_ = l.Close()
	}
	s.DisconnectAll()
	s.wg.Wait()
	return err
}

// track 登记一个新接受的连接；模拟器已关闭时改为立即关闭它，调用方的第一次读写随即失败。
func (s *Server) track(c *trackedConn) {
	s.mu.Lock()
	closed := s.closed
	if !closed {
		s.conns[c] = struct{}{}
	}
	s.mu.Unlock()
	if closed {
		_ = c.Close()
	}
}

// untrack 注销一个已关闭的连接。
func (s *Server) untrack(c *trackedConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, c)
}

// trackingListener 包在 TCP 监听器外，登记接受的每个连接，供 DisconnectAll 与 Close 立即断开。
type trackingListener struct {
	net.Listener
	srv *Server
}

// Accept 接受一个 TCP 连接并登记。
func (l *trackingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tc := &trackedConn{Conn: c, srv: l.srv}
	l.srv.track(tc)
	return tc, nil
}

// trackedConn 是登记过的 TCP 连接；关闭时注销。TLS 在它之上进行，关闭它就断开了整条连接。
type trackedConn struct {
	net.Conn
	srv  *Server
	once sync.Once
}

// Close 注销并关闭连接；可重复调用。
func (c *trackedConn) Close() error {
	c.once.Do(func() { c.srv.untrack(c) })
	return c.Conn.Close()
}

// smtpListener 在登记过的 TCP 连接上建立隐式 TLS 的服务端（握手在第一次读写时进行），再包上 smtpConn 交给 go-smtp。
type smtpListener struct {
	net.Listener
	srv    *Server
	config *tls.Config
}

// Accept 接受一个连接，包上 TLS 与 smtpConn。
func (l *smtpListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &smtpConn{Conn: tls.Server(c, l.config), srv: l.srv}, nil
}

// smtpConn 是 go-smtp 服务端看到的连接：在隐式 TLS 连接之上，按行检查命令模式下客户端发来的数据，用来注入「DATA 命令被拒绝」——
// go-smtp 服务端收到 DATA 就回 354，不经过 Session，只能在这一层拦截。接收正文期间（inData，由 smtpSession.Data 设置）原样转发，
// 不做检查。Read 只由 go-smtp 为这条连接开的处理协程调用，与 Session 的方法在同一协程中依次执行，pending 因此不需要加锁。
type smtpConn struct {
	net.Conn // 服务端的 *tls.Conn
	srv      *Server
	inData   atomic.Bool // 354 之后、结束标记之前
	pending  []byte      // 已读到、尚未交给 go-smtp 的数据
}

// Read 交出客户端发来的数据：接收正文时原样读取；命令模式下逐行交出，注入了 DataCode 时把恰为 DATA 的命令行换成直接写给客户端的
// 错误回复，不交给 go-smtp（它随后读到的是客户端的下一条命令）。连接在行中途结束时先交出残行，下一次再报告错误。
func (c *smtpConn) Read(p []byte) (int, error) {
	for len(c.pending) == 0 {
		if c.inData.Load() {
			return c.Conn.Read(p)
		}
		line, err := c.readLine()
		if c.rejectData(line) {
			line = nil
		}
		c.pending = line
		if len(line) == 0 && err != nil {
			return 0, err
		}
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

// readLine 逐字节读到行尾（含 LF）、maxCommandLine 字节或读取出错为止，返回已读到的字节与错误。逐字节读取只发生在命令模式，
// 命令都很短；TLS 连接自带缓冲，不会因此逐字节读网络。
func (c *smtpConn) readLine() ([]byte, error) {
	var line []byte
	var b [1]byte
	for len(line) < maxCommandLine {
		n, err := c.Conn.Read(b[:])
		line = append(line, b[:n]...)
		if err != nil {
			return line, err
		}
		if n == 1 && b[0] == '\n' {
			break
		}
	}
	return line, nil
}

// rejectData 在注入了 DataCode 且 line 恰为 DATA 命令时，直接向客户端写出该状态码的回复并返回 true。go-smtp 每条回复都立即 flush，
// 此时没有它尚未写出的数据，直接写不会与它的回复交错。
func (c *smtpConn) rejectData(line []byte) bool {
	code := c.srv.currentFaults().DataCode
	if code == 0 || !strings.EqualFold(strings.TrimRight(string(line), "\r\n"), "DATA") {
		return false
	}
	_, _ = fmt.Fprintf(c.Conn, "%d %s\r\n", code, injectedText)
	return true
}

// smtpBackend 为每条 SMTP 连接创建会话。
type smtpBackend struct {
	srv *Server
}

// NewSession 在 EHLO 时创建会话；连接一定来自 smtpListener，是 *smtpConn。
func (b smtpBackend) NewSession(c *gosmtp.Conn) (gosmtp.Session, error) {
	return &smtpSession{srv: b.srv, conn: c.Conn().(*smtpConn)}, nil
}

// smtpSession 是一条 SMTP 连接上的会话：记录是否已认证与本封邮件的信封，按当前故障回复各步骤。方法由 go-smtp 在该连接的处理协程中
// 依次调用，不会并发。
type smtpSession struct {
	srv    *Server
	conn   *smtpConn
	authed bool
	from   string
	to     []string
}

// reply 构造一条不带增强状态码的 SMTP 回复；go-smtp 把会话方法返回的 *SMTPError 原样写出，250 也借此带上 QQ 的文本。
func reply(code int, text string) *gosmtp.SMTPError {
	return &gosmtp.SMTPError{Code: code, EnhancedCode: gosmtp.NoEnhancedCode, Message: text}
}

// AuthMechanisms 只提供 PLAIN：产品只用 AUTH PLAIN。
func (s *smtpSession) AuthMechanisms() []string {
	return []string{sasl.Plain}
}

// Auth 返回 PLAIN 的服务端，其他机制以 504 拒绝。每次 PLAIN 尝试计入 Stats.SMTPAuths：注入了 AuthCode 时以它失败；否则授权身份为空或
// 等于用户名、用户名等于机器人地址且授权码一致才成功，其余以 535 失败（QQ 对授权码错误的回复）。
func (s *smtpSession) Auth(mech string) (sasl.Server, error) {
	if mech != sasl.Plain {
		return nil, gosmtp.ErrAuthUnknownMechanism
	}
	return sasl.NewPlainServer(func(identity, username, password string) error {
		f := s.srv.countAuth()
		switch {
		case f.AuthCode != 0:
			return reply(f.AuthCode, injectedText)
		case identity != "" && identity != username, username != s.srv.addr,
			subtle.ConstantTimeCompare([]byte(password), []byte(s.srv.password)) != 1:
			return reply(535, "Login fail. Please enter your authorization code to login")
		}
		s.authed = true
		return nil
	}), nil
}

// Mail 按 QQ 的规矩检查信封发件人：没有认证以 503 拒绝，注入了 MailCode 时以它失败，发件人不等于登录账户以 501 拒绝。
func (s *smtpSession) Mail(from string, _ *gosmtp.MailOptions) error {
	if !s.authed {
		return reply(503, "Error: need EHLO and AUTH first !")
	}
	if code := s.srv.currentFaults().MailCode; code != 0 {
		return reply(code, injectedText)
	}
	if from != s.srv.addr {
		return reply(501, "mail from address must be same as authorization user")
	}
	s.from = from
	return nil
}

// Rcpt 记录收件人；注入了 RcptCode 时以它失败。
func (s *smtpSession) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	if code := s.srv.currentFaults().RcptCode; code != 0 {
		return reply(code, injectedText)
	}
	s.to = append(s.to, to)
	return nil
}

// Data 读完正文（到结束标记），再按当前故障处理：Drop 不回响应即断开（DropAccepted 先照常收下）；SubmissionCode 以它拒绝、不收下；
// 否则收下并回复与 QQ 相同的 250 文本（不含 ID）。
func (s *smtpSession) Data(r io.Reader) error {
	s.conn.inData.Store(true)
	raw, err := io.ReadAll(r)
	s.conn.inData.Store(false)
	if err != nil {
		return err
	}
	f := s.srv.currentFaults()
	switch {
	case f.Drop != NoDrop:
		if f.Drop == DropAccepted {
			s.srv.accept(s.from, s.to, raw)
		}
		_ = s.conn.Close()
		return errDropped
	case f.SubmissionCode != 0:
		return reply(f.SubmissionCode, injectedText)
	}
	s.srv.accept(s.from, s.to, raw)
	return reply(250, queuedText)
}

// Reset 放弃当前邮件的信封（RSET、每封邮件之后与重新 EHLO 时调用），认证状态保留。
func (s *smtpSession) Reset() {
	s.from, s.to = "", nil
}

// Logout 在连接关闭时调用，没有要释放的资源。
func (s *smtpSession) Logout() error {
	return nil
}

// imapSession 是一条 IMAP 连接的会话：登录之前 UserSession 为空，登录之后把读邮箱的操作交给 imapmemserver。它在 LOGIN、SELECT/EXAMINE
// 与 LIST 上注入故障，把各文件夹的 UIDVALIDITY 统一为模拟器的取值（L1 实测三者相同），并拒绝一切修改邮箱的命令。imapserver 在同一
// 连接上依次调用这些方法，不会并发。
type imapSession struct {
	*imapmemserver.UserSession
	srv *Server
}

// Login 计入 Stats.IMAPLogins；注入了 IMAPLoginFails 时以 NO [AUTHENTICATIONFAILED] 拒绝，否则按账户核对用户名与授权码。
func (s *imapSession) Login(username, password string) error {
	if s.srv.countLogin().IMAPLoginFails {
		return imapserver.ErrAuthFailed
	}
	if err := s.srv.user.Login(username, password); err != nil {
		return err
	}
	s.UserSession = imapmemserver.NewUserSession(s.srv.user)
	return nil
}

// Select 执行 SELECT 与 EXAMINE：注入了 SentExamineFails 时「已发送」以 NO [UNAVAILABLE] 拒绝；选中「已发送」之前先写入到期的延迟副本；
// 返回的 UIDVALIDITY 换成模拟器的统一取值。
func (s *imapSession) Select(name string, options *goimap.SelectOptions) (*goimap.SelectData, error) {
	if name == imap.FolderSent {
		if s.srv.currentFaults().SentExamineFails {
			return nil, &goimap.Error{Type: goimap.StatusResponseTypeNo, Code: goimap.ResponseCodeUnavailable, Text: "qqsim: folder is unavailable"}
		}
		s.srv.release(false)
	}
	data, err := s.UserSession.Select(name, options)
	if err != nil {
		return nil, err
	}
	data.UIDValidity = s.srv.uidValidity
	return data, nil
}

// Status 执行 STATUS：「已发送」先写入到期的延迟副本；请求了 UIDVALIDITY 时换成统一取值。
func (s *imapSession) Status(name string, options *goimap.StatusOptions) (*goimap.StatusData, error) {
	if name == imap.FolderSent {
		s.srv.release(false)
	}
	data, err := s.UserSession.Status(name, options)
	if err != nil {
		return nil, err
	}
	if options.UIDValidity {
		data.UIDValidity = s.srv.uidValidity
	}
	return data, nil
}

// List 执行 LIST：模式为空时只回层级分隔符（不可选的根，RFC 3501）；否则按模式列出三个文件夹，注入了 SentHidden 时不列「已发送」。
// 不支持 LIST 的扩展选项（产品不用），文件夹不带属性。
func (s *imapSession) List(w *imapserver.ListWriter, ref string, patterns []string, _ *goimap.ListOptions) error {
	if len(patterns) == 0 {
		return w.WriteList(&goimap.ListData{Attrs: []goimap.MailboxAttr{goimap.MailboxAttrNoSelect}, Delim: delim})
	}
	hidden := s.srv.currentFaults().SentHidden
	for _, name := range []string{imap.FolderInbox, imap.FolderJunk, imap.FolderSent} {
		if hidden && name == imap.FolderSent {
			continue
		}
		if !slices.ContainsFunc(patterns, func(pattern string) bool { return imapserver.MatchList(name, delim, ref, pattern) }) {
			continue
		}
		if err := w.WriteList(&goimap.ListData{Mailbox: name, Delim: delim}); err != nil {
			return err
		}
	}
	return nil
}

// Create 拒绝新建文件夹。
func (s *imapSession) Create(string, *goimap.CreateOptions) error {
	return errReadOnly
}

// Delete 拒绝删除文件夹。
func (s *imapSession) Delete(string) error {
	return errReadOnly
}

// Rename 拒绝重命名文件夹。
func (s *imapSession) Rename(string, string, *goimap.RenameOptions) error {
	return errReadOnly
}

// Subscribe 拒绝订阅文件夹。
func (s *imapSession) Subscribe(string) error {
	return errReadOnly
}

// Unsubscribe 拒绝取消订阅文件夹。
func (s *imapSession) Unsubscribe(string) error {
	return errReadOnly
}

// Append 拒绝客户端追加邮件；测试用 Deliver 放入邮件。
func (s *imapSession) Append(string, goimap.LiteralReader, *goimap.AppendOptions) (*goimap.AppendData, error) {
	return nil, errReadOnly
}

// Expunge 拒绝删除邮件。
func (s *imapSession) Expunge(*imapserver.ExpungeWriter, *goimap.UIDSet) error {
	return errReadOnly
}

// Store 拒绝修改标志。
func (s *imapSession) Store(*imapserver.FetchWriter, goimap.NumSet, *goimap.StoreFlags, *goimap.StoreOptions) error {
	return errReadOnly
}

// Copy 拒绝复制邮件。
func (s *imapSession) Copy(goimap.NumSet, string) (*goimap.CopyData, error) {
	return nil, errReadOnly
}

// Move 拒绝移动邮件（服务器也不公告 MOVE）。
func (s *imapSession) Move(*imapserver.MoveWriter, goimap.NumSet, string) error {
	return errReadOnly
}

// rewriteID 按 L1 实测的 QQ 行为改写一封邮件的头部：第一个 Message-Id 字段（名称不区分大小写，可以折行）的取值换成 id，紧接着写入
// X-OQ-MSGID 保存原取值（去掉换行与首尾空白）；其余 Message-Id 与原有的 X-OQ-MSGID 字段删去；原信没有 Message-Id 时在头部开头补上一个；
// 原取值为空时不写 X-OQ-MSGID。头部以第一个空行为界，新写的行沿用原信第一行的换行（CRLF 或 LF），其余字节原样保留。
// 返回改写后的字节与原取值。QQ 对原信缺少或重复 Message-Id 时的做法 L1 没有观察，这里取最简单的一致处理。
func rewriteID(raw []byte, id string) ([]byte, string) {
	newline := "\r\n"
	if i := bytes.IndexByte(raw, '\n'); i >= 0 && (i == 0 || raw[i-1] != '\r') {
		newline = "\n"
	}
	// span 是头部中一个字段（含续行与行尾）在 raw 中的范围。
	type span struct{ start, end int }
	var fields []span
	pos := 0
	for pos < len(raw) {
		end := len(raw)
		if i := bytes.IndexByte(raw[pos:], '\n'); i >= 0 {
			end = pos + i + 1
		}
		line := raw[pos:end]
		if len(bytes.TrimRight(line, "\r\n")) == 0 {
			break
		}
		if (line[0] == ' ' || line[0] == '\t') && len(fields) > 0 {
			fields[len(fields)-1].end = end
		} else {
			fields = append(fields, span{pos, end})
		}
		pos = end
	}
	var head bytes.Buffer
	original, found := "", false
	for _, f := range fields {
		field := raw[f.start:f.end]
		name, value, _ := bytes.Cut(field, []byte(":"))
		switch key := strings.TrimSpace(string(name)); {
		case strings.EqualFold(key, "Message-Id"):
			if found {
				continue
			}
			found = true
			original = strings.TrimSpace(strings.NewReplacer("\r", "", "\n", "").Replace(string(value)))
			head.WriteString("Message-Id: " + id + newline)
			if original != "" {
				head.WriteString(oqHeader + ": " + original + newline)
			}
		case strings.EqualFold(key, oqHeader):
		default:
			head.Write(field)
		}
	}
	var out bytes.Buffer
	if !found {
		out.WriteString("Message-Id: " + id + newline)
	}
	out.Write(head.Bytes())
	out.Write(raw[pos:])
	return out.Bytes(), original
}

// Reply 按 QQ 邮箱 App 回复的结构（mailfixture 的 qq-app-reply 样本，L1 实测）合成对收件人副本 d 的回复：发件人为 from，收件人为 d 的
// 信封发件人（机器人），主题为 subjectPrefix 加原主题（解码后，含标签），新的 <tencent_…@qq.com> Message-ID，In-Reply-To 与 References
// 都等于 d 的实际投递 ID；multipart/alternative 的纯文本与 HTML 两部分都是 utf-8/base64，纯文本先是 body，再是「原始邮件」分隔线、
// 引用头块与原信第一个非附件 text/plain 部件的全文（通知页脚的令牌行因此出现在引用中，没有引用前缀），HTML 用 QQ 自有标记、
// 不用 blockquote。body 的换行可以是 LF 或 CRLF；各参数与原信中形如占位符的文字原样保留。
//
// 原信无法解析、没有可引用的纯文本、字符集不是 utf-8 或 us-ascii、传输编码损坏，以及 from、subjectPrefix 或原主题含换行、
// body 或原文含单独的 CR 时返回错误；错误文本不含主题、正文与地址。
func Reply(d Delivered, from, subjectPrefix, body string) ([]byte, error) {
	subject, quoted, err := quoteOf(d.Raw)
	if err != nil {
		return nil, err
	}
	sample, err := mailfixture.Load(replySample)
	if err != nil {
		return nil, err
	}
	return sample.Build(mailfixture.Values{Delivered: d.MessageID, Optional: map[string]string{
		"FROM": from, "TO": d.From, "PREFIX": subjectPrefix, "SUBJECT": subject, "MESSAGEID": newMessageID(),
		"BODY": strings.ReplaceAll(body, "\r\n", "\n"), "QUOTED": quoted,
	}})
}

// quoteOf 从收件人副本中取出 Reply 引用所需的解码后主题，与第一个非附件 text/plain 部件的文字（换行统一为 LF，去掉末尾的空行）。
func quoteOf(raw []byte) (string, string, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", "", errUnreadable
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		return "", "", errUnreadable
	}
	text, found, err := plainPart(textproto.MIMEHeader(msg.Header), msg.Body, 0)
	switch {
	case err != nil:
		return "", "", err
	case !found:
		return "", "", errNoPlain
	}
	return subject, strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), nil
}

// plainPart 深度优先查找第一个非附件的 text/plain 部件，解码其传输编码并核对字符集（只接受 utf-8 与 us-ascii，与 4b 的通知一致）；
// multipart 的子部件依次查找，嵌套至多 maxDepth 层。found 为 false 表示没有这样的部件。
func plainPart(h textproto.MIMEHeader, body io.Reader, depth int) (text string, found bool, err error) {
	if depth > maxDepth {
		return "", false, errUnreadable
	}
	mediaType, params := "text/plain", map[string]string{}
	if value := h.Get("Content-Type"); value != "" {
		if mediaType, params, err = mime.ParseMediaType(value); err != nil {
			return "", false, errUnreadable
		}
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		if params["boundary"] == "" {
			return "", false, errUnreadable
		}
		r := multipart.NewReader(body, params["boundary"])
		for {
			part, err := r.NextRawPart()
			if err == io.EOF {
				return "", false, nil
			}
			if err != nil {
				return "", false, errUnreadable
			}
			if text, found, err := plainPart(part.Header, part, depth+1); err != nil || found {
				return text, found, err
			}
		}
	}
	disposition, _, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
	if mediaType != "text/plain" || disposition == "attachment" {
		return "", false, nil
	}
	data, err := decodeTransfer(h.Get("Content-Transfer-Encoding"), body)
	if err != nil {
		return "", false, err
	}
	if charset := strings.ToLower(params["charset"]); charset != "" && charset != "utf-8" && charset != "us-ascii" || !utf8.Valid(data) {
		return "", false, errCharset
	}
	return string(data), true, nil
}

// decodeTransfer 按传输编码解码部件正文：base64（忽略换行）与 quoted-printable 解码，7bit、8bit、binary 与缺省原样读出，其余不支持。
func decodeTransfer(encoding string, body io.Reader) ([]byte, error) {
	var r io.Reader
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		r = quotedprintable.NewReader(body)
	case "", "7bit", "8bit", "binary":
		r = body
	default:
		return nil, errUnreadable
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, errUnreadable
	}
	return data, nil
}
