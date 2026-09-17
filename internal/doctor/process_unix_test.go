//go:build unix

package doctor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunVersionKillsProcessGroup 确认 Unix 上取消版本检查会终止整个进程组，包装脚本派生的孙进程不会遗留。
// 超时与取消走同一个 Cancel；这里等孙进程 PID 写出后再取消，避免依赖紧凑的时序。
func TestRunVersionKillsProcessGroup(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("TURNCOURIER_TEST_VERSION_PROCESS", "group")
	t.Setenv("TURNCOURIER_TEST_PID_FILE", pidFile)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := runVersion(ctx, executable)
		result <- err
	}()
	pid := readPID(t, pidFile)
	cancel()
	if err := <-result; err == nil {
		t.Error("cancelled version process should fail")
	}
	deadline := time.Now().Add(10 * time.Second)
	for processExists(pid) {
		if time.Now().After(deadline) {
			killProcess(pid)
			t.Fatalf("grandchild %d survived cancellation", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
