// Package renderer 测试通知的 MIME 组装：主题头的原始字节（标签以原始 ASCII 位于首行、不进 encoded-word，标题拆成多个 encoded-word
// 时同样成立）、各头部与两个部件、页脚的内容与顺序（标记行与回复说明取自 internal/mail 的常量）、HTML 转义、标题的组成与 60 个字符的上限、
// 令牌与机密的金丝雀、纯状态模式、信封与 msg-id 的形状校验、标签与选项的校验、256 KiB 的上限，以及错误文本不回显内容。
// 渲染结果一律用标准库（net/mail、mime、mime/multipart、mime/quotedprintable）独立解析，不经过被测代码所用的 go-message。
package renderer

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"net/textproto"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/chaoRookie/turncourier/internal/mail"
	"github.com/chaoRookie/turncourier/internal/security/token"
)

const (
	// testFrom 与 testTo 是测试信封的地址（保留域名）。
	testFrom = "bot@example.invalid"
	testTo   = "me@example.invalid"
	// testMessageID 是测试信封的我方 Message-ID。
	testMessageID = "<n20260924.0001@bot.example.invalid>"
	// noticeLine 是页脚中的回复说明：契约规定由 mail.FooterNotice 加后半句拼成。
	noticeLine = mail.FooterNotice + "；请保留主题中的 [TC …] 标签，不要改动。"
	// omissionPrefix 是页脚「有省略」一行的开头，其后是任务 ID 与「）。」。
	omissionPrefix = "部分内容已省略，完整输出留在本机（任务 "
	// htmlOpen 与 htmlClose 是 HTML 部件包住转义后纯文本的容器，规格规定用 white-space: pre-wrap。
	htmlOpen  = `<html><body><div style="white-space: pre-wrap">`
	htmlClose = "</div></body></html>\n"
)

// testEnvelope 返回一份合法的信封：地址用保留域名，日期固定为 UTC。
func testEnvelope() Envelope {
	return Envelope{From: testFrom, To: testTo, MessageID: testMessageID, Date: time.Date(2026, 9, 24, 7, 0, 0, 0, time.UTC)}
}

// filteredOptions 返回过滤模式与测试用的 Scrubber。
func filteredOptions() Options {
	return Options{Mode: ModeFiltered, Scrub: testScrubber()}
}

// renderedPart 是渲染结果中的一个部件：部件头与 quoted-printable 解码后、换行统一为 LF 的文字。
type renderedPart struct {
	header textproto.MIMEHeader
	text   string
}

// rendered 是用标准库独立解析的一封通知：头部、Subject 字段的全部物理行、解码后的主题与各部件。
type rendered struct {
	header       netmail.Header
	subjectLines []string
	subject      string
	parts        []renderedPart
}

// plain 返回纯文本部件的文字（部件数不对时由 parseRendered 先报错）。
func (r rendered) plain() string { return r.parts[0].text }

// htmlText 返回 HTML 部件的文字。
func (r rendered) htmlText() string { return r.parts[1].text }

// parseRendered 用标准库解析渲染结果：每一行以 CRLF 结束且不超过 998 字节，恰有一个 Subject 字段，顶层为 multipart/alternative，
// 部件按原样读取（NextRawPart 不替调用方解码）后再做 quoted-printable 解码。
func parseRendered(t *testing.T, raw []byte) rendered {
	t.Helper()
	text := string(raw)
	if strings.Count(text, "\n") != strings.Count(text, "\r\n") || strings.Contains(strings.ReplaceAll(text, "\r\n", ""), "\r") {
		t.Fatalf("渲染结果中有裸换行，SMTP 要求每一行都以 CRLF 结束")
	}
	for i, line := range strings.Split(text, "\r\n") {
		if len(line) > 998 {
			t.Fatalf("第 %d 行长 %d 字节，超过 998", i+1, len(line))
		}
	}
	head, _, ok := strings.Cut(text, "\r\n\r\n")
	if !ok {
		t.Fatalf("渲染结果中没有头部与正文之间的空行")
	}
	var r rendered
	fields, inSubject := 0, false
	for _, line := range strings.Split(head, "\r\n") {
		switch {
		case strings.HasPrefix(line, "Subject: "):
			fields, inSubject = fields+1, true
			r.subjectLines = append(r.subjectLines, line)
		case inSubject && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")):
			r.subjectLines = append(r.subjectLines, line)
		default:
			inSubject = false
		}
	}
	if fields != 1 {
		t.Fatalf("渲染结果中有 %d 个 Subject 字段，期望 1 个", fields)
	}
	msg, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("标准库无法解析渲染结果：%v", err)
	}
	r.header = msg.Header
	if r.subject, err = new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject")); err != nil {
		t.Fatalf("标准库无法解码主题：%v", err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/alternative" || params["boundary"] == "" {
		t.Fatalf("顶层 Content-Type 为 %q（错误 %v），期望带边界的 multipart/alternative", msg.Header.Get("Content-Type"), err)
	}
	reader := multipart.NewReader(msg.Body, params["boundary"])
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("读取部件失败：%v", err)
		}
		decoded, err := io.ReadAll(quotedprintable.NewReader(part))
		if err != nil {
			t.Fatalf("quoted-printable 解码失败：%v", err)
		}
		r.parts = append(r.parts, renderedPart{header: part.Header, text: strings.ReplaceAll(string(decoded), "\r\n", "\n")})
	}
	if len(r.parts) != 2 {
		t.Fatalf("有 %d 个部件，期望纯文本与 HTML 两个", len(r.parts))
	}
	return r
}

