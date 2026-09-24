# TurnCourier

[English](README.md)

Email bridge for Codex and Claude Code，开发中：目标是通过回复邮件接续本机的 Agent 会话。

## 状态

Pre-alpha。Go 命令行程序提供 `help`、`version`、只读的 `doctor` 和交互式的 `init`；仓库另有质量门槛和 CI。

`turncourier init` 是第一个真正做事的命令：在 macOS 的交互式终端中、不联网地写出配置文件，生成实例 ID，以不回显方式读入 QQ 邮箱授权码，并把它与两把新生成的密钥一起存入 macOS Keychain。它从不覆盖已有的配置文件和已登记的密钥。

下列包已实现并通过测试，其中一部分已由 `init` 使用，其余等待收发循环接入：

- `internal/config` 加载并严格校验 TOML 配置（见[示例](configs/turncourier.example.toml)），并为 `init` 渲染新配置。
- `internal/task` 与 `internal/queue` 分别是任务状态机、回复队列状态机和待发通知状态机。
- `internal/store/sqlite` 保存任务、回复元数据、实例 ID、密钥元数据、待发通知、收取游标与被拒来信。待处理正文只以 AES-256-GCM 密文落盘，并在处理完成的同一事务中删除。
- `internal/security/keychain`、`token`、`payload` 分别是 Keychain 封装、回复令牌与正文加密。
- `internal/mail/smtp` 与 `internal/mail/imap` 是邮件客户端：只用隐式 TLS，每一步都有期限；IMAP 客户端只读取邮箱，从不改动它。

TurnCourier 仍然不能收发邮件：没有通知渲染、来信解析和收发循环，这些属于 4b。L1 真机探测已经完成，据此起草的 4b 任务契约已经维护者确认，两者都在 [Phase 4 实施清单](docs/zh-CN/plans/phase-04.md) 中。没有 Agent 适配器，也没有后台服务。没有任何发布版本或标签，默认分支为 `main`。

Phase 0–1 的研究探针（Node.js 脚本，不是产品代码）在一台 Mac 上从新进程恢复了同一个 Codex 会话和 Claude Code 会话。Claude Code 首次尝试的第二轮异常退出，原因尚未确定，复测通过。尚未测试真实邮件往返。证据和限制见[验证报告](docs/zh-CN/research/phase-01.md)。

## 目标体验

> 规划中，下文另有注明者除外。

通过 TurnCourier 在 Mac 上启动 Codex 或 Claude Code 任务。默认在回合完成、Agent 等待输入或任务失败时，TurnCourier 用专用 QQ 邮箱把状态和 Agent 的回复发到你自己的邮箱。你直接回复这封邮件；经过发件人、邮件线程和签名令牌校验后，回复成为同一 Agent 会话的下一条用户消息。邮件不能批准工具权限。首版面向 macOS、Codex CLI、Claude Code CLI 和 QQ 邮箱。其中「通过本地 `turncourier init` 输入授权码并存入 macOS Keychain」已经可用，上面其余内容都还没有实现。范围、安全边界和目标目录树以[设计文档](docs/zh-CN/design.md)为准。

## 快速开始

从源码构建。需要 Go 1.27.1（见 `go.mod`）、`make` 和 `git`。

```sh
git clone https://github.com/chaoRookie/turncourier.git
cd turncourier
make build
./dist/turncourier help
./dist/turncourier version --json
./dist/turncourier doctor --json
```

如果 `PATH` 中没有 Go 1.27.1，可以把它解压到被 Git 忽略的 `.local/toolchains/go`，再执行 `export PATH="$PWD/.local/toolchains/go/bin:$PATH"`；也可以直接指定工具链：`make build GO=/path/to/go`。

文本输出目前是简体中文。使用 `--json` 时，字段名和状态值为英文，`description` 和 `detail` 的文字为中文。

在装有三个工具的 macOS 上，`doctor --json` 的输出示例如下（版本号因机器而异）：

