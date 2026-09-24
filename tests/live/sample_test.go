// Package live 的离线自检：用合成邮件字节与合成状态验证样本脱敏与归类、主题前缀白名单、主题标签的构造与归类、
// 自动回复前缀、Return-Path、Message-ID 按相等关系归类、DATA 响应脱敏、IDLE 观测判定、通知渲染、逐封确认文本、
// 样本格式标识与输出目录校验。
// 本文件不带构建标签，随 make test 在 CI 中运行，不读取配置、钥匙串与网络；合成令牌与密钥都在运行时构造，
// 源码中不出现机密形状的字面量。
package live_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime"
	netmail "net/mail"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"

	"github.com/chaoRookie/turncourier/internal/security/token"
	"github.com/chaoRookie/turncourier/tests/live"
)

// 合成的金丝雀值：它们出现在输入中，任何一个出现在样本里都说明脱敏失效。
const (
	canarySentence = "金丝雀句子请勿出现在样本中"
	canaryName     = "张三测试显示名"
	canaryAddress  = "User.Canary@example.invalid"
	canaryDate     = "Wed, 1 Jan 2025 10:11:12 +0800"
	canaryPhone    = "13800138000"
)

// 合成的 Message-ID：只用保留域名，不指向任何真实邮箱。
const (
	oursID     = "<tc.0123456789abcdefghijkl@bot.example.invalid>"
	sentID     = "<tencent_sentcopy@qq.com>"
	ccID       = "<tencent_cccopy@qq.com>"
	dataID     = "<tencent_dataonly@qq.com>"
	unknownID  = "<tencent_unknown@qq.com>"
	stranger   = "<stranger@example.invalid>"
	replySelfI = "<reply.self@example.invalid>"
)

// taskID 是合成的 10 位短任务 ID，与主题标签一致。
const taskID = "0123456789"

// probeID 是合成的探测 ID，出现在 X-TurnCourier-Probe 头中。
const probeID = "probe0000000000a"

// testToken 在运行时签发一个合成令牌并返回其文本；密钥与 nid 都由重复字节与下标构造。
func testToken(t *testing.T) string {
	t.Helper()
	key, err := token.NewKey(1, bytes.Repeat([]byte("k"), token.KeyLen))
	if err != nil {
		t.Fatalf("构造合成密钥失败: %v", err)
	}
	var nid token.NID
	for i := range nid {
		nid[i] = byte(i)
	}
	issued, err := token.Issue(key, nid, token.Claims{TaskID: taskID, Owner: "l1-probe", ExpiresAt: time.UnixMilli(1_790_000_000_000)})
	if err != nil {
		t.Fatalf("签发合成令牌失败: %v", err)
	}
	return issued.Reveal()
}

// testState 返回含一封合成探测邮件的状态；调用方可以改写其中的各来源 ID。
func testState(t *testing.T) live.State {
	t.Helper()
	return live.State{
		Schema: live.Schema,
		Mails: []live.Mail{{
			ProbeID:       probeID,
			TaskID:        taskID,
			MessageID:     oursID,
			Token:         testToken(t),
			DeliveredSent: sentID,
			DeliveredCC:   ccID,
			DeliveredData: []string{dataID},
		}},
	}
}

// subjectTokenState 返回新形态的合成状态：唯一一封探测邮件的主题标签携带令牌（SubjectToken 为 true），
// 与 L1b 起 TestL1SendNotification 写入的记录一致；testState 则对应第一轮留下、没有这个字段的记录。
func subjectTokenState(t *testing.T) live.State {
	t.Helper()
	st := testState(t)
	st.Mails[0].SubjectToken = true
	return st
}

// testRoles 返回合成的地址角色表；全部地址只用保留域名。
func testRoles() live.Roles {
	return live.Roles{
		Bot:       "bot@example.invalid",
		Recipient: "user.canary@example.invalid",
		Allowed:   []string{"user.canary@example.invalid", "other@example.invalid"},
	}
}

// gb18030Base64 把 UTF-8 文本编码为 GB18030 再做 base64，用于构造标注为 gbk 的合成正文。
func gb18030Base64(t *testing.T, text string) string {
	t.Helper()
	encoded, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(text))
	if err != nil {
		t.Fatalf("GB18030 编码失败: %v", err)
	}
	return base64.StdEncoding.EncodeToString(encoded)
}

// encodedSubject 把主题编码为标注 gbk 的 RFC 2047 encoded-word。
func encodedSubject(t *testing.T, subject string) string {
	t.Helper()
	return "=?gbk?B?" + gb18030Base64(t, subject) + "?="
}

// rawMessage 组装一封 multipart/alternative 的合成邮件：两个部件都标注 gbk 并以 base64 传输。
func rawMessage(t *testing.T, headers []string, plain, html string) []byte {
	t.Helper()
	var b strings.Builder
	for _, header := range headers {
		b.WriteString(header + "\r\n")
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/alternative; boundary=\"b1\"\r\n\r\n")
	b.WriteString("--b1\r\nContent-Type: text/plain; charset=\"gbk\"\r\nContent-Transfer-Encoding: base64\r\n\r\n")
	b.WriteString(gb18030Base64(t, plain) + "\r\n")
	b.WriteString("--b1\r\nContent-Type: text/html; charset=\"gbk\"\r\nContent-Transfer-Encoding: base64\r\n\r\n")
	b.WriteString(gb18030Base64(t, html) + "\r\n")
	b.WriteString("--b1--\r\n")
	return []byte(b.String())
}

// replyHeaders 返回合成回复的公共头；extra 追加在后面，用于改写线程头与主题。
func replyHeaders(t *testing.T, extra ...string) []string {
	t.Helper()
	headers := []string{
		"From: \"" + canaryName + "\" <" + canaryAddress + ">",
		"To: \"TurnCourier\" <bot@example.invalid>",
		"Date: " + canaryDate,
		"Message-Id: " + replySelfI,
		"X-Mailer: QQMail 2.x",
		"Received: from mx.example.invalid by mx2.example.invalid; " + canaryDate,
	}
	return append(headers, extra...)
}

// qqPlainBody 返回 QQ A 格式的合成纯文本正文：新正文、引用分隔线、引用头块与页脚令牌。
func qqPlainBody(tokenText string) string {
	return strings.Join([]string{
		canarySentence,
		"",
		"------------------ 原始邮件 ------------------",
		"发件人: \"TurnCourier\" <bot@example.invalid>",
		"发送时间: 2025年1月1日(星期三) 上午10:00",
		"收件人: \"" + canaryName + "\" <" + canaryAddress + ">",
		"主题: [TC " + taskID + "] TurnCourier L1 探测 1/1",
		"",
		"这是 TurnCourier 的真机探测邮件，请用不同客户端直接回复并保留引用。",
		"",
		"回复令牌（请勿删除）：",
		tokenText,
	}, "\n")
}

// qqHTMLBody 返回带 QQ 风格引用标记的合成 HTML 正文。
func qqHTMLBody(tokenText string) string {
	return "<html><body><div>" + canarySentence + "</div>" +
		"<div style=\"font-size: 12px;font-family: Arial Narrow;padding:2px 0 2px 0;\">------------------&nbsp;原始邮件&nbsp;------------------</div>" +
		"<div>" + tokenText + "</div></body></html>"
}

// rawSinglePart 组装一封只有一个部件的合成邮件：Content-Type 与正文都原样写出，用于构造畸形头与只有 HTML 的来信。
func rawSinglePart(t *testing.T, headers []string, contentType, body string) []byte {
	t.Helper()
	var b strings.Builder
	for _, header := range headers {
		b.WriteString(header + "\r\n")
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: " + contentType + "\r\n\r\n")
	b.WriteString(body + "\r\n")
	return []byte(b.String())
}

// analyze 调用被测的 Analyze 并要求样本被接受。
func analyze(t *testing.T, raw []byte, st live.State) live.Sample {
	t.Helper()
	sample, ok, err := live.Analyze(raw, st, testRoles())
	if err != nil {
		t.Fatalf("解析合成邮件失败: %v", err)
	}
	if !ok {
		t.Fatalf("合成邮件应被判为引用了探测邮件")
	}
	return sample
}

// marshal 把样本序列化为一行 JSON，用于断言输出中不含金丝雀。
func marshal(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化样本失败: %v", err)
	}
	return string(data)
}

// TestAnalyzeRedactsAutoReply 用 GB18030 base64 正文、QQ A 格式引用与 Auto-Submitted: auto-replied 的合成自动回复，
// 断言样本正确归类且不含正文、显示名、地址与日期。
func TestAnalyzeRedactsAutoReply(t *testing.T) {
	tokenText := testToken(t)
	raw := rawMessage(t, replyHeaders(t,
		"Subject: "+encodedSubject(t, "回复：[TC "+taskID+"] TurnCourier L1 探测 1/1"),
		"In-Reply-To: "+sentID,
		"References: "+oursID+" "+sentID,
		"Auto-Submitted: auto-replied",
		"Authentication-Results: mx.example.invalid; dkim=pass; spf=pass; dmarc=pass",
	), qqPlainBody(tokenText), qqHTMLBody(tokenText))

	sample := analyze(t, raw, testState(t))
	if sample.Schema != live.Schema {
		t.Errorf("schema = %q，期望 %q", sample.Schema, live.Schema)
	}
	if sample.Kind != "auto" {
		t.Errorf("kind = %q，期望 auto", sample.Kind)
	}
	if sample.FromRole != "recipient" || !sample.FromCaseVariant {
		t.Errorf("from_role = %q、from_case_variant = %v，期望 recipient 与 true", sample.FromRole, sample.FromCaseVariant)
	}
	if sample.Plain.Tokens != 1 || !sample.Plain.TokenMatchesSent {
		t.Errorf("plain 令牌 = %d、匹配 = %v，期望 1 与 true", sample.Plain.Tokens, sample.Plain.TokenMatchesSent)
	}
	if sample.Plain.TokenLinePrefix != "" {
		t.Errorf("token_line_prefix = %q，期望空", sample.Plain.TokenLinePrefix)
	}
	if !slicesContains(sample.Plain.Separators, "qq_a_zh") {
		t.Errorf("separators = %v，期望含 qq_a_zh", sample.Plain.Separators)
	}
	// 骨架按 qqPlainBody 的已知结构逐项对照：连续的同类合并，头块中的每一行单独保留。
	wantSkeleton := []string{"text", "blank", "sep", "header", "header", "header", "header", "blank", "quote_text", "blank", "quote_text"}
	if strings.Join(sample.Plain.Skeleton, ",") != strings.Join(wantSkeleton, ",") {
		t.Errorf("skeleton = %v，期望 %v", sample.Plain.Skeleton, wantSkeleton)
	}
	if sample.HTML.Tokens != 1 || !slicesContains(sample.HTML.Markers, "qq_div_style") {
		t.Errorf("html = %+v，期望 1 个令牌且含 qq_div_style", sample.HTML)
	}
	if sample.Thread.InReplyTo != "delivered_sent" {
		t.Errorf("in_reply_to = %q，期望 delivered_sent", sample.Thread.InReplyTo)
	}
	if sample.Subject.Prefix == nil || *sample.Subject.Prefix != "回复：" || !sample.Subject.TagIntact {
		t.Errorf("subject = %+v，期望前缀「回复：」且标签完整", sample.Subject)
	}
	if sample.Subject.Encoding != "gbk/B" {
		t.Errorf("subject.encoding = %q，期望 gbk/B", sample.Subject.Encoding)
	}
	if sample.Received != 1 {
		t.Errorf("received = %d，期望 1", sample.Received)
	}
	if sample.Auto.AutoSubmitted != "auto-replied" || sample.AuthResults.DKIM != "pass" {
		t.Errorf("auto = %+v、auth_results = %+v，期望 auto-replied 与 dkim=pass", sample.Auto, sample.AuthResults)
	}
	wantMIME := []string{"multipart/alternative", "text/plain", "text/html"}
	gotMIME := []string{sample.MIME[0].Type, sample.MIME[0].Children[0].Type, sample.MIME[0].Children[1].Type}
	for i := range wantMIME {
		if gotMIME[i] != wantMIME[i] {
			t.Errorf("mime = %v，期望 %v", gotMIME, wantMIME)
			break
		}
	}
	if sample.MIME[0].Children[0].Charset != "gbk" || sample.MIME[0].Children[0].Transfer != "base64" {
		t.Errorf("text/plain 部件 = %+v，期望 charset gbk、transfer base64", sample.MIME[0].Children[0])
	}
	output := marshal(t, sample)
	for _, canary := range []string{canarySentence, canaryName, canaryAddress, strings.ToLower(canaryAddress), canaryDate, "10:11:12", tokenText} {
		if strings.Contains(output, canary) {
			t.Errorf("样本中出现了金丝雀 %q", canary)
		}
	}
}

