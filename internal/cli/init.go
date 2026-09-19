// Package cli 的 init 命令交互式创建配置，并把 QQ 授权码与两把密钥写入 macOS Keychain；
// 不联网、不覆盖已有的配置与密钥，未经明确确认不替换授权码，错误文本不含路径与机密。
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chaoRookie/turncourier/internal/config"
	"github.com/chaoRookie/turncourier/internal/security/keychain"
	"github.com/chaoRookie/turncourier/internal/store/sqlite"
)

// InitDeps 是 init 的外部依赖。
type InitDeps struct {
	Terminal      Terminal
	Keychain      func() (keychain.Store, error) // 生产为 keychain.New(keychain.InteractiveTimeout)；非 macOS 返回 keychain.ErrUnsupported
	Getenv        func(string) string
	UserConfigDir func() (string, error)
	Random        io.Reader // 实例 ID 与密钥的随机源，生产为 crypto/rand.Reader
	Now           func() time.Time
}

// Terminal 是 init 的交互终端；ctx 结束时恢复终端回显并返回 ctx.Err()。
type Terminal interface {
	Interactive() bool                                             // 标准输入与标准输出都是终端
	ReadLine(ctx context.Context, prompt string) (string, error)   // 回显读入一行，至多 1024 字节，去掉结尾换行
	ReadSecret(ctx context.Context, prompt string) (string, error) // 关闭回显读入一行，标志与 term.ReadPassword 相同
}

// maxAttempts 是同一项输入连续无效的次数上限，达到即退出。
const maxAttempts = 3

// maxAuthCodeLen 是授权码的长度上限。
const maxAuthCodeLen = 128

// init 打印的固定文案；两段说明在摘要之后原样输出。
const (
	threatNote = "注意：授权码与密钥保存在登录钥匙串中，同一用户下的任何进程（包括 Agent 执行的命令）都能读取；读到它们的进程可以伪造通过全部校验的回复、把任意内容注入任一任务，也可以直接改写本地队列。TurnCourier 不防同一用户下的进程。请只使用专用的机器人邮箱；怀疑泄露时，请在 QQ 邮箱中停用该授权码。"
	backupNote = "待投递的回复与通知会加密暂存在数据目录中，处理完成后删除；APFS 快照与 Time Machine 备份中可能留有已删除数据的密文副本，正文密钥仍在 Keychain 中时它们可以被解密。不要把数据目录恢复到旧的备份，否则已发出的通知可能重发、已确认的回复可能再次派发。当前版本尚不能收发邮件。"
	notYesNo   = "输入不是 y 或 N，授权码未替换；如果刚才粘贴的是授权码，请清屏，并考虑在 QQ 邮箱中停用它。"
)

// init 的失败原因；每个都是打印给用户的一行中文。
var (
	errTooManyAttempts = errors.New("同一项连续 3 次输入无效，init 已退出。")
	errKeyMissing      = errors.New("密钥元数据存在，但 Keychain 中缺少对应条目；init 不会重新生成，否则待处理数据将无法解密。")
	//lint:ignore ST1005 这是原样打印给用户的完整中文句子，文案由实施清单规定，以产品名 Keychain 开头
	errKeyMismatch = errors.New("Keychain 中的密钥与数据库登记的不符；init 不会覆盖它。")
	//lint:ignore ST1005 同上，文案由实施清单规定
	errKeyMalformed = errors.New("Keychain 中已有格式不符的同名条目；请按 development.md 手工删除后重新运行。")
)

// runInit 执行 init 并返回退出码：成功时把摘要写到 stdout；失败时向 stderr 打印一行中文原因并返回 1，
// ctx 已结束（Ctrl-C）时打印「已取消。」。
func runInit(ctx context.Context, stdout, stderr io.Writer, deps InitDeps) int {
	summary, err := initialize(ctx, stderr, deps)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(stderr, "已取消。")
		} else {
			fmt.Fprintln(stderr, err)
		}
		return 1
	}
	_, err = io.WriteString(stdout, summary)
	return outputStatus(err, stderr)
}

