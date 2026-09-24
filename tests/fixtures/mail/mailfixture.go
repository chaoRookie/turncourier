// Package mailfixture 读取本目录下的合成回归样本模板，把占位符换成运行时的值，再按模板声明的字符集与传输编码组装为原始邮件字节，
// 供入站解析器、入站验证与离线闭环的测试共用。模板只保存头部与各部件解码后的文字，令牌、任务 ID 与实际投递 ID 用占位符表示，
// 格式见同目录的 README.md。组装只用标准库与 x/text，不经过被测解析器所用的 go-message，解析器读到的因此是独立产生的字节。
// 本包是测试基础设施，不进入产品二进制：模板嵌入在包内，不读写文件、不联网；错误文本不回显占位符的取值。
package mailfixture

import (
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"mime/quotedprintable"
	"slices"
	"strings"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// templates 是嵌入的全部样本模板。
//
//go:embed *.json
var templates embed.FS

const (
	// SourceL1 标记按 L1 真机探测实测的结构合成的样本。
	SourceL1 = "l1"
	// SourcePublic 标记按公开资料推断合成、待 L1b 或以后的真机样本核实的样本。
	SourcePublic = "public"
	// maxWordLen 是一个 RFC 2047 编码词的最大长度（RFC 2047 第 2 节）。
	maxWordLen = 75
	// base64LineLen 是 base64 传输编码每行的字符数（RFC 2045 第 6.8 节规定不超过 76）。
	base64LineLen = 76
)

// ErrorNames 是模板期望中允许出现的错误名，与 internal/mail/parser 中 NewText 的哨兵错误同名。
var ErrorNames = []string{"ErrNoPlainText", "ErrUncertain", "ErrEmpty", "ErrMalformed"}

// charsets 把模板允许的字符集标签（小写）映射到编码函数。gbk、gb2312 与 gb18030 一律用 GB18030 编码器：
// 它是前两者的超集，标注 gbk 而含四字节字符的来信正是这样产生的。
var charsets = map[string]func(string) ([]byte, error){
	"utf-8":    encodeUTF8,
	"us-ascii": encodeASCII,
	"gbk":      encodeGB18030,
	"gb2312":   encodeGB18030,
	"gb18030":  encodeGB18030,
}

// Values 是占位符的运行时取值；取值不得含换行或占位符。
type Values struct {
	Task      string // 替换 {{TASK}}：任务 ID
	Token     string // 替换 {{TOKEN}}：令牌文本，由测试用 security/token 签发
	Delivered string // 替换 {{DELIVERED}}：通知的实际投递 Message-ID，带尖括号
}

// Expect 是样本的期望结果：NewText 为解析器应得到的新正文，Error 为期望的错误名（ErrorNames 之一），二者恰好一个非空。
type Expect struct {
	NewText string `json:"new_text"`
	Error   string `json:"error"`
}

// Sample 是一份已加载并校验过的样本：名称（文件名去掉 .json）、来源（SourceL1 或 SourcePublic）、说明、期望结果与模板。
type Sample struct {
	Name    string
	Source  string
	Note    string
	Expect  Expect
	message part
}

// template 是模板 JSON 文件的结构。
type template struct {
	Source  string `json:"source"`
	Note    string `json:"note"`
	Message part   `json:"message"`
	Expect  Expect `json:"expect"`
}

// part 是模板中的一个 MIME 部件。根部件的 Headers 是整封邮件的头部；多部件用 Boundary（为空时自动生成）与 Parts，
// 叶子用 Charset、Encoding、Newline（crlf 或 lf，默认 crlf）与 Lines（解码后的文字，逐行给出）。
type part struct {
	Headers     []header          `json:"headers"`
	Type        string            `json:"type"`
	Params      map[string]string `json:"params"`
	Charset     string            `json:"charset"`
	Encoding    string            `json:"encoding"`
	Disposition string            `json:"disposition"`
	Boundary    string            `json:"boundary"`
	Newline     string            `json:"newline"`
	Lines       []string          `json:"lines"`
	Parts       []part            `json:"parts"`
}

// header 是模板中的一个头部：Value 原样写入，Words 依次拼接（与 Value 不能同时给出）。
type header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Words []word `json:"words"`
}

