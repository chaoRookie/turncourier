// Package cli 解析 TurnCourier 命令；当前只提供帮助、版本和只读环境诊断。
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/chaoRookie/turncourier/internal/doctor"
)

// Version 由构建参数覆盖；开发版本不代表邮件功能已可用。
var Version = "0.1.0-dev"

// Help 描述当前可用能力与规划命令，供文本和 JSON 输出共用。
type Help struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Commands    []string `json:"commands"`
	Planned     []string `json:"planned"`
}

// Run 执行命令并返回退出码：成功为 0，诊断失败或写入错误为 1，用法错误为 2。
// 传入的是进程标准输出或标准错误且其管道已断开时，写入不会返回错误，进程按 Unix SIGPIPE 惯例直接终止；
// 其他写入错误返回 1。checker 仅用于 doctor；其他命令不会查找或运行外部工具。
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, checker doctor.Checker) int {
	command := "help"
	if len(args) > 0 {
		command, args = args[0], args[1:]
	}
	switch command {
	case "--help", "-h":
		command = "help"
	case "--version":
		command = "version"
	}
	switch command {
	case "help", "version", "doctor":
		// 已实现命令继续校验参数；命令名错误优先于参数错误报告。
	case "init", "run", "tasks", "logs", "service":
		fmt.Fprintf(stderr, "%s 尚未实现；Phase 2 仅提供 help、version、doctor。\n", command)
		return 2
	default:
		fmt.Fprintf(stderr, "未知命令 %q；运行 turncourier help 查看用法。\n", command)
		return 2
	}
	jsonOutput := len(args) == 1 && args[0] == "--json"
	if len(args) > 0 && !jsonOutput {
		fmt.Fprintln(stderr, "参数错误：只接受 help、version、doctor 的可选 --json 参数。")
		return 2
	}
	switch command {
	case "help":
		help := Help{
			Name:        "turncourier",
			Description: "Phase 2 工程骨架：当前没有邮件收发、任务执行或后台服务能力。",
			Commands:    []string{"help [--json]", "version [--json]", "doctor [--json]"},
			Planned:     []string{"init", "run", "tasks", "logs", "service"},
		}
		if jsonOutput {
			return writeJSON(stdout, stderr, help)
		}
		_, err := fmt.Fprintf(stdout, "%s — Email bridge for Codex and Claude Code\n\n%s\n\n用法：turncourier <命令> [--json]\n\n可用命令：\n  help       查看帮助\n  version    查看版本\n  doctor     只读检查平台及 Git、Codex、Claude 的版本\n\n规划命令（尚未实现）：init、run、tasks、logs、service\n", help.Name, help.Description)
		return outputStatus(err, stderr)
	case "version":
		if jsonOutput {
			return writeJSON(stdout, stderr, map[string]string{"name": "turncourier", "version": Version})
		}
		_, err := fmt.Fprintf(stdout, "turncourier %s\n", Version)
		return outputStatus(err, stderr)
	default:
		// 前面的命令检查已排除其他命令，这里只剩 doctor。
		report := checker.Check(ctx)
		if code := writeReport(stdout, stderr, report, jsonOutput); code != 0 {
			return code
		}
		if !report.Ready {
			return 1
		}
		return 0
	}
}

// writeReport 输出诊断状态；Ready 仅表示依赖可用，不声称已登录或邮箱可用。
func writeReport(stdout, stderr io.Writer, report doctor.Report, jsonOutput bool) int {
	if jsonOutput {
		return writeJSON(stdout, stderr, report)
	}
	if _, err := fmt.Fprintf(stdout, "平台：%s（首发支持 macOS：%t）\n", report.Platform, report.Supported); err != nil {
		return outputStatus(err, stderr)
	}
	for _, tool := range report.Tools {
		detail := tool.Version
		if detail == "" {
			detail = tool.Detail
		}
		if _, err := fmt.Fprintf(stdout, "%s: %s — %s\n", tool.Name, tool.Status, detail); err != nil {
			return outputStatus(err, stderr)
		}
	}
	_, err := fmt.Fprintln(stdout, "此诊断仅检查基础依赖，不验证登录、权限、Agent 会话或邮箱连接。")
	return outputStatus(err, stderr)
}

// writeJSON 以单个 JSON 对象输出结果，并统一处理管道写入失败。
func writeJSON(stdout, stderr io.Writer, value any) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return outputStatus(encoder.Encode(value), stderr)
}

// outputStatus 将输出错误映射到非零退出码，不把系统错误中的路径写入诊断。
func outputStatus(err error, stderr io.Writer) int {
	if err != nil {
		fmt.Fprintln(stderr, "无法写入命令输出。")
		return 1
	}
	return 0
}
