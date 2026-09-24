// Package parser 测试引用与签名的剥离与残留检查：五类引用边界各自的正反用例（含单独一行 From:、其后不是引用的「写道：」、
// 不含页脚标记与 [TC 的 > 行）、行内逐段回复、边界的先后、签名分隔线与客户端签名、四种残留触发各自独立成立，以及页脚常量
// 与这些规则互不干扰。输入是已解码、换行已统一的文字，经 newText（NewText 解码之后的全部步骤）处理；令牌由 security/token 签发。
package parser

import (
	"errors"
	"strings"
	"testing"

	"github.com/chaoRookie/turncourier/internal/mail"
)

// quoteCase 是剥离规则表中的一行：逐行给出的输入、期望的新正文与期望的错误（nil 表示成功）。
type quoteCase struct {
	name  string
	lines []string
	want  string
	err   error
}

// noticeLines 是一段不带引用前缀的我方通知：正文、页脚标记行、固定句子与令牌副本。它若没有被剥离，残留检查必然命中。
func noticeLines(tokenText string) []string {
	return []string{
		"任务 " + fixtureTask + " 回合完成。",
		"",
		mail.FooterMarker,
		mail.FooterNotice + "；请保留主题中的 [TC …] 标签，不要改动。",
		"令牌副本（仅供核对）：",
		tokenText,
	}
}

// quoted 给每一行加上前缀 prefix，模拟客户端的 > 引用。
func quoted(prefix string, lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = strings.TrimRight(prefix+line, " ")
	}
	return out
}

// join 把若干组行依次连接成一个切片。
func join(groups ...[]string) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// runQuoteCases 逐行运行规则表：newText 的结果与期望的新正文、期望的错误都一致。
func runQuoteCases(t *testing.T, cases []quoteCase) {
	t.Helper()
	for _, tc := range cases {
		got, err := newText(strings.Join(tc.lines, "\n"))
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("%s: newText = %q, %v; want %q, %v", tc.name, got, err, tc.want, tc.err)
		}
	}
}

// TestSeparatorBoundaries 覆盖规则 1 与 2：QQ 的「原始邮件」与 Original 分隔线（长短两种、不换行空格、行首尾空白）、单独一行的
// 「原始邮件」、Outlook 的 Original Message 分隔线（不区分大小写）都是边界，其后全部丢弃；只是提到这些字样的行不是边界。
func TestSeparatorBoundaries(t *testing.T) {
	notice := noticeLines(issueText(t, fixtureTask, 0x11))
	cases := []quoteCase{
		{"qq long", join([]string{"新文字", "", "------------------ 原始邮件 ------------------"}, notice), "新文字", nil},
		{"qq short", join([]string{"新文字", "-------- 原始邮件 --------"}, notice), "新文字", nil},
		{"qq original long", join([]string{"新文字", "------------------ Original ------------------"}, notice), "新文字", nil},
		{"qq original short", join([]string{"新文字", "--- Original ---"}, notice), "新文字", nil},
		{"qq no-break spaces", join([]string{"新文字", "------------------\u00a0原始邮件\u00a0------------------"}, notice), "新文字", nil},
		{"qq no spaces", join([]string{"新文字", "---原始邮件---"}, notice), "新文字", nil},
		{"qq indented", join([]string{"新文字", "  \u3000------ 原始邮件 ------  "}, notice), "新文字", nil},
		{"lone 原始邮件", join([]string{"新文字", "原始邮件"}, notice), "新文字", nil},
		{"outlook", join([]string{"新文字", "", "-----Original Message-----"}, notice), "新文字", nil},
		{"outlook lower case", join([]string{"新文字", "----- original message -----"}, notice), "新文字", nil},
		{"separator first", join([]string{"------------------ 原始邮件 ------------------"}, notice), "", ErrEmpty},
		{"mentions 原始邮件", []string{"原始邮件已归档，请查看。", "谢谢"}, "原始邮件已归档，请查看。\n谢谢", nil},
		{"dashes on one side", []string{"---原始邮件", "继续"}, "---原始邮件\n继续", nil},
		{"original lower case is not qq", []string{"新文字", "--- original ---", "继续"}, "新文字\n--- original ---\n继续", nil},
		{"mentions original message", []string{"Original Message 已收到", "继续"}, "Original Message 已收到\n继续", nil},
		{"outlook without dashes", []string{"新文字", "Original Message", "继续"}, "新文字\nOriginal Message\n继续", nil},
	}
	runQuoteCases(t, cases)
}

