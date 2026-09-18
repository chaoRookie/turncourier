//go:build !unix

// Package config 在非 Unix 平台不检查配置文件属主与权限位，只保留常规文件与大小检查。
package config

import "io/fs"

// checkFileOwner 在非 Unix 平台直接通过：这些系统的权限模型与 Unix 权限位不对应，
// 按位判断会给出错误结论；相应的保护留待支持这些平台时设计。
func checkFileOwner(fs.FileInfo) error { return nil }
