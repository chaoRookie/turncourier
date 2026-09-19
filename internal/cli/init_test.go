// Package cli 的 init 测试用脚本化终端、以 map 实现的 Keychain 替身与临时目录驱动完整流程，
// 从不访问真实钥匙串、终端或用户配置目录；授权码与密钥一律在运行时构造。
package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/chaoRookie/turncourier/internal/config"
	"github.com/chaoRookie/turncourier/internal/security/keychain"
	"github.com/chaoRookie/turncourier/internal/store/sqlite"
)

// 测试用的地址与 init 的提示原文；提示在测试中逐字断言。
const (
	botAddress     = "bot@example.invalid"
	meAddress      = "me@example.invalid"
	promptMailbox  = "机器人邮箱地址（专用 QQ 邮箱，用于发送通知）："
	promptReceiver = "接收通知的邮箱地址："
	promptSenders  = "允许回复的发件人（逗号分隔，直接回车表示只允许接收通知的地址）："
	promptReplace  = "Keychain 中已有授权码，是否替换？[y/N]："
	promptCode     = "QQ 邮箱授权码（输入时不显示）："
	promptConfirm  = "再次输入授权码："
)

// 摘要之后的两段固定说明，逐字断言。
const (
	wantThreatNote = "注意：授权码与密钥保存在登录钥匙串中，同一用户下的任何进程（包括 Agent 执行的命令）都能读取；读到它们的进程可以伪造通过全部校验的回复、把任意内容注入任一任务，也可以直接改写本地队列。TurnCourier 不防同一用户下的进程。请只使用专用的机器人邮箱；怀疑泄露时，请在 QQ 邮箱中停用该授权码。"
	wantBackupNote = "待投递的回复与通知会加密暂存在数据目录中，处理完成后删除；APFS 快照与 Time Machine 备份中可能留有已删除数据的密文副本，正文密钥仍在 Keychain 中时它们可以被解密。不要把数据目录恢复到旧的备份，否则已发出的通知可能重发、已确认的回复可能再次派发。当前版本尚不能收发邮件。"
)

// fixedNow 是测试时钟的固定时刻。
var fixedNow = time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)

// canary 返回授权码金丝雀，在运行时构造，源码中没有机密形状的字面量。
func canary() string { return strings.Repeat("canary", 3) }

// keyText 返回由 fill 重复 32 次构成的密钥文本（43 个 base64url 字符），在运行时构造。
func keyText(fill byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

// countingReader 是固定随机源：依次产生 0、1、2…（按字节回绕），每次运行 init 的实例 ID 与密钥因此可重现。
type countingReader struct{ next byte }

// Read 用递增的字节填满 p。
func (r *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.next
		r.next++
	}
	return len(p), nil
}

// scriptTerminal 按脚本依次返回回答，分别记录 ReadLine 与 ReadSecret 的提示；脚本用尽时返回错误。
type scriptTerminal struct {
	interactive    bool
	lines          []string
	secrets        []string
	linePrompts    []string
	secretPrompts  []string
	interactiveHit int
	// onSecret 非 nil 时在每次 ReadSecret 开始时调用，用来模拟 Ctrl-C 或其他进程的并发写入；返回错误时 ReadSecret 返回它。
	onSecret func() error
}

// errScriptExhausted 表示脚本中的回答已经用完。
var errScriptExhausted = errors.New("script exhausted")

// Interactive 返回脚本设定的终端状态并计数。
func (s *scriptTerminal) Interactive() bool {
	s.interactiveHit++
	return s.interactive
}

// ReadLine 记录提示并返回下一行回答。
func (s *scriptTerminal) ReadLine(_ context.Context, prompt string) (string, error) {
	s.linePrompts = append(s.linePrompts, prompt)
	if len(s.lines) == 0 {
		return "", errScriptExhausted
	}
	line := s.lines[0]
	s.lines = s.lines[1:]
	return line, nil
}

// ReadSecret 记录提示，先运行 onSecret，再返回下一条机密回答。
func (s *scriptTerminal) ReadSecret(_ context.Context, prompt string) (string, error) {
	s.secretPrompts = append(s.secretPrompts, prompt)
	if s.onSecret != nil {
		if err := s.onSecret(); err != nil {
			return "", err
		}
	}
	if len(s.secrets) == 0 {
		return "", errScriptExhausted
	}
	secret := s.secrets[0]
	s.secrets = s.secrets[1:]
	return secret, nil
}

// fakeKeychain 是以 map 实现的 keychain.Store 替身：分别统计各方法的调用次数，可注入 Get、Set 与 Add 的错误；
// beforeAdd 在 Add 检查条目之前运行，用来模拟并发的另一个写入者。
type fakeKeychain struct {
	items                     map[string]string
	gets, sets, adds, deletes int
	getErr                    func(account string) error
	setErr, addErr            error
	beforeAdd                 func(account string)
}

// Get 返回条目内容；getErr 对该 account 返回错误时返回它，不存在时返回 keychain.ErrNotFound。
func (k *fakeKeychain) Get(_ context.Context, account string) (string, error) {
	k.gets++
	if k.getErr != nil {
		if err := k.getErr(account); err != nil {
			return "", err
		}
	}
	value, ok := k.items[account]
	if !ok {
		return "", keychain.ErrNotFound
	}
	return value, nil
}

// Set 创建或覆盖条目；注入了 setErr 时不写入并返回它。
func (k *fakeKeychain) Set(_ context.Context, account, secret string) error {
	k.sets++
	if k.setErr != nil {
		return k.setErr
	}
	k.items[account] = secret
	return nil
}

// Add 只创建新条目：已存在时返回 keychain.ErrExists 且不改动；注入了 addErr 时不写入并返回它。
func (k *fakeKeychain) Add(_ context.Context, account, secret string) error {
	k.adds++
	if k.beforeAdd != nil {
		k.beforeAdd(account)
	}
	if k.addErr != nil {
		return k.addErr
	}
	if _, ok := k.items[account]; ok {
		return keychain.ErrExists
	}
	k.items[account] = secret
	return nil
}

// Delete 删除条目；不存在时返回 keychain.ErrNotFound。
func (k *fakeKeychain) Delete(_ context.Context, account string) error {
	k.deletes++
	if _, ok := k.items[account]; !ok {
		return keychain.ErrNotFound
	}
	delete(k.items, account)
	return nil
}

// calls 返回全部方法的调用次数之和。
func (k *fakeKeychain) calls() int { return k.gets + k.sets + k.adds + k.deletes }

// resetCounts 清零调用计数，保留条目与注入的行为。
func (k *fakeKeychain) resetCounts() { k.gets, k.sets, k.adds, k.deletes = 0, 0, 0, 0 }

