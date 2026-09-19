// Package keychain 让测试二进制扮演假的 security 程序，验证机密只经标准输入写入、经标准输出读出，
// 以及退出码映射、读回核对、只创建语义、输入校验与期限；测试不接触真实钥匙串。
package keychain

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 假 security 的控制变量：状态文件路径非空时，测试二进制在 TestMain 中扮演 security 而不运行测试。
const (
	envState = "TURNCOURIER_TEST_SECURITY_STATE"
	envLog   = "TURNCOURIER_TEST_SECURITY_LOG"
	envMode  = "TURNCOURIER_TEST_SECURITY_MODE"
)

// testInstance 是测试用的实例 ID：16 位小写 Crockford base32。
const testInstance = "0123456789abcdef"

// rivalValue 是 add-exists 模式下「另一写入者」抢先写入的值；在运行时构造，源码中不出现形似机密的字面量。
var rivalValue = strings.Repeat("r", 12)

// 编译期确认 Security 实现 Store。
var _ Store = (*Security)(nil)

// call 是假 security 记录的一次调用：参数、完整标准输入、环境变量与进程号。
type call struct {
	Args  []string
	Stdin string
	Env   []string
	PID   int
}

// fake 是一个测试使用的假钥匙串：指向测试二进制的 Security、状态文件与调用日志。
type fake struct {
	security *Security
	state    string
	log      string
}

// TestMain 在设置了假钥匙串状态文件时扮演 security，否则正常运行测试。
func TestMain(m *testing.M) {
	if os.Getenv(envState) != "" {
		os.Exit(fakeSecurity())
	}
	os.Exit(m.Run())
}

// fakeSecurity 扮演 security：先把本次调用追加写入日志，再按模式动作，返回进程退出码。
// 状态文件保存 account 到机密的 JSON 映射；output 模式下它的内容原样写到标准输出。
// hang 模式下读取与删除睡眠 30 秒，security -i 的写入照常完成，以便让期限落在写入后的读回核对中。
func fakeSecurity() int {
	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		return 3
	}
	record, err := json.Marshal(call{Args: os.Args[1:], Stdin: string(stdin), Env: os.Environ(), PID: os.Getpid()})
	if err != nil {
		return 3
	}
	logFile, err := os.OpenFile(os.Getenv(envLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 3
	}
	_, err = logFile.Write(append(record, '\n'))
	if closeErr := logFile.Close(); err != nil || closeErr != nil {
		return 3
	}
	mode, statePath := os.Getenv(envMode), os.Getenv(envState)
	switch {
	case mode == "exit36":
		return 36
	case mode == "exit1":
		fmt.Fprint(os.Stderr, "STDERR-CANARY")
		return 1
	case mode == "hang" && !slices.Equal(os.Args[1:], []string{"-i"}):
		time.Sleep(30 * time.Second)
		return 0
	case mode == "output":
		data, err := os.ReadFile(statePath)
		if err != nil {
			return 3
		}
		if _, err := os.Stdout.Write(data); err != nil {
			return 3
		}
		return 0
	}
	items := map[string]string{}
	data, err := os.ReadFile(statePath)
	if err == nil {
		err = json.Unmarshal(data, &items)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 3
	}
	code := fakeCommand(items, mode, os.Args[1:], string(stdin))
	if data, err = json.Marshal(items); err != nil || os.WriteFile(statePath, data, 0o600) != nil {
		return 3
	}
	return code
}

// fakeCommand 按 security 的语法执行一条命令并修改 items；语法不符时以 2 退出，使参数错误在测试中显形。
// 条目不存在时以 44（errSecItemNotFound 的低 8 位）退出。
func fakeCommand(items map[string]string, mode string, args []string, stdin string) int {
	switch {
	case slices.Equal(args, []string{"-i"}):
		return fakeAdd(items, mode, stdin)
	case len(args) == 6 && args[0] == "find-generic-password" && args[1] == "-s" && args[2] == Service &&
		args[3] == "-a" && args[5] == "-w":
		value, ok := items[args[4]]
		if !ok {
			return 44
		}
		fmt.Println(value)
		return 0
	case len(args) == 5 && args[0] == "delete-generic-password" && args[1] == "-s" && args[2] == Service &&
		args[3] == "-a":
		if _, ok := items[args[4]]; !ok {
			return 44
		}
		delete(items, args[4])
		fmt.Println(`class: "genp"`)
		return 0
	}
	return 2
}

