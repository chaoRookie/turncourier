// Package token 的主题标签：Tag 把任务 ID 与令牌组成 [TC <任务 ID> <令牌>]，ParseSubject 在已解码的完整主题中
// 按 D4 的严格文法定位恰好一个标签。主题标签是令牌唯一的验证输入；标签字符串一旦拼出就失去类型带来的脱敏，
// 所以明文只经 Reveal 与 RevealToken 交给渲染器，其余出口一律脱敏。
package token

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
)

const (
	// tagOpen 与 tagClose 是主题标签的首尾；tagOpen 以一个 U+0020 结束，其后紧接任务 ID。
	tagOpen  = "[TC "
	tagClose = "]"
	// redactedTag 是主题标签在一切格式化输出中的替代文本。
	redactedTag = "[redacted subject tag]"
)

var (
	// ErrInvalidTag 表示构造标签时任务 ID 不是 10 个字母表字符，或令牌不是由 Issue 或 Parse 得到（kid 为 0）；错误文本固定，不回显输入。
	ErrInvalidTag = errors.New("invalid subject tag")
	// ErrTagMissing 表示主题中没有 [TC（ASCII 不区分大小写），对应原因码 subject_tag_missing。
	ErrTagMissing = errors.New("subject tag missing")
	// ErrTagDamaged 表示主题中有 [TC，严格文法却一次也没有命中：折行、截断、空白或大小写被改动都归此类，
	// 对应原因码 subject_tag_damaged，与令牌无效（ErrMalformed）区分开。
	ErrTagDamaged = errors.New("subject tag damaged")
	// ErrTagMultiple 表示严格文法在主题中命中两次及以上，对应原因码 subject_tag_multiple。
	ErrTagMultiple = errors.New("multiple subject tags")
)

// subjectTagPattern 是 D4 的主题标签严格文法 \[TC [字母表]{10} [字母表]{48}\]，在运行时由字母表常量拼出：
// [TC、一个 U+0020、10 个任务 ID 字符、恰好一个 U+0020、48 个令牌字符与 ]。字母表只有小写字母与数字，
// 放进方括号不需要转义；匹配区分大小写，大写的任务 ID 或令牌不算命中。两个分组依次是任务 ID 与令牌文本。
var subjectTagPattern = regexp.MustCompile(`\[TC ([` + alphabet + `]{10}) ([` + alphabet + `]{48})\]`)

// Tag 是主题标签 [TC <任务 ID> <令牌>] 的值：任务 ID 与令牌。零值不可用：标签只能由 NewTag 或 ParseSubject 得到，
// 零值的 Reveal 不是合法标签。String、Format 与 LogValue 一律输出脱敏文本，只有 Reveal 与 RevealToken 返回明文。
// 已知局限与 Token 相同：Tag 作为其他包中结构体的未导出字段时，fmt 无法调用它的方法，会按原始字段打印任务 ID 与令牌字节；
// %p 作用于标签值（或含标签的结构体值、数组值）时同样如此；%w 作用于标签值或指针时，fmt 在调用 Format 之前就按错误动词处理
// 非 error 的操作数，同样按原始字段打印（格式串为常量时 vet 的 printf 检查会拦下）。4b 以装配后的金丝雀测试覆盖实际路径
// （Task 11、13、14）。
type Tag struct {
	taskID string
	token  Token
}

// NewTag 用任务 ID 与令牌构造主题标签：任务 ID 须恰为 10 个小写字母表字符（与 Claims.TaskID 的规则相同），
// 令牌须由 Issue 或 Parse 得到（kid 不为 0，零值令牌因此被拒）；否则返回 ErrInvalidTag。这两条保证 Reveal 的结果
// 恰为 64 个 ASCII 字符并符合 ParseSubject 的文法，可以原样写入邮件头。NewTag 不核对令牌与任务 ID 的绑定：
// 任务 ID 不随令牌携带，绑定由 Verify 按通知行核对。
func NewTag(taskID string, t Token) (Tag, error) {
	if !validTaskID(taskID) || t.kid == 0 {
		return Tag{}, ErrInvalidTag
	}
	return Tag{taskID: taskID, token: t}, nil
}

