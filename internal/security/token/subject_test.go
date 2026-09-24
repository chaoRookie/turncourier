// Package token 测试主题标签：严格文法的表驱动用例、往返、Reveal 的形状、NewTag 的拒绝、零值、脱敏金丝雀，
// 以及以逐字节参考实现为准的模糊测试。令牌都由 Issue 在运行时签发（密钥材料用 bytes.Repeat 构造），版本字节或 kid
// 不合法的令牌文本由运行时构造的 30 个原始字节编码得到，源码中不出现令牌或标签形状的字面量。
package token

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// tagMarker 是规格规定的标签脱敏文本，直接写出以钉住输出，不引用被测包的常量。
const tagMarker = "[redacted subject tag]"

// tagHolder 含导出的标签字段，用来确认 fmt 与 slog 格式化嵌套字段时同样调用标签的格式化方法。
type tagHolder struct {
	T Tag
}

// tagEmbedder 以嵌入字段持有标签，另有一个普通字段；嵌入使标签的格式化方法提升到外层结构体。
type tagEmbedder struct {
	Tag
	Note string
}

// subjectCase 是文法表中的一行：主题、期望的错误（nil 表示接受）与接受时期望得到的标签。
type subjectCase struct {
	name    string
	subject string
	err     error
	want    Tag
}

// issueToken 以 seed 重复 32 次作密钥材料、以 kid 为密钥号，为 taskID 签发令牌；nid 取从 seed 起的 12 个字节，
// 不同的 seed 因此得到不同的令牌。
func issueToken(tb testing.TB, taskID string, kid, seed byte) Token {
	tb.Helper()
	k, err := NewKey(kid, bytes.Repeat([]byte{seed}, KeyLen))
	if err != nil {
		tb.Fatalf("NewKey: %v", err)
	}
	issued, err := Issue(k, NID(sequence(12, seed)), Claims{TaskID: taskID, Owner: "local", ExpiresAt: testExpiry})
	if err != nil {
		tb.Fatalf("Issue: %v", err)
	}
	return issued
}

// issueTag 签发令牌（见 issueToken）并与同一个任务 ID 构造主题标签。
func issueTag(tb testing.TB, taskID string, kid, seed byte) Tag {
	tb.Helper()
	tag, err := NewTag(taskID, issueToken(tb, taskID, kid, seed))
	if err != nil {
		tb.Fatalf("NewTag: %v", err)
	}
	return tag
}

// rawText 把版本字节、kid 与 28 个递增字节拼成 30 字节的原始令牌，再用与本包相同的 base32 编码为 48 个字母表字符；
// 用来不经过 Issue 构造文法命中的令牌文本，版本字节与 kid 可以不合法，MAC 部分是任意字节。
func rawText(version, kid byte) string {
	return refEncoding.EncodeToString(append([]byte{version, kid}, sequence(28, 0x40)...))
}

