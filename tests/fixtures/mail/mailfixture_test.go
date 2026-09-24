// Package mailfixture 测试样本模板的加载、校验与组装：每个模板都能加载且字段合法；组装结果用标准库（net/mail、mime、
// mime/multipart、mime/quotedprintable、encoding/base64、html）与 x/text 独立解码后，头部与各部件的文字等于替换占位符之后的模板
// （可选占位符取默认值或调用方的取值，text/html 部件中的取值按 HTML 转义）；四份 L1 样本符合契约记下的实测结构；组装是确定的；
// 缺少占位符的值、未知的占位符、不合规的可选占位符与默认值、不支持的字符集与传输编码、分隔串与内容冲突等都被拒绝；
// README 登记了每个样本及其来源。测试中的令牌文本用 strings.Repeat 构造，只需形状像令牌。
package mailfixture

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// l1Samples 是按 L1 实测结构合成的四份样本；契约点名了它们的结构，TestL1TemplatesMatchRecordedStructure 逐项核对。
var l1Samples = []string{"qq-app-reply", "qq-web-reply", "qq-vacation-auto-reply", "qq-bounce"}

// testValues 返回测试用的占位符取值：令牌文本是 48 个低熵的字母表字符，投递 ID 是合成的。
func testValues() Values {
	return Values{Task: "q7m3k9x2pa", Token: strings.Repeat("ab", 24), Delivered: "<tencent_delivered_0001@example.invalid>"}
}

// leaf 是按标准库独立解码得到的一个叶子部件：媒体类型、原样的 charset 参数、小写的传输编码与解码后的文字。
type leaf struct {
	mediaType string
	charset   string
	encoding  string
	text      string
}

// decodedMail 是一封组装结果按标准库解码后的样子：头部、顶层媒体类型，以及深度优先排列的叶子部件。
type decodedMail struct {
	header    mail.Header
	mediaType string
	leaves    []leaf
}

// testCharsetReader 供 mime.WordDecoder 解码 gb 系列标注的编码词，与模板组装时一致地按 GB18030 解码。
func testCharsetReader(label string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(label) {
	case "gbk", "gb2312", "gb18030":
		return simplifiedchinese.GB18030.NewDecoder().Reader(input), nil
	}
	return nil, errors.New("unsupported test charset")
}

// decodeHeaderText 用标准库解码一个头部取值中的编码词。
func decodeHeaderText(t *testing.T, value string) string {
	t.Helper()
	decoder := mime.WordDecoder{CharsetReader: testCharsetReader}
	text, err := decoder.DecodeHeader(value)
	if err != nil {
		t.Fatalf("decode header words: %v", err)
	}
	return text
}

// decodeCharset 把部件字节按 charset 参数解码为文字；gb 系列按 GB18030，其余（含缺省）按 UTF-8 原样。
func decodeCharset(t *testing.T, label string, data []byte) string {
	t.Helper()
	switch strings.ToLower(label) {
	case "gbk", "gb2312", "gb18030":
		text, err := simplifiedchinese.GB18030.NewDecoder().Bytes(data)
		if err != nil {
			t.Fatalf("decode GB18030: %v", err)
		}
		return string(text)
	}
	return string(data)
}

// decodeEntity 递归解码一个部件：多部件用 mime/multipart 的 NextRawPart 逐个取出（不让标准库自动解开 quoted-printable），
// 叶子按传输编码与字符集解码。
func decodeEntity(t *testing.T, header textproto.MIMEHeader, body io.Reader) []leaf {
	t.Helper()
	mediaType, params := "text/plain", map[string]string{}
	if value := header.Get("Content-Type"); value != "" {
		var err error
		if mediaType, params, err = mime.ParseMediaType(value); err != nil {
			t.Fatalf("parse Content-Type: %v", err)
		}
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		reader := multipart.NewReader(body, params["boundary"])
		var leaves []leaf
		for {
			part, err := reader.NextRawPart()
			if err == io.EOF {
				return leaves
			}
			if err != nil {
				t.Fatalf("next part: %v", err)
			}
			leaves = append(leaves, decodeEntity(t, part.Header, part)...)
		}
	}
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read part: %v", err)
	}
	encoding := strings.ToLower(header.Get("Content-Transfer-Encoding"))
	switch encoding {
	case "base64":
		data, err = base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(data)), ""))
	case "quoted-printable":
		data, err = io.ReadAll(quotedprintable.NewReader(bytes.NewReader(data)))
	}
	if err != nil {
		t.Fatalf("decode %s body: %v", encoding, err)
	}
	return []leaf{{mediaType: mediaType, charset: params["charset"], encoding: encoding, text: decodeCharset(t, params["charset"], data)}}
}

// decodeMail 用标准库读取组装结果：net/mail 解析头部，decodeEntity 解码 MIME 树。
func decodeMail(t *testing.T, raw []byte) decodedMail {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("net/mail cannot read the assembled message: %v", err)
	}
	mediaType := "text/plain"
	if value := msg.Header.Get("Content-Type"); value != "" {
		if mediaType, _, err = mime.ParseMediaType(value); err != nil {
			t.Fatalf("parse top-level Content-Type: %v", err)
		}
	}
	return decodedMail{header: msg.Header, mediaType: mediaType, leaves: decodeEntity(t, textproto.MIMEHeader(msg.Header), msg.Body)}
}

