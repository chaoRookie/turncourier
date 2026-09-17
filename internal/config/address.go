// Package config 加载并校验 TurnCourier 的 TOML 配置；配置只保存账户与选项，不保存任何凭据。
package config

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidAddress 表示邮箱地址不是可接受的单个 ASCII addr-spec。
var ErrInvalidAddress = errors.New("invalid email address")

const (
	// maxAddressLen 是规范化后地址的总长上界。
	maxAddressLen = 254
	// maxLocalLen 是本地部分的长度上界。
	maxLocalLen = 64
	// maxDomainLen 是域名的长度上界。
	maxDomainLen = 253
	// maxLabelLen 是单个域名标签的长度上界。
	maxLabelLen = 63
	// dotAtomSpecials 是 dot-atom 本地部分允许的非字母数字字符，点号另行按位置校验。
	dotAtomSpecials = "!#$%&'*+/=?^_`{|}~-"
)

// NormalizeAddress 把单个邮箱地址规范化为小写 addr-spec。
// 拒绝显示名、尖括号、多个地址、引号本地部分、非 ASCII、超长输入与不合规域名，
// 结果可用于发件人白名单的精确匹配。
func NormalizeAddress(raw string) (string, error) {
	address := strings.TrimSpace(raw)
	if len(address) > maxAddressLen {
		return "", fmt.Errorf("%w: address is longer than %d characters", ErrInvalidAddress, maxAddressLen)
	}
	if strings.Count(address, "@") != 1 {
		return "", fmt.Errorf("%w: want exactly one @ in a single address", ErrInvalidAddress)
	}
	local, domain, _ := strings.Cut(address, "@")
	if err := checkLocalPart(local); err != nil {
		return "", err
	}
	if err := checkDomain(domain); err != nil {
		return "", err
	}
	// 校验通过的地址只含 ASCII，转小写不会改变长度或再次引入非法字符。
	return strings.ToLower(address), nil
}

// checkLocalPart 校验本地部分：1–64 个 dot-atom 字符，点号不在首尾且不连续。
func checkLocalPart(local string) error {
	if local == "" || len(local) > maxLocalLen {
		return fmt.Errorf("%w: local part must be 1 to %d characters", ErrInvalidAddress, maxLocalLen)
	}
	if strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return fmt.Errorf("%w: local part has a leading, trailing or repeated dot", ErrInvalidAddress)
	}
	if strings.ContainsFunc(local, func(r rune) bool { return !isDotAtomRune(r) }) {
		return fmt.Errorf("%w: local part has a character outside the dot-atom set", ErrInvalidAddress)
	}
	return nil
}

// checkDomain 校验域名：总长不超过 253，至少两个标签，
// 每个标签 1–63 个 ASCII 字母、数字或连字符，且不以连字符开头或结尾。
func checkDomain(domain string) error {
	if len(domain) > maxDomainLen {
		return fmt.Errorf("%w: domain is longer than %d characters", ErrInvalidAddress, maxDomainLen)
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return fmt.Errorf("%w: domain needs at least two labels", ErrInvalidAddress)
	}
	for _, label := range labels {
		if label == "" || len(label) > maxLabelLen {
			return fmt.Errorf("%w: domain label must be 1 to %d characters", ErrInvalidAddress, maxLabelLen)
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return fmt.Errorf("%w: domain label has a leading or trailing hyphen", ErrInvalidAddress)
		}
		if strings.ContainsFunc(label, func(r rune) bool { return !isLabelRune(r) }) {
			return fmt.Errorf("%w: domain label has a character outside letters, digits and hyphen", ErrInvalidAddress)
		}
	}
	return nil
}

// isDotAtomRune 报告字符是否属于本地部分允许的 ASCII 集合，点号包含在内。
func isDotAtomRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.':
		return true
	default:
		return strings.ContainsRune(dotAtomSpecials, r)
	}
}

// isLabelRune 报告字符是否可用于域名标签：ASCII 字母、数字或连字符。
func isLabelRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		return true
	default:
		return false
	}
}