// mustRender 渲染一封通知并用标准库解析，失败即终止测试。
func mustRender(t *testing.T, env Envelope, tag Tag, c Content, opts Options) ([]byte, rendered) {
	t.Helper()
	raw, err := Render(env, tag, c, opts)
	if err != nil {
		t.Fatalf("Render 返回错误：%v", err)
	}
	return raw, parseRendered(t, raw)
}

// footer 按契约的内容与顺序拼出页脚各行：标记行、任务 ID 与事件、回复说明、令牌副本（先说明、再单独一行令牌）、
// 有省略时的说明、加通讯录的建议与不要开启假期自动回复的提醒。
func footer(taskID, eventName, tokenText string, omitted bool) []string {
	lines := []string{mail.FooterMarker, "任务 " + taskID + " · " + eventName, noticeLine, "令牌副本（仅供核对）：", tokenText}
	if omitted {
		lines = append(lines, omissionPrefix+taskID+"）。")
	}
	return append(lines, "请把机器人地址加入通讯录，以免通知落进垃圾箱。", "请不要在接收本通知的邮箱中开启假期自动回复。")
}

// expectedPlain 拼出期望的纯文本：正文（非空时）、一个空行与页脚，以换行结尾。
func expectedPlain(body string, footerLines []string) string {
	var lines []string
	if body != "" {
		lines = append(lines, body, "")
	}
	return strings.Join(append(lines, footerLines...), "\n") + "\n"
}

// TestRenderSubjectRawBytes 断言主题的编码结构：Subject 字段的第一行恰为「Subject: 」加标签（标签以原始 ASCII 出现，标签之后才折行），
// 解码后等于「标签 + 空格 + 标题」；标题长到拆成多个 encoded-word 时同样成立；标签在头部原文中恰好出现一次，从不进入 encoded-word；
// 解码后的主题中标签恰好一次，token.ParseSubject 能从中取回同一个标签（跨包的完整往返在 tests/e2e）。
func TestRenderSubjectRawBytes(t *testing.T) {
	tag := issueTag(t, testTask, 0x51)
	for _, tc := range []struct {
		name     string
		title    string
		want     string
		minWords int
	}{
		{"短标题", "部署脚本已更新", "回合完成：部署脚本已更新", 1},
		{"长标题拆成多个 encoded-word", strings.Repeat("标题很长", 20), "回合完成：" + strings.Repeat("标题很长", 13) + "标题…", 3},
	} {
		c := validContent()
		c.Title = tc.title
		raw, r := mustRender(t, testEnvelope(), tag, c, filteredOptions())
		if first := r.subjectLines[0]; first != "Subject: "+tag.Reveal() {
			t.Errorf("%s：主题字段首行不是「Subject: 」加完整的标签（首行 %d 个字符）", tc.name, len(first))
		}
		if r.subject != tag.Reveal()+" "+tc.want {
			t.Errorf("%s：解码后的主题为 %q，期望标签加空格加 %q", tc.name, strings.TrimPrefix(r.subject, tag.Reveal()), tc.want)
		}
		field := strings.Join(r.subjectLines, "\r\n")
		if words := strings.Count(field, "=?utf-8?b?"); words < tc.minWords {
			t.Errorf("%s：主题字段中有 %d 个 encoded-word，期望至少 %d 个", tc.name, words, tc.minWords)
		}
		head, _, _ := strings.Cut(string(raw), "\r\n\r\n")
		if strings.Count(head, tag.Reveal()) != 1 || strings.Count(r.subject, tag.Reveal()) != 1 {
			t.Errorf("%s：标签在头部原文或解码后的主题中不是恰好一次", tc.name)
		}
		parsed, err := token.ParseSubject(r.subject)
		if err != nil || parsed.TaskID() != testTask || parsed.RevealToken() != tag.RevealToken() {
			t.Errorf("%s：token.ParseSubject 取不回同一个标签（错误 %v）", tc.name, err)
		}
	}
}