```json
{
  "platform": "darwin",
  "supported": true,
  "ready": true,
  "tools": [
    {
      "name": "git",
      "status": "ok",
      "version": "git version 2.54.0"
    },
    {
      "name": "codex",
      "status": "ok",
      "version": "codex-cli 0.154.0"
    },
    {
      "name": "claude",
      "status": "ok",
      "version": "2.1.263 (Claude Code)"
    }
  ]
}
```

只有在 macOS 上且三个工具都是 `ok` 时，`ready` 才为 `true`。工具状态为其他值（`missing`、`error`、`timeout`、`invalid`、`cancelled`）时，用简短的 `detail` 代替 `version`。

`doctor` 只运行 `git --version`、`codex --version` 和 `claude --version`。每项检查超时 3 秒；超时后终止版本子进程（Unix 上终止整个进程组），最多再等 1 秒回收输出。报告不包含可执行文件路径和原始错误信息。它不验证登录、权限、Agent 会话或邮箱。

## 命令

| 命令 | 说明 | 退出码 |
| --- | --- | --- |
| `help [--json]` | 用法、可用命令和规划命令。不带参数、`-h` 或 `--help` 时同样执行。 | 0 |
| `version [--json]` | 版本号。`make build` 设为 `0.1.0-dev`。`--version` 为别名。 | 0 |
| `doctor [--json]` | 只读检查平台以及 `git`、`codex`、`claude` 的版本。 | 0 就绪，1 未就绪 |
| `init` | 交互式创建配置，并把授权码与两把密钥存入 Keychain。仅 macOS、仅交互式终端；不接受任何参数，也不联网。 | 0 完成，1 失败 |
| `run`、`tasks`、`logs`、`service` | 规划中。输出该命令尚未实现。 | 2 |
| 其他命令或参数 | 用法错误。 | 2 |

退出码：

- `0`：成功。
- `1`：`doctor` 未就绪（平台不是 macOS，或 `git`、`codex`、`claude` 任一缺失、执行失败、超时、被中断或输出异常），或 `init` 失败，或写入输出失败。`init` 会打印一行原因，其中不含路径与机密。
- `2`：用法错误、未知命令或规划命令。
- 标准输出的管道已断开时，进程按 Unix 惯例被 `SIGPIPE` 终止（shell 退出状态 141）。

## 开发

参与贡献前请阅读 [CONTRIBUTING.md](CONTRIBUTING.md) 和[开发指南](docs/zh-CN/development.md)。

| 目标 | 作用 |
| --- | --- |
| `make build` | 构建 `dist/turncourier`。 |
| `make fmt`、`make fmt-check` | 格式化或检查 `cmd`、`internal`、`tests`、`tools` 的格式。 |
| `make vet` | `go vet ./...`，再加 `go vet -tags live ./tests/live/`。 |
| `make modverify` | `go mod verify`：模块内容须与 `go.sum` 记录的校验和一致。 |
| `make comments` | `tools/commentcheck`：检查包、类型和具名函数的中文注释。 |
| `make test` | `go test -race` 并写入 `coverage.out`；`tools/covercheck` 统计全部手写 Go 代码，语句覆盖率低于 80% 即失败，无排除。 |
| `make lint` | staticcheck v0.8.1，`staticcheck.conf` 额外启用 ST1020–ST1022；再以 `-tags live` 检查 `tests/live`。 |
| `make check` | 包含 `fmt-check`、`vet`、`modverify`、`comments`、`test`、`lint`。 |
| `make security` | govulncheck v1.8.0 和 `secrets`。 |
| `make secrets` | gitleaks v8.30.1 扫描全部 Git 历史、暂存区，以及受跟踪和未忽略文件的快照。仓库根目录存在 `.gitleaks.toml` 或 `.gitleaksignore` 时拒绝扫描。 |
| `make workflows` | actionlint v1.7.12。 |
| `make tools` | 安装四个质量工具。 |

