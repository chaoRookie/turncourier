# TurnCourier Phase 3 实施清单：配置、存储与状态机

> **执行说明：** 按任务顺序执行，每个任务先写失败的测试，再做最小实现，通过后提交。writing-plans 提到的 superpowers 执行类子技能在本机不可用；按维护者选择，每个任务由独立子代理实现，再由规格与质量两名审查子代理复核，确认的问题修复并复查后才进入下一任务。实现在 `feat/phase-03-storage` 分支进行。步骤使用 `- [ ]` 勾选跟踪。

**Goal:** 为后续邮件闭环提供经过测试的本地基础：严格校验的 TOML 配置、SQLite 持久化（迁移、去重、FIFO 回复队列、崩溃恢复），以及任务与回复队列状态机。本阶段不收发邮件、不启动 Agent、不读写 Keychain，`turncourier` 命令行行为不变。

**Architecture:** `internal/task` 与 `internal/queue` 是无 I/O 的纯状态机，各自维护唯一的合法转移表。`internal/config` 负责 TOML 解析、默认值、环境变量白名单和文件安全检查。`internal/store/sqlite` 只依赖两个状态机包，在事务中完成全部状态写入，写入前一律经状态机校验，并用版本号、唯一约束和部分唯一索引防止重复入队与并发重复投递。`tests/integration` 验证三者组合后的完整生命周期。

**Tech Stack:** Go 1.27.1、`database/sql`、`embed`、`modernc.org/sqlite` v1.59.0（纯 Go，无 CGO）、`github.com/BurntSushi/toml` v1.6.0、`testing`。

---

## 0. 开工前须确认的决策

以下两项改变了已公开的项目规则或阶段范围。**2026-09-18 维护者已确认 D1 与 D2，并选择由子代理逐任务实施、任务之间复核。** 其余实现细节已在本清单中定下，按 `AGENTS.md` 约定不再逐项询问。

### D1：直接引入两个第三方 Go 模块

设计文档已确定使用 TOML 配置和 SQLite 存储，Go 标准库两者都不提供。`CONTRIBUTING.md` 与 README 目前写明「生产代码只使用标准库，新增依赖先讨论」，因此本清单即依赖提案，确认后同步修改这些文档。

| 候选 | 版本 | 许可证 | CGO | 结论 |
| --- | --- | --- | --- | --- |
| `modernc.org/sqlite` | v1.59.0 | BSD 风格 | 不需要 | **推荐**：内置 SQLite 3.53.4；支持本清单使用的 `_pragma`、`_txlock` 连接参数；示例程序 arm64 二进制 10.2 MB |
| `github.com/ncruces/go-sqlite3` | v0.35.5 | MIT | 不需要 | 备选：同为 SQLite 3.53.4，只链接 5 个模块，但示例程序 arm64 二进制 14.2 MB |
| `github.com/mattn/go-sqlite3` | v1.14.52 | MIT | 需要 | 不采用：`build.yml` 以 `CGO_ENABLED=0` 在 arm64 runner 上交叉编译 amd64，引入 CGO 需要重做候选构建 |
| `github.com/BurntSushi/toml` | v1.6.0 | MIT | 不需要 | **推荐**：可通过 `MetaData.Undecoded()` 精确拒绝未知键 |
| `github.com/pelletier/go-toml/v2` | v2.4.3 | MIT | 不需要 | 备选：功能等价，无采用理由 |
| 仅标准库 | — | — | — | 不采用：只能改用 JSON 配置和自研文件存储，偏离已批准设计，且难以保证去重与入队的原子性 |

实测记录（2026-09-18，本机临时模块，示例程序同时导入驱动与 TOML 库，打开 WAL 数据库并触发一次唯一约束冲突）：

- 两种纯 Go 驱动都能在 `CGO_ENABLED=0` 下编译 darwin/arm64、darwin/amd64，功能检查通过，govulncheck 无发现。modernc 另外验证了 linux/amd64。
- modernc 示例程序的二进制：arm64 10,214,194 字节，amd64 10,418,112 字节；ncruces 为 14,173,458 与 14,969,088 字节。当前 `turncourier`（arm64）为 4,161,362 字节。
- modernc 版二进制按 `go version -m` 实际链接 11 个模块：`BurntSushi/toml`、`dustin/go-humanize`、`google/uuid`、`mattn/go-isatty`、`ncruces/go-strftime`、`remyoudompheng/bigfft`、`golang.org/x/sys`、`modernc.org/libc`、`mathutil`、`memory`、`sqlite`，许可证均为 MIT 或 BSD 风格。模块图共 27 个模块，其中 MPL-2.0 的 `hashicorp/golang-lru/v2` 只被 `modernc.org/libc` 的测试引用，不进入二进制。
- 本机首次下载 modernc 模块用时约 11 分钟（ncruces 约 3 分钟）；CI 的冷启动耗时在 Task 12 实测。

依赖规则（确认后写入开发指南）：只接受 MIT、BSD、Apache-2.0、ISC 等宽松许可证；版本固定在 `go.mod`/`go.sum`；`make check` 增加 `go mod verify`；govulncheck 与 Dependabot 覆盖依赖。正式发布二进制前必须补齐第三方许可声明，这属于发布阶段的工作。

### D2：本阶段不保存邮件正文，不接入 Keychain

设计要求待投递正文加密落盘、密钥存 Keychain。本阶段还没有邮件正文，Keychain 的接入方式（无 CGO 时调用 `/usr/bin/security` 的凭据传递安全性，或引入 CGO）需要与 `init` 命令一起设计。因此本阶段：

- 存储只保存元数据与正文 SHA-256 摘要，不提供任何保存正文的字段或接口；
- 配置文件拒绝任何凭据类键；
- 正文加密、密钥来源与 Keychain 放到邮件闭环阶段，并且必须在第一次保存正文之前完成。

### 已定的实现细节

| 事项 | 决定 |
| --- | --- |
| 配置文件位置 | `$TURNCOURIER_CONFIG`（须为绝对路径），否则 `os.UserConfigDir()/TurnCourier/turncourier.toml`，macOS 上即 `~/Library/Application Support/TurnCourier/turncourier.toml` |
| 数据目录 | `$TURNCOURIER_DATA_DIR`（须为绝对路径），否则与配置同目录；数据库文件名 `turncourier.db` |
| 可由环境变量覆盖的配置 | 仅 `TURNCOURIER_NOTIFY_EVENTS`（逗号分隔）。白名单、邮箱地址、令牌有效期等安全相关项不接受环境变量覆盖 |
| 时间 | 存储为 UTC Unix 毫秒整数，时钟可注入 |
| 任务 ID | 10 位 Crockford base32 小写字符（50 位随机数，来自 `crypto/rand`），主键冲突时最多重试 3 次 |
| 地址规范化 | 只接受单个 ASCII addr-spec，整体转小写。QQ 邮箱本地部分是否区分大小写，在邮件阶段用真实样本复核 |
| 投递不确定的恢复 | 本地核对为「已送达」时回复变为 ACKNOWLEDGED，任务回到 RUNNING；核对为「未送达」时回复按原序号回到 QUEUED，任务恢复到派发前的 COMPLETED 或 WAITING_INPUT。系统从不自动重发 |
| SQLite 参数 | WAL、`synchronous=FULL`、`foreign_keys=ON`、`busy_timeout=5000`，写事务使用 IMMEDIATE，进程内 `SetMaxOpenConns(1)`；使用 STRICT 表 |
| 文件权限 | 配置文件须归当前用户所有，且组和其他用户不可写；数据目录为 0700，数据库文件为 0600，已有目录或文件权限更宽时拒绝打开 |
| 错误信息 | 不包含本机绝对路径，与现有命令输出规则一致 |

## 范围

**本阶段实现：** 任务状态机、回复队列状态机、邮箱地址规范化、配置加载与校验、配置示例、SQLite 打开与迁移、任务持久化、入站回复去重与入队、派发与确认、崩溃后的在途回复恢复、集成测试、质量门槛与文档同步。

**不在本阶段：** `init`/`run`/`tasks`/`logs`/`service` 命令，SMTP/IMAP，MIME 与引用解析，签名令牌与撤销，正文存储与加密，Keychain，待发通知（outbox），Agent 适配器，后台服务与单实例锁，worktree，日志与遥测模块。命令行不导入新包，产品二进制内容不变。

## 文件结构

```text
configs/
└── turncourier.example.toml         # 配置示例；测试会加载它，保证与校验规则同步
internal/
├── task/
│   ├── state.go                     # 任务状态、事件、转移表、派发与入队判定
│   └── state_test.go                # 全部状态 × 事件的穷举矩阵
├── queue/
│   ├── state.go                     # 回复队列状态、事件与转移表
│   └── state_test.go
├── config/
│   ├── address.go                   # NormalizeAddress
│   ├── address_test.go              # 表格用例与模糊测试种子
│   ├── config.go                    # Config 类型、默认值、Load、校验
│   ├── config_test.go
│   ├── paths.go                     # ResolvePaths：环境变量与默认目录
│   ├── paths_test.go
│   ├── fileperm_unix.go             # 配置文件属主与写权限检查（//go:build unix）
│   └── fileperm_other.go            # 非 Unix 平台只检查常规文件与大小
└── store/sqlite/
    ├── store.go                     # Open、Close、目录与文件权限、连接参数
    ├── migrate.go                   # 嵌入式迁移与 user_version
    ├── migrations/0001_init.sql     # 初始表结构
    ├── perm_unix.go / perm_other.go # 数据目录与数据库文件权限检查
    ├── id.go                        # 任务 ID 生成
    ├── tasks.go                     # 任务创建、启动、事件、查询
    ├── replies.go                   # 回复去重入队、派发、确认、恢复
    └── *_test.go                    # 每个文件对应的测试，使用临时目录中的真实 SQLite
tests/integration/
└── lifecycle_test.go                # 配置 → 存储 → 状态机的完整生命周期
```

同时修改：`Makefile`、`go.mod`、`go.sum`、`internal/cli/cli.go` 与测试（帮助文案去掉阶段号）、`README.md`、`README.zh-CN.md`、`docs/en/architecture.md`、`docs/zh-CN/development.md`、`docs/zh-CN/design.md`（状态行）、`CONTRIBUTING.md`、`CHANGELOG.md`、`AGENTS.md`、`HANDOFF.md`。

---

### Task 1：任务状态机

**Files:**
- Create: `internal/task/state.go`
- Test: `internal/task/state_test.go`

契约（实现必须与之一致，后续任务直接引用这些名称）：

