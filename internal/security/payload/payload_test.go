// Package payload 测试正文加密：往返与密文格式、随机 nonce、篡改、绑定调换、截断与超长、非法输入、
// 与按规格独立拼接关联数据的参考实现互通，以及错误文本与格式化输出不泄露明文和密钥材料。
// 密钥与明文都在运行时按下标填充构造，源码中不出现密钥形状的字面量。
package payload

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// 规格规定的数值与文本，直接写出以钉住被测包的常量，不引用它们。
const (
	maxPlain   = 1 << 20                  // 明文上限 1 MiB
	overhead   = 29                       // 格式 1 + nonce 12 + 标签 16
	keyMarker  = "[redacted payload key]" // 密钥的脱敏文本
	testTask   = "0123456789"             // 合法的任务 ID
	canaryText = "PLAINTEXT-CANARY"       // 明文金丝雀
)

// keyHolder 含导出的密钥字段，用来确认 fmt 格式化嵌套字段时同样调用密钥的格式化方法。
type keyHolder struct {
	K Key
}

// sequence 返回从 start 起逐个加 1（按字节回绕）的 n 个字节，用于在运行时构造密钥与明文。
func sequence(n int, start byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = start + byte(i)
	}
	return out
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

// mustSeal 加密并在失败时终止测试。
func mustSeal(t *testing.T, k *Key, kind Kind, taskID string, seq int64, plaintext []byte) []byte {
	t.Helper()
	sealed, err := k.Seal(kind, taskID, seq, plaintext)
	if err != nil {
		t.Fatalf("Seal(%c, %s, %d, %d bytes): %v", kind, taskID, seq, len(plaintext), err)
	}
	return sealed
}

// referenceAD 按规格逐段拼接关联数据，不调用被测包：前缀 ‖ 用途 ‖ kid ‖ 任务 ID ‖ 序号（uint64 大端）。
func referenceAD(kind, kid byte, taskID string, seq uint64) []byte {
	ad := []byte("turncourier/payload/v1\x00")
	ad = append(ad, kind, kid)
	ad = append(ad, taskID...)
	return binary.BigEndian.AppendUint64(ad, seq)
}

// referenceGCM 直接用 crypto/aes 与 crypto/cipher 构造标准的 AES-GCM（12 字节 nonce、16 字节标签）。
func referenceGCM(t *testing.T, material []byte) cipher.AEAD {
	t.Helper()
	block, err := aes.NewCipher(material)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("cipher.NewGCM: %v", err)
	}
	return gcm
}

// referenceSeal 按规格的密文格式独立构造：0x01 ‖ nonce ‖ 密文 ‖ 标签，nonce 由调用方指定。
func referenceSeal(t *testing.T, material, nonce, ad, plaintext []byte) []byte {
	t.Helper()
	sealed := append([]byte{0x01}, nonce...)
	return referenceGCM(t, material).Seal(sealed, nonce, plaintext, ad)
}

// TestRoundTrip 确认 1、1000 与上限字节的明文在两种用途下都能往返，密文比明文长 29 字节且首字节为 1，并钉住导出常量。
func TestRoundTrip(t *testing.T) {
	if FormatV1 != 1 || KeyLen != 32 || Overhead != overhead || MaxPlaintext != maxPlain || KindReply != 'r' || KindNotification != 'n' {
		t.Fatal("exported constants differ from the specification")
	}
	k := mustKey(t, 7, sequence(KeyLen, 1))
	if k.ID() != 7 {
		t.Errorf("ID() = %d, want 7", k.ID())
	}
	for _, kind := range []Kind{KindReply, KindNotification} {
		for _, n := range []int{1, 1000, maxPlain} {
			plaintext := sequence(n, 3)
			sealed := mustSeal(t, k, kind, testTask, 1, plaintext)
			if len(sealed) != n+overhead || sealed[0] != 1 {
				t.Errorf("%c/%d: sealed length %d, first byte %d; want %d and 1", kind, n, len(sealed), sealed[0], n+overhead)
			}
			opened, err := k.Open(kind, testTask, 1, sealed)
			if err != nil || !bytes.Equal(opened, plaintext) {
				t.Errorf("%c/%d: Open = %d bytes, %v; want the original plaintext", kind, n, len(opened), err)
			}
		}
	}
}