// initialize 按顺序执行 init 的各步并返回摘要文本。配置文件与 Keychain 只在全部输入读完之后写入：
// 输入阶段失败或取消时，至多留下数据目录与数据库中的实例行，重新运行会继续使用它们。
// 返回的错误是给用户看的原因；其中包含的底层错误按 config、store、keychain 的约定不含路径与机密。
func initialize(ctx context.Context, stderr io.Writer, deps InitDeps) (string, error) {
	kc, err := deps.Keychain()
	if errors.Is(err, keychain.ErrUnsupported) {
		return "", errors.New("init 只支持 macOS：授权码与密钥只能保存在 macOS Keychain。")
	}
	if err != nil {
		return "", fmt.Errorf("无法使用 Keychain：%w", err)
	}
	if !deps.Terminal.Interactive() {
		return "", errors.New("init 需要在交互式终端中运行；请直接在终端执行 turncourier init。")
	}
	paths, err := config.ResolvePaths(deps.Getenv, deps.UserConfigDir)
	if err != nil {
		return "", fmt.Errorf("无法确定配置文件位置：%w", err)
	}

	// rendered 非 nil 表示配置文件尚不存在，需要在输入全部读完后创建。
	var rendered []byte
	_, err = config.Load(paths, deps.Getenv)
	switch {
	case errors.Is(err, config.ErrNotFound):
		// Load 跟随符号链接，CreateFile 中的 MkdirAll 与 os.Link 不跟随：配置文件或它的上级目录是悬空的符号链接时
		// CreateFile 永远无法创建，在询问之前退出，免得用户每次重新运行都要再输入一遍授权码。
		if danglingLink(paths.ConfigFile) {
			return "", errors.New("配置文件或它的上级目录是悬空的符号链接；init 不会修改它，请检查后重新运行 turncourier init。")
		}
		if rendered, err = askConfig(ctx, stderr, deps.Terminal); err != nil {
			return "", err
		}
	case err != nil:
		return "", fmt.Errorf("现有配置文件无效，init 不会修改它：%w", err)
	}

	db, err := sqlite.Open(ctx, paths.DataDir, sqlite.Options{Now: deps.Now, Random: deps.Random})
	if err != nil {
		return "", fmt.Errorf("无法打开数据库：%w", err)
	}
	defer db.Close()
	id, _, err := db.EnsureInstance(ctx)
	if err != nil {
		return "", fmt.Errorf("无法创建实例 ID：%w", err)
	}
	authCode, authStatus, err := askAuthCode(ctx, stderr, deps.Terminal, kc, keychain.AuthCodeAccount(id))
	if err != nil {
		return "", err
	}

	configStatus := "已存在"
	if rendered != nil {
		if err := config.CreateFile(paths.ConfigFile, rendered); errors.Is(err, fs.ErrExist) {
			return "", errors.New("配置文件刚被其他进程创建；init 没有覆盖它，请重新运行 turncourier init。")
		} else if err != nil {
			return "", fmt.Errorf("无法创建配置文件：%w", err)
		}
		if _, err := config.Load(paths, deps.Getenv); err != nil {
			return "", fmt.Errorf("新建的配置文件无法加载：%w", err)
		}
		configStatus = "已创建"
	}
	if authCode != "" {
		if err := kc.Set(ctx, keychain.AuthCodeAccount(id), authCode); err != nil {
			return "", fmt.Errorf("无法把授权码写入 Keychain：%w", err)
		}
	}
	tokenStatus, err := ensureKey(ctx, db, kc, deps.Random, id, sqlite.KeyPurposeToken)
	if err != nil {
		return "", err
	}
	payloadStatus, err := ensureKey(ctx, db, kc, deps.Random, id, sqlite.KeyPurposePayload)
	if err != nil {
		return "", err
	}

	configPlace, dataPlace := "用户配置目录下的 TurnCourier/turncourier.toml", "配置文件所在的目录"
	if deps.Getenv("TURNCOURIER_CONFIG") != "" {
		configPlace = "TURNCOURIER_CONFIG 指定的文件"
	}
	if deps.Getenv("TURNCOURIER_DATA_DIR") != "" {
		dataPlace = "TURNCOURIER_DATA_DIR 指定的目录"
	}
	return fmt.Sprintf("配置文件：%s（%s）\n数据目录：%s\n实例 ID：%s\n授权码：%s\n令牌签名密钥：%s\n正文加密密钥：%s\n\n%s\n\n%s\n",
		configStatus, configPlace, dataPlace, id, authStatus, tokenStatus, payloadStatus, threatNote, backupNote), nil
}

// danglingLink 报告 path 或它最近的已存在的上级是否为悬空的符号链接：自 path 起逐级向上，找到第一个 Lstat 成功的路径，
// 它的 Stat 返回不存在即为悬空。Lstat 成功说明更上级都能解析，不必继续向上。
func danglingLink(path string) bool {
	for {
		if _, err := os.Lstat(path); err == nil {
			_, err = os.Stat(path)
			return errors.Is(err, fs.ErrNotExist)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return false
		}
		path = parent
	}
}