```go
// Package task 定义任务状态、事件与唯一合法的状态转移表，不执行任何 I/O。
package task

// State 是持久化在存储中的任务状态名称。
type State string

const (
	Created           State = "CREATED"
	Running           State = "RUNNING"
	WaitingInput      State = "WAITING_INPUT"
	WaitingApproval   State = "WAITING_APPROVAL"
	Completed         State = "COMPLETED"
	Failed            State = "FAILED"
	DeliveryUncertain State = "DELIVERY_UNCERTAIN"
	Closed            State = "CLOSED"
)

// Event 是驱动任务状态变化的事件名称。
type Event string

const (
	Start             Event = "start"
	TurnCompleted     Event = "turn_completed"
	InputRequested    Event = "input_requested"
	ApprovalRequested Event = "approval_requested"
	ApprovalResolved  Event = "approval_resolved"
	Fail              Event = "fail"
	ReplyDispatched   Event = "reply_dispatched"
	DeliveryUnknown   Event = "delivery_unknown"
	DeliveryConfirmed Event = "delivery_confirmed"
	Close             Event = "close"
)

// ErrInvalidTransition 表示当前状态不接受该事件，或状态名未知。
var ErrInvalidTransition = errors.New("invalid task transition")

// transitions 是任务状态机的唯一事实来源；未列出的组合一律非法。
var transitions = map[State]map[Event]State{
	Created:           {Start: Running, Fail: Failed, Close: Closed},
	Running:           {TurnCompleted: Completed, InputRequested: WaitingInput, ApprovalRequested: WaitingApproval, Fail: Failed, DeliveryUnknown: DeliveryUncertain, Close: Closed},
	WaitingInput:      {ReplyDispatched: Running, Fail: Failed, Close: Closed},
	WaitingApproval:   {ApprovalResolved: Running, Fail: Failed, Close: Closed},
	Completed:         {ReplyDispatched: Running, Close: Closed},
	Failed:            {Close: Closed},
	DeliveryUncertain: {DeliveryConfirmed: Running, Close: Closed},
	Closed:            {},
}

// Valid 报告状态名是否属于已知集合，供存储读取时校验。
func (s State) Valid() bool

// Next 返回事件发生后的状态；错误包装 ErrInvalidTransition 并写明状态与事件名。
func Next(from State, event Event) (State, error)

// ResumeAfterUnsent 在确认回复没有送达 Agent 后恢复派发前的空闲状态。
// from 必须是 Running（派发时即确认失败）或 DeliveryUncertain（本地核对为未送达），
// resume 必须是 Completed 或 WaitingInput。
func ResumeAfterUnsent(from, resume State) (State, error)

// AcceptsReplies 报告该状态下收到的合法回复是否入队等待；Created、Failed、Closed 返回 false。
func AcceptsReplies(s State) bool

// CanDispatchReply 报告是否可以立即把队首回复交给 Agent；仅 Completed 与 WaitingInput 返回 true。
func CanDispatchReply(s State) bool
```

- [x] **Step 1：写失败的测试。** `state_test.go` 中独立写一份期望矩阵 `want := map[State]map[Event]State{...}`（内容与上面的表逐项相同，作为规格副本），对 8 个状态 × 10 个事件全部调用 `Next`：矩阵中有的组合断言返回目标状态且 `err == nil`；没有的组合断言 `errors.Is(err, ErrInvalidTransition)`，且错误文本同时包含状态名和事件名。另外覆盖：
  - `Next("BOGUS", Start)` 返回 `ErrInvalidTransition`；
  - `Closed` 对任何事件都非法；
  - `ResumeAfterUnsent`：`(Running, Completed)`→`Completed`，`(DeliveryUncertain, WaitingInput)`→`WaitingInput`，`(Completed, Completed)`、`(Running, Running)`、`(Running, Closed)` 均为非法；
  - `AcceptsReplies`：Running、WaitingInput、WaitingApproval、Completed、DeliveryUncertain 为 true，Created、Failed、Closed 为 false；
  - `CanDispatchReply`：只有 Completed、WaitingInput 为 true；
  - `Valid`：8 个已知状态为 true，`""` 与 `"running"` 为 false。
- [x] **Step 2：确认测试失败。** 运行 `go test ./internal/task/`，期望编译失败（`undefined: Next` 等）。
- [x] **Step 3：最小实现。** 按契约实现；`Next` 查表，未命中时 `fmt.Errorf("%w: %s --%s-->", ErrInvalidTransition, from, event)`。
- [x] **Step 4：确认通过。** `go test -race ./internal/task/` 通过，`go test -cover ./internal/task/` 覆盖率 100%。
- [x] **Step 5：提交。** `git add internal/task && git commit -m "feat(task): add task state machine"`

### Task 2：回复队列状态机

**Files:**
- Create: `internal/queue/state.go`
- Test: `internal/queue/state_test.go`

```go
// Package queue 定义回复队列项的状态、事件与转移表；队列状态独立于任务状态，不执行任何 I/O。
package queue

// State 是回复队列项的持久化状态名称。
type State string

const (
	Queued       State = "QUEUED"
	Dispatching  State = "DISPATCHING"
	Acknowledged State = "ACKNOWLEDGED"
	Uncertain    State = "UNCERTAIN"
	Rejected     State = "REJECTED"
)

// Event 是驱动队列项状态变化的事件名称。
type Event string

const (
	Claim         Event = "claim"
	Acknowledge   Event = "acknowledge"
	MarkUncertain Event = "mark_uncertain"
	Requeue       Event = "requeue"
	Reject        Event = "reject"
)

// ErrInvalidTransition 表示队列项当前状态不接受该事件，或状态名未知。
var ErrInvalidTransition = errors.New("invalid reply queue transition")

// transitions 是队列状态机的唯一事实来源；Acknowledged 与 Rejected 为终态。
var transitions = map[State]map[Event]State{
	Queued:       {Claim: Dispatching, Reject: Rejected},
	Dispatching:  {Acknowledge: Acknowledged, MarkUncertain: Uncertain, Requeue: Queued, Reject: Rejected},
	Uncertain:    {Acknowledge: Acknowledged, Requeue: Queued, Reject: Rejected},
	Acknowledged: {},
	Rejected:     {},
}

// Valid 报告状态名是否属于已知集合。
func (s State) Valid() bool

// Next 返回事件发生后的状态；错误包装 ErrInvalidTransition 并写明状态与事件名。
func Next(from State, event Event) (State, error)
```

- [x] **Step 1：写失败的测试。** 与 Task 1 相同的方法：测试文件中写独立期望矩阵，穷举 5 个状态 × 5 个事件；覆盖未知状态、两个终态对所有事件非法、`Valid` 的已知与未知输入。
- [x] **Step 2：** `go test ./internal/queue/` 编译失败。
- [x] **Step 3：** 按契约实现。
- [x] **Step 4：** `go test -race -cover ./internal/queue/` 通过且覆盖率 100%。
- [x] **Step 5：** `git commit -m "feat(queue): add reply queue state machine"`

### Task 3：质量门槛调整

**Files:**
- Modify: `Makefile`

- [x] **Step 1：** 新增目标并加入 `check`（`fmt` 目录列表加入 `tests` 的延后项在 Task 10 创建该目录时完成，因为 gofmt 遇到不存在的目录会报错）：

```make
# go.sum 记录的依赖哈希须与模块缓存一致，防止被篡改的依赖进入构建。
modverify:
	$(GO) mod verify

check: fmt-check vet modverify comments test lint
```

并把 `.PHONY` 补上 `modverify`，第一行注释改为「工程命令在本地与 CI 共用；依赖固定在 go.mod/go.sum 并由 modverify 校验。」
- [x] **Step 2：** `make modverify` 输出 `all modules verified`；`make check` 通过。
- [x] **Step 3：** `git commit -m "build: verify module checksums in make check"`

**实施说明：** `check` 链变化使 6 处枚举子目标的文档过时（`README.md`、`README.zh-CN.md`、`CONTRIBUTING.md` 两处、`docs/zh-CN/development.md`、`docs/en/architecture.md`），另有 3 处 make 目标表格缺 `modverify` 行（两个 README 与 `development.md`）。本任务范围只允许改 `Makefile`，故不在此同步文档；上述位置已逐一写入 Task 11 Step 2/3/4/5，并由 Step 6 的 grep 断言兜底。

### Task 4：邮箱地址规范化

**Files:**
- Create: `internal/config/address.go`
- Test: `internal/config/address_test.go`

```go
// Package config 加载并校验 TurnCourier 的 TOML 配置；配置只保存账户与选项，不保存任何凭据。
package config

// ErrInvalidAddress 表示邮箱地址不是可接受的单个 ASCII addr-spec。
var ErrInvalidAddress = errors.New("invalid email address")

// NormalizeAddress 把单个邮箱地址规范化为小写 addr-spec。
// 拒绝显示名、尖括号、多个地址、引号本地部分、非 ASCII、超长输入与不合规域名，
// 结果可用于发件人白名单的精确匹配。
func NormalizeAddress(raw string) (string, error)
```

规则：去除首尾空白；总长 ≤ 254；恰好一个 `@`；本地部分 1–64 个字符，仅 dot-atom 字符 ``A-Za-z0-9.!#$%&'*+/=?^_`{|}~-``，不以 `.` 开头或结尾，不含 `..`；域名至少两个标签，每个标签 1–63 个 `[A-Za-z0-9-]` 且不以 `-` 开头或结尾，总长 ≤ 253；最后整体转小写。

- [x] **Step 1：写失败的测试。** 表格用例：

| 输入 | 期望 |
| --- | --- |
| `Me@Example.Invalid` | `me@example.invalid` |
| `  me@example.invalid  ` | `me@example.invalid` |
| `first.last+tag@mail.example.invalid` | `first.last+tag@mail.example.invalid` |
| `Me <me@example.invalid>` | 错误 |
| `<me@example.invalid>` | 错误 |
| `a@example.invalid, b@example.invalid` | 错误 |
| `"quoted"@example.invalid` | 错误 |
| `用户@example.invalid` | 错误 |
| `me@例子.invalid` | 错误 |
| `me@example` | 错误 |
| `me@@example.invalid` | 错误 |
| `.me@example.invalid`、`me.@example.invalid`、`m..e@example.invalid` | 错误 |
| `me@-bad.invalid`、`me@bad-.invalid` | 错误 |
| 空字符串、`@example.invalid`、`me@` | 错误 |
| 本地部分 65 个 `a` | 错误 |
| 总长 255 | 错误 |

  所有错误断言 `errors.Is(err, ErrInvalidAddress)`。再加 `FuzzNormalizeAddress`，种子为上表输入；性质：不 panic；成功时输出等于 `strings.ToLower(输出)`、只含一个 `@`、再次规范化结果不变。
- [x] **Step 2：** `go test ./internal/config/` 编译失败。
- [x] **Step 3：** 按规则实现，只用标准库。
- [x] **Step 4：** `go test -race ./internal/config/` 通过；`go test -run=^$ -fuzz=FuzzNormalizeAddress -fuzztime=30s ./internal/config/` 无失败（本地执行一次，CI 只运行种子）。
- [x] **Step 5：** `git commit -m "feat(config): add strict email address normalization"`

