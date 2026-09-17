//go:build unix

// Package config 在 Unix 平台要求配置文件归当前用户所有，且组和其他用户不可写。
package config

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// checkFileOwner 校验配置文件的属主与写权限：他人可写的配置能被改成指向别处的服务器，
// 因此宽权限一律拒绝打开；错误只描述原因，不包含本机路径。
func checkFileOwner(info fs.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot read config file ownership")
	}
	if int64(stat.Uid) != int64(os.Getuid()) {
		return errors.New("config file must be owned by the current user")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("config file must not be writable by group or others")
	}
	return nil
}
