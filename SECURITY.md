# Security Policy

[中文](#安全策略)

## Supported versions

TurnCourier is pre-alpha. There are no releases and no version tags. Please confirm that a problem reproduces on the latest commit of the `main` branch before reporting it.

Binaries from the manual "Candidate build" workflow are temporary Actions artifacts, not releases. If you found a problem in one, also give the commit it was built from as context.

## Reporting a vulnerability

Use GitHub private vulnerability reporting. It is the only reporting channel:

https://github.com/chaoRookie/turncourier/security/advisories/new

Do not report suspected vulnerabilities in public issues, pull requests or discussions. There is no security email address.

Include:

- the commit SHA and the output of `turncourier version` (a Candidate build binary includes the short commit SHA in its version; a local `make build` shows only `0.1.0-dev`);
- operating system and architecture;
- the affected component, for example `doctor`, `scripts/scan-secrets.sh`, the Makefile or a workflow;
- steps to reproduce, expected and actual behavior, and the impact.

Do not include real credentials (QQ Mail authorization codes, tokens, passwords, API keys), real email content or headers, Agent session transcripts, local absolute paths or personal data. Build the reproduction with synthetic data instead: addresses under the reserved `.invalid` domain, made-up prompts and placeholder values. If a real credential has already been exposed, revoke or rotate it with its provider first.

## What to expect

Reports are handled on a best-effort basis. No response time, fix time or outcome is promised, and there is no bug bounty. Please allow time for assessment before disclosing details publicly. A fix may be published together with a GitHub security advisory.

TurnCourier is provided under the [Apache License 2.0](LICENSE), without warranties of any kind.

## Scope

The current code is a CLI skeleton (`help`, `version`, `doctor`) plus build, test and scanning scripts. Examples of relevant reports:

- `doctor` running anything other than `git --version`, `codex --version` and `claude --version`, printing executable paths, raw errors or control characters taken from tool output, or, on Unix, leaving version subprocesses running after a timeout;
- a way to make `make secrets` pass while skipping Git history, staged changes or files that would be published;
- workflow problems such as broader token permissions, actions not pinned to a full commit SHA, or exposed secrets.

Out of scope:

- `doctor` reporting "not ready" on platforms other than macOS or when a tool is missing; this is expected behavior;
- vulnerabilities in Git, Codex CLI, Claude Code, Go or GitHub Actions themselves; report those to their maintainers;
- documented limitations of the research probes in `experiments/phase01`, such as the Node.js VM not being a security sandbox.

## Boundaries of the planned design

Email handling, Agent adapters, configuration, Keychain storage, SQLite storage and the background service are not implemented. The approved [design](docs/zh-CN/design.md) sets these boundaries for them:

- A Git worktree isolates working copies. It is not an operating-system sandbox, so the Agent's own sandbox and permission controls stay in place.
- An email reply becomes natural-language input to an existing Agent session. Email cannot approve tool permissions or set parameters that bypass approval; approval requests are reported for local handling.
- A sender address can be forged and is not enough on its own. A reply is rejected unless the sender allowlist, thread references, short task ID and signed token all match.
- Mailbox authorization codes and signing keys are to be stored in the macOS Keychain, not in configuration files or the repository. The authorization code is to be entered through a local `init` command.
- Filtering outgoing email cannot guarantee that every secret is removed.

Because this code does not exist yet, comments on these boundaries are design feedback and can go to a feature request. Do not include exploit details for existing code there.

---

# 安全策略

[English](#security-policy)

## 支持范围

TurnCourier 处于 pre-alpha 阶段，没有发布版本，也没有版本标签。报告前请确认问题能在 `main` 分支最新提交上复现。

手动触发的「Candidate build」工作流产生的二进制是临时 Actions artifact，不是发布版本。如果在其中发现问题，请同时提供它对应的提交作为背景。

## 报告漏洞

请使用 GitHub 私密漏洞报告，这是唯一的报告渠道：

https://github.com/chaoRookie/turncourier/security/advisories/new

不要在公开 issue、PR 或讨论中报告疑似漏洞。项目没有安全报告邮箱。

报告请包含：

- 提交 SHA 和 `turncourier version` 的输出（Candidate build 产物的版本号包含短提交 SHA，本地 `make build` 只显示 `0.1.0-dev`）；
- 操作系统和架构；
- 受影响的组件，例如 `doctor`、`scripts/scan-secrets.sh`、Makefile 或某个工作流；
- 复现步骤、期望结果与实际结果，以及影响。

报告中不要包含真实凭据（QQ 邮箱授权码、令牌、密码、API Key）、真实邮件内容或邮件头、Agent 会话记录、本机绝对路径或个人信息。请改用合成数据复现：保留域名 `.invalid` 下的地址、虚构的提示词和占位值。如果真实凭据已经泄露，请先到对应服务吊销或更换。

## 处理方式

报告按力所能及的方式处理。不承诺响应时间、修复时间或处理结果，也没有漏洞赏金。公开披露细节前，请留出评估时间。修复可能随 GitHub 安全公告一起发布。

TurnCourier 按 [Apache License 2.0](LICENSE) 提供，不附带任何形式的担保。

## 报告范围

当前代码只是 CLI 骨架（`help`、`version`、`doctor`）以及构建、测试和扫描脚本。相关报告示例：

- `doctor` 执行了 `git --version`、`codex --version`、`claude --version` 以外的命令，输出了可执行文件路径、原始错误或来自工具输出的控制字符，或在 Unix 上超时后仍留下版本子进程；
- 能让 `make secrets` 在跳过 Git 历史、暂存区或待公开文件的情况下仍然通过的方法；
- 工作流问题，例如令牌权限扩大、Action 未固定到完整提交 SHA 或泄露密钥。

不属于范围：

- `doctor` 在非 macOS 平台或缺少工具时报告未就绪，这是预期行为；
- Git、Codex CLI、Claude Code、Go 或 GitHub Actions 本身的漏洞，请报告给各自的维护者；
- `experiments/phase01` 研究探针中已写明的限制，例如 Node.js VM 不是安全沙箱。

## 规划设计中的边界

邮件处理、Agent 适配器、配置、Keychain 存储、SQLite 存储和后台服务都尚未实现。已批准的[设计](docs/zh-CN/design.md)为它们规定了以下边界：

- Git worktree 隔离工作副本，但不是操作系统沙箱，Agent 自身的沙箱和权限控制仍然保留。
- 邮件回复只作为自然语言输入送入已有的 Agent 会话。邮件不能批准工具权限，也不能设置绕过审批的参数；审批请求会通知到本地处理。
- 发件人地址可以伪造，不能单独作为身份依据。发件人白名单、线程引用、短任务 ID 和签名令牌必须全部一致，否则拒绝。
- 邮箱授权码和签名密钥将存入 macOS Keychain，不写入配置文件或仓库；授权码将通过本地 `init` 命令输入。
- 外发邮件的过滤不能保证清除所有机密。

这些代码尚不存在，因此对上述边界的意见属于设计反馈，可以通过功能建议提交。不要在其中包含针对现有代码的利用细节。
