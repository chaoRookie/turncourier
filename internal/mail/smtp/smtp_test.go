// Package smtp 用 go-smtp 服务端在本机回环地址上扮演离线假服务器，验证隐式 TLS、证书与主机名校验、
// 参数校验先于联网、结果分类、逐步期限与取消，以及错误文本不含机密、地址与服务器响应文本；测试不连接任何真实服务器。
package smtp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
)

// 测试用的合成地址与金丝雀；地址只用保留的 example.invalid 域名。
const (
	botAddr       = "bot@example.invalid"
	userAddr      = "user@example.invalid"
	otherAddr     = "other@example.invalid"
	serverCanary  = "SERVER-TEXT-CANARY"
	messageCanary = "MESSAGE-CANARY"
)

// testPassword 是测试用的授权码，在运行时构造，源码中不出现形似机密的字面量。
var testPassword = strings.Repeat("pw", 8)

// waitLimit 是等待假服务器侧事件的上限；Send 本身的返回时限由各用例单独断言。
const waitLimit = 2 * time.Second

// event 是只触发一次的测试信号，可从任意 goroutine 触发。
type event struct {
	once sync.Once
	ch   chan struct{}
}

// newEvent 返回尚未触发的信号。
func newEvent() *event {
	return &event{ch: make(chan struct{})}
}

// fire 触发信号；重复调用无效。
func (e *event) fire() {
	e.once.Do(func() { close(e.ch) })
}

// fired 报告信号是否已触发。
func (e *event) fired() bool {
	select {
	case <-e.ch:
		return true
	default:
		return false
	}
}

// wait 等待信号，超过 waitLimit 仍未触发即让用例失败。
func (e *event) wait(t *testing.T, what string) {
	t.Helper()
	select {
	case <-e.ch:
	case <-time.After(waitLimit):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// backend 是假服务器的后端：按用例注入拒绝与阻塞，并记录客户端发来的认证、信封与 DATA。
// 同一用例只有一个会话，因此记录放在后端上。
type backend struct {
	mechs     []string      // 公告的认证机制；为空时不公告 AUTH
	authCode  int           // 非 0 时认证以该状态码拒绝
	mailCode  int           // 非 0 时 MAIL 以该状态码拒绝
	rcptCode  int           // 非 0 时 RCPT 以该状态码拒绝
	rcptBlock bool          // RCPT 不回复，阻塞到连接关闭
	dataCode  int           // 非 0 时结束标记后以该状态码拒绝
	dataText  string        // dataCode 为 0 时 250 响应的文本；为空时使用库的默认文本
	dataBlock bool          // 读完 DATA 后不回复，阻塞到连接关闭
	dataStall bool          // 发出 354 后不再读取，阻塞到用例结束
	dataPace  time.Duration // 非 0 时每读至多 64 KiB DATA 停顿这么久，模拟慢速但持续读取的服务器

	release     *event     // 放行 dataStall 的阻塞；用例结束时一定触发
	rcptEntered *event     // 进入阻塞的 RCPT
	dataEntered *event     // 进入 DATA（354 已发出）
	dataRead    *event     // 已读完包括结束标记在内的 DATA
	connClosed  *event     // 处理函数发现客户端关闭了连接
	ended       *event     // 会话结束（服务端关闭连接时调用 Logout）
	transcript  transcript // 服务端看到的 TLS 之内的全部往来字节

	mu       sync.Mutex
	hello    string
	auths    int
	username string
	password string
	mails    []string
	rcpts    []string
	data     []byte
	dataSeen bool
}

// newBackend 返回默认接受一切、公告 AUTH PLAIN 的后端。
func newBackend() *backend {
	return &backend{
		mechs:       []string{sasl.Plain},
		release:     newEvent(),
		rcptEntered: newEvent(),
		dataEntered: newEvent(),
		dataRead:    newEvent(),
		connClosed:  newEvent(),
		ended:       newEvent(),
	}
}

// NewSession 为 EHLO 之后的连接创建会话。
func (b *backend) NewSession(c *gosmtp.Conn) (gosmtp.Session, error) {
	return &session{b: b, conn: c}, nil
}

// transcript 是可并发写入的字节记录，接在 go-smtp 服务端的调试输出上，记下 TLS 之内的往来内容。
type transcript struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write 追加一段往来内容。
func (tr *transcript) Write(p []byte) (int, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.buf.Write(p)
}

// String 返回已记录的内容。
func (tr *transcript) String() string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.buf.String()
}

// snapshot 在锁内复制后端的记录。
func (b *backend) snapshot() backendRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	return backendRecord{
		hello: b.hello, auths: b.auths, username: b.username, password: b.password,
		mails: append([]string(nil), b.mails...), rcpts: append([]string(nil), b.rcpts...),
		data: append([]byte(nil), b.data...), dataSeen: b.dataSeen,
	}
}

// backendRecord 是后端记录的副本，供断言使用。
type backendRecord struct {
	hello    string
	auths    int
	username string
	password string
	mails    []string
	rcpts    []string
	data     []byte
	dataSeen bool
}

// reject 构造假服务器的拒绝回复，文本一律为服务器金丝雀。
func reject(code int, enhanced gosmtp.EnhancedCode) *gosmtp.SMTPError {
	return &gosmtp.SMTPError{Code: code, EnhancedCode: enhanced, Message: serverCanary}
}

// enhancedFor 返回测试中各状态码对应的增强状态码；未列出的状态码不带增强状态码。
func enhancedFor(code int) gosmtp.EnhancedCode {
	switch code {
	case 535:
		return gosmtp.EnhancedCode{5, 7, 8}
	case 534:
		return gosmtp.EnhancedCode{5, 7, 9}
	case 454:
		return gosmtp.EnhancedCode{4, 7, 0}
	case 550:
		return gosmtp.EnhancedCode{5, 1, 1}
	case 451:
		return gosmtp.EnhancedCode{4, 3, 0}
	case 554:
		return gosmtp.EnhancedCode{5, 6, 0}
	}
	return gosmtp.NoEnhancedCode
}

