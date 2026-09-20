# Contributing to TurnCourier

[中文版](#参与贡献)

## Project status and scope

TurnCourier is pre-alpha. There are no releases and no version tags; the default branch is `main`.

What exists today:

- `turncourier help`, `turncourier version` and `turncourier doctor`, each with an optional `--json` flag.
- `doctor` checks the platform and runs `git --version`, `codex --version` and `claude --version`, with a 3-second timeout per tool. It does not check logins, permissions, Agent sessions or mailboxes.
- `init` is interactive setup: on macOS, in an interactive terminal and without any network access, it writes the configuration file and stores the mail authorization code and two keys in the macOS Keychain.
- `run`, `tasks`, `logs` and `service` are planned. They print that they are not implemented and exit with code 2.
- Configuration loading, SQLite storage, the state machines, the Keychain wrapper, reply tokens, body encryption and the SMTP and IMAP clients exist as internal packages with tests. So far only `init` uses any of them.
- Exit codes: `0` on success; `1` when `doctor` is not ready (the platform is not macOS, or the version check for `git`, `codex` or `claude` did not pass: missing, timed out, failed, cancelled or printed unexpected output), when `init` fails, or when writing output fails; `2` for usage errors, unknown commands and planned commands. If stdout is a closed pipe, the process is terminated by `SIGPIPE` (exit status 141).

What does not exist yet: sending or receiving email in the product (the clients exist, but nothing renders a notification, parses an inbound message or runs a send and receive loop), Agent adapters, and any background service. Do not describe these as supported in code, help output or documentation.

The approved scope, boundaries and target directory tree are in [docs/zh-CN/design.md](docs/zh-CN/design.md). The current phase checklist is [docs/zh-CN/plans/phase-04.md](docs/zh-CN/plans/phase-04.md). Before working on something that belongs to a later phase or changes the design, open a feature request so the scope can be agreed first.

## Development environment

- Go 1.27.1, as declared in [go.mod](go.mod). Besides the Go standard library, the product binary links `modernc.org/sqlite` v1.59.0, `github.com/BurntSushi/toml` v1.6.0, `golang.org/x/term` v0.46.0 and `golang.org/x/sys` v0.48.0; the mail packages use the `emersion` modules and `tests/live` uses `golang.org/x/text`, and no command imports those yet. All versions are pinned in `go.mod` and `go.sum`. A new dependency must have its reason and license stated in an implementation plan or issue. Only permissive licenses such as MIT, BSD, Apache-2.0 and ISC are accepted.
- `git`, `bash` and `tar` for `make secrets`.
- Network access the first time each quality tool is installed, and the first time Go module dependencies are downloaded (by `make vet`, `make modverify`, `make test`, `make lint` or `make check`).

If Go 1.27.1 is not your default toolchain, unpack it into `.local/toolchains/go` (ignored by Git) and put it first on `PATH` in the current shell:

```sh
export PATH="$PWD/.local/toolchains/go/bin:$PATH"
go version
```

Or pass the Go binary to Make:

```sh
make GO=/path/to/go check
```

Quality tools are pinned and installed on first use into `.local/bin/<tool>-<version>/`: staticcheck v0.8.1, govulncheck v1.8.0, gitleaks v8.30.1 and actionlint v1.7.12. `make tools` installs all four.

## Checks before you submit

Run these from the repository root:

| Command | What it runs |
| --- | --- |
| `make check` | `fmt-check`, `vet`, `modverify` (`go mod verify` against `go.sum`), `comments` (tools/commentcheck), `test` (`go test -race` writing `coverage.out`, then tools/covercheck requiring at least 80% statement coverage over all handwritten Go code, with no exclusions) and `lint` (staticcheck with [staticcheck.conf](staticcheck.conf)) |
| `make security` | `make secrets` (gitleaks over the full Git history, the staged changes and a snapshot of tracked and untracked, non-ignored files) and govulncheck |
| `make workflows` | actionlint on the GitHub Actions workflows |
| `make build` | builds `dist/turncourier` |

After `make build`, run a manual smoke test:

```sh
./dist/turncourier help
./dist/turncourier version
./dist/turncourier doctor --json
```

`doctor` exits with 1 on platforms other than macOS or when a tool is missing. That is expected behavior.

`make fmt` applies gofmt to `cmd`, `internal`, `tests` and `tools`.

`make secrets` refuses to run if `.gitleaks.toml` or `.gitleaksignore` exists at the repository root, because either file can replace the default rules or allow findings while the scan still passes. Do not add them.

Pull requests run the same targets in GitHub Actions. The CI workflow runs `make check`, `make build` and a `help`/`version` smoke test on ubuntu-24.04 and macos-15, plus `make workflows` on Linux. The Security workflow runs `make security` on ubuntu-24.04 with full history. If you could not run a command locally, say so in the pull request instead of ticking it.

## Code and comment rules

The full rules are in [AGENTS.md](AGENTS.md) and the development guide [docs/zh-CN/development.md](docs/zh-CN/development.md). In short:

- Identifiers are in English.
- Every package, every type (including types declared inside functions) and every named function or method (including tests) needs a comment written in Chinese. `make comments` enforces this with tools/commentcheck. Files with a standard generated-code header are exempt, and anonymous functions do not need their own comment.
- Comments on exported identifiers start with the identifier name, for example `// Run 执行命令并返回退出码。` The staticcheck checks ST1020, ST1021 and ST1022, enabled in [staticcheck.conf](staticcheck.conf), enforce this.
- For complex logic, comments explain error handling, concurrency and security boundaries.
- Create a directory only when there is code for it. No empty placeholder directories, dynamic plugins or microservices. The README trees show actual files; the full target tree stays in the design document.
- `turncourier` output must not contain credentials, local absolute paths or raw system errors. `doctor` reports omit executable paths and raw errors; keep it that way.

## Tests

- Unit tests live next to the code they test.
- Tests run offline. They must not contact a real mailbox, call a model or an Agent CLI, or read real credentials. Inject dependencies instead; `doctor.Checker` takes `Lookup` and `Run` functions for this purpose.
- Use synthetic data only: addresses under the reserved `.invalid` domain, made-up prompts and obvious placeholder values. Do not commit real email, session transcripts or absolute paths from your machine. Avoid placeholders shaped like real secrets, because gitleaks may flag them and ignore files are not accepted.
- Test failure behavior as well as success, such as invalid input, timeouts, cancellation and malformed output.
- Tests run with the race detector. The 80% coverage threshold counts all handwritten Go code in `cmd`, `internal` and `tools`. Add tests rather than lowering the threshold or excluding files.
- Public CI does not run real mailbox or model tests and holds no credentials.
- The Node.js scripts in [experiments/phase01](experiments/phase01/README.md) are research probes. They are not product code and not part of `make check`. The `--live` modes of `codex-probe.mjs` and `claude-probe.mjs` use your own CLI login and subscription quota.

## Pull requests

1. For anything beyond a small fix, open an issue first and describe the use case.
2. Fork the repository and branch from `main`. Keep each pull request to one change.
3. Run the checks above and fill in the pull request template. Tick only what you actually ran on the final version of the change.
4. Update documentation in the same pull request: `README.md` and `README.zh-CN.md` when commands, status or the files shown in their trees change, and the `[Unreleased]` section of [CHANGELOG.md](CHANGELOG.md).
5. The CI and Security workflows must pass. A maintainer reviews the change and may ask for revisions.

## Sensitive data

Never put QQ Mail authorization codes, tokens, passwords, real email content, Agent session transcripts or local absolute paths in issues, pull requests, commits, test data or logs. Reproduce problems with synthetic data. `turncourier init` stores the mailbox authorization code in the macOS Keychain on your own machine; note that any process running as the same user can read it back, so use a dedicated bot mailbox. Do not send an authorization code to anyone, including maintainers.

## License of contributions

TurnCourier is licensed under the Apache License 2.0; see [LICENSE](LICENSE). Unless you explicitly state otherwise, any contribution you intentionally submit is licensed under the same license, as section 5 of the license describes (inbound = outbound). No contributor license agreement (CLA) is required.

Only submit work you have the right to license this way. If you reuse compatible third-party code, keep its copyright and license notices.

## Security issues

Do not report vulnerabilities in public issues or pull requests. Follow [SECURITY.md](SECURITY.md).

## Code of conduct

Everyone taking part in this project follows the [Code of Conduct](CODE_OF_CONDUCT.md) (Contributor Covenant 2.1). Conduct concerns go to the maintainer, `@chaoRookie`, on GitHub.

---

# 参与贡献

[English](#contributing-to-turncourier)

## 项目阶段与范围

TurnCourier 处于 pre-alpha 阶段。没有发布版本，也没有版本标签，默认分支为 `main`。

目前已有：

- `turncourier help`、`turncourier version`、`turncourier doctor`，均支持可选的 `--json`。
- `doctor` 检查平台，并执行 `git --version`、`codex --version`、`claude --version`，每个工具超时 3 秒。它不验证登录、权限、Agent 会话或邮箱。
- `init` 是交互式初始化：在 macOS 的交互式终端中、不联网地写出配置文件，并把邮箱授权码与两把密钥存入 macOS Keychain。
- `run`、`tasks`、`logs`、`service` 是规划命令，运行时输出「尚未实现」，退出码为 2。
- 配置加载、SQLite 存储、各状态机、Keychain 封装、回复令牌、正文加密以及 SMTP、IMAP 客户端都已作为内部包实现并有测试；目前只有 `init` 使用了其中一部分。
- 退出码：`0` 表示成功；`1` 表示 `doctor` 未就绪（平台不是 macOS，或 `git`、`codex`、`claude` 任一版本检查未通过：缺失、超时、执行失败、被取消或输出异常），或 `init` 失败，或写入输出失败；`2` 表示用法错误、未知命令或规划命令。stdout 管道已断开时，进程按 `SIGPIPE` 终止（退出状态 141）。

尚不存在：产品中的邮件收发（客户端已有，但没有通知渲染、来信解析和收发循环）、Agent 适配器，以及任何后台服务。不要在代码、帮助输出或文档中把它们写成已支持。

已批准的范围、边界和目标目录树见 [docs/zh-CN/design.md](docs/zh-CN/design.md)，当前阶段清单见 [docs/zh-CN/plans/phase-04.md](docs/zh-CN/plans/phase-04.md)。如果要做属于后续阶段或会改变设计的工作，先提交功能建议，确认范围后再动手。

## 开发环境

- Go 1.27.1，以 [go.mod](go.mod) 为准。除 Go 标准库外，产品二进制链接 `modernc.org/sqlite` v1.59.0、`github.com/BurntSushi/toml` v1.6.0、`golang.org/x/term` v0.46.0 与 `golang.org/x/sys` v0.48.0；邮件包使用 `emersion` 系列模块，`golang.org/x/text` 由 `tests/live` 使用，目前没有命令导入它们。版本全部固定在 `go.mod` 与 `go.sum`。新增依赖须在实施清单或 issue 中说明理由与许可证，只接受 MIT、BSD、Apache-2.0、ISC 等宽松许可证。
- `make secrets` 需要 `git`、`bash` 和 `tar`。
- 每个质量工具首次安装时需要联网；首次下载 Go 模块依赖时（由 `make vet`、`make modverify`、`make test`、`make lint` 或 `make check` 触发）也需要联网。

如果默认工具链不是 Go 1.27.1，可以把它解压到被 Git 忽略的 `.local/toolchains/go`，并在当前终端把它放到 `PATH` 最前面：

```sh
export PATH="$PWD/.local/toolchains/go/bin:$PATH"
go version
```

也可以把 Go 可执行文件传给 Make：

```sh
make GO=/path/to/go check
```

质量工具版本固定，首次使用时安装到 `.local/bin/<tool>-<version>/`：staticcheck v0.8.1、govulncheck v1.8.0、gitleaks v8.30.1、actionlint v1.7.12。`make tools` 一次安装全部四个。

## 提交前检查

在仓库根目录运行：

| 命令 | 内容 |
| --- | --- |
| `make check` | `fmt-check`、`vet`、`modverify`（按 `go.sum` 执行 `go mod verify`）、`comments`（tools/commentcheck）、`test`（`go test -race` 生成 `coverage.out`，再由 tools/covercheck 要求全部手写 Go 代码的语句覆盖率不低于 80%，没有排除项）和 `lint`（按 [staticcheck.conf](staticcheck.conf) 运行 staticcheck） |
| `make security` | `make secrets`（gitleaks 扫描全部 Git 历史、暂存区，以及受跟踪和未被忽略的未跟踪文件快照）和 govulncheck |
| `make workflows` | 用 actionlint 检查 GitHub Actions 工作流 |
| `make build` | 构建 `dist/turncourier` |

`make build` 之后手工冒烟：

```sh
./dist/turncourier help
./dist/turncourier version
./dist/turncourier doctor --json
```

在非 macOS 平台或缺少工具时，`doctor` 退出码为 1，这是预期行为。

`make fmt` 对 `cmd`、`internal`、`tests`、`tools` 执行 gofmt。

仓库根目录存在 `.gitleaks.toml` 或 `.gitleaksignore` 时，`make secrets` 会拒绝扫描，因为这两个文件可能替换默认规则或放行发现，而扫描仍显示通过。不要添加它们。

PR 会在 GitHub Actions 中运行同样的目标。CI 工作流在 ubuntu-24.04 和 macos-15 上运行 `make check`、`make build` 以及 `help`/`version` 冒烟测试，并在 Linux 上运行 `make workflows`；Security 工作流在 ubuntu-24.04 上以完整历史运行 `make security`。本地没能运行的命令，请在 PR 中说明，不要勾选。

## 代码与注释规则

完整规则见 [AGENTS.md](AGENTS.md) 和开发指南 [docs/zh-CN/development.md](docs/zh-CN/development.md)。要点：

- 标识符使用英文。
- 每个包、每个类型（包括函数内声明的局部类型）、每个具名函数和方法（包括测试）都要有中文注释，由 `make comments` 调用 tools/commentcheck 检查。带标准生成代码头的文件除外，匿名函数不需要单独注释。
- 导出标识符的注释以名称开头，例如 `// Run 执行命令并返回退出码。`，由 [staticcheck.conf](staticcheck.conf) 启用的 ST1020、ST1021、ST1022 检查。
- 复杂逻辑的注释要说明错误处理、并发和安全边界。
- 有代码时才建目录。不建空占位目录，不引入动态插件或微服务。README 树形图只列实际文件，完整目标树保留在设计文档中。
- `turncourier` 的输出不得包含凭据、本机绝对路径或原始系统错误。`doctor` 报告不含可执行文件路径和原始错误，修改时保持这一点。

## 测试要求

- 单元测试与被测代码放在同一个包目录。
- 测试离线运行，不连接真实邮箱，不调用模型或 Agent CLI，不读取真实凭据。改用依赖注入；`doctor.Checker` 的 `Lookup` 和 `Run` 字段就是为此设计的。
- 只使用合成数据：保留域名 `.invalid` 下的地址、虚构的提示词和明显的占位值。不要提交真实邮件、会话记录或本机绝对路径。不要使用形似真实密钥的占位值，gitleaks 可能报出，而仓库不接受忽略清单。
- 除成功路径外，也要测试失败行为，例如非法输入、超时、取消和异常输出。
- 测试开启竞态检测。80% 覆盖率门槛统计 `cmd`、`internal`、`tools` 中的全部手写 Go 代码。覆盖率不足时补测试，不要降低门槛或排除文件。
- 公共 CI 不运行真实邮箱或模型测试，也不保存凭据。
- [experiments/phase01](experiments/phase01/README.md) 中的 Node.js 脚本是研究探针，不是产品代码，也不属于 `make check`。`codex-probe.mjs` 和 `claude-probe.mjs` 的 `--live` 模式会使用你自己的 CLI 登录并消耗订阅额度。

## PR 流程

1. 小修复以外的改动，先开 issue 说明使用场景。
2. Fork 仓库，从 `main` 建分支。每个 PR 只做一件事。
3. 运行上面的检查，填写 PR 模板。只勾选针对改动最终版本实际运行过的项目。
4. 在同一个 PR 中更新文档：命令、状态或树形图中的文件变化时更新 `README.md` 和 `README.zh-CN.md`，并更新 [CHANGELOG.md](CHANGELOG.md) 的 `[Unreleased]` 一节。
5. CI 和 Security 工作流必须通过。维护者审查后可能要求修改。

## 敏感数据

不要在 issue、PR、提交、测试数据或日志中放入 QQ 邮箱授权码、令牌、密码、真实邮件内容、Agent 会话记录或本机绝对路径。复现问题时使用合成数据。`turncourier init` 会把邮箱授权码写入你本机的 macOS Keychain；同一用户下的任何进程都能把它读回去，因此请使用专用的机器人邮箱。不要把授权码发给任何人，包括维护者。

## 贡献的许可

TurnCourier 采用 Apache License 2.0，全文见 [LICENSE](LICENSE)。除非你明确另行声明，你有意提交的贡献按许可证第 5 条以同一许可证授权（inbound = outbound）。不需要签署贡献者许可协议（CLA）。

只提交你有权按此方式授权的内容。复用兼容许可的第三方代码时，保留其版权和许可声明。

## 安全问题

不要在公开 issue 或 PR 中报告漏洞，请按 [SECURITY.md](SECURITY.md) 处理。

## 行为准则

参与本项目的所有人都需遵守[行为准则](CODE_OF_CONDUCT.md)（Contributor Covenant 2.1）。行为问题请在 GitHub 上联系维护者 `@chaoRookie`。