**实施说明：** 域名总长 ≤ 253 的规则在总长 ≤ 254 且本地部分至少 1 个字符时不可能触发，按规格保留为防御性上界，其 `return` 因此未被覆盖（`internal/config` 语句覆盖率 97.4%）。

### Task 5：配置加载与校验

**Files:**
- Create: `internal/config/config.go`, `internal/config/paths.go`, `internal/config/fileperm_unix.go`, `internal/config/fileperm_other.go`, `configs/turncourier.example.toml`
- Test: `internal/config/config_test.go`, `internal/config/paths_test.go`
- Modify: `go.mod`, `go.sum`（`go get github.com/BurntSushi/toml@v1.6.0`）

示例配置（`configs/turncourier.example.toml`，也是 TOML 结构的规格）：

```toml
# TurnCourier 配置示例。只保存账户与选项：
# 邮箱授权码与签名密钥将由 init 命令写入 macOS Keychain，禁止写在本文件中。

[mailbox]
# 专用机器人发件邮箱
address = "bot@example.invalid"
imap_host = "imap.qq.com"
imap_port = 993
smtp_host = "smtp.qq.com"
smtp_port = 465

[recipient]
# 接收通知的个人邮箱
address = "me@example.invalid"
# 只有这些发件人的回复会被处理；规范化后精确匹配
allowed_senders = ["me@example.invalid"]

[notify]
# 可选：turn_completed、waiting_input、waiting_approval、failed
events = ["waiting_input", "failed", "turn_completed"]

[security]
# 回复令牌有效期，1h 到 720h
token_ttl = "168h"
```

```go
// Config 是经过默认值填充与校验的只读配置，不包含任何凭据。
type Config struct {
	Mailbox   Mailbox
	Recipient Recipient
	Notify    Notify
	Security  Security
	Paths     Paths
}

// Mailbox 描述机器人发件邮箱及其 IMAP、SMTP 服务器。
type Mailbox struct {
	Address  string
	IMAPHost string
	IMAPPort int
	SMTPHost string
	SMTPPort int
}

// Recipient 描述通知收件人与允许回复的发件人白名单（均已规范化）。
type Recipient struct {
	Address        string
	AllowedSenders []string
}

// Notify 列出会触发通知邮件的事件，顺序与去重后的配置一致。
type Notify struct {
	Events []string
}

// Security 保存与回复令牌相关的非敏感选项。
type Security struct {
	TokenTTL time.Duration
}

// Paths 是解析后的配置文件、数据目录与数据库文件绝对路径。
type Paths struct {
	ConfigFile string
	DataDir    string
	Database   string
}

// ErrNotFound 表示配置文件不存在；调用方据此提示用户创建配置。
var ErrNotFound = errors.New("config file not found")

// ResolvePaths 根据环境变量与用户配置目录计算路径；环境变量给出的路径必须是绝对路径。
func ResolvePaths(getenv func(string) string, userConfigDir func() (string, error)) (Paths, error)

// Load 读取 paths.ConfigFile，严格解码、填充默认值、应用环境变量白名单并完整校验；
// 所有校验错误通过 errors.Join 一次返回，错误文本不包含本机绝对路径。
func Load(paths Paths, getenv func(string) string) (Config, error)
```

默认值：`imap_host = imap.qq.com`、`imap_port = 993`、`smtp_host = smtp.qq.com`、`smtp_port = 465`、`events = [waiting_input, failed, turn_completed]`、`token_ttl = 168h`。

- [x] **Step 1：写失败的测试（`paths_test.go`）。**
  - 未设置环境变量、`userConfigDir` 返回 `/home/u/.config` → `ConfigFile=/home/u/.config/TurnCourier/turncourier.toml`，`DataDir=/home/u/.config/TurnCourier`，`Database=.../turncourier.db`；
  - `TURNCOURIER_CONFIG=/abs/c.toml`、`TURNCOURIER_DATA_DIR=/abs/data` 分别覆盖；
  - 两个变量为相对路径时报错；`userConfigDir` 报错且未设置变量时报错。
- [x] **Step 2：写失败的测试（`config_test.go`）。** 用 `t.TempDir()` 写入 0600 文件后调用 `Load`：
  - 加载 `configs/turncourier.example.toml` 的副本成功，字段与示例一致；
  - 只写必填字段时得到上述默认值；
  - 缺少 `mailbox.address`、`recipient.address`，或 `allowed_senders` 为空时报错，而且三个错误在同一个返回值中都能找到；
  - 未知键 `[mailbox] passwrod = "x"` 报错并包含键名；凭据类键 `password`、`authorization_code`、`auth_code`、`token`、`secret` 报错，并提示「凭据将由 init 写入 Keychain」；
  - 地址非法（复用 Task 4 的错误）、白名单规范化后重复、`mailbox.address` 出现在白名单中（防止机器人自回环）均报错；
  - `events` 含未知事件或重复事件报错；`events = []` 合法；
  - 端口 0、65536 报错；主机名含 `://`、空白或为空报错；
  - `token_ttl` 为 `59m`、`721h`、`abc` 报错，`1h` 与 `720h` 合法；
  - `TURNCOURIER_NOTIFY_EVENTS="failed, waiting_input"` 覆盖文件中的事件；值非法时报错；值为空字符串时视为未设置；
  - 文件不存在时 `errors.Is(err, ErrNotFound)`；文件超过 1 MiB 报错；不是常规文件（目录）报错；
  - 仅 Unix：权限 0622、0602 报错，0600、0644 通过；
  - 所有错误文本都不包含 `t.TempDir()` 路径。
- [x] **Step 3：** `go test ./internal/config/` 失败。
- [x] **Step 4：实现。** `go get github.com/BurntSushi/toml@v1.6.0`。读取时先用 `os.Stat` 检查常规文件与大小，再调用 `checkFileOwner(info)`（unix 版本比较 `Stat_t.Uid == os.Getuid()` 并检查 `mode&0o022 == 0`；other 版本直接返回 nil），然后 `toml.NewDecoder(io.LimitReader(f, 1<<20+1)).Decode(&raw)`，读取 `md.Undecoded()`。`raw` 结构使用 `toml` 标签，端口用 `*int` 区分未设置与 0。
- [x] **Step 5：** `go test -race ./internal/config/` 通过，覆盖率 ≥ 90%；`GOOS=windows go vet ./internal/config/` 通过；`make comments` 通过。
- [x] **Step 6：** `git add configs internal/config go.mod go.sum && git commit -m "feat(config): load and validate TOML configuration"`

**实施说明：** 权限用例 0622 与 0602 都带「其他用户可写」位，把掩码收窄成 `mode&0o002` 仍能通过（变异测试验证），因此在 Step 2 的权限用例中增加只让组可写的 0620，使掩码的两个位都被钉住。

### Task 6：SQLite 打开与迁移

**Files:**
- Create: `internal/store/sqlite/store.go`, `migrate.go`, `migrations/0001_init.sql`, `perm_unix.go`, `perm_other.go`
- Test: `internal/store/sqlite/store_test.go`, `migrate_test.go`
- Modify: `go.mod`, `go.sum`（`go get modernc.org/sqlite@v1.59.0`）

```go
// Package sqlite 用 SQLite 持久化任务、入站回复与回复队列；所有状态写入都在事务中经状态机校验。
package sqlite

// Store 是可并发使用的 SQLite 存储；进程内所有操作串行经过单个连接。
type Store struct {
	db     *sql.DB
	now    func() time.Time
	random io.Reader
}

// Options 允许测试注入时钟与随机源；零值使用 time.Now 与 crypto/rand。
type Options struct {
	Now    func() time.Time
	Random io.Reader
}

// ErrSchemaTooNew 表示数据库由更新版本的 TurnCourier 创建，当前版本拒绝打开以免破坏数据。
var ErrSchemaTooNew = errors.New("database schema is newer than this build")

// Open 在 dataDir 中打开或创建 turncourier.db 并执行未应用的迁移。
// dataDir 必须是绝对路径；目录权限须为 0700、数据库文件须为 0600（Unix），否则拒绝打开。
func Open(ctx context.Context, dataDir string, opts Options) (*Store, error)

// Close 关闭数据库连接。
func (s *Store) Close() error

// SchemaVersion 返回数据库当前的 user_version。
func (s *Store) SchemaVersion(ctx context.Context) (int, error)
```

连接字符串：`"file:" + 转义后的路径 + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)&_txlock=immediate"`。路径中的 `%`、`?`、`#` 必须转义；目录名含空格与中文时须由测试证明可用。打开后 `db.SetMaxOpenConns(1)`，并用 `PingContext` 触发实际打开。

`migrations/0001_init.sql`：

```sql
CREATE TABLE tasks (
    id          TEXT PRIMARY KEY CHECK (length(id) = 10),
    owner       TEXT NOT NULL DEFAULT 'local',
    agent       TEXT NOT NULL CHECK (agent IN ('codex', 'claude')),
    session_id  TEXT CHECK (session_id IS NULL OR length(session_id) BETWEEN 1 AND 200),
    state       TEXT NOT NULL CHECK (state IN ('CREATED', 'RUNNING', 'WAITING_INPUT', 'WAITING_APPROVAL', 'COMPLETED', 'FAILED', 'DELIVERY_UNCERTAIN', 'CLOSED')),
    version     INTEGER NOT NULL CHECK (version >= 1),
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
) STRICT;

CREATE TABLE task_events (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id     TEXT NOT NULL REFERENCES tasks (id),
    event       TEXT NOT NULL,
    from_state  TEXT NOT NULL,
    to_state    TEXT NOT NULL,
    created_at  INTEGER NOT NULL
) STRICT;

CREATE INDEX task_events_by_task ON task_events (task_id, seq);

CREATE TABLE inbound_messages (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    account       TEXT NOT NULL CHECK (length(account) BETWEEN 3 AND 254),
    uid_validity  INTEGER NOT NULL CHECK (uid_validity BETWEEN 1 AND 4294967295),
    uid           INTEGER NOT NULL CHECK (uid BETWEEN 1 AND 4294967295),
    message_id    TEXT NOT NULL CHECK (length(message_id) BETWEEN 3 AND 998),
    body_sha256   BLOB NOT NULL CHECK (length(body_sha256) = 32),
    task_id       TEXT NOT NULL REFERENCES tasks (id),
    received_at   INTEGER NOT NULL,
    UNIQUE (account, uid_validity, uid),
    UNIQUE (account, message_id)
) STRICT;

CREATE TABLE replies (
    seq            INTEGER PRIMARY KEY AUTOINCREMENT,
    inbound_id     INTEGER NOT NULL UNIQUE REFERENCES inbound_messages (id),
    task_id        TEXT NOT NULL REFERENCES tasks (id),
    state          TEXT NOT NULL CHECK (state IN ('QUEUED', 'DISPATCHING', 'ACKNOWLEDGED', 'UNCERTAIN', 'REJECTED')),
    resume_state   TEXT CHECK (resume_state IS NULL OR resume_state IN ('COMPLETED', 'WAITING_INPUT')),
    reject_reason  TEXT CHECK (reject_reason IS NULL OR reject_reason IN ('task_not_accepting', 'task_failed', 'task_closed')),
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
) STRICT;

CREATE UNIQUE INDEX replies_one_in_flight ON replies (task_id) WHERE state IN ('DISPATCHING', 'UNCERTAIN');
CREATE INDEX replies_by_task_state ON replies (task_id, state, seq);
```

