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

- [ ] **Step 1：写失败的测试。** 表格用例：

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
- [ ] **Step 2：** `go test ./internal/config/` 编译失败。
- [ ] **Step 3：** 按规则实现，只用标准库。
- [ ] **Step 4：** `go test -race ./internal/config/` 通过；`go test -run=^$ -fuzz=FuzzNormalizeAddress -fuzztime=30s ./internal/config/` 无失败（本地执行一次，CI 只运行种子）。
- [ ] **Step 5：** `git commit -m "feat(config): add strict email address normalization"`

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

- [ ] **Step 1：写失败的测试（`paths_test.go`）。**
  - 未设置环境变量、`userConfigDir` 返回 `/home/u/.config` → `ConfigFile=/home/u/.config/TurnCourier/turncourier.toml`，`DataDir=/home/u/.config/TurnCourier`，`Database=.../turncourier.db`；
  - `TURNCOURIER_CONFIG=/abs/c.toml`、`TURNCOURIER_DATA_DIR=/abs/data` 分别覆盖；
  - 两个变量为相对路径时报错；`userConfigDir` 报错且未设置变量时报错。
- [ ] **Step 2：写失败的测试（`config_test.go`）。** 用 `t.TempDir()` 写入 0600 文件后调用 `Load`：
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
- [ ] **Step 3：** `go test ./internal/config/` 失败。
- [ ] **Step 4：实现。** `go get github.com/BurntSushi/toml@v1.6.0`。读取时先用 `os.Stat` 检查常规文件与大小，再调用 `checkFileOwner(info)`（unix 版本比较 `Stat_t.Uid == os.Getuid()` 并检查 `mode&0o022 == 0`；other 版本直接返回 nil），然后 `toml.NewDecoder(io.LimitReader(f, 1<<20+1)).Decode(&raw)`，读取 `md.Undecoded()`。`raw` 结构使用 `toml` 标签，端口用 `*int` 区分未设置与 0。
- [ ] **Step 5：** `go test -race ./internal/config/` 通过，覆盖率 ≥ 90%；`GOOS=windows go vet ./internal/config/` 通过；`make comments` 通过。
- [ ] **Step 6：** `git add configs internal/config go.mod go.sum && git commit -m "feat(config): load and validate TOML configuration"`

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

- [ ] **Step 1：写失败的测试（`migrate_test.go`）。** 迁移逻辑拆成 `migrate(ctx, db *sql.DB, fsys fs.FS) error` 以便注入：
  - 空库迁移后 `user_version = 1`，四张表与两个索引存在（查询 `sqlite_schema`）；
  - 重复打开不重复执行，版本仍为 1；
  - 手动设置 `PRAGMA user_version = 99` 后 `Open` 返回 `ErrSchemaTooNew`，数据未改动；
  - 注入 `fstest.MapFS{"0001_init.sql": 合法SQL, "0002_bad.sql": "CREATE TABLE broken ("}`：返回错误，`user_version` 停在 1，不存在 `broken` 表；
  - 文件名不是 `四位数字_名称.sql`，或编号不连续（`0001`、`0003`）时报错。
- [ ] **Step 2：写失败的测试（`store_test.go`）。**
  - `PRAGMA journal_mode` 为 `wal`，`PRAGMA foreign_keys` 为 1，`PRAGMA synchronous` 为 2（FULL）；
  - 目录名含空格、`#`、中文时可以打开并完成读写；
  - 相对路径报错；数据库文件内容为随机字节时报错；
  - 仅 Unix：新建目录的权限为 0700、文件为 0600；已有目录权限 0755 或文件权限 0644 时拒绝；
  - 错误文本不包含临时目录路径；
  - STRICT 生效：直接执行 `INSERT INTO tasks (..., version, ...) VALUES (..., 'x', ...)` 失败。
- [ ] **Step 3：** 测试失败。
- [ ] **Step 4：实现。** `go get modernc.org/sqlite@v1.59.0`；用 `//go:embed migrations/*.sql` 嵌入迁移；每个待应用迁移在一个事务中执行 SQL 与 `PRAGMA user_version = N`。
- [ ] **Step 5：** `go test -race ./internal/store/sqlite/` 通过；`make modverify` 通过；`GOOS=windows go vet ./internal/store/sqlite/` 通过；`CGO_ENABLED=0 go test ./internal/store/sqlite/` 通过（证明不依赖 CGO）。
- [ ] **Step 6：** `git commit -m "feat(store): open SQLite database with embedded migrations"`

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