// expectText 按规格独立算出一段模板文字替换占位符之后应有的样子：可选占位符取 optional 中按行给出的取值，
// 在 text/html 部件中（markup 为真）逐行 HTML 转义后以 <br> 连接，其他地方以 newline 连接；固定占位符取 values 中的值，
// 在 text/html 部件中同样转义。可选占位符的取值只可能含固定占位符，因此先换可选、再换固定。
func expectText(text string, values Values, optional map[string][]string, markup bool, newline string) string {
	for name, lines := range optional {
		joined := strings.Join(lines, newline)
		if markup {
			escaped := make([]string, len(lines))
			for i, line := range lines {
				escaped[i] = html.EscapeString(line)
			}
			joined = strings.Join(escaped, "<br>")
		}
		text = strings.ReplaceAll(text, "{{"+name+"}}", joined)
	}
	escape := func(s string) string {
		if markup {
			return html.EscapeString(s)
		}
		return s
	}
	return strings.NewReplacer("{{TASK}}", escape(values.Task), "{{TOKEN}}", escape(values.Token),
		"{{DELIVERED}}", escape(values.Delivered)).Replace(text)
}

// isHTMLType 判断模板部件声明的媒体类型是否为 text/html（不区分大小写，忽略参数）。
func isHTMLType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "text/html"
}

// wantLeaves 按模板列出替换占位符之后应得到的叶子部件：多部件深度优先展开，文字为各行以声明的换行连接并以换行结尾，
// 多行的可选占位符按部件声明的换行展开。
func wantLeaves(p part, values Values, optional map[string][]string) []leaf {
	if len(p.Parts) > 0 {
		var leaves []leaf
		for _, child := range p.Parts {
			leaves = append(leaves, wantLeaves(child, values, optional)...)
		}
		return leaves
	}
	newline := "\r\n"
	if p.Newline == "lf" {
		newline = "\n"
	}
	mediaType := p.Type
	if mediaType == "" {
		mediaType = "text/plain"
	}
	text := expectText(strings.Join(p.Lines, newline)+newline, values, optional, isHTMLType(p.Type), newline)
	return []leaf{{mediaType: mediaType, charset: p.Charset, encoding: strings.ToLower(p.Encoding), text: text}}
}

// wantHeaderText 返回模板中一个头部替换占位符、解码编码词之后应有的文字。
func wantHeaderText(h header, values Values, optional map[string][]string) string {
	if len(h.Words) == 0 {
		return expectText(h.Value, values, optional, false, "")
	}
	var text strings.Builder
	for _, w := range h.Words {
		text.WriteString(expectText(w.Raw+w.Text, values, optional, false, ""))
	}
	return text.String()
}

// TestNamesMatchTemplateFiles 确认 Names 恰好列出本目录下的全部 JSON 模板（按名称排序），且契约点名的样本都在其中。
func TestNamesMatchTemplateFiles(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read directory: %v", err)
	}
	var want []string
	for _, entry := range entries {
		if name, ok := strings.CutSuffix(entry.Name(), ".json"); ok {
			want = append(want, name)
		}
	}
	if got := Names(); !slices.Equal(got, want) {
		t.Fatalf("Names() = %q, want %q", got, want)
	}
	for _, name := range append(slices.Clone(l1Samples), "foxmail-header-block", "apple-mail-wrote", "gmail-wrote", "gbk-four-byte",
		"signature-dash-iphone", "html-only-reply", "footer-without-separator", "inline-reply-wrote", "inline-reply-bare",
		"user-quoted-command", "wrote-in-body") {
		if !slices.Contains(want, name) {
			t.Errorf("sample %s is missing", name)
		}
	}
}

// TestLoadEveryTemplate 确认每个模板都能加载：名称、来源、说明与二选一的期望结果都合法，期望的错误名在允许的范围内，
// 契约点名的四份 L1 样本标为 l1，其余标为 public。
func TestLoadEveryTemplate(t *testing.T) {
	samples, err := All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(samples) != len(Names()) {
		t.Fatalf("All returned %d samples, Names lists %d", len(samples), len(Names()))
	}
	for i, s := range samples {
		if s.Name != Names()[i] {
			t.Errorf("sample %d is named %q, want %q", i, s.Name, Names()[i])
		}
		wantSource := SourcePublic
		if slices.Contains(l1Samples, s.Name) {
			wantSource = SourceL1
		}
		if s.Source != wantSource {
			t.Errorf("%s: source %q, want %q", s.Name, s.Source, wantSource)
		}
		if s.Note == "" {
			t.Errorf("%s: empty note", s.Name)
		}
		if (s.Expect.NewText == "") == (s.Expect.Error == "") {
			t.Errorf("%s: want exactly one of new_text and error", s.Name)
		}
		if s.Expect.Error != "" && !slices.Contains(ErrorNames, s.Expect.Error) {
			t.Errorf("%s: unexpected error name %q", s.Name, s.Expect.Error)
		}
	}
	if _, err := Load("no-such-sample"); err == nil {
		t.Error("Load accepts a missing sample")
	}
}

