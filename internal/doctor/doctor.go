// Package doctor 提供只读的系统与命令行工具诊断，不读取配置、凭据或会话。
package doctor

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxVersionBytes = 4096

// Tool 表示单个工具的检测结果；不公开可执行文件路径和原始错误。
type Tool struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// Report 保存当前平台及三个固定工具的诊断，Ready 仅代表基础依赖可用。
type Report struct {
	Platform  string `json:"platform"`
	Supported bool   `json:"supported"`
	Ready     bool   `json:"ready"`
	Tools     []Tool `json:"tools"`
}

// Checker 将平台、路径查找和版本执行与检查逻辑分离，便于离线测试。
type Checker struct {
	GOOS    string
	Lookup  func(string) (string, error)
	Run     func(context.Context, string) ([]byte, error)
	Timeout time.Duration
}

// New 创建生产诊断器，只执行固定的 --version。
// 单个工具的真实等待上限为 Timeout（三秒）加上最多一秒的 WaitDelay：超时后终止版本子进程
// （Unix 上为整个进程组），或直接子进程已退出但后台进程占住输出时，最多再等待一秒回收输出管道。
func New() Checker {
	return Checker{GOOS: runtime.GOOS, Lookup: exec.LookPath, Run: runVersion, Timeout: 3 * time.Second}
}

// Check 按固定顺序检测工具；取消后不再查找或启动其他进程。
// 首版仅支持 macOS，但其他平台仍可获得只读诊断结果。
func (c Checker) Check(ctx context.Context) Report {
	report := Report{Platform: c.GOOS, Supported: c.GOOS == "darwin", Tools: make([]Tool, 0, 3)}
	report.Ready = report.Supported
	for _, name := range []string{"git", "codex", "claude"} {
		tool := c.checkTool(ctx, name)
		report.Tools = append(report.Tools, tool)
		report.Ready = report.Ready && tool.Status == "ok"
	}
	return report
}

// checkTool 仅保留简短可打印版本行，避免把工具的任意诊断输出写入报告。
func (c Checker) checkTool(ctx context.Context, name string) Tool {
	tool := Tool{Name: name}
	if ctx.Err() != nil {
		tool.Status, tool.Detail = "cancelled", "检查已取消"
		return tool
	}
	path, err := c.Lookup(name)
	if err != nil {
		tool.Status, tool.Detail = "missing", "未在 PATH 中找到工具"
		return tool
	}
	runCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	output, err := c.Run(runCtx, path)
	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		tool.Status, tool.Detail = "timeout", "版本检查超时"
	case runCtx.Err() != nil:
		tool.Status, tool.Detail = "cancelled", "检查已取消"
	case err != nil:
		tool.Status, tool.Detail = "error", "版本命令执行失败"
	default:
		version, valid := versionLine(name, output)
		if valid {
			tool.Status, tool.Version = "ok", version
		} else {
			tool.Status, tool.Detail = "invalid", "版本输出不符合预期"
		}
	}
	return tool
}

// versionLine 验证已知版本格式，拒绝控制字符、超长输出和非版本内容。
func versionLine(name string, output []byte) (string, bool) {
	if len(output) > maxVersionBytes || !utf8.Valid(output) {
		return "", false
	}
	line := strings.TrimSpace(string(output))
	if len(line) == 0 || len(line) > 256 || strings.ContainsAny(line, "\r\n") {
		return "", false
	}
	for _, char := range line {
		if unicode.IsControl(char) {
			return "", false
		}
	}
	var number string
	switch name {
	case "git":
		number = strings.TrimPrefix(line, "git version ")
	case "codex":
		number = strings.TrimPrefix(line, "codex-cli ")
	case "claude":
		if !strings.HasSuffix(line, " (Claude Code)") {
			return "", false
		}
		number = strings.TrimSuffix(line, " (Claude Code)")
	}
	return line, len(number) > 0 && number != line && number[0] >= '0' && number[0] <= '9'
}

// limitedOutput 为版本输出设置硬上限，防止异常可执行文件无限占用内存。
// overflow 记录是否曾经超限，因为 WaitDelay 到期时返回的 ErrWaitDelay 可能吞掉拷贝协程的超限错误。
type limitedOutput struct {
	buffer   bytes.Buffer
	overflow bool
}

// Write 拒绝超过上限的输出，错误会使版本检查失败而不是截断后误判。
func (b *limitedOutput) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > maxVersionBytes {
		b.overflow = true
		return 0, errors.New("version output exceeds limit")
	}
	return b.buffer.Write(p)
}

// runVersion 使用直接进程调用执行固定参数，不经 shell，不读取项目配置。
// Stderr 保持 nil，由 exec 直接连接空设备而不建管道，后台进程占住 stderr 不会拖延检查。
// Run 返回前已等待拷贝协程结束，因此读取 buffer 和 overflow 不与写入并发。
func runVersion(ctx context.Context, path string) ([]byte, error) {
	command := exec.CommandContext(ctx, path, "--version")
	output := &limitedOutput{}
	command.Stdout = output
	command.WaitDelay = time.Second
	configureProcess(command)
	err := command.Run()
	// 直接子进程已成功退出、仅后台进程占住 stdout 时，Wait 在 WaitDelay 后返回 ErrWaitDelay；
	// 已读到的输出仍交给格式校验。超限时保留错误，避免被吞掉的超限错误变成成功。
	if errors.Is(err, exec.ErrWaitDelay) && !output.overflow {
		err = nil
	}
	return output.buffer.Bytes(), err
}