// TestAttributionBoundaries 覆盖规则 3：以「写道：」「写道:」或 wrote:（不区分大小写）结尾、且其后第一个非空行以 > 开头的行是边界；
// 其后不是引用、没有下一行或本身被引用前缀包着的，都不是规则 3 的边界（少了冒号的「He wrote」留在新正文里，其后含页脚标记的
// 引用块由规则 4 剥离）。引用块之后又出现作答为 ErrUncertain，引用块之后只有签名（「-- 」、靠近末尾的「--」、
// 作为最后一行的客户端签名）照常剥离。
func TestAttributionBoundaries(t *testing.T) {
	notice := quoted("> ", noticeLines(issueText(t, fixtureTask, 0x12)))
	longTail := []string{"--", "1", "2", "3", "4", "5", "6", "7"}
	cases := []quoteCase{
		{"full-width colon", join([]string{"新文字", "", "在 2026年9月24日，TurnCourier <bot@example.invalid> 写道：", ""}, notice), "新文字", nil},
		{"half-width colon", join([]string{"新文字", "TurnCourier 于2026年9月24日写道:"}, notice), "新文字", nil},
		{"wrote", join([]string{"新文字", "", "On Wed, Sep 24, 2026 at 3:00 PM TurnCourier <bot@example.invalid> wrote:", "", ""}, notice), "新文字", nil},
		{"WROTE upper case", join([]string{"新文字", "TURNCOURIER WROTE:  "}, notice), "新文字", nil},
		{"quote without notice", []string{"新文字", "X wrote:", "> 任意引用"}, "新文字", nil},
		{"text after 写道", []string{"我看了一下，日志里写道：", "连接超时", "请检查网络。"}, "我看了一下，日志里写道：\n连接超时\n请检查网络。", nil},
		{"写道 at the end", []string{"他在最后写道："}, "他在最后写道：", nil},
		{"blank lines then text", []string{"他写道：", "", "", "没有引用"}, "他写道：\n\n\n没有引用", nil},
		{"wrote without colon", join([]string{"新文字", "He wrote"}, notice), "新文字\nHe wrote", nil},
		{"quoted attribution", []string{"新文字", "> X wrote:", "> 引用"}, "", ErrUncertain},
		{"inline answer", join([]string{"新文字", "X wrote:", "> 第一问", "第一答"}, notice), "", ErrUncertain},
		{"inline after blank", []string{"新文字", "X wrote:", "", "> 第一问", "", "第一答", "", "> 第二问"}, "", ErrUncertain},
		{"dash signature after quote", join([]string{"新文字", "X wrote:"}, notice, []string{"", "-- ", "张三", "平台组"}), "新文字", nil},
		{"short dashes after quote", join([]string{"新文字", "X wrote:"}, notice, []string{"--", "张三", "电话", "地址"}), "新文字", nil},
		{"long dashes after quote", join([]string{"新文字", "X wrote:"}, notice, longTail), "", ErrUncertain},
		{"client signature after quote", join([]string{"新文字", "X wrote:"}, notice, []string{"", " Sent from my iPhone ", ""}), "新文字", nil},
		{"client signature not last", join([]string{"新文字", "X wrote:"}, notice, []string{"发自我的iPhone", "更多作答"}), "", ErrUncertain},
	}
	runQuoteCases(t, cases)
}

