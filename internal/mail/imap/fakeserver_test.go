// Package imap 的离线假服务器：imapserver 加 imapmemserver 在本机回环地址上提供标准 IMAP 行为（INBOX、Junk 与「已发送」），
// 前面是只在测试中存在的代理。代理默认终结 TLS（证书借用 httptest），记录客户端发出的每条命令（标签、命令名与参数，
// LOGIN 只记命令名；UID FETCH 另记数据项，可据此区分取整封的 BODY.PEEK[] 与只取头部的 BODY.PEEK[HEADER]），并按规则注入故障，
// 模拟 QQ 的无标签 BAD、半开连接、问候冻结、文件夹暂时不可用、补扫中到达的 EXISTS、少报的邮件大小、
// 不遵守部分取回、字面量中途断开、FETCH 不带数据或带着非常规的数据（字面量为 NIL、返回的节与请求不一致）、
// 断开（含拒绝登录后断开）、登录前的慢握手、能力差异、不报告 UIDNEXT 与明文入口。
// 测试只连接这里的本地假服务器，不连接任何真实服务器。
package imap

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// testUser 是测试用的合成账户；地址只用保留的 example.invalid 域名。
const testUser = "bot@example.invalid"

// testPassword 是测试用的授权码，在运行时构造，源码中不出现形似机密的字面量。
var testPassword = strings.Repeat("pw", 8)

// allowedCommands 是本包允许发送的命令名；DONE 是结束 IDLE 的续行，也记录在内。
var allowedCommands = []string{"CAPABILITY", "LOGIN", "LIST", "EXAMINE", "UID SEARCH", "UID FETCH", "IDLE", "DONE", "LOGOUT"}

// idleContinuation 是 imapserver 对 IDLE 的继续响应；「进入 IDLE 后冻结」按它的长度设置转发字节数。
const idleContinuation = "+ idling\r\n"

// waitLimit 是等待假服务器侧事件的上限；被测调用本身的返回时限由各用例单独断言。
const waitLimit = 3 * time.Second

var (
	// commandPattern 匹配「标签 命令名」；明文入口收到的 TLS 握手字节不会被误记为命令。
	commandPattern = regexp.MustCompile(`^[A-Za-z0-9]+ [A-Z]+( [A-Z]+)?$`)
	// capabilityPattern 匹配服务器响应中能力列表的开头：问候与 LOGIN 的响应码，以及 CAPABILITY 的无标签响应。
	capabilityPattern = regexp.MustCompile(`(\[CAPABILITY |^\* CAPABILITY )`)
	// sizePattern 匹配 FETCH 响应中的 RFC822.SIZE 数据项。
	sizePattern = regexp.MustCompile(`RFC822\.SIZE \d+`)
	// literalPattern 匹配行尾的字面量长度 {N}。
	literalPattern = regexp.MustCompile(`\{(\d+)\}\r\n$`)
	// uidNextPattern 匹配 EXAMINE 响应中报告 UIDNEXT 的无标签 OK 行。
	uidNextPattern = regexp.MustCompile(`^\* OK \[UIDNEXT \d+\]`)
)

// faultKind 是代理可注入的故障种类。
type faultKind int