// TestBuildDecodesBack 组装每个样本，用标准库独立解码：模板中的每个头部（编码词解码后）与每个叶子部件的媒体类型、字符集参数、
// 传输编码与文字都等于替换占位符之后的模板（可选占位符取模板的默认值）；组装结果中不再有占位符，令牌只以编码后的形式出现在字节里。
func TestBuildDecodesBack(t *testing.T) {
	values := testValues()
	for _, name := range Names() {
		s, err := Load(name)
		if err != nil {
			t.Fatalf("Load(%s): %v", name, err)
		}
		raw, err := s.Build(values)
		if err != nil {
			t.Fatalf("%s: Build: %v", name, err)
		}
		if bytes.Contains(raw, []byte("{{")) {
			t.Errorf("%s: assembled bytes still contain a placeholder", name)
		}
		got := decodeMail(t, raw)
		for _, h := range s.message.Headers {
			if text, want := decodeHeaderText(t, got.header.Get(h.Name)), wantHeaderText(h, values, s.defaults); text != want {
				t.Errorf("%s: header %s decodes to %q, want %q", name, h.Name, text, want)
			}
		}
		want := wantLeaves(s.message, values, s.defaults)
		if len(got.leaves) != len(want) {
			t.Fatalf("%s: %d leaves, want %d", name, len(got.leaves), len(want))
		}
		for i := range want {
			if got.leaves[i] != want[i] {
				t.Errorf("%s: leaf %d = %+v, want %+v", name, i, got.leaves[i], want[i])
			}
		}
	}
}

// TestBuildIsDeterministic 确认同一模板与同一组取值总是组装出相同的字节，这是解析器确定性测试的前提。
func TestBuildIsDeterministic(t *testing.T) {
	for _, name := range Names() {
		s, err := Load(name)
		if err != nil {
			t.Fatalf("Load(%s): %v", name, err)
		}
		first, err := s.Build(testValues())
		if err != nil {
			t.Fatalf("%s: Build: %v", name, err)
		}
		second, err := s.Build(testValues())
		if err != nil || !bytes.Equal(first, second) {
			t.Errorf("%s: two builds differ (err %v)", name, err)
		}
	}
}

// TestL1TemplatesMatchRecordedStructure 按标准库解码后的结果，核对四份 L1 样本符合契约记下的实测结构。
func TestL1TemplatesMatchRecordedStructure(t *testing.T) {
	values := testValues()
	build := func(name string) (decodedMail, string) {
		s, err := Load(name)
		if err != nil {
			t.Fatalf("Load(%s): %v", name, err)
		}
		raw, err := s.Build(values)
		if err != nil {
			t.Fatalf("%s: Build: %v", name, err)
		}
		got := decodeMail(t, raw)
		return got, decodeHeaderText(t, got.header.Get("Subject"))
	}
	tag := "[TC " + values.Task + " " + values.Token + "]"
	for name, prefix := range map[string]string{"qq-app-reply": "Re: ", "qq-web-reply": "回复："} {
		got, subject := build(name)
		if got.mediaType != "multipart/alternative" || len(got.leaves) != 2 {
			t.Fatalf("%s: %s with %d leaves, want multipart/alternative with two", name, got.mediaType, len(got.leaves))
		}
		for i, want := range []string{"text/plain", "text/html"} {
			if l := got.leaves[i]; l.mediaType != want || l.charset != "utf-8" || l.encoding != "base64" {
				t.Errorf("%s: leaf %d is %s %s/%s, want %s utf-8/base64", name, i, l.mediaType, l.charset, l.encoding, want)
			}
		}
		if !strings.HasPrefix(got.header.Get("Subject"), "=?utf-8?B?") || !strings.HasPrefix(subject, prefix+tag) {
			t.Errorf("%s: subject is not a utf-8/B encoded %q followed by the tag", name, prefix)
		}
		if got.header.Get("In-Reply-To") != values.Delivered || got.header.Get("References") != values.Delivered {
			t.Errorf("%s: thread headers do not both equal the delivered ID", name)
		}
		if strings.Contains(got.leaves[1].text, "<blockquote") {
			t.Errorf("%s: HTML uses blockquote", name)
		}
	}
	app, _ := build("qq-app-reply")
	plainLines := strings.Split(app.leaves[0].text, "\r\n")
	if !slices.Contains(plainLines, "------------------ 原始邮件 ------------------") || !slices.Contains(plainLines, values.Token) ||
		!slices.ContainsFunc(plainLines, func(line string) bool { return strings.HasPrefix(line, "主题: ") && strings.Contains(line, tag) }) {
		t.Error("qq-app-reply: plain text lacks the separator, the unprefixed token line or the tagged subject line of the quoted header block")
	}
	web, _ := build("qq-web-reply")
	if strings.Contains(web.leaves[0].text, values.Token) || strings.Contains(web.leaves[1].text, values.Token) {
		t.Error("qq-web-reply: the body quotes the notification")
	}
	vacation, subject := build("qq-vacation-auto-reply")
	if vacation.mediaType != "text/html" || len(vacation.leaves) != 1 || !strings.HasPrefix(subject, "自动回复: "+tag) {
		t.Errorf("qq-vacation-auto-reply: %s, subject prefix mismatch", vacation.mediaType)
	}
	if vacation.header.Get("In-Reply-To") != values.Delivered || vacation.header.Get("References") != "" {
		t.Error("qq-vacation-auto-reply: want In-Reply-To only")
	}
	for _, key := range []string{"Auto-Submitted", "X-Autoreply", "X-Autorespond", "Precedence", "Return-Path"} {
		if _, ok := vacation.header[textproto.CanonicalMIMEHeaderKey(key)]; ok {
			t.Errorf("qq-vacation-auto-reply: has %s", key)
		}
	}
	bounce, subject := build("qq-bounce")
	if bounce.mediaType != "multipart/report" || len(bounce.leaves) != 2 ||
		bounce.leaves[0].mediaType != "text/html" || bounce.leaves[1].mediaType != "application/octet-stream" {
		t.Errorf("qq-bounce: %s with leaves %+v", bounce.mediaType, bounce.leaves)
	}
	if bounce.header.Get("Auto-Submitted") != "auto-generated" || strings.Contains(subject, "[TC") ||
		bounce.header.Get("In-Reply-To") != values.Delivered || bounce.header.Get("References") != values.Delivered {
		t.Error("qq-bounce: Auto-Submitted, subject or thread headers differ from the recorded structure")
	}
	from, err := mail.ParseAddress(decodeHeaderText(t, bounce.header.Get("From")))
	if err != nil || !strings.ContainsFunc(from.Address, unicode.IsUpper) {
		t.Errorf("qq-bounce: sender %v should contain upper-case letters (err %v)", from, err)
	}
}

