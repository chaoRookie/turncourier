// Package token 测试回复令牌 v1：已知答案、确定性、篡改穷举、绑定字段、有效期边界、解析错误、非法输入、
// 通知 ID、脱敏输出与键控摘要。密钥、nid 与令牌文本都在运行时构造，源码中不出现令牌或密钥形状的字面量。
package token

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// refAlphabet 是规格给出的小写 Crockford base32 字母表；参考实现独立使用它，不引用被测包的常量。
const refAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// 规格规定的脱敏文本，直接写出以钉住输出，不引用被测包的常量。
const (
	tokenMarker = "[redacted reply token]"
	keyMarker   = "[redacted token key]"
)

// refEncoding 是参考实现使用的无填充 base32 编码。
var refEncoding = base32.NewEncoding(refAlphabet).WithPadding(base32.NoPadding)

// testExpiry 是已知答案向量的有效期；testNow 早于它，令牌在该时刻有效。
var (
	testExpiry = time.UnixMilli(1_790_000_000_000)
	testNow    = time.UnixMilli(1_789_000_000_000)
)

// tokenHolder 含导出的令牌字段，用来确认 fmt 格式化嵌套字段时同样调用令牌的格式化方法。
type tokenHolder struct {
	T Token
}

// keyHolder 含导出的密钥字段，用途同 tokenHolder。
type keyHolder struct {
	K Key
}

// sequence 返回从 start 起逐个加 1 的 n 个字节，用于在运行时构造测试向量。
func sequence(n int, start byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = start + byte(i)
	}
	return out
}

// testClaims 返回已知答案向量的绑定字段。
func testClaims() Claims {
	return Claims{TaskID: "0123456789", Owner: "local", ExpiresAt: testExpiry}
}

// testNID 返回已知答案向量的通知 ID：字节 0x00…0x0b。
func testNID() NID {
	return NID(sequence(12, 0))
}

// mustKey 用给定密钥号与材料构造密钥，失败时终止测试。
func mustKey(t *testing.T, id uint8, material []byte) *Key {
	t.Helper()
	k, err := NewKey(id, material)
	if err != nil {
		t.Fatalf("NewKey(%d): %v", id, err)
	}
	return k
}

// fixture 返回已知答案向量的密钥（kid 1，字节 0x01…0x20）与据此签发的令牌。
func fixture(t *testing.T) (*Key, Token) {
	t.Helper()
	k := mustKey(t, 1, sequence(KeyLen, 1))
	issued, err := Issue(k, testNID(), testClaims())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return k, issued
}

// referenceRaw 按规格的字节布局与 MAC 输入逐字节拼接，直接调用 crypto/hmac 得到 30 字节令牌，不调用被测函数。
func referenceRaw(material []byte, kid byte, nid []byte, c Claims) []byte {
	msg := []byte("turncourier/reply-token/v1\x00")
	msg = append(msg, 0x01, kid)
	msg = append(msg, nid...)
	msg = append(msg, c.TaskID...)
	msg = binary.BigEndian.AppendUint16(msg, uint16(len(c.Owner)))
	msg = append(msg, c.Owner...)
	msg = binary.BigEndian.AppendUint64(msg, uint64(c.ExpiresAt.UnixMilli()))
	mac := hmac.New(sha256.New, material)
	mac.Write(msg)
	raw := append([]byte{0x01, kid}, nid...)
	return append(raw, mac.Sum(nil)[:16]...)
}

// withChar 返回把 text 第 i 个字节替换为 s 后的字符串。
func withChar(text string, i int, s string) string {
	return text[:i] + s + text[i+1:]
}

// accepted 判断文本能否通过解析，并在有效期内以已知答案的密钥与绑定字段通过验证。
func accepted(k *Key, text string) bool {
	parsed, err := Parse(text)
	return err == nil && Verify(k, parsed, testClaims(), testNow) == nil
}

