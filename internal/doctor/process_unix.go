//go:build unix

// Package doctor 在 Unix 平台把版本子进程放入独立进程组，超时或取消时整体终止。
package doctor

import (
	"os/exec"
	"syscall"
)

// configureProcess 让版本子进程成为独立进程组的组长：终端 Ctrl-C 只送达本进程，由 context 统一判定为取消；
// 超时或取消时向整个进程组发送 SIGKILL，避免包装脚本派生的孙进程继续运行并占住输出管道。
// 子进程恰好在取消时退出，Kill 可能返回 ESRCH 或 EPERM；该错误只影响返回的错误文本，
// checkTool 先按 context 判定超时或取消，不会误报成功。
func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
}