const (
	faultBAD         faultKind = iota + 1 // 不转发，只回无标签 BAD，永不完成该标签
	faultFreeze                           // 不转发，冻结两个方向
	faultFreezeAfter                      // 转发命令，响应转发 bytes 字节后冻结两个方向
	faultReject                           // 不转发，回 <标签> NO [UNAVAILABLE]
	faultRejectBAD                        // 不转发，回 <标签> BAD，模拟服务器以带标签 BAD 拒绝一条命令而连接仍可用
	faultInject                           // 先向客户端写入 exists 中每个 N 的 * N EXISTS，再转发命令
	faultDisconnect                       // 不转发，立即关闭两侧连接
	faultEmpty                            // 不转发，直接回 <标签> OK，模拟服务器没有返回任何数据
	faultRejectClose                      // 不转发，回 <标签> NO [AUTHENTICATIONFAILED] 后立即关闭两侧连接，模拟拒绝登录后断开的服务器
	faultWholeBody                        // 不转发，回一条正文字面量为 bytes 字节的 FETCH 响应再回 <标签> OK，模拟不遵守部分取回的服务器；cut 大于 0 时只发出字面量的前 cut 字节就关闭两侧连接
	faultCloseAfter                       // 转发命令，响应转发 bytes 字节后关闭两侧连接；TLS 以 close_notify 正常结束，客户端读到 io.EOF
	faultReply                            // 不转发，原样写出 reply 再回 <标签> OK，模拟以非常规的数据回应命令的服务器（例如正文节为 NIL、返回的节与请求不一致）
)

// rule 是一条故障规则：命令名相同且命令行含 contains 时生效。
type rule struct {
	command  string    // 大写命令名，例如 UID FETCH
	contains string    // 非空时命令行还须含此片段
	kind     faultKind // 故障种类
	bytes    int       // faultFreezeAfter 与 faultCloseAfter 转发的响应字节数；faultWholeBody 声明的字面量字节数
	cut      int       // faultWholeBody 大于 0 时只发出字面量的前 cut 字节，然后关闭两侧连接（TLS 以 close_notify 正常结束）
	exists   []uint32  // faultInject 注入的各条 EXISTS 的 N
	reply    string    // faultReply 原样写出的无标签响应，须含行尾，可以带字面量（见 fetchReply）
	limit    int       // 生效次数上限；0 表示不限
	hook     func()    // 生效时、转发之前调用，例如同时放入一封新邮件
	used     int       // 已生效次数，受 fakeServer.mu 保护
}

// recorded 是代理记录的一条客户端命令；LOGIN 不记录参数。
type recorded struct {
	Tag   string
	Name  string
	Args  string   // 命令名之后的原文
	Items []string // UID FETCH 的数据项
	At    time.Time
}

// proxyOptions 是代理在整个用例中不变的行为。
type proxyOptions struct {
	plaintext      bool          // 不做 TLS，直接转发（143 端口式的明文入口）
	freezeGreeting bool          // 完成 TLS 握手后不转发服务器问候
	dropCaps       []string      // 从问候、LOGIN 响应与 CAPABILITY 响应中删去的能力
	addCaps        []string      // 追加到上述能力列表的能力
	sizeOverride   int64         // 非 0 时把每个 RFC822.SIZE 改为该值
	maxTLSVersion  uint16        // 非 0 时限制代理的最高 TLS 版本
	noJunk         bool          // 不创建 Junk 文件夹
	noSent         bool          // 不创建「已发送」文件夹（FolderSent），LIST 中因此没有它
	noUIDNext      bool          // 从服务器响应中删去报告 UIDNEXT 的行，模拟 EXAMINE 不报告 UIDNEXT 的服务器
	slowHandshake  time.Duration // 第一个连接的 TLS 握手推迟这么久，模拟 LOGIN 在拨号开始很久之后才发出
}

// fakeServer 是 imapmemserver 加故障代理组成的离线假服务器。
type fakeServer struct {
	t    *testing.T
	user *imapmemserver.User
	port int
	pool *x509.CertPool
	opts proxyOptions

	mu       sync.Mutex
	rules    []*rule
	cmds     []recorded
	conns    map[*proxyConn]struct{}
	accepted int
	closed   bool
}