// TestBuildRejectsMissingValues 确认模板用到的占位符缺少取值、或取值含换行与占位符时，Build 返回错误且错误文本不含取值。
func TestBuildRejectsMissingValues(t *testing.T) {
	s, err := Load("qq-app-reply")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	full := testValues()
	cases := map[string]Values{
		"no task":          {Token: full.Token, Delivered: full.Delivered},
		"no token":         {Task: full.Task, Delivered: full.Delivered},
		"no delivered":     {Task: full.Task, Token: full.Token},
		"newline in value": {Task: full.Task, Token: full.Token, Delivered: full.Delivered + "\r\nBcc: x@example.invalid"},
		"placeholder":      {Task: "{{TOKEN}}", Token: full.Token, Delivered: full.Delivered},
	}
	for name, values := range cases {
		raw, err := s.Build(values)
		if err == nil || raw != nil {
			t.Errorf("%s: Build succeeded", name)
			continue
		}
		if strings.Contains(err.Error(), full.Token) || strings.Contains(err.Error(), "Bcc") {
			t.Errorf("%s: error echoes a value", name)
		}
	}
	web, err := Load("qq-vacation-auto-reply")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := web.Build(Values{Task: full.Task, Token: full.Token, Delivered: full.Delivered}); err != nil {
		t.Errorf("Build with every value: %v", err)
	}
}

// TestBuildChecksPlaceholderUse 确认只检查模板实际用到的占位符：只出现在子部件正文或头部片段里的占位符缺少取值时同样报错，
// 没有用到任何占位符的模板在取值全空时照常组装。
func TestBuildChecksPlaceholderUse(t *testing.T) {
	cases := map[string]string{
		"nested lines": strings.Replace(minimalTemplate, `"type": "text/plain", "charset": "utf-8", "encoding": "8bit", "lines": ["正文"]`,
			`"type": "multipart/mixed", "parts": [{"lines": ["x"]}, {"lines": ["{{TOKEN}}"]}]`, 1),
		"header word": strings.Replace(minimalTemplate, `"value": "a@example.invalid"`, `"words": [{"raw": "x"}, {"raw": "{{DELIVERED}}"}]`, 1),
	}
	for name, data := range cases {
		s, err := decode(name, []byte(data))
		if err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		if _, err := s.Build(Values{Task: "q7m3k9x2pa"}); err == nil {
			t.Errorf("%s: Build without the used value succeeded", name)
		}
	}
	s, err := decode("plain", []byte(minimalTemplate))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, err := s.Build(Values{}); err != nil {
		t.Errorf("a template without placeholders needs no values: %v", err)
	}
}

// minimalTemplate 是一个最小的合法模板，各拒绝用例在它的基础上替换一处。
const minimalTemplate = `{"source": "public", "note": "测试", "message": {"headers": [{"name": "From", "value": "a@example.invalid"}], ` +
	`"type": "text/plain", "charset": "utf-8", "encoding": "8bit", "lines": ["正文"]}, "expect": {"new_text": "正文"}}`

