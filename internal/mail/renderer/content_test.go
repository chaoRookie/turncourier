// Package renderer 测试通知内容的 JSON 格式：线上格式（字段名、顺序、不转义 HTML 字符）、编码与严格解码的往返、四个通知事件与
// 任务 ID 的校验、严格解码逐项拒绝的输入（未知字段、大小写变体、重复、缺失、null、类型不符、版本不为字面量 1、尾随数据、非法 UTF-8）、
// 1 MiB 上限的边界，以及 Content 的格式化输出与错误文本都不含标题与正文。
package renderer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const (
	// testTask 是测试用的任务 ID：10 个字母表字符。
	testTask = "q7m3k9x2pa"
	// contentMarker 是规格规定的 Content 脱敏文本，直接写出以钉住输出，不引用被测包的常量。
	contentMarker = "[redacted notification content]"
)

// validContent 返回一份合法的通知内容，标题与正文是普通的中文文字。
func validContent() Content {
	return Content{Version: 1, Event: EventTurnCompleted, TaskID: testTask, Title: "部署脚本已更新", Body: "集成测试尚未运行。\n是否继续？"}
}

// TestContentWireFormat 钉住线上格式：字段名与顺序为 v、event、task、title、body，没有多余的空白与换行，
// HTML 字符（<、>、&）不转义为 < 之类，非 ASCII 字符原样输出。
func TestContentWireFormat(t *testing.T) {
	c := validContent()
	c.Title = "<b>标题</b> & 说明"
	got, err := c.Encode()
	if err != nil {
		t.Fatalf("Encode 返回错误：%v", err)
	}
	want := `{"v":1,"event":"turn_completed","task":"` + testTask + `","title":"<b>标题</b> & 说明","body":"集成测试尚未运行。\n是否继续？"}`
	if string(got) != want {
		t.Errorf("Encode = %s\n期望 %s", got, want)
	}
}

// TestContentRoundTrip 断言四个事件的合法内容编码后解码得到相等的值；标题与正文可以为空，也可以含引号、反斜杠、制表与 U+2028。
func TestContentRoundTrip(t *testing.T) {
	for _, event := range []string{EventTurnCompleted, EventWaitingInput, EventWaitingApproval, EventFailed} {
		for _, texts := range [][2]string{{"", ""}, {"标题", "正文"}, {`引号"与\反斜杠`, "制表\t与\u2028分隔"}} {
			c := Content{Version: ContentVersion, Event: event, TaskID: testTask, Title: texts[0], Body: texts[1]}
			data, err := c.Encode()
			if err != nil {
				t.Fatalf("%s：Encode 返回错误：%v", event, err)
			}
			got, err := DecodeContent(data)
			if err != nil || got != c {
				t.Errorf("%s：DecodeContent(Encode(c)) 的结果与原值不同（错误 %v）", event, err)
			}
		}
	}
	if ContentVersion != 1 {
		t.Errorf("ContentVersion = %d，契约规定为 1", ContentVersion)
	}
	if EventTurnCompleted != "turn_completed" || EventWaitingInput != "waiting_input" || EventWaitingApproval != "waiting_approval" || EventFailed != "failed" {
		t.Error("通知事件名与 notify.events 的取值不一致")
	}
}

// TestContentInvalidUTF8 断言标题与正文中的非法 UTF-8 字节由 Encode 换成 U+FFFD（encoding/json 的行为），不报错。
func TestContentInvalidUTF8(t *testing.T) {
	c := validContent()
	c.Title = "坏\xff字节"
	data, err := c.Encode()
	if err != nil {
		t.Fatalf("Encode 返回错误：%v", err)
	}
	got, err := DecodeContent(data)
	if err != nil || got.Title != "坏\ufffd字节" {
		t.Errorf("解码得到的标题为 %q（错误 %v），期望非法字节换成 U+FFFD", got.Title, err)
	}
}

