# TurnCourier Phase 4 实施清单：邮件闭环

> **执行说明：** 本阶段分为 4a（离线实现）、L1（维护者本机真机探测）与 4b（依据 L1 结果实现）。本清单给出 4a 的完整任务契约；4b 的任务在 L1 结果写入本清单后细化。4a 按任务顺序执行：每个任务先写失败的测试，再做最小实现，通过后提交。每个任务由独立子代理实现，再由规格与质量两名审查子代理复核（含变异测试），确认的问题修复并经独立复查后才进入下一任务；阶段末做整阶段审查（Task 15）。实现在 `feat/phase-04a-mail` 分支进行。步骤使用 `- [ ]` 勾选跟踪。
>
> **状态：** 第 0 节 D1–D5 已于 2026-09-19 确认；4a 任务契约已通过两轮审查，独立复查留下的问题已修正。`inbound_messages` 增加文件夹列一事已于同日确认采用方案 (a)，并入 Task 6（记录见末尾「待维护者确认」一节的「已确认事项」）。4a 已实现并合并（PR #12），L1 已完成（PR #13、#14）。2026-09-24：探测工具按「L1b」一节修改，L1b 的真机部分待维护者执行；「4b 任务契约」已起草，其中 D6–D8 已于同日由维护者确认（见末尾「已确认事项」第 2 条）。

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

- **会读到：** 同一用户的任何进程都能静默读出 QQ 授权码与两把密钥，包括 Agent 执行的 shell 命令。**L1 第 5 步已实测证实**：Claude Code 与 Codex 的 shell 都能读出合成条目的机密数据，无弹窗、无拒绝（见「L1 结果」第 5 步）。
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

### D4：线程绑定与令牌载体（2026-09-20 按 L1 第 1–3 步的结果定稿）

`design.md` 要求：线程引用、短任务 ID 与签名令牌必须一致，缺失或冲突即拒绝。D1 的证据表明，「线程引用」指向的很可能是 QQ 分配的 ID，而非我方 ID。L1 证实了这一点，也推翻了原定的令牌载体；证据见下文「L1 结果」。

**定稿规则：**

- **签名令牌：** 放在主题标签中，`[TC <10 位任务 ID> <48 位令牌>]`。**主题标签是唯一的验证输入。** 通知正文页脚仍各放一份（纯文本与 HTML），但那只是给人看的副本，验证时永不回读。
  - 理由：页脚属于我方发出的那封信，在回复里一定落在引用区，而解析器按设计只保留剥离引用后的新正文；若改为读原始正文，到第 N 轮时引用区里累积着第 1…N−1 轮的旧令牌，任何「两处必须相等」的规则都会把每一封合法的多轮回复判为冲突。
  - 因此：正文中有没有令牌、与主题是否一致，都不产生任何拒绝原因码。
- **令牌格式：** HMAC-SHA256 截断到 128 位，编码为 48 个 Crockford base32 字符，与任务 ID 的字母表相同。令牌内含随机通知 ID（nid）；任务、owner 与有效期从数据库中该 nid 对应的通知行取得，一并参与签名。
- **短任务 ID：** 与令牌同在主题标签内，位置在前。
- **主题标签的文法（锚定匹配，不做容错修复）：** 对解码后的完整主题，`[TC ` + 10 个任务 ID 字符 + 恰好一个 U+0020 + 48 个令牌字符 + `]`，字符集同为 Crockford base32 的字母表。
  - 整个主题中该文法必须**恰好命中一次**。命中 0 次拒绝；命中 ≥2 次一律拒绝，不做「取第一个」「取最后一个」「取第一个验得过的」——后者会让攻击者并排堆多个标签，挑仍然开着的任务生效，而用户转发旧通知后在同一线程回复也会自然产生两个标签。
  - **不得**为兼容主题折行而修复标签内部的空白。令牌被折行或截断时按独立原因码拒绝，与「令牌无效」区分开，否则维护者事后分不清是传输弄坏了主题还是有人在伪造。
- **线程引用：** `In-Reply-To` 或 `References` 中任一处包含该通知**实际投递的** Message-ID，即算匹配。实际 ID 取自「已发送」副本，按 `X-OQ-MSGID` 对应回我方 ID；另外两个候选来源经 L1 证明不可用。
  - **定位「已发送」副本只能按我方 Message-ID 或我方写入的自定义头，禁止按主题检索。** 主题里现在有令牌，`UID SEARCH HEADER SUBJECT` 会把它送进 IMAP 命令流与服务端命令日志。探测工具已给出正确做法（按自定义探测头对应），正式通知要保留一个等价的自定义头。
- **接受条件：** 先判定不是自动回复、不是退信（见下一条），再要求发件人规范化后在白名单内、线程引用匹配、主题标签中的任务 ID 与令牌都通过校验，五者齐备才接受。任一缺失或冲突即拒绝，只在本地提示，不自动回信（防回环与反向散射）。
- **自动回复与退信的过滤，现在是承重规则，不再只是纵深防御。** 令牌在正文页脚时，白名单地址的假期自动回复因正文里没有页脚令牌而被拒；令牌改到主题后，自动回复会把带令牌的主题原样带回（L1 已证明主题标签被原样保留），于是发件人、线程引用、任务 ID、令牌四项**全部通过**，那段自动回复文本会作为用户的下一条消息进入 Agent 会话。因此：
  - 过滤必须排在验证顺序的**最前面**，先于 `token.Parse`，判定命中即硬拒并只记元数据；
  - 判定规则沿用「已定的实现细节」中的特征表。该表现在包含一条主题前缀规则：L1 实测的 QQ 假期自动回复**不带任何头部信号**，只有主题前缀可用，维护者已确认为此破例；
  - 该表仍是启发式的，不可能穷举所有服务商与语言。因此 4b 必须另有一道**与分类无关的回环刹车**：同一任务每小时至多接受 N 次邮件触发的回合，机器人每小时至多发出 M 封通知，超出即本地告警并暂停该任务的邮件触发。分类漏判时它保证环转不起来，N 与 M 的取值在 4b 定。
  - 另须在文档与 `init` 的提示中写明：**接收通知的邮箱不要开启假期自动回复**。这是最直接的缓解，但无法远程检测，只能告知。
  - 退信样本仍未采集，是 4b 的前置条件。
- **放宽须另行确认：** 任何放宽仍须维护者另行确认，并先修改 `design.md`。

**令牌脱敏的边界（改动后新增的约束）。** 现有的脱敏是**基于类型**的：`token.Token` 的 `String`/`Format`/`LogValue` 会脱敏，`Reveal()` 返回裸 `string`。令牌一旦被拼进主题字符串就失去全部保护，而主题恰是最容易被打进日志和错误文本的字段。目前仓库里没有泄漏点（`internal/` 与 `cmd/` 的非测试代码中没有 logger），也就是说所有泄漏点都将由 4b 亲手制造。4b 须遵守：

- 由 `internal/security/token` 导出主题标签的构造函数，返回一个同样实现 `Format`/`LogValue` 的类型，使标签字符串不以裸 `string` 形式存在于包外；`Reveal` 的契约注释「只用于写入通知正文」同时更新，否则代码审查会把主题里的 `Reveal()` 当成越界调用。
- 组装好的主题不进任何结构体字段与长生命周期变量，装配完 MIME 即弃。
- 入站解析的拒绝原因文本禁止回显主题，哪怕只回显前 N 个字符。
- 调用 `Send` 之前的信封与大小自检直接拿着含令牌头部的原始字节，其错误文本不得包含消息内容。
- CLI 的本地「被拒来信 / 待发通知」列表永不打印主题。`inbound_rejections` 不存主题、`smtp.Send` 的错误文本不含邮件内容，这两处既有约束保持不变。
- 日志金丝雀测试从「`Token` 作为未导出字段」扩到**组装后的主题字符串**与**出站 MIME 字节**。
- 探测工具逐封确认时会把完整主题回显到 `/dev/tty`；复核新格式前改成只回显标签之外的部分。

**令牌的有效使用范围（待维护者确认，见本节末）。** 令牌在有效期内可以被无限次重放：去重按 (账户, 文件夹, UIDVALIDITY, UID) 与 (账户, Message-ID) 加正文摘要，同一个令牌换一封信就不算重复。载体改到主题后，取得令牌的门槛从「打开并读完邮件正文」降到「看一眼锁屏或邮件列表」。缓解方向与默认有效期见「待维护者确认」。

**推翻的原方向。** 原定「令牌放正文页脚、明确不放进主题」。L1 的两份回复样本证明：QQ 邮箱网页版的回复可以完全不带引用原文，正文页脚的令牌随之丢失；而主题标签在网页版与 App 两个客户端都原样保留。按这条原规则，一封合法的网页版回复会被直接拒掉。2026-09-20 维护者确认改用主题载体。代价是令牌会出现在锁屏预览、通知与邮件列表预览中：令牌仍是持有者凭证，接受这一代价的前提是有效期默认七天且可配置、任务关闭立即撤销、机器人邮箱为专用账户（默认有效期同日随后改为 72 小时，见下文第 2 条）。

**令牌的有效使用范围（2026-09-20 维护者确认）：**

1. **只接受该任务最新一条通知的令牌。** 旧 nid 的令牌一律以原因码 `token_superseded` 拒绝，重放窗口从「整个有效期」缩短到「下一条通知发出为止」。同一条通知下的连续回复仍然全部保留，不违反「连续回复全部保留」的产品边界。已知代价：用户正在读一条旧通知时回复会被拒，本地要能看到这条拒绝原因。4b 需要一个「任务的最新 nid」判定；反正要新增 `NotificationByNID` 与按实际投递 ID 查通知的接口，附带这一个成本很低。
2. **`TokenTTL` 默认值由 168h 改为 72h**（三天；可配置范围仍为 1h–720h）。七天对一个会印在锁屏上的持有者凭证偏长，三天能跨过一个周末。`internal/config` 的默认值与示例配置已同步修改。

**尚未验证：** L1 中主题标签只测了 `[TC <10 位任务 ID>]` 的形态，携带令牌后主题会长出约 49 个字符。4b 实现新主题格式后，须按同一规程重发一封并由两个客户端各回复一次，确认令牌在回复主题中原样保留（不被截断、不被换行折断、编码不变），再据此冻结主题解析器。

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
| 自动回复与退信 | 满足任一条件即判为非人工来信，硬拒并只记录元数据：**解码后的主题以已知的自动回复前缀开头**（`自动回复:`、`自動回覆：`、`Auto-Reply:`、`Automatic reply:` 等，全角半角冒号与大小写都宽容，前缀之后允许空白）；`Auto-Submitted` 取值不为 `no`；带 `X-Autoreply`/`X-Autorespond`；`Precedence` 为 auto_reply/bulk/junk/list；类型为 `multipart/report`；`Return-Path: <>`；发件人为 MAILER-DAEMON/postmaster；正文以 QQ 自动回复的固定开头起始。**主题前缀这一条是 2026-09-21 维护者确认的破例**：原则改为「不按主题关键词过滤正文内容，但允许按前缀分类自动回复」，理由见「L1 结果」第 3 步的补采样本——QQ 的假期自动回复不带任何头部信号，主题前缀是唯一可用的判据 |
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

- [x] **Step 1：写失败的测试（`write_test.go`）。**
  - `Render(Draft{"bot@example.invalid", "me@example.invalid", ["me@example.invalid"]})` 的结果与 `configs/turncourier.example.toml` 逐字节相同；
  - 对 20 组合法草稿（含多个白名单地址、含 `+` 与 `'` 的本地部分），把 `Render` 的结果写入临时文件后 `Load` 成功，字段与草稿一致；
  - 地址未规范化（含大写）、白名单为空或重复、机器人地址在白名单中时报错；
  - `CreateFile`：新建文件权限 0600、父目录 0700、内容一致、目录中没有残留的临时文件；目标已存在时返回 `fs.ErrExist` 且原文件内容与修改时间不变；父路径是普通文件时报错；所有错误文本不含临时目录路径。
- [x] **Step 2：写失败的测试（`init_test.go`）。** 替身：按脚本返回回答的 `Terminal`（记录每次调用的提示，`ReadSecret` 与 `ReadLine` 分开记录）；以 map 实现、分别记录 `Set` 与 `Add` 次数并可注入错误的 `keychain.Store`；`Getenv` 把 `TURNCOURIER_CONFIG` 与 `TURNCOURIER_DATA_DIR` 指向 `t.TempDir()` 下尚不存在的子目录；固定随机源与时钟。授权码金丝雀在运行时构造为 `strings.Repeat("canary", 3)`，遵循「测试向量与密钥扫描」的约定。
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
- [x] **Step 3：写失败的测试（`cli_test.go`、`terminal_test.go`、`main_test.go`）。**
  - 帮助、用法与规划命令的文案与上文逐字相同；`TestHelp` 的 4 个入口输出含新描述且不含 `Phase`；`init --json`、`init extra` 返回 2 与参数错误文案；`run` 返回 2 与新的规划命令文案。
  - `NewTerminal` 以 `os.Pipe` 构造时 `Interactive()` 为 false；`ReadLine` 从管道读出一行并去掉 `\n`，超过 1024 字节时报错；`ReadSecret` 在非终端上返回错误。成功读入机密的路径需要伪终端，自动测试不覆盖，由 L1 中维护者运行 init 验证（写入实施说明）。
  - `main_test.go` 以 `init` 调用 `run`：先用 `t.Setenv` 把 `TURNCOURIER_CONFIG` 与 `TURNCOURIER_DATA_DIR` 指向临时目录；标准输入是终端时（直接在终端里运行编译出的测试二进制）`t.Skip`，以免真实提问并写入登录钥匙串。`go test` 以 `/dev/null` 作为测试进程的标准输入，所以 `Interactive()` 为 false（不带包参数运行 `go test` 时标准输出可能是终端，这不影响判断）。按 `runtime.GOOS` 断言：darwin 上返回 1 与「需要在交互式终端中运行」，其他平台返回 1 与「只支持 macOS」；两种情况都不启动 `security`，临时目录中没有新建文件。
- [x] **Step 4：** `go get golang.org/x/term@v0.46.0`；测试失败。
- [x] **Step 5：实现。** 按上文；`ReadSecret` 先 `term.GetState` 保存状态，在 goroutine 中调用 `term.ReadPassword`，ctx 结束时 `term.Restore` 并返回；`ReadLine` 逐字节读取，不使用带缓冲的读取器，以免吞掉随后 `ReadPassword` 要读的输入。
- [x] **Step 6：** `go test -race ./internal/cli/ ./internal/config/ ./cmd/...` 通过；`make check` 与 `make secrets` 通过；`make build` 后 `./dist/turncourier help`、`help --json`、`init --json`（返回 2）行为符合上文；`go version -m dist/turncourier` 列出 `BurntSushi/toml`、`modernc.org/sqlite`、`golang.org/x/term`，不含 `emersion`。
- [x] **Step 7：** `git commit -m "feat(cli): add interactive init storing credentials in the Keychain"`

**实施说明：** 先写测试：`go test ./internal/config/ ./internal/cli/ ./cmd/...` 失败，config 与 cli 包编译失败（`undefined: Draft`、`undefined: Render`、`undefined: Terminal`、`undefined: InitDeps`、`undefined: Deps`），cmd 包因 `missing go.sum entry for module providing package golang.org/x/term` 无法构建；Step 4 的 `go get golang.org/x/term@v0.46.0` 之后，cmd 包的 `TestRunInitWithoutTerminal` 失败（返回 2 与「init 尚未实现」）。`go mod tidy` 只把 x/term 从间接依赖改为直接依赖，`go.sum` 只新增 x/term v0.46.0 的两行，其他依赖不变。实现要点：`Render` 的模板与示例配置逐字相同，主机、端口、默认事件与令牌有效期取自 `config.go` 的默认值常量；先对每个地址检查「`NormalizeAddress` 之后不变」（错误不回显输入），再复用 `Load` 的 `normalizeSenders` 检查白名单为空、重复与含机器人地址。`CreateFile` 依次 `MkdirAll(0700)`、`CreateTemp`（0600）、写入、`Sync`、关闭、`os.Link`，临时文件由 `defer` 删除；`os.Link` 的错误是同时带两个路径的 `*os.LinkError`，既有的 `withoutPath` 只处理 `*fs.PathError`，因此在 `CreateFile` 内取出底层错误，`errors.Is(err, fs.ErrExist)` 仍成立。init 按清单的 11 步执行：配置与授权码在全部输入读完之后才写入，顺序为创建配置并以 `Load` 核对、`Set` 授权码、令牌密钥、正文密钥；失败原因、重问原因与「输入不是 y 或 N」提示写到标准错误，摘要与两段说明写到标准输出，提示经 `Terminal` 写到终端（生产为标准输出）；`Terminal` 的 `ReadLine` 逐字节读取，读到第 1025 个字节即返回不回显内容的错误、不再读取该行其余部分，换行之前输入结束时返回 `io.EOF`；`ReadSecret` 先 `term.GetState`（不是终端即报错，不读取输入），ctx 已结束时不再启动 `term.ReadPassword`（避免它在恢复之后才关闭回显），ctx 结束时 `term.Restore`，读完补写一个换行。契约之外的补充与取舍：① 授权码条目读取返回 `ErrInvalidSecret`（条目存在但内容格式不符）时按「已存在」处理，同样询问是否替换、不自动覆盖；② 密钥已登记时 `Get` 返回 `ErrInvalidSecret` 按「与数据库登记的不符」处理，未登记时按「格式不符的同名条目」处理；③ 沿用未登记的已有条目（上次中断或并发的 init 写入）时摘要显示「已存在」，只有本次 `Add` 成功才显示「已生成」；④ 失败时只要 ctx 已结束就打印「已取消。」，不限于读入时；⑤ 清单没有给出文案的几处：同一项连续 3 次无效打印「同一项连续 3 次输入无效，init 已退出。」，重问前打印「输入无效：」加原因，`Keychain()` 返回 `ErrUnsupported` 以外的错误打印「无法使用 Keychain：」加原因，配置在读入授权码期间被其他进程创建时打印「配置文件刚被其他进程创建；init 没有覆盖它，请重新运行 turncourier init。」；⑥ 现有配置与新建配置的核对都经 `config.Load`，`TURNCOURIER_NOTIFY_EVENTS` 无效时同样报告；⑦ staticcheck 的 ST1005 不允许错误文本以大写字母开头，而清单规定的两条文案以「Keychain」开头，因此这两个错误变量各加一条 `//lint:ignore ST1005` 并写明原因，这是仓库中第一次使用该指令；没有使用 `gitleaks:allow`。测试方面：`KEYCHAIN-CANARY` 按「底层错误会被报告、但它不是机密」理解，断言它出现在标准错误中，授权码金丝雀与密钥文本不出现；「存储中已预先登记了另一个校验值」在 `Add` 替身的钩子中另开一个存储完成登记（模拟另一个 init 在本次读元数据之后、登记之前登记），若在运行前登记，init 会走「已有元数据」分支，测不到并发登记，同一钩子另测登记同一把密钥时成功；「中断后的状态」第一例为使「没有调用 Add 或 Set」成立，预置配置、实例、授权码、正文密钥（含元数据）与令牌密钥条目，只缺令牌元数据。清单之外补了：默认路径下的摘要相对描述、`n`/`N`/单个其他字符/`yes` 等回答、授权码为空或含非 ASCII、授权码条目格式不符、五种 Keychain 读取错误、随机源失败时不写入不登记、读入授权码期间配置被其他进程创建、新建配置核对失败、读入失败、摘要写入失败、相对路径与其他 `Keychain()` 错误；`ReadLine` 用例还断言一行之后的输入仍留在管道中（钉住不使用带缓冲的读取器），并以 5 秒期限让带缓冲的实现尽快失败。在仓库副本上做了 29 个变异，每次只改一处，涉及：`Render` 不检查规范化、`CreateFile` 改用 `os.Rename`、不删临时文件、父目录 0755、不去掉 `LinkError` 路径、`yes` 视为替换、提示阈值、不比对校验值、`ErrExists` 后不重读、`ErrKeyExists` 后不比对或比对取反、密钥用 `Set` 写入、缺条目视为存在、授权码允许空格或上限 129、取消时不打印「已取消。」、不检查交互终端、空白名单不回落到接收地址、地址可试 4 次、格式不符的授权码不询问、已登记条目格式不符视为相符、未登记格式不符走通用错误、摘要写出配置路径、`created` 恒为真、行上限 1025、`init` 接受 `--json`、写配置先于读授权码、写授权码先于写配置、`ReadLine` 使用带缓冲的读取器，全部被测试杀死（最后一个最初靠 `go test` 的 10 分钟超时才失败，加 5 秒期限后约 5 秒失败）。验证：`go test -race ./internal/cli/ ./internal/config/ ./cmd/...` 通过；`make check`（含中文注释检查与 staticcheck，总覆盖率 93.87%，cli 包 92.8%，config 包 97.8%）、`make secrets`、`GOOS=windows go vet ./...`、`GOOS=linux go vet ./...` 通过；`make build` 后 `./dist/turncourier help` 与 `help --json` 返回 0 且文案与上文一致，`init --json` 返回 2 并打印参数错误；`go version -m dist/turncourier` 列出 `BurntSushi/toml v1.6.0`、`modernc.org/sqlite v1.59.0` 及其依赖、`golang.org/x/term v0.46.0`、`golang.org/x/sys v0.48.0`，不含 `emersion` 与 `golang.org/x/text`，二进制 11,736,130 字节（darwin/arm64）。未运行：成功读入机密的路径（真实终端上的不回显读入、Ctrl-C 后恢复回显）需要伪终端，留给 L1 由维护者在本机运行 init 验证；实施中没有交互式运行 `turncourier init`，没有访问真实钥匙串，也没有读写真实配置目录；测试只在 macOS 上运行，Linux 上的测试由 CI 运行。与前面各任务相同，本任务的提交包含本清单的勾选与实施说明。