// subjectCases 在运行时用两枚不同的标签拼出文法表：恰好一个标签的各种上下文、没有标签、多个标签、标签内的空白被改动、
// 任务 ID 与令牌的长度或大小写不对、字母表之外的字符、版本字节与 kid 不合法，以及 [TC 出现在其他文字里。
func subjectCases(tb testing.TB) []subjectCase {
	tb.Helper()
	a := issueTag(tb, "0123456789", 1, 0x11)
	b := issueTag(tb, "zyxwvtsrqp", 1, 0x22)
	tagA, tagB := a.Reveal(), b.Reveal()
	idA, idB := a.TaskID(), b.TaskID()
	textA, textB := a.RevealToken(), b.RevealToken()
	upperB := strings.ToUpper(textB)
	if upperB == textB || strings.ToUpper(textA) == textA || strings.ToUpper(idB) == idB {
		tb.Fatal("test tokens or task ids have no letters to change case")
	}
	// build 用给定的开头、任务 ID、分隔与令牌文本拼出一个标签形状的串，结尾是 ]。
	build := func(open, id, sep, text string) string {
		return open + id + sep + text + "]"
	}
	// anyMAC 是版本字节与 kid 都合法、MAC 部分任意的令牌：ParseSubject 只检查格式，不验证签名，所以照样接受。
	anyMAC := rawText(0x01, 0x01)
	raw := append([]byte{0x01, 0x01}, sequence(28, 0x40)...)
	anyMACTag := Tag{taskID: idA, token: Token{kid: 1, nid: NID(raw[2:14]), tag: [16]byte(raw[14:])}}

	cases := []subjectCase{
		{name: "单独的标签", subject: tagA, want: a},
		{name: "Re: 前缀与标题", subject: "Re: " + tagA + " 标题", want: a},
		{name: "回复：前缀", subject: "回复：" + tagA + " 部署完成", want: a},
		{name: "多重前缀", subject: "回复：Re: " + tagA, want: a},
		{name: "紧贴中文", subject: "关于" + tagA + "的回复", want: a},
		{name: "全角空格包围", subject: "\u3000" + tagA + "\u3000标题", want: a},
		{name: "标签在末尾", subject: "部署完成 " + tagA, want: a},
		{name: "另有不完整的 [TC", subject: tagA + " [TC " + idA, want: a},
		{name: "另有令牌为大写的标签", subject: tagA + " " + build("[TC ", idB, " ", upperB), want: a},
		{name: "另有 [tcp] 文字", subject: "[tcp] " + tagA, want: a},
		{name: "MAC 任意的合法格式", subject: "Re: " + build("[TC ", idA, " ", anyMAC), want: anyMACTag},

		{name: "没有标签", subject: "普通邮件标题", err: ErrTagMissing},
		{name: "空主题", subject: "", err: ErrTagMissing},
		{name: "以 [ 结尾", subject: "标题 [", err: ErrTagMissing},
		{name: "以 [T 结尾", subject: "标题 [T", err: ErrTagMissing},
		{name: "[ 与 TC 之间有空格", subject: build("[ TC ", idA, " ", textA), err: ErrTagMissing},
		{name: "[T 之后不是 C", subject: "[TX 任务] 标题", err: ErrTagMissing},
		{name: "[ 之后不是 T", subject: "[AC] 空调", err: ErrTagMissing},
		{name: "TC 前面没有 [", subject: "Re: ATC 与 etc. 标题", err: ErrTagMissing},

		{name: "两个相同的标签", subject: tagA + " " + tagA, err: ErrTagMultiple},
		{name: "两个相同的标签紧挨着", subject: tagA + tagA, err: ErrTagMultiple},
		{name: "两个不同的标签", subject: "Re: " + tagA + " " + tagB, err: ErrTagMultiple},
		{name: "同一任务的两个令牌", subject: tagA + " " + build("[TC ", idA, " ", textB), err: ErrTagMultiple},
		{name: "三个标签", subject: tagA + tagB + tagA, err: ErrTagMultiple},
		{name: "两个标签中一个版本字节不对", subject: tagA + " " + build("[TC ", idA, " ", rawText(0x02, 0x01)), err: ErrTagMultiple},

		{name: "[TC 之后多一个空格", subject: build("[TC  ", idA, " ", textA), err: ErrTagDamaged},
		{name: "[TC 之后少一个空格", subject: build("[TC", idA, " ", textA), err: ErrTagDamaged},
		{name: "[TC 之后是制表符", subject: build("[TC\t", idA, " ", textA), err: ErrTagDamaged},
		{name: "[TC 之后是全角空格", subject: build("[TC\u3000", idA, " ", textA), err: ErrTagDamaged},
		{name: "任务 ID 之后多一个空格", subject: build("[TC ", idA, "  ", textA), err: ErrTagDamaged},
		{name: "任务 ID 之后少一个空格", subject: build("[TC ", idA, "", textA), err: ErrTagDamaged},
		{name: "任务 ID 之后是制表符", subject: build("[TC ", idA, "\t", textA), err: ErrTagDamaged},
		{name: "任务 ID 之后是全角空格", subject: build("[TC ", idA, "\u3000", textA), err: ErrTagDamaged},
		{name: "标签内折行", subject: build("[TC ", idA, "\r\n ", textA), err: ErrTagDamaged},
		{name: "令牌 47 个字符", subject: build("[TC ", idA, " ", textA[:47]), err: ErrTagDamaged},
		{name: "令牌 49 个字符", subject: build("[TC ", idA, " ", textA+"0"), err: ErrTagDamaged},
		{name: "任务 ID 9 个字符", subject: build("[TC ", idA[:9], " ", textA), err: ErrTagDamaged},
		{name: "任务 ID 11 个字符", subject: build("[TC ", idA+"0", " ", textA), err: ErrTagDamaged},
		{name: "大写的令牌", subject: "Re: " + build("[TC ", idA, " ", strings.ToUpper(textA)), err: ErrTagDamaged},
		{name: "大写的任务 ID", subject: build("[TC ", strings.ToUpper(idB), " ", textB), err: ErrTagDamaged},
		{name: "小写的 [tc", subject: build("[tc ", idA, " ", textA), err: ErrTagDamaged},
		{name: "[Tc", subject: build("[Tc ", idA, " ", textA), err: ErrTagDamaged},
		{name: "[tC", subject: build("[tC ", idA, " ", textA), err: ErrTagDamaged},
		{name: "缺少 ]", subject: "Re: [TC " + idA + " " + textA, err: ErrTagDamaged},
		{name: "] 换成全角括号", subject: "[TC " + idA + " " + textA + "］", err: ErrTagDamaged},
		{name: "旧形态 [TC <任务 ID>]", subject: "[TC " + idA + "]", err: ErrTagDamaged},
		{name: "[TCP] 在其他文字里", subject: "Re: [TCP] 连接问题", err: ErrTagDamaged},
		{name: "以 [TC 结尾", subject: "标题 [TC", err: ErrTagDamaged},
		{name: "[tc 在词中", subject: "abc[tcdef", err: ErrTagDamaged},

		{name: "版本字节为 2", subject: "Re: " + build("[TC ", idA, " ", rawText(0x02, 0x01)), err: ErrMalformed},
		{name: "版本字节为 0", subject: build("[TC ", idA, " ", rawText(0x00, 0x01)), err: ErrMalformed},
		{name: "kid 为 0", subject: build("[TC ", idA, " ", rawText(0x01, 0x00)), err: ErrMalformed},
	}
	for _, c := range []string{"i", "l", "o", "u"} {
		cases = append(cases, subjectCase{name: "令牌含 " + c, subject: build("[TC ", idA, " ", withChar(textA, 9, c)), err: ErrTagDamaged})
	}
	return cases
}

