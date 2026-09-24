// Package renderer 渲染出站的通知邮件：主题以原始 ASCII 的标签 [TC <任务 ID> <令牌>] 开头、标题按 UTF-8 B 编码，头部带线程引用、
// Auto-Submitted 与 X-TurnCourier-ID，正文是 multipart/alternative 的纯文本与 HTML 两部分（都是 utf-8、quoted-printable），
// 其后是页脚。Agent 提供的标题与正文经 Filter 确定性过滤，或在纯状态模式下完全不进入邮件。
//
// 本包只处理传入的值与字节，不访问存储、配置与 security/*：主题标签经本包定义的 Tag 接口取得（token.Tag 满足它），进程机密经
// Scrubber 接口抹除，页脚标记行、回复说明的固定句子与自定义头名取自 internal/mail 的邮件契约。组装好的主题只作为局部变量存在，
// 写入头部后即弃；错误都是固定文本的哨兵，不回显主题、令牌、标题、正文与地址（D4「令牌脱敏的边界」）。
package renderer

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"

	"github.com/chaoRookie/turncourier/internal/mail"
)

const (
	// MaxMessageSize 是渲染结果的字节上限（256 KiB）：远小于 SMTP 的 4 MiB 与 IMAP 取回正文的 2 MiB，「已发送」副本因此总能被取回。
	MaxMessageSize = 256 << 10
	// maxTitleRunes 是主题中标题（标签之后的部分，含事件名与截断的省略号）的字符数上限。
	maxTitleRunes = 60
	// maxMessageIDLen 是 msg-id 的字节上限：加上最长的头名「X-Turncourier-Id: 」（18 字节）不超过 RFC 5322 的 998 字节行长，
	// go-message 因此不会在 ID 中间强行折行。
	maxMessageIDLen = 980
	// maxAddressLen 是地址的字节上限，与配置的地址规则相同。
	maxAddressLen = 254
	// noticeTail 是页脚回复说明的后半句，前半句是 mail.FooterNotice。
	noticeTail = "；请保留主题中的 [TC …] 标签，不要改动。"
	// copyLabel 是页脚中令牌副本之前的说明；副本只给人核对，验证时永不回读（D4）。
	copyLabel = "令牌副本（仅供核对）："
	// contactHint 与 vacationHint 是页脚最后两句提醒。
	contactHint  = "请把机器人地址加入通讯录，以免通知落进垃圾箱。"
	vacationHint = "请不要在接收本通知的邮箱中开启假期自动回复。"
	// htmlPrefix 与 htmlSuffix 包住转义后的纯文本：HTML 部件只有这一个容器，不接受来自内容的任何标签，也没有外链与跟踪。
	htmlPrefix = `<html><body><div style="white-space: pre-wrap">`
	htmlSuffix = "</div></body></html>\n"
)

// Mode 是通知内容的模式；零值不合法，调用方必须显式选择，漏设模式不会默认发出 Agent 文本。
type Mode int

const (
	// ModeFiltered 发送过滤后的标题与正文（配置 notify.content = "filtered"）。
	ModeFiltered Mode = iota + 1
	// ModeStatus 只发送事件与任务 ID：Agent 提供的标题与正文都不进入邮件（配置 notify.content = "status"）。
	ModeStatus
)

// Tag 是主题标签：Reveal 返回 [TC <任务 ID> <令牌>]，RevealToken 返回 48 个字符的令牌文本。token.Tag 满足它，本包不导入 security/token。
// 这两个方法只在本包写入主题与页脚副本时调用（tests/docs/reveal_test.go 限定引用位置）。
type Tag interface {
	Reveal() string
	RevealToken() string
}

// Scrubber 把进程持有的机密（授权码与两把密钥的文本）原样出现处换成「[已省略]」。机密只留在实现方的未导出字段里
// （internal/app 的 *Secrets，格式化输出脱敏），不以切片等明文形式出现在 Options 或任何参数中。
type Scrubber interface {
	Scrub(text string) string
}

// Options 是渲染选项：内容模式与抹除进程机密的 Scrubber。过滤模式必须提供 Scrub；纯状态模式不使用它。
type Options struct {
	Mode  Mode
	Scrub Scrubber
}