表中不存在任何正文字段（D2）。

- [x] **Step 1：写失败的测试（`migrate_test.go`）。** 迁移逻辑拆成 `migrate(ctx, db *sql.DB, fsys fs.FS) error` 以便注入：
  - 空库迁移后 `user_version = 1`，四张表与两个索引存在（查询 `sqlite_schema`）；
  - 重复打开不重复执行，版本仍为 1；
  - 手动设置 `PRAGMA user_version = 99` 后 `Open` 返回 `ErrSchemaTooNew`，数据未改动；
  - 注入 `fstest.MapFS{"0001_init.sql": 合法SQL, "0002_bad.sql": "CREATE TABLE broken ("}`：返回错误，`user_version` 停在 1，不存在 `broken` 表；
  - 文件名不是 `四位数字_名称.sql`，或编号不连续（`0001`、`0003`）时报错。
- [x] **Step 2：写失败的测试（`store_test.go`）。**
  - `PRAGMA journal_mode` 为 `wal`，`PRAGMA foreign_keys` 为 1，`PRAGMA synchronous` 为 2（FULL）；
  - 目录名含空格、`#`、中文时可以打开并完成读写；
  - 相对路径报错；数据库文件内容为随机字节时报错；
  - 仅 Unix：新建目录的权限为 0700、文件为 0600；已有目录权限 0755 或文件权限 0644 时拒绝；
  - 错误文本不包含临时目录路径；
  - STRICT 生效：直接执行 `INSERT INTO tasks (..., version, ...) VALUES (..., 'x', ...)` 失败。
- [x] **Step 3：** 测试失败。
- [x] **Step 4：实现。** `go get modernc.org/sqlite@v1.59.0`；用 `//go:embed migrations/*.sql` 嵌入迁移；每个待应用迁移在一个事务中执行 SQL 与 `PRAGMA user_version = N`。
- [x] **Step 5：** `go test -race ./internal/store/sqlite/` 通过；`make modverify` 通过；`GOOS=windows go vet ./internal/store/sqlite/` 通过；`CGO_ENABLED=0 go test ./internal/store/sqlite/` 通过（证明不依赖 CGO）。
- [x] **Step 6：** `git commit -m "feat(store): open SQLite database with embedded migrations"`

**实施说明：** Step 1 写的「两个索引」与 `0001_init.sql` 不符：脚本建了 `task_events_by_task`、`replies_one_in_flight`、`replies_by_task_state` 三个索引，测试按四张表加三个索引精确断言。`t.TempDir()` 按 0777 减去 umask 建目录（通常为 0755），直接作为数据目录会被权限检查拒绝，因此测试与后续集成测试都应使用其下由 `Open` 以 0700 新建的子目录。`synchronous` 在本构建中默认即为 FULL，去掉该连接参数的变异无法被测试区分，只能由 PRAGMA 断言与代码审查保证。

### Task 7：任务持久化

**Files:**
- Create: `internal/store/sqlite/id.go`, `internal/store/sqlite/tasks.go`
- Test: `internal/store/sqlite/id_test.go`, `internal/store/sqlite/tasks_test.go`

```go
// Agent 是任务使用的 Agent 类型。
type Agent string

const (
	AgentCodex  Agent = "codex"
	AgentClaude Agent = "claude"
)

// Task 是任务的持久化快照；Version 用于乐观并发控制。
type Task struct {
	ID        string
	Owner     string
	Agent     Agent
	SessionID string
	State     task.State
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TaskEvent 是一条只含元数据的任务状态变化记录。
type TaskEvent struct {
	Seq    int64
	TaskID string
	Event  task.Event
	From   task.State
	To     task.State
	At     time.Time
}

var (
	// ErrNotFound 表示指定的任务或回复不存在。
	ErrNotFound = errors.New("not found")
	// ErrVersionConflict 表示调用方持有的任务版本已过期，需要重新读取后再决定。
	ErrVersionConflict = errors.New("task version conflict")
	// ErrInvalidSessionID 表示 Agent 会话 ID 为空、过长或含不允许的字符。
	ErrInvalidSessionID = errors.New("invalid agent session id")
)

// newTaskID 从 random 读取 7 字节，取前 50 位编码为 10 位 Crockford base32 小写字符。
func newTaskID(random io.Reader) (string, error)

// CreateTask 以 CREATED 状态创建任务；ID 主键冲突时最多重试 3 次。
func (s *Store) CreateTask(ctx context.Context, agent Agent) (Task, error)

// GetTask 按 ID 读取任务。
func (s *Store) GetTask(ctx context.Context, id string) (Task, error)

// StartTask 记录 Agent 会话 ID 并执行 start 事件；会话 ID 一经写入不可修改。
func (s *Store) StartTask(ctx context.Context, id string, version int64, sessionID string) (Task, error)

// ApplyTaskEvent 执行 turn_completed、input_requested、approval_requested、approval_resolved、fail、close 之一。
// start、reply_dispatched、delivery_unknown、delivery_confirmed 必须由对应的存储操作原子完成，这里直接拒绝。
// fail 与 close 在同一事务中把该任务所有 QUEUED 回复改为 REJECTED（原因分别为 task_failed、task_closed）。
func (s *Store) ApplyTaskEvent(ctx context.Context, id string, version int64, event task.Event) (Task, error)

// TaskEvents 按发生顺序返回任务的状态变化记录。
func (s *Store) TaskEvents(ctx context.Context, id string) ([]TaskEvent, error)
```

会话 ID 规则：1–200 个 `[A-Za-z0-9._:-]` 字符。

- [x] **Step 1：写失败的测试。**
  - `newTaskID`：固定输入 `bytes.NewReader([]byte{0,0,0,0,0,0,0})` 得到 `0000000000`，全 `0xff` 得到 `zzzzzzzzzz`；读取不足 7 字节时报错；1000 次真实随机生成的结果都匹配 `^[0-9abcdefghjkmnpqrstvwxyz]{10}$`；
  - `CreateTask`：返回 CREATED、Version 1、Owner `local`，时间来自注入时钟；未知 agent 报错；随机源连续 3 次给出与已有任务相同的 ID 时报错，第 2 次给出新 ID 时成功；
  - `GetTask` 不存在时 `ErrNotFound`；
  - `StartTask`：成功后为 RUNNING、Version 2、SessionID 已保存；版本过期返回 `ErrVersionConflict`；会话 ID 为空、201 字符、含空格或换行返回 `ErrInvalidSessionID`；对 RUNNING 任务再次调用返回 `task.ErrInvalidTransition`；
  - `ApplyTaskEvent`：六个允许事件各至少一条成功路径；四个保留事件返回 `task.ErrInvalidTransition`；不存在的任务返回 `ErrNotFound`；错误版本返回 `ErrVersionConflict`，且数据库未改动；
  - `TaskEvents`：CREATED→RUNNING→COMPLETED→CLOSED 依次记录 from/to/event，按 seq 升序；
  - fail 与 close 拒绝排队回复的行为在 Task 8 数据就绪后补测（写入 Task 8 Step 1）。
- [x] **Step 2：** 测试失败。
- [x] **Step 3：实现。** 所有写操作使用 `BeginTx`（连接串已设置 IMMEDIATE）。更新语句形如 `UPDATE tasks SET state=?, version=version+1, updated_at=? WHERE id=? AND version=?`，受影响行数为 0 时再查一次任务，区分 `ErrNotFound` 与 `ErrVersionConflict`。读取时校验 `task.State(...).Valid()`，非法值返回错误，不静默接受。
- [x] **Step 4：** `go test -race ./internal/store/sqlite/` 通过。
- [x] **Step 5：** `git commit -m "feat(store): persist tasks with optimistic versioning"`

**实施说明：** 「主键冲突时最多重试 3 次」与 Step 1「随机源连续 3 次给出与已有任务相同的 ID 时报错」只有按「连同首次共尝试 3 次」理解才一致，实现如此，`CreateTask` 文档注释相应写明；测试在 3 个重复 ID 之后再放一个新 ID，断言仍报错且新 ID 未被读取，并用「第 3 次成功」钉住下限。Step 3 的写法在事务内先读取任务（`task.Next` 需要当前状态），不存在返回 `ErrNotFound`、版本不符返回 `ErrVersionConflict`，版本比较先于状态机校验，使持有过期版本的调用方总是得到版本冲突；`UPDATE` 仍带 `version` 条件，受影响行数不为 1 时返回 `ErrVersionConflict`，在 IMMEDIATE 事务下只作防御，去掉该 SQL 条件的变异无法被测试区分。清单未规定 `TaskEvents` 查询不存在的任务时的行为，按其他以 ID 读取的方法统一返回 `ErrNotFound`。

### Task 8：入站回复去重与入队

**Files:**
- Create: `internal/store/sqlite/replies.go`
- Test: `internal/store/sqlite/replies_test.go`

```go
// InboundReply 是已通过发件人、线程与令牌校验的入站回复元数据；不含正文。
type InboundReply struct {
	TaskID      string
	Account     string // 调用方已用 config.NormalizeAddress 规范化的机器人邮箱地址
	UIDValidity uint32
	UID         uint32
	MessageID   string // 与邮件头一致的原样字符串
	BodySHA256  [32]byte
}

// Reply 是回复队列项的持久化快照；Seq 即本地入队顺序。
type Reply struct {
	Seq          int64
	TaskID       string
	State        queue.State
	ResumeState  task.State // 仅在派发后记录派发前的任务状态
	RejectReason string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// RecordResult 说明 RecordReply 是新入队还是命中了已有记录。
type RecordResult struct {
	Reply     Reply
	Duplicate bool
}

// ErrMessageConflict 表示同一邮件标识对应了不同内容或不同 Message-ID，必须拒绝并告警。
var ErrMessageConflict = errors.New("inbound message conflict")

// RecordReply 在同一事务中完成去重检查、写入入站记录与写入回复队列项。
// 任务处于 AcceptsReplies 为 true 的状态时入队为 QUEUED；否则记录为 REJECTED，原因为 task_not_accepting、task_failed 或 task_closed。
func (s *Store) RecordReply(ctx context.Context, in InboundReply) (RecordResult, error)
```