// TestRenderHeaders 断言头部：From、To、Date、Message-Id、X-TurnCourier-ID（头名取 mail.IDHeader，取值等于 Message-ID）、
// MIME-Version、Auto-Submitted；References 为空时没有线程头，非空时 References 按顺序列出、In-Reply-To 取最后一个；
// 两个部件依次是 text/plain 与 text/html，都是 utf-8 与 quoted-printable。
func TestRenderHeaders(t *testing.T) {
	tag := issueTag(t, testTask, 0x52)
	_, r := mustRender(t, testEnvelope(), tag, validContent(), filteredOptions())
	for name, want := range map[string]string{
		"From": "<" + testFrom + ">", "To": "<" + testTo + ">", "Date": "Thu, 24 Sep 2026 07:00:00 +0000",
		"Message-Id": testMessageID, mail.IDHeader: testMessageID, "Mime-Version": "1.0", "Auto-Submitted": "auto-generated",
	} {
		if got := r.header.Get(name); got != want {
			t.Errorf("%s = %q，期望 %q", name, got, want)
		}
	}
	for _, name := range []string{"In-Reply-To", "References"} {
		if _, ok := r.header[name]; ok {
			t.Errorf("References 为空时不应有 %s", name)
		}
	}
	for i, want := range []string{"text/plain", "text/html"} {
		h := r.parts[i].header
		mediaType, params, err := mime.ParseMediaType(h.Get("Content-Type"))
		if err != nil || mediaType != want || strings.ToLower(params["charset"]) != "utf-8" {
			t.Errorf("第 %d 个部件的 Content-Type 为 %q，期望 %s; charset=utf-8", i+1, h.Get("Content-Type"), want)
		}
		if got := h.Get("Content-Transfer-Encoding"); got != "quoted-printable" {
			t.Errorf("第 %d 个部件的传输编码为 %q，期望 quoted-printable", i+1, got)
		}
	}

	env := testEnvelope()
	env.References = []string{"<first@qq.example.invalid>", "<second@qq.example.invalid>", "<third@qq.example.invalid>"}
	_, r = mustRender(t, env, tag, validContent(), filteredOptions())
	if got := r.header.Get("In-Reply-To"); got != "<third@qq.example.invalid>" {
		t.Errorf("In-Reply-To = %q，期望最后一个引用", got)
	}
	if got := strings.Fields(r.header.Get("References")); strings.Join(got, " ") != strings.Join(env.References, " ") {
		t.Errorf("References = %q，期望按顺序列出 %q", got, env.References)
	}
}

// TestRenderFooter 断言过滤模式下没有省略时的纯文本与 HTML 逐字节等于期望：正文、空行与页脚；页脚第一行单独就是 mail.FooterMarker，
// 回复说明是 mail.FooterNotice 起头的整句；HTML 是同一段纯文本经 html.EscapeString 转义后放进 pre-wrap 容器，
// 同样含转义后的回复说明；没有省略时不写「部分内容已省略」。
func TestRenderFooter(t *testing.T) {
	tag := issueTag(t, testTask, 0x53)
	c := validContent()
	_, r := mustRender(t, testEnvelope(), tag, c, filteredOptions())
	want := expectedPlain(c.Body, footer(testTask, "回合完成", tag.RevealToken(), false))
	if r.plain() != want {
		t.Errorf("纯文本为\n%s\n期望\n%s", r.plain(), want)
	}
	if r.htmlText() != htmlOpen+html.EscapeString(want)+htmlClose {
		t.Errorf("HTML 部件为\n%s", r.htmlText())
	}
	lines := strings.Split(r.plain(), "\n")
	marker := strings.Count(c.Body, "\n") + 2
	if lines[marker-1] != "" || lines[marker] != mail.FooterMarker {
		t.Errorf("页脚的第一行不是单独的 mail.FooterMarker：%q", lines[marker])
	}
	if !strings.Contains(r.plain(), "\n"+mail.FooterNotice+"；请保留主题中的 [TC …] 标签，不要改动。\n") {
		t.Error("纯文本页脚中没有以 mail.FooterNotice 起头的整句回复说明")
	}
	if !strings.Contains(r.htmlText(), html.EscapeString(mail.FooterNotice+"；请保留主题中的 [TC …] 标签，不要改动。")) {
		t.Error("HTML 页脚中没有转义后的回复说明")
	}
	if strings.Contains(r.plain(), omissionPrefix) {
		t.Error("没有省略时不应写「部分内容已省略」")
	}
	// 正文为空时纯文本只有页脚。
	c.Body = "\n\n"
	_, r = mustRender(t, testEnvelope(), tag, c, filteredOptions())
	if want := expectedPlain("", footer(testTask, "回合完成", tag.RevealToken(), false)); r.plain() != want {
		t.Errorf("正文为空时纯文本为\n%s", r.plain())
	}
}

