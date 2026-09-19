// Package config 的路径测试覆盖默认目录、环境变量覆盖与绝对路径要求。
package config

import (
	"errors"
	"path/filepath"
	"testing"
)

// envOf 返回按给定键值对查表的 getenv 替身，未列出的键返回空串。
func envOf(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

// userConfigDirOf 返回固定结果的 userConfigDir 替身，用于断言默认目录与错误分支。
func userConfigDirOf(dir string, err error) func() (string, error) {
	return func() (string, error) { return dir, err }
}

// TestResolvePathsDefaults 验证未设置环境变量时按用户配置目录推导三个路径。
func TestResolvePathsDefaults(t *testing.T) {
	got, err := ResolvePaths(envOf(nil), userConfigDirOf("/home/u/.config", nil))
	if err != nil {
		t.Fatalf("ResolvePaths 返回错误: %v", err)
	}
	base := filepath.Join("/home/u/.config", "TurnCourier")
	want := Paths{
		ConfigFile: filepath.Join(base, "turncourier.toml"),
		DataDir:    base,
		Database:   filepath.Join(base, "turncourier.db"),
	}
	if got != want {
		t.Errorf("ResolvePaths = %+v; want %+v", got, want)
	}
}

// TestResolvePathsConfigEnv 验证 TURNCOURIER_CONFIG 覆盖配置文件，数据目录随之落在同一目录。
func TestResolvePathsConfigEnv(t *testing.T) {
	got, err := ResolvePaths(
		envOf(map[string]string{"TURNCOURIER_CONFIG": "/abs/c.toml"}),
		userConfigDirOf("", errors.New("不应调用用户配置目录")),
	)
	if err != nil {
		t.Fatalf("ResolvePaths 返回错误: %v", err)
	}
	want := Paths{
		ConfigFile: "/abs/c.toml",
		DataDir:    "/abs",
		Database:   filepath.Join("/abs", "turncourier.db"),
	}
	if got != want {
		t.Errorf("ResolvePaths = %+v; want %+v", got, want)
	}
}

// TestResolvePathsDataDirEnv 验证 TURNCOURIER_DATA_DIR 只覆盖数据目录与数据库路径。
func TestResolvePathsDataDirEnv(t *testing.T) {
	got, err := ResolvePaths(
		envOf(map[string]string{"TURNCOURIER_DATA_DIR": "/abs/data"}),
		userConfigDirOf("/home/u/.config", nil),
	)
	if err != nil {
		t.Fatalf("ResolvePaths 返回错误: %v", err)
	}
	want := Paths{
		ConfigFile: filepath.Join("/home/u/.config", "TurnCourier", "turncourier.toml"),
		DataDir:    "/abs/data",
		Database:   filepath.Join("/abs/data", "turncourier.db"),
	}
	if got != want {
		t.Errorf("ResolvePaths = %+v; want %+v", got, want)
	}
}

// TestResolvePathsErrors 验证相对路径环境变量、不可用或为相对路径的用户配置目录都会报错。
func TestResolvePathsErrors(t *testing.T) {
	cases := []struct {
		name          string
		env           map[string]string
		userConfigDir func() (string, error)
	}{
		{"配置文件为相对路径", map[string]string{"TURNCOURIER_CONFIG": "c.toml"}, userConfigDirOf("/home/u/.config", nil)},
		{"数据目录为相对路径", map[string]string{"TURNCOURIER_DATA_DIR": "data"}, userConfigDirOf("/home/u/.config", nil)},
		{"用户配置目录不可用", nil, userConfigDirOf("", errors.New("no home"))},
		{"用户配置目录为相对路径", nil, userConfigDirOf("relative/.config", nil)},
	}
	for _, testCase := range cases {
		got, err := ResolvePaths(envOf(testCase.env), testCase.userConfigDir)
		if err == nil {
			t.Errorf("%s: ResolvePaths = %+v; want error", testCase.name, got)
		}
		if got != (Paths{}) {
			t.Errorf("%s: ResolvePaths = %+v; want zero value on error", testCase.name, got)
		}
	}
}
