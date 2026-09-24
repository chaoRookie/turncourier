// Package live 为 L1 真机探测提供不访问外部资源的纯函数：合成通知的渲染（含携带令牌的主题标签与逐封确认文本）、
// 来信的脱敏归类（含主题标签状态、自动回复前缀与 Return-Path）、主题前缀白名单、Message-ID 按相等关系归类、DATA 响应脱敏、
// IDLE 观测判定、探测状态与样本文件的读写，以及输出目录校验。
// 本文件不带构建标签，随 make test 在 CI 中编译与测试；真正的探测在带 live 标签的文件中，只由维护者人工执行。
// 这里的函数只处理传入的字节与值（输出目录校验与状态文件读写只接触调用方给出的路径），不读取配置、钥匙串、网络与环境变量。
package live

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// Schema 是样本与状态文件的格式标识。turncourier-l1/4 起通知的主题标签携带令牌，来信样本增加 subject.tag_state、
// subject.tag_count、auto.subject_prefix 与 return_path。
const Schema = "turncourier-l1/4"

const (
	// ProbeHeader 是探测邮件自带的标识头，用来在「已发送」与抄送副本中找回同一封邮件。
	ProbeHeader = "X-TurnCourier-Probe"
	// OQHeader 是 QQ 在改写 Message-ID 时保留客户端原 ID 的头。
	OQHeader = "X-OQ-MSGID"
)

const (
	// stateFile 记录本次探测发出的每封邮件的各来源 ID、一次性令牌与补扫游标。
	stateFile = "state.json"
	// samplesFile 是 JSON Lines 样本，每行一个对象。
	samplesFile = "samples.jsonl"
)

const (
	// noticeLine 是通知正文中的说明句。
	noticeLine = "这是 TurnCourier 的真机探测邮件，请用不同客户端直接回复并保留引用。"
	// tokenLabel 是令牌所在行之前的提示句；令牌单独占一行，便于样本记录引用前缀。
	tokenLabel = "回复令牌（请勿删除）："
)

const (
	// maxPartBytes 是每个部件读取的字节上限，只用于归类与查找令牌。
	maxPartBytes = 1 << 20
	// maxDepth 与 maxParts 限制 MIME 树的深度与每层部件数，避免畸形邮件耗尽栈或输出。
	maxDepth = 8
	maxParts = 32
	// maxPrefixRunes 是主题前缀与引用前缀输出的字符数上限。
	maxPrefixRunes = 20
	// maxClientRunes 是客户端标识输出的字符数上限。
	maxClientRunes = 40
	// maxLabelRunes 是媒体类型、字符集、传输编码等短标签输出的字符数上限，取 RFC 6838 对媒体类型的上限
	// （type 与 subtype 各至多 127 个字符，加分隔符共 255）。脱敏屏障是 safeLabel 的形状检查，这个上限只是兜底：
	// 截到 40 个字符会让 OOXML 的 Word 与 Excel 类型在样本中无法分辨，而 L1 采集 MIME 结构正是为了 4b 的解析器。
	maxLabelRunes = 255
	// maxCharsetRunes 是字符集与自动来信关键字的字符数上限：已注册的字符集名称与 auto-submitted、precedence 的
	// 合法取值都远短于媒体类型，而主题编码字里的字符集位置直接取自 Subject 头（清单点名不输出完整主题），
	// 因此这些位置在形状检查之外另按更短的上限截断。
	maxCharsetRunes = 40
	// crockfordAlphabet 是回复令牌使用的小写 Crockford base32 字母表；令牌文本为其中的 48 个字符。
	crockfordAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"
)

// init 把 GBK 系列的字符集标签映射到 GB18030 解码器：标注为 gbk、gb2312 的 QQ 邮件可能含 GB18030 四字节字符（D2）。
// 它改写 go-message/charset 的全局表，只影响探测与本包的测试进程；4b 的解析器另行实现并测试这一映射。
func init() {
	for _, label := range []string{"gbk", "gb2312", "cp936", "x-gbk", "windows-936"} {
		charset.RegisterEncoding(label, simplifiedchinese.GB18030)
	}
}

// State 是 state.json 的内容：本次探测发出的邮件与各文件夹的补扫游标，不含个人信息。
type State struct {
	Schema  string            `json:"schema"`
	Mails   []Mail            `json:"mails"`
	Cursors map[string]Cursor `json:"cursors,omitempty"`
}

// Mail 是一封已发出的探测邮件：我方 ID、一次性令牌，以及 QQ 为它分配的各来源 ID。
// 令牌由测试进程内随机生成、用后即弃的密钥签发，不能用于任何真实验证。
// SubjectToken 为 true 表示这封邮件的主题标签携带令牌（L1b 起的新形态 [TC <任务 ID> <令牌>]）。第一轮留下的 state.json
// 没有这个字段，读入后为 false，按旧形态 [TC <任务 ID>] 归类，因此复用旧输出目录也不会误判。
// DeliveredData 不带 omitempty：没有候选时它必须写成空数组，以便与「这封邮件没记录过 DATA 候选」区分。
type Mail struct {
	ProbeID       string   `json:"probe_id"`
	TaskID        string   `json:"task_id"`
	MessageID     string   `json:"message_id"`
	Token         string   `json:"token"`
	SubjectToken  bool     `json:"subject_token,omitempty"`
	DeliveredSent string   `json:"delivered_sent,omitempty"`
	DeliveredCC   string   `json:"delivered_cc,omitempty"`
	DeliveredData []string `json:"delivered_data"`
}

// Cursor 是某个文件夹的补扫游标，与 imap.Cursor 字段相同。
type Cursor struct {
	UIDValidity uint32 `json:"uid_validity"`
	LastUID     uint32 `json:"last_uid"`
}

// Roles 是地址角色表；样本只输出角色，不输出地址。
type Roles struct {
	Bot       string
	Recipient string
	Allowed   []string
}

// Record 是 samples.jsonl 中非来信样本的记录：能力、发信结果、IDLE 观测与钥匙串往返。
type Record struct {
	Schema string `json:"schema"`
	Kind   string `json:"kind"`
	Data   any    `json:"data,omitempty"`
}

// Sample 是一封来信的脱敏样本，写入 samples.jsonl。
type Sample struct {
	Schema          string      `json:"schema"`
	Kind            string      `json:"kind"`
	Folder          string      `json:"folder,omitempty"`
	Client          string      `json:"client"`
	FromRole        string      `json:"from_role"`
	FromCaseVariant bool        `json:"from_case_variant"`
	Received        int         `json:"received"`
	Thread          Thread      `json:"thread"`
	Subject         Subject     `json:"subject"`
	MIME            []Part      `json:"mime"`
	Plain           Plain       `json:"plain"`
	HTML            HTML        `json:"html"`
	Auto            Auto        `json:"auto"`
	ReturnPath      ReturnPath  `json:"return_path"`
	AuthResults     AuthResults `json:"auth_results"`
}

// Thread 是线程头中各 Message-ID 的来源归类。
type Thread struct {
	InReplyTo  string   `json:"in_reply_to"`
	References []string `json:"references"`
}

// Subject 是主题的脱敏描述：白名单内的回复前缀、主题标签的状态、D4 严格文法的命中次数与编码方式；
// 不输出主题中的其他文字，也不输出标签里的任务 ID 与令牌。TagIntact 当且仅当 TagState 为 intact，保留它便于与第一轮的样本对照。
type Subject struct {
	Prefix    *string `json:"prefix"`
	TagIntact bool    `json:"tag_intact"`
	TagState  string  `json:"tag_state"`
	TagCount  int     `json:"tag_count"`
	Encoding  string  `json:"encoding"`
}