// TestRenderOmissionNote 断言正文或标题有内容被过滤、或标题被截断时，页脚注明「部分内容已省略，完整输出留在本机（任务 <ID>）。」，
// 过滤后的正文进入纯文本，代码块按行数替换。
func TestRenderOmissionNote(t *testing.T) {
	tag := issueTag(t, testTask, 0x54)
	for _, tc := range []struct {
		name, title, body, wantBody string
	}{
		{"正文中的路径", "标题", "日志在 /var/log/app/x.log 里", "日志在 [路径已省略] 里"},
		{"正文中的代码块", "标题", "输出：\n```\na\nb\n```\n完", "输出：\n[代码块已省略：2 行]\n完"},
		{"标题中的路径", "写入 /srv/data/out", "正文", "正文"},
		{"标题被截断", strings.Repeat("长", 80), "正文", "正文"},
	} {
		c := validContent()
		c.Title, c.Body = tc.title, tc.body
		_, r := mustRender(t, testEnvelope(), tag, c, filteredOptions())
		if want := expectedPlain(tc.wantBody, footer(testTask, "回合完成", tag.RevealToken(), true)); r.plain() != want {
			t.Errorf("%s：纯文本为\n%s\n期望\n%s", tc.name, r.plain(), want)
		}
	}
}

// TestRenderHTMLEscaping 断言 HTML 部件不接受来自内容的任何标签：正文中的 <b>、<script>、<img> 与 &、引号都被转义，
// HTML 部件中除了容器之外没有任何标签，也没有外链与跟踪。
func TestRenderHTMLEscaping(t *testing.T) {
	tag := issueTag(t, testTask, 0x55)
	c := validContent()
	c.Body = `<b>粗体</b> & "引号" '单引号' <script>alert(1)</script> <img src=x onerror=y> <a href=x>链接</a>`
	_, r := mustRender(t, testEnvelope(), tag, c, filteredOptions())
	if !strings.Contains(r.htmlText(), `&lt;b&gt;粗体&lt;/b&gt; &amp; &#34;引号&#34; &#39;单引号&#39; &lt;script&gt;`) {
		t.Errorf("HTML 部件没有转义正文：%s", r.htmlText())
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(r.htmlText(), htmlOpen), htmlClose)
	if strings.ContainsAny(inner, "<>") {
		t.Errorf("HTML 容器之内出现了标签字符")
	}
	if !strings.Contains(r.plain(), c.Body) {
		t.Error("纯文本中的正文不应被转义")
	}
}

// TestRenderTitle 断言主题中的标题：过滤模式为事件中文名加「：」加过滤后的标题（四个事件各一），标题为空或只有空白时只有事件名，
// 连续空白（含换行与制表）合并为一个空格，标题至多 60 个字符，超出以「…」截断，恰好 60 个时不截断。
func TestRenderTitle(t *testing.T) {
	tag := issueTag(t, testTask, 0x56)
	names := map[string]string{EventTurnCompleted: "回合完成", EventWaitingInput: "等待输入", EventWaitingApproval: "等待审批", EventFailed: "任务失败"}
	for event, name := range names {
		c := validContent()
		c.Event = event
		_, r := mustRender(t, testEnvelope(), tag, c, filteredOptions())
		if r.subject != tag.Reveal()+" "+name+"：部署脚本已更新" {
			t.Errorf("%s：主题为 %q", event, strings.TrimPrefix(r.subject, tag.Reveal()))
		}
		if !strings.Contains(r.plain(), "\n任务 "+testTask+" · "+name+"\n") {
			t.Errorf("%s：页脚中没有任务 ID 与事件名", event)
		}
	}
	for _, tc := range []struct{ name, title, want string }{
		{"空标题", "", "回合完成"},
		{"只有空白", " \n\t\u3000", "回合完成"},
		{"合并空白", " 第一行\n第二行\t\t尾 ", "回合完成：第一行 第二行 尾"},
		{"恰好 60 个字符", strings.Repeat("字", 55), "回合完成：" + strings.Repeat("字", 55)},
		{"超过 60 个字符", strings.Repeat("字", 56), "回合完成：" + strings.Repeat("字", 54) + "…"},
		{"过滤后的标题", "写入 /srv/data/out", "回合完成：写入 [路径已省略]"},
	} {
		c := validContent()
		c.Title = tc.title
		_, r := mustRender(t, testEnvelope(), tag, c, filteredOptions())
		if got := strings.TrimPrefix(r.subject, tag.Reveal()+" "); got != tc.want {
			t.Errorf("%s：标题为 %q，期望 %q", tc.name, got, tc.want)
		}
		if n := utf8.RuneCountInString(strings.TrimPrefix(r.subject, tag.Reveal()+" ")); n > 60 {
			t.Errorf("%s：标题有 %d 个字符", tc.name, n)
		}
	}
}