- [x] **Step 1：写失败的测试。** 以 RUNNING 任务 `T`、账户 `bot@example.invalid` 为基础：

| 场景 | 期望 |
| --- | --- |
| 新邮件（uv=7, uid=1, `<a@example.invalid>`, 摘要 A） | QUEUED，`Duplicate=false`，seq=1 |
| 同 uv/uid、同 Message-ID、同摘要再次记录 | `Duplicate=true`，返回同一 seq，行数不变 |
| 不同 uid（uid=2）、同 Message-ID、同摘要 | `Duplicate=true`，行数不变 |
| 同 Message-ID、摘要 B | `ErrMessageConflict`，行数不变 |
| 同 uv/uid、不同 Message-ID | `ErrMessageConflict`，行数不变 |
| UIDVALIDITY 变为 8、uid=1、同 Message-ID、同摘要 | `Duplicate=true` |
| 任务不存在 | `ErrNotFound`，行数不变 |
| 任务 FAILED / CLOSED / CREATED | REJECTED，原因分别为 `task_failed`、`task_closed`、`task_not_accepting` |
| 被拒绝的邮件再次记录 | `Duplicate=true`，仍为 REJECTED |
| 字段校验：Account 为空、超过 254 字符或含空白，UIDValidity=0，UID=0，MessageID 少于 3 字符 | 事务开始前返回错误 |
| 原子性：测试中创建触发器 `CREATE TRIGGER boom BEFORE INSERT ON replies BEGIN SELECT RAISE(ABORT, 'boom'); END;` 后记录新邮件 | 返回错误，`inbound_messages` 行数不变 |

  补测 Task 7 遗留项：3 条 QUEUED 回复后执行 `fail`，3 条都变为 REJECTED(`task_failed`)；另一任务执行 `close` 后都变为 REJECTED(`task_closed`)；两种情况下已处于 DISPATCHING 的回复保持不变。
- [x] **Step 2：** 测试失败。
- [x] **Step 3：实现。** 事务内顺序：读取任务 → 按 `(account, uid_validity, uid)` 查找 → 按 `(account, message_id)` 查找 → 判定重复或冲突 → 插入 `inbound_messages` → 插入 `replies`。存储层不导入 `config`，只做长度与空白检查，地址规范化由调用方负责，以保持 `store/sqlite` 只依赖 `task` 与 `queue`。
- [x] **Step 4：** `go test -race ./internal/store/sqlite/` 通过。
- [x] **Step 5：** `git commit -m "feat(store): deduplicate and enqueue inbound replies atomically"`

**实施说明：** 表格只比较 Message-ID 与摘要，入站记录却同时绑定任务；同一邮件标识指向另一任务时按「同一邮件标识对应了不同内容」处理，返回 `ErrMessageConflict`，而不是返回别的任务的回复并标为重复，测试「同一邮件指向另一任务」钉住这一点。字段校验与表约束对齐：Account 为 3–254 个字符且不含空白（空字符串包含在内），MessageID 为 3–998 个字符，UIDValidity 与 UID 为正；长度按 Unicode 字符计，与表约束中 SQLite 的 `length()` 一致。`length()` 遇到 NUL 即停止计数，而地址与邮件头本不应含 NUL，因此 Account 或 MessageID 含 NUL 时同样在事务前拒绝，这是清单之外的补充；含非法 UTF-8 时两者计数可能不同，仍由 CHECK 约束兜底。清单未定义字段错误的哨兵值，错误文本统一以 `invalid inbound reply` 开头，测试据此区分 Go 端校验与 CHECK 约束、任务查询的报错；用已取消的上下文调用仍得到校验错误，证明校验先于开始事务，用「任务不存在且 UID 为 0」证明校验先于读取任务。按 Step 3 的顺序先读任务再查重，因此任务不存在时总是返回 `ErrNotFound`；重复邮件返回原回复，不按任务的当前状态重新判定。fail 与 close 拒绝 QUEUED 回复的行为 Task 7 已实现，本任务只补测试。

### Task 9：派发、确认与崩溃恢复

**Files:**
- Modify: `internal/store/sqlite/replies.go`
- Test: `internal/store/sqlite/dispatch_test.go`

```go
// ErrNoDispatchableReply 表示任务当前不可派发、已有在途回复或队列为空。
var ErrNoDispatchableReply = errors.New("no dispatchable reply")

// ClaimNextReply 在一个事务中把任务的最早 QUEUED 回复改为 DISPATCHING，
// 记录派发前的任务状态，并执行 reply_dispatched 使任务进入 RUNNING。
func (s *Store) ClaimNextReply(ctx context.Context, taskID string) (Reply, Task, error)

// AcknowledgeReply 在 Agent 确认收到后把 DISPATCHING 回复改为 ACKNOWLEDGED；任务状态不变。
func (s *Store) AcknowledgeReply(ctx context.Context, seq int64) (Reply, Task, error)

// MarkReplyUncertain 在无法确认 Agent 是否收到时把 DISPATCHING 改为 UNCERTAIN；
// 任务为 RUNNING 时同时执行 delivery_unknown。
func (s *Store) MarkReplyUncertain(ctx context.Context, seq int64) (Reply, Task, error)

// RequeueUnsentReply 在确定回复没有发给 Agent 时把 DISPATCHING 回复按原序号放回 QUEUED，
// 任务恢复到派发前状态；任务已不接受回复时改为 REJECTED。
func (s *Store) RequeueUnsentReply(ctx context.Context, seq int64) (Reply, Task, error)

// ResolveUncertainReply 记录本地核对结果：delivered 为 true 时回复 ACKNOWLEDGED，任务处于 DELIVERY_UNCERTAIN 时执行 delivery_confirmed；
// 为 false 时回复按原序号回到 QUEUED、任务恢复派发前状态；任务已关闭或失败时回复改为 REJECTED，任务状态不变。
func (s *Store) ResolveUncertainReply(ctx context.Context, seq int64, delivered bool) (Reply, Task, error)

// RecoverInFlight 在进程启动时调用：把所有 DISPATCHING 回复改为 UNCERTAIN（对应 RUNNING 任务进入 DELIVERY_UNCERTAIN），
// 返回全部 UNCERTAIN 回复供本地核对；不会自动重新派发任何回复。
func (s *Store) RecoverInFlight(ctx context.Context) ([]Reply, error)
```

- [x] **Step 1：写失败的测试。**
  - **FIFO：** 任务 COMPLETED，依次记录 3 条回复；`ClaimNextReply` 得到 seq 1，任务 RUNNING，`ResumeState=COMPLETED`；再次调用返回 `ErrNoDispatchableReply`（任务 RUNNING 且有在途）；`AcknowledgeReply(1)` 后仍返回 `ErrNoDispatchableReply`（任务 RUNNING）；`turn_completed` 后得到 seq 2。
  - **等待输入：** WAITING_INPUT 任务派发后 `ResumeState=WAITING_INPUT`。
  - **不可派发状态：** 任务为 RUNNING、WAITING_APPROVAL、DELIVERY_UNCERTAIN、FAILED、CLOSED 时返回 `ErrNoDispatchableReply`；任务不存在返回 `ErrNotFound`；队列为空返回 `ErrNoDispatchableReply`。
  - **不确定：** 派发后 `MarkReplyUncertain`：回复 UNCERTAIN，任务 DELIVERY_UNCERTAIN；`ClaimNextReply` 返回 `ErrNoDispatchableReply`；`ResolveUncertainReply(delivered=true)`：回复 ACKNOWLEDGED，任务 RUNNING。
  - **未送达：** `ResolveUncertainReply(delivered=false)`：回复 QUEUED、seq 不变，任务回到 COMPLETED；再次 `ClaimNextReply` 仍得到同一 seq。
  - **派发前失败：** `RequeueUnsentReply` 使回复回到 QUEUED，任务回到派发前状态；任务在派发期间被关闭后调用，回复变为 REJECTED(`task_closed`)，任务保持 CLOSED。
  - **非法调用：** 对 QUEUED 回复调用 `AcknowledgeReply`、`MarkReplyUncertain`、`ResolveUncertainReply` 返回 `queue.ErrInvalidTransition`；不存在的 seq 返回 `ErrNotFound`。
  - **崩溃恢复：** 派发后直接 `Close` 存储（模拟进程崩溃），重新 `Open`，`RecoverInFlight` 返回该回复且状态为 UNCERTAIN，任务为 DELIVERY_UNCERTAIN；再次调用 `RecoverInFlight` 结果不变（幂等）。
  - **跨进程并发：** 两个独立 `Open` 同一数据目录的 Store，在 20 个 goroutine 中同时对同一任务 `ClaimNextReply`，恰好 1 个成功，其余返回 `ErrNoDispatchableReply`，没有 `SQLITE_BUSY` 错误泄漏；循环 20 轮。
  - **数据库兜底：** 绕过 API 直接插入同一任务的第二条 DISPATCHING 回复，违反 `replies_one_in_flight` 而失败。
  - **事件记录：** 上述操作产生的任务事件按顺序出现在 `TaskEvents` 中。
- [x] **Step 2：** 测试失败。
- [x] **Step 3：实现。** 每个方法一个事务：读取回复与任务 → 用 `queue.Next` 与 `task.Next`/`task.ResumeAfterUnsent` 计算新状态 → 以 `WHERE seq=? AND state=?`、`WHERE id=? AND version=?` 条件更新 → 写任务事件。任何一步受影响行数不为 1 时回滚并返回对应错误。
- [x] **Step 4：** `go test -race -count=3 ./internal/store/sqlite/` 通过，无抖动；包覆盖率 ≥ 85%。
- [x] **Step 5：** `git commit -m "feat(store): dispatch, acknowledge and recover replies without blind resend"`

