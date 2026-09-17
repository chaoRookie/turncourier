# TurnCourier Phase 2 Implementation Plan

**Goal:** 交付可编译、可测试、可公开的 Go 工程骨架，包含环境诊断和 CI 门槛。

**Architecture:** 使用单 Go 模块和标准库。cmd 只装配入口，internal/cli 解析命令，internal/doctor 提供可注入依赖的只读诊断；工程工具放 tools，产品功能按后续阶段添加。

**Tech Stack:** Go 1.27.1、testing、go/ast、GitHub Actions；生产代码不引入第三方依赖。

用户已批准进入下一阶段，沿用已批准设计，直接执行。writing-plans 要求的执行类子技能在本机不可用，因此由当前任务按本计划实施，独立模块并行开发，集成后统一验收。当前范围不包含完整邮件闭环或正式 alpha 发布。

## 文件与责任

```text
cmd/turncourier/            入口及退出码
internal/cli/              help、version、doctor；其他规划命令明确返回未实现
internal/doctor/           OS、Git、Codex、Claude 的只读检测与 JSON 结果
tools/commentcheck/        AST 检查所有手写 Go 函数、类型及包级中文注释
tools/covercheck/          解析 coverprofile，执行 80% 全部手写 Go 代码门槛
.github/workflows/         CI、安全扫描、手动生成 macOS 候选构建
.github/ISSUE_TEMPLATE/    合成复现优先的 Issue 表单
docs/zh-CN/                实际目录树、开发指南、阶段报告
docs/en/                  英文架构及当前支持范围
```

## 1. 工具链与模块

- [x] 从 go.dev 获取当前稳定 macOS arm64 归档及 SHA-256，校验后解压至 Git 忽略的 `.local/toolchains`，不修改系统 shell 配置。
- [x] 创建 go.mod，内容如下，版本与实际已验证归档一致：

```go
module github.com/chaoRookie/turncourier

go 1.27.1
```

- [x] 用 `go version` 验证版本；用 `git check-ignore .local/toolchains/go/bin/go` 确认工具链不入库。

## 2. CLI 和诊断

- [x] 新建 `cmd/turncourier/main.go`、`internal/cli`、`internal/doctor`。所有具名函数、结构体、接口与包有中文说明。
- [x] 先编写可注入 lookup/version-runner 的测试：缺少工具、命令超时、异常输出、unsupported OS、可用工具、已取消 context。
- [x] 实现 help、version、doctor 的人类可读和 `--json` 输出。doctor 仅调用固定可执行文件的 `--version`，不读取授权码、不查询真实任务、不调用模型、不发邮件。
- [x] CLI 验证未知参数和未知命令；规划中的 init/run/tasks/logs/service 返回非零并说明尚未实现，不伪报执行成功。
- [x] 运行 `go test ./cmd/turncourier ./internal/...`，测试必须以输入输出契约和失败行为为依据。

## 3. 工程检查器

- [x] 新建 `tools/commentcheck` 和对应测试，覆盖缺注释、非中文、包/接口/结构体/具名函数以及标准生成文件豁免；不能把匿名回调逐一注释当作目标。
- [x] 新建 `tools/covercheck` 和对应测试，计算语句加权覆盖率；边界 80% 通过、79.99% 失败；畸形或空 profile 失败。
- [x] 覆盖率计入 cmd、internal、tools 的全部手写 Go 代码，无核心代码排除规则。
- [x] 执行 `go run ./tools/commentcheck .` 及 `go test -race -coverprofile=coverage.out ./...`、`go run ./tools/covercheck -min 80 coverage.out`。

## 4. 文档与开源治理

- [x] 更新双语 README，展示实际文件树、可运行命令和尚未实现范围。
- [x] 加入 Apache-2.0 原文、贡献指南、SECURITY、行为准则、变更日志、Issue/PR 模板。
- [ ] SECURITY 使用 GitHub 私密漏洞报告入口，公开前启用对应设置；不虚构邮箱、响应时限或保证。
- [x] 更新设计状态与英文架构概要；详细目标树保留，不创建空目录。