CI 会运行 `make check`、`make security` 和 `make workflows`；提交 PR 前请在本地运行。质量工具首次使用时安装到 `.local/bin/<tool>-<version>/`，需要联网。首次运行 `make check` 还会下载 Go 模块依赖；`modernc.org/sqlite` 体积较大，首次下载和编译需要几分钟。

代码规则：

- 除 Go 标准库外，产品二进制链接 `modernc.org/sqlite` v1.59.0（纯 Go 实现的 SQLite，不需要 CGO）、`github.com/BurntSushi/toml` v1.6.0、`golang.org/x/term` v0.46.0 与 `golang.org/x/sys` v0.48.0。邮件包使用 `github.com/emersion/go-smtp` v0.25.0、`github.com/emersion/go-imap/v2` v2.0.0-beta.8、`github.com/emersion/go-message` v0.18.2 与 `github.com/emersion/go-sasl`，`golang.org/x/text` v0.42.0 由 `tests/live` 使用（`go-message/charset` 也需要它）；目前没有命令导入这五个模块，二进制中也就没有它们。版本固定在 `go.mod` 与 `go.sum`。新增依赖须在实施清单或 issue 中说明理由与许可证，只接受 MIT、BSD、Apache-2.0、ISC 等宽松许可证。
- 标识符使用英文。
- 每个手写的包、类型（包括局部类型）和具名函数（包括方法和测试）都要有中文注释，由 `tools/commentcheck` 检查。
- 导出标识符的注释以标识符名称开头，由 staticcheck 检查。

CI：

- `ci.yml` 在 ubuntu-24.04 和 macos-15 上运行 `make check`、`make build` 以及 `help`/`version` 冒烟测试，Linux 上另外运行 `make workflows`。
- `security.yml` 在 ubuntu-24.04 上以完整 Git 历史运行 `make security`。
- `build.yml` 只能手动触发。它在 macos-15 上运行 `make check` 和 `make security`，然后构建 darwin arm64、amd64 二进制和 `SHA256SUMS`，作为 Actions artifact 保留 14 天，不创建 Release。
- Actions 固定到完整提交 SHA，权限为 `contents: read`。Dependabot 每周检查 `github-actions` 和 `gomod`。

## 文档

- [设计文档](docs/zh-CN/design.md)：已批准的范围、架构、安全边界和目标目录树
- [Phase 2 实施计划](docs/zh-CN/plans/phase-02.md)
- [Phase 3 实施清单](docs/zh-CN/plans/phase-03.md)：配置、存储与状态机
- [Phase 4 实施清单](docs/zh-CN/plans/phase-04.md)：邮件闭环，分为 4a（已实现）、L1（本机真机探测）与 4b
- [Phase 0–1 验证报告](docs/zh-CN/research/phase-01.md)
- [架构概要](docs/en/architecture.md)（英文）
- [研究探针说明](experiments/phase01/README.md)
- [安全策略](SECURITY.md)
- [行为准则](CODE_OF_CONDUCT.md)
- [变更日志](CHANGELOG.md)

## 当前文件树

下面是仓库当前的实际文件，省略了 `.local/`、`dist/`、`coverage.out` 等被 Git 忽略的本地文件。目标目录树见[设计文档](docs/zh-CN/design.md)。

