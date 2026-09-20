//go:build live

// Package live_test 的 L1 探测开关与共用辅助：live 构建标签与 TURNCOURIER_LIVE=1 的双重开关、CI 下拒绝运行、
// 访问钥匙串与网络之前经 /dev/tty 的总确认，以及配置、实例 ID、授权码、输出目录与探测状态的读取。
// 本文件只在 live 构建标签下编译，由维护者在本机人工执行；CI 只做编译检查，从不运行。
package live_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	// 导入时注册纯 Go 的 "sqlite" 驱动，用于只读查询实例 ID；探测不调用 sqlite.Open，不执行迁移与检查点。
	_ "modernc.org/sqlite"

	"github.com/chaoRookie/turncourier/internal/config"
	"github.com/chaoRookie/turncourier/internal/mail/imap"
	"github.com/chaoRookie/turncourier/internal/mail/smtp"
	"github.com/chaoRookie/turncourier/internal/security/keychain"
	"github.com/chaoRookie/turncourier/tests/live"
)

// 探测工具读取的环境变量。
const (
	liveEnv         = "TURNCOURIER_LIVE"
	ciEnv           = "CI"
	outEnv          = "TURNCOURIER_LIVE_OUT"
	sendCountEnv    = "TURNCOURIER_LIVE_SEND_COUNT"
	ccBotEnv        = "TURNCOURIER_LIVE_CC_BOT"
	keepEnv         = "TURNCOURIER_LIVE_KEEP"
	idleMinutesEnv  = "TURNCOURIER_LIVE_IDLE_MINUTES"
	idleSelfSendEnv = "TURNCOURIER_LIVE_IDLE_SELF_SEND"
)

const (
	// sentFolder 是 QQ 的「已发送」文件夹名。
	sentFolder = "Sent Messages"
	// probeOwner 是合成令牌中的 owner，只出现在探测进程内。
	probeOwner = "l1-probe"
	// idAlphabet 是合成任务 ID、探测 ID 与我方 Message-ID 使用的小写 Crockford base32 字母表。
	idAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	// minSchemaVersion 是探测要求的数据库迁移版本。
	minSchemaVersion = 2
)

// probeNames 是全部探测项；总确认会列出本次 -run 选中的项。
var probeNames = []string{
	"TestL1KeychainRoundTrip",
	"TestL1Capabilities",
	"TestL1SendNotification",
	"TestL1Replies",
	"TestL1Idle",
}

// uriEscaper 转义 SQLite URI 路径中有特殊含义的字符，与 store/sqlite 的处理一致。
var uriEscaper = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23")

// probeInput 是 TestMain 解析出的共用输入：配置与已校验的输出目录真实路径。
type probeInput struct {
	cfg config.Config
	out string
}

// input 是本次运行的共用输入，各探测项只读它。
var input probeInput

// cachedAuthCode 缓存本次运行读出的授权码，避免每个探测项都访问一次钥匙串。
var cachedAuthCode string