- [ ] **Step 1：写失败的测试。**
  - `newTaskID`：固定输入 `bytes.NewReader([]byte{0,0,0,0,0,0,0})` 得到 `0000000000`，全 `0xff` 得到 `zzzzzzzzzz`；读取不足 7 字节时报错；1000 次真实随机生成的结果都匹配 `^[0-9abcdefghjkmnpqrstvwxyz]{10}$`；
  - `CreateTask`：返回 CREATED、Version 1、Owner `local`，时间来自注入时钟；未知 agent 报错；随机源连续 3 次给出与已有任务相同的 ID 时报错，第 2 次给出新 ID 时成功；
  - `GetTask` 不存在时 `ErrNotFound`；
  - `StartTask`：成功后为 RUNNING、Version 2、SessionID 已保存；版本过期返回 `ErrVersionConflict`；会话 ID 为空、201 字符、含空格或换行返回 `ErrInvalidSessionID`；对 RUNNING 任务再次调用返回 `task.ErrInvalidTransition`；
  - `ApplyTaskEvent`：六个允许事件各至少一条成功路径；四个保留事件返回 `task.ErrInvalidTransition`；不存在的任务返回 `ErrNotFound`；错误版本返回 `ErrVersionConflict`，且数据库未改动；
  - `TaskEvents`：CREATED→RUNNING→COMPLETED→CLOSED 依次记录 from/to/event，按 seq 升序；
  - fail 与 close 拒绝排队回复的行为在 Task 8 数据就绪后补测（写入 Task 8 Step 1）。
- [ ] **Step 2：** 测试失败。
- [ ] **Step 3：实现。** 所有写操作使用 `BeginTx`（连接串已设置 IMMEDIATE）。更新语句形如 `UPDATE tasks SET state=?, version=version+1, updated_at=? WHERE id=? AND version=?`，受影响行数为 0 时再查一次任务，区分 `ErrNotFound` 与 `ErrVersionConflict`。读取时校验 `task.State(...).Valid()`，非法值返回错误，不静默接受。
- [ ] **Step 4：** `go test -race ./internal/store/sqlite/` 通过。
- [ ] **Step 5：** `git commit -m "feat(store): persist tasks with optimistic versioning"`

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

- [ ] **Step 1：写失败的测试。** 以 RUNNING 任务 `T`、账户 `bot@example.invalid` 为基础：

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
- [ ] **Step 2：** 测试失败。
- [ ] **Step 3：实现。** 事务内顺序：读取任务 → 按 `(account, uid_validity, uid)` 查找 → 按 `(account, message_id)` 查找 → 判定重复或冲突 → 插入 `inbound_messages` → 插入 `replies`。存储层不导入 `config`，只做长度与空白检查，地址规范化由调用方负责，以保持 `store/sqlite` 只依赖 `task` 与 `queue`。
- [ ] **Step 4：** `go test -race ./internal/store/sqlite/` 通过。
- [ ] **Step 5：** `git commit -m "feat(store): deduplicate and enqueue inbound replies atomically"`

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

- [ ] **Step 1：写失败的测试。**
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
- [ ] **Step 2：** 测试失败。
- [ ] **Step 3：实现。** 每个方法一个事务：读取回复与任务 → 用 `queue.Next` 与 `task.Next`/`task.ResumeAfterUnsent` 计算新状态 → 以 `WHERE seq=? AND state=?`、`WHERE id=? AND version=?` 条件更新 → 写任务事件。任何一步受影响行数不为 1 时回滚并返回对应错误。
- [ ] **Step 4：** `go test -race -count=3 ./internal/store/sqlite/` 通过，无抖动；包覆盖率 ≥ 85%。
- [ ] **Step 5：** `git commit -m "feat(store): dispatch, acknowledge and recover replies without blind resend"`

### Task 10：集成测试

**Files:**
- Create: `tests/integration/lifecycle_test.go`
- Modify: `Makefile`（`fmt`、`fmt-check` 目录加入 `tests`）