// session 是假服务器的一个 SMTP 会话。
type session struct {
	b    *backend
	conn *gosmtp.Conn
}

// AuthMechanisms 返回后端配置的认证机制。
func (s *session) AuthMechanisms() []string {
	return s.b.mechs
}

// Auth 以 PLAIN 服务端记录用户名与密码，并按后端配置接受或拒绝。
func (s *session) Auth(mech string) (sasl.Server, error) {
	if mech != sasl.Plain {
		return nil, gosmtp.ErrAuthUnknownMechanism
	}
	return sasl.NewPlainServer(func(identity, username, password string) error {
		s.b.mu.Lock()
		s.b.auths++
		s.b.username, s.b.password = username, password
		s.b.mu.Unlock()
		if s.b.authCode != 0 {
			return reject(s.b.authCode, enhancedFor(s.b.authCode))
		}
		return nil
	}), nil
}

// Mail 记录 EHLO 名与信封发件人。
func (s *session) Mail(from string, _ *gosmtp.MailOptions) error {
	s.b.mu.Lock()
	s.b.hello = s.conn.Hostname()
	s.b.mails = append(s.b.mails, from)
	s.b.mu.Unlock()
	if s.b.mailCode != 0 {
		return reject(s.b.mailCode, enhancedFor(s.b.mailCode))
	}
	return nil
}

// Rcpt 记录收件人；rcptBlock 时不回复，直到客户端关闭连接。
func (s *session) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	s.b.mu.Lock()
	s.b.rcpts = append(s.b.rcpts, to)
	s.b.mu.Unlock()
	if s.b.rcptBlock {
		s.b.rcptEntered.fire()
		s.waitClosed()
		return reject(451, enhancedFor(451))
	}
	if s.b.rcptCode != 0 {
		return reject(s.b.rcptCode, enhancedFor(s.b.rcptCode))
	}
	return nil
}

// Data 按后端配置读取、记录并回复 DATA。
func (s *session) Data(r io.Reader) error {
	s.b.dataEntered.fire()
	if s.b.dataStall {
		<-s.b.release.ch
		return reject(451, enhancedFor(451))
	}
	data, err := readPaced(r, s.b.dataPace)
	s.b.mu.Lock()
	s.b.data, s.b.dataSeen = data, true
	s.b.mu.Unlock()
	if err != nil {
		// 结束标记之前连接就断了：客户端在提交阶段之前或期间关闭了连接。
		s.b.connClosed.fire()
		return err
	}
	s.b.dataRead.fire()
	if s.b.dataBlock {
		s.waitClosed()
		return reject(451, enhancedFor(451))
	}
	if s.b.dataCode != 0 {
		return reject(s.b.dataCode, enhancedFor(s.b.dataCode))
	}
	if s.b.dataText != "" {
		// go-smtp 把 Data 返回的 *SMTPError 原样写出，借此让 250 响应带上用例指定的文本。
		return &gosmtp.SMTPError{Code: 250, EnhancedCode: gosmtp.EnhancedCode{2, 0, 0}, Message: s.b.dataText}
	}
	return nil
}

// readPaced 读完 r；pace 非 0 时每读至多 64 KiB 停顿 pace。
func readPaced(r io.Reader, pace time.Duration) ([]byte, error) {
	if pace == 0 {
		return io.ReadAll(r)
	}
	var data []byte
	buf := make([]byte, 64<<10)
	for {
		n, err := r.Read(buf)
		data = append(data, buf[:n]...)
		if err == io.EOF {
			return data, nil
		}
		if err != nil {
			return data, err
		}
		time.Sleep(pace)
	}
}

// waitClosed 直接读底层连接直到客户端关闭它；此时客户端在等待回复，不会再发送数据。
func (s *session) waitClosed() {
	_, _ = io.Copy(io.Discard, s.conn.Conn())
	s.b.connClosed.fire()
}

// Reset 放弃当前邮件；假服务器无需处理。
func (s *session) Reset() {}

// Logout 在服务端关闭连接时调用，标记会话结束。
func (s *session) Logout() error {
	s.b.ended.fire()
	return nil
}

// recorder 记录监听器接受的连接收到的全部入站字节，并在读到连接结束时发出信号。
type recorder struct {
	mu   sync.Mutex
	data []byte
	eof  *event
}

// bytes 返回已记录的入站字节副本。
func (r *recorder) bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.data...)
}

// recordingConn 在读取时把入站字节追加到 recorder。
type recordingConn struct {
	net.Conn
	rec *recorder
}

// Read 读取并记录入站字节；读到错误（含 EOF）时发出信号。
func (c *recordingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.rec.mu.Lock()
	c.rec.data = append(c.rec.data, p[:n]...)
	c.rec.mu.Unlock()
	if err != nil {
		c.rec.eof.fire()
	}
	return n, err
}

// listener 包装测试监听器：计数接受的连接，可缩小每个连接的接收缓冲，可记录入站字节。
type listener struct {
	net.Listener
	readBuffer int
	rec        *recorder
	accepted   atomic.Int32
}

// Accept 接受连接并按配置包装。
func (l *listener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.accepted.Add(1)
	if l.readBuffer > 0 {
		if err := conn.(*net.TCPConn).SetReadBuffer(l.readBuffer); err != nil {
			conn.Close()
			return nil, err
		}
	}
	if l.rec != nil {
		return &recordingConn{Conn: conn, rec: l.rec}, nil
	}
	return conn, nil
}