// canaryContent 返回标题与正文都带金丝雀的内容：三份进程机密、另一枚令牌、路径、带 userinfo 的链接与可辨认的中英文字样。
func canaryContent(t *testing.T) (Content, []string) {
	authCode, signingKey, bodyKey := testSecrets()
	other := issueText(t, testTask, 0x66)
	c := validContent()
	c.Title = "金丝雀标题CANARYTITLE " + authCode + " " + other
	c.Body = "金丝雀正文CANARYBODY\n" + signingKey + "\n" + bodyKey + "\n" + other + "\nhttps://canary:pw@example.com/p?q=CANARYQUERY\n/Users/canaryuser/x"
	return c, []string{"金丝雀", "CANARY", "canary", authCode, signingKey, bodyKey, other, strings.ToUpper(other)}
}

// TestRenderCanaries 是过滤模式的金丝雀：解码后的 Subject、text/plain 与 text/html 中令牌文本各恰好出现一次
// （原始字节中 quoted-printable 的软换行可能把它拆开，所以按解码后计数）；Scrubber 抹去的机密与内容中另一枚令牌
// 不出现在原始字节与任何解码后的部件中。
func TestRenderCanaries(t *testing.T) {
	tag := issueTag(t, testTask, 0x57)
	c, canaries := canaryContent(t)
	raw, r := mustRender(t, testEnvelope(), tag, c, filteredOptions())
	for label, text := range map[string]string{"主题": r.subject, "纯文本": r.plain(), "HTML": r.htmlText()} {
		if n := strings.Count(text, tag.RevealToken()); n != 1 {
			t.Errorf("%s中令牌文本出现 %d 次，期望恰好 1 次", label, n)
		}
	}
	for _, secret := range canaries[3:] {
		for label, text := range map[string]string{"原始字节": string(raw), "主题": r.subject, "纯文本": r.plain(), "HTML": r.htmlText()} {
			if strings.Contains(text, secret) {
				t.Errorf("%s中出现了应被抹去的机密或令牌", label)
			}
		}
	}
}

// TestRenderStatusMode 断言纯状态模式：主题的标题只有事件中文名，正文是固定的一句状态说明加页脚；Title 与 Body 中的任何金丝雀
// 都不出现在原始字节、解码后的主题与两个部件中；令牌仍在主题与页脚各一次；纯状态模式不需要 Scrubber。
func TestRenderStatusMode(t *testing.T) {
	tag := issueTag(t, testTask, 0x58)
	c, canaries := canaryContent(t)
	c.Event = EventWaitingInput
	for _, opts := range []Options{{Mode: ModeStatus}, {Mode: ModeStatus, Scrub: testScrubber()}} {
		raw, r := mustRender(t, testEnvelope(), tag, c, opts)
		if r.subject != tag.Reveal()+" 等待输入" {
			t.Errorf("主题为 %q，期望只有事件名", strings.TrimPrefix(r.subject, tag.Reveal()))
		}
		status := "任务 " + testTask + " 的新状态是「等待输入」，本通知只含状态，任务输出留在本机。"
		if want := expectedPlain(status, footer(testTask, "等待输入", tag.RevealToken(), false)); r.plain() != want {
			t.Errorf("纯文本为\n%s\n期望\n%s", r.plain(), want)
		}
		for _, canary := range canaries {
			for label, text := range map[string]string{"原始字节": string(raw), "主题": r.subject, "纯文本": r.plain(), "HTML": r.htmlText()} {
				if strings.Contains(text, canary) {
					t.Errorf("%s中出现了内容中的金丝雀 %q", label, canary)
				}
			}
		}
		for label, text := range map[string]string{"主题": r.subject, "纯文本": r.plain(), "HTML": r.htmlText()} {
			if n := strings.Count(text, tag.RevealToken()); n != 1 {
				t.Errorf("%s中令牌文本出现 %d 次，期望恰好 1 次", label, n)
			}
		}
	}
}

