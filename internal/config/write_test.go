// Package config 的写入测试验证 Render 与示例配置逐字节一致、渲染结果能被 Load 读回，以及 CreateFile 原子创建且从不覆盖。
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// exampleDraft 是与 configs/turncourier.example.toml 对应的草稿。
func exampleDraft() Draft {
	return Draft{MailboxAddress: "bot@example.invalid", RecipientAddress: "me@example.invalid", AllowedSenders: []string{"me@example.invalid"}}
}

// TestRenderMatchesExample 验证示例草稿的渲染结果与仓库中的示例配置逐字节相同：注释、默认值与格式都一致。
func TestRenderMatchesExample(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("..", "..", "configs", "turncourier.example.toml"))
	if err != nil {
		t.Fatalf("读取示例配置失败: %v", err)
	}
	got, err := Render(exampleDraft())
	if err != nil {
		t.Fatalf("Render 返回错误: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("Render 与示例配置不同:\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRenderLoadRoundTrip 对 20 组合法草稿（含多个白名单地址、含 + 与 ' 的本地部分）渲染后写入文件再 Load，
// 地址与草稿一致，其余字段为默认值。
func TestRenderLoadRoundTrip(t *testing.T) {
	locals := []string{"me", "me+tag", "o'neil", "a.b-c_d", "x!#$%&*/=?^`{|}~y"}
	for i := range 20 {
		draft := Draft{
			MailboxAddress:   fmt.Sprintf("bot%d@bot.example.invalid", i),
			RecipientAddress: fmt.Sprintf("%s%d@example.invalid", locals[i%len(locals)], i),
		}
		for j := range i%3 + 1 {
			draft.AllowedSenders = append(draft.AllowedSenders, fmt.Sprintf("%s.%d@mail%d.example.invalid", locals[(i+j)%len(locals)], i, j))
		}
		data, err := Render(draft)
		if err != nil {
			t.Fatalf("草稿 %d: Render 返回错误: %v", i, err)
		}
		paths := writeConfig(t, string(data), 0o600)
		cfg, err := Load(paths, envOf(nil))
		if err != nil {
			t.Fatalf("草稿 %d: Load 返回错误: %v\n%s", i, err, data)
		}
		wantMailbox := Mailbox{Address: draft.MailboxAddress, IMAPHost: "imap.qq.com", IMAPPort: 993, SMTPHost: "smtp.qq.com", SMTPPort: 465}
		if cfg.Mailbox != wantMailbox || cfg.Recipient.Address != draft.RecipientAddress || !slices.Equal(cfg.Recipient.AllowedSenders, draft.AllowedSenders) {
			t.Errorf("草稿 %d: Load = %+v; want %+v", i, cfg, draft)
		}
		if !slices.Equal(cfg.Notify.Events, defaultNotifyEvents) || cfg.Security.TokenTTL != 168*time.Hour {
			t.Errorf("草稿 %d: 通知事件或令牌有效期不是默认值: %+v", i, cfg)
		}
	}
}

// TestRenderRejects 验证与 Load 相同的地址规则：地址须已规范化（小写、无首尾空白）且合法，白名单非空且不重复，
// 机器人地址不在白名单中；出错时不返回任何文本。
func TestRenderRejects(t *testing.T) {
	cases := []struct {
		name   string
		modify func(*Draft)
		want   string
	}{
		{"机器人地址含大写", func(d *Draft) { d.MailboxAddress = "Bot@example.invalid" }, "mailbox.address"},
		{"接收地址含大写", func(d *Draft) { d.RecipientAddress = "me@Example.invalid" }, "recipient.address"},
		{"白名单地址含大写", func(d *Draft) { d.AllowedSenders = []string{"ME@example.invalid"} }, "recipient.allowed_senders"},
		{"白名单地址有首尾空白", func(d *Draft) { d.AllowedSenders = []string{" me@example.invalid"} }, "recipient.allowed_senders"},
		{"接收地址带显示名", func(d *Draft) { d.RecipientAddress = "Me <me@example.invalid>" }, "recipient.address"},
		{"机器人地址为空", func(d *Draft) { d.MailboxAddress = "" }, "mailbox.address"},
		{"白名单为空", func(d *Draft) { d.AllowedSenders = nil }, "at least one address"},
		{"白名单重复", func(d *Draft) { d.AllowedSenders = []string{"me@example.invalid", "me@example.invalid"} }, "repeats"},
		{"机器人地址在白名单中", func(d *Draft) { d.AllowedSenders = append(d.AllowedSenders, "bot@example.invalid") }, "must not contain the mailbox address"},
	}
	for _, testCase := range cases {
		draft := exampleDraft()
		testCase.modify(&draft)
		data, err := Render(draft)
		if err == nil || data != nil || !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("%s: Render = %q, %v; want 含 %q 的错误", testCase.name, data, err, testCase.want)
		}
	}
}

// TestCreateFile 验证新建文件权限 0600、缺失的父目录以 0700 创建、内容一致且目录中没有残留的临时文件；
// 目标已存在时返回 fs.ErrExist，原文件内容与修改时间不变，同样不留临时文件。
// 系统临时目录指向不存在的路径：临时文件必须建在目标所在目录，改建到系统临时目录时创建失败
// （否则目标在另一个卷上时 os.Link 会以 EXDEV 失败，而同一卷上的测试察觉不到）。
func TestCreateFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(root, "missing"))
	dir := filepath.Join(root, "a", "b")
	path := filepath.Join(dir, "turncourier.toml")
	data := []byte("first\n")
	if err := CreateFile(path, data); err != nil {
		t.Fatalf("CreateFile 返回错误: %v", err)
	}
	requireOnlyFile(t, dir, "turncourier.toml")
	if got, err := os.ReadFile(path); err != nil || string(got) != string(data) {
		t.Fatalf("文件内容 = %q, %v; want %q", got, err, data)
	}
	if runtime.GOOS != "windows" {
		requireMode(t, path, 0o600)
		requireMode(t, dir, 0o700)
		requireMode(t, filepath.Dir(dir), 0o700)
	}

	// 把修改时间调到过去，任何改写都会使它变化。
	old := time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	err := CreateFile(path, []byte("second\n"))
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("目标已存在时 CreateFile = %v; want fs.ErrExist", err)
	}
	requireNoPath(t, err, root)
	if got, err := os.ReadFile(path); err != nil || string(got) != string(data) {
		t.Errorf("已有文件被改写: %q, %v", got, err)
	}
	if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(old) {
		t.Errorf("已有文件的修改时间被改变: %v, %v", info.ModTime(), err)
	}
	requireOnlyFile(t, dir, "turncourier.toml")
}

// TestCreateFileParentIsFile 验证父路径是普通文件时报错，且错误文本不含临时目录路径。
func TestCreateFileParentIsFile(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "file")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := CreateFile(filepath.Join(parent, "turncourier.toml"), []byte("x"))
	if err == nil {
		t.Fatal("父路径是普通文件时 CreateFile 应当报错")
	}
	requireNoPath(t, err, root)
}

// requireOnlyFile 断言目录中只有 name 一个条目，即没有残留的临时文件。
func requireOnlyFile(t *testing.T, dir, name string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, []string{name}) {
		t.Errorf("目录中的条目 = %v; want [%s]", names, name)
	}
}

// requireMode 断言 path 的权限位恰为 want。
func requireMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s 的权限 = %o; want %o", filepath.Base(path), got, want)
	}
}

// requireNoPath 断言错误文本不含临时目录路径。
func requireNoPath(t *testing.T, err error, root string) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), root) {
		t.Errorf("错误文本包含本机路径: %v", err)
	}
}
