//go:build !darwin

// Package cli 在 macOS 以外的平台上不支持不回显读入：init 只支持 macOS，在这些平台上读取任何输入之前就已退出。
package cli

import "errors"

// disableEcho 在 macOS 以外的平台上总是报错，ReadSecret 因此不读取输入。
func disableEcho(int) error {
	return errors.New("reading a secret is only supported on macOS")
}