// serverOptions 选择假服务器的传输方式与监听器行为。
type serverOptions struct {
	plaintext  bool // 不加 TLS，并允许明文认证，用于明文防护用例
	readBuffer int  // 非 0 时缩小每个接受的连接的接收缓冲
	record     bool // 记录全部入站字节
}

// testCerts 返回 httptest 自签证书的服务端 TLS 配置与只含其根证书的证书池；证书的 SAN 为 127.0.0.1、::1、example.com、*.example.com。
func testCerts(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	srv.StartTLS()
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv.TLS.Clone(), pool
}

// startServer 在本机回环地址的随机端口上启动假服务器，返回监听器；用例结束时放行阻塞的处理函数并关闭服务器。
func startServer(t *testing.T, b *backend, serverTLS *tls.Config, opts serverOptions) *listener {
	t.Helper()
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lis := &listener{Listener: raw, readBuffer: opts.readBuffer}
	if opts.record {
		lis.rec = &recorder{eof: newEvent()}
	}
	srv := gosmtp.NewServer(b)
	srv.Domain = "example.com"
	srv.ErrorLog = log.New(io.Discard, "", 0)
	srv.Debug = &b.transcript
	var serveOn net.Listener = lis
	if opts.plaintext {
		srv.AllowInsecureAuth = true
	} else {
		serveOn = tls.NewListener(lis, serverTLS)
	}
	go func() { _ = srv.Serve(serveOn) }()
	t.Cleanup(func() {
		b.release.fire()
		_ = srv.Close()
	})
	return lis
}

// generous 是不作为被测对象的步骤所用的宽松期限，避免机器繁忙时误判。
var generous = Timeouts{Command: 10 * time.Second, Submission: 10 * time.Second}

// testConfig 返回连接 127.0.0.1 上假服务器的配置。
func testConfig(l net.Listener, pool *x509.CertPool, timeouts Timeouts) Config {
	return Config{Host: "127.0.0.1", Port: l.Addr().(*net.TCPAddr).Port, Username: botAddr, RootCAs: pool, Timeouts: timeouts}
}

// testEnvelope 返回发件人为机器人、两个收件人的信封。
func testEnvelope() Envelope {
	return Envelope{From: botAddr, To: []string{userAddr, otherAddr}}
}

// testMessage 返回合成邮件：CRLF 换行，含以点开头的行与只有一个点的行，正文带金丝雀。
func testMessage() []byte {
	return []byte("From: " + botAddr + "\r\nTo: " + userAddr + "\r\nSubject: synthetic\r\n" +
		"Auto-Submitted: auto-generated\r\n\r\n.leading dot\r\n..two dots\r\n.\r\n" + messageCanary + "\r\n")
}

// bigMessage 返回恰为 size 字节、CRLF 换行的合成邮件，正文带金丝雀。
func bigMessage(size int) []byte {
	header := "Subject: synthetic\r\n\r\n" + messageCanary + "\r\n"
	line := strings.Repeat("x", 78) + "\r\n"
	var b bytes.Buffer
	b.WriteString(header)
	for b.Len()+len(line) <= size-2 {
		b.WriteString(line)
	}
	b.WriteString(strings.Repeat("y", size-2-b.Len()))
	b.WriteString("\r\n")
	return b.Bytes()
}

// checkErrorText 断言错误文本不含密码、地址、邮件金丝雀与服务器响应文本。
func checkErrorText(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	text := err.Error()
	for _, leak := range []string{testPassword, botAddr, userAddr, otherAddr, "example.invalid", messageCanary, serverCanary} {
		if strings.Contains(text, leak) {
			t.Errorf("error text %q leaks %q", text, leak)
		}
	}
}

// checkOutcome 断言错误恰好属于期望的结果类别，其余三个哨兵都不匹配。
func checkOutcome(t *testing.T, err, want error) {
	t.Helper()
	checkErrorText(t, err)
	for _, sentinel := range []error{ErrNotSent, ErrRejected, ErrUncertain} {
		if got := errors.Is(err, sentinel); got != (sentinel == want) {
			t.Errorf("errors.Is(%v, %v) = %v", err, sentinel, got)
		}
	}
}

// checkTimedOut 断言错误属于期望的结果类别、注明在期望的步骤超时，且 Send 在 2 秒内返回。
func checkTimedOut(t *testing.T, err, want error, step string, elapsed time.Duration) {
	t.Helper()
	checkOutcome(t, err, want)
	if !strings.HasSuffix(err.Error(), ": "+step+" timed out") {
		t.Errorf("err = %v, want a %s timeout", err, step)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Send took %v", elapsed)
	}
}

// sendTimed 以测试密码调用 Send，返回耗时与错误。
func sendTimed(ctx context.Context, cfg Config, env Envelope, msg []byte) (time.Duration, error) {
	start := time.Now()
	_, err := Send(ctx, cfg, testPassword, env, msg)
	return time.Since(start), err
}

// TestSendSuccess 验证正常投递：服务器收到的认证、信封、EHLO 名与去掉点填充后的 DATA 都与输入一致，返回 250 响应的文本。
func TestSendSuccess(t *testing.T) {
	serverTLS, pool := testCerts(t)
	b := newBackend()
	b.dataText = "OK queued as SYNTHETIC-ID"
	lis := startServer(t, b, serverTLS, serverOptions{})
	msg := testMessage()

	res, err := Send(context.Background(), testConfig(lis, pool, generous), testPassword, testEnvelope(), msg)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if want := "2.0.0 OK queued as SYNTHETIC-ID"; res.Response != want {
		t.Errorf("Response = %q, want %q", res.Response, want)
	}
	b.ended.wait(t, "session end after QUIT")
	if !strings.Contains(b.transcript.String(), "\r\nQUIT\r\n") {
		t.Error("client did not send QUIT after the message was accepted")
	}
	got := b.snapshot()
	if got.auths != 1 || got.username != botAddr || got.password != testPassword {
		t.Errorf("auth = %d attempts, username %q, password matches %v", got.auths, got.username, got.password == testPassword)
	}
	if got.hello != "localhost" {
		t.Errorf("EHLO name = %q, want localhost", got.hello)
	}
	if len(got.mails) != 1 || got.mails[0] != botAddr {
		t.Errorf("MAIL FROM = %q", got.mails)
	}
	if len(got.rcpts) != 2 || got.rcpts[0] != userAddr || got.rcpts[1] != otherAddr {
		t.Errorf("RCPT TO = %q", got.rcpts)
	}
	if !bytes.Equal(got.data, msg) {
		t.Errorf("DATA = %q, want %q", got.data, msg)
	}
}

