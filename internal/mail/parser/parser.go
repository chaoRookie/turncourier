// Package parser 解析收到的邮件：Parse 读取头部与 MIME 结构，ParseHeader 只读头部；NewText 取第一个非附件的纯文本部件，
// 按字符集解码、统一换行、剥离引用与签名，得到回复的新正文；BodyPrefix 给出自动回复判定所需的正文开头。本包只处理传入的字节，
// 不访问存储、配置与 security/*，页脚标记与自定义头名取自 internal/mail 的邮件契约。
//
// 错误一律是固定文本的哨兵：go-message 会把来信的头部行、字段名、字符集名与传输编码名放进错误文本，这些错误都在包内吞掉，
// 换成哨兵返回，来信字节从不进入错误。Message 的格式化输出同样脱敏，因为它的 Subject 带着令牌。解析结果是确定的：
// 同一输入字节永远得到同一新正文，回复的键控摘要依赖这一点，修改解析规则时须评估对已有摘要的影响。
package parser

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"

	"github.com/chaoRookie/turncourier/internal/mail"
)

const (
	// PrefixLen 是 BodyPrefix 返回的每段正文开头的最大字节数；自动回复判定只在这一段里查找固定开头。
	PrefixLen = 512
	// maxHeaderBytes 是顶层头部的字节上限，与 go-message 的默认值相同，写在这里使它成为本包契约的一部分。
	maxHeaderBytes = 1 << 20
	// maxDepth 是部件嵌套深度的上限：根部件的深度为 0，多部件中的部件比外层深 1，深度超过 8 的部件不再读取。
	maxDepth = 8
	// maxParts 是每个多部件中读取的部件数上限。
	maxParts = 32
	// maxPartBytes 是纯文本部件解码后的字节上限；新正文因此不超过 1 MiB，与正文密文的明文上限相同。
	maxPartBytes = 1 << 20
	// headerTooLargeText 是 go-message v0.18.2 在头部超限时返回的固定错误文本（其中没有来信字节）。该错误未导出，只能按文本比对；
	// 上游改写文本时这一类退回 ErrMalformed，仍然不会泄露来信字节。
	headerTooLargeText = "message: header exceeds maximum size"
	// oqHeader 是 QQ 改写 Message-ID 时保留客户端原 ID 的头；我方自定义头缺失时，「已发送」副本按它对应回通知。
	oqHeader = "X-OQ-MSGID"
	// redactedMessage 是 Message 在一切格式化输出中的替代文本。
	redactedMessage = "[redacted inbound message]"
)

var (
	// ErrMalformed 表示来信无法按规范解析：头部行或字段名畸形、From 缺失、无法解析或不止一个地址、主题的编码词用了未知字符集；
	// 或所选纯文本部件的字符集、传输编码未知，解码失败，解码后不是合法 UTF-8，超过 1 MiB，以及在找到纯文本部件之前
	// 多部件的结构损坏或超过深度、部件数上限。
	ErrMalformed = errors.New("malformed inbound message")
	// ErrHeaderTooLarge 表示顶层头部超过 maxHeaderBytes，整封来信都没有解析。
	ErrHeaderTooLarge = errors.New("inbound message header exceeds the parser limit")
	// ErrNoPlainText 表示来信没有可用的纯文本部件（只有 HTML、只有附件等），或 Message 来自只解析头部的 ParseHeader。
	ErrNoPlainText = errors.New("inbound message has no plain text part")
	// ErrUncertain 表示分不清新正文与引用：> 引用块之后又有作答（行内逐段回复）、没有引用头的 > 行里没有我方页脚的标记行与 [TC，
	// 或剥离之后仍残留令牌形状的串、[TC、页脚的标记行或固定句子。解析不确定时不执行。
	ErrUncertain = errors.New("cannot tell the new text from quoted text")
	// ErrEmpty 表示剥离引用与签名、去掉首尾空白之后没有剩下任何文字。
	ErrEmpty = errors.New("inbound message has no new text")
)

// msgIDPattern 匹配头部取值中一个形如 <左部@右部> 的 Message-ID。线程头按它逐个提取，不因其间的逗号、注释或其他文字
// 丢弃其后的 ID：线程引用不是机密，宽松提取只会多认出客户端写得不规范的 ID。
var msgIDPattern = regexp.MustCompile(`<[^<>@\s]+@[^<>@\s]+>`)