// newFakeServer 启动 imapmemserver（含 INBOX、Junk 与「已发送」，除非 noJunk 或 noSent）与前置代理；用例结束时关闭两者，
// 并断言代理记录的命令名都在白名单内、每个正文数据项都是 BODY.PEEK[]<0.N> 或 BODY.PEEK[HEADER]<0.N>。
func newFakeServer(t *testing.T, opts proxyOptions) *fakeServer {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser(testUser, testPassword)
	mem.AddUser(user)
	folders := []string{FolderInbox}
	if !opts.noJunk {
		folders = append(folders, FolderJunk)
	}
	if !opts.noSent {
		folders = append(folders, FolderSent)
	}
	for _, name := range folders {
		if err := user.Create(name, nil); err != nil {
			t.Fatal(err)
		}
	}
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		// TLS 由前面的代理终结，imapserver 只看到明文连接，默认会公告 LOGINDISABLED 并拒绝认证。
		InsecureAuth: true,
		Logger:       log.New(io.Discard, "", 0),
	})
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(backend) }()

	serverTLS, pool := testCerts(t)
	if opts.maxTLSVersion != 0 {
		// 服务端默认最低版本为 TLS 1.2，须显式放开才能协商出旧版本。
		serverTLS.MinVersion, serverTLS.MaxVersion = tls.VersionTLS10, opts.maxTLSVersion
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeServer{t: t, user: user, port: ln.Addr().(*net.TCPAddr).Port, pool: pool, opts: opts, conns: map[*proxyConn]struct{}{}}
	go fs.serve(ln, backend.Addr().String(), serverTLS)
	t.Cleanup(func() {
		ln.Close()
		fs.mu.Lock()
		fs.closed = true
		fs.mu.Unlock()
		fs.disconnectAll()
		srv.Close()
		for _, cmd := range fs.commands() {
			if !slices.Contains(allowedCommands, cmd.Name) {
				t.Errorf("client sent %q, which is not in the command whitelist", cmd.Name)
			}
			for _, item := range cmd.Items {
				if item != "UID" && item != "RFC822.SIZE" && !strings.HasPrefix(item, "BODY.PEEK[]<0.") && !strings.HasPrefix(item, "BODY.PEEK[HEADER]<0.") {
					t.Errorf("UID FETCH requested %q; want only UID, RFC822.SIZE, BODY.PEEK[]<0.N> and BODY.PEEK[HEADER]<0.N>", item)
				}
			}
		}
	})
	return fs
}

// testCerts 返回 httptest 自签证书的服务端 TLS 配置与只含其根证书的证书池；证书的 SAN 为 127.0.0.1、::1、example.com、*.example.com。
// 服务端配置不设 NextProtos：imapclient 会协商 ALPN "imap"，而 httptest 的 "http/1.1" 会让握手失败。
func testCerts(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	srv.StartTLS()
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return &tls.Config{Certificates: srv.TLS.Certificates}, pool
}

// config 返回连接到本代理的会话参数。
func (fs *fakeServer) config(timeouts Timeouts) Config {
	return Config{Host: "127.0.0.1", Port: fs.port, Username: testUser, RootCAs: fs.pool, Timeouts: timeouts}
}

// serve 接受客户端连接，逐个交给 handle。
func (fs *fakeServer) serve(ln net.Listener, backend string, serverTLS *tls.Config) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		fs.mu.Lock()
		fs.accepted++
		first := fs.accepted == 1
		fs.mu.Unlock()
		go fs.handle(conn, backend, serverTLS, first)
	}
}

// handle 完成 TLS 握手（明文入口除外），连接 imapmemserver，并启动两个方向的转发；first 表示这是接受的第一个连接。
func (fs *fakeServer) handle(raw net.Conn, backend string, serverTLS *tls.Config, first bool) {
	client := raw
	if first {
		time.Sleep(fs.opts.slowHandshake)
	}
	if !fs.opts.plaintext {
		tlsConn := tls.Server(raw, serverTLS)
		_ = tlsConn.SetDeadline(time.Now().Add(waitLimit))
		if err := tlsConn.Handshake(); err != nil {
			raw.Close()
			return
		}
		_ = tlsConn.SetDeadline(time.Time{})
		client = tlsConn
	}
	server, err := net.Dial("tcp", backend)
	if err != nil {
		client.Close()
		return
	}
	pc := &proxyConn{fs: fs, client: client, server: server, budget: -1, frozen: make(chan struct{}), done: make(chan struct{})}
	fs.mu.Lock()
	if fs.closed {
		fs.mu.Unlock()
		client.Close()
		server.Close()
		return
	}
	fs.conns[pc] = struct{}{}
	fs.mu.Unlock()
	if fs.opts.freezeGreeting {
		pc.freeze()
	}
	go pc.clientToServer()
	go pc.serverToClient()
}

