// Package doctor 使用合成依赖和受控子进程验证诊断边界。
package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// healthyChecker 创建离线工具集合；不依赖测试机器是否安装真实 Agent。
func healthyChecker() Checker {
	versions := map[string]string{"git": "git version 2.50.0", "codex": "codex-cli 0.154.0", "claude": "2.1.263 (Claude Code)"}
	return Checker{
		GOOS:    "darwin",
		Lookup:  func(name string) (string, error) { return name, nil },
		Run:     func(_ context.Context, path string) ([]byte, error) { return []byte(versions[path] + "\n"), nil },
		Timeout: time.Second,
	}
}

// TestCheckHealthy 确认固定工具顺序与版本识别，不把依赖可用误认为产品已实现，也不把可执行文件路径写入报告。
func TestCheckHealthy(t *testing.T) {
	checker := healthyChecker()
	runner := checker.Run
	checker.Lookup = func(name string) (string, error) { return "/private/home/" + name, nil }
	checker.Run = func(ctx context.Context, path string) ([]byte, error) {
		return runner(ctx, strings.TrimPrefix(path, "/private/home/"))
	}
	report := checker.Check(context.Background())
	if !report.Ready || !report.Supported || report.Platform != "darwin" || len(report.Tools) != 3 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if strings.Contains(fmt.Sprint(report), "/private/home/") {
		t.Fatalf("path leaked: %+v", report)
	}
	for index, name := range []string{"git", "codex", "claude"} {
		tool := report.Tools[index]
		if tool.Name != name || tool.Status != "ok" || tool.Version == "" {
			t.Errorf("unexpected tool: %+v", tool)
		}
	}
}

// TestCheckUnsupportedOS 保留跨平台诊断，同时明确首版产品仅支持 macOS。
func TestCheckUnsupportedOS(t *testing.T) {
	for _, platform := range []string{"linux", "windows", "plan9"} {
		t.Run(platform, func(t *testing.T) {
			checker := healthyChecker()
			checker.GOOS = platform
			report := checker.Check(context.Background())
			if report.Ready || report.Supported || report.Platform != platform || report.Tools[0].Status != "ok" {
				t.Fatalf("unexpected report: %+v", report)
			}
		})
	}
}

// TestCheckFailures 确认缺失、非零退出（含已输出合法版本）和异常输出均使依赖检查失败且不泄露原始错误。
func TestCheckFailures(t *testing.T) {
	for _, failure := range []string{"missing", "error", "error-with-version", "invalid"} {
		t.Run(failure, func(t *testing.T) {
			checker := healthyChecker()
			lookup, runner := checker.Lookup, checker.Run
			checker.Lookup = func(name string) (string, error) {
				if name == "codex" && failure == "missing" {
					return "", errors.New("private path should not leak")
				}
				return lookup(name)
			}
			checker.Run = func(ctx context.Context, path string) ([]byte, error) {
				if path == "codex" {
					switch failure {
					case "error":
						return []byte("private output"), errors.New("private path should not leak")
					case "error-with-version":
						return []byte("codex-cli 0.1.0"), errors.New("private path should not leak")
					case "invalid":
						return []byte("login required: private output"), nil
					}
				}
				return runner(ctx, path)
			}
			report := checker.Check(context.Background())
			want := strings.TrimSuffix(failure, "-with-version")
			if report.Ready || report.Tools[1].Status != want || report.Tools[1].Version != "" {
				t.Fatalf("unexpected report: %+v", report)
			}
			if strings.Contains(fmt.Sprint(report), "private") {
				t.Fatal("raw failure data leaked into report")
			}
			if report.Tools[2].Status != "ok" {
				t.Fatal("one missing dependency should not prevent later checks")
			}
		})
	}
}