// Message 是一封来信的解析结果：判定所需的头部字段，以及（Parse 得到时）纯文本与 HTML 部件的解码结果。
// 格式化输出（String、Format、LogValue、MarshalJSON）一律是固定的脱敏文本：Subject 带着令牌，%+v 打印一个 Message 就会泄漏。
// 已知局限与 token.Tag 相同：Message 作为其他包中结构体的未导出字段时，fmt 无法调用它的方法，会按原始字段打印；
// %p 作用于 Message 值（或含它的结构体值）时同样如此。
type Message struct {
	From          string   // From 头中唯一地址的原文 addr-spec，规范化由调用方经 config.NormalizeAddress 完成
	Subject       string   // 解码后的完整主题
	MessageID     string   // Message-Id 头去掉首尾空白后的原样字符串，缺失时为空
	InReplyTo     []string // In-Reply-To 中的 Message-ID，带尖括号，按出现顺序；没有时为 nil
	References    []string // References 中的 Message-ID，带尖括号，按出现顺序；没有时为 nil
	AutoSubmitted string   // 第一个 Auto-Submitted 的原文，去首尾空白
	XAutoreply    bool     // 存在 X-Autoreply 或 X-Autorespond（取值为空也算）
	Precedence    string   // 第一个 Precedence 的原文，去首尾空白
	ReturnPath    string   // 第一个 Return-Path 的原文，去首尾空白
	MediaType     string   // 顶层 Content-Type 分号之前的部分，只折叠 ASCII 大小写；没有时为 text/plain
	IDHeader      string   // 我方自定义头 X-TurnCourier-ID 的取值，补成带尖括号的形式
	OQMsgID       string   // X-OQ-MSGID 的取值，补成带尖括号的形式

	body *content // Parse 的正文解码结果；ParseHeader 得到的 Message 为 nil
}

// content 是遍历 MIME 树的结果。plain 是第一个非附件 text/plain 部件解码后的字节（至多 maxPartBytes+1），plainBad 表示它
// 不可用；html 是第一个非附件 text/html 部件解码后的前 PrefixLen 字节；incomplete 表示遍历因结构损坏或超过上限而提前停止。
type content struct {
	plainFound bool
	plain      []byte
	plainBad   bool
	htmlFound  bool
	html       []byte
	incomplete bool
}

// walker 深度优先遍历 MIME 树，把找到的部件写入 found。
type walker struct {
	found content
}

// Parse 读取来信的头部与 MIME 结构，只解析、不做判定。头部畸形、From 不是恰好一个可解析的地址、主题的编码词无法解码时返回
// ErrMalformed，头部超过上限返回 ErrHeaderTooLarge；正文的问题（没有纯文本、字符集未知、结构损坏等）不在这里报错，
// 留给 NewText：自动回复与退信的判定只需要头部与正文开头，不应被正文的问题挡住。
func Parse(raw []byte) (*Message, error) {
	entity, rootUndecodable, err := readEntity(raw)
	if err != nil {
		return nil, err
	}
	m, err := fromHeader(entity.Header)
	if err != nil {
		return nil, err
	}
	var w walker
	w.walk(entity, rootUndecodable, 0)
	m.body = &w.found
	return m, nil
}

// ParseHeader 只解析头部、不读正文（「已发送」的批只取回头部），字段与错误都与 Parse 相同；得到的 Message 调用 NewText 与
// BodyPrefix 时返回 ErrNoPlainText。
func ParseHeader(raw []byte) (*Message, error) {
	entity, _, err := readEntity(raw)
	if err != nil {
		return nil, err
	}
	return fromHeader(entity.Header)
}

// readEntity 读取顶层头部并构造实体，正文尚未读取。go-message 只在头部读取失败时返回 nil 实体，此时按错误文本归入两个哨兵；
// 实体非 nil 而带错误，说明顶层部件的字符集或传输编码未知（实体仍可读），以第二个返回值告诉遍历。
func readEntity(raw []byte) (*message.Entity, bool, error) {
	entity, err := message.ReadWithOptions(bytes.NewReader(raw), &message.ReadOptions{MaxHeaderBytes: maxHeaderBytes})
	switch {
	case entity != nil:
		return entity, err != nil, nil
	case err != nil && err.Error() == headerTooLargeText:
		return nil, false, ErrHeaderTooLarge
	}
	return nil, false, ErrMalformed
}