**审查后修正：** 变异均在仓库副本中进行，每次只改一处。① 授权码替换确认：`TestInitRerun` 补 ` y`、`y `、`Y\t`（按 N 处理并提示）与单个多字节字符 `是`（按 N 处理、不提示）；提示本身含「 y 」，回显检查改为先去掉提示再查找回答。比较前 `strings.TrimSpace` 与按字节计数（`len(answer) > 1`）两个变异都被杀死。② 地址重问：`TestInitAddressValidation` 补一例，机器人地址与接收地址各先无效 2 次再输入有效值，断言成功且三项提示依次出现 3、3、1 次；三项共用一个无效计数的变异被杀死。③ `TestCreateFile` 在调用前把 `TMPDIR` 指向不存在的目录，临时文件改建到系统临时目录（`os.CreateTemp("", …)`）的变异被杀死（否则目标在另一个卷上时 `os.Link` 会以 EXDEV 失败）。④ `ReadSecret` 的取消竞态：原实现先检查 ctx 再在协程中调用 `term.ReadPassword`，关闭回显发生在协程里；ctx 恰在两者之间结束时，`term.Restore` 先于关闭回显，进程退出后终端停留在不回显的状态。现改为在启动读取协程之前经新增的 `terminal_darwin.go`（`x/sys/unix` 的 `IoctlGetTermios`/`IoctlSetTermios`，标志与 `term.ReadPassword` 相同：去掉 ECHO，保留 ICANON 与 ISIG，加 ICRNL）同步关闭回显，`defer term.Restore` 恢复，协程只用 `readLine` 逐字节读取，不再改动终端设置；macOS 以外的平台由 `terminal_other.go` 报错（init 在这些平台上于读取任何输入之前就已退出）。这是对 Step 5 与 `Terminal` 契约注释（「用 term.ReadPassword 不回显读入一行」）的有意偏差，注释已改为「关闭回显读入一行，标志与 term.ReadPassword 相同」；随之与 `ReadPassword` 的细微差别是：机密一行同样以 1024 字节为上限，换行之前遇到输入结束时返回 `io.EOF`，不再返回已读的部分。审查建议的另一做法（取消后短暂等待再恢复一次）只是缓解，没有采用。`golang.org/x/sys` 因此由间接依赖改为直接依赖：`go mod tidy` 只把 `go.mod` 中 v0.48.0 这一行移到直接依赖块，`go.sum` 不变，没有引入新模块或新版本（该模块已随 Task 1 升级到 D2 规定的 v0.48.0，并已链接进产品二进制）。⑤ 新增 `terminal_darwin_test.go`，在 macOS 的 `/dev/ptmx` 上离线测试终端生产实现（从端设备名经 `TIOCPTYGNAME` 取得；`x/sys` 的 `SYS_IOCTL` 在 macOS 上标为弃用，staticcheck 报 SA1019，因此这一处用标准库 `syscall.Syscall`）：从端同时作标准输入与标准输出时 `Interactive` 为 true，标准输出是普通文件或标准输入是管道时为 false；`ReadLine` 之后 `ReadSecret` 读到下一行，读入期间回显关闭，终端输出恰为 `"bot@example.invalid\r\n地址：授权码：\r\n"`（机密没有回显），读完回显恢复；读取阻塞时取消，回显立即恢复；ctx 已结束时不写提示、回显开着；另有 300 轮在读取刚开始时取消、取消时机逐轮在 0–39 微秒之间变化、每轮稍等 2 毫秒后检查回显的竞态用例。清理时先关闭主端（挂断使阻塞在从端上的读取返回）再关闭从端，否则被放弃的原实现读取协程会让从端的 `close` 一直阻塞。Interactive 只检查标准输入、`&&` 改为 `||`、去掉 `defer term.Restore`、去掉 ctx 预检查、不清除 ECHO、把关闭回显挪进读取协程六个变异都被杀死；把 `terminal.go` 换回修复前的实现，竞态用例在 7 次运行中全部失败（记下轮次的 6 次都在第 30 轮之前）；修复后的实现连续 20 次运行（加与不加 `-race`）都通过。这组测试覆盖了原先留给 L1 的「成功读入机密」路径，L1 仍在真实终端上复核（由 Ctrl-C 经 SIGINT 触发的取消只在真实终端上验证），L1 规程表「准备」一行的备注相应修改。⑥ 配置路径是悬空的符号链接时，`Load` 跟随链接得到「不存在」，`os.Link` 不跟随链接而报已存在，init 每次都在读完授权码之后才失败，无法收敛。现在 `Load` 返回 `ErrNotFound` 后先 `os.Lstat` 配置路径，存在即在询问之前退出，打印「配置文件的位置已存在但无法读取（可能是悬空的符号链接）；init 不会修改它，请检查后重新运行 turncourier init。」（清单之外的文案，不含路径）；`TestInitExistingData` 补此例，断言没有读入任何输入、Keychain 未写入、链接与链接目标都未被改动或创建、数据目录不存在；去掉该检查的变异被杀死。验证：`go test -race ./internal/cli/ ./internal/config/ ./cmd/...` 通过；`make check`（总覆盖率 94.21%，cli 包 95.8%，config 包 97.8%）、`GOOS=windows go vet ./...`、`GOOS=linux go vet ./...`、`GOOS=linux` 下的 staticcheck 与 `make secrets` 通过。未运行：Linux 上的测试由 CI 运行，伪终端测试只在 macOS 上编译运行；没有交互式运行 `turncourier init`，没有访问真实钥匙串。 ⑦ 复查后补充：⑥ 只检查配置路径本身。配置目录或更上级的目录是悬空的符号链接时（例如 dotfiles 工具把配置目录链接到尚不存在的位置），`Lstat` 同样报不存在，init 照常询问全部地址与两次授权码；随后 `CreateFile` 的 `MkdirAll` 在链接上 `Mkdir` 得到 EEXIST，该错误以 `%w` 包装、满足 `errors.Is(err, fs.ErrExist)`，init 误报「配置文件刚被其他进程创建」，重新运行结果相同。现在从两处修正：`CreateFile` 包装 `MkdirAll` 的错误改用 `%v`（文本不变），使 `fs.ErrExist` 只表示目标已存在，与契约一致；init 的询问前检查改为 `danglingLink`，自配置路径起逐级向上找到第一个 `Lstat` 成功的路径（找到即说明更上级都能解析），它的 `Stat` 报不存在即为悬空，此时在询问之前退出，⑥ 的提示改为「配置文件或它的上级目录是悬空的符号链接；init 不会修改它，请检查后重新运行 turncourier init。」（清单之外的文案，不含路径）。`TestInitExistingData` 的悬空链接用例扩为三例，链接分别位于配置文件、配置目录与更上级的目录，断言没有读入任何输入、Keychain 未写入、链接未被改动、链接目标未被创建、数据目录不存在；另补一例，配置目录链接到已有目录时照常创建配置文件。新增 `TestCreateFileParentIsDanglingLink`：父目录是悬空的符号链接时报错，错误不满足 `fs.ErrExist`、不含路径，链接目标未被创建。改前新用例失败的情形与审查所述一致（配置目录与更上级目录两例打印「配置文件刚被其他进程创建」并读入了授权码）。变异：`MkdirAll` 的错误改回 `%w`、去掉检查、检查退回只看配置路径本身是否存在、不向上查找、至多查两级、把任何符号链接都当作悬空、只查配置目录一级，七个变异都被杀死。验证：`make check`（总覆盖率 94.19%，cli 包 95.6%，config 包 97.8%）、`GOOS=windows go vet ./...`、`GOOS=linux go vet ./...`、`GOOS=linux` 下的 staticcheck 与 `make secrets` 通过。未运行：Linux 上的测试由 CI 运行；没有交互式运行 `turncourier init`，没有访问真实钥匙串。

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
{"schema":"turncourier-l1/3","kind":"reply","client":"QQMail 2.x","from_role":"recipient","from_case_variant":false,
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

- [x] **Step 1：** `go get github.com/emersion/go-message@v0.18.2 golang.org/x/text@v0.42.0`；修改 Makefile 与 `.gitignore`：

```make
vet:
	$(GO) vet ./...
	$(GO) vet -tags live ./tests/live/

lint: $(STATICCHECK)
	goroot="$$($(GO) env GOROOT)" && PATH="$$goroot/bin:$$PATH" $(STATICCHECK) ./... && PATH="$$goroot/bin:$$PATH" $(STATICCHECK) -tags live ./tests/live/
```

- [x] **Step 2：写失败的离线测试（`sample_test.go`，不带标签）。** 用合成的 `.invalid` 邮件字节（含 GB18030 base64 正文、带令牌的 QQ A 格式引用、`Auto-Submitted: auto-replied` 自动回复）调用样本函数，断言：
  - 输出中不含合成正文里的金丝雀句子、显示名、地址与日期；令牌识别正确；自动回复 `kind` 为 `auto`；
  - `In-Reply-To` 等于 `state.json` 中「已发送」副本 ID 的回复归类为 `delivered_sent`；同时等于 DATA 响应 ID 时为 `delivered_sent+delivered_data`；形如 `tencent_…@qq.com` 但不等于任何记录的 ID 为 `other_tencent`；
  - 主题 `回复：[TC …] …` 的前缀为 `回复：`；主题 `关于合同 13800138000 [TC …]` 的前缀为 `"other"`；主题被改写为 `关于合同 13800138000`（没有标签）时 `prefix` 为 `null`、`tag_intact` 为 false；三种情况的输出都不含 `13800138000`；
  - DATA 响应文本依次含我方 ID、抄送 ID、一个不带尖括号且不等于任何记录的 `tencent_…@qq.com` 形式的 ID、一个普通地址，并把我方 ID 重复一次：前三者依次换成 `<ours>`、`<delivered_cc>`、`<other_tencent>`，普通地址换成 `<addr>`；`delivered_data` 恰为按出现顺序的三个元素（第三个补上尖括号，重复的我方 ID 只记一次）；`In-Reply-To` 等于其中第三个元素的回复归类为 `delivered_data`；
  - 输出目录校验：仓库根目录内的子目录、以大小写变体书写的同一目录（在不区分大小写的文件系统上它存在，校验按 inode 拒绝；在区分大小写的文件系统上它不存在，同样被拒绝）、位于仓库外但指向仓库内子目录的符号链接、权限 0755 的目录、相对路径与不存在的目录都被拒绝；仓库外权限 0700 的目录通过，指向它的符号链接也通过，且返回解析后的真实路径。去掉「先解析符号链接」的变异使符号链接用例失败。
- [x] **Step 3：写探测工具。** 按上表与输出规则实现 `sample.go` 与三个带标签的文件；写中文包注释。带标签的文件只做 I/O 与确认，脱敏与归类全部调用 `sample.go` 的纯函数。
- [x] **Step 4：验证开关。**
  - `go test ./...`、`make check` 与 `make secrets` 通过；`tests/live` 只运行 `sample_test.go` 的离线测试，不读取配置、Keychain 或网络；`sample_test.go` 中的合成令牌在运行时构造（例如用 `bytes.Repeat` 构造的密钥与按下标填充的 nid 签发后取 `Reveal()`），测试向量遵循「测试向量与密钥扫描」的约定；
  - `env -u TURNCOURIER_LIVE go test -count=1 -tags live ./tests/live/` 输出「跳过」并通过，没有读取配置、Keychain 或网络（在临时 HOME 下运行，确认没有新建任何文件）；
  - `CI=1 TURNCOURIER_LIVE=1 go test -count=1 -tags live ./tests/live/` 失败并输出「拒绝在 CI 中运行」；
  - `grep -rn 'tags live\|TURNCOURIER_LIVE' .github/` 无结果。
- [x] **Step 5：** `git add .gitignore Makefile go.mod go.sum tests/live && git commit -m "test(live): add manual L1 probes guarded by build tag and explicit consent"`

**实施说明：** Step 1 的 `go get` 把 go-message 固定在 v0.18.2、x/text 由模块图中的 v0.14.0 升到 v0.42.0；写完代码后的 `go mod tidy` 把两者由间接改为直接依赖，`go.sum` 另有 x/mod、x/sync、x/tools 三行哈希随 x/text v0.42.0 的模块图更新，它们只在模块图中，产品二进制仍不链接邮件依赖。Makefile 的 `vet` 与 `lint` 按清单增加 live 标签的检查。Step 2 先写 `sample_test.go` 并运行 `go test -count=1 ./tests/live/`，失败基线为 `no non-test Go files in …/tests/live`（`sample.go` 尚不存在）；Step 3 实现后这 13 个顶层离线用例全部通过，不带标签部分的语句覆盖率为 87.6%。不带标签的 `sample.go` 提供 `Analyze`（来信脱敏归类）、`ClassifyID`、`RedactDataResponse`、`ComposeNotification`、`CopyHeaders`、`JudgeIdleCheck`、`IdleConclusion`、`RepoRoot`、`ValidateOutputDir` 与状态、样本文件的读写；带标签的三个文件只做 I/O 与确认。清单没有给出 QQ A/B/C 等分隔线的正则，本任务按公开可见的形式定了一张表：A 为长破折号包围「原始邮件」或 Original、B 为短破折号、C 为单独一行，另有「写道」与 `wrote:` 行、没有分隔线就直接开始的引用头块（Foxmail 式）和 `>` 引用；HTML 标记表同理。这些类别名只是观测标记，L1 之后由维护者把类别与客户端对应起来。样本在清单给出的字段之外多了两项：`folder`（来信所在文件夹，这正是同时补扫 Junk 与 INBOX 的原因）与 `received`（清单要求 Received 头「只计数」）。预检与 FETCH 检查的判定以同一连接上一次 EXAMINE 的往返耗时为尺子，要求 Idle 耗时的两倍仍小于一次往返，才算「远小于 IDLE 与 DONE 两次往返」。钥匙串往返中，合成条目的删除是本用例自己所建条目的清理，写在第一次确认的文案里，并由 Cleanup 在中途失败时兜底；对已存在条目直接调用 `security -i` 以记录退出码之前另有一次确认。`sample.go` 中令牌字母表的常量名为 `crockfordAlphabet` 而不是 `tokenAlphabet`：后者会让 gitleaks 默认的 generic-api-key 规则因「token」关键字加 32 个字符的高熵值而报泄漏（已实测）。偏差一项：`go test` 在包列表模式下会丢弃通过的包的输出，`env -u TURNCOURIER_LIVE go test -count=1 -tags live ./tests/live/` 通过但看不到「跳过」，加 `-v` 才显示，本任务因此在验证中同时运行了带 `-v` 的命令；`CI=1 TURNCOURIER_LIVE=1 …` 的失败输出不受影响，直接可见。Step 4 验证全部通过：`go test -count=1 ./tests/live/`、`make check`（总覆盖率 93.3%，含 `go vet -tags live ./tests/live/` 与 `staticcheck -tags live ./tests/live/`）、`make secrets`（无泄漏）、`GOOS=windows go vet ./...`、`GOOS=linux go vet ./...`；在临时 HOME 下 `env -u TURNCOURIER_LIVE go test -count=1 -tags live ./tests/live/` 通过，且该 HOME 下只留下 Go 工具链自己的缓存，没有任何 TurnCourier 文件；`CI=1 TURNCOURIER_LIVE=1 …` 失败并输出「拒绝在 CI 中运行」；`grep -rn 'tags live\|TURNCOURIER_LIVE' .github/` 无结果。另用只含 example.invalid 地址的合成配置，在没有控制终端的环境下跑通 TestMain 的流程：输出目录合法时停在总确认并报「打不开 /dev/tty」，输出目录在仓库内、权限不是 0700、配置缺失时分别给出对应的错误，全程不访问钥匙串与网络。变异测试：去掉「先解析符号链接」使符号链接用例失败；去掉主题前缀白名单后主题中的数字进入样本；DATA 响应不替换普通地址、来源标签顺序颠倒，都被离线用例拦截。本任务没有运行任何真机探测：没有登录邮箱、没有发信，也没有读写任何真实钥匙串条目；开关验证中虽然设置过 TURNCOURIER_LIVE=1，但每次都停在总确认之前（CI 拒绝运行、打不开 /dev/tty、配置或输出目录无效），不曾访问钥匙串与网络。

**审查后的修正（Task 13）：** 两名审查子代理提出的 11 项全部确认并修复，均补了能拦截该问题的离线用例。脱敏：主题 encoded-word 的字符集位置、`Content-Type` 的媒体类型（解析失败时 go-message 返回整行原文）、`charset` 参数与 `Content-Transfer-Encoding` 都来自来信可控的字节，此前原样写入样本，构成「不输出完整主题」的旁路；现统一经 `safeLabel` 约束为标签形状（小写，只含 ASCII 字母、数字与 `. _ + / -` 且至少有一个字母，至多 255 个字符（媒体类型），字符集、传输编码与自动来信关键字另按 40 个字符截断），不符合的一律记为 `other`。筛选：`Analyze` 此前丢弃 HTML 正文的令牌匹配结果，只有纯文本的匹配算「正文含探测令牌」，没有线程头、主题被改写、令牌只留在 HTML 引用中的回复会被静默丢弃（正是 D1 指出的 QQ App/Foxmail 行为），现由两处正文一并判定。`state.json` 的 `delivered_data` 去掉 `omitempty`，使「没有候选」按清单写成空数组；`TestL1SendNotification` 在 `ErrUncertain`（已进入提交阶段、可能已投递）时也先把这封邮件写入 `state.json`，否则对方回复时 `TestL1Replies` 认不出它。新增的离线用例覆盖畸形标签、令牌行前缀的兜底脱敏与 `>` 引用前缀、令牌边界、`Plain.Skeleton` 的逐项结构、`HTML.Blockquote`、`client` 与主题前缀的两个截断上限、`other_tencent` 不算已知来源，以及空的 `delivered_data`；用变异测试逐条确认这些用例能杀死对应的改动。偏差一项（清单第二处）：`TestL1Idle` 的 FETCH 检查在「预检未能进行」时，清单写的是「本检查照常执行」，此前实现退化为零游标，即 `UID SEARCH UID 1:*`，会从真实收件箱取回至多 50 封无关邮件的完整正文（至多 16 MiB），与预检「不以回绕的游标继续」的取向相反；现改为以本会话 `Examine` 得到的 UIDNEXT−2 构造游标（只取回 UID 最大的那一封，即刚发出的自发邮件），UIDNEXT 小于 2 时把本检查记为 `unknown` 并跳过补扫。修复后验证：`go test -count=1 ./tests/live/`（不带标签部分覆盖率 88.7%）、`make check`、`GOOS=windows go vet ./...`、`GOOS=linux go vet ./...`、`make secrets` 均通过；仍然没有运行任何真机探测。 第三轮复查后补充：把上限提到 255 本是为了让 OOXML 的 Word 与 Excel 媒体类型可分辨，但同一个上限也作用在主题编码字的字符集位置，而那段字节直接取自 Subject 头，形状合法的主题文字因此可以输出到 248 个字符。现另设 `maxCharsetRunes = 40` 并新增 `safeCharset`，用于主题编码字的字符集、`Content-Type` 的 charset、传输编码与 `Auto-Submitted`/`Precedence` 关键字；媒体类型仍为 255。`TestAnalyzeCapsCharsetLabels` 钉住这一点，去掉该上限的变异使它失败。

**第二轮审查后的修正（Task 13）：** 两项全部确认并修复。① 上一轮只把 `safeLabel` 用在 `encodingOf` 与 `describePart` 的四处，同一文件的 `keyword`（供 `autoSignals` 读取 `Auto-Submitted` 与 `Precedence`）仍是「取分号前的文字、转小写、截到 40 个字符」后原样写入样本；这两个头同样完全由来信控制，合法取值同样是极小的一组 token，垃圾邮件（`TestL1Replies` 会补扫 Junk）正会往其中塞任意文字，脱敏旁路依旧存在。现改为经 `safeLabel` 约束（顺带去掉复用 `maxClientRunes` 的错配），`TestAnalyzeRedactsMalformedLabels` 增加「畸形自动来信头」子用例：两个头含金丝雀手机号时取值为 `other`、样本中不出现该号码；修复前该子用例失败（取值为 `客户张三的手机号 13800138000 合同编号 abc` 与 `金丝雀 13800138000`）。归为 `other` 不影响 `classifyKind`：畸形取值仍非空且不等于 `no`，来信照样记为 `auto`。② 自定上限 `maxLabelRunes = 40` 对形状合法的标签也一律截断，`application/vnd.openxmlformats-officedocument.wordprocessingml.document` 与 `…spreadsheetml.sheet` 的前 40 个字符完全相同，两种附件在样本中无法分辨，而清单的样本结构对 `mime[].type` 并未规定长度上限，L1 采集 MIME 结构正是为了让 4b 的解析器建立在真实观测上。形状检查（禁空白、禁非 ASCII、必须含字母）才是脱敏屏障，对已通过形状检查的 token 再截断不增加脱敏强度。现把 `maxLabelRunes` 提到 255（RFC 6838 对媒体类型的上限：type 与 subtype 各至多 127 个字符），注释中写明截断只是兜底；新增 `TestAnalyzeKeepsLongMediaType`，对上述两个类型断言原样保留，修复前两者都被截成 `application/vnd.openxmlformats-officedoc`。修复后验证：`go test -count=1 ./tests/live/`（不带标签部分覆盖率仍为 88.7%）、`make check`（总覆盖率 93.45%）、`GOOS=windows go vet ./...`、`GOOS=linux go vet ./...`、`make secrets` 均通过；仍然没有运行任何真机探测：没有登录邮箱、没有发信，也没有读写任何真实钥匙串条目。整阶段审查后补充（SEC-2）：同一文件的 `client` 是样本中唯一没有形状约束的来信可控自由文本，清单「客户端：`client` 取 `X-Mailer` 或 `User-Agent`，截断到 40 个字符」据此加固为「形状合法（可打印 ASCII 且不含引号）时截到 40 个字符，否则记为 `other`」；详见 Task 15 的「整阶段审查后修正（`tests/live`、`tests/docs`、测试有效性）」。

### Task 14：文档同步

**Files:**
- Modify: `docs/zh-CN/design.md`、`SECURITY.md`、`docs/en/architecture.md`、`docs/zh-CN/development.md`、`README.md`、`README.zh-CN.md`、`CHANGELOG.md`、`AGENTS.md`、`CONTRIBUTING.md`

- [x] **Step 1：`design.md`。**
  - 状态行：Phase 4 已拆分为 4a、L1、4b，4a 的内部包与 `init` 已实现，尚不能收发邮件；init 的环境诊断（复用 `doctor`）与真实往返测试在 4b 接入。
  - 「安全与持久化细化」写入 D3 的威胁模型：经 `/usr/bin/security` 创建的条目，受信任应用是 `/usr/bin/security`、分区为 `apple-tool:`，同一用户的任何进程（包括 Agent 执行的 shell 命令，只要其沙箱放行）都能静默读出授权码与两把密钥；读出之后，它可以用令牌密钥签发有效令牌，伪造通过发件人、线程、主题标签与令牌全部校验的回复，把任意内容注入任一任务，也可以直接改写本地队列，TurnCourier 不防同一用户下的进程；Keychain 在这里防的是明文进入配置、仓库、日志与备份，防其他系统用户，防没有登录密码的离线磁盘访问；不在进程内调用 Security.framework 的原因（Go 二进制没有稳定签名，每次升级都会重新弹窗）；缓解措施（专用机器人邮箱、可随时停用授权码）；发布阶段再评估 Developer ID 签名加进程内调用（优先 purego，不引入 CGO）。
  - 同一节写明：正文摘要为 HMAC-SHA256（令牌签名密钥、独立前缀、对解析后的新正文计算），列名 `body_sha256` 因已发布而保留；待处理正文以 AES-256-GCM 加密、关联数据绑定用途、密钥号、任务与序号，进入终态的同一事务中删除，数据库启用 `secure_delete` 并在删除后尽力执行 TRUNCATE 检查点；APFS 快照与 Time Machine 中的残留不在 SQLite 能控制的范围内，正文密钥仍在 Keychain 中时这些残留的密文可以被解密；把数据目录恢复到旧备份会让已发出的通知重发、已确认的回复再次派发。
  - 目标目录树：`security/` 下增加 `payload/`（待处理正文加密）；`mail/smtp/` 的注释由「发信与重试」改为「发信（重试由调用方按待发通知状态机处理）」。
  - 开发顺序一段的「当前处于」改为邮件闭环阶段（4a 已完成离线部分，等待 L1）。
  - 不改动 D4 仍待 L1 定稿的线程规则表述（包括「稳定 Message-ID」一句），它们在 4b 修订。
- [x] **Step 2：`SECURITY.md`（中英文同步）。**
  - 报告范围加入 `internal/security/keychain`、`token`、`payload`、`internal/mail/smtp`、`imap`、`internal/queue` 的待发通知状态机、迁移 `0002` 与 `turncourier init`，举例：机密进入进程参数、环境变量、日志、错误文本或数据库明文；接受被篡改或过期的令牌；接受被调换的密文；删除后正文仍可从数据库或 WAL 文件中恢复；在未加密的连接上发送凭据；IMAP 修改机器人邮箱；init 覆盖已有配置或密钥；`tests/live` 在未设置显式开关时联网或访问 Keychain。
  - 不属于范围：同一用户的进程读取 Keychain 条目，以及由此伪造回复或改写本地队列（已记录的威胁模型）；APFS 快照与 Time Machine 中的残留。
  - 「规划设计中的边界」一节写入威胁模型摘要，并把尚未实现的部分改为：邮件收发循环、入站验证、Agent 适配器与后台服务。「发件人白名单、线程引用、短任务 ID 和签名令牌必须全部一致，否则拒绝」一句（中英文）补充限定：这一校验防的是其他人冒充，不防能读取本机 Keychain 条目的同一用户进程。
- [x] **Step 3：`docs/en/architecture.md`。** 状态行；Implemented 表增加 Keychain、令牌、正文加密、待发通知状态机与存储、SMTP、IMAP、`init`、`tests/live`；Not implemented 更新；包与依赖方向图按本清单「4a 文件结构」一节；包职责表逐包补充；命令解析规则与退出码表加入 `init`（0、1、2 的含义）；「Persistence and recovery」补充 `0002` 的各表、`secure_delete` 与检查点、正文生命周期、键控摘要、实例 ID 与 Keychain 条目布局、待发通知的状态与恢复（`RecoverSendingNotifications` 与 `RecoverInFlight` 同样只能由唯一的进程在开始工作之前调用）；Dependencies 表加入 D2 的模块、版本、许可证与使用方，并写明产品二进制目前链接的模块；Quality gates 写明 `vet` 与 `lint` 也检查 live 构建标签下的代码。
- [x] **Step 4：`docs/zh-CN/development.md`。** 状态行；第三方依赖表；新增「Keychain 条目」一节：service 与 account 的命名，查看条目用 `security find-generic-password -s io.github.chaorookie.turncourier -a <account>`（不加 `-w`，不打印机密），数据目录删除后遗留条目的手工删除方法，「有元数据但缺少条目」与「条目与登记的校验值不符」时 init 为何拒绝重新生成或覆盖，密钥条目只以不覆盖的方式创建；测试约定补充假 `security` 子进程（`TURNCOURIER_TEST_SECURITY_*`）、go-smtp 与 imapmemserver 假服务器、借用 `httptest` 证书、故障代理；新增「真机探测（tests/live）」一节：构建标签与 `TURNCOURIER_LIVE=1` 双重开关、`CI` 下拒绝运行、访问 Keychain 与网络前的总确认、逐封确认、全部命令带 `-count=1`、输出目录必须在仓库之外且权限为 0700、不带标签的样本函数与其离线测试随 `make test` 运行、各探测项与环境变量、样本入库前须人工审阅；make 目标表的 `vet`、`lint` 行写明包含 live 标签；「配置文件与数据目录」改为 init 已能创建配置，数据库保存待处理正文的密文，并写明不要把数据目录恢复到旧版本：从不自动重发与防重复派发都以数据库状态连续为前提，必须恢复时先人工核对 PENDING 通知与 QUEUED 回复；「隐私与凭据」改为授权码已由 init 写入 Keychain。
- [x] **Step 5：README（中英文同步）。** 状态与当前能力（`init` 可用，尚不能收发邮件）；实际文件树按 `git ls-files --cached --others --exclude-standard | sort` 核对，加入本阶段新建的全部文件，`phase-04.md` 的注释改为「Phase 4：邮件闭环（4a 已实现，待 L1）」与对应英文；代码规则中的依赖说明加入 D2 的模块；「安全与私密」一段写入威胁模型摘要。
- [x] **Step 6：** `CHANGELOG.md` 在 `[Unreleased]` 增加 Phase 4a 条目（Added、Changed、Security 分开写）；`AGENTS.md` 当前范围改为「已实现配置、存储、状态机、Keychain、令牌、正文加密、SMTP/IMAP 客户端与 init；尚未接入收发循环与 Agent」；`CONTRIBUTING.md` 中英文的当前阶段清单改为 `phase-04.md`。
- [x] **Step 7：检查。** `git diff --check` 通过；在全部文档中检索本机路径、用户名与订阅档位无结果；README 两份文件树一致且覆盖 `git ls-files` 的全部文件；Phase 3 Task 11 Step 6 的 `check` 链断言仍无输出；检索「已支持邮件收发」「can send」一类表述无结果（不把规划能力写成已实现）。
- [x] **Step 8：** `git commit -m "docs: document phase 4a mail foundations and the keychain threat model"`

**实施说明：** 本任务只改文档，没有改产品代码。开工前的失败基线：两份 README 的文件树各漏 44 个本阶段新建的文件（按 `git ls-files --cached --others --exclude-standard` 的 basename 逐项比对），其余四项 Step 7 检查在改动前就已通过，改完后重新跑全部五项均通过。

Step 1：`design.md` 状态行补上 Phase 4 的 4a/L1/4b 拆分与 4a 已交付的内容，并写明仍不能收发邮件、init 的环境诊断与真实往返测试在 4b；顺带去掉 Phase 3「尚未接入命令行」的说法，因为 init 已经使用配置与存储。「安全与持久化细化」新增两段：D3 的 Keychain 威胁模型（受信任应用与分区、同一用户的进程可读出三项机密、读出后可签发有效令牌伪造通过全部校验的回复并改写本地队列、Keychain 实际防住的三件事、不在进程内调用 Security.framework 的原因、缓解措施与发布阶段再评估 Developer ID 加 purego），以及正文摘要与密文（`body_sha256` 列名保留但改为键控摘要、AES-256-GCM 与关联数据、终态同一事务删除、`secure_delete` 与 TRUNCATE 检查点、APFS 快照与 Time Machine 残留可解密、恢复旧备份会重发与重复派发）。目标目录树 `security/` 下加 `payload/`，`mail/smtp/` 注释改为「发信（重试由调用方按待发通知状态机处理）」；开发顺序末句改为邮件闭环阶段。按清单要求没有动 D4 仍待 L1 定稿的线程规则表述。

Step 2：`SECURITY.md` 中英文同步。报告范围改为列出 CLI（含 `init`）与各内部包（含迁移 `0002`、keychain、token、payload、mail/smtp、mail/imap、待发通知状态机），并按清单加了 6 条示例（机密进入参数/环境变量/日志/错误文本/数据库明文、接受被篡改或过期的令牌、接受被调换的密文、删除后仍可从数据库或 WAL 恢复、未加密连接上发凭据或 IMAP 修改邮箱、init 覆盖已有配置或密钥、`tests/live` 无开关即联网或访问钥匙串）。不属于范围新增两条：同一用户的进程读取 Keychain 条目及由此伪造回复、改写队列；APFS 快照与 Time Machine 残留。「规划设计中的边界」改写：未实现的部分改为收发循环、入站验证、通知渲染、Agent 适配器与后台服务，新增一整段威胁模型，四条校验一致性的边界补上「防其他人冒充，不防能读取本机 Keychain 条目的同一用户进程」。偏差一处：删掉了英文「Mailbox authorization codes and signing keys are to be stored in the macOS Keychain…entered through a local `init` command」一条（中文对应条同样删除），因为它描述的能力已经实现，留着会与新增的威胁模型段落重复且自相矛盾。

Step 3：`docs/en/architecture.md` 按清单逐项更新。Implemented 表新增 init、Keychain、令牌、正文加密、邮件客户端、`tests/live` 六行，并改写 CLI、配置、状态机、存储、集成测试各行；Not implemented 改为「产品中的收发尚未接线」，并点名 `internal/app` 不存在、令牌撤销与密钥轮换未实现。依赖方向图照搬清单「4a 文件结构」一节的图，另外说明 `cmd` 也直接导入 `internal/security/keychain`（它要构造 Keychain 工厂）。包职责表补 keychain、token、payload、mail/smtp、mail/imap、`tests/live` 六行并改写 cli、config、queue、store、integration 五行。命令解析规则写明 `init` 不接受任何参数（含 `--json`），退出码表三行都补上 init 的含义（0 的摘要、1 的各种原因与「已取消。」、2 含 `init --json`）。「Persistence and recovery」新增 `0002` 各表的表格、Pending bodies、Keys and the Keychain、Outgoing notifications 三段，并改写 Database（`secure_delete` 为 ON 而非 FAST）、Deduplication（UID 键含 folder、Message-ID 键不含、摘要为键控）、Recovery（`RecoverSendingNotifications` 与 `RecoverInFlight` 同样只能由唯一进程在开始工作前调用）。Dependencies 表加入 D2 的六个模块并写明产品二进制当前链接的模块，Quality gates 写明 vet 与 lint 也带 live 标签。顺带修掉三处因本阶段而失效的旧句：配置文件段「Keychain 尚未接线」、Planned architecture 的开头免责句与末段「当前阶段」。

Step 4：`docs/zh-CN/development.md`。状态行、第三方依赖表（六个新模块的版本、许可证、使用方与选型理由，并写明产品二进制只链接 toml、sqlite、x/term、x/sys）；make 目标表的 vet、lint 行写明带 live 标签；新增「Keychain 条目」一节（service/account 表、实例 ID 的来源与作用、查看与删除命令、密钥条目只创建不覆盖、`crypto_keys` 只存元数据、两种不一致时 init 为何拒绝、威胁模型摘要、开发测试从不接触真实钥匙串）；新增「真机探测（tests/live）」一节（两类文件、四道关、输出目录规则、五个探测项与七个环境变量的表格、`-count=1`、样本入库前人工审阅）；测试约定补充假 `security` 子进程的三个 `TURNCOURIER_TEST_SECURITY_*` 变量、go-smtp 与 imapmemserver 加故障代理的假服务器、借用 `httptest` 自签证书（含 IMAP 需清 `NextProtos` 的原因）、三个 `source_test.go` 钉住的否定性质；「配置文件与数据目录」改为 init 已能创建配置、数据库保存待处理正文密文、不要恢复旧备份；「隐私与凭据」改为授权码已由 init 写入 Keychain，并补上「机密形状的测试向量一律在运行时构造、提交前跑 `make secrets`」的约定与 `.gitignore` 新增的两个文件名；候选构建一节的命令列表补上 `init`。

Step 5：两份 README。状态段改写为「init 可用、仍不能收发邮件」，并逐包说明本阶段新增的包；「目标体验」点明其中只有 init 录入授权码这一条已实现；命令表拆出 `init` 行、规划命令去掉 init；退出码补 init；代码规则的依赖说明列出全部模块并区分「二进制链接的」与「只有测试使用的」；文档列表的 phase-04 注释改为 4a 已实现、待 L1；「安全与私密」新增威胁模型摘要、密文残留与不要恢复旧备份、`tests/live` 的开关说明。文件树用脚本生成以保证两份对齐一致（注释统一从第 38 列开始，与原有写法相同），新增 `internal/mail`、`internal/security`、`tests/live` 等目录，`tests/integration` 改挂到新的 `tests/` 节点下。核对方式：`git ls-files --cached --others --exclude-standard` 的每个文件名都能在树中找到（两份均缺失 0 项），反向检查树中也没有不存在的文件；两份树的名称列逐字节相同。偏差一处：两份 README 的 make 目标表里 `make vet`、`make lint` 两行仍写着旧的命令，Task 13 改了 Makefile 但没有同步这两处，本任务一并补上 live 标签。

Step 6：`CHANGELOG.md` 的 `[Unreleased]` 导语改写，新增 `#### Phase 4a: offline mail foundations` 的 Added 条目，并在已有的 Changed 一节后追加 4a 的 Changed 条目、新增 Security 一节（威胁模型、正文密文与残留、只用隐式 TLS 且由源码测试钉住、结果不确定从不自动重发）。`AGENTS.md` 当前范围按清单改写并补上 Phase 4 清单的位置。`CONTRIBUTING.md` 中英文的当前阶段清单改为 `phase-04.md`。偏差一处：`CONTRIBUTING.md` 的「项目阶段与范围」「开发环境」「隐私与凭据」中英文共 8 处仍写着 init 是规划命令、没有命令使用配置与存储、Keychain 存储尚不存在、生产代码只用两个模块、init 尚未实现，与本阶段状态直接矛盾；Step 6 只点名了阶段清单链接，本任务把这 8 处一并改为与实现一致。同理，`CHANGELOG.md` 中 Phase 3 小节的「no command uses them…binary links no new modules」改为过去式并指向下文的 4a 条目。

Step 7 的五项检查全部通过（命令见上）。另外运行：`make check`（通过，总语句覆盖率 93.4487%，含 `go vet -tags live ./tests/live/` 与 `staticcheck -tags live ./tests/live/`）、`GOOS=windows go vet ./...`、`GOOS=linux go vet ./...`、`make secrets`（无泄漏）均通过。文档中引用的实现细节都对照源码核对过，包括 `internal/security/*` 与 `internal/mail/*` 的包注释与导出注释、`0002_mail.sql` 的表与触发器、`internal/cli/init.go` 的拒绝条件与摘要文案、`internal/store/sqlite/wal.go` 的检查点行为，以及 `go version -m dist/turncourier` 的实际输出（只列出 toml、modernc 及其依赖、x/term、x/sys v0.48.0，不含 emersion 与 x/text）。本任务没有登录任何邮箱、没有发信、没有读写任何真实钥匙串条目，也没有运行 `tests/live` 中的真机测试。

**审查后修正：** 审查指出的六条都成立，都出在「使用方」「校验值」两类事实陈述上。① `golang.org/x/text` 的使用方：两份依赖表原文照搬 D2 的「只被 `go-message/charset` 导入」，但 Task 13 的 `tests/live/sample.go` 直接导入了 `encoding/simplifiedchinese`，go.mod 也因此把它列在直接 require 块中；现按事实改写，保留「经 `tests/live` 进入构建图」的结论。② `README.md`、`README.zh-CN.md` 与 `CONTRIBUTING.md` 中英文共 4 处把 x/text 归给邮件包（`go list -deps ./internal/mail/imap` 与 `./internal/mail/smtp` 都不含 x/text），改为由 `tests/live` 使用。③ 两份依赖表补 `golang.org/x/sys` v0.48.0 一行：`internal/cli/terminal_darwin.go` 直接导入 `golang.org/x/sys/unix`（见 Task 12 审查后修正 ⑤），仓库依赖规则要求直接依赖在表中留有记录；同时把它从「`modernc.org/sqlite` 另外带入」的清单里移出，并在 `README.md`、`README.zh-CN.md`、`CONTRIBUTING.md` 的「二进制链接」句中补上它。④ `docs/en/architecture.md` 的「Keys and the Keychain」补上校验值的 MAC 密钥就是密钥材料本身，否则同一 (purpose, kid) 的校验值是常量，与下一句「`init` compares that value with the entry in the Keychain」讲不通。⑤ `CHANGELOG.md` 中 Phase 3 的「Only metadata and body SHA-256 digests are stored」按同小节另一句已采用的做法改为过去式并指向 4a（`body_sha256` 已改为键控摘要，`notification_payloads`、`reply_payloads` 会把待处理正文以密文落盘）。 复查后补充（主会话）：统一了六处口径不一致的表述——CHANGELOG 的产品二进制枚举补上 `golang.org/x/sys`；两份 README 的「只读访问邮箱」限定到 IMAP 客户端；「规划体验」的引用块改为「另有注明者除外」，与下文 `init` 已可用不再矛盾；`architecture.md` 的依赖方向图把 `tests/live ─► security/*` 展开为实际导入的 `security/keychain, security/token`，`0002` 表格中「已有行补 INBOX」移到 `inbound_messages` 一侧；`development.md` 的测试约定补上 `tests/docs/`，模糊测试一段补上 `internal/security/token` 的 `FuzzParse`。

清单偏差与额外改动三处：① 依赖方向图在 `internal/cli` 一行补上 `golang.org/x/sys/unix`（termios，仅 macOS）。本清单「4a 文件结构」的图里没有它，Step 3 要求「按本清单」，但那张图早于 Task 12 审查后修正 ⑤ 引入 `terminal_darwin.go`，照抄会与新补的表行自相矛盾，因此只改 `docs/en/architecture.md` 中的图，清单原图不动。② 同一处核对发现 `github.com/emersion/go-sasl` 的使用方也照搬了 D2 的「由 go-imap/v2 间接引入，不单独使用」，而 `internal/mail/smtp/smtp.go` 直接导入它并调用 `sasl.NewPlainClient`；这与审查指出的是同一类错误，且不改就无法让新增的测试通过，一并改为如实描述。③ 新增测试包 `tests/docs`（`deps_test.go`，只读仓库文本，不联网、不访问钥匙串、不起子进程）：解析 `go.mod` 的 require 块取出全部直接依赖，用 `go/parser` 以 ImportsOnly 扫描仓库中全部非测试 Go 文件得到每个模块的仓库内直接导入方，再解析两份文档中四栏、首栏为反引号标识符的表行，要求每个直接依赖在两份表中都有一行、版本栏写明 go.mod 中的版本（伪版本按后 12 位子串比对），且每个直接导入方都在「使用方」一栏中以反引号点名。失败基线：在仓库副本上把 x/sys 行与 go-sasl 的使用方改回修复前的写法，该测试对两份文档各报一条缺行、各报一条 go-sasl 未点名 `internal/mail/smtp`，共 4 条失败；另做两个变异（把 `golang.org/x/term` 的版本栏改为 v0.45.0、把 x/text 的使用方改为「邮件包使用它」）也都被杀死。该测试拦得住「表中漏记直接依赖」「版本写错」「使用方漏掉真正的导入方」，拦不住写错理由的散文（x/text 那条旧文案点名了 `tests/live`，只是理由错），也拦不住 README、CONTRIBUTING 的散文与 ④⑤ 两条措辞问题——这三处只做了改写，没有自动化检查。随新增文件同步更新：两份 README 的文件树（`tests/docs/`）、`docs/en/architecture.md` 的包职责表、`CHANGELOG.md` 的 4a Added 一条。

验证：`make check`（通过，总语句覆盖率 93.4487%，`tests/docs` 无语句）、`GOOS=windows go vet ./...`、`GOOS=linux go vet ./...`、`make secrets`（无泄漏）、`git diff --check` 均通过；Step 7 的 README 文件树检查重跑，两份树对 `git ls-files --cached --others --exclude-standard` 各缺 0 项、名称列逐行相同（167 行）。本次修正没有登录任何邮箱、没有发信、没有读写任何真实钥匙串条目，也没有运行 `tests/live` 中的真机测试。

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
**整阶段审查后修正（`internal/mail`）：** 以下三条由整阶段审查提出、经反方验证员复现后确认，修复后各以变异证明测试有效。除 MAIL-2 外每条都先写能拦住该问题的测试。各 Task 的契约正文不改，本段说明实现相对契约的调整。

- **MAIL-1（major）`internal/mail/imap/watcher.go`：Junk 持续失败会让 INBOX 完全收不到新邮件。** 原实现每轮先补扫 Junk 再补扫 INBOX，且只有「EXAMINE 被带标签 NO 拒绝」（`ErrNoFolder`）被当作 Junk 的非致命失败；Junk 的其他持续失败（带标签 BAD、无标签 BAD 造成的超时、FETCH 失败等）会从 `serve` 返回而拆掉整条连接，INBOX 在该连接上一次也没补扫过，重连后又在同一处失败，于是 Junk 长期异常时收信完全停摆，并且每轮都消耗一次登录，很快撞上「1 小时至多 12 次登录」的上限。修法三点：① 每轮改为先补扫 INBOX 再补扫 Junk（Task 11 契约与「4a 已定的实现细节」表中的「先 Junk 后 INBOX」按本条改为「先 INBOX 后 Junk」，契约原文保留存档），Junk 的失败不再影响本次连接已交付的 INBOX 新邮件；② `runner` 记 Junk 连续失败次数（跨重连累计，成功一次清零），单轮失败只发出 `folder_unavailable` 并跳过本轮、不把错误交给 `serve`，连续失败达到包内变量 `junkFailLimit`（默认 3，测试可降低）后发出一次新状态 `folder_disabled`（`StatusFolderDisabled`，命名与既有 `idle_disabled` 一致，`Folder` 为文件夹名），本次 `Run` 余下时间不再扫描 Junk；③ Junk 批次的 `Handle`／`Cursor` 失败同样以 `junkFailLimit` 为上限（本轮累计，成功只复位退避、不清零计数）结束本轮（INBOX 仍按契约在同一连接内无限重试），否则 Junk 的本地失败会在 `drain` 内无限重试而同样饿死 INBOX。INBOX 的失败、退避与登录频率限制、只读访问、命令白名单都不变。改为 INBOX 优先带出两处必须同时修正的细节，否则 INBOX 的推送会丢：一是 Junk 的 EXAMINE（无论成功还是被拒绝）都会使选中的文件夹不再是 INBOX，而 IDLE 只在选中的文件夹上等待，所以补扫 Junk 之后重新 EXAMINE 一次 INBOX（这一步失败按 INBOX 的失败处理）；二是 INBOX 补扫期间到达的 EXISTS 会被 Junk 的 `Scan` 在 EXAMINE 之前排空，补扫 Junk 前后把该信号原样放回，`wait` 才能立即回到 INBOX 再补扫一轮。新增 `TestWatcherDegradesJunkAfterRepeatedFailures` 三个子用例：Junk 的 EXAMINE 持续被带标签 BAD 拒绝（代理新增 `faultRejectBAD`）时连接不断、`LOGIN` 仍为 1 次、Junk 恰好被 EXAMINE 3 次后降级、INBOX 的新邮件照常交付；Junk 的 EXAMINE 持续被无标签 BAD 挂起（超时关连接）时第一条连接上 INBOX 已交付、降级后不再登录也不再碰 Junk、每封 INBOX 邮件只交付一次；Junk 批次的 `Handle` 一直失败时每轮恰好 2 次 `handle_failed` 后结束本轮、3 轮后降级、Junk 游标不前进、INBOX 继续交付。另把 `TestWatcherDelivers` 的首批次断言改为「先 INBOX 后 Junk」。六个变异都被杀死：去掉降级、恢复 Junk 优先、忽略本地失败上限、去掉补扫 Junk 后重新 EXAMINE INBOX、去掉 EXISTS 信号的放回、Junk 失败重新交给 `serve`（其中「恢复 Junk 优先」由第一条 EXAMINE 必须是 INBOX 的断言与 `TestWatcherDelivers` 共同拦截）。
- **MAIL-3（minor）`internal/mail/smtp/smtp.go`：`ctx` 在写正文期间结束会被误判为结果不确定。** `Send` 的契约是「`ctx` 在进入提交阶段之前结束返回 `ErrNotSent`」，但写正文的分块经库的 4 KiB 缓冲，小邮件或末尾数据仍在缓冲时，看门狗因 `ctx` 结束关闭连接后这些写入照样返回 nil，失败被推迟到提交阶段而归为 `ErrUncertain`——这类邮件的结束标记从未写出，服务器不可能已接受，却因此永远不再自动重发。按审查建议采用方案①：在提交一步开始之前补一次 `ctx` 检查，已结束时返回包装 `ErrNotSent` 的错误（步骤名 `body`）。提交阶段之后的保守分类不变。新增 `TestSendCanceledDuringBody`：写正文时取消 `ctx`、等看门狗关闭连接后再让该分块写入（返回 nil），断言错误同时满足 `ErrNotSent` 与 `context.Canceled`、文本以 `: body: context canceled` 结尾，且服务器从未收到结束标记；去掉这次检查，该用例得到 `ErrUncertain` 而失败。既有 `TestSendFlushStall`（flush 期间失败 → `ErrUncertain`）与 `TestSendCanceled`（结束标记之后取消 → `ErrUncertain`）保留并通过。
- **MAIL-2（经验证降为 nit）`internal/mail/imap/session.go`：`LOGIN` 被带标签 NO 拒绝一律记为 `ErrAuthFailed`。** 不改行为，只在该处补注明理由：本包不保留服务器响应文本与响应码，无法区分授权码错误与「暂时不可用」一类的临时拒绝；而调用方对认证失败的处置（暂停 `AuthPause` 再重试）在两种原因下都安全，反过来把认证失败当作临时错误会反复登录，可能触发服务器的封禁。这与 Task 11 契约中「`LOGIN` 被回 NO 为 `ErrAuthFailed`」以及「认证失败后至少暂停 10 分钟」的已定决策一致；QQ 实际使用的拒绝响应码留待 L1 记录，任何细分放在 4b 并须维护者确认。本条为纯注释改动，没有新增测试。

**整阶段审查后修正（`internal/store/sqlite`、`internal/cli`、`internal/security/token`）：** 以下五条由整阶段审查提出、经反方验证员复现后确认，除纯注释的 CRYPTO-2 外每条都先写能拦住该问题的测试，再以变异（去掉修复后测试必须失败）证明测试有效。各 Task 的契约正文不改，本段说明实现相对契约的调整。

- **CRYPTO-1（经验证为 minor）`internal/cli/init.go`：存在性判定与 `RegisterKey` 的唯一性域不一致。** `ensureKey` 用 `ActiveKeyID` 判断「该用途是否已有密钥」，而 `RegisterKey` 的唯一性域是「该用途是否已有任何状态的密钥」。把 `crypto_keys` 中令牌密钥的状态改为 `retired` 或 `destroyed` 后重新运行 `init`：`ActiveKeyID` 返回 `ErrNotFound`，`init` 走进「尚未登记」分支，沿用 Keychain 中已有的条目、被 `RegisterKey` 的 `ErrKeyExists` 拦下、再比对校验值相同，于是退出码 0、摘要写「已存在」——已被弃用的密钥被当作当前密钥继续使用，与 Task 12 第 9 步的描述相反；条目缺失时这条路径还会生成一把新密钥。修法：`internal/store/sqlite/instance.go` 新增 `RegisteredKey(ctx, purpose) (kid, state, error)`，查询条件与 `RegisterKey` 的唯一性检查相同（该用途的任何一行，按 kid 升序取一条，没有时返回 `ErrNotFound`）；`ensureKey` 改用它判断存在性，登记过就一律走校验分支，状态不是 `active` 时返回新的 `errKeyNotActive`（「密钥元数据存在，但登记的密钥状态不是 active；init 不会生成新密钥，也不会改动任何条目。」）并退出，不生成密钥、不写 Keychain、不改元数据。Task 12 第 9 步「已有元数据：`ActiveKeyID` 找到 kid 时……」按本条改为按 `RegisteredKey` 判断，契约原文保留存档；4a 每个用途仍只登记 kid 1 的 active 密钥，正常路径的行为不变。新增 `TestInitKeyNotActive`：`retired` 与 `destroyed` 两个子例各断言退出码 1、标准错误恰为该句、`Add` 与 `Set` 均为 0 次、Keychain 条目与令牌密钥状态不变、正文密钥的元数据仍是 kid 1 的 active 且与条目相符、输出不泄露机密或路径（改状态经独立连接直接写 `crypto_keys`，4a 的存储只登记 `active`）。三个变异都被杀死：① 改回 `ActiveKeyID`；② 保留 `RegisteredKey` 但去掉状态判断；③ `RegisteredKey` 的查询加上 `state = 'active'`。
- **CRYPTO-3（minor）`internal/store/sqlite/notifications.go`：owner 超出令牌 Claims 的范围时仍会建出通知。** `NewNotification.validate` 不校验 owner（owner 来自任务行，不在输入中），而 `token.Claims` 要求 owner 为 1–255 字节，于是 owner 为空或超过 255 字节时存储会建出一条永远签不出令牌、因而永远发不出去的通知，正文也已加密落盘。`tasks.owner` 没有长度约束，正常路径只写默认值，外部工具改写后即可出现。修法：`CreateNotification` 在读出任务、确认未关闭之后，写入任何行之前按字节校验 owner（包内新增 `maxOwnerLen = 255`，注释写明与 `token.Claims` 同一区间、存储不导入 token 包），不合法时返回 `invalid notification:` 开头的错误，不建通知也不加密内容。新增 `TestCreateNotificationChecksOwner`：空、256 字节、86 个三字节字符（258 字节）三种取值都被拒且通知与正文表仍为 0 行；1 字节、255 字节、85 个三字节字符（255 字节）都能创建且快照中的 `Owner` 与任务一致。两个变异都被杀死：① 去掉校验；② 把长度改为按 Unicode 字符计（258 字节的取值只有 86 个字符，会被放行）。
- **STORE-1（minor）`internal/store/sqlite/notifications.go`：`RecordDeliveredMessageID` 缺合法 UTF-8 校验。** 该函数按注释「长度按 Unicode 字符计，与表约束一致」在开始事务前校验，却没有按同一注释所依赖的前提检查合法 UTF-8。实测（modernc.org/sqlite v1.59.0，SQLite 3.53.4）：`"<\xff\xfe>"` 与 `"<\x80>"` 在 Go 与 SQLite 的 `length()` 中分别都计为 4 与 3 个字符，两者都通过 CHECK 约束、带着非法字节原样落盘——补 `utf8.ValidString` 的理由是「表约束挡不住非法 UTF-8」，而不是「Go 与 SQLite 计数不一致导致事务中途被拒」。（本条措辞按整阶段审查后的复查 RECHECK-3 更正，原记录称 `"<\x80>"` 在 SQLite 中只计 2 个字符并在事务中途被 CHECK 拒绝，该后果并未发生。）修法：按注释补上 `utf8.ValidString`，错误文本相应改为 `must be 3-998 characters of valid UTF-8 without NUL`，函数注释写明理由（同 `checkMailbox`）。`TestRecordDeliveredMessageID` 的非法取值表补上这两个值，另加一例上下文已取消时仍返回校验错误而不是 `context.Canceled`（钉住「在开始事务前报错」），并断言通知快照不变。去掉 `utf8.ValidString` 的变异被杀死。
- **STORE-2（minor）`internal/store/sqlite/replies.go`：`InboundReply.validate` 与 `checkMailbox` 规则不一致。** `validate` 对 `Account`、`Folder`、`MessageID` 只检查字符数与 NUL，不要求合法 UTF-8，而 4a 新增的 `checkMailbox`（游标与被拒来信用它）要求合法 UTF-8，于是同一个文件夹名可以记录回复却推不动它的游标；非法 UTF-8 的取值还会按 STORE-1 的两种方式落盘或在事务中途被拒。修法：`validate` 改为先调用 `checkMailbox(in.Account, in.Folder)` 并以 `invalid inbound reply:` 包装，两处从此是同一份判定；`MessageID` 增加 `utf8.ValidString` 要求，与原有的 NUL 检查合为一条分支。`Account` 与 `Folder` 的长度、空白、NUL 规则以及 UID、Message-ID 长度、正文的判定都不变，去重与冲突判定不涉及这些规则，未改动。随 `unicode` 包不再被 `replies.go` 使用而删去该 import。测试：`TestRecordReplyValidation` 补 `Account`、`Folder`（0xff 与续字节 0x80 两种）、`MessageID` 的非法 UTF-8 用例；新增 `TestRecordReplyMatchesMailboxRule`，对七组取值断言 `AdvanceCursor` 与 `RecordReply` 要么都接受、要么都拒绝。把 `validate` 改回原实现的变异被两个测试同时杀死。
- **CRYPTO-2（经验证降为 nit）`internal/security/token/token.go`：`BodyDigest` 不拒绝零值 `Key`。** 直接构造的零值 `Key`（kid 0、密钥全零）在 `BodyDigest` 中不报错，算出的是以全零密钥求得的摘要；同一个零值 `Key` 在 `Issue` 中返回 `ErrInvalidKey`。不改行为：`BodyDigest` 的签名由 Task 3 契约规定，没有错误返回，改签名或改为 panic 都超出本阶段范围，而实际到不了这条路径——`NewKey` 出错时返回 nil，忽略错误的调用方在这里 panic 而不是得到坏摘要；按 4b 的入站顺序，密钥按通知行的 `token_kid` 取出且令牌先经 `Verify`，零值 `Key` 只会得到 `ErrKeyMismatch`。按最小修法处理：在 `BodyDigest` 的注释中记为已知局限并写明这条边界，`TestBodyDigest` 补两条断言钉住文字——零值 `Key` 的摘要恰等于全零密钥的参考实现，以及零值 `Key` 的 `Verify` 返回 `ErrKeyMismatch`。这两条断言钉住文档所说的安全边界，不对应某个实现变异。

验证：`make check`（含中文注释检查、`-race`、staticcheck 与覆盖率门槛，总覆盖率 93.2437%，`internal/cli` 95.6%、`internal/store/sqlite` 87.6%、`internal/security/token` 100%）、`CGO_ENABLED=0 go test -count=1 ./...`、全仓 `GOOS=linux go vet` 与 `GOOS=windows go vet`、`make secrets`、`git diff --check` 均通过。本次修正不涉及钥匙串与网络：没有读写真实钥匙串条目，没有登录真实邮箱或连接 QQ 的服务器，没有以 `live` 标签运行 `tests/live`；测试只在 macOS 上运行，Linux 上的测试由 CI 运行。

**整阶段审查后修正（`tests/live`、`tests/docs`、测试有效性）：** 以下各条由整阶段审查提出、经反方验证员复现后确认，每条都先写能拦住该问题的测试，再以变异（去掉修复后测试必须失败）证明测试有效。各 Task 的契约正文不改，本段说明实现相对契约的调整。

- **SEC-1（minor）`tests/live/sample.go`：`Analyze` 与 `CopyHeaders` 把来信字节带进错误文本。** 两处原先在解析失败时返回 `fmt.Errorf("cannot parse message: %w", err)`，而 go-message 在头部畸形时会把整行来信原文放进错误文本（本地实验复现：`message: malformed MIME header line: <整行原文>`、`message: malformed MIME header key: <字段名>`），`probe_test.go` 的补扫循环又把该错误原样打印到测试输出，绕过了「只输出脱敏的结构样本」。修法：新增两个不携带任何来信字节的哨兵错误——`ErrMalformedHeader`（首行、某个头行或字段名不合规范）与 `ErrHeaderTooLarge`（头部超过解析器上限；go-message 的这条错误未导出也没有包装，只能按其固定文本判别，文本中不含来信字节，上游改写文本时这一类退回 `ErrMalformedHeader`，仍不泄露来信字节）——两个函数改为经包内 `parseMessage` 解析，只返回哨兵错误。未知字符集与传输编码不算解析失败（实体仍可读，而这类错误文本同样含来信字节），照旧丢弃并继续归类；MIME 结构畸形不另设一类：`describePart` 保留已解析出的部件，样本记录到哪一层为止，这一取向不变。调用处只打印文件夹、UID 与哨兵错误（`probe_test.go` 本就只打印这三项，未改动）。`TestAnalyzeRejectsMalformed` 改为三个子用例（畸形头行、畸形字段名、头部超限），每个都对 `Analyze` 与 `CopyHeaders` 断言错误恰为对应哨兵且文本中不含金丝雀句子。变异（改回包装底层错误）被杀死，且失败信息里直接出现了金丝雀句子，与审查的判断一致。
- **SEC-2（经验证为 nit）`tests/live/sample.go`：`client` 是样本中唯一没有形状约束的来信可控自由文本。** `X-Mailer` 与 `User-Agent` 完全由来信控制，畸形邮件可以把正文或主题塞进去，原先只按 40 个字符截断，截断不是脱敏屏障。按审查建议加形状约束：新增 `safeClient`，只接受可打印 ASCII（0x20–0x7e）且不含单双引号的取值，不符合的一律记为 `other`，合法取值仍按清单截到 40 个字符；两个头都没有时保留空串，以便与「有客户端标识但形状不合」区分。Task 13 清单中「**客户端：** `client` 取 `X-Mailer` 或 `User-Agent`，截断到 40 个字符」按本条加固为「形状合法时截到 40 个字符，否则记为 `other`」，契约原文保留存档。新增 `TestAnalyzeRedactsClient` 五个子用例（合法取值原样输出、非 ASCII、含引号、含控制字符、两个头都没有），并断言样本中不出现客户端标识里的金丝雀；既有 `TestAnalyzeTruncatesLongValues` 的超长取值由非 ASCII 改为 ASCII，因为它钉的是截断上限，而非 ASCII 现在会先被记为 `other`。变异（改回只截断）被「含引号」与「含控制字符」两个子用例杀死。
- **SEC-3（nit）`tests/live/sample.go`：`authResult` 的取值没有白名单也没有长度上限。** `Authentication-Results` 同样取自来信可控的字节，原先只做小写化，正则只限制为字母、不限长度。现收敛到 RFC 8601 第 2.7 节的已知取值（pass、fail、none、neutral、softfail、temperror、permerror、policy），其余记为 `other`；头缺失时仍为空串。新增 `TestAnalyzeConstrainsAuthResults` 两个子用例（已知取值原样输出、白名单之外记为 `other` 且样本中不出现该文字）。变异（去掉白名单）被第二个子用例杀死，样本中出现了 120 个字符的来信文字。
- **TEST-01（minor）`tests/docs/imports_test.go`（新增）：清单「依赖方向（4a 结束时）」没有任何测试守卫。** 那是跨包的架构不变量，单个包的测试看不见自己被谁导入，只有整仓扫描能拦住。新增只读源码的测试，用 `go/parser` 以 `ImportsOnly` 解析仓库中全部非测试 `.go` 文件（跳过点开头的目录与 `dist`），断言：`internal/mail/**` 不导入 `internal/store/**`、`internal/config`、`internal/security/**`；`internal/store/sqlite` 不导入 `internal/security/token` 与 `internal/config`；`cmd`、`internal` 下没有包同时导入 `internal/mail/**` 与 `internal/store/**`。测试不联网、不访问钥匙串、不执行子进程。三个变异都被杀死：在 `internal/mail/imap` 中空导入 `internal/config`、在 `internal/store/sqlite` 中空导入 `internal/security/token`、在 `internal/cli` 中空导入 `internal/mail/smtp`（该包已导入存储）。
- **TEST-03（minor）`tests/live/sample_test.go`：副本排除的两条依据只覆盖了一条。** `isProbeCopy` 按 Message-ID 等于我方 ID、「已发送」副本或抄送副本排除探测邮件自身，但原有用例的副本都带 `X-TurnCourier-Probe` 头，第一条判断就已返回，按 ID 排除的分支从未执行。`TestAnalyzeSkipsProbeCopies` 增加两个子用例：不带探测头、Message-ID 分别等于 `delivered_sent` 与 `delivered_cc` 的副本都不输出样本（它们的正文含发出的令牌，去掉对应分支后会被当成回复采样）。两个变异（分别去掉 `DeliveredSent`、`DeliveredCC` 的比较）各杀死对应子用例。
- **TEST-04（minor）`tests/live/sample_test.go`：退信判定的两条依据没有各自独立成立的用例。** `classifyKind` 以「类型为 `multipart/report`」或「发件人本地部分为 MAILER-DAEMON/postmaster」判为 `bounce`，而 `TestAnalyzeBounce` 的那封退信同时满足两条，任一条失效都拦不住。新增 `TestAnalyzeBounceGrounds`：`multipart/report` 但发件人是普通收件人、以及普通 `multipart/alternative` 但发件人是 postmaster，各自都记为 `bounce`；同一封信两条都不成立时仍是 `reply`（对照项，防止把判定改成恒为 `bounce` 也能通过）。两个变异（去掉 `multipart/report` 判断、把退信发件人白名单清空）各杀死对应子用例，`TestAnalyzeBounce` 在这两个变异下都照常通过，印证了这处缺口。
- **TEST-05（minor）`internal/security/keychain/keychain_test.go`：「期限与取消先于退出码判断」的顺序契约没有用例。** `classify` 先看 `ctx.Err()` 再看退出码，因为被 exec 结束的子进程同样以非 0 退出；顺序颠倒会把「进程被我们杀掉」报成钥匙串的结论（`Get` 把超时报成 `ErrNotFound`，调用方据此另生成一把密钥或覆盖条目）。这种「期限与退出码同时成立」的情形靠真实往返撞不稳，因此直接对包内 `classify` 写用例：新增 `TestClassifyPrefersContext`，用已取消与已过期的 `ctx` 配上退出码 44、36 的真实 `*exec.ExitError`（`os.ProcessState` 无法直接构造，仍由测试二进制扮演的假 `security` 子进程产生，不接触真实钥匙串），断言返回取消或超时错误且文本一字不差；对照项是同一个退出码在正常 `ctx` 下仍归类为 `ErrNotFound`／`ErrInteractionNotAllowed`。变异（把退出码判断提到 `ctx` 判断之前）只被本用例杀死，既有的 `TestTimeout` 拦不住。
- **TEST-02、TEST-06 至 TEST-10（nit）：本次基本未处理。** 这几条的正文没有随本组分派下发，仓库与工作副本中也没有审查发现清单，无法逐条判断是否值得修；合并前由提出者确认是否仍需处理。按「值得修的顺手修掉」的要求，本次自行补了一处明显的覆盖缺口：`roleOf` 的「机器人自己」「白名单发件人」与「地址无法解析」三条分支此前都没有用例，而 `from_role` 是发件地址在样本中唯一的输出形式；新增 `TestAnalyzeFromRole` 覆盖这三条并断言样本中不出现地址，去掉白名单分支的变异被杀死。仍未覆盖的位置记录如下，供对照：`sizeBucket` 的各区间、`encodingOf` 的 8bit/plain 分支，以及状态与样本文件读写的 I/O 失败分支。

验证：`make check`（含中文注释检查、`-race`、staticcheck 与覆盖率门槛；总覆盖率记为 93.4426%，`tests/live` 不带标签部分由 88.7% 升至 90.8%，`internal/security/keychain` 仍为 100%；该总数经复查 RECHECK-6 证实与本机不符，实际见下一段，覆盖率随环境变化，只有 80% 门槛是固定要求）、`CGO_ENABLED=0 go test -count=1 ./...`、全仓 `GOOS=linux go vet` 与 `GOOS=windows go vet`、`env -u TURNCOURIER_LIVE go test -count=1 -tags live ./tests/live/`（输出「跳过」）、`make secrets`、`git diff --check` 均通过。本次修正不涉及钥匙串与网络：没有读写真实钥匙串条目，没有登录真实邮箱或连接 QQ 的服务器，也没有以 `live` 标签运行任何真机探测；测试只在 macOS 上运行，Linux 上的测试由 CI 运行。

**整阶段审查后修正的复查（RECHECK）：** 独立复查员对上述三组修复提出 7 条问题（2 major、1 minor、4 nit），逐条处理如下。成立的每条都先复现、再以最小改动修复并补上能拦住它的测试，最后以变异（去掉修复后测试必须失败）证明测试有效。

- **RECHECK-1（major）`internal/mail/imap/watcher.go`：MAIL-1 的降级把连接层的瞬时失败也计入 `junkFails`。** `drainJunk` 对 `drain` 返回的任何非 `ctx` 错误一视同仁地 `junkFails++`，而 `junkOff` 在整个 `Run` 内没有复位路径，于是三次恰好落在补扫 Junk 窗口内的断连就会让本次 `Run` 余下时间再也不扫描 Junk——修复前同样的情况只是断开重连后重试 Junk、能自行恢复，因此这是 MAIL-1 新引入的行为回归。复现：在 `EXAMINE Junk` 上注入 `faultDisconnect` 且只断 `junkFailLimit` 次（故障会自行消失），探针输出「EXAMINE Junk 次数 3 -> 3；Junk 交付 []；folder_disabled 1」，Junk 中那封邮件在本次 `Run` 内再也没有被取回。修法采用审查建议①：`drainJunk` 对 `errors.Is(err, ErrClosed)` 的连接层失败直接返回、既不计数也不发 `folder_unavailable`，由紧随其后的 `Examine(INBOX)` 触发原有的重连；`ErrNoFolder`、带标签 NO/BAD 的拒绝、`ErrTimeout` 与本地处理失败仍照旧计数（MAIL-1 的三个既有子用例都覆盖这些路径，未受影响）。新增 `TestWatcherDegradesJunkAfterRepeatedFailures/transient disconnects`：断言故障耗尽后 Junk 那封邮件被交付、`disconnected` 恰好 `junkFailLimit` 次（证明注入的确是连接层失败）、`folder_disabled` 与 `folder_unavailable` 都为 0。去掉这条 `ErrClosed` 分支的变异被杀死（Junk 交付超时）。
- **RECHECK-2（major）`internal/mail/imap/watcher_test.go`：`TestWatcherDelivers` 的首批次断言是时序竞态。** 补扫顺序改为「先 INBOX 后 Junk」之后，等到 INBOX 的 uid 3 并不意味着 Junk 的批次已经入列，而紧接着的断言却要求已有两个批次且第二个是 Junk。确定性复现：在 `drainJunk` 开头注入 30 ms 延迟后 `go test -count=5 -run 'TestWatcherDelivers$'` 五次全部在该断言处失败，失败信息显示只有一个 INBOX 批次。修法：快照 `h.batches` 之前补一行 `h.waitDelivered(FolderJunk, 1)`。保留同一处 30 ms 延迟重跑 `-count=5` 全部通过，说明这行确实消掉了竞态而不是掩盖它。
- **RECHECK-3（minor）`docs/zh-CN/plans/phase-04.md`：STORE-1 段记的「实测两种后果」第二条与实际 SQLite 行为不符。** 独立探针（modernc.org/sqlite v1.59.0，`sqlite_version()` = 3.53.4）：`SELECT length(CAST(x'3c803e' AS TEXT))` 返回 3（不是记录中的 2），建同样 CHECK 的表插入成功、落盘字节 `3c803e`；`x'3cfffe3e'` 同样 length 4、插入成功；对照项合法多字节 `x'e4b8ade69687'` length 2、被 CHECK 拒绝。即「事务中途被 CHECK 拒绝」这一后果并未发生。修复本身（补 `utf8.ValidString`）依然正确且必要，只是理由应是「表约束挡不住非法 UTF-8」。已按实测改写 STORE-1 段的该句并注明原记录有误。
- **RECHECK-4（nit）`internal/mail/imap/watcher.go`：`drain` 的 `maxLocal` 统计的是本次调用内累计的本地失败次数，不是注释所说的「连续失败」。** `local` 声明在循环之外，成功分支只调 `r.local.reset()` 复位退避、从不把 `local` 归零。按代码判定属实（未写运行探针：构造多批次且失败交错的场景需要 Junk 中放入超过单批上限的邮件，成本高于收益）。当前行为偏保守——每轮 Junk 的本地失败总数有上界——比「连续」更稳妥，因此不改行为，只把 `watcher.go` 中 `drain` 与 `Run` 的注释、以及 MAIL-1 段第③点的措辞由「连续失败」改为「本轮累计失败（成功只复位退避、不清零计数）」，使文字与代码一致。本条为纯措辞改动，没有新增测试。
- **RECHECK-5（经复现升为可修）`internal/mail/smtp/smtp.go`：MAIL-3 的补救只检查 `ctx.Err()`，漏了看门狗计时器到期。** 看门狗的 `run` 对 `timer.C` 到期与 `ctx.Done()` 做的是同一件事（`closeConn()` 后退出），因此「末尾分块仍在库的 4 KiB 缓冲里、连接已被关闭而写入仍返回 nil」这条推理对计时器到期同样成立。复查员未能构造稳定用例，本次构造成功：新增 `TestSendBodyTimedOut`，用 `writeChunk` 钩子等到写正文这一步的 `Command` 期限（300 ms）到期、看门狗关闭连接后再让该分块写入返回 nil。修法与 MAIL-3 同形：在提交一步之前，`ctx` 检查之后再查 `w.expired.Load()`，为真时返回 `ErrNotSent` 且文本为 `body timed out`（与 `fail` 的超时分类一致）。用例断言错误满足 `ErrNotSent`、文本以 `: body timed out` 结尾、服务器从未收到结束标记。去掉这次检查的变异被杀死：错误变为 `smtp: delivery outcome is uncertain: submission timed out`，正是复查员预测的误判。
- **RECHECK-6（nit）`docs/zh-CN/plans/phase-04.md`：记录的总覆盖率与本机不符。** 复查员在与 `HEAD` 一致的副本上两次独立运行都稳定得到 93.5095%（2795/2989），而记录写的是 93.4426%。本次在本条与其余各条修完之后重跑 `make check`，实测 93.5160%（2798/2992，分母增加来自本次新增的三条语句）。已在原记录处注明该数字随环境变化、只有 80% 的门槛是固定要求。其余分项（`tests/live` 90.8%、`internal/cli` 95.6%、`internal/store/sqlite` 87.6%、`internal/security/keychain` 100%）本次复测一致。
- **RECHECK-7（nit）TEST-02 与 TEST-06 至 TEST-10 的处理结论仍待提出者补正文。** 复查员确认原记录诚实，但同样拿不到这 6 条的正文，无法核对原问题与处理方式；也确认本次自行补的覆盖缺口（`TestAnalyzeFromRole`）有效，去掉白名单分支的变异被杀死。本次仍无法改变这一状态：仓库与工作副本中依然不存在审查发现清单，因此不把「无法判断」改写成「已确认无需处理」——那会把没做过的确认写成做过的。合并前由提出者附上这 6 条正文、维护者逐条确认「不修」的结论；按任务规则 nit 不阻塞合并。仍未覆盖的位置与原记录一致：`sizeBucket` 的各区间、`encodingOf` 的 8bit/plain 分支、状态与样本文件读写的 I/O 失败分支。

验证（复查后重跑）：`make check`（含中文注释检查、`-race`、staticcheck 与覆盖率门槛，总覆盖率约 93.5%，逐次运行会变，只有 80% 门槛是固定要求，详见下段 RECHECK2-2）、`GOOS=windows go vet ./...`、`GOOS=linux go vet ./...`、`CGO_ENABLED=0 go test -count=1 ./...`、`make secrets`、`git diff --check` 均通过。另跑了两处针对性验证：`go test -race -count=3` 跑 `TestWatcherDelivers`、`TestWatcherJunkFolder` 与 `TestWatcherDegradesJunkAfterRepeatedFailures`，以及上述四个变异各自被对应用例杀死。本次未运行的项如实记录：没有以 `-tags live` 运行 `tests/live` 的真机探测。本次修正不涉及钥匙串与网络：没有读写、创建或删除任何真实钥匙串条目，没有登录真实邮箱、连接 `imap.qq.com` 或 `smtp.qq.com`、发信；测试只在 macOS 上运行，Linux 上的测试由 CI 运行。

**第二轮复查后修正：** ① RECHECK2-1（minor）：上一轮只排除了连接层失败（`ErrClosed`），`junkOff` 仍然没有复位路径，超时或暂时性的 `NO [UNAVAILABLE]` 累计三次后，本次 Run 余下时间照样永久停扫 Junk，被误判为垃圾邮件的回复会被静默丢弃。现改为在同一连接上定期重新 LIST 时清零 `junkFails` 与 `junkOff`（`watcher.go` 的 relist 分支），恢复上限为 `relistInterval`（1 小时）；登录后的首次 LIST 不复位，避免「Junk 的失败拆掉连接、重连后再试」形成重连风暴——把复位放进 `list()` 时 `untagged BAD` 子用例立即失败，因为每次重连都会清零计数、永远达不到降级。新增 `TestWatcherRetriesJunkAfterRetryWindow`：Junk 的 EXAMINE 先被持续拒绝而降级，故障消失后下一次重新 LIST 解除降级、Junk 被交付且不重新登录；`-race -count=3` 稳定，去掉复位一行的变异使它失败（等待 Junk 交付超时）。② RECHECK2-2（nit）：覆盖率逐次运行会变（同一机器上先后测得 93.4426%、93.5095%、93.5160%、93.5225%），因此本清单不再把它当作确定值记录：门槛是 `covercheck` 的 80%，实测约 93.5%，具体数字以每次 `make check` 的输出为准。

**第三轮复查后修正：** ① R2-1-RESIDUAL（minor）：上一轮把复位挂在 `serve` 循环的 relist 分支上，而 `listedAt` 是 `serve` 的局部变量，每次重连都从零开始计时，登录后的首次 LIST 按设计又不复位；于是连接活不到 `relistInterval`（生产值 1 小时）时 relist 分支永远轮不到，`junkOff` 仍然没有复位路径，后果与原问题一字不差。复查员的探针（每条连接约 80 ms 即被切断）实测 29 次重连、29 次 LIST，Junk 始终未被重新补扫。现改为时间闸门：`runner` 增加 `junkRetryAt`，降级时置为 `now+relistInterval`，`serve` 每轮判断 `junkOff && !now.Before(junkRetryAt)` 即清零 `junkFails` 并恢复补扫；重连不再清零计时，重试仍被闸门压在每 `relistInterval` 至多一簇（`junkFailLimit` 次），不形成命令或重连风暴。新增 `TestWatcherRetriesJunkOnReconnectingLink`（IDLE 每次被切断，LIST 次数等于 LOGIN 次数即从未定期重新 LIST，Junk 仍在闸门到期后交付）与 `TestWatcherResetsJunkBudget`（Junk 持续不可用时每个窗口都重新失败满 `junkFailLimit` 次），`-race -count=3` 稳定；变异「把闸门改回 relist 分支」使 `TestWatcherRetriesJunkOnReconnectingLink` 失败（另两个恢复用例跑在稳定长连接上，对闸门落在哪一处不敏感，因此钉住落点的只有这一个用例），变异「只解除 `junkOff`、不清零 `junkFails`」使额度用例失败（4 次 vs 期望 6 次）。② COMMENT-STALE（nit）：`drainJunk` 注释中「junkOff 没有复位路径」一句与新增的闸门矛盾，已改写。③ DOC-COVERAGE（nit）：上一段裸写的覆盖率确定值已按同一口径加注。④ TEST-JUNKFAILS（nit）：由 `TestWatcherResetsJunkBudget` 覆盖，已不再是缺口。

**远端 CI 后修正：** PR 的 `quality (ubuntu-24.04)` 失败，`quality (macos-15)` 通过：`internal/mail/smtp` 的三个用例依赖内核 socket 缓冲大小与读取时机，两个平台行为不同（Task 10 的实施说明已写明这两个用例「未在本机验证，由 CI 运行」）。Linux 上的表现是：`TestSendSlowReaderWithinChunkDeadlines` 报 body 超时（每块 64 KiB 被分成更多次读取，停顿累计超过 Command）；`TestSendCanceled/after end of data` 收到 451 而不是取消（服务端读裸连接判断「客户端已关闭」时抢在取消之前发出了响应）；`TestSendBodyWriteStall` 等不到服务端会话结束（会话是否结束取决于缓冲中残留数据与 RST 的时机）。三处都改为与平台无关的写法，不改产品代码：慢速读取用例把停顿降到 2 ms、期限提到 1 s，使一块无论分几次读完都远在期限内、整封仍远超；`dataBlock` 与 `dataStall` 两条假服务器路径改为先等用例显式放行，再分别读走剩余 DATA 或判断连接关闭，断言由「会话结束」改为「连接已关闭」。第一次修正后 Linux 仍失败，据 CI 日志再改两处：`readPaced` 改为按累计读走的字节数停顿（单次读到多少字节取决于内核缓冲与 TLS 记录大小，按读取次数停顿会让同一封正文的耗时相差数十倍），慢速读取用例相应改为每块停顿 60 ms、期限 300 ms、正文 1 MiB（16 块），一块一次停顿远在期限内而整封远超；`dataStall` 放行后改为直接读裸连接到 EOF 来判断客户端已关闭（客户端中途关闭在各平台上有时报错、有时只是 EOF，读 DATA 流判断不可靠）。第二次修正后 Linux 仍失败，说明调内核缓冲大小这条路在两个平台上都不稳（缓冲越小 Linux 吞吐越低，缓冲越大 macOS 又不再阻塞）。最终改为不依赖内核：慢速读取用例改用包内 `writeChunk` 钩子按写入字节数制造延迟（每 64 KiB 停顿 100 ms，期限 500 ms，正文 1 MiB），一块一次停顿远在期限内、整封合计约 1.6 s 远超期限；`dataStall` 的连接关闭改由监听器的记录器在下一次读取时观察（`recordingConn.Read` 读到 EOF 即发信号），不再读 DATA 流或裸连接。第三次修正后 Linux 上只剩两处同类问题，都出在「服务端判断客户端已关闭连接」上：`rcptBlock` 与 `dataBlock` 一样改为先等用例放行再判断，避免服务器抢在取消之前发出 451；`TestSendBodyWriteStall` 不再断言服务端观察到连接关闭（客户端在 DATA 中途关闭时缓冲里还压着几 MiB 未读数据，服务端看到 EOF、读错误还是 RST 各平台不同），该用例只断言客户端的超时与分类，「超时即关闭连接」由 `TestSendCanceled` 在服务端可靠观察到的时点断言。本机 `-race -count=3` 通过；变异「把分块大小改成整封」使慢速读取用例失败（body 超时），说明「按块计时」仍被钉住。Linux 由 CI 复验。

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
| 准备 | `turncourier init` | 授权码由维护者在本机终端不回显输入；同时在真实终端上复核 `ReadSecret` 的终端路径（Task 12 已在 macOS 伪终端上自动覆盖） |
| 1 | `TURNCOURIER_LIVE=1 TURNCOURIER_LIVE_OUT=<目录> go test -count=1 -tags live -run TestL1Capabilities -v ./tests/live/` | 另记录 INBOX、Junk、Sent Messages 的 UIDVALIDITY（只作记录，见 Task 6 的文件夹列）；下列各步同样带 `-count=1` |
| 2 | 同上，`-run TestL1SendNotification`；可加 `TURNCOURIER_LIVE_CC_BOT=1` | 覆盖 D4 实际投递 ID 的三个候选来源：DATA 响应、「已发送」副本、抄送给机器人自己的副本，三者的 ID 都写入 `state.json`，第 3 步的样本按相等关系判断回复的线程头等于哪一个；发送成功同时说明 QQ 接受 EHLO 名 `localhost` |
| 3 | 维护者用各客户端回复后运行 `-run TestL1Replies` | 同时用别名地址与大小写变体回信，`from_case_variant` 复核 Phase 3 的小写化假设 |
| 4 | `-run TestL1Idle`；可加 `TURNCOURIER_LIVE_IDLE_SELF_SEND=1` | 结果用于确认或调整 Task 11 的 `IdleMax`、退避与 `Poll` 数值。邮件数不变时 UID SEARCH 的响应是否带 EXISTS 由预检会话记录，UID FETCH 的检查需加 `TURNCOURIER_LIVE_IDLE_SELF_SEND=1`；两项检查各在独立会话中以 10 秒期限调用 `Idle`，结果单独记录；测量会话的全部返回计入推送统计 |
| 5 | `-run TestL1KeychainRoundTrip`，加 `TURNCOURIER_LIVE_KEEP=1`；自动回复与退信样本由 `TestL1Replies` 归类 | 须维护者另行授权。沙箱读取检查只对合成条目执行，从不针对真实 account：条目保留期间，在 Codex 与 Claude Code 的沙箱中分别执行 `security find-generic-password -s io.github.chaorookie.turncourier -a <合成 account>`（只读属性）与 `security find-generic-password -s io.github.chaorookie.turncourier -a <合成 account> -w \| shasum -a 256`（读取数据，只输出哈希），把哈希与 `/dev/tty` 上显示的值比对：两边都是「值加结尾换行」的 SHA-256，只比较十六进制摘要，忽略 `shasum` 输出末尾的 ` -`。不加 `-w` 时 `security` 只读取属性、不读取机密数据，无法回答 D3 的问题。结果按「能读出数据」「只能读属性」「被拒」三类记录 |

L1 的结论（能力、实际投递 ID 的来源、各客户端的线程头与引用格式、IDLE 数值、沙箱读取结果）写入本清单新增的「L1 结果」一节；`samples.jsonl` 不进仓库，经维护者审阅后按 4b 的要求转为合成回归样本。

## L1 结果（2026-09-20）

机器人账户为专用 QQ 邮箱，接收地址同为 QQ 邮箱。规程第 1–5 步已全部执行。样本文件保存在仓库之外的 0700 目录，不进仓库。

### 第 1 步：能力与文件夹

- **CAPABILITY（登录后）：** `CHILDREN COMPRESS=DEFLATE ID IDLE IMAP4 IMAP4REV1 MOVE NAMESPACE UIDPLUS XAPPLEPUSHSERVICE XLIST`。`internal/mail/imap` 实际只要求 `IMAP4REV1`（且未公告 `LOGINDISABLED`），调用 `Idle` 时要求 `IDLE`；两者都在列。`MOVE`、`UIDPLUS`、`ID` 有公告但本包不使用（只读访问，不 MOVE、不 EXPUNGE、不 APPEND，也不发 `ID` 命令），只作记录。
- **文件夹：** `其他文件夹 | INBOX | Sent Messages | Drafts | Deleted Messages | Junk`。代码中使用的 `INBOX`、`Junk`、`Sent Messages` 三个名称都存在。
- **UIDVALIDITY：** INBOX、Junk、Sent Messages 三者取值相同。这证实迁移 `0002` 把文件夹并入 UID 唯一键是必需的：只用 `(UIDVALIDITY, UID)` 会跨文件夹撞键。
- **邮件数：** 第 1 步时机器人的 INBOX、Junk、Sent Messages 都是 0 封。这是读懂第 2、3 步游标变化的前提。

### 第 2 步：发信与实际投递 ID

发出一封合成通知，同时抄送机器人自己。SMTP 发送成功，说明 QQ 接受 EHLO 名 `localhost`。

| 候选来源 | 结果 |
| --- | --- |
| DATA 的 250 响应 | 响应文本为 `OK: queued as.`，**不含 ID**，不可用 |
| 「已发送」副本 | **可用。** Message-ID 被 QQ 改写为 `tencent_…@qq.com` 形式，与我方 ID 不同；`X-OQ-MSGID` 存在且等于我方 Message-ID |
| 抄送机器人自己的副本 | 60 秒内未出现在 INBOX。探测只查了 INBOX，没查 Junk，而机器人的 Junk 在此期间多了一封（见下），因此不能断定副本不存在；本轮不采用这条来源 |

**结论：** `RecordDeliveredMessageID` 的来源固定为「已发送」副本，按 `X-OQ-MSGID` 对应回我方 ID。

**两条与垃圾箱有关的事实，来源不同，不要混为一谈：**

1. **机器人自己的 Junk 里多了一封**（实测）。第 1 步时机器人 Junk 为 0 封，第 3 步结束时 Junk 游标已推进到 UID 1，而该邮件未被 `Analyze` 判为回复样本。这封很可能就是抄送给机器人自己的那份副本。**这是 Task 11 必须补扫 Junk 的实测依据**：机器人要在自己的 Junk 里找回复。
2. **通知落进了收件人的垃圾箱**（维护者在本机人工观察，**不在 samples.jsonl 中**；探测工具只登录机器人邮箱，看不到收件人那侧）。这只影响送达率，与上一条无关，不能用来论证机器人端的 Junk 补扫。4b 的送达率工作从这里开始。

### 第 3 步：回复样本（2 份）

两份都来自白名单内的接收地址，客户端标识（`X-Mailer` 或 `User-Agent`，样本只保留合并后的值）均为 `QQMail 2.x`，都落在机器人的 INBOX。

**下表中「App」与「网页版」的归属由维护者操作时记录，不在样本字段里**：两份样本的客户端标识相同，无法从数据本身区分。

| 特征 | QQ 邮箱 App | QQ 邮箱网页版 |
| --- | --- | --- |
| `In-Reply-To` 与 `References` | 都等于「已发送」副本的 ID | 同左 |
| 主题前缀 | `Re:` | `回复：` |
| 主题标签 `[TC …]` | 原样保留 | 原样保留 |
| 主题编码 | utf-8/B | utf-8/B |
| 正文结构 | multipart/alternative，plain 与 html 均 utf-8/base64 | 同左 |
| 引用原文 | 有，中文「原始邮件」分隔符；HTML 用 QQ 自有标记，**不用 blockquote** | **完全没有** |
| 正文中的令牌 | 1 个，与发出的一致 | **0 个** |

两份样本都**没有 `Received` 头，也没有 `Authentication-Results`**（同域投递）。发件人地址没有大小写变体；别名地址尚未测试。

另外两项实测，4b 会直接用到：

- **自动回复判定信号全为阴性：** 两份真人回复的 `Auto-Submitted`、`X-Autoreply`、`Precedence` 都为空或 false，`Return-Path` 不为 `<>`。这是 4b 自动回复过滤器的基线——真人回复不带任何这些信号。（`Return-Path` **是否存在、取值为何**没有记录，只记了是否为空信封；补采样本时要一并记下：它是同域投递中唯一可能不由发信方书写的来源线索。）
- **页脚令牌行在引用块中没有引用前缀**（`token_line_prefix` 为空）。4b 的引用剥离与令牌扫描规则要按这个形态写。

**结论：** 线程头与主题标签在两个客户端都可靠，正文令牌不可靠。D4 的令牌载体据此改为主题标签。主题前缀至少有 `Re:` 与 `回复：` 两种（成因未确定：可能是客户端差异，也可能是对方「回复/转发时主题」的设置），解析器不能按前缀白名单匹配，只能按 `[TC …]` 标签本身定位。

### 第 4 步：IDLE（30 分钟一轮，含自发邮件）

样本中的 `elapsed_ns` 与 `round_trip_ns` 是纳秒（`time.Duration` 的 JSON 编码）。schema/2 里这两个字段误名为 `_ms`，本轮数据即按 `_ms` 写出，读旧样本时要按纳秒解释；工具已改名并升到 schema/3。

- **服务器在 30 分钟内没有断开连接。** 测量会话只有两个事件：14.4 秒时一次 `EXISTS`（自发邮件触发），1800.0 秒时是探测自己的期限到点。QQ 的实际保持时间 ≥30 分钟，本轮没有测到上界。
- **IDLE 推送延迟约 1.4 秒**（自发邮件从 250 响应到收到 `EXISTS`）。远小于 `IdleAck`（30 秒）。
- **邮件数不变时，`UID SEARCH` 与 `UID FETCH` 的响应都不附带 `* N EXISTS`。** 预检会话与 FETCH 检查会话各以 10 秒期限独立调用 `Idle`，两次都是无新邮件、`returned` 为假、到期结束，邮件数与 `UIDNEXT` 前后不变（往返分别约 54 毫秒与 144 毫秒），结论为 `none`。Task 11 担心的「补扫自身触发 EXISTS、导致连续补扫」在 QQ 上不成立；Step 5 里「`Scan` 在 EXAMINE 之前排空 EXISTS 通道」的实现仍然保留，它防的是补扫进行中真有新邮件到达的情况。

**参数决定（按规程，L1 测得的数值与调整都要写进本清单）：** `IdleMax` 维持 5 分钟，不上调到接近 29 分钟。理由：`IdleMax` 到期不重新登录，只是结束本次 IDLE 并补扫一轮，代价是每 5 分钟一次 `UID SEARCH`，这是对「推送丢失」的兜底；而退避的复位条件之一是「连接自登录起健康达到 `IdleMax`」，上调会让故障后的退避恢复同比变慢。`IdleAck`、`Poll` 与退避参数同样维持不变。

### 第 5 步：Keychain 往返与 Agent 沙箱读取

在登录钥匙串中建一个合成条目（随机 account 与随机值，`service` 与生产相同），检查结束后删除；全程不读写任何真实 account。

- **往返正常：** 写入、读回、删除、删除后 `Get` 返回 `ErrNotFound`，全部符合封装的约定。
- **重复添加的退出码是 45**（对已存在的条目执行不带 `-U` 的 `add-generic-password`），原值不变。封装此前只记过 44（找不到）与 36（不允许交互）；45 由封装的「先 `Get` 再写入」路径挡在前面，不影响行为，但应补进 `internal/security/keychain` 的退出码注释。
- **两个 Agent 的沙箱都能静默读出机密数据。** 条目保留期间，分别在 Claude Code 与 Codex 的 shell 中执行 `security find-generic-password -s <service> -a <合成 account>`（只读属性，退出 0）与同一命令加 `-w` 并管道给 `shasum -a 256`（读取数据，只输出哈希）。两边算出的哈希都与 `/dev/tty` 上显示的值逐字相同，**没有授权弹窗、没有拒绝、没有任何提示**。

**结论：** D3 的威胁模型从推断变为实测。「同一用户下的进程能读出授权码与两把密钥」这一条，对本项目最相关的两个 Agent 都已证实成立；`design.md` 与 `SECURITY.md` 中「只要 Agent 的沙箱放行」的条件式表述据此改为已证实。alpha 仍按 D3 接受这一风险，缓解措施不变：使用专用机器人邮箱，怀疑泄露即在 QQ 邮箱停用授权码。

### 第 3 步补采：假期自动回复（2026-09-21）

在接收邮箱上开启 QQ 的假期自动回复，重发一封合成通知，自动回复回到机器人 INBOX 后取样。

**四项头部信号一个都没有：** `Auto-Submitted` 不存在、`X-Autoreply` 不存在、`Precedence` 不存在、`Return-Path` 不是空信封。原定的过滤表对这封信**全部落空**。

**而它通过了入站校验的每一项：** 发件人是白名单内的接收地址；`In-Reply-To` 等于「已发送」副本的 ID；主题标签原样保留（新格式下令牌随之回到机器人手里）。按 D4 冻结前的规则，它会被当作一次真人回复进入 Agent 会话，并因此成环：通知 → 自动回复 → 新回合 → 新通知 → 自动回复。

与真人回复的可区分之处只有三点，按可靠性排序：

1. **主题前缀为 `自动回复: `**，标签跟在其后（实际主题形如 `自动回复: [TC <任务 ID>] …`）。这是唯一足够可靠的判据，已据此破例立规，见「已定的实现细节」。实现时须从原始头部核对冒号是全角还是半角，探测工具出于脱敏只把未知前缀记为 `other`。
2. **只有 `text/html`，没有 `text/plain` 部分**；真人回复是 `multipart/alternative` 两部分俱全。弱信号，只作记录，不承重。
3. **`References` 为空，只有 `In-Reply-To`**；真人回复两者都有。弱信号：清单已记录 Foxmail 可能完全不带 `In-Reply-To`，说明线程头的缺失模式因客户端而异，不能反推。

三项都不作为独立的拒绝依据；承重的是主题前缀规则加回环刹车。

### 第 3 步补采：退信（2026-09-21）

先把收件地址临时指向同域不存在的地址，**QQ 在提交时就以 `550` 同步拒绝，根本不产生退信报文**：`smtp: message was rejected: smtp: submission rejected with 550`。这条路径第一次在真服务器上验证了 `ErrRejected` 与 `*ReplyError` 的分类。产品中最常见的「收件地址打错且同域」因此不会产生 DSN，而是在发信时就失败。

改为外域的不存在地址（RFC 2606 的保留域）后 QQ 收下并异步退信，DSN 回到机器人 INBOX：

| 特征 | 退信 | 对比：假期自动回复 |
| --- | --- | --- |
| `Content-Type` | `multipart/report`（子部件为 `text/html` 与 `application/octet-stream`） | 单个 `text/html` |
| `Auto-Submitted` | **`auto-generated`** | 不存在 |
| `Return-Path: <>` | 否 | 否 |
| 主题标签 `[TC …]` | **不含**，主题是退信自己的文字 | 原样保留 |
| `In-Reply-To` / `References` | **都指向「已发送」副本的 ID** | 只有 `In-Reply-To` |
| 发件人 | 非白名单，且**含大写字母** | 白名单内的接收地址 |

**结论：**

- **退信不是威胁**，它有两条互相独立的强信号（`multipart/report` 与 `Auto-Submitted: auto-generated`），而且既没有主题标签也没有令牌，即使过滤漏判也过不了令牌校验。原定的退信过滤规则不需要修改。危险的只有自动回复。
- **DSN 带着指向我方投递 ID 的线程头**，所以按线程绑定它会命中对应的通知。4b 据此可以把退信关联回具体通知并转为「明确未送达」，但必须先判定为退信、走 `RecordRejection`，不能进入回复流程。
- 原信是以 `application/octet-stream` 而非 `message/rfc822` 附上的，解析器不要假定后者。
- **首次观察到发件人地址含大写字母**（退信发件人）。Phase 3 的地址小写化假设在这里直接生效，规范化必须覆盖这类地址；真人回复的大小写变体仍未测。

### 待补

- ~~假期自动回复与退信样本~~：两类均已于 2026-09-21 采到，见上文两节。
- 第 4 步的上界：本轮只测到「≥30 分钟不断开」，没有测到 QQ 实际的 IDLE 保持上限；如果以后要上调 `IdleMax`，须先用更长的一轮测出上界。
- 别名地址与**真人回复**的大小写变体样本；同一轮记下正常回复的 `Return-Path` 是否存在、取值为何。退信发件人已观察到大写字母，但那不能替代真人回复的测试。
- Foxmail、Apple Mail、Gmail 的回复样本；本轮只采到 QQ 邮箱自家的两个客户端。
- 新主题格式（含令牌）的往返复核，见 D4「尚未验证」。**复核前必须先改探测工具**：`tests/live/sample.go` 的 `Notification.Token` 与 `ComposeNotification` 仍把令牌写在正文页脚，`subjectInfo` 的 `TagIntact` 按 `[TC <任务 ID>]` 精确匹配（`sample.go` 中），对新格式一律判为不完整——不先改，复核会对每一封回复谎报「标签被破坏」。探测工具已按下文「L1b」一节修改（2026-09-24），复核本身仍待维护者在本机执行。

## L1b：补采样本与新主题格式复核

L1 余下的真机工作合为一轮，称为 L1b。其中**新主题格式的往返复核**决定 4b Task 1 的主题解析器能否冻结；别名与大小写变体、`Return-Path` 与其他客户端的样本影响白名单与解析器的细节，结果出来后按需修订。L1b 不阻塞 4b 其他任务的实现，但 4b 的 PR 合并之前必须全部完成（见 4b「门槛」）。IDLE 上界只在打算上调 `IdleMax` 时才需要测，本轮不做。

### 探测工具的修改

**Files:**
- Modify: `tests/live/sample.go`、`tests/live/sample_test.go`（不带构建标签）、`tests/live/probe_test.go`（live 标签）

修改只涉及探测工具，不改产品代码，也不新增依赖。

1. **令牌进入主题标签。** 新增纯函数 `SubjectTag(taskID, tokenText string) string`：`tokenText` 非空时返回 `[TC <任务 ID> <令牌>]`，为空时返回旧形态 `[TC <任务 ID>]`（只用于 IDLE 自发邮件，它不写入 `state.json`）。`TestL1SendNotification` 的每封通知都用新形态，页脚照旧保留一份令牌（与 D4 一致：页脚只是给人看的副本）。`Mail` 增加 `SubjectToken bool`（`json:"subject_token,omitempty"`），新发出的邮件记为 true；第一轮留下的 `state.json` 没有这个字段，读入后为 false，按旧形态 `[TC <任务 ID>]` 归类，因此复用旧输出目录也不会误判（仍建议为 L1b 新建输出目录，见下文规程）。
2. **主题的编码方式与 4b 渲染器约定的一致。** `Notification` 的 `Subject` 字段拆成 `Tag`（原样放在主题开头的 ASCII 标签）与 `Title`（标签之后的文字）。`ComposeNotification` 把主题头的原始取值写成 `Tag`，`Title` 非空时再接一个空格与 `mime.BEncoding.Encode("utf-8", Title)`（纯 ASCII 的 `Title` 原样保留），不再对整个主题调用 `SetSubject`。这样标签以原始 ASCII 出现在头部、从不进入 encoded-word，而 `Subject: ` 加 64 个字符的标签共 73 个字符，go-message 按 76 列折行时只会在标签之后的空格处折行，标签本身不会被折断。`Tag` 必须符合 `SubjectTag` 输出的形状（`^\[TC [字母表]{10}( [字母表]{48})?\]$`，字母表为小写 Crockford base32），否则返回不回显输入的错误。4b 的渲染器按同一结构生成主题（4b Task 5），L1b 复核的正是这一结构。
3. **主题归类。** `Subject` 保留 `prefix`、`tag_intact`、`encoding`，另增 `tag_state` 与 `tag_count`，仍然不输出主题中的任何其他文字：
   - `tag_count`：解码后的主题中，D4 严格文法 `\[TC [字母表]{10} [字母表]{48}\]`（只接受小写，任务 ID 与令牌之间恰好一个 U+0020）不重叠命中的次数。
   - `tag_state` 按以下顺序取第一个成立的值；「期望标签」对每封已记录的邮件取 `SubjectTag(任务 ID, SubjectToken ? 令牌 : "")`，对全部已记录邮件逐一比较：
     1. `missing`：主题中没有 `[TC`（ASCII 不区分大小写）；此时 `prefix` 为 null。
     2. `multiple`：`tag_count` ≥ 2（D4：命中两次及以上一律拒绝）。
     3. `intact`：某封邮件的期望标签按字节原样出现在主题中。
     4. `case_changed`：按 ASCII 不区分大小写能找到某个期望标签。
     5. `whitespace_changed`：主题与期望标签都删去全部空白（`unicode.IsSpace`，含全角空格）后，按 ASCII 不区分大小写能找到——对应折行插入、替换或删除了标签内的空白。
     6. `truncated`：在删去空白、转小写后的主题中，从某个 `[tc` 起的剩余部分与某个期望标签（同样处理）的公共前缀至少覆盖 `[tc` 加完整的任务 ID（删去空白后二者相连，共 13 个字节）、短于整个期望标签，且公共前缀之后是主题末尾；或者公共前缀之后的那个字符不是 `]`、不是 ASCII 字母或数字（例如省略号），并且期望标签没有在它之后接上。「接上」有三种：跳过这个字符，其后紧接着期望标签的其余部分（插入了零宽空格、软连字符、`-` 等）；跳过它，其后紧接着期望标签去掉一个字符后的其余部分（替换了一个字符）；它之后任意位置出现期望标签的最后 8 个字节，即令牌的最后 7 个字符加 `]`（插入或替换了多个字符）。接上的属于改写而不是截断，归入 `other`。只差 `]`（令牌完整）时另有规定：分岔处是省略号一类的字符（`…`、`⋯`、`.`）才算截断；全角括号、圆括号，或 `]` 被删去而紧接着标题文字，都是改写。仍有的局限：分岔落在期望标签的最后 8 个字节之内、又插入或替换了多个字符时，仍记为 `truncated`。
     7. `other`：其余情况（有 `[TC`，但与任何期望标签都对不上）。
   - `tag_intact` 当且仅当 `tag_state` 为 `intact`。
4. **自动回复前缀。** `prefix` 的白名单改为按 ASCII 不区分大小写匹配（输出来信中的原文，至多 20 个字符），条目为 `回复：`、`回复:`、`答复：`、`答复:`、`转发：`、`转发:`、`Re:`、`Fwd:`、`FW:`，再加自动回复前缀 `自动回复：`、`自动回复:`、`自動回覆：`、`自動回覆:`、`自动答复：`、`自动答复:`、`Auto-Reply:`、`AutoReply:`、`AutomaticReply:`（前缀先删去空白再比较，`Automatic reply:` 因此对应最后一项）；英文条目另有全角冒号的变体（`Re：`、`Fwd：`、`FW：`、`Auto-Reply：`、`AutoReply：`、`AutomaticReply：`），与产品规则「全角半角冒号都宽容」一致。这样假期自动回复的冒号是全角还是半角会直接出现在样本里，不必再回头看原始头部。`classifyKind` 增加与产品规则相同的一条：解码后的主题删去空白、转小写后以上述任一自动回复前缀开头，即记为 `auto`；`Auto` 增加 `subject_prefix`（布尔）记录这一信号。第一轮的假期自动回复只有 `text/html`，原有的「正文以 QQ 自动回复的固定开头起始」只查纯文本，对它不起作用，这一条补上了探测工具自己的漏判。
5. **`Return-Path`。** 样本增加 `return_path`：`count`（`Return-Path` 头的个数）、`empty`（第一个取值是否为 `<>`）、`role`（第一个地址的角色，规则同 `from_role`；没有该头或为空信封时为空串，无法解析时为 `other`）、`matches_from`（规范化后与 `From` 地址相同）、`same_domain_as_from`（域名相同，不区分大小写）。仍然只输出角色与布尔值，不输出地址。原有的 `auto.return_path_empty` 保留。
6. **标题长度与 4b 通知相近。** `sendOne` 的标题约 40 个字符、含中文（「TurnCourier L1b 探测 k/n：新主题格式复核，请用不同客户端直接回复本邮件」），B 编码后被拆成多个 encoded-word，复核因此覆盖 4b 通知（标题至多 60 个字符）会遇到的折行，而不只是一个短标题。
7. **逐封确认不回显标签。** 新增纯函数 `SendPrompt(bot, recipient string, index, total int, title string, ccBot bool) string` 渲染逐封确认的文本，签名里没有标签参数，含令牌的标签因此无从进入 `/dev/tty`；文案注明「主题标签含一次性令牌，不回显」。`sendOne` 改用它。
8. **样本格式升为 `turncourier-l1/4`。**
9. **被拒邮件的「已发送」副本（核实 D7 的前提，2026-09-24 审查后增补）：** `smtp.Send` 以 `ErrRejected` 失败时，探测在让用例失败之前，仍以发信前记下的游标只读地在「已发送」中查找这封邮件的副本，结果写入发信样本的 `sent_copy_after_reject`。副本的对照记录（`sent_copy`、`sent_copy_after_reject` 与 `cc_copy`）都带 `searched`：补扫正常结束（找到副本，或等满窗口、每次补扫都成功而一直没有找到）时为 true；补扫出错或探测被取消时为 false，此时 `found: false` 不说明副本不存在。

**离线测试（`sample_test.go`，先写并确认失败）：**

- `SubjectTag` 的两种形态；`ComposeNotification`：原始头部中出现完整的 `Subject: <标签>` 一行（标签未被折断），解码后的主题等于 `标签 + " " + Title`；`Title` 足够长、会被拆成多个 encoded-word 时同样成立；`Title` 为空时主题就是标签；形状不对的 `Tag`（含 CR/LF、大写、长度不对）被拒绝且错误中不含输入；页脚仍含令牌。
- 主题归类：七种 `tag_state` 各有一个用例，另有 `tag_count` 为 0、1、2 的用例与旧形态（`SubjectToken` 为 false）的 `intact`；每个用例都断言样本中不出现主题里的金丝雀文字。
- 自动回复：只有 `text/html`、主题以 `自动回复: ` 开头的来信 `kind` 为 `auto`、`auto.subject_prefix` 为 true、`prefix` 为 `自动回复:`；全角冒号的变体 `prefix` 为 `自动回复：`；大小写不同的 `AUTO-REPLY:` 同样命中；真人回复的 `subject_prefix` 为 false。
- `Return-Path`：没有该头、`<>`、与 `From` 相同（含大小写不同）、同域不同地址、异域地址、无法解析的取值各一条；样本中不出现这些地址。
- `SendPrompt` 含序号、地址与 `Title`，不含任何 48 个字母表字符的串。
- 变异测试：逐条删去 `tag_state` 的判定分支、把严格文法改为不区分大小写、去掉 `classifyKind` 的主题前缀规则、`matches_from` 改为区分大小写，都应被上述用例拦下。

**验证：** `go test -count=1 ./tests/live/`、`make check`、`make secrets`、`GOOS=linux go vet ./...`、`GOOS=windows go vet ./...`；`go vet -tags live ./tests/live/` 与 `staticcheck -tags live ./tests/live/`（均含在 `make check` 中）。修改期间不运行任何真机探测。

**实施说明（2026-09-24）：** 由独立子代理按上述契约实现。先写离线测试，失败基线为编译失败：`SubjectTag`、`SendPrompt`、`ErrInvalidTag`、`ReturnPath`、`Mail.SubjectToken`、`Subject.TagState` 与 `TagCount`、`Auto.SubjectPrefix`、`Notification.Tag` 与 `Title` 都未定义。

- **新增用例：**
  - `TestSubjectTag`；
  - `TestComposeNotificationSubjects`：长标题拆成多个 encoded-word、纯 ASCII 标题、空标题、旧形态标签；
  - `TestComposeNotificationRejectsBadTag`：9 种形状；
  - `TestSendPrompt`；
  - `TestAnalyzeTagStates`：22 例，七种状态都有，`tag_count` 取 0、1、2 的都有；
  - `TestAnalyzeAutoReplySubject`：7 例；
  - `TestAnalyzeReturnPath`：9 例。

  测试中的主题用标准库（`net/mail` 与 `mime.WordDecoder`）独立解码，不经过被测代码所用的 go-message。
- **变异测试：** 契约列出的 10 个变异与另外 18 个全部被杀死。另外 18 个涉及：前缀与大小写（白名单区分大小写、用 `strings.ToLower` 代替只折叠 ASCII——被开尔文符号的用例拦下、自动回复前缀改为在任意位置匹配）；截断规则（下限改为 14、公共前缀之后是字母表字符或 `]` 也算截断）；期望标签（忽略 `SubjectToken`、只比较第一封邮件、不跳过没有任务 ID 的记录）；其他判定（`missing` 区分大小写、为前缀查找 `[TC` 时区分大小写、域名比较区分大小写、只删去 ASCII 空格、`return_path` 忽略空信封）；主题渲染（不校验标签、标题不做 B 编码、整个主题交给 `SetSubject`）。「对已编码的主题调用 `SetSubject`」是等价变异：该主题是纯 ASCII，`SetSubject` 不改动它；改用真正的旧行为（把原始标题交给 `SetSubject`）后被杀死。
- **按字面解释或补充的地方：**
  1. 截断的下限按删去空白后的形式计为 13 个字节（`[tc` 加 10 位任务 ID）。契约初稿误写为 14，已更正。
  2. 标签形状不对时返回新导出的哨兵错误 `ErrInvalidTag`，文本固定。
  3. 没有任务 ID 的记录不产生期望标签，`[TC ]` 这样的残片不会因它被判为完整。
  4. `missing` 在未删去空白的解码主题上判定，`[ TC …` 因此记为 `missing`。
  5. `Return-Path` 存在但取值为空白时，`role` 为 `other`，`empty` 为 false。
  6. 另把 `sendOne` 的标题加长到约 40 个字符（第 6 项）。
- **读 L1b 样本时注意：** 引用原文的回复（例如 QQ 邮箱 App）在引用头块的「主题:」行里也带着令牌，所以 `plain.tokens` 与 `html.tokens` 预计为 2。`token_line_prefix` 描述的是先出现的那一行，也就是主题行，会记为 `other`，不再对应页脚的令牌行。这不影响 4b，因为正文中的令牌永不回读。
- **验证：** 以下均通过：
  - `go test -count=1 ./tests/live/`，不带标签部分覆盖率 92.2%；
  - `make check`，总覆盖率 93.36%；
  - `make secrets`；
  - `GOOS=linux go vet ./...` 与 `GOOS=windows go vet ./...`；
  - `go vet -tags live ./tests/live/`；
  - `env -u TURNCOURIER_LIVE go test -count=1 -tags live -v ./tests/live/`：输出「跳过」并通过。

  没有运行任何真机探测。

**审查后的修正（L1b 探测工具，2026-09-24）：** 一名独立的质量审查子代理复核了上述实现：没有「主要」问题，脱敏成立（新字段只来自固定词表、角色与布尔值，白名单前缀只能输出白名单字节），主题头的布局经实测确认，没有 panic 路径，各扫描都是线性的。确认并修复的问题如下，修复同样先写失败的用例：

1. **截断的误判。** 在标签中间插入一个字母表之外的字符（零宽空格、软连字符、`-`）或把一个令牌字符换成 `o`，原规则都记为 `truncated`，而 D4 要求把截断与改写区分开。规则改为第 3 项第 6 条现在的写法：分岔处是 `]`、ASCII 字母或数字时是改写；其余字符只在期望标签没有在它之后接续（既不是插入也不是替换）时才算截断。另有一处边角由主会话决定：令牌完整、只是 `]` 被省略号取代时，「替换后接续」的部分为空，检查恒成立，按字面会记为 `other`；这正是客户端恰好在 `]` 之前截断主题的样子，因此替换检查只在其后还剩期望标签的内容时进行，记为 `truncated`（去掉这道条件的变异被用例拦下）。
2. **测试缺口。** 审查员另拟的 15 个变异体中 14 个存活。现在都有用例拦截：严格文法中「恰好一个空格」、退信先于主题前缀、没有标签或前缀之后是其他文字的自动回复、诱饵 `[tcp]` 之后的标签（原样、截断、大小写改变）、标签形状的 `^` 锚、`Return-Path` 的角色比较与最后一个 `@`、只看第一个 `Return-Path`、逐条的白名单条目、`Schema` 字面值、三个标签、字母表之外的字母、以及只折叠 ASCII 时的字节对齐（前缀中含小写后变长的字符时，错误的实现会越界 panic）。
3. **全角冒号。** 英文条目补上全角冒号的变体，自动回复前缀补上「自动答复」，与产品规则一致（第 4 项）。
4. **注释与效率。** 修正几处与行为不符的注释（`Return-Path` 的说明改为：它由最终投递的服务器按信封发件人写入；QQ 要求信封发件人等于登录账户，同域投递中它可能反映真实的发信账户，而 `From` 可以伪造，这一点待 L1b 核实）；白名单的折叠形式在包初始化时算好一次，每封来信的主题只折叠、去空白一次（在 30 万个随机输入上比较新旧实现，结果完全相同）。
5. **增补（核实 D7 的前提）：** 第 9 项。

修复后用例数：`TestAnalyzeTagStates` 38、`TestAnalyzeAutoReplySubject` 19、`TestAnalyzeSubjects` 21、`TestAnalyzeReturnPath` 12、`TestComposeNotificationRejectsBadTag` 10，新增 `TestAnalyzeBounceBeforeAutoReplySubject` 与 `TestSchemaVersion`。变异测试共 57 个，全部被杀死：审查员列出的 M1–M15 与 M17，另加 M4b、M15′、M17′ 三个变体，共 19 个；新截断规则的 8 个；重构的 5 个；逐条删去白名单的 24 个条目；主会话为 `]` 边角加的 1 个。`sample.go` 每次恢复后与原文件的 SHA-256 相同。验证：`go test -count=1 -cover ./tests/live/`（不带标签部分覆盖率 92.3%）、`make check`（总覆盖率 93.38%）、`make secrets`、`go vet -tags live ./tests/live/`、`GOOS=linux go vet ./...`、`GOOS=windows go vet ./...` 通过，三个文件中没有零宽、双向控制与其他不可见字符。仍然没有运行任何真机探测。

**第二轮复查（L1b 探测工具，2026-09-24）：** 同一名质量审查子代理复核了上述修正：修正都成立，没有「主要」与「次要」问题。截断的误判已消除，136 万次模糊执行没有 panic；它此前的 15 个变异与新拟的 12 个全部被杀死；在 40 万个对抗性随机主题上，重构前后的结果完全相同；拒收之后查找副本的路径不泄漏任何地址或 ID。余下三条「细节」，都已修复，同样先写失败的用例：

1. **`]` 处的例外过宽。** 令牌完整而 `]` 换成任何非字母数字字符都记为 `truncated`，包括输入法打出的全角括号 `］`、`】` 与 `)`，而理由只适用于省略号。现在只有 `…`、`⋯`、`.` 算截断，其余是改写（第 3 项第 6 条）。
2. **多个字符的改写仍记为 `truncated`**：插入两个零宽空格、零宽空格加软连字符、令牌中间插入 `...`、两个字符换成全角字母。现在分岔之后若出现期望标签的最后 8 个字节，就判为改写；分岔落在这 8 个字节之内的多字符改写仍记为截断，作为局限写入契约。
3. **`found: false` 有歧义。** 「已发送」补扫出错、探测被取消与确实没有副本，此前都记为 `found: false`，补扫出错会被误读为支持 D7 前提的证据。现在副本记录带 `searched`（第 9 项）。

修复后 `TestAnalyzeTagStates` 为 52 例（新增 14 例）。本轮 18 个变异全部被用例的失败拦下：新规则的 12 个（`]` 处的各字符、末尾比对、比对长度、检查顺序、逐字符的插入与替换检查），以及审查员针对 `cutAt` 的 6 个变异在新代码上重跑。其中「只解码一个字节」起初只是编译失败，改成能编译的写法后重跑，照样被拦下。`sample.go` 每次恢复后 SHA-256 不变；三个文件仍没有不可见字符与替换字符（测试以 `\u` 转义构造它们）。验证：`go test -count=1 -cover ./tests/live/`（不带标签部分覆盖率 92.4%）、`make check`（总覆盖率 93.39%）、`make secrets`、`go vet -tags live ./tests/live/`、`GOOS=linux go vet ./...`、`GOOS=windows go vet ./...` 均通过。仍然没有运行任何真机探测。

### 规程（维护者本机执行）

前提同 L1：`turncourier init` 已完成，命令在仓库根目录执行。共同前缀与 L1 相同：`TURNCOURIER_LIVE=1 TURNCOURIER_LIVE_OUT=<目录>`，再加 `go test -count=1 -tags live -run <用例> -v ./tests/live/`。

1. **新建输出目录**（`mkdir -m 700 <目录>`，仓库之外）。复用第一轮的目录也能正确归类，但新目录让 L1b 的样本与第一轮分开，便于审阅；新目录没有游标，`TestL1Replies` 第一次会从 UID 1 起补扫机器人邮箱，只输出引用了本轮探测邮件的来信。
2. `-run TestL1SendNotification`：发出 1 封新格式通知（逐封确认时只显示标签之后的文字）。
3. 在接收邮箱中回复这封通知：
   - **必做：** QQ 邮箱 App 与网页版各回复一次（D4 要求的两个客户端）；
   - **别名：** 若接收账户有别名（英文别名或 foxmail.com 别名），以别名作为发件人再回复一次。**不要**预先把别名加入 `allowed_senders`：样本的 `from_role` 为 `recipient` 说明 QQ 把发件人改写回主地址，为 `other` 说明别名原样保留（那样的回复会被产品的白名单拒绝，用户须自己把别名加入 `allowed_senders`）；别名含大写字母时 `from_case_variant` 同时记下大小写是否保留；
   - **可选：** 平时实际使用 Foxmail、Apple Mail 或 Gmail 收取这个邮箱的，各回复一次。
   - **可选（核实 D7 的前提）：** 把 `recipient.address` 临时改为同域不存在的地址再发一封。QQ 会在提交时以 550 拒绝（L1 已观察到），探测随即检查「已发送」中有没有这封邮件的副本，写入样本的 `sent_copy_after_reject`。D7 推断被拒的邮件没有副本：`searched: true` 且 `found: false` 支持这一推断，`searched: false` 说明补扫没有完成、不能下结论；若 `found: true`，D7 需要重新考虑。做完把地址改回来。
   - **可选（假期自动回复的频率）：** 若愿意再开一次接收邮箱的假期自动回复，连发两封通知，看 QQ 是每封都回复还是对同一发件人只回复一次。这只帮助估计漏判时环转得多快，不影响规则（主题前缀规则已能识别它）。做完记得关闭。
4. `-run TestL1Replies`：取回并归类全部回复。
5. 把结论写进本清单新增的「L1b 结果」一节：每份样本的 `tag_state`、`tag_count`、`prefix`、`encoding`、`return_path`、`from_role` 与 `from_case_variant`，以及它们对 4b 的影响（见 4b「门槛」）。`samples.jsonl` 仍不进仓库。

## 4b 与 4a 的衔接

4b 的范围见上文「范围」一节，L1 之后细化为任务契约。4a 为它留下的接口与前置条件：

- **单实例锁（Phase 3 已定的前置条件）：** 收发循环所在的进程必须先取得单实例锁，锁覆盖 `RecoverInFlight`、`RecoverSendingNotifications`、全部派发与发送。
- **装配：** 新建 `internal/app`，把 `imap.Watcher` 的 `Handle` 接到解析器、入站验证与 `RecordReply`/`RecordRejection`（`InboundReply.Folder` 与 `Rejection.Folder` 取自 `Batch.Folder`），一批全部提交后再 `AdvanceCursor`；把发送循环接到 `ClaimNextNotification` → 渲染 → `token.Issue`（Claims 取自通知行）→ `smtp.Send` → 按 Task 10 的映射更新状态。进程启动时从 Keychain 读取授权码与两把密钥，失败即不启动收发与派发；每把密钥读出后用 `keychain.KeyCheck` 与 `KeyCheckOf` 比对，不符即以「Keychain 中的密钥与登记不符」拒绝启动。授权码读出后缓存在进程内存中，`imap.Watcher.Password` 与发送循环都使用缓存值，只在 IMAP 或 SMTP 认证失败后、或用户在本地操作后重新读取 Keychain，避免钥匙串锁定或 ACL 变化时每次重连都弹窗。Keychain 读取超时或返回 `ErrInteractionNotAllowed` 时报告为独立的 `keychain_unavailable` 状态，不计入连接失败。`imap.Watcher` 的 `Password` 回调重新读取 Keychain 失败时返回错误，Watcher 随之发出 `credentials_unavailable`；装配层把它与启动时、发送循环中的 Keychain 读取失败一并报告为 `keychain_unavailable`。两者是同一情况在两层的名称：`internal/mail/imap` 不知道凭据来自 Keychain，所以用通用名称。（2026-09-24：4b 任务契约有两处例外——认证熔断期间的 `credentials_unavailable` 报告为 `auth_circuit_open`（4b Task 11）；回复被延后时只把游标推进到被延后的那封之前，而不是整批提交后一次推进（D6、4b Task 9）。）
- **认证熔断：** SMTP 返回 `ErrAuth` 或 IMAP 返回 `ErrAuthFailed` 时，打开一个两端共享的熔断，持续时间不少于 `AuthPause`（15 分钟，下限 10 分钟）；熔断期间 IMAP 不登录，领取到的通知直接 `RequeueNotification` 到熔断结束之后，不尝试 SMTP AUTH。已确认的「认证失败后至少暂停 10 分钟」由此同时约束发送端。4b 为此补测试。
- **SMTP 结果的分流顺序：** 认证 5xx 的错误链同时含 `ErrNotSent`、`ErrAuth` 与 `*ReplyError`，所以 4b 必须先判断 `ErrAuth`（打开认证熔断、通知重新排队），再按 `ReplyError` 的 4xx/5xx 分流；先判断通用的 5xx 规则会把认证失败误当成邮件被拒而放弃通知。参数校验失败同样返回不附 `ReplyError` 的 `ErrNotSent`，与暂时性失败无法区分，因此 4b 在调用 `Send` 之前自行校验信封与邮件大小（例如渲染后超过 `MaxMessageSize`），不合法的通知直接放弃，不交给 `Send` 后再按「暂时性失败」无限重新排队。问候与 EHLO 被拒绝时不附 `ReplyError`（连接级失败，不代表这封邮件被拒），同样按暂时性失败处理。
- **令牌验证（4b 的初步建议，L1 后定稿）：** 以下一条无论 L1 结果如何都成立：令牌的 kid 必须等于通知行的 `TokenKeyID`，不等即拒绝，否则持有旧密钥的一方可以为新密钥下发的通知签出可用令牌。另一条同样无条件成立：**该 nid 必须是所属任务最新一条已发出通知的 nid**，否则以 `token_superseded` 拒绝（D4 已确认）。其余顺序为初步建议，但有一处已按 D4 固定：**自动回复与退信的判定排在最前，先于 `token.Parse`**，命中即硬拒、只记元数据（理由见 D4）。此后为：`token.Parse` → `NotificationByNID` → kid 与 `TokenKeyID` 相等 → `KeyStateOf(token, kid)` → 用同一 kid 的 `BodyDigest` 计算摘要 → 按 (账户, Message-ID) 查找已有记录，Message-ID、摘要与任务都一致即判为重复并忽略（不写被拒记录）→ `token.Verify` → 任务状态 → `RecordReply`。去重排在有效期与任务状态之前，是为了让 UIDVALIDITY 变化后的全量补扫把早已处理的回复识别为重复，而不是误报为 `token_expired` 或 `task_closed`；为此 4b 需新增按 Message-ID 查找入站记录的存储接口，并补「过期后重扫」的集成测试。retired 密钥签发的令牌是否仍接受、有效期按本地处理时间还是 IMAP INTERNALDATE 判断，列在「风险与后续」，L1 后在 4b 定稿。线程头与主题标签的规则已按 L1 结果在 D4 中冻结：令牌由主题标签携带并且是唯一的验证输入，正文页脚只是给人看的副本、永不回读；同一封信里出现多个 `[TC …]` 标签一律拒绝（D4 的主题标签文法）。任何放宽按 D4 另行确认。（2026-09-24：完整顺序已在「4b 任务契约」Task 9 中定稿，两处未定项见「4b 已定的实现细节」。）
- **实际投递 ID：** 按 L1 选定的来源——「已发送」副本，按 `X-OQ-MSGID` 对应——写入 `RecordDeliveredMessageID`，并新增按实际投递 ID 查找通知的存储接口。发信后须读一次「已发送」才能拿到该 ID，这一步要计入发送循环。（2026-09-24：4b 任务契约按 D6 改为由收取循环在每一轮最先补扫「已发送」、发信后唤醒它；副本按我方写入的 `X-TurnCourier-ID` 对应，缺失时退回 `X-OQ-MSGID`，见 4b Task 6、10。）
- **通知内容：** `NewNotification.Content` 的格式、确定性敏感信息过滤与代码块裁剪由 4b 的渲染器定义；频率、合并与退避参数在 4b 定。（2026-09-24：见 4b Task 5、D8 与「4b 已定的实现细节」中的「通知不合并」。）
- **解析器：** gbk、gb2312、cp936 等字符集标签映射到 GB18030 在 `internal/mail/parser` 中集中管理并测试。
- **init：** 加入设计要求的环境诊断（复用 `doctor` 的检查）与真实往返测试。
- **文档：** design.md 中依赖 QQ 是否改写 Message-ID 的表述已按 L1 结果修订：QQ 确实改写，线程绑定以「已发送」副本中的 ID 为准。

## 4b 任务契约

> **执行说明：** 与 4a 相同：按任务顺序执行，每个任务先写失败的测试，再做最小实现，通过后提交；每个任务由独立子代理实现，再由规格与质量两名审查子代理复核（含变异测试），确认的问题修复并经独立复查后才进入下一任务；阶段末做整阶段审查（Task 18）。实现在新的特性分支进行，步骤使用 `- [ ]` 勾选跟踪。「先写失败的测试」有三类例外，各任务中另有说明：守卫测试（`tests/docs` 中扫描源码与文档的测试）所守护的约束在写下时已经成立的，写下时就应通过，以临时引入违规来确认它有效（Task 1 的 `reveal_test.go`）；约束要到本任务才成立的照常先失败（Task 17 的 `notices_test.go`：`THIRD_PARTY_NOTICES.md` 尚不存在）；集成测试（Task 14）写在全部实现之后；只由人工执行的 live 文件（Task 16）只能编译检查。
>
> **编号：** 本节中的「Task N」都指 4b 的任务；引用 4a 的任务时写作「4a Task N」。
>
> **状态：** 本节于 2026-09-24 起草，经两名独立审查子代理审查与一名子代理的三次复查逐轮修订；最后一次复查确认此前的问题全部修复，只余两处措辞，已改。下文「4b 须确认的决策」中的 D6–D8 已于同日由维护者确认，均采用建议方案（含 D8 对 D4 字面的那一处解释），实现按任务顺序开始；其余细节在已确认决策与 `design.md` 的范围内定下，列在「4b 已定的实现细节」。L1b 的新主题格式复核是 Task 1 冻结主题解析器的门槛（见「门槛」）。

**Goal:** 让 TurnCourier 用专用 QQ 机器人邮箱安全地完成「通知 → 回复 → 验证 → 入队 → 派发」的闭环：渲染并发出任务通知，在「已发送」中找回 QQ 实际投递的 ID，收取并逐项验证回复，把合法回复的新正文加密入队，再交给 Agent。本阶段结束时，用不调用模型的合成 Agent 在离线假服务器上走通闭环（`tests/e2e`），`init` 能做一次真实往返测试，维护者再用真实邮箱完成往返验收（L2）。

**4b 的交付：** 主题标签的构造与解析、存储补充、MIME 与引用解析器及合成回归样本、自动回复与退信判定、通知渲染与确定性敏感信息过滤、IMAP 的「已发送」补扫与唤醒、QQ 行为模拟器、单实例锁与启动时的密钥核对、入站验证流水线、「已发送」副本处理、发送循环与认证熔断、派发循环与最小 Agent 契约、`internal/app` 装配、离线闭环测试、`init` 的环境诊断与真实往返测试、L2 验收工具、文档与第三方许可声明。

**Architecture:** 新建 `internal/app` 负责装配与三个循环（收取、发送、派发），它是产品代码中唯一同时导入邮件与存储的包；`internal/mail/parser` 与 `internal/mail/renderer` 只处理字节，不依赖存储、配置与 `security/*`（渲染器经接口取得主题标签）。收发都在一个进程内、在单实例锁之下进行。网络、Keychain 与 Agent 都藏在小接口之后，CI 只用离线替身与 `tests/qqsim` 模拟器，真实邮箱只出现在人工执行的 `tests/live`。

**Tech Stack:** Go 1.27.1；标准库 `mime`、`net/mail`、`syscall`（flock）、`encoding/json`、`html`；D2 已接入的 go-imap/v2、go-smtp、go-message、x/text。**不新增任何模块。**

### 4b 须确认的决策

**2026-09-24 维护者已确认 D6–D8，均采用本节的建议方案；D8 中对 D4 字面的解释（M 超出时推迟发信并告警，不暂停任务）一并确认。** D6 与 D7 不放宽 D4 的验证规则：D6 改变的是回复可能被处理的时刻，D7 改变 UNCERTAIN 通知的解除方式，并把 D4「最新一条已发出通知」解释为包括 UNCERTAIN（比只看 SENT 更严）。D8 按 D4 取定 N 与 M、保留 D4 的「暂停」并新增一道 24 小时上限；其中有一处是对 D4 字面的解释而不是照搬（M 超出时推迟发信、不暂停任务），单独列出请维护者确认。

#### D6：「已发送」副本并入收取循环，回复凭证据才因缺少副本被拒

**问题。** 线程校验要求回复的 `In-Reply-To` 或 `References` 含该通知**实际投递的** Message-ID，而它只能从「已发送」副本中取得（L1 第 2 步）。「4b 与 4a 的衔接」原打算把「读一次已发送」放进发送循环：每发一封就另开一条 IMAP 连接补扫「已发送」。这样每封通知多一次 IMAP 登录，按 D8 的上限每小时可多出 40 次，而收取循环自己已把登录限制在每小时 12 次（4a Task 11 的退避参数），QQ 对频繁登录的风控阈值不公开。

**建议。**

1. **一条连接：** `imap.Watcher` 每一轮先补扫「Sent Messages」（只取头部），再补扫 INBOX 与 Junk。发送循环在 250 之后、以及通知进入 UNCERTAIN 之后唤醒 Watcher，使「已发送」在几秒内被补扫，不必等到 `IdleMax`。
2. **「已发送」是承重路径，不降级：** 它的补扫失败只让本轮跳过，下一轮照常重试，不像 Junk 那样在连续失败后停扫一小时；「已发送」不在 LIST 中同样按补扫失败处理；连续失败 3 轮起发出 `sent_unavailable` 告警。
3. **先已发送、后收件：** 同一轮中「已发送」排在 INBOX 与 Junk 之前。通知的副本在发信时就已存在，回复必然更晚到达，因此正常情况下处理回复时实际投递 ID 已经记下。
4. **延后而不是拒绝，拒绝要有证据：** 回复引用的通知还没有记下实际投递 ID 时——它已是 SENT 而副本尚未补扫到，或仍是 SENDING，或是提交阶段失去响应的 UNCERTAIN（D7 可能在下一轮凭副本把它核对为已送达）——把这封及其后的邮件留到下一轮（不推进游标，30 秒后唤醒一次）。只有拿到「副本确实不在」的证据才以 `thread_mismatch` 拒绝：某次**成功完成**的「已发送」补扫，其开始时刻已晚于通知最近一次状态变化（SENT 取 `sent_at`，其余取 `updated_at`）10 分钟以上，却仍没有找到副本；此时同时告警「「已发送」中找不到通知副本」（常见原因是 QQ 邮箱关闭了「保存到已发送」）。「已发送」一直补扫失败时拿不到证据，回复只继续延后，不被拒绝。回复完全没有线程头（`In-Reply-To` 与 `References` 都缺失）时延后没有意义，立即以 `thread_mismatch` 拒绝。

**代价。**

- 收取循环多扫一个文件夹（只取头部，每封约 1–2 KiB）；Watcher 的契约多出「唤醒」「延后」「补扫完成回调」与「跳过历史」四个概念（Task 6）。
- 被延后的一封会挡住其后到达的全部 INBOX 来信（所有任务都受影响），直到副本记下或证据出现；「已发送」无法访问时 INBOX 的处理停在那一封，只有告警。游标只有「最后 UID」一个值，表达不了「跳过这一封、先处理后面的」，这是换取简单与不丢信的代价。
- 极端情况下一封回复要晚 10 分钟以上才处理。

**不采用：** 发送循环另开连接（登录次数翻倍）；抄送机器人自己（L1 中抄送副本 60 秒内没有进 INBOX、疑似进了 Junk，不可靠）；按发信后的固定时长拒绝（「已发送」补扫失败的那段时间里，合法回复会被永久拒绝，而拒绝不可撤销）。

#### D7：凭「已发送」副本解除 UNCERTAIN；「最新通知」包括 UNCERTAIN

**问题。** SMTP 进入提交阶段后没有得到响应的通知记为 UNCERTAIN，按 4a 的约定「只能由本地核对解除」，`ResolveUncertainNotification` 的注释写着「系统从不自动调用它」。在 4b 之前这只是一个待核对的状态；4b 之后它还意味着：UNCERTAIN 的通知没有实际投递 ID（表约束只允许 SENT 记录它），用户对它的回复一律过不了线程校验。另一处牵连是 D4 的 `token_superseded`：「最新一条已发出通知」若只看 SENT，一条其实已经送达的 UNCERTAIN 通知不会让上一条通知的令牌失效，重放窗口比 D4 说的「下一条通知发出为止」更长。

**建议。**

1. 「已发送」中出现带我方 `X-TurnCourier-ID`（缺失时看 `X-OQ-MSGID`）、对应一条 UNCERTAIN 通知的副本时，把这份副本当作本地核对的证据：在**同一事务**中把通知核对为已送达并记下实际投递 ID（Task 2 的新接口），发出本地事件「已凭「已发送」副本确认送达」。找不到副本的 UNCERTAIN 仍然等人工核对，**从不自动重发**的约定不变。`ResolveUncertainNotification` 的注释随之改为「由本地核对调用；D7 的副本证据改走 `ResolveUncertainAsDelivered`」。核对为已送达时，`sent_at` 取该通知进入 UNCERTAIN 的时刻（它当时的 `updated_at`），不取核对时刻：通知的发出顺序因此不随核对而变（见第 2 条）。
2. `token_superseded` 判定中的「最新一条已发出通知」取该任务的 SENT 通知与**尚无缺失证据**的 UNCERTAIN 通知中发出时刻最晚的一条（SENT 取 `sent_at`，UNCERTAIN 取进入 UNCERTAIN 的时刻 `updated_at`，相同时取 id 较大者）。按发出时刻而不是 id：重新排队会推迟较早的通知，它可能在较新的通知之后才发出，用户最后看到的是它。UNCERTAIN 多半已经送达，算作「已发出」才符合 D4 的重放窗口；但「已发送」中一直没有它的副本时它多半没有送达，缺失证据成立后就不再计入。缺失证据与 D6 相同：某次**成功完成**的「已发送」补扫开始于它进入 UNCERTAIN 10 分钟之后，仍没有找到它的副本。**比令牌所属通知更新的只有尚无缺失证据的 UNCERTAIN 通知时，回复不拒绝而是延后**（机制同 D6：游标停在这封之前，30 秒后再判）：那条通知凭副本核对为已送达，回复即以 `token_superseded` 拒绝；缺失证据成立，回复被接受。拒绝同样要有证据：被拒不可撤销，而那条 UNCERTAIN 通知若没有送达，用户看到的最后一条正是上一条通知。代价：延后期间（通常几秒，至多十几分钟；「已发送」一直补扫失败时一直延后）其后到达的来信都被挡住，与 D6 的延后相同；它若其实送达了而 QQ 没有写入副本（例如关闭了「保存到已发送」），证据出现之后上一条通知的令牌重新有效，直到下一条通知发出或令牌过期——那种设置下回复本来就过不了线程校验（D6），实际影响很小。

**理由与未验证的前提。** 解除依据的是「QQ 只在接受邮件之后才把副本写入「已发送」」。这是推断：L1 只观察过发送成功时副本存在，没有观察过提交失败的情形；服务端为一封拒绝接收的邮件写入「已发送」没有道理。推断若错，后果是一条没有送达的通知被当作已送达的最新通知，用户回复上一条通知以 `token_superseded` 被拒，本地可见；不会引起重发，也不会让伪造的回复通过。解除的方向始终是「已送达」。

**不采用：** 维持纯人工核对（4b 没有常驻命令与核对界面，UNCERTAIN 的通知在 4b 中实际上无法解除，其回复永远被拒）；「最新」只看 SENT（窗口比 D4 长）；UNCERTAIN 无论有没有缺失证据都计入「最新」（4b 无法解除它，一条没有送达的 UNCERTAIN 通知会让该任务的邮件通道一直不通）。

#### D8：回环刹车与发信频率（保留 D4 的暂停）

D4 要求一道「与分类无关的回环刹车」：同一任务每小时至多接受 N 次邮件触发的回合，机器人每小时至多发出 M 封通知，超出即本地告警并暂停该任务的邮件触发；N 与 M 留给 4b。

**建议：**

- **任务级刹车（N）：** 同一任务在任意滚动 1 小时内至多接受 10 封回复入队，并且在任意滚动 24 小时内至多接受 40 封（按 `replies.created_at` 计，REJECTED 不计）。超出的那封以 `rate_limited` 拒绝，同时**持久地暂停**该任务的邮件触发：迁移 `0003` 新增 `mail_pauses` 表记下暂停，此后该任务的来信一律以 `mail_paused` 拒绝，直到本地解除；解除之后刹车只计此后接受的回复，否则同一窗口里的旧回复会让它立刻再次暂停。4b 提供暂停与解除的存储接口并测试；解除用的本地命令随常驻命令一起在后续阶段提供。
- **为什么要 24 小时上限：** 只按小时计，挡不住慢循环。Agent 每回合 6 分钟以上时，一个漏判的自动回复者每小时触发不到 10 个回合，按小时计的刹车永远不触发，环会一直转下去；每封合法的通知还会把它重新拉起来。按天计的上限保证这种环至多空转 40 个回合就停下。
- **为什么暂停要本地解除：** 环一旦出现就说明分类漏判，到点自动恢复只会让它再转起来。
- **发信上限（M）：** 全局在任意滚动 1 小时内至多发出 40 封通知（按进入 SENT 或 UNCERTAIN 的时刻计）。达到上限时发送循环不再领取，通知留在 PENDING，窗口腾出后照常发送，不丢弃，发出一次本地告警。
- 三个数都是 `internal/app` 中的常量，alpha 不提供配置项。

**理由。** 一个人与一个任务来回交互，Agent 每回合通常需要数分钟，每小时 10 封、每天 40 封已经很密。单个任务每 5 分钟一个回合约为每小时 12 封通知，两三个任务同时密集交互也在 40 封之内，所以 M 只会在异常时触发。

**请一并确认的解释：** D4 写的是「机器人每小时至多发出 M 封通知，超出即本地告警并暂停该任务的邮件触发」。M 是全局量，超出时没有一个对应的「该任务」可以暂停，而发信多并不说明有回环（回环已由任务级刹车约束），所以这里把 M 超出的处理解释为「推迟发信并告警」，不暂停任何任务。

### 4b 已定的实现细节（在已确认决策与 design.md 的范围内定下，不再逐项确认）

| 事项 | 决定 | 所在任务 |
| --- | --- | --- |
| 主题标签的类型 | `token.Tag`（任务 ID 与令牌），`String`/`Format`/`LogValue` 脱敏，只有 `Reveal` 返回 `[TC <任务 ID> <令牌>]`、`RevealToken` 返回令牌文本；两者只在渲染器写入主题与页脚时使用，`tests/docs` 以源码扫描钉住产品代码中引用这两个方法的位置（含方法值，不只是调用） | 1 |
| 标签文法与原因码 | 按 D4：对解码后的完整主题，`\[TC [字母表]{10} [字母表]{48}\]`（只接受小写，恰好一个 U+0020）不重叠命中恰好一次才接受。命中 0 次：主题中没有 `[TC`（ASCII 不区分大小写）为 `subject_tag_missing`，有 `[TC` 为 `subject_tag_damaged`（折行、截断、大小写被改都归此类，与令牌无效区分开）；命中 ≥2 次为 `subject_tag_multiple`；文法命中但版本字节或 kid 不合法为 `token_invalid`。L1b 若显示某个必测客户端改动了标签，按结果修订并经维护者确认（任何放宽按 D4 另行确认） | 1 |
| 主题的编码 | 标签以原始 ASCII 放在主题开头，其后一个空格，再接 `mime.BEncoding.Encode("utf-8", 标题)`（纯 ASCII 标题原样保留）。`Subject: ` 加 64 个字符的标签共 73 个字符，go-message 按 76 列折行时只在标签之后的空格处折行，标签不会进入 encoded-word，也不会被折断。标题至多 60 个字符，超出以 `…` 截断。L1b 用同一结构复核 | 1、5 |
| 验证顺序 | 见 Task 9 的列表：已有被拒记录的邮件直接跳过（批被重新交付或 UIDVALIDITY 重置后全量补扫时，拒绝不会被翻案）；非人工来信判定最先，先于解析标签与令牌；去重排在有效期、令牌新旧、暂停与任务状态之前 | 9 |
| 单封来信的错误 | 每封来信的处理结果只有三种：入队、判为重复、以原因码拒绝。只有基础设施错误——数据库忙或读写失败、正文密钥不可用、ctx 结束——让 `Handle` 返回错误、整批重试。来信内容引起的任何失败都映射为原因码（多为 `malformed`）：解析失败；字段不符合存储的校验（Message-ID 缺失、少于 3 或多于 998 个字符、非法 UTF-8 或含 NUL；新正文非法 UTF-8）。来信中的 Message-ID 与发件人先用存储导出的校验函数检查（Task 2），不合法的不作为查询参数，也不写入被拒记录（留空）；以来信内容为参数的查询若仍返回 `sqlite.ErrInvalidArgument`，按找不到处理。**其余存储错误一律是基础设施错误**：不能也按找不到处理，否则数据库的一次短暂失败会让「已发送」副本被忽略、游标照常前进，此后对该通知的回复因没有实际投递 ID 被 `thread_mismatch` 永久拒绝。每封的处理包在 `recover` 中，panic 记为 `malformed` 并发出 `internal_error` 事件。任何一封来信都不能让收取循环停下 | 9、10 |
| 首次运行的游标 | 文件夹没有持久化游标、且库中还没有任何通知时，Watcher 不补扫历史，直接把游标设在当前 UIDNEXT−1（Task 6 的 `SkipHistory`）：那时不可能有合法回复，不必为历史邮件（L1、L2 与往返测试留下的来信）逐封写被拒记录 | 6、13 |
| 令牌有效期的判定时刻 | 按本地处理时间，不取 IMAP INTERNALDATE（L1 没有核实 QQ 如何设置它，imap 包也不取它）。服务停机超过有效期期间到达的回复会以 `token_expired` 拒绝，本地可见；默认 72 小时足以跨过周末 | 9 |
| retired 密钥 | 4b 没有轮换命令，库中只有 active 的 kid 1。令牌的 kid 对应的密钥不是 active 时一律以 `token_invalid` 拒绝，轮换阶段再定过渡期 | 9 |
| 「最新通知」 | 按 D7：`token_superseded` 所比较的「最新一条已发出通知」取该任务的 SENT 通知与尚无缺失证据的 UNCERTAIN 通知中 `COALESCE(sent_at, updated_at)` 最晚的一条，相同时取 id 较大者；核对为已送达时（D7 的副本证据与本地人工核对都如此）`sent_at` 沿用进入 UNCERTAIN 的时刻，顺序不随核对而变。比令牌所属通知更新的只有尚无缺失证据的 UNCERTAIN 通知时，延后而不拒绝（Task 9 第 13 步）。按发出时刻而不是 id：重新排队会推迟较早通知的 `not_before`，它可能在较新的通知之后才发出，用户最后看到的是它 | 2、9 |
| 只处理纯文本正文 | 回复必须含 `text/plain` 部件（在 `multipart/alternative` 中取第一个，附件与 `Content-Disposition: attachment` 的部件不取）。只有 HTML 的来信以 `parse_uncertain` 拒绝：不做 HTML 转文本，也就不新增依赖；L1 的两个 QQ 客户端与常见客户端都发两个部件，只有 HTML 的实测样本只有自动回复。这是「解析不确定时不执行」的直接应用 | 3 |
| 引用与签名剥离 | 自上而下找第一条引用边界，边界及其后全部丢弃；边界规则见 Task 3。「写道：」「wrote:」这类引用头只有在其后第一个非空行以 `>` 开头时才算边界，免得截断用户自己写的「日志里写道：…」。`>` 引用块之后又出现非签名的文字（行内逐段回复）时以 `parse_uncertain` 拒绝，而不是只留引用之前的部分；没有引用头或分隔线的 `>` 行只有在其中出现我方页脚的标记行或 `[TC` 时才算引用我方通知，否则可能是用户自己用 `>` 标出的命令或引文，同样以 `parse_uncertain` 拒绝，不猜测。签名分隔线只认恰为 `-- ` 的行；单独的 `--` 只在其后至多 6 行就到正文末尾时才算。新正文中出现令牌形状的串（前后都不是字母表字符的恰好 48 个字母表字符，不区分大小写）、`[TC`（ASCII 不区分大小写）、我方页脚的标记行或页脚的固定句子，一律以 `parse_uncertain` 拒绝：说明引用没有剥净，令牌副本正要进入 Agent 会话 | 3 |
| 字符集 | gbk、gb2312、cp936、x-gbk、windows-936 映射到 GB18030 解码器，在 `internal/mail/parser` 的包初始化中注册（go-message/charset 的表是进程级的）；未知字符集、解码失败或解码后不是合法 UTF-8 的正文以 `malformed` 拒绝 | 3 |
| 非人工来信 | 沿用 4a「已定的实现细节」中的特征表：退信（顶层 `multipart/report`，或发件人本地部分为 MAILER-DAEMON、postmaster）先于自动回复判定；自动回复的主题前缀按「删去空白、ASCII 转小写后以 `自动回复`、`自動回覆`、`自动答复`、`auto-reply`、`autoreply`、`automaticreply` 之一开头，其后紧跟半角或全角冒号」匹配，只看主题开头（`Re: 自动回复:` 不算）；QQ 自动回复的固定开头同时在纯文本与 HTML 正文的前 512 字节中查找 | 4 |
| 通知内容格式 | `NewNotification.Content` 是 JSON：`{"v":1,"event":…,"task":…,"title":…,"body":…}`，严格解码（未知字段、版本不为 1 都拒绝），整体不超过 1 MiB（`renderer.MaxContentSize`，与 `payload.MaxPlaintext` 相等，由 `internal/app` 的测试核对：渲染器不能导入 `security/*`） | 5 |
| 敏感信息过滤 | 确定性规则，按顺序：① 去掉控制字符（保留换行与制表）与 Unicode 格式字符（双向控制、零宽字符）；② 围栏代码块（``` 或 ~~~，按 CommonMark 允许的缩进识别；没有闭合的围栏一直到正文末尾）整体换成「[代码块已省略：N 行]」；③ 进程持有的机密（授权码与两把密钥的文本）原样出现处，以及任何令牌形状的串（恰好 48 个字母表字符），换成「[已省略]」；④ URL：`file://` 链接整体按路径处理；其他 `scheme://` 链接去掉 userinfo（`user:pass@`），查询串与片段整体换成「[已省略]」（预签名链接的 `X-Amz-Signature`、OAuth 回调的 `access_token` 都在这里），保留主机与路径；⑤ 绝对路径（不属于 `scheme://host` 的 `/…/…`，包括 `PATH=/usr/bin:/Users/…` 中冒号之后的每一段与 `host:/path`；`~/…`；`X:\…`）换成「[路径已省略]」；⑥ 密钥形状：PEM 私钥块、已知令牌前缀、`Authorization:` 之后的取值，以及键名以 `password`、`passwd`、`secret`、`token`、`api_key`、`apikey`、`access_key`、`private_key`、`auth`、`credential`、`credentials`、`signature` 结尾（不区分大小写，如 `OPENAI_API_KEY`、`db_password`）的键值对的取值，换成「[已省略]」；⑦ 过滤后正文超过 4000 个字符即截断。内容被省略或截断时，页脚注明「部分内容已省略，完整输出留在本机（任务 <ID>）」——design.md 要求的「本地报告位置的相对描述」；4b 没有本地报告文件，只写任务 ID、不写路径。过滤不承诺发现所有机密，邮件与文档都如此注明 | 5 |
| 纯状态通知 | 新配置项 `notify.content`：`"filtered"`（默认，发过滤后的内容）或 `"status"`。`"status"` 时主题的标题只有事件名，正文只写事件与任务 ID：Agent 提供的标题与正文都不进入邮件（主题是暴露面最大的字段）。只接受这两个取值，环境变量不能覆盖 | 5 |
| 通知线程 | 一个任务的通知属于一个邮件线程：除第一封外，`References` 列出该任务此前已记下实际投递 ID 的通知（至多 20 个，按发出先后），`In-Reply-To` 取其中最后一个 | 5、11 |
| 通知不合并 | 同一任务积压多条通知（熔断、频率上限或断网之后）时全部发出（按 `(not_before, id)` 领取，重新排队的通知可能晚于较新的通知发出），只有最后发出的一条令牌有效（D4 的 `token_superseded`，「最新」按发出时刻）。合并需要新的放弃原因，而 `abandon_reason` 的取值由表约束固定，改它要重建通知表；alpha 不做，由 D8 的发信上限兜底 | 11 |
| 自定义头 | 每封通知带 `X-TurnCourier-ID: <我方 Message-ID>`，用于在「已发送」中对应回通知（D4：定位副本只能按我方 ID 或我方自定义头，禁止按主题检索）；该头不含 nid、任务 ID 或令牌 | 5、10 |
| 渲染大小 | 渲染后的邮件至多 256 KiB：远小于 SMTP 的 4 MiB 与 IMAP 取回正文的 2 MiB，「已发送」副本因此总能被取回。超出即放弃该通知，不交给 `Send` | 5、11 |
| 发信结果分流 | 按「4b 与 4a 的衔接」：先判 `ErrAuth`（打开认证熔断、通知重新排队到熔断结束之后），再按 `*ReplyError` 分 4xx 与 5xx，再判 `ErrUncertain`（UNCERTAIN，并唤醒 Watcher 以便 D7 尽早凭副本核对），其余 `ErrNotSent` 重新排队。4xx 重新排队。5xx 不在第一次就放弃：QQ 限流时可能用 4xx 也可能用 5xx，第一次就放弃会在限流期间逐条丢掉整个队列；通知的 `attempts` 达到 3 时才放弃（原因 `rejected`），此前按退避重新排队。RCPT 步骤的 5xx 另发出 `recipient_rejected` 告警，提示检查收件地址；提交阶段（结束标记之后）的 550 另发出 `submission_rejected` 告警，提示「收件地址或内容被拒」：L1 实测同域不存在的地址在提交阶段以 550 被拒，而 QQ 的内容过滤同样回 550。两个告警每条通知只发一次。4xx、5xx、`ErrUncertain` 与连接级失败之后，**整个发送循环**按退避暂停，而不只推迟这一封：不暂停就会对每条待发通知各登录一次，把整个队列耗在限流或断网上 | 11 |
| 本地的永久失败 | 通知内容无法解码、正文行缺失、密文无法解密（`ErrPayloadMissing`、包装 `payload.ErrDecrypt` 的错误）或渲染超过 256 KiB，都属于重试也不会好的本地失败：`AbandonNotification(rejected)` 并发出带通知 ID 的事件，不让一条坏数据阻塞全部发送（4b 没有人工放弃的命令）。`rejected` 是表约束允许的取值中最接近的一个（「永久失败、不再重试」），事件区分具体原因。只有 `ErrPayloadKeyUnavailable`（没有正文密钥或 kid 不符，属于配置问题）不放弃，只告警并等待 | 11 |
| 重新排队的退避 | 通知第 k 次重新排队推迟 min(2^(k−1) 分钟, 60 分钟)，k 为通知的 `attempts`。整个发送循环的暂停另按**全局的连续失败次数** c 计：min(2^(c−1) 分钟, 60 分钟)，250 之后清零，只在内存中（重启后从 0 开始）。不能按通知计：积压的通知 `attempts` 都是 1，暂停会一直是 1 分钟，持续限流时每小时登录约 60 次；按全局计，暂停依次为 1、2、4……60 分钟，持续限流时约每小时一次 | 11 |
| 认证熔断 | SMTP 的 `ErrAuth` 或 IMAP 的 `auth_failed` 打开两端共享的熔断，持续 15 分钟（`AuthPause`，下限 10 分钟）；熔断期间发送循环不领取，Watcher 的 `Password` 回调返回错误（Watcher 因此不登录，并在 `credentials_unavailable` 之后等待 `Backoff.Max`，所以熔断结束后 IMAP 至多再等 10 分钟才重新登录），熔断结束后重新读取 Keychain 中的授权码 | 8、11 |
| 授权码缓存 | 启动时读出后缓存在进程内存；只在认证失败后或熔断结束时重新读取 | 8 |
| 唤醒通道 | 容量为 1，发送方非阻塞写入（已有信号即丢弃）：Watcher 卡住时，发送循环与入站处理不会因唤醒它而阻塞 | 6、13 |
| 单实例锁 | 数据目录下的 `turncourier.lock`（0600，`O_NOFOLLOW`），`flock(LOCK_EX\|LOCK_NB)`；锁被占用返回 `ErrAlreadyRunning`。锁在 `RecoverInFlight`、`RecoverSendingNotifications` 与全部派发、发送之前取得，进程结束时释放；非 Unix 平台返回不支持 | 8 |
| 派发契约 | `app.Agent.Deliver(ctx, sessionID, body)`：返回 nil 表示 Agent 已确认收到；返回包装 `app.ErrNotDelivered` 的错误表示确定没有送达（放回队列）；其他错误表示结果不确定（UNCERTAIN，不自动重发）。Codex 与 Claude 适配器在后续阶段实现它，届时移入 `internal/agent` | 12 |
| 常驻命令 | 4b 不增加 `run`、`service` 等常驻命令：没有 Agent 适配器时，常驻的收发循环对用户没有用处。收发循环只在 `tests/e2e`、`init` 的真实往返测试与 L2 验收工具中运行；四个邮件模块经 `init` 进入产品二进制。解除邮件暂停（D8）、人工核对 UNCERTAIN 的命令也随常驻命令一起在后续阶段提供 | 13、15 |
| 本地事件 | `app.Event` 只含种类、任务 ID、通知 ID、回复序号、原因码、文件夹、计数与时长，不含地址、主题、正文、令牌与授权码；由调用方注入的回调接收。被拒来信的事件按原因码聚合，同一原因码每分钟至多一次（带计数），垃圾邮件突发不会刷屏。4b 不写日志文件 | 13 |
| 被拒记录的保留 | 被拒来信保留 30 天，运行中的 `App` 每天清理一次更早的记录（`PruneRejections` 单次至多删 10000 条，返回满额时继续删），表不会随垃圾邮件无限增长。「拒绝不翻案」在清理之后仍成立，靠的是 30 天不短于令牌有效期的上限（720 小时）：记录被清理时，其中的令牌都已过期，重新判定只会以 `token_expired` 或更早的原因码拒绝。改动这两个数中的任何一个，都要重新核对这一点 | 2、13 |
| 被拒原因码增补 | `subject_tag_missing`、`subject_tag_multiple`、`subject_tag_damaged`、`token_superseded`、`rate_limited`、`mail_paused`、`empty_body`；原有代码保留 | 2 |
| 解析器变更与摘要 | 键控摘要依赖解析器的确定性（「风险与后续」）。4b 冻结解析规则后不给摘要加版本：补扫中出现的 `message_conflict` 照常记录并告警（原回复早已处理，只是一条多余的拒绝记录）；以后修改解析规则的 PR 须评估这一点 | 3、9 |
| 送达率 | 通知落进收件人垃圾箱（L1 人工观察）是已知约束：主题中的随机令牌本身像垃圾邮件特征。4b 做三件事：通知只用标准头部与纯文本、HTML 两个部件，不带跟踪与外链；`init` 与文档提示用户把机器人地址加入通讯录或白名单；L2 验收逐封记录通知落在收件箱还是垃圾箱。改善送达率的其他办法（例如 DKIM 对齐的发信域）不在 alpha 范围 | 5、15、16 |

### 门槛

- **L1b（冻结主题解析器之前）：** 维护者按「L1b」一节的规程完成新主题格式复核。QQ 邮箱 App 与网页版的回复都为 `tag_state: intact`、`tag_count: 1` 时，Task 1 的文法按上表冻结；否则按结果修订 Task 1 并经维护者确认。Task 1 可以在 L1b 之前实现，结果若要求修订，在 4b 合并之前修订。L1b 的别名与 `Return-Path` 结果若要求白名单多一个比对点，作为 Task 9 的修订另行确认。4b 的其他任务不等 L1b，但 **4b 的 PR 合并之前 L1b 必须完成**（包括别名与大小写变体的样本）。
- **L2（4b 合并之后）：** 维护者用 Task 16 的工具以真实邮箱完成往返验收，结果写入本清单新增的「L2 结果」一节，与 4a 之后的 L1 相同。

### 4b 文件结构

```text
internal/
├── security/token/
│   ├── subject.go                   # 主题标签：Tag、NewTag、ParseSubject，脱敏输出
│   └── subject_test.go
├── mail/
│   ├── gateway.go                   # 邮件契约：页脚标记行、页脚固定句子与 X-TurnCourier-ID 头名（只有常量）
│   ├── imap/                        # 修改：FolderSent、ScanHeaders、Watcher 的「已发送」补扫、Wake、ErrDefer、Scanned、SkipHistory、Now
│   ├── parser/
│   │   ├── parser.go                # MIME 解析：头部解码、线程头、纯文本部件、大小与深度上限；Message 的脱敏输出
│   │   ├── charset.go               # GBK 系列标签映射到 GB18030
│   │   ├── quote.go                 # 引用与签名剥离、令牌与页脚残留检查
│   │   ├── classify.go              # 自动回复与退信判定
│   │   └── *_test.go                # 读取 tests/fixtures/mail 的合成样本模板；模糊测试
│   └── renderer/
│       ├── content.go               # 通知内容的 JSON 格式
│       ├── filter.go                # 确定性敏感信息过滤
│       ├── renderer.go              # MIME 组装：主题、线程头、自定义头、页脚
│       └── *_test.go
├── app/
│   ├── lock.go、lock_unix.go、lock_other.go  # 单实例锁
│   ├── secrets.go                   # 启动时读取授权码与两把密钥、KeyCheck 核对、授权码缓存
│   ├── inbound.go                   # 入站验证流水线（判定核心与存储无关，供 init 往返测试复用）
│   ├── sent.go                      # 「已发送」副本 → 实际投递 ID；凭副本解除 UNCERTAIN（D7）
│   ├── outbound.go、breaker.go       # 发送循环、结果分流、发信上限、认证熔断
│   ├── dispatch.go、agent.go         # 派发循环、最小 Agent 契约、任务事件 → 通知
│   ├── app.go、events.go             # 装配、启动恢复、生命周期、本地事件与聚合
│   ├── roundtrip.go                 # init 的真实往返测试
│   └── *_test.go
├── store/sqlite/                    # 修改：迁移 0003（邮件暂停、被拒记录的 Message-ID 索引）；查询、计数、原子核对、校验哨兵、被拒记录的清理
│   └── migrations/0003_mail_pause.sql
└── cli/                             # 修改：init 增加环境诊断与真实往返测试
tests/
├── fixtures/mail/                   # 合成回归样本模板（令牌与任务 ID 用占位符，测试运行时替换并按声明的传输编码组装）
├── qqsim/                           # QQ 行为模拟器：隐式 TLS 的 SMTP 与 IMAP、改写 Message-ID、写入「已发送」、故障注入
├── e2e/                             # 合成 Agent 的离线闭环
└── live/                            # 增加 L2 验收（live 标签）
THIRD_PARTY_NOTICES.md               # 产品二进制链接的第三方模块及许可证
```

同时修改：`internal/config`（`notify.content`）、`configs/turncourier.example.toml`、`internal/config/write.go`（init 渲染的配置）、`tests/docs`（依赖方向、`Reveal` 的使用位置、第三方许可声明的覆盖）、两份依赖表（`docs/en/architecture.md` 与 `docs/zh-CN/development.md` 的「使用方」一栏，由 `tests/docs/deps_test.go` 核对），以及 Task 17 列出的文档。`Makefile` 不需要新目标：`make test` 已覆盖 `tests/...`。

### 依赖方向（4b 结束时）

```text
cmd/turncourier ─► internal/cli ─► internal/doctor
                        ├──────► internal/config
                        ├──────► internal/store/sqlite
                        ├──────► internal/security/keychain
                        ├──────► internal/app ─► internal/mail/{smtp,imap,parser,renderer}
                        │                  ├───► internal/store/sqlite、internal/config
                        │                  ├───► internal/security/{token,payload,keychain}
                        │                  └───► internal/task、internal/queue
                        └──────► golang.org/x/term

internal/mail/parser    ─► internal/mail（gateway 常量）、go-message、go-message/mail、go-message/charset、x/text
internal/mail/renderer  ─► internal/mail（gateway 常量）、go-message、go-message/mail（经接口取得标签，不导入 security/token）
tests/qqsim             ─► go-imap/v2（imapserver、imapmemserver）、go-smtp（服务端）、go-sasl（服务端认证）
tests/e2e               ─► internal/app、tests/qqsim、store/sqlite、config、task
tests/live（L2，live 标签） ─► internal/app、store/sqlite、security/keychain（只读取授权码）
```

- `internal/mail/*` 仍不依赖存储、配置与 `security/*`；`store/sqlite` 仍不依赖 `token` 与 `config`。
- 产品代码中同时导入邮件与存储的包只有 `internal/app`；`tests/docs` 的检查由「没有」改为「只有 `internal/app`」。
- `tests/qqsim` 不是测试文件，`tests/docs/deps_test.go` 会扫描它：两份依赖表中 go-imap/v2、go-smtp、go-sasl 的「使用方」一栏都要点名 `tests/qqsim`（Task 7）。
- 产品二进制从本阶段起链接四个邮件模块与 x/text（经 `internal/cli` → `internal/app`），约 16.5 MB（arm64，D2 的估计）。`THIRD_PARTY_NOTICES.md` 在同一 PR 中补齐。

---

### Task 1：主题标签（`internal/security/token`）

**Files:**
- Create: `internal/security/token/subject.go`、`internal/security/token/subject_test.go`、`tests/docs/reveal_test.go`
- Modify: `internal/security/token/token.go`（`Reveal` 的契约注释）

**契约：**

- `Tag` 保存任务 ID 与 `Token`，零值不可用。`NewTag(taskID string, t Token) (Tag, error)`：任务 ID 须为 10 个字母表字符，令牌须来自 `Issue` 或 `Parse`（kid 不为 0），否则返回 `ErrInvalidTag`。
- `(Tag) TaskID() string`、`(Tag) Token() Token`；`(Tag) Reveal() string` 返回 `[TC <任务 ID> <令牌>]`（64 个字符）；`(Tag) RevealToken() string` 返回 48 个字符的令牌文本。两者的契约注释写明「只用于渲染器写入主题与页脚副本」。
- `String`、`Format`（`%T`、`%p` 之外的全部动词；`%w` 同样由 fmt 先于 `Format` 处理，见本任务「审查后的修正」第 4 条）与 `LogValue` 输出固定的 `[redacted subject tag]`，与 `Token` 的做法一致；`Tag` 作为其他包结构体的未导出字段时同样有原始字节泄漏的可能，这一点照 4a 的「风险与后续」由装配后的金丝雀测试覆盖（Task 11、13、14）。
- `ParseSubject(subject string) (Tag, error)`：输入是解码后的完整主题。按「已定的实现细节」中的文法计数：0 次且没有 `[TC`（ASCII 不区分大小写）返回 `ErrTagMissing`；0 次但有 `[TC` 返回 `ErrTagDamaged`；≥2 次返回 `ErrTagMultiple`；恰好 1 次时把 48 个字符交给 `Parse`，失败返回 `ErrMalformed`。所有错误文本固定，不回显主题。文法的正则在运行时由字母表常量拼出，变量名不含 token 一词（「测试向量与密钥扫描」的约定）。
- `Token.Reveal` 的注释由「只用于写入通知正文」改为「只用于构造主题标签与页脚副本」。

**`tests/docs/reveal_test.go`：** 扫描 `cmd/` 与 `internal/` 下的非测试 Go 文件（`go/parser` 完整解析，不只看导入），凡是选择子名为 `Reveal` 或 `RevealToken` 的表达式——调用与方法值（`f := tag.Reveal`）都算——所在包只能是 `internal/security/token` 与 `internal/mail/renderer`。代码审查因此不必再人工检索这些位置。这是守卫测试：写下时就应通过，确认它有效的办法是临时在别的包里加一处引用、看它失败。

**测试（先写并确认失败）：**
- 文法表驱动：恰好一个标签（前后有 `Re:`、`回复：`、中文、全角空格）；没有标签；两个完全相同的标签；两个不同的标签；标签内多一个空格、少一个空格、空格换成制表符或全角空格；令牌 47 与 49 个字符；大写的令牌；小写的 `[tc`（期望 `ErrTagDamaged`：`[TC` 的查找不区分大小写）；令牌含 i、l、o、u；版本字节不为 1（运行时构造 48 个字母表字符）；`[TC` 出现在其他文字里而没有完整标签。
- `ParseSubject("Re: " + tag.Reveal() + " 标题")` 得到的 `Tag` 与原值相等。
- 模糊测试：任意输入不 panic；成功时 `Reveal()` 是输入的子串。
- 金丝雀：`fmt` 的全部常用动词、`%+v` 嵌入结构体（导出字段）、`slog` 的 JSON 处理器都不输出任务 ID 之外的标签内容与令牌文本；三个错误的文本不含输入。
- 变异测试：去掉「恰好一次」改为取第一个、文法改为不区分大小写、`[TC` 的查找改为区分大小写，都应被用例拦下。

- [x] **Step 1：** 写 `subject_test.go` 与 `reveal_test.go`，运行确认 `subject_test.go` 失败（`Tag` 等标识符未定义）。
- [x] **Step 2：** 实现 `subject.go`，更新 `Reveal` 注释。
- [x] **Step 3：** `go test -count=1 ./internal/security/token/ ./tests/docs/`、`make check`、`make secrets` 通过；变异测试逐条记录。
- [x] **Step 4：** `git commit -m "feat(token): subject tag type and anchored subject parsing"`

**实施说明（2026-09-24）：** 由独立子代理按上述契约实现。先写测试，失败基线为编译失败：`Tag`、`NewTag`、`ParseSubject`、`ErrInvalidTag`、`ErrTagMissing`、`ErrTagDamaged`、`ErrTagMultiple` 未定义。`reveal_test.go` 是守卫测试，写下时即通过；临时在 `internal/cli/cli.go`（一处调用、一处方法值）、只在 darwin 编译的 `terminal_darwin.go` 与 `cmd/turncourier/main.go` 中引入违规，四处都被报出，恢复后 SHA-256 不变。

- **新增用例：** `TestParseSubjectGrammar`（文法表 56 行：字面 52 行，另由 i、l、o、u 生成 4 行；拒绝时同时断言零值标签与哨兵的固定文本）、`TestParseSubjectRoundTrip`、`TestTagReveal`、`TestNewTagRejects`（12 种任务 ID、零值令牌、kid 为 0，并钉住 kid 255、`Parse` 得到的令牌与字母表两端的字符）、`TestTagZeroValue`、`TestTagRedaction`、`TestSubjectErrorsFixed`、`FuzzParseSubject`（以文法表为种子，与不用正则的逐字节参考实现做差分）；`tests/docs/reveal_test.go` 的 `TestRevealReferences` 与 `TestRevealSelectorsDetector`。
- **变异测试：** 52 个。契约列出的三个都被拦下；另 49 个涉及计数与判定、错误出口、文法各部分、`[TC` 的查找、`NewTag` 与 `validTaskID` 的各边界、`Reveal` 与 `RevealToken`、四个脱敏出口。「不检查 C」「不检查 T」「`LogValue` 用指针接收者」起初存活，补用例后被拦下；「删除 `Format`」「`Format` 用指针接收者」在普通 `go test` 中由 vet 的 printf 检查拦下。「命中计数改为重叠计数」与「`[TC` 的查找改用 `strings.ToLower`」是等价变异：标签以 `[` 开头、其后 63 个字符都不是 `[`，两个命中不可能重叠；穷举全部码点，没有非 ASCII 字符经 Unicode 折叠变成 `t`、`c` 或 `[`。
- **按字面解释或补充的地方：** ① 恰好一个严格命中时，主题中其余的 `[TC` 残片、令牌为大写的第二个标签都不影响接受；两个命中中有一个版本字节不对，按 `ErrTagMultiple` 拒绝（先计数、后解析）。② `[` 与 `TC` 之间有空格时主题中没有 `[TC`，记为 `ErrTagMissing`；旧形态 `[TC <任务 ID>]` 记为 `ErrTagDamaged`，与 L1b 探测工具的归类一致。③ `ParseSubject` 只检查格式，不验证 MAC；令牌解析失败时返回 `ErrMalformed` 本身，只需区分零次、一次与至少两次，所以最多取前两个命中。④ 零值 `Tag` 不做特殊处理：它的 `Reveal` 不被 `ParseSubject` 接受，也不能再用来 `NewTag`，格式化输出照样脱敏。⑤ 任务 ID 的规则抽成 `validTaskID`，由 `Claims.valid` 与 `NewTag` 共用，行为不变。⑥ 包注释改为「载体是主题标签，本包定义类型与严格文法」。
- **本地模糊测试：** 按默认的 `-fuzzminimizetime` 运行 `FuzzParseSubject`，几秒后执行数会停在 0/秒：工作进程在最小化一个约 6.5 KB 的输入，尝试次数随长度平方增长，被测代码没有挂起。改用 `-fuzzminimizetime=2s` 后，120 秒 430 万次执行，没有失败。development.md 的模糊测试一节已补上这条提示。
- **验证：** `go test -count=1 ./internal/security/token/ ./tests/docs/`（token 包覆盖率 100%）与 `-race`、`make check`（总覆盖率 93.4447%，2908/3112）、`make secrets`、`GOOS=windows go vet ./...`、`GOOS=linux go vet ./...`、两个包的 `CGO_ENABLED=0 go test`、不可见字符检查均通过。L1b 尚未执行，文法已实现但尚未按「门槛」冻结。

**审查后的修正（Task 1，2026-09-24）：** 规格与质量两名审查子代理各自复核，结论都是「修复后合入」，没有「主要」问题。确认并修复的问题如下，修复都先写失败的用例（或先确认用例能拦下对应的变异）：

1. **守卫测试的盲区（规格审查，次要）。** 原实现与 go 工具的通配规则一样，跳过以「.」「_」开头的目录与 `testdata`。但这些目录只是不参与 `./...`，被显式导入时照样编译进产品：审查员在 `internal/_x` 与 `internal/doctor/testdata/y` 中放了引用、由产品包导入，`go build ./cmd/turncourier` 成功而守卫没有报。现在目录一律进入，只跳过以「.」「_」开头的文件（go/build 确实忽略它们）、非 `.go` 文件与测试文件。仓库中目前没有这类目录，改动不会新增误报。
2. **守卫的遍历与允许列表没有测试（质量审查，次要）。** 「允许列表多一个包」「允许列表改成前缀 `internal/`」「按构建约束跳过文件」三个变异都存活。遍历抽成 `revealViolations(root, roots)`，新增 `TestRevealViolationsTree`：在临时目录中搭 11 个文件的合成源码树，期望恰好报出 6 处（含以「.」「_」开头的目录、`testdata`、只在 darwin 编译的文件与允许包的子目录），`_skip.go` 与测试文件不报，并钉住允许列表与契约逐字相同。上述三个变异与「跳过这类目录」都被它拦下。文件头注明按名字的反射调用与模板字段访问（`{{.Reveal}}`）查不出来，仍靠代码审查（两名审查员的细节项）。
3. **任务 ID 含 i、l、o、u 没有用例（质量审查，次要）。** 把正则中任务 ID 的字符类放宽为 `[0-9a-z]` 的变异存活，它会接受 `NewTag` 拒绝的任务 ID。文法表补上任务 ID 含 i、l、o、u 的 4 行，另加开尔文符号（U+212A）与长 s（U+017F）的 2 行（期望 `ErrTagDamaged`），文法表共 62 行。
4. **`%w` 也先于 `Format` 处理（质量审查，次要）。** `fmt.Errorf(f, tag)` 这类对非 error 操作数使用 `%w` 的调用，fmt 在调用 `Format` 之前按错误动词处理，标签值与指针都按原始字段打印，足以还原令牌；格式串为常量时 vet 的 printf 检查会拦下。类型本身无法拦截，因此写进 `Tag` 的已知局限与 `Tag.Format`、`Token.Format` 的注释（契约原文只写了「`%T`、`%p` 之外」）。
5. **细节：** 金丝雀的动词表只是抽样，「`Format` 对 `%U` 或 `%c` 输出明文」存活，现逐一检查 `%T`、`%p`、`%w` 之外的全部字母动词；解析得到的任务 ID 是主题的子串、与整条主题共用底层内存，改为 `strings.Clone`，新增 `TestParseSubjectCopiesTaskID`（先失败）；`tagHolder` 的注释误称 slog 的 JSON 处理器会调用标签的方法，已更正；实现子代理的提交说明把文法表写成 57 行、称变异全部被拦下，实际为 56 行、另有上面第 3 条的存活变异，以本节为准。development.md 补上 `imports_test.go`、`reveal_test.go` 与 `FuzzParseSubject`，architecture.md 的 `tests/docs` 一行补上 `imports_test.go` 与守卫的扫描范围。

**独立复查（Task 1 与 IMAP 修复，2026-09-24）：** 一名子代理复查 `5215fb5`（见 Task 6 之下「开工前修复的 4a 缺陷」）与 `1a2b17a`：修复都成立，没有「主要」与「次要」问题。它以 20 次独立进程的 `-race` 运行核实 IMAP 修复：去掉新检查时 `TestScanBodyLiteralCutShort` 20 次都报告竞争，带修复 0 次（Task 6 实施时的复测只有 2/20，这个端到端用例并不稳定，见 Task 6 之下的更正）；原先出问题的 `body_literal_stalls_halfway` 在 4 个并行进程各跑 30 次的负载下，无修复时 4 个进程都报告竞争，有修复时 0 个。余下 7 条细节的处理：

1. CHANGELOG 把文法写成「已冻结」，改为「按 D4 规定，待 L1b 用真实客户端复核后冻结」。
2. 新检查中的 `limit+1` 没有用例钉住（改成 `limit` 的变异存活），与假服务器两处没跟上 `faultCloseAfter` 的注释一起并入 Task 6：它正在改这两个文件，给 `faultWholeBody` 加「只发前 N 字节后关闭」的选项即可构造这个边界。
3. `revealViolations` 注释承诺的两点没有用例：合成树补上一个以「.」开头、内容不是 Go 源码的文件（被读就会解析失败），另断言解析失败的文件让扫描返回错误；「只跳过以 _ 开头的文件」「解析失败静默跳过」两个存活变异因此被拦下。
4. `%w` 的局限写全：任何含标签或令牌的非 error 操作数（值、指针，以及含它们的切片、映射、结构体与其指针）都会按原始字段打印；契约中「`%T`、`%p` 之外」一句旁加了指向本节第 4 条的说明。
5. 测试注释对 D4 的转述更正为「D4 要求组装好的主题不进结构体字段与长生命周期变量，入站的主题同理」。
6. `tests/docs/deps_test.go` 与 `imports_test.go` 原先跳过所有以「.」开头的目录，本意只是跳过根目录下的 `.git`、`.local`，与守卫原先的盲区同理：`internal/.y` 这样的包会逃过依赖表与依赖方向的检查。现在两者共用 `skipRepoDir`，只跳过仓库根目录下以「.」开头的目录与 `dist`，`TestSkipRepoDir` 钉住这条规则。
7. 质量审查清单中的「透传 `Parse` 的错误」存活而记录没有提到：`Parse` 只返回 `ErrMalformed`，是等价变异。

### Task 2：存储补充（`internal/store/sqlite`）

**Files:**
- Create: `internal/store/sqlite/migrations/0003_mail_pause.sql`
- Modify: `internal/store/sqlite/notifications.go`、`replies.go`、`mailbox.go`、`tasks.go` 及对应测试

**迁移 `0003`：** 只新增一张表与一个索引，不改已发布的 `0001` 与 `0002`：

```sql
CREATE TABLE mail_pauses (
    task_id     TEXT PRIMARY KEY REFERENCES tasks (id),
    reason      TEXT NOT NULL CHECK (reason IN ('hourly', 'daily')),
    paused_at   INTEGER NOT NULL,
    resumed_at  INTEGER CHECK (resumed_at IS NULL OR resumed_at >= paused_at)
) STRICT;

-- UIDVALIDITY 重置后按 Message-ID 认出早已拒绝的来信（4b Task 9 第 0 步）。
CREATE INDEX inbound_rejections_by_message ON inbound_rejections (account, folder, message_id) WHERE message_id IS NOT NULL;
```

按 Message-ID 查找被拒记录用上面的新索引；其余查询大多落在已有的唯一约束或索引上（`notifications.message_id` 与 `delivered_message_id` 的 UNIQUE、`notifications_by_task`、`inbound_messages` 的 `UNIQUE (account, message_id)`、`inbound_rejections` 的 UID 唯一键与 `inbound_rejections_by_time`、`replies_by_task_state`）。`CountNotificationsSince`、`AbandonedSince` 扫描通知表，`TasksWithQueuedReplies` 扫描 `replies_by_task_state`（2026-09-24 审查时以 `EXPLAIN QUERY PLAN` 核实）：表的规模是单用户的量级，每次毫秒级，不为它们增加索引。

**新增接口：**

| 接口 | 语义 |
| --- | --- |
| `NotificationByMessageID(ctx, messageID)` | 按我方 Message-ID 读取通知；不存在返回 `ErrNotFound`。供「已发送」副本对应（Task 10） |
| `NotificationByDeliveredID(ctx, deliveredID)` | 按实际投递 ID 读取通知；供退信关联回通知（Task 9） |
| `LatestAttemptedNotification(ctx, taskID, uncertainAfter)` | 该任务的 SENT 通知与 `updated_at` 不早于 `uncertainAfter` 的 UNCERTAIN 通知中，`COALESCE(sent_at, updated_at)` 最晚的一条（相同时取 id 较大者）；没有时 `ErrNotFound`。`uncertainAfter` 由调用方按 D7 的缺失证据给出：最近一次成功补扫「已发送」的开始时刻减 10 分钟，还没有成功补扫时为零值（全部 UNCERTAIN 都计入）。供 `token_superseded` 的延后判定（D7、「已定的实现细节」中的「最新通知」） |
| `LatestSentNotification(ctx, taskID)` | 该任务 SENT 通知中 `sent_at` 最晚的一条（相同时取 id 较大者）；没有时 `ErrNotFound`。供 Task 9 第 13 步判断有没有比令牌所属通知更新、确已发出的通知 |
| `ResolveUncertainAsDelivered(ctx, id, deliveredID)` | 在一个事务中把 UNCERTAIN 通知核对为已送达并记下实际投递 ID（D7），`sent_at` 取它进入 UNCERTAIN 的时刻（核对前的 `updated_at`），不取核对时刻；通知不是 UNCERTAIN 时返回 `queue.ErrInvalidOutboxTransition`，实际投递 ID 已属于别的通知时返回 `ErrDeliveredMessageIDConflict`，两种情况都不改动数据 |
| `ThreadReferences(ctx, taskID, beforeID, limit)` | 该任务 id 小于 `beforeID`、已记下实际投递 ID 的 SENT 通知的实际投递 ID，按 `sent_at` 升序取最后 `limit` 个（1–20） |
| `CountNotificationsSince(ctx, since)` | 状态为 SENT 或 UNCERTAIN、且 `COALESCE(sent_at, updated_at)` 不早于 `since` 的通知数（D8 的 M）。核对为已送达的通知（D7 的副本证据与本地人工核对都如此）按进入 UNCERTAIN 的时刻计（`sent_at` 沿用该时刻） |
| `AbandonedSince(ctx, since, afterID)` | 原因为 `expired` 或 `task_closed`、且 `(updated_at, id)` 在 `(since, afterID)` 之后（`updated_at > since`，或 `updated_at = since` 且 `id > afterID`）的 ABANDONED 通知，按 `(updated_at, id)` 升序，至多 100 条。同一轮内以上一页最后一行为下一页的游标，返回满 100 条时继续取，同一毫秒被批量放弃的通知因此不重不漏；跨轮从上一轮最后一行那一毫秒的 id 0 起重读并按已提示的通知 ID 去重（Task 11），否则同一毫秒内读取之后才放弃的、id 更小的通知会落在游标之前（2026-09-24 审查后由 `(since)`、按 id 升序改为游标分页）。供发送循环提示「令牌将过期而放弃」（4a「风险与后续」要求 4b 在本地提示） |
| `SentWithoutDeliveredID(ctx, sentBefore, afterID)` | `sent_at` 早于 `sentBefore`、`id > afterID`、仍没有实际投递 ID 的 SENT 通知，按 id 升序，至多 100 条。`afterID` 只用于同一轮内逐页取完（第一页为 0，下一页取上一页最后一行的 id）；跨轮不以已报告的最大 id 作游标，而是每轮从 0 起重读、按已报告的通知 ID 去重（Task 10）：id 较小的通知可能因重新排队而较晚发出，或较晚才被本地核对为已送达（`sent_at` 回溯到进入 UNCERTAIN 的时刻），越过 `sentBefore` 的先后与 id 的先后不一致（2026-09-24 审查后增加 `afterID`：关闭「保存到已发送」后这类通知只增不减，没有游标时积满 100 条就只返回最旧的 100 条，新通知不再告警）。供「副本缺失」告警 |
| `InboundByMessageID(ctx, account, messageID)` | 按 (账户, Message-ID) 读取入站记录：任务 ID、摘要、文件夹、UIDVALIDITY、UID 与回复序号；供验证流水线在有效期与任务状态之前去重 |
| `RejectionExists(ctx, account, folder, uidValidity, uid)` | 该封邮件是否已有被拒记录；供验证流水线跳过已经判定过的邮件 |
| `RejectedBeforeReset(ctx, account, folder, uidValidity, messageID)` | 同一账户与文件夹中，是否有 Message-ID 相同、而 UIDVALIDITY 不同于 `uidValidity` 的被拒记录：UIDVALIDITY 重置后的全量补扫中 UID 都变了，Message-ID 没变。只认 UIDVALIDITY 不同的记录，是为了不让伪造者借一条被拒记录挡掉同一 UIDVALIDITY 下 Message-ID 相同的合法回复 |
| `PruneRejections(ctx, before)` | 删除 `received_at` 早于 `before` 的被拒记录，返回删除条数；一次至多删 10000 条（一个短事务，实测 40–80 毫秒），调用方在返回值等于 10000 时继续调用 |
| `CountAcceptedRepliesSince(ctx, taskID, since)` | 该任务 `created_at` 不早于 `since`、状态不是 REJECTED 的回复数（D8 的 1 小时与 24 小时上限都用它；调用方取窗口起点与最近一次解除时刻中较晚的一个作为 `since`） |
| `PauseMail(ctx, taskID, reason)`、`MailPaused(ctx, taskID)`、`ResumeMail(ctx, taskID)` | D8 的持久暂停，每个任务至多一行：`PauseMail` 写入原因与 `paused_at` 并清空 `resumed_at`（正在暂停时不改动）；`MailPaused` 返回是否暂停与最近一次解除的时刻（从未解除为零值）；`ResumeMail` 记下 `resumed_at`（没有暂停时不是错误）。保留解除时刻，刹车才能只计解除之后的回复（Task 9 第 15 步） |
| `TasksWithQueuedReplies(ctx)` | 有 QUEUED 回复、且任务状态可以派发（COMPLETED 或 WAITING_INPUT）的任务 ID，按各自最早 QUEUED 回复的序号升序 |
| `HasNotifications(ctx)` | 库中是否有过任何通知；供「首次运行的游标」判断是否跳过历史 |

- `RejectReason` 增补 7 个原因码（见「已定的实现细节」），`knownRejectReasons` 同步。
- `ResolveUncertainNotification` 的注释「系统从不自动调用它」改为「由本地核对调用；D7 的副本证据改走 `ResolveUncertainAsDelivered`」。
- `ClaimNextReply` 在正文行缺失、正文密钥不可用或密文无法解密时，与 `ClaimNextNotification` 一样返回该回复未改动的 QUEUED 快照（正文为 nil），不再返回零值：派发循环据此发出带回复序号的事件（Task 12）。
- 输入校验沿用现有规则：Message-ID 为 3–998 个字符的合法 UTF-8、不含 NUL；账户与文件夹按 `checkMailbox`；任务 ID 按 `taskIDPattern`；`limit` 越界返回校验错误。**新旧接口的校验错误一律包装新的哨兵 `ErrInvalidArgument`**（不叫 `ErrInvalidInput`：`payload` 包已有同名哨兵，`internal/app` 两者都要处理）：现有的校验错误都是不带哨兵的 `errors.New`，调用方无从区分校验失败与数据库故障。另导出 `ValidMessageID(string) bool` 与 `ValidSender(string) bool`，规则与存储的校验相同，供 Task 9、10 在查询与写入之前检查来信中的字段。错误文本不回显 Message-ID、地址、账户与文件夹（与现有错误一致，见 `RecordRejection` 的注释）。

**测试（先写并确认失败）：** 迁移 `0003` 可在已有 `0002` 数据的库上执行，`user_version` 为 3；每个接口的命中、未命中与边界：`LatestAttemptedNotification` 取发出时刻最晚的一条（较早创建、较晚发出的通知胜出），跳过 PENDING、SENDING 与 ABANDONED，`updated_at` 早于 `uncertainAfter` 的 UNCERTAIN 不计入、恰在边界上的计入，较新的通知发出之后再核对较早的 UNCERTAIN 通知，结果仍是较新的那条；`ResolveUncertainAsDelivered` 的两种拒绝都不改动数据，成功时状态与实际投递 ID 同时生效，`sent_at` 等于进入 UNCERTAIN 的时刻；`RejectedBeforeReset` 只认 UIDVALIDITY 不同的记录；`ThreadReferences` 不含没有实际投递 ID 的通知且 `limit` 生效；各计数恰在 `since` 的边界（按毫秒）；`PruneRejections` 的边界与单次上限；暂停的写入、重复写入、查询、解除与解除之后再次暂停（`resumed_at` 被清空）；`ClaimNextReply` 的三种正文错误都返回未改动的 QUEUED 快照；新旧接口的每个校验错误都满足 `errors.Is(err, ErrInvalidArgument)`，`ValidMessageID` 与 `ValidSender` 和存储实际接受的取值一致（3 与 998 个字符、254 个字符的地址、非法 UTF-8、NUL）；`TasksWithQueuedReplies` 排除 RUNNING、WAITING_APPROVAL、DELIVERY_UNCERTAIN 与关闭的任务，且按最早序号排序；新原因码可以写入与读回，未知原因码仍被拒绝。

- [x] **Step 1：** 写失败的测试。
- [x] **Step 2：** 实现，`make check` 通过（`internal/store/sqlite` 覆盖率不低于当前的 87%）。
- [x] **Step 3：** `git commit -m "feat(store): mail pauses and the lookups, counters and atomic resolution 4b needs"`

**实施说明（2026-09-24）：** 由独立子代理在独立 worktree 中实现（提交 `0f24c23`，合入后为 `8006f73`），与 Task 1 并行开发、按顺序审查与合入。先写测试，`go test ./internal/store/sqlite/` 编译失败（`-gcflags=-e` 列出 33 个未定义标识符：`ErrInvalidArgument`、`PauseMail`、`ResolveUncertainAsDelivered`、`InboundRecord`、`ValidMessageID`、新原因码等）。`0003_mail_pause.sql` 的两条语句从本清单逐字取出，只在前面加了说明注释，`TestMigrateUpgradesVersion2Data` 把 `sqlite_schema` 中的文本与契约原文逐字比较。实现要点：

1. 输入校验错误统一由 `invalidArgument` 构造：`Error` 返回原文，`Unwrap` 同时给出 `ErrInvalidArgument` 与原错误，原来包装的哨兵照常匹配（`ErrInvalidSessionID` 本身同时满足 `ErrInvalidArgument`；`ApplyTaskEvent` 拒绝保留事件时同时包装 `task.ErrInvalidTransition`）。取决于数据库状态的结果（`ErrTaskNotNotifiable`、`ErrCursorRegression`、`ErrKeyExists`、`ErrNoDispatchableReply`、状态机拒绝的转移、目录权限）不包装。为此改了清单外的 `store.go`（`Open`）与 `instance.go`（`RegisterKey`）；`CreateNotification` 的 owner 检查取决于任务行，但重试也不会成功，也包装，哨兵注释把它列为例外。
2. Message-ID、账户与发件人的规则原在多处各写一份，合并为 `ValidMessageID` 与 `validAddress`（`ValidSender` 即它），存储各处与导出函数共用；几条错误文本的后半句随之统一（提交说明称文本不变，并不准确；仓库中没有依赖这些文本的地方）。
3. 通知快照的读取拆出 `scanNotification`；`ResolveUncertainAsDelivered` 在一个事务中先改状态、再写实际投递 ID，冲突或失败时整个事务回滚。
4. `TasksWithQueuedReplies` 在 SQL 中只分组并按最早 QUEUED 序号排序，可否派发交给 `task.CanDispatchReply`。
5. `ClaimNextReply` 读出正文失败时返回读取时的 QUEUED 快照与任务快照（事务随后回滚）。

按字面解释或补充的地方：`ValidMessageID("")`、`ValidSender("")` 为假；`PruneRejections` 先删最早的；`ResumeMail` 以 `max(当前时间, paused_at)` 记下解除时刻，没有暂停时不改动；`PauseMail` 对不存在的任务返回 `ErrNotFound`，正在暂停时返回 nil 且不改动；`MailPaused` 对没有记录的任务返回 false 与零值；零值 `uncertainAfter` 不特判；`ThreadReferences` 不校验 `beforeID`；新增导出类型 `InboundRecord` 与 `PauseReason`。契约之外补了：`EXPLAIN QUERY PLAN` 钉住 `RejectedBeforeReset` 走新索引；D7 核对的故障注入；`ResumeMail` 的时钟回拨；取消上下文时新方法返回 `context.Canceled` 而非 `ErrInvalidArgument`；三个上限各用超出一条的数据验证。变异测试 58 个，全部被拦下。验证：本包 `go test -race -count=3` 通过（覆盖率 87.6% → 88.3%）、`make check`（总覆盖率 93.18%）、`make secrets`、两个平台的 vet、`CGO_ENABLED=0 go test` 通过。

**审查后的修正（Task 2，2026-09-24）：** 规格审查的结论是「可以合入」，质量审查是「修复后合入，只需补测试」，都没有「主要」问题；两处次要的功能问题在尚未接入的接口上（本地核对的 `sent_at`、两个至多 100 条的列表），与其余测试缺口一起修复。由独立子代理修复（`a3cb23b`，合入后为 `25bc695`），每条都先写失败的用例，或先确认用例能拦下对应的变异：

1. **七个存活变异（质量审查，次要）：** 「最新通知」平局先按状态再按 id；排序改用 `coalesce(sent_at, created_at)`；缺失证据按 `created_at` 判断；核对后的 `sent_at` 取 `created_at`（这三个打在 D7 的核心语义上：原有用例的 UNCERTAIN 通知创建与进入 UNCERTAIN 在同一时刻，分不清两者）；在途回复也算待派发；`MailPaused` 把数据库错误当作没有暂停（与 D6 同类的风险：短暂失败会绕过回环刹车）；`ResumeMail` 不限定任务。并入审查员验证过的 6 个用例后全部被拦下。
2. **本地人工核对与 D7 不一致（两名审查员，待确认）：** `ResolveUncertainNotification(id, true)` 原先以核对时刻为 `sent_at`：A 先进入 UNCERTAIN、B 随后 SENT、再人工核对 A，A 就成了「最新 SENT 通知」，对 B 的回复会被误判 `token_superseded`，A 还会在核对时刻的发信计数里再计一次。主会话决定对齐：两种核对共用 `resolveDelivered`，`sent_at` 取进入 UNCERTAIN 的时刻；`delivered=false` 不变。新用例 `TestResolveUncertainNotificationKeepsSendOrder` 在修复前失败；4a 的 `TestNotificationOutcomes` 原断言 `sent_at` 等于核对时刻（4a 契约并未规定），随之改正。
3. **两个「至多 100 条」的查询会漏报（两名审查员）：** `AbandonedSince` 同一毫秒被放弃超过 100 条（关闭一个积压很多通知的任务）时重复或遗漏；`SentWithoutDeliveredID` 在关闭「保存到已发送」后积满 100 条，此后只返回最旧的 100 条。签名改为 `AbandonedSince(ctx, since, afterID)` 与 `SentWithoutDeliveredID(ctx, sentBefore, afterID)`（接口表已同步），新增 250 条分三页、同一毫秒 150 条分两页（其中一例是真实的任务关闭）与 `afterID` 边界的用例，另做 14 个分页变异，全部被拦下。修复者进一步指出调用方的两处剩余缺口，已写入 Task 10、Task 11 的契约：「已发送」的缺失告警每轮从 `afterID = 0` 起重读并按已报告集合去重，不以最大 id 作跨轮游标（重新排队会打乱 id 与发出的先后）；放弃提示的初始游标为启动时刻，并按该毫秒已提示的 ID 去重。
4. **注释与钉住的用例：** `MailPaused` 注明刹车计入与解除同一毫秒接受的回复（偏严的一侧），`TestBrakeCountsFromResume` 钉住它（规格审查次要项：Task 9 的刹车用例在完全冻结的时钟下前后两半不可能同时成立，契约措辞已改为「各步之间前进至少 1 毫秒」）；`ResumeMail` 注明取 `max(当前时间, paused_at)` 的原因与代价（时钟回拨期间刹车不计回拨幅度内的回复）。
5. **architecture.md：** 「每次状态变化先经状态机」对邮件暂停不成立，已改写；UNCERTAIN 的解除方式与「核对不改变发出时刻」已写入。
6. **转给后续任务契约的其余意见**（已写入对应任务）：Task 10 在写入实际投递 ID 被拒时如何处理（`ErrDeliveredMessageIDConflict` 与 `ErrInvalidOutboxTransition`）；Task 13 在 `PruneRejections` 返回满额时继续调用；Task 5 在写入头部前校验 msg-id 的形状（存储的规则不排除 CR、LF）；契约中「其余查询都落在已有索引上」对三个查询不成立，改为如实描述。

验证：本包覆盖率 88.5%；`make check`（总覆盖率 93.58%）、`make secrets`、两个平台的 vet、`CGO_ENABLED=0 go test`、不可见字符检查通过；本节共做变异 29 个，全部被拦下。

**独立复查（Task 2，2026-09-24）：** 一名子代理复查 `25bc695` 与两次契约修订（`06ca434`、`1bde490`）：修复成立，没有「主要」问题，也没有新的行为缺陷；它以临时用例在真实库上复现了三种场景（重新排队、本地核对使 `sent_at` 回溯、同一毫秒先读后放弃 id 更小的通知），确认修订后的调用方用法不漏。处理如下：

1. **注释与接口表仍写着旧用法（次要）：** `SentWithoutDeliveredID` 的注释与接口表还写「调用方记住已报告的最大 id」，与 Task 10「不以最大 id 作跨轮游标」相矛盾；`AbandonedSince` 缺 Task 11 的跨轮规则。两处注释与接口表已改为：游标只用于同一轮内逐页取完，跨轮按 Task 10、Task 11 的规则重读并去重；「越过 `sentBefore` 的先后与 id 不一致」的成因补上第二种——本地核对为已送达使 `sent_at` 回溯。
2. **Task 10、11 的测试没有钉住修订后的用法（次要）：** 两个任务的测试段补上了对应用例与变异（较晚发出或较晚核对的小 id 通知只计入一次、以最大 id 作跨轮游标应失败；关闭积压 150 条通知的任务各一个 `notification_dropped`、`notification_expired`、同一毫秒先读后放弃、启动之前的放弃不提示，以最后一行作跨轮游标或不去重应失败）。
3. **存活变异（细节）：** 本地核对为已送达的调用点绕过 `resolveDelivered` 的状态检查时，SENDING 的通知会被改为 SENT、正文被删除；原用例只对 SENDING 调用了 `delivered=false`。现在两个方向都断言被拒且数据不变，该变异被 `TestNotificationOutcomes` 拦下（4a 起就有的缺口）。
4. **分页用例的边界（细节）：** 「逐页取完」一例以 `n%6` 排除，60 是 6 的倍数，两个分页边界恰好都落在毫秒边界上，从不走「同一毫秒、id 更大」的续取分支。改为 `n%7`（共 258 条，分页 100、100、58，两个边界都在某一毫秒中间）。
5. **Task 11 的剩余缺口与措辞（细节）：** 游标规则改写为「同一轮内逐页、跨轮从那一毫秒的 id 0 重读去重」，并把时钟回拨、停机窗口与同轮竞态写成「尽力而为」的已知局限。
6. **概括措辞（细节）：** 上文「审查后的修正」开头原称「产品代码没有功能缺陷」，改为如实描述两处次要的功能问题。
7. **重启后逐条刷屏（细节，待确认）：** 按「每轮重读、进程内去重」，关闭「保存到已发送」时每次重启都会为积压中的每条通知各发一次 `sent_copy_missing`。主会话定为按轮聚合：每轮一条带计数的事件（写入 Task 10 的契约）。

### Task 3：解析器（`internal/mail/parser`）与合成回归样本

**Files:**
- Create: `internal/mail/gateway.go`（页脚标记行、页脚固定句子与自定义头名三个常量）、`internal/mail/parser/parser.go`、`charset.go`、`quote.go`、`parser_test.go`、`quote_test.go`、`fuzz_test.go`、`tests/fixtures/mail/` 下的样本模板与期望结果、`tests/fixtures/mail/README.md`
- Modify: `docs/zh-CN/development.md`、`docs/en/architecture.md`（依赖表「使用方」一栏点名 `internal/mail/parser`，`tests/docs/deps_test.go` 据此核对）

**契约：**

- `Parse(raw []byte) (*Message, error)`：只读头部与 MIME 结构，不做判定。失败只返回不含来信字节的哨兵错误（`ErrMalformed`、`ErrHeaderTooLarge`，与 `tests/live/sample.go` 的 `parseMessage` 同样的理由）。`Message` 的字段：
  - `From`：`From` 头中唯一的地址（原文 addr-spec，规范化由调用方经 `config.NormalizeAddress` 完成）；`From` 缺失、无法解析或多于一个地址时 `Parse` 返回 `ErrMalformed`；
  - `Subject`（解码后的完整主题）、`MessageID`（`Message-Id` 头去掉首尾空白后的原样字符串，缺失时为空）、`InReplyTo` 与 `References`（带尖括号的列表）；
  - 判定所需的头部信号：`AutoSubmitted`、`XAutoreply`（`X-Autoreply` 或 `X-Autorespond` 存在）、`Precedence`、`ReturnPath`（原样，去空白）、`MediaType`（顶层媒体类型，小写）；
  - 「已发送」副本对应所需的 `IDHeader`（`X-TurnCourier-ID` 的取值）与 `OQMsgID`（`X-OQ-MSGID` 的取值），都补成带尖括号的形式。
  - `Message` 实现与 `token.Tag` 相同的脱敏输出（`String`、`Format`、`LogValue` 只输出固定文本）：它的 `Subject` 带着令牌，`%+v` 打印一个 `Message` 就会泄漏。
- `ParseHeader(raw []byte) (*Message, error)`：只解析头部、不读正文，字段与 `Parse` 相同（正文相关的方法返回 `ErrNoPlainText`），供「已发送」的只取头部的批使用（Task 10）。
- `(*Message) NewText() (string, error)`：取第一个非附件的 `text/plain` 部件（MIME 深度至多 8、每层至多 32 个部件、部件解码后至多 1 MiB），按字符集解码，换行统一为 `\n`，去掉 BOM，再剥离引用与签名，去掉首尾空行与空白。结果保证是合法 UTF-8。错误：没有 `text/plain` 为 `ErrNoPlainText`；按「已定的实现细节」判为引用没有剥净或解析不确定时为 `ErrUncertain`；剥离后为空为 `ErrEmpty`；解码失败或解码后不是合法 UTF-8 为 `ErrMalformed`。
- **引用边界**（自上而下，第一条命中的行及其后全部丢弃）：
  1. QQ 的分隔线：由破折号包围的「原始邮件」或 `Original`（长短两种）、单独一行的「原始邮件」；
  2. Outlook 的 `-----Original Message-----`；
  3. 以「写道：」或「写道:」结尾、或以 `wrote:` 结尾（不区分大小写）的行，**且其后第一个非空行以 `>` 开头**（Apple Mail、iOS、Gmail 的引用头）；其后的 `>` 引用块之后若又出现既不是引用、也不是签名的文字，返回 `ErrUncertain`（行内逐段回复，Gmail 与 Apple Mail 最常见的形态；只丢弃引用头及其后的全部内容，会让 Agent 只收到第一段）；
  4. 以 `>` 开头的行，且这一段连续的 `>` 行去掉前缀后出现我方页脚的标记行或 `[TC`（ASCII 不区分大小写），即引用的是我方通知。不含这些的 `>` 行不当作边界，返回 `ErrUncertain`：它可能是用户自己用 `>` 标出的命令或引文（例如「请运行：」之后一行「> make test」），按引用丢弃会让 Agent 只收到「请运行：」。`>` 引用块之后若又出现既不是引用、也不是签名的文字，同样返回 `ErrUncertain`（行内逐段回复）；
  5. 没有分隔线就直接开始的引用头块（Foxmail 式）：以「发件人」或 `From` 加冒号开头的行，且其后 3 行内还有「发送时间」「日期」「收件人」「主题」「Sent」「Date」「To」「Subject」之一加冒号开头的行——单独一行 `From: …` 不算边界，免得误切用户自己的文字。
- **签名：** 最后一个恰为 `-- ` 的行及其后丢弃（RFC 3676）；单独的 `--` 只在其后至多 6 行就到正文末尾时才按签名处理；最后一个非空行等于已知的客户端签名（「发自我的iPhone」「发自我的 iPhone」「Sent from my iPhone」「发自我的iPad」「来自QQ邮箱」等，列表在代码中）时丢弃该行。
- **残留检查：** 剥离之后的新正文中出现令牌形状的串、`[TC`、我方页脚的标记行或页脚的固定句子（「已定的实现细节」中的「引用与签名剥离」），返回 `ErrUncertain`。页脚标记行、页脚固定句子与自定义头名定义在 `internal/mail/gateway.go`（设计目录中的「邮件契约」，包 `mail`，只有常量），解析器、渲染器与 `internal/app` 共用，不在各包重复定义。
- 解析器输出必须确定：同一输入字节永远得到同一新正文（键控摘要依赖它）。

**合成回归样本（`tests/fixtures/mail/`）：** 每个样本是一份**模板**加一份期望结果（新正文，或期望的错误名）。模板保存头部与各部件解码后的文字，并声明每个部件的字符集与传输编码；测试加载时先把占位符 `{{TASK}}`、`{{TOKEN}}`、`{{DELIVERED}}` 换成运行时签发的值，再按声明的编码（例如 utf-8/base64、gbk/base64）组装为原始字节——直接存 base64 的 `.eml` 无法替换占位符，而把令牌写进文件又违反密钥扫描的约定。样本全部合成，地址用 `example.invalid`。按 L1 实测结构合成的有：QQ 邮箱 App 回复（`multipart/alternative`，两部分均 utf-8/base64，纯文本带「原始邮件」分隔线与引用头块，HTML 用 QQ 自有标记、不用 blockquote，页脚令牌行在引用中且没有引用前缀，引用头块的「主题:」行也带着标签与令牌）、QQ 邮箱网页版回复（完全没有引用）、QQ 假期自动回复（只有 `text/html`，主题 `自动回复: `，只有 `In-Reply-To`，没有四项头部信号）、退信（`multipart/report`，子部件 `text/html` 与 `application/octet-stream`，`Auto-Submitted: auto-generated`，发件人含大写字母，线程头指向实际投递 ID）。按公开资料合成、待 L1b 或以后真机样本核实的有：Foxmail 式头块（没有 `In-Reply-To`）、Apple Mail 的「写道：」加 `>` 引用、Gmail 的 `wrote:` 加 `gmail_quote`、标注 gbk 而含 GB18030 四字节字符的回复、带 `-- ` 签名与「发自我的iPhone」的回复、只有 HTML 的真人回复（`ErrNoPlainText`）、引用没有分隔线而带我方页脚的回复（`ErrUncertain`）、行内逐段回复（带「wrote:」引用头与不带引用头的各一份，都是 `ErrUncertain`；不带引用头的那份，`>` 块中含我方页脚的标记行、块后是用户的作答，这样判为不确定的是行内回复检查，而不是「没有页脚标记」那一条，去掉规则 4 的行内检查的变异才会被拦下）、用户自己用 `>` 标出命令而没有引用我方通知的回复（`ErrUncertain`）、正文里写着「日志里写道：」但其后不是引用的回复（整段保留）。每个样本的来源（L1 实测结构 / 公开资料推断）写在 `tests/fixtures/mail/README.md`。

**测试（先写并确认失败）：** 逐个样本比对期望结果；头部解析的边界（多个 `From`、缺失 `Message-Id`、`In-Reply-To` 含多个 ID、encoded-word 主题标注 gbk）；上述五类引用边界各自的正反用例（包括单独一行 `From:`、后面不是引用的「写道：」都不被当作边界，不含页脚标记与 `[TC` 的 `>` 行返回 `ErrUncertain`）；签名剥离（`-- ` 与靠近末尾的 `--`、远离末尾的 `--` 不算）；残留检查的四种触发；非法 UTF-8 的正文；`Message` 的 `%v`、`%+v` 与 `slog` 输出不含主题；上限（深度、部件数、1 MiB）；确定性（同一输入重复解析结果相同）；模糊测试（任意字节不 panic、`NewText` 的结果不超过 1 MiB 且是合法 UTF-8）；错误文本不含来信字节。

- [ ] **Step 1：** 写样本模板、期望结果与失败的测试。
- [ ] **Step 2：** 实现 `parser.go`、`charset.go`、`quote.go`；更新两份依赖表。
- [ ] **Step 3：** `make check`、`make secrets` 通过；变异测试：逐条去掉引用边界规则、去掉「写道：」之后须是引用的条件、`>` 行不要求页脚标记或 `[TC`、去掉行内回复的不确定判定（规则 3 与规则 4 分别去掉）、去掉 GB18030 映射、去掉残留检查，都应被样本拦下。
- [ ] **Step 4：** `git commit -m "feat(parser): parse inbound mail and strip quotes and signatures"`

### Task 4：自动回复与退信判定（`internal/mail/parser`）

**Files:**
- Create: `internal/mail/parser/classify.go`、`classify_test.go`

**契约：** `Classify(m *Message, plain, html []byte) Verdict`，`Verdict` 为 `Human`、`AutoReply` 或 `Bounce`，另带一个信号名（`subject_prefix`、`auto_submitted`、`x_autoreply`、`precedence`、`return_path_empty`、`qq_vacation_body`、`multipart_report`、`daemon_sender`）供本地事件使用。`plain` 与 `html` 是正文部件的前 512 字节（由 `Message` 的一个方法取得，不经过引用剥离）。规则按「已定的实现细节」：先判退信（`multipart/report`、MAILER-DAEMON 或 postmaster），再判自动回复（主题前缀、`Auto-Submitted` 不为 `no`、`X-Autoreply`/`X-Autorespond`、`Precedence` 为 auto_reply/bulk/junk/list、`Return-Path: <>`、QQ 假期自动回复的固定开头），其余为 `Human`。

**测试（先写并确认失败）：** Task 3 的四份 L1 样本分别得到 `Human`、`Human`、`AutoReply`（信号 `subject_prefix`）、`Bounce`；每条规则单独成立时都命中（每个用例只满足一条）；主题前缀的全部条目（表驱动）、全角与半角冒号、大小写、前缀前后的空白；主题以 `Auto-Reply:` 开头的 `multipart/report` 仍是 `Bounce`；`Re: 自动回复:` 与正文中间出现「自动回复」都不命中；`Auto-Submitted: no` 不命中。

- [ ] **Step 1：** 写失败的测试。
- [ ] **Step 2：** 实现。
- [ ] **Step 3：** `make check` 通过；变异测试逐条删除规则、把主题前缀判定挪到退信之前。
- [ ] **Step 4：** `git commit -m "feat(parser): classify auto-replies and bounces before any token check"`

### Task 5：通知渲染、敏感信息过滤与 `notify.content`（`internal/mail/renderer`、`internal/config`）

**Files:**
- Create: `internal/mail/renderer/content.go`、`filter.go`、`renderer.go` 及测试
- Modify: `internal/config/config.go`（`Notify.Content`、`knownKeys`）、`internal/config/write.go`（init 渲染的配置写入 `content = "filtered"`）、`configs/turncourier.example.toml`、config 测试、两份依赖表（`internal/mail/renderer` 使用 go-message）

**契约：**

- `Content{Version, Event, TaskID, Title, Body}` 与 `(Content) Encode() ([]byte, error)`、`DecodeContent([]byte) (Content, error)`：JSON，严格解码，`Version` 须为 1，`Event` 为四个通知事件之一，`TaskID` 为 10 个字母表字符，编码后至多 `MaxContentSize`（1 MiB，与 `payload.MaxPlaintext` 相同：本包不能导入 `security/*`，常量在此重复定义，由 `internal/app` 的一个测试断言两者相等）；错误不回显内容。
- `Tag` 接口：`Reveal() string` 与 `RevealToken() string`。`token.Tag` 满足它，本包不导入 `security/token`。
- `Render(env Envelope, tag Tag, c Content, opts Options) ([]byte, error)`，`Envelope{From, To, MessageID, References []string, Date}`，`Options{Mode, Scrub Scrubber}`：`Mode` 为 `ModeFiltered` 或 `ModeStatus`；`Scrubber` 是本包定义的接口 `Scrub(text string) string`，把进程持有的机密（授权码与两把密钥的文本）原样出现处换成「[已省略]」。机密只留在实现方（`internal/app` 的 `*Secrets`，格式化输出脱敏）的未导出字段里，不以切片等明文形式出现在 `Options` 或任何参数中，`%+v` 打印 `Options` 只会得到脱敏文本：
  - 头部：`From`、`To`、`Subject`（见「主题的编码」；`ModeFiltered` 的标题为事件中文名加「：」加过滤后的 `Title`，`ModeStatus` 的标题只有事件中文名；至多 60 个字符）、`Date`、`Message-Id`、`In-Reply-To` 与 `References`（`References` 非空时）、`MIME-Version`、`Auto-Submitted: auto-generated`、`X-TurnCourier-ID`（等于 `MessageID`）；
  - `multipart/alternative`：纯文本与 HTML 两部分，均 utf-8、quoted-printable；HTML 部分由纯文本经 `html.EscapeString` 转义后放入 `white-space: pre-wrap` 的容器，不接受任何来自内容的标签；
  - 正文：`ModeFiltered` 为过滤后的 `Body`，`ModeStatus` 为固定的一句状态说明；其后是页脚，第一行为标记行，然后是任务 ID 与事件、回复说明（「直接回复本邮件即可把新消息交给该任务；请保留主题中的 [TC …] 标签，不要改动。」）、令牌副本（`RevealToken`，注明仅供核对）、内容被省略或截断时的「部分内容已省略，完整输出留在本机（任务 <ID>）」、「请把机器人地址加入通讯录，以免通知落进垃圾箱」，以及「请不要在接收本通知的邮箱中开启假期自动回复」；
  - 渲染结果超过 256 KiB 返回 `ErrTooLarge`；组装好的主题只作为局部变量存在，写入头部后即弃（D4「令牌脱敏的边界」）。
  - `Envelope` 的 `MessageID` 与 `References` 中的每个 ID 在写入头部之前按 msg-id 的形状校验：`<` + 左部 + `@` + 右部 + `>`，只含可打印 ASCII（0x21–0x7e），不含空白与控制字符；不合法即返回错误、不渲染。这些值来自存储，而存储的 `ValidMessageID` 只要求合法 UTF-8、不含 NUL，不排除 CR、LF（2026-09-24 Task 2 质量审查指出）；它们又来自机器人自己的「已发送」，风险低，这一条是防头部注入的纵深防御。
- `Filter(text string, scrub Scrubber) string`：按「已定的实现细节」的顺序执行，确定性，幂等（对输出再过滤一次不变）。

**测试（先写并确认失败）：** 主题头的原始字节：第一行恰为 `Subject: ` 加标签，解码后等于「标签 + 空格 + 标题」，标题被拆成多个 encoded-word 时同样成立；`ParseSubject` 能从渲染结果的主题中取回同一个标签（跨包的往返测试放在 `tests/e2e`，本包用字符串断言）；页脚第一行等于 `mail.FooterMarker`，`X-TurnCourier-ID` 用 `mail.IDHeader`；过滤规则逐条的正反用例：URL 的主机与路径保留而 userinfo、查询串与片段被抹去、`file://` 链接按路径处理、`PATH=/usr/bin:/Users/…` 的每一段、`host:/path`、Windows 路径、PEM 块、`Authorization:`、`OPENAI_API_KEY=…` 与 `db_password: …`、围栏代码块计行（含没有闭合与带缩进的围栏）、双向控制与零宽字符、`Scrubber` 抹去的机密与令牌形状的串、截断——以及幂等；金丝雀：解码后的 `Subject`、`text/plain` 与 `text/html` 三处各恰好出现一次令牌文本（原始字节中 quoted-printable 的软换行可能把它拆开，所以按解码后计数），`Render` 的错误、`Content` 的 `%v` 都不含正文与令牌；`ModeStatus` 的输出（含主题）不含 `Title` 与 `Body` 中的任何金丝雀；配置 `notify.content` 的默认值、两个合法值、非法值与大小写变体键名被拒绝。

- [ ] **Step 1：** 写失败的测试。
- [ ] **Step 2：** 实现，更新配置与示例、依赖表。
- [ ] **Step 3：** `make check`、`make secrets` 通过；变异测试：去掉 HTML 转义、去掉路径规则、`ModeStatus` 仍写标题、页脚少写令牌副本、主题改用 `SetSubject`（标签进入 encoded-word），都应被拦下。
- [ ] **Step 4：** `git commit -m "feat(renderer): render notifications with subject tag, thread headers and content filter"`

### Task 6：IMAP 扩展（`internal/mail/imap`）

**Files:**
- Modify: `internal/mail/imap/session.go`、`watcher.go`、`fakeserver_test.go` 及测试

**契约：**

- `FolderSent = "Sent Messages"`（L1 第 1 步实测的名称）。
- `(*Session) ScanHeaders(ctx, folder, cur) (Batch, error)`：与 `Scan` 相同，但逐封取 `BODY.PEEK[HEADER]`，每封至多 64 KiB（超出记为 `TooLarge`）；批的正文合计上限照旧。
- `Watcher` 增加五个字段：
  - `Sent bool`：为真时每一轮最先以 `ScanHeaders` 补扫 `FolderSent`，交给同一个 `Handle`（`Batch.Folder` 为 `FolderSent`）。它是承重路径（D6）：失败只跳过本轮、下一轮照常重试，**不像 Junk 那样降级**；每次失败发出 `folder_unavailable`（`Folder: FolderSent`），不拆掉连接。LIST 中没有它时同样按失败处理：每轮发出 `folder_unavailable`、不调用 `Scanned`，而不是像 Junk 那样只报一次 `folder_missing`——对 D6 而言，文件夹缺失与补扫失败一样拿不到证据，回复只会一直延后，所以必须计入 `sent_unavailable` 告警（Task 10）。
  - `Wake <-chan struct{}`：可为 nil。等待新邮件时（IDLE 或 `Poll` 间隔）收到信号即正常结束等待——IDLE 发出 DONE 并按正常结束处理——开始新的一轮。补扫期间到达的信号留在通道中，使下一次等待立即结束。
  - `Scanned func(folder string, started time.Time)`：可为 nil。某个文件夹本轮补扫成功完成（最后一批 `More` 为 false，且 `Handle` 没有返回错误或 `ErrDefer`）时调用，参数是本轮补扫开始的时刻。D6 用它判断「副本确实不在」的证据。
  - `SkipHistory func(folder string) bool`：可为 nil。文件夹没有持久化游标（`Cursor` 返回零值）且它返回真时，Watcher 不补扫历史，只以 EXAMINE 得到的 UIDVALIDITY 与 UIDNEXT−1 构造一个不含邮件的批（`Next` 即该游标）交给 `Handle` 持久化；UIDNEXT 缺失时照常补扫。
  - `Now func() time.Time`：可为 nil（取 `time.Now`）。只用于 `Scanned` 的补扫开始时刻：D6 与 D7 拿它和存储的时间戳比较，两者必须出自同一个时钟（Task 13 传入 App 的时钟，测试注入的时钟因此对两边都生效）。登录间隔、退避与 Junk 的重试等计时仍用真实时间。
- `ErrDefer`：`Handle` 返回包装 `ErrDefer` 的错误，表示本批中有邮件须留到下一轮处理、`Handle` 已自行持久化可以前进的部分游标。Watcher 结束该文件夹的本轮补扫，不发出 `handle_failed`、不退避、不计入 Junk 的失败次数、不调用 `Scanned`，照常进入下一个文件夹与等待；INBOX 也如此（其余错误的无限重试不变）。
- 每一轮的顺序：「已发送」→ INBOX → Junk → 回到 INBOX 等待。

**测试（先写并确认失败，用 4a 的假服务器）：** `ScanHeaders` 只返回头部且不设 `\Seen`；Watcher 的补扫顺序（命令记录中「已发送」的 EXAMINE 在 INBOX 之前）；`Sent` 为假时不访问它；LIST 中没有「已发送」时每轮都发出 `folder_unavailable`、不调用 `Scanned`；「已发送」连续失败多次也不降级、不影响 INBOX；`Wake` 使 IDLE 以 DONE 结束且不重连、使 `Poll` 等待提前结束；`Scanned` 只在成功完成时调用、时刻是本轮开始的时刻；`SkipHistory` 为真时不取回任何历史邮件、游标落在 UIDNEXT−1，为假或已有游标时照常；`Scanned` 的时刻取自注入的 `Now`；`Handle` 返回 `ErrDefer` 时不发 `handle_failed`、不退避、游标停在 `Handle` 持久化的位置，下一轮重新交付被延后的邮件；原有测试全部照旧通过。

- [x] **Step 1：** 扩展假服务器（`BODY.PEEK[HEADER]` 的命令记录），写失败的测试。
- [x] **Step 2：** 实现。
- [x] **Step 3：** `make check` 通过（`internal/mail/imap` 覆盖率不低于 96%：`make test` 的 `-race -covermode=atomic` 下当前为 97% 左右，普通 `go test -cover` 略低）；变异测试：「已发送」排到 INBOX 之后、「已发送」照 Junk 降级、`ErrDefer` 当普通错误处理、`Wake` 不结束 IDLE、`Scanned` 在失败时也调用，都应被拦下。
- [x] **Step 4：** `git commit -m "feat(imap): scan Sent headers first, wake on demand, defer batches and skip history"`

**开工前修复的 4a 缺陷（2026-09-24）：** 验证 Task 1 时，`make check` 在机器负载较高时于 `TestCommandDeadlines/body_literal_stalls_halfway` 报告数据竞争。根因在 `Session.body`：服务器在正文字面量中途以 close_notify 正常关闭连接时，go-imap 的字面量读取器把 `io.EOF` 当作字面量结束，`io.ReadAll` 带着不完整的正文「成功」返回，解码协程也被放行；随后的 `msg.Next()` 丢弃剩余字面量时再次读取同一个读缓冲，与解码协程争用。4a 的注释只防住了「读取失败」这一条路径。修法：读到的字节少于字面量声明的长度（且未到上限）时，与读取失败同样处理，按连接断开结束本命令、不再调用 `Next`。假服务器新增故障 `faultCloseAfter`（转发 N 字节后以 close_notify 关闭），新用例 `TestScanBodyLiteralCutShort` 在修复前于 `-race` 下报告竞争（Task 1 的独立复查当时连续 20 次独立运行都报告），修复后通过。本任务新增的 `ScanHeaders` 读取头部字面量时须沿用同一条检查。**更正（Task 6 实施时）：** 这个端到端用例能否看到竞争取决于调度与机器负载，并不稳定：Task 6 的实现子代理在同一台机器上去掉该检查后重跑，20 次中只有 2 次报告竞争，不带 `-race` 时 0 次（功能结果完全相同）。提交 `5215fb5` 说明中「每次都报告」的说法因此不成立（已推送的提交说明不改写，以此处为准）。Task 6 把这条检查提取为纯函数 `readLiteral`，由 `TestReadLiteral` 确定地钉住边界，端到端用例保留作辅证（见下方实施说明第 8 条）。

**实施说明（2026-09-24）：** 由独立子代理按上述契约实现（提交 `5003bd9`，由 worktree 中的 `c002eab` 拣选而来）。先扩展假服务器（「已发送」文件夹、LIST 中不列出它的 `noSent`、EXAMINE 不报告 UIDNEXT 的 `noUIDNext`、`faultWholeBody` 只发出字面量前若干字节即关闭两侧连接的 `cut`，UIDFETCH 数据项白名单加入 `BODY.PEEK[HEADER]<0.N>`，取头部时响应的节与请求一致）并写测试，失败基线为编译失败：`FolderSent`、`ScanHeaders`、`maxHeaderSize` 未定义；`TestReadLiteral` 以 `undefined: readLiteral` 失败；`TestWatcherSkipHistoryAsksAfterExamine` 与 `TestWatcherSkipHistory` 的「UIDNEXT missing」子用例在当时「EXAMINE 之前询问」的实现上失败。

- **新增用例：** `TestScanHeadersReturnsHeadersReadOnly`、`TestScanHeadersTooLarge`、`TestScanHeadersBatchBytes`（2 例）、`TestScanHeadersLiteralCutShort`、`TestScanLiteralCutAtLimit`（`Scan` 与 `ScanHeaders`，各重复 25 轮）、`TestReadLiteral`（9 例）、`TestWatcherScansSentFirst`、`TestWatcherLeavesSentAloneByDefault`、`TestWatcherSentMissingFromList`、`TestWatcherSentFailuresNeverDegrade`（带标签 BAD、`NO [UNAVAILABLE]`、无标签 BAD、`Handle` 一直失败）、`TestWatcherWakeEndsIdle`、`TestWatcherWakeEndsPoll`、`TestWatcherKeepsWakeFromScan`、`TestWatcherKeepsWakeDuringHandleBackoff`、`TestWatcherScanned`（3 例）、`TestWatcherSkipHistory`（4 例）、`TestWatcherSkipHistoryAsksAfterExamine`、`TestWatcherDefer`（INBOX、Junk、「已发送」）。原有测试只有 `TestCloseLogsOut` 随内部 `sleep` 的签名多传一个 `nil`；头部取回用的 `FetchItemBodySection` 字面量本身带 `Peek: true`，`source_test.go` 的白名单不需要扩展。
- **变异测试：** 契约点名的 5 个（「已发送」排到 INBOX 之后、照 Junk 降级、`ErrDefer` 当普通错误处理、`Wake` 不结束 IDLE、`Scanned` 在失败时也调用）与另外 41 个全部被杀死。另外 41 个涉及：`Scanned` 的调用条件与开始时刻（取结束时刻、取 `time.Now`、取重试的开始、每批都调用）；「已发送」的失败处理（`resumeInbox` 不置或不清、连接层失败不报、失败时拆连接、LIST 中缺失时仍 EXAMINE 或不报、本地失败无限重试或只试一次、取整封）；`ErrDefer`（用 `==` 比较、把错误交给调用方、调用 `Scanned`）；`Wake`（等待前不检查已有信号、`Poll` 不响应、处理失败的退避被打断、`woken` 恒真、不发 DONE）；`SkipHistory`（不看 UIDNEXT、已有游标也跳过、游标差一、不置 `Reset`、在 EXAMINE 之前询问）；头部取回（取整封、按整封大小判过大、上限误用 `maxMessageSize`、合计上限的两种错误预计、部分取回长度改为上限）；`readLiteral` 的 4 个（`limit+1` 改为 `limit`、去掉短读检查、过大时仍返回已读字节、忽略读取错误）。每次恢复后 SHA-256 与原文件相同。
- **按字面解释或补充的地方：**
  1. `MaxHeaderSize`（64 KiB）导出，仿 `MaxMessageSize` 另有包内变量供测试调小。头部大小事先未知，不按整封的声明大小跳过取回；取下一封之前的合计上限检查按 `min(声明大小, maxHeaderSize)` 预计（生产中 50 × 64 KiB 远小于 16 MiB，这条检查实际不会触发）。`TooLarge` 的邮件 `Size` 仍是整封的 RFC822.SIZE。
  2. 「已发送」的连接层失败（超时、断开）同样算失败、发出 `folder_unavailable`，与 Junk 对 `ErrClosed` 的例外不同：对 D6 而言任何失败的一轮都拿不到证据，必须计入 `sent_unavailable`。失败且连接已被关闭时，重连后的那一轮跳过「已发送」、从 INBOX 继续（`resumeInbox`），否则「已发送」持续超时会使 INBOX 永远轮不到补扫，做不到契约的「不影响 INBOX」。
  3. 「已发送」的本地失败（`Handle` 返回错误）每轮至多重试 3 次（`sentFailLimit`），从不降级。
  4. Junk 返回 `ErrDefer` 不算失败，Junk 的失败计数清零（说明它可以访问）；`ErrDefer` 不复位本地重试的退避，不发任何状态。
  5. `Wake` 只在等待时读取。等待前已有的信号：取走、不发 IDLE，也不算 IDLE 正常结束（与 4a 审查后修正②对 EXISTS 的处理一致）；IDLE 中收到信号：发 DONE，按正常结束复位重连退避；退避与重连等待期间到达的信号留在通道里（重连之后多补扫一轮，无害）。
  6. `Scanned` 对每个文件夹都调用；开始时刻取该文件夹本轮第一次读游标之前，同一连接内重试过也取第一次尝试之前；`Scanned` 为 nil 时不调用 `Now`；跳过历史的那一轮也调用（按字面）。
  7. `SkipHistory` 在 EXAMINE 返回之后才询问（按契约目的补充）：通知恰在 EXAMINE 途中发出时，其副本计入 UIDNEXT、被新游标越过，若在 EXAMINE 之前询问、得到「库中没有通知」，这条通知就永远拿不到实际投递 ID。已有游标或服务器没有报告 UIDNEXT 时不询问。跳过历史的批 `Reset` 为真、没有邮件、`More` 为假。Task 13 的 `SkipHistory` 因此必须在每次调用时实时查询 `HasNotifications`，不能在启动时缓存结果（已写入 Task 13 的契约）。
  8. `5215fb5` 的「短读即按连接断开、不再调用 `Next`」检查提取为纯函数 `readLiteral`，正文与头部共用 `fetchSection`，头部因此同样受它约束。实测端到端的截断只有 `-race` 偶尔能看出：单次场景 `-race -count=10` 的 20 次子用例中 3 次报告竞争、每个子用例重复 25 轮时 20 次中 8 次、原有的 `TestScanBodyLiteralCutShort` 20 次中 2 次、头部版 0 次，不带 `-race` 都是 0 次；因此以 `TestReadLiteral` 确定地钉住边界（`limit+1` 改为 `limit` 的变异不带 `-race` 也确定失败），端到端用例保留作辅证。
- **既有问题（未改）：** 补扫 Junk 期间到达 INBOX 的新邮件，重新 EXAMINE INBOX 时服务器报告的 EXISTS 被 SelectData 吸收、不留下信号，要等到 `IdleMax` 或 `Wake` 才补扫；「已发送」排在 INBOX 之前没有带来新的这类窗口。
- **验证：** `make check`（`internal/mail/imap` 的 `-race -covermode=atomic` 覆盖率 98.1%，总覆盖率 93.59%）、`go test -race -count=2 ./internal/mail/imap/`、`CGO_ENABLED=0 go test ./internal/mail/imap/`、`make secrets`、`GOOS=linux go vet ./...`、`GOOS=windows go vet ./...` 通过，无不可见字符；拣选到集成分支后 `make check`（总覆盖率 94.26%）与 `make secrets` 再次通过。未运行：govulncheck（云端网络策略拒绝 vuln.go.dev）。

### Task 7：QQ 行为模拟器（`tests/qqsim`）

**Files:**
- Create: `tests/qqsim/qqsim.go`、`tests/qqsim/qqsim_test.go`
- Modify: `docs/en/architecture.md`、`docs/zh-CN/development.md`（go-imap/v2、go-smtp、go-sasl 的「使用方」一栏点名 `tests/qqsim`；`qqsim.go` 不是测试文件，`tests/docs/deps_test.go` 会扫描它）

**契约：** 一个进程内的离线假服务器，供 `internal/app`、`internal/cli` 与 `tests/e2e` 的测试共用；只用 go-imap/v2 的 `imapserver`、`imapmemserver` 与 go-smtp 的服务端（其 AUTH 需要 go-sasl 的 `sasl.Server`），不联网，监听 `127.0.0.1` 的随机端口，隐式 TLS 使用测试时生成的自签 CA（调用方经 `RootCAs` 信任它）。

- 一个机器人账户（地址与授权码由测试给出，授权码在运行时构造），文件夹 INBOX、Junk、`Sent Messages`。
- SMTP 收下邮件时按 L1 实测的 QQ 行为：把 `Message-Id` 改写为 `<tencent_<随机十六进制>@qq.com>`，原 ID 写入 `X-OQ-MSGID`，改写后的副本追加到「已发送」；DATA 的 250 响应不含 ID（go-smtp 服务端固定回 `OK: queued`，与 QQ 实测的 `OK: queued as.` 一样不含 ID）；收件人副本放入 `Delivered()` 通道，供测试据此构造回复。
- 测试辅助：`Reply(delivered, from, subjectPrefix, body)` 按 QQ 邮箱 App 的结构（Task 3 的样本）合成回复；`Deliver(folder, raw)` 把任意字节追加到 INBOX 或 Junk。
- 故障注入：AUTH 失败、MAIL、RCPT、DATA 命令与结束标记之后的 4xx 与 5xx（结束标记之后的 550 即 L1 实测的同域收件人不存在）、收到结束标记后不回响应即断开（产生 `ErrUncertain`）、「已发送」副本延迟 N 秒写入或不写入（模拟关闭「保存到已发送」）、「已发送」的 EXAMINE 被拒绝（模拟文件夹不可用）、LIST 中没有「已发送」、IMAP 登录失败、立即断开当前全部 IMAP 与 SMTP 连接（断网恢复用）。
- 模拟器是测试基础设施，放在 `tests/` 下，不进入产品二进制；它有自己的测试，计入覆盖率。

**测试（先写并确认失败）：** 用 `internal/mail/smtp.Send` 发一封信后，「已发送」中出现改写了 ID 的副本且 `X-OQ-MSGID` 等于原 ID；`imap.Session` 能登录、`ScanHeaders` 取到副本；每种故障注入都产生对应的客户端错误分类（`ErrAuth`、`*ReplyError` 4xx/5xx、`ErrUncertain`、`ErrAuthFailed`、`ErrNoFolder`、断开后的 `ErrClosed`）；延迟写入的副本在延迟之后才出现。

- [ ] **Step 1：** 写失败的测试。
- [ ] **Step 2：** 实现；更新两份依赖表。
- [ ] **Step 3：** `make check`、`make secrets` 通过。
- [ ] **Step 4：** `git commit -m "test(qqsim): offline QQ Mail simulator with Message-ID rewriting and fault injection"`

### Task 8：单实例锁、密钥读取与授权码缓存（`internal/app`）

**Files:**
- Create: `internal/app/lock.go`、`lock_unix.go`、`lock_other.go`、`secrets.go` 及测试

**契约：**

- `AcquireLock(dataDir string) (*Lock, error)`：以 `O_RDWR|O_CREATE|O_NOFOLLOW`、0600 打开 `<数据目录>/turncourier.lock`，`flock(LOCK_EX|LOCK_NB)`；被占用返回 `ErrAlreadyRunning`，锁文件是符号链接或不是普通文件时返回错误（错误文本不含路径）。`(*Lock) Close()` 释放。非 Unix 平台（`lock_other.go`）返回 `ErrUnsupported`。锁文件留在数据目录中，不删除（删除会让另一个进程在旧 inode 上持锁，形成两把锁）。flock 在网络文件系统上不可靠，development.md 写明数据目录须在本地磁盘。
- `LoadSecrets(ctx, kc keychain.Store, st *sqlite.Store, instanceID string) (*Secrets, error)`：读取授权码与两类 active 密钥（`ActiveKeyID`），每把密钥解码后用 `keychain.KeyCheck` 与 `KeyCheckOf` 比对，不符返回 `ErrKeyMismatch`（「Keychain 中的密钥与登记不符」）；Keychain 读取超时或 `ErrInteractionNotAllowed` 包装为 `ErrKeychainUnavailable`。`Secrets` 持有 `*token.Key`（按 kid）、`*payload.Key` 与授权码缓存，格式化输出（`String`、`Format` 的全部动词、`LogValue`）一律脱敏；`*Secrets` 实现 `renderer.Scrubber`：`Scrub(text)` 把授权码与两把密钥的文本原样出现处换成「[已省略]」，机密不以切片等形式离开 `Secrets`。
- 授权码缓存：`(*Secrets) AuthCode(ctx)` 返回缓存值；`Invalidate()` 清空缓存，下一次调用重新读取 Keychain（认证失败后、熔断结束时由 Task 11 调用）。`Scrub` 不用这份缓存，而用另存的「最近读到的机密」：启动时与每次重新读取时更新，`Invalidate` 不清空它，否则认证失败之后渲染的第一封通知会漏掉授权码的抹除。
- Keychain 的期限：`internal/app` 在收发循环中用 `keychain.New(keychain.DefaultTimeout)`（10 秒，后台不等人输入）；`init` 的往返测试沿用 init 已有的交互期限（`InteractiveTimeout`）。

**测试（先写并确认失败）：** 同一进程两次加锁第二次失败、`Close` 后可再次加锁；以测试二进制再启动一个子进程加锁，父进程持锁时子进程得到 `ErrAlreadyRunning`；锁文件为符号链接时拒绝、权限为 0600；`LoadSecrets` 用 4a 的假 Keychain：正常、条目缺失、校验值不符、交互不允许、超时；`Secrets` 的 `%v`、`%+v`、`%#v`、`slog` 输出不含授权码与密钥，放进 `renderer.Options` 之后以 `%+v`、`%#v` 打印 `Options` 同样不含；`Scrub` 抹去三种机密，`Invalidate` 之后仍抹去授权码；`Invalidate` 之后重新读取。

- [ ] **Step 1：** 写失败的测试。
- [ ] **Step 2：** 实现。
- [ ] **Step 3：** `make check`、`make secrets` 通过；`GOOS=windows go vet ./...` 通过（`lock_other.go`）。
- [ ] **Step 4：** `git commit -m "feat(app): single-instance lock and verified secrets at startup"`

### Task 9：入站验证流水线（`internal/app/inbound.go`）

**Files:**
- Create: `internal/app/inbound.go`、`inbound_test.go`
- Modify: `tests/docs/imports_test.go`（「产品代码中没有包同时导入邮件与存储」改为「只有 `internal/app`」）

**契约：** `(*inbound) Handle(ctx, b imap.Batch) error` 与 `Cursor(ctx, folder)` 接到 `imap.Watcher`；「已发送」的批交给 Task 10。INBOX 与 Junk 的每封邮件按 UID 升序依次经过下列步骤，第一步不通过即以括号中的原因码调用 `RecordRejection`，并继续处理下一封。被拒记录只记元数据：账户、文件夹、UIDVALIDITY、UID，以及通过了存储校验的 Message-ID 与规范化发件人（不通过的留空），和已由通知行确认的任务 ID（经 nid 或实际投递 ID 找到的通知所属的任务；主题标签里的任务 ID 未经验证，而 `RecordRejection` 对不存在的任务返回 `ErrNotFound`）。错误的分类见「已定的实现细节」中的「单封来信的错误」：来信内容引起的失败一律成为原因码，只有基础设施错误让 `Handle` 返回错误；每封的处理包在 `recover` 中。

0. 该封已有被拒记录（`RejectionExists`，按文件夹、UIDVALIDITY 与 UID）→ 跳过，不重新判定：批因基础设施错误被重新交付时，已拒绝的邮件不会因时间窗移动等原因被翻案。第 2 步取得合法的 Message-ID 之后再查一次 `RejectedBeforeReset`，命中同样跳过：UIDVALIDITY 重置后的全量补扫中 UID 都变了，否则迟到的副本、白名单的修改或 `ResumeMail` 会让早已拒绝的来信在有效期内被接受。
1. `Raw` 为 nil（过大）→ `too_large`。
2. `parser.Parse` 失败，或 `Message-Id` 缺失、不符合存储的规则（`sqlite.ValidMessageID`）→ `malformed`。
3. `parser.Classify`：退信 → `bounce`；自动回复 → `auto_reply`。**这一步先于任何标签与令牌处理**（D4）。两者都只按线程头尽力关联任务（`NotificationByDeliveredID`；`ValidMessageID` 为假的 ID 直接跳过），关联到时发出 `notification_bounced` 或 `auto_reply_received` 事件，不解析主题中的令牌。
4. 发件人规范化后不在 `allowed_senders` 中 → `sender_not_allowed`。
5. `token.ParseSubject`：→ `subject_tag_missing`、`subject_tag_multiple`、`subject_tag_damaged` 或 `token_invalid`。
6. `NotificationByNID` 不存在 → `token_invalid`。
7. 标签中的任务 ID 与通知的任务不同 → `subject_tag_invalid`。
8. 令牌的 kid 不等于通知的 `TokenKeyID`，或该 kid 不是 active、不在已读出的密钥中 → `token_invalid`。
9. 线程：通知的实际投递 ID 出现在 `In-Reply-To` 或 `References` 中才通过（比较前两边都规范为带尖括号的形式，按字节比较）。不通过时：
   - 回复完全没有线程头 → 立即 `thread_mismatch`；
   - 通知尚无实际投递 ID、状态为 SENT、SENDING 或 UNCERTAIN，且还没有「副本确实不在」的证据（D6：没有哪次**成功完成**的「已发送」补扫开始于通知最近一次状态变化 10 分钟之后）→ **延后**：把游标持久化到这封之前（这封是本批第一封时不推进），安排 30 秒后唤醒 Watcher，返回包装 `imap.ErrDefer` 的错误，本批余下的邮件留到下一轮；
   - 其余情况 → `thread_mismatch`；因证据成立而拒绝的另发 `sent_copy_missing` 事件。
10. `NewText`：`ErrNoPlainText` 与 `ErrUncertain` → `parse_uncertain`；`ErrEmpty` → `empty_body`；`ErrMalformed` → `malformed`。
11. 用该 kid 的令牌密钥计算 `BodyDigest`；`InboundByMessageID` 命中且任务与摘要都相同 → 判为重复，**不写被拒记录**，继续下一封；命中但任一不同 → `message_conflict`，并发出事件。去重排在有效期、令牌新旧、暂停与任务状态之前，UIDVALIDITY 变化后的全量补扫因此不会把早已处理的回复误报为 `token_expired`、`token_superseded` 或 `mail_paused`。
12. `token.Verify`（按本地处理时间）：`ErrBadMAC` → `token_invalid`；`ErrExpired` → `token_expired`；`ErrInvalidClaims` → `token_invalid`，并发出数据损坏事件。
13. 令牌新旧（D4 已确认，「最新」与缺失证据按 D7）。令牌所属的通知此时必为 SENT（第 9 步要求它有实际投递 ID）。`LatestSentNotification(任务)` 不是它 → `token_superseded`（有更新的、确已发出的通知）；否则 `LatestAttemptedNotification(任务, 最近一次成功补扫「已发送」的开始时刻 − 10 分钟)` 不是它（更新的只有尚无缺失证据的 UNCERTAIN 通知）→ **延后**，做法同第 9 步；两者都是它 → 通过。
14. 该任务的邮件触发已暂停（`MailPaused`）→ `mail_paused`。
15. 回环刹车（D8）：该任务滚动 1 小时内已接受 10 封，或滚动 24 小时内已接受 40 封（都只计最近一次解除之后的回复，解除时刻由 `MailPaused` 给出）→ `rate_limited`，同时 `PauseMail`（原因 `hourly` 或 `daily`）并发出 `mail_paused` 告警。
16. `RecordReply`：新入队或判为重复都算成功；任务状态不接受回复时存储记为 REJECTED 的回复（原有语义），发出事件；`ErrMessageConflict` → `message_conflict`；`sqlite.ErrInvalidArgument` → `malformed`；`ErrPayloadKeyUnavailable` 与数据库错误让 `Handle` 返回错误（Watcher 按退避重试同一批，发出 `keychain_unavailable` 或 `store_error` 事件）。

全部处理完后 `AdvanceCursor(账户, 文件夹, b.Next)`。判定核心（第 2–15 步）是只读的：它经一个小接口读取通知（按 nid、按实际投递 ID、最新的 SENT 通知与「最新通知」）、密钥状态、入站记录、UIDVALIDITY 重置前的被拒记录（`RejectedBeforeReset`）、暂停状态与回复计数，最近一次成功补扫「已发送」的开始时刻与当前时刻作为参数传入；返回判定结果（入队、重复、延后，或以原因码拒绝；第 15 步另带「须暂停」）；被拒记录、`PauseMail` 与 `RecordReply` 等写入由流水线在核心之外执行。核心不直接依赖 `*sqlite.Store`，Task 15 的往返测试用内存实现复用它。错误与事件都不含主题、正文、令牌与地址。

**测试（先写并确认失败）：** 用真实 SQLite、Task 3 的合成样本与 Task 7 模拟器的 `Reply` 构造：

- 每个原因码至少一个用例；**来自白名单地址、线程与标签令牌全部正确的假期自动回复被判为 `auto_reply`**（D4 的承重规则）；退信关联到通知；同一封信在 INBOX 与 Junk 各一份只入队一次；UIDVALIDITY 变化后的全量补扫中，过期、已被取代与所属任务已暂停的旧回复都判为重复，以 `thread_mismatch`、`sender_not_allowed` 与 `rate_limited` 拒绝过的来信不被重新判定（其间修改了白名单、执行了 `ResumeMail` 也如此），而同一 UIDVALIDITY 下与一封被拒来信 Message-ID 相同的另一封信照常判定。
- **单封来信不能卡住收取：** 下列来信都被拒绝并推进游标，批不失败——伪造的白名单发件人配上形状合法的标签（随机任务 ID、版本 1、kid 不为 0 的随机令牌，不需要任何机密）；Message-ID 为 `<>`、超过 998 个字符或非法 UTF-8；线程头中含不合法的 ID；声明 utf-8 而实为非法字节的正文；让解析器 panic 的输入（以测试替身注入）。反过来，查询因数据库错误失败时批失败、游标不前进：只有 `ErrInvalidArgument` 按找不到处理。
- 批因数据库忙被重新交付时，已以 `rate_limited`、`sender_not_allowed`、`thread_mismatch` 拒绝的邮件不被重新判定。
- 延后：没有实际投递 ID 时（SENT、SENDING、UNCERTAIN 各一例）返回 `ErrDefer`、游标停在前一封、记下实际投递 ID 后下一轮通过；「已发送」一直补扫失败时继续延后而不拒绝；成功的补扫开始于 10 分钟之后仍没有副本时 `thread_mismatch`；没有线程头的回复立即 `thread_mismatch`；较新的通知为 UNCERTAIN 时，回复上一条通知在缺失证据成立之前延后（`ErrDefer`），证据成立后被接受，那条通知改为凭副本核对为已送达时则以 `token_superseded` 拒绝；更新的通知中有一条 SENT 时直接 `token_superseded`（即使还有更新的 UNCERTAIN）。
- 回环刹车：第 11 封（1 小时）与第 41 封（24 小时，注入时钟模拟慢循环）被 `rate_limited` 拒绝并暂停，此后的来信为 `mail_paused`；`ResumeMail` 之后恢复接受（时钟不跨过窗口：解除之前的回复不再计入），再接受 10 封后再次暂停。存储按毫秒记时，刹车只计「不早于解除时刻」的回复，与解除同一毫秒接受的回复也计入（偏严的一侧），所以这个用例在回复、暂停与解除之间各让时钟前进至少 1 毫秒；完全冻结的时钟下，用例的前后两半不可能同时成立（2026-09-24 Task 2 规格审查指出，措辞随之修订）。
- 关闭的任务得到 REJECTED 回复；正文密钥不可用时 `Handle` 返回错误且不写任何记录；金丝雀：事件与错误中不出现主题、正文、令牌、地址。
- 变异测试：把第 3 步移到第 5 步之后（自动回复用例应失败）、去掉第 0 步、去掉第 11 步（补扫用例应失败）、去掉第 13 步、延后不要求证据（按固定时长拒绝）、只按小时计的刹车、不暂停、刹车计入解除之前的回复、`ErrInvalidArgument` 以外的查询错误也按找不到处理、去掉 `RejectedBeforeReset`、较新的 UNCERTAIN 没有证据时直接拒绝而不延后、第 13 步只查 `LatestAttemptedNotification`（中间夹着 SENT 通知的用例应失败）。

- [ ] **Step 1：** 写失败的测试。
- [ ] **Step 2：** 实现；修改 `tests/docs/imports_test.go`。
- [ ] **Step 3：** `make check`、`make secrets` 通过，变异测试逐条记录。
- [ ] **Step 4：** `git commit -m "feat(app): inbound validation pipeline with auto-reply filter first"`

### Task 10：「已发送」副本（`internal/app/sent.go`）

**Files:**
- Create: `internal/app/sent.go`、`sent_test.go`

**契约：** 「已发送」批中的每封用 `parser.ParseHeader` 只读头部：我方 ID 取自 `X-TurnCourier-ID`，没有时退回 `X-OQ-MSGID`（L1 实测它等于我方 Message-ID）；我方 ID 与副本的 `Message-Id` 任一缺失或不符合存储的规则（`sqlite.ValidMessageID`）、头部无法解析的，一律忽略；`NotificationByMessageID` 找不到或返回 `ErrInvalidArgument` 的忽略（不是本实例发出的），查询的其他错误让 `Handle` 返回错误、整批重试——当作找不到会越过这份副本、推进游标，该通知就再也拿不到实际投递 ID。单封副本的任何问题都不让批失败（「单封来信的错误」）。找到时按通知状态：

| 状态 | 处理 |
| --- | --- |
| SENT，尚无实际投递 ID | `RecordDeliveredMessageID(副本的 Message-ID)`，以带尖括号的形式记录 |
| SENT，已记录且相同 | 不改动 |
| SENT，已记录但不同 | 发出 `delivered_id_conflict` 事件，不改动 |
| UNCERTAIN | 按 D7：`ResolveUncertainAsDelivered(id, 副本的 Message-ID)`（同一事务），发出 `notification_resolved_by_copy` 事件 |
| SENDING | 发送循环尚未把它标为 SENT：游标停在这封之前，安排 30 秒后唤醒 Watcher，返回 `imap.ErrDefer` |
| PENDING、ABANDONED | 发出 `notification_copy_unexpected` 事件（可能已被人工改回待发送），不改动 |

写入实际投递 ID 时的两种拒绝（2026-09-24 Task 2 规格审查后补充）：`RecordDeliveredMessageID` 或 `ResolveUncertainAsDelivered` 返回 `ErrDeliveredMessageIDConflict`（副本的 Message-ID 已记在另一条通知上）时发出 `delivered_id_conflict` 事件、不改动，照常处理下一封并推进游标；返回 `queue.ErrInvalidOutboxTransition`（读取与写入之间通知的状态已变，例如发送循环刚把它从 SENDING 标为 SENT）时重新读取该通知，按新状态再查一次上表；仍然被拒则发出事件并跳过这份副本。两者都不是基础设施错误：按「其余存储错误」处理会让整批无限重试，「已发送」的游标、D6 的证据与其后的回复都随之停摆。

处理完成后推进「已发送」的游标。Watcher 的 `Scanned` 回调记下「已发送」最近一次成功补扫的开始时刻，供 Task 9 第 9 步判断证据；每次成功补扫之后以 `SentWithoutDeliveredID(本轮开始时刻 − 10 分钟, afterID)` 从 `afterID = 0` 起逐页取完（下一页的 `afterID` 取上一页最后一行的 id），用进程内已报告的通知 ID 集合去重，每条通知在本进程生命周期内只计入一次；每轮把新发现的缺失合成一条带计数（`Count`）的 `sent_copy_missing` 事件，不逐条发出——关闭「保存到已发送」时积压只增不减，逐条发出会在每次重启时刷屏（2026-09-24 Task 2 复查的细节项，主会话定为聚合）。不以「已报告的最大 id」作跨轮游标：id 较小的通知可能因重新排队而晚于 id 较大的通知发出，也可能较晚才被本地核对为已送达，越过 `sentBefore` 时游标已在它之后，它就永远不会被报告（2026-09-24 Task 2 修复与复查时指出）；每轮重读的代价随「保存到已发送」关闭后的积压线性增长，受 D8 每小时至多 40 封约束；连续 3 轮补扫失败（含「已发送」不在 LIST 中）时发出一次 `sent_unavailable`，恢复后清零。

**测试（先写并确认失败）：** 表中每一行；不是本实例的副本、无法解析的头部、不合法的 Message-ID 与不合法的我方 ID（超长、非法 UTF-8）都被忽略且批不失败；查询因数据库错误失败时批失败、游标不前进；SENDING 的副本延后，30 秒后重试；游标推进；`Scanned` 只在成功时更新证据时刻，且只认 `Scanned(FolderSent, …)`：「已发送」补扫失败或被延后、而 INBOX 与 Junk 照常完成的一轮，以及 `Scanned(INBOX)`、`Scanned(Junk)` 的调用，都不改变证据时刻（变异：任何文件夹的 `Scanned` 都记为证据，这条用例应失败；2026-09-24 Task 6 规格审查补充）；`sent_copy_missing` 按轮聚合、每条通知在进程内只计入一次，`sent_unavailable` 只发一次；因重新排队而较晚发出、id 较小的通知与较晚才被本地核对为已送达的通知都被计入一次，下一轮不重复（变异：以已报告的最大 id 作跨轮游标，这条用例应失败）；D7 的解除只在副本存在时发生，没有副本的 UNCERTAIN 保持不变，解除与记录在同一事务中（注入失败时两者都没有生效）。

- [ ] **Step 1：** 写失败的测试。
- [ ] **Step 2：** 实现。
- [ ] **Step 3：** `make check` 通过。
- [ ] **Step 4：** `git commit -m "feat(app): record delivered Message-IDs from Sent copies"`

### Task 11：发送循环与认证熔断（`internal/app/outbound.go`、`breaker.go`）

**Files:**
- Create: `internal/app/outbound.go`、`breaker.go` 及测试

**契约：**

- 触发：创建通知后与启动时各唤醒一次，另有 30 秒的定时检查（`not_before` 到期的通知）。
- 每次循环：熔断打开或发送循环处于暂停时等待到结束；滚动 1 小时内已发 40 封（D8）时发出一次 `send_rate_limited` 事件并每分钟重查；否则 `ClaimNextNotification`。每次领取之前以 `AbandonedSince` 取出上次检查以来因令牌将过期或任务关闭而被放弃的通知（同一轮内逐页以上一页最后一行的 `(updated_at, id)` 为游标，返回满 100 条时继续取；下一轮从上一轮最后一行那一毫秒的 id 0 起重读，并以进程内记下的、该毫秒已提示过的通知 ID 去重，这样不会漏掉「同一毫秒内先读、后放弃一条 id 更小的通知」；进程启动时的初始游标是启动时刻与 0，不把历史上的放弃再提示一遍。这是尽力而为的本地提示，已知的剩余缺口：时钟回拨期间放弃的通知落在游标之前；上一进程最后一次检查之后、停机期间发生的放弃不再提示；一页恰好满 100 条、两次取页之间先后提交了同一毫秒 id 更小与下一毫秒的放弃（需要一毫秒内超过 100 条放弃，可以忽略）），各发出一次 `notification_expired` 或 `notification_dropped` 事件。领取的结果：
  - `ErrNoSendableNotification` 等待下一次触发；
  - `ErrPayloadMissing` 或包装 `payload.ErrDecrypt` 的错误（返回值带着该通知未改动的 PENDING 快照）→ 按「本地的永久失败」`AbandonNotification(rejected)`，发出带通知 ID 的事件，继续领取下一条；
  - `ErrPayloadKeyUnavailable` → 发出 `payload_unavailable` 事件后等待定时检查，不空转、不放弃。
- 领取成功后：`DecodeContent` 失败或渲染 `ErrTooLarge` → `AbandonNotification(rejected)` 并发出事件；令牌密钥不可用 → 重新排队 15 分钟；`token.Issue`（Claims 取自通知行）→ `token.NewTag` → `ThreadReferences(任务, 通知 ID, 20)` → `renderer.Render`（模式取 `notify.content`，`Scrub` 取 `*Secrets`）→ 自检信封（来自配置，已校验）与大小 → `smtp.Send`。
- 结果按「已定的实现细节」分流：250 → `MarkNotificationSent`，唤醒 Watcher（D6），发出 `notification_sent`，连续失败次数清零；`ErrAuth` → 打开熔断、`Invalidate` 授权码、重新排队到熔断结束之后；`*ReplyError` 4xx → 按退避重新排队；5xx → `attempts` 未到 3 时按退避重新排队，到 3 时 `AbandonNotification(rejected)`；RCPT 的 5xx 发出 `recipient_rejected`，提交阶段的 550 发出 `submission_rejected`，每条通知只发一次；`ErrUncertain` → `MarkNotificationUncertain`，唤醒 Watcher，发出事件；其余 `ErrNotSent` → 按退避重新排队。4xx、5xx、`ErrUncertain` 与其余 `ErrNotSent` 之后连续失败次数加 1，整个发送循环按它暂停。
- **领取之后的每一次状态写入**（`MarkNotificationSent`、`MarkNotificationUncertain`、`RequeueNotification`、`AbandonNotification`）失败时，都用一个不随 ctx 取消的上下文重试 3 次（间隔 1 秒）；仍失败则发出 `store_error` 并让发送循环以错误结束，`Run` 随之返回错误。不这样做，停在 SENDING 的通知会让之后每一次领取都得到 `ErrNoSendableNotification`，发送循环在没有任何事件的情况下永久停摆；「已发送」中若已有它的副本，Task 10 每轮都对它返回 `ErrDefer`，实际投递 ID 与 D6 的证据也都停了。通知停在 SENDING，重启后由 `RecoverSendingNotifications` 转为 UNCERTAIN：确实已发出的由 D7 凭副本解除；其实没有发出的（例如 4xx 之后重新排队失败）找不到副本，停在 UNCERTAIN 等待人工核对——这是「从不自动重发」的代价，缺失证据成立后它也不再让上一条通知的令牌失效（D7）。
- 熔断：`breaker` 由发送循环与收取循环共享。收取循环的 `auth_failed` 状态同样打开它；熔断期间 Watcher 的 `Password` 回调返回内部错误 `errBreakerOpen`，Watcher 因此不登录；熔断结束后第一次取授权码重新读取 Keychain。装配层据 `errBreakerOpen` 把随后的 `credentials_unavailable` 报告为 `auth_circuit_open`，不误报为 `keychain_unavailable`。
- 组装好的主题与 MIME 字节只作为局部变量存在，不进入事件、错误与任何结构体字段。

**测试（先写并确认失败，用 Task 7 的模拟器）：** 正常发送后通知为 SENT、「已发送」中有副本且 Watcher 被唤醒；每种 SMTP 故障的分流结果与退避时长（注入时钟），4xx、5xx 与 `ErrUncertain` 之后下一条通知不会被立即领取；持续 4xx 时发送循环的暂停依次为 1、2、4……60 分钟，250 之后清零；持续 5xx 时每条通知第 3 次尝试才放弃；关闭一个积压 150 条通知的任务时每条恰好一个 `notification_dropped`，令牌将过期而放弃时发出 `notification_expired`，同一毫秒内先读、后放弃一条 id 更小的通知仍被提示一次，启动之前的放弃不提示（变异：下一轮直接以最后一行的 `(updated_at, id)` 为游标、不去重，这些用例应失败）；RCPT 的 5xx 发出 `recipient_rejected`、提交阶段的 550 发出 `submission_rejected`，同一通知重试三次也只发一次；认证失败后 15 分钟内不再领取、不登录 IMAP，结束后重新读取授权码；第 41 封被推迟而不是放弃；正文缺失与无法解密的通知被放弃并发出事件，其后的通知照常发出；正文密钥不可用时不空转；领取之后的四种状态写入各自持续失败时 `Run` 都返回错误，重启后该通知为 UNCERTAIN，只失败两次时发送循环照常继续；第二封通知的 `References` 含第一封的实际投递 ID；`notify.content = "status"` 时主题与正文都不含 `Title` 与 `Body`；金丝雀：事件与错误中不出现主题、令牌、正文与地址，出站 MIME 字节只交给 SMTP，`Scrubber` 抹去的机密不出现在出站字节中。变异测试：先按 5xx 再判 `ErrAuth`（认证失败用例应失败）、`ErrUncertain` 改为重新排队（应失败）、去掉发信上限、去掉整体暂停、暂停按通知的 `attempts` 计（持续限流用例应失败）、5xx 第一次就放弃、只对 `MarkNotificationSent` 重试、`ErrDecrypt` 不放弃。

- [ ] **Step 1：** 写失败的测试。
- [ ] **Step 2：** 实现。
- [ ] **Step 3：** `make check`、`make secrets` 通过，变异测试逐条记录。
- [ ] **Step 4：** `git commit -m "feat(app): send loop with outcome mapping, rate limit and shared auth breaker"`

### Task 12：派发循环、Agent 契约与任务事件（`internal/app/dispatch.go`、`agent.go`）

**Files:**
- Create: `internal/app/dispatch.go`、`agent.go` 及测试

**契约：** 本任务的类型不依赖 Task 13 的 `App`：`dispatcher` 与 `taskEvents` 各自持有存储、配置与唤醒函数，Task 13 把它们装进 `App` 并以同名方法对外暴露。

- `Agent` 接口与 `ErrNotDelivered` 见「已定的实现细节」。
- 派发循环（`dispatcher`）：由回复入队、任务进入可派发状态与启动时唤醒，另有 30 秒的定时检查。对 `TasksWithQueuedReplies` 返回的每个任务 `ClaimNextReply`：
  - `ErrNoDispatchableReply` 跳过；
  - `ErrPayloadMissing`、`ErrPayloadKeyUnavailable` 或包装 `payload.ErrDecrypt` 的错误：回复停在队首、需要人工处理（4a 的约定），发出带回复序号的 `reply_payload_unavailable` 事件（序号取自 `ClaimNextReply` 返回的 QUEUED 快照，Task 2），该任务在本进程中不再重试，直到下一次启动；
  - 领取成功 → `Agent.Deliver`（期限 2 分钟）：nil 则 `AcknowledgeReply`；`ErrNotDelivered` 则 `RequeueUnsentReply`，该任务 1 分钟内不再派发——若它以包装 `task.ErrInvalidTransition` 的错误拒绝（任务自派发以来已有其他事件，说明 Agent 已处理该回复），改为 `AcknowledgeReply`（`replies.go` 的约定，防重复投递的关键判定）；其他错误则 `MarkReplyUncertain`，发出事件。交付后尽力清零正文缓冲。
- `taskEvents.StartTask(ctx, agent, sessionID)`：`CreateTask` 加 `StartTask`，供合成 Agent 与验收工具使用。
- `taskEvents.TaskEvent(ctx, taskID, event, title, body)`：`ApplyTaskEvent`（版本冲突时重读重试一次）；事件对应的通知事件（turn_completed、waiting_input、waiting_approval、failed）在 `notify.events` 中时，编码 `renderer.Content` 并 `CreateNotification`（域名取机器人地址，TTL 取 `security.token_ttl`），唤醒发送循环；任务进入 COMPLETED 或 WAITING_INPUT 时唤醒派发循环。

**测试（先写并确认失败）：** 合成 Agent 的三种返回各自的回复与任务状态；`RequeueUnsentReply` 因任务已有其他事件而拒绝时改为确认、后续回复不被阻塞；三种正文错误不空转且只发一次事件；忙时（RUNNING）收到的多封回复按入队顺序逐一派发，每回合结束后才派发下一封（FIFO）；WAITING_APPROVAL 时不派发；未订阅的事件不创建通知；通知事件名的映射。

- [ ] **Step 1：** 写失败的测试。
- [ ] **Step 2：** 实现。
- [ ] **Step 3：** `make check` 通过。
- [ ] **Step 4：** `git commit -m "feat(app): dispatch loop, minimal agent contract and task events"`

### Task 13：装配、启动恢复与本地事件（`internal/app/app.go`、`events.go`）

**Files:**
- Create: `internal/app/app.go`、`events.go` 及测试

**契约：**

- `New(Options) (*App, error)`：`Options` 含配置、Keychain、Agent、事件回调，以及只供测试注入的时钟、随机源、根证书池、SMTP 与 IMAP 期限、退避参数。`(*App) StartTask` 与 `(*App) TaskEvent` 转交 Task 12 的 `taskEvents`。
- `(*App) Run(ctx) error`：按顺序 ① 取得单实例锁；② 以无正文密钥打开存储读取实例 ID（没有则提示先运行 `init`），`LoadSecrets` 核对密钥，关闭后以正文密钥重新打开；③ `RecoverInFlight` 与 `RecoverSendingNotifications`，结果作为事件发出；④ 启动 Watcher（`Sent: true`，容量为 1 的唤醒通道——从不关闭：关闭的通道永远就绪，Watcher 会不停补扫；Task 9、10 的延后唤醒经 30 秒定时器发出，从不在返回 `ErrDefer` 之前直接唤醒，否则同样形成不限速的补扫循环（2026-09-24 Task 6 审查补充）——`Scanned` 接 Task 10，`SkipHistory` 在 `HasNotifications` 为假时返回真——每次调用时实时查询、不在启动时缓存（Watcher 在 EXAMINE 返回之后才询问，缓存会重新打开 Task 6 实施说明第 7 条关闭的竞争），查询失败时返回假、照常补扫，`Now` 取 App 的时钟）、发送循环、派发循环与每天一次的 `PruneRejections(now − 30 天)`（返回值等于单次上限 10000 时继续调用，直到不足 10000）；⑤ ctx 结束后依次停止各循环、关闭存储、释放锁。①–③ 任一失败即返回错误，不启动任何循环；某个循环以错误结束（例如 Task 11 的状态写入失败）时停止其余循环并返回该错误。
- `Event{Kind, TaskID, NotificationID, ReplySeq, Reason, Folder, Count, Delay}`，`Kind` 为本节各任务列出的事件名与 Watcher 的状态名（`credentials_unavailable` 报告为 `keychain_unavailable`，与「4b 与 4a 的衔接」一致；熔断引起的除外，见 Task 11）。被拒来信的事件按原因码聚合，每分钟至多一次、带计数。事件回调不得阻塞。

**测试（先写并确认失败）：** 启动顺序（锁被占用、实例不存在、密钥不符、Keychain 不可用时都不启动循环，也不改动数据库）；启动时存在 DISPATCHING 回复与 SENDING 通知时转为 UNCERTAIN 并发出事件；`Run` 在 ctx 结束后按序退出、锁被释放；一个循环以错误结束时 `Run` 返回该错误；一封被延后的回复只在 30 秒之后引起下一次唤醒（注入时钟），不形成不限速的补扫；拒绝事件的聚合与清理；**装配后的金丝雀测试**：用模拟器跑一轮完整的收发，主题、令牌、正文与地址都取金丝雀值，断言全部事件与错误文本中都不出现它们（4a「风险与后续」对未导出字段打印原始字节的担忧，由这一条在实际路径上覆盖）。

- [ ] **Step 1：** 写失败的测试。
- [ ] **Step 2：** 实现。
- [ ] **Step 3：** `make check`、`make secrets` 通过。
- [ ] **Step 4：** `git commit -m "feat(app): assemble the mail loop under the single-instance lock"`

### Task 14：离线闭环（`tests/e2e`）

**Files:**
- Create: `tests/e2e/loop_test.go`（包 `e2e_test`，写中文包注释）

**契约：** 用 Task 7 的模拟器、临时数据目录中的真实 SQLite、假 Keychain 与合成 Agent，经 `app.Run` 走完设计要求的闭环，并覆盖 `design.md`「工程规范与验收」中可以离线完成的各项：

1. 多轮往返：任务启动 → 回合完成 → 通知 → 「已发送」副本 → 模拟回复 → 验证 → 入队 → 派发 → 下一回合的通知在同一线程中（`References` 递增）；主题标签由 `ParseSubject` 从渲染结果中取回（渲染器与解析器的跨包往返）。
2. 重复、伪造与过期：同一封回复投递两次（INBOX 与 Junk）；改动令牌一个字符；换成另一任务的标签；白名单外的发件人；超过有效期；回复旧通知（`token_superseded`）；假期自动回复与退信；畸形来信（Message-ID 不合法、正文非法 UTF-8）之后的合法回复照常被处理。
3. 忙时 FIFO：Agent 回合进行中连续到达三封回复，按入队顺序逐一派发。
4. 断网与恢复：IMAP 连接被断开后重连补扫，期间到达的回复不丢；SMTP 暂时失败后按退避重发；进程在派发中途被取消后重启，回复转为 UNCERTAIN 而不是重发；「已发送」暂时不可用时回复被延后、恢复后通过。
5. 隐私过滤：Agent 输出含绝对路径、代码块、URL 凭据与密钥形状时，收件人收到的邮件中都已替换；`notify.content = "status"` 时收件人收到的邮件（含主题）不含 Agent 提供的标题与正文。
6. 回环刹车：合成 Agent 每收到一封回复就完成回合、模拟器对每封通知都回一封没有任何自动回复特征的回信。快循环在第 11 封被 `rate_limited` 拒绝并暂停；慢循环（注入时钟，每回合 7 分钟，每小时不到 10 封）在第 41 封被拒绝并暂停；两种情况下环都停止，此后的来信为 `mail_paused`。
7. 首次运行：机器人邮箱里已有历史邮件、库中没有通知时，第一次启动不为历史邮件写被拒记录。

每个场景断言数据库状态、收件人收到的邮件与本地事件；测试有总时限，不依赖真实时钟的长等待（注入时钟与缩短的期限）。

本任务是集成测试：此时 Task 1–13 已完成，测试写好后应直接通过（「先失败」不适用），任何失败都按缺陷回到对应任务修复并记录。

- [ ] **Step 1：** 写测试并运行；失败即回到对应任务修复。
- [ ] **Step 2：** `make check` 通过，`go test -race -count=3 ./tests/e2e/` 稳定通过。
- [ ] **Step 3：** `git commit -m "test(e2e): offline closed loop with a synthetic agent"`

### Task 15：`init` 的环境诊断与真实往返测试（`internal/cli`、`internal/app/roundtrip.go`）

**Files:**
- Create: `internal/app/roundtrip.go`、`roundtrip_test.go`
- Modify: `internal/cli/init.go`（`InitDeps` 增加诊断与往返测试的依赖；`backupNote` 中「当前版本尚不能收发邮件」改为与往返测试一致的说法）、`init_test.go`、`internal/cli/cli.go`（`help` 的说明改为「pre-alpha：可以用 init 保存配置、授权码与密钥并做一次真实往返测试；尚不能执行任务或运行后台服务」）与 `cli_test.go`、`cmd/turncourier/main.go`（装配生产实现）

**契约：**

- **环境诊断：** 摘要之后运行 `doctor.Checker`（与 `turncourier doctor` 相同），逐项打印；诊断失败不影响 init 的退出码，只提示。
- **往返测试：** 诊断之后询问「现在做一次真实往返测试吗？将用机器人邮箱向 <接收地址> 发送一封测试邮件，需要你在邮件客户端中直接回复。[y/N]」，默认不做；非交互终端不询问。回答 y 时调用 `app.RoundTrip`：
  1. 取得单实例锁（与常驻进程互斥）；读取授权码与令牌密钥（`LoadSecrets`）。
  2. 记下 INBOX、Junk 与「已发送」当前的 UIDNEXT，作为本次测试专用的内存游标（只找此后到达的邮件）。
  3. 在内存中构造一条测试通知：随机任务 ID 与 nid，owner 为 `local`，有效期 1 小时，令牌由 active 的令牌密钥签发；**不写入任务表与通知表**。按正常通知的格式渲染（事件 `turn_completed`，标题「TurnCourier 往返测试」），经 SMTP 发出。
  4. 在「已发送」中找回副本（至多 90 秒），取得实际投递 ID；找不到即失败并提示检查「保存到已发送」设置。
  5. 提示用户回复，然后每 15 秒补扫一次 INBOX 与 Junk（至多 10 分钟）；对每封新来信运行 Task 9 的判定核心（内存实现：只有这一条测试通知，它也是最新的通知；没有被拒记录，回复计数恒为 0，没有暂停，补扫时刻取零值），直到出现引用这条测试通知的来信。
  6. 通过则打印「往返测试通过」；被拒则打印原因码与对应的中文提示（例如 `sender_not_allowed` 提示检查 `allowed_senders` 与别名，`auto_reply` 提示关闭假期自动回复）；超时则提示检查垃圾箱，并建议把机器人地址加入通讯录。
  7. 不改动持久化的收取游标：前移游标可能越过尚未处理的真实回复（init 可以重复执行，以后运行时库中可能已有任务）。库中还没有任何通知时，以后第一次启动收取循环会跳过历史（「首次运行的游标」），测试邮件不会留下被拒记录；已有通知时，测试回复会被记为一条 `token_invalid` 的被拒来信（只有元数据），「已发送」中的测试副本因找不到通知而被忽略。
- 往返测试不创建任何任务、回复或通知行，不写被拒记录，也不改动收取游标；打印的内容不含令牌与主题。回复的新正文只在终端显示前 80 个字符，先去掉控制字符、双向控制与零宽字符（用户自己的文字，但可能被邮件客户端插入这些字符），不落盘。
- init 的提示文案增加「接收通知的邮箱不要开启假期自动回复」（D4）与「把机器人地址加入通讯录」。

**测试（先写并确认失败，用模拟器与假终端）：** 诊断输出与退出码；拒绝往返测试时不联网；往返测试的通过、各原因码的失败、「已发送」副本缺失、等待超时；三种结果下数据库都没有新增任务、通知、回复与被拒记录，收取游标不变；锁被常驻进程占用时提示并跳过；终端输出中没有令牌、主题与双向控制字符。

- [ ] **Step 1：** 写失败的测试。
- [ ] **Step 2：** 实现。
- [ ] **Step 3：** `make check`、`make secrets`、`make build` 通过；`go version -m dist/turncourier` 此时应列出四个邮件模块与 x/text。
- [ ] **Step 4：** `git commit -m "feat(init): environment diagnostics and a real mail round-trip test"`

### Task 16：L2 真实往返验收工具（`tests/live`）

**Files:**
- Create: `tests/live/acceptance_test.go`（live 标签）
- Modify: `tests/live/live_test.go`（`probeNames` 增加 `TestL2Acceptance`）、`tests/live/sample.go` 与测试（事件摘要的纯函数）

**契约：** `TestL2Acceptance` 沿用 L1 的双重开关、CI 拒绝、总确认与逐封确认。它在**临时数据目录**中运行完整的 `internal/app`：授权码从真实实例的 Keychain 读取（只读），令牌与正文密钥是本次运行生成、用后即弃的内存密钥（登记到临时数据库），因此不写产品数据库、不新增 Keychain 条目。启动前把 INBOX、Junk 与「已发送」的游标设为当前位置。合成 Agent 每收到一封回复就完成一个回合，正文为「第 k 轮：已收到回复（N 个字符）」。

流程：发出第一封通知 → 在 `/dev/tty` 请维护者记下这封通知落在收件箱还是垃圾箱（送达率），并用指定客户端回复（默认 QQ 邮箱 App 与网页版交替）→ 显示每封来信的判定（接受或原因码）→ 共 `TURNCOURIER_LIVE_ROUNDS` 轮（默认 3，至多 8：关闭任务之后的那封回复必须仍在 1 小时 10 封的刹车之内，才能验证 `task_closed` 而不是 `rate_limited`）→ 关闭任务后请维护者再回复一次，确认得到 `task_closed`。样本只记录每轮的判定、原因码、耗时、通知落点与本地事件种类，另记每封通知从发出到记下实际投递 ID 的耗时与「已发送」的 `folder_unavailable` 次数，写入输出目录的 `samples.jsonl`（schema `turncourier-l2/1`）。后两项是 `ScanHeaders` 第一次经过真实服务器：L1 只验证过整封的部分取回（`BODY.PEEK[]<0.N>`），没有对 QQ 发过 `BODY.PEEK[HEADER]<0.N>`（2026-09-24 Task 6 审查补充，见「风险与后续」）。

**测试：** 事件摘要的纯函数在 `sample_test.go` 中离线测试；`make vet` 与 `make lint` 以 live 标签编译检查验收文件（它本身只由维护者人工执行，「先失败」只适用于纯函数）。

- [ ] **Step 1：** 写纯函数的失败测试。
- [ ] **Step 2：** 实现。
- [ ] **Step 3：** `make check` 通过；开关验证同 4a Task 13 Step 4。
- [ ] **Step 4：** `git commit -m "test(live): L2 acceptance with a synthetic agent against a real mailbox"`

### Task 17：文档同步与第三方许可声明

**Files:**
- Create: `THIRD_PARTY_NOTICES.md`、`tests/docs/notices_test.go`
- Modify: `docs/zh-CN/design.md`、`SECURITY.md`、`README.md`、`README.zh-CN.md`、`docs/en/architecture.md`、`docs/zh-CN/development.md`、`CONTRIBUTING.md`（如涉及）、`CHANGELOG.md`、`HANDOFF.md`、本清单

**内容：**

- `design.md`：状态行；D6–D8 的结论；已实现的验证顺序与过滤规则的摘要（令牌暴露面此前已写入，「接收通知的邮箱不要开启假期自动回复」在起草本契约时写入）。
- `SECURITY.md`（中英两部分）：已实现的入站验证与出站过滤、仍未实现的部分、过滤不承诺发现所有机密、令牌出现在主题中的暴露面、邮件暂停。
- README（中英）：状态、实际树形目录图（新增的包与 `tests/` 子目录）、产品二进制链接的依赖、把机器人地址加入通讯录的建议。
- `architecture.md`：包职责、依赖方向、三个循环与单实例锁、依赖表的「使用方」；「只有本地核对才能解除 UNCERTAIN」等描述按 D7 改写。
- `development.md`：依赖表、`tests/e2e` 与 `tests/qqsim`、L2 验收的命令与环境变量、`notify.content`、数据目录须在本地磁盘（flock）。
- `THIRD_PARTY_NOTICES.md`：产品二进制链接的每个非标准库模块的名称、版本、许可证与版权行；`tests/docs/notices_test.go` 核对它覆盖产品二进制可能链接的全部模块（以 `go.mod` 与 `cmd/`、`internal/` 下各包的导入闭包为准，不执行子进程）。
- `CHANGELOG.md` 与 `HANDOFF.md`。

文档任务没有「先失败」的测试；`notices_test.go` 是守卫测试，写下时应失败（`THIRD_PARTY_NOTICES.md` 尚不存在），补上文件后通过。

- [ ] **Step 1：** 写 `notices_test.go` 并确认失败；更新文档；`go test ./tests/docs/` 通过。
- [ ] **Step 2：** `make check` 通过。
- [ ] **Step 3：** `git commit -m "docs: sync documentation for phase 4b and add third-party notices"`

### Task 18：整阶段验证、审查与合并

- [ ] **Step 1：** 全部验证：`make check`、`make security`、`make workflows`、`make build`、`GOOS=linux go vet ./...`、`GOOS=windows go vet ./...`、`CGO_ENABLED=0 go test ./...`、`go test -race -count=3 ./tests/e2e/`；`go version -m dist/turncourier` 列出四个邮件模块与 x/text，且与 `THIRD_PARTY_NOTICES.md` 一致；冒烟 `help`、`version`、`doctor --json`。
- [ ] **Step 2：** 整阶段审查：由独立子代理分安全（令牌与机密的全部出口、验证顺序、回环、单封来信能否卡住收取）、正确性（状态机、崩溃恢复、游标、去重）、测试有效性（变异测试抽查）三个维度审查全部 4b 提交；确认的问题逐条修复并经独立复查。
- [ ] **Step 3：** 确认 L1b 已完成（见「门槛」），把结果与据此做的修订记入本清单；把本阶段的验证记录写入本清单与 `HANDOFF.md`。
- [ ] **Step 4：** 开 PR，等三项必需检查通过，经维护者确认后合并。合并后由维护者执行 L2。

## 完成标准（4b）

- 线程绑定按 D4 冻结，并经 L1b 的真实客户端复核：主题标签是唯一的验证输入，恰好一个标签，折行或截断以独立原因码拒绝。
- 非人工来信的判定排在一切标签与令牌处理之前；来自白名单地址、线程与标签令牌全部正确的假期自动回复被拒绝；回环刹车在分类漏判时——包括每回合数分钟的慢循环——仍能停下回环，并持久暂停该任务的邮件触发，直到本地解除。
- 任何一封来信（畸形、超长、伪造、非法编码）都不能让收取循环停下：来信内容引起的失败一律成为原因码，已判定的拒绝在批被重新交付或 UIDVALIDITY 重置之后都不被翻案。
- 合法回复的新正文经解析器剥离引用与签名后加密入队；只有 HTML、引用无法识别、行内逐段回复、残留令牌或为空的回复不执行；同一封信的多个副本与补扫只入队一次；有更新的、确已发出的通知时，旧通知的令牌以 `token_superseded` 拒绝（更新的只有尚无缺失证据的 UNCERTAIN 通知时先延后）。
- 通知的主题标签以原始 ASCII 出现、不被折断；正文经确定性过滤；纯状态模式的主题与正文都不含 Agent 文本；令牌只出现在主题标签与页脚副本中，不出现在任何事件、错误与持久化字段里。
- 实际投递 ID 取自「已发送」副本并在处理回复之前记下；回复只在「副本确实不在」的证据成立后才因缺少副本被拒；UNCERTAIN 的通知只凭副本解除，从不自动重发；认证失败打开两端共享的熔断；服务器限流时整个发送循环按全局的连续失败次数暂停，一条坏数据或一次状态写入失败都不会让发送悄悄停摆。
- 收发与派发只在单实例锁之下进行；启动时恢复在途回复与发送中的通知；进程中途退出不造成重复投递。
- 合成 Agent 的离线闭环覆盖 `design.md` 验收清单中可离线的各项；`init` 可以完成一次真实往返测试；L2 验收工具就绪。
- `CGO_ENABLED=0` 下全部测试通过；`make check`、`make security`、`make workflows`、`make build` 与远端三项检查通过；覆盖率不低于 80%；文档、依赖表与第三方许可声明和实际代码一致。

## 风险与后续

- go-imap/v2 与 go-message 都未到 1.0，升级可能带来破坏性变更，所以固定版本，由封装层隔离。
- QQ 的发信频率阈值不公开，所以要节流、合并通知；遇到 550/450 类错误时退避，并标记状态。（4b：D8 的发信上限与 4b Task 11 的退避——4xx、5xx、`ErrUncertain` 与连接级失败之后，整个发送循环按全局的连续失败次数指数暂停，至多 60 分钟；5xx 在同一通知第 3 次尝试时才放弃；通知不合并，见「4b 已定的实现细节」。）
- 同一用户的进程可以读取 Keychain 中的机密（D3），发布阶段需重新评估签名与进程内调用。
- ~~线程绑定的实际投递 ID 来源~~：L1 已定，取自「已发送」副本（见「L1 结果」第 2 步）。QQ 会改写 Message-ID，`X-OQ-MSGID` 保留我方 ID。
- 地址统一转小写基于 QQ 邮箱不区分大小写的假设（Phase 3 风险）。L1 的两份样本中发件人没有大小写变体，**别名地址与大小写变体仍未复核**，须在 4b 之前用同一规程补采（见「L1b」：不阻塞 4b 其他任务的实现，但 4b 合并之前必须完成）。
- 入站验证不能依赖 `Authentication-Results` 与 `Received` 链：L1 证明 QQ 同域投递两者都不存在。这不是「维持原判」，而是两处削弱叠加：入站的四项判据里只有令牌是机密——发件人头可伪造且本地无从核对，线程引用（实际投递的 Message-ID）对任何见过这封通知的人都是公开的，任务 ID 就印在主题里——整条链路的强度等于令牌的保密性，而令牌刚被搬到最容易被旁人看见的位置。`From` 被伪造成白名单地址时四项全过，内容会作为用户消息进入 Agent 会话（Agent 自身的沙箱与工具权限仍在）。`Return-Path` 是同域投递中唯一可能不由发信方书写的线索，补采样本时要记下它是否存在、取值为何，评估能否作为白名单的第二个比对点。
- 令牌移到主题后新增的暴露面，只有「锁屏与邮件列表预览」写进了 `design.md`。其余需要在 4b 之前补记：IMAP 服务端的主题索引与搜索（保留期由服务商决定，不受 `TokenTTL` 约束）；邮件客户端的本地全文索引（Spotlight 等，在 `secure_delete` 与 WAL 检查点的覆盖范围之外）；令牌沿线程向前传播——用户自己写的回复、转发与对方的存档里都带着它；退信报文与自动回复回显原主题；日志与错误文本（约束见 D4「令牌脱敏的边界」）。（2026-09-24 核对：`design.md`「安全与持久化细化」已列出这些暴露面，只差日志一项，它由 D4 的约束与 4b 的金丝雀测试覆盖。）
- **送达率与主题载体相冲突：** 主题里多出约 49 个随机字符本身就是垃圾邮件特征，而 L1 已观察到通知落进收件人垃圾箱。4b 的送达率工作要把这一点当作已知约束，不能指望只靠调整正文解决。
- Keychain 中的密钥被替换时，由 `crypto_keys.key_check` 在 init 与 4b 启动时发现并拒绝继续，而不是等到解密或验证失败。正文无法解密（数据损坏，或绕过启动检查替换了密钥）的回复会停在该任务队首、阻塞它的派发；这样的通知每次领取都会失败，因领取按 `(not_before, id)` 排序，会阻塞全部发送。4a 只保证不领取、不改动数据并返回明确的错误，处置靠 `AbandonNotification(manual)` 或人工核对；4b 须在本地告警中提示这一情况。（4b：无法解密或缺失正文的通知按「本地的永久失败」放弃并发出带通知 ID 的事件，不再阻塞发送；回复一侧发出 `reply_payload_unavailable`，见 4b Task 11、12。）
- 删除数据目录后，Keychain 中该实例的条目成为孤儿；元数据存在而条目缺失时 init 拒绝重新生成。4a 没有修复、轮换或卸载命令，手工处理方法写在 development.md。
- `Token` 作为其他包结构体的**未导出**字段时，`fmt` 会打印其原始字节；4b 以装配后的日志金丝雀测试覆盖实际路径。
- go-smtp 的 `DialTLS` 拨号期限固定为 30 秒且不响应 ctx，进程退出时最多多等 30 秒；已确认的决策只允许 `DialTLS`，不为此改用其他拨号方式。
- IMAP 的期限、IDLE 重建间隔与退避数值、2 MiB 正文上限都是推断，以 L1 的实测结果调整，调整写入本清单。
- 服务器若在邮件数不变时也在 UID SEARCH 或 UID FETCH 的响应中附带 `* N EXISTS`（RFC 3501 允许），按 Task 11 Step 5，每轮补扫后都会留下信号，Watcher 不再发 IDLE，而是不限速地连续补扫。4a 按已确认的契约保留现状，L1 第 4 步记录 QQ 是否如此。若是，可以只在邮件数超过 EXAMINE 时的数目时留下信号，或给两轮补扫之间设最小间隔；这会改动 Step 5「通道是唯一状态」的约定，须经维护者确认。
- 「已发送」只取头部（4b Task 6 的 `ScanHeaders`，`BODY.PEEK[HEADER]<0.N>`）尚未经过 QQ 实测，L2 第一次验证它（Task 16）。QQ 若拒绝对头部的部分取回，「已发送」每轮补扫失败，只有 `sent_unavailable` 告警，回复因拿不到实际投递 ID 而一直延后；届时改为不带部分取回的 `BODY.PEEK[HEADER]`，须另行确认。`ScanHeaders` 对「UID SEARCH 列出、FETCH 却没有返回数据」的副本按本轮失败处理而不越过（Task 6 审查后的修正），一封持续取不到的副本因此会挡住所有需要证据的回复，但有告警、可以恢复；越过它则是静默且不可撤销的拒绝。
- 「已发送」从不降级（D6）：对它的补扫持续超时时，每一轮都拆掉连接、重连后从 INBOX 继续，而被延后的回复每 30 秒唤醒一次 Watcher，收取循环会长期顶在每小时 12 次的登录上限（4a 的上限之内）。`sent_unavailable` 告警覆盖这一情形。
- init 不回显读入授权码的终端路径需要伪终端，没有自动测试，由 L1 准备步骤中维护者实际运行 init 验证。
- 键控摘要对解析器输出计算，依赖解析器的确定性。4b 或以后修改引用、签名的剥离规则时，同一封信重新取回（例如 UIDVALIDITY 变化后的全量补扫）会得到不同摘要，从「重复」变成 `ErrMessageConflict` 误报。修改解析规则时须评估这一点，必要时让补扫中的冲突只作告警，或给摘要加版本（在 4b 定）。（4b 已定：不加版本，见「4b 已定的实现细节」。）
- 令牌验证中有两个问题留给 4b 在 L1 之后定稿：retired 密钥签发的令牌是否仍接受；有效期按本地处理时间还是 IMAP INTERNALDATE 判断。按本地处理时间，服务停机或 Keychain 不可用期间到达、过期之后才处理的合法回复会被拒绝；INTERNALDATE 由服务器写入，不是发件人提供的 Date，但它是否可信、QQ 如何设置要由 L1 确认。去重排在有效期与任务状态检查之前的初步建议见「4b 与 4a 的衔接」。（4b 已定：只接受 active 密钥；按本地处理时间判断，见「4b 已定的实现细节」。）
- 待发通知在令牌将于 10 分钟内过期时被放弃（`expired`），SMTP 中断时间超过令牌有效期时，这段时间的通知不会发出；4b 须在本地提示这一情况。（4b：4b Task 11 的 `notification_expired` 事件。）
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

**2. 4b 任务契约的 D6–D8（2026-09-24 确认，均采用建议方案）。** 契约于同日起草，经两名独立审查子代理审查与一名子代理的三次复查逐轮修订后提交维护者确认；确认之后 4b 按任务顺序开始实现。详见「4b 须确认的决策」：

1. **D6：** 「已发送」副本由收取循环在每一轮最先补扫（只取头部），发信后与通知进入 UNCERTAIN 后唤醒它；「已发送」不降级。回复引用的通知尚无实际投递 ID 时延后处理，只有在「成功完成的补扫开始于通知发出 10 分钟之后仍没有副本」这一证据成立时才以 `thread_mismatch` 拒绝；代价是被延后的一封会挡住其后的全部来信。
2. **D7：** 在「已发送」中找到对应副本时，自动（在同一事务中）把 UNCERTAIN 通知核对为已送达并记下实际投递 ID；没有副本的仍等人工核对，从不自动重发。`token_superseded` 所比较的「最新通知」包括尚无缺失证据的 UNCERTAIN（「已发送」补扫在它进入 UNCERTAIN 10 分钟之后仍没有它的副本，即为缺失证据），按发出时刻取最晚的一条，核对为已送达不改变发出时刻；更新的只有这样的 UNCERTAIN 时，对上一条通知的回复先延后，等副本或缺失证据出现再判。所依据的「QQ 只为已接受的邮件写入「已发送」」是推断，L1b 可选地核实。
3. **D8：** 同一任务滚动 1 小时至多接受 10 封回复、24 小时至多 40 封，超出即以 `rate_limited` 拒绝并**持久暂停**该任务的邮件触发（迁移 `0003`），直到本地解除，解除之后重新计数；全局每小时至多发出 40 封通知，超出的推迟。对 D4 字面的一处解释一并确认：M 超出时推迟发信并告警，不暂停任务。