// addRule 追加一条故障规则。
func (fs *fakeServer) addRule(r *rule) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.rules = append(fs.rules, r)
}

// clearRules 删除全部故障规则。
func (fs *fakeServer) clearRules() {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.rules = nil
}

// observe 记录一条客户端命令并返回对它生效的规则与标签；不像命令的行（DONE 除外）不记录。
func (fs *fakeServer) observe(line string) (string, *rule) {
	text := strings.TrimRight(line, "\r\n")
	tag, rest, _ := strings.Cut(text, " ")
	name, args, _ := strings.Cut(rest, " ")
	name = strings.ToUpper(name)
	if name == "UID" {
		sub, subArgs, _ := strings.Cut(args, " ")
		name, args = name+" "+strings.ToUpper(sub), subArgs
	}
	switch {
	case text == "DONE":
		tag, name, args = "", "DONE", ""
	case !commandPattern.MatchString(tag + " " + name):
		return "", nil
	case name == "LOGIN":
		args = "" // 从不记录 LOGIN 的参数
	}
	cmd := recorded{Tag: tag, Name: name, Args: args, At: time.Now()}
	if name == "UID FETCH" {
		cmd.Items = fetchItems(args)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.cmds = append(fs.cmds, cmd)
	for _, r := range fs.rules {
		if r.command == name && strings.Contains(text, r.contains) && (r.limit == 0 || r.used < r.limit) {
			r.used++
			return tag, r
		}
	}
	return tag, nil
}

// fetchItems 从 UID FETCH 的参数中取出数据项列表。
func fetchItems(args string) []string {
	_, list, _ := strings.Cut(args, " ")
	return strings.Fields(strings.TrimSuffix(strings.TrimPrefix(list, "("), ")"))
}

// rewrite 按用例选项改写服务器响应行中的能力列表与 RFC822.SIZE。
func (fs *fakeServer) rewrite(line string) string {
	if loc := capabilityPattern.FindStringIndex(line); loc != nil && (len(fs.opts.dropCaps) > 0 || len(fs.opts.addCaps) > 0) {
		start := loc[1]
		end := start + strings.IndexAny(line[start:], "]\r\n")
		var caps []string
		for _, c := range strings.Fields(line[start:end]) {
			if !slices.ContainsFunc(fs.opts.dropCaps, func(d string) bool { return strings.EqualFold(c, d) }) {
				caps = append(caps, c)
			}
		}
		line = line[:start] + strings.Join(append(caps, fs.opts.addCaps...), " ") + line[end:]
	}
	if fs.opts.sizeOverride != 0 {
		line = sizePattern.ReplaceAllString(line, "RFC822.SIZE "+strconv.FormatInt(fs.opts.sizeOverride, 10))
	}
	return line
}

// commands 返回代理迄今记录的命令副本。
func (fs *fakeServer) commands() []recorded {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return slices.Clone(fs.cmds)
}

// count 返回代理记录的某个命令名的条数。
func (fs *fakeServer) count(name string) int {
	n := 0
	for _, cmd := range fs.commands() {
		if cmd.Name == name {
			n++
		}
	}
	return n
}

// acceptedConns 返回代理接受过的 TCP 连接数（含握手失败的连接）。
func (fs *fakeServer) acceptedConns() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.accepted
}