// fakeAdd 解析 security -i 从标准输入读到的唯一一行 add-generic-password 命令。
// 不带 -U 且条目已存在时以 45（errSecDuplicateItem 的低 8 位）退出且不改动；
// drop 不保存、add-fails 以 1 退出、corrupt 存成另一个值、add-exists 模拟另一写入者抢先创建同名条目。
func fakeAdd(items map[string]string, mode, stdin string) int {
	line, found := strings.CutSuffix(stdin, "\n")
	fields := strings.Split(line, " ")
	update := len(fields) == 8 && fields[1] == "-U"
	if update {
		fields = slices.Delete(fields, 1, 2)
	}
	if !found || strings.Contains(line, "\n") || len(fields) != 7 || fields[0] != "add-generic-password" ||
		fields[1] != "-s" || fields[2] != Service || fields[3] != "-a" || fields[5] != "-X" {
		return 2
	}
	value, err := hex.DecodeString(fields[6])
	if err != nil {
		return 2
	}
	account := fields[4]
	switch mode {
	case "drop":
		return 0
	case "add-fails":
		return 1
	case "corrupt":
		value = append([]byte("x"), value...)
	case "add-exists":
		items[account] = rivalValue
	}
	if _, exists := items[account]; exists && !update {
		return 45
	}
	items[account] = string(value)
	return 0
}

// newFake 在临时目录中准备空的假钥匙串，以测试二进制作为 security 路径，并设定假程序的模式。
func newFake(t *testing.T, mode string) *fake {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	f := &fake{
		security: &Security{path: executable, timeout: DefaultTimeout},
		state:    filepath.Join(dir, "state.json"),
		log:      filepath.Join(dir, "calls.log"),
	}
	t.Setenv(envState, f.state)
	t.Setenv(envLog, f.log)
	t.Setenv(envMode, mode)
	// 竞态检测下以 0 退出的进程默认先睡 1 秒再退出，假程序每次成功调用都会因此变慢；保留调用方已有的其他 GORACE 选项。
	t.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	return f
}

// calls 读出假程序记录的全部调用；日志不存在表示假程序从未运行。
func (f *fake) calls(t *testing.T) []call {
	t.Helper()
	data, err := os.ReadFile(f.log)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var result []call
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var record call
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("malformed call log line: %v", err)
		}
		result = append(result, record)
	}
	return result
}

// items 读出假钥匙串的当前内容。
func (f *fake) items(t *testing.T) map[string]string {
	t.Helper()
	items := map[string]string{}
	data, err := os.ReadFile(f.state)
	if errors.Is(err, fs.ErrNotExist) {
		return items
	}
	if err == nil {
		err = json.Unmarshal(data, &items)
	}
	if err != nil {
		t.Fatal(err)
	}
	return items
}

// interactiveCalls 统计以 security -i 写入的次数。
func interactiveCalls(calls []call) int {
	count := 0
	for _, c := range calls {
		if slices.Equal(c.Args, []string{"-i"}) {
			count++
		}
	}
	return count
}

// assertNotExposed 确认给定文本不出现在任何一次调用的参数与环境变量中；失败信息不打印这些文本。
func assertNotExposed(t *testing.T, calls []call, values ...string) {
	t.Helper()
	for index, c := range calls {
		exposed := strings.Join(c.Args, "\x00") + "\x00" + strings.Join(c.Env, "\x00")
		for _, value := range values {
			if strings.Contains(exposed, value) {
				t.Errorf("call %d exposes secret material in argv or environment", index)
			}
		}
	}
}

