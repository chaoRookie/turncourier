// Package keychain 通过 /usr/bin/security 读写 macOS 登录钥匙串中的通用密码条目。
// 机密只经标准输入写入、经标准输出读出，从不出现在子进程参数、环境变量、错误文本或日志中。
// 经 security 创建的条目，受信任应用是 /usr/bin/security，同一用户的任何进程都能读出（见 design.md 的威胁模型）。
package keychain

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Service 是 TurnCourier 全部条目共用的 service 名。
const Service = "io.github.chaorookie.turncourier"

var (
	// ErrNotFound 表示条目不存在（security 退出码 44，即 errSecItemNotFound 的低 8 位）。
	ErrNotFound = errors.New("keychain item not found")
	// ErrUnsupported 表示当前平台没有受支持的凭据存储；不提供明文文件后备。
	ErrUnsupported = errors.New("keychain is only supported on macOS")
	// ErrInteractionNotAllowed 表示钥匙串已锁定且当前环境不允许弹窗（security 退出码 36）。
	ErrInteractionNotAllowed = errors.New("keychain is locked and user interaction is not allowed")
	// ErrInvalidName 表示 account 不是 1–128 个 [A-Za-z0-9._@:+-] 字符。
	ErrInvalidName = errors.New("invalid keychain account name")
	// ErrInvalidSecret 表示机密不是 1–1000 个不含空白的可打印 ASCII 字符，或密钥文本不是 43 个 base64url 字符。
	ErrInvalidSecret = errors.New("invalid keychain secret")
	// ErrReadBackMismatch 表示写入后读回的内容不一致或读不到。
	ErrReadBackMismatch = errors.New("keychain read-back mismatch")
	// ErrExists 表示 Add 的目标条目已存在；已有条目不会被改动。
	ErrExists = errors.New("keychain item already exists")
)

const (
	// DefaultTimeout 是后台读取使用的每次调用期限。
	DefaultTimeout = 10 * time.Second
	// InteractiveTimeout 是 init 使用的期限，给用户留出在解锁或授权弹窗中输入登录密码的时间。
	InteractiveTimeout = 60 * time.Second
)

const (
	// securityPath 是系统自带的 security 工具，按绝对路径启动，不经 PATH 查找。
	securityPath = "/usr/bin/security"
	// maxOutput 是 Get 接受的标准输出字节数上限；多读 1 字节用来发现超长输出。
	maxOutput = 4096
	// exitItemNotFound 与 exitInteractionNotAllowed 是 security 的退出码，即对应 OSStatus 的低 8 位。
	exitItemNotFound          = 44
	exitInteractionNotAllowed = 36
	// keyCheckPrefix 是 KeyCheck 的域分隔前缀，与令牌 MAC、正文键控摘要的前缀互不为前缀。
	keyCheckPrefix = "turncourier/key-check/v1\x00"
)

// Store 是系统凭据存储的最小接口；使用方依赖它，测试注入内存替身。
type Store interface {
	Get(ctx context.Context, account string) (string, error)
	Set(ctx context.Context, account, secret string) error
	Add(ctx context.Context, account, secret string) error
	Delete(ctx context.Context, account string) error
}

// Security 是基于 /usr/bin/security 的 Store 实现，可并发使用。
type Security struct {
	path    string        // 生产固定为 /usr/bin/security，不经 PATH 查找；测试在包内替换为测试二进制
	timeout time.Duration // 每次调用的期限；调用方的 ctx 更早到期时以 ctx 为准
}

// New 返回生产实现，timeout 为 0 时使用 DefaultTimeout；非 darwin 平台返回 ErrUnsupported。
func New(timeout time.Duration) (*Security, error) {
	return newFor(runtime.GOOS, timeout)
}

// newFor 按给定平台构造实现，使平台判断在任一平台上都能测试。
func newFor(goos string, timeout time.Duration) (*Security, error) {
	if goos != "darwin" {
		return nil, ErrUnsupported
	}
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	return &Security{path: securityPath, timeout: timeout}, nil
}

