//go:build darwin

// Package cli 的伪终端测试在 macOS 的 /dev/ptmx 上验证终端生产实现：交互判断要求标准输入与标准输出都是终端，
// ReadLine 之后 ReadSecret 不回显地读到下一行并恢复回显，取消后回显恢复（包括在读取刚开始时取消），ctx 已结束时不写提示。
package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPTY 打开一对伪终端并返回主端与从端，测试结束时先关闭主端再关闭从端：主端关闭即挂断，
// 阻塞在从端上的读取（被放弃的读取协程）随之返回，关闭从端时不会等待它们。
func openPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	var slave *os.File
	t.Cleanup(func() {
		master.Close()
		if slave != nil {
			slave.Close()
		}
	})
	fd := int(master.Fd())
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0); err != nil {
		t.Fatal(err)
	}
	// TIOCPTYGNAME 把从端设备名写入 128 字节的缓冲区；x/sys 没有对应的封装，x/sys 的 SYS_IOCTL 在 macOS 上已标为弃用，这里用标准库的 syscall。
	var name [128]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&name[0]))); errno != 0 {
		t.Fatal(errno)
	}
	slave, err = os.OpenFile(string(name[:bytes.IndexByte(name[:], 0)]), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	return master, slave
}

// echoOn 报告终端 fd 当前是否开启回显。
func echoOn(t *testing.T, fd int) bool {
	t.Helper()
	termios, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		t.Fatal(err)
	}
	return termios.Lflag&unix.ECHO != 0
}

// waitEchoOff 等待 fd 的回显被关闭，3 秒内没有关闭即终止测试。
func waitEchoOff(t *testing.T, fd int) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); echoOn(t, fd); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("回显没有被关闭")
		}
	}
}

// ptyOutput 在后台读取伪终端主端，收集写到终端的全部内容（提示与回显）。
type ptyOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// start 启动读取主端的协程，主端关闭或出错时退出。
func (o *ptyOutput) start(master *os.File) {
	go func() {
		chunk := make([]byte, 1024)
		for {
			n, err := master.Read(chunk)
			o.mu.Lock()
			o.buf.Write(chunk[:n])
			o.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
}

// waitFor 等待收集到的内容以 suffix 结尾并返回全部内容，3 秒内没有等到即终止测试。
func (o *ptyOutput) waitFor(t *testing.T, suffix string) string {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(time.Millisecond) {
		o.mu.Lock()
		got := o.buf.String()
		o.mu.Unlock()
		if bytes.HasSuffix([]byte(got), []byte(suffix)) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("终端输出 = %q; 没有等到 %q", got, suffix)
		}
	}
}

// TestTerminalInteractivePTY 验证只有标准输入与标准输出都是终端时 Interactive 为 true：
// 标准输出换成普通文件、或标准输入换成管道时都为 false。
func TestTerminalInteractivePTY(t *testing.T) {
	_, slave := openPTY(t)
	in, _ := newPipe(t)
	cases := []struct {
		name    string
		in, out *os.File
		want    bool
	}{
		{"两端都是终端", slave, slave, true},
		{"标准输出是普通文件", slave, newOutput(t), false},
		{"标准输入是管道", in, slave, false},
	}
	for _, testCase := range cases {
		if got := NewTerminal(testCase.in, testCase.out).Interactive(); got != testCase.want {
			t.Errorf("%s: Interactive() = %v; want %v", testCase.name, got, testCase.want)
		}
	}
}

// TestTerminalReadSecretPTY 验证在真实终端设备上 ReadLine 之后 ReadSecret 读到下一行：读入期间回显关闭，
// 机密不出现在终端输出中，读完后回显恢复；ReadLine 没有吞掉属于 ReadSecret 的输入。
func TestTerminalReadSecretPTY(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	master, slave := openPTY(t)
	var output ptyOutput
	output.start(master)
	terminal := NewTerminal(slave, slave)
	if _, err := master.WriteString("bot@example.invalid\n"); err != nil {
		t.Fatal(err)
	}
	if line, err := terminal.ReadLine(ctx, "地址："); err != nil || line != "bot@example.invalid" {
		t.Fatalf("ReadLine = %q, %v", line, err)
	}

	secret := string(bytes.Repeat([]byte("pty"), 4))
	// result 是后台 ReadSecret 的返回值。
	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		text, err := terminal.ReadSecret(ctx, "授权码：")
		done <- result{text, err}
	}()
	waitEchoOff(t, int(slave.Fd()))
	if _, err := master.WriteString(secret + "\n"); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.err != nil || r.text != secret {
		t.Fatalf("ReadSecret = %q, %v; want 录入的机密", r.text, r.err)
	}
	if !echoOn(t, int(slave.Fd())) {
		t.Error("读完后回显没有恢复")
	}
	// 终端把输出的换行显示为回车加换行；机密被回显时会出现在「授权码：」与结尾换行之间。
	if got, want := output.waitFor(t, "授权码：\r\n"), "bot@example.invalid\r\n地址：授权码：\r\n"; got != want {
		t.Errorf("终端输出 = %q; want %q", got, want)
	}
}