// TestKnownAnswer 确认签发结果与独立参考实现一致，并与写入测试的 30 个原始字节一致，防止两边同时改错。
func TestKnownAnswer(t *testing.T) {
	k, issued := fixture(t)
	nid := testNID()
	if issued.Reveal() != refEncoding.EncodeToString(referenceRaw(sequence(KeyLen, 1), 1, nid[:], testClaims())) {
		t.Fatal("Reveal differs from the reference implementation")
	}
	pinned := []byte{
		0x01, 0x01, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x21,
		0xad, 0xb4, 0xb7, 0xad, 0xc0, 0xc8, 0xed, 0xa6, 0xfd, 0x68, 0x0d, 0x3d, 0xc8, 0xb6, 0x20,
	}
	if issued.Reveal() != refEncoding.EncodeToString(pinned) {
		t.Fatal("Reveal differs from the pinned raw bytes")
	}
	if k.ID() != 1 || issued.KeyID() != 1 || issued.NID() != nid {
		t.Errorf("ID %d, KeyID %d, NID match %v", k.ID(), issued.KeyID(), issued.NID() == nid)
	}
}

// TestIssueDeterministicAndFormat 确认同样的输入得到同样的令牌，文本为 48 个字母表字符，且解析不区分大小写。
func TestIssueDeterministicAndFormat(t *testing.T) {
	k, issued := fixture(t)
	again, err := Issue(k, testNID(), testClaims())
	if err != nil || again != issued {
		t.Fatalf("second Issue differs (err %v)", err)
	}
	text := issued.Reveal()
	if len(text) != TextLen || TextLen != 48 {
		t.Fatalf("text length %d, TextLen %d", len(text), TextLen)
	}
	for i := range len(text) {
		if !strings.ContainsRune(refAlphabet, rune(text[i])) {
			t.Fatalf("byte %d is outside the alphabet", i)
		}
	}
	for _, variant := range []string{text, strings.ToUpper(text)} {
		parsed, err := Parse(variant)
		if err != nil || parsed != issued {
			t.Errorf("Parse round trip failed (err %v)", err)
		}
	}
}

// TestTamperRejected 穷举 48 个位置各换成另外 31 个字符，以及 30 个原始字节的 240 个单比特翻转，没有一个被接受。
func TestTamperRejected(t *testing.T) {
	k, issued := fixture(t)
	text := issued.Reveal()
	if !accepted(k, text) {
		t.Fatal("original token is not accepted")
	}
	variants := 0
	for i := range TextLen {
		for _, c := range refAlphabet {
			if byte(c) == text[i] {
				continue
			}
			variants++
			if accepted(k, withChar(text, i, string(c))) {
				t.Errorf("variant with %q at position %d accepted", c, i)
			}
		}
	}
	if variants != 1488 {
		t.Errorf("tried %d character variants, want 1488", variants)
	}
	raw, err := refEncoding.DecodeString(text)
	if err != nil || len(raw) != 30 {
		t.Fatalf("decode reference: %v", err)
	}
	for bit := range 240 {
		flipped := slices.Clone(raw)
		flipped[bit/8] ^= 1 << (bit % 8)
		if accepted(k, refEncoding.EncodeToString(flipped)) {
			t.Errorf("bit flip %d accepted", bit)
		}
	}
}