// sequence 返回按下标填充的字节 0、1、…、n-1，用于在运行时构造低熵的测试密钥。
func sequence(n int) []byte {
	result := make([]byte, n)
	for i := range result {
		result[i] = byte(i)
	}
	return result
}

// processExists 用 0 号信号探测进程是否仍存在；已被回收的进程返回 false。
func processExists(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	defer process.Release()
	return process.Signal(syscall.Signal(0)) == nil
}

// assertPanics 确认 fn 触发 panic。
func assertPanics(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s did not panic", name)
		}
	}()
	fn()
}

// TestSetGetRoundTrip 钉住写入与读取的参数与标准输入：机密只以十六进制出现在 security -i 的标准输入中，
// 机密原文与十六进制文本都不进入任何一次调用的参数与环境变量。
func TestSetGetRoundTrip(t *testing.T) {
	f := newFake(t, "normal")
	ctx := context.Background()
	secret := strings.Repeat("aB3", 4)
	account := "0123456789abcdef:qq-auth-code"
	if err := f.security.Set(ctx, account, secret); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, err := f.security.Get(ctx, account); err != nil || got != secret {
		t.Fatalf("Get returned a different value or error %v", err)
	}
	calls := f.calls(t)
	if len(calls) != 3 {
		t.Fatalf("got %d security calls, want write, read-back and read", len(calls))
	}
	secretHex := hex.EncodeToString([]byte(secret))
	wantStdin := "add-generic-password -U -s " + Service + " -a 0123456789abcdef:qq-auth-code -X " + secretHex + "\n"
	if !slices.Equal(calls[0].Args, []string{"-i"}) || calls[0].Stdin != wantStdin {
		t.Errorf("unexpected write call: argv %q", calls[0].Args)
	}
	wantFind := []string{"find-generic-password", "-s", Service, "-a", account, "-w"}
	for _, index := range []int{1, 2} {
		if !slices.Equal(calls[index].Args, wantFind) || calls[index].Stdin != "" {
			t.Errorf("unexpected read call %d: %q", index, calls[index].Args)
		}
	}
	assertNotExposed(t, calls, secret, secretHex)
}

// TestMaximumLengths 确认 128 字符的 account 与 1000 字符的机密可以写入，且整行不超过 security -i 的
// 4096 字节行缓冲；机密的十六进制含字母，同时钉住小写十六进制。
func TestMaximumLengths(t *testing.T) {
	f := newFake(t, "normal")
	ctx := context.Background()
	account := strings.Repeat("a", 128)
	secret := strings.Repeat("~", 1000)
	if err := f.security.Set(ctx, account, secret); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, err := f.security.Get(ctx, account); err != nil || got != secret {
		t.Fatalf("Get returned a different value or error %v", err)
	}
	stdin := f.calls(t)[0].Stdin
	if want := "add-generic-password -U -s " + Service + " -a " + account + " -X " + hex.EncodeToString([]byte(secret)) + "\n"; stdin != want {
		t.Error("write command is not the expected lowercase hex line")
	}
	if len(stdin) > 4095 {
		t.Errorf("command line is %d bytes, exceeds the security -i line buffer", len(stdin))
	}
}