// refTagStarts 是严格文法的参考实现，不用正则：从左到右逐字节比对 64 字节的窗口，命中后跳过整个标签，返回各命中的起始下标。
func refTagStarts(s string) []int {
	var starts []int
	for i := 0; i+64 <= len(s); i++ {
		if refTagAt(s[i : i+64]) {
			starts = append(starts, i)
			i += 63
		}
	}
	return starts
}

// refTagAt 判断 64 字节的 w 是否恰为一个标签：「[TC 」、10 个字母表字符、一个 U+0020、48 个字母表字符与「]」。
func refTagAt(w string) bool {
	if w[:4] != "[TC " || w[14] != ' ' || w[63] != ']' {
		return false
	}
	for i := 4; i < 63; i++ {
		if i != 14 && strings.IndexByte(refAlphabet, w[i]) < 0 {
			return false
		}
	}
	return true
}

// refHasOpening 是 [TC 查找的参考实现：只把 ASCII 的 A–Z 折成小写，再找 [tc。
func refHasOpening(s string) bool {
	folded := []byte(s)
	for i, c := range folded {
		if 'A' <= c && c <= 'Z' {
			folded[i] = c + 'a' - 'A'
		}
	}
	return bytes.Contains(folded, []byte("[tc"))
}

// referenceParseSubject 是 ParseSubject 的参考实现：命中次数取自 refTagStarts，[TC 取自 refHasOpening，
// 恰好一次命中时令牌文本交给 Parse。
func referenceParseSubject(s string) (Tag, error) {
	starts := refTagStarts(s)
	switch {
	case len(starts) >= 2:
		return Tag{}, ErrTagMultiple
	case len(starts) == 1:
		i := starts[0]
		parsed, err := Parse(s[i+15 : i+63])
		if err != nil {
			return Tag{}, ErrMalformed
		}
		return Tag{taskID: s[i+4 : i+14], token: parsed}, nil
	case refHasOpening(s):
		return Tag{}, ErrTagDamaged
	}
	return Tag{}, ErrTagMissing
}

// TestParseSubjectGrammar 逐行核对文法表：接受时得到期望的标签；拒绝时得到期望的哨兵错误、零值标签与哨兵的固定文本。
func TestParseSubjectGrammar(t *testing.T) {
	for _, tc := range subjectCases(t) {
		got, err := ParseSubject(tc.subject)
		if tc.err == nil {
			if err != nil || got != tc.want {
				t.Errorf("%s: ParseSubject error = %v, tag match %v; want the expected tag", tc.name, err, got == tc.want)
			}
			continue
		}
		if !errors.Is(err, tc.err) || err.Error() != tc.err.Error() || got != (Tag{}) {
			t.Errorf("%s: ParseSubject error = %v, zero tag %v; want %v and the zero Tag", tc.name, err, got == Tag{}, tc.err)
		}
	}
}