// TestClaimsBinding 确认任务 ID、owner、有效期与密钥都参与验证，且检查顺序为密钥号、绑定字段、标签。
func TestClaimsBinding(t *testing.T) {
	k, issued := fixture(t)
	base := testClaims()
	if err := Verify(k, issued, base, testNow); err != nil {
		t.Fatalf("Verify original: %v", err)
	}
	changed := map[string]Claims{
		"task id":   {TaskID: "0123456788", Owner: base.Owner, ExpiresAt: base.ExpiresAt},
		"owner":     {TaskID: base.TaskID, Owner: "Local", ExpiresAt: base.ExpiresAt},
		"owner sp":  {TaskID: base.TaskID, Owner: "local ", ExpiresAt: base.ExpiresAt},
		"expiry +1": {TaskID: base.TaskID, Owner: base.Owner, ExpiresAt: base.ExpiresAt.Add(time.Millisecond)},
		"expiry -1": {TaskID: base.TaskID, Owner: base.Owner, ExpiresAt: base.ExpiresAt.Add(-time.Millisecond)},
	}
	for name, c := range changed {
		if err := Verify(k, issued, c, testNow); !errors.Is(err, ErrBadMAC) {
			t.Errorf("%s: Verify = %v, want ErrBadMAC", name, err)
		}
	}
	otherID := mustKey(t, 2, sequence(KeyLen, 1))
	if err := Verify(otherID, issued, base, testNow); !errors.Is(err, ErrKeyMismatch) {
		t.Errorf("kid 2: Verify = %v, want ErrKeyMismatch", err)
	}
	if err := Verify(otherID, issued, Claims{}, testNow); !errors.Is(err, ErrKeyMismatch) {
		t.Errorf("kid 2 with invalid claims: Verify = %v, want ErrKeyMismatch first", err)
	}
	otherMaterial := mustKey(t, 1, bytes.Repeat([]byte{0x5a}, KeyLen))
	if err := Verify(otherMaterial, issued, base, testNow); !errors.Is(err, ErrBadMAC) {
		t.Errorf("other material: Verify = %v, want ErrBadMAC", err)
	}
	if err := Verify(otherMaterial, issued, Claims{}, testNow); !errors.Is(err, ErrInvalidClaims) {
		t.Errorf("other material with invalid claims: Verify = %v, want ErrInvalidClaims before the tag", err)
	}
}

// TestExpiryBoundary 确认验证时刻等于有效期即过期、早 1 毫秒仍有效，且被篡改的过期令牌报告标签错误。
func TestExpiryBoundary(t *testing.T) {
	k, issued := fixture(t)
	c := testClaims()
	if err := Verify(k, issued, c, c.ExpiresAt); !errors.Is(err, ErrExpired) {
		t.Errorf("now == ExpiresAt: Verify = %v, want ErrExpired", err)
	}
	if err := Verify(k, issued, c, c.ExpiresAt.Add(-time.Millisecond)); err != nil {
		t.Errorf("now == ExpiresAt-1ms: Verify = %v, want nil", err)
	}
	tampered := issued
	tampered.tag[0] ^= 1
	if err := Verify(k, tampered, c, c.ExpiresAt.Add(time.Hour)); !errors.Is(err, ErrBadMAC) {
		t.Errorf("tampered and expired: Verify = %v, want ErrBadMAC", err)
	}
}

// TestParseMalformed 确认长度不符、字母表之外的字符、错误的版本字节、kid 为 0 与多字节字符都返回固定的 ErrMalformed。
func TestParseMalformed(t *testing.T) {
	_, issued := fixture(t)
	text := issued.Reveal()
	raw, err := refEncoding.DecodeString(text)
	if err != nil {
		t.Fatalf("decode reference: %v", err)
	}
	inputs := map[string]string{"47 chars": text[:47], "49 chars": text + "0", "multibyte": text[:45] + "中"}
	for _, c := range []string{"i", "l", "o", "u", "I", "L", "O", "U", "=", " ", "\n"} {
		inputs[fmt.Sprintf("char %q", c)] = withChar(text, 9, c)
	}
	version := slices.Clone(raw)
	version[0] = 0x02
	inputs["version 2"] = refEncoding.EncodeToString(version)
	kid := slices.Clone(raw)
	kid[1] = 0
	inputs["kid 0"] = refEncoding.EncodeToString(kid)
	for name, input := range inputs {
		if len(input) != 47 && len(input) != 49 && len(input) != TextLen {
			t.Fatalf("%s: unexpected test input length %d", name, len(input))
		}
		_, err := Parse(input)
		if !errors.Is(err, ErrMalformed) || err.Error() != "malformed reply token" {
			t.Errorf("%s: Parse error = %v, want ErrMalformed", name, err)
		}
	}
}

