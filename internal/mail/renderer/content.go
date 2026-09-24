// Package renderer 的通知内容格式：待发通知行中加密保存的 Content 是一段 JSON，{"v":1,"event":…,"task":…,"title":…,"body":…}。
// Encode 与 DecodeContent 对称地校验版本、事件与任务 ID，编码后至多 MaxContentSize 字节；DecodeContent 严格解码（字段名逐字比较、
// 不接受未知或重复字段、缺失字段、null、类型不符与尾随数据）。错误文本固定，不回显内容；Content 的格式化输出一律脱敏。
package renderer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	// ContentVersion 是通知内容格式的唯一版本。
	ContentVersion = 1
	// MaxContentSize 是编码后通知内容的字节上限，等于 payload.MaxPlaintext（1 MiB）。本包不能导入 security/*，
	// 两者相等由 internal/app 的测试核对。
	MaxContentSize = 1 << 20
)

// 四个通知事件，取值与配置 notify.events 的事件名相同。
const (
	// EventTurnCompleted 表示 Agent 的一个回合结束，任务仍可继续。
	EventTurnCompleted = "turn_completed"
	// EventWaitingInput 表示 Agent 在等待用户输入。
	EventWaitingInput = "waiting_input"
	// EventWaitingApproval 表示 Agent 在等待审批；审批只能在本机处理，邮件不能批准。
	EventWaitingApproval = "waiting_approval"
	// EventFailed 表示任务失败。
	EventFailed = "failed"
)

const (
	// alphabet 是任务 ID 与回复令牌共用的小写 Crockford base32 字母表（与 security/token 相同；本包不能导入 security/*）。
	alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	// taskIDLen 是任务 ID 的长度。
	taskIDLen = 10
	// redactedContent 是 Content 在一切格式化输出中的替代文本。
	redactedContent = "[redacted notification content]"
)

// eventNames 是四个通知事件的中文名，用于主题与页脚。
var eventNames = map[string]string{
	EventTurnCompleted:   "回合完成",
	EventWaitingInput:    "等待输入",
	EventWaitingApproval: "等待审批",
	EventFailed:          "任务失败",
}

// wireFields 是线上格式的五个字段名，DecodeContent 逐字比较，缺一不可。
var wireFields = []string{"v", "event", "task", "title", "body"}

// ErrInvalidContent 表示通知内容不合法：版本不为 1、事件不是四个之一、任务 ID 不是 10 个字母表字符、编码后超过 MaxContentSize，
// 或（解码时）JSON 不符合严格格式。错误文本只含固定的原因，不含内容。
var ErrInvalidContent = errors.New("invalid notification content")

// Content 是一条通知的内容：Agent 提供的标题与正文，以及事件与任务 ID。String、Format、LogValue 与 MarshalJSON 一律输出脱敏文本，
// 标题与正文不会随格式化输出进入日志；编码只经 Encode。已知局限与 token.Tag 相同：Content 作为其他包中结构体的未导出字段时，
// fmt 无法调用它的方法，会按原始字段打印；%p 作用于 Content 值（或含它的结构体值）时同样如此。
type Content struct {
	Version int    // 格式版本，须为 ContentVersion
	Event   string // 四个通知事件之一
	TaskID  string // 10 个小写字母表字符
	Title   string // Agent 提供的标题，渲染时过滤
	Body    string // Agent 提供的正文，渲染时过滤
}