// slicesContains 判断字符串切片是否含某个值。
func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestAnalyzeClassifiesThreadIDs 断言线程头按相等关系归类：已发送副本、同时等于 DATA 候选、形似 QQ 的未知 ID 与缺失。
func TestAnalyzeClassifiesThreadIDs(t *testing.T) {
	tokenText := testToken(t)
	cases := []struct {
		name     string
		inReply  string
		mutate   func(*live.State)
		want     string
		wantRefs []string
	}{
		{name: "已发送副本", inReply: sentID, want: "delivered_sent", wantRefs: []string{"ours", "delivered_sent"}},
		{
			name:    "同时等于 DATA 候选",
			inReply: sentID,
			mutate:  func(st *live.State) { st.Mails[0].DeliveredData = []string{sentID} },
			want:    "delivered_sent+delivered_data", wantRefs: []string{"ours", "delivered_sent+delivered_data"},
		},
		{name: "未知的腾讯 ID", inReply: unknownID, want: "other_tencent", wantRefs: []string{"ours", "delivered_sent"}},
		{name: "其他 ID", inReply: stranger, want: "other", wantRefs: []string{"ours", "delivered_sent"}},
		{name: "只有 DATA 候选", inReply: dataID, want: "delivered_data", wantRefs: []string{"ours", "delivered_sent"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := testState(t)
			if c.mutate != nil {
				c.mutate(&st)
			}
			raw := rawMessage(t, replyHeaders(t,
				"Subject: "+encodedSubject(t, "回复：[TC "+taskID+"] 探测"),
				"In-Reply-To: "+c.inReply,
				"References: "+oursID+" "+sentID,
			), qqPlainBody(tokenText), qqHTMLBody(tokenText))
			sample, ok, err := live.Analyze(raw, st, testRoles())
			if err != nil || !ok {
				t.Fatalf("解析失败 ok=%v err=%v", ok, err)
			}
			if sample.Thread.InReplyTo != c.want {
				t.Errorf("in_reply_to = %q，期望 %q", sample.Thread.InReplyTo, c.want)
			}
			if strings.Join(sample.Thread.References, ",") != strings.Join(c.wantRefs, ",") {
				t.Errorf("references = %v，期望 %v", sample.Thread.References, c.wantRefs)
			}
		})
	}
	t.Run("缺失线程头", func(t *testing.T) {
		raw := rawMessage(t, replyHeaders(t, "Subject: "+encodedSubject(t, "回复：[TC "+taskID+"] 探测")),
			qqPlainBody(tokenText), qqHTMLBody(tokenText))
		sample := analyze(t, raw, testState(t))
		if sample.Thread.InReplyTo != "missing" || len(sample.Thread.References) != 0 {
			t.Errorf("thread = %+v，期望 in_reply_to 为 missing、references 为空", sample.Thread)
		}
	})
}

// TestAnalyzeSubjects 断言主题前缀白名单：已知前缀按 ASCII 不区分大小写匹配、输出来信中的原文（先删去空白），
// 其他文字记为 other，没有标签时为 null；[TC 本身同样按 ASCII 不区分大小写查找。各情况都不输出主题中的其他文字。
// 回复与转发前缀逐项各有一例（英文前缀也接受全角冒号），删去白名单中的任一项都会让对应用例失败；自动回复前缀的逐项用例
// 在 TestAnalyzeAutoReplySubject 中。状态取第一轮的旧形态（SubjectToken 为 false），期望标签为 [TC <任务 ID>]。
func TestAnalyzeSubjects(t *testing.T) {
	tokenText := testToken(t)
	// subjectCase 是一个主题前缀用例：解码后的主题、期望的前缀（nil 表示 null）与标签是否完整。
	type subjectCase struct {
		name      string
		subject   string
		want      *string
		tagIntact bool
	}
	cases := []subjectCase{
		{name: "已知前缀", subject: "回复：[TC " + taskID + "] 探测", want: stringPtr("回复："), tagIntact: true},
		{name: "重复前缀", subject: "答复: Re:[TC " + taskID + "] 探测", want: stringPtr("答复:Re:"), tagIntact: true},
		{name: "大小写不同的前缀", subject: "RE: fwd: [TC " + taskID + "] 探测", want: stringPtr("RE:fwd:"), tagIntact: true},
		{name: "重复的全角冒号英文前缀", subject: "FW：Fwd： Re：[TC " + taskID + "] 探测", want: stringPtr("FW：Fwd：Re："), tagIntact: true},
		{name: "带空格的自动回复前缀", subject: "Automatic reply: [TC " + taskID + "] 探测", want: stringPtr("Automaticreply:"), tagIntact: true},
		{name: "不在白名单的英文前缀", subject: "Reply: [TC " + taskID + "] 探测", want: stringPtr("other"), tagIntact: true},
		{name: "标签中的 TC 为小写", subject: "回复：[tc " + taskID + "] 探测", want: stringPtr("回复："), tagIntact: false},
		{name: "其他文字", subject: "关于合同 " + canaryPhone + " [TC " + taskID + "]", want: stringPtr("other"), tagIntact: true},
		{name: "没有标签", subject: "关于合同 " + canaryPhone, want: nil, tagIntact: false},
	}
	for _, prefix := range []string{"回复：", "回复:", "答复：", "答复:", "转发：", "转发:", "Re:", "Re：", "Fwd:", "Fwd：", "FW:", "FW："} {
		cases = append(cases, subjectCase{name: "白名单 " + prefix, subject: prefix + " [TC " + taskID + "] 探测", want: stringPtr(prefix), tagIntact: true})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := rawMessage(t, replyHeaders(t, "Subject: "+encodedSubject(t, c.subject), "In-Reply-To: "+sentID),
				qqPlainBody(tokenText), qqHTMLBody(tokenText))
			sample := analyze(t, raw, testState(t))
			switch {
			case c.want == nil && sample.Subject.Prefix != nil:
				t.Errorf("prefix = %q，期望 null", *sample.Subject.Prefix)
			case c.want != nil && sample.Subject.Prefix == nil:
				t.Errorf("prefix = null，期望 %q", *c.want)
			case c.want != nil && *sample.Subject.Prefix != *c.want:
				t.Errorf("prefix = %q，期望 %q", *sample.Subject.Prefix, *c.want)
			}
			if sample.Subject.TagIntact != c.tagIntact {
				t.Errorf("tag_intact = %v，期望 %v", sample.Subject.TagIntact, c.tagIntact)
			}
			if output := marshal(t, sample); strings.Contains(output, canaryPhone) {
				t.Errorf("样本中出现了主题里的数字 %q", canaryPhone)
			}
		})
	}
}

// stringPtr 返回指向字符串的指针，用于表达「有值」的期望。
func stringPtr(s string) *string {
	return &s
}

// describePrefix 把样本中的主题前缀渲染为便于阅读的文字：null 或带引号的取值。
func describePrefix(prefix *string) string {
	if prefix == nil {
		return "null"
	}
	return strconv.Quote(*prefix)
}

