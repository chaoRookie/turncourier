// Package parser 的引用与签名剥离、残留检查。输入是已解码的纯文本；自上而下找第一条引用边界，边界所在行及其后全部丢弃，
// 再按签名规则丢弃结尾的签名，最后检查新正文里有没有我方通知的残留。规则见 docs/zh-CN/plans/phase-04.md 4b Task 3 与
// 「已定的实现细节」中的「引用与签名剥离」；解析不确定时不执行，所以凡是可能把用户的文字当成引用、或把引用留给 Agent 的形态，
// 都返回 ErrUncertain，而不是猜测。
package parser

import (
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/chaoRookie/turncourier/internal/mail"
)

const (
	// alphabet 是任务 ID 与回复令牌共用的小写 Crockford base32 字母表（与 security/token 相同；本包不能导入 security/*）。
	alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	// shapeLen 是令牌文本的长度：新正文中前后都不是字母表字符、恰好这么长的一段字母表字符，按令牌副本的残留处理。
	shapeLen = 48
	// maxDashTail 是单独一行「--」之后最多允许的行数：其后至多这么多行就到正文末尾时，它才按签名分隔线处理。
	maxDashTail = 6
	// byteOrderMark 是 UTF-8 正文开头可能带着的 BOM（U+FEFF）。
	byteOrderMark = "\ufeff"
)

var (
	// alphabetRun 匹配一段连续的字母表字符，大小写都算，在运行时由字母表常量拼出。匹配取最长，所以每次命中恰是前后都不是
	// 字母表字符的一整段。
	alphabetRun = regexp.MustCompile("[" + alphabet + strings.ToUpper(alphabet) + "]+")
	// qqSeparator 匹配 QQ 的分隔线（规则 1）：由破折号包围的「原始邮件」或 Original，长短都算，文字两侧可有空白、
	// 不换行空格与全角空格。
	qqSeparator = regexp.MustCompile(`^-+[\s\x{00A0}\x{3000}]*(?:原始邮件|Original)[\s\x{00A0}\x{3000}]*-+$`)
	// outlookSeparator 匹配只折叠了 ASCII 大小写的 Outlook 分隔线 -----Original Message-----（规则 2）。
	outlookSeparator = regexp.MustCompile(`^-+[\s\x{00A0}\x{3000}]*original message[\s\x{00A0}\x{3000}]*-+$`)
	// fromLine 匹配没有分隔线的引用头块的首行（规则 5）：「发件人」或 From 加冒号，冒号为半角或全角，前面可有空白。
	fromLine = regexp.MustCompile(`^(?:发件人|From)[\s\x{00A0}\x{3000}]*[:：]`)
	// headerLine 匹配引用头块中首行之后 3 行内应出现的某一行。
	headerLine = regexp.MustCompile(`^(?:发送时间|日期|收件人|主题|Sent|Date|To|Subject)[\s\x{00A0}\x{3000}]*[:：]`)
	// squeezedMarker 与 squeezedNotice 是页脚标记行与固定句子删去全部空白后的形式：比较时两边都删去空白，
	// 被客户端折行、改动空白的副本同样认得出。
	squeezedMarker = removeSpace(mail.FooterMarker)
	squeezedNotice = removeSpace(mail.FooterNotice)
)

// clientSignatures 是常见客户端自动附加的签名。新正文（去掉首尾空白后）的最后一个非空行等于其中之一时，丢弃这一行。
var clientSignatures = []string{
	"发自我的iPhone", "发自我的 iPhone", "发自我的iPad", "发自我的 iPad",
	"Sent from my iPhone", "Sent from my iPad",
	"来自QQ邮箱", "发自网易邮箱大师",
	"Get Outlook for iOS", "Get Outlook for Android",
}

// newText 对已解码的纯文本执行 NewText 的其余步骤：统一换行、去掉开头的 BOM、剥离引用、剥离签名、去掉首尾空白，
// 为空返回 ErrEmpty，有残留返回 ErrUncertain。输入是合法 UTF-8 时结果也是，且不长于输入。
func newText(text string) (string, error) {
	text = strings.TrimPrefix(normalizeNewlines(text), byteOrderMark)
	kept, err := stripQuotes(text)
	if err != nil {
		return "", err
	}
	kept = strings.TrimSpace(stripSignature(kept))
	switch {
	case kept == "":
		return "", ErrEmpty
	case hasResidue(kept):
		return "", ErrUncertain
	}
	return kept, nil
}

