// Package config 的地址测试用表格用例逐条覆盖规范化规则，并把同一批输入用作模糊测试种子。
package config

import (
	"errors"
	"strings"
	"testing"
)

// addressCase 是一条地址规范化用例；want 为空串表示该输入必须被拒绝。
type addressCase struct {
	raw  string
	want string
}

// maxLocal 是 64 个字符的本地部分，用于本地部分长度上界的用例。
var maxLocal = strings.Repeat("a", 64)

// maxLabel 是 63 个字符的域名标签，用于标签长度上界的用例。
var maxLabel = strings.Repeat("b", 63)

// maxAddress 是总长 254 的合法地址：64 字符本地部分加 189 字符域名。
var maxAddress = maxLocal + "@" + maxLabel + "." + maxLabel + "." + strings.Repeat("c", 61)

// addressCases 覆盖清单列出的全部规则与边界，同时作为 FuzzNormalizeAddress 的种子语料。
var addressCases = []addressCase{
	{"Me@Example.Invalid", "me@example.invalid"},
	{"  me@example.invalid  ", "me@example.invalid"},
	{"first.last+tag@mail.example.invalid", "first.last+tag@mail.example.invalid"},
	{"a!#$%&'*+/=?^_`{|}~-b@example.invalid", "a!#$%&'*+/=?^_`{|}~-b@example.invalid"},
	{maxLocal + "@example.invalid", maxLocal + "@example.invalid"},
	{"me@" + maxLabel + ".invalid", "me@" + maxLabel + ".invalid"},
	{maxAddress, maxAddress},
	{"Me <me@example.invalid>", ""},
	{"<me@example.invalid>", ""},
	{"a@example.invalid, b@example.invalid", ""},
	{`"quoted"@example.invalid`, ""},
	{"用户@example.invalid", ""},
	{"me@例子.invalid", ""},
	// K 开尔文符号与 İ 带点大写 I 小写后都变成 ASCII，用来钉住「先校验后转小写」的顺序。
	{"K@example.invalid", ""},
	{"İ@example.invalid", ""},
	{"me(x@example.invalid", ""},
	{"me@my_host.invalid", ""},
	{"me@example", ""},
	{"me@@example.invalid", ""},
	{".me@example.invalid", ""},
	{"me.@example.invalid", ""},
	{"m..e@example.invalid", ""},
	{"me@-bad.invalid", ""},
	{"me@bad-.invalid", ""},
	{"", ""},
	{"@example.invalid", ""},
	{"me@", ""},
	{"me@example..invalid", ""},
	{strings.Repeat("a", 65) + "@example.invalid", ""},
	{"me@" + maxLabel + "c.invalid", ""},
	{maxLocal + "@" + maxLabel + "." + maxLabel + "." + strings.Repeat("c", 62), ""},
}

// TestNormalizeAddress 逐条执行表格用例：期望成功的比对规范化结果，期望失败的断言错误包装 ErrInvalidAddress 且结果为空串。
func TestNormalizeAddress(t *testing.T) {
	for _, testCase := range addressCases {
		got, err := NormalizeAddress(testCase.raw)
		if testCase.want != "" {
			if err != nil || got != testCase.want {
				t.Errorf("NormalizeAddress(%q) = %q, %v; want %q", testCase.raw, got, err, testCase.want)
			}
			continue
		}
		if !errors.Is(err, ErrInvalidAddress) {
			t.Errorf("NormalizeAddress(%q) error = %v; want ErrInvalidAddress", testCase.raw, err)
		}
		if got != "" {
			t.Errorf("NormalizeAddress(%q) = %q; want empty string on error", testCase.raw, got)
		}
	}
}

// FuzzNormalizeAddress 以表格用例为种子验证性质：不 panic；失败必定包装 ErrInvalidAddress；
// 成功的结果全小写、只含一个 @，且再次规范化保持不变。
func FuzzNormalizeAddress(f *testing.F) {
	for _, testCase := range addressCases {
		f.Add(testCase.raw)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got, err := NormalizeAddress(raw)
		if err != nil {
			if !errors.Is(err, ErrInvalidAddress) {
				t.Errorf("NormalizeAddress(%q) error = %v; want ErrInvalidAddress", raw, err)
			}
			return
		}
		if got != strings.ToLower(got) {
			t.Errorf("NormalizeAddress(%q) = %q; want lower case", raw, got)
		}
		if strings.Count(got, "@") != 1 {
			t.Errorf("NormalizeAddress(%q) = %q; want exactly one @", raw, got)
		}
		again, err := NormalizeAddress(got)
		if err != nil || again != got {
			t.Errorf("NormalizeAddress(%q) = %q, %v; want %q unchanged", got, again, err, got)
		}
	})
}