// Envelope 是通知的信封与线程信息：地址是单个 addr-spec；MessageID 与 References 中的每个 ID 都是 <左部@右部> 形式的 msg-id；
// References 是该任务此前已记下实际投递 ID 的通知，按发出先后，In-Reply-To 取其中最后一个；Date 不能为零值。
type Envelope struct {
	From       string
	To         string
	MessageID  string
	References []string
	Date       time.Time
}

var (
	// ErrTooLarge 表示渲染结果超过 MaxMessageSize；调用方放弃该通知，不交给 Send。
	ErrTooLarge = errors.New("rendered notification exceeds the size limit")
	// ErrInvalidEnvelope 表示信封不合法：地址不是单个 addr-spec，Message-ID 或某个引用不是 msg-id 的形状，或日期为零值。
	ErrInvalidEnvelope = errors.New("invalid notification envelope")
	// ErrInvalidTag 表示主题标签的形状不对、令牌副本与标签中的令牌不一致，或标签的任务 ID 与内容的任务不同。
	ErrInvalidTag = errors.New("invalid notification subject tag")
	// ErrInvalidOptions 表示模式未选定或未知，或过滤模式没有提供 Scrubber。
	ErrInvalidOptions = errors.New("invalid notification options")
	// errAssemble 表示 go-message 组装 MIME 失败。输入在此之前都已校验，写入的是内存缓冲，实际不会发生；
	// go-message 的错误文本含头名，这里换成固定文本。
	errAssemble = errors.New("cannot assemble the notification")
)

// tagShape 是标签的完整形状 ^[TC <10 个字母表字符> <48 个字母表字符>]$，在运行时由字母表常量拼出；两个分组依次是任务 ID 与令牌文本。
var tagShape = regexp.MustCompile(`^\[TC ([` + alphabet + `]{10}) ([` + alphabet + `]{48})\]$`)

// Render 校验内容、信封、选项与标签，按模式组成标题与正文，组装成一封 MIME 邮件：
//
//   - 头部：From、To、Subject（原始 ASCII 标签加空格，再接 mime.BEncoding 编码的标题；不对整个主题调用 SetSubject，否则标签会进入
//     encoded-word）、Date、Message-Id、In-Reply-To 与 References（References 非空时）、MIME-Version、Auto-Submitted: auto-generated、
//     X-TurnCourier-ID（等于 Message-ID）。
//   - 正文：multipart/alternative，纯文本与 HTML 两部分，都是 utf-8、quoted-printable；HTML 是纯文本经 html.EscapeString 转义后放进
//     white-space: pre-wrap 的容器。纯文本是正文、空行与页脚（见 plainText）。
//
// 过滤模式的标题是事件中文名加「：」加过滤后的 Title，纯状态模式只有事件中文名；标题至多 60 个字符。渲染结果超过 MaxMessageSize
// 返回 ErrTooLarge。出错时不返回任何字节，错误文本不含内容、令牌与地址。
func Render(env Envelope, tag Tag, c Content, opts Options) ([]byte, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	if err := env.validate(); err != nil {
		return nil, err
	}
	if err := opts.validate(); err != nil {
		return nil, err
	}
	subjectTag, tokenText, err := checkTag(tag, c.TaskID)
	if err != nil {
		return nil, err
	}
	title, body, omitted := compose(c, opts)
	raw, err := assemble(env, subjectTag, title, plainText(c, body, tokenText, omitted))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxMessageSize {
		return nil, ErrTooLarge
	}
	return raw, nil
}

// validate 检查信封：两个地址都是单个 addr-spec，Message-ID 与每个引用都是 msg-id 的形状，日期不为零值。
// 这些值来自配置与存储；存储的 ValidMessageID 不排除 CR、LF，这里的形状校验是防头部注入的纵深防御。
func (e Envelope) validate() error {
	switch {
	case !validAddress(e.From):
		return fmt.Errorf("%w: sender is not a single address", ErrInvalidEnvelope)
	case !validAddress(e.To):
		return fmt.Errorf("%w: recipient is not a single address", ErrInvalidEnvelope)
	case !validMessageID(e.MessageID):
		return fmt.Errorf("%w: message id is not a msg-id", ErrInvalidEnvelope)
	case e.Date.IsZero():
		return fmt.Errorf("%w: date is missing", ErrInvalidEnvelope)
	}
	for _, id := range e.References {
		if !validMessageID(id) {
			return fmt.Errorf("%w: a reference is not a msg-id", ErrInvalidEnvelope)
		}
	}
	return nil
}