// Get 执行 find-generic-password -s Service -a account -w，只读标准输出（至多 4096 字节），
// 去掉恰好一个结尾换行后校验与 Set 相同的格式；标准错误丢弃，不进入错误文本。
func (s *Security) Get(ctx context.Context, account string) (string, error) {
	if !validAccount(account) {
		return "", ErrInvalidName
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return s.get(ctx, "get", account)
}

// Set 启动 security -i，经标准输入只发一行 add-generic-password -U -s Service -a account -X <小写十六进制>，
// 退出码为 0 后用 Get 读回核对。security -i 的退出码只反映最后一条命令，因此每个进程只发一条命令。
// 不使用 -w 取值、-p、-v、-A 或 -T；机密与十六进制文本都不出现在参数列表中。
// 用于替换授权码；密钥条目一律用 Add 创建，从不经 Set 覆盖。
func (s *Security) Set(ctx context.Context, account, secret string) error {
	if err := validate(account, secret); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if _, err := s.run(ctx, "set", addCommand(true, account, secret), "-i"); err != nil {
		return err
	}
	return s.readBack(ctx, "set", account, secret)
}

// Add 只创建新条目：先 Get，已存在返回 ErrExists；不存在时经 security -i 发送不带 -U 的
// add-generic-password -s Service -a account -X <小写十六进制>。退出码非 0 时再 Get 一次，条目已存在
// （并发的另一个写入者先写入）返回 ErrExists，否则返回原错误；退出码为 0 后读回核对。不依赖「条目已存在」的具体退出码。
func (s *Security) Add(ctx context.Context, account, secret string) error {
	if err := validate(account, secret); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if _, err := s.get(ctx, "add", account); err == nil {
		return ErrExists
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	if _, err := s.run(ctx, "add", addCommand(false, account, secret), "-i"); err != nil {
		if _, getErr := s.get(ctx, "add", account); getErr == nil {
			return ErrExists
		}
		return err
	}
	return s.readBack(ctx, "add", account, secret)
}

// Delete 执行 delete-generic-password -s Service -a account 并丢弃输出；条目不存在时返回 ErrNotFound。
func (s *Security) Delete(ctx context.Context, account string) error {
	if !validAccount(account) {
		return ErrInvalidName
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	_, err := s.run(ctx, "delete", "", "delete-generic-password", "-s", Service, "-a", account)
	return err
}

// get 在调用方已设定的期限内读取条目；op 只用于错误文本，使 Set 与 Add 中的读取错误标明所属操作。
// 格式不符的输出只报告 ErrInvalidSecret，不回显读到的内容。
func (s *Security) get(ctx context.Context, op, account string) (string, error) {
	output, err := s.run(ctx, op, "", "find-generic-password", "-s", Service, "-a", account, "-w")
	if err != nil {
		return "", err
	}
	secret, found := strings.CutSuffix(string(output), "\n")
	if !found || !validSecret(secret) {
		return "", fmt.Errorf("keychain %s read an %w", op, ErrInvalidSecret)
	}
	return secret, nil
}

// readBack 在同一期限内读回刚写入的条目：读不到或内容不同都报告 ErrReadBackMismatch；
// 期限到达或已取消时返回对应的期限错误，便于调用方区分超时与内容不符。
func (s *Security) readBack(ctx context.Context, op, account, secret string) error {
	got, err := s.get(ctx, op, account)
	if err != nil && ctx.Err() != nil {
		return err
	}
	if err != nil || got != secret {
		return ErrReadBackMismatch
	}
	return nil
}

// run 按固定路径启动一次 security：不经 shell，继承父进程环境变量，标准错误接空设备，input 非空时作为标准输入。
// 返回至多 maxOutput+1 字节的标准输出；ctx 到期时 exec 结束子进程，Wait 返回前子进程已被回收。
func (s *Security) run(ctx context.Context, op, input string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, s.path, args...)
	if input != "" {
		command.Stdin = strings.NewReader(input)
	}
	stdout, err := command.StdoutPipe()
	if err == nil {
		err = command.Start()
	}
	var output []byte
	if err == nil {
		var readErr error
		output, readErr = io.ReadAll(io.LimitReader(stdout, maxOutput+1))
		if err = command.Wait(); err == nil {
			err = readErr
		}
	}
	return output, classify(ctx, op, err)
}

// classify 把一次调用的结果归类为包级错误。期限与取消先于退出码判断，因为被结束的子进程同样以非 0 退出；
// 其他失败只报告操作名与退出码，不含 security 的输出与可执行文件路径。
func classify(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return fmt.Errorf("keychain %s timed out: %w", op, ctxErr)
		}
		return fmt.Errorf("keychain %s canceled: %w", op, ctxErr)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return fmt.Errorf("keychain %s could not run the security tool", op)
	}
	switch code := exitErr.ExitCode(); code {
	case exitItemNotFound:
		return ErrNotFound
	case exitInteractionNotAllowed:
		return ErrInteractionNotAllowed
	default:
		return fmt.Errorf("keychain %s failed with exit code %d", op, code)
	}
}