// TestNewKeyValidation 确认密钥号为 0 或长度不是 32 字节时返回 ErrInvalidKey，且构造后修改传入的切片不影响签发。
func TestNewKeyValidation(t *testing.T) {
	cases := []struct {
		id uint8
		n  int
	}{{0, KeyLen}, {1, 0}, {1, 31}, {1, 33}}
	for _, tc := range cases {
		if k, err := NewKey(tc.id, sequence(tc.n, 1)); !errors.Is(err, ErrInvalidKey) || k != nil {
			t.Errorf("NewKey(%d, %d bytes) = %v, %v; want nil, ErrInvalidKey", tc.id, tc.n, k, err)
		}
	}
	material := sequence(KeyLen, 1)
	k := mustKey(t, 1, material)
	before, err := Issue(k, testNID(), testClaims())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	material[0] ^= 0xff
	after, err := Issue(k, testNID(), testClaims())
	if err != nil || after != before {
		t.Errorf("modifying the caller's slice changed the token (err %v)", err)
	}
	if _, err := Issue(&Key{}, testNID(), testClaims()); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Issue with the zero Key = %v, want ErrInvalidKey", err)
	}
}

// TestInvalidClaims 确认任务 ID、owner 或有效期不合法时 Issue 与 Verify 都返回 ErrInvalidClaims，并钉住合法边界。
func TestInvalidClaims(t *testing.T) {
	k, issued := fixture(t)
	base := testClaims()
	invalid := map[string]Claims{
		"task id 9":     {TaskID: "012345678", Owner: base.Owner, ExpiresAt: base.ExpiresAt},
		"task id 11":    {TaskID: "0123456789a", Owner: base.Owner, ExpiresAt: base.ExpiresAt},
		"task id u":     {TaskID: "012345678u", Owner: base.Owner, ExpiresAt: base.ExpiresAt},
		"task id upper": {TaskID: "012345678A", Owner: base.Owner, ExpiresAt: base.ExpiresAt},
		"owner empty":   {TaskID: base.TaskID, Owner: "", ExpiresAt: base.ExpiresAt},
		"owner 256":     {TaskID: base.TaskID, Owner: strings.Repeat("o", 256), ExpiresAt: base.ExpiresAt},
		"expiry zero":   {TaskID: base.TaskID, Owner: base.Owner},
		"expiry epoch":  {TaskID: base.TaskID, Owner: base.Owner, ExpiresAt: time.UnixMilli(0)},
	}
	for name, c := range invalid {
		if _, err := Issue(k, testNID(), c); !errors.Is(err, ErrInvalidClaims) {
			t.Errorf("%s: Issue = %v, want ErrInvalidClaims", name, err)
		}
		if err := Verify(k, issued, c, testNow); !errors.Is(err, ErrInvalidClaims) {
			t.Errorf("%s: Verify = %v, want ErrInvalidClaims", name, err)
		}
	}
	boundary := Claims{TaskID: "zyxwvtsrqp", Owner: strings.Repeat("o", 255), ExpiresAt: time.UnixMilli(1)}
	signed, err := Issue(k, testNID(), boundary)
	if err != nil {
		t.Fatalf("Issue at the valid boundary: %v", err)
	}
	if err := Verify(k, signed, boundary, time.UnixMilli(0)); err != nil {
		t.Errorf("Verify at the valid boundary: %v", err)
	}
}