// initEnv 是一次 init 测试的环境：TURNCOURIER_CONFIG 与 TURNCOURIER_DATA_DIR 指向临时目录下尚不存在的子目录，
// Keychain 替身在多次运行之间保留条目。
type initEnv struct {
	root        string
	configFile  string
	dataDir     string
	env         map[string]string
	keychain    *fakeKeychain
	keychainErr error
}

// newInitEnv 创建空的测试环境。
func newInitEnv(t *testing.T) *initEnv {
	t.Helper()
	root := t.TempDir()
	e := &initEnv{
		root:       root,
		configFile: filepath.Join(root, "config", "turncourier.toml"),
		dataDir:    filepath.Join(root, "data"),
		keychain:   &fakeKeychain{items: map[string]string{}},
	}
	e.env = map[string]string{"TURNCOURIER_CONFIG": e.configFile, "TURNCOURIER_DATA_DIR": e.dataDir}
	return e
}

// deps 返回注入替身的 InitDeps：随机源每次从 0 开始计数，时钟固定，不允许查询用户配置目录。
func (e *initEnv) deps(t *testing.T, terminal Terminal) InitDeps {
	return InitDeps{
		Terminal: terminal,
		Keychain: func() (keychain.Store, error) {
			if e.keychainErr != nil {
				return nil, e.keychainErr
			}
			return e.keychain, nil
		},
		Getenv: func(key string) string { return e.env[key] },
		UserConfigDir: func() (string, error) {
			t.Error("设置了 TURNCOURIER_CONFIG 时不应查询用户配置目录")
			return "", errors.New("unexpected call")
		},
		Random: &countingReader{},
		Now:    func() time.Time { return fixedNow },
	}
}

// run 以 ctx 与给定终端运行 init，返回退出码与标准输出、标准错误。
func (e *initEnv) run(t *testing.T, ctx context.Context, terminal Terminal) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(ctx, []string{"init"}, &stdout, &stderr, Deps{Init: e.deps(t, terminal)})
	return code, stdout.String(), stderr.String()
}

// firstRunTerminal 返回首次运行的标准回答：机器人地址、接收地址、空白名单与两次相同的授权码。
func firstRunTerminal(code string) *scriptTerminal {
	return &scriptTerminal{interactive: true, lines: []string{botAddress, meAddress, ""}, secrets: []string{code, code}}
}