// TestValidMessageID 钉住 msg-id 的形状：< 左部 @ 右部 >，左右两部非空、只含 0x21–0x7e 中除 <、>、@ 之外的字符，
// 总长至多 980 字节（加上最长的头名与冒号空格仍不超过 998）；CR、LF、空白、制表、NUL、DEL、非 ASCII、缺尖括号或 @ 都不合法。
func TestValidMessageID(t *testing.T) {
	long := func(n int) string {
		return "<" + strings.Repeat("a", n-len("<@example.invalid>")) + "@example.invalid>"
	}
	for _, id := range []string{"<a@b>", testMessageID, "<tencent_A1.b-c+d=e_f@qq.example.invalid>", "<!#$%&'*+/=?^_`{|}~@[1.2.3.4]>", long(980)} {
		if !validMessageID(id) {
			t.Errorf("合法的 msg-id 被拒绝：%q", id)
		}
	}
	for _, id := range []string{
		"", "<>", "<@>", "a@b", "<a@b", "a@b>", "<ab>", "<@b>", "<a@>", "<a@b@c>", "<a<b@c>", "<a@b>c>", "<a@b> ", " <a@b>",
		"<a b@c>", "<a\tb@c>", "<a\r@b>", "<a\n@b>", "<a@b\r\nBcc: x@example.invalid>", "<a\x00@b>", "<a\x7f@b>", "<中@b>", "<a@例子>",
		"<a@b>\r\n", long(981),
	} {
		if validMessageID(id) {
			t.Errorf("不合法的 msg-id 被接受：%q", id)
		}
	}
}

// TestRenderRejectsEnvelope 断言信封不合法时 Render 返回 ErrInvalidEnvelope、不返回任何字节：Message-ID 或任一 References
// 不符合 msg-id 的形状（含 CR、LF、空白、缺尖括号、缺 @、非 ASCII），地址不是单个 addr-spec，日期为零值。
func TestRenderRejectsEnvelope(t *testing.T) {
	tag := issueTag(t, testTask, 0x59)
	cases := []struct {
		name string
		edit func(*Envelope)
	}{
		{"Message-ID 含 CR", func(e *Envelope) { e.MessageID = "<a\r@b.example.invalid>" }},
		{"Message-ID 含 LF 注入头部", func(e *Envelope) { e.MessageID = "<a@b.example.invalid>\nBcc: x@example.invalid" }},
		{"Message-ID 含空白", func(e *Envelope) { e.MessageID = "<a b@b.example.invalid>" }},
		{"Message-ID 缺尖括号", func(e *Envelope) { e.MessageID = "a@b.example.invalid" }},
		{"Message-ID 缺 @", func(e *Envelope) { e.MessageID = "<ab.example.invalid>" }},
		{"Message-ID 非 ASCII", func(e *Envelope) { e.MessageID = "<通知@b.example.invalid>" }},
		{"Message-ID 为空", func(e *Envelope) { e.MessageID = "" }},
		{"References 中一个含 CRLF", func(e *Envelope) { e.References = []string{"<a@b>", "<c@d>\r\nBcc: x@example.invalid"} }},
		{"References 中一个缺尖括号", func(e *Envelope) { e.References = []string{"<a@b>", "c@d"} }},
		{"References 中一个为空", func(e *Envelope) { e.References = []string{""} }},
		{"发件地址为空", func(e *Envelope) { e.From = "" }},
		{"发件地址带显示名", func(e *Envelope) { e.From = "Bot <" + testFrom + ">" }},
		{"发件地址含 CRLF", func(e *Envelope) { e.From = testFrom + "\r\nBcc: x@example.invalid" }},
		{"收件地址没有 @", func(e *Envelope) { e.To = "me.example.invalid" }},
		{"收件地址两个 @", func(e *Envelope) { e.To = "me@x@example.invalid" }},
		{"收件地址含空白", func(e *Envelope) { e.To = "me @example.invalid" }},
		{"收件地址非 ASCII", func(e *Envelope) { e.To = "我@example.invalid" }},
		{"收件地址过长", func(e *Envelope) { e.To = strings.Repeat("a", 64) + "@" + strings.Repeat("b", 186) + ".invalid" }},
		{"日期为零值", func(e *Envelope) { e.Date = time.Time{} }},
	}
	for _, tc := range cases {
		env := testEnvelope()
		tc.edit(&env)
		raw, err := Render(env, tag, validContent(), filteredOptions())
		if !errors.Is(err, ErrInvalidEnvelope) || raw != nil {
			t.Errorf("%s：Render = %d 字节, %v；期望 nil 与 ErrInvalidEnvelope", tc.name, len(raw), err)
		}
	}
	env := testEnvelope()
	env.To = strings.Repeat("a", 64) + "@" + strings.Repeat("b", 181) + ".invalid"
	if _, err := Render(env, tag, validContent(), filteredOptions()); err != nil {
		t.Errorf("254 个字符的地址被拒绝：%v", err)
	}
}