// TestQuotePrefixBoundaries 覆盖规则 4：没有引用头的 > 行，只有当这一段连续的 > 行去掉前缀后含页脚标记行（忽略空白、折行）
// 或 [TC（ASCII 不区分大小写）时才是边界；不含这些的 > 行（用户标出的命令或引文）为 ErrUncertain；
// 标记只出现在空行之后的另一段时同样不算；引用块之后又出现作答为 ErrUncertain，只有签名时照常剥离。
func TestQuotePrefixBoundaries(t *testing.T) {
	notice := noticeLines(issueText(t, fixtureTask, 0x13))
	cases := []quoteCase{
		{"marker", join([]string{"新文字", ""}, quoted("> ", notice)), "新文字", nil},
		{"tag opening", []string{"新文字", "> Re: [TC abc] 标题", "> 正文"}, "新文字", nil},
		{"lower-case tag opening", []string{"新文字", "> 主题：[tc abc]"}, "新文字", nil},
		{"nested prefix", join([]string{"新文字"}, quoted(">> ", notice)), "新文字", nil},
		{"spaced nested prefix", join([]string{"新文字"}, quoted("> > ", notice)), "新文字", nil},
		{"no space after >", join([]string{"新文字"}, quoted(">", notice)), "新文字", nil},
		{"indented", join([]string{"新文字"}, quoted("  > ", notice)), "新文字", nil},
		{"marker wrapped", []string{"新文字", "> ---- Turn", "> Courier ----"}, "新文字", nil},
		{"marker respaced", []string{"新文字", ">   ----TurnCourier----"}, "新文字", nil},
		{"quote at top", quoted("> ", notice), "", ErrEmpty},
		{"user command", []string{"请运行：", "", "> make test"}, "", ErrUncertain},
		{"user citation", []string{"文档里说", "> 先备份再升级", "所以先备份。"}, "", ErrUncertain},
		{"marker in a later run", join([]string{"新文字", "> 第一段", ""}, quoted("> ", notice)), "", ErrUncertain},
		{"answer after block", join([]string{"新文字"}, quoted("> ", notice), []string{"作答"}), "", ErrUncertain},
		{"answer after blank", join([]string{"新文字"}, quoted("> ", notice), []string{"", "", "作答"}), "", ErrUncertain},
		{"signature after block", join([]string{"新文字"}, quoted("> ", notice), []string{"", "-- ", "签名"}), "新文字", nil},
		{"client signature after block", join([]string{"新文字"}, quoted("> ", notice), []string{"", "发自我的iPhone"}), "新文字", nil},
		{"full-width gt", []string{"请运行：", "＞ make test"}, "请运行：\n＞ make test", nil},
	}
	runQuoteCases(t, cases)
}

// TestHeaderBlockBoundaries 覆盖规则 5：以「发件人」或 From 加冒号（半角或全角，冒号前可有空白）开头、且其后 3 行内有
// 发送时间、日期、收件人、主题、Sent、Date、To、Subject 之一加冒号开头的行，是边界；单独一行 From:、第 4 行才出现的头、
// 只是以这些词开头的文字都不是边界。
func TestHeaderBlockBoundaries(t *testing.T) {
	notice := noticeLines(issueText(t, fixtureTask, 0x14))
	cases := []quoteCase{
		{"foxmail", join([]string{"新文字", "", "发件人：TurnCourier", "发送时间：2026-09-24 15:00", "收件人：用户", "主题：回合完成", ""}, notice), "新文字", nil},
		{"outlook style", join([]string{"新文字", "From: TurnCourier <bot@example.invalid>", "Sent: Wednesday, September 24, 2026 3:00 PM"}, notice), "新文字", nil},
		{"third line", join([]string{"新文字", "From: TurnCourier", "Cc: a", "Reply-To: b", "Subject: 回合完成"}, notice), "新文字", nil},
		{"spaced colon", join([]string{"新文字", "  发件人 ：TurnCourier", "日期 ： 2026-09-24"}, notice), "新文字", nil},
		{"date", join([]string{"新文字", "From: X", "Date: Wed"}, notice), "新文字", nil},
		{"to", join([]string{"新文字", "发件人: X", "To: user"}, notice), "新文字", nil},
		{"收件人", join([]string{"新文字", "发件人: X", "收件人: 用户"}, notice), "新文字", nil},
		{"lone from", []string{"From: 我自己", "请继续执行。"}, "From: 我自己\n请继续执行。", nil},
		{"fourth line", []string{"新文字", "From: X", "a", "b", "c", "Subject: s"}, "新文字\nFrom: X\na\nb\nc\nSubject: s", nil},
		{"fromage", []string{"Fromage: 奶酪", "Date: 周三"}, "Fromage: 奶酪\nDate: 周三", nil},
		{"today", []string{"From: X", "Today: 晴"}, "From: X\nToday: 晴", nil},
		{"header first", []string{"主题: 我想换个话题", "发件人: 我"}, "主题: 我想换个话题\n发件人: 我", nil},
	}
	runQuoteCases(t, cases)
}