// addCommand 渲染 security -i 读入的一行写入命令；update 为 true 时带 -U（已存在则更新）。
// account 已通过名称校验，只含无需引号的字符；机密以小写十六进制经 -X 传入。
func addCommand(update bool, account, secret string) string {
	flag := ""
	if update {
		flag = " -U"
	}
	return "add-generic-password" + flag + " -s " + Service + " -a " + account + " -X " + hex.EncodeToString([]byte(secret)) + "\n"
}

// validate 在启动任何进程之前检查 account 与机密；错误不回显输入。
func validate(account, secret string) error {
	if !validAccount(account) {
		return ErrInvalidName
	}
	if !validSecret(secret) {
		return ErrInvalidSecret
	}
	return nil
}

// validAccount 检查 account 为 1–128 个 [A-Za-z0-9._@:+-] 字符；这些字符在 security -i 的命令行中无需引号。
func validAccount(account string) bool {
	if len(account) == 0 || len(account) > 128 {
		return false
	}
	for i := range len(account) {
		c := account[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("._@:+-", c) >= 0) {
			return false
		}
	}
	return true
}

// validSecret 检查机密为 1–1000 个不含空白的可打印 ASCII 字符（0x21–0x7e）。
func validSecret(secret string) bool {
	if len(secret) == 0 || len(secret) > 1000 {
		return false
	}
	for i := range len(secret) {
		if secret[i] < 0x21 || secret[i] > 0x7e {
			return false
		}
	}
	return true
}

// AuthCodeAccount 返回 QQ 授权码条目的 account：<instanceID>:qq-auth-code。
func AuthCodeAccount(instanceID string) string {
	return instanceID + ":qq-auth-code"
}

// TokenKeyAccount 返回令牌签名密钥条目的 account：<instanceID>:token-key-<kid>；kid 为 0 属于编程错误，函数 panic。
func TokenKeyAccount(instanceID string, kid uint8) string {
	return keyAccount(instanceID, "token", kid)
}

// PayloadKeyAccount 返回正文加密密钥条目的 account：<instanceID>:payload-key-<kid>；kid 为 0 时 panic。
func PayloadKeyAccount(instanceID string, kid uint8) string {
	return keyAccount(instanceID, "payload", kid)
}

// keyAccount 拼接 <instanceID>:<purpose>-key-<kid>，kid 为十进制 1–255。
func keyAccount(instanceID, purpose string, kid uint8) string {
	if kid == 0 {
		panic("keychain: key id 0 is reserved")
	}
	return instanceID + ":" + purpose + "-key-" + strconv.Itoa(int(kid))
}

// NewKeyText 从 random 读取 32 字节，返回 base64.RawURLEncoding 文本（43 个字符），供写入 Keychain。
func NewKeyText(random io.Reader) (string, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(random, key); err != nil {
		return "", fmt.Errorf("cannot generate key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(key), nil
}

// DecodeKeyText 把 Keychain 中的密钥文本还原为 32 字节；长度或编码不符时返回 ErrInvalidSecret，错误不回显输入。
// 严格模式拒绝末字符含非零填充比特的文本，同一把密钥只有一种写法；解码器忽略的换行会使解码结果不足 32 字节而被拒绝。
func DecodeKeyText(text string) ([]byte, error) {
	if len(text) != 43 {
		return nil, ErrInvalidSecret
	}
	key, err := base64.RawURLEncoding.Strict().DecodeString(text)
	if err != nil || len(key) != 32 {
		return nil, ErrInvalidSecret
	}
	return key, nil
}

// KeyCheck 返回 HMAC-SHA256(key, "turncourier/key-check/v1\x00" ‖ purpose ‖ kid) 的前 8 字节，
// 登记在 crypto_keys.key_check 中，用于发现 Keychain 中的密钥被替换；purpose 须为 token 或 payload，kid 为 0 时 panic。
func KeyCheck(purpose string, kid uint8, key []byte) [8]byte {
	if purpose != "token" && purpose != "payload" || kid == 0 {
		panic("keychain: invalid key check purpose or key id")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(keyCheckPrefix + purpose))
	mac.Write([]byte{kid})
	var check [8]byte
	copy(check[:], mac.Sum(nil))
	return check
}