// validMessageID 判断 id 是否为 <左部@右部> 形式的 msg-id：左右两部非空，只含 0x21–0x7e 中除 <、>、@ 之外的字符（因此没有空白、
// 控制字符与非 ASCII，恰好一个 @），总长至多 maxMessageIDLen 字节。比 RFC 5322 严格（不接受 [] 字面量中的 @），与解析器提取线程头的
// 形状一致。
func validMessageID(id string) bool {
	if len(id) > maxMessageIDLen || len(id) < len("<a@b>") || id[0] != '<' || id[len(id)-1] != '>' {
		return false
	}
	left, right, _ := strings.Cut(id[1:len(id)-1], "@")
	return isIDPart(left) && isIDPart(right)
}

// isIDPart 判断 msg-id 的左部或右部：非空，只含 0x21–0x7e 中除 <、>、@ 之外的字符。
func isIDPart(part string) bool {
	return part != "" && strings.IndexFunc(part, func(r rune) bool { return r < 0x21 || r > 0x7e || r == '<' || r == '>' || r == '@' }) < 0
}

// validAddress 判断 addr 是否为单个 addr-spec：至多 maxAddressLen 字节，恰好一个 @，两侧非空，只含 0x21–0x7e 中除
// < > ( ) [ ] \ , ; : " 与 @ 之外的字符（配置规范化后的地址总满足这一条）。
func validAddress(addr string) bool {
	local, domain, _ := strings.Cut(addr, "@")
	return len(addr) <= maxAddressLen && isAddressPart(local) && isAddressPart(domain)
}

// isAddressPart 判断地址的本地部分或域名：非空，只含 0x21–0x7e 中除 < > ( ) [ ] \ , ; : " 与 @ 之外的字符。
func isAddressPart(part string) bool {
	return part != "" && strings.IndexFunc(part, func(r rune) bool { return r < 0x21 || r > 0x7e || strings.ContainsRune(`<>()[]\,;:"@`, r) }) < 0
}

// validate 检查模式为 ModeFiltered 或 ModeStatus，过滤模式带有 Scrubber：没有 Scrubber 时进程机密不会被抹除，宁可不渲染。
func (o Options) validate() error {
	switch o.Mode {
	case ModeFiltered:
		if o.Scrub == nil {
			return fmt.Errorf("%w: filtered mode needs a scrubber", ErrInvalidOptions)
		}
		return nil
	case ModeStatus:
		return nil
	}
	return fmt.Errorf("%w: unknown mode", ErrInvalidOptions)
}

// checkTag 取出标签与令牌副本并核对：Reveal 须恰为 [TC <任务 ID> <令牌>] 的形状（小写字母表、单个空格、没有其他文字），
// RevealToken 须等于其中的令牌，任务 ID 须等于内容的任务。返回的两段明文只交给 assemble 与 plainText。
func checkTag(tag Tag, taskID string) (string, string, error) {
	if tag == nil {
		return "", "", ErrInvalidTag
	}
	full, text := tag.Reveal(), tag.RevealToken()
	m := tagShape.FindStringSubmatch(full)
	if m == nil || m[1] != taskID || m[2] != text {
		return "", "", ErrInvalidTag
	}
	return full, text, nil
}

