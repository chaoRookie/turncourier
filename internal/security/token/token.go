// Package token 签发与验证回复令牌 v1：HMAC-SHA256 截断到 128 位的持有者凭证。
// 令牌只携带版本、密钥号与随机通知 ID（nid）；任务、owner 与有效期由通知行和任务行提供并参与 MAC。
// 令牌在邮件中的载体是主题标签 [TC <任务 ID> <令牌>]（D4：主题标签是唯一的验证输入），本包定义它的类型与严格文法
// （subject.go）。本包不解析 MIME 与邮件头（主题由调用方解码后传入），不规定页脚副本的格式，也不包含线程匹配与发件人规则。
package token

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

const (
	// Version1 是当前唯一的令牌格式版本。
	Version1 byte = 1
	// TextLen 是令牌文本长度：30 字节编码为 48 个 Crockford base32 字符。
	TextLen = 48
	// KeyLen 是签名密钥的字节数。
	KeyLen = 32
)

const (
	// alphabet 是小写 Crockford base32 字母表，与任务 ID 相同，去掉了容易混淆的 i、l、o、u。
	alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	// rawLen 是令牌的原始字节数：版本 1、kid 1、nid 12、标签 16。
	rawLen = 30
	// macPrefix 与 digestPrefix 是令牌 MAC 与正文键控摘要的域分隔前缀，二者互不为前缀，同一把密钥的两种用途输入域不相交。
	macPrefix    = "turncourier/reply-token/v1\x00"
	digestPrefix = "turncourier/body-digest/v1\x00"
	// redactedToken 与 redactedKey 是令牌与密钥在一切格式化输出中的替代文本。
	redactedToken = "[redacted reply token]"
	redactedKey   = "[redacted token key]"
)

// encoding 是令牌文本的无填充 base32 编码；它会忽略换行，因此 Parse 在解码前逐字节检查字母表。
var encoding = base32.NewEncoding(alphabet).WithPadding(base32.NoPadding)

// NID 是通知 ID：12 字节随机数，与通知行一一对应；它不是机密，单独泄露不足以伪造令牌。
type NID [12]byte

// Claims 是令牌绑定、但不随令牌携带的字段，验证时从通知行与任务行读取。
type Claims struct {
	TaskID    string    // 10 位小写 Crockford base32
	Owner     string    // tasks.owner，1–255 字节
	ExpiresAt time.Time // 通知行的 token_expires_at，按 UTC Unix 毫秒参与 MAC，须晚于 1970-01-01
}

// Key 是一把签名密钥；String、Format 与 LogValue 一律输出脱敏文本。
type Key struct {
	id     uint8
	secret [KeyLen]byte
}

// Token 是签发或解析得到的令牌；String、Format 与 LogValue 一律输出脱敏文本，只有 Reveal 返回令牌文本。
type Token struct {
	kid uint8
	nid NID
	tag [16]byte
}

var (
	// ErrMalformed 表示令牌文本不符合 v1 格式；错误文本固定，不回显输入。
	ErrMalformed = errors.New("malformed reply token")
	// ErrInvalidKey 表示密钥号为 0 或密钥长度不是 32 字节。
	ErrInvalidKey = errors.New("invalid reply token key")
	// ErrInvalidClaims 表示任务 ID、owner 或有效期不合法（通常意味着数据损坏）。
	ErrInvalidClaims = errors.New("invalid reply token claims")
	// ErrKeyMismatch 表示令牌的密钥号与传入的密钥不同。
	ErrKeyMismatch = errors.New("reply token key id mismatch")
	// ErrBadMAC 表示标签校验失败。
	ErrBadMAC = errors.New("reply token signature mismatch")
	// ErrExpired 表示验证时刻不早于有效期。
	ErrExpired = errors.New("reply token expired")
)

// NewKey 复制 secret 构造密钥；id 须为 1–255，secret 须为 32 字节。
func NewKey(id uint8, secret []byte) (*Key, error) {
	if id == 0 || len(secret) != KeyLen {
		return nil, ErrInvalidKey
	}
	return &Key{id: id, secret: [KeyLen]byte(secret)}, nil
}

