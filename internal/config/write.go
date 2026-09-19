// Package config 的 Render 为 init 渲染与示例同格式的配置文本，CreateFile 以 0600 原子创建配置文件且从不覆盖。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Draft 是 init 收集的配置内容；地址须已经 NormalizeAddress 规范化。
type Draft struct {
	MailboxAddress   string
	RecipientAddress string
	AllowedSenders   []string
}

// Render 生成与 configs/turncourier.example.toml 同格式的 TOML 文本：注释相同，主机、端口、事件与有效期写默认值，地址按草稿填写。
// 校验与 Load 相同的地址规则：每个地址规范化后不变、白名单非空且不重复、机器人地址不在白名单中。
// 规范化后的 addr-spec 不含引号与反斜杠，strconv.Quote 的结果即合法的 TOML 基本字符串。
func Render(d Draft) ([]byte, error) {
	problems := []error{
		checkNormalized("mailbox.address", d.MailboxAddress),
		checkNormalized("recipient.address", d.RecipientAddress),
	}
	for _, sender := range d.AllowedSenders {
		problems = append(problems, checkNormalized("recipient.allowed_senders", sender))
	}
	if err := errors.Join(problems...); err != nil {
		return nil, err
	}
	if _, senderProblems := normalizeSenders(d.AllowedSenders, d.MailboxAddress); len(senderProblems) > 0 {
		return nil, errors.Join(senderProblems...)
	}
	return []byte(fmt.Sprintf(configTemplate,
		strconv.Quote(d.MailboxAddress), strconv.Quote(defaultIMAPHost), defaultIMAPPort, strconv.Quote(defaultSMTPHost), defaultSMTPPort,
		strconv.Quote(d.RecipientAddress), quoteList(d.AllowedSenders),
		quoteList(defaultNotifyEvents), strconv.Quote(strconv.Itoa(int(defaultTokenTTL.Hours()))+"h"))), nil
}

// configTemplate 是 Render 的文本模板，注释与 configs/turncourier.example.toml 逐字相同。
const configTemplate = `# TurnCourier 配置示例。只保存账户与选项：
# 邮箱授权码与两把密钥由 turncourier init 写入 macOS Keychain，禁止写在本文件中。

[mailbox]
# 专用机器人发件邮箱
address = %s
imap_host = %s
imap_port = %d
smtp_host = %s
smtp_port = %d

[recipient]
# 接收通知的个人邮箱
address = %s
# 只有这些发件人的回复会被处理；规范化后精确匹配
allowed_senders = [%s]

[notify]
# 可选：turn_completed、waiting_input、waiting_approval、failed
events = [%s]

[security]
# 回复令牌有效期，1h 到 720h
token_ttl = %s
`

// checkNormalized 要求 address 是合法地址且已经规范化（小写、无首尾空白）；错误不回显未规范化的输入。
func checkNormalized(field, address string) error {
	normalized, err := NormalizeAddress(address)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	if normalized != address {
		return fmt.Errorf("%s: %w: address must be lowercase without surrounding spaces", field, ErrInvalidAddress)
	}
	return nil
}

// quoteList 把字符串逐个加引号并以逗号加空格连接，作为 TOML 数组的内容。
func quoteList(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = strconv.Quote(value)
	}
	return strings.Join(quoted, ", ")
}

// CreateFile 以 0600 原子创建 path，从不覆盖：父目录缺失时以 0700 创建；先在同一目录写临时文件并 Sync，
// 再用 os.Link 放到 path（已存在时返回包装 fs.ErrExist 的错误），最后删除临时文件。错误文本不含路径。
func CreateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	// 用 %v 而不是 %w：目录路径是悬空的符号链接时 MkdirAll 报 EEXIST，它不能被当成「目标已存在」。
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cannot create config directory: %v", withoutPath(err))
	}
	// CreateTemp 以 0600 创建文件；无论成功与否都删除临时文件，目标文件由硬链接保留。
	temp, err := os.CreateTemp(dir, ".turncourier-*.tmp")
	if err != nil {
		return fmt.Errorf("cannot create config file: %w", withoutPath(err))
	}
	defer os.Remove(temp.Name())
	_, err = temp.Write(data)
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("cannot write config file: %w", withoutPath(err))
	}
	// os.Link 在目标已存在时失败，不会替换它；LinkError 同时含两个路径，只保留底层错误。
	if err := os.Link(temp.Name(), path); err != nil {
		var linkErr *os.LinkError
		if errors.As(err, &linkErr) {
			err = linkErr.Err
		}
		return fmt.Errorf("cannot create config file: %w", err)
	}
	return nil
}