// Part 是 MIME 树中的一个部件；多部件只记类型与子部件，叶子另记字符集、传输编码与大小区间。
type Part struct {
	Type     string `json:"type"`
	Charset  string `json:"charset,omitempty"`
	Transfer string `json:"transfer,omitempty"`
	Size     string `json:"size,omitempty"`
	Children []Part `json:"children,omitempty"`
}

// Plain 是纯文本正文的结构特征：令牌个数与是否与发出的一致、令牌行前缀、分隔线类别与逐行类别。
type Plain struct {
	Tokens           int      `json:"tokens"`
	TokenMatchesSent bool     `json:"token_matches_sent"`
	TokenLinePrefix  string   `json:"token_line_prefix"`
	Separators       []string `json:"separators"`
	Skeleton         []string `json:"skeleton"`
}

// HTML 是 HTML 正文的结构特征：令牌个数、是否有 blockquote 与已知的引用标记。
type HTML struct {
	Tokens     int      `json:"tokens"`
	Blockquote bool     `json:"blockquote"`
	Markers    []string `json:"markers"`
}

// Auto 是判断非人工来信所依据的信号：各头部信号，以及解码后的主题是否以自动回复前缀开头（SubjectPrefix）。
// 第一轮实测的 QQ 假期自动回复不带任何头部信号，主题前缀是唯一可用的判据（D4）。
type Auto struct {
	AutoSubmitted   string `json:"auto_submitted"`
	XAutoreply      bool   `json:"x_autoreply"`
	Precedence      string `json:"precedence"`
	ReturnPathEmpty bool   `json:"return_path_empty"`
	SubjectPrefix   bool   `json:"subject_prefix"`
}

// ReturnPath 是 Return-Path 头的脱敏描述：头的个数、第一个取值是否为空信封 <>、第一个地址的角色（规则同 from_role；
// 没有该头或为空信封时为空串，无法解析时为 other），以及它与 From 地址规范化后是否相同、域名是否相同（不区分大小写）。
// 同域投递中它是唯一可能不由发信方书写的来源线索；与 from_role 一样只输出角色与布尔值，不输出地址。
type ReturnPath struct {
	Count            int    `json:"count"`
	Empty            bool   `json:"empty"`
	Role             string `json:"role"`
	MatchesFrom      bool   `json:"matches_from"`
	SameDomainAsFrom bool   `json:"same_domain_as_from"`
}

// AuthResults 是 Authentication-Results 头中的三项结果，只输出取值，不输出验证方与域名。
type AuthResults struct {
	Present bool   `json:"present"`
	DKIM    string `json:"dkim"`
	SPF     string `json:"spf"`
	DMARC   string `json:"dmarc"`
}