// TestNewNID 确认 NewNID 读取恰好 12 字节（分次返回的读取器也能读满），读取器不足时报错，真实随机源不重复。
func TestNewNID(t *testing.T) {
	source := sequence(20, 0x40)
	reader := bytes.NewReader(source)
	nid, err := NewNID(iotest.OneByteReader(reader))
	if err != nil || nid != NID(source[:12]) || reader.Len() != 8 {
		t.Fatalf("NewNID = %x, %v, %d bytes left; want the first 12 bytes and 8 left", nid, err, reader.Len())
	}
	if _, err := NewNID(bytes.NewReader(sequence(11, 0))); err == nil {
		t.Error("NewNID with 11 bytes succeeded")
	}
	seen := make(map[NID]bool)
	for range 1000 {
		nid, err := NewNID(rand.Reader)
		if err != nil {
			t.Fatalf("NewNID(rand.Reader): %v", err)
		}
		if seen[nid] {
			t.Fatal("NewNID repeated a value")
		}
		seen[nid] = true
	}
}

// TestRedaction 以金丝雀检查令牌与密钥的全部格式化出口：fmt 各动词、嵌套容器、slog 两种处理器与错误文本
// 都只输出脱敏文本，不含令牌文本的任何 10 字符子串，也不含密钥材料的十六进制与 base64 文本。
func TestRedaction(t *testing.T) {
	k, issued := fixture(t)
	text := issued.Reveal()
	material := sequence(KeyLen, 1)
	var forbidden []string
	for i := 0; i+10 <= len(text); i++ {
		forbidden = append(forbidden, text[i:i+10], strings.ToUpper(text[i:i+10]))
	}
	forbidden = append(forbidden,
		hex.EncodeToString(material), strings.ToUpper(hex.EncodeToString(material)),
		base64.StdEncoding.EncodeToString(material), base64.RawURLEncoding.EncodeToString(material))
	clean := func(label, out string) {
		for _, bad := range forbidden {
			if strings.Contains(out, bad) {
				t.Errorf("%s leaks secret material", label)
				return
			}
		}
	}
	for _, verb := range []string{"%v", "%s", "%q", "%x", "%X", "%d", "%+v", "%#v"} {
		for _, v := range []any{issued, &issued} {
			if out := fmt.Sprintf(verb, v); out != tokenMarker {
				t.Errorf("%s of %T = %q, want %q", verb, v, out, tokenMarker)
			}
		}
		for _, v := range []any{*k, k} {
			if out := fmt.Sprintf(verb, v); out != keyMarker {
				t.Errorf("%s of %T = %q, want %q", verb, v, out, keyMarker)
			}
		}
		for _, v := range []any{[]Token{issued}, map[string]Token{"t": issued}, tokenHolder{T: issued}, &tokenHolder{T: issued}} {
			out := fmt.Sprintf(verb, v)
			if !strings.Contains(out, tokenMarker) {
				t.Errorf("%s of %T = %q, want it to contain %q", verb, v, out, tokenMarker)
			}
			clean(fmt.Sprintf("%s of %T", verb, v), out)
		}
		for _, v := range []any{[]Key{*k}, map[string]*Key{"k": k}, keyHolder{K: *k}, &keyHolder{K: *k}} {
			out := fmt.Sprintf(verb, v)
			if !strings.Contains(out, keyMarker) {
				t.Errorf("%s of %T = %q, want it to contain %q", verb, v, out, keyMarker)
			}
			clean(fmt.Sprintf("%s of %T", verb, v), out)
		}
	}
	var buf bytes.Buffer
	for _, logger := range []*slog.Logger{slog.New(slog.NewJSONHandler(&buf, nil)), slog.New(slog.NewTextHandler(&buf, nil))} {
		buf.Reset()
		logger.Info("issued", slog.Any("t", issued), slog.Any("k", k))
		out := buf.String()
		if !strings.Contains(out, tokenMarker) || !strings.Contains(out, keyMarker) {
			t.Errorf("slog output %q lacks the redaction markers", out)
		}
		clean("slog output", out)
	}
	if issued.String() != tokenMarker || k.String() != keyMarker {
		t.Error("String does not return the redaction marker")
	}
	if issued.LogValue().String() != tokenMarker || k.LogValue().String() != keyMarker {
		t.Error("LogValue does not return the redaction marker")
	}
	flipped := withChar(text, TextLen-1, "0")
	if flipped == text {
		flipped = withChar(text, TextLen-1, "1")
	}
	parsedFlip, err := Parse(flipped)
	if err != nil {
		t.Fatalf("Parse(flipped): %v", err)
	}
	var errs []error
	for _, input := range []string{text[:47], text + "0", withChar(text, 9, "u")} {
		_, err := Parse(input)
		errs = append(errs, err)
	}
	errs = append(errs,
		Verify(k, parsedFlip, testClaims(), testNow),
		Verify(k, issued, testClaims(), testExpiry),
		Verify(mustKey(t, 2, material), issued, testClaims(), testNow),
		Verify(k, issued, Claims{}, testNow))
	for i, err := range errs {
		if err == nil {
			t.Errorf("error %d is nil", i)
			continue
		}
		clean(fmt.Sprintf("error %d", i), err.Error())
	}
}

