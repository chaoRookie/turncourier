// Package parser 测试入站邮件的解析：合成回归样本逐个比对期望的新正文或错误；头部字段（发件人、主题、线程头、自动回复信号、
// 「已发送」副本的两个 ID）的边界；只解析头部的 ParseHeader 与 Parse 一致；纯文本部件的选取、字符集与传输编码、换行与 BOM；
// 深度、部件数、正文与头部大小的上限；确定性；脱敏输出与不回显来信字节的错误。样本中的令牌由 security/token 在运行时签发，
// 测试用的机密形状的文字都在运行时构造。
package parser

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"

	"github.com/chaoRookie/turncourier/internal/mail"
	"github.com/chaoRookie/turncourier/internal/security/token"
	mailfixture "github.com/chaoRookie/turncourier/tests/fixtures/mail"
)

const (
	// fixtureTask 是样本中 {{TASK}} 的取值，含字母与数字，与令牌的字母表相同。
	fixtureTask = "q7m3k9x2pa"
	// fixtureDelivered 是样本中 {{DELIVERED}} 的取值，形如 QQ 改写后的实际投递 ID。
	fixtureDelivered = "<tencent_delivered_0001@example.invalid>"
	// messageMarker 是规格规定的 Message 脱敏文本，直接写出以钉住输出，不引用被测包的常量。
	messageMarker = "[redacted inbound message]"
)

// sentinels 把样本期望中的错误名映射到哨兵错误。
var sentinels = map[string]error{
	"ErrNoPlainText": ErrNoPlainText,
	"ErrUncertain":   ErrUncertain,
	"ErrEmpty":       ErrEmpty,
	"ErrMalformed":   ErrMalformed,
}

// issueText 用由 seed 重复而成的密钥为 taskID 签发一枚真实令牌，返回 48 个字符的令牌文本；不同的 seed 得到不同的令牌。
func issueText(tb testing.TB, taskID string, seed byte) string {
	tb.Helper()
	k, err := token.NewKey(1, bytes.Repeat([]byte{seed}, token.KeyLen))
	if err != nil {
		tb.Fatalf("NewKey: %v", err)
	}
	var nid token.NID
	for i := range nid {
		nid[i] = seed + byte(i)
	}
	issued, err := token.Issue(k, nid, token.Claims{TaskID: taskID, Owner: "local", ExpiresAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		tb.Fatalf("Issue: %v", err)
	}
	return issued.Reveal()
}

// fixtureValues 返回样本占位符的运行时取值：令牌由 security/token 签发。
func fixtureValues(tb testing.TB) mailfixture.Values {
	tb.Helper()
	return mailfixture.Values{Task: fixtureTask, Token: issueText(tb, fixtureTask, 0x42), Delivered: fixtureDelivered}
}

// buildSample 加载并组装一个合成样本。
func buildSample(tb testing.TB, name string, values mailfixture.Values) []byte {
	tb.Helper()
	s, err := mailfixture.Load(name)
	if err != nil {
		tb.Fatalf("Load(%s): %v", name, err)
	}
	raw, err := s.Build(values)
	if err != nil {
		tb.Fatalf("Build(%s): %v", name, err)
	}
	return raw
}

// mustParse 解析 raw，失败即终止测试。
func mustParse(tb testing.TB, raw []byte) *Message {
	tb.Helper()
	m, err := Parse(raw)
	if err != nil {
		tb.Fatalf("Parse: %v", err)
	}
	return m
}

// plainMessage 拼出一封单部件的邮件：合法的 From 与 Message-Id、额外头部与给定的 Content-Type（为空时不写），CRLF 换行；
// body 原样写入。
func plainMessage(contentType, body string, headers ...string) []byte {
	var b strings.Builder
	b.WriteString("From: user@example.invalid\r\nMessage-Id: <m@example.invalid>\r\n")
	for _, h := range headers {
		b.WriteString(h + "\r\n")
	}
	if contentType != "" {
		b.WriteString("Content-Type: " + contentType + "\r\n")
	}
	b.WriteString("\r\n" + body)
	return []byte(b.String())
}

// multipartBody 用分隔串把各部件（含部件头）拼成多部件的正文，结尾是结束分隔线。
func multipartBody(boundary string, parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString("--" + boundary + "\r\n" + p + "\r\n")
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.String()
}

// newTextOf 解析 raw 并返回 NewText 的结果。
func newTextOf(tb testing.TB, raw []byte) (string, error) {
	tb.Helper()
	return mustParse(tb, raw).NewText()
}

// gbEncode 用 GB18030 编码器把文字编为字节，测试中用它构造标注 gbk 系列而含四字节字符的正文与编码词。
func gbEncode(tb testing.TB, text string) []byte {
	tb.Helper()
	out, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(text))
	if err != nil {
		tb.Fatalf("encode GB18030: %v", err)
	}
	return out
}

// headerFields 返回 Message 的全部导出字段，供比较两个 Message 是否一致（不比较正文）。
func headerFields(m *Message) string {
	return fmt.Sprintf("%q|%q|%q|%q|%q|%q|%v|%q|%q|%q|%q|%q", m.From, m.Subject, m.MessageID, m.InReplyTo, m.References,
		m.AutoSubmitted, m.XAutoreply, m.Precedence, m.ReturnPath, m.MediaType, m.IDHeader, m.OQMsgID)
}