// TestAnalyzeTagStates 断言主题标签的七种状态按清单顺序取第一个成立的值，并记录 D4 严格文法不重叠命中的次数（0 到 3 次都有）。
// 期望标签按 state.json 中每封邮件的 SubjectToken 取新形态或旧形态，并与全部已记录的邮件逐一比较；
// tag_intact 当且仅当 tag_state 为 intact。标签之前另有一个以 [tc 开头的诱饵（[tcp]）时，各步仍要检查主题中的每一个 [tc。
// 截断只认两种分岔：主题在此结束，或分岔处的字符不是 ]、字母、数字且期望标签没有在它之后接续；
// 插入零宽空格、软连字符或 -，以及把一个字符换成 o 或替换字符，都是改写，归入 other。
// 每个用例的主题都含金丝雀号码，多数还含完整或部分令牌；样本中既不出现金丝雀号码，也不出现令牌的前 20 个字符。
func TestAnalyzeTagStates(t *testing.T) {
	tokenText := testToken(t)
	tag := live.SubjectTag(taskID, tokenText)
	// lookalike 与 lookalike2 形状合法（严格文法能命中），但令牌与记录的不同。
	lookalike := live.SubjectTag(taskID, strings.Repeat("z", 48))
	lookalike2 := live.SubjectTag(taskID, strings.Repeat("y", 48))
	title := " TurnCourier L1 探测 1/1 " + canaryPhone
	// encodedTitle 是 4b 渲染器的结构：标签为原始 ASCII，标签之后的文字单独 B 编码。
	encodedTitle := mime.BEncoding.Encode("utf-8", strings.TrimSpace(title))
	// substitute 是与令牌第 31 个字符不同的字母表字符，用来构造「在截断处被改写」而不是截断的主题。
	substitute := "0"
	if tokenText[30] == '0' {
		substitute = "1"
	}
	const oldTaskID = "abcdefghjk"
	oldForm := func(st *live.State) { st.Mails[0].SubjectToken = false }
	secondOldMail := func(st *live.State) {
		st.Mails = append(st.Mails, live.Mail{
			ProbeID: probeID, TaskID: oldTaskID, MessageID: "<tc.old@bot.example.invalid>", DeliveredData: []string{},
		})
	}
	// noTaskMail 追加一条没有任务 ID 的记录：它不产生期望标签，[TC ] 这样的残片不能因它变成 intact。
	noTaskMail := func(st *live.State) {
		st.Mails = append(st.Mails, live.Mail{ProbeID: probeID, MessageID: "<tc.none@bot.example.invalid>", DeliveredData: []string{}})
	}
	for _, c := range []struct {
		name       string
		header     string // Subject 头的原始取值
		mutate     func(*live.State)
		wantState  string
		wantCount  int
		wantPrefix *string
	}{
		{name: "没有标签", header: encodedSubject(t, "关于合同 "+canaryPhone+" "+tokenText), wantState: "missing"},
		{name: "两个标签", header: encodedSubject(t, "Re: "+tag+" 转发："+lookalike+title),
			wantState: "multiple", wantCount: 2, wantPrefix: stringPtr("Re:")},
		{name: "三个标签", header: encodedSubject(t, "Re: "+tag+" "+lookalike+" "+lookalike2+title),
			wantState: "multiple", wantCount: 3, wantPrefix: stringPtr("Re:")},
		{name: "原样保留", header: encodedSubject(t, "回复："+tag+title),
			wantState: "intact", wantCount: 1, wantPrefix: stringPtr("回复：")},
		{name: "诱饵之后原样保留", header: encodedSubject(t, "Re: [tcp] "+tag+title),
			wantState: "intact", wantCount: 1, wantPrefix: stringPtr("Re:")},
		// U+023A 经 strings.ToLower 变成 U+2C65，多出一个字节；个数超过标签之后的字节数时，按 strings.ToLower 的结果求出的
		// [tc 下标会越过主题末尾。前缀的下标必须按逐字节对齐的 lowerASCII 求，结果为 other 且不会 panic。
		{name: "前缀含小写后变长的字符", header: encodedSubject(t, canaryPhone+strings.Repeat("\u023a", len(tag)+1)+tag),
			wantState: "intact", wantCount: 1, wantPrefix: stringPtr("other")},
		{name: "标签为原始 ASCII、在原有空格处折行", header: "Re: [TC " + taskID + "\r\n " + tokenText + "] " + encodedTitle,
			wantState: "intact", wantCount: 1, wantPrefix: stringPtr("Re:")},
		{name: "大写的相似标签不计数", header: encodedSubject(t, "回复："+tag+" "+strings.ToUpper(lookalike)+title),
			wantState: "intact", wantCount: 1, wantPrefix: stringPtr("回复：")},
		{name: "旧形态", header: encodedSubject(t, "Re: "+live.SubjectTag(taskID, "")+title), mutate: oldForm,
			wantState: "intact", wantPrefix: stringPtr("Re:")},
		{name: "第二封邮件的旧形态", header: encodedSubject(t, "Re: "+live.SubjectTag(oldTaskID, "")+title), mutate: secondOldMail,
			wantState: "intact", wantPrefix: stringPtr("Re:")},
		{name: "令牌改为大写", header: encodedSubject(t, "回复："+live.SubjectTag(taskID, strings.ToUpper(tokenText))+title),
			wantState: "case_changed", wantPrefix: stringPtr("回复：")},
		{name: "TC 改为小写", header: encodedSubject(t, "回复：[tc "+taskID+" "+tokenText+"]"+title),
			wantState: "case_changed", wantPrefix: stringPtr("回复：")},
		{name: "诱饵之后大小写改变", header: encodedSubject(t, "Re: [tcp] [tc "+taskID+" "+strings.ToUpper(tokenText)+"]"+title),
			wantState: "case_changed", wantPrefix: stringPtr("Re:")},
		{name: "令牌中插入空格", header: encodedSubject(t, "回复：[TC "+taskID+" "+tokenText[:20]+" "+tokenText[20:]+"]"+title),
			wantState: "whitespace_changed", wantPrefix: stringPtr("回复：")},
		{name: "折行插入空白", header: "Re: [TC " + taskID + " " + tokenText[:30] + "\r\n " + tokenText[30:] + "] " + encodedTitle,
			wantState: "whitespace_changed", wantPrefix: stringPtr("Re:")},
		{name: "分隔换成全角空格", header: encodedSubject(t, "回复：[TC "+taskID+"　"+tokenText+"]"+title),
			wantState: "whitespace_changed", wantPrefix: stringPtr("回复：")},
		{name: "分隔被删去且大小写改变", header: encodedSubject(t, "回复：[tc "+taskID+strings.ToUpper(tokenText)+"]"+title),
			wantState: "whitespace_changed", wantPrefix: stringPtr("回复：")},
		// 严格文法要求任务 ID 两侧各恰好一个 U+0020：多一个空格或换成制表符都不计数。
		{name: "任务 ID 与令牌之间两个空格", header: encodedSubject(t, "回复：[TC "+taskID+"  "+tokenText+"]"+title),
			wantState: "whitespace_changed", wantPrefix: stringPtr("回复：")},
		{name: "任务 ID 与令牌之间是制表符", header: encodedSubject(t, "回复：[TC "+taskID+"\t"+tokenText+"]"+title),
			wantState: "whitespace_changed", wantPrefix: stringPtr("回复：")},
		{name: "TC 与任务 ID 之间是制表符", header: encodedSubject(t, "回复：[TC\t"+taskID+" "+tokenText+"]"+title),
			wantState: "whitespace_changed", wantPrefix: stringPtr("回复：")},
		{name: "截断后接省略号", header: encodedSubject(t, "回复：[TC "+taskID+" "+tokenText[:30]+"…"+title),
			wantState: "truncated", wantPrefix: stringPtr("回复：")},
		{name: "截断在主题末尾", header: encodedSubject(t, "关于 "+canaryPhone+" [TC "+taskID+" "+tokenText[:20]),
			wantState: "truncated", wantPrefix: stringPtr("other")},
		{name: "只缺右方括号且主题在此结束", header: encodedSubject(t, "关于 "+canaryPhone+" [TC "+taskID+" "+tokenText),
			wantState: "truncated", wantPrefix: stringPtr("other")},
		{name: "只剩任务 ID", header: encodedSubject(t, "回复：[TC "+taskID+"…"+title),
			wantState: "truncated", wantPrefix: stringPtr("回复：")},
		{name: "诱饵之后被截断", header: encodedSubject(t, "Re: [tcp] [TC "+taskID+" "+tokenText[:20]+"…"+title),
			wantState: "truncated", wantPrefix: stringPtr("Re:")},
		{name: "令牌与记录不同", header: encodedSubject(t, "回复："+lookalike+title),
			wantState: "other", wantCount: 1, wantPrefix: stringPtr("回复：")},
		{name: "新形态的记录只对上旧形态标签", header: encodedSubject(t, "回复："+live.SubjectTag(taskID, "")+title),
			wantState: "other", wantPrefix: stringPtr("回复：")},
		{name: "在截断处被改写", header: encodedSubject(t, "回复：[TC "+taskID+" "+tokenText[:30]+substitute+title),
			wantState: "other", wantPrefix: stringPtr("回复：")},
		{name: "标签提前闭合", header: encodedSubject(t, "回复：[TC "+taskID+" "+tokenText[:30]+"]"+title),
			wantState: "other", wantPrefix: stringPtr("回复：")},
		// 下面几例的分岔处都不是截断：标签在插入或替换的字符之后照样接续，或分岔处是字母表之外的字母（i、l、o、u）。
		// 零宽空格与软连字符不是 unicode.IsSpace 意义上的空白，删去空白后仍在，分岔处要按完整的 UTF-8 字符跳过。
		{name: "令牌中插入零宽空格", header: encodedSubject(t, "回复：[TC "+taskID+" "+tokenText[:30]+"\u200b"+tokenText[30:]+"]"+title),
			wantState: "other", wantPrefix: stringPtr("回复：")},
		{name: "令牌中插入软连字符", header: encodedSubject(t, "回复：[TC "+taskID+" "+tokenText[:30]+"\u00ad"+tokenText[30:]+"]"+title),
			wantState: "other", wantPrefix: stringPtr("回复：")},
		{name: "令牌中插入连字符", header: encodedSubject(t, "回复：[TC "+taskID+" "+tokenText[:30]+"-"+tokenText[30:]+"]"+title),
			wantState: "other", wantPrefix: stringPtr("回复：")},
		{name: "令牌的一个字符换成 o", header: encodedSubject(t, "回复：[TC "+taskID+" "+tokenText[:30]+"o"+tokenText[31:]+"]"+title),
			wantState: "other", wantPrefix: stringPtr("回复：")},
		{name: "令牌的一个字符换成替换字符", header: encodedSubject(t, "回复：[TC "+taskID+" "+tokenText[:30]+"\ufffd"+tokenText[31:]+"]"+title),
			wantState: "other", wantPrefix: stringPtr("回复：")},
		{name: "分岔处是字母表之外的字母", header: encodedSubject(t, "回复：[TC "+taskID+" "+tokenText[:30]+"o"+title),
			wantState: "other", wantPrefix: stringPtr("回复：")},
		// 令牌完整、只是 ] 被省略号取代：这正是客户端恰好在 ] 之前截断主题的样子，替换检查在期望标签的最后一个字节处不进行，记为 truncated。
		{name: "令牌完整而右方括号换成省略号", header: encodedSubject(t, "回复：[TC "+taskID+" "+tokenText+"…"+title),
			wantState: "truncated", wantPrefix: stringPtr("回复：")},
		{name: "没有任务 ID 的记录", header: encodedSubject(t, "回复：[TC ]"+title), mutate: noTaskMail,
			wantState: "other", wantPrefix: stringPtr("回复：")},
		// 开尔文符号 U+212A 经 strings.ToLower 会变成 k；大小写只折叠 ASCII，它不算大小写变化。
		{name: "开尔文符号不算大小写变化", header: encodedSubject(t, "Re: [TC abcdefghjK]"+title), mutate: secondOldMail,
			wantState: "other", wantPrefix: stringPtr("Re:")},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := subjectTokenState(t)
			if c.mutate != nil {
				c.mutate(&st)
			}
			raw := rawMessage(t, replyHeaders(t, "Subject: "+c.header, "In-Reply-To: "+sentID),
				qqPlainBody(tokenText), qqHTMLBody(tokenText))
			got := analyze(t, raw, st)
			if got.Subject.TagState != c.wantState || got.Subject.TagCount != c.wantCount {
				t.Errorf("tag_state = %q、tag_count = %d，期望 %q 与 %d",
					got.Subject.TagState, got.Subject.TagCount, c.wantState, c.wantCount)
			}
			if got.Subject.TagIntact != (c.wantState == "intact") {
				t.Errorf("tag_intact = %v，与期望的 tag_state %q 不一致", got.Subject.TagIntact, c.wantState)
			}
			if describePrefix(got.Subject.Prefix) != describePrefix(c.wantPrefix) {
				t.Errorf("prefix = %s，期望 %s", describePrefix(got.Subject.Prefix), describePrefix(c.wantPrefix))
			}
			if output := strings.ToLower(marshal(t, got)); strings.Contains(output, canaryPhone) || strings.Contains(output, tokenText[:20]) {
				t.Errorf("样本中出现了主题里的金丝雀号码或令牌")
			}
		})
	}
}