// ParseSubject 在已解码的完整主题中按严格文法（见 subjectTagPattern）定位主题标签，不修复任何空白、折行或大小写。
// 按不重叠命中的次数判定：两次及以上返回 ErrTagMultiple，不取第一个、最后一个或第一个验得过的；零次时主题中有 [TC
// （只按 ASCII 不区分大小写）返回 ErrTagDamaged，没有则返回 ErrTagMissing；恰好一次时把 48 个令牌字符交给 Parse，
// 版本字节或 kid 不合法返回 ErrMalformed。标签可以出现在主题的任何位置，回复会在前面加上 Re:、回复： 等前缀。
// 本函数只检查格式，不验证签名；返回的错误都是固定文本的哨兵，不回显主题。
// 只需区分零次、一次与至少两次，所以最多取前两个命中。标签以 [ 开头，其后的 63 个字符都不是 [，两个命中不可能重叠，
// 不重叠计数与逐位置计数的结果相同。
func ParseSubject(subject string) (Tag, error) {
	matches := subjectTagPattern.FindAllStringSubmatchIndex(subject, 2)
	switch {
	case len(matches) >= 2:
		return Tag{}, ErrTagMultiple
	case len(matches) == 0 && hasTagOpening(subject):
		return Tag{}, ErrTagDamaged
	case len(matches) == 0:
		return Tag{}, ErrTagMissing
	}
	m := matches[0]
	parsed, err := Parse(subject[m[4]:m[5]])
	if err != nil {
		return Tag{}, ErrMalformed
	}
	// 复制任务 ID：子串会与整条主题共用底层内存，标签活多久，主题中的令牌明文与标题就被多留多久。
	return Tag{taskID: strings.Clone(subject[m[2]:m[3]]), token: parsed}, nil
}

// hasTagOpening 判断主题中是否出现 [TC，T 与 C 只按 ASCII 不区分大小写，不做 Unicode 大小写折叠。
// 逐字节比较对 UTF-8 是安全的：多字节字符的每个字节都不小于 0x80，不会被当成 [、T 或 C。
func hasTagOpening(subject string) bool {
	for i := 0; i+2 < len(subject); i++ {
		if subject[i] == '[' && (subject[i+1] == 'T' || subject[i+1] == 't') && (subject[i+2] == 'C' || subject[i+2] == 'c') {
			return true
		}
	}
	return false
}

// TaskID 返回任务 ID；任务 ID 不是机密。
func (g Tag) TaskID() string {
	return g.taskID
}

// Token 返回标签携带的令牌；令牌自身的格式化输出同样脱敏。
func (g Tag) Token() Token {
	return g.token
}

// Reveal 返回主题标签的明文 [TC <任务 ID> <令牌>]（64 个字符）；只用于渲染器写入主题与页脚副本。
// 产品代码中引用它的位置由 tests/docs/reveal_test.go 限定在本包与 internal/mail/renderer。
func (g Tag) Reveal() string {
	return tagOpen + g.taskID + " " + g.token.Reveal() + tagClose
}

// RevealToken 返回 48 个小写字符的令牌文本；只用于渲染器写入主题与页脚副本，引用位置的限制与 Reveal 相同。
func (g Tag) RevealToken() string {
	return g.token.Reveal()
}

// String 返回脱敏文本。
func (g Tag) String() string {
	return redactedTag
}

// Format 对 %T、%p、%w 之外的 fmt 动词输出脱敏文本；fmt 先于 Format 处理这三个动词，%p 与 %w 的局限见 Tag 的说明。
func (g Tag) Format(f fmt.State, _ rune) {
	io.WriteString(f, redactedTag)
}

// LogValue 让 slog 记录脱敏文本。
func (g Tag) LogValue() slog.Value {
	return slog.StringValue(redactedTag)
}
