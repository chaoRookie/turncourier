// Package cli 的终端测试用 os.Pipe 与临时文件构造非终端的输入输出，验证非交互判断、逐字节读入一行与非终端上拒绝读入机密；
// 真实终端设备上的行为（交互判断、不回显读入机密、取消后恢复回显）由 terminal_darwin_test.go 在 macOS 的伪终端上验证。
package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newPipe 返回一对管道端点，测试结束时关闭两端，使被放弃的读取协程退出。
func newPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		w.Close()
		r.Close()
	})
	return r, w
}

// newOutput 返回用于接收提示的临时文件。
func newOutput(t *testing.T) *os.File {
	t.Helper()
	out, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { out.Close() })
	return out
}

// TestTerminalNotInteractive 验证标准输入是管道、标准输出是管道或普通文件时 Interactive 为 false。
func TestTerminalNotInteractive(t *testing.T) {
	in, _ := newPipe(t)
	_, pipeOut := newPipe(t)
	for _, out := range []*os.File{pipeOut, newOutput(t)} {
		if NewTerminal(in, out).Interactive() {
			t.Error("管道输入不应被判为交互式终端")
		}
	}
}

// TestTerminalReadLine 验证 ReadLine 写出提示、读出一行并去掉结尾换行，1024 字节的行可以读入，超过 1024 字节时报错且不回显内容；
// 读取逐字节进行，一行之后的输入仍留在管道中，没有被缓冲读取器吞掉（吞掉时后续读取会阻塞，由 5 秒期限让测试尽快失败）。
func TestTerminalReadLine(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	in, writer := newPipe(t)
	out := newOutput(t)
	long, tooLong := strings.Repeat("a", 1024), strings.Repeat("b", 1025)
	if _, err := io.WriteString(writer, "first line\n\n"+long+"\n"+tooLong+"\nrest"); err != nil {
		t.Fatal(err)
	}
	terminal := NewTerminal(in, out)
	for i, want := range []string{"first line", "", long} {
		got, err := terminal.ReadLine(ctx, "提示：")
		if err != nil || got != want {
			t.Fatalf("第 %d 行 = %q, %v; want %q", i+1, got, err, want)
		}
	}
	if got, err := terminal.ReadLine(ctx, "提示："); err != errLineTooLong || got != "" {
		t.Fatalf("超长行 = %q, %v; want 不回显内容的错误", got, err)
	}
	// 超长行读到第 1025 字节即报错，不再读取，其后的换行与 rest 仍在管道中。
	writer.Close()
	remaining, err := io.ReadAll(in)
	if err != nil || string(remaining) != "\nrest" {
		t.Errorf("管道中剩余 %q, %v; want %q", remaining, err, "\nrest")
	}
	prompts, err := os.ReadFile(out.Name())
	if err != nil || string(prompts) != strings.Repeat("提示：", 4) {
		t.Errorf("提示输出 = %q, %v", prompts, err)
	}
}

// TestTerminalReadLineEOF 验证输入在换行之前结束时 ReadLine 报错，不返回不完整的一行。
func TestTerminalReadLineEOF(t *testing.T) {
	in, writer := newPipe(t)
	if _, err := io.WriteString(writer, "partial"); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	if got, err := NewTerminal(in, newOutput(t)).ReadLine(t.Context(), ""); err == nil || got != "" {
		t.Errorf("ReadLine = %q, %v; want 错误", got, err)
	}
}

// TestTerminalReadLineCancel 验证读取阻塞时 ctx 结束，ReadLine 立即返回 ctx.Err()。
func TestTerminalReadLineCancel(t *testing.T) {
	in, _ := newPipe(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got, err := NewTerminal(in, newOutput(t)).ReadLine(ctx, ""); err != context.Canceled || got != "" {
		t.Errorf("ReadLine = %q, %v; want context.Canceled", got, err)
	}
}

// TestTerminalReadSecretNotTerminal 验证标准输入不是终端时 ReadSecret 返回错误且不读取输入。
func TestTerminalReadSecretNotTerminal(t *testing.T) {
	in, writer := newPipe(t)
	if _, err := io.WriteString(writer, "typed\n"); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	if got, err := NewTerminal(in, newOutput(t)).ReadSecret(t.Context(), "机密："); err == nil || got != "" {
		t.Errorf("ReadSecret = %q, %v; want 错误", got, err)
	}
	if remaining, err := io.ReadAll(in); err != nil || string(remaining) != "typed\n" {
		t.Errorf("管道中剩余 %q, %v; want 输入未被读取", remaining, err)
	}
}