// TestAnalyzeAutoReplySubject 断言主题的自动回复前缀：QQ 的假期自动回复只有 text/html、不带任何头部信号，
// 主题以自动回复前缀开头是唯一的判据，kind 因此为 auto；冒号是全角还是半角原样记入 prefix，大小写与空白不影响命中。
// 前缀表逐项各有一例（六个词干各配半角与全角冒号），删去任一项都会让对应用例失败。
// 这一信号只看解码后的主题开头，与白名单输出的 prefix 无关：前缀之后是其他文字（prefix 为 other）或主题中根本没有标签
// （prefix 为 null）时照样命中。以「回复：」开头的真人回复不命中，即使其后出现自动回复字样；样本中不出现正文、令牌与金丝雀号码。
func TestAnalyzeAutoReplySubject(t *testing.T) {
	tokenText := testToken(t)
	tag := live.SubjectTag(taskID, tokenText)
	title := " TurnCourier L1 探测 1/1"
	// autoReplyCase 是一个自动回复用例：解码后的主题、是否只有 text/html，以及期望的 kind、subject_prefix、prefix（nil 表示 null）
	// 与 tag_state。
	type autoReplyCase struct {
		name       string
		subject    string
		htmlOnly   bool
		wantKind   string
		wantSignal bool
		wantPrefix *string
		wantState  string
	}
	cases := []autoReplyCase{
		{name: "大小写不同", subject: "AUTO-REPLY: " + tag + title, htmlOnly: true,
			wantKind: "auto", wantSignal: true, wantPrefix: stringPtr("AUTO-REPLY:"), wantState: "intact"},
		{name: "带空格的英文前缀", subject: "Automatic reply: " + tag + title, htmlOnly: true,
			wantKind: "auto", wantSignal: true, wantPrefix: stringPtr("Automaticreply:"), wantState: "intact"},
		{name: "带空格的英文前缀、全角冒号", subject: "Automatic Reply：" + tag + title, htmlOnly: true,
			wantKind: "auto", wantSignal: true, wantPrefix: stringPtr("AutomaticReply："), wantState: "intact"},
		{name: "前缀之后是其他文字", subject: "自动回复: 关于合同 " + canaryPhone + " " + tag, htmlOnly: true,
			wantKind: "auto", wantSignal: true, wantPrefix: stringPtr("other"), wantState: "intact"},
		{name: "没有主题标签", subject: "自动回复：您好 " + canaryPhone, htmlOnly: true,
			wantKind: "auto", wantSignal: true, wantPrefix: nil, wantState: "missing"},
		{name: "真人回复", subject: "回复：" + tag + title,
			wantKind: "reply", wantPrefix: stringPtr("回复："), wantState: "intact"},
		{name: "自动回复字样不在开头", subject: "回复：自动回复: " + tag + title,
			wantKind: "reply", wantPrefix: stringPtr("回复：自动回复:"), wantState: "intact"},
	}
	for _, prefix := range []string{
		"自动回复：", "自动回复:", "自動回覆：", "自動回覆:", "自动答复：", "自动答复:",
		"Auto-Reply:", "Auto-Reply：", "AutoReply:", "AutoReply：", "AutomaticReply:", "AutomaticReply：",
	} {
		cases = append(cases, autoReplyCase{name: "前缀 " + prefix + "，只有 HTML", subject: prefix + " " + tag + title, htmlOnly: true,
			wantKind: "auto", wantSignal: true, wantPrefix: stringPtr(prefix), wantState: "intact"})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			headers := replyHeaders(t, "Subject: "+encodedSubject(t, c.subject), "In-Reply-To: "+sentID)
			raw := rawMessage(t, headers, qqPlainBody(tokenText), qqHTMLBody(tokenText))
			if c.htmlOnly {
				raw = rawSinglePart(t, headers, `text/html; charset="utf-8"`, "<html><body>"+canarySentence+"</body></html>")
			}
			sample := analyze(t, raw, subjectTokenState(t))
			if sample.Kind != c.wantKind || sample.Auto.SubjectPrefix != c.wantSignal {
				t.Errorf("kind = %q、auto.subject_prefix = %v，期望 %q 与 %v", sample.Kind, sample.Auto.SubjectPrefix, c.wantKind, c.wantSignal)
			}
			if describePrefix(sample.Subject.Prefix) != describePrefix(c.wantPrefix) {
				t.Errorf("prefix = %s，期望 %s", describePrefix(sample.Subject.Prefix), describePrefix(c.wantPrefix))
			}
			if sample.Subject.TagState != c.wantState {
				t.Errorf("tag_state = %q，期望 %q", sample.Subject.TagState, c.wantState)
			}
			output := strings.ToLower(marshal(t, sample))
			if strings.Contains(output, canarySentence) || strings.Contains(output, tokenText) || strings.Contains(output, canaryPhone) {
				t.Errorf("样本中出现了正文、令牌或主题里的金丝雀号码")
			}
		})
	}
}

// TestAnalyzeBounceBeforeAutoReplySubject 断言退信判定先于自动回复信号：主题以自动回复前缀开头的 multipart/report 仍记为 bounce，
// auto.subject_prefix 照实记为 true。发件人不是 MAILER-DAEMON 或 postmaster，退信只凭 multipart/report 这一条依据成立。
func TestAnalyzeBounceBeforeAutoReplySubject(t *testing.T) {
	headers := []string{
		"From: <" + canaryAddress + ">",
		"To: <bot@example.invalid>",
		"Subject: " + encodedSubject(t, "Auto-Reply: "+live.SubjectTag(taskID, testToken(t))+" 探测"),
		"Message-Id: " + replySelfI,
		"In-Reply-To: " + sentID,
	}
	sample := analyze(t, rawReport(t, headers), subjectTokenState(t))
	if sample.Kind != "bounce" || !sample.Auto.SubjectPrefix {
		t.Errorf("kind = %q、auto.subject_prefix = %v，期望 bounce 与 true", sample.Kind, sample.Auto.SubjectPrefix)
	}
}

// TestAnalyzeReturnPath 断言 Return-Path 的脱敏描述：头的个数、第一个取值是否为空信封、第一个地址的角色（规则同 from_role），
// 以及它与 From 规范化后是否相同（不区分大小写）、域名是否相同；任一地址缺失或无法解析时两项对照都为 false。
// empty 与 role 只看第一个 Return-Path 头；matches_from 比较地址本身，两个不同的地址角色相同（都是 other）也不算相同；
// 域名取最后一个 @ 之后的部分，带引号的本地部分可以含 @。
// 样本中只有角色与布尔值，不出现任何地址。From 默认为 canaryAddress，角色为 recipient。
func TestAnalyzeReturnPath(t *testing.T) {
	tokenText := testToken(t)
	for _, c := range []struct {
		name  string
		from  string // 非空时替换 replyHeaders 中的 From 头
		extra []string
		want  live.ReturnPath
	}{
		{name: "没有该头", want: live.ReturnPath{}},
		{name: "空信封", extra: []string{"Return-Path: <>"}, want: live.ReturnPath{Count: 1, Empty: true}},
		{name: "与 From 相同", extra: []string{"Return-Path: <" + canaryAddress + ">"},
			want: live.ReturnPath{Count: 1, Role: "recipient", MatchesFrom: true, SameDomainAsFrom: true}},
		{name: "与 From 只差大小写", extra: []string{"Return-Path: <user.canary@EXAMPLE.INVALID>"},
			want: live.ReturnPath{Count: 1, Role: "recipient", MatchesFrom: true, SameDomainAsFrom: true}},
		{name: "同域不同地址", extra: []string{"Return-Path: <bounce.canary@Example.Invalid>"},
			want: live.ReturnPath{Count: 1, Role: "other", SameDomainAsFrom: true}},
		{name: "异域地址", extra: []string{"Return-Path: <user.canary@elsewhere.invalid>"},
			want: live.ReturnPath{Count: 1, Role: "other"}},
		{name: "无法解析", extra: []string{"Return-Path: 关于合同 " + canaryPhone},
			want: live.ReturnPath{Count: 1, Role: "other"}},
		{name: "两个头且第一个为空信封", extra: []string{"Return-Path: <>", "Return-Path: <" + canaryAddress + ">"},
			want: live.ReturnPath{Count: 2, Empty: true}},
		{name: "两个头且第二个为空信封", extra: []string{"Return-Path: <" + canaryAddress + ">", "Return-Path: <>"},
			want: live.ReturnPath{Count: 2, Role: "recipient", MatchesFrom: true, SameDomainAsFrom: true}},
		{name: "两个不同的地址角色都是 other", from: "<first.canary@example.invalid>", extra: []string{"Return-Path: <second.canary@example.invalid>"},
			want: live.ReturnPath{Count: 1, Role: "other", SameDomainAsFrom: true}},
		{name: "带引号的本地部分含 @", extra: []string{`Return-Path: <"bounce@canary"@example.invalid>`},
			want: live.ReturnPath{Count: 1, Role: "other", SameDomainAsFrom: true}},
		{name: "From 无法解析", from: canaryName, extra: []string{"Return-Path: <" + canaryAddress + ">"},
			want: live.ReturnPath{Count: 1, Role: "recipient"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			headers := replyHeaders(t, append([]string{
				"Subject: " + encodedSubject(t, "回复："+live.SubjectTag(taskID, tokenText)+" 探测"),
				"In-Reply-To: " + sentID,
			}, c.extra...)...)
			if c.from != "" {
				headers[0] = "From: " + c.from
			}
			raw := rawMessage(t, headers, qqPlainBody(tokenText), qqHTMLBody(tokenText))
			sample := analyze(t, raw, subjectTokenState(t))
			if sample.ReturnPath != c.want {
				t.Errorf("return_path = %+v，期望 %+v", sample.ReturnPath, c.want)
			}
			if sample.Auto.ReturnPathEmpty != c.want.Empty {
				t.Errorf("auto.return_path_empty = %v，与 return_path.empty 不一致", sample.Auto.ReturnPathEmpty)
			}
			output := strings.ToLower(marshal(t, sample))
			for _, canary := range []string{"example.invalid", "elsewhere.invalid", "canary", canaryPhone} {
				if strings.Contains(output, canary) {
					t.Errorf("样本中出现了地址或头部文字 %q", canary)
				}
			}
		})
	}
}

