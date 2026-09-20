// Package main 装配命令入口：doctor 的只读诊断依赖，以及 init 的终端、Keychain、路径、随机源与时钟。
package main

import (
	"context"
	"crypto/rand"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chaoRookie/turncourier/internal/cli"
	"github.com/chaoRookie/turncourier/internal/doctor"
	"github.com/chaoRookie/turncourier/internal/security/keychain"
)

// main 将中断信号和 SIGTERM 转为上下文取消，使诊断能终止版本检查并输出已取消结果、init 能恢复终端回显后退出，
// 再将命令退出码传递给操作系统。
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run 将标准流与生产依赖注入 CLI，便于不退出测试进程地验证入口行为。
// init 的提示与读入直接使用进程的标准输入与标准输出终端，摘要与错误写到传入的 stdout 与 stderr。
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return cli.Run(ctx, args, stdout, stderr, cli.Deps{
		Checker: doctor.New(),
		Init: cli.InitDeps{
			Terminal:      cli.NewTerminal(os.Stdin, os.Stdout),
			Keychain:      func() (keychain.Store, error) { return keychain.New(keychain.InteractiveTimeout) },
			Getenv:        os.Getenv,
			UserConfigDir: os.UserConfigDir,
			Random:        rand.Reader,
			Now:           time.Now,
		},
	})
}