// TestParseSubjectRoundTrip 确认 ParseSubject 从「Re: <标签> 标题」取回的标签与原值相等，任务 ID、令牌与两种明文都一致。
func TestParseSubjectRoundTrip(t *testing.T) {
	issued := issueToken(t, "zyxwvtsrqp", 7, 0x5a)
	tag, err := NewTag("zyxwvtsrqp", issued)
	if err != nil {
		t.Fatalf("NewTag: %v", err)
	}
	got, err := ParseSubject("Re: " + tag.Reveal() + " 标题")
	if err != nil || got != tag {
		t.Fatalf("ParseSubject round trip failed (err %v)", err)
	}
	if got.TaskID() != "zyxwvtsrqp" || got.Token() != issued || got.Token().KeyID() != 7 {
		t.Errorf("TaskID %q, token match %v, KeyID %d", got.TaskID(), got.Token() == issued, got.Token().KeyID())
	}
	if got.Reveal() != tag.Reveal() || got.RevealToken() != issued.Reveal() {
		t.Error("the parsed tag reveals different text")
	}
}

// TestTagReveal 确认 Reveal 恰为「[TC 」、任务 ID、一个空格、令牌文本与「]」的拼接（64 个字符），RevealToken 恰为
// 48 个字符的令牌文本，TaskID 与 Token 返回构造时的值，且 Reveal 的结果按参考实现恰好是一个完整的标签。
func TestTagReveal(t *testing.T) {
	issued := issueToken(t, "abcdefghjk", 1, 0x33)
	tag, err := NewTag("abcdefghjk", issued)
	if err != nil {
		t.Fatalf("NewTag: %v", err)
	}
	want := "[TC " + "abcdefghjk" + " " + issued.Reveal() + "]"
	if tag.Reveal() != want || len(tag.Reveal()) != 64 {
		t.Errorf("Reveal has length %d and differs from the expected layout: %v", len(tag.Reveal()), tag.Reveal() != want)
	}
	if tag.RevealToken() != issued.Reveal() || len(tag.RevealToken()) != TextLen {
		t.Errorf("RevealToken has length %d and differs from Token.Reveal: %v", len(tag.RevealToken()), tag.RevealToken() != issued.Reveal())
	}
	if tag.TaskID() != "abcdefghjk" || tag.Token() != issued {
		t.Errorf("TaskID %q, token match %v", tag.TaskID(), tag.Token() == issued)
	}
	if starts := refTagStarts(tag.Reveal()); len(starts) != 1 || starts[0] != 0 {
		t.Errorf("reference grammar finds %v in Reveal, want one tag at 0", starts)
	}
}

// TestNewTagRejects 确认任务 ID 不是恰好 10 个小写字母表字符、或令牌不是由 Issue 或 Parse 得到（kid 为 0）时，
// NewTag 返回 ErrInvalidTag、固定的错误文本与零值标签；并钉住合法边界：kid 255 的令牌、Parse 得到的令牌与字母表两端的字符。
func TestNewTagRejects(t *testing.T) {
	issued := issueToken(t, "0123456789", 1, 0x44)
	ids := map[string]string{
		"empty":     "",
		"9 chars":   "012345678",
		"11 chars":  "0123456789a",
		"upper":     "012345678A",
		"i":         "012345678i",
		"l":         "012345678l",
		"o":         "012345678o",
		"u":         "012345678u",
		"space":     "01234 6789",
		"bracket":   "012345678]",
		"newline":   "012345678\n",
		"multibyte": "0123456中",
	}
	for name, id := range ids {
		got, err := NewTag(id, issued)
		if !errors.Is(err, ErrInvalidTag) || err.Error() != "invalid subject tag" || got != (Tag{}) {
			t.Errorf("%s: NewTag error = %v, zero tag %v; want ErrInvalidTag and the zero Tag", name, err, got == Tag{})
		}
	}
	zeroKID := issued
	zeroKID.kid = 0
	for name, tok := range map[string]Token{"zero Token": {}, "kid 0": zeroKID} {
		if got, err := NewTag("0123456789", tok); !errors.Is(err, ErrInvalidTag) || got != (Tag{}) {
			t.Errorf("%s: NewTag error = %v, zero tag %v; want ErrInvalidTag and the zero Tag", name, err, got == Tag{})
		}
	}
	parsed, err := Parse(issued.Reveal())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for name, tok := range map[string]Token{"kid 255": issueToken(t, "0123456789", 255, 0x44), "parsed": parsed} {
		for _, id := range []string{"0000000000", "zzzzzzzzzz", "0123456789"} {
			got, err := NewTag(id, tok)
			if err != nil || got.TaskID() != id || got.Token() != tok {
				t.Errorf("%s with task id %q: NewTag error = %v", name, id, err)
			}
		}
	}
}

