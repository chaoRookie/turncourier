// Package renderer 的模糊测试：任意文字经 Filter 不 panic，结果幂等、是合法 UTF-8、至多 4000 个字符、没有控制与格式字符、
// 没有令牌形状的串与 Scrubber 的机密；任意字节经 DecodeContent 不 panic，解码成功的内容合法，编码后再解码得到同一个值。
// 普通 go test 只运行种子；种子中的令牌与机密都在运行时构造。
package renderer

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// filterSeeds 返回模糊测试的种子：覆盖七条规则、规则之间的交界与截断切口的几种形状。
func filterSeeds(tb testing.TB) []string {
	authCode, signingKey, _ := testSecrets()
	shape := issueText(tb, testTask, 0x71)
	return []string{
		"",
		"普通文字\n第二行",
		"a\x00b\u202ec\u200bd\r\ne\rf",
		"前\n```go\ncode\n```\n后\n~~~\nx",
		" ```\n\u200b```\n    ```\n``` a`b",
		"码 " + authCode + " 钥 " + signingKey + " 令牌 " + shape + " " + strings.ToUpper(shape),
		"https://u:p@example.com/a/b?q=1#f file:///Users/a/b <https://h/p?x>",
		"PATH=/usr/bin:/Users/a/bin host:/var/x ~/a/b C:\\Users\\a 文件在/Users/张三/x中",
		pemBlock("RSA PRIVATE KEY", "AAAA") + "\n" + pemLine("BEGIN", "EC PRIVATE KEY") + "\nB",
		"Authorization: Bearer x\npassword=\"a\\\"b\" token='c' api_key: d\nsecret=",
		"ghp_" + cycleText(mixedChars, 36, 1) + " AKIA" + cycleText("ABCDEFGHIJKLMNOPQRSTUVWXYZ", 16, 2) + " eyJ" + cycleText(mixedChars, 12, 3) + "." + cycleText(mixedChars, 12, 4),
		strings.Repeat("中", maxBodyRunes-49) + strings.Repeat(shape, 2)[:60],
		strings.Repeat("中", maxBodyRunes-12) + " password=" + strings.Repeat("x", 50),
		strings.Repeat("a\n", 10) + "``` " + strings.Repeat("x ", 3000) + "`",
		"password=" + omit + "\n" + omitPath + " [代码块已省略：3 行]",
	}
}

// FuzzFilter 断言任意输入经 Filter 之后：不 panic；再过滤一次不变；是合法 UTF-8；至多 4000 个字符；除换行与制表外没有控制字符，
// 也没有 Unicode 格式字符；没有令牌形状的串；不含 Scrubber 的机密。
func FuzzFilter(f *testing.F) {
	for _, seed := range filterSeeds(f) {
		f.Add(seed)
	}
	scrub := testScrubber()
	f.Fuzz(func(t *testing.T, in string) {
		out := Filter(in, scrub)
		if again := Filter(out, scrub); again != out {
			t.Fatalf("不幂等：\n第一次 %q\n第二次 %q", out, again)
		}
		if !utf8.ValidString(out) {
			t.Fatalf("结果不是合法 UTF-8：%q", out)
		}
		if n := utf8.RuneCountInString(out); n > maxBodyRunes {
			t.Fatalf("结果有 %d 个字符", n)
		}
		for _, r := range out {
			if (unicode.IsControl(r) && r != '\n' && r != '\t') || unicode.Is(unicode.Cf, r) {
				t.Fatalf("结果中有控制或格式字符 %U", r)
			}
		}
		if hasShape(out) {
			t.Fatalf("结果中有令牌形状的串：%q", out)
		}
		for _, secret := range scrub {
			if strings.Contains(out, secret) {
				t.Fatalf("结果中有 Scrubber 的机密")
			}
		}
	})
}

// FuzzDecodeContent 断言任意字节经 DecodeContent 不 panic；解码成功时内容通过校验，编码成功后再解码得到同一个值；失败时返回零值。
func FuzzDecodeContent(f *testing.F) {
	valid, err := validContent().Encode()
	if err != nil {
		f.Fatalf("Encode：%v", err)
	}
	for _, seed := range []string{
		string(valid), "", "null", "{}", "[]",
		`{"v":1,"event":"failed","task":"` + testTask + `","title":"","body":""}`,
		`{"v":1,"event":"failed","task":"` + testTask + `","title":"\u2028\ud83d\ude00","body":"a\"b"}`,
		`{"v":1,"V":1,"event":"failed","task":"` + testTask + `","title":"","body":""}`,
		`{"v":2,"event":"failed","task":"` + testTask + `","title":"","body":""}`,
		`{"v":1,"event":"failed","task":"` + testTask + `","title":"","body":""} {}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := DecodeContent(data)
		if err != nil {
			if c != (Content{}) {
				t.Fatalf("出错时返回了非零值的内容")
			}
			return
		}
		if err := c.validate(); err != nil {
			t.Fatalf("解码成功的内容没有通过校验：%v", err)
		}
		encoded, err := c.Encode()
		if err != nil {
			return // 重新编码可能更长（例如 U+2028 会被转义），超过上限时 Encode 拒绝，这不是解码的问题。
		}
		again, err := DecodeContent(encoded)
		if err != nil || again != c {
			t.Fatalf("编码后再解码得到不同的值（错误 %v）", err)
		}
	})
}