// normalizeNewlines 把 CRLF 与单独的 CR 都换成 LF。
func normalizeNewlines(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

// stripQuotes 自上而下找第一条引用边界，返回边界之前的文字；五类边界依次是：
//
//  1. QQ 的分隔线（qqSeparator）与单独一行的「原始邮件」；
//  2. Outlook 的 -----Original Message-----；
//  3. 以「写道：」「写道:」或 wrote:（不区分大小写）结尾的引用头，且其后第一个非空行以 > 开头；
//  4. 以 > 开头的行：这一段连续的 > 行去掉前缀后含页脚标记行或 [TC 才是引用我方通知，否则返回 ErrUncertain（可能是用户
//     自己用 > 标出的命令或引文）；
//  5. 以「发件人」或 From 加冒号开头、其后 3 行内还有发送时间、日期、收件人、主题等头行的引用头块（Foxmail 式）。
//
// 规则 3 与 4 的引用块之后若又出现既不是引用、也不是签名的文字（行内逐段回复），同样返回 ErrUncertain：只保留引用之前的部分
// 会让 Agent 只收到第一段。规则 4 只看引用前缀，所以以 > 开头的行先按规则 4 处理，规则 3 只认不带前缀的引用头。
func stripQuotes(text string) (string, error) {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if isQuoteLine(line) {
			end := i + 1
			for end < len(lines) && isQuoteLine(lines[end]) {
				end++
			}
			if !quotesNotice(lines[i:end]) || !onlyQuotesAfter(lines, i) {
				return "", ErrUncertain
			}
			return strings.Join(lines[:i], "\n"), nil
		}
		trimmed := strings.TrimSpace(line)
		if isSeparator(trimmed) {
			return strings.Join(lines[:i], "\n"), nil
		}
		// 只在引用头上查找下一个非空行：每次查找止于下一个非空行，而它就是下一个被检查的候选，总开销是线性的。
		if isAttribution(trimmed) {
			if next := nextNonBlank(lines, i+1); next >= 0 && isQuoteLine(lines[next]) {
				if !onlyQuotesAfter(lines, next) {
					return "", ErrUncertain
				}
				return strings.Join(lines[:i], "\n"), nil
			}
		}
		if isHeaderBlock(lines, i) {
			return strings.Join(lines[:i], "\n"), nil
		}
	}
	return text, nil
}

// isSeparator 判断去掉首尾空白的一行是否为规则 1 或 2 的分隔线。
func isSeparator(trimmed string) bool {
	return trimmed == "原始邮件" || qqSeparator.MatchString(trimmed) || outlookSeparator.MatchString(lowerASCII(trimmed))
}

// isAttribution 判断去掉首尾空白的一行是否以「写道：」「写道:」或 wrote:（只折叠 ASCII 大小写）结尾；它是否为边界还要看下一个非空行。
func isAttribution(trimmed string) bool {
	return strings.HasSuffix(trimmed, "写道：") || strings.HasSuffix(trimmed, "写道:") || strings.HasSuffix(lowerASCII(trimmed), "wrote:")
}

// isHeaderBlock 判断第 i 行是否为规则 5 的引用头块首行：它匹配 fromLine，且其后 3 行内有一行匹配 headerLine。
// 单独一行 From: 不算，免得截断用户自己的文字。
func isHeaderBlock(lines []string, i int) bool {
	if !fromLine.MatchString(strings.TrimSpace(lines[i])) {
		return false
	}
	for k := i + 1; k < len(lines) && k <= i+3; k++ {
		if headerLine.MatchString(strings.TrimSpace(lines[k])) {
			return true
		}
	}
	return false
}

// isQuoteLine 判断一行在行首空白之后是否以 > 开头。
func isQuoteLine(line string) bool {
	return strings.HasPrefix(strings.TrimLeftFunc(line, unicode.IsSpace), ">")
}

// unquote 去掉行首空白与任意层 > 前缀（>、>>、> > 等）及其后的空格与制表符。
func unquote(line string) string {
	line = strings.TrimLeftFunc(line, unicode.IsSpace)
	for strings.HasPrefix(line, ">") {
		line = strings.TrimLeft(line[1:], " \t")
	}
	return line
}

// quotesNotice 判断一段连续的 > 行是否引用我方通知：去掉前缀后任一行含 [TC（ASCII 不区分大小写），或整段拼接后
// （忽略空白）含页脚标记行。拼接之后再比较，标记行被折成两行时同样认得出。
func quotesNotice(run []string) bool {
	var joined strings.Builder
	for _, line := range run {
		content := unquote(line)
		if hasTagOpening(content) {
			return true
		}
		joined.WriteString(content)
	}
	return hasFooterMarker(joined.String())
}

// onlyQuotesAfter 判断从第 start 行起是否只有引用行与空行，直到文字结束或一段签名开始；否则就是行内逐段回复。
func onlyQuotesAfter(lines []string, start int) bool {
	last := lastNonBlank(lines)
	for k := start; k < len(lines); k++ {
		if isBlank(lines[k]) || isQuoteLine(lines[k]) {
			continue
		}
		return signatureStartsAt(lines, k, last)
	}
	return true
}

