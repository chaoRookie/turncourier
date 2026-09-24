// Package parser 的自动回复与退信判定：Classify 按 4a「已定的实现细节」的特征表判断来信是不是人写的。它是入站验证的第一步，
// 先于一切标签与令牌处理（D4）：令牌放进主题之后，白名单地址的假期自动回复会把带令牌的主题原样带回，发件人、线程、任务 ID
// 与令牌四项校验全都通过，只能靠这里拦下。特征表是启发式的，漏判由与分类无关的回环刹车兜底（D8）。
package parser

import (
	"bytes"
	"slices"
	"strings"
)

// Kind 是来信的类别。
type Kind int

const (
	// Human 表示没有任何非人工来信的特征，交给后续的标签、令牌与线程校验。
	Human Kind = iota
	// AutoReply 表示自动回复（假期回复、自动应答、邮件列表等），以 auto_reply 拒绝。
	AutoReply
	// Bounce 表示退信（投递状态通知），以 bounce 拒绝。
	Bounce
)

// 判定命中的信号名，供本地事件使用；每个名称对应特征表中的一条规则。
const (
	// SignalMultipartReport 表示顶层媒体类型为 multipart/report（投递状态通知）。
	SignalMultipartReport = "multipart_report"
	// SignalDaemonSender 表示发件人的本地部分为 MAILER-DAEMON 或 postmaster（不区分大小写）。
	SignalDaemonSender = "daemon_sender"
	// SignalSubjectPrefix 表示解码后的主题以自动回复前缀开头；QQ 的假期自动回复只有这一条信号（2026-09-21 维护者确认的破例）。
	SignalSubjectPrefix = "subject_prefix"
	// SignalAutoSubmitted 表示 Auto-Submitted 的关键字存在且不为 no（RFC 3834）。
	SignalAutoSubmitted = "auto_submitted"
	// SignalXAutoreply 表示存在 X-Autoreply 或 X-Autorespond 头。
	SignalXAutoreply = "x_autoreply"
	// SignalPrecedence 表示 Precedence 为 auto_reply、bulk、junk 或 list。
	SignalPrecedence = "precedence"
	// SignalReturnPathEmpty 表示 Return-Path 为空信封 <>。
	SignalReturnPathEmpty = "return_path_empty"
	// SignalQQVacationBody 表示纯文本或 HTML 正文的前 PrefixLen 字节中出现 QQ 假期自动回复的固定开头。
	SignalQQVacationBody = "qq_vacation_body"
)

// qqVacationOpening 是 QQ 假期自动回复正文的固定开头（L1 补采样本中它位于只有 HTML 的正文开头）。
const qqVacationOpening = "这是来自QQ邮箱的假期自动回复邮件"

var (
	// autoReplyStems 是自动回复主题前缀的词干，已删去空白并只折叠 ASCII 大小写：Automatic reply 因此写作 automaticreply。
	autoReplyStems = []string{"自动回复", "自動回覆", "自动答复", "auto-reply", "autoreply", "automaticreply"}
	// autoReplyColons 是紧跟词干的冒号，半角与全角都算。
	autoReplyColons = []string{":", "："}
	// autoPrecedences 是判为自动来信的 Precedence 取值（只折叠 ASCII 大小写后比较）。
	autoPrecedences = []string{"auto_reply", "bulk", "junk", "list"}
	// daemonLocals 是退信常用的发件人本地部分（只折叠 ASCII 大小写后比较）。
	daemonLocals = []string{"mailer-daemon", "postmaster"}
)

// Verdict 是 Classify 的结果：类别与命中的信号名；Kind 为 Human 时 Signal 为空。
type Verdict struct {
	Kind   Kind
	Signal string
}