// TestTerminalReadSecretCancelPTY 验证读取阻塞时取消 ctx，ReadSecret 返回 context.Canceled 且回显立即恢复。
func TestTerminalReadSecretCancelPTY(t *testing.T) {
	_, slave := openPTY(t)
	fd := int(slave.Fd())
	terminal := NewTerminal(slave, newOutput(t))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := terminal.ReadSecret(ctx, "授权码：")
		done <- err
	}()
	waitEchoOff(t, fd)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadSecret = %v; want context.Canceled", err)
	}
	if !echoOn(t, fd) {
		t.Error("取消后回显没有恢复")
	}
}

// TestTerminalReadSecretCancelRacePTY 反复在 ReadSecret 刚开始时取消 ctx：每一轮返回后稍等片刻，回显都必须开着。
// 回显若在读取协程中才关闭（例如在协程里调用 term.ReadPassword），取消恰在关闭之前发生时恢复会先于关闭，
// 协程随后关掉回显，进程退出后终端停留在不回显的状态；稍等片刻即可观察到。取消时机逐轮在 0–39 微秒之间变化，
// 以覆盖「检查 ctx 之后、关闭回显之前」的窗口。本机实测：在协程中调用 term.ReadPassword 的原实现，以及只把关闭回显
// 挪进读取协程的变异，每次运行都在 300 轮之内被拦住；回显在协程启动前同步关闭时不会出现假阳性。
func TestTerminalReadSecretCancelRacePTY(t *testing.T) {
	_, slave := openPTY(t)
	fd := int(slave.Fd())
	terminal := NewTerminal(slave, newOutput(t))
	for round := range 300 {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := terminal.ReadSecret(ctx, "")
			done <- err
		}()
		for start, delay := time.Now(), time.Duration(round%40)*time.Microsecond; time.Since(start) < delay; {
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("第 %d 轮 ReadSecret = %v; want context.Canceled", round, err)
		}
		time.Sleep(2 * time.Millisecond)
		if !echoOn(t, fd) {
			t.Fatalf("第 %d 轮取消后回显停留在关闭状态", round)
		}
	}
}

// TestTerminalReadSecretCanceledPTY 验证 ctx 已结束时 ReadSecret 在终端上直接返回 ctx.Err()：不写提示、不关闭回显。
func TestTerminalReadSecretCanceledPTY(t *testing.T) {
	_, slave := openPTY(t)
	out := newOutput(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got, err := NewTerminal(slave, out).ReadSecret(ctx, "授权码："); !errors.Is(err, context.Canceled) || got != "" {
		t.Errorf("ReadSecret = %q, %v; want context.Canceled", got, err)
	}
	if prompts, err := os.ReadFile(out.Name()); err != nil || len(prompts) != 0 {
		t.Errorf("提示输出 = %q, %v; want 空", prompts, err)
	}
	if !echoOn(t, int(slave.Fd())) {
		t.Error("回显被关闭")
	}
}