// Copy 是「已发送」或抄送副本中可记录的头；两个 ID 都带尖括号。
type Copy struct {
	ProbeID   string `json:"probe_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	OQMsgID   string `json:"x_oq_msgid,omitempty"`
}

// Notification 是一封合成探测通知的内容；除收件地址来自配置外，各字段都由探测工具生成，不含个人信息。
// 主题由 Tag 与 Title 组成，结构与 4b 渲染器约定的一致：Tag 以原始 ASCII 放在主题开头，Title 跟在一个空格之后、按需 B 编码。
type Notification struct {
	From      string
	To        string
	CC        string // 为空时不抄送
	Tag       string // 主题开头的标签，必须是 SubjectTag 输出的形状；新形态含一次性令牌
	Title     string // 标签之后的文字；为空时主题就是标签
	MessageID string // 含尖括号
	ProbeID   string
	Token     string // 页脚中的一次性令牌文本；为空时不写令牌行
	Date      time.Time
}

// IdleCheck 是预检或 FETCH 检查会话中一次 10 秒 Idle 的原始观测值。
type IdleCheck struct {
	Messages0        uint32 `json:"messages_before"`
	Messages1        uint32 `json:"messages_after"`
	UIDNext0         uint32 `json:"uidnext_before"`
	UIDNext1         uint32 `json:"uidnext_after"`
	NewMail          bool   `json:"new_mail"`
	Returned         bool   `json:"returned"`
	DeadlineExceeded bool   `json:"deadline_exceeded"`
	// Elapsed 与 RoundTrip 是 time.Duration，JSON 编码为纳秒，字段名因此用 _ns；schema/2 中误名为 _ms。
	Elapsed   time.Duration `json:"elapsed_ns"`
	RoundTrip time.Duration `json:"round_trip_ns"`
}

var (
	// idPattern 匹配 DATA 响应中的候选 ID：带尖括号的 <左部@右部>，以及不带尖括号、形如 tencent_…@qq.com 的子串。
	idPattern = regexp.MustCompile(`<[^<>@\s]+@[^<>@\s]+>|tencent_[^<>@\s]*@qq\.com`)
	// addressPattern 匹配脱敏后文本中残留的形似地址的部分。
	addressPattern = regexp.MustCompile(`[^<>@\s]+@[^<>@\s]+`)
	// tencentPattern 判断一个带尖括号的 ID 是否形如 <tencent_…@qq.com>。
	tencentPattern = regexp.MustCompile(`^<tencent_[^<>@\s]*@qq\.com>$`)
	// tokenPattern 匹配 48 个 Crockford base32 字符（不区分大小写）；边界另行检查，长于 48 的连续串不算令牌。
	tokenPattern = regexp.MustCompile(`(?i)[0-9abcdefghjkmnpqrstvwxyz]{48}`)
	// strictTagPattern 是 D4 的主题标签严格文法：[TC、一个 U+0020、10 个任务 ID 字符、恰好一个 U+0020、48 个令牌字符与 ]，
	// 字符集为小写字母表、不区分大小写的匹配一律不算；tag_count 统计它在解码后的主题中不重叠命中的次数。
	strictTagPattern = regexp.MustCompile(`\[TC [` + crockfordAlphabet + `]{10} [` + crockfordAlphabet + `]{48}\]`)
	// tagShapePattern 是 ComposeNotification 接受的标签形状，即 SubjectTag 的两种输出：锚定整串，令牌部分可以没有。
	// Go 正则的 $ 只匹配文本末尾，结尾带换行的标签同样被拒绝。
	tagShapePattern = regexp.MustCompile(`^\[TC [` + crockfordAlphabet + `]{10}( [` + crockfordAlphabet + `]{48})?\]$`)
	// encodedWordPattern 匹配主题中的第一个 RFC 2047 encoded-word，用于记录字符集与编码方式。
	encodedWordPattern = regexp.MustCompile(`=\?([^?]+)\?([BbQq])\?`)
	// quotePrefixPattern 匹配只由引用标记与空白组成的行前缀。
	quotePrefixPattern = regexp.MustCompile(`^[>|\s\x{00A0}]*$`)
	// headerLinePattern 匹配引用头块中的一行。
	headerLinePattern = regexp.MustCompile(`^(发件人|发送时间|发件时间|收件人|抄送|主题|日期|时间|From|Sent|Date|To|Cc|Subject|Reply-To)[\s\x{00A0}]*[:：]`)
	// fromLinePattern 匹配引用头块的首行，用来识别没有分隔线的 Foxmail 头块。
	fromLinePattern = regexp.MustCompile(`^(发件人|From)[\s\x{00A0}]*[:：]`)
	// labelPattern 匹配可以原样输出的短标签：只含 ASCII 字母、数字与 . _ + / -，且至少有一个字母。
	labelPattern = regexp.MustCompile(`^[a-z0-9._+/-]*[a-z][a-z0-9._+/-]*$`)
)

// separatorPatterns 是分隔线与引用头行的正则表；类别名是 L1 的观测对象，实际对应哪个客户端由 L1 的结果确定。
// QQ 的三种形式按分隔线长度与有无破折号区分：A 为长破折号包围、B 为短破折号包围、C 为单独一行。
var separatorPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"qq_a_zh", regexp.MustCompile(`^-{6,}[\s\x{00A0}]*原始邮件[\s\x{00A0}]*-{6,}$`)},
	{"qq_a_en", regexp.MustCompile(`^-{6,}[\s\x{00A0}]*Original[\s\x{00A0}]*-{6,}$`)},
	{"qq_b_zh", regexp.MustCompile(`^-{1,5}[\s\x{00A0}]*原始邮件[\s\x{00A0}]*-{1,5}$`)},
	{"qq_b_en", regexp.MustCompile(`^-{1,5}[\s\x{00A0}]*Original[\s\x{00A0}]*-{1,5}$`)},
	{"qq_c_zh", regexp.MustCompile(`^原始邮件$`)},
	{"wrote_zh", regexp.MustCompile(`写道[：:][\s\x{00A0}]*$`)},
	{"wrote_en", regexp.MustCompile(`(?i)wrote:[\s\x{00A0}]*$`)},
	{"dashes", regexp.MustCompile(`^-{3,}$`)},
}

// htmlMarkers 是 HTML 正文中已知的引用标记；同样只输出类别名，不输出原文。
var htmlMarkers = []struct {
	name string
	re   *regexp.Regexp
}{
	{"qq_div_style", regexp.MustCompile(`(?i)font-family:[\s]*Arial Narrow`)},
	{"qq_original_zh", regexp.MustCompile(`原始邮件`)},
	{"gmail_quote", regexp.MustCompile(`(?i)class="[^"]*gmail_quote`)},
	{"apple_cite", regexp.MustCompile(`(?i)type="cite"`)},
	{"outlook_rplyfwd", regexp.MustCompile(`(?i)id="divRplyFwdMsg"`)},
	{"moz_cite", regexp.MustCompile(`(?i)class="moz-cite-prefix"`)},
}

// authPatterns 匹配 Authentication-Results 中各验证方法的结果值。
var authPatterns = map[string]*regexp.Regexp{
	"dkim":  regexp.MustCompile(`(?i)\bdkim=([a-zA-Z]+)`),
	"spf":   regexp.MustCompile(`(?i)\bspf=([a-zA-Z]+)`),
	"dmarc": regexp.MustCompile(`(?i)\bdmarc=([a-zA-Z]+)`),
}

// authValues 是 Authentication-Results 中各方法的已知结果取值（RFC 8601 第 2.7 节），其余记为 other。
var authValues = []string{"pass", "fail", "none", "neutral", "softfail", "temperror", "permerror", "policy"}

// autoReplyPrefixes 是自动回复的主题前缀，与产品的主题前缀规则相同（D4 与「已定的实现细节」，2026-09-21 维护者确认的破例）。
// 比较前先删去主题中的全部空白，再按 ASCII 不区分大小写匹配，因此 Automatic reply: 对应最后一项。
var autoReplyPrefixes = []string{"自动回复：", "自动回复:", "自動回覆：", "自動回覆:", "Auto-Reply:", "AutoReply:", "AutomaticReply:"}

// replyPrefixes 是主题前缀白名单：回复、转发前缀与自动回复前缀，按 ASCII 不区分大小写匹配（非 ASCII 部分按原样比较），
// 可以重复出现；[TC 之前去掉空白的文字只由它们组成时才输出来信中的原文，冒号的全角半角因此直接出现在样本里。
// 每一项都以冒号结尾且冒号只出现在末尾，任何两项都不会同时是同一段文字的前缀，逐项贪心匹配没有歧义。
var replyPrefixes = slices.Concat([]string{"回复：", "回复:", "答复：", "答复:", "转发：", "转发:", "Re:", "Fwd:", "FW:"}, autoReplyPrefixes)

// precedenceAuto 是判为非人工来信的 Precedence 取值。
var precedenceAuto = []string{"auto_reply", "bulk", "junk", "list"}

// daemonLocals 是退信常用的发件人本地部分。
var daemonLocals = []string{"mailer-daemon", "postmaster"}

// bodies 是从 MIME 树中取出的首个纯文本与 HTML 正文。
type bodies struct {
	plain string
	html  string
}

// 解析失败时返回的哨兵错误。go-message 在头部畸形时会把整行来信原文放进错误文本
// （本地实验：`message: malformed MIME header line: <整行原文>`、`… header key: <字段名>`），
// 而调用处会把错误打印到测试输出，包装底层错误就绕过了「只输出脱敏的结构样本」。
// 因此 Analyze 与 CopyHeaders 只返回下面这些不携带任何来信字节的错误。
var (
	// ErrMalformedHeader 表示来信头部无法解析：首行、某个头行或字段名不合规范。
	ErrMalformedHeader = errors.New("cannot parse the message header")
	// ErrHeaderTooLarge 表示来信头部超过解析器的上限，整封邮件都没有解析。
	ErrHeaderTooLarge = errors.New("message header exceeds the parser limit")
)

// headerTooLargeText 是 go-message v0.18.2 在头部超限时返回的固定错误文本，其中不含来信字节。
// 该错误未导出也没有包装，只能按文本比对；上游改写文本时这一类退回 ErrMalformedHeader，仍不会泄露来信字节。
const headerTooLargeText = "message: header exceeds maximum size"

// parseMessage 解析一封来信的头部，失败时只返回哨兵错误。go-message 只在读取头部失败时返回 nil 实体，
// 此时按原因归类；未知字符集与传输编码不算失败（实体仍可读，而这类错误文本同样含来信字节），照常继续归类。
// MIME 结构畸形不在这里报错：describePart 保留已解析出的部件，样本记录到哪一层为止。
func parseMessage(raw []byte) (*message.Entity, error) {
	entity, err := message.Read(bytes.NewReader(raw))
	if entity != nil {
		return entity, nil
	}
	if err != nil && err.Error() == headerTooLargeText {
		return nil, ErrHeaderTooLarge
	}
	return nil, ErrMalformedHeader
}

// Analyze 解析一封来信并返回脱敏样本。ok 为 false 表示它没有引用任何探测邮件，或它本身就是探测邮件的副本
// （带 ProbeHeader，或 Message-ID 等于已记录的我方、已发送、抄送副本 ID）。
// 样本只含结构特征：不输出正文、显示名、日期、完整主题（主题标签只输出状态与命中次数，不输出其中的任务 ID 与令牌）
// 与 Received 头，地址（含 Return-Path）一律换成角色。
func Analyze(raw []byte, st State, roles Roles) (Sample, bool, error) {
	entity, err := parseMessage(raw)
	if err != nil {
		return Sample{}, false, err
	}
	h := mail.Header{Header: entity.Header}
	ownID := bracket(headerMessageID(h))
	if h.Get(ProbeHeader) != "" || isProbeCopy(ownID, st.Mails) {
		return Sample{}, false, nil
	}

	var b bodies
	root := describePart(entity, &b, 0)
	thread, ids := threadOf(h, st.Mails)
	decoded, _ := h.Subject()
	subject := subjectInfo(decoded, h.Get("Subject"), st.Mails)
	plain := analyzePlain(b.plain, st.Mails)
	htmlTokens, htmlMatched, _ := findTokens(b.html, st.Mails)
	if !referencesProbe(ids, decoded, plain.TokenMatchesSent || htmlMatched, st.Mails) {
		return Sample{}, false, nil
	}

	fromAddress := firstAddress(h, "From")
	auto := autoSignals(h, decoded)
	sample := Sample{
		Schema:          Schema,
		Kind:            classifyKind(root.Type, fromAddress, auto, b.plain),
		Client:          safeClient(clientOf(h)),
		FromRole:        roles.roleOf(fromAddress),
		FromCaseVariant: fromAddress != strings.ToLower(fromAddress),
		Received:        h.FieldsByKey("Received").Len(),
		Thread:          thread,
		Subject:         subject,
		MIME:            []Part{root},
		Plain:           plain,
		HTML:            HTML{Tokens: htmlTokens, Blockquote: strings.Contains(strings.ToLower(b.html), "<blockquote"), Markers: markersOf(b.html)},
		Auto:            auto,
		ReturnPath:      returnPathOf(h, fromAddress, roles),
		AuthResults:     authResultsOf(h),
	}
	return sample, true, nil
}

// headerMessageID 读取本封邮件的 Message-ID，不带尖括号；解析失败时退回原始取值。
func headerMessageID(h mail.Header) string {
	id, err := h.MessageID()
	if err != nil || id == "" {
		return strings.TrimSpace(h.Get("Message-Id"))
	}
	return id
}

// isProbeCopy 判断某个 Message-ID 是否等于已记录的我方 ID、「已发送」副本或抄送副本，即这封信是探测邮件本身的副本。
func isProbeCopy(id string, mails []Mail) bool {
	if id == "" {
		return false
	}
	for _, m := range mails {
		if id == m.MessageID || id == m.DeliveredSent || id == m.DeliveredCC {
			return true
		}
	}
	return false
}

// referencesProbe 判断来信是否引用了探测邮件：线程头含已记录的任一 ID、主题含合成任务 ID，
// 或正文含探测令牌（tokenMatched 由纯文本与 HTML 两处正文一并得出，HTML 同样是正文）。
func referencesProbe(ids []string, subject string, tokenMatched bool, mails []Mail) bool {
	for _, id := range ids {
		if known(id, mails) {
			return true
		}
	}
	for _, m := range mails {
		if m.TaskID != "" && strings.Contains(subject, m.TaskID) {
			return true
		}
	}
	return tokenMatched
}

// known 判断 ID 是否等于任一已记录的来源。
func known(id string, mails []Mail) bool {
	source := sourceOf(id, mails, true)
	return source != "other" && source != "other_tencent"
}

// ClassifyID 按相等关系归类邮件头中的一个 Message-ID：ours、delivered_sent、delivered_cc、delivered_data，
// 同时等于多个来源时按此顺序以 + 连接；都不相等时形如 tencent_…@qq.com 的记为 other_tencent，其余记为 other；缺失记为 missing。
func ClassifyID(id string, mails []Mail) string {
	if id == "" {
		return "missing"
	}
	return sourceOf(bracket(id), mails, true)
}

// sourceOf 计算来源标签。withData 为 false 时不与 DATA 响应中的候选比较：脱敏 DATA 响应本身时，
// 每个候选必然等于自己，比较结果不携带信息。
func sourceOf(id string, mails []Mail, withData bool) string {
	var sources []string
	if matches(id, mails, func(m Mail) []string { return []string{m.MessageID} }) {
		sources = append(sources, "ours")
	}
	if matches(id, mails, func(m Mail) []string { return []string{m.DeliveredSent} }) {
		sources = append(sources, "delivered_sent")
	}
	if matches(id, mails, func(m Mail) []string { return []string{m.DeliveredCC} }) {
		sources = append(sources, "delivered_cc")
	}
	if withData && matches(id, mails, func(m Mail) []string { return m.DeliveredData }) {
		sources = append(sources, "delivered_data")
	}
	switch {
	case len(sources) > 0:
		return strings.Join(sources, "+")
	case tencentPattern.MatchString(id):
		return "other_tencent"
	default:
		return "other"
	}
}

// matches 判断 id 是否等于 pick 从任一封探测邮件中取出的非空 ID。
func matches(id string, mails []Mail, pick func(Mail) []string) bool {
	for _, m := range mails {
		for _, candidate := range pick(m) {
			if candidate != "" && candidate == id {
				return true
			}
		}
	}
	return false
}

// bracket 给没有尖括号的 Message-ID 补上尖括号，使各来源的 ID 能按字符串相等比较。
func bracket(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || strings.HasPrefix(id, "<") {
		return id
	}
	return "<" + id + ">"
}

// RedactDataResponse 按相等关系脱敏 DATA 的 250 响应文本，并返回其中的候选 ID（按出现顺序去重，都带尖括号）。
// 候选是带尖括号的 <左部@右部>，以及不带尖括号、形如 tencent_…@qq.com 的子串；其余形似地址的部分换成 <addr>。
func RedactDataResponse(text string, mails []Mail) (string, []string) {
	ids := []string{}
	redacted := idPattern.ReplaceAllStringFunc(text, func(match string) string {
		id := bracket(match)
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
		return "<" + sourceOf(id, mails, false) + ">"
	})
	return addressPattern.ReplaceAllString(redacted, "<addr>"), ids
}

// threadOf 归类 In-Reply-To 与 References 中的 ID，并返回其中的原始 ID 供判断是否引用了探测邮件。
// 头缺失记为 missing；头存在但没有可解析的 ID 记为 other；In-Reply-To 含多个 ID 时各归类以逗号连接。
func threadOf(h mail.Header, mails []Mail) (Thread, []string) {
	thread := Thread{References: []string{}}
	var all []string
	inReplyTo, raw := msgIDList(h, "In-Reply-To")
	all = append(all, inReplyTo...)
	switch {
	case raw == "":
		thread.InReplyTo = "missing"
	case len(inReplyTo) == 0:
		thread.InReplyTo = "other"
	default:
		classes := make([]string, 0, len(inReplyTo))
		for _, id := range inReplyTo {
			classes = append(classes, ClassifyID(id, mails))
		}
		thread.InReplyTo = strings.Join(classes, ",")
	}
	references, rawReferences := msgIDList(h, "References")
	all = append(all, references...)
	for _, id := range references {
		thread.References = append(thread.References, ClassifyID(id, mails))
	}
	if len(references) == 0 && rawReferences != "" {
		thread.References = append(thread.References, "other")
	}
	return thread, all
}

// msgIDList 解析一个消息 ID 列表头，返回带尖括号的 ID 与该头的原始取值；解析失败时返回已解析出的部分。
func msgIDList(h mail.Header, key string) ([]string, string) {
	raw := strings.TrimSpace(h.Get(key))
	parsed, _ := h.MsgIDList(key)
	ids := make([]string, 0, len(parsed))
	for _, id := range parsed {
		ids = append(ids, bracket(id))
	}
	return ids, raw
}

// subjectInfo 归类主题：tag_count 与 tag_state 见 tagState；找不到 [TC（ASCII 不区分大小写）时前缀为 null，
// 找到时，第一个 [TC 之前去掉空白的文字只由白名单前缀组成才输出原文（至多 20 个字符），否则记为 other。
// 不输出主题中的其他文字。
func subjectInfo(decoded, rawValue string, mails []Mail) Subject {
	count := len(strictTagPattern.FindAllStringIndex(decoded, -1))
	state := tagState(decoded, count, mails)
	subject := Subject{TagIntact: state == "intact", TagState: state, TagCount: count, Encoding: encodingOf(rawValue)}
	// lowerASCII 不改变字节长度，下标可以直接用于原文。
	index := strings.Index(lowerASCII(decoded), "[tc")
	if index < 0 {
		return subject
	}
	cleaned := removeSpace(decoded[:index])
	prefix := "other"
	if onlyReplyPrefixes(cleaned) {
		prefix = truncateRunes(cleaned, maxPrefixRunes)
	}
	subject.Prefix = &prefix
	return subject
}

// expectedTag 是一封已记录邮件的期望标签：原文、删去空白并转小写后的形式，以及截断判定要求公共前缀至少覆盖的字节数。
type expectedTag struct {
	text     string
	squeezed string
	minKept  int
}

// expectedTags 为 state.json 中的每封邮件构造期望标签：SubjectToken 为 true 时是 SubjectTag(任务 ID, 令牌)，
// 否则是旧形态 SubjectTag(任务 ID, "")。截断判定要求公共前缀至少覆盖 [tc 加完整的任务 ID（删去空白后二者之间没有空格）。
// 没有任务 ID 的记录不产生期望标签，否则 [TC ] 这样的残片也会被当成某封邮件的标签。
func expectedTags(mails []Mail) []expectedTag {
	tags := make([]expectedTag, 0, len(mails))
	for _, m := range mails {
		if m.TaskID == "" {
			continue
		}
		carried := ""
		if m.SubjectToken {
			carried = m.Token
		}
		text := SubjectTag(m.TaskID, carried)
		tags = append(tags, expectedTag{text: text, squeezed: lowerASCII(removeSpace(text)), minKept: len("[tc") + len(m.TaskID)})
	}
	return tags
}

// tagState 按清单顺序返回第一个成立的主题标签状态：missing（没有 [TC，ASCII 不区分大小写）、multiple（严格文法命中 ≥2 次，
// D4 一律拒绝）、intact（某封邮件的期望标签按字节原样出现）、case_changed（按 ASCII 不区分大小写出现）、
// whitespace_changed（主题与期望标签都删去全部空白后按 ASCII 不区分大小写出现）、truncated（见 truncatedTag），其余为 other。
// 每一步都与全部已记录邮件的期望标签逐一比较，任何一封邮件满足前一步都先于后一步。
// 大小写只折叠 ASCII：strings.ToLower 会把开尔文符号 U+212A 等非 ASCII 字符折成字母表中的 k，也会改变字节长度。
func tagState(decoded string, count int, mails []Mail) string {
	folded := lowerASCII(decoded)
	if !strings.Contains(folded, "[tc") {
		return "missing"
	}
	if count >= 2 {
		return "multiple"
	}
	tags := expectedTags(mails)
	for _, tag := range tags {
		if strings.Contains(decoded, tag.text) {
			return "intact"
		}
	}
	for _, tag := range tags {
		if strings.Contains(folded, lowerASCII(tag.text)) {
			return "case_changed"
		}
	}
	squeezed := lowerASCII(removeSpace(decoded))
	for _, tag := range tags {
		if strings.Contains(squeezed, tag.squeezed) {
			return "whitespace_changed"
		}
	}
	for _, tag := range tags {
		if truncatedTag(squeezed, tag) {
			return "truncated"
		}
	}
	return "other"
}

// truncatedTag 判断标签是否被截断：squeezed（删去空白、转小写的主题）中从某个 [tc 起的剩余部分与 tag.squeezed 的公共前缀
// 至少覆盖 [tc 加完整的任务 ID、短于整个期望标签，且公共前缀之后是主题末尾，或是字母表与 ] 之外的字符（例如省略号）。
// 公共前缀之后若是字母表字符或 ]，标签是在该处被改写或提前闭合，而不是被截断，归入 other。
func truncatedTag(squeezed string, tag expectedTag) bool {
	for rest := squeezed; ; rest = rest[1:] {
		index := strings.Index(rest, "[tc")
		if index < 0 {
			return false
		}
		rest = rest[index:]
		kept := commonPrefixLen(rest, tag.squeezed)
		if kept >= tag.minKept && kept < len(tag.squeezed) && (kept == len(rest) || (rest[kept] != ']' && !isTokenChar(rest[kept]))) {
			return true
		}
	}
}

// commonPrefixLen 返回两段文字逐字节相同的前缀长度。
func commonPrefixLen(a, b string) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// removeSpace 删去文字中的全部空白（unicode.IsSpace，含全角空格 U+3000 与不换行空格 U+00A0）。
func removeSpace(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, text)
}

// lowerASCII 只把 ASCII 大写字母 A–Z 转成小写，其余字节原样保留：结果与原文逐字节对齐，
// 按它求出的下标可以直接用于原文，也不会像 strings.ToLower 那样把非 ASCII 字符折成 ASCII 字母。
func lowerASCII(text string) string {
	b := []byte(text)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// onlyReplyPrefixes 判断去掉空白的文字是否只由白名单中的前缀组成（可重复，按 ASCII 不区分大小写）；空文字视为只有前缀。
// lowerASCII 不改变字节长度，按前缀长度切片不会错位；非 ASCII 字节原样比较，全角冒号与半角冒号因此仍是两项。
func onlyReplyPrefixes(text string) bool {
	folded := lowerASCII(text)
	for folded != "" {
		matched := false
		for _, prefix := range replyPrefixes {
			if prefix = lowerASCII(prefix); strings.HasPrefix(folded, prefix) {
				folded, matched = folded[len(prefix):], true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// hasAutoReplyPrefix 判断解码后的主题删去全部空白、转小写（只折叠 ASCII）后是否以任一自动回复前缀开头，
// 与产品的主题前缀规则相同；前缀之后是什么不影响判定。
func hasAutoReplyPrefix(subject string) bool {
	folded := lowerASCII(removeSpace(subject))
	for _, prefix := range autoReplyPrefixes {
		if strings.HasPrefix(folded, lowerASCII(prefix)) {
			return true
		}
	}
	return false
}

// encodingOf 记录主题的编码方式：第一个 encoded-word 的字符集与 B/Q，没有 encoded-word 时记为 plain 或 8bit。
// 字符集位置经 safeLabel 约束：畸形 encoded-word 的这一位置可以是主题中的任意文字，原样输出会绕过脱敏。
func encodingOf(rawValue string) string {
	if match := encodedWordPattern.FindStringSubmatch(rawValue); match != nil {
		return safeCharset(match[1]) + "/" + strings.ToUpper(match[2])
	}
	for i := range len(rawValue) {
		if rawValue[i] > 0x7e {
			return "8bit"
		}
	}
	return "plain"
}

// describePart 递归描述一个 MIME 部件并把首个纯文本与 HTML 正文写入 b；depth 与部件数有上限，避免畸形邮件耗尽栈或输出。
func describePart(e *message.Entity, b *bodies, depth int) Part {
	// 三项取值都来自来信可控的字节（Content-Type 解析失败时媒体类型就是整行原文），一律经 safeLabel 约束。
	mediaType, params, _ := e.Header.ContentType()
	part := Part{
		Type:     safeLabel(mediaType),
		Charset:  safeCharset(params["charset"]),
		Transfer: safeCharset(e.Header.Get("Content-Transfer-Encoding")),
	}
	if reader := e.MultipartReader(); reader != nil && depth < maxDepth {
		part.Charset, part.Transfer = "", ""
		for len(part.Children) < maxParts {
			child, err := reader.NextPart()
			if err != nil {
				break
			}
			part.Children = append(part.Children, describePart(child, b, depth+1))
		}
		return part
	}
	data, _ := io.ReadAll(io.LimitReader(e.Body, maxPartBytes+1))
	part.Size = sizeBucket(len(data))
	switch part.Type {
	case "text/plain":
		if b.plain == "" {
			b.plain = string(data)
		}
	case "text/html":
		if b.html == "" {
			b.html = string(data)
		}
	}
	return part
}

// sizeBucket 把部件的字节数归入固定区间，不输出精确长度。
func sizeBucket(n int) string {
	switch {
	case n < 1<<10:
		return "0-1KiB"
	case n < 4<<10:
		return "1-4KiB"
	case n < 16<<10:
		return "4-16KiB"
	case n < 64<<10:
		return "16-64KiB"
	case n < 256<<10:
		return "64-256KiB"
	case n <= maxPartBytes:
		return "256KiB-1MiB"
	default:
		return ">1MiB"
	}
}

// analyzePlain 归类纯文本正文：统计令牌、记录令牌行的引用前缀、按正则表归类分隔线与引用头，并输出逐行类别。
// 连续的同类正文、空行与引用文字合并为一项，头块中的每一行单独保留。
func analyzePlain(text string, mails []Mail) Plain {
	tokens, matched, prefix := findTokens(text, mails)
	plain := Plain{Tokens: tokens, TokenMatchesSent: matched, TokenLinePrefix: prefix, Separators: []string{}, Skeleton: []string{}}
	quoted, headerZone := false, false
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		class := "text"
		switch {
		case trimmed == "":
			class, headerZone = "blank", false
		case separatorName(trimmed) != "":
			class, quoted, headerZone = "sep", true, true
			plain.Separators = appendOnce(plain.Separators, separatorName(trimmed))
		case strings.HasPrefix(trimmed, ">"):
			class, quoted, headerZone = "quote_text", true, false
			plain.Separators = appendOnce(plain.Separators, "gt_quote")
		case headerZone && headerLinePattern.MatchString(trimmed):
			class = "header"
		case !quoted && fromLinePattern.MatchString(trimmed):
			// 没有分隔线就直接开始的引用头块，Foxmail 是已知的这类客户端。
			class, quoted, headerZone = "header", true, true
			plain.Separators = appendOnce(plain.Separators, "foxmail_header")
		case quoted:
			class, headerZone = "quote_text", false
		default:
			headerZone = false
		}
		if class == "header" || class == "sep" || len(plain.Skeleton) == 0 || plain.Skeleton[len(plain.Skeleton)-1] != class {
			plain.Skeleton = append(plain.Skeleton, class)
		}
	}
	return plain
}

// separatorName 返回行匹配到的分隔线或引用头类别名，没有匹配时返回空串。
func separatorName(line string) string {
	for _, pattern := range separatorPatterns {
		if pattern.re.MatchString(line) {
			return pattern.name
		}
	}
	return ""
}

// appendOnce 把类别名追加到列表中，已有时不重复追加。
func appendOnce(values []string, value string) []string {
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

// findTokens 在文本中查找令牌：48 个字母表字符且两端不是字母表字符。返回个数、是否有一个与发出的令牌相同，
// 以及第一个令牌所在行的前缀（只由引用标记与空白组成时原样输出，至多 20 个字符，否则记为 other）。
func findTokens(text string, mails []Mail) (count int, matched bool, prefix string) {
	for _, span := range tokenPattern.FindAllStringIndex(text, -1) {
		if !tokenBoundary(text, span[0], span[1]) {
			continue
		}
		count++
		candidate := strings.ToLower(text[span[0]:span[1]])
		for _, m := range mails {
			if m.Token != "" && strings.ToLower(m.Token) == candidate {
				matched = true
			}
		}
		if count == 1 {
			prefix = linePrefix(text[:span[0]])
		}
	}
	return count, matched, prefix
}

// tokenBoundary 判断候选令牌两端不是字母表字符，避免把更长的连续串当成令牌。
func tokenBoundary(text string, start, end int) bool {
	if start > 0 && isTokenChar(text[start-1]) {
		return false
	}
	return end >= len(text) || !isTokenChar(text[end])
}

// isTokenChar 判断一个字节是否属于令牌字母表（不区分大小写）。
func isTokenChar(c byte) bool {
	if 'A' <= c && c <= 'Z' {
		c += 'a' - 'A'
	}
	return strings.IndexByte(crockfordAlphabet, c) >= 0
}

// linePrefix 取文本末行的内容作为令牌行前缀，只由引用标记与空白组成时原样输出，否则记为 other。
func linePrefix(before string) string {
	if index := strings.LastIndexByte(before, '\n'); index >= 0 {
		before = before[index+1:]
	}
	if !quotePrefixPattern.MatchString(before) {
		return "other"
	}
	return truncateRunes(before, maxPrefixRunes)
}

// markersOf 归类 HTML 正文中的已知引用标记，只输出类别名。
func markersOf(html string) []string {
	markers := []string{}
	for _, marker := range htmlMarkers {
		if marker.re.MatchString(html) {
			markers = append(markers, marker.name)
		}
	}
	return markers
}

// autoSignals 读取判断非人工来信所依据的信号：Auto-Submitted 与 Precedence 只保留分号前的关键字，
// SubjectPrefix 记录解码后的主题 subject 是否以自动回复前缀开头。
func autoSignals(h mail.Header, subject string) Auto {
	return Auto{
		AutoSubmitted:   keyword(h.Get("Auto-Submitted")),
		XAutoreply:      h.Get("X-Autoreply") != "" || h.Get("X-Autorespond") != "",
		Precedence:      keyword(h.Get("Precedence")),
		ReturnPathEmpty: strings.TrimSpace(h.Get("Return-Path")) == "<>",
		SubjectPrefix:   hasAutoReplyPrefix(subject),
	}
}

// returnPathOf 描述 Return-Path 头：个数、第一个取值是否为空信封、第一个地址的角色（规则同 from_role：没有该头或为空信封时
// 为空串，无法解析时为 other），以及它与 From 地址规范化后是否相同、域名是否相同（不区分大小写）。
// 任一地址缺失或无法解析时两项对照都为 false；只输出角色与布尔值，地址本身不离开本函数。
func returnPathOf(h mail.Header, fromAddress string, roles Roles) ReturnPath {
	path := ReturnPath{Count: h.FieldsByKey("Return-Path").Len()}
	if path.Count == 0 {
		return path
	}
	if strings.TrimSpace(h.Get("Return-Path")) == "<>" {
		path.Empty = true
		return path
	}
	address := firstAddress(h, "Return-Path")
	path.Role = roles.roleOf(address)
	if address == "" || fromAddress == "" {
		return path
	}
	path.MatchesFrom = normalizeAddress(address) == normalizeAddress(fromAddress)
	domain := addressDomain(address)
	path.SameDomainAsFrom = domain != "" && strings.EqualFold(domain, addressDomain(fromAddress))
	return path
}

// addressDomain 取地址中最后一个 @ 之后的域名：带引号的本地部分可以含 @，所以按最后一个切分；没有 @ 时返回空串。
func addressDomain(address string) string {
	index := strings.LastIndexByte(address, '@')
	if index < 0 {
		return ""
	}
	return strings.TrimSpace(address[index+1:])
}

// keyword 取头部取值中分号之前的关键字，再经 safeLabel 约束为标签形状：这两个头同样完全由来信控制，
// 合法取值也同样是极小的一组 token，畸形邮件可以把任意文字放进去，不加约束就会绕过「不输出正文与完整主题」。
func keyword(value string) string {
	value, _, _ = strings.Cut(value, ";")
	return safeCharset(value)
}

// classifyKind 判断来信类别：multipart/report 或来自 MAILER-DAEMON、postmaster 的记为 bounce（先于其他判定）；
// 满足任一自动来信信号记为 auto，其中包括与产品规则相同的主题前缀（auto.SubjectPrefix）与 QQ 假期自动回复的固定开头：
// 第一轮的假期自动回复只有 text/html，后者只查纯文本、对它不起作用，前者补上了这一漏判。其余记为 reply。
func classifyKind(mediaType, fromAddress string, auto Auto, plain string) string {
	local, _, _ := strings.Cut(strings.ToLower(fromAddress), "@")
	if mediaType == "multipart/report" || slices.Contains(daemonLocals, local) {
		return "bounce"
	}
	switch {
	case auto.AutoSubmitted != "" && auto.AutoSubmitted != "no",
		auto.XAutoreply,
		slices.Contains(precedenceAuto, auto.Precedence),
		auto.ReturnPathEmpty,
		auto.SubjectPrefix,
		strings.HasPrefix(strings.TrimSpace(plain), "这是来自QQ邮箱的假期自动回复邮件"):
		return "auto"
	default:
		return "reply"
	}
}

// authResultsOf 读取 Authentication-Results 中的 dkim、spf 与 dmarc 结果。
func authResultsOf(h mail.Header) AuthResults {
	value := h.Get("Authentication-Results")
	results := AuthResults{Present: strings.TrimSpace(value) != ""}
	if !results.Present {
		return results
	}
	results.DKIM = authResult(value, "dkim")
	results.SPF = authResult(value, "spf")
	results.DMARC = authResult(value, "dmarc")
	return results
}

// authResult 取出 Authentication-Results 中某一项的结果值，并收敛到 authValues 中的已知取值，
// 其余记为 other：这个位置同样取自来信可控的字节，取值长度没有上限，白名单之外的文字不进样本。
func authResult(value, method string) string {
	match := authPatterns[method].FindStringSubmatch(value)
	if match == nil {
		return ""
	}
	result := strings.ToLower(match[1])
	if !slices.Contains(authValues, result) {
		return "other"
	}
	return result
}

// clientOf 取 X-Mailer 或 User-Agent 作为客户端标识。
func clientOf(h mail.Header) string {
	if value := h.Get("X-Mailer"); value != "" {
		return value
	}
	return h.Get("User-Agent")
}

// safeClient 把客户端标识（X-Mailer 或 User-Agent）约束为形状后再输出：这两个头同样完全由来信控制，
// 是样本中唯一原样输出来信文字的位置，畸形邮件可以把正文或主题塞进去，只截断拦不住。
// 只接受可打印 ASCII（0x20–0x7e）且不含引号的取值，其余一律记为 other；合法取值仍按清单截到 40 个字符。
// 取值为空表示两个头都没有，保留空串以便与「有客户端标识但形状不合」区分。
func safeClient(value string) string {
	value = strings.TrimSpace(value)
	for i := range len(value) {
		if c := value[i]; c < 0x20 || c > 0x7e || c == '"' || c == '\'' {
			return "other"
		}
	}
	return truncateRunes(value, maxClientRunes)
}

// firstAddress 取某个地址头中的第一个地址；解析失败时返回空串，样本据此记为 other。
func firstAddress(h mail.Header, key string) string {
	addresses, err := h.AddressList(key)
	if err != nil || len(addresses) == 0 {
		return ""
	}
	return addresses[0].Address
}

// normalizeAddress 按 Phase 3 的规则规范化地址：去掉首尾空白并转小写（依据是 QQ 邮箱地址不区分大小写的假设）。
func normalizeAddress(address string) string {
	return strings.ToLower(strings.TrimSpace(address))
}

// roleOf 按规范化（去空白、转小写）后的地址判断角色：bot、recipient、allowed，其余为 other。
func (r Roles) roleOf(address string) string {
	normalized := normalizeAddress(address)
	switch {
	case normalized == "":
		return "other"
	case normalized == strings.ToLower(r.Bot):
		return "bot"
	case normalized == strings.ToLower(r.Recipient):
		return "recipient"
	}
	for _, allowed := range r.Allowed {
		if normalized == strings.ToLower(allowed) {
			return "allowed"
		}
	}
	return "other"
}

// safeLabel 把来信可控的短标签（媒体类型、字符集、传输编码、主题 encoded-word 的字符集、Auto-Submitted、
// Precedence）转小写后约束为标签形状：含空白、非 ASCII 或其他字符时一律记为 other，
// 形状合法时截到至多 maxLabelRunes 个字符（形状检查才是脱敏屏障，截断只是兜底）。
// 这些位置的取值都来自来信字节，畸形邮件可以把任意文字放进去；不加约束就会绕过「不输出正文、显示名与完整主题」。
func safeLabel(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value != "" && !labelPattern.MatchString(value) {
		return "other"
	}
	return truncateRunes(value, maxLabelRunes)
}

// safeCharset 与 safeLabel 相同，但按 maxCharsetRunes 这个更短的上限截断，用于取值本就很短、
// 又更贴近来信正文与主题的位置：主题编码字的字符集、Content-Type 的 charset、传输编码与自动来信关键字。
func safeCharset(value string) string {
	return truncateRunes(safeLabel(value), maxCharsetRunes)
}

// truncateRunes 把文本截到至多 n 个字符，不会截断在 UTF-8 字符中间。
func truncateRunes(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n])
}

// ErrInvalidTag 表示通知的主题标签不是 SubjectTag 输出的形状；错误文本固定，不回显标签（新形态的标签含令牌）。
var ErrInvalidTag = errors.New("notification subject tag does not have the expected shape")

// SubjectTag 返回主题标签：tokenText 非空时为 D4 的新形态 [TC <任务 ID> <令牌>]（10 位任务 ID 与 48 位令牌时共 64 个字符），
// 为空时为旧形态 [TC <任务 ID>]，只用于 IDLE 自发邮件（它不写入 state.json）。
// 新形态的返回值含一次性令牌，调用方只能把它交给 ComposeNotification，不得写入日志、错误文本、样本或 /dev/tty。
func SubjectTag(taskID, tokenText string) string {
	if tokenText == "" {
		return "[TC " + taskID + "]"
	}
	return "[TC " + taskID + " " + tokenText + "]"
}

// SendPrompt 渲染逐封确认的文本：发件账户、收件地址、第几封、主题标签之后的文字与是否抄送机器人自己。
// 签名里没有标签参数：新形态的标签含一次性令牌，确认文本只注明「主题标签含一次性令牌，不回显」，令牌因此无从进入 /dev/tty。
func SendPrompt(bot, recipient string, index, total int, title string, ccBot bool) string {
	return fmt.Sprintf("将用 %s 向 %s 发送第 %d/%d 封合成探测邮件，主题标签之后的文字为「%s」（主题标签含一次性令牌，不回显），抄送机器人自己：%v。",
		bot, recipient, index, total, title, ccBot)
}

// ComposeNotification 渲染一封合成探测通知：multipart/alternative、Auto-Submitted: auto-generated、
// 带探测 ID 头，正文页脚含说明句与单独占一行的一次性令牌（D4：页脚只是给人看的副本）。返回的字节可直接交给 smtp.Send。
// 主题头的原始取值是 Tag，Title 非空时再接一个空格与 mime.BEncoding.Encode("utf-8", Title)（纯 ASCII 的 Title 原样保留），
// 不对整个主题调用 SetSubject：标签因此以原始 ASCII 出现在头部、从不进入 encoded-word；「Subject: 」加 64 个字符的新形态标签
// 共 73 列，go-message 按 76 列折行时只会在标签之后的空格处折行，标签本身不会被折断。
// Tag 不是 SubjectTag 输出的形状（含 CR/LF、大写、长度不对等）时返回 ErrInvalidTag，不渲染任何字节，也不回显输入。
func ComposeNotification(n Notification) ([]byte, error) {
	if !tagShapePattern.MatchString(n.Tag) {
		return nil, ErrInvalidTag
	}
	subject := n.Tag
	if n.Title != "" {
		subject += " " + mime.BEncoding.Encode("utf-8", n.Title)
	}
	var h mail.Header
	h.SetContentType("multipart/alternative", nil)
	h.SetAddressList("From", []*mail.Address{{Address: n.From}})
	h.SetAddressList("To", []*mail.Address{{Address: n.To}})
	if n.CC != "" {
		h.SetAddressList("Cc", []*mail.Address{{Address: n.CC}})
	}
	h.Set("Subject", subject)
	h.SetDate(n.Date)
	h.Set("Message-Id", n.MessageID)
	h.Set("Auto-Submitted", "auto-generated")
	h.Set(ProbeHeader, n.ProbeID)

	var buf bytes.Buffer
	writer, err := message.CreateWriter(&buf, h.Header)
	if err != nil {
		return nil, fmt.Errorf("cannot create message writer: %w", err)
	}
	for _, part := range []struct {
		contentType string
		body        string
	}{
		{"text/plain", plainBody(n.Token)},
		{"text/html", htmlBody(n.Token)},
	} {
		var partHeader message.Header
		partHeader.SetContentType(part.contentType, map[string]string{"charset": "utf-8"})
		partHeader.Set("Content-Transfer-Encoding", "quoted-printable")
		partWriter, err := writer.CreatePart(partHeader)
		if err != nil {
			return nil, fmt.Errorf("cannot create message part: %w", err)
		}
		if _, err := io.WriteString(partWriter, part.body); err != nil {
			return nil, fmt.Errorf("cannot write message part: %w", err)
		}
		if err := partWriter.Close(); err != nil {
			return nil, fmt.Errorf("cannot close message part: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("cannot close message: %w", err)
	}
	return buf.Bytes(), nil
}

// plainBody 渲染通知的纯文本正文；令牌单独占一行。
func plainBody(tokenText string) string {
	lines := []string{noticeLine, ""}
	if tokenText != "" {
		lines = append(lines, tokenLabel, tokenText, "")
	}
	return strings.Join(lines, "\n")
}

// htmlBody 渲染通知的 HTML 正文；令牌单独占一段。
func htmlBody(tokenText string) string {
	body := "<html><body><p>" + noticeLine + "</p>"
	if tokenText != "" {
		body += "<p>" + tokenLabel + "</p><p>" + tokenText + "</p>"
	}
	return body + "</body></html>\n"
}

// CopyHeaders 从一封原始邮件中读取探测 ID、Message-ID 与 X-OQ-MSGID；两个 ID 都补上尖括号。
func CopyHeaders(raw []byte) (Copy, error) {
	entity, err := parseMessage(raw)
	if err != nil {
		return Copy{}, err
	}
	h := mail.Header{Header: entity.Header}
	return Copy{
		ProbeID:   strings.TrimSpace(h.Get(ProbeHeader)),
		MessageID: bracket(headerMessageID(h)),
		OQMsgID:   bracket(strings.TrimSpace(h.Get(OQHeader))),
	}, nil
}

// JudgeIdleCheck 判定 EXISTS 是否随前一条命令的响应附带：10 秒内返回 true、耗时远小于 IDLE 与 DONE 两次往返，
// 且邮件数与 UIDNEXT 都没有变化时记为 attached；ctx 到期（说明已真正发出 IDLE）记为 not_attached；其余记为 unknown。
func JudgeIdleCheck(c IdleCheck) string {
	sameMailbox := c.Messages0 == c.Messages1 && c.UIDNext0 == c.UIDNext1
	switch {
	case c.Returned && c.NewMail && sameMailbox && c.Elapsed*2 < c.RoundTrip:
		return "attached"
	case c.DeadlineExceeded:
		return "not_attached"
	default:
		return "unknown"
	}
}

// IdleConclusion 综合预检与 FETCH 检查的判定：预检说明 UID SEARCH 是否附带 EXISTS，FETCH 检查补上 UID FETCH 的情形；
// 预检未能进行时只能记为「未知或随 UID FETCH」。fetch 为空表示没有做 FETCH 检查。
func IdleConclusion(preflight, fetch string) string {
	switch {
	case preflight == "attached":
		return "uid_search"
	case preflight == "not_attached" && fetch == "attached":
		return "uid_fetch"
	case preflight == "not_attached" && fetch == "not_attached":
		return "none"
	case preflight != "not_attached" && fetch == "attached":
		return "unknown_or_uid_fetch"
	default:
		return "unknown"
	}
}

// RepoRoot 从 dir 逐级向上查找含 go.mod 的目录，作为仓库根目录；找不到时返回错误。
func RepoRoot(dir string) (string, error) {
	for current := filepath.Clean(dir); ; {
		if info, err := os.Stat(filepath.Join(current, "go.mod")); err == nil && info.Mode().IsRegular() {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("cannot find the repository root: no go.mod above the working directory")
		}
		current = parent
	}
}

// ValidateOutputDir 校验探测输出目录并返回解析后的真实路径：必须是绝对路径；解析路径中的全部符号链接后，
// 必须是权限位恰为 0700 的目录；从真实路径逐级向上到文件系统根，每一级都不能与仓库根目录是同一个目录
// （按设备号与 inode 比较，不受文件系统是否区分大小写影响）。
// 必须先解析符号链接：只按字面路径逐级向上时，位于仓库外、指向仓库内子目录的符号链接会通过校验。
func ValidateOutputDir(dir, repoRoot string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", errors.New("output directory must be an absolute path")
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", errors.New("output directory cannot be resolved")
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", errors.New("output directory cannot be read")
	}
	if !info.IsDir() {
		return "", errors.New("output directory is not a directory")
	}
	if info.Mode().Perm() != 0o700 {
		return "", errors.New("output directory permissions must be exactly 0700")
	}
	rootInfo, err := os.Stat(repoRoot)
	if err != nil {
		return "", errors.New("repository root cannot be read")
	}
	for current := real; ; {
		info, err := os.Stat(current)
		if err != nil {
			return "", errors.New("output directory has an unreadable parent directory")
		}
		if os.SameFile(info, rootInfo) {
			return "", errors.New("output directory must be outside the repository")
		}
		parent := filepath.Dir(current)
		if parent == current {
			return real, nil
		}
		current = parent
	}
}

// LoadState 读取输出目录中的 state.json；文件不存在时返回空状态。
func LoadState(dir string) (State, error) {
	data, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return State{Schema: Schema}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("cannot read probe state: %w", err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("cannot parse probe state: %w", err)
	}
	return state, nil
}

// SaveState 以 0600 原子写入 state.json：先在同一目录写临时文件，再改名覆盖，避免中断留下半个文件。
func SaveState(dir string, state State) error {
	state.Schema = Schema
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode probe state: %w", err)
	}
	file, err := os.CreateTemp(dir, "state-*.json")
	if err != nil {
		return fmt.Errorf("cannot create probe state file: %w", err)
	}
	name := file.Name()
	_, err = file.Write(append(data, '\n'))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(name, 0o600)
	}
	if err == nil {
		err = os.Rename(name, filepath.Join(dir, stateFile))
	}
	if err != nil {
		os.Remove(name)
		return fmt.Errorf("cannot write probe state: %w", err)
	}
	return nil
}

// AppendSample 以 0600 追加一行 JSON 到 samples.jsonl。
func AppendSample(dir string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("cannot encode sample: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(dir, samplesFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("cannot open sample file: %w", err)
	}
	_, err = file.Write(append(data, '\n'))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("cannot write sample: %w", err)
	}
	return nil
}