// TestDecodeRejectsInvalidTemplates 确认模板的格式错误在加载时就被拒绝：未知字段、尾随内容、来源与说明、期望结果、
// 头部与编码词、部件的结构、字符集、传输编码、换行与参数（没有媒体类型的部件不能带字符集与参数）。
func TestDecodeRejectsInvalidTemplates(t *testing.T) {
	if _, err := decode("minimal", []byte(minimalTemplate)); err != nil {
		t.Fatalf("minimal template rejected: %v", err)
	}
	replace := func(old, new string) string {
		if !strings.Contains(minimalTemplate, old) {
			t.Fatalf("minimal template lacks %q", old)
		}
		return strings.Replace(minimalTemplate, old, new, 1)
	}
	cases := map[string]string{
		"not json":             "{",
		"unknown field":        replace(`"note": "测试"`, `"note": "测试", "extra": 1`),
		"trailing data":        minimalTemplate + "{}",
		"unknown source":       replace(`"public"`, `"guess"`),
		"empty note":           replace(`"测试"`, `""`),
		"no expectation":       replace(`{"new_text": "正文"}`, `{}`),
		"both expectations":    replace(`{"new_text": "正文"}`, `{"new_text": "正文", "error": "ErrEmpty"}`),
		"unknown error name":   replace(`{"new_text": "正文"}`, `{"error": "ErrOther"}`),
		"placeholder expect":   replace(`{"new_text": "正文"}`, `{"new_text": "{{TASK}}"}`),
		"header without name":  replace(`"name": "From"`, `"name": ""`),
		"header name colon":    replace(`"name": "From"`, `"name": "From:"`),
		"value and words":      replace(`"value": "a@example.invalid"`, `"value": "a", "words": [{"raw": "b"}]`),
		"empty word":           replace(`"value": "a@example.invalid"`, `"words": [{}]`),
		"raw and text":         replace(`"value": "a@example.invalid"`, `"words": [{"raw": "a", "text": "b", "charset": "utf-8"}]`),
		"raw with charset":     replace(`"value": "a@example.invalid"`, `"words": [{"raw": "a", "charset": "utf-8"}]`),
		"value line break":     replace(`"value": "a@example.invalid"`, `"value": "a@example.invalid\r\nBcc: b@example.invalid"`),
		"word line break":      replace(`"value": "a@example.invalid"`, `"words": [{"text": "文字\n", "charset": "utf-8"}]`),
		"text no charset":      replace(`"value": "a@example.invalid"`, `"words": [{"text": "文字"}]`),
		"word charset":         replace(`"value": "a@example.invalid"`, `"words": [{"text": "文字", "charset": "big5"}]`),
		"body charset":         replace(`"charset": "utf-8"`, `"charset": "latin1"`),
		"body encoding":        replace(`"encoding": "8bit"`, `"encoding": "uuencode"`),
		"newline":              replace(`"lines"`, `"newline": "cr", "lines"`),
		"lf with qp":           replace(`"encoding": "8bit"`, `"encoding": "quoted-printable", "newline": "lf"`),
		"charset without type": replace(`"type": "text/plain", `, ``),
		"params without type":  replace(`"type": "text/plain", "charset": "utf-8"`, `"params": {"format": "flowed"}`),
		"type without slash":   replace(`"type": "text/plain"`, `"type": "text"`),
		"unparsable type":      replace(`"type": "text/plain"`, `"type": "text/plain; x"`),
		"leaf with parts":      replace(`"lines": ["正文"]`, `"lines": ["正文"], "parts": [{"type": "text/plain"}]`),
		"leaf with boundary":   replace(`"lines"`, `"boundary": "b", "lines"`),
		"charset param":        replace(`"lines"`, `"params": {"charset": "utf-8"}, "lines"`),
		"boundary param":       replace(`"lines"`, `"params": {"Boundary": "b"}, "lines"`),
		"empty param":          replace(`"lines"`, `"params": {"": "x"}, "lines"`),
		"multipart no parts":   replace(`"type": "text/plain", "charset": "utf-8", "encoding": "8bit", "lines": ["正文"]`, `"type": "multipart/mixed"`),
		"multipart lines":      replace(`"type": "text/plain", "charset": "utf-8", "encoding": "8bit"`, `"type": "multipart/mixed", "parts": [{"lines": ["a"]}]`),
		"multipart charset":    replace(`"type": "text/plain", "charset": "utf-8", "encoding": "8bit", "lines": ["正文"]`, `"type": "multipart/mixed", "charset": "utf-8", "parts": [{"lines": ["a"]}]`),
		"multipart encoding":   replace(`"type": "text/plain", "charset": "utf-8", "encoding": "8bit", "lines": ["正文"]`, `"type": "multipart/mixed", "encoding": "base64", "parts": [{"lines": ["a"]}]`),
		"multipart newline":    replace(`"type": "text/plain", "charset": "utf-8", "encoding": "8bit", "lines": ["正文"]`, `"type": "multipart/mixed", "newline": "lf", "parts": [{"lines": ["a"]}]`),
		"invalid child":        replace(`"type": "text/plain", "charset": "utf-8", "encoding": "8bit", "lines": ["正文"]`, `"type": "multipart/mixed", "parts": [{"encoding": "x"}]`),
		"invalid child param":  replace(`"type": "text/plain", "charset": "utf-8", "encoding": "8bit", "lines": ["正文"]`, `"type": "multipart/mixed", "parts": [{"headers": [{"name": ""}]}]`),
	}
	for name, data := range cases {
		if _, err := decode(name, []byte(data)); err == nil {
			t.Errorf("%s: decode accepted the template", name)
		}
	}
}

