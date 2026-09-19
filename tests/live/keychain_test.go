//go:build live

// Package live_test 的真实钥匙串往返：在登录钥匙串中写入、读回、重复添加与删除一个合成条目，确认 Delete 之后
// Get 返回 ErrNotFound、Add 对已存在的条目返回 ErrExists 且不改动原值，并记录 security 重复添加时的退出码。
// 只使用随机生成的合成 account 与合成值，从不读写授权码与密钥等真实条目；每次写入之前都经 /dev/tty 确认。
package live_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/chaoRookie/turncourier/internal/security/keychain"
)

// securityPath 是系统自带的 security 工具，按绝对路径启动，不经 PATH 查找。
const securityPath = "/usr/bin/security"

// keychainRecord 是 TestL1KeychainRoundTrip 写入样本的内容：往返是否完成，以及重复添加时 security 的退出码。
type keychainRecord struct {
	Status           string `json:"status"`
	DuplicateAddExit int    `json:"duplicate_add_exit,omitempty"`
	Kept             bool   `json:"kept,omitempty"`
}

// TestL1KeychainRoundTrip 在真实钥匙串中完成一个合成条目的写入、读回、重复添加与删除。
// 设置 TURNCOURIER_LIVE_KEEP=1 时，删除之前在 /dev/tty 上显示合成 account 与「合成值加一个换行符」的 SHA-256，
// 等待维护者按回车；显示的哈希与 security find-generic-password … -w | shasum -a 256 的口径相同，
// 可用于在另一个终端做沙箱读取检查。本探测不发信。
func TestL1KeychainRoundTrip(t *testing.T) {
	store, err := keychain.New(keychain.InteractiveTimeout)
	if err != nil {
		t.Fatalf("无法使用钥匙串：%v", err)
	}
	account := "live-test-" + randomID(t, 16)
	secret, replacement := randomSecret(t), randomSecret(t)
	ctx, cancel := probeTimeout(10 * time.Minute)
	defer cancel()
	if !confirm(t, fmt.Sprintf("将在登录钥匙串中写入一个合成条目（service %s，account %s），随后读回、尝试重复添加并删除；不会读写任何真实条目。",
		keychain.Service, account)) {
		record(t, "keychain", keychainRecord{Status: "skipped"})
		t.Skip("已跳过真实钥匙串往返")
	}
	// 用例中途失败时也不把合成条目留在钥匙串中。
	t.Cleanup(func() {
		if err := store.Delete(context.Background(), account); err != nil && !errors.Is(err, keychain.ErrNotFound) {
			t.Errorf("清理合成条目失败：%v", err)
		}
	})

	if err := store.Set(ctx, account, secret); err != nil {
		t.Fatalf("写入合成条目失败：%v", err)
	}
	if got, err := store.Get(ctx, account); err != nil || got != secret {
		t.Fatalf("读回合成条目失败：err=%v，内容一致=%v", err, err == nil && got == secret)
	}
	if err := store.Add(ctx, account, replacement); !errors.Is(err, keychain.ErrExists) {
		t.Errorf("对已存在的条目调用 Add 返回 %v，期望 ErrExists", err)
	}
	if got, err := store.Get(ctx, account); err != nil || got != secret {
		t.Errorf("Add 之后原值被改动：err=%v，内容一致=%v", err, err == nil && got == secret)
	}

	data := keychainRecord{Status: "ok"}
	if confirm(t, fmt.Sprintf("将再次直接调用 security，对同一合成条目（account %s）执行不带 -U 的 add-generic-password，只为记录退出码；预期失败且原值不变。", account)) {
		data.DuplicateAddExit = rawAdd(ctx, t, account, replacement)
		t.Logf("security 重复添加的退出码：%d", data.DuplicateAddExit)
		if got, err := store.Get(ctx, account); err != nil || got != secret {
			t.Errorf("security 重复添加之后原值被改动：err=%v，内容一致=%v", err, err == nil && got == secret)
		}
	}
	if os.Getenv(keepEnv) == "1" {
		keepForSandboxCheck(t, account, secret)
		data.Kept = true
	}
	if err := store.Delete(ctx, account); err != nil {
		t.Fatalf("删除合成条目失败：%v", err)
	}
	if _, err := store.Get(ctx, account); !errors.Is(err, keychain.ErrNotFound) {
		t.Errorf("删除之后 Get 返回 %v，期望 ErrNotFound", err)
	}
	record(t, "keychain", data)
}

// rawAdd 直接启动一次 security -i，对已存在的合成条目执行不带 -U 的 add-generic-password，返回退出码。
// keychain.Add 先 Get 再写入，不依赖这个退出码，L1 只作记录；合成值仍只经标准输入以十六进制传入，不出现在进程参数中，
// 标准输出与标准错误一并丢弃。
func rawAdd(ctx context.Context, t *testing.T, account, secret string) int {
	t.Helper()
	command := exec.CommandContext(ctx, securityPath, "-i")
	command.Stdin = strings.NewReader("add-generic-password -s " + keychain.Service + " -a " + account + " -X " + hex.EncodeToString([]byte(secret)) + "\n")
	command.Stdout, command.Stderr = io.Discard, io.Discard
	err := command.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		return exitErr.ExitCode()
	default:
		t.Fatalf("无法运行 security：%v", err)
		return -1
	}
}

// keepForSandboxCheck 保留合成条目并在 /dev/tty 上显示 account 与「合成值加一个换行符」的 SHA-256，等待维护者按回车。
// security find-generic-password … -w 的输出正是「值加一个结尾换行」，因此管道到 shasum -a 256 得到同一个摘要；
// 条目保留期间，可在另一个终端的沙箱中执行该命令，比对摘要即可判断沙箱能否读出机密数据。
func keepForSandboxCheck(t *testing.T, account, secret string) {
	t.Helper()
	digest := sha256.Sum256([]byte(secret + "\n"))
	text := fmt.Sprintf("合成条目已保留，可在另一个终端做沙箱读取检查：\n  account：%s\n  值加换行的 SHA-256：%s\n检查完成后按回车删除该条目：",
		account, hex.EncodeToString(digest[:]))
	if _, err := ttyLine(text); err != nil {
		t.Fatalf("%v", err)
	}
}
