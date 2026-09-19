// Package live 的离线自检：用合成邮件字节与合成状态验证样本脱敏与归类、主题前缀白名单、Message-ID 按相等关系归类、
// DATA 响应脱敏、IDLE 观测判定、通知渲染与输出目录校验。本文件不带构建标签，随 make test 在 CI 中运行，
// 不读取配置、钥匙串与网络；合成令牌与密钥都在运行时构造，源码中不出现机密形状的字面量。
package live_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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

// TestAnalyzeSubjects 断言主题前缀白名单：已知前缀原样输出，其他文字记为 other，没有标签时为 null，
// 三种情况都不输出主题中的其他文字。
func TestAnalyzeSubjects(t *testing.T) {
	tokenText := testToken(t)
	cases := []struct {
		name      string
		subject   string
		want      *string
		tagIntact bool
	}{
		{name: "已知前缀", subject: "回复：[TC " + taskID + "] 探测", want: stringPtr("回复："), tagIntact: true},
		{name: "重复前缀", subject: "答复: Re:[TC " + taskID + "] 探测", want: stringPtr("答复:Re:"), tagIntact: true},
		{name: "其他文字", subject: "关于合同 " + canaryPhone + " [TC " + taskID + "]", want: stringPtr("other"), tagIntact: true},
		{name: "没有标签", subject: "关于合同 " + canaryPhone, want: nil, tagIntact: false},
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

// TestComposeNotification 断言合成通知可被解析：含探测 ID、我方 Message-ID、Auto-Submitted 与页脚令牌，
// 主题标签完整，且两个部件分别为 text/plain 与 text/html。
func TestComposeNotification(t *testing.T) {
	tokenText := testToken(t)
	raw, err := live.ComposeNotification(live.Notification{
		From:      "bot@example.invalid",
		To:        "user.canary@example.invalid",
		CC:        "bot@example.invalid",
		Subject:   "[TC " + taskID + "] TurnCourier L1 探测 1/1",
		MessageID: oursID,
		ProbeID:   probeID,
		Token:     tokenText,
		Date:      time.UnixMilli(1_789_000_000_000).UTC(),
	})
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
	if !strings.Contains(text, tokenText) {
		t.Errorf("通知页脚中缺少令牌")
	}
	if strings.Count(text, "\n") != strings.Count(text, "\r\n") {
		t.Errorf("通知中出现了裸换行，SMTP 要求每一行都以 CRLF 结束")
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
// Content-Type 的 charset 参数与无法解析的 Content-Type 整行都记为 other，样本中不出现其中的文字。
func TestAnalyzeRedactsMalformedLabels(t *testing.T) {
	tokenText := testToken(t)
	malformedWord := "=?关于合同 " + canaryPhone + " 的回复?B?" + gb18030Base64(t, "回复：[TC "+taskID+"] 探测") + "?="
	goodWord := encodedSubject(t, "回复：[TC "+taskID+"] 探测")
	for _, c := range []struct {
		name         string
		subject      string
		contentType  string
		wantEncoding string
		wantType     string
		wantCharset  string
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
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := rawSinglePart(t, replyHeaders(t, "Subject: "+c.subject, "In-Reply-To: "+sentID),
				c.contentType, tokenText)
			sample := analyze(t, raw, testState(t))
			if sample.Subject.Encoding != c.wantEncoding {
				t.Errorf("subject.encoding = %q，期望 %q", sample.Subject.Encoding, c.wantEncoding)
			}
			if sample.MIME[0].Type != c.wantType || sample.MIME[0].Charset != c.wantCharset {
				t.Errorf("mime = %+v，期望类型 %q、字符集 %q", sample.MIME[0], c.wantType, c.wantCharset)
			}
			if output := marshal(t, sample); strings.Contains(output, canaryPhone) {
				t.Errorf("样本中出现了头部里的数字 %q", canaryPhone)
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

// TestAnalyzeTruncatesLongValues 断言清单规定的两个上限：客户端标识截断到 40 个字符，主题前缀至多 20 个字符。
func TestAnalyzeTruncatesLongValues(t *testing.T) {
	tokenText := testToken(t)
	raw := rawMessage(t, []string{
		"From: <" + canaryAddress + ">",
		"To: <bot@example.invalid>",
		"Message-Id: " + replySelfI,
		"X-Mailer: " + strings.Repeat("超长客户端X", 12),
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

// TestAnalyzeRejectsMalformed 断言无法解析的字节返回错误而不是半个样本。
func TestAnalyzeRejectsMalformed(t *testing.T) {
	if _, _, err := live.Analyze([]byte("not a message"), testState(t), testRoles()); err == nil {
		t.Errorf("无法解析的字节应返回错误")
	}
}
