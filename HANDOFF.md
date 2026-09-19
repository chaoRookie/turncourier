# HANDOFF

**目标**：将 TurnCourier 建成公开开源的 Codex / Claude Code 邮件接续工具；Phase 3（配置、存储与状态机）已完成，当前为 Phase 4 邮件闭环，清单已确认，下一步实施 4a。
**更新于**：2026-09-19 · claude-code
**项目目录**：本仓库根目录（含 `go.mod` 的目录）
**基线 commit**：`e046ff4`（Phase 3 最后一个 PR #9 合并后的 main，CI 与 Security 通过）；其后为记录 Phase 3 验证结果的文档提交。以 `git log --oneline -3` 与 `git status --short` 核对。
**暂停原因**：Phase 4 实施清单 `docs/zh-CN/plans/phase-04.md` 已写成：D1–D5 与「文件夹列」方案 (a) 已由维护者确认，4a 的 15 个任务契约经两轮审查与复查。下一步在 `feat/phase-04a-mail` 分支从 4a Task 1 开始实施。

## 已完成

- [x] 用户批准设计、Phase 0–1 与 Phase 2；公开目标为个人账户 `chaoRookie` 下的 `turncourier`。Phase 0–1 详见 `docs/zh-CN/research/phase-01.md`，无需重复验证。
- [x] Go 1.27.1 工具链位于被忽略的 `.local/toolchains/go`，SHA-256 已校验；质量工具按版本装在 `.local/bin/<tool>-<version>/`。
- [x] Phase 2：CLI（help/version/doctor 及 `--json`）、doctor、commentcheck、covercheck、Makefile、三个工作流、Dependabot、密钥扫描脚本、治理文档与双语文档；多维审查后修复。详见 `docs/zh-CN/plans/phase-02.md`。
- [x] 公开仓库 https://github.com/chaoRookie/turncourier ；私密漏洞报告已启用；main 设必需检查；Dependabot PR #1–#3 已评审合并。
- [x] Phase 3（PR #6–#9）：
  - 任务与回复队列状态机（`internal/task`、`internal/queue`）；
  - 严格校验的 TOML 配置（`internal/config`，键名区分大小写）；
  - SQLite 存储（`internal/store/sqlite`）：迁移、去重、FIFO、每个任务最多一条在途回复、崩溃后转 UNCERTAIN 且永不自动重发；
  - 集成测试 `tests/integration`；
  - 文档同步，以及整阶段审查的修复。

  命令行尚未使用这些包，产品二进制不链接新依赖。验证记录见 `docs/zh-CN/plans/phase-03.md` 末尾。

## 未完成

- [ ] Phase 4 邮件闭环：按 `docs/zh-CN/plans/phase-04.md` 分三段实施：4a 离线 → L1 真机探测（需维护者在本机录入授权码并授权发信）→ 4b。4a 从 Task 1 开始；Task 13 的 L1 工具只由维护者授权后人工运行。前置条件（均已写入 phase-03.md「风险与后续」，并在 phase-04 中落实）：
  - D2 的正文加密与 Keychain 接入必须先于任何正文存储；
  - 后台服务的单实例锁须覆盖 `RecoverInFlight` 与全部派发操作；
  - 地址小写化须用真实 QQ 邮箱样本复核。
- [ ] macOS 断电持久性（`fullfsync`）留到安全与恢复阶段评估；接入命令行后须补齐链接模块的第三方许可声明。
- [ ] 关注 actions/setup-go 补丁版本：7.0.0 打包的 undici、brace-expansion 有已公开安全公告（旧版同样受影响，本仓库输入不触及），上游已修复未发版；Dependabot 提出后按同样流程评审。
- [ ] Codex、Claude 适配器与后台服务属于后续阶段。真实邮箱各十轮验收之前不打 `v0.1.0-alpha`。

## 下一步

1. 读 `AGENTS.md`，核对 `git status --short`、`git log --oneline -3` 与 `gh run list --repo chaoRookie/turncourier --limit 5`。
2. `export PATH="$PWD/.local/toolchains/go/bin:$PATH"`，运行下方验证命令，确认仍全部通过。
3. 从 main 建 `feat/phase-04a-mail`，按 phase-04.md 从 4a Task 1 起逐任务实施：先写失败的测试，再由子代理实现，经规格与质量审查（含变异测试）、修复和独立复查后提交；阶段末做整阶段审查，然后开 PR。
4. 4a 合并后进入 L1：需维护者在本机操作，授权码只在本机终端输入，每封探测邮件发出前逐封确认。

## 验证方式

```sh
export PATH="$PWD/.local/toolchains/go/bin:$PATH"
make check
make security
make workflows
make build
GOOS=linux go vet ./...
GOOS=windows go vet ./...
CGO_ENABLED=0 go test ./...
go version -m dist/turncourier
./dist/turncourier help
./dist/turncourier version
./dist/turncourier doctor --json
git diff --check
```

期望：
- `make check` 通过，覆盖率 >= 80%；
- gitleaks 与 govulncheck 无发现，actionlint 通过；
- `go version -m` 的输出中没有 `modernc.org/sqlite` 与 `BurntSushi/toml`（在命令行接入前）；
- `doctor --json` 在装有 git/codex/claude 的 macOS 上返回 0。doctor 只检查 OS 与三个工具的 `--version`，不证明登录、邮箱或会话能力。

### 验证记录