// TestRedactDataResponse 断言 DATA 响应按相等关系脱敏、候选 ID 按出现顺序去重，且候选可用于归类线程头。
func TestRedactDataResponse(t *testing.T) {
	st := testState(t)
	st.Mails[0].DeliveredData = nil
	bare := "tencent_barecandidate@qq.com"
	text := "OK: queued as " + oursID + " copy " + ccID + " id " + bare + " by postmaster@qq.com again " + oursID
	redacted, ids := live.RedactDataResponse(text, st.Mails)
	want := "OK: queued as <ours> copy <delivered_cc> id <other_tencent> by <addr> again <ours>"
	if redacted != want {
		t.Errorf("脱敏结果 = %q，期望 %q", redacted, want)
	}
	wantIDs := []string{oursID, ccID, "<" + bare + ">"}
	if strings.Join(ids, ",") != strings.Join(wantIDs, ",") {
		t.Errorf("候选 = %v，期望 %v", ids, wantIDs)
	}

	st.Mails[0].DeliveredData = ids
	tokenText := st.Mails[0].Token
	raw := rawMessage(t, replyHeaders(t, "Subject: "+encodedSubject(t, "回复：[TC "+taskID+"] 探测"), "In-Reply-To: "+ids[2]),
		qqPlainBody(tokenText), qqHTMLBody(tokenText))
	sample := analyze(t, raw, st)
	if sample.Thread.InReplyTo != "delivered_data" {
		t.Errorf("in_reply_to = %q，期望 delivered_data", sample.Thread.InReplyTo)
	}
}

// TestSubjectTag 断言主题标签的两种形态：带令牌时为 [TC <任务 ID> <令牌>]，共 64 个字符（加上「Subject: 」为 73 列，
// 不到 go-message 折行的 76 列）；令牌为空时为旧形态 [TC <任务 ID>]，只用于 IDLE 自发邮件。
func TestSubjectTag(t *testing.T) {
	tokenText := testToken(t)
	tag := live.SubjectTag(taskID, tokenText)
	if tag != "[TC "+taskID+" "+tokenText+"]" {
		t.Errorf("带令牌的标签形态不对（%d 个字符）", len(tag))
	}
	if len(tag) != 64 {
		t.Errorf("带令牌的标签长度 = %d，期望 64", len(tag))
	}
	if got, want := live.SubjectTag(taskID, ""), "[TC "+taskID+"]"; got != want {
		t.Errorf("旧形态标签 = %q，期望 %q", got, want)
	}
}

// TestSchemaVersion 固定样本与状态文件的格式标识：L1b 的样本（主题标签携带令牌，另有 tag_state、tag_count、
// subject_prefix 与 return_path）标为 turncourier-l1/4，读样本时据此与第一轮区分。
func TestSchemaVersion(t *testing.T) {
	if live.Schema != "turncourier-l1/4" {
		t.Errorf("Schema = %q，期望 turncourier-l1/4", live.Schema)
	}
}

// testNotification 返回一封合成通知：地址只用保留域名，主题由调用方给出的标签与标题组成，页脚含 tokenText。
func testNotification(tag, title, tokenText string) live.Notification {
	return live.Notification{
		From:      "bot@example.invalid",
		To:        "user.canary@example.invalid",
		CC:        "bot@example.invalid",
		Tag:       tag,
		Title:     title,
		MessageID: oursID,
		ProbeID:   probeID,
		Token:     tokenText,
		Date:      time.UnixMilli(1_789_000_000_000).UTC(),
	}
}

// subjectField 返回通知头部中 Subject 字段的全部物理行（首行与折行后的续行），以及用标准库独立解码的主题：
// net/mail 展开折行，mime.WordDecoder 解码 encoded-word，与探测工具所用的 go-message 互不依赖。
func subjectField(t *testing.T, raw []byte) ([]string, string) {
	t.Helper()
	header, _, ok := strings.Cut(string(raw), "\r\n\r\n")
	if !ok {
		t.Fatalf("通知中没有头部与正文之间的空行")
	}
	var lines []string
	fields, inSubject := 0, false
	for _, line := range strings.Split(header, "\r\n") {
		switch {
		case strings.HasPrefix(line, "Subject: "):
			fields, inSubject = fields+1, true
			lines = append(lines, line)
		case inSubject && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")):
			lines = append(lines, line)
		default:
			inSubject = false
		}
	}
	if fields != 1 {
		t.Fatalf("通知中有 %d 个 Subject 头，期望 1 个", fields)
	}
	message, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("标准库无法解析通知: %v", err)
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(message.Header.Get("Subject"))
	if err != nil {
		t.Fatalf("标准库无法解码主题: %v", err)
	}
	return lines, decoded
}

// TestComposeNotification 断言合成通知可被解析：含探测 ID、我方 Message-ID、Auto-Submitted 与页脚令牌，
// 两个部件分别为 text/plain 与 text/html；主题头的首行恰为「Subject: 」加完整的标签（标签之后才折行），
// 解码后的主题等于「标签 + 空格 + 标题」。
func TestComposeNotification(t *testing.T) {
	tokenText := testToken(t)
	tag := live.SubjectTag(taskID, tokenText)
	title := "TurnCourier L1 探测 1/1"
	raw, err := live.ComposeNotification(testNotification(tag, title, tokenText))
	if err != nil {
		t.Fatalf("渲染通知失败: %v", err)
	}
	copyInfo, err := live.CopyHeaders(raw)
	if err != nil {
		t.Fatalf("读取通知头失败: %v", err)
	}
	if copyInfo.ProbeID != probeID || copyInfo.MessageID != oursID {
		t.Errorf("通知头 = %+v，期望探测 ID %q 与 Message-ID %q", copyInfo, probeID, oursID)
	}
	text := string(raw)
	for _, want := range []string{"Auto-Submitted: auto-generated", "text/plain", "text/html", "multipart/alternative"} {
		if !strings.Contains(text, want) {
			t.Errorf("通知中缺少 %q", want)
		}
	}
	// 主题标签里也有令牌，页脚中的那一份只能在正文里找。
	if _, body, _ := strings.Cut(text, "\r\n\r\n"); !strings.Contains(body, tokenText) {
		t.Errorf("通知页脚中缺少令牌")
	}
	if strings.Count(text, "\n") != strings.Count(text, "\r\n") {
		t.Errorf("通知中出现了裸换行，SMTP 要求每一行都以 CRLF 结束")
	}
	lines, decoded := subjectField(t, raw)
	if lines[0] != "Subject: "+tag {
		t.Errorf("主题头首行不是「Subject: 」加完整的标签（首行 %d 个字符）", len(lines[0]))
	}
	if decoded != tag+" "+title {
		t.Errorf("解码后的主题不等于「标签 + 空格 + 标题」（得到 %d 个字符）", len(decoded))
	}
}

// TestComposeNotificationSubjects 断言主题的编码结构与 4b 渲染器约定的一致：标签以原始 ASCII 位于主题头首行开头，
// 从不进入 encoded-word，也不被折行折断；标题按 UTF-8 B 编码，足够长时拆成多个 encoded-word，纯 ASCII 时原样保留；
// 标题为空时主题就是标签。旧形态标签（IDLE 自发邮件）同样适用。
func TestComposeNotificationSubjects(t *testing.T) {
	tokenText := testToken(t)
	tag := live.SubjectTag(taskID, tokenText)
	for _, c := range []struct {
		name     string
		tag      string
		title    string
		minWords int // Subject 字段中至少应有的 encoded-word 个数；为 0 时不应出现 encoded-word
	}{
		{name: "长标题拆成多个 encoded-word", tag: tag, title: strings.Repeat("探测标题很长", 12), minWords: 2},
		{name: "纯 ASCII 标题原样保留", tag: tag, title: "TurnCourier L1 probe 1/1"},
		{name: "标题为空", tag: tag},
		{name: "旧形态标签", tag: live.SubjectTag(taskID, ""), title: "TurnCourier L1 IDLE 探测", minWords: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw, err := live.ComposeNotification(testNotification(c.tag, c.title, tokenText))
			if err != nil {
				t.Fatalf("渲染通知失败: %v", err)
			}
			lines, decoded := subjectField(t, raw)
			head := "Subject: " + c.tag
			if first := lines[0]; !strings.HasPrefix(first, head) || (len(first) > len(head) && first[len(head)] != ' ') {
				t.Errorf("主题头首行没有以完整的标签开头（首行 %d 个字符）", len(first))
			}
			want := c.tag
			if c.title != "" {
				want += " " + c.title
			}
			if decoded != want {
				t.Errorf("解码后的主题不等于「标签 + 空格 + 标题」（得到 %d 个字符，期望 %d 个字符）", len(decoded), len(want))
			}
			words := strings.Count(strings.Join(lines, "\r\n"), "=?utf-8?b?")
			if c.minWords == 0 && words != 0 {
				t.Errorf("Subject 字段中出现了 %d 个 encoded-word，期望没有", words)
			}
			if words < c.minWords {
				t.Errorf("Subject 字段中有 %d 个 encoded-word，期望至少 %d 个", words, c.minWords)
			}
		})
	}
}

// TestComposeNotificationRejectsBadTag 断言形状不对的标签被拒绝：含 CR/LF（头部注入）、大写、长度不对、多余空白，
// 或标签之前、之后有多余文字时返回 ErrInvalidTag 且不渲染任何字节，错误文本不回显输入（新形态的标签含令牌）。
func TestComposeNotificationRejectsBadTag(t *testing.T) {
	tokenText := testToken(t)
	tag := live.SubjectTag(taskID, tokenText)
	for _, c := range []struct {
		name string
		tag  string
	}{
		{name: "含 CR/LF", tag: tag + "\r\nBcc: " + canaryAddress},
		{name: "结尾换行", tag: live.SubjectTag(taskID, "") + "\n"},
		{name: "令牌大写", tag: live.SubjectTag(taskID, strings.ToUpper(tokenText))},
		{name: "TC 小写", tag: "[tc " + taskID + " " + tokenText + "]"},
		{name: "令牌少一位", tag: live.SubjectTag(taskID, tokenText[:47])},
		{name: "任务 ID 多一位", tag: live.SubjectTag(taskID+"0", tokenText)},
		{name: "两个空格", tag: live.SubjectTag(taskID, " "+tokenText)},
		{name: "标签后有其他文字", tag: tag + " " + canaryPhone},
		{name: "标签前有其他文字", tag: "x" + tag},
		{name: "空标签", tag: ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw, err := live.ComposeNotification(testNotification(c.tag, "TurnCourier L1 探测 1/1", tokenText))
			if !errors.Is(err, live.ErrInvalidTag) {
				t.Fatalf("错误 = %v，期望 ErrInvalidTag", err)
			}
			if raw != nil {
				t.Errorf("标签被拒绝时不应返回邮件字节")
			}
			for _, canary := range []string{tokenText, strings.ToUpper(tokenText), taskID, canaryAddress, canaryPhone} {
				if strings.Contains(err.Error(), canary) {
					t.Errorf("错误文本回显了输入")
				}
			}
		})
	}
}

