//go:build !unix

// Package sqlite 在非 Unix 平台不检查数据目录与数据库文件的权限位。
package sqlite

import "io/fs"

// checkPrivate 在非 Unix 平台直接通过：这些系统的权限模型与 Unix 权限位不对应，
// 按位判断会给出错误结论；相应的保护留待支持这些平台时设计。
func checkPrivate(fs.FileInfo, string) error { return nil }
