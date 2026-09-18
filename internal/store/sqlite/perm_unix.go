//go:build unix

// Package sqlite 在 Unix 平台要求数据目录与数据库文件只对当前用户开放。
package sqlite

import (
	"errors"
	"io/fs"
)

// checkPrivate 拒绝组或其他用户拥有任何权限的数据目录或数据库文件；what 只用于错误文本，不含本机路径。
// 数据库保存任务与回复元数据，他人可读会泄露会话信息，可写则能篡改队列。
func checkPrivate(info fs.FileInfo, what string) error {
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New(what + " must not be accessible by group or others")
	}
	return nil
}
