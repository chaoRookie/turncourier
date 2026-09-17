// Package main 装配命令入口及只读诊断依赖。
package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/chaoRookie/turncourier/internal/cli"
	"github.com/chaoRookie/turncourier/internal/doctor"
)

// main 将中断信号和 SIGTERM 转为上下文取消，使诊断能终止版本检查并输出已取消结果，
// 再将命令退出码传递给操作系统。
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run 将标准流与诊断器注入 CLI，便于不退出测试进程地验证入口行为。
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return cli.Run(ctx, args, stdout, stderr, doctor.New())
}