// word 是头部取值中的一个片段：Raw 原样写入；Text 按 Charset 编为 RFC 2047 的 B 编码词，过长时拆成多个编码词并折行。
type word struct {
	Raw     string `json:"raw"`
	Text    string `json:"text"`
	Charset string `json:"charset"`
}

// builder 在一次组装中替换占位符并记录自动生成的分隔串序号。
type builder struct {
	replace    *strings.Replacer
	boundaries int
}

// Names 返回全部样本的名称，按字典序排列。模板在编译时嵌入，读取嵌入目录不会失败。
func Names() []string {
	entries, _ := fs.ReadDir(templates, ".")
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if name, ok := strings.CutSuffix(entry.Name(), ".json"); ok {
			names = append(names, name)
		}
	}
	return names
}

// Load 加载并校验名为 name 的样本。
func Load(name string) (*Sample, error) {
	data, err := templates.ReadFile(name + ".json")
	if err != nil {
		return nil, fmt.Errorf("mailfixture: no sample named %q", name)
	}
	return decode(name, data)
}

// All 按名称顺序加载全部样本。
func All() ([]*Sample, error) {
	var samples []*Sample
	for _, name := range Names() {
		s, err := Load(name)
		if err != nil {
			return nil, err
		}
		samples = append(samples, s)
	}
	return samples, nil
}

// decode 严格解析一个模板（未知字段与尾随内容都拒绝）并校验它的结构。
func decode(name string, data []byte) (*Sample, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var t template
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("mailfixture: sample %q: %w", name, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("mailfixture: sample %q: trailing data after the template", name)
	}
	if err := t.validate(); err != nil {
		return nil, fmt.Errorf("mailfixture: sample %q: %w", name, err)
	}
	return &Sample{Name: name, Source: t.Source, Note: t.Note, Expect: t.Expect, message: t.Message}, nil
}

// validate 检查来源、说明与期望结果，再递归检查部件。
func (t template) validate() error {
	switch {
	case t.Source != SourceL1 && t.Source != SourcePublic:
		return errors.New("source must be l1 or public")
	case t.Note == "":
		return errors.New("note is empty")
	case (t.Expect.NewText == "") == (t.Expect.Error == ""):
		return errors.New("expect needs exactly one of new_text and error")
	case t.Expect.Error != "" && !slices.Contains(ErrorNames, t.Expect.Error):
		return errors.New("expect names an unknown error")
	case strings.Contains(t.Expect.NewText, "{{"):
		return errors.New("the expected new text contains a placeholder")
	}
	return t.Message.validate()
}

