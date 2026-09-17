//go:build !unix

// Package doctor 在非 Unix 平台沿用 exec.CommandContext 默认的取消方式，不创建进程组。
package doctor

import "os/exec"

// configureProcess 在非 Unix 平台保持命令默认配置；超时只终止直接子进程，不清理其派生的孙进程。
func configureProcess(*exec.Cmd) {}