// TestFixtureSamples 逐个样本比对期望：Parse 必须成功，NewText 得到期望的新正文或期望的哨兵错误；同一字节解析两次、
// NewText 调用两次，结果都相同（键控摘要依赖解析器的确定性）。
func TestFixtureSamples(t *testing.T) {
	values := fixtureValues(t)
	samples, err := mailfixture.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, s := range samples {
		raw, err := s.Build(values)
		if err != nil {
			t.Fatalf("Build(%s): %v", s.Name, err)
		}
		m := mustParse(t, raw)
		got, err := m.NewText()
		if s.Expect.Error != "" {
			want := sentinels[s.Expect.Error]
			if want == nil || !errors.Is(err, want) || got != "" {
				t.Errorf("%s: NewText = %q, %v; want %s", s.Name, got, err, s.Expect.Error)
			}
		} else if err != nil || got != s.Expect.NewText {
			t.Errorf("%s: NewText = %q, %v; want %q", s.Name, got, err, s.Expect.NewText)
		}
		again, err2 := mustParse(t, raw).NewText()
		third, err3 := m.NewText()
		if again != got || third != got || !errors.Is(err2, err) || !errors.Is(err3, err) {
			t.Errorf("%s: repeated parsing is not deterministic", s.Name)
		}
	}
}

// TestFixtureHeaders 核对 L1 样本与几份公开资料样本的头部字段：发件人原样、主题完整解码（标签能被 token.ParseSubject 取回）、
// 线程头带尖括号、顶层媒体类型与自动来信信号。
func TestFixtureHeaders(t *testing.T) {
	values := fixtureValues(t)
	tag := "[TC " + values.Task + " " + values.Token + "]"
	title := " 回合完成：部署脚本已更新"
	delivered := []string{fixtureDelivered}
	// want 是一个样本期望的头部字段。
	type want struct {
		from, subject, messageID, mediaType, autoSubmitted string
		inReplyTo, references                              []string
	}
	cases := map[string]want{
		"qq-app-reply": {from: "user@example.invalid", subject: "Re: " + tag + title, messageID: "<tencent_qqapp_reply_0001@example.invalid>",
			mediaType: "multipart/alternative", inReplyTo: delivered, references: delivered},
		"qq-web-reply": {from: "user@example.invalid", subject: "回复：" + tag + title, messageID: "<tencent_qqweb_reply_0001@example.invalid>",
			mediaType: "multipart/alternative", inReplyTo: delivered, references: delivered},
		"qq-vacation-auto-reply": {from: "user@example.invalid", subject: "自动回复: " + tag + title,
			messageID: "<tencent_vacation_0001@example.invalid>", mediaType: "text/html", inReplyTo: delivered},
		"qq-bounce": {from: "PostMaster@example.invalid", subject: "系统退信", messageID: "<bounce_0001@example.invalid>",
			mediaType: "multipart/report", autoSubmitted: "auto-generated", inReplyTo: delivered, references: delivered},
		"foxmail-header-block": {from: "user@example.invalid", subject: "回复: " + tag + title,
			messageID: "<202609241512100261234@example.invalid>", mediaType: "multipart/alternative"},
		"gbk-four-byte": {from: "user@example.invalid", subject: "回复：" + tag + title, messageID: "<gbk-reply-0001@example.invalid>",
			mediaType: "text/plain", inReplyTo: delivered, references: delivered},
		"gmail-wrote": {from: "user@example.invalid", subject: "Re: " + tag + title,
			messageID: "<CAFx7yQ2p-synthetic-reply-0001@mail.example.invalid>", mediaType: "multipart/alternative",
			inReplyTo: delivered, references: delivered},
	}
	for name, w := range cases {
		m := mustParse(t, buildSample(t, name, values))
		if m.From != w.from || m.Subject != w.subject || m.MessageID != w.messageID || m.MediaType != w.mediaType ||
			m.AutoSubmitted != w.autoSubmitted || !slices.Equal(m.InReplyTo, w.inReplyTo) || !slices.Equal(m.References, w.references) {
			t.Errorf("%s: fields %s differ from the expected ones", name, headerFields(m))
		}
		if m.XAutoreply || m.Precedence != "" || m.ReturnPath != "" || m.IDHeader != "" || m.OQMsgID != "" {
			t.Errorf("%s: unexpected signals %s", name, headerFields(m))
		}
		if strings.Contains(w.subject, tag) {
			parsed, err := token.ParseSubject(m.Subject)
			if err != nil || parsed.TaskID() != fixtureTask || parsed.RevealToken() != values.Token {
				t.Errorf("%s: token.ParseSubject cannot take the tag back from the decoded subject (err %v)", name, err)
			}
		}
	}
}

// TestFixtureFootersMatchGateway 确认引用我方通知的样本在纯文本中逐字带着 internal/mail 的页脚标记行与固定句子：
// 改动这两个常量而不同步样本时，这里先失败。
func TestFixtureFootersMatchGateway(t *testing.T) {
	values := fixtureValues(t)
	for _, name := range []string{"qq-app-reply", "foxmail-header-block", "apple-mail-wrote", "gmail-wrote", "gbk-four-byte",
		"footer-without-separator", "inline-reply-wrote"} {
		body := mustParse(t, buildSample(t, name, values)).body
		plain := strings.ReplaceAll(string(body.plain), "\r\n", "\n")
		if !slices.Contains(strings.Split(plain, "\n"), mail.FooterMarker) && !strings.Contains(plain, "> "+mail.FooterMarker) {
			t.Errorf("%s: the quoted footer lacks the marker line", name)
		}
		if !strings.Contains(plain, mail.FooterNotice+"；请保留主题中的 [TC …] 标签，不要改动。") {
			t.Errorf("%s: the quoted footer lacks the fixed sentence", name)
		}
	}
}