// validate 检查一个部件：头部合法；参数不含 charset 与 boundary（它们有专门的字段）；媒体类型为空或形如 type/subtype；
// 多部件必须有子部件、不能有叶子的字段；
// 叶子不能有子部件与分隔串，字符集、传输编码与换行都在支持的范围内，quoted-printable 不能保留单独的 LF（它把换行规范为 CRLF）。
func (p part) validate() error {
	for _, h := range p.Headers {
		if err := h.validate(); err != nil {
			return err
		}
	}
	for key := range p.Params {
		if key == "" || strings.EqualFold(key, "charset") || strings.EqualFold(key, "boundary") {
			return errors.New("params may not be empty, charset or boundary")
		}
	}
	if mediaType, _, err := mime.ParseMediaType(p.Type); p.Type != "" && (err != nil || !strings.Contains(mediaType, "/")) {
		return errors.New("the media type must have the form type/subtype")
	}
	if strings.HasPrefix(strings.ToLower(p.Type), "multipart/") {
		if len(p.Parts) == 0 || len(p.Lines) > 0 || p.Charset != "" || p.Encoding != "" || p.Newline != "" {
			return errors.New("a multipart part needs parts and no lines, charset, encoding or newline")
		}
		for _, child := range p.Parts {
			if err := child.validate(); err != nil {
				return err
			}
		}
		return nil
	}
	switch {
	case len(p.Parts) > 0 || p.Boundary != "":
		return errors.New("a leaf part may not have parts or a boundary")
	case p.Type == "" && (p.Charset != "" || len(p.Params) > 0):
		return errors.New("a part without a type may not have a charset or params")
	case p.Charset != "" && charsets[strings.ToLower(p.Charset)] == nil:
		return fmt.Errorf("unsupported charset %q", p.Charset)
	case !slices.Contains([]string{"", "base64", "quoted-printable", "7bit", "8bit"}, strings.ToLower(p.Encoding)):
		return fmt.Errorf("unsupported transfer encoding %q", p.Encoding)
	case p.Newline != "" && p.Newline != "crlf" && p.Newline != "lf":
		return errors.New("newline must be crlf or lf")
	case p.Newline == "lf" && strings.EqualFold(p.Encoding, "quoted-printable"):
		return errors.New("quoted-printable cannot keep bare LF line breaks")
	}
	return nil
}

// validate 检查一个头部：名称非空且不含冒号与空白；取值与片段都不含换行；Value 与 Words 不同时给出；
// 每个片段恰有 Raw 与 Text 之一，Raw 不带字符集，Text 的字符集在支持的范围内。
func (h header) validate() error {
	if h.Name == "" || strings.ContainsAny(h.Name, ": \t\r\n") {
		return errors.New("a header name is empty or has a colon or white space")
	}
	if strings.ContainsAny(h.Value, "\r\n") || (h.Value != "" && len(h.Words) > 0) {
		return fmt.Errorf("header %s has a line break, or both value and words", h.Name)
	}
	for _, w := range h.Words {
		switch {
		case (w.Raw == "") == (w.Text == ""):
			return fmt.Errorf("header %s: a word needs exactly one of raw and text", h.Name)
		case strings.ContainsAny(w.Raw+w.Text, "\r\n"):
			return fmt.Errorf("header %s: a word has a line break", h.Name)
		case w.Raw != "" && w.Charset != "":
			return fmt.Errorf("header %s: a raw word has no charset", h.Name)
		case w.Text != "" && charsets[strings.ToLower(w.Charset)] == nil:
			return fmt.Errorf("header %s: unsupported word charset %q", h.Name, w.Charset)
		}
	}
	return nil
}

// uses 判断部件（含子部件与头部）的文字中是否出现某个占位符。
func (p part) uses(placeholder string) bool {
	for _, h := range p.Headers {
		if strings.Contains(h.Value, placeholder) {
			return true
		}
		for _, w := range h.Words {
			if strings.Contains(w.Raw+w.Text, placeholder) {
				return true
			}
		}
	}
	for _, line := range p.Lines {
		if strings.Contains(line, placeholder) {
			return true
		}
	}
	for _, child := range p.Parts {
		if child.uses(placeholder) {
			return true
		}
	}
	return false
}

// replacer 返回把三个占位符换成本组取值的替换器。
func (v Values) replacer() *strings.Replacer {
	return strings.NewReplacer("{{TASK}}", v.Task, "{{TOKEN}}", v.Token, "{{DELIVERED}}", v.Delivered)
}

// check 确认取值不含换行与占位符（否则会伪造出额外的头部或无法识别的占位符），且样本用到的每个占位符都有取值；
// 错误只写占位符的名称，不写取值。
func (v Values) check(s *Sample) error {
	for _, pair := range [][2]string{{"{{TASK}}", v.Task}, {"{{TOKEN}}", v.Token}, {"{{DELIVERED}}", v.Delivered}} {
		placeholder, value := pair[0], pair[1]
		if strings.ContainsAny(value, "\r\n") || strings.Contains(value, "{{") {
			return fmt.Errorf("mailfixture: the value for %s contains a line break or a placeholder", placeholder)
		}
		if value == "" && s.message.uses(placeholder) {
			return fmt.Errorf("mailfixture: sample %q uses %s but no value was given", s.Name, placeholder)
		}
	}
	return nil
}