// TestTagZeroValue 确认零值标签不可用而无害：任务 ID 为空、令牌为零值，Reveal 的结果不能被 ParseSubject 接受，
// 两者也不能再用来构造标签，格式化输出同样脱敏。
func TestTagZeroValue(t *testing.T) {
	var zero Tag
	if zero.TaskID() != "" || zero.Token() != (Token{}) {
		t.Error("zero Tag has a task id or a token")
	}
	if _, err := ParseSubject("Re: " + zero.Reveal()); err == nil {
		t.Error("ParseSubject accepts the zero Tag's Reveal")
	}
	if _, err := NewTag(zero.TaskID(), zero.Token()); !errors.Is(err, ErrInvalidTag) {
		t.Errorf("NewTag from the zero Tag = %v, want ErrInvalidTag", err)
	}
	if out := fmt.Sprintf("%v %+v %d", zero, zero, zero); out != tagMarker+" "+tagMarker+" "+tagMarker {
		t.Errorf("zero Tag formats as %q", out)
	}
}

// TestTagRedaction 以金丝雀检查标签的全部格式化出口：fmt 的常用动词作用于值与指针时恰为脱敏文本；切片、映射、数组、
// 含导出字段与嵌入字段的结构体中含脱敏文本，且不含标签明文、令牌文本的任何 10 字符子串与令牌原始字节的十六进制、十进制形式；
// slog 的 JSON 与文本处理器同样如此；%p 作用于指针、切片与映射时只输出地址。%p 作用于标签值是已知局限，这里不断言。
func TestTagRedaction(t *testing.T) {
	tag := issueTag(t, "zyxwvtsrqp", 1, 0x66)
	text := tag.RevealToken()
	raw, err := refEncoding.DecodeString(text)
	if err != nil || len(raw) != 30 {
		t.Fatalf("decode reference: %v", err)
	}
	forbidden := []string{tag.Reveal(), "[TC ", fmt.Sprint(raw[2:14]), fmt.Sprint(raw[14:])}
	for _, part := range [][]byte{raw, raw[2:14], raw[14:]} {
		forbidden = append(forbidden, hex.EncodeToString(part), strings.ToUpper(hex.EncodeToString(part)))
	}
	for i := 0; i+10 <= len(text); i++ {
		forbidden = append(forbidden, text[i:i+10], strings.ToUpper(text[i:i+10]))
	}
	// clean 报告 out 中出现的任何禁止子串，只写出位置说明，不打印 out 本身。
	clean := func(label, out string) {
		for _, bad := range forbidden {
			if strings.Contains(out, bad) {
				t.Errorf("%s leaks subject tag material", label)
				return
			}
		}
	}
	embedded := tagEmbedder{Tag: tag, Note: "note"}
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%t", "%o", "%b", "%e", "%10v", "%-30s"}
	for _, verb := range verbs {
		for _, v := range []any{tag, &tag} {
			if out := fmt.Sprintf(verb, v); out != tagMarker {
				t.Errorf("%s of %T = %q, want %q", verb, v, out, tagMarker)
			}
		}
		containers := []any{[]Tag{tag}, map[string]Tag{"t": tag}, [1]Tag{tag}, tagHolder{T: tag}, &tagHolder{T: tag}, embedded, &embedded}
		for _, v := range containers {
			out := fmt.Sprintf(verb, v)
			if !strings.Contains(out, tagMarker) {
				t.Errorf("%s of %T = %q, want it to contain %q", verb, v, out, tagMarker)
			}
			clean(fmt.Sprintf("%s of %T", verb, v), out)
		}
	}
	for _, v := range []any{&tag, []Tag{tag}, map[string]Tag{"t": tag}, &tagHolder{T: tag}, &embedded} {
		if out := fmt.Sprintf("%p", v); !strings.HasPrefix(out, "0x") || strings.ContainsAny(out, "{[ ") {
			t.Errorf("%%p of %T = %q, want an address", v, out)
		}
	}
	var buf bytes.Buffer
	for _, logger := range []*slog.Logger{slog.New(slog.NewJSONHandler(&buf, nil)), slog.New(slog.NewTextHandler(&buf, nil))} {
		buf.Reset()
		logger.Info("subject", slog.Any("tag", tag), slog.Any("ptr", &tag), slog.Any("holder", tagHolder{T: tag}),
			slog.Any("embedded", embedded), slog.Any("list", []Tag{tag}))
		out := buf.String()
		if !strings.Contains(out, tagMarker) {
			t.Errorf("slog output %q lacks the redaction marker", out)
		}
		clean("slog output", out)
	}
	buf.Reset()
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("subject", slog.Any("tag", tag))
	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil || record["tag"] != tagMarker {
		t.Errorf("JSON handler logs a Tag value as %v, want %q (err %v)", record["tag"], tagMarker, err)
	}
	if tag.String() != tagMarker {
		t.Error("String does not return the redaction marker")
	}
	for _, v := range []slog.Value{tag.LogValue(), slog.AnyValue(tag).Resolve(), slog.AnyValue(&tag).Resolve()} {
		if v.Kind() != slog.KindString || v.String() != tagMarker {
			t.Errorf("LogValue resolves to kind %v, want the redaction marker", v.Kind())
		}
	}
}