// TestParseHeaderMatchesParse 确认 ParseHeader 只读头部：对每个样本的头部字节与完整字节，导出字段都与 Parse 相同；
// 正文相关的两个方法返回 ErrNoPlainText。
func TestParseHeaderMatchesParse(t *testing.T) {
	values := fixtureValues(t)
	for _, name := range mailfixture.Names() {
		raw := buildSample(t, name, values)
		full := mustParse(t, raw)
		end := bytes.Index(raw, []byte("\r\n\r\n"))
		if end < 0 {
			t.Fatalf("%s: no header terminator", name)
		}
		for label, input := range map[string][]byte{"header only": raw[:end+4], "whole message": raw} {
			m, err := ParseHeader(input)
			if err != nil {
				t.Fatalf("%s (%s): ParseHeader: %v", name, label, err)
			}
			if headerFields(m) != headerFields(full) {
				t.Errorf("%s (%s): ParseHeader fields %s, Parse fields %s", name, label, headerFields(m), headerFields(full))
			}
			if text, err := m.NewText(); text != "" || !errors.Is(err, ErrNoPlainText) {
				t.Errorf("%s (%s): NewText on a header-only message = %q, %v", name, label, text, err)
			}
			if plain, html, err := m.BodyPrefix(); plain != nil || html != nil || !errors.Is(err, ErrNoPlainText) {
				t.Errorf("%s (%s): BodyPrefix on a header-only message = %v", name, label, err)
			}
		}
	}
	if _, err := ParseHeader([]byte("Bad Header Line\r\n\r\n")); !errors.Is(err, ErrMalformed) {
		t.Errorf("ParseHeader of a malformed header = %v, want ErrMalformed", err)
	}
	if _, err := ParseHeader([]byte("Subject: x\r\n\r\n")); !errors.Is(err, ErrMalformed) {
		t.Errorf("ParseHeader without From = %v, want ErrMalformed", err)
	}
}

// TestParseFrom 覆盖 From 的边界：缺失、为空、出现两次、含两个地址、空的组、无法解析、显示名的字符集未知都返回 ErrMalformed；
// 合法时取唯一地址的原文（不改大小写，显示名被丢弃）。
func TestParseFrom(t *testing.T) {
	name := "=?utf-8?B?" + base64.StdEncoding.EncodeToString([]byte("张三")) + "?="
	rejected := map[string][]string{
		"missing":          {"Subject: x"},
		"empty":            {"From: "},
		"two fields":       {"From: a@example.invalid", "From: b@example.invalid"},
		"two addresses":    {"From: a@example.invalid, b@example.invalid"},
		"empty group":      {"From: undisclosed-recipients:;"},
		"not an address":   {"From: nobody"},
		"unknown charset":  {"From: =?x-unknown?B?YWJj?= <a@example.invalid>"},
		"invalid utf-8":    {"From: \xff <a@example.invalid>"},
		"unclosed bracket": {"From: <a@example.invalid"},
	}
	for label, headers := range rejected {
		raw := []byte(strings.Join(headers, "\r\n") + "\r\nContent-Type: text/plain\r\n\r\n正文")
		if m, err := Parse(raw); !errors.Is(err, ErrMalformed) || m != nil {
			t.Errorf("%s: Parse = %v, want ErrMalformed", label, err)
		}
	}
	accepted := map[string]string{
		"From: user@example.invalid":                     "user@example.invalid",
		"From: " + name + " <Zhang.San@Example.Invalid>": "Zhang.San@Example.Invalid",
		"From: \"用户\" <user@example.invalid>":            "user@example.invalid",
		"From: Group: user@example.invalid;":             "user@example.invalid",
		"from:   User <user@example.invalid>  ":          "user@example.invalid",
	}
	for header, want := range accepted {
		m, err := Parse([]byte(header + "\r\n\r\n正文"))
		if err != nil || m.From != want {
			t.Errorf("%q: From = %v, %v; want %q", header, m, err, want)
		}
	}
}

// TestParseIDs 覆盖 Message-Id 与线程头：Message-Id 去掉首尾空白后原样保留，缺失时为空；In-Reply-To 含多个 ID 时全部取出，
// References 中的逗号、注释与其他文字不影响其后的 ID；没有可识别 ID 的线程头为空；「已发送」副本的两个 ID 补成带尖括号的形式。
func TestParseIDs(t *testing.T) {
	m := mustParse(t, plainMessage("text/plain", "正文",
		"In-Reply-To: <a@example.invalid> <b@example.invalid>",
		"References: <c@example.invalid>, <d@example.invalid> (comment) Your message <e@example.invalid>",
		"X-TurnCourier-ID: tc.ours@example.invalid",
		"X-OQ-MSGID:   <tc.oq@example.invalid>  "))
	if m.MessageID != "<m@example.invalid>" {
		t.Errorf("MessageID = %q", m.MessageID)
	}
	if !slices.Equal(m.InReplyTo, []string{"<a@example.invalid>", "<b@example.invalid>"}) {
		t.Errorf("InReplyTo = %q", m.InReplyTo)
	}
	if !slices.Equal(m.References, []string{"<c@example.invalid>", "<d@example.invalid>", "<e@example.invalid>"}) {
		t.Errorf("References = %q", m.References)
	}
	if m.IDHeader != "<tc.ours@example.invalid>" || m.OQMsgID != "<tc.oq@example.invalid>" {
		t.Errorf("IDHeader %q, OQMsgID %q", m.IDHeader, m.OQMsgID)
	}
	raw := []byte("From: user@example.invalid\r\nMessage-ID:   <  spaced@example.invalid >  \r\nIn-Reply-To: none\r\n" +
		mail.IDHeader + ": <tc.header@example.invalid>\r\n\r\nx")
	m = mustParse(t, raw)
	if m.MessageID != "<  spaced@example.invalid >" || m.InReplyTo != nil || m.References != nil || m.IDHeader != "<tc.header@example.invalid>" {
		t.Errorf("fields %s", headerFields(m))
	}
	m = mustParse(t, []byte("From: user@example.invalid\r\n\r\nx"))
	if m.MessageID != "" || m.InReplyTo != nil || m.References != nil || m.IDHeader != "" || m.OQMsgID != "" {
		t.Errorf("missing headers give %s", headerFields(m))
	}
}