// TestBodyDigest 确认键控摘要与直接调用 crypto/hmac 的参考实现一致，依赖密钥，且不同于无键摘要与不带前缀的 HMAC。
func TestBodyDigest(t *testing.T) {
	k, _ := fixture(t)
	body := []byte("new reply body\n")
	reference := hmac.New(sha256.New, sequence(KeyLen, 1))
	reference.Write([]byte("turncourier/body-digest/v1\x00"))
	reference.Write(body)
	got := k.BodyDigest(body)
	if got != [32]byte(reference.Sum(nil)) {
		t.Fatal("BodyDigest differs from the reference implementation")
	}
	if mustKey(t, 1, bytes.Repeat([]byte{0x5a}, KeyLen)).BodyDigest(body) == got {
		t.Error("BodyDigest does not depend on the key")
	}
	if got == sha256.Sum256(body) {
		t.Error("BodyDigest equals the unkeyed SHA-256")
	}
	unprefixed := hmac.New(sha256.New, sequence(KeyLen, 1))
	unprefixed.Write(body)
	if got == [32]byte(unprefixed.Sum(nil)) {
		t.Error("BodyDigest equals the HMAC without the domain prefix")
	}
	macDomain, digestDomain := "turncourier/reply-token/v1\x00", "turncourier/body-digest/v1\x00"
	if strings.HasPrefix(macDomain, digestDomain) || strings.HasPrefix(digestDomain, macDomain) {
		t.Error("domain prefixes overlap")
	}
}

// FuzzParse 确认 Parse 对任意输入不 panic；成功时输入恰为 48 字节，Reveal 等于输入的小写形式。
func FuzzParse(f *testing.F) {
	k, err := NewKey(1, sequence(KeyLen, 1))
	if err != nil {
		f.Fatal(err)
	}
	issued, err := Issue(k, testNID(), testClaims())
	if err != nil {
		f.Fatal(err)
	}
	text := issued.Reveal()
	raw, err := refEncoding.DecodeString(text)
	if err != nil {
		f.Fatal(err)
	}
	version := slices.Clone(raw)
	version[0] = 0x02
	kid := slices.Clone(raw)
	kid[1] = 0
	seeds := []string{
		text, strings.ToUpper(text), text[:47], text + "0", text[:45] + "中", "",
		refEncoding.EncodeToString(version), refEncoding.EncodeToString(kid),
	}
	for _, c := range []string{"i", "l", "o", "u", "=", " ", "\n"} {
		seeds = append(seeds, withChar(text, 9, c))
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		parsed, err := Parse(input)
		if err != nil {
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("Parse error %v is not ErrMalformed", err)
			}
			return
		}
		if len(input) != TextLen || parsed.Reveal() != strings.ToLower(input) {
			t.Fatalf("accepted input of length %d does not round-trip", len(input))
		}
	})
}