// Build 替换占位符并组装原始邮件字节（CRLF 换行）。同一模板与同一组取值总是得到相同的字节。
func (s *Sample) Build(v Values) ([]byte, error) {
	if err := v.check(s); err != nil {
		return nil, err
	}
	b := builder{replace: v.replacer()}
	var buf bytes.Buffer
	if err := b.writePart(&buf, s.message); err != nil {
		return nil, fmt.Errorf("mailfixture: sample %q: %w", s.Name, err)
	}
	return buf.Bytes(), nil
}

// text 替换一段文字中的占位符；替换之后仍有 {{ 说明模板用了未知的占位符。
func (b *builder) text(s string) (string, error) {
	out := b.replace.Replace(s)
	if strings.Contains(out, "{{") {
		return "", errors.New("unknown placeholder")
	}
	return out, nil
}

// writePart 写出一个部件：模板头部、Content-Type（带 charset、其他参数与多部件的分隔串）、传输编码与 Content-Disposition，
// 空行，再写正文或各子部件。子部件的字节中出现分隔线时返回错误，免得组装出有歧义的邮件。
func (b *builder) writePart(buf *bytes.Buffer, p part) error {
	for _, h := range p.Headers {
		if err := b.writeHeader(buf, h); err != nil {
			return err
		}
	}
	multipart := strings.HasPrefix(strings.ToLower(p.Type), "multipart/")
	boundary := p.Boundary
	if multipart && boundary == "" {
		b.boundaries++
		boundary = fmt.Sprintf("=_mailfixture_%d_=", b.boundaries)
	}
	if p.Type != "" {
		params := make(map[string]string, len(p.Params)+1)
		for key, value := range p.Params {
			params[key] = value
		}
		if p.Charset != "" {
			params["charset"] = p.Charset
		}
		if multipart {
			params["boundary"] = boundary
		}
		value := mime.FormatMediaType(p.Type, params)
		if value == "" {
			return errors.New("invalid media type or parameters")
		}
		buf.WriteString("Content-Type: " + value + "\r\n")
	}
	if p.Encoding != "" {
		buf.WriteString("Content-Transfer-Encoding: " + p.Encoding + "\r\n")
	}
	if p.Disposition != "" {
		buf.WriteString("Content-Disposition: " + p.Disposition + "\r\n")
	}
	buf.WriteString("\r\n")
	if !multipart {
		return b.writeBody(buf, p)
	}
	delimiter := []byte("--" + boundary)
	for _, child := range p.Parts {
		var childBuf bytes.Buffer
		if err := b.writePart(&childBuf, child); err != nil {
			return err
		}
		if bytes.Contains(childBuf.Bytes(), delimiter) {
			return errors.New("the boundary appears inside a part")
		}
		buf.WriteString("--" + boundary + "\r\n")
		buf.Write(childBuf.Bytes())
		buf.WriteString("\r\n")
	}
	buf.WriteString("--" + boundary + "--\r\n")
	return nil
}

// writeHeader 写出一个头部：Value 原样；Words 中 Raw 原样、Text 编为编码词，同一段 Text 拆出的多个编码词之间折行。
func (b *builder) writeHeader(buf *bytes.Buffer, h header) error {
	value, err := b.text(h.Value)
	if err != nil {
		return err
	}
	for _, w := range h.Words {
		piece, err := b.text(w.Raw + w.Text)
		if err != nil {
			return err
		}
		if w.Raw == "" {
			words, err := encodeWords(piece, w.Charset)
			if err != nil {
				return err
			}
			piece = strings.Join(words, "\r\n ")
		}
		value += piece
	}
	buf.WriteString(h.Name + ": " + value + "\r\n")
	return nil
}