// TestParseSubject 覆盖主题的解码：标注 gbk 系列（gbk、gb2312、cp936、x-gbk、windows-936，大小写不同也一样）的编码词按 GB18030
// 解码，含四字节字符；utf-8 的 B 与 Q 编码；编码词之间的折行；纯 ASCII 原样；缺失为空；字符集未知时 Parse 返回 ErrMalformed。
func TestParseSubject(t *testing.T) {
	text := "回复：[TC " + fixtureTask + "] 𠮷野家㐀😀"
	for _, label := range []string{"gbk", "gb2312", "cp936", "x-gbk", "windows-936", "GBK", "GB18030"} {
		word := "=?" + label + "?B?" + base64.StdEncoding.EncodeToString(gbEncode(t, text)) + "?="
		m := mustParse(t, plainMessage("text/plain", "x", "Subject: "+word))
		if m.Subject != text {
			t.Errorf("%s: Subject = %q, want %q", label, m.Subject, text)
		}
	}
	first := "=?utf-8?B?" + base64.StdEncoding.EncodeToString([]byte("前半")) + "?="
	second := "=?UTF-8?Q?=E5=90=8E=E5=8D=8A?="
	cases := map[string]string{
		"Subject: Re: [TC x] " + first + "\r\n " + second: "Re: [TC x] 前半后半",
		"Subject: plain ascii":                            "plain ascii",
		"X-Other: 1":                                      "",
	}
	for header, want := range cases {
		raw := []byte("From: user@example.invalid\r\n" + header + "\r\n\r\nx")
		if m := mustParse(t, raw); m.Subject != want {
			t.Errorf("%q: Subject = %q, want %q", header, m.Subject, want)
		}
	}
	if m, err := Parse(plainMessage("text/plain", "x", "Subject: =?x-unknown?B?YWJj?= [TC x]")); !errors.Is(err, ErrMalformed) || m != nil {
		t.Errorf("unknown subject charset: %v, want ErrMalformed", err)
	}
}