// TestContentValidation 断言版本、事件与任务 ID 的校验在 Encode 与 DecodeContent 中一致：版本不为 1、事件不是四个之一、
// 任务 ID 不是 10 个小写字母表字符都返回 ErrInvalidContent，Encode 不返回任何字节。
func TestContentValidation(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Content)
	}{
		{"版本为 0", func(c *Content) { c.Version = 0 }},
		{"版本为 2", func(c *Content) { c.Version = 2 }},
		{"事件为空", func(c *Content) { c.Event = "" }},
		{"事件大写", func(c *Content) { c.Event = "TURN_COMPLETED" }},
		{"未知事件", func(c *Content) { c.Event = "closed" }},
		{"事件带空白", func(c *Content) { c.Event = " failed" }},
		{"任务 ID 为空", func(c *Content) { c.TaskID = "" }},
		{"任务 ID 9 位", func(c *Content) { c.TaskID = testTask[:9] }},
		{"任务 ID 11 位", func(c *Content) { c.TaskID = testTask + "0" }},
		{"任务 ID 大写", func(c *Content) { c.TaskID = strings.ToUpper(testTask) }},
		{"任务 ID 含 i", func(c *Content) { c.TaskID = "i" + testTask[1:] }},
		{"任务 ID 含 l", func(c *Content) { c.TaskID = testTask[:9] + "l" }},
		{"任务 ID 含 o", func(c *Content) { c.TaskID = "o" + testTask[1:] }},
		{"任务 ID 含 u", func(c *Content) { c.TaskID = "u" + testTask[1:] }},
		{"任务 ID 含非 ASCII", func(c *Content) { c.TaskID = "任务" + testTask[:4] }},
	}
	for _, tc := range cases {
		c := validContent()
		tc.edit(&c)
		data, err := c.Encode()
		if !errors.Is(err, ErrInvalidContent) || data != nil {
			t.Errorf("%s：Encode = %q, %v；期望 nil 与 ErrInvalidContent", tc.name, data, err)
		}
		wire := fmt.Sprintf(`{"v":%d,"event":%q,"task":%q,"title":"t","body":"b"}`, c.Version, c.Event, c.TaskID)
		if _, err := DecodeContent([]byte(wire)); !errors.Is(err, ErrInvalidContent) {
			t.Errorf("%s：DecodeContent 返回 %v，期望 ErrInvalidContent", tc.name, err)
		}
	}
	// 字母表两端与每个事件都被接受。
	for _, id := range []string{"0000000000", "zzzzzzzzzz", "0123456789", "abcdefghjk", "mnpqrstvwx"} {
		c := validContent()
		c.TaskID = id
		if _, err := c.Encode(); err != nil {
			t.Errorf("任务 ID %s 被拒绝：%v", id, err)
		}
	}
}

// TestDecodeContentStrict 逐项断言严格解码拒绝的输入：不是对象、未知字段、键名的大小写变体、重复字段、缺少任一字段、取值为 null、
// 类型不符、版本不是字面量 1（1.0、1e0、"1"、true 也不行）、对象之后还有数据、非法 UTF-8；错误都包装 ErrInvalidContent，
// 错误文本不含输入中的标题与正文。
func TestDecodeContentStrict(t *testing.T) {
	const good = `{"v":1,"event":"failed","task":"` + testTask + `","title":"CANARYTITLE","body":"CANARYBODY"}`
	if _, err := DecodeContent([]byte(good)); err != nil {
		t.Fatalf("合法输入被拒绝：%v", err)
	}
	// replace 返回把 good 中的 old 换成 repl 的结果。
	replace := func(old, repl string) string { return strings.Replace(good, old, repl, 1) }
	cases := map[string]string{
		"空输入":        "",
		"空白":         " \n",
		"null":       "null",
		"数组":         "[" + good + "]",
		"字符串":        `"` + testTask + `"`,
		"数字":         "1",
		"空对象":        "{}",
		"未知字段":       replace(`"v":1`, `"v":1,"extra":"CANARYTITLE"`),
		"键名大写 V":     replace(`"v":1`, `"V":1`),
		"键名大写 Event": replace(`"event"`, `"Event"`),
		"键名全大写":      replace(`"title"`, `"TITLE"`),
		"重复字段":       replace(`"v":1`, `"v":1,"v":1`),
		"重复标题":       replace(`"title":"CANARYTITLE"`, `"title":"CANARYTITLE","title":"x"`),
		"缺少 v":       replace(`"v":1,`, ``),
		"缺少 event":   replace(`"event":"failed",`, ``),
		"缺少 task":    replace(`"task":"`+testTask+`",`, ``),
		"缺少 title":   replace(`"title":"CANARYTITLE",`, ``),
		"缺少 body":    replace(`,"body":"CANARYBODY"`, ``),
		"v 为 null":   replace(`"v":1`, `"v":null`),
		"标题为 null":   replace(`"title":"CANARYTITLE"`, `"title":null`),
		"正文为 null":   replace(`"body":"CANARYBODY"`, `"body":null`),
		"v 为字符串":     replace(`"v":1`, `"v":"1"`),
		"v 为 1.0":    replace(`"v":1`, `"v":1.0`),
		"v 为 1e0":    replace(`"v":1`, `"v":1e0`),
		"v 为 true":   replace(`"v":1`, `"v":true`),
		"v 为 2":      replace(`"v":1`, `"v":2`),
		"v 为 -1":     replace(`"v":1`, `"v":-1`),
		"v 为对象":      replace(`"v":1`, `"v":{}`),
		"事件为数字":      replace(`"event":"failed"`, `"event":3`),
		"标题为数组":      replace(`"title":"CANARYTITLE"`, `"title":["CANARYTITLE"]`),
		"正文为对象":      replace(`"body":"CANARYBODY"`, `"body":{"x":"CANARYBODY"}`),
		"正文为 true":   replace(`"body":"CANARYBODY"`, `"body":true`),
		"尾随对象":       good + "{}",
		"尾随文字":       good + " CANARYBODY",
		"尾随逗号":       replace(`"CANARYBODY"}`, `"CANARYBODY",}`),
		"缺少右括号":      strings.TrimSuffix(good, "}"),
		"非法 UTF-8":   replace(`CANARYBODY`, "CANARY\xffBODY"),
		"原始控制字符":     replace(`CANARYBODY`, "CANARY\x01BODY"),
	}
	for name, input := range cases {
		c, err := DecodeContent([]byte(input))
		if !errors.Is(err, ErrInvalidContent) {
			t.Errorf("%s：DecodeContent 返回 %v，期望 ErrInvalidContent", name, err)
			continue
		}
		if c != (Content{}) {
			t.Errorf("%s：出错时返回了非零值的内容", name)
		}
		if strings.Contains(err.Error(), "CANARY") || strings.Contains(err.Error(), testTask) {
			t.Errorf("%s：错误文本回显了输入：%v", name, err)
		}
	}
}