```text
turncourier/
├── .github/                         # GitHub 配置
│   ├── ISSUE_TEMPLATE/              # bug_report.yml、feature_request.yml、config.yml
│   ├── pull_request_template.md     # PR 模板
│   ├── dependabot.yml               # 每周更新 github-actions 和 gomod
│   └── workflows/                   # ci.yml、security.yml、build.yml（手动）
├── cmd/turncourier/                 # 可执行程序
│   ├── main.go                      # 程序入口：信号、依赖装配与退出码
│   └── main_test.go                 # 入口测试
├── configs/                         # 配置示例
│   └── turncourier.example.toml     # 测试会加载它，保证与校验规则同步
├── internal/                        # 产品代码包
│   ├── cli/                         # 命令行接口
│   │   ├── cli.go                   # help、version、doctor、init；规划命令退出码 2
│   │   ├── cli_test.go              # 输出、参数和退出码测试
│   │   ├── init.go                  # init 命令的交互流程
│   │   ├── init_test.go             # init 流程、拒绝条件与错误文案测试
│   │   ├── terminal.go              # 基于 x/term 的终端：逐行与不回显读入
│   │   ├── terminal_test.go         # 非终端输入输出的行为测试
│   │   ├── terminal_darwin.go       # macOS：经 termios 关闭终端回显
│   │   ├── terminal_darwin_test.go  # macOS 伪终端测试
│   │   └── terminal_other.go        # 其他平台：不支持不回显读入
│   ├── config/                      # TOML 配置
│   │   ├── address.go               # 严格的邮箱地址规范化
│   │   ├── address_test.go          # 表格用例与模糊测试种子
│   │   ├── config.go                # 配置类型、默认值、Load 与校验
│   │   ├── config_test.go           # 默认值、拒绝的键、环境变量覆盖
│   │   ├── paths.go                 # 配置文件与数据目录位置
│   │   ├── paths_test.go            # 默认目录与环境变量路径
│   │   ├── write.go                 # init：渲染配置并原子创建文件
│   │   ├── write_test.go            # 渲染、原子创建与从不覆盖
│   │   ├── fileperm_unix.go         # Unix：配置文件属主与写权限检查
│   │   ├── fileperm_unix_test.go    # Unix：属主检查、FIFO 与不可访问的父目录
│   │   └── fileperm_other.go        # 其他平台：只检查常规文件与大小
│   ├── doctor/                      # 环境诊断
│   │   ├── doctor.go                # 只读的平台和 --version 检查
│   │   ├── doctor_test.go           # 缺失、失败、超时和取消测试
│   │   ├── process_unix.go          # Unix：终止版本子进程所在进程组
│   │   ├── process_other.go         # 其他平台：默认取消方式
│   │   └── process_unix_test.go     # 进程组终止测试
│   ├── mail/                        # 邮件客户端；不依赖存储与配置
│   │   ├── imap/                    # 只读 IMAP，只用隐式 TLS
│   │   │   ├── session.go           # 登录、EXAMINE、UID 补扫、PEEK、IDLE
│   │   │   ├── watcher.go           # 长连接循环：看门狗与退避重连
│   │   │   ├── fakeserver_test.go   # imapmemserver 加可注入故障的代理
│   │   │   ├── session_test.go      # 无标签 BAD、半开连接、UIDVALIDITY 变化
│   │   │   ├── watcher_test.go      # 退避、登录频率上限、认证失败暂停
│   │   │   └── source_test.go       # 源码检查：DialTLS、只读、PEEK
│   │   └── smtp/                    # 只用隐式 TLS 发送单封邮件
│   │       ├── smtp.go              # 逐步期限与投递结果三分类
│   │       ├── smtp_test.go         # go-smtp 假服务器与明文防护
│   │       └── source_test.go       # 源码检查：只用 DialTLS，无调试输出
│   ├── queue/                       # 队列状态机，不执行 I/O
│   │   ├── state.go                 # 回复队列状态、事件与转移表
│   │   ├── state_test.go            # 穷举全部状态 × 事件组合
│   │   ├── outbox.go                # 待发通知状态、事件与转移表
│   │   └── outbox_test.go           # 穷举全部状态 × 事件组合
│   ├── security/                    # 凭据、令牌与正文加密
│   │   ├── keychain/                # /usr/bin/security 薄封装
│   │   │   ├── keychain.go          # Get、Set、Add、Delete 与条目命名
│   │   │   └── keychain_test.go     # 假 security 子进程与泄漏检查
│   │   ├── payload/                 # 待处理正文加密
│   │   │   ├── payload.go           # AES-256-GCM、关联数据与密文格式
│   │   │   └── payload_test.go      # 绑定、调换与长度测试
│   │   └── token/                   # 回复令牌与主题标签
│   │       ├── token.go             # 签发、解析、验证与键控正文摘要
│   │       ├── subject.go           # 主题标签类型与主题的严格文法
│   │       ├── token_test.go        # 篡改、绑定、有效期、金丝雀与模糊测试
│   │       ├── subject_test.go      # 文法表、往返、金丝雀与模糊测试
│   │       └── source_test.go       # 源码检查：常数时间比较
│   ├── store/sqlite/                # SQLite 存储
│   │   ├── store.go                 # Open、Close 与连接参数
│   │   ├── migrate.go               # 嵌入式迁移与 user_version
│   │   ├── migrations/0001_init.sql # 初始表结构
│   │   ├── migrations/0002_mail.sql # 文件夹列、密钥、通知与正文密文
│   │   ├── perm_unix.go             # Unix：数据目录与数据库文件权限检查
│   │   ├── perm_other.go            # 其他平台：不检查权限位
│   │   ├── id.go                    # 任务 ID 生成
│   │   ├── instance.go              # 实例 ID 与密钥元数据
│   │   ├── wal.go                   # 清理正文后的 TRUNCATE 检查点
│   │   ├── tasks.go                 # 任务与任务事件，按版本号更新
│   │   ├── replies.go               # 回复去重入队、派发、确认与恢复
│   │   ├── notifications.go         # 待发通知的创建、领取、记录与恢复
│   │   ├── mailbox.go               # 收取游标与被拒来信元数据
│   │   ├── store_test.go            # 连接参数、路径转义与文件权限测试
│   │   ├── migrate_test.go          # 迁移版本、回滚与版本过新测试
│   │   ├── schema_test.go           # 0002 的约束、索引与触发器测试
│   │   ├── id_test.go               # 任务 ID 编码测试
│   │   ├── instance_test.go         # 实例 ID 与密钥登记测试
│   │   ├── wal_test.go              # 检查点不等待读者的测试
│   │   ├── tasks_test.go            # 任务生命周期与版本冲突测试
│   │   ├── replies_test.go          # 去重、冲突与原子性测试
│   │   ├── notifications_test.go    # 待发通知状态、过期与从不重发测试
│   │   ├── mailbox_test.go          # 游标回退与被拒来信测试
│   │   ├── residue_test.go          # 数据库与 WAL 中无密文残留测试
│   │   └── dispatch_test.go         # FIFO、投递不确定与崩溃恢复测试
│   └── task/                        # 任务状态机，不执行 I/O
│       ├── state.go                 # 任务状态、事件与转移表
│       └── state_test.go            # 穷举全部状态 × 事件组合
├── tests/                           # 跨包测试与人工执行的测试
│   ├── docs/                        # 核对文档与代码是否一致
│   │   ├── deps_test.go             # 依赖表与 go.mod、实际导入的比对
│   │   ├── imports_test.go          # 整仓扫描包的依赖方向
│   │   └── reveal_test.go           # 令牌明文方法只在 token 与 renderer 中引用
│   ├── integration/                 # 跨包测试
│   │   ├── lifecycle_test.go        # 配置、存储与状态机的完整生命周期
│   │   └── payload_test.go          # 通知、令牌、摘要与正文密文的生命周期
│   └── live/                        # 人工执行的 L1 探测；make test 不运行
│       ├── sample.go                # 不带标签：脱敏归类与输出目录校验
│       ├── sample_test.go           # 不带标签：上述纯函数的离线测试
│       ├── live_test.go             # live 标签：开关与逐项确认
│       ├── probe_test.go            # live 标签：能力、发信、回复结构、IDLE
│       └── keychain_test.go         # live 标签：真实钥匙串往返
├── tools/                           # make 调用的工程检查工具
│   ├── commentcheck/                # make comments
│   │   ├── main.go                  # 中文注释检查器
│   │   └── main_test.go             # 检查器测试
│   └── covercheck/                  # make test
│       ├── main.go                  # 80% 语句覆盖率门槛
│       └── main_test.go             # 门槛与覆盖率文件解析测试
├── scripts/                         # make 调用的脚本
│   └── scan-secrets.sh              # gitleaks 扫描历史、暂存区和工作树
├── docs/                            # 文档
│   ├── en/architecture.md           # 英文架构概要
│   └── zh-CN/                       # 中文文档
│       ├── design.md                # 已批准设计与目标目录树
│       ├── development.md           # 开发指南
│       ├── plans/                   # 阶段实施清单
│       │   ├── phase-02.md          # Phase 2：工程骨架
│       │   ├── phase-03.md          # Phase 3：配置、存储与状态机
│       │   └── phase-04.md          # Phase 4：邮件闭环（4a 已实现，L1 已完成，4b 已规划）
│       └── research/phase-01.md     # Phase 0–1 验证结果与限制
├── experiments/phase01/             # 研究探针，不是产品代码
│   ├── README.md                    # 探针运行方式
│   ├── codex-probe.mjs              # Codex app-server 握手与会话恢复
│   ├── claude-probe.mjs             # Claude Code stream-json 与会话恢复
│   └── competitor-probe.mjs         # 固定版本上游项目的离线检查
├── .gitignore                       # 本地状态、凭据和构建产物
├── AGENTS.md                        # 面向编码 Agent 的项目规则
├── CHANGELOG.md                     # 变更记录
├── CLAUDE.md                        # 引导 Claude Code 阅读 AGENTS.md 与 HANDOFF.md
├── CODE_OF_CONDUCT.md               # Contributor Covenant 2.1
├── CONTRIBUTING.md                  # 贡献指南
├── HANDOFF.md                       # 跨 AI 编码工具的任务续点
├── LICENSE                          # Apache-2.0
├── Makefile                         # build、check、security、workflows 等目标
├── README.md                        # 英文说明
├── README.zh-CN.md                  # 中文说明
├── SECURITY.md                      # 漏洞报告方式
├── go.mod                           # 模块路径、Go 1.27.1 与依赖
├── go.sum                           # 依赖校验和
└── staticcheck.conf                 # 启用 ST1020–ST1022
```