// Classify 判断来信是真人回复、自动回复还是退信。m 是 Parse 或 ParseHeader 的结果（不能为 nil）；plain 与 html 是 BodyPrefix
// 返回的纯文本与 HTML 正文开头（没有时为 nil），更长的切片也只看前 PrefixLen 字节，不经过引用剥离。
// 判定顺序固定：先判退信（multipart/report、MAILER-DAEMON 或 postmaster 发件），再判自动回复（主题前缀、Auto-Submitted 不为 no、
// X-Autoreply 或 X-Autorespond、Precedence 为 auto_reply/bulk/junk/list、Return-Path 为 <>、正文开头的 QQ 假期自动回复固定句），
// 返回第一条命中的规则；都不命中为 Human。退信排在前面：退信报文可能抄着带自动回复前缀的原主题，按退信处理才能关联回通知。
func Classify(m *Message, plain, html []byte) Verdict {
	switch {
	case m.MediaType == "multipart/report":
		return Verdict{Kind: Bounce, Signal: SignalMultipartReport}
	case isDaemonSender(m.From):
		return Verdict{Kind: Bounce, Signal: SignalDaemonSender}
	case hasAutoReplyPrefix(m.Subject):
		return Verdict{Kind: AutoReply, Signal: SignalSubjectPrefix}
	case isAutoSubmitted(m.AutoSubmitted):
		return Verdict{Kind: AutoReply, Signal: SignalAutoSubmitted}
	case m.XAutoreply:
		return Verdict{Kind: AutoReply, Signal: SignalXAutoreply}
	case slices.Contains(autoPrecedences, keyword(m.Precedence)):
		return Verdict{Kind: AutoReply, Signal: SignalPrecedence}
	case removeSpace(m.ReturnPath) == "<>":
		return Verdict{Kind: AutoReply, Signal: SignalReturnPathEmpty}
	case hasVacationOpening(plain) || hasVacationOpening(html):
		return Verdict{Kind: AutoReply, Signal: SignalQQVacationBody}
	}
	return Verdict{Kind: Human}
}

// isDaemonSender 判断发件地址最后一个 @ 之前的本地部分（带引号的本地部分可以含 @）是否为 MAILER-DAEMON 或 postmaster；
// 没有 @ 时不算。
func isDaemonSender(from string) bool {
	at := strings.LastIndexByte(from, '@')
	return at >= 0 && slices.Contains(daemonLocals, lowerASCII(from[:at]))
}

// hasAutoReplyPrefix 判断主题删去全部空白、只折叠 ASCII 大小写之后，是否以某个词干紧跟半角或全角冒号开头。
// 只看主题开头：「Re: 自动回复:」是对自动回复的回复，不算。
func hasAutoReplyPrefix(subject string) bool {
	squeezed := lowerASCII(removeSpace(subject))
	for _, stem := range autoReplyStems {
		rest, ok := strings.CutPrefix(squeezed, stem)
		if ok && slices.ContainsFunc(autoReplyColons, func(colon string) bool { return strings.HasPrefix(rest, colon) }) {
			return true
		}
	}
	return false
}

// isAutoSubmitted 判断 Auto-Submitted 的关键字（分号之前的部分）是否存在且不为 no。
func isAutoSubmitted(value string) bool {
	word := keyword(value)
	return word != "" && word != "no"
}

// keyword 取头部取值中分号之前的部分，去首尾空白并只折叠 ASCII 大小写。
func keyword(value string) string {
	word, _, _ := strings.Cut(value, ";")
	return lowerASCII(strings.TrimSpace(word))
}

// hasVacationOpening 判断正文的前 PrefixLen 字节中是否出现 QQ 假期自动回复的固定开头。
func hasVacationOpening(body []byte) bool {
	return bytes.Contains(body[:min(len(body), PrefixLen)], []byte(qqVacationOpening))
}

// String 返回类别的文字形式（human、auto_reply、bounce），供本地事件使用；未知的取值为 unknown。
func (k Kind) String() string {
	switch k {
	case Human:
		return "human"
	case AutoReply:
		return "auto_reply"
	case Bounce:
		return "bounce"
	}
	return "unknown"
}