// TestSendDefaultResponseAndTruncation 验证库默认的 250 文本原样返回，超过 512 字节的响应文本被截断到 512 字节。
func TestSendDefaultResponseAndTruncation(t *testing.T) {
	serverTLS, pool := testCerts(t)
	for _, tc := range []struct {
		name, text, want string
	}{
		{"default", "", "2.0.0 OK: queued"},
		{"exactly 512", strings.Repeat("a", 506), "2.0.0 " + strings.Repeat("a", 506)},
		{"long", strings.Repeat("b", 1000), "2.0.0 " + strings.Repeat("b", 506)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBackend()
			b.dataText = tc.text
			lis := startServer(t, b, serverTLS, serverOptions{})
			res, err := Send(context.Background(), testConfig(lis, pool, generous), testPassword, testEnvelope(), testMessage())
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if res.Response != tc.want {
				t.Errorf("Response = %q (%d bytes), want %d bytes", res.Response, len(res.Response), len(tc.want))
			}
		})
	}
}

// sendArgs 是一次 Send 调用的全部参数，参数校验用例在合法参数上逐项改动。
type sendArgs struct {
	cfg      Config
	password string
	env      Envelope
	msg      []byte
}

// TestSendValidatesBeforeDialing 验证参数不合法时在联网之前返回 ErrNotSent，监听器没有收到任何连接；边界值可以发送。
func TestSendValidatesBeforeDialing(t *testing.T) {
	serverTLS, pool := testCerts(t)
	b := newBackend()
	lis := startServer(t, b, serverTLS, serverOptions{})
	for _, tc := range []struct {
		name   string
		mutate func(*sendArgs)
	}{
		{"from differs from username", func(a *sendArgs) { a.env.From = userAddr }},
		{"empty from and username", func(a *sendArgs) { a.cfg.Username, a.env.From = "", "" }},
		{"from with CRLF", func(a *sendArgs) { a.cfg.Username += "\r\n"; a.env.From = a.cfg.Username }},
		{"no recipients", func(a *sendArgs) { a.env.To = nil }},
		{"recipient with CRLF", func(a *sendArgs) { a.env.To = []string{userAddr, "x@example.invalid\r\nDATA"} }},
		{"recipient with LF", func(a *sendArgs) { a.env.To = []string{"x@example.invalid\nRSET"} }},
		{"recipient with CR", func(a *sendArgs) { a.env.To = []string{"x@example.invalid\rRSET"} }},
		{"empty recipient", func(a *sendArgs) { a.env.To = []string{userAddr, ""} }},
		{"empty message", func(a *sendArgs) { a.msg = nil }},
		{"oversized message", func(a *sendArgs) { a.msg = bigMessage(MaxMessageSize + 1) }},
		{"empty password", func(a *sendArgs) { a.password = "" }},
		{"password with space", func(a *sendArgs) { a.password = strings.Repeat("pw ", 4) }},
		{"password with tab", func(a *sendArgs) { a.password = strings.Repeat("pw\t", 4) }},
		{"password with non-ASCII", func(a *sendArgs) { a.password = strings.Repeat("pw\u00e9", 4) }},
		{"password with DEL", func(a *sendArgs) { a.password = strings.Repeat("pw\x7f", 4) }},
		{"password with NUL", func(a *sendArgs) { a.password = strings.Repeat("pw\x00", 4) }},
		{"port zero", func(a *sendArgs) { a.cfg.Port = 0 }},
		{"port too large", func(a *sendArgs) { a.cfg.Port = 65536 }},
		{"negative port", func(a *sendArgs) { a.cfg.Port = -1 }},
		{"empty host", func(a *sendArgs) { a.cfg.Host = "" }},
		{"negative command timeout", func(a *sendArgs) { a.cfg.Timeouts.Command = -time.Second }},
		{"negative submission timeout", func(a *sendArgs) { a.cfg.Timeouts.Submission = -time.Second }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := sendArgs{cfg: testConfig(lis, pool, generous), password: testPassword, env: testEnvelope(), msg: testMessage()}
			tc.mutate(&a)
			_, err := Send(context.Background(), a.cfg, a.password, a.env, a.msg)
			checkOutcome(t, err, ErrNotSent)
			if errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), ": invalid ") {
				t.Errorf("err = %v, want a validation error", err)
			}
			if n := lis.accepted.Load(); n != 0 {
				t.Fatalf("listener accepted %d connections", n)
			}
		})
	}
	// 边界值可以发送：恰为 4 MiB 的邮件，以及由全部可打印 ASCII 组成的密码。
	var printable strings.Builder
	for c := byte('!'); c <= '~'; c++ {
		printable.WriteByte(c)
	}
	msg := bigMessage(MaxMessageSize)
	if _, err := Send(context.Background(), testConfig(lis, pool, generous), printable.String(), testEnvelope(), msg); err != nil {
		t.Fatalf("Send at the limits: %v", err)
	}
	if got := b.snapshot(); got.password != printable.String() || !bytes.Equal(got.data, msg) {
		t.Errorf("server saw password match %v and %d bytes of DATA", got.password == printable.String(), len(got.data))
	}
}