// TestCheckTimeout 让注入的执行器像被 exec 杀死的进程一样返回非 context 错误，验证超时不会误报成功或执行失败。
func TestCheckTimeout(t *testing.T) {
	checker := healthyChecker()
	checker.Timeout = time.Millisecond
	checker.Run = func(ctx context.Context, _ string) ([]byte, error) {
		<-ctx.Done()
		return nil, errors.New("signal: killed")
	}
	report := checker.Check(context.Background())
	for _, tool := range report.Tools {
		if tool.Status != "timeout" {
			t.Fatalf("expected timeout: %+v", tool)
		}
	}
}

// TestCheckTimeoutWithRealProcess 用真实 runVersion 运行等待中的测试子进程，确认进程被杀后仍报告超时。
func TestCheckTimeoutWithRealProcess(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TURNCOURIER_TEST_VERSION_PROCESS", "wait")
	checker := Checker{
		GOOS:    "darwin",
		Lookup:  func(string) (string, error) { return executable, nil },
		Run:     runVersion,
		Timeout: 200 * time.Millisecond,
	}
	report := checker.Check(context.Background())
	for _, tool := range report.Tools {
		if tool.Status != "timeout" {
			t.Fatalf("expected timeout: %+v", tool)
		}
	}
}

// TestCheckCancelled 确保已有取消信号时不查找工具也不启动进程。
func TestCheckCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	checker := healthyChecker()
	checker.Lookup = func(string) (string, error) { t.Fatal("lookup after cancellation"); return "", nil }
	report := checker.Check(ctx)
	for _, tool := range report.Tools {
		if tool.Status != "cancelled" {
			t.Fatalf("expected cancellation: %+v", tool)
		}
	}
}

// TestCheckCancelledDuringRun 检查运行中取消时后续工具保持取消状态。
func TestCheckCancelledDuringRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checker := healthyChecker()
	checker.Run = func(context.Context, string) ([]byte, error) { cancel(); return nil, context.Canceled }
	report := checker.Check(ctx)
	for _, tool := range report.Tools {
		if tool.Status != "cancelled" {
			t.Fatalf("expected cancellation: %+v", tool)
		}
	}
}

// TestVersionLineRejectsAnomalies 覆盖乱码、多行、控制字符、空输出和未知格式。
// 最后三个用例带合法前缀，分别只能被 UTF-8、单行长度和总字节上限拦下。
func TestVersionLineRejectsAnomalies(t *testing.T) {
	cases := []struct {
		name string
		text []byte
	}{
		{"git", nil}, {"git", []byte("git version 2.0\nextra")},
		{"git", []byte("git version 2.0\x00")}, {"git", []byte{0xff}},
		{"git", []byte(strings.Repeat("x", 257))}, {"git", []byte(strings.Repeat("x", maxVersionBytes+1))},
		{"codex", []byte("git version 2.0")}, {"claude", []byte("2.1.263")},
		{"claude", []byte("not-a-version (Claude Code)")}, {"other", []byte("other 1.0")},
		{"git", []byte("git version 2.50.0\xff")},
		{"git", []byte("git version 2" + strings.Repeat("0", 300))},
		{"git", []byte("git version 2.0" + strings.Repeat(" ", maxVersionBytes))},
	}
	for _, test := range cases {
		if output, valid := versionLine(test.name, test.text); valid {
			t.Errorf("accepted invalid %s version %q", test.name, output)
		}
	}
}

// TestLimitedOutput 检查缓冲区在上限内完整写入，超限后不接受部分内容并记录超限。
func TestLimitedOutput(t *testing.T) {
	buffer := &limitedOutput{}
	if written, err := buffer.Write(make([]byte, maxVersionBytes)); written != maxVersionBytes || err != nil || buffer.overflow {
		t.Fatalf("valid write failed: %d %v", written, err)
	}
	if written, err := buffer.Write([]byte("x")); written != 0 || err == nil || buffer.buffer.Len() != maxVersionBytes || !buffer.overflow {
		t.Fatalf("overflow was not rejected: %d %v", written, err)
	}
}