// askConfig 依次询问机器人地址、接收地址与白名单，逐项按 NormalizeAddress 与 Render 的规则校验，
// 返回只在内存中渲染的配置文本。
func askConfig(ctx context.Context, stderr io.Writer, terminal Terminal) ([]byte, error) {
	var draft config.Draft
	err := ask(ctx, stderr, terminal, "机器人邮箱地址（专用 QQ 邮箱，用于发送通知）：", func(line string) (err error) {
		draft.MailboxAddress, err = config.NormalizeAddress(line)
		return err
	})
	if err != nil {
		return nil, err
	}
	err = ask(ctx, stderr, terminal, "接收通知的邮箱地址：", func(line string) (err error) {
		draft.RecipientAddress, err = config.NormalizeAddress(line)
		return err
	})
	if err != nil {
		return nil, err
	}
	var rendered []byte
	err = ask(ctx, stderr, terminal, "允许回复的发件人（逗号分隔，直接回车表示只允许接收通知的地址）：", func(line string) error {
		draft.AllowedSenders = []string{draft.RecipientAddress}
		if strings.TrimSpace(line) != "" {
			draft.AllowedSenders = nil
			for _, part := range strings.Split(line, ",") {
				address, err := config.NormalizeAddress(part)
				if err != nil {
					return err
				}
				draft.AllowedSenders = append(draft.AllowedSenders, address)
			}
		}
		var err error
		rendered, err = config.Render(draft)
		return err
	})
	return rendered, err
}

// ask 以 prompt 读入一行交给 accept；accept 报错时打印原因并重问，同一项连续 maxAttempts 次无效返回 errTooManyAttempts。
func ask(ctx context.Context, stderr io.Writer, terminal Terminal, prompt string, accept func(string) error) error {
	for range maxAttempts {
		line, err := terminal.ReadLine(ctx, prompt)
		if err != nil {
			return fmt.Errorf("无法读取输入：%w", err)
		}
		if err := accept(line); err != nil {
			fmt.Fprintf(stderr, "输入无效：%v\n", err)
			continue
		}
		return nil
	}
	return errTooManyAttempts
}

// askAuthCode 决定是否录入授权码：Keychain 中没有时直接录入；已有时（含内容格式不符）只有回答 y 或 Y 才重新录入。
// 返回待写入的授权码（不需要写入时为空）与摘要中的状态。回答经 ReadLine 回显读入，超过 1 个字符时按 N 处理并提示，不回显回答。
func askAuthCode(ctx context.Context, stderr io.Writer, terminal Terminal, kc keychain.Store, account string) (string, string, error) {
	status := "已保存"
	_, err := kc.Get(ctx, account)
	switch {
	case errors.Is(err, keychain.ErrNotFound):
	case err == nil || errors.Is(err, keychain.ErrInvalidSecret):
		answer, err := terminal.ReadLine(ctx, "Keychain 中已有授权码，是否替换？[y/N]：")
		if err != nil {
			return "", "", fmt.Errorf("无法读取输入：%w", err)
		}
		if answer != "y" && answer != "Y" {
			if utf8.RuneCountInString(answer) > 1 {
				fmt.Fprintln(stderr, notYesNo)
			}
			return "", "保持不变", nil
		}
		status = "已替换"
	default:
		return "", "", fmt.Errorf("无法读取 Keychain 中的授权码：%w", err)
	}
	code, err := readAuthCode(ctx, stderr, terminal)
	return code, status, err
}

// readAuthCode 两次不回显读入授权码，要求两次相同且为 1–128 个不含空白的可打印 ASCII 字符；不符时重问，
// 连续 maxAttempts 次不符返回 errTooManyAttempts。授权码只留在返回值中，提示与错误都不回显它。
func readAuthCode(ctx context.Context, stderr io.Writer, terminal Terminal) (string, error) {
	for range maxAttempts {
		first, err := terminal.ReadSecret(ctx, "QQ 邮箱授权码（输入时不显示）：")
		if err != nil {
			return "", fmt.Errorf("无法读取输入：%w", err)
		}
		second, err := terminal.ReadSecret(ctx, "再次输入授权码：")
		if err != nil {
			return "", fmt.Errorf("无法读取输入：%w", err)
		}
		switch {
		case first != second:
			fmt.Fprintln(stderr, "两次输入不一致，请重新输入。")
		case !validAuthCode(first):
			fmt.Fprintln(stderr, "授权码须为 1–128 个不含空白的可打印 ASCII 字符，请重新输入。")
		default:
			return first, nil
		}
	}
	return "", errTooManyAttempts
}