// TestReferenceInterop 用独立拼接关联数据与格式的参考实现双向互通，钉住前缀、各段顺序、kid 与序号的大端 64 位编码，
// 以及 nonce ‖ 密文 ‖ 标签的布局；序号取超过 32 位的值，kid 取 7，都不与其他字段的取值重合。
func TestReferenceInterop(t *testing.T) {
	material := sequence(KeyLen, 1)
	k := mustKey(t, 7, material)
	const task = "zyxwvtsrqp"
	const seq = 0x0102030405060708
	plaintext := sequence(100, 9)
	for _, kind := range []Kind{KindReply, KindNotification} {
		ad := referenceAD(byte(kind), 7, task, seq)
		sealed := mustSeal(t, k, kind, task, seq, plaintext)
		opened, err := referenceGCM(t, material).Open(nil, sealed[1:13], sealed[13:], ad)
		if err != nil || !bytes.Equal(opened, plaintext) {
			t.Errorf("%c: reference cannot open Seal output: %v", kind, err)
		}
		ours, err := k.Open(kind, task, seq, referenceSeal(t, material, sequence(12, 0x40), ad, plaintext))
		if err != nil || !bytes.Equal(ours, plaintext) {
			t.Errorf("%c: Open cannot read the reference ciphertext: %v", kind, err)
		}
	}
}

// TestRandomNonce 确认同样的输入加密两次得到不同的 nonce（第 2–13 字节），两份密文都能解密。
func TestRandomNonce(t *testing.T) {
	k := mustKey(t, 1, sequence(KeyLen, 1))
	plaintext := sequence(64, 0)
	first := mustSeal(t, k, KindReply, testTask, 1, plaintext)
	second := mustSeal(t, k, KindReply, testTask, 1, plaintext)
	if bytes.Equal(first[1:13], second[1:13]) {
		t.Error("two seals of the same input share a nonce")
	}
	for i, sealed := range [][]byte{first, second} {
		if opened, err := k.Open(KindReply, testTask, 1, sealed); err != nil || !bytes.Equal(opened, plaintext) {
			t.Errorf("seal %d: Open = %v", i, err)
		}
	}
}

// TestTamperRejected 翻转 64 字节明文密文（93 字节）的每一个比特，744 个变体全部返回 ErrDecrypt。
func TestTamperRejected(t *testing.T) {
	k := mustKey(t, 1, sequence(KeyLen, 1))
	sealed := mustSeal(t, k, KindReply, testTask, 1, sequence(64, 0))
	if len(sealed) != 93 {
		t.Fatalf("sealed length %d, want 93", len(sealed))
	}
	variants := 0
	for i := range sealed {
		for bit := range 8 {
			tampered := bytes.Clone(sealed)
			tampered[i] ^= 1 << bit
			variants++
			if opened, err := k.Open(KindReply, testTask, 1, tampered); err != ErrDecrypt || opened != nil {
				t.Errorf("byte %d bit %d: Open = %d bytes, %v; want nil, ErrDecrypt", i, bit, len(opened), err)
			}
		}
	}
	if variants != 744 {
		t.Errorf("checked %d variants, want 744", variants)
	}
}

// TestBindingSwap 确认密文换到别的用途、任务、序号、密钥号或密钥下都返回 ErrDecrypt，序号 1 与 2 的密文互换后同样如此。
func TestBindingSwap(t *testing.T) {
	material := sequence(KeyLen, 1)
	k := mustKey(t, 1, material)
	sealed := mustSeal(t, k, KindReply, testTask, 5, sequence(64, 0))
	cases := []struct {
		name   string
		key    *Key
		kind   Kind
		taskID string
		seq    int64
	}{
		{"kind", k, KindNotification, testTask, 5},
		{"task", k, KindReply, "0123456788", 5},
		{"seq-1", k, KindReply, testTask, 4},
		{"seq+1", k, KindReply, testTask, 6},
		{"kid", mustKey(t, 2, material), KindReply, testTask, 5},
		{"key", mustKey(t, 1, sequence(KeyLen, 2)), KindReply, testTask, 5},
	}
	for _, tc := range cases {
		if opened, err := tc.key.Open(tc.kind, tc.taskID, tc.seq, sealed); err != ErrDecrypt || opened != nil {
			t.Errorf("%s: Open = %d bytes, %v; want nil, ErrDecrypt", tc.name, len(opened), err)
		}
	}
	one := mustSeal(t, k, KindNotification, testTask, 1, []byte("first"))
	two := mustSeal(t, k, KindNotification, testTask, 2, []byte("second"))
	if _, err := k.Open(KindNotification, testTask, 1, two); err != ErrDecrypt {
		t.Errorf("seq 1 with the seq 2 ciphertext = %v, want ErrDecrypt", err)
	}
	if _, err := k.Open(KindNotification, testTask, 2, one); err != ErrDecrypt {
		t.Errorf("seq 2 with the seq 1 ciphertext = %v, want ErrDecrypt", err)
	}
}