// TestBuildRejectsInvalidContent 确认只有在组装时才能发现的问题使 Build 失败：未知的占位符、us-ascii 或 7bit 部件含非 ASCII、
// 编码词的字符集编不出文字、非法的媒体类型、分隔串出现在部件内容中；合法的变体（空的头部取值、没有 Content-Type 的部件、
// 自动生成的分隔串）照常组装。
func TestBuildRejectsInvalidContent(t *testing.T) {
	build := func(data string) ([]byte, error) {
		s, err := decode("inline", []byte(data))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		return s.Build(testValues())
	}
	replace := func(old, new string) string {
		return strings.Replace(minimalTemplate, old, new, 1)
	}
	bad := map[string]string{
		"unknown placeholder": replace(`["正文"]`, `["{{OTHER}}"]`),
		"us-ascii non-ascii":  replace(`"charset": "utf-8"`, `"charset": "us-ascii"`),
		"7bit non-ascii":      replace(`"encoding": "8bit"`, `"encoding": "7bit"`),
		"word us-ascii":       replace(`"value": "a@example.invalid"`, `"words": [{"text": "文字", "charset": "us-ascii"}]`),
		"invalid param name":  replace(`"lines"`, `"params": {"bad name": "x"}, "lines"`),
		"boundary in content": replace(`"type": "text/plain", "charset": "utf-8", "encoding": "8bit", "lines": ["正文"]`,
			`"type": "multipart/mixed", "boundary": "b1", "parts": [{"lines": ["--b1"]}]`),
		"nested boundary clash": replace(`"type": "text/plain", "charset": "utf-8", "encoding": "8bit", "lines": ["正文"]`,
			`"type": "multipart/mixed", "boundary": "b1", "parts": [{"type": "multipart/alternative", "boundary": "b1x", "parts": [{"lines": ["x"]}]}]`),
		"child error": replace(`"type": "text/plain", "charset": "utf-8", "encoding": "8bit", "lines": ["正文"]`,
			`"type": "multipart/mixed", "parts": [{"encoding": "7bit", "lines": ["正文"]}]`),
		"header placeholder": replace(`"value": "a@example.invalid"`, `"value": "{{OTHER}}"`),
		"word placeholder":   replace(`"value": "a@example.invalid"`, `"words": [{"raw": "{{OTHER}}"}]`),
	}
	for name, data := range bad {
		if raw, err := build(data); err == nil || raw != nil {
			t.Errorf("%s: Build succeeded", name)
		}
	}
	if _, err := encodeCharset("文字", "big5"); err == nil {
		t.Error("encodeCharset accepts an unsupported label")
	}
	good := map[string]string{
		"empty header value": replace(`"value": "a@example.invalid"`, `"value": ""`),
		"no content type":    replace(`"type": "text/plain", "charset": "utf-8", `, ``),
		"generated boundary": replace(`"type": "text/plain", "charset": "utf-8", "encoding": "8bit", "lines": ["正文"]`,
			`"type": "multipart/mixed", "parts": [{"type": "multipart/alternative", "parts": [{"lines": ["x"]}, {"type": "text/html", "lines": ["<p>y</p>"]}]}]`),
		"ascii 7bit":        replace(`"encoding": "8bit", "lines": ["正文"]`, `"encoding": "7bit", "lines": ["plain"]`),
		"us-ascii text":     replace(`"charset": "utf-8", "encoding": "8bit", "lines": ["正文"]`, `"charset": "us-ascii", "encoding": "7bit", "lines": ["plain"]`),
		"gb18030 body":      replace(`"charset": "utf-8"`, `"charset": "GB18030"`),
		"disposition":       replace(`"lines"`, `"disposition": "attachment; filename=\"a.txt\"", "lines"`),
		"part headers":      replace(`"lines"`, `"headers": [{"name": "Content-ID", "value": "<a@example.invalid>"}], "lines"`),
		"long encoded word": replace(`"value": "a@example.invalid"`, `"words": [{"text": "`+strings.Repeat("很长的主题", 20)+`", "charset": "gbk"}]`),
	}
	for name, data := range good {
		raw, err := build(data)
		if err != nil {
			t.Errorf("%s: Build: %v", name, err)
			continue
		}
		decodeMail(t, raw)
		for _, line := range strings.Split(string(raw), "\r\n") {
			if strings.HasPrefix(line, "=?") || strings.HasPrefix(line, " =?") || strings.HasPrefix(line, "From: =?") {
				if len(strings.TrimSpace(strings.TrimPrefix(line, "From:"))) > 75 {
					t.Errorf("%s: encoded word longer than 75 characters", name)
				}
			}
		}
	}
}

// replyOptional 是 qq-app-reply 声明的可选占位符；tests/qqsim 的 Reply 靠它们合成回复。
var replyOptional = []string{"FROM", "TO", "PREFIX", "SUBJECT", "MESSAGEID", "BODY", "QUOTED"}

