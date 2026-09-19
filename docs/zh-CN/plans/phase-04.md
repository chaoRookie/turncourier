# TurnCourier Phase 4 实施清单：邮件闭环

> **执行说明：** 本阶段分为 4a（离线实现）、L1（维护者本机真机探测）与 4b（依据 L1 结果实现）。本清单给出 4a 的完整任务契约；4b 的任务在 L1 结果写入本清单后细化。4a 按任务顺序执行：每个任务先写失败的测试，再做最小实现，通过后提交。每个任务由独立子代理实现，再由规格与质量两名审查子代理复核（含变异测试），确认的问题修复并经独立复查后才进入下一任务；阶段末做整阶段审查（Task 15）。实现在 `feat/phase-04a-mail` 分支进行。步骤使用 `- [ ]` 勾选跟踪。
>
> **状态：** 第 0 节 D1–D5 已于 2026-09-19 确认；4a 任务契约已通过两轮审查，独立复查留下的问题已修正。`inbound_messages` 增加文件夹列一事已于同日确认采用方案 (a)，并入 Task 6（记录见末尾「待维护者确认」一节的「已确认事项」）。目前没有待维护者确认的事项，4a 可以按任务顺序开始实现。

**Goal:** 让 TurnCourier 能用专用 QQ 机器人邮箱安全地发出任务通知、收取并验证用户回复，把合法回复的新正文加密存入回复队列。本阶段结束时，用一个不调用模型的合成 Agent 完成「通知 → 回复 → 入队 → 派发」的完整闭环，并由维护者在本机用真实邮箱完成往返验收。Codex 与 Claude 适配器分别属于后续两个阶段。

**4a 的交付：** Keychain 封装、`turncourier init`、回复令牌、正文加密、迁移 `0002`（入站记录的文件夹列、实例、密钥元数据、待发通知、正文密文、收取游标、被拒来信）、待发通知状态机与存储、SMTP 与 IMAP 客户端及离线假服务器、只在 `tests/live` 中人工执行的 L1 探测工具，以及文档同步。4a 不渲染通知、不解析回复、不实现入站验证流水线，也不冻结线程绑定规则（D4），这些属于 4b。

**Architecture:** 邮件相关代码按设计目录放在 `internal/mail`（4a 只建 `smtp`、`imap`）与 `internal/security`（`keychain`、`token`、`payload`）。存储经新迁移 `0002` 扩展，已发布的 `0001_init.sql` 不改。所有网络与 Keychain 访问都藏在小接口之后，CI 与单元测试只用离线替身，真实邮箱与 Keychain 只出现在人工执行的 `tests/live`。

**Tech Stack:** Go 1.27.1；标准库 `crypto/hmac`、`crypto/aes`、`crypto/cipher`、`crypto/tls`、`encoding/base32`、`encoding/base64`、`os/exec`；第 0 节 D2 列出的新增模块。

---

## 0. 开工前须确认的决策

**2026-09-19 维护者已确认 D1–D5，均采用本节的建议方案。** D4 只确认方向，细则在 L1 之后定稿；届时任何放宽都要另行确认。

调研于 2026-09-19 完成，分四路：邮件库、Keychain、QQ 邮箱行为、令牌与加密，另由一名审稿人抽查并交叉核对。调研全程没有登录邮箱、没有发信，也没有读写任何钥匙串。证据类型标注为：本地实验、源码、官方文档、第三方资料、推断。

### D1：阶段拆分与真机门槛

**问题。** 两项关键行为离线无法确认，而线程规则和解析器都建立在它们之上：

- **Message-ID 可能被改写。** 公开邮件列表中近期 49 封 qq.com 来信，有 48 封的 Message-ID 是 `tencent_…@qq.com`。其中 21 封把客户端原来的 ID 放进了 `X-OQ-MSGID` 头，且都经 `newxmesmtplogicsvr*.qq.com` 发出，本机探测连到的 `smtp.qq.com` 正是这一族服务器；2017–2019 年经旧服务器发出的同类邮件则保留了原 ID。证据类型：第三方资料，由审稿人独立复核。若确认改写，用户回复的 `In-Reply-To` 将指向 QQ 分配的 ID，而不是我方 ID。
- **各客户端的线程头与引用格式不统一。** 样本中：
  - QQ 自家客户端约 5%–11% 的回复没有任何线程头；
  - Foxmail 从不带 `In-Reply-To`，Mac 版两种线程头都不带；
  - QQ App 可以关闭「带上原文」，回复里可能完全没有引用。

**建议。** 本阶段拆成三段：

1. **4a（离线）：** Keychain、`init` 录入凭据、令牌、正文加密、迁移 `0002`、SMTP/IMAP 客户端与离线假服务器、只读的真机探测工具。
2. **L1（真机探测，需维护者在本机操作）：**
   - 准备一个专用的 QQ 机器人邮箱并开启授权码；
   - 在本机运行 `turncourier init`，授权码以不回显方式录入 Keychain，不经过聊天或 Git；
   - 授权探测工具向你自己的邮箱发送若干封合成通知，再用下文列出的客户端逐一回复。

   探测工具只记录邮件头结构与格式特征，不保存正文与个人信息。脱敏后的样本转为合成回归样本。
3. **4b（依据 L1 结果）：** 冻结线程绑定规则与引用解析器，实现入站验证流水线、通知渲染与敏感信息过滤、合成 Agent 闭环与真实往返验收。

**不采用：** 不拆分、先把解析器和线程规则做完再真机验证。这些规则很可能要推翻重写，而且错误的规则会拒收所有合法回复。

### D2：新增依赖

依赖政策沿用 Phase 3 的 D1：只接受宽松许可证，版本固定在 `go.mod` 与 `go.sum`，`make check` 执行 `go mod verify`。下表模块均已在本机临时模块中以 `CGO_ENABLED=0` 编译 darwin/arm64、darwin/amd64、linux/amd64；x/term 例外，它的版本与许可证只查了模块代理和官方仓库，编译验证留到 Task 1。

| 模块 | 版本 | 许可证 | 用途与结论 |
| --- | --- | --- | --- |
| `github.com/emersion/go-imap/v2` | v2.0.0-beta.8 | MIT | **推荐。** 产品只链接 `imapclient`；同一模块的 `imapserver`、`imapmemserver` 只在测试中用作离线假服务器。支持 IDLE、UIDVALIDITY/UID、UIDPLUS、ID。仍是 beta，各 beta 之间有破坏性 API 变更，因此固定精确版本，封装在 `internal/mail/imap` 之后，升级 PR 须人工审阅 |
| `github.com/emersion/go-message` | v0.18.2 | MIT | **推荐。** MIME 解析与生成，须同时空导入 `charset`。实测能解码 GB18030 quoted-printable 正文、GBK/GB18030 编码的主题与显示名、base64 附件，并提供 `In-Reply-To`/`References` 列表解析 |
| `golang.org/x/text` | v0.42.0 | BSD-3-Clause | **推荐，须显式固定。** go-message 要求的 v0.14.0 有模块级漏洞 GO-2026-5970（修复于 v0.39.0）；升到 v0.42.0 后 govulncheck 无发现 |
| `github.com/emersion/go-sasl` | 伪版本 `b788ff2` | MIT | 由 go-imap/v2 间接引入，不单独使用 |
| `github.com/emersion/go-smtp` | v0.25.0 | MIT | **推荐用于发信。** 自带命令超时；`CloseWithResponse` 能取回服务端 DATA 响应文本，L1 需要它来查找 QQ 分配的 ID；同一模块的服务端可作离线假 SMTP 服务器。它**不会阻止**在明文连接上发送凭据，因此只允许 `DialTLS`，并由测试钉住 |
| `golang.org/x/term` | v0.46.0 | BSD-3-Clause | **推荐。** `init` 以不回显方式读入授权码。需把 `golang.org/x/sys` 从 v0.47.0 升到 v0.48.0 |

- **体积：** 接入命令行后，产品二进制预计约 16.5 MB（arm64，含 Phase 3 的存储与配置）；目前约 4 MB。发布前补齐链接模块的第三方许可声明（Phase 3 风险项）。
- **不采用：**
  - 标准库 `net/smtp`：已冻结，没有内置超时，并丢弃 DATA 响应；
  - go-imap v1：已停更，UIDPLUS 与 ID 要另引多年未更新的扩展；
  - 只用标准库解析 MIME：仍离不开 x/text，还要自行补齐 base64、RFC 2047 参数和 msg-id 列表解析；
  - `keybase/go-keychain`、`99designs/keyring`：需要 CGO，后者多年未发版；
  - `zalando/go-keyring`：macOS 上本质也是调用 `/usr/bin/security`，自写更可控；将来需要 Linux 或 Windows 后端时再评估。
- **库缺陷，由产品自行兜底（本地实验已复现）：**
  - QQ 对不支持的命令只回无标签的 `* BAD`，go-imap/v2 会让该命令永久阻塞。因此每条命令都设期限，超时即关闭连接；只发 CAPABILITY 公告过的命令；离线假服务器必须能模拟这种行为，`imapmemserver` 做不到。
  - 半开连接下，IDLE 与 `Wait` 会一直阻塞，而且库不会自动重连。因此由产品实现看门狗与退避重连。
  - 按 UID 补扫在没有新邮件时仍会返回最后一个 UID，需要再过滤 `uid > last`。
  - 标注为 gbk/gb2312 的邮件可能含 GB18030 四字节字符，这些标签要统一映射到 GB18030 解码器。
  - 两个库的调试输出会写出凭据，产品中禁止开启。

### D3：Keychain 接入方式与威胁模型

**建议实现：** 在 `internal/security/keychain` 中自写 `/usr/bin/security` 的薄封装。只用标准库，不引入 CGO。

- **写入：** 每次启动一个 `security -i` 进程，只经标准输入发一条 `add-generic-password -U -s … -a … -X <十六进制>`，写后读回核对。不用 `-w <值>`、`-p`、`-v`、`-A`：进程参数对本机其他进程可见，`-v` 会把参数打印到 stderr，`-A` 允许任何应用无警告访问。
- **读取：** `find-generic-password -w`，只读标准输出。退出码 44 表示条目不存在，不匹配错误文案。
- **删除：** 提供删除接口，供密钥轮换与卸载使用。
- **其他平台：** 返回「不支持」，不提供明文文件后备。
- **测试：** 单元测试让测试二进制扮演假的 `security` 程序，断言机密不进入参数列表。CI 不接触真实钥匙串，真实钥匙串只在 `tests/live` 中由人工执行。

**威胁模型须修订（需确认）。** 经 `security` 创建的条目，受信任应用是 `/usr/bin/security`，分区为 `apple-tool:`（源码与官方文档）。因此：

- **会读到：** 同一用户的任何进程都能静默读出 QQ 授权码与两把密钥，包括 Agent 执行的 shell 命令，前提是 Agent 的沙箱放行。按 Codex 源码，开启网络的沙箱策略放行了 Keychain 服务；这一点要在 L1 或后续阶段真机确认。
- **能防住：** 明文进入配置、仓库、日志与备份；其他系统用户读取；在没有登录密码的情况下离线访问磁盘。
- **为何不改用进程内调用：** 在进程内调用 Security.framework，能把访问权限绑定到 TurnCourier 自身。但 Go 构建的二进制只有随构建变化的临时签名，或者完全没有签名，结果是每次升级都要重新弹窗授权，后台进程会被卡住。这条路要等有 Developer ID 签名后才实用。

**建议：**

- alpha 接受这一威胁模型，并写入 `design.md` 与 `SECURITY.md`；
- 缓解措施：使用专用机器人邮箱，而不是个人邮箱；授权码可随时在 QQ 邮箱中停用；
- 发布阶段再评估 Developer ID 签名加进程内调用，届时优先选纯 Go 的 purego，不引入 CGO。

**条目布局：**

- service 固定为 `io.github.chaorookie.turncourier`；
- account 为 `<实例 ID>:<用途>`，实例 ID 随数据目录生成，避免多个数据目录互相覆盖；
- 共三个条目，按用途分开：
  - QQ 授权码；
  - 令牌签名密钥；
  - 正文加密密钥。

  这样轮换签名密钥不会让待投递的密文无法解密。
- 二进制密钥用 `crypto/rand` 生成 32 字节，以 base64url 文本保存。

### D4：线程绑定与令牌载体（先定方向，细则在 L1 后定稿）

`design.md` 要求：线程引用、短任务 ID 与签名令牌必须一致，缺失或冲突即拒绝。D1 的证据表明，「线程引用」指向的很可能是 QQ 分配的 ID，而非我方 ID。

**建议方向：**

- **签名令牌：** 放在通知正文页脚，纯文本与 HTML 各一份。令牌是持有者凭证，不放进主题、Message-ID、Reply-To 或自定义头：主题会出现在锁屏预览里；自定义头不会被回复带回。
- **令牌格式：** HMAC-SHA256 截断到 128 位，编码为 48 个 Crockford base32 字符，与任务 ID 的字母表相同。令牌内含随机通知 ID（nid）；任务、owner 与有效期从数据库中该 nid 对应的通知行取得，一并参与签名。
- **短任务 ID：** 放在主题标签 `[TC <10 位任务 ID>]`。
- **线程引用：** `In-Reply-To` 或 `References` 中任一处包含该通知**实际投递的** Message-ID，即算匹配。实际 ID 的来源由 L1 决定，候选有三种：
  - 从「已发送」读取，按 `X-OQ-MSGID` 对应；
  - 给机器人自己抄送一份，从收件箱读取；
  - 从 DATA 响应中取得。
- **缺失时的处理：** 线程头或引用中的令牌缺失时拒绝，只在本地提示，不自动回信（防回环与反向散射）。
- **放宽须另行确认：** 若 L1 表明常用客户端经常丢失线程头或引用，任何放宽都要维护者另行确认，并修改 `design.md` 后才能实施。例如：线程头缺失时，只要令牌与主题标签都有效就接受。

**不采用：** 把令牌放进主题。它能提高到达率，但令牌会暴露在锁屏、通知与邮件列表预览中。

### D5：对 Phase 3 已定细节的两处修改

1. **SQLite 连接参数增加 `secure_delete(1)`。** 删除正文后尽力执行 `PRAGMA wal_checkpoint(TRUNCATE)`。本地实验（modernc，SQLite 3.53.4）：
   - 默认 `secure_delete=0` 时，删除的内容会一直留在主库，直到 VACUUM；
   - `FAST` 模式对溢出页无效；
   - `ON` 模式能清掉主库，但 WAL 里仍有历史帧，要执行 TRUNCATE 检查点才会清零。

   APFS 快照与 Time Machine 备份中的残留不在 SQLite 能控制的范围内，文档中写明。
2. **`inbound_messages.body_sha256` 改为键控摘要。** 对短正文直接求 SHA-256，相当于可被字典反查的明文。改为 HMAC-SHA256，密钥不写入数据库，具体来源在 Task 契约中定。列名不变，因为 `0001` 已发布；语义变化写入文档与迁移说明。

### 已定的实现细节（不再逐项确认）

| 事项 | 决定 |
| --- | --- |
| 传输安全 | IMAP 与 SMTP 一律隐式 TLS，开启证书校验；不支持 STARTTLS 与明文。本机实测 QQ 的 143 端口不需要 TLS 就公告明文认证，25 与 587 端口在建立 TLS 之前也公告认证。配置中的端口仍可修改，但 TLS 模式固定 |
| 对机器人邮箱的写操作 | 只读：`BODY.PEEK`，不改 `\Seen`，不 MOVE、不 EXPUNGE，不清理「已发送」。扫描 INBOX 与 Junk |
| 收取游标 | `(账户, 文件夹, UIDVALIDITY, 最后 UID)`；UIDVALIDITY 变化时全量补扫，靠去重兜底 |
| IMAP 健壮性 | 每条命令都设期限，超时即关闭连接；命令白名单以 CAPABILITY 为准；IDLE 定期重建，并以低频 UID SEARCH 补扫；认证失败后至少暂停 10 分钟（官方建议 10–15 分钟），并提示授权码可能因修改 QQ 密码而失效 |
| 发信 | 串行发送；信封发件人与 `From` 都等于登录地址；`multipart/alternative`；带 `Auto-Submitted: auto-generated`；每一步都设期限；投递结果不确定时不自动重发（与回复队列一致） |
| 自动回复与退信 | 满足任一条件即判为非人工来信，只记录元数据：`Auto-Submitted` 取值不为 `no`；带 `X-Autoreply`/`X-Autorespond`；`Precedence` 为 auto_reply/bulk/junk/list；类型为 `multipart/report`；`Return-Path: <>`；发件人为 MAILER-DAEMON/postmaster；正文以 QQ 自动回复的固定开头起始。不按主题关键词过滤，避免误伤 |
| 被拒来信 | 只记录元数据与原因码，不回信；本地可查询 |
| 正文加密 | AES-256-GCM，随机 nonce（`cipher.NewGCMWithRandomNonce`）；关联数据绑定用途、任务 ID 与序号，防止密文被调换；只有待处理的正文落盘，处理完成后在同一事务中删除；Keychain 读取失败时拒绝发送与派发 |
| 令牌保密 | 令牌类型的各种格式化输出一律脱敏；错误文本不回显输入；只保存解析出的新正文，不保存原始 MIME（引用中带着令牌）；测试以金丝雀令牌断言日志与错误中不出现令牌 |
| 字符集 | gbk、gb2312、cp936 等标签统一映射到 GB18030 解码器；读取正文有大小上限 |

## 范围

**4a：**
- 依赖接入；
- Keychain 封装；
- `init`：写配置，并以不回显方式把授权码录入 Keychain，同时生成两把密钥；
- 令牌签发与验证；
- 正文加密；
- 迁移 `0002`：给入站记录增加文件夹列（重建 `inbound_messages` 与 `replies`），并新增密钥元数据、通知、正文密文、收取游标和被拒来信元数据；
- 待发通知的状态机与存储；
- SMTP 客户端与离线假服务器；
- IMAP 客户端（看门狗、重连、游标、补扫）与离线假服务器，假服务器能模拟 QQ 的无标签 `BAD`；
- L1 探测工具，只在 `tests/live` 中由人工执行；
- 文档同步与整阶段审查。

**L1：** 维护者在本机执行真机探测。结果写入本清单，再据此细化 4b 的任务。

**4b：**
- 按 L1 结果冻结线程绑定规则；
- 引用与签名解析器，附合成回归样本；
- 入站验证流水线：白名单、线程头、主题标签、令牌、有效期、任务状态，通过后调用 `RecordReply`；
- 自动回复与退信过滤；
- 通知渲染与确定性敏感信息过滤（设计要求的代码块裁剪、路径过滤）；
- 用合成 Agent 做闭环测试；
- 真实往返验收：维护者本机执行，覆盖 L1 列出的客户端。

**不在本阶段：** Codex 与 Claude 适配器、launchd 服务与菜单栏、worktree、遥测、邮件附件、多用户。

## 4a 文件结构

```text
internal/
├── security/
│   ├── keychain/
│   │   ├── keychain.go              # /usr/bin/security 薄封装：Get、Set、Delete，Store 接口，条目命名，二进制密钥的文本编码
│   │   └── keychain_test.go         # 测试二进制扮演假 security，断言机密不进入参数与环境变量
│   ├── token/
│   │   ├── token.go                 # 回复令牌 v1：签发、解析、验证、脱敏输出；正文键控摘要
│   │   ├── token_test.go            # 篡改穷举、绑定字段、边界时刻、金丝雀、模糊测试
│   │   └── source_test.go           # 源码检查：标签只用常数时间比较
│   └── payload/
│       ├── payload.go               # 正文加密：AES-256-GCM、关联数据、密文格式
│       └── payload_test.go
├── queue/
│   ├── outbox.go                    # 待发通知状态、事件与转移表（与回复队列并列）
│   └── outbox_test.go
├── store/sqlite/
│   ├── migrations/0002_mail.sql     # 重建入站记录与回复表（文件夹列），实例、密钥元数据、通知、正文密文、收取游标、被拒来信、触发器
│   ├── instance.go                  # 实例 ID 与密钥元数据
│   ├── wal.go                       # 清理正文后的 TRUNCATE 检查点
│   ├── notifications.go             # 待发通知的创建、领取、结果记录与恢复
│   ├── mailbox.go                   # 收取游标与被拒来信元数据
│   └── *_test.go                    # 每个文件对应的测试
├── mail/
│   ├── smtp/
│   │   ├── smtp.go                  # 只用隐式 TLS 的单封发送，逐步期限，投递结果三分类
│   │   └── smtp_test.go             # go-smtp 服务端作假服务器；明文防护与源码白名单测试
│   └── imap/
│       ├── session.go               # 单连接：登录、只读 EXAMINE、UID 补扫、PEEK 取信、IDLE；每条命令有期限
│       ├── watcher.go               # 长连接循环：IDLE 定期重建、看门狗、退避重连、认证失败暂停
│       ├── fakeserver_test.go       # imapmemserver 加可注入故障的代理：无标签 BAD、半开连接、命令记录
│       └── *_test.go
├── config/
│   └── write.go                     # init 用：渲染配置文本、原子且不覆盖地创建配置文件
└── cli/
    ├── init.go                      # init 命令的交互流程
    ├── terminal.go                  # 基于 x/term 的交互终端：逐行读入、不回显读入机密
    └── *_test.go
tests/
├── integration/
│   └── payload_test.go              # 通知、令牌、键控摘要、正文密文与派发的组合生命周期
└── live/                            # 探测文件带 //go:build live 且须设置 TURNCOURIER_LIVE=1，只由维护者人工执行
    ├── sample.go                    # 不带构建标签：样本脱敏归类、主题前缀白名单、输出目录校验等纯函数
    ├── sample_test.go               # 不带构建标签：用合成邮件字节离线自检，随 make test 在 CI 中运行
    ├── live_test.go                 # live：开关、总确认、配置与 Keychain 读取、逐封确认
    ├── probe_test.go                # live：L1 探测（能力、发信、回复结构、IDLE）
    └── keychain_test.go             # live：合成条目的真实钥匙串往返
```

同时修改：`.gitignore`、`go.mod`、`go.sum`、`Makefile`、`cmd/turncourier/main.go` 与测试、`internal/cli/cli.go` 与测试、`internal/config/config.go`（凭据提示文案）与测试、`configs/turncourier.example.toml`（注释改为现在时）、`internal/store/sqlite/store.go`、`replies.go`、`tasks.go` 及其测试、`tests/integration/lifecycle_test.go`，以及 Task 14 列出的文档。

**依赖方向（4a 结束时）：**

```text
cmd/turncourier ─► internal/cli ─► internal/doctor
                        ├──────► internal/config
                        ├──────► internal/store/sqlite ─► internal/task
                        │                 ├───────────► internal/queue
                        │                 └───────────► internal/security/payload
                        ├──────► internal/security/keychain
                        └──────► golang.org/x/term

internal/security/token      只用标准库
internal/security/payload    只用标准库
internal/security/keychain   只用标准库（运行时调用 /usr/bin/security）
internal/mail/smtp           ─► github.com/emersion/go-smtp、go-sasl
internal/mail/imap           ─► github.com/emersion/go-imap/v2（imapclient）、go-message（imapclient 导入 go-message/mail）

tests/live（sample.go，无构建标签） ─► go-message、go-message/charset、x/text
tests/live（探测文件，live 标签）   ─► config、security/*、mail/*、modernc.org/sqlite（只读查询实例 ID）
tests/integration ─► config、store/sqlite、task、queue、security/token、security/payload
```

- `internal/mail/*` 不依赖存储、配置与 `security/*`：它们只收发字节，游标与密码由调用方以值或函数传入。
- `internal/security/*` 互不依赖，也不依赖邮件与存储。`store/sqlite` 只依赖 `payload`，因为正文要在分配序号的同一事务中加密；它不依赖 `token`，通知 ID（nid）以 `[12]byte` 保存。
- `store/sqlite` 仍不导入 `config`；机器人域名、令牌有效期等由调用方传入。
- 4a 的装配者只有两处：`internal/cli`（`init` 用配置、存储、Keychain 与终端）和 `tests/live`（探测工具）。4b 新建 `internal/app` 装配收发循环、入站验证与存储，此前产品代码中没有任何包同时导入邮件与存储。
- 产品二进制从本阶段起链接 `BurntSushi/toml`、`modernc.org/sqlite`（及其依赖）与 `golang.org/x/term`；`go-imap`、`go-smtp`、`go-message`、`x/text` 要到 4b 由 `internal/app` 接入命令后才进入产品二进制，4a 只有测试与 `tests/live` 使用它们。

## 4a 已定的实现细节（在已确认决策范围内定下，不再逐项确认）

| 事项 | 决定 | 所在任务 |
| --- | --- | --- |
| 依赖接入时机 | 每个模块在第一个导入它的任务中以 `go get <模块>@<版本>` 固定到 D2 的版本；`golang.org/x/sys` 在 Task 1 单独升到 v0.48.0。imapclient 导入 `go-message/mail`，所以 go-message v0.18.2 随 go-imap/v2 在 Task 11 进入 `go.mod`（间接依赖）并链接进 `internal/mail/imap`；x/text 只被 `go-message/charset` 导入，Task 11–12 不在构建图中（v0.14.0 只出现在模块图里），在 Task 13 导入 charset 时显式固定为 v0.42.0 | 1、10、11、12、13 |
| Keychain 条目 | service `io.github.chaorookie.turncourier`；account `<实例 ID>:qq-auth-code`、`<实例 ID>:token-key-<kid>`、`<实例 ID>:payload-key-<kid>`，kid 为十进制 1–255。D3 的「三个条目」指三类用途：每类用途按 kid 各有一个条目，4a 只生成 kid=1，因此 4a 结束时恰好三个；轮换时新旧密钥可以并存 | 2、12 |
| 测试向量与密钥扫描 | gitleaks 默认规则 generic-api-key 的触发条件大致是：关键词（不区分大小写的 access、api、auth、credential、creds、key、passwd、password、secret、token，可以出现在标识符里，也可以出现在字符串里）之后隔着至多 20 个字符，再经 `=`、`:=`、`:`、`,` 等分隔符（中间可以有空白与换行）跟着一个 10 个字符以上、香农熵不低于 3.5 的值。必须出现的 account 名 `qq-auth-code` 本身含 auth，参数列表中的逗号就能充当分隔符，所以只让变量名避开关键词不够。约定：授权码、密码、密钥、令牌、HMAC 输出，以及它们的十六进制、base64、base32 文本，这些测试向量一律在运行时构造：用 `strings.Repeat`、`bytes.Repeat` 生成低熵值；或按下标循环填充字节数组，再调用 `hex.EncodeToString`、`base64`、`base32` 编码。断言用的期望文本同样在运行时由这些值拼接，源码中不出现这类值的字符串字面量。`[]byte{0x01, …}` 形式的字节字面量可以使用，其中每个元素都短于 10 个字符。写有这类向量的任务都在提交之前的验证步骤运行 `make secrets`（Task 11 由 Step 4 的 `make security` 覆盖，它包含 `make secrets`）：`make check` 不含密钥扫描，而扫描脚本检查全部历史且拒绝 `.gitleaksignore`，这类字面量一旦提交就只能改写分支历史。不使用 `gitleaks:allow`，确需使用时须经审查并写入实施说明 | 2、3、4、6、7、8、10、11、12、13 |
| 实例 ID | `init` 首次运行时由 `crypto/rand` 生成 10 字节，编码为 16 位小写 Crockford base32（与任务 ID 同一字母表）；存于数据库 `instance` 表的唯一一行，数据目录不变则实例 ID 不变 | 6、12 |
| 二进制密钥 | 32 字节，`crypto/rand` 生成，Keychain 中保存 `base64.RawURLEncoding` 文本（43 个字符）。密钥条目只用不带 `-U` 的 `Add` 创建，从不覆盖。这比 D3 更严格，不是弱化：D3 的写入命令带 `-U`（条目已存在时更新），授权码仍按原命令经 `Set` 写入；密钥条目只是去掉 `-U`、只创建不更新，D3 的其余要求（每次一个 `security -i` 进程、只经标准输入发送、`-X` 十六进制、不用 `-w <值>`、`-p`、`-v`、`-A`、写后读回核对）全部照旧，D3 原文不改。元数据（用途、kid、状态、8 字节校验值）存 `crypto_keys` 表，表中没有密钥材料；校验值为 HMAC-SHA256(密钥, `"turncourier/key-check/v1\x00"` ‖ 用途 ‖ kid) 的前 8 字节，用于发现 Keychain 中的密钥被替换 | 2、6、12 |
| 令牌 v1 | 30 字节：版本 `0x01`、kid、12 字节随机 nid、16 字节截断 HMAC-SHA256 标签；编码为 48 个 Crockford base32 字符（30 字节恰为 5 的倍数，没有尾位歧义）；解析不区分大小写 | 3 |
| 我方 Message-ID | `<tc.` + 24 位随机 base32（15 字节）+ `@` + 机器人地址的域名 + `>`，创建通知时生成，重试不变；不含 nid、任务 ID 或令牌 | 7 |
| 入站记录的文件夹 | `inbound_messages` 增加 `folder` 列（NOT NULL，无默认值，1–255 个字符），UID 唯一键由 `(account, uid_validity, uid)` 改为 `(account, folder, uid_validity, uid)`，`UNIQUE (account, message_id)` 保留，按 Message-ID 的去重不变；`0002` 开头同时重建 `inbound_messages` 与 `replies`，已有行的 folder 填 `INBOX`；`InboundReply` 增加 `Folder`，`RecordReply` 按含文件夹的键查找（维护者 2026-09-19 确认方案 (a)） | 6 |
| 键控摘要（D5 第 2 条） | `inbound_messages.body_sha256` 保存 HMAC-SHA256(令牌签名密钥, `"turncourier/body-digest/v1\x00"` ‖ 新正文)。密钥为该回复所引用通知的令牌密钥（kid 与通知行 `token_kid` 相同），计算对象是解析器输出、将被加密入队的新正文 UTF-8 字节，不是原始 MIME | 3、8 |
| 正文密文 | `0x01` ‖ 12 字节随机 nonce ‖ 密文 ‖ 16 字节标签，比明文长 29 字节；关联数据 `"turncourier/payload/v1\x00"` ‖ 用途（`r` 或 `n`）‖ kid ‖ 任务 ID（10 字节）‖ 序号（uint64 大端）；明文上限 1 MiB | 4 |
| 正文清理 | 由数据库触发器在状态变为终态的同一事务中删除；提交后尽力执行 `PRAGMA wal_checkpoint(TRUNCATE)`，打开数据库时也执行一次。检查点期间临时把 `busy_timeout` 设为 0，不等待读者与写者，忙时立即跳过，留待下一次 | 6、7、8 |
| 待发通知 | 状态 PENDING、SENDING、SENT、UNCERTAIN、ABANDONED；全局至多一条 SENDING（串行发送）；投递结果不确定时进入 UNCERTAIN，只能由本地核对解除，从不自动重发。任务关闭后，通知不再回到 PENDING；领取前放弃令牌将在 10 分钟内过期的 PENDING 通知（原因 `expired`） | 5、7 |
| SMTP | 只用 go-smtp `DialTLS`；拨号与 TLS 握手 30 秒（库固定）；问候、EHLO、AUTH、MAIL、RCPT、DATA 各 30 秒；正文按 64 KiB 分块写入，每块 30 秒内须写完；从开始 flush 剩余正文、写结束标记到收到响应共 120 秒。期限由看门狗计时器到期关闭连接来保证，库自身的超时只作第二道保险；EHLO 名为 `localhost`；只在 TLS 连接上、服务器公告 `AUTH PLAIN` 时认证；证书校验参数在包内构造，调用方只能传入根证书池 | 10 |
| IMAP | 只用 imapclient `DialTLS`；拨号与握手 15 秒、问候 15 秒、普通命令 30 秒、单封 FETCH 60 秒、IDLE 确认 30 秒、IDLE 最长 5 分钟、结束 IDLE 10 秒；超时即关闭连接 | 11 |
| IMAP 重连 | 退避初值 15 秒、倍数 2、上限 10 分钟、±20% 抖动。只有连接自登录起保持健康达到 IdleMax，或一次 IDLE 正常结束，才复位退避；任意 1 小时内至多 12 次登录；同一次运行中 IDLE 连续 2 次超时后改为按 Poll 间隔轮询；认证失败暂停 15 分钟；`Handle` 返回的本地错误在同一连接内按退避重试，不重新登录 | 11 |
| IMAP 收取 | 每次登录后 LIST 一次，按 LIST 结果判断 Junk 是否存在，同一连接上每小时重新 LIST。先 Junk 后 INBOX，每轮 EXAMINE、`UID SEARCH UID <last+1>:*` 并过滤 `uid > last`、按批（至多 50 封，正文合计至多 16 MiB）`UID FETCH (UID RFC822.SIZE)`，再对声明不超过 2 MiB 的邮件逐封取 `BODY.PEEK[]<0.2097153>`，读取字面量至多 2 MiB + 1 字节，超出即判为过大；UIDVALIDITY 变化时从 UID 1 全量补扫 | 11 |
| 被拒来信原因码 | 数据库只约束形状（1–40 个 `[a-z_]` 字符），合法取值由 Go 端列表唯一定义，4b 可增补而不需要迁移 | 9 |
| init | 仅 macOS、仅交互式终端；不联网；未经明确确认不替换授权码；配置与已登记的密钥从不覆盖；错误文本不含路径与机密。设计要求的环境诊断与真实往返测试在 4b 接入 | 12 |
| L1 探测 | 构建标签 `live` 加 `TURNCOURIER_LIVE=1` 双重开关；访问 Keychain 或网络之前经 `/dev/tty` 总确认一次，每封信发出前再逐项确认；只输出脱敏的结构样本 | 13 |

---

### Task 1：依赖核验与 x/sys 升级

**Files:**
- Modify: `go.mod`、`go.sum`

本任务不导入新模块。各模块在第一个导入它的任务中接入，避免 `go mod tidy` 删除尚未使用的依赖：go-smtp 与 go-sasl 在 Task 10；go-imap/v2 在 Task 11，go-message 随之作为间接依赖进入 `go.mod`（imapclient 导入 `go-message/mail`）；x/term 在 Task 12；x/text 在 Task 13 导入 `go-message/charset` 时显式固定（此前它不在任何包的构建图中，只以 go-message 要求的 v0.14.0 出现在模块图里）。`golang.org/x/sys` 先单独升级，使现有存储栈在新版本上的回归与新功能分开。

- [x] **Step 1：补齐 D2 留下的 x/term 编译验证。** 在仓库之外的临时模块中写一个示例程序，调用 `term.IsTerminal` 与 `term.ReadPassword`，固定 `golang.org/x/term@v0.46.0`；以 `CGO_ENABLED=0` 分别构建 darwin/arm64、darwin/amd64、linux/amd64，并对三者运行 `go vet`。用 `go mod graph` 确认 x/term v0.46.0 要求 `golang.org/x/sys v0.48.0`。
- [x] **Step 2：许可证核对。** 对 D2 表中的每个模块执行 `go mod download -json <模块>@<版本>`，读取返回的 `Dir` 下的 LICENSE 文件，确认许可证与 D2 表一致（go-imap/v2、go-message、go-sasl、go-smtp 为 MIT；x/text、x/term、x/sys 为 BSD-3-Clause）。结果写入本任务的实施说明，不写本机路径。
- [x] **Step 3：升级。** `go get golang.org/x/sys@v0.48.0`，再 `go mod tidy`。`git diff go.mod` 只改动 `golang.org/x/sys` 一行。
- [x] **Step 4：验证。** `make check`、`make security`（govulncheck 无发现）、`CGO_ENABLED=0 go test -count=1 ./...`、`GOOS=linux go vet ./...`、`GOOS=windows go vet ./...` 全部通过。
- [x] **Step 5：提交。** `git add go.mod go.sum && git commit -m "build: upgrade golang.org/x/sys to v0.48.0"`

**实施说明：** 本任务不新增代码与测试，以升级前 `go list -m golang.org/x/sys` 为 v0.47.0 作为失败基线，升级后为 v0.48.0。Step 1 的临时模块（go 1.27.1）调用 `term.IsTerminal` 与 `term.ReadPassword`，`go get golang.org/x/term@v0.46.0` 同时加入 `golang.org/x/sys v0.48.0`；以 `CGO_ENABLED=0` 构建 darwin/arm64、darwin/amd64、linux/amd64 并运行 `go vet` 均通过，`go mod graph` 含 `golang.org/x/term@v0.46.0 golang.org/x/sys@v0.48.0`。Step 2 的许可证与 D2 表一致：go-imap/v2 v2.0.0-beta.8、go-message v0.18.2、go-sasl `v0.0.0-20241020182733-b788ff22d5a6`（即 D2 的 `b788ff2`，由 go-imap/v2 的 `go.mod` 指定）、go-smtp v0.25.0 的 LICENSE 为 MIT 全文；x/text v0.42.0、x/term v0.46.0、x/sys v0.48.0 的 LICENSE 为 BSD-3-Clause（三项条件），另附 Go 项目的 PATENTS 专利授权文件。多个模块的 LICENSE 列有多个版权方（例如 go-smtp 列有 The Go Authors、Gleez Technologies、emersion、Proton Technologies AG；go-imap/v2 列有 The Go-IMAP Authors、Proton Technologies AG、Simon Ser），发布前补齐第三方许可声明时随附各模块 LICENSE 原文，保留全部版权行。Step 3 中 `go mod tidy` 没有其他改动：`go.mod` 只改 x/sys 一行，`go.sum` 只替换 x/sys 的两行哈希，与临时模块下载所得一致。Step 4 全部通过：`make check`（覆盖率 93.3%）、`make security`（govulncheck 无发现，gitleaks 无泄漏）、`CGO_ENABLED=0 go test -count=1 ./...`、`GOOS=linux go vet ./...`、`GOOS=windows go vet ./...`。本任务的勾选与实施说明随 Step 5 的同一提交写入本清单，因此该提交在 `go.mod`、`go.sum` 之外还包含本文件。

### Task 2：Keychain 封装

**Files:**
- Create: `internal/security/keychain/keychain.go`
- Test: `internal/security/keychain/keychain_test.go`

契约：

```go
// Package keychain 通过 /usr/bin/security 读写 macOS 登录钥匙串中的通用密码条目。
// 机密只经标准输入写入、经标准输出读出，从不出现在子进程参数、环境变量、错误文本或日志中。
// 经 security 创建的条目，受信任应用是 /usr/bin/security，同一用户的任何进程都能读出（见 design.md 的威胁模型）。
package keychain

// Service 是 TurnCourier 全部条目共用的 service 名。
const Service = "io.github.chaorookie.turncourier"

var (
	// ErrNotFound 表示条目不存在（security 退出码 44，即 errSecItemNotFound 的低 8 位）。
	ErrNotFound = errors.New("keychain item not found")
	// ErrUnsupported 表示当前平台没有受支持的凭据存储；不提供明文文件后备。
	ErrUnsupported = errors.New("keychain is only supported on macOS")
	// ErrInteractionNotAllowed 表示钥匙串已锁定且当前环境不允许弹窗（security 退出码 36）。
	ErrInteractionNotAllowed = errors.New("keychain is locked and user interaction is not allowed")
	// ErrInvalidName 表示 account 不是 1–128 个 [A-Za-z0-9._@:+-] 字符。
	ErrInvalidName = errors.New("invalid keychain account name")
	// ErrInvalidSecret 表示机密不是 1–1000 个不含空白的可打印 ASCII 字符，或密钥文本不是 43 个 base64url 字符。
	ErrInvalidSecret = errors.New("invalid keychain secret")
	// ErrReadBackMismatch 表示写入后读回的内容不一致或读不到。
	ErrReadBackMismatch = errors.New("keychain read-back mismatch")
	// ErrExists 表示 Add 的目标条目已存在；已有条目不会被改动。
	ErrExists = errors.New("keychain item already exists")
)

const (
	// DefaultTimeout 是后台读取使用的每次调用期限。
	DefaultTimeout = 10 * time.Second
	// InteractiveTimeout 是 init 使用的期限，给用户留出在解锁或授权弹窗中输入登录密码的时间。
	InteractiveTimeout = 60 * time.Second
)

// Store 是系统凭据存储的最小接口；使用方依赖它，测试注入内存替身。
type Store interface {
	Get(ctx context.Context, account string) (string, error)
	Set(ctx context.Context, account, secret string) error
	Add(ctx context.Context, account, secret string) error
	Delete(ctx context.Context, account string) error
}

// Security 是基于 /usr/bin/security 的 Store 实现，可并发使用。
type Security struct {
	path    string        // 生产固定为 /usr/bin/security，不经 PATH 查找；测试在包内替换为测试二进制
	timeout time.Duration // 每次调用的期限；调用方的 ctx 更早到期时以 ctx 为准
}

// New 返回生产实现，timeout 为 0 时使用 DefaultTimeout；非 darwin 平台返回 ErrUnsupported。
func New(timeout time.Duration) (*Security, error)

// Get 执行 find-generic-password -s Service -a account -w，只读标准输出（至多 4096 字节），
// 去掉恰好一个结尾换行后校验与 Set 相同的格式；标准错误丢弃，不进入错误文本。
func (s *Security) Get(ctx context.Context, account string) (string, error)

// Set 启动 security -i，经标准输入只发一行 add-generic-password -U -s Service -a account -X <小写十六进制>，
// 退出码为 0 后用 Get 读回核对。security -i 的退出码只反映最后一条命令，因此每个进程只发一条命令。
// 不使用 -w 取值、-p、-v、-A 或 -T；机密与十六进制文本都不出现在参数列表中。
// 用于替换授权码；密钥条目一律用 Add 创建，从不经 Set 覆盖。
func (s *Security) Set(ctx context.Context, account, secret string) error

// Add 只创建新条目：先 Get，已存在返回 ErrExists；不存在时经 security -i 发送不带 -U 的
// add-generic-password -s Service -a account -X <小写十六进制>。退出码非 0 时再 Get 一次，条目已存在
// （并发的另一个写入者先写入）返回 ErrExists，否则返回原错误；退出码为 0 后读回核对。不依赖「条目已存在」的具体退出码。
func (s *Security) Add(ctx context.Context, account, secret string) error

// Delete 执行 delete-generic-password -s Service -a account 并丢弃输出；条目不存在时返回 ErrNotFound。
func (s *Security) Delete(ctx context.Context, account string) error

// AuthCodeAccount 返回 QQ 授权码条目的 account：<instanceID>:qq-auth-code。
func AuthCodeAccount(instanceID string) string

// TokenKeyAccount 返回令牌签名密钥条目的 account：<instanceID>:token-key-<kid>；kid 为 0 属于编程错误，函数 panic。
func TokenKeyAccount(instanceID string, kid uint8) string

// PayloadKeyAccount 返回正文加密密钥条目的 account：<instanceID>:payload-key-<kid>；kid 为 0 时 panic。
func PayloadKeyAccount(instanceID string, kid uint8) string

// NewKeyText 从 random 读取 32 字节，返回 base64.RawURLEncoding 文本（43 个字符），供写入 Keychain。
func NewKeyText(random io.Reader) (string, error)

// DecodeKeyText 把 Keychain 中的密钥文本还原为 32 字节；长度或编码不符时返回 ErrInvalidSecret，错误不回显输入。
func DecodeKeyText(text string) ([]byte, error)

// KeyCheck 返回 HMAC-SHA256(key, "turncourier/key-check/v1\x00" ‖ purpose ‖ kid) 的前 8 字节，
// 登记在 crypto_keys.key_check 中，用于发现 Keychain 中的密钥被替换；purpose 须为 token 或 payload，kid 为 0 时 panic。
func KeyCheck(purpose string, kid uint8, key []byte) [8]byte
```

其他退出码返回 `keychain <操作> failed with exit code N`；期限到达时返回包装 `context.DeadlineExceeded` 的 `keychain <操作> timed out`，并确保子进程被结束。子进程继承父进程环境变量，不另外设置任何变量。

- [x] **Step 1：写失败的测试。** 沿用 `internal/doctor` 的测试辅助子进程模式：`TestMain` 发现 `TURNCOURIER_TEST_SECURITY_STATE`（假钥匙串状态文件）非空时不运行测试，而是扮演 `security`：把每次调用的 argv、完整标准输入与 `os.Environ()` 追加写入 `TURNCOURIER_TEST_SECURITY_LOG`，再按 `TURNCOURIER_TEST_SECURITY_MODE`（`normal`、`exit36`、`exit1`、`corrupt`、`drop`、`hang`、`output`、`add-exists`）动作；`normal` 模式下不带 `-U` 的写入遇到已有条目时以非 0 退出且不改动。测试把 `Security.path` 设为 `os.Executable()`。用例：
  - **往返与参数：** 机密在运行时构造为 `strings.Repeat("aB3", 4)`（含小写、大写与数字）；`Set(ctx, "0123456789abcdef:qq-auth-code", 机密)` 后 `Get` 返回同一值；日志中第一次调用 argv 恰为 `["-i"]`，标准输入恰为 `"add-generic-password -U -s " + Service + " -a 0123456789abcdef:qq-auth-code -X " + hex.EncodeToString([]byte(机密)) + "\n"`（期望文本在运行时拼接；`hex.EncodeToString` 输出小写，同时钉住小写十六进制）；第二次调用 argv 恰为 `["find-generic-password", "-s", Service, "-a", 账户, "-w"]`。机密原文与其十六进制文本都不出现在任何一次调用的 argv 与环境变量中。
  - **退出码映射：** `Get` 不存在的条目返回 `ErrNotFound`（假程序退出 44）；`exit36` 返回 `ErrInteractionNotAllowed`；`exit1` 时假程序向 stderr 写入 `STDERR-CANARY` 并退出 1，返回的错误含 `exit code 1`，且不含 `STDERR-CANARY`。
  - **读回核对：** `corrupt`（写入时存成另一个值）与 `drop`（`-i` 退出 0 但不保存）下 `Set` 都返回 `ErrReadBackMismatch`。
  - **输出校验：** `output` 模式依次让 `Get` 读到 `abc`（没有结尾换行）、`ab c\n`、`abc\n\n`、4097 字节的可打印文本，均返回错误，且错误文本不含读到的内容。
  - **名称与机密校验先于启动进程：** account 为 `""`、`a b`、`a'b`、`a"b`、`中文`、129 个 `a` 时返回 `ErrInvalidName`；机密为 `""`、`a b`、`a\tb`、`a\nb`、`é`、1001 个 `a` 时返回 `ErrInvalidSecret`；两类情况下日志文件都不存在（假程序从未运行），错误文本不含输入。
  - **行长上界：** 1000 个字符的机密可以写入，日志中的标准输入行不超过 4095 字节（security -i 的行缓冲为 4096 字节）。
  - **期限：** `hang` 模式下假程序睡眠 30 秒并把自身 PID 写入日志；`timeout = 200ms` 时 `Get` 在 2 秒内返回包装 `context.DeadlineExceeded` 的错误，之后该 PID 已不存在。
  - **只创建（Add）：** 条目不存在时 `Add` 成功，标准输入恰为 `add-generic-password -s io.github.chaorookie.turncourier -a <账户> -X <十六进制>\n`（不含 `-U`），随后读回核对；条目已存在时返回 `ErrExists`，假钥匙串中的原值不变，且没有启动 `-i` 进程；`add-exists` 模式（第一次 Get 读不到，写入时假程序发现条目已被另一写入者创建、以非 0 退出）返回 `ErrExists`、原值不变。
  - **删除：** 存在的条目 `Delete` 后 `Get` 返回 `ErrNotFound`，argv 恰为 `["delete-generic-password", "-s", Service, "-a", 账户]`；删除不存在的条目返回 `ErrNotFound`。
  - **平台与期限：** 包内辅助函数 `newFor(goos string, timeout time.Duration)` 在 `darwin` 返回 path 为 `/usr/bin/security` 的实例，timeout 为 0 时取 `DefaultTimeout`，传入 `InteractiveTimeout` 时为 60 秒；在 `linux`、`windows` 返回 `ErrUnsupported`；`New` 只是以 `runtime.GOOS` 调用它。
  - **条目命名：** `AuthCodeAccount("0123456789abcdef")` 为 `0123456789abcdef:qq-auth-code`；`TokenKeyAccount(id, 1)` 为 `…:token-key-1`；`PayloadKeyAccount(id, 255)` 为 `…:payload-key-255`；三者都满足名称规则；kid 为 0 时 panic。
  - **密钥文本：** 字节 `0x00…0x1f` 的读取器给出的文本，等于测试中用 `base64.RawURLEncoding` 对同一字节数组编码的结果（不把编码结果写成字符串字面量，见 Step 4 的 `make secrets`）；读取器不足 32 字节时报错；`DecodeKeyText` 能还原；42 或 44 个字符、带 `=` 填充、含 `+` 或 `/` 时返回 `ErrInvalidSecret`，错误文本不含输入。
  - **校验值：** `KeyCheck` 与测试内直接调用 `crypto/hmac` 的参考实现一致；用途、kid 或密钥任一不同时结果不同；与 `token.BodyDigest` 及令牌 MAC 的前缀互不为前缀；未知用途或 kid 为 0 时 panic。
- [x] **Step 2：** `go test ./internal/security/keychain/` 编译失败。
- [x] **Step 3：实现。** 只用标准库。`exec.CommandContext` 配合 `context.WithTimeout`；`Set` 与 `Add` 的标准输入用 `strings.NewReader`；`Get` 以 `io.LimitReader(stdout, 4097)` 读取；标准错误设为 `nil`（丢弃）。
- [x] **Step 4：** `go test -race ./internal/security/keychain/` 通过，覆盖率 ≥ 90%；`CGO_ENABLED=0 go test ./internal/security/keychain/`、`GOOS=linux go vet ./internal/security/keychain/`、`GOOS=windows go vet ./internal/security/keychain/` 通过；`make comments` 与 `make secrets` 通过。测试向量按「4a 已定的实现细节」中「测试向量与密钥扫描」一行的约定在运行时构造，源码中不出现机密形状的字面量；只让变量名避开关键词不够。
- [x] **Step 5：** `git add internal/security/keychain && git commit -m "feat(keychain): wrap the macOS security tool without exposing secrets"`

**实施说明：** 先写测试，`go test ./internal/security/keychain/` 编译失败（`undefined: Store`、`Security`、`Service`、`DefaultTimeout` 等），再实现。实现要点：每个公开方法只设一个期限，Set、Add 的存在性检查、写入与读回共用它；错误文本中的操作名为 get、set、add、delete；无法启动 security 时返回不含路径的固定文本；读回阶段期限到达时返回期限错误而不是 `ErrReadBackMismatch`；Get 读到格式不符的输出时返回包装 `ErrInvalidSecret` 的错误，不回显内容。契约之外的两处小补充：ctx 被取消时返回包装 `context.Canceled` 的 `keychain <操作> canceled`，避免把取消报成超时；`DecodeKeyText` 用严格模式解码，拒绝末字符含非零填充比特的文本，使同一把密钥只有一种写法。测试方面：`strings.Repeat("aB3", 4)` 的十六进制是 `614233…`，不含字母，单靠它钉不住小写十六进制，小写改由 Add 用例（base64url 密钥文本）与 1000 个 `~` 的行长用例（`7e…`）逐字节比对钉住；假程序在清单的 8 种模式之外增加 `add-fails`（写入以 1 退出且不保存），用来钉住「Add 写入失败、条目仍不存在时返回原错误」；`hang` 只让读取与删除挂起、写入照常完成，同一模式还用来测试读回阶段超时；期限用例最多重试 5 次，防止慢机器上假程序来不及写日志、取不到 PID；测试为子进程设置 `GORACE=atexit_sleep_ms=0`（保留已有选项），否则竞态检测下每次以 0 退出都要多睡 1 秒，本包测试会从约 3 秒变成约 26 秒。另补了 exit36 下的 Set、Delete、Add（存在性检查失败时不启动写入进程），31 字节编码加换行、43 字符加换行两个解码用例，允许字符的正例与 `0x7f`。在仓库副本上做了 22 个变异，涉及读回与超时、Add 的预检和再读、退出码先后、严格解码、大小写十六进制、`-U` 有无、换行处理、字符集与长度边界、KeyCheck 输入顺序、环境变量泄露等，全部被测试杀死。验证：`go test -race` 通过，覆盖率 100%，`-count=10` 稳定；`CGO_ENABLED=0 go test`、包级与全仓的 `GOOS=linux go vet`、`GOOS=windows go vet`、`make comments`、`make secrets`、`make check`（总覆盖率 93.97%）均通过。未运行：真实钥匙串往返（按安全规则留给 Task 13 的 `tests/live`，由维护者人工执行）；本任务只在 macOS 上运行了测试，Linux 上的测试由 CI 运行。

**审查后修正：** 变异均在临时副本中逐项施加，每次只改一处。① 标准输出超过管道缓冲（约 64 KiB）时，`run` 读满 4097 字节后不再读取，子进程阻塞在写管道上，`Get` 要等到期限才返回 `keychain get timed out`，而不是格式错误。现在读满上限后用 `io.Copy(io.Discard, stdout)` 丢弃其余输出（受同一期限约束），内存中仍至多保留 4097 字节，超长输出按 `ErrInvalidSecret` 报告；修复放在各操作共用的 `run` 中，Delete 与写入的输出同样不会写满管道。审查建议之一是超长时结束子进程，这里没有采用：丢弃同样有上界，而且不会中断 Delete 等操作。输出校验用例增加 1 MiB 加换行的输出（改前等满 10 秒期限后报超时），并直接调用 `run` 断言只保留 `maxOutput+1` 字节、没有超时；去掉丢弃与去掉 `io.LimitReader` 两个变异都被杀死。② 假程序在日志中额外记录标准错误是否为空设备（`os.SameFile` 比对 `os.DevNull`），往返用例断言每次调用都是如此，且记录的环境变量排序后等于父进程的 `os.Environ()`；`command.Stderr = os.Stderr` 与额外设置一个环境变量两个变异都被杀死。③ 期限用例补 Delete：`hang` 模式、200 毫秒期限下 2 秒内返回包装 `context.DeadlineExceeded` 的 `keychain delete timed out`。④ 假程序增加 `hang-write` 模式，只让 `security -i` 睡眠 30 秒；1 秒期限下 Add 的存在性检查照常完成、期限落在写入中，再读因期限失败，断言返回 `keychain add timed out` 且不是 `ErrExists`，假钥匙串中没有该条目。去掉 Add 或 Delete 的期限、把再读的判断改成「不是 `ErrNotFound` 就返回 `ErrExists`」三个变异都被杀死。⑤ `exit1` 段补断言 Set 与 Delete 的错误恰为 `keychain set failed with exit code 1` 与 `keychain delete failed with exit code 1`；Set 写入阶段操作名改为 add、Delete 操作名改为 get 两个变异都被杀死。 ③ 复查后补充：`TestTimeout` 的读回超时断言原先查看整份调用日志，前面超时循环留下的读取可能让它空过；现只看本次 `Set` 产生的调用，要求恰为一次 `-i` 写入加一次读取。另记两点实现行为：`Add` 的预检读取遇到已有条目内容格式不符时，返回包装 `ErrInvalidSecret` 的错误而不是 `ErrExists`（以 `Get` 成功为「已存在」）；与 Task 1 相同，本任务的提交包含本清单的勾选与实施说明。

### Task 3：回复令牌 v1

**Files:**
- Create: `internal/security/token/token.go`
- Test: `internal/security/token/token_test.go`、`internal/security/token/source_test.go`

```go
// Package token 签发与验证回复令牌 v1：HMAC-SHA256 截断到 128 位的持有者凭证。
// 令牌只携带版本、密钥号与随机通知 ID（nid）；任务、owner 与有效期由通知行和任务行提供并参与 MAC。
// 本包不解析邮件，不规定令牌在邮件中的位置，也不包含线程匹配规则；这些在 L1 之后于 4b 定稿（D4）。
package token

const (
	// Version1 是当前唯一的令牌格式版本。
	Version1 byte = 1
	// TextLen 是令牌文本长度：30 字节编码为 48 个 Crockford base32 字符。
	TextLen = 48
	// KeyLen 是签名密钥的字节数。
	KeyLen = 32
)

// NID 是通知 ID：12 字节随机数，与通知行一一对应；它不是机密，单独泄露不足以伪造令牌。
type NID [12]byte

// Claims 是令牌绑定、但不随令牌携带的字段，验证时从通知行与任务行读取。
type Claims struct {
	TaskID    string    // 10 位小写 Crockford base32
	Owner     string    // tasks.owner，1–255 字节
	ExpiresAt time.Time // 通知行的 token_expires_at，按 UTC Unix 毫秒参与 MAC，须晚于 1970-01-01
}

// Key 是一把签名密钥；String、Format 与 LogValue 一律输出脱敏文本。
type Key struct {
	id     uint8
	secret [KeyLen]byte
}

// Token 是签发或解析得到的令牌；String、Format 与 LogValue 一律输出脱敏文本，只有 Reveal 返回令牌文本。
type Token struct {
	kid uint8
	nid NID
	tag [16]byte
}

var (
	// ErrMalformed 表示令牌文本不符合 v1 格式；错误文本固定，不回显输入。
	ErrMalformed = errors.New("malformed reply token")
	// ErrInvalidKey 表示密钥号为 0 或密钥长度不是 32 字节。
	ErrInvalidKey = errors.New("invalid reply token key")
	// ErrInvalidClaims 表示任务 ID、owner 或有效期不合法（通常意味着数据损坏）。
	ErrInvalidClaims = errors.New("invalid reply token claims")
	// ErrKeyMismatch 表示令牌的密钥号与传入的密钥不同。
	ErrKeyMismatch = errors.New("reply token key id mismatch")
	// ErrBadMAC 表示标签校验失败。
	ErrBadMAC = errors.New("reply token signature mismatch")
	// ErrExpired 表示验证时刻不早于有效期。
	ErrExpired = errors.New("reply token expired")
)

// NewKey 复制 secret 构造密钥；id 须为 1–255，secret 须为 32 字节。
func NewKey(id uint8, secret []byte) (*Key, error)

// ID 返回密钥号。
func (k *Key) ID() uint8

// NewNID 从 random 读取 12 字节。
func NewNID(random io.Reader) (NID, error)

// Issue 用 k 为 nid 与 c 计算标签；同样的输入总是得到同样的令牌，因此令牌无需落盘，重试发送得到同一令牌。
func Issue(k *Key, nid NID, c Claims) (Token, error)

// Parse 按顺序检查：长度恰为 48 字节；每个字节属于字母表（大写字母先转小写，i、l、o、u 与其他字符一律拒绝）；
// 解码为 30 字节；版本字节为 0x01；kid 不为 0。任一步失败都返回 ErrMalformed。
func Parse(text string) (Token, error)

// Verify 按顺序检查：t 的 kid 等于 k.ID()（ErrKeyMismatch）；c 合法（ErrInvalidClaims）；
// 用 hmac.Equal 比较标签（ErrBadMAC）；now 严格早于 c.ExpiresAt（ErrExpired）。
// 标签先于有效期检查，被篡改的过期令牌报 ErrBadMAC。任务状态与密钥状态由调用方另行检查。
func Verify(k *Key, t Token, c Claims, now time.Time) error

// KeyID 返回令牌的密钥号。
func (t Token) KeyID() uint8

// NID 返回令牌携带的通知 ID。
func (t Token) NID() NID

// Reveal 返回 48 个小写字符的令牌文本；只用于写入通知正文，调用处须能被代码审查检索到。
func (t Token) Reveal() string

// BodyDigest 返回 HMAC-SHA256(k, "turncourier/body-digest/v1\x00" ‖ body)，用作 inbound_messages.body_sha256（D5 第 2 条）。
func (k *Key) BodyDigest(body []byte) [32]byte
```

**字节布局与 MAC 输入：**

| 部分 | 长度 | 内容 |
| --- | --- | --- |
| 版本 | 1 | `0x01` |
| kid | 1 | 1–255，与 `crypto_keys` 中 `purpose = 'token'` 的 kid 对应 |
| nid | 12 | 通知行的 `nid` |
| 标签 | 16 | HMAC-SHA256 输出的前 16 字节 |

MAC 输入为 `"turncourier/reply-token/v1\x00"` ‖ 版本 ‖ kid ‖ nid ‖ 任务 ID（10 个 ASCII 字节）‖ owner 字节数（uint16 大端）‖ owner ‖ 有效期（UTC Unix 毫秒，uint64 大端）。30 字节按字母表 `0123456789abcdefghjkmnpqrstvwxyz` 无填充编码；30 是 5 的倍数，没有尾部冗余比特，不存在同一令牌的第二种写法。键控摘要的前缀 `"turncourier/body-digest/v1\x00"` 与 MAC 前缀互不为前缀，同一把密钥用于两种用途时输入域不相交。

**脱敏输出：** `Token` 的 `String` 与所有 `fmt` 动词（经 `Format`）输出 `[redacted reply token]`，`LogValue` 返回同一字符串；`Key` 对应输出 `[redacted token key]`。已知局限：`Token` 作为其他包中结构体的**未导出**字段时，`fmt` 无法调用其方法，会按原始字段打印；4b 的日志金丝雀测试覆盖装配后的实际路径。另一处已知局限：`fmt` 在调用 `Format` 之前先处理 `%p`，作用于 `Token` 或 `Key` 的值（而非指针）、或含它们的结构体值与数组值时走错误动词路径，按原始字段打印完整令牌或密钥材料；两种类型实现了 `fmt.Formatter`，`go vet` 也不报。`%p` 作用于指针、切片与映射时只打印地址。不得对这两种类型的值使用 `%p`。

- [x] **Step 1：写失败的测试。**
  - **已知答案：** 密钥为字节 `0x01…0x20`、kid 1、nid 为字节 `0x00…0x0b`、`Claims{TaskID: "0123456789", Owner: "local", ExpiresAt: time.UnixMilli(1_790_000_000_000)}`。测试中另写一份按上表逐字节拼接 MAC 输入、直接调用 `crypto/hmac` 与 `encoding/base32` 的参考实现（不调用被测函数），断言 `Issue(...).Reveal()` 与之相同；并把参考实现得到的 30 个原始字节以 `[]byte{0x01, …}` 字面量写入测试，断言 `Reveal()` 等于其编码，防止两边同时改错（不写 48 字符的文本字面量，以免被密钥扫描误报，见 Step 4）。
  - **确定性与格式：** 两次 `Issue` 结果相同；文本 48 个字符且都在字母表中；`Parse(t.Reveal())` 与 `Parse(strings.ToUpper(t.Reveal()))` 都等于 `t`。
  - **篡改穷举：** 48 个位置各换成字母表中另外 31 个字符（1488 个变体），以及 30 个原始字节的 240 个单比特翻转（翻转后重新编码），每个变体 `Parse` 失败或 `Verify` 失败，没有一个被接受。
  - **绑定字段：** 任务 ID 改为 `0123456788`、owner 改为 `Local` 或 `local `、有效期 ±1 毫秒时 `Verify` 返回 `ErrBadMAC`；同一密钥材料但 kid 为 2 的密钥返回 `ErrKeyMismatch`；kid 相同、材料不同返回 `ErrBadMAC`。
  - **有效期边界：** `now == ExpiresAt` 返回 `ErrExpired`；`ExpiresAt − 1ms` 通过；篡改且已过期的令牌返回 `ErrBadMAC`。
  - **解析错误：** 47 与 49 个字符；在第 10 位放入 `i`、`l`、`o`、`u`、`=`、空格、`\n`；版本字节为 `0x02`（构造原始字节后编码）；kid 为 0；含一个多字节 UTF-8 字符、总长 48 字节的字符串。全部返回 `ErrMalformed`，且 `err.Error()` 恰为 `malformed reply token`。
  - **非法输入：** `NewKey` 的 id 为 0、密钥 31 或 33 字节返回 `ErrInvalidKey`；构造后修改传入的切片不影响之后签发的令牌。`Issue` 与 `Verify` 在任务 ID 为 9 个字符、含 `u`、含大写字母，owner 为空或 256 字节，有效期为零值时返回 `ErrInvalidClaims`。
  - **NewNID：** 读取恰好 12 字节；读取器不足 12 字节时报错；真实随机源生成 1000 个互不相同。
  - **金丝雀：** 对令牌 `t` 与密钥 `k`，分别用 `%v %s %q %x %X %d %+v %#v` 格式化值、指针、`[]Token`、`map[string]Token`，以及含导出字段 `T token.Token` 的结构体；用 `slog.NewJSONHandler` 与 `slog.NewTextHandler` 记录 `slog.Any("t", t)`。输出中都不含 `t.Reveal()` 或它的任何 10 字符子串，也不含密钥材料的十六进制与 base64 文本。所有返回的错误文本都不含令牌文本。
  - **键控摘要：** 已知答案（参考实现直接调用 `crypto/hmac`）；不同密钥结果不同；与 `sha256.Sum256(body)`、以及不带前缀的 HMAC 都不同。
  - **模糊测试：** `FuzzParse`，种子为一个合法令牌与上述错误输入；性质：不 panic；成功时输入长度为 48，`Reveal()` 等于 `strings.ToLower(输入)`。
  - **常数时间比较（源码检查，`source_test.go`）：** 用 `go/parser` 与 `go/types` 检查本包非测试文件：`Verify` 的函数体调用了 `hmac.Equal`（或 `subtle.ConstantTimeCompare`）；任何 `==`、`!=` 的操作数类型都不是字节数组；不调用 `bytes.Equal`。把标签比较改为 `==` 或 `bytes.Equal` 的临时副本必须使该测试失败。功能测试无法区分这两种写法，这一性质只由本测试钉住。
- [x] **Step 2：** `go test ./internal/security/token/` 编译失败。
- [x] **Step 3：实现。** 只用标准库；`encoding/base32.NewEncoding(字母表).WithPadding(base32.NoPadding)`；解码前逐字节检查字母表，因为标准库解码会忽略 `\r\n`。
- [x] **Step 4：** `go test -race ./internal/security/token/` 通过，覆盖率 ≥ 95%；`go test -run='^$' -fuzz=FuzzParse -fuzztime=30s ./internal/security/token/` 无失败（本地执行一次）；`CGO_ENABLED=0 go test ./internal/security/token/` 与 `make secrets` 通过；已知答案的密钥与 nid 按下标循环填充，测试向量遵循「测试向量与密钥扫描」的约定。
- [x] **Step 5：** `git add internal/security/token && git commit -m "feat(token): add signed reply token v1 with redacted formatting"`

**实施说明：** 先写测试，`go test ./internal/security/token/` 编译失败（`undefined: Token`、`Key`、`Claims`、`NID` 等），再实现。实现要点：`Parse` 先逐字节转小写并检查字母表，再用无填充 base32 解码，因为标准库解码会跳过换行；`ID` 与 `BodyDigest` 按契约用指针接收者，两种类型的 `String`、`Format`、`LogValue` 用值接收者，使 `Key` 按值或按指针格式化都输出脱敏文本（`%p` 作用于值除外，见下文审查后修正）；有效期「晚于 1970-01-01」按 Unix 毫秒值大于 0 判断（纪元本身不合法，1 毫秒合法），过期判断为 `now` 不早于 `ExpiresAt`。契约之外的一处补充：`Issue` 遇到密钥号为 0 的零值 `Key` 时返回 `ErrInvalidKey`，否则会签出 kid 为 0、永远无法解析的令牌；`Verify` 仍按契约的检查顺序，零值密钥只会得到 `ErrKeyMismatch` 或 `ErrBadMAC`。源码检查（`source_test.go`）用 `importer.ForCompiler(fset, "source", nil)` 做类型检查，没有用 `importer.Default()`：本机工具链放在模块目录之内，gc 导入器在 GOROOT 下运行 `go list -export`，会把标准库目录当作主模块中的包而失败；source 导入器在进程内读取标准库源码，与工具链位置无关，竞态检测下约 2 秒。检查比规格略严：除字节数组的 `==`、`!=` 与 `bytes.Equal` 外，含字节数组的结构体与数组（例如整个 `Token` 相比较）、由字节切片或字节数组转换得到的字符串之间的比较，以及 `bytes.Compare`、`slices.Equal`、`reflect.DeepEqual` 也会报告。测试方面：「第 10 位」按从 1 开始计数，取下标 9；解析错误另加大写 `I`、`L`、`O`、`U`；非法绑定字段另加 11 位任务 ID 与纪元时刻，并钉住合法边界（任务 ID `zyxwvtsrqp`、255 字节 owner、有效期 1 毫秒）；另钉住检查顺序（kid 先于绑定字段，绑定字段先于标签）；`NewNID` 用逐字节返回的读取器钉住 `io.ReadFull`；金丝雀用例对值与指针要求输出恰为脱敏文本，对切片、映射、含导出字段的结构体与 slog 输出要求含脱敏文本且不含禁止子串，并直接断言 `String`（fmt 优先调用 `Format`，否则 `String` 覆盖不到）。在仓库副本上做了 41 个变异，每次只改一处，涉及标签比较（`==`、`bytes.Equal`、字符串比较、整个 `Token` 比较）、字母表与大小写、长度、版本与 kid 检查、绑定字段校验的各个边界、Verify 的检查顺序、MAC 输入的各段（kid 段的缺口见下文审查后修正）、两个域分隔前缀、`io.ReadFull`、零值密钥、`Reveal` 大小写、各格式化出口泄露与指针接收者，全部被测试杀死。验证：`go test -race` 通过，覆盖率 100%，`-count=5` 稳定；`FuzzParse` 本地运行 30 秒（约 456 万次执行）无失败，没有产生 testdata；`CGO_ENABLED=0 go test`、包级与全仓的 `GOOS=linux go vet`、`GOOS=windows go vet`、`make comments`、`make check`（总覆盖率 94.32%）、`make secrets` 均通过。本任务不涉及钥匙串与网络；测试只在 macOS 上运行，Linux 上的测试由 CI 运行。与 Task 1、2 相同，本任务的提交包含本清单的勾选与实施说明。

**审查后修正：** 变异均在临时副本中逐项施加，每次只改一处。① 上文所说「MAC 输入的各段全部被杀死」对 kid 段不成立：已知答案用 kid 1，恰好等于版本字节 `0x01`；其余用例只用 kid 1 签发，kid 为 2 的密钥又先被 `ErrKeyMismatch` 拦下，走不到标签比较。因此把 `tag` 中的 `append(msg, Version1, k.id)` 改为 `(Version1, Version1)`、`(k.id, k.id)`、`(Version1, 1)`、`(k.id, Version1)` 的四个变异原先全部存活。现 `TestKnownAnswer` 补一个 kid 7 的令牌与参考实现比对，单独即可杀死这四个变异；`TestClaimsBinding` 补一个改标为 kid 2 的令牌，用同材料、kid 2 的密钥验证须得 `ErrBadMAC`，单独可杀死 `(Version1, Version1)` 与 `(Version1, 1)`。② 契约「所有 `fmt` 动词（经 `Format`）输出脱敏文本」与上文「`Key` 按值或按指针格式化都输出脱敏文本」对 `%p` 不成立。fmt 先于 `Format` 处理 `%p`，值不是指针时进入错误动词路径并停用方法调用，按原始字段打印完整令牌（kid、nid、标签）或 32 字节密钥材料；含这两种类型字段的结构体值与数组值（如 `[1]Token`、`[1]Key`）同样如此，`go vet` 不报。本任务按最小修法处理：在上文「脱敏输出」中记为已知局限，`Token.Format`、`Key.String`、`Key.Format` 的注释同步改正；`TestRedaction` 补断言 `%p` 作用于令牌与密钥的指针、切片、映射及含它们的结构体指针时只输出地址。这一断言钉住文档所说的安全边界，不对应某个实现变异。审查建议的可选加固（把 `Key.secret` 改为 `*[KeyLen]byte`，使值上的 `%p` 与作为未导出字段打印时都只输出地址）没有采用：契约写明 `secret [KeyLen]byte`，而且零值 `Key` 会在 `Verify` 与 `BodyDigest` 中解引用空指针，需要另加防护；`Token` 要保持可比较的值语义，无法同样加固。验证：`go test -race` 通过，覆盖率仍为 100%，`-count=5` 稳定；`CGO_ENABLED=0 go test`、全仓 `GOOS=linux go vet` 与 `GOOS=windows go vet`、`make check`（含中文注释检查与 staticcheck，总覆盖率 94.32%）、`make secrets` 均通过。本次修正没有改动 `Parse`，没有重跑 `FuzzParse`。 ③ 复查后补充：已知局限的范围补上数组值；`TestRedaction` 的 `%p` 断言补上以 `Key` 为元素的映射，使文字与测试一致。

### Task 4：正文加密

**Files:**
- Create: `internal/security/payload/payload.go`
- Test: `internal/security/payload/payload_test.go`

```go
// Package payload 用 AES-256-GCM 加密待处理的回复正文与待发通知内容。
// 关联数据把密文绑定到用途、密钥号、任务与序号，密文被调换到别的行时无法解密。
package payload

// Kind 是密文的用途，写入关联数据。
type Kind byte

const (
	// KindReply 是回复正文，序号为 replies.seq。
	KindReply Kind = 'r'
	// KindNotification 是通知内容，序号为 notifications.id。
	KindNotification Kind = 'n'
)

const (
	// FormatV1 是密文首字节，标识当前格式。
	FormatV1 byte = 1
	// KeyLen 是密钥字节数。
	KeyLen = 32
	// Overhead 是密文比明文多出的字节数：格式 1 + nonce 12 + 标签 16。
	Overhead = 29
	// MaxPlaintext 是明文上限。
	MaxPlaintext = 1 << 20
)

// Key 是一把正文加密密钥；String、Format 与 LogValue 一律输出 [redacted payload key]。
type Key struct {
	id   uint8
	aead cipher.AEAD // cipher.NewGCMWithRandomNonce 的结果
}

var (
	// ErrInvalidKey 表示密钥号为 0 或密钥长度不是 32 字节。
	ErrInvalidKey = errors.New("invalid payload key")
	// ErrInvalidInput 表示用途、任务 ID、序号或明文长度不合法。
	ErrInvalidInput = errors.New("invalid payload binding or size")
	// ErrDecrypt 表示密文无法用给定密钥与绑定解密；不区分原因，不含明文或密文。
	ErrDecrypt = errors.New("payload cannot be decrypted")
)

// NewKey 由 32 字节密钥构造 AES-256-GCM（随机 96 位 nonce）；id 须为 1–255。
func NewKey(id uint8, secret []byte) (*Key, error)

// ID 返回密钥号，存储时写入 key_id 列。
func (k *Key) ID() uint8

// Seal 返回 FormatV1 ‖ nonce ‖ 密文 ‖ 标签；taskID 须为 10 位小写 Crockford base32，seq ≥ 1，明文 1 字节到 MaxPlaintext。
func (k *Key) Seal(kind Kind, taskID string, seq int64, plaintext []byte) ([]byte, error)

// Open 校验长度（Overhead+1 到 MaxPlaintext+Overhead，明文至少 1 字节）、格式字节与绑定后解密。零值密钥返回 ErrInvalidKey，
// 用途、任务 ID 或序号不合法返回 ErrInvalidInput，二者都先于长度检查；长度、格式字节与认证失败一律返回 ErrDecrypt。
func (k *Key) Open(kind Kind, taskID string, seq int64, sealed []byte) ([]byte, error)
```

关联数据为 `"turncourier/payload/v1\x00"` ‖ 用途（1 字节）‖ kid（1 字节）‖ 任务 ID（10 字节）‖ 序号（uint64 大端）。nonce 不由序号派生：新建数据库后序号从 1 重新计数，而 Keychain 中的密钥可能仍在，派生会造成 nonce 重复。回复放回队列时保留原序号（Phase 3），密文无需重新加密。

- [x] **Step 1：写失败的测试。**
  - **往返：** 明文 1、1000、`MaxPlaintext` 字节都能往返；密文长度等于明文加 29，首字节为 1。
  - **随机 nonce：** 同样的输入加密两次，第 2–13 字节不同，两份都能解密。
  - **篡改：** 64 字节明文的密文（93 字节），744 个单比特翻转全部返回 `ErrDecrypt`。
  - **调换：** 用其他用途、任务 ID、序号 ±1 解密返回 `ErrDecrypt`；同一密钥材料但 kid 为 2 的密钥返回 `ErrDecrypt`；序号 1 与 2 的两份密文互换后都返回 `ErrDecrypt`；错误的密钥返回 `ErrDecrypt`。
  - **截断：** `sealed[:n]`（n 为 0–29 以及 `len−1`）返回 `ErrDecrypt`，不 panic；长于 `MaxPlaintext+29` 的输入返回 `ErrDecrypt`。
  - **非法输入：** 用途 `'x'`、任务 ID 9 个字符或含 `u`、序号 0 与 −1、明文为空或 `MaxPlaintext+1` 字节时，`Seal` 与 `Open` 返回 `ErrInvalidInput`。`NewKey` 的 id 为 0、密钥 16、31、33 字节返回 `ErrInvalidKey`。
  - **不泄露：** 明文含 `PLAINTEXT-CANARY` 时，所有错误文本不含它；`Key` 按 Task 3 的同一组动词与 slog 处理器格式化，输出不含密钥材料。
- [x] **Step 2：** `go test ./internal/security/payload/` 编译失败。
- [x] **Step 3：实现。** 只用标准库：`aes.NewCipher` 加 `cipher.NewGCMWithRandomNonce`；`Seal` 调用 `aead.Seal(nil, nil, 明文, 关联数据)` 并在前面加格式字节，`Open` 对 `sealed[1:]` 调用 `aead.Open(nil, nil, …, 关联数据)`。
- [x] **Step 4：** `go test -race -cover ./internal/security/payload/` 通过且覆盖率 100%；`CGO_ENABLED=0 go test ./internal/security/payload/` 与 `make secrets` 通过；测试密钥用 `bytes.Repeat` 或按下标填充构造，测试向量遵循「测试向量与密钥扫描」的约定。
- [x] **Step 5：** `git add internal/security/payload && git commit -m "feat(payload): encrypt pending bodies with AES-256-GCM bound to their row"`

**实施说明：** 先写测试，`go test ./internal/security/payload/` 编译失败（`undefined: Key`、`NewKey`、`Kind`、`FormatV1` 等），再实现。实现要点：`NewKey` 先调用 `aes.NewCipher` 与 `cipher.NewGCMWithRandomNonce`，再把密钥号、长度与构造错误合并为一次检查：`aes.NewCipher` 也接受 16 与 24 字节密钥，长度须另查；对 32 字节密钥两个构造函数都不会失败，合并检查使错误仍被处理，又不留下无法执行的分支（覆盖率要求 100%）。`Seal` 以只含格式字节的切片作 `aead.Seal` 的 dst，没有照 Step 3 写成 `aead.Seal(nil, nil, …)` 再在前面加格式字节：输出相同，省去一次最长 1 MiB 的复制。任务 ID 字母表在本包内另写一份，因为本包只用标准库，不导入 `internal/task`。契约与 Step 1 对 `Open` 的错误有一处需合读：契约写「任何失败都返回 `ErrDecrypt`」，Step 1 又要求非法的用途、任务 ID 与序号使 `Open` 返回 `ErrInvalidInput`。现按二者合读实现：绑定参数本身不合法时 `Open` 与 `Seal` 一样返回 `ErrInvalidInput`，并且先于长度检查；长度、格式字节与认证失败一律返回 `ErrDecrypt`。Step 1 中「明文为空或 `MaxPlaintext+1` 字节」只适用于 `Seal`，`Open` 对应的 29 字节输入与超长输入按「截断」一项返回 `ErrDecrypt`。格式化方法用值接收者；`Key` 只持有 AEAD、不保存密钥原文，`%p` 作用于密钥值时 fmt 绕过 `Format`，也只打印内部 AEAD 的地址，因此没有 Task 3 记录的 `%p` 泄露局限，测试对此做了断言。测试方面，契约之外补了三类用例。① 参考实现互通：测试按规格逐段拼接关联数据，直接用 `crypto/aes` 与显式 nonce 的 `cipher.NewGCM` 加解密，与 `Seal`、`Open` 双向互通；kid 取 7，序号取 `0x0102030405060708`，任务 ID 取 `zyxwvtsrqp`，钉住前缀、各段顺序、kid、序号的大端 64 位编码，以及 nonce ‖ 密文 ‖ 标签的布局。`Seal` 与 `Open` 共用同一个关联数据函数，只靠往返测试发现不了这类改错。② 长度边界：用参考实现构造认证有效、但明文为空（29 字节）或为 `MaxPlaintext+1` 字节的密文，断言 `Open` 返回 `ErrDecrypt`。普通的截断与超长输入不论有没有长度检查都会因标签不符而失败，钉不住边界本身。③ 其他：11 位与含大写字母的任务 ID；16 字节密钥，钉住长度检查不能交给 `aes.NewCipher`；非法绑定配空密文，钉住 `ErrInvalidInput` 先于长度检查；导出常量与 `ID()` 以字面量钉住。在仓库副本上做了 52 个变异，每次只改一处，涉及：各导出常量与两种用途值；字母表（字母表与用途校验的缺口见下文审查后修正）；关联数据的前缀与各段（缺失、换序、kid 换成常量、序号改小端或 32 位）；绑定校验的各边界；`Seal` 的明文上下界；`Open` 的长度上下界、格式字节检查与检查顺序；`Seal`、`Open` 不传关联数据；`Open` 透出底层错误；`NewKey` 的密钥号与长度检查；各格式化出口泄露与指针接收者；`ID` 返回常量；把随机 nonce 换成固定 nonce。全部被测试杀死。验证：`go test -race -cover` 通过，覆盖率 100%，`-count=3` 稳定；`CGO_ENABLED=0 go test`、全仓 `GOOS=linux go vet` 与 `GOOS=windows go vet`、`make check`（含中文注释检查与 staticcheck，总覆盖率 94.49%）、`make secrets` 均通过。本任务不涉及钥匙串与网络；测试只在 macOS 上运行，Linux 上的测试由 CI 运行。与 Task 1–3 相同，本任务的提交包含本清单的勾选与实施说明。

**审查后修正：** 变异均在临时副本中逐项施加，每次只改一处。① 上文所说「字母表」变异全部被杀死并不完整：合法任务 ID 只用过 `0123456789`、`0123456788`、`zyxwvtsrqp`，字母 a–h、j、k、m、n 从未作为合法字符出现；非法字符只测过 `u` 与 `A`，而且都在末位。因此字母表中 a 改为 i、m 改为 o、k 改为 l，逐字符检查跳过首位，以及把查表改为接受 i、l、o 与 `:` 的范围判断，这五个变异原先全部存活。现新增 `TestTaskIDAlphabet`：`0123456789`、`abcdefghjk`、`mnpqrstvwx`、`yz01234567` 四个合法 ID 覆盖整个字母表并须往返；`i`、`l`、`o`、`u`、`I`、`L`、`O`、`U`、`:`、`/`、空格、`-`、`_`，以及紧邻小写字母区间两端的 `` ` `` 与 `{`，逐个放在下标 0、5、9，`Seal` 与 `Open` 都须返回 `ErrInvalidInput`。② 用途只用 `'x'` 测过，把用途校验改为「只拒绝 `'x'`」或「接受 `'n'` 到 `'r'` 的区间」的两个变异原先存活。现 `TestInvalidInput` 的非法用途补上 0、`'o'`、`'q'`、`'R'`、`'N'`。实现未改动。修正后上述七个变异全部被杀死；另从字母表逐个删去 32 个字符、分别追加 i、l、o、u，以及跳过末位，共 37 个变异，也全部被杀死；原测试对前述七个变异确实全部存活。验证：`go test -race -cover` 通过，覆盖率仍为 100%，`-count=3` 稳定；`CGO_ENABLED=0 go test`、全仓 `GOOS=linux go vet` 与 `GOOS=windows go vet`、`make check`（含中文注释检查与 staticcheck，总覆盖率 94.49%）、`make secrets` 均通过。 ③ 复查后补充：对未经 `NewKey` 构造的零值 `Key` 调用 `Seal` 或 `Open` 原先会在 `aead` 上 panic，现由 `associatedData` 先返回 `ErrInvalidKey`，与 Task 3 的 `Issue` 一致，`TestInvalidInput` 补断言，去掉该检查的变异使测试失败；`Open` 注释与上文契约同步写明错误划分：零值密钥返回 `ErrInvalidKey`，绑定参数不合法返回 `ErrInvalidInput`，长度、格式字节与认证失败返回 `ErrDecrypt`。

### Task 5：待发通知状态机

**Files:**
- Create: `internal/queue/outbox.go`
- Test: `internal/queue/outbox_test.go`

待发通知与回复队列同属「持久化调度」，放在 `internal/queue`，类型与回复队列分开，互不复用转移表。

```go
// OutboxState 是待发通知的持久化状态名称。
type OutboxState string

const (
	OutboxPending   OutboxState = "PENDING"   // 等待发送，内容密文已落盘
	OutboxSending   OutboxState = "SENDING"   // 已由唯一的发送进程领取，SMTP 会话进行中
	OutboxSent      OutboxState = "SENT"      // 服务器以 250 接受，或本地核对为已投递
	OutboxUncertain OutboxState = "UNCERTAIN" // SMTP 已进入提交阶段（结束标记可能已写出）但未得到响应，或发送中进程崩溃
	OutboxAbandoned OutboxState = "ABANDONED" // 放弃：服务器永久拒绝、任务已关闭，或本地核对后决定不再发送
)

// OutboxEvent 是驱动待发通知状态变化的事件名称。
type OutboxEvent string

const (
	OutboxClaim         OutboxEvent = "claim"
	OutboxDelivered     OutboxEvent = "delivered"
	OutboxMarkUncertain OutboxEvent = "mark_uncertain"
	OutboxRequeue       OutboxEvent = "requeue"
	OutboxAbandon       OutboxEvent = "abandon"
)

// ErrInvalidOutboxTransition 表示待发通知当前状态不接受该事件，或状态名未知。
var ErrInvalidOutboxTransition = errors.New("invalid outbox transition")

// outboxTransitions 是待发通知状态机的唯一事实来源；SENT 与 ABANDONED 为终态。
var outboxTransitions = map[OutboxState]map[OutboxEvent]OutboxState{
	OutboxPending:   {OutboxClaim: OutboxSending, OutboxAbandon: OutboxAbandoned},
	OutboxSending:   {OutboxDelivered: OutboxSent, OutboxMarkUncertain: OutboxUncertain, OutboxRequeue: OutboxPending, OutboxAbandon: OutboxAbandoned},
	OutboxUncertain: {OutboxDelivered: OutboxSent, OutboxRequeue: OutboxPending, OutboxAbandon: OutboxAbandoned},
	OutboxSent:      {},
	OutboxAbandoned: {},
}

// Valid 报告状态名是否属于已知集合。
func (s OutboxState) Valid() bool

// NextOutbox 返回事件发生后的状态；错误包装 ErrInvalidOutboxTransition 并写明状态与事件名。
func NextOutbox(from OutboxState, event OutboxEvent) (OutboxState, error)
```

**各转移的触发条件（由 Task 7 的存储操作执行）：**

| 转移 | 触发 |
| --- | --- |
| PENDING → SENDING | 发送进程领取最早到期的通知；同一时刻全局至多一条 SENDING |
| SENDING → SENT | SMTP 在结束标记后返回 250 |
| SENDING → PENDING | 确定未投递：进入提交阶段之前失败（连接、TLS、认证、MAIL、RCPT、DATA 或分块写正文出错），或结束标记后服务器以 4xx 明确拒绝；按调用方给出的延迟设置下次可发送时间。任务此时已关闭则改走 SENDING → ABANDONED |
| SENDING → UNCERTAIN | 已进入提交阶段（flush 剩余正文、写结束标记、读响应），但没有得到任何响应（超时、断线、取消）；flush 剩余正文时的失败也归入此类，因为无法确定结束标记是否已写出（Task 10）；进程启动时仍为 SENDING 的通知也转为 UNCERTAIN |
| SENDING → ABANDONED | 结束标记后服务器以 5xx 永久拒绝，或认证、MAIL、RCPT 被 5xx 拒绝且调用方判定不可恢复；或确定未投递时任务已关闭（`task_closed`） |
| UNCERTAIN → SENT | 本地核对为已投递（例如 4b 在「已发送」或收件端找到这封信） |
| UNCERTAIN → PENDING | 本地核对为未投递且决定重发，且任务未关闭；只能由人工或 4b 规定的核对流程发起，系统从不自动执行 |
| UNCERTAIN → ABANDONED | 本地核对后决定不再发送，或核对为未投递时任务已关闭 |
| PENDING → ABANDONED | 任务关闭（与关闭任务的同一事务）；领取前发现令牌将在 10 分钟内过期（`expired`）或任务已关闭（兜底）；人工放弃 |

- [x] **Step 1：写失败的测试。** 与 Phase 3 Task 2 相同的方法：测试文件中另写一份期望矩阵，穷举 5 个状态 × 5 个事件；覆盖未知状态、两个终态对所有事件非法、每个已知状态收到未知事件 `"bogus"` 时返回 `ErrInvalidOutboxTransition` 且错误文本含状态名与 `bogus`、`Valid` 的已知与未知输入（`""`、`"pending"`、回复队列的 `"QUEUED"`）。两张转移表互不共用：`NextOutbox(OutboxState("QUEUED"), OutboxClaim)` 与 `Next(State("PENDING"), Claim)` 都返回各自的非法转移错误。
- [x] **Step 2：** `go test ./internal/queue/` 编译失败。
- [x] **Step 3：** 按契约实现。
- [x] **Step 4：** `go test -race -cover ./internal/queue/` 通过且覆盖率 100%。
- [x] **Step 5：** `git commit -m "feat(queue): add outbox state machine for notifications"`

**实施说明：** 先写测试，`go test ./internal/queue/` 编译失败（`undefined: OutboxState`、`OutboxPending`、`OutboxEvent`、`OutboxClaim` 等），再按契约实现。实现与回复队列的 `state.go` 同构：`Valid` 查 `outboxTransitions` 的键，`NextOutbox` 查两级映射，查不到时返回空状态与 `fmt.Errorf("%w: %s --%s-->", ErrInvalidOutboxTransition, from, event)`，未知状态按非法转移处理；回复队列的 `state.go` 未改动。测试在 `outbox_test.go` 中另写一份期望矩阵，穷举 5 个状态 × 5 个事件，非法组合还断言返回的状态为空；此外覆盖：状态与事件的持久化字符串值（Task 6 的表约束依赖它们）、未知状态 `""`、`"pending"`、`"BOGUS"`、每个已知状态收到 `"bogus"` 时错误文本含状态名与 `bogus`、两个终态对全部事件非法、`Valid` 对 5 个已知状态为真而对 `""`、`"pending"`、`"QUEUED"` 为假。「两张转移表互不共用」一项断言：`NextOutbox(OutboxState("QUEUED"), OutboxClaim)` 包装 `ErrInvalidOutboxTransition` 且不包装 `ErrInvalidTransition`；`Next(State("PENDING"), Claim)` 包装 `ErrInvalidTransition` 且不包装 `ErrInvalidOutboxTransition`；`State("PENDING").Valid()` 为假。新测试的辅助函数取名 `assertInvalidOutbox`，回复队列一侧复用已有的 `assertInvalid`。在仓库副本上做了 56 个变异，每次只改一处，涉及：转移表中每条转移的删除与目标改错、各状态上多出规格之外的转移（含终态上增加转移）、删去终态行、表中多出 `""`、`"pending"`、`"QUEUED"` 行；10 个状态与事件的字符串值；`Valid` 恒真、只判非空、改查回复队列的表、两表任一命中即真、先转大写再查；`NextOutbox` 查不到时退回回复队列的表、先转大写再查、错误改包装 `ErrInvalidTransition`、同时包装两个哨兵、哨兵直接取为 `ErrInvalidTransition`、`%w` 改为 `%v`、错误文本缺状态名或事件名、出错时返回原状态。全部被测试杀死（其中 3 个初次施加时因未使用或缺少导入而编译失败，补齐导入后重跑，均被杀死）。验证：`go test -race -cover ./internal/queue/` 通过，覆盖率 100%；`CGO_ENABLED=0 go test ./internal/queue/`、全仓 `GOOS=linux go vet` 与 `GOOS=windows go vet`、`make check`（含中文注释检查与 staticcheck，总覆盖率 94.52%）、`make secrets` 均通过。本任务不涉及钥匙串、网络与机密形状的测试向量。与 Task 1–4 相同，本任务的提交包含本清单的勾选与实施说明。 复查后补充：`TestOutboxPersistedNames` 钉住哨兵文本 `invalid outbox transition`，使排查时能从错误文本分辨是哪一张转移表报的错。

### Task 6：迁移 0002（含入站记录的文件夹列）、secure_delete 与实例、密钥元数据

**Files:**
- Create: `internal/store/sqlite/migrations/0002_mail.sql`、`internal/store/sqlite/instance.go`、`internal/store/sqlite/wal.go`
- Modify: `internal/store/sqlite/store.go`（连接参数、打开后检查点）、`internal/store/sqlite/replies.go`（`InboundReply.Folder`、`RecordReply` 按文件夹查找）、`internal/store/sqlite/migrate_test.go`、`internal/store/sqlite/store_test.go`、`internal/store/sqlite/replies_test.go`、`internal/store/sqlite/dispatch_test.go`、`tests/integration/lifecycle_test.go`
- Test: `internal/store/sqlite/instance_test.go`、`internal/store/sqlite/wal_test.go`、`internal/store/sqlite/schema_test.go`

**入站记录的文件夹列（维护者 2026-09-19 确认方案 (a)，记录见文末「已确认事项」第 1 项）：** `0001` 以 `UNIQUE (account, uid_validity, uid)` 标识一封已收取的邮件，没有文件夹；而 UID 只在同一文件夹内唯一（RFC 3501）。本迁移给 `inbound_messages` 增加 `folder` 列，UID 唯一键改为 `(account, folder, uid_validity, uid)`，保留 `UNIQUE (account, message_id)`：同一封信在 INBOX 与 Junk 各有一份时，仍按 Message-ID 判为重复，Phase 3 的防重复投递没有放宽。`0001_init.sql` 不改。

- **为什么两张表一起重建：** SQLite 不能删除或修改表级唯一约束，只能重建表。`replies.inbound_id` 以外键引用 `inbound_messages`，而迁移在事务中执行，事务内不能关闭 `foreign_keys`。只重建 `inbound_messages` 并借助 `PRAGMA defer_foreign_keys = ON` 不可行：`DROP TABLE inbound_messages` 的隐式 DELETE 会为每条引用它的回复记下一次延迟外键违例，之后的 RENAME 不会抵消这一计数，有回复行时 COMMIT 报 `FOREIGN KEY constraint failed`（审查时在副本中复现）。修改 `migrate.go`、让该迁移在事务外关闭 `foreign_keys` 也能做到，但会改动 Phase 3 的迁移框架，不采用。
- **步骤（位于 `0002` 开头，在 `reply_payloads` 与各触发器之前，此时库中还没有引用这两张表的触发器）：**
  1. 建 `inbound_messages_new`（带 folder），复制数据，folder 填 `INBOX`；建 `replies_new`，其外键指向 `inbound_messages_new (id)`，复制数据；
  2. 把两张旧表在 `sqlite_sequence` 中的值复制给新表，保留 AUTOINCREMENT 序列（显式插入只会把序列设为现有最大 id，已删除行的 id 会被重新分配）；
  3. 先 `DROP TABLE replies`（子表），再 `DROP TABLE inbound_messages`，此时已没有引用旧父表的行；
  4. `ALTER TABLE inbound_messages_new RENAME TO inbound_messages`（`replies_new` 中的外键随之改写），再 `ALTER TABLE replies_new RENAME TO replies`（`sqlite_sequence` 中的名称随重命名更新）；
  5. 重建 `replies_one_in_flight` 与 `replies_by_task_state` 两个索引。

  编写本清单时已用 sqlite3 3.51 在副本中以 `foreign_keys = ON` 的 IMMEDIATE 事务执行下文完整的 `0002`：版本 1 的库含 2 个任务、5 条入站记录与 QUEUED、DISPATCHING、UNCERTAIN、REJECTED、ACKNOWLEDGED 各一条回复，且各有一行已删除使序列大于最大 id；可以提交，`PRAGMA foreign_key_check` 无输出，两张表的序列不回退，`replies` 的外键指向 `inbound_messages`，同一任务第二条在途回复仍被拒绝，与已有行 (account, uid_validity, uid) 相同但 folder 为 `Junk` 的入站记录可以插入，不带 folder 的插入报 `NOT NULL constraint failed`，重建后的 `replies` 上正文触发器照常工作。空库、以及行已全部删除但 `sqlite_sequence` 仍有记录的库同样可以升级，后者的序列得到保留。实施时仍以 Step 1 的测试为准。
- **对 Phase 3 代码的连带影响：** folder 列 NOT NULL 且没有默认值，`0002` 一落地，Phase 3 `RecordReply` 不带 folder 的插入就会失败，因此 `InboundReply.Folder`、`RecordReply` 的查找与插入、Phase 3 测试的辅助函数都在本任务内修改（见下文契约与 Step 3），不留到 Task 8。

`migrations/0002_mail.sql`（完整内容）：

```sql
-- 0002：邮件闭环（Phase 4a）。0001 已发布，不修改。
-- inbound_messages.body_sha256 的列名沿用 0001，自本版本起保存键控摘要 HMAC-SHA256（phase-04.md Task 8）；
-- 本迁移之前写入的行只可能来自测试或内部调用，保留原值不改写。

-- 入站记录增加文件夹：UID 只在同一文件夹内唯一（RFC 3501），唯一键改为 (account, folder, uid_validity, uid)；
-- 按 Message-ID 的唯一键不变，同一封信在 INBOX 与 Junk 的两份副本仍判为重复。迁移在事务中执行，不能关闭外键，
-- 而 replies 以外键引用 inbound_messages，因此两张表一起重建：先删子表再删父表，删除时已没有引用旧父表的行。
-- 这一段必须位于本文件开头，在任何引用这两张表的触发器之前。
CREATE TABLE inbound_messages_new (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    account       TEXT NOT NULL CHECK (length(account) BETWEEN 3 AND 254),
    folder        TEXT NOT NULL CHECK (length(folder) BETWEEN 1 AND 255),
    uid_validity  INTEGER NOT NULL CHECK (uid_validity BETWEEN 1 AND 4294967295),
    uid           INTEGER NOT NULL CHECK (uid BETWEEN 1 AND 4294967295),
    message_id    TEXT NOT NULL CHECK (length(message_id) BETWEEN 3 AND 998),
    body_sha256   BLOB NOT NULL CHECK (length(body_sha256) = 32),
    task_id       TEXT NOT NULL REFERENCES tasks (id),
    received_at   INTEGER NOT NULL,
    UNIQUE (account, folder, uid_validity, uid),
    UNIQUE (account, message_id)
) STRICT;

INSERT INTO inbound_messages_new (id, account, folder, uid_validity, uid, message_id, body_sha256, task_id, received_at)
SELECT id, account, 'INBOX', uid_validity, uid, message_id, body_sha256, task_id, received_at FROM inbound_messages;

CREATE TABLE replies_new (
    seq            INTEGER PRIMARY KEY AUTOINCREMENT,
    inbound_id     INTEGER NOT NULL UNIQUE REFERENCES inbound_messages_new (id),
    task_id        TEXT NOT NULL REFERENCES tasks (id),
    state          TEXT NOT NULL CHECK (state IN ('QUEUED', 'DISPATCHING', 'ACKNOWLEDGED', 'UNCERTAIN', 'REJECTED')),
    resume_state   TEXT CHECK (resume_state IS NULL OR resume_state IN ('COMPLETED', 'WAITING_INPUT')),
    reject_reason  TEXT CHECK (reject_reason IS NULL OR reject_reason IN ('task_not_accepting', 'task_failed', 'task_closed')),
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
) STRICT;

INSERT INTO replies_new (seq, inbound_id, task_id, state, resume_state, reject_reason, created_at, updated_at)
SELECT seq, inbound_id, task_id, state, resume_state, reject_reason, created_at, updated_at FROM replies;

-- 保留 AUTOINCREMENT 序列：显式插入只会把新表的序列设为现有最大 id；旧表从未有过行时 sqlite_sequence 中没有它的记录，不复制。
DELETE FROM sqlite_sequence WHERE name IN ('inbound_messages_new', 'replies_new');
INSERT INTO sqlite_sequence (name, seq)
SELECT name || '_new', seq FROM sqlite_sequence WHERE name IN ('inbound_messages', 'replies');

DROP TABLE replies;
DROP TABLE inbound_messages;
ALTER TABLE inbound_messages_new RENAME TO inbound_messages;
ALTER TABLE replies_new RENAME TO replies;

CREATE UNIQUE INDEX replies_one_in_flight ON replies (task_id) WHERE state IN ('DISPATCHING', 'UNCERTAIN');
CREATE INDEX replies_by_task_state ON replies (task_id, state, seq);

CREATE TABLE instance (
    singleton    INTEGER PRIMARY KEY CHECK (singleton = 1),
    instance_id  TEXT NOT NULL CHECK (length(instance_id) = 16 AND instance_id NOT GLOB '*[^0-9abcdefghjkmnpqrstvwxyz]*'),
    created_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE crypto_keys (
    purpose     TEXT NOT NULL CHECK (purpose IN ('token', 'payload')),
    kid         INTEGER NOT NULL CHECK (kid BETWEEN 1 AND 255),
    state       TEXT NOT NULL CHECK (state IN ('active', 'retired', 'destroyed')),
    key_check   BLOB NOT NULL CHECK (length(key_check) = 8),
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (purpose, kid)
) STRICT;

CREATE UNIQUE INDEX crypto_keys_one_active ON crypto_keys (purpose) WHERE state = 'active';

CREATE TABLE notifications (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id               TEXT NOT NULL REFERENCES tasks (id),
    event                 TEXT NOT NULL CHECK (event IN ('turn_completed', 'waiting_input', 'waiting_approval', 'failed')),
    nid                   BLOB NOT NULL UNIQUE CHECK (length(nid) = 12),
    message_id            TEXT NOT NULL UNIQUE CHECK (length(message_id) BETWEEN 3 AND 998),
    delivered_message_id  TEXT UNIQUE CHECK (delivered_message_id IS NULL OR length(delivered_message_id) BETWEEN 3 AND 998),
    token_kid             INTEGER NOT NULL CHECK (token_kid BETWEEN 1 AND 255),
    token_expires_at      INTEGER NOT NULL,
    state                 TEXT NOT NULL CHECK (state IN ('PENDING', 'SENDING', 'SENT', 'UNCERTAIN', 'ABANDONED')),
    abandon_reason        TEXT CHECK (abandon_reason IS NULL OR abandon_reason IN ('rejected', 'task_closed', 'expired', 'manual')),
    attempts              INTEGER NOT NULL CHECK (attempts >= 0),
    not_before            INTEGER NOT NULL,
    sent_at               INTEGER,
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL,
    CHECK (token_expires_at > created_at),
    CHECK ((state = 'ABANDONED') = (abandon_reason IS NOT NULL)),
    CHECK ((state = 'SENT') = (sent_at IS NOT NULL)),
    CHECK (delivered_message_id IS NULL OR state = 'SENT')
) STRICT;

-- 串行发送：全局至多一条 SENDING。
CREATE UNIQUE INDEX notifications_one_sending ON notifications (state) WHERE state = 'SENDING';
CREATE INDEX notifications_pending ON notifications (not_before, id) WHERE state = 'PENDING';
CREATE INDEX notifications_by_task ON notifications (task_id, id);

-- 令牌的 MAC 输入与我方 Message-ID 一经写入不可修改；实际投递的 Message-ID 只能写入一次。
CREATE TRIGGER notifications_binding_immutable
BEFORE UPDATE OF task_id, event, nid, message_id, token_kid, token_expires_at, created_at ON notifications
BEGIN SELECT RAISE(ABORT, 'notification binding is immutable'); END;

CREATE TRIGGER notifications_delivered_id_once
BEFORE UPDATE OF delivered_message_id ON notifications
WHEN OLD.delivered_message_id IS NOT NULL
BEGIN SELECT RAISE(ABORT, 'delivered message id is already recorded'); END;

CREATE TABLE notification_payloads (
    notification_id  INTEGER PRIMARY KEY REFERENCES notifications (id),
    key_id           INTEGER NOT NULL CHECK (key_id BETWEEN 1 AND 255),
    sealed           BLOB NOT NULL CHECK (length(sealed) BETWEEN 30 AND 1048605)
) STRICT;

CREATE TABLE reply_payloads (
    seq      INTEGER PRIMARY KEY REFERENCES replies (seq),
    key_id   INTEGER NOT NULL CHECK (key_id BETWEEN 1 AND 255),
    sealed   BLOB NOT NULL CHECK (length(sealed) BETWEEN 30 AND 1048605)
) STRICT;

-- 只有待处理的正文落盘：通知在 PENDING 时写入，回复在 QUEUED 时写入；密文不可改写。
CREATE TRIGGER notification_payloads_pending_only
BEFORE INSERT ON notification_payloads
WHEN (SELECT state FROM notifications WHERE id = NEW.notification_id) IS NOT 'PENDING'
BEGIN SELECT RAISE(ABORT, 'notification payload requires a PENDING notification'); END;

CREATE TRIGGER notification_payloads_immutable
BEFORE UPDATE ON notification_payloads
BEGIN SELECT RAISE(ABORT, 'payloads are immutable'); END;

CREATE TRIGGER reply_payloads_queued_only
BEFORE INSERT ON reply_payloads
WHEN (SELECT state FROM replies WHERE seq = NEW.seq) IS NOT 'QUEUED'
BEGIN SELECT RAISE(ABORT, 'reply payload requires a QUEUED reply'); END;

CREATE TRIGGER reply_payloads_immutable
BEFORE UPDATE ON reply_payloads
BEGIN SELECT RAISE(ABORT, 'payloads are immutable'); END;

-- 进入终态的同一事务中删除正文；UNCERTAIN 保留，因为核对为未送达时要按原序号放回。
CREATE TRIGGER notifications_drop_payload
AFTER UPDATE OF state ON notifications
WHEN NEW.state IN ('SENT', 'ABANDONED')
BEGIN DELETE FROM notification_payloads WHERE notification_id = NEW.id; END;

CREATE TRIGGER replies_drop_payload
AFTER UPDATE OF state ON replies
WHEN NEW.state IN ('ACKNOWLEDGED', 'REJECTED')
BEGIN DELETE FROM reply_payloads WHERE seq = NEW.seq; END;

CREATE TABLE fetch_cursors (
    account       TEXT NOT NULL CHECK (length(account) BETWEEN 3 AND 254),
    folder        TEXT NOT NULL CHECK (length(folder) BETWEEN 1 AND 255),
    uid_validity  INTEGER NOT NULL CHECK (uid_validity BETWEEN 1 AND 4294967295),
    last_uid      INTEGER NOT NULL CHECK (last_uid BETWEEN 0 AND 4294967295),
    updated_at    INTEGER NOT NULL,
    PRIMARY KEY (account, folder)
) STRICT;

CREATE TABLE inbound_rejections (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    account       TEXT NOT NULL CHECK (length(account) BETWEEN 3 AND 254),
    folder        TEXT NOT NULL CHECK (length(folder) BETWEEN 1 AND 255),
    uid_validity  INTEGER NOT NULL CHECK (uid_validity BETWEEN 1 AND 4294967295),
    uid           INTEGER NOT NULL CHECK (uid BETWEEN 1 AND 4294967295),
    message_id    TEXT CHECK (message_id IS NULL OR length(message_id) BETWEEN 3 AND 998),
    sender        TEXT CHECK (sender IS NULL OR length(sender) BETWEEN 3 AND 254),
    reason        TEXT NOT NULL CHECK (length(reason) BETWEEN 1 AND 40 AND reason NOT GLOB '*[^a-z_]*'),
    task_id       TEXT REFERENCES tasks (id),
    received_at   INTEGER NOT NULL,
    UNIQUE (account, folder, uid_validity, uid)
) STRICT;

CREATE INDEX inbound_rejections_by_time ON inbound_rejections (received_at, id);
```

表中没有明文正文、授权码、密钥或令牌列。被拒来信只有元数据：发件人地址属于元数据，只在本地保存，用于提示「合法用户的回复被拒」；不保存主题与正文。

**连接参数与检查点：** `connectionParams` 在现有参数后追加 `&_pragma=secure_delete(1)`（不用 `FAST`，它对溢出页无效）。`wal.go` 提供 `truncateWAL`：TRUNCATE 检查点会调用忙等待处理，在 `busy_timeout(5000)` 下，其他进程只要持有读事务就会先等 5 秒，而存储只有一个连接，这 5 秒内同进程的全部操作都被阻塞。因此它用 `db.Conn` 取得这唯一的连接，依次执行 `PRAGMA busy_timeout = 0`、`PRAGMA wal_checkpoint(TRUNCATE)`、`PRAGMA busy_timeout = 5000`（与 `connectionParams` 相同的常量）。恢复失败时让该连接作废（`Conn.Raw` 的回调返回 `driver.ErrBadConn`），下一次操作按连接参数新建连接。结果行的 `busy` 为 0 时视为完成；出错或忙时不返回错误、不重试，由下一次清理或下一次打开再做。`Open` 在迁移完成后调用一次。之后每个会让正文进入终态的操作在提交后调用一次（Task 7、8）。不把 VACUUM 当作常规手段；APFS 快照与 Time Machine 中的残留不在 SQLite 能控制的范围内（写入 Task 14 的文档）。

```go
// KeyPurpose 是 crypto_keys 中密钥的用途。
type KeyPurpose string

const (
	KeyPurposeToken   KeyPurpose = "token"
	KeyPurposePayload KeyPurpose = "payload"
)

// KeyState 是密钥的生命周期状态；4a 只登记 active，retired 与 destroyed 留给以后的轮换命令。
type KeyState string

const (
	KeyActive    KeyState = "active"
	KeyRetired   KeyState = "retired"
	KeyDestroyed KeyState = "destroyed"
)

// ErrKeyExists 表示该用途已登记过密钥（4a 每个用途只登记 kid=1）。
var ErrKeyExists = errors.New("key already registered")

// EnsureInstance 返回实例 ID；不存在时从随机源读取 10 字节，编码为 16 位小写 Crockford base32 后写入，created 为 true。
// 在一个 IMMEDIATE 事务中先读后写，多个进程同时调用时只有一个写入，其余读到同一个值。
func (s *Store) EnsureInstance(ctx context.Context) (id string, created bool, err error)

// InstanceID 读取实例 ID；尚未运行 init 时返回 ErrNotFound。
func (s *Store) InstanceID(ctx context.Context) (string, error)

// RegisterKey 把 (purpose, kid) 登记为 active，并保存 keychain.KeyCheck 算出的校验值；kid 须为 1–255。
// 该用途已有任何密钥时返回 ErrKeyExists。调用方必须先把密钥写入 Keychain 并读回核对，再登记。
// 元数据只说明该密钥曾被登记；使用前须读出 Keychain 中的密钥并与 KeyCheckOf 比对，不符即拒绝使用。
func (s *Store) RegisterKey(ctx context.Context, purpose KeyPurpose, kid uint8, check [8]byte) error

// KeyCheckOf 返回 (purpose, kid) 登记的校验值；未登记时返回 ErrNotFound。
func (s *Store) KeyCheckOf(ctx context.Context, purpose KeyPurpose, kid uint8) ([8]byte, error)

// ActiveKeyID 返回该用途 active 密钥的 kid；没有时返回 ErrNotFound。
func (s *Store) ActiveKeyID(ctx context.Context, purpose KeyPurpose) (uint8, error)

// KeyStateOf 返回 (purpose, kid) 的状态；未登记时返回 ErrNotFound。
func (s *Store) KeyStateOf(ctx context.Context, purpose KeyPurpose, kid uint8) (KeyState, error)
```

存储层不依赖 `keychain`，也不接触密钥材料：校验值由调用方计算后传入，比对也由调用方（`init` 与 4b 的启动流程）完成。

实例 ID 的编码复用 `id.go` 中的 `crockfordAlphabet`（`base32.NewEncoding(crockfordAlphabet).WithPadding(base32.NoPadding)`，10 字节恰为 16 个字符）。

**入站回复带上文件夹（`replies.go`）：**

```go
// InboundReply 新增 Folder，其余字段不变（Task 8 再增加正文，并把摘要字段改名为 BodyDigest）。
type InboundReply struct {
	TaskID      string
	Account     string   // 调用方已用 config.NormalizeAddress 规范化的机器人邮箱地址
	Folder      string   // 取回该邮件的文件夹，4b 传入 imap.Batch.Folder；1–255 个字符（按字符计，与表约束一致），不含 NUL，可以含空格
	UIDValidity uint32
	UID         uint32
	MessageID   string   // 与邮件头一致的原样字符串
	BodySHA256  [32]byte
}

// RecordReply 的去重与拒绝规则不变，只是按 UID 的查找与插入带上文件夹：同一账户、同一文件夹中 (UIDVALIDITY, UID)
// 已有记录，或同一账户中 Message-ID 已有记录时，Message-ID、正文摘要与任务都一致才算重复，返回原回复且不写入；
// 任一不一致返回 ErrMessageConflict。按 Message-ID 的查找不含文件夹，同一封信在 INBOX 与 Junk 各有一份时判为重复。
func (s *Store) RecordReply(ctx context.Context, in InboundReply) (RecordResult, error)
```

`validate` 在开始事务前检查 Folder，规则与已有字段相同：按 Unicode 字符计长度，含 NUL 一律拒绝（SQLite 的 `length()` 遇到 NUL 即停止计数）。IMAP 文件夹名可以含空格（例如 `Sent Messages`），因此不按空白拒绝。存储层不限定文件夹的取值，INBOX、Junk 等名称由调用方传入。

- [x] **Step 1：写失败的测试（`migrate_test.go`、`schema_test.go`）。**
  - 空库迁移后 `user_version = 2`；`sqlite_schema` 中的表、索引与触发器集合与 0001 加 0002 的全部名称精确相等（原「四张表加三个索引」的断言改为新集合，实施说明记录名称清单）。
  - 使用真实迁移、断言版本为 1 的既有用例同样改为 2：`migrate_test.go` 的 `TestMigrateTwiceIsNoop`（`user_version` 与 `SchemaVersion` 两处）、`TestMigrateRereadsVersionInsideTransaction`（持锁事务只应用 0001，本次迁移在事务内读到 1 后跳过 0001、执行 0002），以及 `store_test.go` 的 `TestOpenConcurrentlyOnNewDirectory`（断言与函数注释）。行为改变的用例同步改写函数注释：`TestMigrateRereadsVersionInsideTransaction` 改为「跳过已提交的 0001、再执行 0002」，`TestMigrateEmptyDatabase` 改为版本 2 及 0002 新增的表、索引与触发器。`TestMigrateRollsBackFailedMigration` 使用自己的 `fstest.MapFS`，版本仍为 1，不改。
  - **升级路径：** 用只含嵌入的 `0001_init.sql` 的 `fstest.MapFS` 迁移出版本 1 的库，写入多个任务、多条入站记录，以及 QUEUED、DISPATCHING、UNCERTAIN、ACKNOWLEDGED、REJECTED 各状态的回复（DISPATCHING 与 UNCERTAIN 分属不同任务），再删除最新的一条回复及其入站记录，使两张表的序列都大于各自的最大 id；关闭后用真实迁移 `Open`：
    - 版本为 2；`tasks`、`replies` 与 `inbound_messages` 逐行逐字段不变（新增的 folder 为 `INBOX`），`replies.inbound_id` 的关联不变；`reply_payloads` 为空；
    - `PRAGMA foreign_key_check` 无输出；`replies` 的外键指向 `inbound_messages`；`sqlite_schema` 中不残留 `inbound_messages_new`、`replies_new`；
    - `replies_one_in_flight` 仍然生效（同一任务再插入一条 DISPATCHING 失败）；
    - 两张表在 `sqlite_sequence` 中的值与升级前相同，升级后新插入行的 id 与 seq 大于升级前的序列值；
    - 两张表的定义除 folder 与 UID 唯一键外与 `0001` 逐字相同：升级前从版本 1 的库读出 `inbound_messages`、`replies` 与两个 `replies` 索引在 `sqlite_schema.sql` 中的文本（即 `0001_init.sql` 中对应语句去掉结尾分号），升级后再读一次。RENAME 把表名改写为带双引号的形式（`CREATE TABLE "inbound_messages" (`、`REFERENCES "inbound_messages" (id)`、`CREATE TABLE "replies" (`），比较前只去掉这两个表名两侧的双引号。期望值由升级前的文本构造：`inbound_messages` 在 account 列那一行之后插入 folder 列那一行，并把 `UNIQUE (account, uid_validity, uid)` 换成 `UNIQUE (account, folder, uid_validity, uid)`，两处替换各须恰好命中一次；`replies` 与两个索引不变。升级后的文本与期望值整体相等。文本相等即覆盖列、类型、非空、主键、外键、唯一约束、STRICT 与全部 CHECK 约束。Phase 3 没有直接插入非法值来触发这两张表 CHECK 约束的测试，重建时漏写或抄错一条 CHECK 只能由这一比较发现（编写本清单时已用 sqlite3 3.51 在副本中核对，上文 `0002` 与 `0001` 的差异恰为上述几处）；
    - 升级后以 `Folder: "INBOX"` 与原有行相同的 UIDVALIDITY、UID、Message-ID、摘要和任务调用 `RecordReply`，返回 `Duplicate = true`，证明旧行按 INBOX 参与去重。
  - **没有数据的版本 1 库：** 库中从未有过行（`sqlite_sequence` 中没有这两张表的记录）时升级同样成功，两张表在 `sqlite_sequence` 中仍没有记录；两张表的行已全部删除、`sqlite_sequence` 仍有记录时，升级后序列值不变，新插入行的 id 与 seq 大于它（只用 UPDATE 复制序列的实现会在这里丢失序列）。
  - 版本 99 的库仍返回 `ErrSchemaTooNew`；注入 `0003_bad.sql` 的失败迁移使版本停在 2。
  - **约束：** 下列直接插入都失败：`instance` 第二行（singleton 为 2）；`instance_id` 为 15 个字符或含 `u`；`crypto_keys` 的 kid 为 0 或 256、`key_check` 为 7 字节、同一用途两条 active；两张正文表的 `sealed` 为 29 字节（30 字节可以插入）；`notifications` 的 `abandon_reason` 为 `bogus`（`expired` 可以）、nid 为 11 字节、状态为 `BOGUS`、状态为 PENDING 但带 `abandon_reason`、状态为 SENT 但 `sent_at` 为空、状态为 PENDING 但带 `delivered_message_id`、`token_expires_at` 等于 `created_at`、第二条 SENDING；`fetch_cursors` 的 `last_uid` 为 −1；`inbound_rejections` 的 `reason` 为 `Bad Reason`、41 个字符或空字符串；`inbound_messages` 不带 folder、folder 为空字符串或 256 个字符，以及同一 (account, folder, uid_validity, uid) 的第二行（同一 (account, uid_validity, uid) 但 folder 不同的一行可以插入）。
  - **触发器：** 为 ACKNOWLEDGED 回复或不存在的序号插入 `reply_payloads` 失败；更新 `reply_payloads` 失败；回复状态改为 ACKNOWLEDGED 或 REJECTED 时正文行被删除，改为 DISPATCHING 或 UNCERTAIN 时保留；通知对应的四条规则同样成立；`UPDATE notifications SET nid = …` 与 `SET token_expires_at = …` 失败；`delivered_message_id` 第二次写入失败。
- [x] **Step 2：写失败的测试（`store_test.go`、`instance_test.go`、`wal_test.go`）。**
  - `PRAGMA secure_delete` 为 1；原有 `journal_mode`、`foreign_keys`、`synchronous` 断言不变。
  - `EnsureInstance`：随机源为 10 个零字节时得到 `0000000000000000`、`created = true`；再次调用返回同一值、`created = false`，且不再读取随机源（第二次注入的读取器一读就报错）；随机源不足 10 字节时报错且不写入；两个独立 `Open` 的存储各在 10 个 goroutine 中同时调用，结果全部相同且恰有一次 `created = true`。
  - `InstanceID` 在 `EnsureInstance` 之前返回 `ErrNotFound`。
  - `RegisterKey(token, 1, c)` 后 `ActiveKeyID(token)` 为 1、`KeyStateOf(token, 1)` 为 active、`KeyCheckOf(token, 1)` 等于 `c`；再次登记返回 `ErrKeyExists` 且校验值不变；`KeyStateOf(token, 2)`、`KeyCheckOf(token, 2)` 与 `ActiveKeyID(payload)` 返回 `ErrNotFound`；kid 为 0 或未知用途在开始事务前报错。
  - `truncateWAL`：写入若干行后 `-wal` 文件大小大于 0，调用后返回 true 且大小为 0；另一个连接持有读事务时在 200ms 内返回 false、不报错，之后在同一存储上查询 `PRAGMA busy_timeout` 仍为 5000；读事务结束后再次调用返回 true。去掉「临时设为 0」的变异使耗时断言失败（约 5 秒）。
  - **打开时检查点：** 存储 A 写入若干行并保持打开，打开同一目录的存储 B 后 `-wal` 文件大小为 0。
- [x] **Step 3：修改 Phase 3 测试并写失败的测试（`replies_test.go`、`dispatch_test.go`、`tests/integration/lifecycle_test.go`）。**
  - **机械修改：** `replies_test.go` 的 `inbound` 辅助函数补 `Folder: "INBOX"`（`dispatch_test.go` 的 `enqueueReplies` 与 `tasks_test.go` 都经它构造回复，无需另改）；`dispatch_test.go` 中 `TestInFlightUniqueIndex` 直接插入 `inbound_messages` 的语句补上 folder 列，取 `'INBOX'`；`lifecycle_test.go` 的 `syntheticReply` 补 `Folder: "INBOX"`。只做这些修改，断言不变；修改后在旧实现上编译失败（`InboundReply` 没有 `Folder`）。Phase 3 原有的去重、冲突、拒绝与原子性用例全部照旧通过。
  - **校验：** `TestRecordReplyValidation` 的表中增加 Folder 为空、256 个字符、含 NUL 三行，在开始事务前报错；Folder 为 `Sent Messages`（含空格）与 255 个字符时可以记录。
  - **不同文件夹、相同 UID：** INBOX 与 Junk 中 UIDVALIDITY 与 UID 都相同、Message-ID 与摘要不同的两封邮件都能入队，得到两个不同的 seq，都为 QUEUED。`RecordReply` 按 UID 查找时去掉文件夹的变异使第二封返回 `ErrMessageConflict`，本用例失败。
  - **同一封信的两份副本：** 同一 Message-ID、同一摘要与任务，先以 (INBOX, uv 7, uid 1)、再以 (Junk, uv 7, uid 5) 记录：第二次返回 `Duplicate = true` 与第一次的回复，`inbound_messages` 与 `replies` 都没有新增行。按 Message-ID 查找时加上文件夹的变异会漏掉已有记录，第二封因 `UNIQUE (account, message_id)` 而插入失败，本用例失败。同一 Message-ID 从 Junk 取回但摘要不同时返回 `ErrMessageConflict`，与同一文件夹内的规则一致。
- [x] **Step 4：** 测试失败。
- [x] **Step 5：实现。** 按上文。迁移文件名 `0002_mail.sql` 符合现有命名规则，无需改 `migrate.go`。`RecordReply` 的 UID 查找条件改为 `account = ? AND folder = ? AND uid_validity = ? AND uid = ?`，插入语句写入 folder，按 Message-ID 的查找不变。
- [x] **Step 6：** `go test -race ./internal/store/sqlite/ ./tests/...` 通过；`CGO_ENABLED=0 go test ./internal/store/sqlite/`、`GOOS=windows go vet ./internal/store/sqlite/` 与 `make secrets` 通过；`RegisterKey` 的校验值与直接插入的 `key_check`（截断的 HMAC 输出）用字节数组字面量或 `bytes.Repeat` 构造，直接插入时以绑定参数传入、不写成 SQL 十六进制字面量，遵循「测试向量与密钥扫描」的约定。
- [x] **Step 7：** `git commit -m "feat(store): add migration 0002 with folder-aware inbound keys, instance, key metadata and secure delete"`

**实施说明：** 先写测试，`go test ./internal/store/sqlite/ ./tests/...` 编译失败（`unknown field Folder in struct literal of type sqlite.InboundReply`、`store.EnsureInstance undefined`、`store.InstanceID undefined`、`store.RegisterKey undefined`、`undefined: KeyPurposeToken` 等），再实现。`0002_mail.sql` 从本清单逐字取出，与上文代码块逐行比对一致。迁移后 `sqlite_schema` 中的名称清单（`TestMigrateEmptyDatabase` 按此精确比较）：表 `tasks`、`task_events`、`inbound_messages`、`replies`、`instance`、`crypto_keys`、`notifications`、`notification_payloads`、`reply_payloads`、`fetch_cursors`、`inbound_rejections`；索引 `task_events_by_task`、`replies_one_in_flight`、`replies_by_task_state`、`crypto_keys_one_active`、`notifications_one_sending`、`notifications_pending`、`notifications_by_task`、`inbound_rejections_by_time`；触发器 `notifications_binding_immutable`、`notifications_delivered_id_once`、`notification_payloads_pending_only`、`notification_payloads_immutable`、`reply_payloads_queued_only`、`reply_payloads_immutable`、`notifications_drop_payload`、`replies_drop_payload`。测试辅助函数 `schemaObjects` 随之把触发器计入，`TestOpenRejectsNewerSchema` 等前后比较的用例因此同时覆盖触发器。实现要点：`store.go` 新增字符串常量 `busyTimeoutMillis = "5000"`，`connectionParams` 与 `truncateWAL` 的恢复语句共用它，`&_pragma=secure_delete(1)` 追加在参数串末尾；`truncateWAL(ctx, db) bool` 是包内函数，按契约依次执行三条 PRAGMA，设置为 0 失败时不做检查点，但仍执行恢复，恢复失败时以 `Conn.Raw` 回调返回 `driver.ErrBadConn` 作废连接，这一分支由 `TestTruncateWALDiscardsConnOnRestoreFailure` 覆盖（见下文审查后修正）；`EnsureInstance` 与 `InstanceID` 共用按 `rowQuerier` 读取的 `instanceID`，已存在时直接返回、不读随机源；`RegisterKey` 在开始事务前校验用途与 kid，事务内以 `count(*)` 判断该用途是否已有任何密钥；`KeyCheckOf` 用 `copy` 取出 8 字节，表被外部改坏时只会得到不相符的校验值，使调用方拒绝使用该密钥，不会 panic。清单之外的一处改动：`tasks.go` 中 `ErrNotFound` 的注释从「任务或回复」改为「任务、回复、实例 ID 或密钥元数据」，因为新增的方法也返回它；`tasks.go` 不在本任务的文件清单中，只改了这一行注释。测试方面，契约之外补了：`EnsureInstance` 另用按 5 位分组依次为 0–15 的 10 个字节（期望 `0123456789abcdef`）与全 `0xff`（期望 16 个 `z`）钉住字母表与位序；`RegisterKey` 以另一 kid 再次登记同样返回 `ErrKeyExists`，另一用途的登记与查询互不影响，大写用途按未知用途拒绝；约束用例逐条断言错误文本（`CHECK constraint failed`、`NOT NULL constraint failed: inbound_messages.folder` 或具体的 `UNIQUE constraint failed: …`），每组非法行旁都有只差一处的合法行；绑定不可变触发器对全部 7 列各做一次写回原值的更新；Folder 为 255 个非 ASCII 字符时可以记录，钉住按字符计长；升级路径比较整张 `sqlite_sequence`，失败的 `0003_bad.sql` 之后结构仍与版本 2 相同。测试向量：`key_check` 与 `RegisterKey` 的校验值用 `bytes.Repeat` 或 `[8]byte{…}` 字面量构造并以绑定参数传入，nid 与密文用 `bytes.Repeat`，没有使用 `gitleaks:allow`。在仓库副本上做了 36 个变异，每次只改一处，涉及：`RecordReply` 按 UID 查找去掉文件夹、按 Message-ID 查找加上文件夹、插入时把 folder 写成常量；Folder 校验改按字节计、去掉长度校验、上限改为 256、去掉 NUL 检查、按空白拒绝；重建的 `inbound_messages` 与 `replies` 各漏一条 CHECK；不复制序列、只用 UPDATE 复制序列、不重建 `replies_one_in_flight`；正文触发器保留 REJECTED、删除 UNCERTAIN 的正文、保留 ABANDONED；绑定触发器漏 `created_at`；去掉「实际投递 Message-ID 只写一次」；去掉 `secure_delete` 或改为 `FAST`；`Open` 不做检查点；`truncateWAL` 不临时关闭忙等待（`TestTruncateWAL` 约 5.1 秒后失败）、不恢复忙等待、忽略 busy、改用 PASSIVE；实例已存在仍读随机源、随机源改为至少读 1 字节、改用标准 base32 字母表、已存在时 created 为 true；`RegisterKey` 不查已有密钥、允许 kid 0、不校验用途、登记为 retired；`KeyCheckOf`、`KeyStateOf` 忽略 kid，`ActiveKeyID` 不按用途过滤。全部被测试杀死（其中两个初次施加有误：`s.random.Read` 因 `io` 未使用而编译失败，改为 `io.ReadAtLeast(…, 1)` 后由 `TestEnsureInstanceShortRandom` 杀死；PASSIVE 的替换串在注释中也出现一次，改为只替换语句后被 `TestTruncateWAL` 与 `TestOpenTruncatesWAL` 杀死）。验证：`go test -race ./internal/store/sqlite/ ./tests/...` 通过，新增用例以 `-race -count=10` 重复运行稳定；`CGO_ENABLED=0 go test ./internal/store/sqlite/`、`GOOS=windows go vet ./internal/store/sqlite/`、全仓 `GOOS=windows go vet` 与 `GOOS=linux go vet`、`make check`（含中文注释检查与 staticcheck，总覆盖率 93.76%，本包 86.4%）、`make secrets` 均通过。本任务不涉及钥匙串与网络；测试只在 macOS 上运行，Linux 上的测试由 CI 运行。与 Task 1–5 相同，本任务的提交包含本清单的勾选与实施说明。

**审查后修正：** 变异均在临时副本中逐项施加，每次只改一处。① 上文原称恢复 busy_timeout 失败的分支「难以稳定触发」，不成立；该分支原先没有测试，去掉 `Conn.Raw` 作废连接的变异存活。这一分支在实际中会走到：调用方的 ctx 在检查点与恢复之间被取消即可，此时忙等待为 0 的唯一连接若回到连接池，此后争锁的操作都会立即返回 SQLITE_BUSY。现新增 `TestTruncateWALDiscardsConnOnRestoreFailure`：以 `sql.OpenDB` 与测试内的 `driver.Connector` 包装 modernc 驱动（不注册全局驱动名），包装后的连接对恢复语句 `PRAGMA busy_timeout = 5000` 返回注入的错误，其余语句原样转发，关闭时计数；按 `connectionParams` 打开、单连接、Ping 之后调用 `truncateWAL`，断言返回 false、连接恰好关闭一次、随后 `PRAGMA busy_timeout` 读回 5000。② `RegisterKey` 在该用途已有任何状态的密钥时返回 `ErrKeyExists`、`ActiveKeyID` 只返回 active 的 kid，这两条契约原先没有测试钉住：计数改为只数 active，以及 `ActiveKeyID` 去掉状态过滤（改为 `ORDER BY kid` 或 `ORDER BY kid DESC LIMIT 1`）的变异全部存活。现新增 `TestRegisterKeyNonActive`：对 retired 与 destroyed 各以绑定参数直接插入一条 kid 为 1 的 payload 密钥（`key_check` 用 `bytes.Repeat` 构造），断言 `ActiveKeyID(payload)` 返回 `ErrNotFound`、`KeyStateOf(payload, 1)` 读回该状态、`RegisterKey(payload, 2, …)` 返回 `ErrKeyExists` 且 `crypto_keys` 行数仍为 1。③ 上文「升级路径比较整张 `sqlite_sequence`」原写作「含 `task_events`」，与事实不符：升级测试的版本 1 数据不写 `task_events`，`sqlite_sequence` 中只有 `inbound_messages` 与 `replies` 两条记录，已删去该括注。实现未改动。修正后上述四个变异全部被杀死。验证：`go test -race -count=10` 重复运行两个新用例稳定；`CGO_ENABLED=0 go test ./internal/store/sqlite/`、全仓 `GOOS=windows go vet` 与 `GOOS=linux go vet`、`make check`（含中文注释检查与 staticcheck，总覆盖率 93.98%，本包 87.0%）、`make secrets` 均通过。 复查后补充：`TestOpenTruncatesWAL` 增加新目录用例，断言迁移之后 WAL 为 0 字节；把检查点挪到迁移之前的变异使 WAL 留下约 177 KB 的迁移帧，测试失败。

### Task 7：待发通知的存储

**Files:**
- Create: `internal/store/sqlite/notifications.go`
- Modify: `internal/store/sqlite/store.go`（`Options.PayloadKey`）、`internal/store/sqlite/tasks.go`（关闭任务时放弃待发通知、提交后检查点）
- Test: `internal/store/sqlite/notifications_test.go`、`internal/store/sqlite/residue_test.go`

```go
// Options 新增 PayloadKey：进程启动时从 Keychain 读出的正文密钥。为 nil 时，
// 所有需要加密或解密正文的操作返回 ErrPayloadKeyUnavailable（「Keychain 读取失败时拒绝发送与派发」）。
type Options struct {
	Now        func() time.Time
	Random     io.Reader
	PayloadKey *payload.Key
}

// NewNotification 是创建待发通知的输入。Content 是 4b 渲染通知所需的内容（格式由 4b 定义），只以密文落盘。
type NewNotification struct {
	TaskID  string
	Event   string        // turn_completed、waiting_input、waiting_approval、failed
	Domain  string        // 机器人地址的域名，用作 Message-ID 右侧；1–253 个 [a-z0-9.-] 字符
	TTL     time.Duration // 令牌有效期，调用方传 config.Security.TokenTTL；1h–720h
	Content []byte        // 1 字节到 payload.MaxPlaintext
}

// Notification 是待发通知的持久化快照，不含内容。
type Notification struct {
	ID                 int64
	TaskID             string
	Owner              string // 任务的 owner，与 TaskID、TokenExpiresAt 一起构成令牌的 Claims
	Event              string
	NID                [12]byte
	MessageID          string // 我方 Message-ID，创建时生成，重试不变
	DeliveredMessageID string // 实际投递的 Message-ID；来源由 L1 决定（D4），未知时为空
	TokenKeyID         uint8
	TokenExpiresAt     time.Time
	State              queue.OutboxState
	AbandonReason      string // rejected、task_closed、expired、manual
	Attempts           int
	NotBefore          time.Time
	SentAt             time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

var (
	// ErrPayloadKeyUnavailable 表示没有正文密钥，或密文的 key_id 与当前密钥不符、该 kid 未登记为 active。
	ErrPayloadKeyUnavailable = errors.New("payload key is not available")
	// ErrPayloadMissing 表示待处理的通知或回复没有正文行（例如 0002 之前写入的回复）。
	ErrPayloadMissing = errors.New("pending payload is missing")
	// ErrNoSendableNotification 表示已有通知在发送中，或没有到期的 PENDING 通知。
	ErrNoSendableNotification = errors.New("no sendable notification")
	// ErrTaskNotNotifiable 表示任务已关闭，不再创建通知。
	ErrTaskNotNotifiable = errors.New("task does not accept notifications")
	// ErrDeliveredMessageIDConflict 表示实际投递的 Message-ID 已记录为别的值，或已属于另一条通知。
	ErrDeliveredMessageIDConflict = errors.New("delivered message id conflict")
)

// CreateNotification 在一个事务中：确认任务存在且未关闭（FAILED 任务允许，用于发送失败通知）；读取令牌用途的 active kid
// （没有时返回包装 ErrNotFound 的错误）；确认正文密钥的 kid 已登记为 active；从随机源生成 12 字节 nid 与 15 字节
// Message-ID 随机部分；以 PENDING、attempts 0、not_before 与 created_at 为当前时间、token_expires_at 为当前时间加 TTL
// 插入通知行；再用 (KindNotification, TaskID, id) 加密内容并插入 notification_payloads。nid 或 Message-ID 冲突时报错，不重试。
func (s *Store) CreateNotification(ctx context.Context, n NewNotification) (Notification, error)

// ClaimNextNotification 分两个事务执行。第一个事务把以下 PENDING 通知改为 ABANDONED（经 queue.NextOutbox 校验）：
// token_expires_at 不晚于当前时间加 10 分钟的（expired，令牌发出后已来不及回复）；所属任务已 CLOSED 的（task_closed，
// 正常路径不会产生这种行，这是兜底）。有改动时提交后执行检查点。第二个事务：已有 SENDING 通知时返回
// ErrNoSendableNotification；取 not_before 不晚于当前时间、按 (not_before, id) 最早的 PENDING 通知；读出并解密内容，
// 失败时不领取（返回 ErrPayloadMissing、ErrPayloadKeyUnavailable 或包装 payload.ErrDecrypt 的错误）；
// 成功时改为 SENDING、attempts 加 1，返回快照与明文内容。从不领取 UNCERTAIN 通知。
func (s *Store) ClaimNextNotification(ctx context.Context) (Notification, []byte, error)

// MarkNotificationSent 在 SMTP 返回 250 后把 SENDING 改为 SENT 并记录 sent_at；内容由触发器删除，提交后执行检查点。
// 任务在发送期间被关闭时照样记为 SENT：邮件已经发出。
func (s *Store) MarkNotificationSent(ctx context.Context, id int64) (Notification, error)

// MarkNotificationUncertain 在 SMTP 已进入提交阶段、结束标记可能已写出但没有得到响应时（smtp.ErrUncertain）把 SENDING 改为 UNCERTAIN；内容保留。
func (s *Store) MarkNotificationUncertain(ctx context.Context, id int64) (Notification, error)

// RequeueNotification 在确定未投递时把 SENDING 改回 PENDING，not_before 设为当前时间加 delay（0 到 24 小时）；
// 任务已关闭时改为 ABANDONED(task_closed)，内容由触发器删除，提交后执行检查点（与 ResolveUncertainNotification 一致）。
func (s *Store) RequeueNotification(ctx context.Context, id int64, delay time.Duration) (Notification, error)

// AbandonNotification 把 PENDING、SENDING 或 UNCERTAIN 通知改为 ABANDONED；reason 只能是 rejected 或 manual
// （task_closed 与 expired 只由存储自身写入）。内容由触发器删除，提交后执行检查点。
func (s *Store) AbandonNotification(ctx context.Context, id int64, reason string) (Notification, error)

// ResolveUncertainNotification 记录本地核对结果：delivered 为 true 时 UNCERTAIN 改为 SENT；为 false 时改回 PENDING
// （not_before 为当前时间），任务已关闭时改为 ABANDONED(task_closed)。进入 SENT 或 ABANDONED 时内容由触发器删除，
// 提交后执行检查点。系统从不自动调用它。
func (s *Store) ResolveUncertainNotification(ctx context.Context, id int64, delivered bool) (Notification, error)

// RecordDeliveredMessageID 为 SENT 通知记录实际投递的 Message-ID（3–998 个字符，不含 NUL）；同值重复记录不改动，
// 已有不同值或该值已属于另一条通知时返回 ErrDeliveredMessageIDConflict；通知不是 SENT 时返回 queue.ErrInvalidOutboxTransition。
func (s *Store) RecordDeliveredMessageID(ctx context.Context, id int64, messageID string) (Notification, error)

// RecoverSendingNotifications 在发送进程启动时调用：把所有 SENDING 改为 UNCERTAIN，返回全部 UNCERTAIN 供本地核对，
// 从不重新发送。与 RecoverInFlight 相同，只能由唯一的发送进程在开始发送之前调用。
func (s *Store) RecoverSendingNotifications(ctx context.Context) ([]Notification, error)

// NotificationByNID 按 nid 读取通知，供 4b 验证令牌时取得 Claims；不存在时返回 ErrNotFound。
func (s *Store) NotificationByNID(ctx context.Context, nid [12]byte) (Notification, error)
```

所有状态变化先经 `queue.NextOutbox` 计算，更新语句带 `WHERE id = ? AND state = ?`，受影响行数不为 1 时回滚，与 Phase 3 的回复操作一致。`ApplyTaskEvent(close)` 在同一事务中把该任务的 PENDING 通知改为 ABANDONED(`task_closed`)（经 `queue.NextOutbox` 校验），SENDING 与 UNCERTAIN 不变：它们的结果尚未确定，之后由 `RequeueNotification` 或 `ResolveUncertainNotification(false)` 在确认未投递时改为 ABANDONED(`task_closed`)，已关闭任务的通知因此不会再回到 PENDING；`fail` 不影响通知。`close` 与 `fail` 提交后调用 `truncateWAL`（排队回复的正文随拒绝被触发器删除）。按实际投递 Message-ID 查找通知属于线程规则，留到 4b，`delivered_message_id` 的唯一约束已为其提供索引。

- [x] **Step 1：写失败的测试。** 存储以固定时钟、固定随机源与测试正文密钥打开，并登记 `(token, 1)`、`(payload, 1)`（存储层不比对校验值，测试传入固定的 8 字节）；任务 T 为 RUNNING。
  - **创建：** 返回 PENDING、`Attempts = 0`、`NotBefore` 为当前时间、`TokenExpiresAt` 为当前时间加 TTL（毫秒精度）、`TokenKeyID = 1`、`Owner = "local"`；`MessageID` 匹配 `^<tc\.[0-9abcdefghjkmnpqrstvwxyz]{24}@example\.invalid>$`；NID 等于随机源给出的字节。直接查询 `notification_payloads.sealed` 不含内容中的金丝雀文本，用 `payload.Open(KindNotification, T, id, …)` 能还原内容。
  - **创建时的校验（在开始事务前返回，数据库无新行）：** 未知事件、域名含大写字母、含 `_` 或为空、TTL 为 59 分钟或 721 小时、内容为空或超过上限、任务 ID 格式错误。事务内：任务不存在返回 `ErrNotFound`；任务 CLOSED 返回 `ErrTaskNotNotifiable`；任务 FAILED 可以创建；没有 active 令牌密钥返回包装 `ErrNotFound` 的错误；`PayloadKey` 为 nil 或其 kid 未登记返回 `ErrPayloadKeyUnavailable`；在 `notification_payloads` 上创建 `RAISE(ABORT)` 触发器后创建失败，且 `notifications` 行数不变。
  - **领取：** 依次创建通知 1、2；领取得到 1，内容与原文相同、`Attempts = 1`；1 为 SENDING 时再次领取返回 `ErrNoSendableNotification`；`RequeueNotification(1, 30s)` 后立即领取得到 2；时钟推进 30 秒并把 2 标记为 SENT 后领取得到 1。去掉 `notification_payloads_immutable` 触发器后翻转密文一个字节，领取返回包装 `payload.ErrDecrypt` 的错误，通知仍为 PENDING；删除正文行后领取返回 `ErrPayloadMissing`；`PayloadKey` 为 nil 时返回 `ErrPayloadKeyUnavailable`。三种情况都不改动任何行。
  - **结果：** SENT 后 `SentAt` 为当前时间、正文行已删除，再次标记返回 `queue.ErrInvalidOutboxTransition`；对 PENDING 调用 `MarkNotificationSent` 同样报错。UNCERTAIN 保留正文；`ResolveUncertainNotification(true)` 改为 SENT、正文行已删除；`(false)` 改回 PENDING 且能再次领取到相同内容；任务关闭后 `(false)` 改为 ABANDONED(`task_closed`)、正文行已删除。`RequeueNotification` 的延迟为 −1 秒或超过 24 小时时报错。
  - **放弃：** PENDING、SENDING、UNCERTAIN 各一条以 `rejected` 或 `manual` 放弃成功、正文行被删除；原因为 `task_closed`、`expired` 或 `bogus` 时报错；对 SENT 调用返回 `queue.ErrInvalidOutboxTransition`。
  - **关闭任务：** 任务 T 有 PENDING、SENDING、UNCERTAIN 各一条通知与两条排队回复；`close` 后 PENDING 通知为 ABANDONED(`task_closed`)，另两条不变，回复按 Phase 3 规则被拒绝；另一个任务执行 `fail` 后它的 PENDING 通知不变。在 `notifications` 上创建 `BEFORE UPDATE … RAISE(ABORT)` 触发器后执行 `close` 失败，任务、回复与通知都保持原状（与 Phase 3 的 `TestTaskEventsRollBackOnFailure` 同一写法）。
  - **关闭之后：** 通知领取为 SENDING 后关闭任务，再 `RequeueNotification(id, 0)`：状态为 ABANDONED(`task_closed`)、正文行已删除，随后领取返回 `ErrNoSendableNotification`；另一用例中，SENDING 通知在任务关闭后 `MarkNotificationSent` 仍记为 SENT。绕过 API 直接把已关闭任务的一条通知改回 PENDING 后领取：该通知被改为 ABANDONED(`task_closed`)，领取返回 `ErrNoSendableNotification`。
  - **即将过期：** TTL 为 1 小时的通知在时钟推进 49 分 59 秒后仍被领取。另一用例先创建 A（TTL 1 小时），时钟推进 50 分钟（A 距过期恰 10 分钟）后创建 B：领取返回 B，A 为 ABANDONED(`expired`)、正文行已删除；只有 A 时领取返回 `ErrNoSendableNotification`，A 同样被放弃。
  - **恢复：** 领取后直接关闭存储，重新打开后 `RecoverSendingNotifications` 返回该通知且为 UNCERTAIN；再次调用结果相同；之后仍能领取其他 PENDING 通知（UNCERTAIN 不阻塞发送），且反复领取直到 `ErrNoSendableNotification` 的过程中从不返回这条 UNCERTAIN 通知。
  - **实际投递 ID：** SENT 通知记录 `<tencent_synthetic@example.invalid>` 成功，同值再次记录不改动，另一个值返回 `ErrDeliveredMessageIDConflict`；另一条 SENT 通知记录同一值返回 `ErrDeliveredMessageIDConflict`；PENDING 通知返回 `queue.ErrInvalidOutboxTransition`；值为 2 个字符或含 NUL 时在开始事务前报错。
  - **串行：** 两个独立 `Open` 的存储在 20 个 goroutine 中同时领取，每轮恰有 1 个成功，其余返回 `ErrNoSendableNotification`，没有 SQLITE_BUSY 泄漏，循环 20 轮；绕过 API 直接插入第二条 SENDING 违反 `notifications_one_sending`。
  - `NotificationByNID` 找到已有通知；未知 nid 返回 `ErrNotFound`。
- [x] **Step 2：写失败的测试（`residue_test.go`）。** 辅助函数 `assertAbsentOnDisk(t, dataDir, needle)` 读取数据目录中的数据库文件与 `-wal` 文件，断言都不含 `needle`。用例分两种内容大小各跑一遍：64 字节（单元格在页内），以及 64 KiB（占用溢出页链，真实通知超过约 4 KB 时即是如此）。创建通知，读出其密文 `sealed`，领取并标记 SENT；之后数据库文件与 `-wal` 文件中都找不到 `sealed` 的任何 32 字节片段。两个对照组都在另一目录用测试内拼接的连接串直接打开数据库（不经 `Open`），执行同样的插入、删除与 TRUNCATE 检查点：去掉 `secure_delete` 时，两种大小都能在数据库文件中找到密文片段；改用 `secure_delete(FAST)` 时，64 KiB 内容的密文片段必须仍能找到。后者证明测试能区分 ON 与 FAST（D5：FAST 对溢出页无效），实施说明记录两组对照的实测结果。
- [x] **Step 3：** 测试失败。
- [x] **Step 4：实现。** 每个方法一个事务，结构与 Phase 3 的 `changeReply` 相同；随机部分用 `s.random`；时间以 UTC 毫秒存储。
- [x] **Step 5：** `go test -race -count=3 ./internal/store/sqlite/` 通过，无抖动；包覆盖率 ≥ 85%；`make secrets` 通过。测试正文密钥用 `bytes.Repeat` 或按下标填充构造，登记用的 8 字节校验值同 Task 6，测试向量遵循「测试向量与密钥扫描」的约定。
- [x] **Step 6：** `git commit -m "feat(store): persist notifications with encrypted content and no blind resend"`

**实施说明：** 先写测试，`go test ./internal/store/sqlite/` 编译失败（`unknown field PayloadKey in struct literal of type Options`、`undefined: NewNotification`、`s.CreateNotification undefined` 等），再实现。实现要点：① `store.go` 增加 `Options.PayloadKey` 与 `Store.payloadKey`，`Open` 原样保存。② 单条通知的变化都经 `changeNotification`，结构与 `changeReply` 相同：一个事务内读取通知及其任务，经 `nextNotification`（先核对该操作要求的来源状态，再调用 `queue.NextOutbox`）计算，`updateNotification` 带 `WHERE id = ? AND state = ?`，受影响行数不为 1 时返回错误使事务回滚；通知因此进入 SENT 或 ABANDONED 时提交后调用 `truncateWAL`。各操作只接受契约规定的来源状态：`MarkNotificationSent`、`MarkNotificationUncertain`、`RequeueNotification` 只接受 SENDING，`ResolveUncertainNotification` 只接受 UNCERTAIN，`AbandonNotification` 接受 PENDING、SENDING、UNCERTAIN（终态由 `queue.NextOutbox` 拒绝），不符时返回包装 `queue.ErrInvalidOutboxTransition` 的错误。批量放弃（领取前的 expired 与 task_closed、`close` 时的 task_closed）经 `abandonPending` 执行，目标状态同样由 `queue.NextOutbox(PENDING, abandon)` 算出，写法与 Phase 3 `close`、`fail` 批量拒绝回复相同。③ 创建：开始事务前校验事件、域名（`^[a-z0-9.-]{1,253}$`）、有效期、内容长度与任务 ID（由 `crockfordAlphabet` 构造的 10 位正则），错误文本以 `invalid notification` 开头、不含内容；事务内依次检查任务、令牌用途的 active kid、正文密钥。随机源以 `io.ReadFull` 一次读 27 字节，前 12 字节为 nid，后 15 字节用实例 ID 的同一编码 `instanceEncoding`（小写 Crockford base32、无填充）得到 24 个字符；时间以 UTC 毫秒存储，`token_expires_at` 为当前毫秒加 `TTL.Milliseconds()`。正文密钥的检查集中在 `activePayloadKey`：`PayloadKey` 为 nil、其 kid 未登记或状态不是 active 时返回 `ErrPayloadKeyUnavailable`，Task 8 可复用。④ 领取：第一个事务先放弃 expired、再放弃 task_closed，没有改动时回滚、不做检查点；第二个事务按契约执行（审查后在选取条件中增加令牌到期条件，内容无法读出时返回该通知的快照，见「审查后修正」），读取内容的顺序为：正文行缺失返回 `ErrPayloadMissing`，正文密钥不可用或密文 `key_id` 与当前密钥不符返回 `ErrPayloadKeyUnavailable`，解密失败返回包装 `payload.ErrDecrypt` 的错误，错误文本不含内容与密文。⑤ `RecordDeliveredMessageID` 先查状态，同值直接返回，已有不同值或该值已属于另一条通知时返回 `ErrDeliveredMessageIDConflict`；写入时同时更新 `updated_at`（契约未规定，按「记录有改动即更新」处理，同值重复记录不改动），条件带 `state = 'SENT' AND delivered_message_id IS NULL`；它不改变状态，不做检查点。⑥ 快照的 `Owner` 由 `JOIN tasks` 读取任务的当前 owner，表中不另存。⑦ `tasks.go`：`applyEvent` 在 `close` 时于同一事务中放弃该任务的 PENDING 通知；`close` 与 `fail` 提交后调用 `truncateWAL`。清单之外的一处改动：`instance.go` 中 `ActiveKeyID`、`KeyStateOf` 的查询提取为接受 `rowQuerier` 的 `activeKeyID`、`keyStateOf`（与已有的 `instanceID` 同一写法），供创建与领取在事务内读取密钥元数据：存储只有一个连接，事务内再经 `s.db` 查询会一直等待空闲连接。导出方法的行为不变；`instance.go` 不在本任务的文件清单中。

测试方面：存储以 UTC+8 的固定时钟、固定种子的 `math/rand/v2` ChaCha8 随机源与 `bytes.Repeat` 构造的 kid 1 测试正文密钥打开，登记 `(token, 1)`、`(payload, 1)`，校验值为 `[8]byte` 字面量。需要断言 nid 与 Message-ID 时改用 `bytes.Reader` 注入确定的字节：15 字节按 5 位分组依次为 0–23，期望的 Message-ID 在运行时由 `crockfordAlphabet[:24]` 拼接。契约之外补了：Message-ID 的已知答案；修改任务 owner 后 `NotificationByNID` 读到新值；把 payload 的 active 密钥换成 kid 2 后，正文行的 `key_id` 为 2 且能领取；nid 冲突与 Message-ID 冲突各只触发一条唯一约束，随机源剩余字节证明冲突后不重试；随机源不足 27 字节时报错；CREATED 任务可以创建；各操作在错误来源状态上的调用；`not_before` 恰到与差 1 毫秒两侧；`not_before` 较早的通知即使 id 较大也先被领取；FAILED 任务的通知放回后仍为 PENDING；同一 `BEFORE UPDATE` 故障触发器下 `fail` 照常成功（`fail` 不改动通知）；对 9 个操作注入 `RAISE(ABORT)` 与 `RAISE(IGNORE)` 验证整体回滚（其中 `ClaimNextNotification` 一组的故障只在第二个事务中触发：该组没有要放弃的通知，第一个事务的更新不命中任何行；领取前放弃的回滚见「审查后修正」）；上下文已取消时各方法返回 `context.Canceled`，而延迟、放弃原因与实际投递 ID 的校验仍先于开始事务；每个应做检查点的操作（`MarkNotificationSent`、`AbandonNotification`、`ResolveUncertainNotification` 两种结果、`RequeueNotification` 放弃、领取前放弃、`close`、`fail`）之后 WAL 为 0 字节。`residue_test.go` 中 `assertAbsentOnDisk(t, dataDir, needle)` 接受多个 needle，数据库文件与 WAL 文件只读一次；「`sealed` 的任何 32 字节片段」按逐字节滑动实现：把 `sealed` 的全部 32 字节窗口放进集合，再逐字节扫描数据库文件与 WAL 文件查表，64 KiB 密文的 65534 个窗口扫描约 200 KB 的文件只需数毫秒；短于 32 字节的 needle 按整体查找（Task 8 查找明文金丝雀时用到）。对照组在 `t.TempDir()` 中用测试内拼接的连接串打开数据库，执行迁移后构造存储，走同样的创建、领取、`MarkNotificationSent`（触发器删除正文并执行 TRUNCATE 检查点），并先断言 `PRAGMA secure_delete` 分别为 0 与 2。实测：经 `Open`（ON）时，64 字节与 64 KiB 两种大小在数据库文件与 WAL 文件中都找不到任何片段；去掉 `secure_delete` 时，数据库文件含 64 字节的 62/62 个片段、64 KiB 的 64974/65534 个片段；`FAST` 时 64 字节为 0/62、64 KiB 为 64522/65534；四组的 WAL 都是 0 字节。

在仓库副本上做了 88 个变异，每次只改一处，涉及：各项输入校验的边界与字符集；CLOSED、FAILED 的判定；令牌 kid 的用途；正文密钥的 nil、未登记与非 active 检查；nid 与 Message-ID 的取字节位置、前缀、到期时间与 `not_before`；加密的用途、序号与 `key_id`；领取的 SENDING 检查、到期比较、排序、attempts、`key_id` 核对、解密用途与缺行错误；领取前放弃的边界、比较符、原因、task_closed 兜底、提交与检查点；批量放弃的状态条件；各操作的来源状态、`sent_at`、`not_before`、延迟边界、关闭任务时的放弃与原因、放弃原因白名单；实际投递 ID 的长度、NUL、计长方式、状态检查、同值、冲突检查与 `updated_at`；恢复的标记与排序；受影响行数检查；`owner` 来源；`close` 放弃通知的条件、范围与原因；`close`、`fail` 后的检查点；`Open` 不保存正文密钥。其中 5 个初次施加时因变量或导入未使用而编译失败（`s.random.Read`、`if true`、去掉 NUL 检查、按字节计长、恢复时不写回），改为可编译的等价写法（`io.ReadAtLeast(…, 1)`、`>= 0`、`ContainsRune(…, 1)`、`len` 加 `RuneCountInString(messageID[:0])`、`_, _ = next, now`）后全部被杀死；另补一个 `owner` 按错误列 LEFT JOIN 的变异，同样被杀死。存活 2 个，均为等价变异：`MarkNotificationUncertain` 的来源状态改为通知当前状态（转移表只允许 SENDING 执行 `mark_uncertain`，其他状态仍由 `queue.NextOutbox` 拒绝）；查询的 JOIN 条件追加恒真的 `AND 1`。其余全部被测试杀死。验证：`go test -race -count=3 ./internal/store/sqlite/` 通过且无抖动，本包覆盖率 87.7%；`CGO_ENABLED=0 go test ./internal/store/sqlite/`、`go test ./tests/...`、全仓 `GOOS=windows go vet` 与 `GOOS=linux go vet`、`make check`（含中文注释检查与 staticcheck，总覆盖率 93.12%）、`make secrets` 均通过。测试向量只有 `bytes.Repeat` 构造的密钥、`[8]byte` 与 `[]byte{…}` 字面量，没有使用 `gitleaks:allow`。本任务不涉及钥匙串与网络；测试只在 macOS 上运行，Linux 上的测试由 CI 运行。与 Task 1–6 相同，本任务的提交包含本清单的勾选与实施说明。

**审查后修正：** 变异均在临时副本中逐项施加，每次只改一处。① 领取前放弃（第一个事务）原先没有故障注入测试，上文「9 个操作整体回滚」对它不成立：忽略 expired 或 task_closed 一步的错误、task_closed 一步出错时先提交再返回 nil、`ClaimNextNotification` 吞掉第一个事务的错误继续领取，这四个变异全部存活。现新增 `TestClaimAbandonRollsBackOnFailure`：准备一条距过期恰 10 分钟的通知、一条被绕过 API 改回 PENDING 的已关闭任务的通知和一条正常通知，分别以 `WHEN NEW.abandon_reason = 'expired'` 与 `'task_closed'` 的 `RAISE(ABORT)` 触发器让对应一步失败，断言领取返回注入的错误、所有通知与正文行保持原状（没有通知变为 SENDING）。② 领取前的 task_closed 兜底原先只有 RUNNING 任务的领取用例，兜底条件放宽为 `state IN (?, 'FAILED')` 或 `state != 'RUNNING' OR state = ?` 的变异存活。现新增 `TestClaimNotificationsOfOpenTasks`：任务分别进入 WAITING_INPUT、WAITING_APPROVAL、COMPLETED、FAILED 后创建对应事件的通知，断言按创建顺序依次领取成功、标记 SENT 后正文行被删除。复查后补充：放宽到 `state IN (?, 'DELIVERY_UNCERTAIN')` 与 `state IN (?, 'CREATED')` 的变异仍然存活。前者是真实场景：任务在 COMPLETED 时创建 turn_completed 通知，派发回复回到 RUNNING，再经 `MarkReplyUncertain` 进入 DELIVERY_UNCERTAIN，此时通知仍为 PENDING，兜底一旦放宽，它会在领取前被静默放弃。该用例现补两种情况：`completedTask` 后创建 turn_completed 通知、派发回复并标为不确定，断言任务为 DELIVERY_UNCERTAIN 后该通知仍被领取；CREATED 任务的通知同样被领取。放宽到这两种状态以及前述四种状态、`state != 'RUNNING' OR state = ?` 的七个变异都被杀死。③ 残留检查改为按 Step 2 的原意逐字节滑动（见上文），原先的首尾相接切分会漏掉 32–62 字节、跨越片段边界的残留；新增 `TestFragmentsIn` 用互不相同的合成字节验证任意起点的 32 字节残留都能发现、31 字节不算命中、短 needle 按整体查找，把窗口改回按 32 字节步进的变异被它杀死。④ 契约之外的偏差：第二个事务的选取条件增加 `token_expires_at > 当前时间 + 10 分钟`。两个事务之间连接会释放，另一进程此时可以经 `ResolveUncertainNotification(false)` 把令牌只剩几分钟的通知放回 PENDING，时间本身也会前进；只靠第一个事务放弃，第二个事务仍可能领取这样的通知，违反「领取前放弃令牌将在 10 分钟内过期的 PENDING 通知」。加上条件后，这类通知留在 PENDING，下一次领取时由第一个事务放弃（expired）。`TestClaimAbandonsExpiringNotifications` 增加用例：时钟每次读取前进 1 毫秒，第一个事务读到距过期 10 分钟加 1 毫秒、第二个事务读到恰 10 分钟，断言该通知既不被放弃也不被领取，下一次领取时改为 ABANDONED(expired)；去掉该条件或把 `>` 改为 `>=` 的变异都被杀死。⑤ 契约之外的补充：内容无法读出时，领取返回该通知未改动的 PENDING 快照（内容为 nil）与原有错误，而不是零值。「风险与后续」写明这样的通知会阻塞全部发送、处置靠 `AbandonNotification(manual)`，而 4a 没有列出 PENDING 通知的接口，原先 id 只出现在错误文本中；`TestClaimNextNotificationUnreadablePayload` 改为断言返回的快照等于领取前的快照，改回返回零值的变异被杀死。修正后上述变异全部被杀死；覆盖率显示第一个事务两处错误返回都已执行，只剩提交失败分支未覆盖。验证：`go test -race -count=3 ./internal/store/sqlite/` 通过且无抖动，本包覆盖率 88.0%；`make check`（含中文注释检查与 staticcheck，总覆盖率 93.24%）、全仓 `GOOS=windows go vet` 与 `GOOS=linux go vet`、`make secrets` 均通过。 复查后补充：`ErrNotFound` 的注释补上通知；`TestClaimAbandonsExpiringNotifications` 的局部函数 `hourly` 补中文注释。

### Task 8：回复正文密文与键控摘要

**Files:**
- Modify: `internal/store/sqlite/replies.go`、`internal/store/sqlite/replies_test.go`、`internal/store/sqlite/dispatch_test.go`、`internal/store/sqlite/tasks_test.go`、`tests/integration/lifecycle_test.go`
- Test: `tests/integration/payload_test.go`

**D5 第 2 条的落地：**

- **计算对象：** 4b 解析器输出、将被加密入队的新正文 UTF-8 字节，与 `Body` 完全相同。不对原始 MIME 计算：同一 Message-ID 的不同投递（例如同时进入 INBOX 与 Junk 的两份副本）带有不同的 `Received` 等头部，对原始 MIME 计算会把它们误报为冲突。（重新 FETCH 同一份存储副本得到的字节相同，不是问题所在。）代价是摘要依赖解析器输出的确定性，见「风险与后续」。
- **密钥来源：** 令牌签名密钥，取该回复所引用通知的 `token_kid`，经 `token.Key.BodyDigest` 以独立前缀做 HMAC（Task 3）。不新增 Keychain 条目，D3 的三类条目不变。能到达 `RecordReply` 的回复必然带有效令牌，所以对应 kid 的密钥此时一定可用；该 kid 被销毁后令牌也全部失效，回复在验证阶段就被拒绝，不会出现用不同密钥比较同一封信的情况。
- **与去重、冲突检测的关系：** 判定规则沿用 Task 6：同一账户同一文件夹中 (UIDVALIDITY, UID)，或同一账户中 Message-ID 已有记录时，Message-ID、摘要与任务都一致才算重复，否则返回 `ErrMessageConflict`。摘要只做字节比较，存储层不知道密钥，也不重新计算。同一封信总是引用同一条通知（令牌相同），因而用同一把密钥，得到同一摘要；同一 Message-ID 带来不同正文时摘要不同，仍按冲突拒绝。
- **不做的事：** 存储层不校验摘要与正文是否对应（它没有令牌密钥）；这一对应关系由 4b 的入站流水线保证，并由 `tests/integration/payload_test.go` 钉住调用方式。

```go
// InboundReply 是已通过发件人、线程与令牌校验的入站回复。Body 只以密文落盘。
type InboundReply struct {
	TaskID      string
	Account     string   // 调用方已用 config.NormalizeAddress 规范化的机器人邮箱地址
	Folder      string   // Task 6 已增加，本任务不改
	UIDValidity uint32
	UID         uint32
	MessageID   string   // 与邮件头一致的原样字符串
	BodyDigest  [32]byte // token.Key.BodyDigest(Body)，写入 body_sha256 列（原字段名 BodySHA256）
	Body        []byte   // 解析出的新正文，1 字节到 payload.MaxPlaintext，须为合法 UTF-8
}

// RecordReply 的去重与拒绝规则沿用 Task 6（UID 查找带文件夹）。新增：开始事务前校验 Body 并确认 PayloadKey 非空；
// 事务内确认正文密钥的 kid 已登记为 active；入队为 QUEUED 时，插入 replies 取得 seq 后
// 用 (KindReply, TaskID, seq) 加密 Body 并插入 reply_payloads。记为 REJECTED 或命中重复时不写正文。
func (s *Store) RecordReply(ctx context.Context, in InboundReply) (RecordResult, error)

// ClaimNextReply 的判定不变，改为同时返回解密后的正文。解密在同一事务中进行：正文行缺失返回 ErrPayloadMissing，
// 没有密钥或 kid 不符返回 ErrPayloadKeyUnavailable，密文无法解密返回包装 payload.ErrDecrypt 的错误；三种情况都不领取。
func (s *Store) ClaimNextReply(ctx context.Context, taskID string) (Reply, Task, []byte, error)
```

正文的删除完全由 Task 6 的触发器完成：ACKNOWLEDGED 与 REJECTED（包括 `fail`、`close` 批量拒绝与放回时发现任务已停止）在状态改变的同一事务中删除，UNCERTAIN 与重新排队保留。`AcknowledgeReply`、`ResolveUncertainReply`、`RequeueUnsentReply` 提交后调用 `truncateWAL`。无法解密的回复会一直停在队首并阻塞该任务的派发，需要人工处理，记入「风险与后续」。

- [x] **Step 1：修改既有测试。** 辅助函数的 `Folder` 已在 Task 6 补上，本步只做与正文和摘要有关的修改：Phase 3 的测试辅助函数改为以测试正文密钥打开存储并登记 `(token, 1)`、`(payload, 1)`；构造回复的辅助函数为每封邮件生成不同的 `Body`，`BodyDigest` 用固定测试令牌密钥的 `BodyDigest(Body)` 计算；`ClaimNextReply` 的调用处接收新增的返回值。只做这些机械修改，断言不变，修改后在旧实现上编译失败。
- [x] **Step 2：写失败的测试。**
  - **加密入队：** 新邮件入队后 `reply_payloads` 有一行，`sealed` 不含正文金丝雀，`payload.Open(KindReply, T, seq, …)` 还原正文；`inbound_messages.body_sha256` 等于传入的摘要，且不等于 `sha256.Sum256(Body)`。
  - **不写正文的情况：** 任务 CLOSED 时记为 REJECTED(`task_closed`)，没有正文行；重复记录不增加正文行。
  - **校验：** `Body` 为空、超过上限或不是合法 UTF-8 时在开始事务前报错；`PayloadKey` 为 nil 或 kid 未登记返回 `ErrPayloadKeyUnavailable`；在 `reply_payloads` 上创建 `RAISE(ABORT)` 触发器后记录新邮件失败，`inbound_messages` 与 `replies` 行数都不变。
  - **派发：** `ClaimNextReply` 返回的正文与入队时相同；`PayloadKey` 为 nil、正文行被删除（模拟 0002 之前写入的 QUEUED 回复）、去掉不可改写触发器后翻转密文一个字节，这三种情况分别返回 `ErrPayloadKeyUnavailable`、`ErrPayloadMissing` 与包装 `payload.ErrDecrypt` 的错误，回复仍为 QUEUED，任务状态、版本与事件都不变。
  - **清理：** `AcknowledgeReply` 后正文行消失；`MarkReplyUncertain` 后保留；`ResolveUncertainReply(true)` 后消失；`ResolveUncertainReply(false)` 后保留，再次派发得到同一 seq 与同一正文；`RequeueUnsentReply` 后保留；派发期间任务被关闭再放回时回复为 REJECTED 且正文消失；3 条排队回复后执行 `fail`，3 行正文全部消失。
  - **残留：** 以带金丝雀的正文完成「入队 → 派发 → 确认」，之后数据库文件与 `-wal` 文件中都找不到密文片段，也找不到明文金丝雀（复用 Task 7 的 `assertAbsentOnDisk`）；与 Task 7 相同，正文取 64 字节与 64 KiB 两种大小。
- [x] **Step 3：写集成测试（`tests/integration/payload_test.go`）。** `TestNotificationReplyPayloadLifecycle`：
  1. 加载示例配置；以测试正文密钥打开存储；`EnsureInstance`；登记 `(token, 1)`、`(payload, 1)`；创建 codex 任务，启动并执行 `turn_completed`；
  2. 以域名 `example.invalid`、TTL `cfg.Security.TokenTTL` 创建 `turn_completed` 通知；用通知行的 `NID`、`TaskID`、`Owner`、`TokenExpiresAt` 与令牌密钥签发令牌；`Parse(Reveal())` 后在当前时间验证通过，在 `TokenExpiresAt` 时刻验证返回 `token.ErrExpired`；
  3. 领取通知得到原内容，标记为 SENT，记录实际投递 ID `<tencent_synthetic@example.invalid>`；`NotificationByNID` 取回的 Claims 与签发时一致；
  4. 以正文 `first synthetic reply` 与 `tokenKey.BodyDigest(正文)` 记录回复，派发得到同一正文，确认后正文行消失；
  5. 再次记录同一封信为 `Duplicate = true`；同一 Message-ID 配正文 `changed synthetic reply` 及其摘要返回 `ErrMessageConflict`；
  6. 关闭任务后，之前签发的令牌仍能通过 `token.Verify`（令牌本身不知道任务状态），而 `RecordReply` 记为 REJECTED(`task_closed`)，证明撤销由任务状态承担。

  `lifecycle_test.go` 改用上述方式计算摘要并提供正文，事件序列断言不变。
- [x] **Step 4：** `go test -race -count=3 ./internal/store/sqlite/ ./tests/...` 通过；包覆盖率 ≥ 85%；`make check` 与 `make secrets` 通过。单元测试与集成测试中的测试令牌密钥、正文密钥用 `bytes.Repeat` 或按下标填充构造，摘要、令牌文本及断言用的期望值都在运行时计算，测试向量遵循「测试向量与密钥扫描」的约定。
- [x] **Step 5：** `git commit -m "feat(store): encrypt queued reply bodies and key the body digest"`

**实施说明：** 先改测试：Step 1 的机械修改与 Step 2、3 的新测试在旧实现上编译失败（`unknown field BodyDigest in struct literal of type sqlite.InboundReply`、`unknown field Body …`、`assignment mismatch: 4 variables but store.ClaimNextReply returns 3 values` 等），再实现。实现要点（只改 `replies.go`）：① `InboundReply` 的 `BodySHA256` 改名为 `BodyDigest` 并增加 `Body`；`validate` 按字节检查 `Body` 为 1 字节到 `payload.MaxPlaintext` 的合法 UTF-8（`utf8.Valid`），错误文本固定、不含正文。② `RecordReply` 在校验之后、开始事务前确认 `PayloadKey` 非空；事务内在读取任务之后、去重查找之前调用 Task 7 的 `activePayloadKey` 确认 kid 已登记为 active。契约没有说明重复邮件与将被拒绝的邮件是否也要求正文密钥，按「开始事务前确认 `PayloadKey` 非空」对所有邮件成立的写法，事务内的检查同样对所有邮件执行（Keychain 读取失败时不处理回复），函数注释写明，测试钉住。入队为 QUEUED 时，插入 `replies` 取得 seq 后以 `(KindReply, TaskID, seq)` 加密并插入 `reply_payloads`，`key_id` 取当前密钥的 kid；记为 REJECTED 或命中重复时不写正文。摘要只做字节比较，存储层不校验它与正文的对应关系。③ `ClaimNextReply` 增加正文返回值；读取队首回复后、改动任何数据之前经 `openReplyPayload` 读出并解密，错误划分与 Task 7 的 `openNotificationPayload` 相同（正文行缺失返回 `ErrPayloadMissing`，没有密钥、`key_id` 与当前密钥不符或 kid 不是 active 返回 `ErrPayloadKeyUnavailable`，解密失败返回包装 `payload.ErrDecrypt` 的错误），错误文本不含正文与密文；失败时返回零值快照，契约没有要求返回队首回复的快照（与 Task 7 的通知不同，调用方已知任务 ID，也没有放弃单条排队回复的接口）。`openReplyPayload` 与 `openNotificationPayload` 结构相同，只差表名、用途与错误文本中的名称；为不改动 Task 7 已审查的 `notifications.go`，另写一个并列函数，没有合并成通用函数。④ 检查点：`changeReply` 提交后，回复若处于 ACKNOWLEDGED 或 REJECTED 即调用 `truncateWAL`。各操作只接受 DISPATCHING 或 UNCERTAIN 作为来源状态，成功后处于这两种状态就意味着本次进入终态、正文已被触发器删除；因此 `AcknowledgeReply`、`ResolveUncertainReply(true)` 以及任务已停止时的 `RequeueUnsentReply` 与 `ResolveUncertainReply(false)` 都会执行检查点，`MarkReplyUncertain` 与放回 QUEUED 时不执行（这时正文没有删除，与 Task 6「每个会让正文进入终态的操作在提交后调用一次」、Task 7 的 `changeNotification` 一致）。最初照 `changeNotification` 另加了「状态有变化」的条件，变异测试表明它对回复恒为真，已删去。⑤ `replies.go` 的包注释与 `InboundReply` 注释原写「不保存正文」，改为现状。

测试辅助函数（Step 1）：`openTaskStore` 以 kid 1 的测试正文密钥打开存储，并用 Task 7 的 `registerTestKeys` 登记 `(token, 1)`、`(payload, 1)`；新增的 `bodyDigest` 用 `bytes.Repeat` 构造的 kid 1 令牌密钥计算 `token.Key.BodyDigest`，`digestA`、`digestB` 改为原合成正文的键控摘要；`inbound` 带正文 A 及其摘要，`enqueueReplies` 与 `TestFailAndCloseRejectQueuedReplies` 的入队闭包经 `withBody` 为每封邮件生成不同的正文 `synthetic reply <uid>`；冲突用例只把字段名改为 `BodyDigest`（正文仍为 A，存储层只比较摘要）；`ClaimNextReply` 的调用处接收并丢弃新增的返回值。`lifecycle_test.go` 的 `openStore` 以测试正文密钥打开存储，首次打开后登记两把密钥，`syntheticReply` 提供正文并以测试令牌密钥计算摘要，事件序列断言不变；示例配置的加载提取为 `loadExampleConfig`，供 `payload_test.go` 复用。清单之外的改动：`RecordReply` 现在要求已登记的正文密钥并会写入 `reply_payloads`，以下不在本任务文件清单中的测试须作机械修改，断言都不变：`store_test.go` 的 `openStore` 加上测试正文密钥，但不登记（同一目录会被打开多次，密钥元数据属于数据库），需要记录回复的 `TestRecoverInFlightAfterCrash`、`TestClaimNextReplyAcrossProcesses` 与 `migrate_test.go` 的两个升级用例自行调用 `registerTestKeys`；`instance_test.go` 的三个 `RegisterKey` 用例要求 `crypto_keys` 为空，由 `openTaskStore` 改用 `openStore(t, dataDir(t))`；`schema_test.go` 的 `TestSchemaConstraints` 在记录回复后清空 `crypto_keys` 与 `reply_payloads`，使其后的直接插入仍从空表开始，`TestPayloadTriggers` 先删除入队时写入的正文，再直接插入以验证触发器；`migrate_test.go` 另把字段改名为 `BodyDigest` 并提供正文。

新增测试（Step 2、3）：`replies_test.go` 中 `TestRecordReplyEncryptsBody`（正文行 `key_id` 为 1、密文比正文长 29 字节且不含金丝雀、`payload.Open(KindReply, T, seq, …)` 还原正文、`body_sha256` 等于传入摘要且不等于 `sha256.Sum256(Body)`）、`TestRecordReplyWritesNoPayloadForRejectedOrDuplicate`、`TestRecordReplyBodyValidation`、`TestRecordReplyRequiresPayloadKey`（`PayloadKey` 为 nil 时上下文已取消仍返回 `ErrPayloadKeyUnavailable`，证明检查先于开始事务；kid 未登记与 kid 已 retired 在事务内拒绝；三种情况对入队为 QUEUED 的新邮件、发往 CLOSED 任务而将被记为 REJECTED 的新邮件与已记录的邮件都成立）、`TestRecordReplyPayloadAtomic`、`TestReplyPayloadResidue`（64 字节与 64 KiB 两种大小完成入队、派发、确认后，经 `assertAbsentOnDisk` 断言数据库文件与 WAL 文件中既没有密文的任何 32 字节片段，也没有明文金丝雀）；`dispatch_test.go` 中 `TestClaimNextReplyUnreadablePayload`（契约的三种情况，经 `requireRejected` 断言回复仍为 QUEUED、任务状态、版本与事件不变，另断言正文行不变、返回的正文为 nil、错误文本不含正文）与 `TestReplyPayloadLifecycle`（清理的各项，且 `AcknowledgeReply`、`ResolveUncertainReply(true)`、关闭后 `RequeueUnsentReply`、`fail` 之后 WAL 为 0 字节）。契约之外补了：`TestRecordReplyUsesCurrentPayloadKey`（payload 的 active 密钥换为 kid 2 后正文行 `key_id` 为 2 且能派发出原正文）；正文校验另测 nil、截断的多字节字符，以及 1 字节、多字节 UTF-8 与恰为上限的正文可以入队（上限正文的密文恰为表约束允许的最大长度）；派发另测密文 `key_id` 与当前密钥不符、kid 已 retired 两种情况。`tests/integration/payload_test.go` 的 `TestNotificationReplyPayloadLifecycle` 按 Step 3 的六步执行；存储没有读取正文行的接口，测试以独立的 `database/sql` 连接（驱动由存储包注册）打开数据库文件统计 `reply_payloads` 的行数；存储使用真实时钟，「当前时间」取 `time.Now()`；按 nid 取回的 Claims 逐字段比较（有效期用 `Equal`），并用它们再次验证令牌。

在仓库副本上做了 24 个变异，每次只改一处逻辑，涉及：开始事务前的 `PayloadKey` 检查、事务内的 active 检查（去掉，或只对新邮件执行）、加密的用途与序号、`key_id` 取常量、对 REJECTED 也写正文、不写正文、正文长度的上下界与 UTF-8 检查、摘要改存正文的 SHA-256、派发忽略解密错误、派发返回 nil 正文、去掉 `key_id` 核对、解密的用途与序号、缺行错误、解密时跳过 active 检查、检查点的去掉与只在 ACKNOWLEDGED 或只在 REJECTED 时执行。其中两个初次施加时因变量未定义或未使用而编译失败，改为可编译的写法（把 active 检查移入 QUEUED 分支、`_ = body`）后被杀死。存活 2 个：上文 ④ 所说「状态有变化」的条件，为等价变异，已从实现中删去；每次 `changeReply` 成功后都执行检查点，只多做几次尽力而为的检查点，不影响正确性，不以测试禁止。其余全部被测试杀死。验证：`go test -race -count=3 ./internal/store/sqlite/ ./tests/...` 通过且无抖动，本包覆盖率 88.0%；`CGO_ENABLED=0 go test ./internal/store/sqlite/ ./tests/...`、全仓 `GOOS=windows go vet` 与 `GOOS=linux go vet`、`make check`（含中文注释检查与 staticcheck，总覆盖率 93.16%）、`make secrets` 均通过。测试向量只有 `bytes.Repeat` 构造的令牌密钥与正文密钥、`[8]byte` 字面量的校验值，键控摘要在运行时计算，没有使用 `gitleaks:allow`。本任务不涉及钥匙串与网络；测试只在 macOS 上运行，Linux 上的测试由 CI 运行。与 Task 1–7 相同，本任务的提交包含本清单的勾选与实施说明。

**审查后修正：** 变异在临时副本中施加，只改一处。① 上文 ② 所说「事务内的检查同样对所有邮件执行……测试钉住」对将被拒绝的邮件不成立：`TestRecordReplyRequiresPayloadKey` 原先只覆盖入队为 QUEUED 的新邮件与已记录的邮件，把事务内的 active 检查改为只在 `task.AcceptsReplies(current.State)` 为真时返回错误的变异存活；变异后 kid 未登记或已 retired 时，发往 CLOSED 任务的邮件会被记为 REJECTED(`task_closed`)，而不是返回 `ErrPayloadKeyUnavailable`。现在该测试的每个子用例另准备一个已关闭的任务，以正文 `synthetic reply to a closed task`、UID 3、Message-ID `<c@example.invalid>` 构造一封将被拒绝的新邮件，与另两封一起断言返回 `ErrPayloadKeyUnavailable`，行数断言不变（`inbound_messages` 与 `replies` 各 1 行，`reply_payloads` 1 行）。实现未改动。修正后该变异在 kid 未登记与 kid 已 retired 两个子用例中被杀死。 复查后补充：回复与通知两份几乎逐行相同的正文解密函数合并为 `openPayload`，两类正文共用同一套错误划分（缺行、key_id 不符、active 检查、解密失败），避免日后只改一处而语义分叉；`TestRecordReplySameUIDInDifferentFolders` 的第二封改用与摘要对应的正文 B；`TestReplyPayloadLifecycle` 的 fail 用例先断言 3 行正文存在，再断言 fail 后全部删除；`RecordReply` 注释写清正文校验失败返回校验错误、密钥问题返回 `ErrPayloadKeyUnavailable`。

### Task 9：收取游标与被拒来信

**Files:**
- Create: `internal/store/sqlite/mailbox.go`
- Test: `internal/store/sqlite/mailbox_test.go`

```go
// Cursor 是某个文件夹的收取游标；LastUID 为 0 表示该 UIDVALIDITY 下尚未处理任何邮件。
type Cursor struct {
	UIDValidity uint32
	LastUID     uint32
}

// ErrCursorRegression 表示在 UIDVALIDITY 不变时试图把 LastUID 调小。
var ErrCursorRegression = errors.New("fetch cursor would move backwards")

// FetchCursor 读取 (account, folder) 的游标；没有记录时返回 ErrNotFound。
func (s *Store) FetchCursor(ctx context.Context, account, folder string) (Cursor, error)

// AdvanceCursor 写入游标：UIDVALIDITY 与已有记录相同时 LastUID 只能增加或不变，否则返回 ErrCursorRegression 且不改动；
// UIDVALIDITY 不同或尚无记录时整体写入（重置）。调用方在一批邮件全部处理并提交后才调用。
func (s *Store) AdvanceCursor(ctx context.Context, account, folder string, c Cursor) error

// RejectReason 是被拒来信的原因码；合法取值只由下列常量定义，4b 可增补。
type RejectReason string

const (
	RejectAutoReply        RejectReason = "auto_reply"
	RejectBounce           RejectReason = "bounce"
	RejectSenderNotAllowed RejectReason = "sender_not_allowed"
	RejectTooLarge         RejectReason = "too_large"
	RejectMalformed        RejectReason = "malformed"
	RejectParseUncertain   RejectReason = "parse_uncertain"
	RejectThreadMismatch   RejectReason = "thread_mismatch"
	RejectSubjectTag       RejectReason = "subject_tag_invalid"
	RejectTokenMissing     RejectReason = "token_missing"
	RejectTokenInvalid     RejectReason = "token_invalid"
	RejectTokenExpired     RejectReason = "token_expired"
	RejectTaskUnknown      RejectReason = "task_unknown"
	RejectMessageConflict  RejectReason = "message_conflict"
)

// Rejection 是一封被拒来信的元数据，不含主题与正文。
type Rejection struct {
	ID          int64
	Account     string
	Folder      string
	UIDValidity uint32
	UID         uint32
	MessageID   string // 可为空
	Sender      string // 调用方规范化后的发件人地址，可为空
	Reason      RejectReason
	TaskID      string // 能识别任务时填写，可为空
	ReceivedAt  time.Time
}

// RecordRejection 写入被拒来信；同一 (account, folder, uid_validity, uid) 已有记录时返回原记录 ID 与 duplicate=true，不覆盖。
// 原因码不在上表、字段长度或空白不符时在开始事务前报错，错误文本不回显地址与 Message-ID；TaskID 不存在时返回 ErrNotFound。
func (s *Store) RecordRejection(ctx context.Context, r Rejection) (id int64, duplicate bool, err error)

// Rejections 按 (received_at, id) 升序返回 since 之后的被拒来信，至多 limit 条（1–1000）。
func (s *Store) Rejections(ctx context.Context, since time.Time, limit int) ([]Rejection, error)
```

被拒来信只记录，不回信（防回环与反向散射）；由 CLI 或菜单栏展示属于后续阶段。

- [x] **Step 1：写失败的测试。**
  - **游标：** 没有记录时 `ErrNotFound`；写入 `(7, 10)` 后读回；`(7, 12)` 成功；`(7, 11)` 返回 `ErrCursorRegression` 且仍为 `(7, 12)`；`(7, 12)` 重复写入成功；`(8, 3)` 重置成功；INBOX 与 Junk 的游标互不影响；账户为空或含空白、文件夹为空、256 个字符或含 NUL、UIDVALIDITY 为 0 时在开始事务前报错。
  - **被拒来信：** 首次写入返回新 ID 与 `duplicate = false`；同一 (账户, 文件夹, UIDVALIDITY, UID) 再次写入返回同一 ID 与 `true`，已有字段不变；原因码 `Bad` 或空字符串报错；Message-ID 为 2 个字符报错，且错误文本不含该值与发件人地址；TaskID 不存在返回 `ErrNotFound`；Message-ID 与发件人为空时可以写入；`Rejections` 按时间升序返回，`limit` 生效，`limit` 为 0 或 1001 时报错。
  - **Go 与数据库一致：** 遍历全部原因码常量，每个都满足数据库的形状约束（逐个插入成功）。
- [x] **Step 2：** 测试失败。
- [x] **Step 3：** 实现。`AdvanceCursor` 用 `INSERT … ON CONFLICT (account, folder) DO UPDATE … WHERE excluded.uid_validity <> fetch_cursors.uid_validity OR excluded.last_uid >= fetch_cursors.last_uid`，受影响行数为 0 即回退。
- [x] **Step 4：** `go test -race ./internal/store/sqlite/` 通过。
- [x] **Step 5：** `git commit -m "feat(store): persist fetch cursors and rejected mail metadata"`

**实施说明：** 先写测试，`go test ./internal/store/sqlite/` 编译失败（`undefined: Cursor`、`s.AdvanceCursor undefined`、`s.FetchCursor undefined`、`undefined: Rejection` 等），再实现。实现只新增 `mailbox.go`：① `AdvanceCursor` 按 Step 3 用一条 `INSERT … ON CONFLICT (account, folder) DO UPDATE SET uid_validity、last_uid、updated_at … WHERE excluded.uid_validity <> fetch_cursors.uid_validity OR excluded.last_uid >= fetch_cursors.last_uid`，受影响行数为 0 即返回 `ErrCursorRegression`；单条语句自身是原子的，不另开事务，多个进程并发写入同一游标也不会让它后退。「在开始事务前报错」按「执行任何 SQL 之前校验」落实。② 账户与文件夹的校验按与 `InboundReply.validate` 相同的规则另写为 `checkMailbox`（`replies.go` 未改，两处各有一份），规则与 `InboundReply.validate` 相同（账户 3–254 个字符且不含空白，文件夹 1–255 个字符、可含空格，按 Unicode 字符计，含 NUL 一律拒绝；审查后另要求合法 UTF-8，见下文「审查后修正」），游标与被拒来信共用；`replies.go` 未改动。③ `RecordRejection` 在校验后开启一个 IMMEDIATE 事务：先按 (account, folder, uid_validity, uid) 查找，已有记录即返回原 ID 与 `duplicate = true`，不覆盖；否则 TaskID 非空时经 `getTask` 确认任务存在（不存在返回包装 `ErrNotFound` 的错误），再 `INSERT … RETURNING id`。空的 Message-ID、发件人与 TaskID 写为 NULL（小函数 `optional`）。④ 原因码的合法取值集中在 `knownRejectReasons`（与契约的 13 个常量一一对应），数据库只约束形状；`Rejections` 读回时原因码按原样返回，不按本版本的列表校验，因为它只用于展示，较新版本增补的原因码仍可读出。⑤ 清单之外的一处改动：`tasks.go` 中 `ErrNotFound` 的注释补上「收取游标」，因为 `FetchCursor` 也返回它；只改这一行注释。

契约未规定之处的取舍（均写入函数注释并由测试钉住）：`Rejection.ID` 与 `ReceivedAt` 由存储填写，`RecordRejection` 忽略输入中的这两个字段，`received_at` 取存储时钟的当前时间（UTC 毫秒），与 `inbound_messages.received_at` 由 `RecordReply` 取当前时间一致，因此也不需要对输入时间做校验；`Rejections` 的「since 之后」按严格晚于处理（`received_at > since`，按毫秒比较，恰在 since 的不返回；因此 since 加 limit 不能用来逐页读完，见下文「审查后修正」）；已有记录的邮件再次写入时先按重复返回、不再检查任务；`FetchCursor` 对账户与文件夹做与写入相同的校验（契约只要求写入时校验），避免非法参数被误读为「没有游标」而触发全量补扫；发件人按规范化地址不允许空白，Message-ID 校验长度与 NUL，另须为合法 UTF-8（`InboundReply` 不校验 UTF-8，见下文审查后修正）；错误文本不含地址、文件夹与 Message-ID，只有未知原因码会以 `%q` 回显原因码本身（它来自程序常量，不来自邮件）。

测试（`mailbox_test.go`）：`TestAdvanceCursor` 按契约顺序走完 `ErrNotFound`、(7, 10)、(7, 12)、(7, 11) 回退、(7, 12) 重复、(8, 3) 重置，另断言回退被拒后整行（含 `updated_at`）不变、重复写入刷新 `updated_at`、重置后再以 (7, 1) 重置且 (7, 0) 仍为回退、新 UIDVALIDITY 下 `LastUID = 0` 可重复写入、uint32 最大值可写入；`TestCursorFoldersIndependent` 覆盖 INBOX 与 Junk、两个账户各自前进、回退与重置互不影响；`TestCursorValidation` 覆盖契约列出的非法输入与账户过短、过长、末尾换行，上下文已取消时仍返回校验错误且不写入，`FetchCursor` 对同样的账户与文件夹报错，文件夹含空格、恰为 255 个字符（含非 ASCII 字符）与账户边界值可以写入；`TestRecordRejection` 断言首次写入后全部字段逐一读回、输入中的 ID 与 ReceivedAt 被忽略，再次写入时其余字段不同也返回同一 ID 与 `true` 且原记录不变，四个键中任一不同都是新记录，Message-ID、发件人与任务为空时写入且库中为 NULL；`TestRecordRejectionUnknownTask` 覆盖 TaskID 不存在返回 `ErrNotFound` 且不写入、已有记录时按重复返回；`TestRecordRejectionValidation` 覆盖原因码 `Bad`、空字符串与形状合法但不在列表中的 `synthetic`，以及各字段的长度、空白、NUL 与取值范围，上下文已取消时仍返回校验错误，错误文本不含账户、Message-ID 与发件人，边界值可以写入；`TestRejectionsOrderAndLimit` 以乱序时间写入四条（含同一毫秒的两条），断言按 (received_at, id) 升序、`limit` 生效、since 的边界，以及 `limit` 为 0、−1、1001 时报错；`TestRejectReasonsMatchDatabase` 断言 `knownRejectReasons` 恰为契约的 13 个常量，并逐个经 `RecordRejection` 写入成功（即满足数据库的形状约束）后按原样读回；`TestMailboxCanceledContext` 断言输入合法而上下文已取消时四个方法都返回 `context.Canceled`。测试只用合成地址与 `example.invalid` 域名，不含机密形状的测试向量。

在仓库副本上做了 55 个变异，每次只改一处，涉及：UPSERT 的 `>=` 改为 `>`、去掉「UIDVALIDITY 不同即重置」、去掉整个 WHERE、OR 改为 AND、回退判定失效、不刷新 `updated_at`、不更新 UIDVALIDITY 或 LastUID；`FetchCursor` 不按文件夹或账户查找、不校验；UIDVALIDITY 为 0 不拒绝；账户与文件夹的长度上下界、空白、NUL、按字节计长与按空白拒绝文件夹；被拒来信按四个键去重时各漏一个键、重复时返回 false、不检查任务、`received_at` 取输入、空字符串不写为 NULL；原因码、UID、UIDVALIDITY、Message-ID 与发件人的各项校验（上下界、按字节计长、空白、NUL、空值也校验长度）；错误文本回显 Message-ID、发件人或账户；不提交事务；`Rejections` 改用 `>=`、只按 id 排序、同一毫秒按 id 倒序、忽略 limit、limit 上下界、不换算 UTC、不读回 task_id；原因码列表少一项。全部被测试杀死，没有存活与编译失败的变异。验证：`go test -race ./internal/store/sqlite/` 通过，新增用例以 `-race -count=3` 重复运行稳定；`CGO_ENABLED=0 go test ./internal/store/sqlite/`、全仓 `GOOS=windows go vet` 与 `GOOS=linux go vet`、`make check`（含中文注释检查与 staticcheck，总覆盖率 93.12%，本包 88.3%，`mailbox.go` 中未覆盖的只有数据库出错与提交失败的分支）、`make secrets` 均通过。本任务不涉及钥匙串与网络；测试只在 macOS 上运行，Linux 上的测试由 CI 运行。与 Task 1–8 相同，本任务的提交包含本清单的勾选与实施说明。

**审查后修正：** ① `Rejections` 的契约只有 since 与 limit，没有续读游标；实现按严格晚于过滤，而 `received_at` 取存储时钟的毫秒值，一批来信在同一毫秒内被拒是现实情况，以上一页最后一条的 `ReceivedAt` 作为下一页的 since 会漏掉与它同一毫秒的其余记录（`TestRejectionsOrderAndLimit` 中 limit 1 得到 b，再以 b 与 c 共同的时间为 since 得到 d、a，c 被跳过）；改用 `>=` 又会重复返回，同一毫秒的记录超过 limit 时还会原地打转。本任务只改文档：`Rejections` 的函数注释写明它只适合查看某一时刻之后的前 limit 条，不能用来逐页读完；实现与测试未改动。4b 的 CLI 若需要完整分页，由维护者修改契约（例如增加按 (received_at, id) 续读的参数），本任务不自行增加接口。② `checkMailbox` 与 `Rejection.validate` 原先注释称按 Unicode 字符计长「与表约束中 SQLite 的 `length()` 一致」，对非法 UTF-8 不成立：`length()` 遇到不小于 0xC0 的字节后会把其后的续字节并入同一个字符，计数小于 Go。例如 `"\xc0\x80\xc0\x80"` 在 Go 中计为 4 个字符、在 SQLite 中计为 2 个，作 Message-ID、发件人或账户时通过 Go 端校验，却在执行 SQL 后被 CHECK 约束拒绝，违反「字段长度或空白不符时在开始事务前报错」；Message-ID 由发件人控制，被拒来信又恰是收录畸形来信的地方。现在 `checkMailbox` 对账户与文件夹、`Rejection.validate` 对非空的 Message-ID 与发件人增加 `utf8.ValidString` 检查，与 NUL 检查合为一项（错误文本分别为 `account and folder must be valid UTF-8 without NUL` 与 `invalid rejection: message id and sender must be valid UTF-8 without NUL`，不含取值）；字符串合法时两边计数相同，原有边界不变。这是清单之外的补充，与 Phase 3 对 `InboundReply` 的取舍（含非法 UTF-8 时由 CHECK 约束兜底）不同，`replies.go` 未改动，两处注释写明了这一差别。4b 遇到非法 UTF-8 的 Message-ID 或发件人时，须清空或替换后再记录被拒来信。新增用例：`TestCursorValidation` 中账户为 `"\xc0\x80\xc0\x80"`、文件夹为 `"IN\xffBOX"`，`TestRecordRejectionValidation` 中 Message-ID 与发件人为 `"\xc0\x80\xc0\x80"`，都在上下文已取消时返回校验错误。在仓库副本上验证：原实现下这四个用例全部失败（返回 `context canceled`，说明校验没有拦住）；分别去掉账户、文件夹、Message-ID、发件人的 UTF-8 检查，四个变异各自被对应用例杀死。 ③ 复查后补充：`Rejection.validate` 另要求任务 ID 为空或 10 位任务 ID 的形式（与 `NewNotification` 相同的 `taskIDPattern`），任意字符串不再进入 `getTask`，错误文本也不回显它；`TestRecordRejectionValidation` 补两个用例，并把任务 ID 加入「错误文本不回显」的断言，去掉该检查的变异使两个用例失败。另记一项契约层面的局限（不改代码）：被拒来信按 (账户, 文件夹, UIDVALIDITY, UID) 去重，UIDVALIDITY 重置后全量补扫时，以前被拒的来信会以新的 UIDVALIDITY 再记一条，同一封信在 INBOX 与 Junk 各有一份时也记两条；「靠去重兜底」只对按 Message-ID 去重的 `inbound_messages` 成立，4b 展示被拒记录时按 Message-ID 合并。

### Task 10：SMTP 客户端

**Files:**
- Create: `internal/mail/smtp/smtp.go`
- Test: `internal/mail/smtp/smtp_test.go`、`internal/mail/smtp/source_test.go`
- Modify: `go.mod`、`go.sum`（`go get github.com/emersion/go-smtp@v0.25.0`；`github.com/emersion/go-sasl` 随之成为直接依赖，版本保持伪版本 `b788ff2`）

```go
// Package smtp 经隐式 TLS 向 SMTP 服务器提交单封邮件，把结果分为：已接受、确定未投递、被拒绝与结果不确定。
// 只调用 go-smtp 的 DialTLS，不支持 STARTTLS 与明文，不开启调试输出（它会写出凭据）。
// 本包不渲染邮件、不重试、不访问存储；重试与状态记录由调用方按 queue 的待发通知状态机处理。
package smtp

// Config 是一次发送的连接参数。
type Config struct {
	Host      string      // 例如 smtp.qq.com
	Port      int         // 默认 465；端口可配置，TLS 模式固定为隐式 TLS
	Username  string         // 机器人地址，也是信封发件人
	RootCAs   *x509.CertPool // nil 时使用系统根证书；测试注入自签 CA。tls.Config 只在包内构造：ServerName 固定为 Host，MinVersion 为 TLS 1.2
	Timeouts  Timeouts
}

// Timeouts 是逐步期限；零值字段使用 DefaultTimeouts 中的对应值。
type Timeouts struct {
	Command    time.Duration // 问候、EHLO、AUTH、MAIL、RCPT、DATA（至 354）各自的期限，也是写正文时每个 64 KiB 分块的期限；默认 30 秒
	Submission time.Duration // 从调用 CloseWithResponse（flush 剩余正文、写结束标记）到收到响应的期限，默认 120 秒
}

// DefaultTimeouts 返回生产期限；拨号与 TLS 握手的 30 秒由 go-smtp 的 DialTLS 固定，不在此配置。
func DefaultTimeouts() Timeouts

// Envelope 是信封；From 必须等于 Config.Username（QQ 要求信封发件人与登录用户一致）。
type Envelope struct {
	From string
	To   []string
}

// Result 是服务器接受邮件后的结果。
type Result struct {
	Response string // 结束标记后 250 响应的文本（不含状态码），至多 512 字节；L1 用它查找服务器分配的 ID
}

// MaxMessageSize 是单封邮件的字节上限。
const MaxMessageSize = 4 << 20

var (
	// ErrNotSent 表示进入提交阶段（调用 CloseWithResponse）之前失败：结束标记尚未开始写出，服务器不可能已接受这封邮件。
	ErrNotSent = errors.New("smtp: message was not sent")
	// ErrAuth 表示认证被拒绝；返回的错误同时满足 errors.Is(err, ErrNotSent)。
	ErrAuth = errors.New("smtp: authentication failed")
	// ErrRejected 表示服务器对结束标记回复了 4xx 或 5xx，确定未投递。
	ErrRejected = errors.New("smtp: message was rejected")
	// ErrUncertain 表示已进入提交阶段（flush 剩余至多 4 KiB 正文、写结束标记、读响应），但没有得到任何响应：可能已投递，
	// 调用方不得自动重发。flush 剩余正文时失败也归入此类：此时结束标记可能尚未写出，但本包无法区分，按保守方向处理。
	ErrUncertain = errors.New("smtp: delivery outcome is uncertain")
)

// ReplyError 携带服务器拒绝时的步骤与状态码，不含服务器响应文本。
type ReplyError struct {
	Step     string // auth、mail、rcpt、data、submission
	Code     int
	Enhanced [3]int // 服务器未给出时为零值
}

// Error 返回形如 "smtp: rcpt rejected with 550 5.1.1" 的文本。
func (e *ReplyError) Error() string

// Temporary 在 4xx 时返回 true。
func (e *ReplyError) Temporary() bool

// Send 校验参数后连接、认证并提交一封邮件；ctx 结束时关闭连接。
// ctx 在进入提交阶段之前结束返回 ErrNotSent，进入之后（包括 flush 剩余正文期间）返回 ErrUncertain。错误文本只含步骤名、状态码与增强状态码，
// 不含服务器响应文本、地址、密码或邮件内容；服务器以 4xx/5xx 拒绝时错误链中含 *ReplyError。
func Send(ctx context.Context, cfg Config, password string, env Envelope, msg []byte) (Result, error)
```

**发送步骤与结果分类：**

| 步骤 | 期限 | 失败时 |
| --- | --- | --- |
| 参数校验：Host 非空、端口 1–65535、`From == Username`、`To` 至少 1 个且不含 CR/LF、邮件 1 字节到 4 MiB、密码为不含空白的可打印 ASCII | 不联网 | 普通错误，不包装 `ErrNotSent` 以外的哨兵 |
| `smtp.DialTLS(host:port, 包内构造的 tls.Config)`，`RootCAs` 取自配置、`ServerName = Host`、`MinVersion = TLS 1.2` | 30 秒（库固定，含握手） | `ErrNotSent` |
| 问候与 EHLO（沿用库默认名 `localhost`，不透露本机主机名） | 30 秒 | `ErrNotSent` |
| 确认 `TLSConnectionState` 存在且服务器公告 `AUTH PLAIN`，否则不发送凭据 | — | `ErrNotSent` |
| `AUTH PLAIN` | 30 秒 | 535 等拒绝为 `ErrAuth`（同时是 `ErrNotSent`），其他为 `ErrNotSent` |
| `MAIL FROM`、每个 `RCPT TO`、`DATA`（至收到 354） | 各 30 秒 | `ErrNotSent`，服务器拒绝时附 `*ReplyError` |
| 写正文：按 64 KiB 分块写入 `DataCommand` | 每块 30 秒 | `ErrNotSent`（尚未进入提交阶段，结束标记尚未写出） |
| `CloseWithResponse`（提交阶段）：flush 剩余正文、写结束标记并读响应 | 整个调用 120 秒 | 4xx/5xx 为 `ErrRejected` 附 `*ReplyError`；超时、断线或 ctx 结束为 `ErrUncertain`，flush 剩余正文时的失败同样如此（保守） |
| `QUIT` | 30 秒 | 忽略，结果以上一步为准 |

**期限的实现：** go-smtp 只在命令与读取 DATA 响应时设置连接期限：`Data()` 收到 354 后把期限清零，随后经 `DotWriter` 写正文没有期限；`CloseWithResponse` 先 flush 剩余正文并写结束标记，之后才设置 `SubmissionTimeout`。服务器停止读取（TCP 窗口写满）或连接半开时，这两段会永久阻塞。因此表中每一步都由包内的看门狗负责：进入一步前以该步的期限重置计时器，计时器到期或 ctx 结束时调用 `client.Close()`，使阻塞的读写返回；一个原子标志记录当前是否已进入 `CloseWithResponse`，据此把关闭造成的错误分为 `ErrNotSent` 或 `ErrUncertain`。标志在调用 `CloseWithResponse` 之前置位，而库在该调用内部先 flush 剩余正文、再写结束标记，本包看不到两者的分界，因此 flush 期间的失败也归为 `ErrUncertain`：宁可让一封确定未投递的邮件等待本地核对，也不冒重复投递的风险。库的 `CommandTimeout` 与 `SubmissionTimeout` 设为同样的值，只作第二道保险。

调用方（4b）按以下方式映射到 Task 5 的状态机：成功 → `MarkNotificationSent`；`ErrUncertain` → `MarkNotificationUncertain`；`ErrRejected` 与 `ErrNotSent` 在 `ReplyError.Temporary()` 为真、或没有 `ReplyError` 时 → `RequeueNotification` 并退避，在 5xx 时 → `AbandonNotification(rejected)`；`ErrAuth` → 重新排队、打开与 IMAP 共享的认证熔断（见「4b 与 4a 的衔接」），并在本地提示授权码可能失效。频率阈值、退避时长与合并策略在 4b 定。

- [x] **Step 1：写失败的测试（`smtp_test.go`）。** 假服务器用 go-smtp 的服务端：后端记录 `AUTH PLAIN` 收到的用户名与密码、`MAIL`、`RCPT` 与 DATA 的全部字节，并可按用例设置：认证返回 535；`RCPT` 返回 550；结束标记后返回 451、554（文本为 `SERVER-TEXT-CANARY`）；DATA 读完后阻塞直到测试结束；收到 354 之后不再读取 DATA（接受的 TCP 连接以 `SetReadBuffer(4096)` 缩小接收缓冲）；会话不提供认证机制（不公告 AUTH）。TLS 证书借用 `httptest.NewUnstartedServer(nil)` 调用 `StartTLS()` 后的证书与 `Certificate()` 生成的根证书池（SAN 为 127.0.0.1、::1、example.com、*.example.com），成功类用例的 Host 为 `127.0.0.1`；测试期限用 200ms 级别的 `Timeouts`。
  - **成功：** `Send` 返回服务器 250 响应的文本；后端收到用户名 `bot@example.invalid`、密码（运行时构造的 `strings.Repeat("pw", 8)`，遵循「测试向量与密钥扫描」的约定）、信封发件人与收件人一致；DATA 字节等于输入（输入含以 `.` 开头的行，服务端去掉点填充后相同）。
  - **参数校验先于联网：** `From` 与 `Username` 不同、`To` 为空或含 `\r\n`、邮件为空或 4 MiB + 1 字节、密码为空或含空格、端口为 0 时返回错误，监听器没有收到任何连接。
  - **明文防护：** 在不加 TLS 的普通监听器上运行同一个 go-smtp 服务端，监听器外包一层记录全部入站字节的连接；`Send` 返回 `ErrNotSent`，后端从未收到 `AUTH`，记录的入站字节中不含密码，也不含 `AUTH`。服务器证书不在 `RootCAs` 中时同样返回 `ErrNotSent` 且没有 `AUTH`。
  - **主机名校验：** CA 受信，但 Host 为不在证书 SAN 中的 `localhost` 时返回 `ErrNotSent`，服务器没有收到 `AUTH`。
  - **不公告 AUTH PLAIN：** 返回 `ErrNotSent`，后端没有收到 `AUTH`。
  - **分类：** 535 → `errors.Is(err, ErrAuth)` 与 `errors.Is(err, ErrNotSent)` 都成立，后端没有收到 `MAIL`；`RCPT` 550 → `ErrNotSent`，`errors.As` 得到 `ReplyError{Step: "rcpt", Code: 550}` 且 `Temporary()` 为 false，后端没有收到 DATA；结束标记后 451 → `ErrRejected`，`Temporary()` 为 true；554 → `ErrRejected`，`Temporary()` 为 false。
  - **结果不确定：** 结束标记后服务端阻塞，`Submission = 200ms` 时 `Send` 在 2 秒内返回 `ErrUncertain`，此时后端已收到完整的 DATA。
  - **写正文时服务器停止读取：** 服务器收到 354 后不再读取，邮件为 4 MiB，`Command = 200ms` 时 `Send` 在 2 秒内返回 `ErrNotSent`，连接已关闭。实施说明记录「去掉写正文计时器」的变异下该用例阻塞到测试期限；若本机 socket 缓冲吸收了全部正文使变异存活，须调整用例（例如再缩小缓冲），不得放过。
  - **flush 阶段阻塞：** flush 剩余正文时才阻塞的情形无法用真实 socket 稳定构造（剩余部分不超过 `DotWriter` 的 4 KiB 缓冲）。因此 `CloseWithResponse` 经包内函数变量 `closeData` 调用；测试把它换成一个阻塞到服务端检测到连接被关闭才返回错误的替身，`Submission = 200ms` 时 `Send` 在 2 秒内返回 `ErrUncertain`。这证明看门狗在调用 `CloseWithResponse` 之前就以 `Submission` 生效；去掉该阶段看门狗的变异使本用例超时失败。
  - **问候阻塞：** 一个只完成 TLS 握手、从不写问候的监听器，`Command = 200ms` 时在 2 秒内返回 `ErrNotSent`。
  - **取消：** 后端在 `RCPT` 中阻塞时取消 ctx，返回的错误同时满足 `ErrNotSent` 与 `context.Canceled`；结束标记后阻塞时取消 ctx 返回 `ErrUncertain`。两种情况下连接都已关闭（服务端会话结束）。
  - **错误文本：** 以上所有用例返回的错误文本都不含密码、收件人与发件人地址、邮件中的金丝雀与 `SERVER-TEXT-CANARY`。
- [x] **Step 2：写失败的测试（`source_test.go`）。** 用 `go/parser` 解析本包全部非测试 `.go` 文件：对 go-smtp 包的选择器只允许 `DialTLS`、`Client`、`DataCommand`、`DataResponse`、`SMTPError`；对 go-sasl 只允许 `NewPlainClient`；任何文件都不出现标识符 `DebugWriter`、`InsecureSkipVerify`、`KeyLogWriter`、`VerifyPeerCertificate`、`VerifyConnection`；`tls.Config` 字面量只出现一次，位于包内构造它的函数中。在临时副本中加入对 `smtp.Dial` 或 `DialStartTLS` 的调用、或设置 `KeyLogWriter`，测试必须失败。
- [x] **Step 3：** `go get github.com/emersion/go-smtp@v0.25.0`；测试失败。
- [x] **Step 4：实现。** 按上表与「期限的实现」；看门狗与 ctx 监视合并为一个 goroutine：它等待 `ctx.Done()` 或当前步骤的计时器到期，任一发生即调用 `client.Close()`；`Send` 返回前停止该 goroutine。`*smtp.SMTPError` 转为 `*ReplyError`，丢弃其 `Message`。
- [x] **Step 5：** `go test -race ./internal/mail/smtp/` 通过，覆盖率 ≥ 90%；`CGO_ENABLED=0 go test ./internal/mail/smtp/`、`GOOS=windows go vet ./internal/mail/smtp/` 通过；`make modverify` 与 `make secrets` 通过。
- [x] **Step 6：** `git add go.mod go.sum internal/mail/smtp && git commit -m "feat(smtp): submit mail over implicit TLS with per-step deadlines"`

**实施说明：** 先写测试：`smtp_test.go` 与 `source_test.go` 写好后，`go test ./internal/mail/smtp/` 因缺少模块失败（`no required module provides package github.com/emersion/go-sasl`）；`go get github.com/emersion/go-smtp@v0.25.0` 之后编译失败（`undefined: Timeouts`、`undefined: Config`、`undefined: Envelope` 等），再实现。`go get` 同时加入 go-sasl `v0.0.0-20241020182733-b788ff22d5a6`（即 D2 的 `b788ff2`，由 go-smtp 的 `go.mod` 指定），`go mod tidy` 后两者都是直接依赖；`go.mod` 只多这两行，`go.sum` 只多四行哈希。本任务只新增 `internal/mail/smtp` 的三个文件。

实现要点（`smtp.go`）：① 契约中的类型、常量、哨兵与签名照抄；go-smtp 的包名也是 `smtp`，以别名 `gosmtp` 导入。② 按表依次执行：参数校验；ctx 已结束则不拨号；`DialTLS`，`tls.Config` 只由 `tlsConfig` 构造（`ServerName` 为 Host、`RootCAs` 取自配置、`MinVersion` 为 TLS 1.2）；`Hello("localhost")`；确认 TLS 连接且公告 `AUTH PLAIN`；`AUTH PLAIN`；`MAIL`；逐个 `RCPT`；`DATA`；按 64 KiB 分块写正文；经包内函数变量 `closeData` 调用 `CloseWithResponse`；`QUIT`，忽略其结果。`Result.Response` 取 `DataResponse.StatusText`，即 250 之后的文本（含服务器给出的增强状态码，例如 `2.0.0 OK: queued`），按字节截到 512 字节。③ 看门狗是一次 `Send` 唯一的后台 goroutine：`step(d, fn)` 进入前以 d 重置计时器，fn 返回后停止计时，计时器只在步骤内运行（步骤之间没有网络读写）；ctx 结束或计时器到期即调用 `client.Close()` 并退出；`Send` 返回前 `halt` 通知它退出并等待。库的 `CommandTimeout` 与 `SubmissionTimeout` 设为同样的值，作第二道保险。ctx 在 `DialTLS` 期间结束时，看门狗启动后立即关闭连接，返回 `ErrNotSent`（`DialTLS` 本身最多 30 秒，见「风险与后续」）。④ 错误文本只由本包的固定文字、步骤名与状态码组成。`fail` 按以下规则包装：服务器 4xx/5xx 回复附 `*ReplyError`，丢弃 `Message`；ctx 结束时附 `ctx.Err()`，满足 `errors.Is(err, context.Canceled)`；超时写 `<步骤> timed out`；其余库错误只写 `<步骤> failed`，且不进入错误链，因为它们可能带地址、服务器文本或证书细节。步骤名为 `dial`、`ehlo`、`auth`、`mail`、`rcpt`、`data`、`body`（写正文）与 `submission`。

契约未规定之处的取舍，均写入注释并由测试钉住：a. 参数校验失败返回包装 `ErrNotSent` 的错误（契约为「不包装 `ErrNotSent` 以外的哨兵」），文本以 `invalid` 开头，不含地址与密码。契约列表之外补了三项：From 非空且不含 CR/LF（From 与 Username 同为空时能通过相等检查）；每个收件人非空；期限不得为负（负值会让计时器立即到期）。b. 契约写「535 等拒绝为 `ErrAuth`」，实现把认证步骤的全部 5xx 回复都算作 `ErrAuth`；4xx（例如 454）只是 `ErrNotSent`，附 `Temporary()` 为真的 `ReplyError`。认证步骤的非回复错误也只是 `ErrNotSent`，例如 334 的挑战不是合法 base64。c. 问候与 EHLO 失败不附 `ReplyError`：契约表中该行只写 `ErrNotSent`，`ReplyError.Step` 的取值也不含 ehlo。这样 4b 按「`ErrNotSent` 附 5xx → `AbandonNotification(rejected)`」映射时，不会把服务器对连接的拒绝（例如问候阶段的 554）当成对这封邮件的拒绝。d. 只有 400–599 的回复转为 `ReplyError`；意外的 2xx、3xx 或 6xx 回复按普通失败处理，提交阶段为 `ErrUncertain`，之前为 `ErrNotSent`。e. EHLO 名为常量 `ehloName`，显式调用 `Hello("localhost")`，与库的默认值相同。这样「问候与 EHLO」成为一个有期限的步骤（契约表中两者同为一行、一个 30 秒期限），失败也不会被 `SupportsAuth` 吞掉。f. 看门狗计时器到期与库连接期限到期（`os.ErrDeadlineExceeded`）都写作超时。两者期限相同，库的期限只比看门狗晚设几微秒，哪一个先生效取决于调度；只看看门狗的标志，文本会在 `timed out` 与 `failed` 之间抖动。

与契约的一处偏差：「期限的实现」写「一个原子标志记录当前是否已进入 `CloseWithResponse`，据此把关闭造成的错误分为 `ErrNotSent` 或 `ErrUncertain`」。实现在 `Send` 自身的 goroutine 内分类：失败发生在哪一步，就用哪一步的结果类别（提交一步为 `ErrUncertain`，之前各步为 `ErrNotSent`）。看门狗只负责关闭连接，不读取这一状态，所以没有另设原子的阶段标志；看门狗唯一的原子变量 `expired` 记录连接是否因计时器到期而关闭，只用于错误文本。语义与契约相同：契约的标志在调用 `CloseWithResponse` 之前置位，这里对应提交一步开始时以 `Submission` 重置计时器，flush 期间的失败同样归为 `ErrUncertain`，由 `TestSendFlushStall` 钉住。

测试：`smtp_test.go` 用 go-smtp 服务端作假服务器，后端记录认证、信封、EHLO 名与 DATA，服务端的调试输出接到 `transcript`，记录 TLS 之内的往来内容。证书取自 `httptest.NewUnstartedServer(nil)` 的 `StartTLS()`，根证书池只含 `Certificate()`。所有用例都传入非 nil 的根证书池，不调用系统证书校验；证书不受信任的用例用空证书池，而不是 nil。
- `TestSendSuccess`：两个收件人；断言用户名、密码、EHLO 名 `localhost`、MAIL、RCPT、Response；去掉点填充后的 DATA 等于输入（含以点开头的行与只有一个点的行）；服务端收到 `QUIT`。
- `TestSendDefaultResponseAndTruncation`：库默认文本、恰为 512 字节与超长三种响应。
- `TestSendValidatesBeforeDialing`：22 种非法参数，监听器没有收到连接，错误为 `ErrNotSent` 且是校验错误；另验证恰为 4 MiB 的邮件与由 94 个可打印 ASCII 组成的密码可以发送。
- `TestSendCanceledBeforeDialing`。
- `TestSendRefusesPlaintextServer`：明文 go-smtp 服务端开启 `AllowInsecureAuth` 并公告 `AUTH PLAIN`，监听器记录全部入站字节。等服务端读到连接结束后断言没有 `AUTH`，也没有密码。
- `TestSendRejectsUntrustedCertificate`、`TestSendVerifiesHostname`（Host 为 `localhost`）。
- `TestSendRequiresAuthPlain`：不公告 AUTH 与只公告 LOGIN 两种情形。
- `TestSendClassifiesReplies`：认证 535 与 534 同时满足 `ErrAuth` 与 `ErrNotSent`，且没有发出 MAIL；认证 454 不是 `ErrAuth`；MAIL 553 不带增强状态码；RCPT 550 之后没有进入 DATA；结束标记后 451 与 554。
- `TestSendUncertainWhenSubmissionStalls`：`Command` 10 秒、`Submission` 200ms；服务端收到完整 DATA 后不回复，Send 返回 `submission timed out`。
- `TestSendBodyWriteStall`：4 MiB 邮件，`SetReadBuffer(4096)`，`Command` 200ms、`Submission` 10 秒，返回 `body timed out`。放行服务端后会话随即结束，说明客户端已关闭连接；若连接仍打开，服务端会一直等待结束标记。
- `TestSendFlushStall`：`closeData` 替身在服务端发现连接被关闭前不返回；看门狗缺席时替身 5 秒后放弃，用例以耗时失败。
- `TestSendGreetingStall`，以及 `TestSendCanceled`（阻塞在 RCPT 与结束标记之后两种情形）。

各阻塞用例断言 2 秒内返回、错误文本以 `<步骤> timed out` 或 `<步骤>: context canceled` 结尾，以及服务端观察到连接关闭。所有错误用例都断言错误文本不含密码、三个地址、`example.invalid`、邮件金丝雀与 `SERVER-TEXT-CANARY`。Step 1 中「DATA 读完后阻塞直到测试结束」实现为「阻塞到客户端关闭连接」，服务器至迟在用例结束时关闭连接，这样还能断言连接已关闭。

契约之外补了：
- `TestSendSlowReaderWithinChunkDeadlines`：服务端每读 64 KiB 停顿 10ms，4 MiB 邮件在 `Command` 200ms 下投递成功。本机写正文的循环约 0.8 秒，由此钉住「每块一个期限」，而不是整封正文一个期限。
- `TestSendUnusualReplies`：`startScripted` 是按脚本应答的 TLS 假服务器，用来给出 go-smtp 服务端给不出的回复。用例覆盖：DATA 命令被 554 拒绝，得到 `ReplyError{Step: "data"}`；DATA 命令回 250、结束标记后回 354 或 600 时不附 `ReplyError`，分别为 `ErrNotSent` 与 `ErrUncertain`；MAIL 时断线为 `mail failed`；EHLO 554 不附 `ReplyError`；334 的挑战不是合法 base64 时不算 `ErrAuth`。
- `TestSendIgnoresQuitFailure`：邮件被接受后 QUIT 无回应，2 秒内照常返回成功。
- `TestWatchdogHalt`、`TestReplyError`、`TestTimeouts`。

`source_test.go` 按 Step 2 检查白名单与禁用标识符；另要求 `tls.Config` 字面量的字段恰为 `ServerName`、`RootCAs`、`MinVersion`，并把 `tlsConfig` 之外对 `tls.Config` 的任何引用（例如 `new(tls.Config)`）报告为违规。`TestSourceProblemsDetectsViolations` 用内存中的源码确认检查本身有效，包括改名导入。测试只连接 127.0.0.1 上的本地假服务器，不连接任何真实服务器，不涉及钥匙串。测试密码用 `strings.Repeat("pw", 8)` 在运行时构造，没有使用 `gitleaks:allow`。

变异测试在仓库副本中进行，每次只改一处。契约点名的三项：
- 去掉写正文计时器（在步骤之外直接写）：`TestSendBodyWriteStall` 阻塞到测试期限（`-timeout 40s` 时报 `test timed out`）。本机 socket 缓冲没有吸收全部 4 MiB 正文，不需要再缩小缓冲。
- 去掉提交阶段的看门狗：`TestSendFlushStall` 在 5 秒后以耗时失败。
- 在副本中加入 `gosmtp.Dial`、`gosmtp.DialStartTLS`，或设置 `KeyLogWriter`：`TestSourceRestrictions` 分别失败。

全部 75 个变异中，67 个被杀死，涉及：各项参数校验的去掉与边界；结果分类（提交阶段失败归为 `ErrNotSent`、拒绝报告为结果不确定、`ErrAuth` 的判定范围与包装、ehlo 附 `ReplyError`）；`replyOf` 的上下界；`fail` 忽略 ctx 或计时器、保留库错误文本；各步骤名；截断与 512 的边界；默认期限；分块大小（审查指出原测试只能区分分块与整封，见下文「审查后修正」）；整封正文共用一个计时器；EHLO 名；`AUTH PLAIN` 检查；固定 `ServerName`；去掉 `MinVersion`（只被源码检查杀死，功能上等价；降级变异见下文「审查后修正」）；只发第一个收件人；绕过 `closeData`；计时器到期不关闭连接；看门狗不监视 ctx；`halt` 不等待；跳过 QUIT；QUIT 失败时返回错误。其中 halt、QUIT 两项是补测后才被杀死的。去掉 `RootCAs` 的变异只对源码检查运行（被杀死），没有运行功能测试，以免调用系统证书校验。

存活 8 个。审查指出其中 Hello 与 `os.ErrDeadlineExceeded` 两项并不等价，已补测试杀死，见下文「审查后修正」；其余为等价或设计上的冗余：
- Hello 或 QUIT 放在看门狗之外、去掉库的 `CommandTimeout` 或 `SubmissionTimeout`（4 个）：命令步骤与读取提交响应有看门狗与库期限两道保险，期限相同，任一道单独都够；写正文与 flush 只有看门狗，去掉即被杀死。（更正：这只对一次往来的步骤成立，Hello 不在此列。）
- 步骤之间不停止计时（2 种写法）：步骤之间没有网络读写。
- 去掉 `os.ErrDeadlineExceeded` 分支：本机 3 次运行都是看门狗先生效；保留这个分支是为了让错误文本确定。（更正：不等价，见下文「审查后修正」⑤。）
- 去掉 `TLSConnectionState` 检查：`DialTLS` 得到的一定是 TLS 连接，按契约保留作纵深防御。

验证：
- `go test -race ./internal/mail/smtp/` 通过，覆盖率 100.0%；`-race -count=5` 稳定；与 sqlite、security 各包的 `-race -count=3` 并行运行时，本包 `-race -count=20` 通过。
- `CGO_ENABLED=0 go test ./internal/mail/smtp/` 与 `CGO_ENABLED=0 go test ./...` 通过。
- `GOOS=windows go vet ./internal/mail/smtp/`、全仓 `GOOS=windows go vet ./...` 与 `GOOS=linux go vet ./...` 通过。
- `make modverify`、`make secrets`、`make check` 通过（含中文注释检查与 staticcheck，总覆盖率 93.59%）。

测试只在 macOS 上运行，Linux 上的测试由 CI 运行；写正文阻塞与慢速读取两个用例依赖 socket 缓冲吸收不了全部正文，这一点在 Linux 上未在本机验证。与 Task 1–9 相同，本任务的提交包含本清单的勾选与实施说明。

**审查后修正：** 变异均在仓库副本中进行，每次只改一处（组合变异另行注明）。 复查后补充：`Result.Response` 的注释改为「不含三位状态码，可含增强状态码」；`truncate` 截断点落在 UTF-8 字符中间时退到该字符之前，`TestTruncateRuneBoundary` 钉住。问候与 EHLO 被拒绝时不附 `*ReplyError` 是对契约 `Send` 注释的有意偏差（连接级失败不代表这封邮件被拒），与认证 5xx 的分流顺序、参数校验错误的处理一并写入「4b 与 4a 的衔接」。

① 命令步骤的看门狗计时。原说明把「Hello 放在看门狗之外」列为等价变异，理由是两道期限相同，这不成立。库的 `CommandTimeout` 按单次往来设置：问候与 EHLO 各有一个，EHLO 回 500 或 502 时回退的 HELO 还有一个；AUTH 收到 334 后发 `"*"` 中止，这次往来也另有一个。契约表把「问候与 EHLO」写作一行、一个期限，AUTH 也是一个期限，这两步只有看门狗能保证。新增 `TestSendStepDeadlineSpansExchanges`：脚本服务器每次回复前停顿 200ms，`Command` 为 300ms，每次往来都在库的期限之内，而步骤合计超过 300ms。ehlo 一例让问候与 EHLO 回复各慢 200ms，auth 一例让 334 与 501 各慢 200ms，断言 `ErrNotSent`、不是 `ErrAuth`、错误文本以 `<步骤> timed out` 结尾、2 秒内返回。另新增 `TestSendCommandStalls`：服务器在 EHLO、AUTH、MAIL、RCPT、DATA（至 354）处不再回复，断言与上一用例相同；这些步骤原先只有问候阻塞一个超时用例，RCPT 的阻塞只用取消测过。为此 `startScripted` 增加 `pauses` 参数；它先显式完成握手再停顿，否则停顿会落在 `DialTLS` 的握手里。回复写完后服务器只读不回，5 秒后关闭连接，客户端缺少期限时用例以耗时失败，而不是挂起到测试超时。变异结果：Hello、AUTH 分别移出看门狗，或全部命令步骤的看门狗期限改为 0，被 `TestSendStepDeadlineSpansExchanges` 杀死。组合变异把库的两个超时改为 1 小时，同时把 AUTH、MAIL、RCPT 或 DATA 之一移出看门狗，即两道期限都去掉，被 `TestSendCommandStalls` 的对应子用例杀死；对 Hello 做同样处理时，`TestSendCommandStalls/ehlo` 单独运行在 5 秒时失败，完整运行时原有的 `TestSendGreetingStall` 先阻塞到测试期限。MAIL、RCPT、DATA、QUIT 单独移出看门狗仍然存活：它们只有一次往来，两道期限相同，几乎同时设置，任一道单独都够，是等价变异。契约措辞前后不一：「已定的实现细节」写问候、EHLO「各 30 秒」，`Timeouts.Command` 的注释写「各自的期限」，发送步骤表则把「问候与 EHLO」写作一行 30 秒。实现按表取后者，两者合计一个期限，比各自一个期限更严，现已由测试钉住；措辞是否统一留给维护者决定，本任务不改契约文字。

② 分块大小。原说明称「分块大小」变异被杀死，这不准确：`TestSendSlowReaderWithinChunkDeadlines` 只能区分分块与整封正文共用一个期限，分块改为 128 KiB 到 1 MiB 都能存活。现在仿照 `closeData`，写正文改为经包内函数变量 `writeChunk`（即 `(*gosmtp.DataCommand).Write`）调用。新增 `TestSendWritesBodyInChunks`，替换它来记录每次写入的长度：128 KiB + 1 字节的邮件恰好写成 64 KiB、64 KiB、1 字节三块。分块改为 32 KiB、128 KiB、256 KiB、1 MiB，或绕过 `writeChunk` 直接写，这些变异都被杀死。这是清单之外增加的测试替换点，发送行为不变。

③ 点导入。源码检查原先只看包选择器：点导入 go-smtp 后直接调用 `Dial` 能绕过白名单，点导入 `crypto/tls` 也能绕过对 `tls.Config` 的检查。现在 `sourceProblems` 把任何点导入都报告为违规。这比只禁止受限路径更简单，本包原本也没有点导入。`TestSourceProblemsDetectsViolations` 增加两例：点导入 go-smtp 后调用 `Dial`，以及点导入 `crypto/tls`。在副本的 `smtp.go` 中加入点导入与 `Dial` 调用后，`TestSourceRestrictions` 失败。

④ TLS 最低版本。源码检查只核对 `tls.Config` 字面量的字段名。把 `MinVersion` 降为 TLS 1.0 或 1.1 时，原有测试全部通过，客户端会在 TLS 1.1 上发送凭据。原说明列为已杀死的「去掉 `MinVersion`」只被源码检查杀死，功能上是等价的，因为 Go 客户端的默认最低版本就是 TLS 1.2。新增 `TestSendRefusesOldTLS`：服务器只支持 TLS 1.0–1.1，断言 `ErrNotSent`，且后端没有收到 AUTH。用例先用一条允许 TLS 1.0 的对照连接，确认服务器确实能协商出 TLS 1.1，避免 Go 日后移除旧版本支持时用例空转通过。`MinVersion` 改为 TLS 1.0 或 TLS 1.1 的变异都被杀死。源码检查没有另外核对取值，降级由这个功能用例覆盖。

⑤ `fail` 的 `os.ErrDeadlineExceeded` 分支。原说明列为等价，这不成立：两道期限同时到期时，`Send` 可能先读到库的期限错误，看门狗随后才置位，去掉该分支后错误文本会从 `timed out` 变成 `failed`。新增 `TestWatchdogFailDeadline`，直接调用 `fail`：包装 `os.ErrDeadlineExceeded` 的错误写作 `mail timed out`；其他库错误写作 `submission failed`，且不含库的错误文本。去掉该分支、所有失败都写作超时，这两个变异都被杀死。

修正后的验证：`go test -race ./internal/mail/smtp/` 通过，覆盖率 100.0%；`-race -count=5` 稳定；与 sqlite、security 各包的 `-race -count=3` 并行运行时，新增及改动的用例 `-race -count=10` 通过。`CGO_ENABLED=0 go test ./internal/mail/smtp/`、全仓 `GOOS=windows go vet ./...` 与 `GOOS=linux go vet ./...`、`make check`（总覆盖率 93.59%）、`make secrets` 均通过。

### Task 11：IMAP 客户端

**Files:**
- Create: `internal/mail/imap/session.go`、`internal/mail/imap/watcher.go`
- Test: `internal/mail/imap/fakeserver_test.go`、`internal/mail/imap/session_test.go`、`internal/mail/imap/watcher_test.go`、`internal/mail/imap/source_test.go`
- Modify: `go.mod`、`go.sum`（`go get github.com/emersion/go-imap/v2@v2.0.0-beta.8`；go-message v0.18.2 随之成为间接依赖）

```go
// Package imap 以只读方式从 IMAP 服务器收取新邮件：隐式 TLS 登录、EXAMINE、按 UID 补扫、BODY.PEEK 取信与 IDLE。
// 每条命令都有期限，超时即关闭连接，以应对 QQ 对不支持命令只回无标签 BAD、以及半开连接时库永久阻塞的问题。
// 本包不解析 MIME、不访问存储、不修改邮箱（不设 \Seen、不 MOVE、不 EXPUNGE、不 APPEND）。
package imap

const (
	FolderInbox = "INBOX"
	FolderJunk  = "Junk" // QQ 的垃圾箱名称；以 LIST 结果判断是否存在，不存在时跳过
	// MaxBatch 是一次补扫取回的最多邮件数。
	MaxBatch = 50
	// MaxMessageSize 是取回正文的默认上限；更大的邮件只返回 UID 与大小。实现读取包内变量 maxMessageSize，
	// 其初值为本常量，测试可在包内降低它。
	MaxMessageSize = 2 << 20
	// MaxBatchBytes 是一批中已取回正文的合计上限；达到后本批提前结束并置 More，余下邮件留给下一批。
	// 实现读取包内变量 maxBatchBytes，其初值为本常量，测试可在包内降低它。
	MaxBatchBytes = 16 << 20
)

// Config 是连接参数。
type Config struct {
	Host     string         // 例如 imap.qq.com
	Port     int            // 默认 993；端口可配置，TLS 模式固定为隐式 TLS
	Username string         // 机器人地址
	RootCAs  *x509.CertPool // nil 时使用系统根证书；tls.Config 只在包内构造：ServerName 固定为 Host，MinVersion 为 TLS 1.2
	Timeouts Timeouts
}

// Timeouts 是逐项期限；零值字段使用 DefaultTimeouts 中的对应值。
type Timeouts struct {
	Dial     time.Duration // TCP 连接与 TLS 握手，15 秒
	Greeting time.Duration // 服务器问候，15 秒
	Command  time.Duration // CAPABILITY、LOGIN、LIST、EXAMINE、UID SEARCH、LOGOUT，各 30 秒
	Fetch    time.Duration // 一批 UID FETCH 中的每封邮件，60 秒
	IdleAck  time.Duration // 发出 IDLE 到收到继续响应，30 秒
	IdleMax  time.Duration // 一次 IDLE 的最长持续时间，5 分钟；到期即结束并补扫
	IdleStop time.Duration // 发出 DONE 到 IDLE 完成，10 秒
	Poll     time.Duration // 服务器不支持 IDLE 时两次补扫的间隔，2 分钟
}

// DefaultTimeouts 返回生产期限。这些数值在 L1 测得 IDLE 推送延迟与服务器断开时间后可调整，调整须写入本清单。
func DefaultTimeouts() Timeouts

// Cursor 是某个文件夹的收取游标；与 store/sqlite.Cursor 字段相同，但本包不依赖存储。
type Cursor struct {
	UIDValidity uint32
	LastUID     uint32
}

// Message 是取回的一封邮件；超过 MaxMessageSize 时 Raw 为 nil、TooLarge 为 true。
type Message struct {
	UID      uint32
	Size     int64
	Raw      []byte
	TooLarge bool
}

// Batch 是一次补扫的结果。Reset 为 true 表示 UIDVALIDITY 与游标不同（或没有游标），本批从 UID 1 开始全量补扫。
// Next 是本批全部处理完成后应持久化的游标；Messages 按 UID 升序。
type Batch struct {
	Folder      string
	UIDValidity uint32
	Reset       bool
	Messages    []Message
	Next        Cursor
	More        bool // 本批之外还有新邮件：UID SEARCH 结果超出 MaxBatch 封，或已取回正文合计达到 MaxBatchBytes 而提前结束；
	                 // 调用方处理本批后应以 Next 立即再次 Scan
}

var (
	// ErrAuthFailed 表示 LOGIN 被拒绝；授权码可能错误，或已因修改 QQ 密码而失效。
	ErrAuthFailed = errors.New("imap: authentication failed")
	// ErrTimeout 表示某条命令超过期限（包括无标签 BAD 造成的挂起与半开连接）；连接已被关闭。
	ErrTimeout = errors.New("imap: command timed out")
	// ErrClosed 表示连接已被服务器或本端关闭。
	ErrClosed = errors.New("imap: connection closed")
	// ErrCapability 表示服务器没有公告 IMAP4rev1、公告了 LOGINDISABLED（此时不发送凭据），或调用 Idle 时没有公告 IDLE。
	ErrCapability = errors.New("imap: required capability missing")
	// ErrNoFolder 表示 EXAMINE 被服务器以 NO 拒绝。原因可能是文件夹不存在，也可能是暂时不可用（例如 [UNAVAILABLE]），
	// 本包不区分；文件夹是否存在由调用方按 ListFolders 的结果判断。
	ErrNoFolder = errors.New("imap: folder cannot be examined")
)

// Session 是一条已登录的连接；方法不可并发调用。
type Session struct { /* imapclient.Client、能力集合、期限、当前 EXAMINE 的文件夹 */ }

// Dial 以 imapclient.DialTLS 连接，等待问候，读取 CAPABILITY，确认 IMAP4rev1 且未公告 LOGINDISABLED 后 LOGIN，
// 登录后重新读取 CAPABILITY。LOGIN 被拒绝返回 ErrAuthFailed。
func Dial(ctx context.Context, cfg Config, password string) (*Session, error)

// Capabilities 返回登录后的能力名称（大写，排序），供 L1 记录。
func (s *Session) Capabilities() []string

// ListFolders 执行 LIST "" "*"，返回文件夹名称（修改版 UTF-7 原样），供 L1 记录与确认 Junk 是否存在。
func (s *Session) ListFolders(ctx context.Context) ([]string, error)

// Mailbox 是 EXAMINE 返回的文件夹状态。
type Mailbox struct {
	UIDValidity uint32
	UIDNext     uint32
	Messages    uint32
}

// Examine 以只读方式选中 folder 并返回其状态；EXAMINE 被拒绝时返回 ErrNoFolder。L1 用它记录各文件夹的 UIDVALIDITY。
func (s *Session) Examine(ctx context.Context, folder string) (Mailbox, error)

// Scan 对 folder 执行 EXAMINE；UIDVALIDITY 与 cur 不同则从 UID 1 全量补扫（Reset）。
// 执行 UID SEARCH UID <LastUID+1>:*，丢弃 uid ≤ LastUID 的结果（没有新邮件时服务器仍返回最后一个 UID），
// 取最小的 MaxBatch 个，先 UID FETCH (UID RFC822.SIZE)，再对声明大小不超过 maxMessageSize 的逐封
// UID FETCH (BODY.PEEK[]<0.maxMessageSize+1>)。字面量以流式读取，至多读 maxMessageSize+1 字节：超出即判为 TooLarge、
// 其余部分读出丢弃（受 Fetch 期限约束），因此服务器少报 RFC822.SIZE 或不遵守部分取回时内存仍有上界。
// 已取回正文合计达到 MaxBatchBytes 时本批提前结束并置 More。
func (s *Session) Scan(ctx context.Context, folder string, cur Cursor) (Batch, error)

// Idle 若发现自上次 Scan 开始以来已收到 EXISTS（例如补扫的 UID FETCH 进行中新邮件到达；以 EXISTS 通道中是否有信号判断，
// 见 Step 5），不发送 IDLE，取走信号并立即返回 true。
// 否则在已 EXAMINE 的文件夹上执行 IDLE，直到收到 EXISTS（返回 true）、达到 IdleMax（返回 false）或 ctx 结束；
// 然后发出 DONE 并在 IdleStop 内等待完成，超时即关闭连接并返回 ErrTimeout。服务器未公告 IDLE 时返回 ErrCapability。
func (s *Session) Idle(ctx context.Context) (newMail bool, err error)

// Close 在 Command 期限内尽力发送 LOGOUT，然后关闭连接。
func (s *Session) Close() error

// Status 是 Watcher 的状态通知，不含地址、密码或邮件内容。
type Status struct {
	Kind   StatusKind    // connected、disconnected、auth_failed、backoff、credentials_unavailable、handle_failed、
	                     // idle_disabled、folder_missing、folder_unavailable
	Delay  time.Duration // backoff、auth_failed、credentials_unavailable、handle_failed 时为下次尝试前的等待
	Folder string        // folder_missing 与 folder_unavailable 时为文件夹名
}

// StatusKind 是状态种类。
type StatusKind string

// Backoff 是重连退避参数；零值字段使用默认值。
type Backoff struct {
	Initial     time.Duration // 15 秒；两次登录之间也至少间隔 Initial
	Max         time.Duration // 10 分钟
	AuthPause   time.Duration // 认证失败后的暂停，15 分钟；不得小于 10 分钟（测试可在包内降低下限）
	Jitter      float64       // ±20%
	MaxLogins   int           // LoginWindow 内至多登录的次数，12；达到后等到窗口内最早一次登录满 LoginWindow 再登录
	LoginWindow time.Duration // 1 小时
}

// Watcher 维护长连接收取循环。Password 在每次登录前调用；4b 装配时它返回进程启动时读出、缓存在内存中的授权码，
// 只在认证失败后才重新读取 Keychain（见「4b 与 4a 的衔接」），因此重连不会反复启动 security 或触发钥匙串弹窗。
// Password 返回错误时不连接，发出 credentials_unavailable 并等待 Max 后再试，不计入连接失败与登录次数。本包不知道凭据来自
// 何处，所以状态名是通用的 credentials_unavailable；4b 装配层把它报告为 keychain_unavailable（见「4b 与 4a 的衔接」）。
// Cursor 返回某文件夹已持久化的游标（没有时返回零值与 nil）；Handle 处理一批邮件并在成功后持久化 b.Next。
// Handle 返回错误（本地数据库忙、正文密钥不可用等）时本批游标不前进，Watcher 发出 handle_failed，按退避等待后
// 在同一连接上以 Cursor 重新补扫并再次交付同一批，不断开、不重新登录；连接在等待期间断开时按正常重连处理。
type Watcher struct {
	Config   Config
	Password func(ctx context.Context) (string, error)
	Cursor   func(ctx context.Context, folder string) (Cursor, error)
	Handle   func(ctx context.Context, b Batch) error
	Status   func(Status) // 可为 nil
	Backoff  Backoff
}

// Run 循环直到 ctx 结束并返回 ctx.Err()：连接并登录 → LIST 确定 Junk 是否存在 → 依次对 Junk（存在时）、INBOX
// 补扫直到 More 为 false → 在 INBOX 上 IDLE（不支持或已降级时等待 Poll）→ 回到补扫。连接与命令错误关闭连接并按退避重连；
// 认证失败暂停 AuthPause 并发出 auth_failed 状态。同一连接上每小时重新 LIST 一次。Watcher 在一次 Run 中记住上一次 LIST
// 对 Junk 是否存在的结论，初值为「存在」：因此首次 LIST 就没有 Junk 时发出一次 folder_missing，此后只在由存在变为缺失时
// 再发出一次；重连不重置这一结论，Junk 一直缺失时不会每次重连都重复发出。LIST 中存在但 EXAMINE 返回 ErrNoFolder 时
// 本轮跳过 Junk 并发出 folder_unavailable，下一轮照常重试。
func (w *Watcher) Run(ctx context.Context) error
```

**命令白名单：** 本包只发送 `CAPABILITY`、`LOGIN`、`LIST`、`EXAMINE`、`UID SEARCH`、`UID FETCH`、`IDLE`/`DONE`、`LOGOUT`。`IDLE` 只在公告 `IDLE` 时发送；不发送 `ID`、`ENABLE`、`UNSELECT`、`CLOSE`、`NOOP`、`STATUS`、`SEARCH`、`FETCH`（非 UID）、`NAMESPACE`、`SELECT`、`STORE`、`COPY`、`MOVE`、`EXPUNGE`、`APPEND`、`STARTTLS`、`AUTHENTICATE`，以及 ACL、METADATA、QUOTA 类命令。所有命令经一个内部包装：启动计时器，在期限或 ctx 结束时调用 `client.Close()` 并返回 `ErrTimeout`（或 ctx 的错误），同时监听 `client.Closed()` 返回 `ErrClosed`。`client.Close()` 要等解码协程退出，包装在独立 goroutine 中调用它，至多等待 `IdleStop`，超时即视为已关闭并返回，不让关闭本身再次挂起。

**退避与登录频率：** 退避只在连接自登录起保持健康达到 `IdleMax`、或一次 IDLE 正常结束（收到 EXISTS，或到达 `IdleMax` 后 DONE 成功）后复位；补扫成功不复位，否则只在 IDLE 阶段出现的故障会让退避永远停在 `Initial`，形成持续的登录。两次登录之间至少间隔 `Initial`，任意 `LoginWindow` 内至多 `MaxLogins` 次，达到上限时发出 `backoff` 并等待。同一次 `Run` 中 `Idle` 连续 2 次返回 `ErrTimeout`（包括对 IDLE 只回无标签 BAD 造成的 `IdleAck` 超时）后，本次 `Run` 余下时间不再发送 IDLE，改为每隔 `Poll` 补扫，并发出一次 `idle_disabled`。

**离线假服务器（`fakeserver_test.go`）：** `imapserver` 加 `imapmemserver` 在普通监听器上提供标准行为，测试用户与邮件通过 imapmemserver 的用户接口放入。`imapserver` 在明文连接上默认公告 `LOGINDISABLED` 并拒绝认证，因此须设置 `Options.InsecureAuth = true`（TLS 由前面的代理终结）。其前面是一个只在测试中存在的 TLS 代理（证书同 Task 10 借用 `httptest`），代理记录客户端发出的每条命令：标签与命令名，`UID FETCH` 另记录数据项列表，从不记录 `LOGIN` 的参数。代理支持以下故障：

| 故障 | 代理行为 | 模拟的真实情况 |
| --- | --- | --- |
| 无标签 BAD | 指定命令名的请求行不转发，只回 `* BAD Command!\r\n`，永不完成该标签 | QQ 对不支持命令的响应 |
| 半开 | 冻结两个方向的转发但不关闭连接 | 网络静默丢包、休眠唤醒 |
| 问候冻结 | 完成 TLS 握手后不转发服务器问候 | 服务器接受连接后无响应 |
| 命令后冻结 | 收到指定命令后冻结两个方向 | 命令发出后网络静默 |
| 响应中途冻结 | 收到指定命令后，转发其响应的前 N 字节后冻结 | 字面量传到一半时连接静默 |
| 拒绝 | 指定命令不转发，回 `<标签> NO [UNAVAILABLE] …`，可限定只拒绝前 k 次 | 文件夹暂时不可用 |
| 注入 | 收到指定命令后，在转发其响应之前插入一条或多条 `* N EXISTS`（每条的 N 可分别指定） | 补扫进行中新邮件到达 |
| 大小改写 | 把 `RFC822.SIZE` 的值改为指定数值 | 服务器少报邮件大小 |
| 断开 | 立即关闭两侧连接 | 服务器超时断开 |
| 能力改写 | 从 CAPABILITY 响应与问候中删去指定能力（例如 `IDLE`、`AUTH=PLAIN`），或追加指定能力（例如 `LOGINDISABLED`） | 能力列表不同的服务器 |
| 明文 | 不做 TLS，直接转发 | 143 端口式的明文入口 |

UIDVALIDITY 变化通过在 imapmemserver 中删除并重建 Junk 文件夹模拟（若它不允许删除 INBOX）。

- [x] **Step 1：写失败的测试（`session_test.go`）。** 期限全部设为 100–500ms 级别。
  - **收取与游标：** INBOX 放入 3 封、Junk 放入 1 封；`Scan(INBOX, Cursor{})` 返回 `Reset = true`、3 封按 UID 升序且 `Raw` 与放入的字节相同、`Next = {uv, 3}`；以 `Next` 再次 `Scan` 返回空批（证明过滤了服务器对 `4:*` 返回的 UID 3）；再放入 1 封后只返回第 4 封。
  - **只读：** 以上操作后 imapmemserver 中所有邮件都没有 `\Seen`；代理记录的命令中没有 `SELECT`、`STORE`、`COPY`、`MOVE`、`EXPUNGE`、`APPEND`，每条 `UID FETCH` 的正文项都是 `BODY.PEEK[]<0.N>`（N 为 `maxMessageSize + 1`）。
  - **白名单：** 全部用例中代理记录的命令名都在白名单内；能力改写删去 `IDLE` 时 `Idle` 返回 `ErrCapability` 且没有发出 `IDLE`；删去 `AUTH=PLAIN` 并追加 `LOGINDISABLED` 时 `Dial` 返回 `ErrCapability`，代理没有收到 `LOGIN`。
  - **每条命令都有期限：** 以下每个用例都断言返回 `ErrTimeout`、耗时不超过对应期限加 1 秒，且此后该会话的任何调用返回 `ErrClosed`：
    - 问候冻结：`Dial` 在 `Greeting` 内返回；
    - `LOGIN` 后冻结：`Dial` 在 `Command` 内返回；
    - 对 `EXAMINE` 与 `UID SEARCH` 分别注入无标签 BAD：`Scan` 在 `Command` 内返回；
    - 单封 `UID FETCH (BODY.PEEK[]…)` 的响应转发 512 字节后冻结（字面量传到一半）：`Scan` 在 `Fetch` 内返回；
    - 对 `IDLE` 注入无标签 BAD（没有继续响应）：`Idle` 在 `IdleAck` 内返回；
    - 进入 IDLE 后冻结（半开）：`Idle` 在 `IdleMax + IdleStop` 内返回。
  - **补扫中到达的 EXISTS：** 分别在 `UID SEARCH` 与 `UID FETCH` 的响应前注入 `* N EXISTS`：`Scan` 正常完成；随后 `Idle` 不发送 IDLE、立即返回 `true`。
  - **处理函数不阻塞：** 在 `UID SEARCH` 的响应前连续注入两条 EXISTS（N 与 N+1），其间没有任何一方取走通道中的信号：`Scan` 仍在期限内正常完成，随后 `Idle` 立即返回 `true`。处理函数改为阻塞发送时，第二条 EXISTS 会使解码协程停住，`Scan` 以 `ErrTimeout` 失败，本用例因此能拦住该变异；只注入一条的用例拦不住它。
  - **不残留旧信号：** 一次 `Scan` 中注入 EXISTS 后不调用 `Idle`，直接再 `Scan` 一次（不注入），随后 `Idle` 发出 IDLE（代理有记录），无新邮件时在 `IdleMax` 到期返回 `false`。`Scan` 开始时不排空通道的变异使 `Idle` 立即返回 `true` 而失败。
  - **IDLE 推送：** IDLE 期间向 INBOX 放入 1 封，`Idle` 在 1 秒内返回 `true`；无新邮件时在 `IdleMax` 到期返回 `false` 且连接仍可用。
  - **UIDVALIDITY 变化：** 游标为 `{旧 uv, 5}`，删除并重建 Junk 后放入 2 封，`Scan(Junk, 游标)` 返回 `Reset = true`、从 UID 1 开始的 2 封与新的 UIDVALIDITY。
  - **批量与大小：** 放入 120 封时依次得到 50、50、20 封，前两批 `More = true`。`maxMessageSize` 在测试中降为 1 KiB：一封 4 KiB 的邮件返回 `TooLarge = true`、`Raw = nil`，代理记录显示没有对该 UID 请求正文；同一封邮件在「大小改写」把 `RFC822.SIZE` 改为 100 时，请求的是 `BODY.PEEK[]<0.1025>`，结果仍为 `TooLarge = true`、`Raw = nil`。`MaxBatchBytes` 在测试中降低后，本批在达到上限时提前结束并置 `More`，下一批从未交付的第一封开始。
  - **认证与传输安全：** 错误密码返回 `ErrAuthFailed`，错误文本不含密码；明文模式下 `Dial` 失败，代理没有收到任何命令（握手失败前没有明文命令发出）；证书不受信任同样失败；CA 受信但 Host 为不在证书 SAN 中的 `localhost` 时同样失败，代理没有收到 `LOGIN`。
  - **文件夹：** 不存在的文件夹在 `Examine` 与 `Scan` 中都返回 `ErrNoFolder`，连接仍可用；「拒绝」故障对 `EXAMINE Junk` 回 `NO [UNAVAILABLE]` 时同样返回 `ErrNoFolder`；`Examine(INBOX)` 返回的 UIDVALIDITY、UIDNEXT 与邮件数与 imapmemserver 中一致。
- [x] **Step 2：写失败的测试（`watcher_test.go`）。** `Handle` 把收到的批次写入测试内的切片并更新内存游标；`Status` 写入通道；`Backoff` 与期限在测试中缩短（登录窗口为秒级）。
  - 正常：启动后依次收到 Junk 与 INBOX 的批次；IDLE 期间放入的邮件在 1 秒内交给 `Handle`；每封邮件只交付一次。
  - `Handle` 对某批返回错误：发出 `handle_failed`，按退避等待后同一批被再次交付（游标未前进），之后成功处理；整个过程中代理没有收到新的 `LOGIN`，连接没有断开。
  - 断开与半开：代理断开或冻结后，`Watcher` 发出 `disconnected` 与 `backoff`，按退避重连并继续交付新邮件；退避间隔依次约为 Initial、2×Initial（在 ±20% 内）；一次 IDLE 正常结束后复位。
  - 只在 IDLE 阶段出现的故障：每次进入 IDLE 后代理即断开，补扫每次都成功；至少 5 轮中登录间隔单调不减（直到 Max），任一 `LoginWindow` 内的登录次数不超过 `MaxLogins`。去掉「补扫成功不复位」或登录上限的变异使该用例失败。
  - 对 IDLE 注入无标签 BAD：前两次 `Idle` 超时后发出一次 `idle_disabled`，此后代理不再收到 `IDLE`，新邮件在 `Poll` 间隔内交付。
  - 对 `UID SEARCH` 注入无标签 BAD：`Watcher` 超时断开并重连，故障解除后恢复交付。
  - 补扫的 `UID FETCH` 进行中注入 EXISTS：新邮件在下一轮交付；随后取消 ctx，`Run` 在 1 秒内返回。
  - 认证失败：发出 `auth_failed`，其 `Delay` 等于 `AuthPause`；`AuthPause` 期间代理没有收到新的 `LOGIN`。生产默认值：`Backoff{}` 的有效 `AuthPause` 为 15 分钟，传入 5 分钟时按 10 分钟下限处理；有效 `MaxLogins` 为 12、`LoginWindow` 为 1 小时。
  - `Password` 返回错误时不连接，发出 `credentials_unavailable`，其 `Delay` 等于 `Max`，不计入登录次数。
  - Junk 不存在时启动：恰好发出一次 `folder_missing`，之后只扫描 INBOX；代理断开触发重连与重新 LIST 后不再发出；在 imapmemserver 中创建 Junk 并断开，重连后开始扫描 Junk，不发出 `folder_missing`；再删除 Junk 并断开，重连后再发出一次。Junk 在 LIST 中存在但第一次 `EXAMINE Junk` 被拒绝（`NO [UNAVAILABLE]`）时发出 `folder_unavailable`，下一轮照常扫描 Junk 并交付其中的邮件。
  - 取消：IDLE 中取消 ctx，`Run` 在 1 秒内返回 `context.Canceled`，服务端会话已结束。
- [x] **Step 3：写失败的测试（`source_test.go`）。** 用 `go/parser` 解析本包非测试文件，再用 `go/types` 做类型检查（导入器用标准库 `go/importer.ForCompiler(fset, "source", nil)`，不引入新依赖），按白名单检查：
  - imapclient 包的函数只允许 `DialTLS`（类型名不受限）；
  - 接收者为 `*imapclient.Client` 的方法调用与方法值只允许 `Capability`、`Caps`、`WaitGreeting`、`Login`、`List`、`Select`、`UIDSearch`、`Fetch`、`Idle`、`Logout`、`Close`、`Closed`、`Mailbox`；
  - 每次 `Select` 调用的选项是带 `ReadOnly: true` 的 `imap.SelectOptions` 字面量；每次 `Fetch` 的第一个参数的静态类型是 `imap.UIDSet`（序号集会发出非 UID 的 `FETCH`）；每个 `imap.FetchItemBodySection` 与 `imap.FetchItemBinarySection` 字面量都带 `Peek: true`；
  - 不出现 `DebugWriter`、`InsecureSkipVerify`、`KeyLogWriter`、`VerifyPeerCertificate`、`VerifyConnection`；`tls.Config` 字面量只出现一次，位于包内构造它的函数中。

  在临时副本中分别加入 `UnselectAndExpunge`、`Noop` 或 `Move` 调用，把 `ReadOnly` 改为 false，去掉 `Peek`，或以 `imap.SeqSet` 调用 `Fetch`，测试都必须失败。
- [x] **Step 4：** `go get github.com/emersion/go-imap/v2@v2.0.0-beta.8`；测试失败。确认 go-message v0.18.2 以间接依赖进入 `go.mod`，`go list -deps ./internal/mail/imap/ | grep golang.org/x/text` 无输出，`make security` 无发现。
- [x] **Step 5：实现。** 按契约；`Options.Dialer` 设为 `&net.Dialer{Timeout: Dial, KeepAlive: 15 * time.Second}`。imapclient 在自己的解码协程中同步调用 `UnilateralDataHandler`，处理函数阻塞会让解码协程停住，而 `client.Close()` 又要等解码协程退出，两者会互相等待。因此 `UnilateralDataHandler.Mailbox` 在收到 EXISTS（`NumMessages` 非空）时只做非阻塞通知：以 `select … default` 向容量为 1 的通道发送，通道已满时直接丢弃（一个待处理的信号已足以触发补扫），从不阻塞。这个通道是「自上次 `Scan` 开始以来是否收到过 EXISTS」的唯一状态，不另设原子标志：`Scan` 在发出 EXAMINE 之前以非阻塞读取排空通道；`Idle` 先非阻塞检查通道，有信号即取走并返回 true、不发送 IDLE，否则进入 IDLE 并等待该通道。排空发生在 EXAMINE 之前，此后到达的 EXISTS 都会留下信号，不会丢失：新邮件要么已被本次 `Scan` 的 UID SEARCH 覆盖，要么让下一次 `Idle` 立即返回、再补扫一轮。不设置 `WordDecoder`（本包不读 ENVELOPE）。
- [x] **Step 6：** `go test -race -count=3 ./internal/mail/imap/` 通过、无抖动，覆盖率 ≥ 85%；`CGO_ENABLED=0 go test ./internal/mail/imap/`、`GOOS=windows go vet ./internal/mail/imap/`、`make modverify` 通过；提交前运行 `make secrets`，覆盖实现过程中改动的测试文件。
- [x] **Step 7：** `git add go.mod go.sum internal/mail/imap && git commit -m "feat(imap): read-only fetch with command deadlines, idle watchdog and reconnect"`

**实施说明：** 先写测试：`fakeserver_test.go`、`session_test.go`、`watcher_test.go` 与 `source_test.go` 写好后，`go test ./internal/mail/imap/` 因缺少模块失败（`no required module provides package github.com/emersion/go-imap/v2`）。`go get github.com/emersion/go-imap/v2@v2.0.0-beta.8` 只把 go-imap/v2 记为间接依赖，测试随即报 `missing go.sum entry for module providing package github.com/emersion/go-message`（测试导入的 `imapserver` 需要它）；执行 `go mod tidy` 后编译失败（`undefined: Watcher`、`undefined: Cursor`、`undefined: Timeouts`、`undefined: Session` 等），再实现。`go.mod` 只多两行：go-imap/v2 v2.0.0-beta.8 为直接依赖，go-message v0.18.2 为间接依赖。`go.sum` 多 35 行：两个模块各两行哈希，另 31 行都是只含 `/go.mod` 的哈希，来自 go-message 未剪枝的模块图（它的 `go.mod` 声明 go 1.14，要求 x/text v0.14.0，后者又引出 x/tools、x/net、x/sys 等旧版本的 `go.mod`）。`go list -deps ./internal/mail/imap/ | grep golang.org/x/text` 无输出；`go list -m all` 中 x/text 仍是 v0.14.0，只在模块图里，留给 Task 13 固定。`make security` 通过：govulncheck 无发现，gitleaks 无泄漏。本任务只新增 `internal/mail/imap` 的六个文件。

实现要点（`session.go`）：
- ① 契约中的常量、类型、哨兵与签名照抄。`maxMessageSize`、`maxBatchBytes` 是包内变量，初值为对应常量。
- ② 每一步都经 `do` 执行：`fn` 在独立 goroutine 中运行，计时器到期、ctx 结束或 `client.Closed()` 关闭时关闭连接，分别返回 `ErrTimeout`、ctx 的错误（错误链可用 `errors.Is` 判断）与 `ErrClosed`。`fn` 的返回值分两类：服务器带标签的 NO/BAD 转为未导出的 `rejectedError`，只保留步骤名与响应类型、丢弃服务器文本，连接仍可用；其余错误都来自连接层（imapclient 在读写失败时自行关闭连接并以该错误结束所有待完成命令），因此关闭连接并返回 `ErrClosed`。关闭经 `shutdown`：在独立 goroutine 中调用 `client.Close()`，至多等待 `IdleStop`。会话关闭后，`Scan`、`Examine`、`ListFolders`、`Idle` 都返回 `ErrClosed`，`Close` 什么也不做。期限到期后 `fn` 仍在运行，直到连接关闭使它返回；它只写自己的局部变量，调用方在 `do` 返回错误后不读取这些变量。
- ③ `Dial`：`DialTLS` 的 `tls.Config` 只由 `tlsConfig` 构造，`Dialer` 按 Step 5 设置。`DialTLS` 不接受 ctx，拨号与握手只受 `Timeouts.Dial` 约束，其后每一步都响应 ctx。拨号失败只返回 `imap: dial failed`，不带库的错误（可能含地址或证书细节）。登录前的能力用 `Caps()` 读取：问候带能力列表时直接取用，否则等库在收到问候后自动发出的 CAPABILITY。登录后显式发送 `Capability()`，因为 LOGIN 的响应不带能力列表时，库在后台重新请求，`Caps()` 可能读到登录前的旧值。imapmemserver 只在登录后公告 IDLE，去掉这一步的变异会让 `Idle` 返回 `ErrCapability`，由测试杀死。LOGIN 被回 NO 为 `ErrAuthFailed`。
- ④ EXISTS 通道按 Step 5 实现：处理函数只做非阻塞发送；`Scan` 在 EXAMINE 之前排空通道；`Idle` 先非阻塞检查，有信号即取走并返回 true。
- ⑤ `Scan`：游标的 `LastUID` 已是 `math.MaxUint32` 时直接返回空批，否则 `last+1` 会回绕成非法的 `0:*`。补扫结果先过滤 `uid ≤ last` 再升序排列，超出 `MaxBatch` 时截断并置 `More`。UID FETCH (UID RFC822.SIZE) 与逐封取正文都用 `Fetch` 期限。
- ⑥ 正文合计上限在取下一封正文之前检查：本批已有邮件、且已取回正文加下一封声明的大小超过 `maxBatchBytes` 时，本批结束并置 `More`。这样服务器如实报告大小时合计不超过上限（与「已定的实现细节」中「正文合计至多 16 MiB」一致），恰好等于上限时仍在本批内；每批至少交付一封，单封大于上限时也能前进。声明大小超过上限的邮件不取正文，也不计入合计。
- ⑦ UID SEARCH 返回、但 UID FETCH 没有返回数据或正文的邮件，视为已在两条命令之间被删除：跳过它，游标越过它。若改为停在这封邮件之前、置 `More` 等下一批，被删除的邮件不会再出现在补扫结果中，看似更保守；但服务器持续如此时，Watcher 会因 `More` 立即反复补扫，形成紧循环，所以没有采用。
- ⑧ 正文以 `io.LimitReader` 流式读取至多 `maxMessageSize+1` 字节，超出即 `TooLarge`、`Raw` 为 nil，剩余部分由库在 `Next` 时读出丢弃（受 `Fetch` 期限约束）。实现中发现 imapclient 的一处数据竞争：字面量读取因连接关闭而失败后，库已放行解码协程；若此时再调用 `Next`，库会再次读取同一字面量来丢弃它，与解码协程争用读缓冲，`-race` 报告数据竞争（「字面量传到一半冻结」的用例复现）。因此读取失败时立即返回、不再调用 `Next`。连接已断开，解码协程会自行退出并结束命令，不会因为没人取走数据而阻塞。修正后该用例 `-race -count=10` 稳定。
- ⑨ `Idle` 在 ctx 结束时不发送 DONE，直接关闭连接并返回 ctx 的错误。`Close` 在 `Command` 期限内尽力发送 LOGOUT，总是返回 nil。

实现要点（`watcher.go`）：
- ① 状态种类导出为 `StatusConnected` 等常量，取值即契约注释中的名称。
- ② 每次登录前的等待取「待定等待」与登录频率限制两者中较长者，只发出一个状态。待定等待有四种来源：连接失败后的重连退避（`backoff`）、认证失败后的 `AuthPause`（`auth_failed`）、`Password` 失败后的 `Max`（`credentials_unavailable`）；只受登录频率限制时也发出 `backoff`。登录频率限制是：距上次登录至少 `Initial`；`LoginWindow` 内已有 `MaxLogins` 次时，等到其中最早一次满窗口。登录次数在每次拨号返回后登记（原为拨号前，见下文「审查后修正」③），拨号失败也计入；`Password` 失败不拨号，不计入。登录前失败（拨号、问候、能力、LOGIN 的非 NO 错误）只发出 `backoff`，`disconnected` 只在已登录的连接结束时发出。
- ③ 重连退避在两种情况下复位：每轮补扫完 INBOX 时连接自登录起已存活 `IdleMax`（此时连接刚完成一轮补扫，证明它健康）；或者一次真正发出了 IDLE 的 `Idle` 正常结束（补扫期间已收到 EXISTS、不发 IDLE 就回到补扫的情形不算，见下文「审查后修正」②）。补扫成功本身不复位。半开连接在冻结后没有任何成功的步骤，所以冻结之后的时间不算健康。
- ④ `Idle` 返回 `ErrTimeout` 时计数加一，返回其他结果（成功或其他错误）时计数清零；计数达到 2 即本次 `Run` 改为轮询，并发出一次 `idle_disabled`。
- ⑤ 空批次只在游标变化时交给 `Handle`，例如 UIDVALIDITY 变化后的空文件夹要持久化新游标；游标不变的空批不交付。
- ⑥ `Cursor` 返回错误同样是本地错误，与 `Handle` 失败一样发出 `handle_failed`、在同一连接内按退避重试；契约只写了 `Handle`，这里补上 `Cursor`，已写入 `Watcher` 的注释。同连接重试的退避与重连退避是两串独立的序列，参数相同，处理成功后复位。
- ⑦ ctx 结束时直接关闭连接，不发送 LOGOUT，所以 `Run` 能在 1 秒内返回。`serve` 因其他原因返回时（例如服务器拒绝了 INBOX），连接可能仍然打开，此时经 `Close` 发送 LOGOUT。
- ⑧ 同一连接上重新 LIST 的间隔为包内变量 `relistInterval`（1 小时），测试降低它来覆盖重新 LIST。这是契约没有列出的测试替换点，与 `minAuthPause` 同类。

与契约的偏差：`ListFolders` 的注释写「修改版 UTF-7 原样」，但 imapclient 解析 LIST 响应时一律把修改版 UTF-7 解码为 UTF-8，无法关闭；`Examine` 与 `Scan` 发送文件夹名时再编码回去，所以返回的名称可以原样传回。`INBOX`、`Junk` 这类 ASCII 名称不受影响，L1 记录的是解码后的名称。注释已改为如实描述。

离线假服务器（`fakeserver_test.go`）：imapserver 加 imapmemserver 在回环地址的普通监听器上运行（`InsecureAuth = true`，日志丢弃），前面是 TLS 代理。代理的证书借用 `httptest`，服务端配置去掉 `NextProtos`：imapclient 协商 ALPN `imap`，而 httptest 的 `http/1.1` 会让握手失败。代理逐行解析客户端命令，记录标签、命令名、参数与 UID FETCH 的数据项，LOGIN 只记命令名；明文入口收到的 TLS 握手字节不会被误记为命令。服务器方向按字面量长度原样转发字面量，对响应行改写能力列表（问候、LOGIN 响应码与 CAPABILITY 响应）与 `RFC822.SIZE`。故障表之外补了三项：一是 `faultEmpty`，不转发，直接回 `<标签> OK`，模拟补扫期间邮件被删除；二是 `noJunk` 选项；三是 `maxTLSVersion` 选项。冻结后两个方向的数据都读出丢弃而不转发，这样客户端关闭连接时代理能看到，用例据此断言「连接已被关闭」。每个假服务器在用例结束时断言：记录的命令名都在白名单内（含 DONE），UID FETCH 的数据项只有 `UID`、`RFC822.SIZE` 与 `BODY.PEEK[]<0.N>`。测试用户与邮件经 imapmemserver 的用户接口放入，测试密码为 `strings.Repeat("pw", 8)`，错误密码为 `strings.Repeat("qx", 8)`，都在运行时构造，没有使用 `gitleaks:allow`。

测试（期限为 100–500ms 级别，拨号放宽到 1 秒）：
- `session_test.go` 按 Step 1 覆盖：`TestScanFetchesNewMessagesReadOnly`（收取、游标、只读、正文项为 `BODY.PEEK[]<0.2097153>`）；`TestCapabilities`（无 IDLE、LOGINDISABLED，另补没有 IMAP4rev1）；`TestCommandDeadlines`（问候冻结、LOGIN 后冻结、EXAMINE 与 UID SEARCH 的无标签 BAD、正文字面量传到 512 字节后冻结、IDLE 的无标签 BAD、进入 IDLE 后半开，另补 UID FETCH (RFC822.SIZE) 的无标签 BAD）；`TestExistsDuringScan`、`TestExistsHandlerNeverBlocks`、`TestScanClearsStaleSignal`、`TestIdlePush`、`TestScanUIDValidityChange`、`TestScanBatches`（120、51、50 封）、`TestScanTooLarge`（另补恰为上限的邮件）、`TestScanBatchBytes`（另补单封超过上限时仍然前进）、`TestDialAuthFailed`、`TestDialTransportSecurity`（明文、证书不受信任、主机名为 `localhost`，另补只支持 TLS 1.1 的服务器，并以对照连接确认它确实能协商出 TLS 1.1）、`TestFolders`。
- `session_test.go` 在契约之外补了：`TestContextCancellation`、`TestServerDisconnect`、`TestScanSkipsVanishedMessages`、`TestScanCursorAtMaxUID`、`TestShutdownBoundedWait`（用测试自建的阻塞处理函数让 `client.Close` 挂住，`shutdown` 在 `IdleStop` 后返回）、`TestCloseLogsOut`、`TestTimeoutsDefaults`、`TestErrorTextHasNoServerText`。
- `watcher_test.go` 按 Step 2 覆盖：`TestWatcherDelivers`、`TestWatcherRetriesHandleOnSameConnection`（另补处理成功后退避复位）、`TestWatcherReconnectsWithBackoff`（断开与半开后退避约为 Initial、2×Initial；IDLE 收到 EXISTS 正常结束后复位为 Initial，此时连接存活时间远小于 `IdleMax`，复位只能来自 IDLE 正常结束）、`TestWatcherIdlePhaseFaults`、`TestWatcherDisablesIdleAfterTimeouts`、`TestWatcherRecoversFromCommandBAD`（UID SEARCH，另补 LIST）、`TestWatcherExistsDuringFetch`、`TestWatcherAuthFailure`、`TestBackoffDefaults`、`TestWatcherCredentialsUnavailable`、`TestWatcherJunkFolder`、`TestWatcherCancelDuringIdle`。
- `TestWatcherIdlePhaseFaults` 使用 `Jitter: 0.01`、`Initial` 100ms、`Max` 400ms、`MaxLogins` 4、`LoginWindow` 1.5s：断言前三个登录间隔每次至少增长 1.5 倍，任一登录间隔不低于 `Initial`，任一窗口内的登录不超过 `MaxLogins`。代理记录的是收到 LOGIN 的时刻，比 Watcher 登记的时刻晚一个握手，所以窗口比较留出 100ms 余量。（审查后改为在拨号返回后登记、去掉余量，并补断言到达 Max 后间隔不回落，见下文「审查后修正」③⑦。）
- `watcher_test.go` 在契约之外补了：`TestWatcherResetsBackoffAfterHealthyPeriod`（不支持 IDLE、按 Poll 补扫时存活达到 `IdleMax` 后复位）、`TestWatcherIdleTimeoutsMustBeConsecutive`、`TestWatcherPollsWithoutIdleCapability`、`TestWatcherBacksOffWhenDialFails`（退避约为 1、2、4 倍 Initial，且带抖动）、`TestWatcherHandleWaitSurvivesDisconnect`、`TestWatcherDrainsMoreBatches`、`TestWatcherRelists`（另断言游标不变的空批不交付）、`TestWatcherLoginSpacing`（`Jitter: 0.9` 时退避可能短于 Initial，登录间隔仍不低于 Initial）。
- `source_test.go` 按 Step 3 用 `go/parser` 与 `go/types` 检查，导入器为 `importer.ForCompiler(fset, "source", nil)`，各用例共用一个实例。检查对象经类型信息确定，改名导入与点导入都绕不过（更正：经嵌入字段提升的方法与经接口调用的方法原先能绕过，见下文「审查后修正」⑥）；另外仍把任何点导入报告为违规。`Select` 与 `Fetch` 只能直接调用，方法值会绕过参数检查，因此报告为违规。tls.Config 的字段须恰为 `ServerName`、`RootCAs`、`MinVersion`，与 Task 10 一致。`TestSourceProblemsDetectsViolations` 用内存中的源码逐类确认检查有效，共 24 例（审查后增至 27 例）。

变异测试在仓库副本中进行，每次只改一处，共 64 个，全部被杀死：
- 契约点名的六项：加入 `UnselectAndExpunge`、`Noop` 或 `Move` 调用，`ReadOnly` 改为 false，去掉 `Peek`，以 `imap.SeqSet` 调用 `Fetch`，都被 `TestSourceRestrictions` 杀死。
- 处理函数改为阻塞发送：被 `TestExistsHandlerNeverBlocks` 杀死；只运行注入一条的 `TestExistsDuringScan` 时存活，与契约的说明一致。
- `Scan` 不排空通道：被 `TestScanClearsStaleSignal` 杀死。
- 「补扫成功复位」与去掉登录上限：被 `TestWatcherIdlePhaseFaults` 杀死；上限的 `>=` 改为 `>` 同样被杀死。
- 其余涉及：
  - Idle 不预先检查通道、IDLE 期间忽略 EXISTS、不发 DONE、不检查 IDLE 能力、ctx 结束不返回；
  - 去掉 `uid > last` 过滤、UIDVALIDITY 变化不从 1 补扫、`MaxBatch` 的截断与边界、合计上限的去掉、边界与「每批至少一封」、大小上限与读取上限的边界、部分取回长度改为 N、跳过的邮件不推进游标、去掉 `MaxUint32` 保护；
  - 去掉 LOGINDISABLED 或 IMAP4rev1 检查、不映射 `ErrAuthFailed` 或 `ErrNoFolder`、用登录前的能力、`rejectedError` 带出服务器文本；
  - `do` 去掉计时器、ctx 分支或 ctx 预检、`shutdown` 无限等待、`Close` 不发 LOGOUT、`MinVersion` 降为 TLS 1.0、去掉 `ServerName`（被源码检查杀死）、默认期限；
  - Watcher 的各条复位规则、最小登录间隔、IDLE 降级的次数与「连续」、同连接重试的复位、处理失败改为重连、`folder_missing` 每次都发或初值为缺失、`folder_unavailable` 改为断开、`AuthPause` 下限、重新 LIST、加倍、上限 `Max`、抖动、`Password` 失败计入登录、认证失败改用退避、凭据不可用改为等 `Initial`、空批次的交付条件（两个方向）。
- 其中 `MaxBatch` 边界、合计上限的 `>=`、同连接重试不复位、IDLE 超时计数不清零四个变异，第一轮存活；补了 51 与 50 封的批量用例、恰等于上限的合计用例、处理成功后再失败的用例，以及 `TestWatcherIdleTimeoutsMustBeConsecutive` 后被杀死。
- 另有一个存活变异：去掉读取正文时的 `io.LimitReader`，改为读到字面量结束。imapmemserver 遵守部分取回，字面量本就不超过 N+1 字节，原有用例的结果相同。审查指出这不是等价变异：服务器不遵守部分取回时，它按字面量大小分配内存，用分配量可以稳定地观察到。现已补测试杀死，见下文「审查后修正」⑤。

验证：
- `go test -race -count=3 ./internal/mail/imap/` 通过，覆盖率 98.0%。与 sqlite、security 各包的 `-race -count=3` 并行运行时同样通过；另外 `-race -count=8` 连续运行通过。
- `CGO_ENABLED=0 go test ./internal/mail/imap/` 通过。该命令第一次运行时发现 `TestWatcherResetsBackoffAfterHealthyPeriod` 偶发失败：用例在新连接刚收到 LOGIN、登录尚未完成时就断开，这属于拨号失败，不发出 `disconnected`。改为等 `connected` 状态且新连接上已有补扫后再断开，此后 `-count=20` 稳定。
- `GOOS=windows go vet ./internal/mail/imap/`、全仓 `GOOS=windows go vet ./...` 与 `GOOS=linux go vet ./...` 通过。
- `make modverify`、`make secrets`、`make security`、`make check` 通过（含中文注释检查与 staticcheck，总覆盖率 94.06%）。

测试只在 macOS 上运行，Linux 上的测试由 CI 运行，本机未验证。测试只连接回环地址上的本地假服务器，不连接任何真实服务器，不涉及钥匙串。与前面各任务相同，本任务的提交包含本清单的勾选与实施说明。

**审查后修正：** 变异均在仓库副本中进行，每次只改一处。 复查后补充：`Backoff.withDefaults` 与 `Timeouts.withDefaults` 把零值或负值字段都替换为默认值，`Jitter` 不在 (0, 1) 内时取默认值；原先负的 `MaxLogins` 会让登录频率限制越界 panic，负的期限会让每条命令立即超时，`TestBackoffDefaults` 与 `TestTimeoutsDefaults` 补负值用例。

① 拒绝登录后立即断开。`do` 的 select 同时等待 `fn` 的结果与 `client.Closed()`。服务器回带标签的 NO 后立即关闭连接时，imapclient 先以 NO 结束 LOGIN，随即读到 EOF、关闭 `Closed`；`fn` 所在的 goroutine 还没把结果写入通道时，`Closed` 分支就可能胜出，`Dial` 返回 `ErrClosed` 而不是 `ErrAuthFailed`，Watcher 随之按普通连接失败退避，既不暂停 `AuthPause`，也不发出 `auth_failed`。现在 `Closed` 分支先关闭会话，再至多等待 `IdleStop` 取 `fn` 的结果，按同样的规则分类：带标签的 NO/BAD 仍为 `rejectedError`，其余为 `ErrClosed`；等不到才返回 `ErrClosed`。imapclient 的读协程退出时先以错误结束全部待完成命令，再关闭 `Closed`，所以 `fn` 很快返回，这里的等待只是保险。`fn` 在连接关闭前已经成功时 `do` 返回 nil，会话已标记关闭，下一步返回 `ErrClosed`。假服务器新增故障 `faultRejectClose`：回 `<标签> NO [AUTHENTICATIONFAILED]` 后立即关闭两侧连接。`TestDialAuthFailed` 对它连续 `Dial` 100 次，每次都须返回 `ErrAuthFailed`。修正前在副本中统计，300 次中误判 113 次，`-race` 下 26 次；按后者估计，100 次全部侥幸通过的概率约为万分之一。`TestWatcherAuthFailure` 改为两个子用例，新增的一个使用这一故障，两个都断言不发出 `backoff`；单次运行不一定拦得住，只作辅证。去掉修正的变异在 `-race -count=3` 下 3 次都被 `TestDialAuthFailed` 杀死。

② 补扫期间已收到 EXISTS 时的复位。`Idle` 发现通道中已有信号时不发 IDLE 就返回 `(true, nil)`，`wait` 却把它当作一次 IDLE 正常结束：复位重连退避，并把 IDLE 连续超时的计数清零。契约只允许在「收到 EXISTS，或到达 `IdleMax` 后 DONE 成功」时复位。结果是：每个连接只要在补扫期间收到过 EXISTS，只在 IDLE 阶段出现的故障就会让退避停在 `Initial`，IDLE 连续超时也永远到不了 2 次，不会降级。现在 `wait` 在调用 `Idle` 之前检查 `len(s.exists) > 0`，有信号就直接回到补扫（下一次 `Scan` 排空信号），不复位退避，也不改变计数。信号只在本 goroutine 中被取走，检查之后不会消失。检查与 `Idle` 的预检之间若恰好到达 EXISTS，`Idle` 仍会不发 IDLE 就返回，`wait` 照旧复位。补扫中到达的 EXISTS 在命令完成之前已由解码协程处理，能落进这一窗口的只有命令之外的主动推送，而服务器能主动推送说明连接健康，所以没有为此给 `Idle` 另加返回值。新增 `TestWatcherPendingExistsIsNotIdle`：没有 Junk，每个连接的第一次 UID SEARCH 响应前注入 EXISTS，IDLE 只回无标签 BAD，IDLE 规则的钩子为下一个连接重新布置注入。断言退避约为 `Initial`、2×`Initial`，两次 IDLE 超时后发出 `idle_disabled`，IDLE 恰好发出 2 次。以下三个变异都被杀死：去掉检查，检查后仍复位退避，检查后仍清零计数。

③ 登录时刻的登记。原实现在拨号之前登记登录时刻，而 LOGIN 要在握手、问候与能力读取之后才发出，按生产期限最多晚约 60 秒（Dial 15 秒、Greeting 15 秒、Command 30 秒）。窗口按拨号前的时刻计算，若窗口内第一次登录较慢、第 13 次较快，真实的 13 次 LOGIN 可以落在同一小时内。现在在 `Dial` 返回后登记，成功或失败都登记，这一时刻不早于 LOGIN 发出的时刻。第 k+12 次拨号开始时距第 k 次的登记已满 `LoginWindow`，所以两次 LOGIN 也至少相距 `LoginWindow`；「两次登录至少间隔 `Initial`」同理。审查建议拨号前先登记、返回后再更新为当前时刻；拨号进行中不会计算等待，拨号前那一项用不上，所以只在返回后登记一次。假服务器新增选项 `slowHandshake`，把第一个连接的 TLS 握手推迟指定时长。新增 `TestWatcherLoginWindowUsesLoginTime`：握手推迟 400ms，LOGIN 即断开，`MaxLogins` 为 2、`LoginWindow` 为 600ms，断言代理收到的第 1 与第 3 次 LOGIN 相距不少于窗口。改回拨号前登记的变异中两者只相距约 200ms，被杀死。`TestWatcherIdlePhaseFaults` 的窗口比较去掉 100ms 余量，`TestWatcherLoginSpacing` 的间隔比较去掉 10ms 余量。

④ 期限字段。原 `TestCommandDeadlines` 的期限都在 100–500ms，断言余量是期限加 1 秒，某一步换用别的期限字段仍能通过；IDLE 进行中服务器断开的情形只在 Watcher 用例中出现，同样拦不住。现在每个子用例只把被测步骤的期限设为 300ms，其余都为 5 秒（拨号 1 秒），断言耗时不超过被测期限加 500ms。覆盖的步骤与期限：问候（`Greeting`），LOGIN、EXAMINE 与 UID SEARCH（`Command`），UID FETCH (RFC822.SIZE) 与单封正文（`Fetch`），IDLE 确认（`IdleAck`）。进入 IDLE 后半开的子用例取 `IdleMax` 1 秒、`IdleStop` 100ms，断言耗时不少于 `IdleMax`、不超过两者之和加 500ms。这比契约 Step 1 的「期限加 1 秒」更严；未测的期限设长只为区分字段，不增加用时。`TestServerDisconnect` 另加一段：`IdleMax` 为 5 秒，进入 IDLE 后代理断开，`Idle` 须在 1 秒内返回 `ErrClosed`。以下变异都被杀死：IDLE 确认改用 `IdleMax`，问候改用 `Command`，DONE 改用 `IdleMax`，单封 FETCH 改用 `IdleMax+Fetch`，删去 `Idle` 等待中的 `client.Closed()` 分支，LOGIN 改用 `Greeting`，EXAMINE 或 UID SEARCH 改用 `Fetch`，UID FETCH (RFC822.SIZE) 改用 `Command`。

⑤ 不遵守部分取回时的内存上界。假服务器新增故障 `faultWholeBody`：代理不转发正文 FETCH，自行回一条正文字面量为指定字节数的 `BODY[]` 响应，无视请求中的 `<0.N>`；字面量由代理分块合成，不经 imapmemserver。新增 `TestScanIgnoredPartialBoundsMemory`：`maxMessageSize` 为 1 KiB，字面量 32 MiB，断言结果为 `TooLarge`、`Raw` 为 nil，且 `Scan` 前后 `runtime.MemStats.TotalAlloc` 的增量小于 16 MiB。本机实测修正后的分配量约为 0.2 MB，`-race` 下约 3 MB；去掉 `LimitReader` 的变异分配约 157 MB，被杀死。

⑥ 源码检查的接收者。`isClientMethod` 原先按选择的接收者（`selection.Recv()`）判断：经嵌入字段提升的方法，接收者是外层结构体；经接口值调用时，接收者是接口。两者都绕过了白名单与 `Select`、`Fetch` 的参数检查。现在按方法的声明接收者判断，提升的方法因此也在检查之内。接收者为接口时，若该方法与 `*imapclient.Client` 的某个方法同名同签名，也按 Client 的方法检查，因为 Client 或嵌入它的类型赋给该接口后，经接口调用发出的是同一条命令。检查落在调用处，不必追踪 Client 被赋值或转换为接口的每一处；嵌入本身也不另行报告，因为提升的方法已在检查之内。`TestSourceProblemsDetectsViolations` 增加三例：经嵌入调用 `UnselectAndExpunge`，经嵌入以 nil 选项调用 `Select`，经接口调用 `UnselectAndExpunge`。改回 `selection.Recv()` 的变异被前两例杀死，去掉接口匹配的变异被第三例杀死。反射调用仍在检查之外，本包没有导入 `reflect`。

⑦ 到达 `Max` 后的退避。原 `TestWatcherIdlePhaseFaults` 只断言前三个间隔每次增长 1.5 倍，到达 `Max` 之后只检查不低于 `Initial`，拦不住「到达上限后回到 `Initial`」。现在另断言每个间隔都不低于前一个间隔与 `Max` 中较小者的 (1−`Jitter`) 倍，再减去 50ms 的调度余量。用例等待 7 次登录，得到 6 个间隔，约为 100、200、400、800、400、400ms，其中 800ms 由登录上限造成；等待上限放宽为 2 倍 `waitLimit`，为此把 `waitFor` 拆出 `waitForWithin`。审查给出的变异（2×d 超过 `Max` 时基准归零）使第 5 个间隔降到约 220ms，被杀死。「补扫成功复位」、去掉登录上限、上限的 `>=` 改为 `>` 三个原有变异仍被本用例杀死；另复查了去掉最小登录间隔（被 `TestWatcherLoginSpacing` 杀死）与 IDLE 正常结束后不复位（被 `TestWatcherReconnectsWithBackoff` 杀死）两个原有变异。

⑧ 邮件数不变时的 EXISTS（未改代码）。审查实验表明：服务器若在每条 UID SEARCH 的响应中都附带 `* N EXISTS`（邮件数不变，RFC 3501 允许），每轮补扫后通道中都有信号，Watcher 就不再发 IDLE，而是不限速地连续发 EXAMINE 与 UID SEARCH（1 秒内约 8000 次）。按 Step 5，处理函数对任何 EXISTS 都留下信号，通道是唯一的状态，修改须经维护者确认，本任务不改代码。已列入 L1 规程第 4 步的记录项与「风险与后续」。第二轮审查指出，这一记录项原先没有探测能采到：`TestL1Idle` 只记录 IDLE 期间的推送，其余探测都不记录 EXISTS；若先 `Scan` 再 `Idle`，这种服务器还会让 `Idle` 不发 IDLE 就返回，被误记成一次推送。现已在 Task 13 的 `TestL1Idle` 契约中补上预检（`Examine`、以 UIDNEXT−1 为游标 `Scan` 两次、再 `Examine`、第一次 `Idle` 单独记录返回值与耗时），以及加 `TURNCOURIER_LIVE_IDLE_SELF_SEND=1` 时含 UID FETCH 的同样检查，并同步 L1 第 4 步的备注。Task 13 尚未开工，只改清单；是否采用由维护者在 Task 13 开工前确认。新增 `TestExistsProbeSequence`，按预检顺序对两种假服务器运行：每条 UID SEARCH 响应都注入邮件数不变的 EXISTS 时，两次 `Examine` 的状态相同，`Idle` 不发 IDLE 立即返回 true；不注入时 `Idle` 发出 IDLE，到 `IdleMax` 返回 false；两种情况都不发 UID FETCH。`Examine` 也排空通道的变异使预检看不到这一行为，原有用例都拦不住（`Scan` 本就在 EXAMINE 之前排空，多排空一次不改变结果），只被本用例杀死。 第三轮复查后补充：`TestL1Idle` 的契约改为三个独立会话。预检会话与 FETCH 检查会话都以 10 秒期限调用 `Idle`，其返回单独记录，不会占用整段测量时长，也不会混入推送统计；测量会话的全部返回计入推送统计；自发邮件在测量开始前逐封确认，于测量会话第一次 `Idle` 开始 10 秒时发出，使推送延迟可测；服务器未报告 UIDNEXT 时记为 `uidnext_missing`、结论为「未知」，不以回绕的游标继续，避免静默的假阴性。

修正后的验证：`go test -race -count=3 -cover ./internal/mail/imap/` 通过，覆盖率 97.5%；新增与改动的用例 `-race -count=10` 通过；与 sqlite、security 各包的 `-race -count=3` 并行运行时通过。`CGO_ENABLED=0 go test ./internal/mail/imap/`、全仓 `GOOS=windows go vet ./...` 与 `GOOS=linux go vet ./...`、`make check`（总覆盖率 94.12%）、`make secrets` 均通过。上述变异共 23 个（针对本次修正的 18 个，复查原有的 5 个），全部被杀死。测试仍只在 macOS 上运行，Linux 由 CI 运行。

### Task 12：`init` 命令

**Files:**
- Create: `internal/config/write.go`、`internal/cli/init.go`、`internal/cli/terminal.go`
- Test: `internal/config/write_test.go`、`internal/cli/init_test.go`、`internal/cli/terminal_test.go`
- Modify: `internal/cli/cli.go`、`internal/cli/cli_test.go`、`cmd/turncourier/main.go`、`cmd/turncourier/main_test.go`、`internal/config/config.go`（凭据提示）、`internal/config/config_test.go`、`configs/turncourier.example.toml`（第 2 行注释）、`go.mod`、`go.sum`（`go get golang.org/x/term@v0.46.0`）

**配置写入（`internal/config/write.go`）：**

```go
// Draft 是 init 收集的配置内容；地址须已经 NormalizeAddress 规范化。
type Draft struct {
	MailboxAddress   string
	RecipientAddress string
	AllowedSenders   []string
}

// Render 生成与 configs/turncourier.example.toml 同格式的 TOML 文本：注释相同，主机、端口、事件与有效期写默认值，地址按草稿填写。
// 校验与 Load 相同的地址规则：每个地址规范化后不变、白名单非空且不重复、机器人地址不在白名单中。
// 规范化后的 addr-spec 不含引号与反斜杠，strconv.Quote 的结果即合法的 TOML 基本字符串。
func Render(d Draft) ([]byte, error)

// CreateFile 以 0600 原子创建 path，从不覆盖：父目录缺失时以 0700 创建；先在同一目录写临时文件并 Sync，
// 再用 os.Link 放到 path（已存在时返回包装 fs.ErrExist 的错误），最后删除临时文件。错误文本不含路径。
func CreateFile(path string, data []byte) error
```

示例配置第 2 行改为 `# 邮箱授权码与两把密钥由 turncourier init 写入 macOS Keychain，禁止写在本文件中。`；`config.go` 中凭据类键的提示「凭据将由 init 写入 Keychain」改为「凭据由 turncourier init 写入 Keychain」，相应断言同步修改。

**命令行（`internal/cli`）：**

```go
// Deps 是命令用到的外部依赖；cmd/turncourier 装配生产实现，测试注入替身。
type Deps struct {
	Checker doctor.Checker
	Init    InitDeps
}

// Run 的签名改为以 Deps 代替原来的 doctor.Checker 参数；退出码规则不变（0 成功、1 失败、2 用法错误）。
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, deps Deps) int

// InitDeps 是 init 的外部依赖。
type InitDeps struct {
	Terminal      Terminal
	Keychain      func() (keychain.Store, error) // 生产为 keychain.New(keychain.InteractiveTimeout)；非 macOS 返回 keychain.ErrUnsupported
	Getenv        func(string) string
	UserConfigDir func() (string, error)
	Random        io.Reader        // 实例 ID 与密钥的随机源，生产为 crypto/rand.Reader
	Now           func() time.Time
}

// Terminal 是 init 的交互终端；ctx 结束时恢复终端回显并返回 ctx.Err()。
type Terminal interface {
	Interactive() bool                                         // 标准输入与标准输出都是终端
	ReadLine(ctx context.Context, prompt string) (string, error)   // 回显读入一行，至多 1024 字节，去掉结尾换行
	ReadSecret(ctx context.Context, prompt string) (string, error) // 用 term.ReadPassword 不回显读入一行
}

// NewTerminal 返回基于 x/term 的生产实现；提示写到 out。
func NewTerminal(in, out *os.File) Terminal
```

`main.go` 装配 `Deps{Checker: doctor.New(), Init: InitDeps{Terminal: cli.NewTerminal(os.Stdin, os.Stdout), Keychain: func() (keychain.Store, error) { return keychain.New(keychain.InteractiveTimeout) }, Getenv: os.Getenv, UserConfigDir: os.UserConfigDir, Random: rand.Reader, Now: time.Now}}`。

**帮助与用法文案（测试逐字断言）：**

- `Help.Description`：`pre-alpha：可以用 init 保存配置、授权码与密钥；尚不能收发邮件、执行任务或运行后台服务。`
- `Help.Commands`：`["help [--json]", "version [--json]", "doctor [--json]", "init"]`；`Help.Planned`：`["run", "tasks", "logs", "service"]`。
- 文本帮助的「可用命令」增加一行 `  init       交互式创建配置，并把 QQ 授权码与密钥存入 macOS Keychain`；规划命令一行改为 `规划命令（尚未实现）：run、tasks、logs、service`。
- 规划命令：`%s 尚未实现；当前仅提供 help、version、doctor、init。`，退出码 2。
- 参数错误：`参数错误：help、version、doctor 只接受可选的 --json 参数；init 不接受参数。`，退出码 2。

**init 流程：** 每一步失败都以退出码 1 结束，并打印一行中文原因；错误文本经过的所有底层错误都已不含路径与机密（config、store、keychain 的既有约定），init 自身不拼接路径。

1. **平台：** 调用 `Keychain()`，返回 `keychain.ErrUnsupported` 时打印「init 只支持 macOS：授权码与密钥只能保存在 macOS Keychain。」。此步不启动 `security`。
2. **终端：** `Interactive()` 为 false 时打印「init 需要在交互式终端中运行；请直接在终端执行 turncourier init。」。不读取任何输入，不创建任何文件。
3. **路径：** `config.ResolvePaths(Getenv, UserConfigDir)`。
4. **配置：** 配置文件存在时 `config.Load`；无效时打印「现有配置文件无效，init 不会修改它：」及 Load 的错误，退出。不存在时依次询问「机器人邮箱地址（专用 QQ 邮箱，用于发送通知）：」「接收通知的邮箱地址：」「允许回复的发件人（逗号分隔，直接回车表示只允许接收通知的地址）：」，每项用 `NormalizeAddress` 与 `Render` 的规则校验，无效时打印原因并重问，同一项连续 3 次无效即退出；此时只在内存中生成配置文本。
5. **存储：** `sqlite.Open(ctx, DataDir, Options{Now, Random})`（缺失时以 0700 创建数据目录），`EnsureInstance`。
6. **授权码：** 读取 `AuthCodeAccount(实例 ID)`。已存在时询问「Keychain 中已有授权码，是否替换？[y/N]：」，只有 `y` 或 `Y` 表示替换。这一回答经 `ReadLine` 回显读入；去掉结尾换行后超过 1 个字符时按 N 处理，并提示「输入不是 y 或 N，授权码未替换；如果刚才粘贴的是授权码，请清屏，并考虑在 QQ 邮箱中停用它。」，不回显该输入。需要录入时两次调用 `ReadSecret`（「QQ 邮箱授权码（输入时不显示）：」「再次输入授权码：」），要求两次相同且为 1–128 个不含空白的可打印 ASCII 字符；不符时重问，连续 3 次失败即退出。授权码只保存在局部变量中，不写入任何文件或输出。
7. **写配置：** 新配置用 `config.CreateFile` 创建，随后 `config.Load` 核对；目标已存在（其他进程刚创建）时退出并提示重新运行。
8. **写授权码：** 需要时 `Set(AuthCodeAccount(id), 授权码)`。
9. **密钥：** 对令牌与正文两个用途分别处理，密钥条目从不覆盖：
   - **已有元数据：** `ActiveKeyID` 找到 kid 时读取对应条目。条目缺失时打印「密钥元数据存在，但 Keychain 中缺少对应条目；init 不会重新生成，否则待处理数据将无法解密。」并退出；条目存在但 `DecodeKeyText` 失败，或 `KeyCheck` 与 `KeyCheckOf` 不符时，打印「Keychain 中的密钥与数据库登记的不符；init 不会覆盖它。」并退出；相符即视为已存在。
   - **没有元数据：** 条目已存在且 `DecodeKeyText` 成功时沿用它（上次在登记前中断，或另一个 init 刚写入，该条目从未被使用）；已存在但格式不符时打印「Keychain 中已有格式不符的同名条目；请按 development.md 手工删除后重新运行。」并退出；不存在时 `NewKeyText(Random)` → `Add(…-key-1, 文本)`，返回 `ErrExists`（并发的另一个 init 先写入）时改为读取并沿用已有条目。随后 `RegisterKey(用途, 1, KeyCheck(用途, 1, 密钥))`；返回 `ErrKeyExists` 时读取 `KeyCheckOf`，与本次的校验值相同即视为成功（并发的 init 登记了同一把密钥），否则按不符退出。
10. **摘要：** 逐行打印配置文件「已创建」或「已存在」及其位置的相对描述（「用户配置目录下的 TurnCourier/turncourier.toml」或「TURNCOURIER_CONFIG 指定的文件」）、数据目录的相对描述、实例 ID（不是机密）、授权码「已保存」「已替换」或「保持不变」、两把密钥「已生成」或「已存在」。随后打印两段固定说明：
   - 「注意：授权码与密钥保存在登录钥匙串中，同一用户下的任何进程（包括 Agent 执行的命令）都能读取；读到它们的进程可以伪造通过全部校验的回复、把任意内容注入任一任务，也可以直接改写本地队列。TurnCourier 不防同一用户下的进程。请只使用专用的机器人邮箱；怀疑泄露时，请在 QQ 邮箱中停用该授权码。」
   - 「待投递的回复与通知会加密暂存在数据目录中，处理完成后删除；APFS 快照与 Time Machine 备份中可能留有已删除数据的密文副本，正文密钥仍在 Keychain 中时它们可以被解密。不要把数据目录恢复到旧的备份，否则已发出的通知可能重发、已确认的回复可能再次派发。当前版本尚不能收发邮件。」
11. **取消：** 任何一次读入时 ctx 结束（Ctrl-C），打印「已取消。」并退出；此时第 7 步之后的写入都未发生，数据库中可能已有实例行，重新运行 init 会继续使用它。

init 不联网，不验证授权码能否登录（L1 第 1 步验证）。按设计，init 还应包含环境诊断与真实往返测试：环境诊断（复用 `doctor` 的检查）与真实往返测试都在 4b 接入，后者依赖 4b 冻结的线程规则。4a 期间用户可以单独运行 `turncourier doctor`。

- [ ] **Step 1：写失败的测试（`write_test.go`）。**
  - `Render(Draft{"bot@example.invalid", "me@example.invalid", ["me@example.invalid"]})` 的结果与 `configs/turncourier.example.toml` 逐字节相同；
  - 对 20 组合法草稿（含多个白名单地址、含 `+` 与 `'` 的本地部分），把 `Render` 的结果写入临时文件后 `Load` 成功，字段与草稿一致；
  - 地址未规范化（含大写）、白名单为空或重复、机器人地址在白名单中时报错；
  - `CreateFile`：新建文件权限 0600、父目录 0700、内容一致、目录中没有残留的临时文件；目标已存在时返回 `fs.ErrExist` 且原文件内容与修改时间不变；父路径是普通文件时报错；所有错误文本不含临时目录路径。
- [ ] **Step 2：写失败的测试（`init_test.go`）。** 替身：按脚本返回回答的 `Terminal`（记录每次调用的提示，`ReadSecret` 与 `ReadLine` 分开记录）；以 map 实现、分别记录 `Set` 与 `Add` 次数并可注入错误的 `keychain.Store`；`Getenv` 把 `TURNCOURIER_CONFIG` 与 `TURNCOURIER_DATA_DIR` 指向 `t.TempDir()` 下尚不存在的子目录；固定随机源与时钟。授权码金丝雀在运行时构造为 `strings.Repeat("canary", 3)`，遵循「测试向量与密钥扫描」的约定。
  - **首次运行：** 回答机器人地址、接收地址、空白名单、两次授权码 → 退出码 0；配置文件内容等于 `Render` 的结果、权限 0600；数据库中有实例行与 `(token, 1)`、`(payload, 1)` 两条 active 元数据；Keychain 替身恰有 `<id>:qq-auth-code`（等于金丝雀）、`<id>:token-key-1`、`<id>:payload-key-1`（43 个字符）三个条目；标准输出含摘要与两段固定说明；标准输出与标准错误都不含金丝雀、两把密钥文本与临时目录路径；授权码只经 `ReadSecret` 读入。
  - **重复运行：** 不询问地址；授权码询问是否替换，回答空行 → 不再读入授权码；`Set` 与 `Add` 调用次数都为 0；配置文件字节与修改时间不变；摘要显示「已存在」「保持不变」。回答 `y` 时重新读入并只对授权码调用一次 `Set`。以授权码金丝雀作答时按 N 处理、打印上述提示，输出不含该回答。
  - **输入校验：** 两次授权码不一致后第三次一致 → 成功；连续 3 次不一致 → 退出码 1，Keychain 没有写入，配置文件不存在。地址输入 `Me <me@example.invalid>` 时重问；白名单含机器人地址时重问；同一项连续 3 次无效 → 退出码 1，配置文件不存在。
  - **环境：** `Interactive()` 为 false → 退出码 1，配置目录与数据目录都不存在，`Keychain()` 返回的替身没有被调用任何方法；`Keychain()` 返回 `ErrUnsupported` → 退出码 1，没有读入任何输入。
  - **已有数据：** 配置含未知键 → 退出码 1，文件未变，输出含键名、不含路径；数据目录已存在且权限为 0755 → 退出码 1，Keychain 未写入。
  - **中断后的状态：** Keychain 中已有 `<id>:token-key-1` 但没有元数据（模拟上次在登记前中断）→ 沿用该条目并以它的校验值登记，条目不变，没有调用 `Add` 或 `Set`，退出码 0；有元数据但 Keychain 缺少条目 → 退出码 1，打印上述提示，不生成新密钥、不改动其他数据。
  - **密钥被替换：** 首次运行后把 `<id>:payload-key-1` 换成另一段格式合法的 43 字符文本，再运行 → 退出码 1，打印「与数据库登记的不符」，条目与元数据都不变。
  - **并发写入：** Keychain 替身在 `Add(<id>:token-key-1)` 时先放入另一段合法密钥再返回 `ErrExists` → init 沿用该密钥并以它的校验值登记，退出码 0；存储中已预先登记了另一个校验值时 → 退出码 1。
  - **Keychain 错误：** `Add` 或 `Set` 返回含 `KEYCHAIN-CANARY` 的错误 → 退出码 1，对应密钥没有登记元数据（重新运行会重新生成），输出不含机密。
  - **取消：** `ReadSecret` 时取消 ctx → 输出「已取消。」、退出码 1，Keychain 没有写入，配置文件不存在。
- [ ] **Step 3：写失败的测试（`cli_test.go`、`terminal_test.go`、`main_test.go`）。**
  - 帮助、用法与规划命令的文案与上文逐字相同；`TestHelp` 的 4 个入口输出含新描述且不含 `Phase`；`init --json`、`init extra` 返回 2 与参数错误文案；`run` 返回 2 与新的规划命令文案。
  - `NewTerminal` 以 `os.Pipe` 构造时 `Interactive()` 为 false；`ReadLine` 从管道读出一行并去掉 `\n`，超过 1024 字节时报错；`ReadSecret` 在非终端上返回错误。成功读入机密的路径需要伪终端，自动测试不覆盖，由 L1 中维护者运行 init 验证（写入实施说明）。
  - `main_test.go` 以 `init` 调用 `run`：先用 `t.Setenv` 把 `TURNCOURIER_CONFIG` 与 `TURNCOURIER_DATA_DIR` 指向临时目录；标准输入是终端时（直接在终端里运行编译出的测试二进制）`t.Skip`，以免真实提问并写入登录钥匙串。`go test` 以 `/dev/null` 作为测试进程的标准输入，所以 `Interactive()` 为 false（不带包参数运行 `go test` 时标准输出可能是终端，这不影响判断）。按 `runtime.GOOS` 断言：darwin 上返回 1 与「需要在交互式终端中运行」，其他平台返回 1 与「只支持 macOS」；两种情况都不启动 `security`，临时目录中没有新建文件。
- [ ] **Step 4：** `go get golang.org/x/term@v0.46.0`；测试失败。
- [ ] **Step 5：实现。** 按上文；`ReadSecret` 先 `term.GetState` 保存状态，在 goroutine 中调用 `term.ReadPassword`，ctx 结束时 `term.Restore` 并返回；`ReadLine` 逐字节读取，不使用带缓冲的读取器，以免吞掉随后 `ReadPassword` 要读的输入。
- [ ] **Step 6：** `go test -race ./internal/cli/ ./internal/config/ ./cmd/...` 通过；`make check` 与 `make secrets` 通过；`make build` 后 `./dist/turncourier help`、`help --json`、`init --json`（返回 2）行为符合上文；`go version -m dist/turncourier` 列出 `BurntSushi/toml`、`modernc.org/sqlite`、`golang.org/x/term`，不含 `emersion`。
- [ ] **Step 7：** `git commit -m "feat(cli): add interactive init storing credentials in the Keychain"`

**实施说明：** 待实施后填写。

### Task 13：L1 探测工具

**Files:**
- Create: `tests/live/sample.go`、`tests/live/sample_test.go`（两者不带构建标签）、`tests/live/live_test.go`、`tests/live/probe_test.go`、`tests/live/keychain_test.go`（三者带 live 标签）
- Modify: `Makefile`（`vet` 与 `lint` 增加 live 构建标签）、`.gitignore`（增加 `state.json` 与 `samples.jsonl`，兜底防止探测输出进入 Git）、`go.mod`、`go.sum`（`go get github.com/emersion/go-message@v0.18.2 golang.org/x/text@v0.42.0`：go-message 由间接依赖变为直接依赖，x/text 显式固定）

**两类文件：**

- **不带标签的纯函数（`sample.go`，包 `live`）：** 样本脱敏归类、主题前缀白名单、Message-ID 按相等关系归类、输出目录校验。它们只处理传入的字节与值，不读取配置、Keychain、网络或环境变量。`sample_test.go` 用合成数据测试它们，随 `make test` 在 CI 中运行，计入覆盖率与中文注释检查。
- **带标签的探测（包 `live_test`）：** 三个文件都以 `//go:build live` 开头，不带标签时 `go test ./...`、`make test` 与 CI 都不编译它们。

**探测不会在 CI 或普通 `go test` 中运行的保证：**

1. 带标签时，`TestMain` 先检查：`TURNCOURIER_LIVE` 必须恰为 `1`，否则打印「跳过：未设置 TURNCOURIER_LIVE=1」并以 0 退出，不运行任何测试；环境变量 `CI` 非空时打印「拒绝在 CI 中运行」并以 1 退出。
2. 通过上述检查后、访问 Keychain 或网络之前，`TestMain` 经 `/dev/tty` 做一次总确认：列出机器人账户、接收地址、输出目录以及本次 `-run` 选中的探测项，要求输入 `yes`，否则打印「已取消」并以 0 退出。`TURNCOURIER_LIVE=1` 残留在 shell 环境中时，误运行的命令会停在这里，而不会静默登录邮箱。
3. 每封邮件发出前、以及每次写入真实钥匙串前，再经 `/dev/tty` 显示将要做的事并要求输入 `yes`；打不开 `/dev/tty`（没有控制终端）即失败。回答其他内容时跳过该项并记录为 `skipped`。
4. `make vet` 与 `make lint` 增加 `go vet -tags live ./tests/live/` 与 `staticcheck -tags live ./tests/live/`，CI 只编译检查这些代码，从不运行。
5. 下文与 L1 规程中的全部探测命令都带 `-count=1`，避免 `go test` 直接显示缓存的结果。

**输入：** 使用与产品相同的配置与数据目录解析（`TURNCOURIER_CONFIG`、`TURNCOURIER_DATA_DIR` 或默认位置）。实例 ID：先确认数据库文件存在（不存在时提示先运行 `turncourier init`），再以 `file:…?mode=ro` 只读打开，确认 `user_version` 不低于 2 后直接查询 `instance` 表；不调用 `sqlite.Open`，因为它会创建数据目录与数据库、执行迁移与检查点。授权码来自 Keychain（`keychain.New(keychain.InteractiveTimeout)`）。探测工具不写入任何产品表，自己的状态放在输出目录。

**输出：** `TURNCOURIER_LIVE_OUT` 必须是已存在的绝对路径目录，权限恰为 0700，且不在本仓库工作树之内。仓库根目录是从测试工作目录向上找到的 `go.mod` 所在目录。校验由 `sample.go` 的纯函数按以下顺序完成，返回解析后的真实路径：

1. 拒绝相对路径；
2. 用 `filepath.EvalSymlinks` 解析输出路径中的全部符号链接，得到真实路径；
3. 对真实路径 `os.Stat`：必须是目录，权限位恰为 0700；
4. 从真实路径开始用 `filepath.Dir` 逐级向上，直到文件系统根，每一级 `os.Stat` 后与仓库根目录的 `os.Stat` 结果做 `os.SameFile` 比较（按设备号与 inode，不受大小写影响），任一级相同即拒绝。

必须先解析符号链接：只按字面路径逐级向上时，位于仓库外、指向仓库内子目录的符号链接，它本身与它的各级字面上级都不是仓库根目录，会通过校验。此后所有文件都写在解析后的真实路径下，以 0600 创建：

- `state.json`：本次探测发出的每封邮件的我方 Message-ID、探测 ID、合成任务 ID、一次性令牌文本，以及 QQ 为这封合成邮件分配的 ID：「已发送」副本的 Message-ID、抄送副本的 Message-ID、DATA 响应中的候选 ID 列表 `delivered_data`（定义见下文「DATA 响应中的 ID」）。这些 ID 不含个人信息；令牌由测试进程内随机生成、用后即弃的密钥签发，不能用于任何真实验证。
- `samples.jsonl`：JSON Lines 样本，只记录结构特征，结构如下（每行一个对象，示例值均为合成）：

```json
{"schema":"turncourier-l1/2","kind":"reply","client":"QQMail 2.x","from_role":"recipient","from_case_variant":false,
 "thread":{"in_reply_to":"delivered_sent+delivered_data","references":["ours","delivered_sent+delivered_data"]},
 "subject":{"prefix":"回复：","tag_intact":true,"encoding":"gb18030/B"},
 "mime":[{"type":"multipart/alternative","children":[
   {"type":"text/plain","charset":"gb18030","transfer":"base64","size":"1-4KiB"},
   {"type":"text/html","charset":"gb18030","transfer":"base64","size":"4-16KiB"}]}],
 "plain":{"tokens":1,"token_matches_sent":true,"token_line_prefix":"","separators":["qq_a_zh"],
          "skeleton":["text","blank","sep","header","header","header","blank","quote_text"]},
 "html":{"tokens":1,"blockquote":false,"markers":["qq_div_style"]},
 "auto":{"auto_submitted":"","x_autoreply":false,"precedence":"","return_path_empty":false},
 "auth_results":{"present":true,"dkim":"pass","spf":"pass","dmarc":"pass"}}
```

规则：

- **地址：** 一律换成角色（`bot`、`recipient`、`allowed`、`other`），角色按规范化后的地址判断；另记原始地址与规范化结果是否只差大小写（`from_case_variant`，用于复核 Phase 3 的小写化假设）。
- **Message-ID 按相等关系归类：** 与 `state.json` 中记录的 ID 逐一比较，得到 `ours`、`delivered_sent`（等于「已发送」副本的 ID）、`delivered_cc`（等于抄送副本的 ID）、`delivered_data`（等于 `delivered_data` 列表中的任一元素）；同时等于多个来源时按此顺序以 `+` 连接。都不相等时，形如 `tencent_…@qq.com` 的记为 `other_tencent`，其余记为 `other`；缺失记为 `missing`。这样 L1 能直接回答 D4 的问题：回复的线程头究竟等于哪一个来源的 ID。
- **主题：** 找不到 `[TC` 时 `prefix` 为 `null`、`tag_intact` 为 false，不输出主题中的任何文字。找到时，`[TC` 之前的文字去掉空白后若只由已知回复前缀组成（白名单：`回复：`、`回复:`、`答复：`、`答复:`、`转发：`、`转发:`、`Re:`、`RE:`、`re:`、`Fwd:`、`FW:`、`Fw:`，可重复），输出该文字，至多 20 个字符；否则输出 `"other"`。
- **不输出：** 正文、显示名、日期、完整主题、`Received` 头（只计数）。
- **客户端：** `client` 取 `X-Mailer` 或 `User-Agent`，截断到 40 个字符。
- **正文结构：** 分隔线与引用头按探测工具内置的正则表归类，只输出类别名（QQ A/B/C 格式、Foxmail 头块、Apple Mail/iOS/Gmail 的「写道」与 `wrote:` 行、`>` 引用），不输出原文；无法归类的行只输出行类别（`text`、`blank`、`quote_text`、`sep`、`header`）。
- **字符集：** gbk、gb2312、cp936、x-gbk、windows-936 在探测进程内映射到 GB18030 解码器后再查找令牌（4b 的解析器另行实现并测试这一映射）。
- **DATA 响应中的 ID：** 候选 ID 是 250 响应文本中带尖括号、形如 `<左部@右部>` 的子串（左右两部都不含空白、`<`、`>`、`@`），以及不带尖括号、形如 `tencent_…@qq.com` 的子串（QQ 分配的 ID 可能不带尖括号出现），后者在比较与记录时补上尖括号。`state.json` 中的 `delivered_data` 是字符串数组，按出现顺序排列，同一 ID 只记一次；没有候选时为空数组。
- **DATA 响应文本：** 每个候选按相等关系替换：与 `ours`、`delivered_sent`、`delivered_cc` 比较，相等时换成对应的 `<来源>`，同时等于多个来源时按此顺序以 `+` 连接（例如 `<ours+delivered_cc>`）；都不相等时，形如 `tencent_…@qq.com` 的换成 `<other_tencent>`，其余换成 `<other>`。这里不与 `delivered_data` 比较：每个候选本来就是 `delivered_data` 的元素，比较结果恒为真，不携带信息；邮件头中的 ID 仍照常与它比较。文本中其余形似地址的部分（不带尖括号、也不是 `tencent_…@qq.com` 形式的 `…@…`）换成 `<addr>`。

样本在加入仓库之前，由维护者逐条审阅并按 4b 的要求转为合成回归样本。

**探测项（各为独立测试，用 `-run` 选择）：**

| 测试 | 做什么 | 发信 |
| --- | --- | --- |
| `TestL1KeychainRoundTrip` | 在真实钥匙串中写入、读回、删除一个合成条目（account `live-test-<16 位随机>`，值由测试随机生成），确认 `Delete` 后 `Get` 返回 `ErrNotFound`；另对已存在的合成条目调用 `Add`，确认返回 `ErrExists`、原值不变，并记录 `security` 此时的退出码。设置 `TURNCOURIER_LIVE_KEEP=1` 时，写入后在 `/dev/tty` 显示合成 account，以及「合成值加一个换行符」的 SHA-256，等待维护者按回车再删除。显示的哈希与 `security find-generic-password … -w \| shasum -a 256` 口径相同：`-w` 的输出是值加一个结尾换行（`Get` 依赖这一格式，读回成功即说明真实的 `security` 如此输出），管道哈希的正是这些字节。条目保留期间，可在另一终端做 L1 第 5 项的沙箱读取检查 | 否 |
| `TestL1Capabilities` | 登录 IMAP，记录登录后的能力、文件夹列表，以及 INBOX、Junk、Sent Messages 的 UIDVALIDITY（各文件夹是否相同只作记录：`0002` 已把文件夹纳入 `inbound_messages` 的 UID 唯一键，相同也不会撞键） | 否 |
| `TestL1SendNotification` | 发出 `TURNCOURIER_LIVE_SEND_COUNT`（默认 1，至多 5）封合成通知给配置中的接收地址：`multipart/alternative`，`Auto-Submitted: auto-generated`，主题 `[TC <合成任务 ID>] TurnCourier L1 探测 k/n`，正文页脚含一次性令牌与「这是 TurnCourier 的真机探测邮件，请用不同客户端直接回复并保留引用」。之后在 60 秒内以只读方式查找「Sent Messages」中的副本（按探测 ID 头 `X-TurnCourier-Probe` 对应），记录是否存在、其 Message-ID 与 `X-OQ-MSGID` 是否等于我方 ID。设置 `TURNCOURIER_LIVE_CC_BOT=1` 时同时抄送机器人自己，并在 INBOX 中查找抄送副本做同样记录。「已发送」副本、抄送副本的 Message-ID 与 DATA 响应中的候选 ID（`delivered_data` 列表）写入 `state.json`，DATA 的 250 响应按上述规则脱敏后写入样本 | 是，逐封确认 |
| `TestL1Replies` | 读取 `state.json`，以只读方式补扫 INBOX 与 Junk（游标存于输出目录），对引用了探测邮件的来信（线程头含 `state.json` 中记录的任一 ID、主题含合成任务 ID，或正文含探测令牌）输出上述样本；自动回复与退信同样输出，`kind` 为 `auto` 或 `bounce` | 否 |
| `TestL1Idle` | 分三个独立会话进行，每个会话只做一件事，互不影响统计。**预检会话**：`Examine(INBOX)` 记下邮件数 N0 与 UIDNEXT U0；U0 为 0（服务器未报告 UIDNEXT）时记录 `uidnext_missing`，预检结论记为「未知」并跳过其余预检步骤，不以回绕的游标继续；否则以返回的 UIDVALIDITY 与 U0−1 构造游标（不取回已有邮件），连续 `Scan(INBOX)` 两次，再 `Examine(INBOX)` 记下 N1、U1，随后以 10 秒期限的 ctx 调用一次 `Idle`，记录返回值与耗时，会话随即关闭。判定：10 秒内返回 true、耗时远小于 IDLE 与 DONE 两次往返，且 N0 = N1、U0 = U1，记为「UID SEARCH 的响应附带 EXISTS」；ctx 到期（说明已真正发出 IDLE）记为「未附带」；其余情况记为「未知」并保留原始值。EXAMINE 响应中的 EXISTS 归入命令结果，不留下信号。**测量会话**：新建连接，`Examine(INBOX)` 后连续调用 `Idle`，时长 `TURNCOURIER_LIVE_IDLE_MINUTES`（默认 30，至多 60），`IdleMax` 放宽到该时长；各次 `Idle` 之间不调用 `Scan`，本会话的每次返回都计入推送统计，记录每次 EXISTS 推送的时刻与服务器断开连接的时刻。设置 `TURNCOURIER_LIVE_IDLE_SELF_SEND=1` 时，自发邮件在开始测量之前逐封确认，确认后于测量会话第一次 `Idle` 开始 10 秒时自动发给机器人自己，记录从 250 响应到该次 `Idle` 返回 true 的延迟。**FETCH 检查会话**（仅在设置了 `TURNCOURIER_LIVE_IDLE_SELF_SEND=1`、测量结束后进行）：新建连接，`Examine(INBOX)`，以预检的游标 `Scan(INBOX)` 一次（取回这封自发邮件，不输出其内容），再 `Examine(INBOX)`，随后以 10 秒期限调用 `Idle`，按预检的规则判定并单独记录；与预检结果对照即可分辨 EXISTS 是随 UID SEARCH 还是随 UID FETCH 附带。预检未能进行时本检查照常执行，结论相应记为「未知或随 UID FETCH」。预检与 FETCH 检查中的 `Idle` 返回只单独记录，不计入推送统计 | 可选，逐封确认 |

- [ ] **Step 1：** `go get github.com/emersion/go-message@v0.18.2 golang.org/x/text@v0.42.0`；修改 Makefile 与 `.gitignore`：

```make
vet:
	$(GO) vet ./...
	$(GO) vet -tags live ./tests/live/

lint: $(STATICCHECK)
	goroot="$$($(GO) env GOROOT)" && PATH="$$goroot/bin:$$PATH" $(STATICCHECK) ./... && PATH="$$goroot/bin:$$PATH" $(STATICCHECK) -tags live ./tests/live/
```

- [ ] **Step 2：写失败的离线测试（`sample_test.go`，不带标签）。** 用合成的 `.invalid` 邮件字节（含 GB18030 base64 正文、带令牌的 QQ A 格式引用、`Auto-Submitted: auto-replied` 自动回复）调用样本函数，断言：
  - 输出中不含合成正文里的金丝雀句子、显示名、地址与日期；令牌识别正确；自动回复 `kind` 为 `auto`；
  - `In-Reply-To` 等于 `state.json` 中「已发送」副本 ID 的回复归类为 `delivered_sent`；同时等于 DATA 响应 ID 时为 `delivered_sent+delivered_data`；形如 `tencent_…@qq.com` 但不等于任何记录的 ID 为 `other_tencent`；
  - 主题 `回复：[TC …] …` 的前缀为 `回复：`；主题 `关于合同 13800138000 [TC …]` 的前缀为 `"other"`；主题被改写为 `关于合同 13800138000`（没有标签）时 `prefix` 为 `null`、`tag_intact` 为 false；三种情况的输出都不含 `13800138000`；
  - DATA 响应文本依次含我方 ID、抄送 ID、一个不带尖括号且不等于任何记录的 `tencent_…@qq.com` 形式的 ID、一个普通地址，并把我方 ID 重复一次：前三者依次换成 `<ours>`、`<delivered_cc>`、`<other_tencent>`，普通地址换成 `<addr>`；`delivered_data` 恰为按出现顺序的三个元素（第三个补上尖括号，重复的我方 ID 只记一次）；`In-Reply-To` 等于其中第三个元素的回复归类为 `delivered_data`；
  - 输出目录校验：仓库根目录内的子目录、以大小写变体书写的同一目录（在不区分大小写的文件系统上它存在，校验按 inode 拒绝；在区分大小写的文件系统上它不存在，同样被拒绝）、位于仓库外但指向仓库内子目录的符号链接、权限 0755 的目录、相对路径与不存在的目录都被拒绝；仓库外权限 0700 的目录通过，指向它的符号链接也通过，且返回解析后的真实路径。去掉「先解析符号链接」的变异使符号链接用例失败。
- [ ] **Step 3：写探测工具。** 按上表与输出规则实现 `sample.go` 与三个带标签的文件；写中文包注释。带标签的文件只做 I/O 与确认，脱敏与归类全部调用 `sample.go` 的纯函数。
- [ ] **Step 4：验证开关。**
  - `go test ./...`、`make check` 与 `make secrets` 通过；`tests/live` 只运行 `sample_test.go` 的离线测试，不读取配置、Keychain 或网络；`sample_test.go` 中的合成令牌在运行时构造（例如用 `bytes.Repeat` 构造的密钥与按下标填充的 nid 签发后取 `Reveal()`），测试向量遵循「测试向量与密钥扫描」的约定；
  - `env -u TURNCOURIER_LIVE go test -count=1 -tags live ./tests/live/` 输出「跳过」并通过，没有读取配置、Keychain 或网络（在临时 HOME 下运行，确认没有新建任何文件）；
  - `CI=1 TURNCOURIER_LIVE=1 go test -count=1 -tags live ./tests/live/` 失败并输出「拒绝在 CI 中运行」；
  - `grep -rn 'tags live\|TURNCOURIER_LIVE' .github/` 无结果。
- [ ] **Step 5：** `git add .gitignore Makefile go.mod go.sum tests/live && git commit -m "test(live): add manual L1 probes guarded by build tag and explicit consent"`

**实施说明：** 待实施后填写。

### Task 14：文档同步

**Files:**
- Modify: `docs/zh-CN/design.md`、`SECURITY.md`、`docs/en/architecture.md`、`docs/zh-CN/development.md`、`README.md`、`README.zh-CN.md`、`CHANGELOG.md`、`AGENTS.md`、`CONTRIBUTING.md`

- [ ] **Step 1：`design.md`。**
  - 状态行：Phase 4 已拆分为 4a、L1、4b，4a 的内部包与 `init` 已实现，尚不能收发邮件；init 的环境诊断（复用 `doctor`）与真实往返测试在 4b 接入。
  - 「安全与持久化细化」写入 D3 的威胁模型：经 `/usr/bin/security` 创建的条目，受信任应用是 `/usr/bin/security`、分区为 `apple-tool:`，同一用户的任何进程（包括 Agent 执行的 shell 命令，只要其沙箱放行）都能静默读出授权码与两把密钥；读出之后，它可以用令牌密钥签发有效令牌，伪造通过发件人、线程、主题标签与令牌全部校验的回复，把任意内容注入任一任务，也可以直接改写本地队列，TurnCourier 不防同一用户下的进程；Keychain 在这里防的是明文进入配置、仓库、日志与备份，防其他系统用户，防没有登录密码的离线磁盘访问；不在进程内调用 Security.framework 的原因（Go 二进制没有稳定签名，每次升级都会重新弹窗）；缓解措施（专用机器人邮箱、可随时停用授权码）；发布阶段再评估 Developer ID 签名加进程内调用（优先 purego，不引入 CGO）。
  - 同一节写明：正文摘要为 HMAC-SHA256（令牌签名密钥、独立前缀、对解析后的新正文计算），列名 `body_sha256` 因已发布而保留；待处理正文以 AES-256-GCM 加密、关联数据绑定用途、密钥号、任务与序号，进入终态的同一事务中删除，数据库启用 `secure_delete` 并在删除后尽力执行 TRUNCATE 检查点；APFS 快照与 Time Machine 中的残留不在 SQLite 能控制的范围内，正文密钥仍在 Keychain 中时这些残留的密文可以被解密；把数据目录恢复到旧备份会让已发出的通知重发、已确认的回复再次派发。
  - 目标目录树：`security/` 下增加 `payload/`（待处理正文加密）；`mail/smtp/` 的注释由「发信与重试」改为「发信（重试由调用方按待发通知状态机处理）」。
  - 开发顺序一段的「当前处于」改为邮件闭环阶段（4a 已完成离线部分，等待 L1）。
  - 不改动 D4 仍待 L1 定稿的线程规则表述（包括「稳定 Message-ID」一句），它们在 4b 修订。
- [ ] **Step 2：`SECURITY.md`（中英文同步）。**
  - 报告范围加入 `internal/security/keychain`、`token`、`payload`、`internal/mail/smtp`、`imap`、`internal/queue` 的待发通知状态机、迁移 `0002` 与 `turncourier init`，举例：机密进入进程参数、环境变量、日志、错误文本或数据库明文；接受被篡改或过期的令牌；接受被调换的密文；删除后正文仍可从数据库或 WAL 文件中恢复；在未加密的连接上发送凭据；IMAP 修改机器人邮箱；init 覆盖已有配置或密钥；`tests/live` 在未设置显式开关时联网或访问 Keychain。
  - 不属于范围：同一用户的进程读取 Keychain 条目，以及由此伪造回复或改写本地队列（已记录的威胁模型）；APFS 快照与 Time Machine 中的残留。
  - 「规划设计中的边界」一节写入威胁模型摘要，并把尚未实现的部分改为：邮件收发循环、入站验证、Agent 适配器与后台服务。「发件人白名单、线程引用、短任务 ID 和签名令牌必须全部一致，否则拒绝」一句（中英文）补充限定：这一校验防的是其他人冒充，不防能读取本机 Keychain 条目的同一用户进程。
- [ ] **Step 3：`docs/en/architecture.md`。** 状态行；Implemented 表增加 Keychain、令牌、正文加密、待发通知状态机与存储、SMTP、IMAP、`init`、`tests/live`；Not implemented 更新；包与依赖方向图按本清单「4a 文件结构」一节；包职责表逐包补充；命令解析规则与退出码表加入 `init`（0、1、2 的含义）；「Persistence and recovery」补充 `0002` 的各表、`secure_delete` 与检查点、正文生命周期、键控摘要、实例 ID 与 Keychain 条目布局、待发通知的状态与恢复（`RecoverSendingNotifications` 与 `RecoverInFlight` 同样只能由唯一的进程在开始工作之前调用）；Dependencies 表加入 D2 的模块、版本、许可证与使用方，并写明产品二进制目前链接的模块；Quality gates 写明 `vet` 与 `lint` 也检查 live 构建标签下的代码。
- [ ] **Step 4：`docs/zh-CN/development.md`。** 状态行；第三方依赖表；新增「Keychain 条目」一节：service 与 account 的命名，查看条目用 `security find-generic-password -s io.github.chaorookie.turncourier -a <account>`（不加 `-w`，不打印机密），数据目录删除后遗留条目的手工删除方法，「有元数据但缺少条目」与「条目与登记的校验值不符」时 init 为何拒绝重新生成或覆盖，密钥条目只以不覆盖的方式创建；测试约定补充假 `security` 子进程（`TURNCOURIER_TEST_SECURITY_*`）、go-smtp 与 imapmemserver 假服务器、借用 `httptest` 证书、故障代理；新增「真机探测（tests/live）」一节：构建标签与 `TURNCOURIER_LIVE=1` 双重开关、`CI` 下拒绝运行、访问 Keychain 与网络前的总确认、逐封确认、全部命令带 `-count=1`、输出目录必须在仓库之外且权限为 0700、不带标签的样本函数与其离线测试随 `make test` 运行、各探测项与环境变量、样本入库前须人工审阅；make 目标表的 `vet`、`lint` 行写明包含 live 标签；「配置文件与数据目录」改为 init 已能创建配置，数据库保存待处理正文的密文，并写明不要把数据目录恢复到旧版本：从不自动重发与防重复派发都以数据库状态连续为前提，必须恢复时先人工核对 PENDING 通知与 QUEUED 回复；「隐私与凭据」改为授权码已由 init 写入 Keychain。
- [ ] **Step 5：README（中英文同步）。** 状态与当前能力（`init` 可用，尚不能收发邮件）；实际文件树按 `git ls-files --cached --others --exclude-standard | sort` 核对，加入本阶段新建的全部文件，`phase-04.md` 的注释改为「Phase 4：邮件闭环（4a 已实现，待 L1）」与对应英文；代码规则中的依赖说明加入 D2 的模块；「安全与私密」一段写入威胁模型摘要。
- [ ] **Step 6：** `CHANGELOG.md` 在 `[Unreleased]` 增加 Phase 4a 条目（Added、Changed、Security 分开写）；`AGENTS.md` 当前范围改为「已实现配置、存储、状态机、Keychain、令牌、正文加密、SMTP/IMAP 客户端与 init；尚未接入收发循环与 Agent」；`CONTRIBUTING.md` 中英文的当前阶段清单改为 `phase-04.md`。
- [ ] **Step 7：检查。** `git diff --check` 通过；在全部文档中检索本机路径、用户名与订阅档位无结果；README 两份文件树一致且覆盖 `git ls-files` 的全部文件；Phase 3 Task 11 Step 6 的 `check` 链断言仍无输出；检索「已支持邮件收发」「can send」一类表述无结果（不把规划能力写成已实现）。
- [ ] **Step 8：** `git commit -m "docs: document phase 4a mail foundations and the keychain threat model"`

**实施说明：** 待实施后填写。

### Task 15：整阶段验证、审查与合并

- [ ] **Step 1：本地门槛。**

```sh
export PATH="$PWD/.local/toolchains/go/bin:$PATH"
make check
make security
make workflows
make build
GOOS=linux go vet ./...
GOOS=windows go vet ./...
GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./cmd/turncourier
CGO_ENABLED=0 go test -count=1 ./...
go test -race -count=3 ./internal/store/sqlite/ ./internal/mail/... ./tests/...
env -u TURNCOURIER_LIVE go test -count=1 -tags live ./tests/live/
go version -m dist/turncourier
ls -l dist/turncourier
./dist/turncourier help
./dist/turncourier help --json
./dist/turncourier version
./dist/turncourier doctor --json
./dist/turncourier init --json
git diff --check
```

  期望：`make check` 通过且总覆盖率 ≥ 80%；gitleaks 与 govulncheck 无发现；actionlint 通过；darwin/amd64 以 `CGO_ENABLED=0` 交叉编译成功（产品二进制从 4a 起首次链接 modernc、toml 与 x/term，Phase 3 不跑候选构建的理由不再成立）；带 live 标签的命令输出「跳过」；`go version -m` 列出 `BurntSushi/toml`、`modernc.org/sqlite` 及其依赖、`golang.org/x/term`、`golang.org/x/sys v0.48.0`，不含 `emersion` 与 `golang.org/x/text`（4b 之前产品不链接邮件依赖），记录二进制大小；`init --json` 返回 2。
- [ ] **Step 2：审查。** 按 Phase 3 Task 12 的做法，每个维度一名审查员在临时副本中取证，一名反方验证员逐条尝试推翻；确认的问题修复后以变异测试证明测试能拦截回归，逐条写入本任务的「审查后修正」。维度：
  1. **机密：** 授权码、两把密钥与令牌不进入进程参数、环境变量、日志、错误文本、数据库明文或 Git；金丝雀测试覆盖所有新包；威胁模型在 design.md、SECURITY.md、init 输出中一致。
  2. **密码学：** 令牌字节布局、MAC 输入、解析与验证顺序；正文密文格式与关联数据；随机 nonce；键控摘要的域分离；`hmac.Equal`。
  3. **存储与状态机：** `0002` 的约束与触发器；待发通知从不自动重发；正文在终态的同一事务中删除，检查点后磁盘上无残留；Phase 3 的不变量未被放宽：按 Message-ID 去重不含文件夹，同一封信在两个文件夹中的副本仍判为重复；未送达重新排队仍核对派发后的事件，`RecoverInFlight` 与 `RecoverSendingNotifications` 的单一进程前提写在注释与文档中；`0001_init.sql` 与 Phase 3 相比没有任何改动（`git diff main -- internal/store/sqlite/migrations/0001_init.sql` 为空）。
  4. **邮件客户端：** 只有隐式 TLS、明文防护、逐步期限、无标签 BAD、半开连接、UIDVALIDITY 变化、只读访问、命令白名单、调试输出关闭。
  5. **init 与命令行：** 不覆盖已有数据、非交互与非 macOS 的行为、中断后可重复执行、错误文本不含路径与机密、帮助文案。
  6. **L1 工具：** 双重开关、CI 拒绝运行、逐封确认、输出目录限制、样本脱敏。
  7. **测试有效性：** 对每个新包抽取关键判定做变异，确认都被测试拦截。至少包括：关联数据少一个字段；触发器改为保留 REJECTED 正文；`secure_delete` 改为 `FAST`（由 64 KiB 残留用例拦截）；`truncateWAL` 不临时关闭忙等待；关闭任务后 `RequeueNotification` 仍改回 PENDING；领取不放弃即将过期的通知；IMAP 命令包装不设期限、单封 FETCH 不设期限、问候不设期限；Watcher 在补扫成功后复位退避；IMAP 的 EXISTS 处理函数改为阻塞发送（由连续注入两条 EXISTS 的用例拦截）；`Scan` 开始时不排空 EXISTS 通道（由「不残留旧信号」用例拦截）；`RecordReply` 按 UID 查找时去掉文件夹，或按 Message-ID 查找时加上文件夹（由 Task 6 的两个跨文件夹用例拦截）；`0002` 重建的两张表漏写一条 CHECK（由 Task 6 升级路径的建表文本比较拦截）；SMTP 改用 `DialStartTLS`；去掉写正文计时器；去掉 `CloseWithResponse` 阶段的看门狗。标签比较由 `hmac.Equal` 改为 `==` 或 `bytes.Equal` 时功能完全相同，任何功能测试都拦不住，这一项由 Task 3 的源码检查拦截，审查记录如实注明。
- [ ] **Step 3：提交 PR。** 推送 `feat/phase-04a-mail` 并开 PR 前，须由维护者在当次对话中确认（对外发布动作的既有规则）。等待 `quality (ubuntu-24.04)`、`quality (macos-15)`、`security` 三项检查通过，记录 CI 耗时。经维护者确认后，在该分支上手动触发 Candidate build（`gh workflow run build.yml --ref feat/phase-04a-mail`），确认 arm64 runner 以 `CGO_ENABLED=0` 交叉编译 amd64 成功，结果写入验证记录。
- [ ] **Step 4：合并与记录。** 按维护者选择的方式合并；在本文件末尾追加「4a 验证记录」（本地、审查、远端分开记录，未运行的项写明原因）；更新 `HANDOFF.md`：基线提交、`go version -m` 的新期望（链接配置、存储与 x/term，不链接邮件依赖）、下一步为 L1 真机探测、「待维护者确认」各项的状态。

**实施说明：** 待实施后填写。

## 完成标准（4a）

- 令牌、正文加密与两个状态机的全部分支都有测试；每一类篡改、调换与越界都被拒绝，错误与日志中没有令牌、密钥或授权码。
- Keychain 封装的机密只经标准输入与标准输出传递；非 macOS 平台返回不支持，没有明文后备。
- 迁移 `0002` 可在已有 `0001` 数据的库上执行，重建后入站记录与回复的数据、关联与序列不变；不同文件夹中 UID 相同的邮件不再撞键，同一封信在 INBOX 与 Junk 的两份副本仍按 Message-ID 判为重复；待处理正文只以密文落盘，进入终态即在同一事务中删除，检查点后数据库与 WAL 文件中都找不到密文；待发通知在结果不确定时进入 UNCERTAIN，从不自动重发。
- SMTP 与 IMAP 只用隐式 TLS，每一步都有期限，离线假服务器覆盖无标签 BAD、半开连接与 UIDVALIDITY 变化；IMAP 不修改机器人邮箱。
- `turncourier init` 可重复执行、不覆盖已有数据，错误文本不含路径与机密。
- `tests/live` 不会在 CI 或普通 `go test` 中运行；维护者可以用它完成 L1。
- `CGO_ENABLED=0` 下全部测试通过；`make check`、`make security`、`make workflows`、`make build` 与远端三项检查通过；文档与实际文件树一致。

## L1 真机探测规程（草案）

维护者在本机执行，助手可以代为运行命令，但授权码只由维护者在本机终端输入。探测前逐项确认将要发出的邮件。

1. 登录后记录 CAPABILITY；确认 IDLE、MOVE、UIDPLUS、ID 是否仍在；记录机器人账户的文件夹列表。
2. 发 1 封合成通知给维护者邮箱，记录：
   - 收件端看到的 Message-ID，以及 `X-OQ-MSGID`；
   - DATA 的 250 响应文本；
   - 「已发送」中是否有副本、能否检索到。
3. 维护者用以下客户端各回复一次：QQ 邮箱网页版、QQ 邮箱 iOS/Android App、Foxmail，以及平时实际使用的 Apple Mail 或 Gmail。逐一记录：
   - 线程头；
   - 主题前缀；
   - 引用格式；
   - 纯文本与 HTML 的结构；
   - 字符集与传输编码；
   - 令牌是否留在引用中。
4. 记录 IDLE 推送延迟，以及服务器多久断开连接；另记录邮件数不变时，UID SEARCH 与 UID FETCH 的响应中是否仍带 `* N EXISTS`（见「风险与后续」中关于连续补扫的一条）。
5. 可选项，须维护者另行授权：
   - 假期自动回复；
   - 退信样本；
   - 别名地址回信；
   - Codex 与 Claude Code 的沙箱能否读取合成 Keychain 条目。

### 与 4a 产出的衔接

L1 在 4a 合并之后进行，全部命令在仓库根目录执行。`TURNCOURIER_LIVE_OUT` 指向仓库之外、维护者自己的已存在目录，权限必须恰为 0700（新建时用 `mkdir -m 700 <目录>`，已有目录用 `chmod 700 <目录>`），否则探测工具拒绝运行；路径中的符号链接先解析再校验，见 Task 13「输出」：

| 规程步骤 | 命令 | 备注 |
| --- | --- | --- |
| 准备 | `turncourier init` | 授权码由维护者在本机终端不回显输入；同时验证 `ReadSecret` 的终端路径（Task 12 未自动覆盖） |
| 1 | `TURNCOURIER_LIVE=1 TURNCOURIER_LIVE_OUT=<目录> go test -count=1 -tags live -run TestL1Capabilities -v ./tests/live/` | 另记录 INBOX、Junk、Sent Messages 的 UIDVALIDITY（只作记录，见 Task 6 的文件夹列）；下列各步同样带 `-count=1` |
| 2 | 同上，`-run TestL1SendNotification`；可加 `TURNCOURIER_LIVE_CC_BOT=1` | 覆盖 D4 实际投递 ID 的三个候选来源：DATA 响应、「已发送」副本、抄送给机器人自己的副本，三者的 ID 都写入 `state.json`，第 3 步的样本按相等关系判断回复的线程头等于哪一个；发送成功同时说明 QQ 接受 EHLO 名 `localhost` |
| 3 | 维护者用各客户端回复后运行 `-run TestL1Replies` | 同时用别名地址与大小写变体回信，`from_case_variant` 复核 Phase 3 的小写化假设 |
| 4 | `-run TestL1Idle`；可加 `TURNCOURIER_LIVE_IDLE_SELF_SEND=1` | 结果用于确认或调整 Task 11 的 `IdleMax`、退避与 `Poll` 数值。邮件数不变时 UID SEARCH 的响应是否带 EXISTS 由预检会话记录，UID FETCH 的检查需加 `TURNCOURIER_LIVE_IDLE_SELF_SEND=1`；两项检查各在独立会话中以 10 秒期限调用 `Idle`，结果单独记录；测量会话的全部返回计入推送统计 |
| 5 | `-run TestL1KeychainRoundTrip`，加 `TURNCOURIER_LIVE_KEEP=1`；自动回复与退信样本由 `TestL1Replies` 归类 | 须维护者另行授权。沙箱读取检查只对合成条目执行，从不针对真实 account：条目保留期间，在 Codex 与 Claude Code 的沙箱中分别执行 `security find-generic-password -s io.github.chaorookie.turncourier -a <合成 account>`（只读属性）与 `security find-generic-password -s io.github.chaorookie.turncourier -a <合成 account> -w \| shasum -a 256`（读取数据，只输出哈希），把哈希与 `/dev/tty` 上显示的值比对：两边都是「值加结尾换行」的 SHA-256，只比较十六进制摘要，忽略 `shasum` 输出末尾的 ` -`。不加 `-w` 时 `security` 只读取属性、不读取机密数据，无法回答 D3 的问题。结果按「能读出数据」「只能读属性」「被拒」三类记录 |

L1 的结论（能力、实际投递 ID 的来源、各客户端的线程头与引用格式、IDLE 数值、沙箱读取结果）写入本清单新增的「L1 结果」一节；`samples.jsonl` 不进仓库，经维护者审阅后按 4b 的要求转为合成回归样本。

## 4b 与 4a 的衔接

4b 的范围见上文「范围」一节，L1 之后细化为任务契约。4a 为它留下的接口与前置条件：

- **单实例锁（Phase 3 已定的前置条件）：** 收发循环所在的进程必须先取得单实例锁，锁覆盖 `RecoverInFlight`、`RecoverSendingNotifications`、全部派发与发送。
- **装配：** 新建 `internal/app`，把 `imap.Watcher` 的 `Handle` 接到解析器、入站验证与 `RecordReply`/`RecordRejection`（`InboundReply.Folder` 与 `Rejection.Folder` 取自 `Batch.Folder`），一批全部提交后再 `AdvanceCursor`；把发送循环接到 `ClaimNextNotification` → 渲染 → `token.Issue`（Claims 取自通知行）→ `smtp.Send` → 按 Task 10 的映射更新状态。进程启动时从 Keychain 读取授权码与两把密钥，失败即不启动收发与派发；每把密钥读出后用 `keychain.KeyCheck` 与 `KeyCheckOf` 比对，不符即以「Keychain 中的密钥与登记不符」拒绝启动。授权码读出后缓存在进程内存中，`imap.Watcher.Password` 与发送循环都使用缓存值，只在 IMAP 或 SMTP 认证失败后、或用户在本地操作后重新读取 Keychain，避免钥匙串锁定或 ACL 变化时每次重连都弹窗。Keychain 读取超时或返回 `ErrInteractionNotAllowed` 时报告为独立的 `keychain_unavailable` 状态，不计入连接失败。`imap.Watcher` 的 `Password` 回调重新读取 Keychain 失败时返回错误，Watcher 随之发出 `credentials_unavailable`；装配层把它与启动时、发送循环中的 Keychain 读取失败一并报告为 `keychain_unavailable`。两者是同一情况在两层的名称：`internal/mail/imap` 不知道凭据来自 Keychain，所以用通用名称。
- **认证熔断：** SMTP 返回 `ErrAuth` 或 IMAP 返回 `ErrAuthFailed` 时，打开一个两端共享的熔断，持续时间不少于 `AuthPause`（15 分钟，下限 10 分钟）；熔断期间 IMAP 不登录，领取到的通知直接 `RequeueNotification` 到熔断结束之后，不尝试 SMTP AUTH。已确认的「认证失败后至少暂停 10 分钟」由此同时约束发送端。4b 为此补测试。
- **SMTP 结果的分流顺序：** 认证 5xx 的错误链同时含 `ErrNotSent`、`ErrAuth` 与 `*ReplyError`，所以 4b 必须先判断 `ErrAuth`（打开认证熔断、通知重新排队），再按 `ReplyError` 的 4xx/5xx 分流；先判断通用的 5xx 规则会把认证失败误当成邮件被拒而放弃通知。参数校验失败同样返回不附 `ReplyError` 的 `ErrNotSent`，与暂时性失败无法区分，因此 4b 在调用 `Send` 之前自行校验信封与邮件大小（例如渲染后超过 `MaxMessageSize`），不合法的通知直接放弃，不交给 `Send` 后再按「暂时性失败」无限重新排队。问候与 EHLO 被拒绝时不附 `ReplyError`（连接级失败，不代表这封邮件被拒），同样按暂时性失败处理。
- **令牌验证（4b 的初步建议，L1 后定稿）：** 以下一条无论 L1 结果如何都成立：令牌的 kid 必须等于通知行的 `TokenKeyID`，不等即拒绝，否则持有旧密钥的一方可以为新密钥下发的通知签出可用令牌。其余顺序为初步建议：`token.Parse` → `NotificationByNID` → kid 与 `TokenKeyID` 相等 → `KeyStateOf(token, kid)` → 用同一 kid 的 `BodyDigest` 计算摘要 → 按 (账户, Message-ID) 查找已有记录，Message-ID、摘要与任务都一致即判为重复并忽略（不写被拒记录）→ `token.Verify` → 任务状态 → `RecordReply`。去重排在有效期与任务状态之前，是为了让 UIDVALIDITY 变化后的全量补扫把早已处理的回复识别为重复，而不是误报为 `token_expired` 或 `task_closed`；为此 4b 需新增按 Message-ID 查找入站记录的存储接口，并补「过期后重扫」的集成测试。retired 密钥签发的令牌是否仍接受、有效期按本地处理时间还是 IMAP INTERNALDATE 判断，列在「风险与后续」，L1 后在 4b 定稿。线程头、主题标签与多令牌规则按 L1 结果冻结，任何放宽按 D4 另行确认。
- **实际投递 ID：** 按 L1 选定的来源写入 `RecordDeliveredMessageID`，并新增按实际投递 ID 查找通知的存储接口。
- **通知内容：** `NewNotification.Content` 的格式、确定性敏感信息过滤与代码块裁剪由 4b 的渲染器定义；频率、合并与退避参数在 4b 定。
- **解析器：** gbk、gb2312、cp936 等字符集标签映射到 GB18030 在 `internal/mail/parser` 中集中管理并测试。
- **init：** 加入设计要求的环境诊断（复用 `doctor` 的检查）与真实往返测试。
- **文档：** design.md 中「使用稳定 Message-ID」等依赖 QQ 是否改写 Message-ID 的表述，按 L1 结果修订。

## 风险与后续

- go-imap/v2 与 go-message 都未到 1.0，升级可能带来破坏性变更，所以固定版本，由封装层隔离。
- QQ 的发信频率阈值不公开，所以要节流、合并通知；遇到 550/450 类错误时退避，并标记状态。
- 同一用户的进程可以读取 Keychain 中的机密（D3），发布阶段需重新评估签名与进程内调用。
- 若 L1 表明 QQ 不在「已发送」保存 SMTP 发出的邮件，且 DATA 响应中也没有实际 ID，线程绑定只能依靠抄送给机器人自己的副本，或依靠令牌加主题标签。后者属于放宽，须按 D4 另行确认。
- 地址统一转小写基于 QQ 邮箱不区分大小写的假设（Phase 3 风险），在 L1 用别名与大小写变体复核。
- Keychain 中的密钥被替换时，由 `crypto_keys.key_check` 在 init 与 4b 启动时发现并拒绝继续，而不是等到解密或验证失败。正文无法解密（数据损坏，或绕过启动检查替换了密钥）的回复会停在该任务队首、阻塞它的派发；这样的通知每次领取都会失败，因领取按 `(not_before, id)` 排序，会阻塞全部发送。4a 只保证不领取、不改动数据并返回明确的错误，处置靠 `AbandonNotification(manual)` 或人工核对；4b 须在本地告警中提示这一情况。
- 删除数据目录后，Keychain 中该实例的条目成为孤儿；元数据存在而条目缺失时 init 拒绝重新生成。4a 没有修复、轮换或卸载命令，手工处理方法写在 development.md。
- `Token` 作为其他包结构体的**未导出**字段时，`fmt` 会打印其原始字节；4b 以装配后的日志金丝雀测试覆盖实际路径。
- go-smtp 的 `DialTLS` 拨号期限固定为 30 秒且不响应 ctx，进程退出时最多多等 30 秒；已确认的决策只允许 `DialTLS`，不为此改用其他拨号方式。
- IMAP 的期限、IDLE 重建间隔与退避数值、2 MiB 正文上限都是推断，以 L1 的实测结果调整，调整写入本清单。
- 服务器若在邮件数不变时也在 UID SEARCH 或 UID FETCH 的响应中附带 `* N EXISTS`（RFC 3501 允许），按 Task 11 Step 5，每轮补扫后都会留下信号，Watcher 不再发 IDLE，而是不限速地连续补扫。4a 按已确认的契约保留现状，L1 第 4 步记录 QQ 是否如此。若是，可以只在邮件数超过 EXAMINE 时的数目时留下信号，或给两轮补扫之间设最小间隔；这会改动 Step 5「通道是唯一状态」的约定，须经维护者确认。
- init 不回显读入授权码的终端路径需要伪终端，没有自动测试，由 L1 准备步骤中维护者实际运行 init 验证。
- 键控摘要对解析器输出计算，依赖解析器的确定性。4b 或以后修改引用、签名的剥离规则时，同一封信重新取回（例如 UIDVALIDITY 变化后的全量补扫）会得到不同摘要，从「重复」变成 `ErrMessageConflict` 误报。修改解析规则时须评估这一点，必要时让补扫中的冲突只作告警，或给摘要加版本（在 4b 定）。
- 令牌验证中有两个问题留给 4b 在 L1 之后定稿：retired 密钥签发的令牌是否仍接受；有效期按本地处理时间还是 IMAP INTERNALDATE 判断。按本地处理时间，服务停机或 Keychain 不可用期间到达、过期之后才处理的合法回复会被拒绝；INTERNALDATE 由服务器写入，不是发件人提供的 Date，但它是否可信、QQ 如何设置要由 L1 确认。去重排在有效期与任务状态检查之前的初步建议见「4b 与 4a 的衔接」。
- 待发通知在令牌将于 10 分钟内过期时被放弃（`expired`），SMTP 中断时间超过令牌有效期时，这段时间的通知不会发出；4b 须在本地提示这一情况。
- 「从不自动重发」与防重复派发都以数据库状态连续为前提。把数据目录恢复到旧备份后，备份中仍为 PENDING 的通知（实际早已发出）会再发一次，已确认的回复会重新成为 QUEUED 并再次派发。development.md 写明不要这样做，必须恢复时先人工核对 PENDING 通知与 QUEUED 回复。
- 产品二进制从 4a 起链接配置、存储与 x/term；4b 接入收发后按 D2 的估计约为 16.5 MB（arm64）。发布前补齐链接模块的第三方许可声明。

## 待维护者确认

目前无待确认事项。

### 已确认事项

**1. `inbound_messages` 的 UID 唯一键加入文件夹（2026-09-19 确认，采用方案 (a)）。**

- **问题：** `0001` 以 `UNIQUE (account, uid_validity, uid)` 标识一封已收取的邮件，没有文件夹。4a 要同时扫描 INBOX 与 Junk，而 UID 只在同一文件夹内唯一（RFC 3501：文件夹名、UIDVALIDITY 与 UID 三者一起才唯一确定一封邮件）。两个文件夹的 UIDVALIDITY 恰好相同时，Junk 中的合法回复会与 INBOX 中 UID 相同的另一封信撞键，被 `RecordReply` 判为 `ErrMessageConflict` 而拒绝。新表 `fetch_cursors` 与 `inbound_rejections` 已按 (账户, 文件夹, …) 设计，不受影响。
- **决定：** 方案 (a)。`0002` 开头同时重建 `inbound_messages` 与 `replies`，给前者增加 `folder` 列，UID 唯一键改为 `(account, folder, uid_validity, uid)`，保留 `UNIQUE (account, message_id)`；`InboundReply` 增加 `Folder`，`RecordReply` 按含文件夹的键查找。按 Message-ID 的去重不变，Phase 3 的防重复投递没有放宽；`0001_init.sql` 不改。
- **理由：** 在 4a 就消除撞键，不依赖 L1 对 UIDVALIDITY 的观测结果，UIDVALIDITY 日后重置也不会再撞键。代价是改动 Phase 3 已发布的表结构（通过新迁移），迁移多出两张表的重建与相应测试。
- **未采用：** 方案 (b) 不改表，待 L1 确认各文件夹的 UIDVALIDITY 不同即可，若相同再由 4b 写 `0003`，风险留到 4b，UIDVALIDITY 重置后仍可能碰撞；把文件夹拼进 `account` 字符串传入不可取，同一封信在两个文件夹中的副本将不再按 Message-ID 去重，会重复入队。
- **落地：** 重建步骤、补测与对 Phase 3 代码和测试的连带修改都在 Task 6 内完成（folder 列 NOT NULL 且没有默认值，`0002` 一落地，旧的插入语句就会失败）；Task 8 只处理正文密文与键控摘要。