// TestExitCodes 覆盖退出码映射：44 为条目不存在，36 为不允许交互，其他退出码只报告数字，
// 标准错误不进入错误文本；无法启动 security 时错误文本不含可执行文件路径。
func TestExitCodes(t *testing.T) {
	ctx := context.Background()
	account := AuthCodeAccount(testInstance)
	f := newFake(t, "normal")
	if _, err := f.security.Get(ctx, account); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing item: %v", err)
	}

	t.Setenv(envMode, "exit36")
	if _, err := f.security.Get(ctx, account); !errors.Is(err, ErrInteractionNotAllowed) {
		t.Errorf("exit 36: %v", err)
	}
	if err := f.security.Set(ctx, account, strings.Repeat("aB3", 4)); !errors.Is(err, ErrInteractionNotAllowed) {
		t.Errorf("Set with locked keychain: %v", err)
	}
	if err := f.security.Delete(ctx, account); !errors.Is(err, ErrInteractionNotAllowed) {
		t.Errorf("Delete with locked keychain: %v", err)
	}
	before := len(f.calls(t))
	if err := f.security.Add(ctx, account, strings.Repeat("aB3", 4)); !errors.Is(err, ErrInteractionNotAllowed) {
		t.Errorf("Add with locked keychain: %v", err)
	}
	if interactiveCalls(f.calls(t)[before:]) != 0 {
		t.Error("Add wrote although the existence check failed")
	}

	t.Setenv(envMode, "exit1")
	_, err := f.security.Get(ctx, account)
	if err == nil || err.Error() != "keychain get failed with exit code 1" || strings.Contains(err.Error(), "STDERR-CANARY") {
		t.Errorf("exit 1: %v", err)
	}

	dir := t.TempDir()
	missing := &Security{path: filepath.Join(dir, "missing-security"), timeout: time.Second}
	if _, err := missing.Get(ctx, account); err == nil || strings.Contains(err.Error(), dir) {
		t.Errorf("missing executable: %v", err)
	}
}

// TestReadBackMismatch 确认写入后读回不一致（corrupt）或读不到（drop）时 Set 与 Add 都报告 ErrReadBackMismatch。
func TestReadBackMismatch(t *testing.T) {
	for _, mode := range []string{"corrupt", "drop"} {
		t.Run(mode, func(t *testing.T) {
			f := newFake(t, mode)
			ctx := context.Background()
			secret := strings.Repeat("aB3", 4)
			if err := f.security.Set(ctx, AuthCodeAccount(testInstance), secret); !errors.Is(err, ErrReadBackMismatch) {
				t.Errorf("Set: %v", err)
			}
			if err := f.security.Add(ctx, TokenKeyAccount(testInstance, 1), secret); !errors.Is(err, ErrReadBackMismatch) {
				t.Errorf("Add: %v", err)
			}
		})
	}
}

// TestGetRejectsMalformedOutput 确认 Get 只接受恰好一个结尾换行的合法机密，超长输出被拒绝，且错误文本不含读到的内容。
func TestGetRejectsMalformedOutput(t *testing.T) {
	f := newFake(t, "output")
	cases := []struct {
		output string
		leak   string
	}{
		{"abc", "abc"},
		{"ab c\n", "ab c"},
		{"abc\n\n", "abc"},
		{strings.Repeat("Q", 4097), "QQQ"},
		{strings.Repeat("Q", 1001) + "\n", "QQQ"},
	}
	for index, test := range cases {
		if err := os.WriteFile(f.state, []byte(test.output), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := f.security.Get(context.Background(), AuthCodeAccount(testInstance))
		if !errors.Is(err, ErrInvalidSecret) || strings.Contains(err.Error(), test.leak) {
			t.Errorf("case %d: %v", index, err)
		}
	}
}

// TestValidationBeforeProcess 确认非法 account 与机密在启动任何进程之前就被拒绝，且错误文本不回显输入。
func TestValidationBeforeProcess(t *testing.T) {
	f := newFake(t, "normal")
	ctx := context.Background()
	secret := strings.Repeat("aB3", 4)
	for _, account := range []string{"", "a b", "a'b", `a"b`, "中文", strings.Repeat("a", 129)} {
		_, getErr := f.security.Get(ctx, account)
		errs := []error{getErr, f.security.Set(ctx, account, secret), f.security.Add(ctx, account, secret), f.security.Delete(ctx, account)}
		for index, err := range errs {
			if !errors.Is(err, ErrInvalidName) || (account != "" && strings.Contains(err.Error(), account)) {
				t.Errorf("account %q, operation %d: %v", account, index, err)
			}
		}
	}
	account := AuthCodeAccount(testInstance)
	for _, bad := range []string{"", "a b", "a\tb", "a\nb", "a\x7fb", "é", strings.Repeat("a", 1001)} {
		for index, err := range []error{f.security.Set(ctx, account, bad), f.security.Add(ctx, account, bad)} {
			if !errors.Is(err, ErrInvalidSecret) || (bad != "" && strings.Contains(err.Error(), bad)) {
				t.Errorf("secret case %q, operation %d: %v", bad, index, err)
			}
		}
	}
	if _, err := os.Stat(f.log); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("fake security ran for invalid input: %v", err)
	}
}

