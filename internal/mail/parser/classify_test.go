// Package parser 测试自动回复与退信判定：四份 L1 样本分别判为真人、真人、自动回复（主题前缀）与退信，其余合成样本都是真人；
// 每条规则单独成立时都命中，且每个用例只满足一条；主题前缀的全部词干、全角与半角冒号、大小写与前缀前后的空白；
// 退信先于自动回复；不该命中的形态（Re: 自动回复:、正文中间的「自动回复」、Auto-Submitted: no 等）都判为真人；
// 固定开头只在正文的前 512 字节中查找。
package parser

import (
	"strings"
	"testing"

	mailfixture "github.com/chaoRookie/turncourier/tests/fixtures/mail"
)

// vacationPhrase 是 QQ 假期自动回复正文的固定开头，直接写出以钉住规则，不引用被测包的常量。
const vacationPhrase = "这是来自QQ邮箱的假期自动回复邮件"

// humanMessage 返回一封没有任何非人工信号的来信：白名单发件人、带标签的回复主题、multipart/alternative。
// 各用例在它的副本上只改一处，保证每个用例只满足一条规则。
func humanMessage() Message {
	return Message{
		From:      "user@example.invalid",
		Subject:   "Re: [TC " + fixtureTask + "] 回合完成",
		MessageID: "<reply@example.invalid>",
		MediaType: "multipart/alternative",
	}
}

// TestClassifyL1Samples 确认四份 L1 样本的判定：QQ 邮箱 App 与网页版的回复是真人，假期自动回复按主题前缀判为自动回复，
// 退信按 multipart/report 判为退信；其余合成样本（都是真人回复）都判为真人。正文开头取自 BodyPrefix。
func TestClassifyL1Samples(t *testing.T) {
	values := fixtureValues(t)
	want := map[string]Verdict{
		"qq-app-reply":           {Kind: Human},
		"qq-web-reply":           {Kind: Human},
		"qq-vacation-auto-reply": {Kind: AutoReply, Signal: SignalSubjectPrefix},
		"qq-bounce":              {Kind: Bounce, Signal: SignalMultipartReport},
	}
	for _, name := range mailfixture.Names() {
		m := mustParse(t, buildSample(t, name, values))
		plain, html, err := m.BodyPrefix()
		if err != nil {
			t.Fatalf("%s: BodyPrefix: %v", name, err)
		}
		expected, ok := want[name]
		if !ok {
			expected = Verdict{Kind: Human}
		}
		if got := Classify(m, plain, html); got != expected {
			t.Errorf("%s: Classify = %+v, want %+v", name, got, expected)
		}
	}
}

