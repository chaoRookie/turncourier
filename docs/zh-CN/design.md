# TurnCourier v0.1.0-alpha 设计

状态：设计和 Phase 0–1 已批准并完成核心验证；Phase 2 工程骨架（help、version、doctor 与质量门槛）已实现并公开，远端 CI 结果见 `plans/phase-02.md`；Phase 3 的配置、存储与状态机已作为内部包实现并通过测试，尚未接入命令行，范围与验证见 `plans/phase-03.md`。接口验证中的细化记录在 `research/phase-01.md`，不得把候选能力写成已实现功能。

## 产品与阶段

TurnCourier — Email bridge for Codex and Claude Code。

本机程序启动 Agent 任务，按配置将完成、等待输入、错误及审批状态发送到独立的 QQ 机器人邮箱所服务的个人收件邮箱。合法邮件回复成为原会话的下一条用户消息。首版单用户，数据模型预留 owner；支持自建 CLI 会话，桌面兼容只作为实验项目。首发 macOS，核心保持跨平台。Go 单模块、后台程序加 CLI；稳定后增加 launchd 安装和 macOS 菜单栏。

当前授权：Phase 0 保存设计和评估竞品；Phase 1 验证原生接口；Phase 2 建立并公开工程骨架。完整邮件实现和正式发布属于后续阶段。先在个人 GitHub 账户建立公开仓库，成熟后可迁移组织。首次公开前扫描密钥；Apache-2.0；中英双语 README，英文标识符，必要的中文代码注释。

## 已确定的产品边界

- 一个任务对应一个邮件线程。主 Agent 汇总子 Agent 状态，不为子 Agent 建立邮件线程。
- 全部事件可配置；默认通知等待输入、失败和回合完成。完成表示本回合结束，任务仍可继续。
- 第一版只将自然语言回复送入 Agent，不通过邮件批准工具权限，不实现邮件重试、停止等控制命令。
- Agent 忙时按本地接收顺序排队，连续回复全部保留，重复邮件不重复入队。
- 专用 QQ 发件账户，通过 SMTP 发送、IMAP 收信；优先 IDLE，断线重连并补扫。
- 解析分隔线以上的新正文，去引用与签名；解析不确定时不执行。附件不作为指令输入。
- 使用 Agent 原始回复，不增加模型摘要调用；文件名、测试结果必须来自可核实事件，否则标记未采集。
- 默认不附加补丁；用户显式开启后才允许发送补丁和报告。
- 首次交互式 init 配置邮箱、Keychain、环境诊断及真实往返测试。真实测试须用户在本地提供邮箱授权码。
- 遥测默认关闭且需主动开启，禁止上传正文、代码、路径、仓库地址、邮箱与身份信息。首版未建设收集服务时不发送遥测。

## 模块职责

CLI 负责用户交互；app 装配依赖与生命周期；task 维护状态；Agent Adapter 管理原始会话；mail 负责收发、解析和渲染；queue 负责持久化调度；security 负责身份与令牌；store 负责 SQLite；worktree 管理工作副本。

```text
CLI → Task Manager → Agent Adapter → Codex / Claude Code
          │               │
          │            结构化事件
          ▼               ▼
       SQLite ← Queue ← Event Policy → SMTP → 用户邮箱
          ▲       │                             │
          │       └─ Agent 空闲时投递             │ 回复
          └──── 验证与正文解析 ← IMAP ←───────────┘
```

Codex 优先自管 app-server JSON-RPC；Claude 使用机器可读 CLI 会话接口，Hooks 辅助事件采集。避免依赖模拟按键。首次启动保存 Agent session ID，所有后续操作显式指定 ID；禁止以“最近会话”定位。Adapter 必须区分自然语言问题、工具审批、回合结束和故障。

Phase 1 确认的首版实现路径是“回合结束后发送原消息，回复开启同会话新回合”。结构化阻塞问题与工具审批需要额外区分，不能见到 `requestUserInput` 就自动答复：Codex 的连接器审批也可能使用这一入口。Claude headless 无交互模式会移除 AskUserQuestion，因此普通提问采用文本回复后结束回合；不能把交互终端中的所有 Hooks 行为直接视为 headless 保证。

## 安全与持久化细化

QQ 授权码和签名密钥存 macOS Keychain，普通 TOML 配置只保存账户与选项；环境变量只临时覆盖非敏感配置。发件人规范化后精确匹配白名单。邮件头线程引用、短任务 ID、带签名令牌必须一致；缺失或冲突则拒绝，不能降级成仅按主题执行。默认七天有效，可配置，任务关闭立即撤销。

签名用于完整性验证，不等于邮件内容加密；令牌绑定任务、用户、有效期、通知 ID。邮件客户端可能不保留自定义头，因此令牌载体必须用真实 QQ 客户端验证。发件人头可以伪造，不能单独作为身份凭据；令牌属于持有者凭证，不可出现在公开日志中。

Message-ID 去重之外，收取记录还需要邮箱账户、UIDVALIDITY、UID 和正文摘要。SQLite 唯一约束与入队必须同一事务。相同 Message-ID 但正文冲突应拒绝并告警。邮件自动回复与退信不可触发 Agent。忙碌任务的 FIFO 顺序以本地入队序号定义，不能信任发件人提供的 Date。

默认仅保留元数据历史，但未投递回复、待发邮件在队列中必须暂存正文，否则无法恢复。待处理载荷加密落盘，密钥存 Keychain，完成后清理；用户可主动开启历史记录。此为“元数据默认”的运行队列例外，必须向用户说明。

