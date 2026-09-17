# HANDOFF

**目标**：将 TurnCourier 建成公开开源的 Codex / Claude Code 邮件接续工具；当前阶段完成 Phase 2 工程骨架并公开到 GitHub。
**更新于**：2026-09-17 · claude-code
**项目目录**：本仓库根目录（含 `go.mod` 的目录）
**基线 commit**：`44ad326`（Phase 0–1）。Phase 2 本地工作已全部完成，尚未整理提交与公开；以 `git log --oneline -3` 与 `git status --short` 核对。
**暂停原因**：本地质量门槛、审查修复与文档均已完成；创建公开仓库并推送属于对外发布，需要用户在对话中确认后再执行。

## 已完成

- [x] 用户批准设计、Phase 0–1 与 Phase 2；公开目标为个人账户 `chaoRookie` 下的 `turncourier`。Phase 0–1 详见 `docs/zh-CN/research/phase-01.md`，无需重复验证。
- [x] Go 1.27.1 工具链位于被忽略的 `.local/toolchains/go`，SHA-256 已校验；质量工具按版本装在 `.local/bin/<tool>-<version>/`。
- [x] CLI（help/version/doctor 及 `--json`）、doctor（Unix 进程组清理、SIGINT/SIGTERM 取消）、commentcheck、covercheck、Makefile、三个工作流、Dependabot、密钥扫描脚本、`staticcheck.conf`。
- [x] 多维审查 28 条发现经反方复现后修复（2 条推翻、1 条经确认改为 staticcheck 检查），每个区域由独立验证员复现并做变异测试；详见 `docs/zh-CN/plans/phase-02.md` 的「验证记录」。
- [x] 治理与文档：LICENSE（Apache-2.0 官方原文，SHA-256 `cfc7749b…3d30`）、CODE_OF_CONDUCT（Contributor Covenant 2.1）、CONTRIBUTING、SECURITY、CHANGELOG、Issue/PR 模板、双语 README（实际文件树）、`docs/en/architecture.md`、`docs/zh-CN/development.md`；设计状态已更新。文档经逐组事实核查与跨文档一致性审查。

## 未完成

- [ ] 整理本地提交：Phase 2 改动尚未提交；未推送的 WIP 提交需压入 Phase 2 提交，避免公开历史含本机路径。提交前运行 `make secrets` 并检查 staged diff。
- [ ] 用户确认后公开：`gh repo view chaoRookie/turncourier` 确认不存在，再 `gh repo create chaoRookie/turncourier --public --source . --remote origin --push`。
- [ ] 启用私密漏洞报告；等待并修复远端 CI；CI 绿灯后设置 main 分支必需检查。
- [ ] 公开后勾选 `docs/zh-CN/plans/phase-02.md` 第 4 节 SECURITY 项与第 6 节，并补充远端验证记录。
- [ ] 完整邮件收发、TOML/Keychain/SQLite、Agent 适配器属于后续阶段。真实邮箱各十轮验收之前不打 `v0.1.0-alpha`。

## 下一步

1. 读 `AGENTS.md`，核对 `git status --short` 与 `git log --oneline -3`。
2. `export PATH="$PWD/.local/toolchains/go/bin:$PATH"`，运行下方验证命令，确认仍全部通过。
3. 按用户确认的方式整理提交并公开；随后执行「未完成」中的远端步骤。
4. 远端 CI 失败时在本地复现修复，不跳过门槛；更新计划的验证记录与本文件。

## 验证方式

```sh
export PATH="$PWD/.local/toolchains/go/bin:$PATH"
make check
make security
make workflows
make build
./dist/turncourier help
./dist/turncourier version
./dist/turncourier doctor --json
git diff --check
```

期望：`make check` 通过且覆盖率 >= 80%；gitleaks 与 govulncheck 无发现；actionlint 通过；`doctor --json` 在装有 git/codex/claude 的 macOS 上返回 0。doctor 只检查 OS 与三个工具的 `--version`，不证明登录、邮箱或会话能力。

### 验证记录

- 2026-09-17 本地：`make check` 通过（覆盖率 97.35%，294/302）；`make security`、`make workflows`、`make build` 通过；`GOOS=linux/windows go vet ./...` 通过；冒烟退出码符合契约。
- Actions 固定 SHA 经 GitHub API 核对为 checkout v5.1.0、setup-go v6.5.0、upload-artifact v4.6.2。
- 远端 CI、私密漏洞报告、分支保护：尚未运行或设置。

## 关键文件

- `docs/zh-CN/design.md` — 产品范围、目录职责、安全和持久化边界。
- `docs/zh-CN/plans/phase-02.md` — Phase 2 清单与验证记录。
- `docs/zh-CN/development.md` — 工具链、make 目标、注释与覆盖率规则、密钥扫描。
- `internal/cli/cli.go` — 命令路由与退出码契约（0/1/2，管道断开按 SIGPIPE 惯例）。
- `internal/doctor/doctor.go`、`process_unix.go` — 只读诊断与进程组清理；禁止改成检查真实账号或发模型请求。
- `tools/commentcheck/main.go`、`tools/covercheck/main.go` — CI 工程约束。
- `Makefile`、`scripts/scan-secrets.sh`、`.github/workflows/` — 本地与 CI 共用门槛。

## 坑 / 已排除的方向

- 不再围绕已批准设计做选择题问答；常规实现决定自行完成。对外发布动作（建公开仓库、推送、改仓库设置）仍需用户在当次对话确认。
- 本机 Go 与质量工具下载很慢，已安装的不要重复下载；Makefile 以版本目录判断是否需要安装。
- gitleaks 仓库为 gitleaks/gitleaks，Go 模块路径仍为 `github.com/zricethezav/gitleaks/v8`。
- 仓库根目录不得添加 `.gitleaks.toml` 或 `.gitleaksignore`，扫描脚本会拒绝运行。
- 审查中推翻的两条不要重复处理：生成文件豁免是计划要求；upload-artifact v4.6.2 保留可执行权限。`fmt-check` 目录范围待 `tests/` 实际出现时再扩展。
- Git 全局未配置提交身份。提交使用单次配置：`git -c user.name=chaoRookie -c user.email=179816769+chaoRookie@users.noreply.github.com commit ...`；不要改全局身份。
- 文档、提交与续点中不要写本机绝对路径、用户名或订阅档位；gitleaks 默认规则不会拦截这类信息。
- 分支保护 required checks 预计为 `quality (ubuntu-24.04)`、`quality (macos-15)`、`security`，以真实 Actions 返回名称为准。
- 所有 QQ 凭据将来通过本地 init 输入，不发送到聊天、不进入测试或 Git。worktree 不是 OS 沙箱，邮件文字不能绕过 Agent 权限。
