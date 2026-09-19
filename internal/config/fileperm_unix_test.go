//go:build unix

// Package config 的 Unix 测试覆盖配置文件属主与写权限检查、FIFO 与不可访问的父目录。
package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeFileInfo 是只实现 fs.FileInfo 的替身，Mode 与 Sys 可控，用于构造真实文件难以得到的属主。
type fakeFileInfo struct {
	mode fs.FileMode
	sys  any
}

// Name 返回固定文件名。
func (f fakeFileInfo) Name() string { return "turncourier.toml" }

// Size 返回 0，属主检查不关心大小。
func (f fakeFileInfo) Size() int64 { return 0 }

// Mode 返回替身的权限位。
func (f fakeFileInfo) Mode() fs.FileMode { return f.mode }

// ModTime 返回零值时间。
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }

// IsDir 恒为 false。
func (f fakeFileInfo) IsDir() bool { return false }

// Sys 返回替身的底层数据。
func (f fakeFileInfo) Sys() any { return f.sys }

// TestCheckFileOwner 验证属主不是当前用户、Sys 不是 *syscall.Stat_t、组或其他用户可写时都被拒绝，
// 只有当前用户所有且组和其他用户不可写时通过；断言拒绝原因，钉住具体是哪项检查生效。
func TestCheckFileOwner(t *testing.T) {
	uid := uint32(os.Getuid())
	cases := []struct {
		name string
		info fakeFileInfo
		want string
	}{
		{"属主是其他用户", fakeFileInfo{0o600, &syscall.Stat_t{Uid: uid + 1}}, "owned by the current user"},
		{"Sys 不是 Stat_t", fakeFileInfo{0o600, nil}, "cannot read config file ownership"},
		{"组可写", fakeFileInfo{0o620, &syscall.Stat_t{Uid: uid}}, "must not be writable"},
		{"其他用户可写", fakeFileInfo{0o602, &syscall.Stat_t{Uid: uid}}, "must not be writable"},
		{"当前用户所有且 0600", fakeFileInfo{0o600, &syscall.Stat_t{Uid: uid}}, ""},
	}
	for _, testCase := range cases {
		err := checkFileOwner(testCase.info)
		if testCase.want == "" {
			if err != nil {
				t.Errorf("%s: checkFileOwner = %v; want nil", testCase.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("%s: checkFileOwner = %v; want 包含 %q 的错误", testCase.name, err, testCase.want)
		}
	}
}

// TestCheckOpenedFileUsesDescriptor 验证检查针对已打开的文件而不是路径：打开组和其他用户可写的文件后，
// 路径被替换成合规文件，检查仍按已打开的文件拒绝，检查与读取不会落在两个不同的文件上。
func TestCheckOpenedFileUsesDescriptor(t *testing.T) {
	paths := writeConfig(t, minimalConfig, 0o622)
	file, err := os.Open(paths.ConfigFile)
	if err != nil {
		t.Fatalf("打开配置文件失败: %v", err)
	}
	defer file.Close()
	safe := writeConfig(t, minimalConfig, 0o600)
	if err := os.Rename(safe.ConfigFile, paths.ConfigFile); err != nil {
		t.Fatalf("替换配置文件失败: %v", err)
	}
	if err := checkOpenedFile(file); err == nil || !strings.Contains(err.Error(), "must not be writable") {
		t.Errorf("checkOpenedFile = %v; want 按已打开的文件报告可写", err)
	}
}

// TestLoadRejectsFIFO 验证配置路径是 FIFO 时 Load 不会阻塞在打开上，并按非常规文件拒绝。
func TestLoadRejectsFIFO(t *testing.T) {
	dir := t.TempDir()
	paths := Paths{ConfigFile: filepath.Join(dir, "turncourier.toml"), DataDir: dir}
	if err := syscall.Mkfifo(paths.ConfigFile, 0o600); err != nil {
		t.Fatalf("创建 FIFO 失败: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Load(paths, envOf(nil))
		done <- err
	}()
	select {
	case err := <-done:
		requireErrorWithoutPath(t, err, paths)
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("错误 = %v; want 常规文件检查失败", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Load 打开 FIFO 时阻塞")
	}
}

// TestLoadUnreadableParent 验证父目录不可访问时报错、错误不含路径，且不会被当作文件不存在。
func TestLoadUnreadableParent(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root 不受目录权限位限制")
	}
	paths := writeConfig(t, minimalConfig, 0o600)
	if err := os.Chmod(paths.DataDir, 0); err != nil {
		t.Fatalf("设置目录权限失败: %v", err)
	}
	// 在 t.TempDir 的清理之前恢复权限，否则临时目录无法删除。
	t.Cleanup(func() { os.Chmod(paths.DataDir, 0o700) })
	_, err := Load(paths, envOf(nil))
	requireErrorWithoutPath(t, err, paths)
	if errors.Is(err, ErrNotFound) {
		t.Errorf("错误 = %v; 父目录不可访问不应视为文件不存在", err)
	}
}