// mustFirstRun 以金丝雀授权码完成一次首次运行并返回实例 ID，失败即终止测试。
func (e *initEnv) mustFirstRun(t *testing.T) string {
	t.Helper()
	if code, stdout, stderr := e.run(t, t.Context(), firstRunTerminal(canary())); code != 0 {
		t.Fatalf("首次运行失败: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	return e.instanceID(t)
}

// openStore 打开测试环境的数据目录，测试结束时关闭。
func (e *initEnv) openStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(t.Context(), e.dataDir, sqlite.Options{Random: &countingReader{}, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// instanceID 读取数据库中的实例 ID。
func (e *initEnv) instanceID(t *testing.T) string {
	t.Helper()
	id, err := e.openStore(t).InstanceID(t.Context())
	if err != nil {
		t.Fatalf("读取实例 ID 失败: %v", err)
	}
	return id
}

// requireNoLeak 断言输出不含授权码金丝雀、Keychain 替身中的任何条目内容与临时目录路径。
func (e *initEnv) requireNoLeak(t *testing.T, outputs ...string) {
	t.Helper()
	forbidden := []string{canary(), e.root}
	for _, value := range e.keychain.items {
		forbidden = append(forbidden, value)
	}
	for _, output := range outputs {
		for _, value := range forbidden {
			if strings.Contains(output, value) {
				t.Errorf("输出包含机密或本机路径: %q", output)
			}
		}
	}
}

// requireKeyRegistered 断言 purpose 用途恰以 kid 1 登记为 active，校验值等于 Keychain 替身中对应条目算出的值。
func (e *initEnv) requireKeyRegistered(t *testing.T, store *sqlite.Store, id string, purpose sqlite.KeyPurpose) {
	t.Helper()
	account := keychain.TokenKeyAccount(id, 1)
	if purpose == sqlite.KeyPurposePayload {
		account = keychain.PayloadKeyAccount(id, 1)
	}
	key, err := keychain.DecodeKeyText(e.keychain.items[account])
	if err != nil {
		t.Fatalf("%s 条目不是合法的密钥文本: %v", purpose, err)
	}
	if kid, err := store.ActiveKeyID(t.Context(), purpose); err != nil || kid != 1 {
		t.Errorf("ActiveKeyID(%s) = %d, %v; want 1", purpose, kid, err)
	}
	if state, err := store.KeyStateOf(t.Context(), purpose, 1); err != nil || state != sqlite.KeyActive {
		t.Errorf("KeyStateOf(%s, 1) = %q, %v; want active", purpose, state, err)
	}
	if check, err := store.KeyCheckOf(t.Context(), purpose, 1); err != nil || check != keychain.KeyCheck(string(purpose), 1, key) {
		t.Errorf("KeyCheckOf(%s, 1) 与 Keychain 中的密钥不符: %v", purpose, err)
	}
}

// requireNotExist 断言 path 不存在。
func requireNotExist(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s 不应存在: %v", what, err)
	}
}

// requireContains 断言 output 含全部 wants。
func requireContains(t *testing.T, output string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(output, want) {
			t.Errorf("输出缺少 %q: %q", want, output)
		}
	}
}

// TestInitFirstRun 验证首次运行：配置文件等于 Render 的结果且权限 0600，数据库有实例行与两条 active 密钥元数据，
// Keychain 恰有授权码与两把 43 字符的密钥，输出含摘要与两段固定说明且不泄露机密或路径；授权码只经 ReadSecret 读入。
func TestInitFirstRun(t *testing.T) {
	e := newInitEnv(t)
	terminal := firstRunTerminal(canary())
	code, stdout, stderr := e.run(t, t.Context(), terminal)
	if code != 0 || stderr != "" {
		t.Fatalf("init 失败: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	want, err := config.Render(config.Draft{MailboxAddress: botAddress, RecipientAddress: meAddress, AllowedSenders: []string{meAddress}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(e.configFile); err != nil || !bytes.Equal(got, want) {
		t.Errorf("配置文件 = %q, %v; want %q", got, err, want)
	}
	if info, err := os.Stat(e.configFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("配置文件权限不是 0600: %v, %v", info, err)
	}

	store := e.openStore(t)
	id, err := store.InstanceID(t.Context())
	if err != nil || len(id) != 16 {
		t.Fatalf("实例 ID = %q, %v", id, err)
	}
	wantAccounts := []string{keychain.AuthCodeAccount(id), keychain.PayloadKeyAccount(id, 1), keychain.TokenKeyAccount(id, 1)}
	var accounts []string
	for account := range e.keychain.items {
		accounts = append(accounts, account)
	}
	slices.Sort(accounts)
	slices.Sort(wantAccounts)
	if !slices.Equal(accounts, wantAccounts) {
		t.Fatalf("Keychain 条目 = %v; want %v", accounts, wantAccounts)
	}
	if e.keychain.items[keychain.AuthCodeAccount(id)] != canary() {
		t.Error("授权码条目不等于录入的授权码")
	}
	tokenText, payloadText := e.keychain.items[keychain.TokenKeyAccount(id, 1)], e.keychain.items[keychain.PayloadKeyAccount(id, 1)]
	if len(tokenText) != 43 || len(payloadText) != 43 || tokenText == payloadText {
		t.Errorf("两把密钥应为不同的 43 字符文本: %d, %d", len(tokenText), len(payloadText))
	}
	if e.keychain.sets != 1 || e.keychain.adds != 2 {
		t.Errorf("Set = %d, Add = %d; want 1, 2", e.keychain.sets, e.keychain.adds)
	}
	e.requireKeyRegistered(t, store, id, sqlite.KeyPurposeToken)
	e.requireKeyRegistered(t, store, id, sqlite.KeyPurposePayload)

	requireContains(t, stdout,
		"配置文件：已创建（TURNCOURIER_CONFIG 指定的文件）\n",
		"数据目录：TURNCOURIER_DATA_DIR 指定的目录\n",
		"实例 ID："+id+"\n",
		"授权码：已保存\n",
		"令牌签名密钥：已生成\n",
		"正文加密密钥：已生成\n",
		wantThreatNote, wantBackupNote)
	e.requireNoLeak(t, stdout, stderr)

	if !slices.Equal(terminal.linePrompts, []string{promptMailbox, promptReceiver, promptSenders}) {
		t.Errorf("ReadLine 提示 = %q", terminal.linePrompts)
	}
	if !slices.Equal(terminal.secretPrompts, []string{promptCode, promptConfirm}) {
		t.Errorf("ReadSecret 提示 = %q", terminal.secretPrompts)
	}
}

// TestInitDefaultPaths 验证未设置环境变量时按用户配置目录解析路径，摘要给出相对描述而不是绝对路径。
func TestInitDefaultPaths(t *testing.T) {
	e := newInitEnv(t)
	e.env = nil
	deps := e.deps(t, firstRunTerminal(canary()))
	deps.UserConfigDir = func() (string, error) { return e.root, nil }
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), []string{"init"}, &stdout, &stderr, Deps{Init: deps}); code != 0 {
		t.Fatalf("init 失败: code=%d stderr=%q", code, stderr.String())
	}
	requireContains(t, stdout.String(),
		"配置文件：已创建（用户配置目录下的 TurnCourier/turncourier.toml）\n",
		"数据目录：配置文件所在的目录\n")
	if _, err := os.Stat(filepath.Join(e.root, "TurnCourier", "turncourier.toml")); err != nil {
		t.Errorf("默认位置没有配置文件: %v", err)
	}
	e.requireNoLeak(t, stdout.String(), stderr.String())
}

// TestInitRerun 验证重复运行：不再询问地址，配置文件字节与修改时间不变，密钥保持不变；
// 只有回答 y 或 Y 才重新录入授权码并只调用一次 Set，超过 1 个字符的回答按 N 处理并提示，且不回显该回答。
func TestInitRerun(t *testing.T) {
	cases := []struct {
		name    string
		answer  string
		replace bool
		warned  bool
	}{
		{"空行", "", false, false},
		{"n", "n", false, false},
		{"N", "N", false, false},
		{"其他单个字符", "x", false, false},
		{"y", "y", true, false},
		{"Y", "Y", true, false},
		{"yes", "yes", false, true},
		{"粘贴了授权码", canary(), false, true},
		// 只比较去掉结尾换行后的原样回答，不去掉空白：带空白的 y 按 N 处理并提示。
		{"前导空格", " y", false, true},
		{"结尾空格", "y ", false, true},
		{"结尾制表符", "Y\t", false, true},
		// 按字符而不是字节计数：单个多字节字符按 N 处理，不提示。
		{"单个多字节字符", "是", false, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			e := newInitEnv(t)
			id := e.mustFirstRun(t)
			before := maps.Clone(e.keychain.items)
			old := time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
			if err := os.Chtimes(e.configFile, old, old); err != nil {
				t.Fatal(err)
			}
			configBefore, err := os.ReadFile(e.configFile)
			if err != nil {
				t.Fatal(err)
			}
			e.keychain.resetCounts()

			replacement := strings.Repeat("replaced", 2)
			terminal := &scriptTerminal{interactive: true, lines: []string{testCase.answer}}
			if testCase.replace {
				terminal.secrets = []string{replacement, replacement}
			}
			code, stdout, stderr := e.run(t, t.Context(), terminal)
			if code != 0 {
				t.Fatalf("重复运行失败: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if !slices.Equal(terminal.linePrompts, []string{promptReplace}) {
				t.Errorf("ReadLine 提示 = %q; want 只询问是否替换", terminal.linePrompts)
			}
			if got, err := os.ReadFile(e.configFile); err != nil || !bytes.Equal(got, configBefore) {
				t.Error("配置文件内容被改变")
			}
			if info, err := os.Stat(e.configFile); err != nil || !info.ModTime().Equal(old) {
				t.Error("配置文件修改时间被改变")
			}
			if e.keychain.adds != 0 {
				t.Errorf("Add 调用 %d 次; want 0", e.keychain.adds)
			}
			wantItems := before
			if testCase.replace {
				wantItems[keychain.AuthCodeAccount(id)] = replacement
				if e.keychain.sets != 1 || !slices.Equal(terminal.secretPrompts, []string{promptCode, promptConfirm}) {
					t.Errorf("替换时 Set = %d、ReadSecret 提示 = %q; want 1 次与两次提示", e.keychain.sets, terminal.secretPrompts)
				}
				requireContains(t, stdout, "授权码：已替换\n")
			} else {
				if e.keychain.sets != 0 || len(terminal.secretPrompts) != 0 {
					t.Errorf("未替换时 Set = %d、ReadSecret 提示 = %q; want 0", e.keychain.sets, terminal.secretPrompts)
				}
				requireContains(t, stdout, "授权码：保持不变\n")
			}
			if !maps.Equal(e.keychain.items, wantItems) {
				t.Error("Keychain 条目与预期不符")
			}
			requireContains(t, stdout, "配置文件：已存在（TURNCOURIER_CONFIG 指定的文件）\n", "令牌签名密钥：已存在\n", "正文加密密钥：已存在\n")
			const warning = "输入不是 y 或 N，授权码未替换；如果刚才粘贴的是授权码，请清屏，并考虑在 QQ 邮箱中停用它。"
			if strings.Contains(stdout+stderr, warning) != testCase.warned {
				t.Errorf("提示出现与否不符: stdout=%q stderr=%q", stdout, stderr)
			}
			// 提示本身含「 y 」，先去掉提示再检查回答是否被回显。
			if testCase.warned && strings.Contains(strings.ReplaceAll(stdout+stderr, warning, ""), testCase.answer) {
				t.Error("输出回显了回答")
			}
			e.requireNoLeak(t, stdout, stderr)
		})
	}
}

// errInvalidSecretRead 模拟 keychain.Security.Get 读到格式不符的条目内容时返回的错误。
var errInvalidSecretRead = fmt.Errorf("keychain get read an %w", keychain.ErrInvalidSecret)

// failGet 返回只对 account 以 suffix 结尾的条目让 Get 返回 err 的函数。
func failGet(suffix string, err error) func(string) error {
	return func(account string) error {
		if strings.HasSuffix(account, suffix) {
			return err
		}
		return nil
	}
}

// TestInitMalformedAuthCode 验证 Keychain 中授权码条目格式不符（Get 返回 ErrInvalidSecret）时按已存在处理：询问是否替换，
// 回答空行时不覆盖，回答 y 时才重新录入。
func TestInitMalformedAuthCode(t *testing.T) {
	e := newInitEnv(t)
	e.mustFirstRun(t)
	e.keychain.resetCounts()
	e.keychain.getErr = failGet(":qq-auth-code", errInvalidSecretRead)
	terminal := &scriptTerminal{interactive: true, lines: []string{""}}
	code, stdout, stderr := e.run(t, t.Context(), terminal)
	if code != 0 || !slices.Equal(terminal.linePrompts, []string{promptReplace}) {
		t.Fatalf("init 失败: code=%d prompts=%q stderr=%q", code, terminal.linePrompts, stderr)
	}
	requireContains(t, stdout, "授权码：保持不变\n")
	if e.keychain.sets != 0 {
		t.Errorf("Set 调用 %d 次; want 0", e.keychain.sets)
	}
}

// TestInitKeychainReadErrors 验证读取 Keychain 失败时的处理：授权码读取失败时退出；已登记的密钥条目格式不符时按不符退出，
// 读取失败时报告原因；未登记的同名条目格式不符时提示手工删除，读取失败时报告原因；已有条目都不被改动，出错的密钥没有经 Add 写入。
func TestInitKeychainReadErrors(t *testing.T) {
	cases := []struct {
		name       string
		registered bool
		suffix     string
		err        error
		want       string
	}{
		{"授权码读取失败", false, ":qq-auth-code", keychain.ErrInteractionNotAllowed, "无法读取 Keychain 中的授权码：" + keychain.ErrInteractionNotAllowed.Error() + "\n"},
		{"已登记的密钥格式不符", true, ":token-key-1", errInvalidSecretRead, errKeyMismatch.Error() + "\n"},
		{"已登记的密钥读取失败", true, ":payload-key-1", keychain.ErrInteractionNotAllowed, "无法读取 Keychain 中的密钥：" + keychain.ErrInteractionNotAllowed.Error() + "\n"},
		{"未登记的同名条目格式不符", false, ":token-key-1", errInvalidSecretRead, errKeyMalformed.Error() + "\n"},
		{"未登记的密钥读取失败", false, ":payload-key-1", keychain.ErrInteractionNotAllowed, "无法在 Keychain 中读取或创建密钥：" + keychain.ErrInteractionNotAllowed.Error() + "\n"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			e := newInitEnv(t)
			terminal := firstRunTerminal(canary())
			if testCase.registered {
				e.mustFirstRun(t)
				terminal = &scriptTerminal{interactive: true, lines: []string{""}}
			}
			before := maps.Clone(e.keychain.items)
			e.keychain.resetCounts()
			e.keychain.getErr = failGet(testCase.suffix, testCase.err)
			code, stdout, stderr := e.run(t, t.Context(), terminal)
			if code != 1 || stderr != testCase.want {
				t.Errorf("code=%d stderr=%q; want 1 与 %q", code, stderr, testCase.want)
			}
			if e.keychain.adds != 0 && testCase.suffix != ":payload-key-1" {
				t.Errorf("Add 调用 %d 次; want 0", e.keychain.adds)
			}
			for account, value := range before {
				if e.keychain.items[account] != value {
					t.Errorf("已有条目 %s 被改动", account)
				}
			}
			e.requireNoLeak(t, stdout, stderr)
		})
	}
}