// fromHeader 从顶层头部取出 Message 的全部字段。
func fromHeader(h message.Header) (*Message, error) {
	header := gomail.Header{Header: h}
	from, ok := singleFrom(header)
	if !ok {
		return nil, ErrMalformed
	}
	subject, err := header.Subject()
	if err != nil {
		return nil, ErrMalformed
	}
	return &Message{
		From:          from,
		Subject:       subject,
		MessageID:     strings.TrimSpace(h.Get("Message-Id")),
		InReplyTo:     msgIDPattern.FindAllString(h.Get("In-Reply-To"), -1),
		References:    msgIDPattern.FindAllString(h.Get("References"), -1),
		AutoSubmitted: strings.TrimSpace(h.Get("Auto-Submitted")),
		XAutoreply:    h.Has("X-Autoreply") || h.Has("X-Autorespond"),
		Precedence:    strings.TrimSpace(h.Get("Precedence")),
		ReturnPath:    strings.TrimSpace(h.Get("Return-Path")),
		MediaType:     mediaTypeOf(h.Get("Content-Type")),
		IDHeader:      bracket(h.Get(mail.IDHeader)),
		OQMsgID:       bracket(h.Get(oqHeader)),
	}, nil
}

// singleFrom 要求恰有一个 From 头、其中恰有一个可解析的地址，返回该地址的原文 addr-spec。
func singleFrom(h gomail.Header) (string, bool) {
	if len(h.Values("From")) != 1 {
		return "", false
	}
	addresses, err := h.AddressList("From")
	if err != nil || len(addresses) != 1 {
		return "", false
	}
	return addresses[0].Address, true
}

// mediaTypeOf 返回 Content-Type 分号之前的部分（只折叠 ASCII 大小写）；参数畸形时同样取得，没有时按 RFC 2045 为 text/plain。
// 它只用于判定（例如 multipart/report），部件的选取另按完整解析的结果进行。
func mediaTypeOf(value string) string {
	base, _, _ := strings.Cut(value, ";")
	if base = strings.TrimSpace(base); base == "" {
		return "text/plain"
	}
	return lowerASCII(base)
}

// bracket 给不带尖括号的 ID 补上尖括号，使它能与存储中带尖括号的 ID 按字节比较；空值保持为空。
func bracket(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "<") {
		return value
	}
	return "<" + value + ">"
}

// dispositionOf 返回 Content-Disposition 分号之前的部分（只折叠 ASCII 大小写），用来识别附件。
func dispositionOf(h message.Header) string {
	base, _, _ := strings.Cut(h.Get("Content-Disposition"), ";")
	return lowerASCII(strings.TrimSpace(base))
}

// walk 处理一个部件，返回 true 表示整个遍历应当停止（纯文本与 HTML 都已找到，或遇到了结构损坏与上限）。
// Content-Type 无法解析的部件既不展开也不选取；附件（Content-Disposition 为 attachment）不取；undecodable 表示
// go-message 报告该部件的字符集或传输编码未知。
func (w *walker) walk(e *message.Entity, undecodable bool, depth int) bool {
	mediaType, params, err := e.Header.ContentType()
	if err != nil || dispositionOf(e.Header) == "attachment" {
		return false
	}
	switch {
	case strings.HasPrefix(mediaType, "multipart/"):
		return w.walkMultipart(e, depth)
	case mediaType == "text/plain" && !w.found.plainFound:
		w.readPlain(e, params["charset"], undecodable)
	case mediaType == "text/html" && !w.found.htmlFound:
		w.found.htmlFound = true
		// HTML 只用于自动回复判定的正文开头，读取失败时保留已读到的部分即可。
		w.found.html, _ = io.ReadAll(io.LimitReader(e.Body, PrefixLen))
	}
	return w.found.plainFound && w.found.htmlFound
}

