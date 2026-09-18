// Package main 验证装配入口不会在帮助或无效命令中启动 Agent。
package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
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