// TestInitKeyGenerationFailure 验证随机源读取失败时不写入密钥条目、不登记元数据，并报告无法生成密钥。
func TestInitKeyGenerationFailure(t *testing.T) {
	e := newInitEnv(t)
	id := e.seedWithoutTokenMetadata(t, keyText(1))
	delete(e.keychain.items, keychain.TokenKeyAccount(id, 1))
	deps := e.deps(t, &scriptTerminal{interactive: true, lines: []string{""}})
	deps.Random = iotest.ErrReader(errors.New("random unavailable"))
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), []string{"init"}, &stdout, &stderr, Deps{Init: deps}); code != 1 || !strings.Contains(stderr.String(), "无法生成密钥") {
		t.Errorf("code=%d stderr=%q", code, stderr.String())
	}
	if e.keychain.adds != 0 {
		t.Errorf("Add 调用 %d 次; want 0", e.keychain.adds)
	}
	if _, err := e.openStore(t).ActiveKeyID(t.Context(), sqlite.KeyPurposeToken); !errors.Is(err, sqlite.ErrNotFound) {
		t.Errorf("ActiveKeyID(token) = %v; want ErrNotFound", err)
	}
}

// TestInitConfigWriteFailures 验证写配置阶段的失败：读入授权码期间其他进程创建了配置文件时不覆盖它并提示重新运行；
// 新建的配置文件核对失败（通知事件环境变量无效）时报告原因；两种情况下 Keychain 都没有写入。
func TestInitConfigWriteFailures(t *testing.T) {
	t.Run("其他进程刚创建", func(t *testing.T) {
		e := newInitEnv(t)
		other := []byte("# written by another init\n")
		terminal := firstRunTerminal(canary())
		terminal.onSecret = func() error {
			if _, err := os.Stat(e.configFile); err != nil {
				return config.CreateFile(e.configFile, other)
			}
			return nil
		}
		code, _, stderr := e.run(t, t.Context(), terminal)
		if code != 1 || stderr != "配置文件刚被其他进程创建；init 没有覆盖它，请重新运行 turncourier init。\n" {
			t.Errorf("code=%d stderr=%q", code, stderr)
		}
		if got, err := os.ReadFile(e.configFile); err != nil || !bytes.Equal(got, other) {
			t.Error("其他进程创建的配置文件被改动")
		}
		if e.keychain.sets+e.keychain.adds != 0 {
			t.Error("Keychain 不应被写入")
		}
	})
	t.Run("核对失败", func(t *testing.T) {
		e := newInitEnv(t)
		e.env["TURNCOURIER_NOTIFY_EVENTS"] = "bogus"
		code, _, stderr := e.run(t, t.Context(), firstRunTerminal(canary()))
		if code != 1 || !strings.HasPrefix(stderr, "新建的配置文件无法加载：") || !strings.Contains(stderr, "bogus") {
			t.Errorf("code=%d stderr=%q", code, stderr)
		}
		if e.keychain.sets+e.keychain.adds != 0 {
			t.Error("Keychain 不应被写入")
		}
	})
}