// walkMultipart 依次处理一个多部件的子部件。深度或部件数超过上限、分隔串缺失、部件头畸形或多部件没有正常结束时，
// 记下 incomplete 并停止整个遍历：纯文本部件若尚未找到，NewText 返回 ErrMalformed，而不是猜它不存在。
func (w *walker) walkMultipart(e *message.Entity, depth int) bool {
	reader := e.MultipartReader()
	if depth >= maxDepth || reader == nil {
		w.found.incomplete = true
		return true
	}
	for n := 0; ; n++ {
		part, err := reader.NextPart()
		switch {
		case err == io.EOF:
			return false
		case part == nil, n >= maxParts:
			w.found.incomplete = true
			return true
		}
		if w.walk(part, err != nil, depth+1) {
			return true
		}
	}
}

// readPlain 读取所选纯文本部件解码后的字节（至多 maxPartBytes+1，用来发现超限）。下列任一情况都使它不可用：字符集或传输编码未知、
// 读取出错（损坏的 base64、被截断的多部件）、超过 1 MiB、不是合法 UTF-8、经过字符集转换却含替换字符（解码器用它顶替了非法字节）。
// 读到的字节无论可用与否都保留，供 BodyPrefix 返回开头。
func (w *walker) readPlain(e *message.Entity, label string, undecodable bool) {
	data, err := io.ReadAll(io.LimitReader(e.Body, maxPartBytes+1))
	w.found.plainFound = true
	w.found.plain = data
	w.found.plainBad = undecodable || err != nil || len(data) > maxPartBytes || !utf8.Valid(data) ||
		(converted(label) && bytes.ContainsRune(data, utf8.RuneError))
}

// NewText 返回回复的新正文：取第一个非附件的 text/plain 部件（深度优先，在 multipart/alternative 中即第一个），按字符集解码，
// 换行统一为 \n，去掉开头的 BOM，剥离引用与签名，去掉首尾的空行与空白。结果是合法 UTF-8，不超过 1 MiB。
// 错误：没有纯文本部件为 ErrNoPlainText；解码失败、不是合法 UTF-8、超过上限或结构损坏为 ErrMalformed；分不清引用或剥离后
// 仍有残留为 ErrUncertain；剥离后为空为 ErrEmpty。同一 Message 重复调用得到相同的结果。
func (m *Message) NewText() (string, error) {
	found := m.body
	switch {
	case found == nil:
		return "", ErrNoPlainText
	case found.plainFound && found.plainBad, !found.plainFound && found.incomplete:
		return "", ErrMalformed
	case !found.plainFound:
		return "", ErrNoPlainText
	}
	return newText(string(found.plain))
}

// BodyPrefix 返回纯文本与 HTML 部件解码后的前至多 PrefixLen 字节（部件的选取同 NewText，不经过换行规范化与引用剥离；
// 纯文本部件解码失败时仍返回已读到的开头），没有该部件时为 nil。返回的是副本。Message 来自 ParseHeader 时返回 ErrNoPlainText。
func (m *Message) BodyPrefix() (plain, html []byte, err error) {
	if m.body == nil {
		return nil, nil, ErrNoPlainText
	}
	return prefixOf(m.body.plain), prefixOf(m.body.html), nil
}

// prefixOf 返回 data 前至多 PrefixLen 字节的副本；data 为 nil 时返回 nil。
func prefixOf(data []byte) []byte {
	return bytes.Clone(data[:min(len(data), PrefixLen)])
}

// String 返回脱敏文本。
func (m Message) String() string {
	return redactedMessage
}

// Format 对 %T、%p 之外的 fmt 动词输出脱敏文本；%p 的局限见 Message 的说明。
func (m Message) Format(f fmt.State, _ rune) {
	io.WriteString(f, redactedMessage)
}

// LogValue 让 slog 记录脱敏文本。
func (m Message) LogValue() slog.Value {
	return slog.StringValue(redactedMessage)
}

// MarshalJSON 把 Message 编码为脱敏文本的 JSON 字符串：slog 的 JSON 处理器对嵌套在其他结构体中的值走 encoding/json，
// 不实现它就会按导出字段输出主题与地址。
func (m Message) MarshalJSON() ([]byte, error) {
	return json.Marshal(redactedMessage)
}