// openConns 返回仍在转发的代理连接数；客户端关闭连接后很快归零。
func (fs *fakeServer) openConns() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return len(fs.conns)
}

// snapshot 返回当前全部代理连接。
func (fs *fakeServer) snapshot() []*proxyConn {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var conns []*proxyConn
	for pc := range fs.conns {
		conns = append(conns, pc)
	}
	return conns
}

// disconnectAll 立即关闭每个代理连接的两侧，模拟服务器超时断开。
func (fs *fakeServer) disconnectAll() {
	for _, pc := range fs.snapshot() {
		pc.close()
	}
}

// freezeAll 冻结每个代理连接两个方向的转发但不关闭连接，模拟半开连接。
func (fs *fakeServer) freezeAll() {
	for _, pc := range fs.snapshot() {
		pc.freeze()
	}
}

// appendMessage 把一封邮件放入文件夹，返回它的 UID；失败即终止用例，只能在测试 goroutine 中调用。
func (fs *fakeServer) appendMessage(folder string, raw []byte) uint32 {
	fs.t.Helper()
	uid, err := fs.put(folder, raw)
	if err != nil {
		fs.t.Fatal(err)
	}
	return uid
}

// put 把一封邮件放入文件夹并返回它的 UID；可在任意 goroutine 中调用。
func (fs *fakeServer) put(folder string, raw []byte) (uint32, error) {
	data, err := fs.user.Append(folder, bytes.NewReader(raw), &goimap.AppendOptions{})
	if err != nil {
		return 0, err
	}
	return uint32(data.UID), nil
}

// status 返回 imapmemserver 中某个文件夹的邮件数、未读数、UIDNEXT 与 UIDVALIDITY。
func (fs *fakeServer) status(folder string) *goimap.StatusData {
	fs.t.Helper()
	data, err := fs.user.Status(folder, &goimap.StatusOptions{NumMessages: true, NumUnseen: true, UIDNext: true, UIDValidity: true})
	if err != nil {
		fs.t.Fatal(err)
	}
	return data
}

// proxyConn 是代理中的一条连接：客户端一侧与 imapmemserver 一侧。
type proxyConn struct {
	fs     *fakeServer
	client net.Conn
	server net.Conn

	wmu    sync.Mutex // 串行化写往客户端的数据，注入的行不会插进其他响应中间
	budget int        // 「响应中途冻结」剩余可转发的字节；小于 0 表示未启用。受 wmu 保护
	cut    bool       // 额度用完时关闭连接而不是冻结（faultCloseAfter）。受 wmu 保护

	freezeOnce sync.Once
	frozen     chan struct{} // 冻结后关闭；此后两个方向的数据都被读出丢弃
	closeOnce  sync.Once
	done       chan struct{} // 连接关闭后关闭
}

// freeze 冻结两个方向的转发。
func (pc *proxyConn) freeze() {
	pc.freezeOnce.Do(func() { close(pc.frozen) })
}

// isFrozen 报告连接是否已冻结。
func (pc *proxyConn) isFrozen() bool {
	select {
	case <-pc.frozen:
		return true
	default:
		return false
	}
}

// close 关闭两侧连接并从代理中移除。
func (pc *proxyConn) close() {
	pc.closeOnce.Do(func() {
		close(pc.done)
		pc.client.Close()
		pc.server.Close()
		pc.fs.mu.Lock()
		delete(pc.fs.conns, pc)
		pc.fs.mu.Unlock()
	})
}

// arm 启用「响应中途冻结」：此后写往客户端的数据累计达到 n 字节即冻结；cut 为真时改为关闭两侧连接。
func (pc *proxyConn) arm(n int, cut bool) {
	pc.wmu.Lock()
	defer pc.wmu.Unlock()
	pc.budget, pc.cut = n, cut
}