// TestContentSizeLimit 钉住 1 MiB 的上限：Encode 的结果恰为 MaxContentSize 字节时成功、多 1 字节时失败；DecodeContent 同样
// 接受恰为上限的输入、拒绝多 1 字节的输入；MaxContentSize 等于 1 MiB（与 payload.MaxPlaintext 相同，由 internal/app 核对）。
func TestContentSizeLimit(t *testing.T) {
	if MaxContentSize != 1<<20 {
		t.Fatalf("MaxContentSize = %d，契约规定为 1 MiB", MaxContentSize)
	}
	c := validContent()
	c.Body = ""
	empty, err := c.Encode()
	if err != nil {
		t.Fatalf("Encode 返回错误：%v", err)
	}
	c.Body = strings.Repeat("a", MaxContentSize-len(empty))
	data, err := c.Encode()
	if err != nil || len(data) != MaxContentSize {
		t.Fatalf("恰为上限：Encode 得到 %d 字节，错误 %v", len(data), err)
	}
	if got, err := DecodeContent(data); err != nil || got != c {
		t.Errorf("恰为上限的输入没有解码回原值（错误 %v）", err)
	}
	c.Body += "a"
	if data, err := c.Encode(); !errors.Is(err, ErrInvalidContent) || data != nil {
		t.Errorf("超过上限 1 字节：Encode = %d 字节, %v；期望 ErrInvalidContent", len(data), err)
	}
	// 解码一侧：在合法 JSON 之后补空白使它恰为上限（尾随空白合法），再多 1 字节即拒绝。
	c.Body = "b"
	small, err := c.Encode()
	if err != nil {
		t.Fatalf("Encode 返回错误：%v", err)
	}
	padded := append(bytes.Clone(small), bytes.Repeat([]byte(" "), MaxContentSize-len(small))...)
	if _, err := DecodeContent(padded); err != nil {
		t.Errorf("恰为上限的输入被拒绝：%v", err)
	}
	if _, err := DecodeContent(append(padded, ' ')); !errors.Is(err, ErrInvalidContent) {
		t.Errorf("超过上限 1 字节的输入返回 %v，期望 ErrInvalidContent", err)
	}
}

// contentHolder 把 Content 放在导出字段中（值与指针各一个），用来检查嵌入其他结构体时的格式化输出。
type contentHolder struct {
	Value   Content
	Pointer *Content
}