// TestBoundaryOrder 确认自上而下取第一条边界：先出现的分隔线使其后的 > 行一并丢弃，先出现的无标记 > 行则直接判为不确定。
func TestBoundaryOrder(t *testing.T) {
	notice := noticeLines(issueText(t, fixtureTask, 0x15))
	cases := []quoteCase{
		{"separator before command", join([]string{"新文字", "------ 原始邮件 ------", "> make test"}, notice), "新文字", nil},
		{"command before separator", join([]string{"请运行：", "> make test", "------ 原始邮件 ------"}, notice), "", ErrUncertain},
		{"header block before attribution", join([]string{"新文字", "From: X", "To: Y", "X wrote:", "> q"}, notice), "新文字", nil},
	}
	runQuoteCases(t, cases)
}

// TestSignatures 覆盖签名剥离：最后一个恰为「-- 」的行及其后丢弃；单独的「--」只在其后至多 6 行（不计结尾空行）就到正文末尾时丢弃；
// 最后一个非空行等于已知的客户端签名（去掉首尾空白后比较）时只丢弃这一行；形状相近的行与不在末尾的客户端签名都保留。
func TestSignatures(t *testing.T) {
	six := []string{"1", "2", "3", "4", "5", "6"}
	cases := []quoteCase{
		{"dash space", []string{"正文", "-- ", "签名"}, "正文", nil},
		{"last of two", []string{"甲", "-- ", "乙", "-- ", "签名"}, "甲\n-- \n乙", nil},
		{"lone dashes six lines", join([]string{"正文", "--"}, six), "正文", nil},
		{"lone dashes seven lines", join([]string{"正文", "--"}, six, []string{"7"}), "正文\n--\n1\n2\n3\n4\n5\n6\n7", nil},
		{"lone dashes trailing blanks", join([]string{"正文", "--"}, six, []string{"", " ", ""}), "正文", nil},
		{"lone dashes last line", []string{"正文", "--"}, "正文", nil},
		{"two spaces", []string{"正文", "--  ", "签名"}, "正文\n--  \n签名", nil},
		{"leading space", []string{"正文", " -- ", "签名"}, "正文\n -- \n签名", nil},
		{"three dashes", []string{"正文", "---", "签名"}, "正文\n---\n签名", nil},
		{"client signature", []string{"正文", "", "  发自我的iPhone  ", ""}, "正文", nil},
		{"only the last one", []string{"正文", "发自我的iPhone", "Sent from my iPhone"}, "正文\n发自我的iPhone", nil},
		{"not last", []string{"发自我的iPhone", "正文"}, "发自我的iPhone\n正文", nil},
		{"unknown client", []string{"正文", "发自我的 iPhone 15"}, "正文\n发自我的 iPhone 15", nil},
		{"dash then client", []string{"正文", "-- ", "张三", "发自我的iPhone"}, "正文", nil},
		{"client then short dashes", []string{"正文", "发自我的iPad", "--", "张三"}, "正文", nil},
	}
	for _, signature := range clientSignatures {
		cases = append(cases, quoteCase{"client " + signature, []string{"正文", signature}, "正文", nil})
	}
	runQuoteCases(t, cases)
}

