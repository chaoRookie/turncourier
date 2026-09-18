// Package config 的 Load 严格解码 TOML 配置，填充默认值、应用环境变量白名单，并通过 errors.Join 一次返回全部校验错误。
package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"
)

// Config 是经过默认值填充与校验的只读配置，不包含任何凭据。
type Config struct {
	Mailbox   Mailbox
	Recipient Recipient
	Notify    Notify
	Security  Security
	Paths     Paths
}

// Mailbox 描述机器人发件邮箱及其 IMAP、SMTP 服务器。
type Mailbox struct {
	Address  string
	IMAPHost string
	IMAPPort int
	SMTPHost string
	SMTPPort int
}

// Recipient 描述通知收件人与允许回复的发件人白名单（均已规范化）。
type Recipient struct {
	Address        string
	AllowedSenders []string
}

// Notify 列出会触发通知邮件的事件，顺序与去重后的配置一致。
type Notify struct {
	Events []string
}

// Security 保存与回复令牌相关的非敏感选项。
type Security struct {
	TokenTTL time.Duration
}

// ErrNotFound 表示配置文件不存在；调用方据此提示用户创建配置。
var ErrNotFound = errors.New("config file not found")

const (
	// maxConfigBytes 是配置文件的大小上界，超过即视为异常输入。
	maxConfigBytes = 1 << 20
	// notifyEventsEnv 是唯一允许覆盖配置的环境变量；安全相关项不接受环境变量覆盖。
	notifyEventsEnv = "TURNCOURIER_NOTIFY_EVENTS"
	// minTokenTTL 与 maxTokenTTL 是回复令牌有效期的上下界。
	minTokenTTL = time.Hour
	maxTokenTTL = 720 * time.Hour
	// defaultIMAPHost 等是未配置时使用的默认服务器与端口。
	defaultIMAPHost = "imap.qq.com"
	defaultIMAPPort = 993
	defaultSMTPHost = "smtp.qq.com"
	defaultSMTPPort = 465
	// defaultTokenTTL 是未配置时的回复令牌有效期。
	defaultTokenTTL = 168 * time.Hour
)

// defaultNotifyEvents 是未配置 notify.events 时的默认通知事件。
var defaultNotifyEvents = []string{"waiting_input", "failed", "turn_completed"}

// knownNotifyEvents 是允许出现在 notify.events 中的事件名。
var knownNotifyEvents = []string{"turn_completed", "waiting_input", "waiting_approval", "failed"}

// credentialKeys 是配置文件中一律拒绝的凭据类键名；凭据只存放在 Keychain。
var credentialKeys = []string{"password", "authorization_code", "auth_code", "token", "secret"}

// rawConfig 是 TOML 文件的原始结构；可选字段用指针区分「未设置」与显式零值。
type rawConfig struct {
	Mailbox   rawMailbox   `toml:"mailbox"`
	Recipient rawRecipient `toml:"recipient"`
	Notify    rawNotify    `toml:"notify"`
	Security  rawSecurity  `toml:"security"`
}

// rawMailbox 是 [mailbox] 表的原始字段。
type rawMailbox struct {
	Address  string  `toml:"address"`
	IMAPHost *string `toml:"imap_host"`
	IMAPPort *int    `toml:"imap_port"`
	SMTPHost *string `toml:"smtp_host"`
	SMTPPort *int    `toml:"smtp_port"`
}

// rawRecipient 是 [recipient] 表的原始字段。
type rawRecipient struct {
	Address        string   `toml:"address"`
	AllowedSenders []string `toml:"allowed_senders"`
}

// rawNotify 是 [notify] 表的原始字段；空列表与未设置的含义不同。
type rawNotify struct {
	Events *[]string `toml:"events"`
}

// rawSecurity 是 [security] 表的原始字段。
type rawSecurity struct {
	TokenTTL *string `toml:"token_ttl"`
}