// fakeTag 是测试用的 Tag 实现，返回任意文本，用来检查渲染器对标签形状与任务的校验。
type fakeTag struct {
	full, text string
}

// Reveal 返回预设的完整标签文本。
func (f fakeTag) Reveal() string { return f.full }

// RevealToken 返回预设的令牌文本。
func (f fakeTag) RevealToken() string { return f.text }

// TestRenderRejectsTag 断言标签的形状与任务都经核对：Reveal 须恰为 [TC <10 位任务 ID> <48 位令牌>]（小写、单个空格、没有其他文字），
// RevealToken 须等于其中的令牌，任务 ID 须等于内容的任务；不符时返回 ErrInvalidTag、不返回字节。形状正确的实现照常渲染。
func TestRenderRejectsTag(t *testing.T) {
	good := issueTag(t, testTask, 0x5a)
	other := issueTag(t, "zzzzzzzzzz", 0x5b)
	full, text := good.Reveal(), good.RevealToken()
	if _, err := Render(testEnvelope(), fakeTag{full, text}, validContent(), filteredOptions()); err != nil {
		t.Fatalf("形状正确的标签被拒绝：%v", err)
	}
	for name, tag := range map[string]Tag{
		"另一个任务的标签":    other,
		"令牌副本不一致":     fakeTag{full, issueText(t, testTask, 0x5c)},
		"令牌大写":        fakeTag{strings.ToUpper(full[:15]) + strings.ToUpper(text) + "]", strings.ToUpper(text)},
		"TC 小写":       fakeTag{"[tc" + full[3:], text},
		"令牌少一位":       fakeTag{full[:62] + "]", text[:47]},
		"两个空格":        fakeTag{full[:14] + " " + full[14:], text},
		"标签后有换行注入":    fakeTag{full + "\r\nBcc: x@example.invalid", text},
		"标签后有其他文字":    fakeTag{full + " x", text},
		"标签前有其他文字":    fakeTag{"x" + full, text},
		"空标签":         fakeTag{"", ""},
		"nil 标签":      nil,
		"令牌含字母表之外的字母": fakeTag{full[:20] + "i" + full[21:], text[:5] + "i" + text[6:]},
	} {
		raw, err := Render(testEnvelope(), tag, validContent(), filteredOptions())
		if !errors.Is(err, ErrInvalidTag) || raw != nil {
			t.Errorf("%s：Render = %d 字节, %v；期望 nil 与 ErrInvalidTag", name, len(raw), err)
		}
	}
}

// TestRenderRejectsOptionsAndContent 断言模式须显式选定（零值与未知值都是 ErrInvalidOptions），过滤模式必须带 Scrubber，
// 不合法的内容返回 ErrInvalidContent；出错时都不返回字节。
func TestRenderRejectsOptionsAndContent(t *testing.T) {
	tag := issueTag(t, testTask, 0x5d)
	for name, opts := range map[string]Options{
		"模式为零值":           {Scrub: testScrubber()},
		"未知模式":            {Mode: ModeStatus + 1, Scrub: testScrubber()},
		"过滤模式没有 Scrubber": {Mode: ModeFiltered},
	} {
		if raw, err := Render(testEnvelope(), tag, validContent(), opts); !errors.Is(err, ErrInvalidOptions) || raw != nil {
			t.Errorf("%s：Render = %d 字节, %v；期望 nil 与 ErrInvalidOptions", name, len(raw), err)
		}
	}
	for name, edit := range map[string]func(*Content){
		"版本不为 1":  func(c *Content) { c.Version = 2 },
		"未知事件":    func(c *Content) { c.Event = "closed" },
		"任务 ID 错": func(c *Content) { c.TaskID = "Q7M3K9X2PA" },
	} {
		c := validContent()
		edit(&c)
		if raw, err := Render(testEnvelope(), tag, c, filteredOptions()); !errors.Is(err, ErrInvalidContent) || raw != nil {
			t.Errorf("%s：Render = %d 字节, %v；期望 nil 与 ErrInvalidContent", name, len(raw), err)
		}
	}
}