// TestInitAuthCodeValidation 验证两次授权码须相同且为 1–128 个不含空白的可打印 ASCII 字符：
// 不符时重问，第三次符合即成功；连续 3 次不符时退出码 1，Keychain 没有写入，配置文件不存在。
func TestInitAuthCodeValidation(t *testing.T) {
	code, other := canary(), strings.Repeat("other", 3)
	cases := []struct {
		name    string
		secrets []string
		wantOK  bool
		want    string
	}{
		{"两次不一致后第三次一致", []string{code, other, other, code, code, code}, true, "两次输入不一致"},
		{"含空白", []string{"a b", "a b", code, code}, true, "1–128 个不含空白的可打印 ASCII 字符"},
		{"超过 128 个字符", []string{strings.Repeat("a", 129), strings.Repeat("a", 129), strings.Repeat("a", 128), strings.Repeat("a", 128)}, true, "1–128"},
		{"空", []string{"", "", code, code}, true, "1–128"},
		{"非 ASCII", []string{strings.Repeat("é", 3), strings.Repeat("é", 3), code, code}, true, "1–128"},
		{"连续 3 次不一致", []string{code, other, other, code, code, other}, false, "连续 3 次"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			e := newInitEnv(t)
			terminal := &scriptTerminal{interactive: true, lines: []string{botAddress, meAddress, ""}, secrets: testCase.secrets}
			exit, stdout, stderr := e.run(t, t.Context(), terminal)
			requireContains(t, stderr, testCase.want)
			if !testCase.wantOK {
				if exit != 1 || e.keychain.sets+e.keychain.adds != 0 || len(e.keychain.items) != 0 {
					t.Errorf("code=%d Set=%d Add=%d; want 1 且 Keychain 没有写入", exit, e.keychain.sets, e.keychain.adds)
				}
				requireNotExist(t, e.configFile, "配置文件")
				e.requireNoLeak(t, stdout, stderr)
				return
			}
			if exit != 0 {
				t.Fatalf("init 失败: code=%d stderr=%q", exit, stderr)
			}
			last := testCase.secrets[len(testCase.secrets)-1]
			if got := e.keychain.items[keychain.AuthCodeAccount(e.instanceID(t))]; got != last {
				t.Errorf("保存的授权码不是最后一次录入的值")
			}
			for _, secret := range testCase.secrets {
				if strings.Contains(stdout+stderr, secret) && secret != "" {
					t.Errorf("输出回显了录入的内容")
				}
			}
		})
	}
}