// Load 读取 paths.ConfigFile，严格解码、填充默认值、应用环境变量白名单并完整校验；
// 所有校验错误通过 errors.Join 一次返回，错误文本不包含本机绝对路径。
func Load(paths Paths, getenv func(string) string) (Config, error) {
	raw, undecoded, err := decodeFile(paths.ConfigFile)
	if err != nil {
		return Config{}, err
	}
	problems := keyProblems(undecoded)

	mailboxAddress, err := requiredAddress("mailbox.address", raw.Mailbox.Address)
	problems = append(problems, err)
	recipientAddress, err := requiredAddress("recipient.address", raw.Recipient.Address)
	problems = append(problems, err)
	senders, senderProblems := normalizeSenders(raw.Recipient.AllowedSenders, mailboxAddress)
	problems = append(problems, senderProblems...)

	imapHost, err := resolveHost("mailbox.imap_host", raw.Mailbox.IMAPHost, defaultIMAPHost)
	problems = append(problems, err)
	imapPort, err := resolvePort("mailbox.imap_port", raw.Mailbox.IMAPPort, defaultIMAPPort)
	problems = append(problems, err)
	smtpHost, err := resolveHost("mailbox.smtp_host", raw.Mailbox.SMTPHost, defaultSMTPHost)
	problems = append(problems, err)
	smtpPort, err := resolvePort("mailbox.smtp_port", raw.Mailbox.SMTPPort, defaultSMTPPort)
	problems = append(problems, err)

	events, eventProblems := resolveEvents(raw.Notify.Events, getenv(notifyEventsEnv))
	problems = append(problems, eventProblems...)
	tokenTTL, err := resolveTokenTTL(raw.Security.TokenTTL)
	problems = append(problems, err)

	// errors.Join 丢弃 nil 元素，全部通过时返回 nil。
	if err := errors.Join(problems...); err != nil {
		return Config{}, err
	}
	return Config{
		Mailbox:   Mailbox{Address: mailboxAddress, IMAPHost: imapHost, IMAPPort: imapPort, SMTPHost: smtpHost, SMTPPort: smtpPort},
		Recipient: Recipient{Address: recipientAddress, AllowedSenders: senders},
		Notify:    Notify{Events: events},
		Security:  Security{TokenTTL: tokenTTL},
		Paths:     paths,
	}, nil
}

// decodeFile 检查配置文件的类型、大小与属主权限后严格解码 TOML，并返回未被识别的键。
// 文件系统错误剥离路径后返回，避免面向用户的错误暴露本机绝对路径。
func decodeFile(path string) (rawConfig, []toml.Key, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return rawConfig{}, nil, ErrNotFound
		}
		return rawConfig{}, nil, fmt.Errorf("cannot read config file: %w", withoutPath(err))
	}
	if !info.Mode().IsRegular() {
		return rawConfig{}, nil, errors.New("config file is not a regular file")
	}
	if info.Size() > maxConfigBytes {
		return rawConfig{}, nil, fmt.Errorf("config file is larger than %d bytes", maxConfigBytes)
	}
	if err := checkFileOwner(info); err != nil {
		return rawConfig{}, nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return rawConfig{}, nil, fmt.Errorf("cannot open config file: %w", withoutPath(err))
	}
	defer file.Close()
	var raw rawConfig
	// 文件可能在 Stat 之后被写大，LimitReader 保证解码器读到的字节不超过上界。
	metadata, err := toml.NewDecoder(io.LimitReader(file, maxConfigBytes+1)).Decode(&raw)
	if err != nil {
		return rawConfig{}, nil, fmt.Errorf("cannot parse config file: %w", withoutPath(err))
	}
	return raw, metadata.Undecoded(), nil
}

// keyProblems 把未被识别的键转成错误；凭据类键单独提示，其余按未知键拒绝。
// 键名排序后输出，使同一份配置每次得到相同的错误文本。
func keyProblems(undecoded []toml.Key) []error {
	keys := make([]string, 0, len(undecoded))
	for _, key := range undecoded {
		keys = append(keys, key.String())
	}
	slices.Sort(keys)
	problems := make([]error, 0, len(keys))
	for _, key := range keys {
		if isCredentialKey(key) {
			problems = append(problems, fmt.Errorf("%s must not be set: 凭据将由 init 写入 Keychain", key))
			continue
		}
		problems = append(problems, fmt.Errorf("unknown key %s", key))
	}
	return problems
}

// isCredentialKey 报告键名的任一段是否是被禁止的凭据类名称。
func isCredentialKey(key string) bool {
	for _, segment := range strings.Split(key, ".") {
		if slices.Contains(credentialKeys, segment) {
			return true
		}
	}
	return false
}