// TestResidueTriggers 覆盖残留检查的四种触发，每个用例只满足其中一种：令牌形状的串（恰好 48 个字母表字符、不区分大小写、
// 前后不是字母表字符）、[TC（ASCII 不区分大小写）、页脚标记行与页脚固定句子（都忽略空白与折行）；形状相近而不满足的都不触发。
// 经 newText 时，触发任何一种都返回 ErrUncertain。
func TestResidueTriggers(t *testing.T) {
	text := issueText(t, fixtureTask, 0x16)
	checks := map[string]func(string) bool{"run": hasAlphabetRun, "tag": hasTagOpening, "marker": hasFooterMarker, "notice": hasFooterNotice}
	cases := []struct {
		name, input, trigger string
	}{
		{"token", "令牌是 " + text + " 。", "run"},
		{"upper-case token", strings.ToUpper(text), "run"},
		{"mixed-case token", strings.ToUpper(text[:24]) + text[24:], "run"},
		{"bounded by i and u", "i" + text + "u", "run"},
		{"bounded by punctuation", "（" + text + "）", "run"},
		{"47 characters", "前" + text[:47] + "后", ""},
		{"49 characters", text + "a", ""},
		{"split by l", text[:20] + "l" + text[20:], ""},
		{"tag", "[TC " + fixtureTask + "]", "tag"},
		{"lower-case tag", "见 [tc 标签", "tag"},
		{"mixed-case tag", "[tC", "tag"},
		{"tag in word", "[TCP] 连接", "tag"},
		{"spaced tag", "[ TC", ""},
		{"marker", mail.FooterMarker, "marker"},
		{"marker respaced", "----  Turn Courier\n  ----", "marker"},
		{"marker in text", "页脚是" + mail.FooterMarker + "吗", "marker"},
		{"marker lookalike", "---- TurnCourier", ""},
		{"notice", mail.FooterNotice + "。", "notice"},
		{"notice wrapped", "直接回复本邮件即可\n把新消息交给该任务", "notice"},
		{"notice partial", "直接回复本邮件即可", ""},
		{"plain reply", "继续执行，完成后告诉我。", ""},
	}
	for _, tc := range cases {
		for name, check := range checks {
			if got := check(tc.input); got != (name == tc.trigger) {
				t.Errorf("%s: %s check = %v", tc.name, name, got)
			}
		}
		if got := hasResidue(tc.input); got != (tc.trigger != "") {
			t.Errorf("%s: hasResidue = %v", tc.name, got)
		}
		got, err := newText(tc.input)
		if tc.trigger != "" && (got != "" || !errors.Is(err, ErrUncertain)) {
			t.Errorf("%s: newText = %q, %v; want ErrUncertain", tc.name, got, err)
		}
		if tc.trigger == "" && err != nil {
			t.Errorf("%s: newText error %v", tc.name, err)
		}
	}
}

// TestGatewayConstants 钉住邮件契约中的常量，并确认两条页脚常量各自只触发对应的残留检查（四种触发因此互相独立）、
// 本身不是引用边界或签名，且不会被 > 行的页脚识别以外的规则误用。
func TestGatewayConstants(t *testing.T) {
	if mail.IDHeader != "X-TurnCourier-ID" {
		t.Errorf("IDHeader = %q", mail.IDHeader)
	}
	for name, value := range map[string]string{"FooterMarker": mail.FooterMarker, "FooterNotice": mail.FooterNotice} {
		if value == "" || strings.ContainsAny(value, "\r\n") || hasTagOpening(value) || hasAlphabetRun(value) {
			t.Errorf("%s must be one non-empty line without [TC or a token-shaped run", name)
		}
		got, err := newText("正文\n" + value)
		if got != "" || !errors.Is(err, ErrUncertain) {
			t.Errorf("%s left in the new text gives %q, %v; want ErrUncertain", name, got, err)
		}
		if kept, err := stripQuotes("正文\n" + value + "\n后文"); err != nil || kept != "正文\n"+value+"\n后文" {
			t.Errorf("%s is treated as a quote boundary", name)
		}
		if stripSignature("正文\n"+value) != "正文\n"+value {
			t.Errorf("%s is treated as a signature", name)
		}
	}
	for _, r := range mail.FooterMarker {
		if r < 0x20 || r > 0x7e {
			t.Errorf("FooterMarker has a non-ASCII or control character %U", r)
		}
	}
	if hasFooterNotice(mail.FooterMarker) || hasFooterMarker(mail.FooterNotice) {
		t.Error("the two footer constants trigger each other's check")
	}
}

// TestTextHelpers 钉住几个文字辅助函数：lowerASCII 只折叠 A–Z（开尔文符号等非 ASCII 字符原样保留、字节长度不变）；
// removeSpace 删去全部 Unicode 空白；unquote 去掉行首空白与任意层 > 前缀；normalizeNewlines 统一 CRLF 与单独的 CR。
func TestTextHelpers(t *testing.T) {
	if got := lowerASCII("WROTE: \u212a Ä"); got != "wrote: \u212a Ä" {
		t.Errorf("lowerASCII = %q", got)
	}
	if got := removeSpace(" a\tb\u3000c\u00a0d\ne "); got != "abcde" {
		t.Errorf("removeSpace = %q", got)
	}
	for in, want := range map[string]string{"> a": "a", ">>a": "a", " > > a b": "a b", "a > b": "a > b", ">": ""} {
		if got := unquote(in); got != want {
			t.Errorf("unquote(%q) = %q, want %q", in, got, want)
		}
	}
	if got := normalizeNewlines("a\r\nb\rc\n\rd"); got != "a\nb\nc\n\nd" {
		t.Errorf("normalizeNewlines = %q", got)
	}
}