## 安全与隐私

- 请通过 [GitHub 私密漏洞报告](https://github.com/chaoRookie/turncourier/security/advisories/new)提交漏洞，不要公开提 issue。详见 [SECURITY.md](SECURITY.md)。目前没有受支持的发布版本，请在 `main` 分支最新提交上复现问题。
- 不要在 issue、PR 或日志中提交 QQ 邮箱授权码、令牌、真实邮件内容、Agent 会话记录或本机绝对路径。复现问题请使用合成数据。
- 配置文件拒绝凭据类键。授权码与两把密钥由 `turncourier init` 写入 macOS Keychain，不进入配置文件，也不进入仓库。
- **威胁模型。** 这些条目经 `/usr/bin/security` 写入，同一用户下的任何进程——包括 Agent 执行的 shell 命令，只要 Agent 的沙箱放行——都能静默读出它们。读到之后，该进程可以签出有效令牌，伪造出通过全部校验的回复，把任意内容注入任一任务，也可以直接改写本地队列。TurnCourier 不防同一用户下的进程。Keychain 在这里防的是明文进入配置文件、仓库、日志与备份，防其他系统用户，防没有登录密码时的离线磁盘访问。请使用专用的机器人邮箱，怀疑泄露时在 QQ 邮箱中停用该授权码。详见 [SECURITY.md](SECURITY.md)。
- 待处理正文只以密文落盘，处理完成即删除；但 APFS 快照与 Time Machine 备份中残留的密文，在密钥仍留在 Keychain 时可以被解密。不要把数据目录恢复到旧备份：已发出的通知会重发，已确认的回复会再次派发。
- 公共 CI 不使用真实邮箱，也不调用模型。`tests/live` 中的探测会用，但只有维护者同时给出 `live` 构建标签、`TURNCOURIER_LIVE=1` 与逐项交互确认时才会运行。

## 许可证

[Apache-2.0](LICENSE)
