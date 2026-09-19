// Package cli 使用注入的离线诊断验证命令输出、帮助与用法文案及错误码。
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
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

// wantDescription 是帮助描述的原文；与实施清单一致，不含阶段号。
const wantDescription = "pre-alpha：可以用 init 保存配置、授权码与密钥；尚不能收发邮件、执行任务或运行后台服务。"

// wantHelpText 是文本帮助的全文，与实施清单逐字一致。
const wantHelpText = "turncourier — Email bridge for Codex and Claude Code\n\n" + wantDescription + "\n\n用法：turncourier <命令> [--json]\n\n可用命令：\n" +
	"  help       查看帮助\n  version    查看版本\n  doctor     只读检查平台及 Git、Codex、Claude 的版本\n" +
	"  init       交互式创建配置，并把 QQ 授权码与密钥存入 macOS Keychain\n\n规划命令（尚未实现）：run、tasks、logs、service\n"

// TestHelp 确认所有帮助入口输出同一份帮助全文：含新描述与 init，不含阶段号，并且不运行诊断或 Agent。
func TestHelp(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"-h"}, {"--help"}} {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), args, &stdout, &stderr, Deps{})
		if code != 0 || stderr.Len() != 0 || stdout.String() != wantHelpText || strings.Contains(stdout.String(), "Phase") {
			t.Errorf("help failed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	}
}

// TestHelpJSON 确认机器可读帮助包含实际命令和未实现能力。
func TestHelpJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"help", "--json"}, &stdout, &stderr, Deps{}); code != 0 {
		t.Fatalf("help failed: %s", stderr.String())
	}
	var help Help
	if err := json.Unmarshal(stdout.Bytes(), &help); err != nil {
		t.Fatal(err)
	}
	if help.Name != "turncourier" || help.Description != wantDescription ||
		!slices.Equal(help.Commands, []string{"help [--json]", "version [--json]", "doctor [--json]", "init"}) ||
		!slices.Equal(help.Planned, []string{"run", "tasks", "logs", "service"}) {
		t.Fatalf("unexpected help: %+v", help)
	}
}

// TestVersion 确认版本只来自构建变量并支持文本与 JSON 格式。
func TestVersion(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}, {"version", "--json"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), args, &stdout, &stderr, Deps{}); code != 0 || stderr.Len() != 0 {
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

// usageError 是参数错误的原文，与实施清单逐字一致。
const usageError = "参数错误：help、version、doctor 只接受可选的 --json 参数；init 不接受参数。\n"

// TestUsageFailures 确认未知命令（即使带参数）、未知参数及规划命令不会伪报执行成功，也不会启动外部工具或进入 init；
// 参数错误与规划命令的提示与实施清单逐字一致，不含阶段号。
func TestUsageFailures(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"unknown"}, "未知命令"}, {[]string{"--json"}, "未知命令"},
		{[]string{"doctr", "--verbose"}, "未知命令"}, {[]string{"bogus", "x", "y"}, "未知命令"},
		{[]string{"doctr", "--json"}, "未知命令"},
		{[]string{"doctor", "--unsafe"}, usageError}, {[]string{"doctor", "--json", "extra"}, usageError},
		{[]string{"version", "other"}, usageError}, {[]string{"help", "--json", "--json"}, usageError},
		{[]string{"init", "--json"}, usageError}, {[]string{"init", "extra"}, usageError},
		{[]string{"run", "codex"}, "run 尚未实现；当前仅提供 help、version、doctor、init。\n"},
		{[]string{"run"}, "run 尚未实现；当前仅提供 help、version、doctor、init。\n"},
		{[]string{"tasks"}, "tasks 尚未实现；当前仅提供 help、version、doctor、init。\n"},
		{[]string{"logs"}, "logs 尚未实现；当前仅提供 help、version、doctor、init。\n"},
		{[]string{"service", "install"}, "service 尚未实现；当前仅提供 help、version、doctor、init。\n"},
	}
	for _, test := range cases {
		t.Run(strings.Join(test.args, "/"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			// init 的依赖为零值：若误入 init 流程会因调用 nil 函数而 panic。
			code := Run(context.Background(), test.args, &stdout, &stderr, Deps{})
			exact := strings.HasSuffix(test.want, "\n")
			if code != 2 || stdout.Len() != 0 || strings.Contains(stderr.String(), "Phase") ||
				(exact && stderr.String() != test.want) || (!exact && !strings.Contains(stderr.String(), test.want)) {
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
		code := Run(context.Background(), []string{"doctor", "--json"}, &stdout, &stderr, Deps{Checker: checker})
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
	if code := Run(context.Background(), []string{"doctor"}, &stdout, &stderr, Deps{Checker: checker}); code != 1 {
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
		code := Run(context.Background(), test.args, &failingWriter{writesLeft: test.writes}, &stderr, Deps{Checker: testChecker()})
		if code != 1 || !strings.Contains(stderr.String(), "无法写入") {
			t.Errorf("output failure lost: args=%v writes=%d code=%d stderr=%q", test.args, test.writes, code, stderr.String())
		}
	}
}