// TestInitAddressValidation 验证地址逐项校验：带显示名的地址与含机器人地址的白名单被重问，大写输入被规范化；
// 各项的无效次数分别计算；同一项连续 3 次无效时退出码 1，配置文件不存在，Keychain 未被访问。
func TestInitAddressValidation(t *testing.T) {
	e := newInitEnv(t)
	terminal := &scriptTerminal{
		interactive: true,
		lines:       []string{"Bot@Example.Invalid", "Me <me@example.invalid>", meAddress, meAddress + ", " + botAddress, " other@example.invalid , ME@example.invalid "},
		secrets:     []string{canary(), canary()},
	}
	code, stdout, stderr := e.run(t, t.Context(), terminal)
	if code != 0 {
		t.Fatalf("init 失败: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	wantPrompts := []string{promptMailbox, promptReceiver, promptReceiver, promptSenders, promptSenders}
	if !slices.Equal(terminal.linePrompts, wantPrompts) {
		t.Errorf("ReadLine 提示 = %q; want %q", terminal.linePrompts, wantPrompts)
	}
	requireContains(t, stderr, "invalid email address", "must not contain the mailbox address")
	want, err := config.Render(config.Draft{MailboxAddress: botAddress, RecipientAddress: meAddress, AllowedSenders: []string{"other@example.invalid", meAddress}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(e.configFile); err != nil || !bytes.Equal(got, want) {
		t.Errorf("配置文件 = %q, %v; want %q", got, err, want)
	}

	// 每一项各有 3 次机会，无效次数不跨项累计：机器人地址与接收地址各无效 2 次后输入有效值，仍然成功。
	e = newInitEnv(t)
	terminal = &scriptTerminal{interactive: true, lines: []string{"a", "b", botAddress, "c", "d", meAddress, ""}, secrets: []string{canary(), canary()}}
	if code, stdout, stderr := e.run(t, t.Context(), terminal); code != 0 {
		t.Fatalf("各项无效 2 次后 init 失败: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	wantPrompts = []string{promptMailbox, promptMailbox, promptMailbox, promptReceiver, promptReceiver, promptReceiver, promptSenders}
	if !slices.Equal(terminal.linePrompts, wantPrompts) {
		t.Errorf("ReadLine 提示 = %q; want %q", terminal.linePrompts, wantPrompts)
	}

	for _, lines := range [][]string{
		{"a", "b", "c"},
		{botAddress, "x", "y", "z"},
		{botAddress, meAddress, botAddress, meAddress + ",", meAddress + "," + meAddress},
	} {
		e := newInitEnv(t)
		code, stdout, stderr := e.run(t, t.Context(), &scriptTerminal{interactive: true, lines: lines})
		if code != 1 || !strings.Contains(stderr, "连续 3 次") {
			t.Errorf("%q: code=%d stderr=%q; want 1 与连续 3 次的提示", lines, code, stderr)
		}
		requireNotExist(t, e.configFile, "配置文件")
		requireNotExist(t, e.dataDir, "数据目录")
		if e.keychain.calls() != 0 {
			t.Errorf("%q: Keychain 被调用 %d 次; want 0", lines, e.keychain.calls())
		}
		e.requireNoLeak(t, stdout, stderr)
	}
}

// TestInitEnvironment 验证非交互终端时不读取输入、不创建任何目录、不调用 Keychain 的任何方法；
// Keychain() 返回 ErrUnsupported 时不读取输入；Keychain 其他错误与路径解析错误同样以退出码 1 结束。
func TestInitEnvironment(t *testing.T) {
	t.Run("非交互", func(t *testing.T) {
		e := newInitEnv(t)
		terminal := firstRunTerminal(canary())
		terminal.interactive = false
		code, stdout, stderr := e.run(t, t.Context(), terminal)
		if code != 1 || stderr != "init 需要在交互式终端中运行；请直接在终端执行 turncourier init。\n" || stdout != "" {
			t.Errorf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		requireNotExist(t, filepath.Dir(e.configFile), "配置目录")
		requireNotExist(t, e.dataDir, "数据目录")
		if e.keychain.calls() != 0 || len(terminal.linePrompts)+len(terminal.secretPrompts) != 0 {
			t.Errorf("Keychain 调用 %d 次、读入 %d 次; want 0", e.keychain.calls(), len(terminal.linePrompts)+len(terminal.secretPrompts))
		}
	})
	t.Run("不支持的平台", func(t *testing.T) {
		e := newInitEnv(t)
		e.keychainErr = keychain.ErrUnsupported
		terminal := firstRunTerminal(canary())
		code, stdout, stderr := e.run(t, t.Context(), terminal)
		if code != 1 || stderr != "init 只支持 macOS：授权码与密钥只能保存在 macOS Keychain。\n" || stdout != "" {
			t.Errorf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if terminal.interactiveHit != 0 || len(terminal.linePrompts)+len(terminal.secretPrompts) != 0 {
			t.Error("不支持的平台上不应查询终端或读取输入")
		}
		requireNotExist(t, filepath.Dir(e.configFile), "配置目录")
		requireNotExist(t, e.dataDir, "数据目录")
	})
	t.Run("Keychain 其他错误", func(t *testing.T) {
		e := newInitEnv(t)
		e.keychainErr = errors.New("KEYCHAIN-CANARY")
		code, _, stderr := e.run(t, t.Context(), firstRunTerminal(canary()))
		if code != 1 || !strings.Contains(stderr, "KEYCHAIN-CANARY") {
			t.Errorf("code=%d stderr=%q", code, stderr)
		}
	})
	t.Run("相对路径", func(t *testing.T) {
		e := newInitEnv(t)
		e.env["TURNCOURIER_CONFIG"] = "relative/turncourier.toml"
		terminal := firstRunTerminal(canary())
		code, _, stderr := e.run(t, t.Context(), terminal)
		if code != 1 || !strings.Contains(stderr, "TURNCOURIER_CONFIG must be an absolute path") || len(terminal.linePrompts) != 0 {
			t.Errorf("code=%d stderr=%q prompts=%q", code, stderr, terminal.linePrompts)
		}
	})
}

// TestInitExistingData 验证已有配置含未知键时不修改它、输出含键名不含路径；配置文件或它的某一级上级目录是悬空的符号链接时
// 不询问即退出，不创建链接目标，配置目录链接到已有目录时照常创建；数据目录权限为 0755 时退出且 Keychain 未写入。
func TestInitExistingData(t *testing.T) {
	t.Run("配置含未知键", func(t *testing.T) {
		e := newInitEnv(t)
		content, err := config.Render(config.Draft{MailboxAddress: botAddress, RecipientAddress: meAddress, AllowedSenders: []string{meAddress}})
		if err != nil {
			t.Fatal(err)
		}
		content = append(content, "\n[extra]\nbogus_key = 1\n"...)
		if err := config.CreateFile(e.configFile, content); err != nil {
			t.Fatal(err)
		}
		old := time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
		if err := os.Chtimes(e.configFile, old, old); err != nil {
			t.Fatal(err)
		}
		terminal := firstRunTerminal(canary())
		code, stdout, stderr := e.run(t, t.Context(), terminal)
		if code != 1 || !strings.HasPrefix(stderr, "现有配置文件无效，init 不会修改它：") || !strings.Contains(stderr, "extra.bogus_key") {
			t.Errorf("code=%d stderr=%q", code, stderr)
		}
		if got, err := os.ReadFile(e.configFile); err != nil || !bytes.Equal(got, content) {
			t.Error("配置文件被改变")
		}
		if info, err := os.Stat(e.configFile); err != nil || !info.ModTime().Equal(old) {
			t.Error("配置文件修改时间被改变")
		}
		if e.keychain.sets+e.keychain.adds != 0 || len(terminal.linePrompts) != 0 {
			t.Error("配置无效时不应询问或写入 Keychain")
		}
		requireNotExist(t, e.dataDir, "数据目录")
		e.requireNoLeak(t, stdout, stderr)
	})
	// 悬空的链接可以是配置文件本身，也可以是它的某一级上级目录（例如 dotfiles 工具把配置目录链接到尚不存在的位置）；
	// link 与 file 是相对临时目录的链接位置与配置文件路径。
	danglingCases := []struct{ name, link, file string }{
		{"配置路径是悬空的符号链接", "config/turncourier.toml", "config/turncourier.toml"},
		{"配置目录是悬空的符号链接", "config", "config/turncourier.toml"},
		{"更上级的目录是悬空的符号链接", "config", "config/sub/turncourier.toml"},
	}
	for _, tc := range danglingCases {
		t.Run(tc.name, func(t *testing.T) {
			e := newInitEnv(t)
			e.configFile = filepath.Join(e.root, tc.file)
			e.env["TURNCOURIER_CONFIG"] = e.configFile
			link := filepath.Join(e.root, tc.link)
			target := filepath.Join(e.root, "dotfiles", "turncourier")
			if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			terminal := firstRunTerminal(canary())
			code, stdout, stderr := e.run(t, t.Context(), terminal)
			if code != 1 || stderr != "配置文件或它的上级目录是悬空的符号链接；init 不会修改它，请检查后重新运行 turncourier init。\n" {
				t.Errorf("code=%d stderr=%q", code, stderr)
			}
			if len(terminal.linePrompts)+len(terminal.secretPrompts) != 0 || e.keychain.sets+e.keychain.adds != 0 {
				t.Error("不应询问任何输入或写入 Keychain")
			}
			if got, err := os.Readlink(link); err != nil || got != target {
				t.Errorf("符号链接被改动: %q, %v", got, err)
			}
			requireNotExist(t, target, "链接目标")
			requireNotExist(t, e.dataDir, "数据目录")
			e.requireNoLeak(t, stdout, stderr)
		})
	}
	t.Run("配置目录是指向已有目录的符号链接", func(t *testing.T) {
		e := newInitEnv(t)
		target := filepath.Join(e.root, "dotfiles")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Dir(e.configFile)); err != nil {
			t.Fatal(err)
		}
		if code, stdout, stderr := e.run(t, t.Context(), firstRunTerminal(canary())); code != 0 {
			t.Fatalf("init 失败: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if _, err := os.Stat(filepath.Join(target, "turncourier.toml")); err != nil {
			t.Errorf("链接目标中没有配置文件: %v", err)
		}
	})
	t.Run("数据目录权限 0755", func(t *testing.T) {
		e := newInitEnv(t)
		if err := os.Mkdir(e.dataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(e.dataDir, 0o755); err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr := e.run(t, t.Context(), firstRunTerminal(canary()))
		if code != 1 || !strings.Contains(stderr, "data directory must not be accessible by group or others") {
			t.Errorf("code=%d stderr=%q", code, stderr)
		}
		if e.keychain.calls() != 0 || len(e.keychain.items) != 0 {
			t.Error("Keychain 不应被访问")
		}
		requireNotExist(t, e.configFile, "配置文件")
		e.requireNoLeak(t, stdout, stderr)
	})
}

// seedWithoutTokenMetadata 模拟上次在登记令牌密钥之前中断：配置、实例、授权码、正文密钥（含元数据）与令牌密钥条目都已存在，
// 只缺令牌密钥的元数据；返回实例 ID。
func (e *initEnv) seedWithoutTokenMetadata(t *testing.T, tokenText string) string {
	t.Helper()
	content, err := config.Render(config.Draft{MailboxAddress: botAddress, RecipientAddress: meAddress, AllowedSenders: []string{meAddress}})
	if err != nil {
		t.Fatal(err)
	}
	if err := config.CreateFile(e.configFile, content); err != nil {
		t.Fatal(err)
	}
	store := e.openStore(t)
	id, _, err := store.EnsureInstance(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	payloadText := keyText(2)
	payloadKey, err := keychain.DecodeKeyText(payloadText)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterKey(t.Context(), sqlite.KeyPurposePayload, 1, keychain.KeyCheck("payload", 1, payloadKey)); err != nil {
		t.Fatal(err)
	}
	e.keychain.items[keychain.AuthCodeAccount(id)] = canary()
	e.keychain.items[keychain.TokenKeyAccount(id, 1)] = tokenText
	e.keychain.items[keychain.PayloadKeyAccount(id, 1)] = payloadText
	return id
}

// TestInitInterruptedBeforeRegistration 验证 Keychain 中已有令牌密钥条目但没有元数据时沿用该条目并以它的校验值登记，
// 条目不变，没有调用 Add 或 Set。
func TestInitInterruptedBeforeRegistration(t *testing.T) {
	e := newInitEnv(t)
	id := e.seedWithoutTokenMetadata(t, keyText(1))
	before := maps.Clone(e.keychain.items)
	code, stdout, stderr := e.run(t, t.Context(), &scriptTerminal{interactive: true, lines: []string{""}})
	if code != 0 {
		t.Fatalf("init 失败: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if e.keychain.adds+e.keychain.sets != 0 || !maps.Equal(e.keychain.items, before) {
		t.Errorf("Add = %d, Set = %d; want 0 且条目不变", e.keychain.adds, e.keychain.sets)
	}
	store := e.openStore(t)
	e.requireKeyRegistered(t, store, id, sqlite.KeyPurposeToken)
	e.requireKeyRegistered(t, store, id, sqlite.KeyPurposePayload)
	requireContains(t, stdout, "令牌签名密钥：已存在\n", "正文加密密钥：已存在\n")
	e.requireNoLeak(t, stdout, stderr)
}

// TestInitMalformedUnregisteredEntry 验证没有元数据、Keychain 中的同名条目格式不符时退出，不覆盖条目、不登记。
func TestInitMalformedUnregisteredEntry(t *testing.T) {
	e := newInitEnv(t)
	e.seedWithoutTokenMetadata(t, strings.Repeat("A", 42))
	before := maps.Clone(e.keychain.items)
	code, _, stderr := e.run(t, t.Context(), &scriptTerminal{interactive: true, lines: []string{""}})
	if code != 1 || stderr != "Keychain 中已有格式不符的同名条目；请按 development.md 手工删除后重新运行。\n" {
		t.Errorf("code=%d stderr=%q", code, stderr)
	}
	if e.keychain.adds+e.keychain.sets != 0 || !maps.Equal(e.keychain.items, before) {
		t.Error("格式不符的条目不应被覆盖")
	}
	if _, err := e.openStore(t).ActiveKeyID(t.Context(), sqlite.KeyPurposeToken); !errors.Is(err, sqlite.ErrNotFound) {
		t.Errorf("ActiveKeyID(token) = %v; want ErrNotFound", err)
	}
}

// TestInitMissingRegisteredKey 验证有元数据但 Keychain 缺少对应条目时退出，不生成新密钥、不改动其他数据。
func TestInitMissingRegisteredKey(t *testing.T) {
	e := newInitEnv(t)
	id := e.mustFirstRun(t)
	delete(e.keychain.items, keychain.TokenKeyAccount(id, 1))
	before := maps.Clone(e.keychain.items)
	payloadCheck, err := e.openStore(t).KeyCheckOf(t.Context(), sqlite.KeyPurposePayload, 1)
	if err != nil {
		t.Fatal(err)
	}
	e.keychain.resetCounts()
	code, _, stderr := e.run(t, t.Context(), &scriptTerminal{interactive: true, lines: []string{""}})
	if code != 1 || stderr != "密钥元数据存在，但 Keychain 中缺少对应条目；init 不会重新生成，否则待处理数据将无法解密。\n" {
		t.Errorf("code=%d stderr=%q", code, stderr)
	}
	if e.keychain.adds+e.keychain.sets != 0 || !maps.Equal(e.keychain.items, before) {
		t.Error("不应生成新密钥或改动 Keychain")
	}
	if check, err := e.openStore(t).KeyCheckOf(t.Context(), sqlite.KeyPurposePayload, 1); err != nil || check != payloadCheck {
		t.Error("正文密钥元数据被改动")
	}
}

// TestInitReplacedKey 验证 Keychain 中登记过的密钥被换成另一段格式合法的文本，或被换成格式不符的文本时，
// init 退出并提示与数据库登记的不符，条目与元数据都不变。
func TestInitReplacedKey(t *testing.T) {
	for _, replacement := range []string{keyText(9), "short"} {
		e := newInitEnv(t)
		id := e.mustFirstRun(t)
		account := keychain.PayloadKeyAccount(id, 1)
		checkBefore, err := e.openStore(t).KeyCheckOf(t.Context(), sqlite.KeyPurposePayload, 1)
		if err != nil {
			t.Fatal(err)
		}
		e.keychain.items[account] = replacement
		before := maps.Clone(e.keychain.items)
		e.keychain.resetCounts()
		code, _, stderr := e.run(t, t.Context(), &scriptTerminal{interactive: true, lines: []string{""}})
		if code != 1 || stderr != "Keychain 中的密钥与数据库登记的不符；init 不会覆盖它。\n" {
			t.Errorf("code=%d stderr=%q", code, stderr)
		}
		if e.keychain.adds+e.keychain.sets != 0 || !maps.Equal(e.keychain.items, before) {
			t.Error("被替换的条目不应被改动")
		}
		if check, err := e.openStore(t).KeyCheckOf(t.Context(), sqlite.KeyPurposePayload, 1); err != nil || check != checkBefore {
			t.Error("密钥元数据被改动")
		}
	}
}

// TestInitConcurrentAdd 验证 Add 时另一个 init 已写入同名条目（替身先放入另一段合法密钥再返回 ErrExists）：
// init 沿用该密钥并以它的校验值登记；另一个 init 已登记了同一校验值时同样成功；已登记了不同的校验值时退出码 1。
func TestInitConcurrentAdd(t *testing.T) {
	cases := []struct {
		name     string
		register string // 并发的 init 登记的密钥文本；空表示不登记
		wantCode int
	}{
		{"只写入条目", "", 0},
		{"登记了同一把密钥", keyText(7), 0},
		{"登记了另一把密钥", keyText(8), 1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			e := newInitEnv(t)
			concurrent := keyText(7)
			e.keychain.beforeAdd = func(account string) {
				if !strings.HasSuffix(account, ":token-key-1") {
					return
				}
				e.keychain.items[account] = concurrent
				if testCase.register == "" {
					return
				}
				key, err := keychain.DecodeKeyText(testCase.register)
				if err != nil {
					t.Fatal(err)
				}
				store, err := sqlite.Open(t.Context(), e.dataDir, sqlite.Options{})
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				if err := store.RegisterKey(t.Context(), sqlite.KeyPurposeToken, 1, keychain.KeyCheck("token", 1, key)); err != nil {
					t.Fatal(err)
				}
			}
			code, stdout, stderr := e.run(t, t.Context(), firstRunTerminal(canary()))
			if code != testCase.wantCode {
				t.Fatalf("code=%d; want %d; stdout=%q stderr=%q", code, testCase.wantCode, stdout, stderr)
			}
			id := e.instanceID(t)
			if e.keychain.items[keychain.TokenKeyAccount(id, 1)] != concurrent {
				t.Error("并发写入的条目被覆盖")
			}
			if code == 1 {
				requireContains(t, stderr, "Keychain 中的密钥与数据库登记的不符；init 不会覆盖它。")
				return
			}
			e.requireKeyRegistered(t, e.openStore(t), id, sqlite.KeyPurposeToken)
			requireContains(t, stdout, "令牌签名密钥：已存在\n", "正文加密密钥：已生成\n")
		})
	}
}

// TestInitKeychainErrors 验证 Add 或 Set 返回错误时退出码 1、对应密钥没有登记元数据（重新运行会重新生成），
// 错误原因会显示，输出不含机密。
func TestInitKeychainErrors(t *testing.T) {
	t.Run("Add", func(t *testing.T) {
		e := newInitEnv(t)
		e.keychain.addErr = errors.New("KEYCHAIN-CANARY add failed")
		code, stdout, stderr := e.run(t, t.Context(), firstRunTerminal(canary()))
		if code != 1 || !strings.Contains(stderr, "KEYCHAIN-CANARY") {
			t.Errorf("code=%d stderr=%q", code, stderr)
		}
		e.requireNoLeak(t, stdout, stderr)
		store := e.openStore(t)
		for _, purpose := range []sqlite.KeyPurpose{sqlite.KeyPurposeToken, sqlite.KeyPurposePayload} {
			if _, err := store.ActiveKeyID(t.Context(), purpose); !errors.Is(err, sqlite.ErrNotFound) {
				t.Errorf("ActiveKeyID(%s) = %v; want ErrNotFound", purpose, err)
			}
		}

		e.keychain.addErr = nil
		code, stdout, stderr = e.run(t, t.Context(), &scriptTerminal{interactive: true, lines: []string{""}})
		if code != 0 {
			t.Fatalf("重新运行失败: code=%d stderr=%q", code, stderr)
		}
		requireContains(t, stdout, "令牌签名密钥：已生成\n", "正文加密密钥：已生成\n")
	})
	t.Run("Set", func(t *testing.T) {
		e := newInitEnv(t)
		e.keychain.setErr = errors.New("KEYCHAIN-CANARY set failed")
		code, stdout, stderr := e.run(t, t.Context(), firstRunTerminal(canary()))
		if code != 1 || !strings.Contains(stderr, "KEYCHAIN-CANARY") {
			t.Errorf("code=%d stderr=%q", code, stderr)
		}
		e.requireNoLeak(t, stdout, stderr)
		if len(e.keychain.items) != 0 || e.keychain.adds != 0 {
			t.Error("授权码写入失败后不应生成密钥")
		}
	})
}

// TestInitCancel 验证读入授权码时取消 ctx：输出「已取消。」、退出码 1，Keychain 没有写入，配置文件不存在。
func TestInitCancel(t *testing.T) {
	e := newInitEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	terminal := firstRunTerminal(canary())
	terminal.onSecret = func() error {
		cancel()
		return ctx.Err()
	}
	code, stdout, stderr := e.run(t, ctx, terminal)
	if code != 1 || stderr != "已取消。\n" || stdout != "" {
		t.Errorf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if e.keychain.sets+e.keychain.adds != 0 || len(e.keychain.items) != 0 {
		t.Error("取消后 Keychain 不应被写入")
	}
	requireNotExist(t, e.configFile, "配置文件")
}

// TestInitReadError 验证读入失败（脚本用尽）时以退出码 1 结束并报告无法读取输入。
func TestInitReadError(t *testing.T) {
	e := newInitEnv(t)
	code, _, stderr := e.run(t, t.Context(), &scriptTerminal{interactive: true})
	if code != 1 || !strings.Contains(stderr, "无法读取输入") {
		t.Errorf("code=%d stderr=%q", code, stderr)
	}
}

// TestInitOutputFailure 验证摘要写入失败时返回 1 并提示无法写入输出。
func TestInitOutputFailure(t *testing.T) {
	e := newInitEnv(t)
	var stderr bytes.Buffer
	code := Run(t.Context(), []string{"init"}, &failingWriter{}, &stderr, Deps{Init: e.deps(t, firstRunTerminal(canary()))})
	if code != 1 || !strings.Contains(stderr.String(), "无法写入") {
		t.Errorf("code=%d stderr=%q", code, stderr.String())
	}
}