// wireContent 是线上格式的结构：字段顺序即编码顺序。
type wireContent struct {
	V     int    `json:"v"`
	Event string `json:"event"`
	Task  string `json:"task"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Encode 校验内容后编码为 JSON：字段依次为 v、event、task、title、body，不转义 HTML 字符，没有结尾换行；标题与正文中的
// 非法 UTF-8 字节由 encoding/json 换成 U+FFFD。编码后超过 MaxContentSize 返回 ErrInvalidContent。
func (c Content) Encode() ([]byte, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// 结构体只含 int 与 string 字段，编码不会失败。
	_ = enc.Encode(wireContent{V: c.Version, Event: c.Event, Task: c.TaskID, Title: c.Title, Body: c.Body})
	data := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(data) > MaxContentSize {
		return nil, fmt.Errorf("%w: encoded content exceeds %d bytes", ErrInvalidContent, MaxContentSize)
	}
	return data, nil
}

// DecodeContent 严格解码 Encode 的输出：至多 MaxContentSize 字节的合法 UTF-8；顶层是恰含 v、event、task、title、body 五个字段的对象，
// 字段名逐字比较（不接受大小写变体）、不重复、取值不为 null；v 是字面量 1，其余四个是字符串；对象之后只允许空白。
// 解码后再校验事件与任务 ID。任何一项不符都返回包装 ErrInvalidContent 的错误与零值，错误文本不含输入。
func DecodeContent(data []byte) (Content, error) {
	if len(data) > MaxContentSize {
		return Content{}, fmt.Errorf("%w: content exceeds %d bytes", ErrInvalidContent, MaxContentSize)
	}
	if !utf8.Valid(data) {
		return Content{}, fmt.Errorf("%w: content is not valid UTF-8", ErrInvalidContent)
	}
	fields, err := decodeObject(data)
	if err != nil {
		return Content{}, fmt.Errorf("%w: %s", ErrInvalidContent, err.Error())
	}
	c := Content{Version: ContentVersion, Event: fields["event"], TaskID: fields["task"], Title: fields["title"], Body: fields["body"]}
	if err := c.validate(); err != nil {
		return Content{}, err
	}
	return c, nil
}

// decodeObject 按词法逐个读取顶层对象：v 的取值须是字面量 1（UseNumber 保留原文），其余字段须是字符串；返回四个字符串字段的取值。
// 返回的错误只含固定的原因；encoding/json 自身的错误会引用输入中的字符，所以一律换成固定文本。
func decodeObject(data []byte) (map[string]string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("content is not a JSON object")
	}
	fields := make(map[string]string, len(wireFields))
	seen := make(map[string]bool, len(wireFields))
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, errors.New("content is not valid JSON")
		}
		name, _ := tok.(string)
		if !slices.Contains(wireFields, name) || seen[name] {
			return nil, errors.New("content has an unknown or repeated field")
		}
		seen[name] = true
		value, err := dec.Token()
		if err != nil {
			return nil, errors.New("content is not valid JSON")
		}
		if name == "v" {
			if value != json.Number("1") {
				return nil, errors.New("content version is not the literal 1")
			}
			continue
		}
		text, ok := value.(string)
		if !ok {
			return nil, errors.New("content field is not a string")
		}
		fields[name] = text
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, errors.New("content is not valid JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("content has data after the object")
	}
	if len(seen) != len(wireFields) {
		return nil, errors.New("content is missing a field")
	}
	return fields, nil
}

// validate 检查版本为 ContentVersion、事件是四个通知事件之一、任务 ID 为 10 个小写字母表字符；标题与正文不设限制。
func (c Content) validate() error {
	switch {
	case c.Version != ContentVersion:
		return fmt.Errorf("%w: version is not %d", ErrInvalidContent, ContentVersion)
	case eventNames[c.Event] == "":
		return fmt.Errorf("%w: unknown notification event", ErrInvalidContent)
	case !validTaskID(c.TaskID):
		return fmt.Errorf("%w: task id is not %d alphabet characters", ErrInvalidContent, taskIDLen)
	}
	return nil
}

// validTaskID 判断任务 ID 是否恰为 10 个字母表字符（小写，没有 i、l、o、u），与 security/token 的规则相同。
func validTaskID(id string) bool {
	if len(id) != taskIDLen {
		return false
	}
	for i := range len(id) {
		if strings.IndexByte(alphabet, id[i]) < 0 {
			return false
		}
	}
	return true
}

// String 返回脱敏文本。
func (c Content) String() string {
	return redactedContent
}

// Format 对 %T、%p、%w 之外的 fmt 动词输出脱敏文本；fmt 先于 Format 处理这三个动词（局限见 Content 的说明）。
func (c Content) Format(f fmt.State, _ rune) {
	io.WriteString(f, redactedContent)
}

// LogValue 让 slog 记录脱敏文本。
func (c Content) LogValue() slog.Value {
	return slog.StringValue(redactedContent)
}

// MarshalJSON 输出脱敏文本的 JSON 字符串，json.Marshal 一个含 Content 的结构体不会带出标题与正文；编码通知内容只用 Encode。
func (c Content) MarshalJSON() ([]byte, error) {
	return json.Marshal(redactedContent)
}