// send 把数据写给客户端；冻结后丢弃。启用「响应中途冻结」（arm）时只写出剩余额度，然后冻结；
// cut 为真（faultCloseAfter）时写出剩余额度后改为关闭两侧连接，客户端读完这些字节后读到 close_notify 带来的 io.EOF。
func (pc *proxyConn) send(b []byte) error {
	pc.wmu.Lock()
	defer pc.wmu.Unlock()
	if pc.isFrozen() {
		return nil
	}
	if pc.budget >= 0 {
		if len(b) >= pc.budget {
			b = b[:pc.budget]
			if pc.cut {
				// 写出剩余额度后关闭：客户端一侧的 TLS 连接发出 close_notify，客户端读完这些字节后读到 io.EOF。
				_, err := pc.client.Write(b)
				pc.freeze()
				pc.close()
				return err
			}
			pc.freeze()
		} else {
			pc.budget -= len(b)
		}
	}
	_, err := pc.client.Write(b)
	return err
}

// clientToServer 逐行读取客户端命令，记录并按规则处理后转发给 imapmemserver；冻结后读出丢弃。
func (pc *proxyConn) clientToServer() {
	defer pc.close()
	br := bufio.NewReader(pc.client)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		if pc.isFrozen() {
			continue
		}
		tag, r := pc.fs.observe(line)
		if r != nil {
			if r.hook != nil {
				r.hook()
			}
			switch r.kind {
			case faultBAD:
				err = pc.send([]byte("* BAD Command!\r\n"))
			case faultReject:
				err = pc.send([]byte(tag + " NO [UNAVAILABLE] Folder is temporarily unavailable\r\n"))
			case faultRejectBAD:
				err = pc.send([]byte(tag + " BAD Command is not supported for this mailbox\r\n"))
			case faultEmpty:
				err = pc.send([]byte(tag + " OK completed\r\n"))
			case faultReply:
				err = pc.send([]byte(r.reply + tag + " OK completed\r\n"))
			case faultFreeze:
				pc.freeze()
			case faultDisconnect:
				return
			case faultRejectClose:
				_ = pc.send([]byte(tag + " NO [AUTHENTICATIONFAILED] Invalid credentials\r\n"))
				return
			case faultWholeBody:
				err = pc.sendWholeBody(tag, line, r.bytes, r.cut)
			case faultInject:
				for _, n := range r.exists {
					if err = pc.send(fmt.Appendf(nil, "* %d EXISTS\r\n", n)); err != nil {
						return
					}
				}
			case faultFreezeAfter:
				pc.arm(r.bytes, false)
			case faultCloseAfter:
				pc.arm(r.bytes, true)
			}
			if err != nil {
				return
			}
			if r.kind != faultInject && r.kind != faultFreezeAfter && r.kind != faultCloseAfter {
				continue
			}
		}
		if _, err := io.WriteString(pc.server, line); err != nil {
			return
		}
	}
}

// sendWholeBody 代替服务器回应 UID FETCH 命令行 line：无视请求的部分取回，回一条字面量声明为 n 字节的 FETCH 响应，再回 <标签> OK；
// 数据项的节与请求相同（取头部时为 BODY[HEADER]，否则为 BODY[]）。cut 大于 0 时只发出字面量的前 cut 字节，然后关闭两侧连接
// （TLS 以 close_notify 正常结束），模拟字面量传到一半时服务器正常关闭连接。字面量由代理分块合成，不经 imapmemserver，
// 服务端不因此分配大块内存。
func (pc *proxyConn) sendWholeBody(tag, line string, n, cut int) error {
	uid, section := strings.Fields(line)[3], "[]"
	if strings.Contains(line, "BODY.PEEK[HEADER]") {
		section = "[HEADER]"
	}
	if err := pc.send(fmt.Appendf(nil, "* 1 FETCH (UID %s BODY%s {%d}\r\n", uid, section, n)); err != nil {
		return err
	}
	if cut > 0 {
		n = min(n, cut)
	}
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	for n > 0 {
		k := min(n, len(chunk))
		if err := pc.send(chunk[:k]); err != nil {
			return err
		}
		n -= k
	}
	if cut > 0 {
		pc.close()
		return net.ErrClosed
	}
	return pc.send([]byte(")\r\n" + tag + " OK FETCH completed\r\n"))
}