// TestParseSignals 覆盖自动回复判定所需的头部信号：Auto-Submitted、Precedence 与 Return-Path 取第一个的原文去首尾空白；
// X-Autoreply 或 X-Autorespond 只要存在（取值为空也算）就为真；MediaType 是顶层 Content-Type 分号前的部分转小写，
// 参数不合法时同样取得，没有 Content-Type 时为 text/plain。
func TestParseSignals(t *testing.T) {
	// want 是一组头部期望得到的信号字段。
	type want struct {
		autoSubmitted, precedence, returnPath, mediaType string
		xAutoreply                                       bool
	}
	cases := []struct {
		name    string
		headers []string
		want    want
	}{
		{"none", nil, want{mediaType: "text/plain"}},
		{"auto-submitted", []string{"Auto-Submitted:  auto-replied; owner-email=\"a@example.invalid\" "},
			want{autoSubmitted: "auto-replied; owner-email=\"a@example.invalid\"", mediaType: "text/plain"}},
		{"x-autoreply empty", []string{"X-Autoreply:"}, want{xAutoreply: true, mediaType: "text/plain"}},
		{"x-autorespond", []string{"X-Autorespond: yes"}, want{xAutoreply: true, mediaType: "text/plain"}},
		{"precedence", []string{"Precedence:  Bulk ", "Precedence: list"}, want{precedence: "Bulk", mediaType: "text/plain"}},
		{"return path", []string{"Return-Path:  <> "}, want{returnPath: "<>", mediaType: "text/plain"}},
		{"report", []string{"Content-Type: Multipart/Report; report-type=delivery-status; boundary=b"}, want{mediaType: "multipart/report"}},
		{"bad params", []string{"Content-Type: multipart/report; ;; boundary"}, want{mediaType: "multipart/report"}},
		{"empty type", []string{"Content-Type: "}, want{mediaType: "text/plain"}},
	}
	for _, tc := range cases {
		raw := []byte(strings.Join(append([]string{"From: user@example.invalid"}, tc.headers...), "\r\n") + "\r\n\r\n")
		m := mustParse(t, raw)
		got := want{autoSubmitted: m.AutoSubmitted, precedence: m.Precedence, returnPath: m.ReturnPath, mediaType: m.MediaType, xAutoreply: m.XAutoreply}
		if got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestParseRejectsBrokenHeaders 确认头部畸形时 Parse 与 ParseHeader 只返回哨兵错误：无法解析的头行、非法的字段名、
// 以续行开头的头部为 ErrMalformed，超过上限的头部为 ErrHeaderTooLarge；错误文本固定，不含来信中的任何字节。
func TestParseRejectsBrokenHeaders(t *testing.T) {
	cases := map[string]struct {
		raw  []byte
		want error
	}{
		"bad line":         {[]byte("From: a@example.invalid\r\nCANARY LINE WITHOUT COLON\r\n\r\nx"), ErrMalformed},
		"bad key":          {[]byte("From: a@example.invalid\r\nCANARY\x01KEY: v\r\n\r\nx"), ErrMalformed},
		"initial space":    {[]byte(" CANARY: v\r\nFrom: a@example.invalid\r\n\r\nx"), ErrMalformed},
		"empty input":      {nil, ErrMalformed},
		"too large header": {[]byte("From: a@example.invalid\r\nX-Canary: " + strings.Repeat("CANARY", maxHeaderBytes/6+1) + "\r\n\r\nx"), ErrHeaderTooLarge},
	}
	for name, tc := range cases {
		for label, parse := range map[string]func([]byte) (*Message, error){"Parse": Parse, "ParseHeader": ParseHeader} {
			m, err := parse(tc.raw)
			if m != nil || !errors.Is(err, tc.want) || err.Error() != tc.want.Error() {
				t.Errorf("%s: %s = %v, want %v", name, label, err, tc.want)
			}
			if err != nil && strings.Contains(strings.ToLower(err.Error()), "canary") {
				t.Errorf("%s: %s error echoes the input", name, label)
			}
		}
	}
}

// TestErrorTexts 钉住五个哨兵的固定文本，并确认它们互不相同。
func TestErrorTexts(t *testing.T) {
	texts := map[error]string{
		ErrMalformed:      "malformed inbound message",
		ErrHeaderTooLarge: "inbound message header exceeds the parser limit",
		ErrNoPlainText:    "inbound message has no plain text part",
		ErrUncertain:      "cannot tell the new text from quoted text",
		ErrEmpty:          "inbound message has no new text",
	}
	for err, want := range texts {
		if err.Error() != want {
			t.Errorf("error text %q, want %q", err.Error(), want)
		}
	}
}

// TestNewTextSelectsPlainPart 确认取第一个非附件的 text/plain 部件：深度优先，跳过 Content-Disposition 为 attachment
// （不区分大小写）的部件，inline 带文件名的照常选取；不进入 message/rfc822；Content-Type 无法解析的部件不选；
// 没有 Content-Type 按 text/plain；只有 HTML 为 ErrNoPlainText。
func TestNewTextSelectsPlainPart(t *testing.T) {
	alternative := "Content-Type: multipart/alternative; boundary=inner\r\n\r\n" + multipartBody("inner",
		"Content-Type: text/plain; charset=utf-8\r\n\r\n正文",
		"Content-Type: text/html; charset=utf-8\r\n\r\n<p>网页</p>")
	cases := map[string]struct {
		raw  []byte
		want string
		err  error
	}{
		"nested after attachment": {plainMessage("multipart/mixed; boundary=outer", multipartBody("outer",
			"Content-Type: text/plain\r\nContent-Disposition: ATTACHMENT; filename=\"a.txt\"\r\n\r\n附件文字",
			alternative,
			"Content-Type: text/plain\r\n\r\n后面的文字")), "正文", nil},
		"inline with file name": {plainMessage("multipart/mixed; boundary=outer", multipartBody("outer",
			"Content-Type: text/plain; charset=utf-8\r\nContent-Disposition: inline; filename=\"note.txt\"\r\n\r\n内联文字")), "内联文字", nil},
		"first of two": {plainMessage("multipart/mixed; boundary=outer", multipartBody("outer",
			"Content-Type: text/plain; charset=utf-8\r\n\r\n第一段",
			"Content-Type: text/plain; charset=utf-8\r\n\r\n第二段")), "第一段", nil},
		"rfc822 not entered": {plainMessage("multipart/mixed; boundary=outer", multipartBody("outer",
			"Content-Type: message/rfc822\r\n\r\nContent-Type: text/plain\r\n\r\n转发的文字")), "", ErrNoPlainText},
		"broken part type": {plainMessage("multipart/mixed; boundary=outer", multipartBody("outer",
			"Content-Type: text/plain; charset=utf-8; charset=gbk\r\n\r\n参数重复")), "", ErrNoPlainText},
		"no content type": {plainMessage("", "默认纯文本"), "默认纯文本", nil},
		"html only":       {plainMessage("text/html; charset=utf-8", "<p>网页</p>"), "", ErrNoPlainText},
		"broken top type": {plainMessage("text/plain; charset=utf-8;; x", "正文"), "", ErrNoPlainText},
		"attachment only": {plainMessage("text/plain", "附件文字", "Content-Disposition: attachment"), "", ErrNoPlainText},
	}
	for name, tc := range cases {
		got, err := newTextOf(t, tc.raw)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("%s: NewText = %q, %v; want %q, %v", name, got, err, tc.want, tc.err)
		}
	}
}

// TestNewTextCharsets 覆盖字符集与传输编码：五个 GBK 系列标签（及大写变体）与 gb18030 都按 GB18030 解码出四字节字符；
// 未知字符集、未知传输编码、损坏的 base64、声明 utf-8 而含非法字节、解码器用替换字符顶替了非法字节，都是 ErrMalformed；
// us-ascii 或缺省字符集下的合法 UTF-8 原样接受，utf-8 正文中本来就有的替换字符也接受；quoted-printable 照常解码。
func TestNewTextCharsets(t *testing.T) {
	text := "报告里的「𠮷」「㐀」都正常😀"
	encoded := base64.StdEncoding.EncodeToString(gbEncode(t, text))
	for _, label := range []string{"gbk", "gb2312", "cp936", "x-gbk", "windows-936", "GBK", "Gb2312", "gb18030"} {
		raw := plainMessage("text/plain; charset="+label, encoded, "Content-Transfer-Encoding: base64")
		if got, err := newTextOf(t, raw); got != text || err != nil {
			t.Errorf("%s: NewText = %q, %v", label, got, err)
		}
	}
	cases := map[string]struct {
		raw  []byte
		want string
		err  error
	}{
		"unknown charset":     {plainMessage("text/plain; charset=x-unknown", "abc"), "", ErrMalformed},
		"empty charset":       {plainMessage("text/plain; charset=\"\"", "abc"), "", ErrMalformed},
		"unknown encoding":    {plainMessage("text/plain; charset=utf-8", "abc", "Content-Transfer-Encoding: x-uuencode"), "", ErrMalformed},
		"corrupt base64":      {plainMessage("text/plain; charset=utf-8", "@@@@", "Content-Transfer-Encoding: base64"), "", ErrMalformed},
		"invalid utf-8":       {plainMessage("text/plain; charset=utf-8", "正文\xff\xfe"), "", ErrMalformed},
		"invalid default":     {plainMessage("text/plain", "abc\xc3"), "", ErrMalformed},
		"gbk replacement":     {plainMessage("text/plain; charset=gbk", "ok\x81\x20ok"), "", ErrMalformed},
		"ascii with utf-8":    {plainMessage("text/plain; charset=us-ascii", "中文"), "中文", nil},
		"literal replacement": {plainMessage("text/plain; charset=utf-8", "替换字符\ufffd保留"), "替换字符\ufffd保留", nil},
		"quoted-printable": {plainMessage("text/plain; charset=utf-8", "=E7=BB=A7=E7=BB=AD=\r\n=E6=89=A7=E8=A1=8C",
			"Content-Transfer-Encoding: quoted-printable"), "继续执行", nil},
		"latin1": {plainMessage("text/plain; charset=iso-8859-1", "caf\xe9"), "café", nil},
	}
	for name, tc := range cases {
		got, err := newTextOf(t, tc.raw)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("%s: NewText = %q, %v; want %q, %v", name, got, err, tc.want, tc.err)
		}
	}
}