**实施说明：** 清单没有给「未送达后恢复派发前状态」命名任务事件，`task.ResumeAfterUnsent` 也不在转移表中，存储层以 `reply_unsent` 记录（与 `reply_dispatched` 对应），该事件不能经 `ApplyTaskEvent` 执行。按序号操作的方法只接受各自的来源状态：`AcknowledgeReply`、`MarkReplyUncertain`、`RequeueUnsentReply` 只接受 DISPATCHING，`ResolveUncertainReply` 只接受 UNCERTAIN；队列转移表允许 UNCERTAIN 直接 acknowledge 或 requeue，但若经 `AcknowledgeReply` 处理，DELIVERY_UNCERTAIN 任务将失去执行 delivery_confirmed 的途径，因此这类调用同样返回 `queue.ErrInvalidTransition`。回到 QUEUED 的回复清除 `resume_state`，与新入队的回复一致；改为 REJECTED 的回复保留它，记录曾被派发。任务仍接受回复时，放回队列与核对为未送达只在任务自派发以来没有发生其他事件时成功：同一事务中读取该任务最新的事件，DISPATCHING 回复要求最新一条为 `reply_dispatched`，UNCERTAIN 回复要求最新两条依次为 `reply_dispatched`、`delivery_unknown`（审批往返后回到 RUNNING 再标记不确定，最新一条同样是 `delivery_unknown`，只看一条会放行）；否则说明 Agent 已处理该回复（例如回合已结束或审批往返），返回包装 `task.ErrInvalidTransition`、提示「回复应核对为已送达」的错误且不改动数据。这不会阻塞队列：DISPATCHING 回复经 `AcknowledgeReply`、UNCERTAIN 回复经 `ResolveUncertainReply(true)` 确认后不再阻塞后续派发（任务回到 COMPLETED 或 WAITING_INPUT 后派发下一条）；审查建议的「只放回回复、不改任务」会造成重复投递，未采纳。任务不是 RUNNING 时标记不确定不改动任务。清单中「任务已关闭或失败时回复改为 REJECTED」只适用于未送达：核对为已送达的回复一律记为 ACKNOWLEDGED，任务已关闭或失败时同样如此，`RejectReason` 为空，任务状态、版本与事件都不变。`ClaimNextReply` 在任务可派发时先检查在途回复，使其返回 `ErrNoDispatchableReply` 而不是违反 `replies_one_in_flight` 的数据库错误。`getReply` 查不到回复时改为返回 `ErrNotFound`。为让回复操作与 `applyEvent` 共用同一条带版本条件的任务更新与事件写入，把这段代码从 `applyEvent` 提取为 `tasks.go` 中的 `writeTaskTransition`，这是 Files 列表之外对 `tasks.go` 的唯一改动，行为不变。清单场景之外补测：不可派发状态加测 CREATED；用触发器让每个多步写入的操作在回复更新、任务更新、事件写入处失败或更新不命中，证明整体回滚，另测已取消的上下文，包覆盖率首次提交时由 83.6% 升至 86.8%，审查修复后为 86.5%。`updateReply` 的 `state` 条件与 Task 7 的版本条件一样，在 IMMEDIATE 事务下只作防御，去掉它的变异无法被测试区分；去掉 `_txlock=immediate` 的变异会被跨进程并发测试以 SQLITE_BUSY 错误拦截。

### Task 10：集成测试

**Files:**
- Create: `tests/integration/lifecycle_test.go`
- Modify: `Makefile`（`fmt`、`fmt-check` 目录加入 `tests`）

- [x] **Step 1：** 修改 Makefile 目录列表；`make fmt-check` 通过。
- [x] **Step 2：写测试。** 包名 `integration_test`，文件开头写中文包注释。`TestReplyLifecycle`：
  1. 把 `configs/turncourier.example.toml` 复制到临时目录并设为 0600；设置 `TURNCOURIER_CONFIG` 与 `TURNCOURIER_DATA_DIR`（均在临时目录内）；`config.ResolvePaths` + `config.Load` 成功；
  2. `sqlite.Open(ctx, cfg.Paths.DataDir, sqlite.Options{})`；创建 codex 任务，`StartTask` 会话 ID `thread-synthetic-0001`，执行 `turn_completed`；
  3. 以 `cfg.Mailbox.Address` 为账户，用 `sha256.Sum256` 计算合成正文 `"first synthetic reply"` 与 `"second synthetic reply"` 的摘要，记录两条回复；
  4. 派发第 1 条 → `MarkReplyUncertain` → 关闭存储 → 重新打开 → `RecoverInFlight` 返回 1 条 → `ResolveUncertainReply(true)` → `turn_completed`；
  5. 派发得到第 2 条 → `AcknowledgeReply` → `close`；
  6. 再次记录第 1 条回复为 `Duplicate=true`；记录第 3 条新回复为 REJECTED(`task_closed`)；
  7. 断言完整任务事件序列：`start, turn_completed, reply_dispatched, delivery_unknown, delivery_confirmed, turn_completed, reply_dispatched, close`。
- [x] **Step 3：** `go test -race ./tests/...` 通过；`make comments` 通过（确认测试专用包的中文包注释被识别）。
- [x] **Step 4：** `git commit -m "test: cover config, store and state machine lifecycle end to end"`

**实施说明：** `tests` 目录在 Step 2 之前不存在，`gofmt -l` 对缺失目录报 `lstat tests: no such file or directory` 并以非零码退出，因此 Step 1 改完 Makefile 后 `make fmt-check` 失败，建立测试文件后才通过；在临时副本中放入未格式化的测试文件，新目录列表下 `make fmt-check` 失败、旧列表下通过。Step 2 第 7 点的事件序列与 Task 8–9 已合入的实现一致（`RecoverInFlight` 处理的是已为 UNCERTAIN 的回复，不产生事件；`AcknowledgeReply` 不产生任务事件），无需调整。清单之外的两处补充：用 `t.Setenv` 清空 `TURNCOURIER_NOTIFY_EVENTS`，避免开发者环境影响加载；断言 `cfg.Paths.DataDir` 等于环境变量给出的目录。数据目录是临时目录下尚不存在、由 `sqlite.Open` 以 0700 创建的子目录。本任务只加测试，存储实现已在 Task 6–9 合入，测试首次运行即通过；在临时副本中对存储层做 5 个变异（派发改为倒序、`RecoverInFlight` 返回空列表、核对已送达时不执行 delivery_confirmed、关闭任务的拒绝原因改为 `task_failed`、确认改为标记不确定）均被本测试拦截，去掉包注释后 `make comments` 执行的 `go run ./tools/commentcheck .` 报「包 integration_test 缺少中文包级文档」。

### Task 11：命令行文案与文档同步

**Files:**
- Modify: `internal/cli/cli.go`、`internal/cli/cli_test.go`、`cmd/turncourier/main_test.go`、`README.md`、`README.zh-CN.md`、`docs/en/architecture.md`、`docs/zh-CN/development.md`、`docs/zh-CN/design.md`、`CONTRIBUTING.md`、`CHANGELOG.md`、`AGENTS.md`

- [x] **Step 1：** 帮助描述由「Phase 2 工程骨架：…」改为「pre-alpha：当前没有邮件收发、任务执行或后台服务能力。」，规划命令的提示去掉阶段号；相应测试断言改为检查 `pre-alpha`。`go test ./cmd/... ./internal/cli/` 通过。
- [x] **Step 2：** README 中英文同步：状态说明（配置、存储、状态机已实现但尚未接入命令）、代码规则中的依赖说明、实际文件树（新增 `configs/`、`internal/task`、`internal/queue`、`internal/config`、`internal/store/sqlite`、`tests/integration`、`docs/zh-CN/plans/phase-03.md`），并用 `git ls-files --cached --others --exclude-standard | sort` 核对；两个 README 的 make 目标表格增加 `make modverify` 行，`make check` 行（`README.md`、`README.zh-CN.md` 各一处）改为 `fmt-check`、`vet`、`modverify`、`comments`、`test`、`lint`。
- [x] **Step 3：** `docs/en/architecture.md`：当前范围、包与依赖方向（`store/sqlite → task, queue`；`config`、`task`、`queue` 互不依赖，也不依赖存储）、持久化与恢复语义、依赖列表；「Quality gates and CI」一节开头列举 `make check` 子目标的句子补上 `modverify`，并加一条说明 `go mod verify` 校验 `go.sum` 哈希。
- [x] **Step 4：** `docs/zh-CN/development.md`：依赖政策与许可证记录、`modverify`（「常用 make 目标」表格新增 `make modverify` 行，并把 `make check` 行的子目标链补上 `modverify`）、配置文件与数据目录位置、SQLite 测试约定（临时目录、真实数据库、触发器注入故障）、集成测试目录、模糊测试的本地运行方式。
- [x] **Step 5：** `CONTRIBUTING.md` 依赖规则改为「新增依赖须在实施清单或 issue 中说明理由与许可证」，并把英文与中文两处「提交前检查」表格的 `make check` 行补上 `modverify`；`CHANGELOG.md` 在 `[Unreleased]` 增加 Phase 3 条目；`AGENTS.md` 当前范围改为「已实现配置、存储与状态机，尚未接入邮件与 Agent」；`design.md` 状态行更新。
- [x] **Step 6：** `git diff --check` 通过；在全部文档中检索本机路径与个人信息无结果；下面的断言无输出，确认没有文档漏掉 `check` 链的新子目标：

```sh
# 同时出现 fmt-check 与 lint 的行就是在枚举 check 链，这些行都必须含 modverify。
grep -rn fmt-check README.md README.zh-CN.md CONTRIBUTING.md \
  docs/en/architecture.md docs/zh-CN/development.md | grep lint | grep -v modverify
```
- [x] **Step 7：** `git commit -m "docs: document phase 3 configuration, storage and state machines"`

**实施说明：** Step 1 的测试除把帮助描述断言改为 `pre-alpha` 外，还断言规划命令与其他用法错误的提示不含 `Phase`，钉住「去掉阶段号」；改动前 `TestRun`、`TestHelpJSON` 与 `TestUsageFailures` 的 5 个规划命令子用例失败，改动后通过。清单之外的同步（均为让既有文字与实现一致，不涉及新功能）：Task 10 已把 `tests` 加入 `fmt`、`fmt-check` 的目录列表，但各步骤未点名描述这两个目标的文档，因此两个 README 的 make 目标表格、`CONTRIBUTING.md` 中英文两处、`development.md` 与 `architecture.md` 的目录列表一并补上 `tests`；`CONTRIBUTING.md` 中英文「项目阶段与范围」原写配置与 SQLite 存储不存在、当前清单为 phase-02，`design.md` 末段写「当前执行工程骨架」，`architecture.md`「Planned architecture」开头写本节均未实现，README「安全与隐私」写当前代码不读取配置，均改为与本阶段实际状态一致；`development.md`「常用 make 目标」的「是否联网」列中 `vet`、`test`、`check` 改为首次下载模块依赖时需要联网（在临时副本中用空模块缓存加 `GOPROXY=off` 验证：`go build ./cmd/turncourier` 与 `go run ./tools/commentcheck .` 可离线完成，`go vet` 与 `go mod verify` 需要下载模块）；`CHANGELOG.md` 在 Phase 3 条目之外新增 `### Changed` 一节，记录依赖规则、`fmt` 目录与帮助文案的变化。README 两份文件树按 `git ls-files --cached --others --exclude-standard` 逐项核对，结构一致，新增 `go.sum` 与 `internal/store/sqlite` 的全部测试文件。`go build` 后 `go version -m` 只列出本模块，确认产品二进制未链接两个新依赖。