// serverToClient 逐行转发 imapmemserver 的响应，字面量按声明的长度原样转发；响应行按用例选项改写，noUIDNext 时删去报告 UIDNEXT 的行。
func (pc *proxyConn) serverToClient() {
	defer pc.close()
	br := bufio.NewReader(pc.server)
	buf := make([]byte, 4096)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		if pc.fs.opts.noUIDNext && uidNextPattern.MatchString(line) {
			continue
		}
		if err := pc.send([]byte(pc.fs.rewrite(line))); err != nil {
			return
		}
		n := 0
		if m := literalPattern.FindStringSubmatch(line); m != nil {
			n, _ = strconv.Atoi(m[1])
		}
		for n > 0 {
			k, err := br.Read(buf[:min(n, len(buf))])
			if err != nil {
				return
			}
			if err := pc.send(buf[:k]); err != nil {
				return
			}
			n -= k
		}
	}
}

// waitFor 轮询 cond 直到成立，超过 waitLimit 即让用例失败。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	waitForWithin(t, what, waitLimit, cond)
}

// waitForWithin 轮询 cond 直到成立，超过 limit 即让用例失败。
func waitForWithin(t *testing.T, what string, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// testMessage 构造第 n 封合成邮件，总长至少 size 字节，行尾为 CRLF。
func testMessage(n, size int) []byte {
	head := fmt.Sprintf("From: user@example.invalid\r\nTo: %s\r\nSubject: message %d\r\nMessage-ID: <m%d@example.invalid>\r\n\r\n", testUser, n, n)
	body := "body\r\n"
	if pad := size - len(head) - 2; pad > len(body) {
		body = strings.Repeat("x", pad) + "\r\n"
	}
	return []byte(head + body)
}

// messageWithHeader 构造第 n 封合成邮件：头部（含结束头部的空行）恰为 headerSize 字节，正文为 bodySize 字节，行尾为 CRLF。
// 头部以若干行 X-Padding 补足，每行至多 80 字节，符合行长限制；headerSize 须比基本头部至少多 16 字节，bodySize 至少为 2。
func messageWithHeader(n, headerSize, bodySize int) []byte {
	var h strings.Builder
	fmt.Fprintf(&h, "From: user@example.invalid\r\nTo: %s\r\nSubject: message %d\r\nMessage-ID: <m%d@example.invalid>\r\n", testUser, n, n)
	const prefix, minLine = "X-Padding: ", 14 // 一行至少是前缀、一个字符与 CRLF
	for rest := headerSize - h.Len() - 2; rest > 0; {
		line := min(rest, 80)
		if left := rest - line; left > 0 && left < minLine {
			line = rest - minLine // 给最后一行留出最短的长度
		}
		h.WriteString(prefix + strings.Repeat("x", line-len(prefix)-2) + "\r\n")
		rest -= line
	}
	h.WriteString("\r\n")
	return []byte(h.String() + strings.Repeat("y", bodySize-2) + "\r\n")
}

// headerOf 返回合成邮件的头部：到结束头部的空行为止（含空行），即 BODY[HEADER] 应返回的字节。
func headerOf(raw []byte) []byte {
	return raw[:bytes.Index(raw, []byte("\r\n\r\n"))+4]
}

// fetchReply 构造供 faultReply 写出的一条 FETCH 响应：UID 1 的数据项 item（例如 BODY[] 或 BODY[HEADER]<5>）带字面量 data。
func fetchReply(item string, data []byte) string {
	return fmt.Sprintf("* 1 FETCH (UID 1 %s {%d}\r\n%s)\r\n", item, len(data), data)
}