// TestClassifySingleRules 确认每条规则单独成立时都命中，并报告对应的信号名；每个用例只在 humanMessage 上改一处。
func TestClassifySingleRules(t *testing.T) {
	cases := []struct {
		name        string
		edit        func(*Message)
		plain, html string
		want        Verdict
	}{
		{name: "multipart/report", edit: func(m *Message) { m.MediaType = "multipart/report" }, want: Verdict{Bounce, SignalMultipartReport}},
		{name: "MAILER-DAEMON", edit: func(m *Message) { m.From = "MAILER-DAEMON@example.invalid" }, want: Verdict{Bounce, SignalDaemonSender}},
		{name: "mailer-daemon", edit: func(m *Message) { m.From = "mailer-daemon@mx.example.invalid" }, want: Verdict{Bounce, SignalDaemonSender}},
		{name: "PostMaster", edit: func(m *Message) { m.From = "PostMaster@example.invalid" }, want: Verdict{Bounce, SignalDaemonSender}},
		{name: "quoted local part", edit: func(m *Message) { m.From = `"a@b"@example.invalid` }, want: Verdict{Kind: Human}},
		// net/mail 把带引号的本地部分去掉引号后放进 Address："postmaster@relay"@example.invalid 变成下面的样子，
		// 本地部分是 postmaster@relay 而不是 postmaster，所以要按最后一个 @ 切分。
		{name: "unquoted local part with @", edit: func(m *Message) { m.From = "postmaster@relay@example.invalid" }, want: Verdict{Kind: Human}},
		{name: "subject prefix", edit: func(m *Message) { m.Subject = "自动回复: [TC " + fixtureTask + "] 回合完成" }, want: Verdict{AutoReply, SignalSubjectPrefix}},
		{name: "auto-replied", edit: func(m *Message) { m.AutoSubmitted = "auto-replied" }, want: Verdict{AutoReply, SignalAutoSubmitted}},
		{name: "auto-generated", edit: func(m *Message) { m.AutoSubmitted = "auto-generated" }, want: Verdict{AutoReply, SignalAutoSubmitted}},
		{name: "auto-notified with parameter", edit: func(m *Message) { m.AutoSubmitted = "Auto-Notified; owner-email=\"a@example.invalid\"" },
			want: Verdict{AutoReply, SignalAutoSubmitted}},
		{name: "x-autoreply", edit: func(m *Message) { m.XAutoreply = true }, want: Verdict{AutoReply, SignalXAutoreply}},
		{name: "precedence bulk", edit: func(m *Message) { m.Precedence = "bulk" }, want: Verdict{AutoReply, SignalPrecedence}},
		{name: "precedence junk", edit: func(m *Message) { m.Precedence = "junk" }, want: Verdict{AutoReply, SignalPrecedence}},
		{name: "precedence list", edit: func(m *Message) { m.Precedence = "list" }, want: Verdict{AutoReply, SignalPrecedence}},
		{name: "precedence auto_reply", edit: func(m *Message) { m.Precedence = "Auto_Reply" }, want: Verdict{AutoReply, SignalPrecedence}},
		{name: "empty return path", edit: func(m *Message) { m.ReturnPath = "<>" }, want: Verdict{AutoReply, SignalReturnPathEmpty}},
		{name: "spaced empty return path", edit: func(m *Message) { m.ReturnPath = "< >" }, want: Verdict{AutoReply, SignalReturnPathEmpty}},
		{name: "vacation opening in plain", plain: vacationPhrase + "。\r\n您好，我最近正在休假中。", want: Verdict{AutoReply, SignalQQVacationBody}},
		{name: "vacation opening in html", html: "<div style=\"font-size:14px;\">" + vacationPhrase + "。</div>", want: Verdict{AutoReply, SignalQQVacationBody}},
		{name: "none", want: Verdict{Kind: Human}},
	}
	for _, tc := range cases {
		m := humanMessage()
		if tc.edit != nil {
			tc.edit(&m)
		}
		if got := Classify(&m, []byte(tc.plain), []byte(tc.html)); got != tc.want {
			t.Errorf("%s: Classify = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestClassifySubjectPrefixes 覆盖主题前缀的全部词干（自动回复、自動回覆、自动答复、auto-reply、autoreply、automaticreply），
// 半角与全角冒号，ASCII 大小写，前缀之前、词干之内与冒号之前的空白（含全角空格与不换行空格）；只有主题开头的前缀才算，
// 缺少冒号、词干之后还有其他文字或前缀前面有 Re: 的都不命中。
func TestClassifySubjectPrefixes(t *testing.T) {
	var hits []string
	for _, stem := range []string{"自动回复", "自動回覆", "自动答复", "Auto-Reply", "AutoReply", "Automatic Reply", "auto-reply", "AUTOREPLY", "AutomaticReply"} {
		for _, colon := range []string{":", "："} {
			hits = append(hits, stem+colon+" [TC "+fixtureTask+"] 标题", "  "+stem+" "+colon+"标题", "\u3000"+stem+colon, "\u00a0\t"+stem+colon)
		}
	}
	hits = append(hits, "Auto - Reply: 标题", "自动 回复：", "Automatic reply:", "AUTOMATIC REPLY：休假中")
	for _, subject := range hits {
		m := humanMessage()
		m.Subject = subject
		if got := Classify(&m, nil, nil); got != (Verdict{AutoReply, SignalSubjectPrefix}) {
			t.Errorf("subject %q: Classify = %+v, want the subject prefix", subject, got)
		}
	}
	misses := []string{
		"Re: 自动回复: [TC " + fixtureTask + "] 标题",
		"回复：自动回复：标题",
		"自动回复 [TC " + fixtureTask + "] 没有冒号",
		"自动回复的说明：标题",
		"Auto-Replied: 标题",
		"autoreplies: 标题",
		"Automatic: reply",
		"关于自动回复: 标题",
		"",
	}
	for _, subject := range misses {
		m := humanMessage()
		m.Subject = subject
		if got := Classify(&m, nil, nil); got != (Verdict{Kind: Human}) {
			t.Errorf("subject %q: Classify = %+v, want Human", subject, got)
		}
	}
}

// TestClassifyBounceFirst 确认退信先于自动回复判定：主题以 Auto-Reply: 开头的 multipart/report 仍是退信，
// MAILER-DAEMON 发出、带着自动回复信号的来信同样是退信；两条退信规则都成立时报告 multipart_report。
func TestClassifyBounceFirst(t *testing.T) {
	m := humanMessage()
	m.MediaType = "multipart/report"
	m.Subject = "Auto-Reply: 退信"
	m.AutoSubmitted = "auto-replied"
	m.XAutoreply = true
	m.Precedence = "bulk"
	m.ReturnPath = "<>"
	opening := []byte(vacationPhrase)
	if got := Classify(&m, opening, opening); got != (Verdict{Bounce, SignalMultipartReport}) {
		t.Errorf("report with auto-reply signals: %+v", got)
	}
	m.From = "postmaster@example.invalid"
	if got := Classify(&m, nil, nil); got != (Verdict{Bounce, SignalMultipartReport}) {
		t.Errorf("report from postmaster: %+v", got)
	}
	m.MediaType = "text/plain"
	if got := Classify(&m, opening, nil); got != (Verdict{Bounce, SignalDaemonSender}) {
		t.Errorf("postmaster with auto-reply signals: %+v", got)
	}
}

// TestClassifySignalOrder 钉住多个自动回复信号同时成立时报告的信号：按主题前缀、Auto-Submitted、X-Autoreply、Precedence、
// 空的 Return-Path、QQ 假期正文的顺序取第一个。
func TestClassifySignalOrder(t *testing.T) {
	m := humanMessage()
	m.Subject = "自动回复: 标题"
	m.AutoSubmitted = "auto-replied"
	m.XAutoreply = true
	m.Precedence = "bulk"
	m.ReturnPath = "<>"
	opening := []byte(vacationPhrase)
	order := []string{SignalSubjectPrefix, SignalAutoSubmitted, SignalXAutoreply, SignalPrecedence, SignalReturnPathEmpty, SignalQQVacationBody}
	clear := []func(*Message){
		func(m *Message) { m.Subject = "Re: 标题" },
		func(m *Message) { m.AutoSubmitted = "" },
		func(m *Message) { m.XAutoreply = false },
		func(m *Message) { m.Precedence = "" },
		func(m *Message) { m.ReturnPath = "" },
	}
	for i, signal := range order {
		if got := Classify(&m, opening, nil); got != (Verdict{AutoReply, signal}) {
			t.Errorf("step %d: Classify = %+v, want %s", i, got, signal)
		}
		if i < len(clear) {
			clear[i](&m)
		}
	}
}

// TestClassifyHumanLookalikes 确认形状相近而不该命中的来信都判为真人：Auto-Submitted 为 no（大小写与参数不同也一样）、
// Precedence 为其他取值、Return-Path 为真实地址、本地部分只是含有 daemon 或 postmaster、顶层为其他多部件类型，
// 以及正文中间出现「自动回复」而不是 QQ 的固定开头。
func TestClassifyHumanLookalikes(t *testing.T) {
	edits := map[string]func(*Message){
		"auto-submitted no":        func(m *Message) { m.AutoSubmitted = "no" },
		"auto-submitted No":        func(m *Message) { m.AutoSubmitted = "No ; comment" },
		"empty auto-submitted":     func(m *Message) { m.AutoSubmitted = " " },
		"precedence normal":        func(m *Message) { m.Precedence = "normal" },
		"precedence first-class":   func(m *Message) { m.Precedence = "first-class" },
		"return path address":      func(m *Message) { m.ReturnPath = "<user@example.invalid>" },
		"daemon substring":         func(m *Message) { m.From = "mailer-daemon-notes@example.invalid" },
		"postmaster in domain":     func(m *Message) { m.From = "user@postmaster.example.invalid" },
		"no at sign":               func(m *Message) { m.From = "postmaster" },
		"multipart/mixed":          func(m *Message) { m.MediaType = "multipart/mixed" },
		"report-like type":         func(m *Message) { m.MediaType = "multipart/report-draft" },
		"subject mentions it late": func(m *Message) { m.Subject = "Re: [TC " + fixtureTask + "] 请关闭自动回复: 已关" },
	}
	for name, edit := range edits {
		m := humanMessage()
		edit(&m)
		if got := Classify(&m, nil, nil); got != (Verdict{Kind: Human}) {
			t.Errorf("%s: Classify = %+v, want Human", name, got)
		}
	}
	m := humanMessage()
	body := []byte("我已经关掉了自动回复，这是真人的回复。")
	if got := Classify(&m, body, body); got != (Verdict{Kind: Human}) {
		t.Errorf("a body that mentions 自动回复: %+v", got)
	}
}

// TestClassifyPrefixWindow 确认 QQ 假期自动回复的固定开头只在正文的前 PrefixLen 字节中查找：恰好落在窗口末尾时命中，
// 跨过窗口边界或完全在窗口之外时不命中；调用方传入更长的切片时同样只看前 PrefixLen 字节。
func TestClassifyPrefixWindow(t *testing.T) {
	m := humanMessage()
	fill := func(n int) string { return strings.Repeat("a", n) }
	cases := map[string]struct {
		body string
		want Verdict
	}{
		"ends at the window": {fill(PrefixLen-len(vacationPhrase)) + vacationPhrase, Verdict{AutoReply, SignalQQVacationBody}},
		"crosses the window": {fill(PrefixLen-len(vacationPhrase)+1) + vacationPhrase, Verdict{Kind: Human}},
		"beyond the window":  {fill(PrefixLen) + vacationPhrase, Verdict{Kind: Human}},
	}
	for name, tc := range cases {
		for label, args := range map[string][2][]byte{"plain": {[]byte(tc.body), nil}, "html": {nil, []byte(tc.body)}} {
			if got := Classify(&m, args[0], args[1]); got != tc.want {
				t.Errorf("%s in %s: Classify = %+v, want %+v", name, label, got, tc.want)
			}
		}
	}
}

// TestKindString 钉住三种类别的文字形式，供本地事件使用。
func TestKindString(t *testing.T) {
	for kind, want := range map[Kind]string{Human: "human", AutoReply: "auto_reply", Bounce: "bounce", Kind(9): "unknown"} {
		if got := kind.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", int(kind), got, want)
		}
	}
}