// TestNewTextNormalization 确认换行统一为 \n（CRLF 与单独的 CR 都转换）、开头的 BOM 被去掉、首尾的空行与空白被去掉，
// 行内与行尾之外的空白保持原样；只有空白或 BOM 的正文为 ErrEmpty。
func TestNewTextNormalization(t *testing.T) {
	cases := map[string]struct {
		body, want string
		err        error
	}{
		"crlf":           {"第一行\r\n第二行\r\n", "第一行\n第二行", nil},
		"lone cr":        {"第一行\r第二行\r\r第三行", "第一行\n第二行\n\n第三行", nil},
		"bom":            {"\ufeff正文", "正文", nil},
		"trim":           {"\r\n \u3000\r\n  缩进的第一行\r\n第二行 \r\n\r\n\t", "缩进的第一行\n第二行", nil},
		"inner blank":    {"甲\r\n\r\n\r\n乙", "甲\n\n\n乙", nil},
		"only blank":     {"\r\n \r\n\t\r\n", "", ErrEmpty},
		"only bom":       {"\ufeff\r\n", "", ErrEmpty},
		"empty":          {"", "", ErrEmpty},
		"only signature": {"-- \r\n张三\r\n", "", ErrEmpty},
		"only quote":     {"\r\n------------------ 原始邮件 ------------------\r\n旧的内容", "", ErrEmpty},
	}
	for name, tc := range cases {
		got, err := newTextOf(t, plainMessage("text/plain; charset=utf-8", tc.body))
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("%s: NewText = %q, %v; want %q, %v", name, got, err, tc.want, tc.err)
		}
	}
}

// nestedMessage 拼出 levels 层 multipart/mixed 嵌套、最内层含一个纯文本部件的邮件：levels 为 8 时纯文本部件的深度恰为 8。
func nestedMessage(levels int) []byte {
	entity := "Content-Type: text/plain; charset=utf-8\r\n\r\n深层正文"
	for i := levels; i >= 1; i-- {
		boundary := fmt.Sprintf("level%d", i)
		entity = "Content-Type: multipart/mixed; boundary=" + boundary + "\r\n\r\n" + multipartBody(boundary, entity)
	}
	return []byte("From: user@example.invalid\r\n" + entity)
}

// partsMessage 拼出一个含 n 个部件的 multipart/mixed：前 n−1 个是附件，纯文本在第 plainAt 个（从 1 计）。
func partsMessage(n, plainAt int) []byte {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "Content-Type: application/octet-stream\r\n\r\nbinary"
	}
	parts[plainAt-1] = "Content-Type: text/plain; charset=utf-8\r\n\r\n第" + fmt.Sprint(plainAt) + "个部件"
	return plainMessage("multipart/mixed; boundary=many", multipartBody("many", parts...))
}