// signatureStartsAt 判断第 k 行是否开始一段签名，规则与 stripSignature 相同：恰为「-- 」的行；单独的「--」且其后至多
// maxDashTail 行就到最后一个非空行（下标 last）；或者它就是最后一个非空行，且等于已知的客户端签名。
func signatureStartsAt(lines []string, k, last int) bool {
	switch lines[k] {
	case "-- ":
		return true
	case "--":
		return last-k <= maxDashTail
	}
	return k == last && isClientSignature(lines[k])
}

// stripSignature 按顺序丢弃签名：最后一个恰为「-- 」的行及其后（RFC 3676）；去掉结尾空行后，最后一个单独的「--」若其后至多
// maxDashTail 行，则它及其后；再去掉结尾空行后，最后一行若等于已知的客户端签名，则丢弃这一行。
func stripSignature(text string) string {
	lines := strings.Split(text, "\n")
	if k := lastIndex(lines, "-- "); k >= 0 {
		lines = lines[:k]
	}
	lines = trimTrailingBlank(lines)
	if k := lastIndex(lines, "--"); k >= 0 && len(lines)-1-k <= maxDashTail {
		lines = lines[:k]
	}
	lines = trimTrailingBlank(lines)
	if n := len(lines); n > 0 && isClientSignature(lines[n-1]) {
		lines = lines[:n-1]
	}
	return strings.Join(lines, "\n")
}

// isClientSignature 判断一行去掉首尾空白后是否等于已知的客户端签名。
func isClientSignature(line string) bool {
	return slices.Contains(clientSignatures, strings.TrimSpace(line))
}

// lastIndex 返回最后一个恰等于 target 的行的下标，没有时返回 -1。
func lastIndex(lines []string, target string) int {
	for k := len(lines) - 1; k >= 0; k-- {
		if lines[k] == target {
			return k
		}
	}
	return -1
}

// trimTrailingBlank 去掉结尾的空行（只含空白的行也算）。
func trimTrailingBlank(lines []string) []string {
	for len(lines) > 0 && isBlank(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// isBlank 判断一行是否只含空白（unicode.IsSpace，含不换行空格与全角空格）。
func isBlank(line string) bool {
	return strings.TrimSpace(line) == ""
}

// nextNonBlank 返回第 from 行起第一个非空行的下标，没有时返回 -1。
func nextNonBlank(lines []string, from int) int {
	for k := from; k < len(lines); k++ {
		if !isBlank(lines[k]) {
			return k
		}
	}
	return -1
}

// lastNonBlank 返回最后一个非空行的下标，没有时返回 -1。
func lastNonBlank(lines []string) int {
	for k := len(lines) - 1; k >= 0; k-- {
		if !isBlank(lines[k]) {
			return k
		}
	}
	return -1
}

// hasResidue 判断剥离之后的新正文里是否残留我方通知的片段：令牌形状的串、[TC、页脚的标记行或固定句子。
// 任一命中都说明引用没有剥净，令牌副本正要进入 Agent 会话。
func hasResidue(text string) bool {
	return hasAlphabetRun(text) || hasTagOpening(text) || hasFooterMarker(text) || hasFooterNotice(text)
}

// hasAlphabetRun 判断文字中是否有一段前后都不是字母表字符、恰好 shapeLen 个字母表字符（不区分大小写）的串。
// 逐段向后查找，找到即停；每段都是最长匹配，所以更长的串不会被误认成令牌。
func hasAlphabetRun(text string) bool {
	for rest := text; ; {
		span := alphabetRun.FindStringIndex(rest)
		if span == nil {
			return false
		}
		if span[1]-span[0] == shapeLen {
			return true
		}
		rest = rest[span[1]:]
	}
}

// hasTagOpening 判断文字中是否出现 [TC，T 与 C 只按 ASCII 不区分大小写。逐字节比较对 UTF-8 是安全的：
// 多字节字符的每个字节都不小于 0x80，不会被当成 [、T 或 C。
func hasTagOpening(text string) bool {
	for i := 0; i+2 < len(text); i++ {
		if text[i] == '[' && (text[i+1] == 'T' || text[i+1] == 't') && (text[i+2] == 'C' || text[i+2] == 'c') {
			return true
		}
	}
	return false
}

// hasFooterMarker 判断文字删去全部空白后是否含页脚标记行（同样删去空白）。
func hasFooterMarker(text string) bool {
	return strings.Contains(removeSpace(text), squeezedMarker)
}

// hasFooterNotice 判断文字删去全部空白后是否含页脚的固定句子（同样删去空白）。
func hasFooterNotice(text string) bool {
	return strings.Contains(removeSpace(text), squeezedNotice)
}

// removeSpace 删去文字中的全部空白（unicode.IsSpace，含换行、不换行空格与全角空格）。
func removeSpace(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, text)
}

// lowerASCII 只把 ASCII 大写字母 A–Z 转成小写，其余字节原样保留：结果与原文逐字节对齐，也不会像 strings.ToLower 那样
// 把开尔文符号等非 ASCII 字符折成 ASCII 字母。
func lowerASCII(text string) string {
	b := []byte(text)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
