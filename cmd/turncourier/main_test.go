// Package main 验证装配入口不会在帮助或无效命令中启动 Agent，且 init 在非交互环境中不提问、不写任何文件。
package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/term"
)

// TestRun 验证真实入口的帮助输出和未实现命令退出码，不访问外部工具。
func TestRun(t *testing.T) {
	cases := []struct {
		args []string
		code int
		want string
	}{
		{nil, 0, "pre-alpha"},
		{[]string{"version"}, 0, "turncourier"},
		{[]string{"run", "codex"}, 2, "尚未实现"},
	}
	for _, test := range cases {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), test.args, &stdout, &stderr); code != test.code {
			t.Errorf("exit code: got %d want %d", code, test.code)
		}
		if !strings.Contains(stdout.String()+stderr.String(), test.want) {
			t.Errorf("missing expected output %q", test.want)
		}
	}
}

// TestCancelledDoctor 验证入口传递取消信号，取消后的诊断不会查找真实工具。
func TestCancelledDoctor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"doctor", "--json"}, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "cancelled") {
		t.Fatalf("cancelled command failed: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// TestRunInitWithoutTerminal 以生产装配运行 init：go test 以 /dev/null 作为标准输入，Interactive 为 false。
// darwin 上返回 1 并提示需要交互式终端，其他平台返回 1 并提示只支持 macOS；两种情况都在调用 Keychain 的任何方法之前退出，
// 不启动 security，临时目录中没有新建文件。标准输入是终端时（直接在终端里运行编译出的测试二进制）跳过，以免真实提问并写入登录钥匙串。
func TestRunInitWithoutTerminal(t *testing.T) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("标准输入是终端：跳过，以免真实提问并写入登录钥匙串")
	}
	dir := t.TempDir()
	t.Setenv("TURNCOURIER_CONFIG", filepath.Join(dir, "config", "turncourier.toml"))
	t.Setenv("TURNCOURIER_DATA_DIR", filepath.Join(dir, "data"))
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"init"}, &stdout, &stderr)
	want := "只支持 macOS"
	if runtime.GOOS == "darwin" {
		want = "需要在交互式终端中运行"
	}
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), want) {
		t.Errorf("init: code=%d stdout=%q stderr=%q; want 1 与 %q", code, stdout.String(), stderr.String(), want)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("临时目录中出现新文件: %v, %v", entries, err)
	}
}