**审查后修正：** ① Step 1 原测试只断言帮助描述含 `pre-alpha`，描述里重新出现阶段号（如「pre-alpha（Phase 3）：…」）时仍全部通过；现在 `TestHelpJSON` 断言描述等于 Step 1 给出的原文，`TestHelp` 的 4 个帮助入口断言输出含该原文且不含 `Phase`。在临时副本中，上述变异与「文本帮助的规划命令行加上 Phase 4」两处变异在旧测试下通过、新测试下失败。② `SECURITY.md` 不在本任务 Files 列表中，但其「Scope／报告范围」仍称当前代码只是 CLI 骨架，「Boundaries／规划设计中的边界」仍称配置与 SQLite 存储未实现，并让人把相关意见提交为公开的功能建议；现中英文同步改为：报告范围包括尚未接入命令的 `internal/config`、`internal/store/sqlite`、`internal/task`、`internal/queue`，并举例绕过配置与数据目录权限检查、重复入队或自动重发不确定回复；边界一节只把邮件、Agent、Keychain 与后台服务列为未实现，现有代码（含配置与存储包）的问题须走私密漏洞报告。③ 原说明中的离线验证没有覆盖 `lint`：在临时副本中以空模块缓存加 `GOPROXY=off` 运行 `staticcheck ./...`，退出码 1，报 `internal/config/config.go` 与 `internal/store/sqlite/store.go` 的 `module lookup disabled by GOPROXY=off`，因此 `development.md` 的 `make lint` 行改为「首次安装或首次下载模块依赖时」，`CONTRIBUTING.md` 中英文「开发环境」补充首次下载 Go 模块依赖也需要联网。④ 「只有 `main` 分支」在 Phase 3 开发期间不成立（远端同时存在 `feat/phase-03-storage`），因此 `CONTRIBUTING.md` 中英文、两个 README 的状态段与安全段、`architecture.md`「Current scope」、`development.md`「候选构建」共 8 处改为不随分支增删失效的说法：「默认分支为 `main`」，README 安全段改为请在 `main` 最新提交上复现，与 `SECURITY.md` 一致；清单与 Task 12 无需为此增加删除远端分支的步骤。修正后在 Phase 1–2 清单之外的 Markdown 中 `git grep`「只有 `main`」「only `main`」无结果；「skeleton」「工程骨架」的剩余命中（`AGENTS.md` 的 Phase 2 授权、`CHANGELOG.md` 的 Phase 2 条目、README 文件树中 `phase-02.md` 的注释、`design.md` 与 `architecture.md` 的状态行和开发顺序）都指 Phase 2 本身，不是对当前代码的描述。⑤ 复查发现 `SECURITY.md` 新增的权限检查示例没有写平台限定，而 `fileperm_other.go` 与 `perm_other.go` 在非 Unix 平台直接放行；中英文改为只有「拒绝凭据类键」跨平台，配置文件属主与写权限、数据目录与数据库权限检查限定「在 Unix 上」，与 `architecture.md`、`development.md` 的写法一致。

### Task 12：全量验证、审查与合并

