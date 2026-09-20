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

The current code is the CLI (`help`, `version`, `doctor`, `init`), build, test and scanning scripts, and internal packages: configuration loading (`internal/config`), SQLite storage (`internal/store/sqlite`) with migration `0002`, the task, reply queue and outgoing notification state machines (`internal/task`, `internal/queue`), Keychain access (`internal/security/keychain`), reply tokens (`internal/security/token`), body encryption (`internal/security/payload`) and the mail clients (`internal/mail/smtp`, `internal/mail/imap`). Examples of relevant reports:

- `doctor` running anything other than `git --version`, `codex --version` and `claude --version`, printing executable paths, raw errors or control characters taken from tool output, or, on Unix, leaving version subprocesses running after a timeout;
- a way to make `make secrets` pass while skipping Git history, staged changes or files that would be published;
- workflow problems such as broader token permissions, actions not pinned to a full commit SHA, or exposed secrets;
- a way to get past the configuration checks (rejecting credential keys and, on Unix, requiring the config file to be owned by the current user and not writable by group or others) or, on Unix, the permission checks on the data directory and database file, or to make storage enqueue the same inbound reply twice or automatically resend a reply whose delivery is uncertain;
- a secret — the mail authorization code, the token signing key, the body encryption key or a reply token — reaching process arguments, environment variables, logs, error text or a database column in plaintext;
- accepting a tampered or expired reply token, or a body ciphertext swapped in from another row;
- a deleted pending body still being recoverable from the database file or the WAL file;
- sending credentials over a connection that is not encrypted, or an IMAP session modifying the bot mailbox (setting `\Seen`, moving, expunging or appending);
- `init` overwriting an existing configuration file or an already registered key, or automatically resending a notification whose delivery outcome is uncertain;
- the probes in `tests/live` reaching the network or the Keychain without the build tag, `TURNCOURIER_LIVE=1` and the interactive confirmations.

Out of scope:

- `doctor` reporting "not ready" on platforms other than macOS or when a tool is missing; this is expected behavior;
- another process of the same user reading the Keychain entries, and anything it can do with them, including forging a reply that passes every check and rewriting the local queue; this is the documented threat model below;
- ciphertext left behind in APFS snapshots or Time Machine backups after a pending body is deleted;
- vulnerabilities in Git, Codex CLI, Claude Code, Go or GitHub Actions themselves; report those to their maintainers;
- documented limitations of the research probes in `experiments/phase01`, such as the Node.js VM not being a security sandbox.

## Boundaries of the planned design

The mail send and receive loop, inbound reply validation, notification rendering, the Agent adapters and the background service are not implemented. Configuration, storage, the state machines, the Keychain wrapper, tokens, body encryption and the SMTP and IMAP clients are implemented; so far only `init` uses any of them.

**Threat model for the Keychain entries.** The authorization code and the two keys are stored with `/usr/bin/security`, so the trusted application is `/usr/bin/security` and the partition is `apple-tool:`. Any process running as the same user — including a shell command an Agent runs, if the Agent's sandbox allows it — can read them silently. A process that has read them can sign valid tokens, forge a reply that passes the sender allowlist, the thread reference, the subject tag and the token check, inject arbitrary content into any task, and rewrite the local queue directly. TurnCourier does not defend against processes of the same user. What the Keychain does protect against is plaintext reaching the configuration file, the repository, logs and backups; other system users reading it; and offline access to the disk without the login password. Use a dedicated bot mailbox rather than a personal one, and disable the authorization code in QQ Mail if you suspect it has leaked. Calling Security.framework in-process would bind access to TurnCourier itself, but a Go binary has only an ad-hoc signature that changes with every build, so every upgrade would ask for authorization again. That path is to be reconsidered once there is a Developer ID signature.

The approved [design](docs/zh-CN/design.md) sets these boundaries for the parts not yet implemented:

- A Git worktree isolates working copies. It is not an operating-system sandbox, so the Agent's own sandbox and permission controls stay in place.
- An email reply becomes natural-language input to an existing Agent session. Email cannot approve tool permissions or set parameters that bypass approval; approval requests are reported for local handling.
- A sender address can be forged and is not enough on its own. A reply is rejected unless the sender allowlist, thread references, short task ID and signed token all match. That check defends against someone else impersonating you; it does not defend against a process of the same user that can read the local Keychain entries, because such a process can sign a token itself.
- Filtering outgoing email cannot guarantee that every secret is removed.

Because this code does not exist yet, comments on these boundaries are design feedback and can go to a feature request. Report problems in existing code, including the configuration, storage, security and mail packages, through private vulnerability reporting, and do not include exploit details for existing code in a feature request.

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

