# HANDOFF

**目标**：将 TurnCourier 建成公开开源的 Codex / Claude Code 邮件接续工具；Phase 3（配置、存储与状态机）与 Phase 4a（邮件闭环的离线实现）已完成，当前处于 L1 真机探测中段。
**更新于**：2026-09-21 · claude-code
**项目目录**：本仓库根目录（含 `go.mod` 的目录）
**基线 commit**：`fc57de5`（L1 结果与 D4 定稿的 PR #13 合并后的 main，CI 与 Security 通过）。以 `git log --oneline -3` 与 `git status --short` 核对。
**暂停原因**：L1 规程第 1–5 步已全部完成，结果写在 `docs/zh-CN/plans/phase-04.md` 的「L1 结果」一节，D4 已据此定稿（令牌载体由正文页脚改为主题标签，维护者 2026-09-20 确认）。进入 4b 之前还差两类真机样本：假期自动回复与退信（承重规则的输入，硬前置），以及别名地址与大小写变体。

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
- [x] Phase 4a（PR #12，合并为 `4f646c6`）：Keychain 封装、`turncourier init`、回复令牌、正文加密、迁移 `0002`、待发通知状态机与存储、SMTP 与 IMAP 客户端及离线假服务器、只由人工执行的 `tests/live` 探测工具，以及文档同步。经 15 个任务逐任务实施、整阶段审查（20 条：1 主要、10 次要、9 细节）与四轮复查。本地 `make check` 覆盖率约 93.5%。
- [x] L1 第 1–5 步（2026-09-20 至 21，维护者本机）：能力与文件夹、发信与实际投递 ID、两份 QQ 客户端回复样本、30 分钟 IDLE 测量、Keychain 往返与 Agent 沙箱读取（实测证实 Claude Code 与 Codex 的 shell 都能静默读出机密，D3 的威胁模型由推断变为实测）。结论见 `docs/zh-CN/plans/phase-04.md`「L1 结果」；D4 定稿（令牌载体改主题标签、只接受最新通知的令牌、`TokenTTL` 默认 72h）并同步修改了 `design.md`、`docs/en/architecture.md` 与 `internal/config`。

## 未完成

- [ ] L1 余下部分（维护者本机执行，命令见下文「下一步」）：
  - 假期自动回复与退信样本——令牌改到主题后这是 4b 的前置条件，不是可选项；
  - 别名地址与大小写变体的回信样本——Phase 3 的小写化假设仍未复核；
  - Foxmail / Apple Mail / Gmail 的回复样本，本轮只采到 QQ 邮箱自家两个客户端。
- [ ] 4b：按 `docs/zh-CN/plans/phase-04.md`「4b 与 4a 的衔接」细化任务契约后实施。前置条件仍然成立：
  - 后台服务的单实例锁须覆盖 `RecoverInFlight`、`RecoverSendingNotifications` 与全部派发和发送；
  - 新主题格式（含令牌）须按同一规程做一次往返复核，再冻结主题解析器（D4「尚未验证」）。
- [ ] macOS 断电持久性（`fullfsync`）留到安全与恢复阶段评估；接入命令行后须补齐链接模块的第三方许可声明。
- [ ] 关注 actions/setup-go 补丁版本：7.0.0 打包的 undici、brace-expansion 有已公开安全公告（旧版同样受影响，本仓库输入不触及），上游已修复未发版；Dependabot 提出后按同样流程评审。
- [ ] Codex、Claude 适配器与后台服务属于后续阶段。真实邮箱各十轮验收之前不打 `v0.1.0-alpha`。

## 下一步

1. 读 `AGENTS.md`，核对 `git status --short`、`git log --oneline -3` 与 `gh run list --repo chaoRookie/turncourier --limit 5`。
2. `export PATH="$PWD/.local/toolchains/go/bin:$PATH"`，运行下方验证命令，确认仍全部通过。
3. 补采自动回复、退信、别名与大小写变体样本，结果写进 phase-04.md 的「L1 结果」。探测命令的共同前缀是 `TURNCOURIER_LIVE=1 TURNCOURIER_LIVE_OUT=<仓库之外、权限恰为 0700 的目录>`，再加 `go test -count=1 -tags live -run <用例> -v ./tests/live/`；IDLE 那一轮默认 30 分钟，必须另加 `-timeout 60m`，否则 `go test` 自己的 10 分钟上限会先把它打断。
4. L1 收尾后按 phase-04.md 细化 4b 任务契约，经确认后在新分支逐任务实施：先写失败的测试，再由子代理实现，经规格与质量审查（含变异测试）、修复和独立复查后提交；阶段末做整阶段审查，然后开 PR。

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
- 2026-09-20 至 21 L1：维护者本机执行第 1–5 步，全部通过；样本写入仓库之外的 0700 目录，未进仓库。合成钥匙串条目检查后已删除，无残留。
- 2026-09-19 Phase 4a：本地 `make check`（覆盖率约 93.5%）、`make security`、`make workflows`、`make build`、两个平台的 vet 与 `CGO_ENABLED=0` 测试通过；PR #12 三项检查通过；合并后 main `4f646c6` 的 CI 与 Security 成功。
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
- 所有 QQ 凭据通过本地 `turncourier init` 输入，不发送到聊天、不进入测试或 Git。真实邮箱地址同样不写进文档、提交与续点。worktree 不是 OS 沙箱，邮件文字不能绕过 Agent 权限。
- QQ 邮箱新版把 IMAP/SMTP 开关挪到了「设置 → 账号与安全」，不在左侧设置列表里；授权码粘贴时常带空格或换行，`init` 会以「不含空白的可打印 ASCII」为由拒绝。IMAP 登录被拒时 `internal/mail/imap` 按设计不保留服务器响应文本，查不到具体原因，先核对 IMAP 服务是否真的开启、再重录授权码。
- L1 已证实的三条，实施 4b 时不要再当作未知：QQ 会改写发出邮件的 Message-ID（`X-OQ-MSGID` 保留我方 ID，实际投递 ID 从「已发送」副本取）；机器人自己的 Junk 里确实会落邮件，Junk 补扫是必需路径；同域投递不带 `Received` 与 `Authentication-Results`。另有一条是维护者人工观察而非探测实测：通知会落进**收件人**的垃圾箱——那只影响送达率，别拿它论证机器人端的 Junk 补扫。
- 回复主题的前缀至少有 `Re:` 与 `回复：` 两种（成因未确定，可能是客户端差异，也可能是对方的主题设置），主题解析器不能按前缀白名单匹配，只能按 `[TC …]` 标签本身定位。
- 令牌改到主题之后，自动回复与退信的过滤从纵深防御变成承重规则：白名单地址的假期自动回复会把带令牌的主题原样带回，四项校验全过。过滤必须排在验证顺序最前面，且这类样本尚未采过——是 4b 的前置条件。