- 2026-09-17 本地：`make check` 通过（覆盖率 97.35%，294/302）；`make security`、`make workflows`、`make build` 通过；`GOOS=linux/windows go vet ./...` 通过；冒烟退出码符合契约。
- Actions 固定 SHA 经 GitHub API 核对：公开时为 checkout v5.1.0、setup-go v6.5.0、upload-artifact v4.6.2；2026-09-18 升级为 checkout v7.0.1、setup-go v7.0.0、upload-artifact v7.0.1。
- 2026-09-17 远端：首次推送的 CI（`quality (ubuntu-24.04)`、`quality (macos-15)`）与 Security（`security`）成功；私密漏洞报告 `enabled: true`；GitHub 识别许可证 Apache-2.0。
- 2026-09-18 远端：Actions 升级合并后 main `9cc5bd7` 的 CI、Security 与 Candidate build 成功；下载产物 `shasum -c` 通过，可执行位保留，arm64 二进制版本为 `0.1.0-dev.9cc5bd7`。
- 2026-09-19 Phase 3：本地 `make check`（覆盖率 93.28%，985/1056）、`make security`、`make workflows`、`make build`、两个平台的 vet 与 `CGO_ENABLED=0` 测试通过；PR #9 三项检查通过（质量任务约 2 分钟）；合并后 main `e046ff4` 的 CI 与 Security 成功。

## 关键文件

- `docs/zh-CN/design.md` — 产品范围、开发顺序、安全和持久化边界。
- `docs/zh-CN/plans/phase-03.md` — Phase 3 清单、已确认决策（D1、D2）、审查后修正与验证记录。
- `docs/zh-CN/development.md` — 工具链、make 目标、依赖政策、测试约定。
- `docs/en/architecture.md` — 包依赖方向、持久化与恢复语义、已知局限。
- `internal/cli/cli.go` — 命令路由与退出码契约（0/1/2，管道断开按 SIGPIPE 惯例）。
- `internal/doctor/` — 只读诊断与进程组清理；禁止改成检查真实账号或发模型请求。
- `internal/task/`、`internal/queue/` — 纯状态机，唯一的合法转移表。
- `internal/config/` — 配置路径、严格解码、键名白名单（`knownKeys` 须与 `rawConfig` 标签一致，有测试钉住）。
- `internal/store/sqlite/` — 存储；`migrations/0001_init.sql` 已发布，不得修改，改表结构只能写新迁移。
- `Makefile`、`scripts/scan-secrets.sh`、`.github/workflows/` — 本地与 CI 共用门槛。

## 坑 / 已排除的方向

- 按已批准设计的顺序推进：配置/存储/状态机 → 邮件闭环 → Codex → Claude → 安全与恢复 → 真实邮箱验收 → 发布。不要为了让维护者早点试收发邮件而压缩阶段或做「最小闭环」演示；维护者问时间点不等于要求改优先级。每阶段先写清单、确认决策，再逐任务实施和审查。
- 不再围绕已批准设计做选择题问答；常规实现决定自行完成。对外发布动作（推送、开或合并 PR、改仓库设置）仍需用户在当次对话确认。
- `make secrets` 扫描全部历史（含本地未推送分支）。清单或测试里不要写密钥形状的字面量，测试向量在运行时构造；推送前先跑 `make secrets`，命中只存在于未推送提交时，压缩或改写这些本地提交后再推送。
- 本机 Go 与质量工具下载很慢，已安装的不要重复下载；modernc.org/sqlite 首次下载约 11 分钟。Makefile 以版本目录判断是否需要安装。
- BurntSushi/toml 在精确匹配失败时按不区分大小写把键匹配到字段，并记为已解码，`Undecoded()` 看不到这些变体；配置改用 `MetaData.Keys()` 对照白名单。新增配置字段时必须同步 `knownKeys`。
- `RecoverInFlight` 把全部 DISPATCHING 当作崩溃遗留，只能由唯一的派发进程在开始派发前调用；CLI 等其他进程不得调用。
- 「未送达」重新排队只允许在任务自派发以来没有其他事件时进行，否则必须核对为已送达，这是防重复投递的关键判定，不要放宽。
- gitleaks 仓库为 gitleaks/gitleaks，Go 模块路径仍为 `github.com/zricethezav/gitleaks/v8`。仓库根目录不得添加 `.gitleaks.toml` 或 `.gitleaksignore`，扫描脚本会拒绝运行。
- 审查中推翻的条目不要重复处理：生成文件豁免是计划要求；upload-artifact 的 zip 保留 755，无需额外打 tar 包。
- actionlint 不校验按 SHA 固定的 action 的 `with` 输入；升级 Action 时须对比新旧 `action.yml`，build.yml 只能手动触发 Candidate build 验证。
- main 分支保护要求分支最新（strict），必需检查为 `quality (ubuntu-24.04)`、`quality (macos-15)`、`security`；修改工作流 job 名称时须同步更新分支保护。多个 PR 需逐个「更新分支 → 等三项检查 → 合并」。
- Git 全局未配置提交身份。提交使用单次配置：`git -c user.name=chaoRookie -c user.email=179816769+chaoRookie@users.noreply.github.com commit ...`；不要改全局身份。
- 文档、提交与续点中不要写本机绝对路径、用户名或订阅档位；gitleaks 默认规则不会拦截这类信息。
- 所有 QQ 凭据将来通过本地 init 输入，不发送到聊天、不进入测试或 Git。worktree 不是 OS 沙箱，邮件文字不能绕过 Agent 权限。