// TestContentRedaction 断言 Content 的全部常用格式化出口（fmt 的各动词、嵌入导出字段、切片与映射、slog 的 JSON 与文本处理器、
// json.Marshal、String 与 LogValue）都只输出固定的脱敏文本，不含标题、正文与任务 ID。
func TestContentRedaction(t *testing.T) {
	c := validContent()
	c.Title = "金丝雀标题CANARYTITLE"
	c.Body = "金丝雀正文CANARYBODY"
	forbidden := []string{"CANARY", "金丝雀", testTask}
	// clean 断言输出中没有任何禁止出现的文字。
	clean := func(label, out string) {
		t.Helper()
		for _, bad := range forbidden {
			if strings.Contains(out, bad) {
				t.Errorf("%s 泄漏了通知内容：%q", label, out)
				return
			}
		}
	}
	holder := contentHolder{Value: c, Pointer: &c}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%t", "%10v", "%-40s"} {
		for _, v := range []any{c, &c} {
			if out := fmt.Sprintf(verb, v); out != contentMarker {
				t.Errorf("%s 作用于 %T 得到 %q，期望 %q", verb, v, out, contentMarker)
			}
		}
		for _, v := range []any{[]Content{c}, []*Content{&c}, map[string]Content{"c": c}, holder, &holder} {
			out := fmt.Sprintf(verb, v)
			if !strings.Contains(out, contentMarker) {
				t.Errorf("%s 作用于 %T 得到 %q，缺少脱敏文本", verb, v, out)
			}
			clean(fmt.Sprintf("%s 作用于 %T", verb, v), out)
		}
	}
	var buf bytes.Buffer
	for _, logger := range []*slog.Logger{slog.New(slog.NewJSONHandler(&buf, nil)), slog.New(slog.NewTextHandler(&buf, nil))} {
		buf.Reset()
		logger.Info("notification", slog.Any("value", c), slog.Any("pointer", &c), slog.Any("holder", holder))
		if out := buf.String(); !strings.Contains(out, contentMarker) {
			t.Errorf("slog 输出缺少脱敏文本：%q", out)
		} else {
			clean("slog 输出", out)
		}
	}
	for _, v := range []any{c, &c, holder, []Content{c}} {
		data, err := json.Marshal(v)
		if err != nil || !strings.Contains(string(data), `"`+contentMarker+`"`) {
			t.Errorf("json.Marshal(%T) = %s, %v；期望脱敏文本", v, data, err)
		}
		clean(fmt.Sprintf("json.Marshal(%T)", v), string(data))
	}
	if c.String() != contentMarker {
		t.Error("String 没有返回脱敏文本")
	}
	if v := c.LogValue(); v.Kind() != slog.KindString || v.String() != contentMarker {
		t.Error("LogValue 没有返回脱敏文本")
	}
}

// TestContentErrorsDoNotEcho 断言 Encode 与 DecodeContent 的错误文本不含输入中的标题、正文、事件与任务 ID。
func TestContentErrorsDoNotEcho(t *testing.T) {
	bad := Content{Version: 2, Event: "CANARYEVENT", TaskID: "CANARYTASK", Title: "CANARYTITLE", Body: "CANARYBODY"}
	for _, edit := range []func(*Content){
		func(c *Content) {},
		func(c *Content) { c.Version = 1 },
		func(c *Content) { c.Version, c.Event = 1, EventFailed },
	} {
		c := bad
		edit(&c)
		_, err := c.Encode()
		if err == nil || strings.Contains(err.Error(), "CANARY") {
			t.Errorf("Encode 的错误 %v 为空或回显了输入", err)
		}
		wire := fmt.Sprintf(`{"v":%d,"event":%q,"task":%q,"title":%q,"body":%q}`, c.Version, c.Event, c.TaskID, c.Title, c.Body)
		_, err = DecodeContent([]byte(wire))
		if err == nil || strings.Contains(err.Error(), "CANARY") {
			t.Errorf("DecodeContent 的错误 %v 为空或回显了输入", err)
		}
	}
	big := validContent()
	big.Body = strings.Repeat("CANARYBODY", MaxContentSize/10+1)
	if _, err := big.Encode(); err == nil || strings.Contains(err.Error(), "CANARY") {
		t.Errorf("超长内容的错误 %v 为空或回显了输入", err)
	}
}