// TestRenderSizeLimit 钉住 256 KiB 的上限：用长 References 把渲染结果调到恰好 MaxMessageSize 字节时成功，多 1 字节即返回 ErrTooLarge
// 且不返回字节。渲染结果的长度除随机边界的内容外都是确定的（边界长度固定）：每多一个 900 个字符的引用，邮件多出固定的字节数
// （引用各占一个折行），中间一个引用每多一个字符，邮件恰好多一个字节，所以只需渲染几次就能算出恰好到上限的输入。
func TestRenderSizeLimit(t *testing.T) {
	if MaxMessageSize != 256<<10 {
		t.Fatalf("MaxMessageSize = %d，契约规定为 256 KiB", MaxMessageSize)
	}
	tag := issueTag(t, testTask, 0x5e)
	ref := func(i, n int) string { return fmt.Sprintf("<%0*d@example.invalid>", n-len("<@example.invalid>"), i) }
	// size 返回以 refs 为 References 渲染的字节数。
	size := func(refs []string) int {
		env := testEnvelope()
		env.References = refs
		raw, err := Render(env, tag, validContent(), filteredOptions())
		if err != nil {
			t.Fatalf("Render 返回错误：%v", err)
		}
		return len(raw)
	}
	refs := []string{ref(0, 900), ref(1, 900)}
	base := size(refs)
	step := size(append(refs, ref(2, 900))) - base
	for n := 2 + (MaxMessageSize-2000-base)/step; len(refs) < n; {
		refs = append(refs, ref(len(refs), 900))
	}
	// 把缺口分到开头之后的几个引用上，每个至多加长到 980 个字符（In-Reply-To 取最后一个引用，不动它）。
	gap := MaxMessageSize - size(refs)
	for i := 1; gap > 0; i++ {
		grow := min(gap, 980-900)
		refs[i] = ref(i, 900+grow)
		gap -= grow
	}
	if got := size(refs); got != MaxMessageSize {
		t.Fatalf("没能把渲染结果调到恰好上限：%d 字节", got)
	}
	last := len(refs) - 2
	refs[last] = ref(last, 901)
	env := testEnvelope()
	env.References = refs
	if raw, err := Render(env, tag, validContent(), filteredOptions()); !errors.Is(err, ErrTooLarge) || raw != nil {
		t.Errorf("超过上限 1 字节：Render = %d 字节, %v；期望 nil 与 ErrTooLarge", len(raw), err)
	}
}

// TestRenderErrorsDoNotEcho 断言 Render 的每一种错误的文本都不含标题、正文、令牌、机密与地址。
func TestRenderErrorsDoNotEcho(t *testing.T) {
	tag := issueTag(t, testTask, 0x5f)
	c, canaries := canaryContent(t)
	forbidden := append(canaries, tag.RevealToken(), testFrom, testTo, "example.invalid")
	bigEnv := testEnvelope()
	for i := range 300 {
		bigEnv.References = append(bigEnv.References, fmt.Sprintf("<%0900d@example.invalid>", i))
	}
	badEnv := testEnvelope()
	badEnv.To = "canary " + testTo
	badContent := c
	badContent.Version = 2
	for name, err := range map[string]error{
		"过大":     second(Render(bigEnv, tag, c, filteredOptions())),
		"信封":     second(Render(badEnv, tag, c, filteredOptions())),
		"标签":     second(Render(testEnvelope(), fakeTag{tag.Reveal() + " " + tag.RevealToken(), tag.RevealToken()}, c, filteredOptions())),
		"选项":     second(Render(testEnvelope(), tag, c, Options{})),
		"内容":     second(Render(testEnvelope(), tag, badContent, filteredOptions())),
		"另一任务标签": second(Render(testEnvelope(), issueTag(t, "zzzzzzzzzz", 0x60), c, filteredOptions())),
	} {
		if err == nil {
			t.Errorf("%s：没有返回错误", name)
			continue
		}
		for _, bad := range forbidden {
			if strings.Contains(err.Error(), bad) {
				t.Errorf("%s：错误文本回显了内容：%v", name, err)
				break
			}
		}
	}
}

// second 返回两个返回值中的错误，便于在表中直接写 Render 调用。
func second(_ []byte, err error) error {
	return err
}