- [ ] **Step 1：本地门槛。**

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
```

  期望：`make check` 通过且总覆盖率 ≥ 80%；gitleaks 与 govulncheck 无发现；actionlint 通过；`go version -m dist/turncourier` 的输出中没有 `modernc.org/sqlite` 与 `BurntSushi/toml`（本阶段命令行未接入新包，产品二进制不应变化）。
- [ ] **Step 2：审查。** 按 Phase 2 的做法，对状态机、配置校验、存储事务与恢复、测试有效性做独立审查；确认的问题修复后以变异测试证明测试能拦截回归。

  **审查后修正：** 配置（`internal/config`）部分。变异均在临时副本中逐项施加，每次只改一处。
  - CFG-1（major）：BurntSushi/toml v1.6.0 精确匹配失败时用 `strings.EqualFold` 把键匹配到字段并记为已解码，`ADDRESS`、`Allowed_Senders`、`[MAILBOX]` 不出现在 `Undecoded()` 中而被静默采用；同一字段写两个大小写变体时，采用哪个取值由 map 遍历顺序决定。现改为遍历 `MetaData.Keys()`，把每个键路径逐段区分大小写地与 `knownKeys` 白名单（13 条，已与 `rawConfig` 的 `toml` 标签逐一核对）比对，不用 `Key.String()` 比较；不在白名单中的键交给 `keyProblems`，凭据类键仍给 Keychain 提示，表数组重复列出的同一路径只报一次。键名有误时 `Load` 只返回键名错误、不再校验取值，否则错误文本会随采用的变体变化。D1 表格中「可通过 `MetaData.Undecoded()` 精确拒绝未知键」与 Task 5 Step 4 的实现说明保留原文，以本条为准；`development.md` 与 `architecture.md` 的依赖表和配置文件说明已改（键名区分大小写）。新测试 `TestLoadRejectsCaseVariantKeys` 在旧代码上失败（三种变体都加载成功）；`TestLoadRejectsDuplicateCaseVariants` 在旧代码上连跑 6 次全部失败，1 次为「Load 应当返回错误」（采用了合法值），5 次只报 `mailbox.address: invalid email address`（采用了变体的值）。变异：改回 `Undecoded()`、白名单改用 `EqualFold` 比较，两个测试都失败；去掉「键名有误时提前返回」，重复变体测试在第 3 次加载时报错误文本不一致；去掉去重，`TestLoadRejectsUnknownKey` 新增的表数组用例报同一键出现 2 次。
  - CFG-2 / TE-3（major）：新增 `fileperm_unix_test.go`（`//go:build unix`），`TestCheckFileOwner` 用只实现 `fs.FileInfo` 的替身覆盖：属主是其他用户、`Sys` 不是 `*syscall.Stat_t`、组可写、其他用户可写都被拒绝并断言拒绝原因，本人所有且 0600 通过。旧代码的这些行为本来正确，测试在旧代码上通过；变异「删掉 UID 比较」「`Sys` 类型不符时返回 nil」分别使对应用例失败。
  - CFG-3（minor）：`decodeFile` 改为先以 `O_RDONLY|openFlags` 打开（`openFlags` 在 `fileperm_unix.go` 为 `syscall.O_NONBLOCK`，在 `fileperm_other.go` 为 0），再由 `checkOpenedFile` 对已打开的文件 `Stat`，检查常规文件、大小与属主；`readConfigData` 用 `io.ReadAll(io.LimitReader(r, maxConfigBytes+1))` 读取，读到超过上界即报 too large；最后 `toml.Decode(string(data), &raw)`。新测试：`TestLoadRejectsFIFO`（5 秒超时保护）；`TestCheckOpenedFileUsesDescriptor`（打开 0622 文件后把路径换成 0600 文件，仍按已打开的文件拒绝）；`TestReadConfigDataRejectsOversize`（超过上界的普通文件被拒绝，恰好 1 MiB 通过）。后两者引用的函数在旧代码上不存在，编译失败；FIFO 用例在旧代码上通过，因为旧代码先 `os.Stat` 就拒绝了 FIFO，阻塞只会在 Stat 与 Open 之间路径被替换时发生。变异：`openFlags` 改为 0，FIFO 用例 5 秒超时失败；去掉长度检查，读到 1048577 字节且无错误；检查改为 `os.Stat(file.Name())`，描述符用例失败。
  - CFG-4（minor）：TOML 词法错误用 `%q` 回显出错位置的原文，未加引号的 `authorization_code = abcdefghijklmnop` 会让凭据进入错误文本。现用 `errors.As` 取 `toml.ParseError`：`LastKey` 是凭据类键时只返回「line N: <键> must not be set: 凭据将由 init 写入 Keychain」，否则只返回行号、列号、`invalid TOML` 与 `LastKey`（非空时）。类型错误（如 `imap_port = "abc"`）经核对只含键名与类型、不回显取值，保持原样并用例钉住。新测试 `TestLoadParseErrorsHideValues` 在旧代码上两个未加引号的用例失败（错误文本含原值）；变异「不转换、原样返回」与「去掉凭据分支」都使其失败。
  - CFG-5（nit）：`Load` 注释、包注释、`architecture.md` 的 `internal/config` 一行与 `CHANGELOG.md` 改为：文件、TOML 语法或类型错误单独返回；未知键与凭据类键合并返回，且不再校验取值（见 CFG-1）；解码成功后的校验错误通过 `errors.Join` 一次返回。除 CFG-1 带来的变化外不改行为。
  - CFG-6（minor）：未设置 `TURNCOURIER_CONFIG` 时，`userConfigDir()` 返回相对路径（HOME 为相对路径）即报「the user config directory must be an absolute path」，错误文本不含路径值。`TestResolvePathsErrors` 新增的用例在旧代码上失败（得到相对路径的 Paths）；去掉该检查的变异使其失败。
  - CFG-7（nit）：`isCredentialKey` 改为按键路径逐段转小写后比较。`TestLoadRejectsCredentialKeys` 新增的 `PASSWORD`、`Secret` 在旧代码上只报 unknown key、缺少 Keychain 提示；去掉 `strings.ToLower` 的变异使其失败。
  - TE-4（minor）：`TestLoadReadsOnlyNotifyEventsEnv` 用记录键名的 getenv 替身，对 `TURNCOURIER_ALLOWED_SENDERS`、`TURNCOURIER_TOKEN_TTL`、`TURNCOURIER_MAILBOX_ADDRESS` 等 6 个变量返回攻击值，断言只读取了 `TURNCOURIER_NOTIFY_EVENTS`，结果与不设环境变量时相同。旧代码上通过；变异「额外读取 `TURNCOURIER_TOKEN_TTL` 并覆盖」使其失败。
  - TE-5（minor）：`TestLoadFileProblems` 增加「父级是普通文件」用例（CFG-3 之后由打开返回 ENOTDIR），Unix 上增加 `TestLoadUnreadableParent`（父目录 0000，root 下跳过），都断言错误不含路径且不是 `ErrNotFound`。旧代码上通过；变异「`withoutPath` 原样返回」使两个用例都报错误文本含临时目录路径。
  - TE-7（nit）：`TestLoadDefaults` 修改返回的 `Events` 后重新加载，断言默认值不变。旧代码上通过；变异「直接返回 `defaultNotifyEvents`」使其失败。
  - README 中英文目录树补上 `fileperm_unix_test.go`。修正后 `make check`（总覆盖率 93.0%，`internal/config` 98.2%）、`GOOS=windows go vet ./...`、`GOOS=linux go vet ./...`、`CGO_ENABLED=0 go test -count=1 ./...`、`git diff --check` 均通过，`go test -race -count=20 ./internal/config/` 通过。

  **审查后修正：** 存储（`internal/store/sqlite`）、状态机（`internal/task`、`internal/queue`）与集成测试部分。变异均在临时副本中逐项施加，每次只改一处；「其余测试」指去掉该项新测试后，同一包与 `tests/integration` 的全部用例。
  - TE-1（major）：没有测试在有排队回复时执行 fail、close 以外的事件，把 `approval_requested` 等加进 `rejectReasons` 的变异能通过全部测试。新增 `TestNonTerminalEventsKeepQueuedReplies`（`replies_test.go`）：RUNNING 任务记录 3 条回复后依次执行 approval_requested、approval_resolved、input_requested，再经派发、标记不确定、核对为未送达、派发时即确认未发出、核对为已送达产生 reply_dispatched、delivery_unknown、reply_unsent（两条路径）、delivery_confirmed，最后 turn_completed；每一步都在推进过的时钟下执行，之后排在队首之后的 2 条回复与入队时的快照完全一致（QUEUED、`RejectReason` 为空、`UpdatedAt` 不变），最后核对完整事件序列。start 不在其中：CREATED 任务收到的回复直接记为 REJECTED，启动前不会有排队回复。旧代码的行为本来正确，测试在旧代码上通过。变异：`rejectReasons` 分别加入 approval_requested、approval_resolved、input_requested，新测试失败而其余测试全部通过（证实原缺口）；加入 turn_completed 时新测试与既有的 `TestClaimNextReplyFIFO` 等同时失败。
  - TE-2（minor）：新增 `TestTaskEventsRollBackOnFailure`（`tasks_test.go`），仿照 `TestReplyOperationsRollBackOnFailure`：`StartTask` 与 `ApplyTaskEvent` 的 turn_completed、close、fail 各自在任务更新失败（`RAISE(ABORT)`）、任务更新不命中（`RAISE(IGNORE)`，报 `changed during update`）、事件写入失败时，以及 close、fail 在拒绝排队回复失败时，共 14 个子用例，断言返回含触发器信息的错误，且任务快照、全部回复与 `task_events` 行数都与操作前一致；三个事件场景都带 2 条排队回复。测试在旧代码上通过。变异：`applyEvent` 丢弃 `writeTaskTransition` 的错误，12 个子用例失败（回复拒绝失败的 2 个不经过该调用）；丢弃拒绝排队回复的错误，close 与 fail 的回复拒绝失败 2 个子用例失败；两个变异下其余测试全部通过。
  - TE-6（minor）：`lifecycle_test.go` 打开存储后断言 `os.Stat(cfg.Paths.Database)` 成功且是常规文件；`store_test.go` 新增 `TestOpenDatabaseFileName`，以字面量 `"turncourier.db"` 断言 `Open` 创建的文件。变异：`store.go` 的文件名改为 `turncourier.sqlite`，`TestOpenDatabaseFileName` 与集成测试失败，其余 store 测试通过；`config/paths.go` 的文件名改为 `turncourier.sqlite`，集成测试失败（`config` 包的 `paths_test.go` 本就以字面量钉住这一侧）。两处改名在旧的集成测试下都能通过。
  - S2（minor）：两个进程首次同时打开同一个新数据目录时，空文件从回滚日志模式转为 WAL 的锁升级冲突不经 busy_timeout 等待，使一方的 `Open` 直接返回 SQLITE_BUSY。`openDB` 改为经 `retryWhileBusy` 调用 `PingContext`：返回 SQLITE_BUSY（`errors.As` 取 `*sqlite.Error`，`Code()&0xff` 等于 `SQLITE_BUSY`）时每 10ms 重试，期限 `openBusyTimeout` 为 5 秒，与 busy_timeout 一致；等待期间 ctx 结束时返回 `ctx.Err()`。为取得错误类型与结果码常量，`store.go` 对 `modernc.org/sqlite` 的空白导入改为具名导入，并导入同一模块的 `modernc.org/sqlite/lib`，依赖不变。新测试 `TestOpenConcurrentlyOnNewDirectory`：每轮 4 个存储实例同时 `Open` 同一个尚不存在的数据目录，共 30 轮，断言全部成功且 `SchemaVersion` 为 1。旧代码上连跑 10 次全部失败，1200 次 `Open` 失败 123 次（10.3%），300 轮中 86 轮出现失败，错误均为 `cannot open database: database is locked (5) (SQLITE_BUSY)`；修复后单次约 0.15 秒，`-count=30` 通过，`-race -count=3` 连跑 3 次通过。`TestRetryWhileBusy` 用真实的 SQLITE_BUSY 错误（存储持有 IMMEDIATE 事务时，另一个不做忙等待的连接开始写事务）驱动替身操作：忙两次后成功返回 nil 且调用 3 次；其他错误只调用 1 次；持续忙时 50ms 期限到后返回 SQLITE_BUSY；等待中上下文取消时返回 `context.Canceled`；`isBusy` 对 nil 与同文本的普通错误返回 false。变异：`openDB` 改回直接 `PingContext`、`isBusy` 恒为 false，并发测试失败，后者同时使 `TestRetryWhileBusy` 失败；忽略期限，持续忙用例在测试另设的 5 秒上下文期限处以 `context deadline exceeded` 失败；上下文结束时返回最后一次的 SQLITE_BUSY、等待时不检查上下文，取消用例失败（后者一直重试到 1 分钟期限）。`architecture.md`「Database」一段写明打开时重试 SQLITE_BUSY。
  - S3（minor）：`migrate.go`「事务内重新读取 user_version，多个进程同时打开也不会重复执行」原无测试，由 S2 的 `TestOpenConcurrentlyOnNewDirectory` 一并覆盖。变异「在 `BeginTx` 之前读取 user_version、事务内不再重读」下连跑 10 次全部失败，1200 次 `Open` 失败 220 次，300 轮中 88 轮出现失败，错误均为 `apply migration 0001: SQL logic error: table tasks already exists (1)`；其余测试全部通过，证实原缺口。因此不再另写专门的并发迁移测试。`architecture.md` 同一段写明版本在事务内重读。
  - SM-2（nit）：`internal/task/state_test.go` 与 `internal/queue/state_test.go` 新增 `TestNextUnknownEvent`：对每个已知状态调用 `Next(state, "bogus")`，断言 `errors.Is(err, ErrInvalidTransition)`，且错误文本含状态名与 `bogus`。测试在旧代码上通过；变异「已知状态收到转移表中没有的事件时返回原状态且无错误」在两个包中都使新测试失败，其余测试通过。两个包的覆盖率仍为 100%。
  - S1（minor，仅文档）：`RecoverInFlight` 把全部 DISPATCHING 回复改为 UNCERTAIN，隐含单一派发进程的前提，而 `ClaimNextReply` 的注释与跨进程并发测试表明派发支持多个进程。`RecoverInFlight` 的文档注释与 `architecture.md`「Recovery」写明：只能由唯一的派发进程在开始派发之前调用，调用期间不得有其他进程持有在途回复，否则另一个进程正在派发的回复会被误标为 UNCERTAIN；「风险与后续」增加单实例锁一条。代码行为不变。
  - S4（nit，仅文档）：`architecture.md` 新增「Durability」一段、「风险与后续」增加一条，写明 `synchronous=FULL` 只保证进程崩溃后的持久性，macOS 上断电或内核崩溃后不保证，并注明在后续安全与恢复阶段评估 `fullfsync`。已定的 SQLite 参数不变。
  - S5（nit，仅文档）：`Open` 在迁移检查之前已执行 `journal_mode(WAL)`，回滚日志模式的新版本数据库会被切换为 WAL，`architecture.md`「Database」原写的 refused unchanged 不准确，改为表结构与数据不变、但打开时文件可能已被切换为 WAL 模式。
  - SM-1（不修复）：任务已关闭或失败时，`RequeueUnsentReply` 与 `ResolveUncertainReply(false)` 不做派发后事件核对，直接把回复记为 REJECTED。这是 Task 9 实施说明写定的行为：事件核对只为防止放回队列后重复投递，关闭或失败的任务不会再派发，拒绝不会造成重复投递；REJECTED 回复保留 `resume_state`，记录它曾被派发。`TestUnsentReplyRejectedWhenTaskStopped` 钉住此行为，代码不改。
  - 修正后 `make check`（总覆盖率 93.3%，`internal/store/sqlite` 87.3%，`internal/task` 与 `internal/queue` 100%）、`GOOS=windows go vet ./...`、`GOOS=linux go vet ./...`、`CGO_ENABLED=0 go test -count=1 ./...`、`git diff --check` 均通过，`go test -race -count=3 ./internal/store/sqlite/ ./tests/...` 通过。
- [ ] **Step 3：提交 PR。** 推送 `feat/phase-03-storage` 分支，开 PR，等待 `quality (ubuntu-24.04)`、`quality (macos-15)`、`security` 三项检查通过；记录 CI 耗时，若 modernc 编译使单次质量任务超过 10 分钟，再单独评估启用 setup-go 缓存，不在本阶段预先修改。
- [ ] **Step 4：合并与记录。** squash 合并；在本文件末尾追加「验证记录」（本地、审查、远端分开记录，未运行的项写明原因），更新 `HANDOFF.md`。

## 完成标准

- 任务与队列状态机的全部状态 × 事件组合都有测试，覆盖率 100%。
- 配置：示例文件可加载；未知键、凭据类键、非法地址、自回环白名单、非法事件与期限、宽权限文件全部被拒绝；错误信息不含本机路径。
- 存储：迁移可重复执行且拒绝更新版本的数据库；去重与入队原子完成；同一任务同时最多一条在途回复（API 与数据库索引双重保证）；崩溃后在途回复进入 UNCERTAIN，永不自动重发；未送达回复按原序号重新排队。
- `CGO_ENABLED=0` 下全部测试通过；`make check`、`make security`、`make workflows`、`make build` 与远端三项检查通过。
- 文档与实际文件树一致；产品二进制不链接新依赖。

## 风险与后续

- modernc.org/sqlite 源码体积大（本机首次下载约 11 分钟），CI 冷启动下载与编译会变慢；以 Task 12 的实测耗时决定是否启用缓存。
- 接入命令行后二进制体积预计由约 4 MB 增至约 10 MB（见 D1 实测）；发布前需补齐链接模块的第三方许可声明。
- 地址统一转小写基于 QQ 邮箱不区分大小写的假设，邮件阶段须用真实样本复核。
- 正文加密与 Keychain 接入方式（D2）是邮件闭环阶段的前置任务，未完成前不得保存任何正文。
- `RecoverInFlight` 把全部 DISPATCHING 回复当作崩溃遗留，只能由唯一的派发进程在开始派发之前调用，调用期间不得有其他进程持有在途回复。接入后台服务时，单实例锁须覆盖 `RecoverInFlight` 与全部派发操作，这是邮件闭环阶段的前置条件。
- `synchronous=FULL` 只保证进程崩溃后已提交的事务不丢失。macOS 上 modernc 驱动只有设置 `PRAGMA fullfsync=1` 时才使用 `F_FULLFSYNC`，默认的 `fsync` 不保证数据写入持久存储，断电或内核崩溃后可能丢失最近提交的事务。本阶段不改已定参数，在后续安全与恢复阶段评估启用 `fullfsync`，需权衡写锁持有时间变长带来的争用。