- [ ] **Step 1：** 修改 Makefile 目录列表；`make fmt-check` 通过。
- [ ] **Step 2：写测试。** 包名 `integration_test`，文件开头写中文包注释。`TestReplyLifecycle`：
  1. 把 `configs/turncourier.example.toml` 复制到临时目录并设为 0600；设置 `TURNCOURIER_CONFIG` 与 `TURNCOURIER_DATA_DIR`（均在临时目录内）；`config.ResolvePaths` + `config.Load` 成功；
  2. `sqlite.Open(ctx, cfg.Paths.DataDir, sqlite.Options{})`；创建 codex 任务，`StartTask` 会话 ID `thread-synthetic-0001`，执行 `turn_completed`；
  3. 以 `cfg.Mailbox.Address` 为账户，用 `sha256.Sum256` 计算合成正文 `"first synthetic reply"` 与 `"second synthetic reply"` 的摘要，记录两条回复；
  4. 派发第 1 条 → `MarkReplyUncertain` → 关闭存储 → 重新打开 → `RecoverInFlight` 返回 1 条 → `ResolveUncertainReply(true)` → `turn_completed`；
  5. 派发得到第 2 条 → `AcknowledgeReply` → `close`；
  6. 再次记录第 1 条回复为 `Duplicate=true`；记录第 3 条新回复为 REJECTED(`task_closed`)；
  7. 断言完整任务事件序列：`start, turn_completed, reply_dispatched, delivery_unknown, delivery_confirmed, turn_completed, reply_dispatched, close`。
- [ ] **Step 3：** `go test -race ./tests/...` 通过；`make comments` 通过（确认测试专用包的中文包注释被识别）。
- [ ] **Step 4：** `git commit -m "test: cover config, store and state machine lifecycle end to end"`

### Task 11：命令行文案与文档同步

**Files:**
- Modify: `internal/cli/cli.go`、`internal/cli/cli_test.go`、`cmd/turncourier/main_test.go`、`README.md`、`README.zh-CN.md`、`docs/en/architecture.md`、`docs/zh-CN/development.md`、`docs/zh-CN/design.md`、`CONTRIBUTING.md`、`CHANGELOG.md`、`AGENTS.md`

- [ ] **Step 1：** 帮助描述由「Phase 2 工程骨架：…」改为「pre-alpha：当前没有邮件收发、任务执行或后台服务能力。」，规划命令的提示去掉阶段号；相应测试断言改为检查 `pre-alpha`。`go test ./cmd/... ./internal/cli/` 通过。
- [ ] **Step 2：** README 中英文同步：状态说明（配置、存储、状态机已实现但尚未接入命令）、代码规则中的依赖说明、实际文件树（新增 `configs/`、`internal/task`、`internal/queue`、`internal/config`、`internal/store/sqlite`、`tests/integration`、`docs/zh-CN/plans/phase-03.md`），并用 `git ls-files --cached --others --exclude-standard | sort` 核对；两个 README 的 make 目标表格增加 `make modverify` 行，`make check` 行（`README.md`、`README.zh-CN.md` 各一处）改为 `fmt-check`、`vet`、`modverify`、`comments`、`test`、`lint`。
- [ ] **Step 3：** `docs/en/architecture.md`：当前范围、包与依赖方向（`store/sqlite → task, queue`；`config`、`task`、`queue` 互不依赖，也不依赖存储）、持久化与恢复语义、依赖列表；「Quality gates and CI」一节开头列举 `make check` 子目标的句子补上 `modverify`，并加一条说明 `go mod verify` 校验 `go.sum` 哈希。
- [ ] **Step 4：** `docs/zh-CN/development.md`：依赖政策与许可证记录、`modverify`（「常用 make 目标」表格新增 `make modverify` 行，并把 `make check` 行的子目标链补上 `modverify`）、配置文件与数据目录位置、SQLite 测试约定（临时目录、真实数据库、触发器注入故障）、集成测试目录、模糊测试的本地运行方式。
- [ ] **Step 5：** `CONTRIBUTING.md` 依赖规则改为「新增依赖须在实施清单或 issue 中说明理由与许可证」，并把英文与中文两处「提交前检查」表格的 `make check` 行补上 `modverify`；`CHANGELOG.md` 在 `[Unreleased]` 增加 Phase 3 条目；`AGENTS.md` 当前范围改为「已实现配置、存储与状态机，尚未接入邮件与 Agent」；`design.md` 状态行更新。
- [ ] **Step 6：** `git diff --check` 通过；在全部文档中检索本机路径与个人信息无结果；下面的断言无输出，确认没有文档漏掉 `check` 链的新子目标：

```sh
# 同时出现 fmt-check 与 lint 的行就是在枚举 check 链，这些行都必须含 modverify。
grep -rn fmt-check README.md README.zh-CN.md CONTRIBUTING.md \
  docs/en/architecture.md docs/zh-CN/development.md | grep lint | grep -v modverify
```
- [ ] **Step 7：** `git commit -m "docs: document phase 3 configuration, storage and state machines"`

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