// TestBuildOptionalPlaceholders 确认 qq-app-reply 声明了回复所需的可选占位符，且调用方的取值替换了默认值：
// 发件人、收件人、主题前缀加原主题、Message-Id 与线程头；纯文本中正文与引用原文按行展开，引用头块的地址与主题随之改变；
// HTML 部件中的取值逐行转义、以 <br> 连接。SUBJECT 与 QUOTED 都给出时，只出现在它们默认值里的 {{TASK}}、{{TOKEN}} 不再需要取值。
func TestBuildOptionalPlaceholders(t *testing.T) {
	s, err := Load("qq-app-reply")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range replyOptional {
		if _, ok := s.defaults[name]; !ok {
			t.Errorf("qq-app-reply declares no default for {{%s}}", name)
		}
	}
	subject := "[TC q7m3k9x2pa " + strings.Repeat("cd", 24) + "] 标题 & <b>"
	delivered := "<tencent_delivered_0002@example.invalid>"
	optional := map[string]string{
		"FROM": "someone@example.invalid", "TO": "robot@example.invalid", "PREFIX": "回复：", "SUBJECT": subject,
		"MESSAGEID": "<tencent_reply_0002@example.invalid>", "BODY": "第一行 <b>&\n\n第三行 \"引号\"", "QUOTED": "原文第一行\n原文 & 第二行",
	}
	raw, err := s.Build(Values{Delivered: delivered, Optional: optional})
	if err != nil {
		t.Fatalf("Build with every optional value and no task or token: %v", err)
	}
	got := decodeMail(t, raw)
	for key, want := range map[string]string{"From": "someone@example.invalid", "To": "robot@example.invalid"} {
		addr, err := mail.ParseAddress(decodeHeaderText(t, got.header.Get(key)))
		if err != nil || addr.Address != want {
			t.Errorf("%s = %v (err %v), want %s", key, addr, err, want)
		}
	}
	if text := decodeHeaderText(t, got.header.Get("Subject")); text != "回复："+subject {
		t.Errorf("Subject decodes to %q", text)
	}
	if got.header.Get("Message-Id") != optional["MESSAGEID"] || got.header.Get("In-Reply-To") != delivered ||
		got.header.Get("References") != delivered {
		t.Error("Message-Id or thread headers do not use the given values")
	}
	if len(got.leaves) != 2 {
		t.Fatalf("%d leaves, want 2", len(got.leaves))
	}
	wantPlain := strings.Join([]string{"第一行 <b>&", "", "第三行 \"引号\"", "", "", "", "------------------ 原始邮件 ------------------",
		"发件人: \"TurnCourier\" <robot@example.invalid>;", "发送时间: 2026年9月24日(星期三) 下午3:00",
		"收件人: \"用户\" <someone@example.invalid>;", "主题: " + subject, "", "原文第一行", "原文 & 第二行"}, "\r\n") + "\r\n"
	if got.leaves[0].text != wantPlain {
		t.Errorf("plain text = %q, want %q", got.leaves[0].text, wantPlain)
	}
	for _, want := range []string{
		"<div>第一行 &lt;b&gt;&amp;<br><br>第三行 &#34;引号&#34;</div>",
		"\"TurnCourier\"&lt;robot@example.invalid&gt;", "\"用户\"&lt;someone@example.invalid&gt;",
		"<b>主题:</b>&nbsp;" + html.EscapeString(subject) + "</div>", ">原文第一行<br>原文 &amp; 第二行</div>",
	} {
		if !strings.Contains(got.leaves[1].text, want) {
			t.Errorf("HTML lacks %q", want)
		}
	}
	if strings.Contains(got.leaves[1].text, "<b>&") || strings.Contains(got.leaves[1].text, subject) {
		t.Error("HTML contains an unescaped value")
	}
}

// TestBuildOptionalExplicitEmpty 确认调用方给出的空串照用而不回退到默认值：主题没有前缀、正文为空；没有给出的仍取默认值。
func TestBuildOptionalExplicitEmpty(t *testing.T) {
	s, err := Load("qq-app-reply")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	values := testValues()
	values.Optional = map[string]string{"PREFIX": "", "BODY": ""}
	raw, err := s.Build(values)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got := decodeMail(t, raw)
	if subject := decodeHeaderText(t, got.header.Get("Subject")); subject != "[TC "+values.Task+" "+values.Token+"] 回合完成：部署脚本已更新" {
		t.Errorf("Subject decodes to %q", subject)
	}
	if !strings.HasPrefix(got.leaves[0].text, "\r\n\r\n\r\n\r\n------------------ 原始邮件 ------------------\r\n") {
		t.Errorf("plain text does not start with the empty body: %q", got.leaves[0].text)
	}
	if !strings.Contains(got.leaves[0].text, "\r\n"+values.Token+"\r\n") {
		t.Error("the default quoted text lost its token line")
	}
}

// TestBuildOptionalVerbatim 确认调用方给出的可选取值原样写入、不再被当作模板：其中形如占位符的文字（固定的或可选的）照原样出现，
// 不会换成令牌或其他取值。替换一趟完成，引用任意原文的回复（tests/qqsim 的 Reply）因此不会被原文中的 {{ 打断，也不会泄漏取值。
func TestBuildOptionalVerbatim(t *testing.T) {
	s, err := Load("qq-app-reply")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	values := testValues()
	values.Optional = map[string]string{"BODY": "模板语法 {{TOKEN}} 与 {{FROM}} 原样保留", "QUOTED": "{{.Name}} {{"}
	raw, err := s.Build(values)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	plain := decodeMail(t, raw).leaves[0].text
	if !strings.HasPrefix(plain, "模板语法 {{TOKEN}} 与 {{FROM}} 原样保留\r\n") || !strings.HasSuffix(plain, "\r\n{{.Name}} {{\r\n") {
		t.Errorf("plain text = %q", plain)
	}
	if strings.Count(plain, values.Token) != 1 {
		t.Errorf("the token appears %d times; want once, in the quoted subject line", strings.Count(plain, values.Token))
	}
}

// TestBuildRejectsBadOptional 确认可选占位符的非法取值使 Build 失败且错误文本不含取值：没有声明的名称（含三个固定占位符）、
// 含 CR、以及用在头部的占位符取值含换行（会伪造出额外的头部）。
func TestBuildRejectsBadOptional(t *testing.T) {
	s, err := Load("qq-app-reply")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := map[string]map[string]string{
		"undeclared":        {"OTHER": "value"},
		"fixed name":        {"TASK": "q7m3k9x2pa"},
		"carriage return":   {"BODY": "first\r\nsecond"},
		"header line break": {"FROM": "a@example.invalid\nBcc: b@example.invalid"},
		"subject break":     {"SUBJECT": "title\nBcc: b@example.invalid"},
	}
	for name, optional := range cases {
		values := testValues()
		values.Optional = optional
		raw, err := s.Build(values)
		if err == nil || raw != nil {
			t.Errorf("%s: Build succeeded", name)
			continue
		}
		for _, value := range optional {
			if strings.Contains(err.Error(), value) || strings.Contains(err.Error(), "Bcc") || strings.Contains(err.Error(), "second") {
				t.Errorf("%s: error %q echoes the value", name, err)
			}
		}
	}
}

