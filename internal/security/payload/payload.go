// Package payload 用 AES-256-GCM 加密待处理的回复正文与待发通知内容。
// 关联数据把密文绑定到用途、密钥号、任务与序号，密文被调换到别的行时无法解密。
package payload

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Kind 是密文的用途，写入关联数据。
type Kind byte

const (
	// KindReply 是回复正文，序号为 replies.seq。
	KindReply Kind = 'r'
	// KindNotification 是通知内容，序号为 notifications.id。
	KindNotification Kind = 'n'
)

const (
	// FormatV1 是密文首字节，标识当前格式。
	FormatV1 byte = 1
	// KeyLen 是密钥字节数。
	KeyLen = 32
	// Overhead 是密文比明文多出的字节数：格式 1 + nonce 12 + 标签 16。
	Overhead = 29
	// MaxPlaintext 是明文上限。
	MaxPlaintext = 1 << 20
)

const (
	// alphabet 是小写 Crockford base32 字母表，与任务 ID 相同；本包只用标准库，不导入 task 包。
	alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	// adPrefix 是关联数据的域分隔前缀。
	adPrefix = "turncourier/payload/v1\x00"
	// redactedKey 是密钥在一切格式化输出中的替代文本。
	redactedKey = "[redacted payload key]"
)

// Key 是一把正文加密密钥；String、Format 与 LogValue 一律输出 [redacted payload key]。
type Key struct {
	id   uint8
	aead cipher.AEAD // cipher.NewGCMWithRandomNonce 的结果
}

var (
	// ErrInvalidKey 表示密钥号为 0 或密钥长度不是 32 字节。
	ErrInvalidKey = errors.New("invalid payload key")
	// ErrInvalidInput 表示用途、任务 ID、序号或明文长度不合法。
	ErrInvalidInput = errors.New("invalid payload binding or size")
	// ErrDecrypt 表示密文无法用给定密钥与绑定解密；不区分原因，不含明文或密文。
	ErrDecrypt = errors.New("payload cannot be decrypted")
)

// NewKey 由 32 字节密钥构造 AES-256-GCM（随机 96 位 nonce）；id 须为 1–255。
// aes.NewCipher 也接受 16 与 24 字节密钥，因此另行检查长度。
func NewKey(id uint8, secret []byte) (*Key, error) {
	var aead cipher.AEAD
	block, err := aes.NewCipher(secret)
	if err == nil {
		aead, err = cipher.NewGCMWithRandomNonce(block)
	}
	// 对 32 字节密钥两个构造函数都不会失败，err 只作兜底，与长度检查合并以免留下无法执行的分支。
	if id == 0 || len(secret) != KeyLen || err != nil {
		return nil, ErrInvalidKey
	}
	return &Key{id: id, aead: aead}, nil
}

// ID 返回密钥号，存储时写入 key_id 列。
func (k *Key) ID() uint8 {
	return k.id
}

// Seal 返回 FormatV1 ‖ nonce ‖ 密文 ‖ 标签；taskID 须为 10 位小写 Crockford base32，seq ≥ 1，明文 1 字节到 MaxPlaintext。
func (k *Key) Seal(kind Kind, taskID string, seq int64, plaintext []byte) ([]byte, error) {
	ad, err := k.associatedData(kind, taskID, seq)
	if err != nil {
		return nil, err
	}
	if len(plaintext) == 0 || len(plaintext) > MaxPlaintext {
		return nil, ErrInvalidInput
	}
	// 以只含格式字节的切片作 dst，aead.Seal 在其后追加 nonce ‖ 密文 ‖ 标签，省去一次整段复制。
	sealed := make([]byte, 1, Overhead+len(plaintext))
	sealed[0] = FormatV1
	return k.aead.Seal(sealed, nil, plaintext, ad), nil
}

// Open 校验长度（Overhead+1 到 MaxPlaintext+Overhead，明文至少 1 字节）、格式字节与绑定后解密；任何失败都返回 ErrDecrypt。
// 用途、任务 ID 或序号本身不合法时与 Seal 一致，先返回 ErrInvalidInput。
func (k *Key) Open(kind Kind, taskID string, seq int64, sealed []byte) ([]byte, error) {
	ad, err := k.associatedData(kind, taskID, seq)
	if err != nil {
		return nil, err
	}
	if len(sealed) <= Overhead || len(sealed) > MaxPlaintext+Overhead || sealed[0] != FormatV1 {
		return nil, ErrDecrypt
	}
	plaintext, err := k.aead.Open(nil, nil, sealed[1:], ad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// String 返回脱敏文本；格式化方法用值接收者，密钥按值或按指针格式化时都会被脱敏。
// Key 不保存密钥原文，%p 作用于密钥值时 fmt 绕过 Format，也只打印内部 AEAD 的地址。
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

// associatedData 校验用途、任务 ID 与序号，返回关联数据：adPrefix ‖ 用途 ‖ kid ‖ 任务 ID（10 字节）‖ 序号（uint64 大端）。
// nonce 随机生成而不由序号派生：新建数据库后序号从 1 重新计数，Keychain 中的密钥却可能仍在。
func (k *Key) associatedData(kind Kind, taskID string, seq int64) ([]byte, error) {
	if (kind != KindReply && kind != KindNotification) || len(taskID) != 10 || seq < 1 {
		return nil, ErrInvalidInput
	}
	for i := range len(taskID) {
		if strings.IndexByte(alphabet, taskID[i]) < 0 {
			return nil, ErrInvalidInput
		}
	}
	ad := make([]byte, 0, len(adPrefix)+2+len(taskID)+8)
	ad = append(ad, adPrefix...)
	ad = append(ad, byte(kind), k.id)
	ad = append(ad, taskID...)
	return binary.BigEndian.AppendUint64(ad, uint64(seq)), nil
}