// TestSendCanceledBeforeDialing 验证 ctx 在调用前已结束时不联网，返回同时满足 ErrNotSent 与 context.Canceled 的错误。
func TestSendCanceledBeforeDialing(t *testing.T) {
	serverTLS, pool := testCerts(t)
	lis := startServer(t, newBackend(), serverTLS, serverOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Send(ctx, testConfig(lis, pool, generous), testPassword, testEnvelope(), testMessage())
	checkOutcome(t, err, ErrNotSent)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled in the chain", err)
	}
	if n := lis.accepted.Load(); n != 0 {
		t.Errorf("listener accepted %d connections", n)
	}
}

// TestSendRefusesPlaintextServer 验证服务器不讲 TLS 时（即使它允许明文认证）不发送凭据：入站字节中没有 AUTH 与密码。
func TestSendRefusesPlaintextServer(t *testing.T) {
	b := newBackend()
	lis := startServer(t, b, nil, serverOptions{plaintext: true, record: true})
	_, pool := testCerts(t)
	_, err := Send(context.Background(), testConfig(lis, pool, generous), testPassword, testEnvelope(), testMessage())
	checkOutcome(t, err, ErrNotSent)
	if n := lis.accepted.Load(); n != 1 {
		t.Fatalf("listener accepted %d connections, want 1", n)
	}
	lis.rec.eof.wait(t, "the server to read the whole client stream")
	inbound := lis.rec.bytes()
	if len(inbound) == 0 {
		t.Fatal("server recorded no inbound bytes")
	}
	if bytes.Contains(inbound, []byte("AUTH")) || bytes.Contains(inbound, []byte(testPassword)) {
		t.Errorf("inbound bytes contain AUTH or the password: %q", inbound)
	}
	if got := b.snapshot(); got.auths != 0 {
		t.Errorf("backend saw %d AUTH attempts", got.auths)
	}
}

// TestSendRejectsUntrustedCertificate 验证服务器证书不在 RootCAs 中时不发送凭据。
func TestSendRejectsUntrustedCertificate(t *testing.T) {
	serverTLS, _ := testCerts(t)
	b := newBackend()
	lis := startServer(t, b, serverTLS, serverOptions{})
	_, err := Send(context.Background(), testConfig(lis, x509.NewCertPool(), generous), testPassword, testEnvelope(), testMessage())
	checkOutcome(t, err, ErrNotSent)
	if n := lis.accepted.Load(); n != 1 {
		t.Fatalf("listener accepted %d connections, want 1", n)
	}
	if got := b.snapshot(); got.auths != 0 {
		t.Errorf("backend saw %d AUTH attempts", got.auths)
	}
}

// TestSendVerifiesHostname 验证 CA 受信但 Host 不在证书 SAN 中时不发送凭据。
func TestSendVerifiesHostname(t *testing.T) {
	serverTLS, pool := testCerts(t)
	b := newBackend()
	lis := startServer(t, b, serverTLS, serverOptions{})
	cfg := testConfig(lis, pool, generous)
	cfg.Host = "localhost"
	_, err := Send(context.Background(), cfg, testPassword, testEnvelope(), testMessage())
	checkOutcome(t, err, ErrNotSent)
	if n := lis.accepted.Load(); n < 1 {
		t.Fatalf("listener accepted %d connections, want at least 1", n)
	}
	if got := b.snapshot(); got.auths != 0 {
		t.Errorf("backend saw %d AUTH attempts", got.auths)
	}
}

// TestSendRequiresAuthPlain 验证服务器不公告 AUTH，或只公告 PLAIN 以外的机制时，不发送凭据。
func TestSendRequiresAuthPlain(t *testing.T) {
	serverTLS, pool := testCerts(t)
	for _, tc := range []struct {
		name  string
		mechs []string
	}{
		{"no AUTH", nil},
		{"LOGIN only", []string{sasl.Login}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBackend()
			b.mechs = tc.mechs
			lis := startServer(t, b, serverTLS, serverOptions{})
			_, err := Send(context.Background(), testConfig(lis, pool, generous), testPassword, testEnvelope(), testMessage())
			checkOutcome(t, err, ErrNotSent)
			if got := b.snapshot(); got.auths != 0 || len(got.mails) != 0 {
				t.Errorf("backend saw %d AUTH attempts and MAIL %q", got.auths, got.mails)
			}
		})
	}
}