// defaultsTemplate 是带可选占位符的最小模板，%s 处填 defaults 对象；{{FROM}} 用在头部，{{BODY}} 用在正文行中。
const defaultsTemplate = `{"source": "public", "note": "测试", "defaults": %s, "message": {"headers": [{"name": "From", "value": "{{FROM}}"}], ` +
	`"type": "text/plain", "charset": "utf-8", "encoding": "8bit", "lines": ["开头", "{{BODY}}"]}, "expect": {"new_text": "正文"}}`

// TestDecodeRejectsBadDefaults 确认可选占位符的默认值在加载时校验：名称须为大写字母且不与固定占位符重名，至少一行，
// 模板必须用到它，多行的默认值不能用在头部，每行不含换行、只可含固定占位符。合法的多行默认值按行展开，没有取值时照常组装。
func TestDecodeRejectsBadDefaults(t *testing.T) {
	s, err := decode("defaults", fmt.Appendf(nil, defaultsTemplate, `{"FROM": ["a@example.invalid"], "BODY": ["第一行 {{TASK}}", "第二行"]}`))
	if err != nil {
		t.Fatalf("valid defaults rejected: %v", err)
	}
	if _, err := s.Build(Values{}); err == nil {
		t.Error("Build accepted a default that uses {{TASK}} without a task value")
	}
	raw, err := s.Build(Values{Task: "q7m3k9x2pa"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if want := "From: a@example.invalid\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" +
		"开头\r\n第一行 q7m3k9x2pa\r\n第二行\r\n"; string(raw) != want {
		t.Errorf("Build = %q, want %q", raw, want)
	}
	bad := map[string]string{
		"lower-case name":   `{"FROM": ["a"], "BODY": ["b"], "body": ["c"]}`,
		"digit in name":     `{"FROM": ["a"], "BODY": ["b"], "BODY2": ["c"]}`,
		"fixed name":        `{"FROM": ["a"], "BODY": ["b"], "TASK": ["c"]}`,
		"no lines":          `{"FROM": [], "BODY": ["b"]}`,
		"unused":            `{"FROM": ["a"], "BODY": ["b"], "EXTRA": ["c"]}`,
		"multi-line header": `{"FROM": ["a", "b"], "BODY": ["c"]}`,
		"line break":        `{"FROM": ["a"], "BODY": ["b\nc"]}`,
		"carriage return":   `{"FROM": ["a"], "BODY": ["b\rc"]}`,
		"optional inside":   `{"FROM": ["a"], "BODY": ["{{FROM}}"]}`,
		"unknown inside":    `{"FROM": ["a"], "BODY": ["{{OTHER}}"]}`,
		"not a list":        `{"FROM": "a", "BODY": ["b"]}`,
	}
	for name, defaults := range bad {
		if _, err := decode(name, fmt.Appendf(nil, defaultsTemplate, defaults)); err == nil {
			t.Errorf("%s: decode accepted the defaults", name)
		}
	}
}

// TestFixedPlaceholdersEscapedInHTML 确认固定占位符在 text/html 部件中按 HTML 转义后替换，在其他部件与头部中原样替换：
// 实际投递 ID 带尖括号，原样写进 HTML 会被当成标签。
func TestFixedPlaceholdersEscapedInHTML(t *testing.T) {
	const template = `{"source": "public", "note": "测试", "message": {"headers": [{"name": "In-Reply-To", "value": "{{DELIVERED}}"}], ` +
		`"type": "multipart/alternative", "parts": [{"type": "text/plain", "charset": "utf-8", "lines": ["{{DELIVERED}}"]}, ` +
		`{"type": "text/html", "charset": "utf-8", "lines": ["<p>{{DELIVERED}}</p>"]}]}, "expect": {"new_text": "正文"}}`
	s, err := decode("escape", []byte(template))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	values := testValues()
	raw, err := s.Build(values)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got := decodeMail(t, raw)
	if got.header.Get("In-Reply-To") != values.Delivered || got.leaves[0].text != values.Delivered+"\r\n" ||
		got.leaves[1].text != "<p>"+html.EscapeString(values.Delivered)+"</p>\r\n" {
		t.Errorf("header %q, leaves %+v", got.header.Get("In-Reply-To"), got.leaves)
	}
}

// readmeRow 匹配 README 样本表中的一行：样本名与来源两栏。
var readmeRow = regexp.MustCompile("^\\| `([a-z0-9-]+)` \\| (L1 实测结构|公开资料推断) \\|")

// TestReadmeListsEverySample 确认 README 的样本表恰好登记了每个样本各一次，来源一栏与模板的 source 一致。
func TestReadmeListsEverySample(t *testing.T) {
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	listed := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		match := readmeRow.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		if _, dup := listed[match[1]]; dup {
			t.Errorf("README lists %s twice", match[1])
		}
		listed[match[1]] = match[2]
	}
	labels := map[string]string{SourceL1: "L1 实测结构", SourcePublic: "公开资料推断"}
	samples, err := All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, s := range samples {
		if listed[s.Name] != labels[s.Source] {
			t.Errorf("README lists %s as %q, want %q", s.Name, listed[s.Name], labels[s.Source])
		}
		delete(listed, s.Name)
	}
	for name := range listed {
		t.Errorf("README lists %s, which has no template", name)
	}
}