// TestMain 执行双重开关与总确认：TURNCOURIER_LIVE 必须恰为 1，否则打印跳过并以 0 退出；CI 非空时拒绝运行并以 1 退出。
// 通过后读取配置与输出目录（不访问钥匙串与网络），再经 /dev/tty 做一次总确认，回答不是 yes 即取消。
func TestMain(m *testing.M) {
	if os.Getenv(liveEnv) != "1" {
		fmt.Fprintln(os.Stderr, "跳过：未设置 TURNCOURIER_LIVE=1")
		os.Exit(0)
	}
	if os.Getenv(ciEnv) != "" {
		fmt.Fprintln(os.Stderr, "拒绝在 CI 中运行")
		os.Exit(1)
	}
	flag.Parse()
	if err := setup(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	agreed, err := ask(overview())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if !agreed {
		fmt.Fprintln(os.Stderr, "已取消")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// setup 解析配置与输出目录：使用与产品相同的路径解析，输出目录必须是仓库之外、权限恰为 0700 的已存在目录。
// 本函数不访问钥匙串与网络。
func setup() error {
	paths, err := config.ResolvePaths(os.Getenv, os.UserConfigDir)
	if err != nil {
		return fmt.Errorf("解析路径失败：%w", err)
	}
	cfg, err := config.Load(paths, os.Getenv)
	if err != nil {
		return fmt.Errorf("读取配置失败：%w", err)
	}
	workDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("读取工作目录失败：%w", err)
	}
	root, err := live.RepoRoot(workDir)
	if err != nil {
		return fmt.Errorf("查找仓库根目录失败：%w", err)
	}
	out, err := live.ValidateOutputDir(os.Getenv(outEnv), root)
	if err != nil {
		return fmt.Errorf("%s 无效：%w", outEnv, err)
	}
	input = probeInput{cfg: cfg, out: out}
	return nil
}

// overview 渲染总确认的文本：机器人账户、接收地址、输出目录与本次选中的探测项。
func overview() string {
	return fmt.Sprintf("即将开始 TurnCourier 的 L1 真机探测：\n  机器人账户：%s\n  接收地址：%s\n  输出目录：%s\n  本次探测项：%s\n"+
		"探测会读取登录钥匙串中的授权码并连接真实邮箱；每封邮件发出前还会再次确认。",
		input.cfg.Mailbox.Address, input.cfg.Recipient.Address, input.out, strings.Join(selectedProbes(), "、"))
}

// selectedProbes 按 -run 的正则筛选本次要运行的探测项；没有 -run、正则无效或没有匹配项时返回全部，确认文本不会漏列。
func selectedProbes() []string {
	pattern := ""
	if f := flag.Lookup("test.run"); f != nil {
		pattern, _, _ = strings.Cut(f.Value.String(), "/")
	}
	if pattern == "" {
		return probeNames
	}
	filter, err := regexp.Compile(pattern)
	if err != nil {
		return probeNames
	}
	var selected []string
	for _, name := range probeNames {
		if filter.MatchString(name) {
			selected = append(selected, name)
		}
	}
	if len(selected) == 0 {
		return probeNames
	}
	return selected
}

// ttyLine 经 /dev/tty 显示 text 并读入一行，返回去掉首尾空白的输入；没有控制终端时返回错误，探测随即失败。
func ttyLine(text string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("打不开 /dev/tty：探测必须在控制终端上人工确认")
	}
	defer tty.Close()
	if _, err := fmt.Fprintf(tty, "\n%s", text); err != nil {
		return "", errors.New("无法写入 /dev/tty")
	}
	line, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("无法从 /dev/tty 读入确认")
	}
	return strings.TrimSpace(line), nil
}

// ask 显示 text 并要求输入 yes；回答其他内容返回 false，调用方据此跳过该项。
func ask(text string) (bool, error) {
	line, err := ttyLine(text + "\n输入 yes 继续：")
	return line == "yes", err
}

// confirm 是 ask 的用例版：没有控制终端时直接让用例失败。
func confirm(t *testing.T, text string) bool {
	t.Helper()
	agreed, err := ask(text)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return agreed
}

// probePassword 读出本实例的 QQ 授权码并在本次运行中缓存；首次调用会访问真实钥匙串，可能弹出解锁提示。
func probePassword(t *testing.T) string {
	t.Helper()
	if cachedAuthCode != "" {
		return cachedAuthCode
	}
	id, err := instanceID(input.cfg.Paths.Database)
	if err != nil {
		t.Fatalf("读取实例 ID 失败：%v", err)
	}
	store, err := keychain.New(keychain.InteractiveTimeout)
	if err != nil {
		t.Fatalf("无法使用钥匙串：%v", err)
	}
	code, err := store.Get(context.Background(), keychain.AuthCodeAccount(id))
	if err != nil {
		t.Fatalf("读取授权码失败：%v", err)
	}
	cachedAuthCode = code
	return code
}