// TestTimeout 确认期限到达时返回包装 context.DeadlineExceeded 的错误并结束子进程；已取消的 ctx 报告取消；
// 期限落在写入后的读回核对中时报告超时而不是 ErrReadBackMismatch。
// 慢机器上假程序可能来不及写日志就被结束，因此最多重试几次以取得它的进程号。
func TestTimeout(t *testing.T) {
	f := newFake(t, "hang")
	f.security.timeout = 200 * time.Millisecond
	account := AuthCodeAccount(testInstance)
	pid := 0
	for attempt := 0; attempt < 5 && pid == 0; attempt++ {
		start := time.Now()
		_, err := f.security.Get(context.Background(), account)
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("Get took %v", elapsed)
		}
		if !errors.Is(err, context.DeadlineExceeded) || !strings.HasPrefix(err.Error(), "keychain get timed out") {
			t.Fatalf("hang: %v", err)
		}
		if calls := f.calls(t); len(calls) > 0 {
			pid = calls[len(calls)-1].PID
		}
	}
	if pid == 0 {
		t.Fatal("fake security never started before the deadline")
	}
	if processExists(pid) {
		t.Errorf("security process %d survived the deadline", pid)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.security.Get(ctx, account); !errors.Is(err, context.Canceled) || !strings.HasPrefix(err.Error(), "keychain get canceled") {
		t.Errorf("cancelled: %v", err)
	}

	// 写入照常完成、读回挂起；1 秒的期限给写入留足时间，使期限落在读回中。
	f.security.timeout = time.Second
	err := f.security.Set(context.Background(), account, strings.Repeat("aB3", 4))
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrReadBackMismatch) || !strings.HasPrefix(err.Error(), "keychain set timed out") {
		t.Errorf("read-back timeout: %v", err)
	}
	if calls := f.calls(t); calls[len(calls)-1].Args[0] != "find-generic-password" {
		t.Error("deadline did not expire during the read-back")
	}
}

// TestAdd 确认 Add 只创建：先读后以不带 -U 的命令写入并读回；已存在时不启动写入进程、原值不变；
// 并发写入者先写入时报告 ErrExists；写入失败且条目仍不存在时返回原错误。
func TestAdd(t *testing.T) {
	f := newFake(t, "normal")
	ctx := context.Background()
	account := TokenKeyAccount(testInstance, 1)
	keyText, err := NewKeyText(bytes.NewReader(sequence(32)))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.security.Add(ctx, account, keyText); err != nil {
		t.Fatalf("Add: %v", err)
	}
	calls := f.calls(t)
	wantFind := []string{"find-generic-password", "-s", Service, "-a", account, "-w"}
	keyHex := hex.EncodeToString([]byte(keyText))
	wantStdin := "add-generic-password -s io.github.chaorookie.turncourier -a " + account + " -X " + keyHex + "\n"
	if len(calls) != 3 || !slices.Equal(calls[0].Args, wantFind) || !slices.Equal(calls[1].Args, []string{"-i"}) ||
		calls[1].Stdin != wantStdin || !slices.Equal(calls[2].Args, wantFind) {
		t.Fatalf("unexpected call sequence: %d calls", len(calls))
	}
	assertNotExposed(t, calls, keyText, keyHex)

	if err := f.security.Add(ctx, account, strings.Repeat("aB3", 4)); !errors.Is(err, ErrExists) {
		t.Errorf("Add over existing item: %v", err)
	}
	if f.items(t)[account] != keyText {
		t.Error("existing item was modified")
	}
	if interactiveCalls(f.calls(t)) != 1 {
		t.Error("Add started a write process for an existing item")
	}

	t.Setenv(envMode, "add-exists")
	raced := PayloadKeyAccount(testInstance, 1)
	if err := f.security.Add(ctx, raced, keyText); !errors.Is(err, ErrExists) {
		t.Errorf("Add losing a race: %v", err)
	}
	if f.items(t)[raced] != rivalValue {
		t.Error("the other writer's item was modified")
	}

	t.Setenv(envMode, "add-fails")
	failed := PayloadKeyAccount(testInstance, 2)
	if err := f.security.Add(ctx, failed, keyText); err == nil || errors.Is(err, ErrExists) ||
		err.Error() != "keychain add failed with exit code 1" {
		t.Errorf("failed Add: %v", err)
	}
}

