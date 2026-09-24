// Package parser 的模糊测试：任意字节交给 Parse 与 ParseHeader 都不 panic，两者对头部的判定一致；NewText 成功时结果是
// 合法 UTF-8、不超过 1 MiB、首尾没有空白、不含残留，重复调用结果相同；错误一律是哨兵。另以任意文字直接驱动 NewText 解码之后的
// 步骤，并确认经 MIME 与不经 MIME 两条路径的结果相同。种子取自合成回归样本（令牌在运行时签发）与上限附近的构造邮件。
package parser

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	mailfixture "github.com/chaoRookie/turncourier/tests/fixtures/mail"
)

// isSentinel 判断 err 是否恰为本包的某个哨兵错误。
func isSentinel(err error) bool {
	return slices.Contains([]error{ErrMalformed, ErrHeaderTooLarge, ErrNoPlainText, ErrUncertain, ErrEmpty}, err)
}

// checkNewText 检查一次 NewText 的结果：成功时是去掉首尾空白、非空、合法 UTF-8、不超过 1 MiB 且没有残留的文字，
// 失败时是 NewText 允许的四个哨兵之一且结果为空。
func checkNewText(t *testing.T, text string, err error) {
	t.Helper()
	if err != nil {
		if text != "" || !slices.Contains([]error{ErrNoPlainText, ErrUncertain, ErrEmpty, ErrMalformed}, err) {
			t.Fatalf("NewText = %q, %v", text, err)
		}
		return
	}
	if text == "" || strings.TrimSpace(text) != text || !utf8.ValidString(text) || len(text) > maxPartBytes || hasResidue(text) {
		t.Fatalf("NewText result breaks its guarantees (%d bytes)", len(text))
	}
}

// FuzzParse 以任意字节驱动 Parse 与 ParseHeader：不 panic；两者要么都失败于同一个哨兵，要么都成功且头部字段相同；
// 成功时 NewText 满足 checkNewText 且两次调用结果相同，BodyPrefix 的两段都不超过 512 字节。
func FuzzParse(f *testing.F) {
	values := fixtureValues(f)
	for _, name := range mailfixture.Names() {
		f.Add(buildSample(f, name, values))
	}
	f.Add(nestedMessage(maxDepth + 1))
	f.Add(partsMessage(maxParts+1, 1))
	f.Add(plainMessage("text/plain; charset=gbk", "ok\x81\x20ok"))
	f.Add(plainMessage("multipart/mixed; boundary=b", "--b\r\nContent-Type: text/plain\r\n\r\n> x\r\ny\r\n--b--"))
	f.Add([]byte("From: a@example.invalid\r\nSubject: =?gbk?B?1eLKx9bQzsQ=?=\r\n\r\n\ufeff正文\r\n-- \r\n签名"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		m, err := Parse(raw)
		h, headerErr := ParseHeader(raw)
		if err != headerErr {
			t.Fatalf("Parse = %v, ParseHeader = %v", err, headerErr)
		}
		if err != nil {
			if m != nil || h != nil || !isSentinel(err) {
				t.Fatalf("failed parse returns %v with a message", err)
			}
			return
		}
		if headerFields(m) != headerFields(h) {
			t.Fatalf("Parse and ParseHeader disagree on the header")
		}
		text, textErr := m.NewText()
		checkNewText(t, text, textErr)
		again, againErr := m.NewText()
		if again != text || !errors.Is(againErr, textErr) {
			t.Fatalf("NewText is not deterministic")
		}
		plain, html, prefixErr := m.BodyPrefix()
		if prefixErr != nil || len(plain) > PrefixLen || len(html) > PrefixLen {
			t.Fatalf("BodyPrefix = %d, %d bytes, %v", len(plain), len(html), prefixErr)
		}
	})
}

// FuzzNewTextBody 以任意文字作为纯文本正文：直接调用 newText 与经过一封 utf-8、8bit 的单部件邮件调用 NewText，
// 两条路径的结果必须相同；结果满足 checkNewText，且不长于输入。非法 UTF-8 的输入只走 MIME 路径，必须是 ErrMalformed。
func FuzzNewTextBody(f *testing.F) {
	issued := issueText(f, fixtureTask, 0x21)
	for _, seed := range []string{
		"继续执行。\r\n\r\n------------------ 原始邮件 ------------------\r\n旧内容",
		"新文字\nX wrote:\n\n> 引用\n\n作答",
		"新文字\n> ---- TurnCourier ----\n-- \n签名",
		"请运行：\n> make test\n发自我的iPhone",
		"正文\n--\n1\n2\n3\n4\n5\n6",
		"发件人：X\n发送时间：Y\n正文",
		"令牌 " + issued + " 残留",
		"\ufeff\r\r正文\r\n",
		"\xff\xfe",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, body string) {
		if len(body) > maxPartBytes {
			return
		}
		viaMIME, mimeErr := mustParse(t, plainMessage("text/plain; charset=utf-8", body, "Content-Transfer-Encoding: 8bit")).NewText()
		if !utf8.ValidString(body) {
			if viaMIME != "" || !errors.Is(mimeErr, ErrMalformed) {
				t.Fatalf("invalid UTF-8 body gives %q, %v", viaMIME, mimeErr)
			}
			return
		}
		text, err := newText(body)
		checkNewText(t, text, err)
		if text != viaMIME || !errors.Is(mimeErr, err) || (err == nil) != (mimeErr == nil) {
			t.Fatalf("MIME path gives %q, %v; direct path gives %q, %v", viaMIME, mimeErr, text, err)
		}
		if len(text) > len(body) {
			t.Fatalf("new text is longer than the body")
		}
	})
}