// writeBody 写出叶子部件的正文：各行以声明的换行连接并以换行结尾，按字符集编码，再做传输编码。
func (b *builder) writeBody(buf *bytes.Buffer, p part) error {
	newline := "\r\n"
	if p.Newline == "lf" {
		newline = "\n"
	}
	content, err := b.text(strings.Join(p.Lines, newline) + newline)
	if err != nil {
		return err
	}
	data, err := encodeCharset(content, p.Charset)
	if err != nil {
		return err
	}
	encoded, err := transferEncode(data, p.Encoding)
	if err != nil {
		return err
	}
	buf.Write(encoded)
	return nil
}

// encodeWords 把文字按字符集编成一个或多个 B 编码词，每个不超过 75 个字符；按字符切分，不会把一个多字节字符拆到两个词里。
func encodeWords(text, charset string) ([]string, error) {
	prefix, suffix := "=?"+charset+"?B?", "?="
	maxBytes := (maxWordLen - len(prefix) - len(suffix)) / 4 * 3
	var words []string
	var chunk []byte
	for _, r := range text {
		encoded, err := encodeCharset(string(r), charset)
		if err != nil {
			return nil, err
		}
		if len(chunk) > 0 && len(chunk)+len(encoded) > maxBytes {
			words = append(words, prefix+base64.StdEncoding.EncodeToString(chunk)+suffix)
			chunk = nil
		}
		chunk = append(chunk, encoded...)
	}
	return append(words, prefix+base64.StdEncoding.EncodeToString(chunk)+suffix), nil
}

// encodeCharset 按字符集标签（不区分大小写，空标签按 UTF-8）编码文字。
func encodeCharset(text, label string) ([]byte, error) {
	if label == "" {
		return encodeUTF8(text)
	}
	encode := charsets[strings.ToLower(label)]
	if encode == nil {
		return nil, fmt.Errorf("unsupported charset %q", label)
	}
	return encode(text)
}

// encodeUTF8 原样返回 UTF-8 字节。
func encodeUTF8(text string) ([]byte, error) {
	return []byte(text), nil
}

// encodeASCII 要求文字只含 ASCII 字符。
func encodeASCII(text string) ([]byte, error) {
	if !isASCII([]byte(text)) {
		return nil, errors.New("us-ascii text has a non-ASCII character")
	}
	return []byte(text), nil
}

// encodeGB18030 用 GB18030 编码器编码文字。
func encodeGB18030(text string) ([]byte, error) {
	out, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(text))
	if err != nil {
		return nil, errors.New("text cannot be encoded as GB18030")
	}
	return out, nil
}

// transferEncode 按传输编码编码部件字节：base64 每 76 个字符一行；quoted-printable 用标准库（换行规范为 CRLF）；
// 7bit 要求全是 ASCII；8bit 与空编码原样。
func transferEncode(data []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(encoding) {
	case "base64":
		text := base64.StdEncoding.EncodeToString(data)
		var out strings.Builder
		for len(text) > base64LineLen {
			out.WriteString(text[:base64LineLen] + "\r\n")
			text = text[base64LineLen:]
		}
		out.WriteString(text + "\r\n")
		return []byte(out.String()), nil
	case "quoted-printable":
		var out bytes.Buffer
		w := quotedprintable.NewWriter(&out)
		// 写入 bytes.Buffer 不会失败，quotedprintable.Writer 的错误只可能来自底层写入。
		_, _ = w.Write(data)
		_ = w.Close()
		return out.Bytes(), nil
	case "7bit":
		if !isASCII(data) {
			return nil, errors.New("7bit content has a non-ASCII byte")
		}
	}
	return data, nil
}

// isASCII 判断字节是否全在 ASCII 范围内。
func isASCII(data []byte) bool {
	for _, c := range data {
		if c >= 0x80 {
			return false
		}
	}
	return true
}