// TestDelete 确认删除的参数，删除后读不到，删除不存在的条目返回 ErrNotFound。
func TestDelete(t *testing.T) {
	f := newFake(t, "normal")
	ctx := context.Background()
	account := AuthCodeAccount(testInstance)
	if err := f.security.Set(ctx, account, strings.Repeat("aB3", 4)); err != nil {
		t.Fatal(err)
	}
	if err := f.security.Delete(ctx, account); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	calls := f.calls(t)
	if want := []string{"delete-generic-password", "-s", Service, "-a", account}; !slices.Equal(calls[len(calls)-1].Args, want) {
		t.Errorf("unexpected delete argv: %q", calls[len(calls)-1].Args)
	}
	if _, err := f.security.Get(ctx, account); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete: %v", err)
	}
	if err := f.security.Delete(ctx, account); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete of missing item: %v", err)
	}
}

// TestNewFor 确认只在 darwin 返回固定路径的实现，期限默认值与交互期限正确，其他平台返回 ErrUnsupported。
func TestNewFor(t *testing.T) {
	if Service != "io.github.chaorookie.turncourier" || DefaultTimeout != 10*time.Second || InteractiveTimeout != 60*time.Second {
		t.Fatal("unexpected package constants")
	}
	s, err := newFor("darwin", 0)
	if err != nil || s.path != "/usr/bin/security" || s.timeout != DefaultTimeout {
		t.Errorf("darwin default: %+v %v", s, err)
	}
	if s, err = newFor("darwin", InteractiveTimeout); err != nil || s.timeout != 60*time.Second {
		t.Errorf("darwin interactive: %+v %v", s, err)
	}
	for _, goos := range []string{"linux", "windows"} {
		if s, err := newFor(goos, 0); s != nil || !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: %+v %v", goos, s, err)
		}
	}
	s, err = New(0)
	if runtime.GOOS == "darwin" {
		if err != nil || s.path != "/usr/bin/security" {
			t.Errorf("New on darwin: %+v %v", s, err)
		}
	} else if s != nil || !errors.Is(err, ErrUnsupported) {
		t.Errorf("New on %s: %+v %v", runtime.GOOS, s, err)
	}
}

// TestAccountNames 钉住三类条目的 account 格式，确认它们满足名称规则，kid 为 0 时 panic。
func TestAccountNames(t *testing.T) {
	names := map[string]string{
		AuthCodeAccount(testInstance):        "0123456789abcdef:qq-auth-code",
		TokenKeyAccount(testInstance, 1):     testInstance + ":token-key-1",
		PayloadKeyAccount(testInstance, 255): testInstance + ":payload-key-255",
	}
	for got, want := range names {
		if got != want || !validAccount(got) {
			t.Errorf("account %q, want %q", got, want)
		}
	}
	if !validAccount("AZaz09._@:+-") || !validSecret("!~") {
		t.Error("allowed name or secret characters rejected")
	}
	assertPanics(t, "TokenKeyAccount kid 0", func() { TokenKeyAccount(testInstance, 0) })
	assertPanics(t, "PayloadKeyAccount kid 0", func() { PayloadKeyAccount(testInstance, 0) })
}