// TestSendPrompt 断言逐封确认的文本含序号、两个地址、标题、是否抄送与「不回显」的说明，且不含任何 48 个字母表字符的串：
// SendPrompt 的签名里没有标签参数，含令牌的标签无从进入 /dev/tty。
func TestSendPrompt(t *testing.T) {
	title := "TurnCourier L1 探测标题"
	prompt := live.SendPrompt("bot@example.invalid", "user.canary@example.invalid", 2, 3, title, true)
	for _, want := range []string{"2/3", "bot@example.invalid", "user.canary@example.invalid", title, "主题标签含一次性令牌，不回显"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("确认文本中缺少 %q：%s", want, prompt)
		}
	}
	if prompt == live.SendPrompt("bot@example.invalid", "user.canary@example.invalid", 2, 3, title, false) {
		t.Errorf("确认文本没有区分是否抄送机器人自己")
	}
	// 用比字母表更宽的 [0-9a-z] 检查：没有 48 个连续的这类字符，就更不会有 48 个连续的字母表字符。
	if longRun := regexp.MustCompile(`(?i)[0-9a-z]{48}`); longRun.MatchString(prompt) {
		t.Errorf("确认文本中出现了 48 个连续的字母表字符")
	}
}

// TestJudgeIdleCheck 断言预检与 FETCH 检查的判定规则：立即返回且邮件数不变记为附带，ctx 到期记为未附带，其余为未知。
func TestJudgeIdleCheck(t *testing.T) {
	base := live.IdleCheck{Messages0: 7, Messages1: 7, UIDNext0: 9, UIDNext1: 9, RoundTrip: 40 * time.Millisecond}
	attached := base
	attached.Returned, attached.NewMail, attached.Elapsed = true, true, time.Millisecond
	notAttached := base
	notAttached.DeadlineExceeded, notAttached.Elapsed = true, 10*time.Second
	changed := attached
	changed.Messages1 = 8
	slow := attached
	slow.Elapsed = 100 * time.Millisecond
	cases := []struct {
		name  string
		check live.IdleCheck
		want  string
	}{
		{name: "附带", check: attached, want: "attached"},
		{name: "未附带", check: notAttached, want: "not_attached"},
		{name: "邮件数变化", check: changed, want: "unknown"},
		{name: "耗时接近一次往返", check: slow, want: "unknown"},
	}
	for _, c := range cases {
		if got := live.JudgeIdleCheck(c.check); got != c.want {
			t.Errorf("%s：判定 = %q，期望 %q", c.name, got, c.want)
		}
	}
}

// TestIdleConclusion 断言预检与 FETCH 检查的结论组合，包括预检未能进行时的「未知或随 UID FETCH」。
func TestIdleConclusion(t *testing.T) {
	cases := []struct {
		preflight, fetch, want string
	}{
		{"attached", "", "uid_search"},
		{"not_attached", "attached", "uid_fetch"},
		{"not_attached", "not_attached", "none"},
		{"unknown", "attached", "unknown_or_uid_fetch"},
		{"unknown", "unknown", "unknown"},
		{"not_attached", "", "unknown"},
	}
	for _, c := range cases {
		if got := live.IdleConclusion(c.preflight, c.fetch); got != c.want {
			t.Errorf("IdleConclusion(%q, %q) = %q，期望 %q", c.preflight, c.fetch, got, c.want)
		}
	}
}

// TestRepoRoot 断言仓库根目录按 go.mod 向上查找：本测试的工作目录位于仓库内，找到的目录含 go.mod。
func TestRepoRoot(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("读取工作目录失败: %v", err)
	}
	root, err := live.RepoRoot(wd)
	if err != nil {
		t.Fatalf("查找仓库根目录失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Errorf("仓库根目录 %q 下没有 go.mod: %v", filepath.Base(root), err)
	}
	if _, err := live.RepoRoot(string(filepath.Separator)); err == nil {
		t.Errorf("文件系统根目录下不应找到 go.mod")
	}
}

// TestValidateOutputDir 断言输出目录校验：仓库内的目录、其大小写变体、指向仓库内的符号链接、权限过宽的目录、
// 相对路径与不存在的目录都被拒绝；仓库外权限 0700 的目录通过，指向它的符号链接也通过并返回解析后的真实路径。
func TestValidateOutputDir(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	inside := filepath.Join(root, "inside")
	outside := filepath.Join(base, "outside")
	wide := filepath.Join(base, "wide")
	for _, dir := range []string{root, inside, outside, wide} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("创建目录失败: %v", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatalf("设置目录权限失败: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.invalid\n"), 0o600); err != nil {
		t.Fatalf("写入 go.mod 失败: %v", err)
	}
	if err := os.Chmod(wide, 0o755); err != nil {
		t.Fatalf("设置宽权限失败: %v", err)
	}
	linkInside := filepath.Join(base, "link-inside")
	linkOutside := filepath.Join(base, "link-outside")
	if err := os.Symlink(inside, linkInside); err != nil {
		t.Fatalf("创建符号链接失败: %v", err)
	}
	if err := os.Symlink(outside, linkOutside); err != nil {
		t.Fatalf("创建符号链接失败: %v", err)
	}
	caseVariant := filepath.Join(base, "REPO", "inside")

	for _, c := range []struct {
		name string
		dir  string
	}{
		{name: "仓库内的子目录", dir: inside},
		{name: "大小写变体", dir: caseVariant},
		{name: "指向仓库内的符号链接", dir: linkInside},
		{name: "权限过宽", dir: wide},
		{name: "相对路径", dir: filepath.Base(outside)},
		{name: "不存在的目录", dir: filepath.Join(base, "missing")},
		{name: "空路径", dir: ""},
	} {
		if _, err := live.ValidateOutputDir(c.dir, root); err == nil {
			t.Errorf("%s 应被拒绝", c.name)
		}
	}

	real, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatalf("解析真实路径失败: %v", err)
	}
	for _, c := range []struct {
		name string
		dir  string
	}{
		{name: "仓库外的目录", dir: outside},
		{name: "指向仓库外的符号链接", dir: linkOutside},
	} {
		got, err := live.ValidateOutputDir(c.dir, root)
		if err != nil {
			t.Errorf("%s 应通过: %v", c.name, err)
			continue
		}
		if got != real {
			t.Errorf("%s 返回 %q，期望解析后的真实路径", c.name, filepath.Base(got))
		}
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Log("区分大小写的文件系统上，大小写变体目录不存在，同样被拒绝")
	}
}

// TestStateAndSamples 断言状态与样本文件以 0600 写入输出目录，状态可以读回，样本按行追加。
func TestStateAndSamples(t *testing.T) {
	dir := t.TempDir()
	if empty, err := live.LoadState(dir); err != nil || len(empty.Mails) != 0 {
		t.Fatalf("空目录应返回空状态: %+v %v", empty, err)
	}
	st := testState(t)
	st.Cursors = map[string]live.Cursor{"INBOX": {UIDValidity: 3, LastUID: 9}}
	// 第二封没有 DATA 候选：清单要求这时 delivered_data 仍写成空数组。
	st.Mails = append(st.Mails, live.Mail{ProbeID: probeID, TaskID: taskID, MessageID: stranger, DeliveredData: []string{}})
	if err := live.SaveState(dir, st); err != nil {
		t.Fatalf("写入状态失败: %v", err)
	}
	got, err := live.LoadState(dir)
	if err != nil {
		t.Fatalf("读回状态失败: %v", err)
	}
	if len(got.Mails) != 2 || got.Mails[0].MessageID != oursID || got.Cursors["INBOX"].LastUID != 9 {
		t.Errorf("读回的状态 = %+v，与写入的不一致", got)
	}
	text, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("读取状态文件失败: %v", err)
	}
	if !strings.Contains(string(text), `"delivered_data": []`) {
		t.Errorf("state.json 中没有候选时应写成空数组")
	}
	for _, name := range []string{"state.json", "samples.jsonl"} {
		if name == "samples.jsonl" {
			if err := live.AppendSample(dir, live.Record{Schema: live.Schema, Kind: "capabilities"}); err != nil {
				t.Fatalf("追加样本失败: %v", err)
			}
			if err := live.AppendSample(dir, live.Record{Schema: live.Schema, Kind: "send"}); err != nil {
				t.Fatalf("追加样本失败: %v", err)
			}
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Errorf("%s 权限 = %o，期望 0600", name, info.Mode().Perm())
		}
	}
	lines, err := os.ReadFile(filepath.Join(dir, "samples.jsonl"))
	if err != nil {
		t.Fatalf("读取样本失败: %v", err)
	}
	if count := strings.Count(string(lines), "\n"); count != 2 {
		t.Errorf("样本行数 = %d，期望 2", count)
	}
}