// TestNewTextLimits 覆盖上限：纯文本部件深度 8 可读、9 为 ErrMalformed；每层第 32 个部件可读、第 33 个为 ErrMalformed，
// 纯文本在上限之前已经找到时照常返回；解码后恰为 1 MiB 的正文可读且结果不超过 1 MiB，多 1 个字节为 ErrMalformed；
// 多部件没有分隔串参数或缺少结束分隔线时，纯文本部件读不完整为 ErrMalformed，已完整读到的照常返回。
func TestNewTextLimits(t *testing.T) {
	cases := map[string]struct {
		raw  []byte
		want string
		err  error
	}{
		"depth 8":          {nestedMessage(maxDepth), "深层正文", nil},
		"depth 9":          {nestedMessage(maxDepth + 1), "", ErrMalformed},
		"part 32":          {partsMessage(maxParts, maxParts), "第32个部件", nil},
		"part 33":          {partsMessage(maxParts+1, maxParts+1), "", ErrMalformed},
		"found before cap": {partsMessage(maxParts+8, 1), "第1个部件", nil},
		"no boundary":      {plainMessage("multipart/mixed", multipartBody("x", "Content-Type: text/plain\r\n\r\n正文")), "", ErrMalformed},
		"truncated plain": {plainMessage("multipart/mixed; boundary=cut",
			"--cut\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n没有结束分隔线"), "", ErrMalformed},
		"truncated after plain": {plainMessage("multipart/mixed; boundary=cut",
			"--cut\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n完整的正文\r\n--cut\r\nContent-Type: text/html\r\n\r\n<p>截断"), "完整的正文", nil},
		"bad part header": {plainMessage("multipart/mixed; boundary=bad",
			"--bad\r\nNot A Header\r\n\r\n正文\r\n--bad--\r\n"), "", ErrMalformed},
	}
	for name, tc := range cases {
		got, err := newTextOf(t, tc.raw)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("%s: NewText = %q, %v; want %q, %v", name, got, err, tc.want, tc.err)
		}
	}
	exact := strings.Repeat("甲", maxPartBytes/3) + strings.Repeat("a", maxPartBytes%3)
	got, err := newTextOf(t, plainMessage("text/plain; charset=utf-8", exact))
	if err != nil || len(got) != maxPartBytes || got != exact {
		t.Errorf("a 1 MiB body gives %d bytes, %v", len(got), err)
	}
	if got, err := newTextOf(t, plainMessage("text/plain; charset=utf-8", exact+"a")); got != "" || !errors.Is(err, ErrMalformed) {
		t.Errorf("a body over 1 MiB gives %d bytes, %v; want ErrMalformed", len(got), err)
	}
	base64Exact := base64.StdEncoding.EncodeToString([]byte(exact))
	if got, err := newTextOf(t, plainMessage("text/plain; charset=utf-8", base64Exact, "Content-Transfer-Encoding: base64")); err != nil || len(got) != maxPartBytes {
		t.Errorf("a base64 body decoding to 1 MiB gives %d bytes, %v", len(got), err)
	}
}

// TestBodyPrefix 确认 BodyPrefix 返回解码后的纯文本与 HTML 部件的前至多 512 字节（不经过换行规范化与引用剥离），
// 没有该部件时为 nil；返回的是副本，改动它不影响下一次调用；解码失败的纯文本部件仍返回已解码的开头。
func TestBodyPrefix(t *testing.T) {
	values := fixtureValues(t)
	plain, html, err := mustParse(t, buildSample(t, "qq-vacation-auto-reply", values)).BodyPrefix()
	if err != nil || plain != nil || len(html) == 0 || len(html) > PrefixLen || !bytes.HasPrefix(html, []byte("<div")) ||
		!bytes.Contains(html, []byte("这是来自QQ邮箱的假期自动回复邮件")) {
		t.Errorf("vacation auto-reply prefixes: plain %q, html %d bytes, %v", plain, len(html), err)
	}
	long := strings.Repeat("前缀测试行\r\n", 200)
	raw := plainMessage("multipart/alternative; boundary=pre", multipartBody("pre",
		"Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n"+base64.StdEncoding.EncodeToString([]byte(long)),
		"Content-Type: text/html; charset=utf-8\r\n\r\n<p>短</p>"))
	m := mustParse(t, raw)
	plain, html, err = m.BodyPrefix()
	if err != nil || len(plain) != PrefixLen || string(plain) != long[:PrefixLen] || string(html) != "<p>短</p>" {
		t.Errorf("prefixes: plain %d bytes (match %v), html %q, %v", len(plain), string(plain) == long[:PrefixLen], html, err)
	}
	plain[0], html[0] = 'X', 'X'
	again, againHTML, _ := m.BodyPrefix()
	if again[0] == 'X' || againHTML[0] == 'X' {
		t.Error("BodyPrefix returns internal buffers")
	}
	bad := mustParse(t, plainMessage("text/plain; charset=utf-8", "开头\xff"))
	if plain, _, err := bad.BodyPrefix(); err != nil || string(plain) != "开头\xff" {
		t.Errorf("prefix of an undecodable body = %q, %v", plain, err)
	}
	if _, err := bad.NewText(); !errors.Is(err, ErrMalformed) {
		t.Errorf("NewText of an undecodable body = %v", err)
	}
}

// messageHolder 以导出字段持有 Message 的值与指针，确认 fmt 与 slog 格式化嵌套字段时同样调用脱敏方法。
type messageHolder struct {
	Value   Message
	Pointer *Message
}