// TestTruncatedAndOversize 确认截断到 0–29 字节或少 1 字节、长于上限的输入返回 ErrDecrypt 而不 panic；
// 另用参考实现构造认证有效、但明文为空或超过上限的密文，钉住 Open 的长度上下界本身，而不只依赖标签校验失败。
func TestTruncatedAndOversize(t *testing.T) {
	material := sequence(KeyLen, 1)
	k := mustKey(t, 1, material)
	sealed := mustSeal(t, k, KindReply, testTask, 1, sequence(64, 0))
	lengths := []int{len(sealed) - 1}
	for n := range overhead + 1 {
		lengths = append(lengths, n)
	}
	for _, n := range lengths {
		if opened, err := k.Open(KindReply, testTask, 1, sealed[:n]); err != ErrDecrypt || opened != nil {
			t.Errorf("sealed[:%d]: Open = %d bytes, %v; want nil, ErrDecrypt", n, len(opened), err)
		}
	}
	oversize := append(mustSeal(t, k, KindReply, testTask, 1, sequence(maxPlain, 0)), 0)
	if _, err := k.Open(KindReply, testTask, 1, oversize); err != ErrDecrypt {
		t.Errorf("%d-byte input: Open = %v, want ErrDecrypt", len(oversize), err)
	}
	ad := referenceAD('r', 1, testTask, 1)
	nonce := sequence(12, 0x40)
	for _, n := range []int{0, maxPlain + 1} {
		authentic := referenceSeal(t, material, nonce, ad, sequence(n, 0))
		if opened, err := k.Open(KindReply, testTask, 1, authentic); err != ErrDecrypt || opened != nil {
			t.Errorf("authentic ciphertext of %d plaintext bytes: Open = %d bytes, %v; want nil, ErrDecrypt", n, len(opened), err)
		}
	}
}

// TestInvalidInput 确认非法的用途、任务 ID 与序号使 Seal 与 Open 返回 ErrInvalidInput（Open 先于长度检查报告），
// 空明文与超过上限的明文使 Seal 返回 ErrInvalidInput，密钥号为 0 或长度不是 32 字节使 NewKey 返回 ErrInvalidKey。
func TestInvalidInput(t *testing.T) {
	k := mustKey(t, 1, sequence(KeyLen, 1))
	sealed := mustSeal(t, k, KindReply, testTask, 1, []byte("x"))
	cases := []struct {
		name   string
		kind   Kind
		taskID string
		seq    int64
	}{
		{"kind x", 'x', testTask, 1},
		{"task 9 chars", KindReply, "012345678", 1},
		{"task with u", KindReply, "012345678u", 1},
		{"task 11 chars", KindReply, "0123456789a", 1},
		{"task uppercase", KindReply, "012345678A", 1},
		{"seq 0", KindReply, testTask, 0},
		{"seq -1", KindReply, testTask, -1},
	}
	for _, tc := range cases {
		if out, err := k.Seal(tc.kind, tc.taskID, tc.seq, []byte("x")); err != ErrInvalidInput || out != nil {
			t.Errorf("%s: Seal = %d bytes, %v; want nil, ErrInvalidInput", tc.name, len(out), err)
		}
		for _, input := range [][]byte{sealed, nil} {
			if out, err := k.Open(tc.kind, tc.taskID, tc.seq, input); err != ErrInvalidInput || out != nil {
				t.Errorf("%s: Open(%d bytes) = %d bytes, %v; want nil, ErrInvalidInput", tc.name, len(input), len(out), err)
			}
		}
	}
	for _, plaintext := range [][]byte{nil, {}, sequence(maxPlain+1, 0)} {
		if out, err := k.Seal(KindReply, testTask, 1, plaintext); err != ErrInvalidInput || out != nil {
			t.Errorf("%d-byte plaintext: Seal = %d bytes, %v; want nil, ErrInvalidInput", len(plaintext), len(out), err)
		}
	}
	keys := []struct {
		id uint8
		n  int
	}{{0, KeyLen}, {1, 16}, {1, 31}, {1, 33}}
	for _, tc := range keys {
		if got, err := NewKey(tc.id, sequence(tc.n, 1)); err != ErrInvalidKey || got != nil {
			t.Errorf("NewKey(%d, %d bytes) = %v, %v; want nil, ErrInvalidKey", tc.id, tc.n, got, err)
		}
	}
}