// validAuthCode 报告 code 是否为 1–128 个可打印 ASCII 字符（0x21–0x7e，不含空白）。
func validAuthCode(code string) bool {
	if len(code) == 0 || len(code) > maxAuthCodeLen {
		return false
	}
	for i := range len(code) {
		if code[i] < 0x21 || code[i] > 0x7e {
			return false
		}
	}
	return true
}

// ensureKey 确保 purpose 用途的密钥在 Keychain 中且已登记，返回摘要中的状态「已生成」或「已存在」。密钥条目从不覆盖：
// 已有元数据时只核对 Keychain 中的条目与登记的校验值；没有元数据时沿用或新建 kid 1 的条目，再登记它的校验值。
func ensureKey(ctx context.Context, db *sqlite.Store, kc keychain.Store, random io.Reader, id string, purpose sqlite.KeyPurpose) (string, error) {
	account := keychain.TokenKeyAccount
	if purpose == sqlite.KeyPurposePayload {
		account = keychain.PayloadKeyAccount
	}
	kid, err := db.ActiveKeyID(ctx, purpose)
	if err == nil {
		return "已存在", verifyKey(ctx, db, kc, account(id, kid), purpose, kid)
	}
	if !errors.Is(err, sqlite.ErrNotFound) {
		return "", fmt.Errorf("无法读取密钥元数据：%w", err)
	}
	key, created, err := findOrAddKey(ctx, kc, random, account(id, 1))
	if err != nil {
		return "", err
	}
	check := keychain.KeyCheck(string(purpose), 1, key)
	if err := db.RegisterKey(ctx, purpose, 1, check); errors.Is(err, sqlite.ErrKeyExists) {
		// 并发的另一个 init 先登记了：登记的是同一把密钥即视为成功。
		registered, err := db.KeyCheckOf(ctx, purpose, 1)
		if err != nil {
			return "", fmt.Errorf("无法读取密钥元数据：%w", err)
		}
		if registered != check {
			return "", errKeyMismatch
		}
	} else if err != nil {
		return "", fmt.Errorf("无法登记密钥：%w", err)
	}
	if created {
		return "已生成", nil
	}
	return "已存在", nil
}

// verifyKey 核对已登记的密钥：Keychain 中缺少条目返回 errKeyMissing；条目格式不符或校验值与登记的不同返回 errKeyMismatch。
func verifyKey(ctx context.Context, db *sqlite.Store, kc keychain.Store, account string, purpose sqlite.KeyPurpose, kid uint8) error {
	text, err := kc.Get(ctx, account)
	switch {
	case errors.Is(err, keychain.ErrNotFound):
		return errKeyMissing
	case errors.Is(err, keychain.ErrInvalidSecret):
		return errKeyMismatch
	case err != nil:
		return fmt.Errorf("无法读取 Keychain 中的密钥：%w", err)
	}
	key, err := keychain.DecodeKeyText(text)
	if err != nil {
		return errKeyMismatch
	}
	registered, err := db.KeyCheckOf(ctx, purpose, kid)
	if err != nil {
		return fmt.Errorf("无法读取密钥元数据：%w", err)
	}
	if keychain.KeyCheck(string(purpose), kid, key) != registered {
		return errKeyMismatch
	}
	return nil
}

// findOrAddKey 返回 account 条目中的密钥与它是否由本次生成：条目已存在且格式合法时沿用（上次在登记前中断，
// 或另一个 init 刚写入，该条目从未被使用）；不存在时生成新密钥并以 Add 创建，Add 报告已存在（并发的另一个 init 先写入）时改为读取并沿用。
// 已存在但格式不符时返回 errKeyMalformed，不覆盖它。
func findOrAddKey(ctx context.Context, kc keychain.Store, random io.Reader, account string) ([]byte, bool, error) {
	created := false
	text, err := kc.Get(ctx, account)
	if errors.Is(err, keychain.ErrNotFound) {
		if text, err = keychain.NewKeyText(random); err != nil {
			return nil, false, fmt.Errorf("无法生成密钥：%w", err)
		}
		err = kc.Add(ctx, account, text)
		created = err == nil
		if errors.Is(err, keychain.ErrExists) {
			text, err = kc.Get(ctx, account)
		}
	}
	if errors.Is(err, keychain.ErrInvalidSecret) {
		return nil, false, errKeyMalformed
	}
	if err != nil {
		return nil, false, fmt.Errorf("无法在 Keychain 中读取或创建密钥：%w", err)
	}
	key, err := keychain.DecodeKeyText(text)
	if err != nil {
		return nil, false, errKeyMalformed
	}
	return key, created, nil
}