// instanceID 以只读方式打开数据库并读取实例 ID：先确认数据库文件存在，再确认 user_version 不低于 2，
// 然后直接查询 instance 表。不调用 sqlite.Open，因为它会创建数据目录与数据库、执行迁移与检查点。
func instanceID(path string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", errors.New("数据库不存在，请先运行 turncourier init")
	}
	db, err := sql.Open("sqlite", "file:"+uriEscaper.Replace(path)+"?mode=ro")
	if err != nil {
		return "", fmt.Errorf("打开数据库失败：%w", err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return "", fmt.Errorf("读取数据库版本失败：%w", err)
	}
	if version < minSchemaVersion {
		return "", fmt.Errorf("数据库版本为 %d，低于探测要求的 %d，请先运行 turncourier init", version, minSchemaVersion)
	}
	var id string
	if err := db.QueryRow("SELECT instance_id FROM instance WHERE singleton = 1").Scan(&id); err != nil {
		return "", fmt.Errorf("读取实例 ID 失败：%w", err)
	}
	return id, nil
}

// imapConfig 按配置构造 IMAP 参数；TLS 模式固定为隐式 TLS，timeouts 的零值字段使用包内默认值。
func imapConfig(timeouts imap.Timeouts) imap.Config {
	return imap.Config{
		Host:     input.cfg.Mailbox.IMAPHost,
		Port:     input.cfg.Mailbox.IMAPPort,
		Username: input.cfg.Mailbox.Address,
		Timeouts: timeouts,
	}
}

// smtpConfig 按配置构造 SMTP 参数；TLS 模式固定为隐式 TLS。
func smtpConfig() smtp.Config {
	return smtp.Config{
		Host:     input.cfg.Mailbox.SMTPHost,
		Port:     input.cfg.Mailbox.SMTPPort,
		Username: input.cfg.Mailbox.Address,
	}
}

// dialIMAP 用钥匙串中的授权码登录 IMAP，并在用例结束时关闭连接；连接已关闭时 Close 不做任何事。
func dialIMAP(ctx context.Context, t *testing.T, timeouts imap.Timeouts) *imap.Session {
	t.Helper()
	session, err := imap.Dial(ctx, imapConfig(timeouts), probePassword(t))
	if err != nil {
		t.Fatalf("登录 IMAP 失败：%v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// loadState 读取输出目录中的探测状态。
func loadState(t *testing.T) live.State {
	t.Helper()
	state, err := live.LoadState(input.out)
	if err != nil {
		t.Fatalf("读取探测状态失败：%v", err)
	}
	return state
}

// saveState 写回探测状态；写入失败直接让用例失败，避免已发出邮件的 ID 与令牌丢失。
func saveState(t *testing.T, state live.State) {
	t.Helper()
	if err := live.SaveState(input.out, state); err != nil {
		t.Fatalf("写入探测状态失败：%v", err)
	}
}

// record 把一条非来信记录追加到样本文件。
func record(t *testing.T, kind string, data any) {
	t.Helper()
	if err := live.AppendSample(input.out, live.Record{Schema: live.Schema, Kind: kind, Data: data}); err != nil {
		t.Fatalf("写入样本失败：%v", err)
	}
}

// randomID 返回 n 个小写 Crockford base32 字符，用于合成任务 ID、探测 ID、我方 Message-ID 与合成 account。
func randomID(t *testing.T, n int) string {
	t.Helper()
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("读取随机数失败：%v", err)
	}
	out := make([]byte, n)
	for i, b := range raw {
		out[i] = idAlphabet[int(b)%len(idAlphabet)]
	}
	return string(out)
}

// randomSecret 返回 43 个 base64url 字符的合成机密，格式与 Keychain 中的密钥文本相同，只用于合成条目。
func randomSecret(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("读取随机数失败：%v", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// intEnv 读取取值为整数的环境变量：未设置时用 def，取值不是整数或不在 [low, high] 内时让用例失败。
func intEnv(t *testing.T, name string, def, low, high int) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < low || value > high {
		t.Fatalf("%s 必须是 %d 到 %d 之间的整数", name, low, high)
	}
	return value
}

// probeTimeout 返回一次探测的总期限，供各用例构造 ctx。
func probeTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
