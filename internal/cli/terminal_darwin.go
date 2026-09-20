//go:build darwin

// Package cli 在 macOS 上经 termios 关闭终端回显，供 ReadSecret 在启动读取协程之前同步调用。
package cli

import "golang.org/x/sys/unix"

// disableEcho 关闭 fd 的回显，保留规范模式与信号键（Ctrl-C 仍产生 SIGINT），并把回车映射为换行；
// 设置的标志与 term.ReadPassword 相同。
func disableEcho(fd int) error {
	termios, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return err
	}
	termios.Lflag &^= unix.ECHO
	termios.Lflag |= unix.ICANON | unix.ISIG
	termios.Iflag |= unix.ICRNL
	return unix.IoctlSetTermios(fd, unix.TIOCSETA, termios)
}