// ID 返回密钥号。
func (k *Key) ID() uint8 {
	return k.id
}

// NewNID 从 random 读取 12 字节。
func NewNID(random io.Reader) (NID, error) {
	var nid NID
	if _, err := io.ReadFull(random, nid[:]); err != nil {
		return NID{}, fmt.Errorf("cannot generate notification id: %w", err)
	}
	return nid, nil
}

// Issue 用 k 为 nid 与 c 计算标签；同样的输入总是得到同样的令牌，因此令牌无需落盘，重试发送得到同一令牌。
// 零值 Key 的密钥号为 0，签出的令牌无法解析，因此返回 ErrInvalidKey。
func Issue(k *Key, nid NID, c Claims) (Token, error) {
	if k.id == 0 {
		return Token{}, ErrInvalidKey
	}
	if !c.valid() {
		return Token{}, ErrInvalidClaims
	}
	return Token{kid: k.id, nid: nid, tag: k.tag(nid, c)}, nil
}

// Parse 按顺序检查：长度恰为 48 字节；每个字节属于字母表（大写字母先转小写，i、l、o、u 与其他字符一律拒绝）；
// 解码为 30 字节；版本字节为 0x01；kid 不为 0。任一步失败都返回 ErrMalformed。
func Parse(text string) (Token, error) {
	if len(text) != TextLen {
		return Token{}, ErrMalformed
	}
	var lower [TextLen]byte
	for i := range TextLen {
		c := text[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if strings.IndexByte(alphabet, c) < 0 {
			return Token{}, ErrMalformed
		}
		lower[i] = c
	}
	var raw [rawLen]byte
	if _, err := encoding.Decode(raw[:], lower[:]); err != nil || raw[0] != Version1 || raw[1] == 0 {
		return Token{}, ErrMalformed
	}
	return Token{kid: raw[1], nid: NID(raw[2:14]), tag: [16]byte(raw[14:])}, nil
}

// Verify 按顺序检查：t 的 kid 等于 k.ID()（ErrKeyMismatch）；c 合法（ErrInvalidClaims）；
// 用 hmac.Equal 比较标签（ErrBadMAC）；now 严格早于 c.ExpiresAt（ErrExpired）。
// 标签先于有效期检查，被篡改的过期令牌报 ErrBadMAC。任务状态与密钥状态由调用方另行检查。
func Verify(k *Key, t Token, c Claims, now time.Time) error {
	if t.kid != k.id {
		return ErrKeyMismatch
	}
	if !c.valid() {
		return ErrInvalidClaims
	}
	want := k.tag(t.nid, c)
	if !hmac.Equal(t.tag[:], want[:]) {
		return ErrBadMAC
	}
	if !now.Before(c.ExpiresAt) {
		return ErrExpired
	}
	return nil
}

// KeyID 返回令牌的密钥号。
func (t Token) KeyID() uint8 {
	return t.kid
}

// NID 返回令牌携带的通知 ID。
func (t Token) NID() NID {
	return t.nid
}

// Reveal 返回 48 个小写字符的令牌文本；只用于构造主题标签与页脚副本（Tag.Reveal、Tag.RevealToken）。
// 产品代码中引用它的位置由 tests/docs/reveal_test.go 限定在本包与 internal/mail/renderer。
func (t Token) Reveal() string {
	raw := make([]byte, 0, rawLen)
	raw = append(raw, Version1, t.kid)
	raw = append(raw, t.nid[:]...)
	raw = append(raw, t.tag[:]...)
	return encoding.EncodeToString(raw)
}

// String 返回脱敏文本。
func (t Token) String() string {
	return redactedToken
}

// Format 对 %T、%p、%w 之外的 fmt 动词输出脱敏文本，使 %x、%d、%#v 等动词也不会打印原始字段。
// fmt 在调用 Format 之前处理 %p：作用于令牌值（或含令牌的结构体值、数组值）时走错误动词路径，按原始字段打印，vet 也不报。
// %w 作用于任何含令牌的非 error 操作数（令牌值、指针，以及含令牌的切片、映射、结构体与它们的指针）时同样先于 Format
// 按错误动词处理，按原始字段打印；格式串为常量时 vet 的 printf 检查会拦下。
func (t Token) Format(f fmt.State, _ rune) {
	io.WriteString(f, redactedToken)
}

// LogValue 让 slog 记录脱敏文本。
func (t Token) LogValue() slog.Value {
	return slog.StringValue(redactedToken)
}

// BodyDigest 返回 HMAC-SHA256(k, "turncourier/body-digest/v1\x00" ‖ body)，用作 inbound_messages.body_sha256（D5 第 2 条）。
// 已知局限：契约规定的签名没有错误返回，直接构造的零值 Key（kid 0、密钥全零）在这里不会被拒绝，会算出以全零密钥求得的摘要，
// 而 Issue 对同一个零值 Key 返回 ErrInvalidKey。NewKey 出错时返回 nil，忽略它的调用方在这里 panic，不会得到坏摘要；
// 能算出坏摘要的只有直接构造零值 Key 的调用方。摘要的调用顺序把这种摘要挡在存储之外：密钥按通知行的 token_kid 取出，
// 令牌先经 Verify，零值 Key 只会得到 ErrKeyMismatch。TestBodyDigest 钉住这条边界。
func (k *Key) BodyDigest(body []byte) [32]byte {
	mac := hmac.New(sha256.New, k.secret[:])
	mac.Write([]byte(digestPrefix))
	mac.Write(body)
	return [32]byte(mac.Sum(nil))
}

// String 返回脱敏文本；格式化方法用值接收者，密钥按值或按指针格式化时都会被脱敏（%p 作用于密钥值除外，见 Token.Format）。
func (k Key) String() string {
	return redactedKey
}

// Format 对 %T、%p 之外的 fmt 动词输出脱敏文本。
func (k Key) Format(f fmt.State, _ rune) {
	io.WriteString(f, redactedKey)
}

// LogValue 让 slog 记录脱敏文本。
func (k Key) LogValue() slog.Value {
	return slog.StringValue(redactedKey)
}

// tag 计算 HMAC-SHA256 的前 16 字节，MAC 输入为 macPrefix ‖ 版本 ‖ kid ‖ nid ‖ 任务 ID ‖
// owner 字节数（uint16 大端）‖ owner ‖ 有效期（UTC Unix 毫秒，uint64 大端）；调用方须先确认 c 合法。
func (k *Key) tag(nid NID, c Claims) [16]byte {
	msg := make([]byte, 0, len(macPrefix)+2+len(nid)+len(c.TaskID)+2+len(c.Owner)+8)
	msg = append(msg, macPrefix...)
	msg = append(msg, Version1, k.id)
	msg = append(msg, nid[:]...)
	msg = append(msg, c.TaskID...)
	msg = binary.BigEndian.AppendUint16(msg, uint16(len(c.Owner)))
	msg = append(msg, c.Owner...)
	msg = binary.BigEndian.AppendUint64(msg, uint64(c.ExpiresAt.UnixMilli()))
	mac := hmac.New(sha256.New, k.secret[:])
	mac.Write(msg)
	return [16]byte(mac.Sum(nil))
}

// valid 检查任务 ID 为 10 个字母表字符、owner 为 1–255 字节、有效期晚于 Unix 纪元（毫秒值为正）。
func (c Claims) valid() bool {
	return validTaskID(c.TaskID) && len(c.Owner) > 0 && len(c.Owner) <= 255 && c.ExpiresAt.UnixMilli() > 0
}

// validTaskID 判断任务 ID 是否恰为 10 个字母表字符（小写，没有 i、l、o、u）；Claims 与 NewTag 共用这一条规则。
func validTaskID(id string) bool {
	if len(id) != 10 {
		return false
	}
	for i := range len(id) {
		if strings.IndexByte(alphabet, id[i]) < 0 {
			return false
		}
	}
	return true
}