// TestSubjectErrorsFixed 钉住三个解析错误与 ErrInvalidTag 的固定文本，并以金丝雀确认错误文本不含主题、任务 ID 与令牌中的任何部分。
func TestSubjectErrorsFixed(t *testing.T) {
	texts := map[error]string{
		ErrTagMissing:  "subject tag missing",
		ErrTagDamaged:  "subject tag damaged",
		ErrTagMultiple: "multiple subject tags",
		ErrInvalidTag:  "invalid subject tag",
	}
	for err, want := range texts {
		if err.Error() != want {
			t.Errorf("error text %q, want %q", err.Error(), want)
		}
	}
	tag := issueTag(t, "0123456789", 1, 0x77)
	canary := "金丝雀标题canary"
	var errs []error
	for _, subject := range []string{
		canary,
		canary + " " + tag.Reveal()[:40],
		tag.Reveal() + canary + tag.Reveal(),
		canary + " [TC " + tag.TaskID() + " " + rawText(0x02, 0x01) + "]",
	} {
		_, err := ParseSubject(subject)
		errs = append(errs, err)
	}
	_, err := NewTag("CANARY9999", tag.Token())
	errs = append(errs, err)
	for i, err := range errs {
		if err == nil {
			t.Errorf("input %d was accepted", i)
			continue
		}
		out := err.Error()
		if strings.Contains(out, "canary") || strings.Contains(out, "金丝雀") || strings.Contains(out, "CANARY") ||
			strings.Contains(out, tag.TaskID()) || strings.Contains(out, tag.RevealToken()[:10]) {
			t.Errorf("error %d echoes its input: %q", i, out)
		}
	}
}

// FuzzParseSubject 以逐字节的参考实现为准做差分：任意输入不 panic；错误只能是四个哨兵之一、文本固定、标签为零值，
// 且与参考实现的判定相同；成功时 Reveal() 是输入的子串，结果与参考实现取出的任务 ID 和令牌相同，并能由 NewTag 重建。
func FuzzParseSubject(f *testing.F) {
	for _, tc := range subjectCases(f) {
		f.Add(tc.subject)
	}
	f.Fuzz(func(t *testing.T, input string) {
		got, err := ParseSubject(input)
		want, wantErr := referenceParseSubject(input)
		if !errors.Is(err, wantErr) || got != want {
			t.Fatalf("ParseSubject error = %v, reference error = %v, tags equal %v", err, wantErr, got == want)
		}
		if err != nil {
			if err.Error() != wantErr.Error() || got != (Tag{}) {
				t.Fatalf("error %q is not the bare sentinel or the tag is not zero", err.Error())
			}
			return
		}
		if !strings.Contains(input, got.Reveal()) {
			t.Fatal("accepted tag is not a substring of the input")
		}
		if rebuilt, err := NewTag(got.TaskID(), got.Token()); err != nil || rebuilt != got {
			t.Fatalf("NewTag cannot rebuild the parsed tag (err %v)", err)
		}
	})
}
