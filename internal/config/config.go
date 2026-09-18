// Package config 的 Load 严格解码 TOML 配置，填充默认值、应用环境变量白名单，并通过 errors.Join 一次返回解码成功后的全部校验错误。
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

// credentialKeys 是配置文件中一律拒绝的凭据类键名（小写，比较时不区分大小写）；凭据只存放在 Keychain。
var credentialKeys = []string{"password", "authorization_code", "auth_code", "token", "secret"}

// knownKeys 是配置文件允许出现的全部键路径，逐段区分大小写比较，须与 rawConfig 的 toml 标签一致。
var knownKeys = []toml.Key{
	{"mailbox"}, {"mailbox", "address"}, {"mailbox", "imap_host"}, {"mailbox", "imap_port"}, {"mailbox", "smtp_host"}, {"mailbox", "smtp_port"},
	{"recipient"}, {"recipient", "address"}, {"recipient", "allowed_senders"},
	{"notify"}, {"notify", "events"},
	{"security"}, {"security", "token_ttl"},
}

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

// Load 读取 paths.ConfigFile，严格解码、填充默认值、应用环境变量白名单并完整校验。
// 文件、TOML 语法或类型错误单独返回；键名错误（未知键、凭据类键）合并后返回，此时不再校验取值；
// 解码成功后的全部校验错误通过 errors.Join 一次返回。错误文本不包含本机绝对路径。
func Load(paths Paths, getenv func(string) string) (Config, error) {
	raw, keys, err := decodeFile(paths.ConfigFile)
	if err != nil {
		return Config{}, err
	}
	// 键名有误时，字段取值可能来自大小写变体等错误的键，且同一字段的多个变体取哪个由 map 遍历顺序决定；
	// 继续校验取值会得到随机变化的错误，因此只返回键名错误。
	if err := errors.Join(keyProblems(keys)...); err != nil {
		return Config{}, err
	}

	var problems []error
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

// decodeFile 先打开配置文件，再对已打开的文件检查类型、大小与属主权限，读取后严格解码 TOML，并返回文件中出现的全部键。
// 先打开再检查，检查与读取针对同一个文件，路径在两者之间被替换也无效；Unix 上以非阻塞方式打开，路径是 FIFO 时不会卡住。
// 文件系统错误剥离路径后返回，避免面向用户的错误暴露本机绝对路径。
func decodeFile(path string) (rawConfig, []toml.Key, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|openFlags, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return rawConfig{}, nil, ErrNotFound
		}
		return rawConfig{}, nil, fmt.Errorf("cannot open config file: %w", withoutPath(err))
	}
	defer file.Close()
	if err := checkOpenedFile(file); err != nil {
		return rawConfig{}, nil, err
	}
	data, err := readConfigData(file)
	if err != nil {
		return rawConfig{}, nil, err
	}
	var raw rawConfig
	metadata, err := toml.Decode(string(data), &raw)
	if err != nil {
		return rawConfig{}, nil, parseError(err)
	}
	return raw, metadata.Keys(), nil
}

// checkOpenedFile 对已打开的文件检查常规文件、大小上界与属主权限。
func checkOpenedFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("cannot read config file: %w", withoutPath(err))
	}
	if !info.Mode().IsRegular() {
		return errors.New("config file is not a regular file")
	}
	if info.Size() > maxConfigBytes {
		return fmt.Errorf("config file is larger than %d bytes", maxConfigBytes)
	}
	return checkFileOwner(info)
}

// readConfigData 最多读取 maxConfigBytes+1 字节，读到的内容超过上界即拒绝：
// 文件可能在检查大小之后被写大，截断后的内容不能交给解析器。
func readConfigData(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read config file: %w", withoutPath(err))
	}
	if len(data) > maxConfigBytes {
		return nil, fmt.Errorf("config file is larger than %d bytes", maxConfigBytes)
	}
	return data, nil
}

// parseError 把 TOML 解码错误转成不回显原文的错误。词法与语法错误（toml.ParseError）的消息会引用出错位置的原始文本，
// 未加引号的凭据值因此会进入错误文本，所以只保留行号、列号与键名；出错的键是凭据类键时改为提示凭据由 init 写入。
// 类型错误只描述键名与类型、不含取值，原样返回。
func parseError(err error) error {
	var parseErr toml.ParseError
	if !errors.As(err, &parseErr) {
		return fmt.Errorf("cannot parse config file: %w", err)
	}
	line, key := parseErr.Position.Line, parseErr.LastKey
	if key != "" && isCredentialKey(strings.Split(key, ".")) {
		return fmt.Errorf("line %d: %s must not be set: 凭据将由 init 写入 Keychain", line, key)
	}
	text := fmt.Sprintf("cannot parse config file: line %d, column %d: invalid TOML", line, parseErr.Position.Col)
	if key != "" {
		text += " (last key " + key + ")"
	}
	return errors.New(text)
}

// keyProblems 把不在 knownKeys 中的键转成错误；凭据类键单独提示，其余按未知键拒绝。
// BurntSushi/toml 在精确匹配失败时按不区分大小写把键匹配到字段并记为已解码，Undecoded() 看不到这些变体，
// 因此逐段区分大小写比对全部键路径。键名排序后输出，使同一份配置每次得到相同的错误文本。
func keyProblems(keys []toml.Key) []error {
	var unknown []toml.Key
	for _, key := range keys {
		if !slices.ContainsFunc(knownKeys, func(known toml.Key) bool { return slices.Equal(known, key) }) {
			unknown = append(unknown, key)
		}
	}
	slices.SortFunc(unknown, func(a, b toml.Key) int { return strings.Compare(a.String(), b.String()) })
	// 表数组（[[x]]）的每个元素都会再列出一次同一键路径，去重后每个键只报一次。
	unknown = slices.CompactFunc(unknown, func(a, b toml.Key) bool { return slices.Equal(a, b) })
	problems := make([]error, 0, len(unknown))
	for _, key := range unknown {
		if isCredentialKey(key) {
			problems = append(problems, fmt.Errorf("%s must not be set: 凭据将由 init 写入 Keychain", key))
			continue
		}
		problems = append(problems, fmt.Errorf("unknown key %s", key))
	}
	return problems
}

// isCredentialKey 报告键路径的任一段转成小写后是否是被禁止的凭据类名称。
func isCredentialKey(segments []string) bool {
	for _, segment := range segments {
		if slices.Contains(credentialKeys, strings.ToLower(segment)) {
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