Agent 接收与 SQLite 提交无法天然形成一个事务。发生“已投递但确认丢失”时进入待核对状态；未证明 Agent 端幂等前禁止盲目重发，不承诺端到端 exactly-once。SMTP 也可能出现服务端接收但本地未获确认，使用稳定 Message-ID，保留重复通知可能性。

Git worktree 隔离代码变更，不是操作系统安全沙箱。自然语言也可以要求 Agent 执行高风险动作，因此必须保留 Agent 原生沙箱和权限控制。禁止由邮件设置绕过审批参数。审批事件单独分类并通知本地处理。

原始模型回复可能包含代码或绝对路径，因此“原样发回复”和“默认不发源码路径”存在冲突。默认发送前做确定性敏感信息过滤、代码块与长内容裁剪；邮件注明内容省略并给出本地报告位置的相对描述。过滤不能承诺发现所有机密，应允许纯状态通知。不开启附件时不发送 diff。

## 状态与恢复

```text
CREATED → RUNNING → COMPLETED → RUNNING（下一封合法回复）
             ├── WAITING_INPUT → RUNNING（回答指定问题）
             ├── WAITING_APPROVAL（邮件无权批准）
             └── FAILED（保留诊断；首版不由邮件重试）
任意非 CLOSED 状态 → CLOSED（本地关闭或到期）
投递确认不确定 → DELIVERY_UNCERTAIN（本地核对后恢复）
```

队列状态独立于任务状态：QUEUED、DISPATCHING、ACKNOWLEDGED、UNCERTAIN、REJECTED。不能用 REPLY_QUEUED 覆盖 RUNNING，否则会错误投递并行消息。守护进程恢复时先核对会话状态，再恢复出队。

## 目标目录树

下列是目标结构，按实现阶段创建，不建立空壳目录。

```text
turncourier/
├── cmd/turncourier/main.go           # 产品入口
├── internal/
│   ├── app/                         # 依赖装配和生命周期
│   ├── cli/                         # init、doctor、run、tasks、logs、service
│   ├── agent/
│   │   ├── adapter.go               # 会话与事件契约
│   │   ├── codex/                   # app-server 适配
│   │   └── claude/                  # headless CLI 与 Hooks
│   ├── task/                        # 状态机与任务归属
│   ├── queue/                       # 回复和待发邮件调度
│   ├── mail/
│   │   ├── gateway.go               # 邮件契约
│   │   ├── smtp/                    # 发信与重试
│   │   ├── imap/                    # 收信与重连
│   │   ├── parser/                  # MIME、引用、签名解析
│   │   └── renderer/                # 纯文本/HTML 模板
│   ├── security/
│   │   ├── token/                   # 签名、撤销、有效期
│   │   ├── replay/                  # 去重与重放检查
│   │   └── keychain/                # 系统凭据存储
│   ├── config/                      # TOML 及临时覆盖
│   ├── store/sqlite/                # 数据迁移与事务
│   ├── worktree/                    # 工作副本生命周期
│   ├── logging/                     # 脱敏日志
│   ├── telemetry/                   # 主动开启的匿名指标
│   └── platform/darwin/             # macOS 集成
├── tools/commentcheck/              # 中文函数注释检查
├── tools/covercheck/                # 全部手写代码的覆盖率门槛
├── configs/turncourier.example.toml
├── docs/
│   ├── zh-CN/
│   │   ├── design.md                # 本设计
│   │   ├── development.md           # 开发指南
│   │   ├── plans/                   # 各阶段实施清单与验证记录
│   │   └── research/phase-01.md      # Phase 0–1 验证报告
│   └── en/                          # 英文架构文档
├── experiments/phase01/              # 可复现的接口研究脚本，非产品实现
├── tests/
│   ├── integration/                 # 模拟邮件与存储
│   ├── e2e/                         # 完整闭环
│   ├── live/                        # 人工授权的真实验收
│   └── fixtures/                    # 合成、脱敏样本
├── scripts/                         # 验收、发布检查
├── .github/                         # CI、Issue 与 PR 模板
├── README.md                        # 英文入口
├── README.zh-CN.md                   # 中文入口
├── CONTRIBUTING.md
├── SECURITY.md
├── CODE_OF_CONDUCT.md
├── CHANGELOG.md
├── LICENSE                          # Apache-2.0
├── go.mod
└── .gitignore
```

## 工程规范与验收

所有手写函数（含未导出函数）、模块、接口、结构体需必要中文注释。导出注释以英文标识符开头；复杂函数说明错误、并发和安全边界；生成文件与第三方源码除外。单元测试随包放置，跨模块测试放 tests；不为目录美观引入动态插件或微服务。

CI 要求 gofmt、go vet、静态检查、单元/集成测试、race、依赖漏洞扫描、密钥扫描、中文函数注释检查、覆盖率至少 80%。覆盖率范围应明确为手写产品代码，不能通过排除核心模块达标。真实 QQ 凭据不进入公共 CI。

发布前，Codex 和 Claude 各连续十轮邮件交互，覆盖断网恢复、重复/伪造/过期邮件、忙时 FIFO、隐私过滤。发布 v0.1.0-alpha macOS 二进制与 SHA-256；跨平台包和 Homebrew 随后推进。代码编译成功不能替代真实验收。

开发顺序：竞品与接口研究 → 公开工程骨架 → 配置/存储/状态机 → 邮件闭环 → Codex → Claude → 安全与恢复 → 真实验收 → 发布。当前处于配置/存储/状态机阶段：相关内部包已实现并通过测试，尚未接入命令行。