// TestAnalyzeSkipsProbeCopies 断言探测邮件自身的副本与无关来信都不会输出样本。
func TestAnalyzeSkipsProbeCopies(t *testing.T) {
	tokenText := testToken(t)
	st := testState(t)
	copyRaw := rawMessage(t, []string{
		"From: <bot@example.invalid>",
		"To: <user.canary@example.invalid>",
		"Subject: " + encodedSubject(t, "[TC "+taskID+"] TurnCourier L1 探测 1/1"),
		"Message-Id: " + ccID,
		"X-TurnCourier-Probe: " + probeID,
	}, qqPlainBody(tokenText), qqHTMLBody(tokenText))
	if _, ok, err := live.Analyze(copyRaw, st, testRoles()); err != nil || ok {
		t.Errorf("探测邮件的副本不应输出样本: ok=%v err=%v", ok, err)
	}
	// 「已发送」与抄送副本经 QQ 转发后可能不带 X-TurnCourier-Probe 头（QQ 会改写头部），
	// 这时只剩 Message-ID 等于已记录的副本 ID 这一条依据：两条依据各自独立成立。
	for _, c := range []struct {
		name string
		id   string
	}{
		{name: "已发送副本", id: sentID},
		{name: "抄送副本", id: ccID},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := rawMessage(t, []string{
				"From: <bot@example.invalid>",
				"To: <user.canary@example.invalid>",
				"Subject: " + encodedSubject(t, "[TC "+taskID+"] TurnCourier L1 探测 1/1"),
				"Message-Id: " + c.id,
			}, qqPlainBody(tokenText), qqHTMLBody(tokenText))
			if _, ok, err := live.Analyze(raw, st, testRoles()); err != nil || ok {
				t.Errorf("不带探测头的副本不应输出样本: ok=%v err=%v", ok, err)
			}
		})
	}
	unrelated := rawMessage(t, []string{
		"From: <" + canaryAddress + ">",
		"To: <bot@example.invalid>",
		"Subject: " + encodedSubject(t, "无关来信"),
		"Message-Id: " + stranger,
	}, canarySentence, "<html><body>"+canarySentence+"</body></html>")
	if _, ok, err := live.Analyze(unrelated, st, testRoles()); err != nil || ok {
		t.Errorf("无关来信不应输出样本: ok=%v err=%v", ok, err)
	}
	// 线程头里带着 tencent_…@qq.com 形式的 ID，但它不等于任何已记录的来源：other_tencent 不算已知来源。
	tencentThread := rawMessage(t, []string{
		"From: <" + canaryAddress + ">",
		"To: <bot@example.invalid>",
		"Subject: " + encodedSubject(t, "无关来信"),
		"Message-Id: " + stranger,
		"In-Reply-To: " + unknownID,
		"References: " + unknownID,
	}, canarySentence, "<html><body>"+canarySentence+"</body></html>")
	if _, ok, err := live.Analyze(tencentThread, st, testRoles()); err != nil || ok {
		t.Errorf("线程头只含未知腾讯 ID 的来信不应输出样本: ok=%v err=%v", ok, err)
	}
}

// TestAnalyzeRedactsMalformedLabels 断言来信可控的短标签不构成脱敏旁路：畸形 encoded-word 的字符集位置、
// Content-Type 的 charset 参数、无法解析的 Content-Type 整行，以及 Auto-Submitted 与 Precedence 的畸形取值
// 都记为 other，样本中不出现其中的文字。
func TestAnalyzeRedactsMalformedLabels(t *testing.T) {
	tokenText := testToken(t)
	malformedWord := "=?关于合同 " + canaryPhone + " 的回复?B?" + gb18030Base64(t, "回复：[TC "+taskID+"] 探测") + "?="
	goodWord := encodedSubject(t, "回复：[TC "+taskID+"] 探测")
	for _, c := range []struct {
		name           string
		subject        string
		contentType    string
		extra          []string
		wantEncoding   string
		wantType       string
		wantCharset    string
		wantSubmitted  string
		wantPrecedence string
	}{
		{
			name: "畸形 encoded-word", subject: malformedWord, contentType: `text/plain; charset="utf-8"`,
			wantEncoding: "other/B", wantType: "text/plain", wantCharset: "utf-8",
		},
		{
			name: "畸形字符集参数", subject: goodWord,
			contentType:  `text/plain; charset="客户手机号 ` + canaryPhone + ` 很长很长的标签"`,
			wantEncoding: "gbk/B", wantType: "text/plain", wantCharset: "other",
		},
		{
			name: "无法解析的媒体类型", subject: goodWord, contentType: "关于合同 " + canaryPhone,
			wantEncoding: "gbk/B", wantType: "other", wantCharset: "",
		},
		{
			name: "畸形自动来信头", subject: goodWord, contentType: `text/plain; charset="utf-8"`,
			extra: []string{
				"Auto-Submitted: 客户张三的手机号 " + canaryPhone + " 合同编号 ABC",
				"Precedence: 金丝雀 " + canaryPhone,
			},
			wantEncoding: "gbk/B", wantType: "text/plain", wantCharset: "utf-8",
			wantSubmitted: "other", wantPrecedence: "other",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			extra := append([]string{"Subject: " + c.subject, "In-Reply-To: " + sentID}, c.extra...)
			raw := rawSinglePart(t, replyHeaders(t, extra...), c.contentType, tokenText)
			sample := analyze(t, raw, testState(t))
			if sample.Subject.Encoding != c.wantEncoding {
				t.Errorf("subject.encoding = %q，期望 %q", sample.Subject.Encoding, c.wantEncoding)
			}
			if sample.MIME[0].Type != c.wantType || sample.MIME[0].Charset != c.wantCharset {
				t.Errorf("mime = %+v，期望类型 %q、字符集 %q", sample.MIME[0], c.wantType, c.wantCharset)
			}
			if sample.Auto.AutoSubmitted != c.wantSubmitted || sample.Auto.Precedence != c.wantPrecedence {
				t.Errorf("auto = %+v，期望 auto_submitted %q、precedence %q",
					sample.Auto, c.wantSubmitted, c.wantPrecedence)
			}
			if output := marshal(t, sample); strings.Contains(output, canaryPhone) {
				t.Errorf("样本中出现了头部里的数字 %q", canaryPhone)
			}
		})
	}
}

// TestAnalyzeCapsCharsetLabels 断言字符集位置按更短的上限截断：主题编码字中的字符集直接取自 Subject 头，
// 形状合法的长文本若按媒体类型的上限输出，就会把主题内容带进样本。
func TestAnalyzeCapsCharsetLabels(t *testing.T) {
	tokenText := testToken(t)
	long := strings.Repeat("ab.cd-ef_", 30) // 形状合法、远超字符集上限
	subject := "=?" + long + "?B?" + base64.StdEncoding.EncodeToString([]byte("回复：[TC "+taskID+"] 探测")) + "?="
	raw := rawSinglePart(t, replyHeaders(t, "Subject: "+subject, "In-Reply-To: "+sentID),
		`text/plain; charset="`+long+`"`, tokenText)
	sample := analyze(t, raw, testState(t))
	wantCharset := long[:40]
	if sample.Subject.Encoding != wantCharset+"/B" {
		t.Errorf("subject.encoding = %q，期望 %q", sample.Subject.Encoding, wantCharset+"/B")
	}
	if sample.MIME[0].Charset != wantCharset {
		t.Errorf("mime[0].charset = %q，期望 %q", sample.MIME[0].Charset, wantCharset)
	}
}

// TestAnalyzeKeepsLongMediaType 断言形状合法的长媒体类型原样保留：Word 与 Excel 的 OOXML 类型前 40 个字符相同，
// 按短标签上限截断会让两种附件在样本中无法分辨。
func TestAnalyzeKeepsLongMediaType(t *testing.T) {
	tokenText := testToken(t)
	for _, mediaType := range []string{
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	} {
		t.Run(mediaType, func(t *testing.T) {
			raw := rawSinglePart(t, replyHeaders(t, "Subject: "+encodedSubject(t, "回复：[TC "+taskID+"] 探测"),
				"In-Reply-To: "+sentID), mediaType, tokenText)
			sample := analyze(t, raw, testState(t))
			if sample.MIME[0].Type != mediaType {
				t.Errorf("mime[0].type = %q，期望 %q", sample.MIME[0].Type, mediaType)
			}
		})
	}
}

// TestAnalyzePlainTokens 断言令牌行前缀的兜底脱敏与令牌边界：前缀不是纯引用标记时记为 other，
// 纯引用标记原样输出，长于 48 的连续字母表字符串不算令牌。
func TestAnalyzePlainTokens(t *testing.T) {
	tokenText := testToken(t)
	for _, c := range []struct {
		name       string
		plain      string
		wantTokens int
		wantPrefix string
	}{
		{name: "正文与令牌同行", plain: canarySentence + " " + tokenText, wantTokens: 1, wantPrefix: "other"},
		{name: "引用前缀", plain: "> " + tokenText, wantTokens: 1, wantPrefix: "> "},
		{name: "超长连续串", plain: strings.Repeat("a", 49) + "\n" + tokenText, wantTokens: 1, wantPrefix: ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := rawMessage(t, replyHeaders(t, "Subject: "+encodedSubject(t, "回复：[TC "+taskID+"] 探测")),
				c.plain, "<html><body>x</body></html>")
			sample := analyze(t, raw, testState(t))
			if sample.Plain.Tokens != c.wantTokens || sample.Plain.TokenLinePrefix != c.wantPrefix {
				t.Errorf("tokens = %d、token_line_prefix = %q，期望 %d 与 %q",
					sample.Plain.Tokens, sample.Plain.TokenLinePrefix, c.wantTokens, c.wantPrefix)
			}
			if output := marshal(t, sample); strings.Contains(output, canarySentence) {
				t.Errorf("样本中出现了令牌同一行上的正文")
			}
		})
	}
}

// TestAnalyzeTruncatesLongValues 断言清单规定的两个上限：形状合法的客户端标识截断到 40 个字符，
// 主题前缀至多 20 个字符。
func TestAnalyzeTruncatesLongValues(t *testing.T) {
	tokenText := testToken(t)
	raw := rawMessage(t, []string{
		"From: <" + canaryAddress + ">",
		"To: <bot@example.invalid>",
		"Message-Id: " + replySelfI,
		"X-Mailer: " + strings.Repeat("LongClient-X", 12),
		"Subject: " + encodedSubject(t, strings.Repeat("Re:", 20)+"[TC "+taskID+"] 探测"),
		"In-Reply-To: " + sentID,
	}, qqPlainBody(tokenText), qqHTMLBody(tokenText))
	sample := analyze(t, raw, testState(t))
	if n := len([]rune(sample.Client)); n != 40 {
		t.Errorf("client 字符数 = %d，期望 40：%q", n, sample.Client)
	}
	want := strings.Repeat("Re:", 20)[:20]
	if sample.Subject.Prefix == nil || *sample.Subject.Prefix != want {
		t.Errorf("subject.prefix = %+v，期望 %q", sample.Subject.Prefix, want)
	}
}

// TestAnalyzeRedactsClient 断言客户端标识同样有形状约束：X-Mailer 与 User-Agent 完全由来信控制，
// 畸形邮件可以把正文或主题塞进去，只截断拦不住。合法取值原样输出，含非 ASCII、引号或控制字符的记为 other。
func TestAnalyzeRedactsClient(t *testing.T) {
	tokenText := testToken(t)
	for _, c := range []struct {
		name   string
		header string
		want   string
	}{
		{name: "合法取值", header: "X-Mailer: Foxmail 7.2.25.148[cn]", want: "Foxmail 7.2.25.148[cn]"},
		{name: "非 ASCII", header: "X-Mailer: 客户手机号 " + canaryPhone + " " + canarySentence, want: "other"},
		{name: "含引号", header: `X-Mailer: "` + canaryPhone + `"`, want: "other"},
		{name: "含控制字符", header: "User-Agent: QQMail\x01" + canaryPhone, want: "other"},
		{name: "两个头都没有", header: "X-TurnCourier-Unused: 1", want: ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := rawMessage(t, []string{
				"From: <" + canaryAddress + ">",
				"To: <bot@example.invalid>",
				"Message-Id: " + replySelfI,
				c.header,
				"Subject: " + encodedSubject(t, "回复：[TC "+taskID+"] 探测"),
				"In-Reply-To: " + sentID,
			}, qqPlainBody(tokenText), qqHTMLBody(tokenText))
			sample := analyze(t, raw, testState(t))
			if sample.Client != c.want {
				t.Errorf("client = %q，期望 %q", sample.Client, c.want)
			}
			if output := marshal(t, sample); strings.Contains(output, canaryPhone) || strings.Contains(output, canarySentence) {
				t.Errorf("样本中出现了客户端标识里的文字")
			}
		})
	}
}