// TestMessageRedaction 以金丝雀检查 Message 的全部格式化出口：fmt 的常用动词作用于值与指针时恰为脱敏文本；切片、映射与
// 含导出字段的结构体中含脱敏文本且不含主题、令牌、地址、Message-ID 与正文；slog 的 JSON 与文本处理器同样如此（JSON 处理器
// 对嵌套的结构体走 encoding/json，所以 json.Marshal 也要脱敏）；%p 作用于指针只输出地址。%p 作用于 Message 值是已知局限
// （与 token.Tag 相同），这里不断言。
func TestMessageRedaction(t *testing.T) {
	text := issueText(t, fixtureTask, 0x77)
	subject := "Re: [TC " + fixtureTask + " " + text + "] 金丝雀标题CANARYSUBJECT"
	word := "=?utf-8?B?" + base64.StdEncoding.EncodeToString([]byte(subject)) + "?="
	m := mustParse(t, []byte("From: canary.sender@example.invalid\r\nSubject: "+word+"\r\nMessage-Id: <canary.id@example.invalid>\r\n"+
		"In-Reply-To: <canary.thread@example.invalid>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n金丝雀正文CANARYBODY"))
	if m.Subject != subject {
		t.Fatalf("subject decoded as %q", m.Subject)
	}
	forbidden := []string{"CANARY", "canary", "金丝雀", text, strings.ToUpper(text), fixtureTask}
	clean := func(label, out string) {
		for _, bad := range forbidden {
			if strings.Contains(out, bad) {
				t.Errorf("%s leaks message content", label)
				return
			}
		}
	}
	holder := messageHolder{Value: *m, Pointer: m}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%t", "%10v", "%-30s"} {
		for _, v := range []any{*m, m} {
			if out := fmt.Sprintf(verb, v); out != messageMarker {
				t.Errorf("%s of %T = %q, want %q", verb, v, out, messageMarker)
			}
		}
		for _, v := range []any{[]*Message{m}, []Message{*m}, map[string]*Message{"m": m}, holder, &holder} {
			out := fmt.Sprintf(verb, v)
			if !strings.Contains(out, messageMarker) {
				t.Errorf("%s of %T = %q, want the redaction marker", verb, v, out)
			}
			clean(fmt.Sprintf("%s of %T", verb, v), out)
		}
	}
	if out := fmt.Sprintf("%p", m); !strings.HasPrefix(out, "0x") {
		t.Errorf("%%p of *Message = %q, want an address", out)
	}
	var buf bytes.Buffer
	for _, logger := range []*slog.Logger{slog.New(slog.NewJSONHandler(&buf, nil)), slog.New(slog.NewTextHandler(&buf, nil))} {
		buf.Reset()
		logger.Info("inbound", slog.Any("value", *m), slog.Any("pointer", m), slog.Any("holder", holder), slog.Any("list", []*Message{m}))
		out := buf.String()
		if !strings.Contains(out, messageMarker) {
			t.Errorf("slog output %q lacks the redaction marker", out)
		}
		clean("slog output", out)
	}
	if m.String() != messageMarker {
		t.Error("String does not return the redaction marker")
	}
	for _, v := range []any{*m, m, holder, []*Message{m}} {
		data, err := json.Marshal(v)
		if err != nil || !strings.Contains(string(data), `"`+messageMarker+`"`) {
			t.Errorf("json.Marshal(%T) = %s, %v; want the redaction marker", v, data, err)
		}
		clean(fmt.Sprintf("json.Marshal(%T)", v), string(data))
	}
	for _, v := range []slog.Value{m.LogValue(), slog.AnyValue(*m).Resolve(), slog.AnyValue(m).Resolve()} {
		if v.Kind() != slog.KindString || v.String() != messageMarker {
			t.Errorf("LogValue resolves to kind %v, want the redaction marker", v.Kind())
		}
	}
}

// TestErrorsDoNotEchoInput 以金丝雀确认 Parse、ParseHeader、NewText 与 BodyPrefix 的错误都是固定文本的哨兵：
// 来信中的头行、字段名、字符集名、传输编码名、地址与正文都不会进入错误文本。
func TestErrorsDoNotEchoInput(t *testing.T) {
	inputs := [][]byte{
		[]byte("From: a@example.invalid\r\nCANARY-LINE\r\n\r\nx"),
		[]byte("From: a@example.invalid\r\nSubject: =?x-canary-charset?B?YWJj?=\r\n\r\nx"),
		[]byte("From: =?x-canary?B?YWJj?= <canary@example.invalid>\r\n\r\nx"),
		[]byte("From: canary@example.invalid, other@example.invalid\r\n\r\nx"),
		plainMessage("text/plain; charset=x-canary", "CANARY"),
		plainMessage("text/plain", "CANARY", "Content-Transfer-Encoding: x-canary"),
		plainMessage("text/plain; charset=utf-8", "CANARY\xff"),
		plainMessage("text/plain; charset=utf-8", "CANARY\r\n\r\n> canary quote"),
		plainMessage("multipart/mixed; boundary=b", "--b\r\nCANARY BAD PART HEADER\r\n\r\nx\r\n--b--"),
		plainMessage("text/html", "<p>CANARY</p>"),
	}
	for i, raw := range inputs {
		var errs []error
		for _, parse := range []func([]byte) (*Message, error){Parse, ParseHeader} {
			m, err := parse(raw)
			errs = append(errs, err)
			if m != nil {
				_, textErr := m.NewText()
				_, _, prefixErr := m.BodyPrefix()
				errs = append(errs, textErr, prefixErr)
			}
		}
		for _, err := range errs {
			if err == nil {
				continue
			}
			if !slices.ContainsFunc([]error{ErrMalformed, ErrHeaderTooLarge, ErrNoPlainText, ErrUncertain, ErrEmpty}, func(s error) bool { return err == s }) {
				t.Errorf("input %d: error %v is not one of the sentinels", i, err)
			}
			if strings.Contains(strings.ToLower(err.Error()), "canary") {
				t.Errorf("input %d: error echoes the input", i)
			}
		}
	}
}

// TestNewTextIsValidUTF8 确认各样本与规范化用例的新正文都是合法 UTF-8、不超过 1 MiB、首尾没有空白。
func TestNewTextIsValidUTF8(t *testing.T) {
	values := fixtureValues(t)
	for _, name := range mailfixture.Names() {
		got, err := mustParse(t, buildSample(t, name, values)).NewText()
		if err != nil {
			continue
		}
		if !utf8.ValidString(got) || len(got) > maxPartBytes || strings.TrimSpace(got) != got {
			t.Errorf("%s: NewText result is not trimmed valid UTF-8", name)
		}
	}
}