// TestNoLeak 确认含金丝雀的明文不会出现在任何错误文本中，密钥经 fmt 各动词、嵌套容器与 slog 两种处理器输出时
// 只有脱敏文本，不含密钥材料的十六进制与 base64 文本；%p 作用于密钥值时也只打印内部 AEAD 的地址。
func TestNoLeak(t *testing.T) {
	material := sequence(KeyLen, 1)
	k := mustKey(t, 1, material)
	plaintext := []byte(strings.Repeat("-", 24) + canaryText + strings.Repeat("-", 24))
	sealed := mustSeal(t, k, KindReply, testTask, 1, plaintext)
	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 1
	forbidden := []string{
		canaryText,
		hex.EncodeToString(material), strings.ToUpper(hex.EncodeToString(material)),
		base64.StdEncoding.EncodeToString(material), base64.RawURLEncoding.EncodeToString(material),
	}
	clean := func(label, out string) {
		for _, bad := range forbidden {
			if strings.Contains(out, bad) {
				t.Errorf("%s leaks the plaintext or key material", label)
				return
			}
		}
	}
	var errs []error
	record := func(_ []byte, err error) {
		errs = append(errs, err)
	}
	record(k.Seal('x', testTask, 1, plaintext))
	record(k.Seal(KindReply, testTask, 1, bytes.Repeat(plaintext, maxPlain/len(plaintext)+1)))
	record(k.Open(KindNotification, testTask, 1, sealed))
	record(k.Open(KindReply, testTask, 2, sealed))
	record(k.Open(KindReply, testTask, 0, sealed))
	record(k.Open(KindReply, testTask, 1, tampered))
	record(k.Open(KindReply, testTask, 1, sealed[:overhead]))
	record(mustKey(t, 1, sequence(KeyLen, 2)).Open(KindReply, testTask, 1, sealed))
	_, err := NewKey(1, material[:31])
	errs = append(errs, err)
	for i, err := range errs {
		if err == nil {
			t.Errorf("error %d is nil", i)
			continue
		}
		clean(fmt.Sprintf("error %d", i), err.Error())
	}
	for _, verb := range []string{"%v", "%s", "%q", "%x", "%X", "%d", "%+v", "%#v"} {
		for _, v := range []any{*k, k} {
			if out := fmt.Sprintf(verb, v); out != keyMarker {
				t.Errorf("%s of %T = %q, want %q", verb, v, out, keyMarker)
			}
		}
		for _, v := range []any{[]Key{*k}, []*Key{k}, map[string]*Key{"k": k}, keyHolder{K: *k}, &keyHolder{K: *k}} {
			out := fmt.Sprintf(verb, v)
			if !strings.Contains(out, keyMarker) {
				t.Errorf("%s of %T = %q, want it to contain %q", verb, v, out, keyMarker)
			}
			clean(fmt.Sprintf("%s of %T", verb, v), out)
		}
	}
	for _, v := range []any{k, []Key{*k}, map[string]*Key{"k": k}, &keyHolder{K: *k}} {
		if out := fmt.Sprintf("%p", v); !strings.HasPrefix(out, "0x") || strings.ContainsAny(out, "{[ ") {
			t.Errorf("%%p of %T = %q, want an address", v, out)
		}
	}
	for _, v := range []any{*k, keyHolder{K: *k}} {
		clean(fmt.Sprintf("%%p of %T", v), fmt.Sprintf("%p", v))
	}
	var buf bytes.Buffer
	for _, logger := range []*slog.Logger{slog.New(slog.NewJSONHandler(&buf, nil)), slog.New(slog.NewTextHandler(&buf, nil))} {
		buf.Reset()
		logger.Info("key", slog.Any("k", k), slog.Any("v", *k))
		out := buf.String()
		if strings.Count(out, keyMarker) != 2 {
			t.Errorf("slog output %q lacks the redaction markers", out)
		}
		clean("slog output", out)
	}
	if k.String() != keyMarker || k.LogValue().String() != keyMarker {
		t.Error("String or LogValue does not return the redaction marker")
	}
}