// TestAnalyzeFromRole 断言发件地址一律换成角色：机器人、白名单发件人与无法解析的地址各有一条分支，
// 样本中只出现角色名，不出现地址。
func TestAnalyzeFromRole(t *testing.T) {
	tokenText := testToken(t)
	for _, c := range []struct {
		name string
		from string
		want string
	}{
		{name: "机器人自己", from: "<bot@example.invalid>", want: "bot"},
		{name: "白名单发件人", from: "<other@example.invalid>", want: "allowed"},
		{name: "无法解析的地址", from: canaryName, want: "other"},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := rawMessage(t, []string{
				"From: " + c.from,
				"To: <bot@example.invalid>",
				"Subject: " + encodedSubject(t, "回复：[TC "+taskID+"] 探测"),
				"Message-Id: " + replySelfI,
				"In-Reply-To: " + sentID,
			}, qqPlainBody(tokenText), qqHTMLBody(tokenText))
			sample := analyze(t, raw, testState(t))
			if sample.FromRole != c.want {
				t.Errorf("from_role = %q，期望 %q", sample.FromRole, c.want)
			}
			if output := marshal(t, sample); strings.Contains(output, "example.invalid") || strings.Contains(output, canaryName) {
				t.Errorf("样本中出现了发件地址")
			}
		})
	}
}

// TestAnalyzeConstrainsAuthResults 断言 Authentication-Results 的结果值收敛到已知取值：
// 这个位置同样取自来信可控的字节，白名单之外的文字记为 other，不进样本。
func TestAnalyzeConstrainsAuthResults(t *testing.T) {
	tokenText := testToken(t)
	for _, c := range []struct {
		name   string
		header string
		want   live.AuthResults
	}{
		{
			name:   "已知取值",
			header: "Authentication-Results: mx.example.invalid; dkim=pass; spf=softfail; dmarc=temperror",
			want:   live.AuthResults{Present: true, DKIM: "pass", SPF: "softfail", DMARC: "temperror"},
		},
		{
			name:   "白名单之外",
			header: "Authentication-Results: mx.example.invalid; dkim=" + strings.Repeat("Canary", 20) + "; spf=none",
			want:   live.AuthResults{Present: true, DKIM: "other", SPF: "none"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := rawMessage(t, replyHeaders(t,
				"Subject: "+encodedSubject(t, "回复：[TC "+taskID+"] 探测"),
				"In-Reply-To: "+sentID,
				c.header,
			), qqPlainBody(tokenText), qqHTMLBody(tokenText))
			sample := analyze(t, raw, testState(t))
			if sample.AuthResults != c.want {
				t.Errorf("auth_results = %+v，期望 %+v", sample.AuthResults, c.want)
			}
			if output := strings.ToLower(marshal(t, sample)); strings.Contains(output, "canary") {
				t.Errorf("样本中出现了验证结果里的文字")
			}
		})
	}
}

// TestAnalyzeAcceptsHTMLOnlyToken 断言令牌只留在 HTML 引用中的回复同样被接受：HTML 也是正文，
// 而 D1 指出 QQ App 与 Foxmail 的回复可能既没有线程头，主题也被改写。
func TestAnalyzeAcceptsHTMLOnlyToken(t *testing.T) {
	tokenText := testToken(t)
	raw := rawSinglePart(t, []string{
		"From: <" + canaryAddress + ">",
		"To: <bot@example.invalid>",
		"Message-Id: " + replySelfI,
		"Subject: " + encodedSubject(t, "无关主题"),
	}, `text/html; charset="utf-8"`, "<html><body><blockquote>"+tokenText+"</blockquote></body></html>")
	sample := analyze(t, raw, testState(t))
	if sample.HTML.Tokens != 1 || !sample.HTML.Blockquote {
		t.Errorf("html = %+v，期望 1 个令牌且 blockquote 为 true", sample.HTML)
	}
}

// TestAnalyzeBounce 断言退信按 multipart/report 与 MAILER-DAEMON 归类为 bounce，并且引用的令牌仍被识别。
func TestAnalyzeBounce(t *testing.T) {
	tokenText := testToken(t)
	raw := []byte(strings.Join([]string{
		"From: Mail Delivery System <MAILER-DAEMON@qq.com>",
		"To: <bot@example.invalid>",
		"Subject: " + encodedSubject(t, "Undelivered Mail Returned to Sender"),
		"Message-Id: <bounce.1@qq.com>",
		"In-Reply-To: " + sentID,
		"Return-Path: <>",
		"MIME-Version: 1.0",
		"Content-Type: multipart/report; report-type=delivery-status; boundary=\"b2\"",
		"",
		"--b2",
		"Content-Type: text/plain; charset=\"us-ascii\"",
		"",
		"delivery failed",
		"",
		"--b2",
		"Content-Type: message/delivery-status",
		"",
		"Final-Recipient: rfc822; user@example.invalid",
		"Action: failed",
		"",
		"--b2--",
		"",
	}, "\r\n"))
	sample, ok, err := live.Analyze(raw, testState(t), testRoles())
	if err != nil || !ok {
		t.Fatalf("退信应输出样本: ok=%v err=%v", ok, err)
	}
	if sample.Kind != "bounce" {
		t.Errorf("kind = %q，期望 bounce", sample.Kind)
	}
	if !sample.Auto.ReturnPathEmpty {
		t.Errorf("return_path_empty = false，期望 true")
	}
	if output := marshal(t, sample); strings.Contains(output, tokenText) {
		t.Errorf("样本中出现了令牌")
	}
}

// TestAnalyzeBounceGrounds 断言退信判定的两条依据各自独立成立：只有 multipart/report、
// 或只有 MAILER-DAEMON/postmaster 发件人时都记为 bounce；两条都不成立时仍是 reply。
// TestAnalyzeBounce 的那封退信同时满足两条，单独一条失效也拦不住。
func TestAnalyzeBounceGrounds(t *testing.T) {
	tokenText := testToken(t)
	for _, c := range []struct {
		name string
		from string
		want string
	}{
		{name: "只有 multipart/report", from: canaryAddress, want: "bounce"},
		{name: "只有退信发件人", from: "postmaster@qq.com", want: "bounce"},
	} {
		t.Run(c.name, func(t *testing.T) {
			headers := []string{
				"From: <" + c.from + ">",
				"To: <bot@example.invalid>",
				"Subject: " + encodedSubject(t, "回复：[TC "+taskID+"] 探测"),
				"Message-Id: " + replySelfI,
				"In-Reply-To: " + sentID,
			}
			var raw []byte
			if c.name == "只有 multipart/report" {
				raw = rawReport(t, headers)
			} else {
				raw = rawMessage(t, headers, qqPlainBody(tokenText), qqHTMLBody(tokenText))
			}
			sample := analyze(t, raw, testState(t))
			if sample.Kind != c.want {
				t.Errorf("kind = %q，期望 %q", sample.Kind, c.want)
			}
			// 两条依据都不成立的同一封信仍是 reply：没有它，把判定改成恒为 bounce 也能通过。
			if c.want == "bounce" {
				headers[0] = "From: <" + canaryAddress + ">"
				plain := rawMessage(t, headers, qqPlainBody(tokenText), qqHTMLBody(tokenText))
				if got := analyze(t, plain, testState(t)).Kind; got != "reply" {
					t.Errorf("对照来信的 kind = %q，期望 reply", got)
				}
			}
		})
	}
}

// rawReport 组装一封 multipart/report 的合成退信；正文与退信报告都不含令牌。
func rawReport(t *testing.T, headers []string) []byte {
	t.Helper()
	lines := append([]string{}, headers...)
	return []byte(strings.Join(append(lines,
		"MIME-Version: 1.0",
		"Content-Type: multipart/report; report-type=delivery-status; boundary=\"b3\"",
		"",
		"--b3",
		"Content-Type: text/plain; charset=\"us-ascii\"",
		"",
		"delivery failed",
		"",
		"--b3",
		"Content-Type: message/delivery-status",
		"",
		"Final-Recipient: rfc822; user@example.invalid",
		"Action: failed",
		"",
		"--b3--",
		"",
	), "\r\n"))
}

// TestAnalyzeRejectsMalformed 断言无法解析的字节返回哨兵错误而不是半个样本，并且错误文本不携带来信字节：
// go-message 在头部畸形时会把整行来信原文放进错误文本，而探测工具会把错误打印到测试输出。
func TestAnalyzeRejectsMalformed(t *testing.T) {
	// 畸形头行与畸形字段名各对应 go-message 的一条错误文本，两条都含整行来信原文。
	badLine := []byte("Subject: " + canarySentence + "\r\n" + canarySentence + "\r\n\r\n" + canarySentence + "\r\n")
	badKey := []byte("X-" + canarySentence + "\x01: 1\r\n\r\n" + canarySentence + "\r\n")
	tooLarge := []byte("Subject: " + strings.Repeat(canarySentence, 1<<16) + "\r\n\r\n")
	for _, c := range []struct {
		name string
		raw  []byte
		want error
	}{
		{name: "畸形头行", raw: badLine, want: live.ErrMalformedHeader},
		{name: "畸形字段名", raw: badKey, want: live.ErrMalformedHeader},
		{name: "头部超限", raw: tooLarge, want: live.ErrHeaderTooLarge},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, ok, err := live.Analyze(c.raw, testState(t), testRoles())
			assertSentinel(t, err, c.want)
			if ok {
				t.Errorf("无法解析的字节不应输出样本")
			}
			_, err = live.CopyHeaders(c.raw)
			assertSentinel(t, err, c.want)
		})
	}
}

// assertSentinel 断言错误恰为给定的哨兵错误，且错误文本不含来信中的金丝雀。
func assertSentinel(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("错误 = %v，期望 %v", err, want)
		return
	}
	if strings.Contains(err.Error(), canarySentence) {
		t.Errorf("错误文本中出现了来信字节")
	}
}