当前代码包括 CLI（`help`、`version`、`doctor`、`init`），构建、测试和扫描脚本，以及各内部包：配置加载（`internal/config`）、含迁移 `0002` 的 SQLite 存储（`internal/store/sqlite`）、任务、回复队列与待发通知状态机（`internal/task`、`internal/queue`）、Keychain 访问（`internal/security/keychain`）、回复令牌（`internal/security/token`）、正文加密（`internal/security/payload`）以及邮件客户端（`internal/mail/smtp`、`internal/mail/imap`）。相关报告示例：

- `doctor` 执行了 `git --version`、`codex --version`、`claude --version` 以外的命令，输出了可执行文件路径、原始错误或来自工具输出的控制字符，或在 Unix 上超时后仍留下版本子进程；
- 能让 `make secrets` 在跳过 Git 历史、暂存区或待公开文件的情况下仍然通过的方法；
- 工作流问题，例如令牌权限扩大、Action 未固定到完整提交 SHA 或泄露密钥；
- 能绕过配置检查（拒绝凭据类键；在 Unix 上还要求配置文件归当前用户所有且组和其他用户不可写）或在 Unix 上绕过数据目录与数据库文件权限检查的方法，或能让存储把同一封入站回复入队两次、自动重发投递结果不确定的回复的方法；
- 机密——邮箱授权码、令牌签名密钥、正文加密密钥或回复令牌——进入进程参数、环境变量、日志、错误文本或数据库明文列；
- 接受被篡改或已过期的回复令牌，或接受从别的行调换过来的正文密文；
- 已删除的待处理正文仍能从数据库文件或 WAL 文件中恢复；
- 在未加密的连接上发送凭据，或 IMAP 会话修改了机器人邮箱（置 `\Seen`、MOVE、EXPUNGE、APPEND）；
- `init` 覆盖了已有的配置文件或已登记的密钥，或自动重发投递结果不确定的通知；
- `tests/live` 的探测在没有构建标签、`TURNCOURIER_LIVE=1` 与逐项确认的情况下联网或访问钥匙串。

不属于范围：

- `doctor` 在非 macOS 平台或缺少工具时报告未就绪，这是预期行为；
- 同一用户的其他进程读取 Keychain 条目，以及它据此能做的一切，包括伪造出通过全部校验的回复、直接改写本地队列；这是下文已记录的威胁模型；
- 待处理正文删除后留在 APFS 快照与 Time Machine 备份中的密文残留；
- Git、Codex CLI、Claude Code、Go 或 GitHub Actions 本身的漏洞，请报告给各自的维护者；
- `experiments/phase01` 研究探针中已写明的限制，例如 Node.js VM 不是安全沙箱。

## 规划设计中的边界

邮件收发循环、入站回复验证、通知渲染、Agent 适配器和后台服务都尚未实现；配置、存储、状态机、Keychain 封装、令牌、正文加密与 SMTP、IMAP 客户端已经实现，目前只有 `init` 使用了其中一部分。

**Keychain 条目的威胁模型。** 授权码与两把密钥经 `/usr/bin/security` 保存，受信任应用因此是 `/usr/bin/security`、分区为 `apple-tool:`。同一用户下的任何进程——包括 Agent 执行的 shell 命令，只要 Agent 的沙箱放行——都能静默读出它们。读到之后，该进程可以签发有效令牌，伪造出通过发件人白名单、线程引用、主题标签与令牌全部校验的回复，把任意内容注入任一任务，也可以直接改写本地队列。TurnCourier 不防同一用户下的进程。Keychain 在这里防的是：明文进入配置文件、仓库、日志与备份；其他系统用户读取；在没有登录密码的情况下离线访问磁盘。请使用专用的机器人邮箱而不是个人邮箱，怀疑泄露时在 QQ 邮箱中停用该授权码。在进程内调用 Security.framework 可以把访问权限绑定到 TurnCourier 自身，但 Go 二进制只有随构建变化的临时签名，每次升级都会重新弹窗授权；这条路要等有 Developer ID 签名后再评估。

已批准的[设计](docs/zh-CN/design.md)为尚未实现的部分规定了以下边界：

- Git worktree 隔离工作副本，但不是操作系统沙箱，Agent 自身的沙箱和权限控制仍然保留。
- 邮件回复只作为自然语言输入送入已有的 Agent 会话。邮件不能批准工具权限，也不能设置绕过审批的参数；审批请求会通知到本地处理。
- 发件人地址可以伪造，不能单独作为身份依据。发件人白名单、线程引用、短任务 ID 和签名令牌必须全部一致，否则拒绝。这一校验防的是其他人冒充，不防能读取本机 Keychain 条目的同一用户进程：那样的进程可以自己签出令牌。
- 外发邮件的过滤不能保证清除所有机密。

这些代码尚不存在，因此对上述边界的意见属于设计反馈，可以通过功能建议提交。现有代码（包括配置、存储、安全与邮件包）中的问题请通过私密漏洞报告提交，不要在功能建议中包含针对现有代码的利用细节。