// compose 按模式组成标题与正文，并返回是否有内容被省略或截断。纯状态模式的标题只有事件中文名，正文是固定的一句状态说明，
// Title 与 Body 完全不读。过滤模式：标题先把连续空白（含换行）合并为一个空格，经 Filter 过滤后再合并一次，接在事件中文名与「：」之后
// （过滤后为空时只有事件名），整体截到至多 60 个字符；正文经 Filter 过滤，去掉首尾的空行。
func compose(c Content, opts Options) (string, string, bool) {
	name := eventNames[c.Event]
	if opts.Mode == ModeStatus {
		return name, "任务 " + c.TaskID + " 的新状态是「" + name + "」，本通知只含状态，任务输出留在本机。", false
	}
	filtered, titleOmitted := filter(collapseSpace(c.Title), opts.Scrub)
	title := name
	if rest := collapseSpace(filtered); rest != "" {
		title += "：" + rest
	}
	title, cut := truncate(title, maxTitleRunes, opts.Scrub)
	body, bodyOmitted := filter(c.Body, opts.Scrub)
	return title, strings.Trim(body, "\n"), titleOmitted || cut || bodyOmitted
}

// collapseSpace 把连续的空白（unicode.IsSpace，含换行、制表与全角空格）合并为一个空格，并去掉首尾空白。
func collapseSpace(text string) string {
	return strings.Join(strings.FieldsFunc(text, unicode.IsSpace), " ")
}

// plainText 拼出纯文本：正文（非空时）与一个空行，之后是页脚——单独一行的标记行（mail.FooterMarker），任务 ID 与事件，
// 回复说明（mail.FooterNotice 加后半句），令牌副本的说明与单独一行的令牌，有省略时的「部分内容已省略，完整输出留在本机（任务 <ID>）。」，
// 以及加通讯录与不要开启假期自动回复的两句提醒。以换行结尾。
func plainText(c Content, body, tokenText string, omitted bool) string {
	var lines []string
	if body != "" {
		lines = append(lines, body, "")
	}
	lines = append(lines, mail.FooterMarker, "任务 "+c.TaskID+" · "+eventNames[c.Event], mail.FooterNotice+noticeTail, copyLabel, tokenText)
	if omitted {
		lines = append(lines, "部分内容已省略，完整输出留在本机（任务 "+c.TaskID+"）。")
	}
	lines = append(lines, contactHint, vacationHint)
	return strings.Join(lines, "\n") + "\n"
}

// assemble 组装 MIME 字节。主题在这里拼出、只作为局部变量写入头部：标签在前，保持原始 ASCII；「Subject: 」加 64 个字符的标签共 73 列，
// go-message 按 76 列折行时只在标签之后的空格处折行，标签既不会进入 encoded-word，也不会被折断。头名按 go-message 的规范形式写出
// （mail.IDHeader 写作 X-Turncourier-Id，Message-Id 同理）；头名不区分大小写，解析器与 IMAP 的 HEADER 检索都按不区分大小写比较。
func assemble(env Envelope, subjectTag, title, text string) ([]byte, error) {
	var h gomail.Header
	h.SetContentType("multipart/alternative", nil)
	h.SetAddressList("From", []*gomail.Address{{Address: env.From}})
	h.SetAddressList("To", []*gomail.Address{{Address: env.To}})
	subject := subjectTag + " " + mime.BEncoding.Encode("utf-8", title)
	h.Set("Subject", subject)
	h.SetDate(env.Date)
	h.Set("Message-Id", env.MessageID)
	if n := len(env.References); n > 0 {
		h.Set("In-Reply-To", env.References[n-1])
		h.Set("References", strings.Join(env.References, " "))
	}
	h.Set("Auto-Submitted", "auto-generated")
	h.Set(mail.IDHeader, env.MessageID)

	var buf bytes.Buffer
	w, err := message.CreateWriter(&buf, h.Header)
	if err != nil {
		return nil, errAssemble
	}
	for _, part := range [...]struct{ mediaType, body string }{
		{"text/plain", text},
		{"text/html", htmlPrefix + html.EscapeString(text) + htmlSuffix},
	} {
		var ph message.Header
		ph.SetContentType(part.mediaType, map[string]string{"charset": "utf-8"})
		ph.Set("Content-Transfer-Encoding", "quoted-printable")
		pw, err := w.CreatePart(ph)
		if err != nil {
			return nil, errAssemble
		}
		if _, err := io.WriteString(pw, part.body); err != nil {
			return nil, errAssemble
		}
		if err := pw.Close(); err != nil {
			return nil, errAssemble
		}
	}
	if err := w.Close(); err != nil {
		return nil, errAssemble
	}
	return buf.Bytes(), nil
}