// TestKeyText 确认密钥文本是 32 字节的 base64url 无填充编码，能还原，非规范文本一律返回不回显输入的 ErrInvalidSecret。
func TestKeyText(t *testing.T) {
	raw := sequence(32)
	text, err := NewKeyText(bytes.NewReader(raw))
	if err != nil || text != base64.RawURLEncoding.EncodeToString(raw) || len(text) != 43 {
		t.Fatalf("NewKeyText: %v", err)
	}
	if _, err := NewKeyText(bytes.NewReader(raw[:31])); err == nil {
		t.Error("short random source accepted")
	}
	if key, err := DecodeKeyText(text); err != nil || !bytes.Equal(key, raw) {
		t.Errorf("DecodeKeyText: %v", err)
	}
	// 32 字节的末字符只承载 4 个数据比特，规范编码的低 2 位为 0；该字符值加 1 仍在同一字母段内，
	// 得到解码结果相同但不规范的文本。解码器忽略换行，31 字节的规范编码加一个换行恰为 43 个字符且能解码。
	invalid := []string{
		text[:42], text + "A", text[:42] + "=", text + "=",
		"+" + text[1:], "/" + text[1:], text[:42] + string(text[42]+1),
		base64.RawURLEncoding.EncodeToString(raw[:31]) + "\n", text + "\n",
	}
	for index, bad := range invalid {
		key, err := DecodeKeyText(bad)
		if key != nil || !errors.Is(err, ErrInvalidSecret) || err.Error() != ErrInvalidSecret.Error() {
			t.Errorf("case %d accepted or echoed: %v", index, err)
		}
	}
}

// referenceKeyCheck 按规格逐字节拼接校验值的 HMAC 输入，不调用被测函数。
func referenceKeyCheck(purpose string, kid uint8, key []byte) [8]byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("turncourier/key-check/v1\x00" + purpose))
	mac.Write([]byte{kid})
	var check [8]byte
	copy(check[:], mac.Sum(nil))
	return check
}

// TestKeyCheck 确认校验值与参考实现一致，绑定用途、kid 与密钥，域分隔前缀与令牌 MAC、键控摘要的前缀互不为前缀，
// 未知用途或 kid 为 0 时 panic。
func TestKeyCheck(t *testing.T) {
	key := sequence(32)
	for _, purpose := range []string{"token", "payload"} {
		for _, kid := range []uint8{1, 255} {
			if got := KeyCheck(purpose, kid, key); got != referenceKeyCheck(purpose, kid, key) {
				t.Errorf("KeyCheck(%s, %d) differs from reference", purpose, kid)
			}
		}
	}
	other := sequence(32)
	other[0] ^= 1
	base := KeyCheck("token", 1, key)
	if base == KeyCheck("payload", 1, key) || base == KeyCheck("token", 2, key) || base == KeyCheck("token", 1, other) {
		t.Error("key check does not bind purpose, kid and key")
	}
	// 后两个前缀来自 Task 3 的令牌契约：正文键控摘要与令牌 MAC。
	prefixes := []string{keyCheckPrefix, "turncourier/body-digest/v1\x00", "turncourier/reply-token/v1\x00"}
	for i, left := range prefixes {
		for j, right := range prefixes {
			if i != j && strings.HasPrefix(left, right) {
				t.Errorf("prefix %d starts with prefix %d", i, j)
			}
		}
	}
	for _, purpose := range []string{"", "Token", "body", "token "} {
		assertPanics(t, "KeyCheck purpose "+purpose, func() { KeyCheck(purpose, 1, key) })
	}
	assertPanics(t, "KeyCheck token kid 0", func() { KeyCheck("token", 0, key) })
	assertPanics(t, "KeyCheck payload kid 0", func() { KeyCheck("payload", 0, key) })
}