// TestSendClassifiesReplies 验证服务器以 4xx/5xx 拒绝时的分类、*ReplyError 的步骤与状态码，以及拒绝之后不再继续。
func TestSendClassifiesReplies(t *testing.T) {
	serverTLS, pool := testCerts(t)
	for _, tc := range []struct {
		name      string
		setup     func(*backend)
		outcome   error
		auth      bool
		want      ReplyError
		temporary bool
		check     func(*testing.T, *backend)
	}{
		{
			name: "auth 535", setup: func(b *backend) { b.authCode = 535 }, outcome: ErrNotSent, auth: true,
			want: ReplyError{Step: "auth", Code: 535, Enhanced: [3]int{5, 7, 8}},
			check: func(t *testing.T, b *backend) {
				if r := b.snapshot(); len(r.mails) != 0 {
					t.Errorf("MAIL sent after failed AUTH: %q", r.mails)
				}
			},
		},
		{
			name: "auth 534", setup: func(b *backend) { b.authCode = 534 }, outcome: ErrNotSent, auth: true,
			want: ReplyError{Step: "auth", Code: 534, Enhanced: [3]int{5, 7, 9}},
		},
		{
			name: "auth 454", setup: func(b *backend) { b.authCode = 454 }, outcome: ErrNotSent, temporary: true,
			want: ReplyError{Step: "auth", Code: 454, Enhanced: [3]int{4, 7, 0}},
			check: func(t *testing.T, b *backend) {
				if r := b.snapshot(); len(r.mails) != 0 {
					t.Errorf("MAIL sent after failed AUTH: %q", r.mails)
				}
			},
		},
		{
			name: "mail 553 without enhanced code", setup: func(b *backend) { b.mailCode = 553 }, outcome: ErrNotSent,
			want: ReplyError{Step: "mail", Code: 553},
			check: func(t *testing.T, b *backend) {
				if r := b.snapshot(); len(r.rcpts) != 0 {
					t.Errorf("RCPT sent after rejected MAIL: %q", r.rcpts)
				}
			},
		},
		{
			name: "rcpt 550", setup: func(b *backend) { b.rcptCode = 550 }, outcome: ErrNotSent,
			want: ReplyError{Step: "rcpt", Code: 550, Enhanced: [3]int{5, 1, 1}},
			check: func(t *testing.T, b *backend) {
				if r := b.snapshot(); len(r.rcpts) != 1 || r.dataSeen || b.dataEntered.fired() {
					t.Errorf("RCPT %q, DATA seen %v after rejected RCPT", r.rcpts, r.dataSeen)
				}
			},
		},
		{
			name: "data end 451", setup: func(b *backend) { b.dataCode = 451 }, outcome: ErrRejected, temporary: true,
			want: ReplyError{Step: "submission", Code: 451, Enhanced: [3]int{4, 3, 0}},
		},
		{
			name: "data end 554", setup: func(b *backend) { b.dataCode = 554 }, outcome: ErrRejected,
			want: ReplyError{Step: "submission", Code: 554, Enhanced: [3]int{5, 6, 0}},
			check: func(t *testing.T, b *backend) {
				if r := b.snapshot(); !r.dataSeen {
					t.Error("server did not receive DATA before rejecting it")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBackend()
			tc.setup(b)
			lis := startServer(t, b, serverTLS, serverOptions{})
			_, err := Send(context.Background(), testConfig(lis, pool, generous), testPassword, testEnvelope(), testMessage())
			checkOutcome(t, err, tc.outcome)
			if got := errors.Is(err, ErrAuth); got != tc.auth {
				t.Errorf("errors.Is(err, ErrAuth) = %v, want %v", got, tc.auth)
			}
			var reply *ReplyError
			if !errors.As(err, &reply) {
				t.Fatalf("err = %v, want a *ReplyError in the chain", err)
			}
			if *reply != tc.want {
				t.Errorf("ReplyError = %+v, want %+v", *reply, tc.want)
			}
			if reply.Temporary() != tc.temporary {
				t.Errorf("Temporary() = %v, want %v", reply.Temporary(), tc.temporary)
			}
			if tc.check != nil {
				tc.check(t, b)
			}
		})
	}
}

// TestSendUncertainWhenSubmissionStalls 验证结束标记之后服务器不回复时，在 Submission 期限后返回 ErrUncertain，此时服务器已收到完整的 DATA。
func TestSendUncertainWhenSubmissionStalls(t *testing.T) {
	serverTLS, pool := testCerts(t)
	b := newBackend()
	b.dataBlock = true
	lis := startServer(t, b, serverTLS, serverOptions{})
	msg := testMessage()
	elapsed, err := sendTimed(context.Background(), testConfig(lis, pool, Timeouts{Command: 10 * time.Second, Submission: 200 * time.Millisecond}), testEnvelope(), msg)
	checkTimedOut(t, err, ErrUncertain, "submission", elapsed)
	b.dataRead.wait(t, "the server to receive the end-of-data marker")
	if got := b.snapshot(); !bytes.Equal(got.data, msg) {
		t.Errorf("DATA = %q, want %q", got.data, msg)
	}
	b.connClosed.wait(t, "the client to close the connection")
}

// TestSendBodyWriteStall 验证服务器发出 354 后不再读取时，写正文在 Command 期限后被中断并返回 ErrNotSent，连接已关闭。
func TestSendBodyWriteStall(t *testing.T) {
	serverTLS, pool := testCerts(t)
	b := newBackend()
	b.dataStall = true
	lis := startServer(t, b, serverTLS, serverOptions{readBuffer: 4096})
	cfg := testConfig(lis, pool, Timeouts{Command: 200 * time.Millisecond, Submission: 10 * time.Second})
	elapsed, err := sendTimed(context.Background(), cfg, testEnvelope(), bigMessage(MaxMessageSize))
	checkTimedOut(t, err, ErrNotSent, "body", elapsed)
	b.dataEntered.wait(t, "the server to send 354")
	// 放行后，服务端丢弃剩余 DATA 时读到连接已关闭，随即结束会话；连接若仍打开，它会一直等待结束标记。
	b.release.fire()
	b.ended.wait(t, "the session to end after the client closed the connection")
}

// TestSendSlowReaderWithinChunkDeadlines 验证 Command 期限按 64 KiB 分块计时：服务器慢速但持续读取，
// 每块都在期限内写完而整封正文的写入远超 Command 时仍投递成功。
func TestSendSlowReaderWithinChunkDeadlines(t *testing.T) {
	serverTLS, pool := testCerts(t)
	b := newBackend()
	b.dataPace = 10 * time.Millisecond
	lis := startServer(t, b, serverTLS, serverOptions{readBuffer: 4096})
	cfg := testConfig(lis, pool, Timeouts{Command: 200 * time.Millisecond, Submission: 10 * time.Second})
	msg := bigMessage(MaxMessageSize)
	if _, err := Send(context.Background(), cfg, testPassword, testEnvelope(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := b.snapshot(); !bytes.Equal(got.data, msg) {
		t.Errorf("server received %d bytes of DATA, want %d", len(got.data), len(msg))
	}
}

// TestSendFlushStall 验证提交阶段在 flush 剩余正文时阻塞，看门狗在调用 CloseWithResponse 之前就以 Submission 期限生效，返回 ErrUncertain。
func TestSendFlushStall(t *testing.T) {
	serverTLS, pool := testCerts(t)
	b := newBackend()
	lis := startServer(t, b, serverTLS, serverOptions{})
	original := closeData
	t.Cleanup(func() { closeData = original })
	closeData = func(*gosmtp.DataCommand) (*gosmtp.DataResponse, error) {
		// 替身不写结束标记，只等服务端发现连接被关闭；看门狗缺席时 5 秒后放弃，让用例以耗时失败。
		select {
		case <-b.connClosed.ch:
		case <-time.After(5 * time.Second):
		}
		return nil, errors.New("flush interrupted")
	}
	cfg := testConfig(lis, pool, Timeouts{Command: 10 * time.Second, Submission: 200 * time.Millisecond})
	elapsed, err := sendTimed(context.Background(), cfg, testEnvelope(), testMessage())
	checkTimedOut(t, err, ErrUncertain, "submission", elapsed)
	if !b.connClosed.fired() {
		t.Error("the connection was not closed during the flush")
	}
}

// startScripted 启动按脚本应答的 TLS 假服务器，用于 go-smtp 服务端无法给出的回复：先写问候，
// 之后每读到一条命令就依次写出 replies 中的一项；对 DATA 命令回复 354 之后，先读完 DATA 直到结束标记再写下一项；
// 回复为空字符串时关闭连接。
func startScripted(t *testing.T, serverTLS *tls.Config, replies ...string) net.Listener {
	t.Helper()
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	go func() {
		conn, err := raw.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		tlsConn := tls.Server(conn, serverTLS)
		r := bufio.NewReader(tlsConn)
		if _, err := io.WriteString(tlsConn, "220 example.com ESMTP\r\n"); err != nil {
			return
		}
		inData := false // 上一项回复是对 DATA 命令的 354：下一项回复对应结束标记，而不是新命令
		for _, reply := range replies {
			if inData {
				for line := ""; line != ".\r\n"; {
					if line, err = r.ReadString('\n'); err != nil {
						return
					}
				}
			} else if _, err := r.ReadString('\n'); err != nil {
				return
			}
			if reply == "" {
				return
			}
			if _, err := io.WriteString(tlsConn, reply+"\r\n"); err != nil {
				return
			}
			inData = !inData && strings.HasPrefix(reply, "354")
		}
		_, _ = io.Copy(io.Discard, r)
	}()
	return raw
}

// TestSendUnusualReplies 验证 go-smtp 服务端给不出的回复：DATA 命令被拒绝、意外的非 4xx/5xx 回复与中途断线。
func TestSendUnusualReplies(t *testing.T) {
	serverTLS, pool := testCerts(t)
	const ehlo = "250-example.com\r\n250 AUTH PLAIN"
	for _, tc := range []struct {
		name    string
		replies []string
		outcome error
		reply   *ReplyError
	}{
		{"data command 554", []string{ehlo, "235 2.7.0 ok", "250 ok", "250 ok", "554 5.3.4 " + serverCanary}, ErrNotSent,
			&ReplyError{Step: "data", Code: 554, Enhanced: [3]int{5, 3, 4}}},
		{"data command 250", []string{ehlo, "235 2.7.0 ok", "250 ok", "250 ok", "250 " + serverCanary}, ErrNotSent, nil},
		{"end of data 354", []string{ehlo, "235 2.7.0 ok", "250 ok", "250 ok", "354 go ahead", "354 " + serverCanary}, ErrUncertain, nil},
		{"end of data 600", []string{ehlo, "235 2.7.0 ok", "250 ok", "250 ok", "354 go ahead", "600 " + serverCanary}, ErrUncertain, nil},
		{"dropped at mail", []string{ehlo, "235 2.7.0 ok", ""}, ErrNotSent, nil},
		{"ehlo 554", []string{"554 5.7.1 " + serverCanary}, ErrNotSent, nil},
		{"auth 334 with bad base64", []string{ehlo, "334 !!" + serverCanary, "501 5.0.0 cancelled"}, ErrNotSent, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lis := startScripted(t, serverTLS, tc.replies...)
			env := Envelope{From: botAddr, To: []string{userAddr}}
			_, err := Send(context.Background(), testConfig(lis, pool, generous), testPassword, env, testMessage())
			checkOutcome(t, err, tc.outcome)
			if errors.Is(err, ErrAuth) {
				t.Errorf("err = %v matches ErrAuth", err)
			}
			var reply *ReplyError
			if got := errors.As(err, &reply); got != (tc.reply != nil) {
				t.Fatalf("err = %v, ReplyError present = %v", err, got)
			}
			if tc.reply != nil && *reply != *tc.reply {
				t.Errorf("ReplyError = %+v, want %+v", *reply, *tc.reply)
			}
		})
	}
}

// TestSendIgnoresQuitFailure 验证邮件被接受后 QUIT 没有回应时，在 Command 期限内关闭连接并照常返回成功。
func TestSendIgnoresQuitFailure(t *testing.T) {
	serverTLS, pool := testCerts(t)
	lis := startScripted(t, serverTLS, "250-example.com\r\n250 AUTH PLAIN", "235 2.7.0 ok", "250 ok", "250 ok", "354 go ahead", "250 2.0.0 queued")
	cfg := testConfig(lis, pool, Timeouts{Command: 200 * time.Millisecond, Submission: 10 * time.Second})
	start := time.Now()
	res, err := Send(context.Background(), cfg, testPassword, Envelope{From: botAddr, To: []string{userAddr}}, testMessage())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Response != "2.0.0 queued" {
		t.Errorf("Response = %q", res.Response)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Send took %v", elapsed)
	}
}

// TestSendGreetingStall 验证 TLS 握手完成后服务器从不写问候时，在 Command 期限后返回 ErrNotSent。
func TestSendGreetingStall(t *testing.T) {
	serverTLS, pool := testCerts(t)
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	closed := newEvent()
	go func() {
		conn, err := raw.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		tlsConn := tls.Server(conn, serverTLS)
		if tlsConn.Handshake() != nil {
			return
		}
		_, _ = io.Copy(io.Discard, tlsConn)
		closed.fire()
	}()
	cfg := testConfig(raw, pool, Timeouts{Command: 200 * time.Millisecond, Submission: 10 * time.Second})
	elapsed, err := sendTimed(context.Background(), cfg, testEnvelope(), testMessage())
	checkTimedOut(t, err, ErrNotSent, "ehlo", elapsed)
	closed.wait(t, "the client to close the connection")
}

// TestSendCanceled 验证取消 ctx 时关闭连接：阻塞在 RCPT 时返回 ErrNotSent，阻塞在结束标记之后时返回 ErrUncertain，两者都带 context.Canceled。
func TestSendCanceled(t *testing.T) {
	serverTLS, pool := testCerts(t)
	for _, tc := range []struct {
		name    string
		setup   func(*backend)
		entered func(*backend) *event
		outcome error
		step    string
	}{
		{"during rcpt", func(b *backend) { b.rcptBlock = true }, func(b *backend) *event { return b.rcptEntered }, ErrNotSent, "rcpt"},
		{"after end of data", func(b *backend) { b.dataBlock = true }, func(b *backend) *event { return b.dataRead }, ErrUncertain, "submission"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBackend()
			tc.setup(b)
			lis := startServer(t, b, serverTLS, serverOptions{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				select {
				case <-tc.entered(b).ch:
					cancel()
				case <-time.After(waitLimit):
				}
			}()
			elapsed, err := sendTimed(ctx, testConfig(lis, pool, generous), testEnvelope(), testMessage())
			checkOutcome(t, err, tc.outcome)
			if !errors.Is(err, context.Canceled) || !strings.HasSuffix(err.Error(), ": "+tc.step+": context canceled") {
				t.Errorf("err = %v, want context.Canceled at %s", err, tc.step)
			}
			if elapsed > 2*time.Second {
				t.Errorf("Send took %v", elapsed)
			}
			b.connClosed.wait(t, "the client to close the connection")
			b.ended.wait(t, "the session to end")
		})
	}
}

// TestWatchdogHalt 验证 halt 返回时看门狗的 goroutine 已退出，之后 ctx 结束也不会再关闭连接。
func TestWatchdogHalt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var closes atomic.Int32
	w := startWatchdog(ctx, func() { closes.Add(1) })
	if err := w.step(time.Hour, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	w.halt()
	select {
	case <-w.done:
	default:
		t.Fatal("watchdog goroutine still running after halt")
	}
	cancel()
	w.set(time.Nanosecond) // 已退出的看门狗不再接收期限，也不会阻塞调用方
	time.Sleep(10 * time.Millisecond)
	if n := closes.Load(); n != 0 {
		t.Errorf("connection closed %d times after halt", n)
	}
}

// TestReplyError 验证 ReplyError 的文本格式与 Temporary。
func TestReplyError(t *testing.T) {
	for _, tc := range []struct {
		err       ReplyError
		text      string
		temporary bool
	}{
		{ReplyError{Step: "rcpt", Code: 550, Enhanced: [3]int{5, 1, 1}}, "smtp: rcpt rejected with 550 5.1.1", false},
		{ReplyError{Step: "mail", Code: 550}, "smtp: mail rejected with 550", false},
		{ReplyError{Step: "submission", Code: 451, Enhanced: [3]int{4, 3, 0}}, "smtp: submission rejected with 451 4.3.0", true},
		{ReplyError{Step: "auth", Code: 454}, "smtp: auth rejected with 454", true},
		{ReplyError{Step: "data", Code: 399}, "smtp: data rejected with 399", false},
		{ReplyError{Step: "data", Code: 400}, "smtp: data rejected with 400", true},
		{ReplyError{Step: "data", Code: 499}, "smtp: data rejected with 499", true},
		{ReplyError{Step: "data", Code: 500}, "smtp: data rejected with 500", false},
	} {
		if got := tc.err.Error(); got != tc.text {
			t.Errorf("Error() = %q, want %q", got, tc.text)
		}
		if got := tc.err.Temporary(); got != tc.temporary {
			t.Errorf("%q Temporary() = %v, want %v", tc.text, got, tc.temporary)
		}
	}
}

// TestTimeouts 验证生产期限，以及零值字段取默认值、非零字段保留。
func TestTimeouts(t *testing.T) {
	if got := DefaultTimeouts(); got != (Timeouts{Command: 30 * time.Second, Submission: 120 * time.Second}) {
		t.Errorf("DefaultTimeouts() = %+v", got)
	}
	if got := (Timeouts{}).withDefaults(); got != DefaultTimeouts() {
		t.Errorf("zero Timeouts = %+v", got)
	}
	if got := (Timeouts{Command: time.Second}).withDefaults(); got != (Timeouts{Command: time.Second, Submission: 120 * time.Second}) {
		t.Errorf("partial Timeouts = %+v", got)
	}
	if got := (Timeouts{Submission: time.Second}).withDefaults(); got != (Timeouts{Command: 30 * time.Second, Submission: time.Second}) {
		t.Errorf("partial Timeouts = %+v", got)
	}
}
