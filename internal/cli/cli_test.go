// Package cli 使用注入的离线诊断验证命令输出及错误码。
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chaoRookie/turncourier/internal/doctor"
)

// testChecker 提供固定版本信息，不查询实际机器或用户配置。
func testChecker() doctor.Checker {
	versions := map[string]string{"git": "git version 2.50.0", "codex": "codex-cli 0.154.0", "claude": "2.1.263 (Claude Code)"}
	return doctor.Checker{
		GOOS:    "darwin",
		Lookup:  func(name string) (string, error) { return name, nil },
		Run:     func(_ context.Context, name string) ([]byte, error) { return []byte(versions[name]), nil },
		Timeout: time.Second,
	}
}

// TestHelp 确认所有帮助入口说明当前阶段，并且不运行诊断或 Agent。
func TestHelp(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"-h"}, {"--help"}} {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), args, &stdout, &stderr, doctor.Checker{})
		if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "没有邮件收发") || !strings.Contains(stdout.String(), "尚未实现") {
			t.Errorf("help failed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	}
}

// TestHelpJSON 确认机器可读帮助包含实际命令和未实现能力。
func TestHelpJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"help", "--json"}, &stdout, &stderr, doctor.Checker{}); code != 0 {
		t.Fatalf("help failed: %s", stderr.String())
	}
	var help Help
	if err := json.Unmarshal(stdout.Bytes(), &help); err != nil {
		t.Fatal(err)
	}
	if help.Name != "turncourier" || len(help.Commands) != 3 || len(help.Planned) != 5 || !strings.Contains(help.Description, "pre-alpha") {
		t.Fatalf("unexpected help: %+v", help)
	}
}

// TestVersion 确认版本只来自构建变量并支持文本与 JSON 格式。
func TestVersion(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}, {"version", "--json"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), args, &stdout, &stderr, doctor.Checker{}); code != 0 || stderr.Len() != 0 {
			t.Fatalf("version failed: %s", stderr.String())
		}
		if len(args) == 2 {
			var version map[string]string
			if err := json.Unmarshal(stdout.Bytes(), &version); err != nil || version["version"] != Version || version["name"] != "turncourier" {
				t.Fatalf("invalid version JSON: %q (%v)", stdout.String(), err)
			}
		} else if stdout.String() != "turncourier "+Version+"\n" {
			t.Fatalf("invalid version text: %q", stdout.String())
		}
	}
}

// TestUsageFailures 确认未知命令（即使带参数）、未知参数及规划命令不会伪报执行成功，也不会启动外部工具；提示中不含阶段号。
func TestUsageFailures(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"unknown"}, "未知命令"}, {[]string{"--json"}, "未知命令"},
		{[]string{"doctr", "--verbose"}, "未知命令"}, {[]string{"bogus", "x", "y"}, "未知命令"},
		{[]string{"doctr", "--json"}, "未知命令"},
		{[]string{"doctor", "--unsafe"}, "参数错误"}, {[]string{"doctor", "--json", "extra"}, "参数错误"},
		{[]string{"version", "other"}, "参数错误"}, {[]string{"help", "--json", "--json"}, "参数错误"},
		{[]string{"init"}, "尚未实现"}, {[]string{"run", "codex"}, "尚未实现"},
		{[]string{"tasks"}, "尚未实现"}, {[]string{"logs"}, "尚未实现"},
		{[]string{"service", "install"}, "尚未实现"},
	}
	for _, test := range cases {
		t.Run(strings.Join(test.args, "/"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), test.args, &stdout, &stderr, doctor.Checker{}); code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), test.want) || strings.Contains(stderr.String(), "Phase") {
				t.Errorf("unexpected usage result: code=%d out=%q err=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

// TestDoctorJSON 确认诊断输出为可解析 JSON，且基础依赖缺失导致非零退出码。
func TestDoctorJSON(t *testing.T) {
	for _, missing := range []bool{false, true} {
		checker := testChecker()
		if missing {
			checker.Lookup = func(string) (string, error) { return "", errors.New("missing") }
		}
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), []string{"doctor", "--json"}, &stdout, &stderr, checker)
		var report doctor.Report
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Ready == missing || (code == 0) == missing || stderr.Len() != 0 || len(report.Tools) != 3 {
			t.Fatalf("unexpected doctor result: code=%d report=%+v", code, report)
		}
	}
}

// TestDoctorText 确认文本报告含已知版本、缺失原因及诊断范围说明。
func TestDoctorText(t *testing.T) {
	checker := testChecker()
	checker.Lookup = func(name string) (string, error) {
		if name == "claude" {
			return "", errors.New("missing")
		}
		return name, nil
	}
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"doctor"}, &stdout, &stderr, checker); code != 1 {
		t.Fatalf("expected missing tool exit: %d", code)
	}
	for _, want := range []string{"darwin", "git version 2.50.0", "codex-cli", "未在 PATH", "不验证登录"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("missing %q from %q", want, stdout.String())
		}
	}
}

// failingWriter 在指定次数写入后返回写入错误，模拟磁盘已满等会返回错误的输出故障。
// 真实进程的 stdout 或 stderr 管道断开时，进程按 Unix SIGPIPE 惯例终止，不经过这里的错误路径。
type failingWriter struct {
	writesLeft int
}

// Write 在预定位置返回错误，验证 CLI 不会将丢失的输出报告为成功。
func (w *failingWriter) Write(data []byte) (int, error) {
	if w.writesLeft == 0 {
		return 0, errors.New("closed pipe")
	}
	w.writesLeft--
	return len(data), nil
}

// TestOutputFailure 覆盖帮助、版本、JSON 与诊断每个输出阶段的写入错误。
func TestOutputFailure(t *testing.T) {
	cases := []struct {
		args   []string
		writes int
	}{
		{[]string{"help"}, 0}, {[]string{"version"}, 0}, {[]string{"help", "--json"}, 0},
		{[]string{"doctor", "--json"}, 0}, {[]string{"doctor"}, 0},
		{[]string{"doctor"}, 1}, {[]string{"doctor"}, 4},
	}
	for _, test := range cases {
		var stderr bytes.Buffer
		code := Run(context.Background(), test.args, &failingWriter{writesLeft: test.writes}, &stderr, testChecker())
		if code != 1 || !strings.Contains(stderr.String(), "无法写入") {
			t.Errorf("output failure lost: args=%v writes=%d code=%d stderr=%q", test.args, test.writes, code, stderr.String())
		}
	}
}