// requiredAddress 校验必填的邮箱地址字段，字段缺失与格式非法分别报错。
func requiredAddress(field, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	address, err := NormalizeAddress(raw)
	if err != nil {
		return "", fmt.Errorf("%s: %w", field, err)
	}
	return address, nil
}

// normalizeSenders 规范化发件人白名单：不得为空、规范化后不得重复，也不得包含机器人自身地址（防止自回环）。
// mailboxAddress 为空表示机器人地址本身已经出错，此时跳过自回环检查以免重复报错。
func normalizeSenders(raw []string, mailboxAddress string) ([]string, []error) {
	if len(raw) == 0 {
		return nil, []error{errors.New("recipient.allowed_senders must list at least one address")}
	}
	var problems []error
	seen := make(map[string]bool, len(raw))
	senders := make([]string, 0, len(raw))
	for _, entry := range raw {
		address, err := NormalizeAddress(entry)
		if err != nil {
			problems = append(problems, fmt.Errorf("recipient.allowed_senders: %w", err))
			continue
		}
		if seen[address] {
			problems = append(problems, fmt.Errorf("recipient.allowed_senders repeats %s", address))
			continue
		}
		seen[address] = true
		if mailboxAddress != "" && address == mailboxAddress {
			problems = append(problems, fmt.Errorf("recipient.allowed_senders must not contain the mailbox address %s", address))
			continue
		}
		senders = append(senders, address)
	}
	return senders, problems
}

// resolveHost 填充主机名默认值，并拒绝空串、含空白或含协议前缀的值。
func resolveHost(field string, raw *string, fallback string) (string, error) {
	if raw == nil {
		return fallback, nil
	}
	host := *raw
	if host == "" || strings.Contains(host, "://") || strings.ContainsFunc(host, unicode.IsSpace) {
		return "", fmt.Errorf("%s must be a host name without spaces or a scheme", field)
	}
	return host, nil
}

// resolvePort 填充端口默认值，并限制在 1 到 65535。
func resolvePort(field string, raw *int, fallback int) (int, error) {
	if raw == nil {
		return fallback, nil
	}
	if *raw < 1 || *raw > 65535 {
		return 0, fmt.Errorf("%s must be between 1 and 65535", field)
	}
	return *raw, nil
}

// resolveEvents 计算通知事件：环境变量非空时整体覆盖文件配置，事件名须已知且不重复；
// 文件与环境变量都未给出时使用默认事件，显式的空列表表示不发送任何通知。
func resolveEvents(fileEvents *[]string, env string) ([]string, []error) {
	source := "notify.events"
	var values []string
	switch {
	case env != "":
		source = notifyEventsEnv
		values = strings.Split(env, ",")
		for i := range values {
			values[i] = strings.TrimSpace(values[i])
		}
	case fileEvents != nil:
		values = *fileEvents
	default:
		return slices.Clone(defaultNotifyEvents), nil
	}
	var problems []error
	seen := make(map[string]bool, len(values))
	events := make([]string, 0, len(values))
	for _, value := range values {
		if !slices.Contains(knownNotifyEvents, value) {
			problems = append(problems, fmt.Errorf("%s has unknown event %q", source, value))
			continue
		}
		if seen[value] {
			problems = append(problems, fmt.Errorf("%s repeats event %q", source, value))
			continue
		}
		seen[value] = true
		events = append(events, value)
	}
	return events, problems
}

// resolveTokenTTL 解析回复令牌有效期，缺省 168h，允许范围 1h 到 720h。
func resolveTokenTTL(raw *string) (time.Duration, error) {
	if raw == nil {
		return defaultTokenTTL, nil
	}
	ttl, err := time.ParseDuration(*raw)
	if err != nil {
		return 0, fmt.Errorf("security.token_ttl is not a duration: %q", *raw)
	}
	if ttl < minTokenTTL || ttl > maxTokenTTL {
		return 0, fmt.Errorf("security.token_ttl must be between %s and %s", minTokenTTL, maxTokenTTL)
	}
	return ttl, nil
}

// withoutPath 去掉文件系统错误中的路径，保证面向用户的错误不暴露本机绝对路径。
func withoutPath(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}