## 5. CI 与候选构建

- [x] 固定 Actions 的完整提交 SHA，默认只读权限，macOS/Linux 运行格式、vet、race、注释和覆盖率门槛。
- [x] 固定 staticcheck、govulncheck、gitleaks 工具版本；扫描 Git 历史与工作树，不输出凭据。
- [x] 手动工作流生成 darwin arm64/amd64 候选二进制及 SHA-256，上传为 Actions artifact，不提前发布未经真实邮箱验收的 v0.1.0-alpha。
- [x] 本地执行同一套检查并做 `help`、`version`、`doctor --json` 冒烟检查。

## 6. 公开与收尾

- [ ] 本地完成密钥扫描、检查 staged diff、提交。
- [ ] 已确认目标 `chaoRookie/turncourier` 不存在；使用已授权个人账户创建 public 仓库并推送，启用私密漏洞报告及 main 分支检查门槛。
- [ ] 检查 GitHub CI 结果；失败则修复并复验。保存阶段报告，区分本地通过、远端通过、尚未实现功能。

## 验证记录

记录日期：2026-09-17（Asia/Shanghai）。环境：macOS arm64、Go 1.27.1（`.local/toolchains/go`）、Git 2.54.0、Codex CLI 0.154.0、Claude Code 2.1.263。

### 本地通过

- `make check`：gofmt、`go vet`、中文注释检查（13 个手写 Go 文件）、`-race` 测试、覆盖率 97.35%（294/302，门槛 80%，无排除）、staticcheck 2026.2.1（含 ST1020–ST1022）。
- `make security`：gitleaks 扫描全部历史、暂存区与未忽略工作树快照，未发现泄漏；govulncheck 未发现可达漏洞。
- `make workflows`：actionlint 通过。`GOOS=linux`、`GOOS=windows` 的 `go vet ./...` 通过。
- `make build` 后冒烟：`help`、`version`、`doctor`、各自 `--json` 返回 0；`init`、未知命令、多余参数返回 2。
- Actions 固定 SHA 经 GitHub API 核对：checkout v5.1.0、setup-go v6.5.0、upload-artifact v4.6.2。
- Apache-2.0 原文 SHA-256 为 `cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30`，与 apache.org 发布文本一致。

### 审查与修复

多维审查（产品代码、工程检查器、CI 与脚本、测试对照计划）提出 28 条发现，经独立反方复现：25 条确认、1 条存疑、2 条推翻；完整性审查另补 6 项缺口。已修复的主要问题：

- doctor：包装脚本遗留后台进程时误报 error；Unix 上改为独立进程组并在超时或取消时整体终止，避免孤儿进程与 Ctrl-C 竞态；main 同时处理 SIGTERM。
- CLI：未知命令带参数时先报告未知命令；管道断开按 SIGPIPE 惯例终止，在注释中如实说明。
- commentcheck：跳过规则与 go 工具一致，`.go` 符号链接报错，0 个文件视为失败，包说明优先来自非测试文件，局部类型纳入检查。
- covercheck：合并 `-coverpkg` 产生的重复覆盖块。
- 工程脚本：密钥扫描增加暂存区、容忍未暂存删除、拒绝仓库内自定义 gitleaks 配置；候选构建完整克隆历史；工具按版本目录安装；`fmt-check` 检查 gofmt 自身失败；`GO` 覆盖对 staticcheck、govulncheck 生效。
- 测试：补强超时、路径泄漏、版本校验防线、方法注释、非中文字符、非标准生成标记、模式头与溢出等用例，并以变异测试确认新用例能拦截回归。

未修改：生成文件豁免与候选产物权限两条经复现推翻；`fmt-check` 目录范围待 `tests/` 实际出现时再扩展。

### 尚未运行或尚未实现

- 远端 GitHub Actions、私密漏洞报告与分支保护：尚未公开，未运行。
- 完整邮件收发、配置、Keychain、SQLite、Agent 适配器与真实邮箱验收：属于后续阶段，未实现。