// TestMain 为版本执行器提供受控测试子进程；正常测试流程不设置该专用环境变量。
func TestMain(m *testing.M) {
	if mode := os.Getenv("TURNCOURIER_TEST_VERSION_PROCESS"); mode != "" {
		if len(os.Args) != 2 || os.Args[1] != "--version" {
			os.Exit(10)
		}
		switch mode {
		case "ok":
			fmt.Println("git version 2.50.0")
		case "error":
			os.Exit(11)
		case "large":
			fmt.Print(strings.Repeat("x", maxVersionBytes+1))
		case "wait":
			time.Sleep(time.Minute)
		case "background":
			startStdoutHolder()
			fmt.Println("git version 2.50.0")
		case "group":
			startStdoutHolder()
			time.Sleep(time.Minute)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// startStdoutHolder 在测试子进程中以 wait 模式派生继承 stdout 的孙进程，模拟包装脚本遗留的后台进程；
// 孙进程 PID 先写临时文件再重命名，测试读取时不会看到写了一半的内容。
func startStdoutHolder() {
	holder := exec.Command(os.Args[0], "--version")
	holder.Env = append(os.Environ(), "TURNCOURIER_TEST_VERSION_PROCESS=wait")
	holder.Stdout = os.Stdout
	if holder.Start() != nil {
		os.Exit(12)
	}
	pidFile := os.Getenv("TURNCOURIER_TEST_PID_FILE")
	pid := []byte(strconv.Itoa(holder.Process.Pid))
	if os.WriteFile(pidFile+".tmp", pid, 0o600) != nil || os.Rename(pidFile+".tmp", pidFile) != nil {
		os.Exit(13)
	}
}

// readPID 轮询测试子进程写入的孙进程 PID；宽松的截止时间只用于防止辅助进程异常时测试永久挂起。
func readPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, err := os.ReadFile(pidFile); err == nil {
			pid, err := strconv.Atoi(string(data))
			if err != nil {
				t.Fatalf("invalid pid file: %q", data)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatal("helper process did not report grandchild pid")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// processExists 用 0 号信号探测进程是否仍存在；尚未被回收的僵尸进程也视为存在。
func processExists(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	defer process.Release()
	return process.Signal(syscall.Signal(0)) == nil
}

// killProcess 终止测试派生的孙进程，避免其在测试结束后继续运行。
func killProcess(pid int) {
	if process, err := os.FindProcess(pid); err == nil {
		_ = process.Kill()
		_ = process.Release()
	}
}

// TestRunVersion 验证真实进程调用固定参数、错误退出、输出上限及取消行为。
func TestRunVersion(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"ok", "error", "large", "wait"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("TURNCOURIER_TEST_VERSION_PROCESS", mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if mode == "wait" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
			}
			defer cancel()
			output, err := runVersion(ctx, executable)
			if mode == "ok" {
				if err != nil || string(output) != "git version 2.50.0\n" {
					t.Fatalf("successful process: %q %v", output, err)
				}
			} else if err == nil {
				t.Fatal("expected process failure")
			}
		})
	}
}

// TestRunVersionBackgroundHolder 确认直接子进程成功退出、后台孙进程仍占住 stdout 时，
// runVersion 在 WaitDelay 后返回已读到的合法输出，而不是把 ErrWaitDelay 当作执行失败。
func TestRunVersionBackgroundHolder(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("TURNCOURIER_TEST_VERSION_PROCESS", "background")
	t.Setenv("TURNCOURIER_TEST_PID_FILE", pidFile)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := runVersion(ctx, executable)
	killProcess(readPID(t, pidFile))
	if err != nil || string(output) != "git version 2.50.0\n" {
		t.Fatalf("background holder caused failure: %q %v", output, err)
	}
}

// TestNew 检查生产装配的默认依赖与有限等待时间。
func TestNew(t *testing.T) {
	checker := New()
	if checker.GOOS == "" || checker.Lookup == nil || checker.Run == nil || checker.Timeout != 3*time.Second {
		t.Fatalf("invalid defaults: %+v", checker)
	}
}
