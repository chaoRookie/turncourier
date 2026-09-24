# 合成回归样本：入站邮件

本目录保存入站邮件解析器（`internal/mail/parser`）的合成回归样本，供 4b Task 3（解析器）、Task 4（自动回复与退信判定）、Task 9（入站验证流水线）与 Task 14（离线闭环）的测试共用。样本全部合成：地址一律使用保留域名 `example.invalid`，不含任何真实邮件、真实地址或真实令牌。

## 格式

每个样本是一个 JSON 文件，同时保存模板与期望结果：

- `source`：来源。`l1` 表示按 L1 真机探测实测的结构合成（见 `docs/zh-CN/plans/phase-04.md`「L1 结果」第 3 步与两次补采）；`public` 表示按公开资料推断合成，待 L1b 或以后的真机样本核实。
- `note`：样本说明，写明它要覆盖的结构。
- `message`：模板。头部与各部件只保存**解码后的文字**，并声明每个部件的媒体类型、字符集与传输编码：
  - 头部是 `name` 加 `value`（原样写入的 ASCII 取值）或 `words`（依次拼接的片段：`raw` 原样写入，`text` 按 `charset` 编为 RFC 2047 的 B 编码词，过长时拆成多个编码词并折行）；
  - 部件有 `type`、`params`（Content-Type 的其他参数）、`charset`、`encoding`（`base64`、`quoted-printable`、`7bit`、`8bit`）、`disposition`、`headers`（写在 MIME 头之前的其他头）、`newline`（部件内的换行，默认 CRLF，`lf` 为单独的 LF）与 `lines`（部件解码后的文字，逐行给出）；多部件改用 `boundary` 与 `parts`。根部件的 `headers` 就是整封邮件的头部。
- `expect`：期望结果，`new_text`（`NewText` 应得到的新正文）与 `error`（期望的错误名：`ErrNoPlainText`、`ErrUncertain`、`ErrEmpty`、`ErrMalformed`）二选一。

模板中的占位符在测试运行时替换：`{{TASK}}` 换成任务 ID，`{{TOKEN}}` 换成测试中用 `security/token` 签发的令牌文本，`{{DELIVERED}}` 换成通知的实际投递 Message-ID（带尖括号）。令牌不写进文件，这是密钥扫描的约定；直接保存 base64 的 `.eml` 又无法替换占位符，所以模板只存解码后的文字。替换之后，本目录的 Go 包 `mailfixture`（`mailfixture.go`）按声明的字符集与传输编码组装原始字节。组装只用标准库与 `golang.org/x/text`，不经过被测解析器所用的 go-message：gbk、gb2312 与 gb18030 一律用 GB18030 编码器编码，标注 gbk 的部件因此可以含 GBK 编不出的四字节字符。

模板还可以用可选的 `defaults` 对象声明**可选占位符**：键是大写字母组成的名称（不能与上面三个重名），值是按行给出的默认值，默认值中只可含上面三个固定占位符。调用方经 `Values.Optional` 给出取值时替换默认值（空串也照用），没有给出的取默认值；样本的 `expect` 对应全部取默认值时的邮件。多行的取值只能用在叶子部件的 `lines` 中，按部件声明的换行展开；用在头部的占位符取值必须是单行。全部占位符一趟替换完：调用方的取值原样写入，其中形如占位符的文字不会再被替换。`text/html` 部件中的取值（含三个固定占位符）按 HTML 转义，多行以 `<br>` 连接。目前只有 `qq-app-reply` 声明了可选占位符：`FROM`、`TO`、`PREFIX`、`SUBJECT`、`MESSAGEID`、`BODY`、`QUOTED`（发件人、收件人、主题前缀与原主题、回复的 Message-Id、正文与引用原文），默认值就是 L1 实测结构的这份样本；`tests/qqsim` 的 `Reply` 给出它们，按同一结构合成对任意通知的回复。

新增样本：在本目录写一个 JSON 文件，在下表登记名称、来源与期望，`go test ./tests/fixtures/mail/` 会核对模板的字段、组装结果与本表是否一致。

## 样本

| 样本 | 来源 | 结构要点 | 期望 |
| --- | --- | --- | --- |
| `qq-app-reply` | L1 实测结构 | QQ 邮箱 App：multipart/alternative，纯文本与 HTML 均为 utf-8/base64；纯文本带「原始邮件」分隔线与引用头块，引用头块的「主题:」行与没有引用前缀的页脚令牌行都带着令牌；HTML 用 QQ 自有标记，不用 blockquote | 新正文 |
| `qq-web-reply` | L1 实测结构 | QQ 邮箱网页版：同样的 multipart/alternative 结构，主题前缀「回复：」，完全没有引用 | 新正文 |
| `qq-vacation-auto-reply` | L1 实测结构 | QQ 假期自动回复：只有 text/html，主题以「自动回复: 」开头，只有 In-Reply-To，没有任何自动回复的头部信号 | `ErrNoPlainText` |
| `qq-bounce` | L1 实测结构 | QQ 退信：multipart/report，子部件 text/html 与 application/octet-stream，`Auto-Submitted: auto-generated`，发件人含大写字母，线程头指向实际投递 ID；report-type 参数与 HTML 文字按公开资料补写 | `ErrNoPlainText` |
| `foxmail-header-block` | 公开资料推断 | Foxmail：没有分隔线的「发件人：」引用头块，gb2312，没有 In-Reply-To | 新正文 |
| `apple-mail-wrote` | 公开资料推断 | Apple Mail（iOS）：「发自我的iPhone」、以「写道：」结尾的引用头与 > 引用，quoted-printable | 新正文 |
| `gmail-wrote` | 公开资料推断 | Gmail：以 wrote: 结尾的引用头与 > 引用，HTML 用 gmail_quote | 新正文 |
| `gbk-four-byte` | 公开资料推断 | 标注 gbk、实含 GB18030 四字节字符的纯文本，主题与显示名的编码词同样标注 gbk | 新正文 |
| `signature-dash-iphone` | 公开资料推断 | 恰为「-- 」的签名分隔线、签名文字与「发自我的iPhone」 | 新正文 |
| `html-only-reply` | 公开资料推断 | 只有 text/html 的真人回复 | `ErrNoPlainText` |
| `footer-without-separator` | 公开资料推断 | 引用没有分隔线、引用头与 > 前缀，却带着我方页脚（残留检查） | `ErrUncertain` |
| `inline-reply-wrote` | 公开资料推断 | 带 wrote: 引用头的行内逐段回复 | `ErrUncertain` |
| `inline-reply-bare` | 公开资料推断 | 不带引用头的行内回复：> 块含我方页脚的标记行，块后是作答 | `ErrUncertain` |
| `user-quoted-command` | 公开资料推断 | 用户自己用 > 标出命令，没有引用我方通知 | `ErrUncertain` |
| `wrote-in-body` | 公开资料推断 | 正文里写着「日志里写道：」而其后不是引用，整段保留 | 新正文 |
| `outlook-original-message` | 公开资料推断 | Outlook 的「-----Original Message-----」分隔线与 From:/Sent: 头块（契约清单之外补充） | 新正文 |
