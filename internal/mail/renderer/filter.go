// Package renderer 的确定性敏感信息过滤。Filter 按 docs/zh-CN/plans/phase-04.md 4b「已定的实现细节」中「敏感信息过滤」的顺序执行七步：
// ① 去掉控制字符（保留换行与制表）与 Unicode 格式字符；② 围栏代码块整体换成「[代码块已省略：N 行]」；③ 进程机密（经 Scrubber）与
// 令牌形状的串换成「[已省略]」；④ URL 去掉 userinfo，查询串与片段换成「[已省略]」，file:// 链接按路径处理；⑤ 绝对路径换成「[路径已省略]」；
// ⑥ 密钥形状（PEM 私钥块、Authorization 与所列键名的取值、已知令牌前缀的串）换成「[已省略]」；⑦ 超过 4000 个字符即截断。
// 唯一的调整：⑥ 中的 PEM 私钥块与 ② 的围栏一样是跨行的块，紧接在 ② 之后整体替换。按字面放在 ⑤ 之后时，紧贴 BEGIN 行的链接查询串或
// 路径会先吞掉 BEGIN 行、让私钥正文漏过，块被替换后前后的文字也可能拼成新的链接而使结果不幂等；除这类粘连的输入外，两种顺序的结果相同。
//
// 结果是确定的，并且幂等：对输出再过滤一次不变。几条约束保证这一点：替代文字都以方括号包住，不含斜杠、冒号与问号，其中唯一的字母表
// 字符是代码块的行数（两侧都是汉字或空格，拼不成 48 个），也不在行首留下围栏；以三个以上反引号或波浪线（至多 3 个空格缩进）开头的行一律是围栏（不按 CommonMark 排除信息串含反引号的行，
// 否则后面的规则删掉那个反引号后，下一次过滤会把这一行当作围栏）；链接的 scheme 前须是单词边界（file:// 整体替换时不会让前面的一段
// 字母表字符缩成 48 个）；路径的起点不能紧跟在 ] 之后（替代文字之后的 /a/b 在两次过滤中的判断相同）。除围栏与 PEM 块外，每条规则都只看
// 一行之内，而输出中已经没有围栏与 PEM 块。截断在切口处可能切出新的形状（半截替代文字、恰好 48 个字母表字符、已知前缀的串），所以每个
// 候选切口都用 ② 至 ⑥ 复核，不稳定就后退。过滤不承诺发现所有机密；它只是纵深防御，纯状态模式才不发出任何 Agent 文本。
package renderer

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// omitted 与 omittedPath 是被抹去的内容与路径的替代文字。
	omitted     = "[已省略]"
	omittedPath = "[路径已省略]"
	// ellipsis 是截断之后的结尾。
	ellipsis = "…"
	// maxBodyRunes 是过滤后正文的字符数上限（按 Unicode 字符计，含截断的省略号）。
	maxBodyRunes = 4000
	// shapeLen 是令牌文本的长度：前后都不是字母表字符、恰好这么长的一段字母表字符按令牌形状处理。
	shapeLen = 48
	// maxBackoff 是截断时从硬切口逐字符后退复核的最多次数：足以退出一段令牌形状的串、一个键值对或一个已知前缀的串；
	// 之后改在最后一个空白处，再退到行首。
	maxBackoff = 64
)

var (
	// alphabetRun 匹配一段连续的字母表字符，大小写都算，在运行时由字母表常量拼出。匹配取最长，每次命中都是前后都不是字母表字符的一整段。
	alphabetRun = regexp.MustCompile("[" + alphabet + strings.ToUpper(alphabet) + "]+")
	// urlScheme 匹配 URL 的 scheme 与 ://：scheme 前是单词边界，以 ASCII 字母开头，其后是字母、数字、+、- 与点。
	urlScheme = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9+.\-]*://`)
	// pemPattern 匹配一个 PEM 私钥块，从 BEGIN 行到第一个 END 行；没有 END 行时一直到文字末尾。边界行在运行时拼出。
	pemPattern = regexp.MustCompile(`(?s)` + pemMarker("BEGIN") + `.*?(?:` + pemMarker("END") + `|\z)`)
	// fieldPattern 匹配一个敏感键值对的键与分隔符：键名（可带引号）以所列名称结尾，不区分大小写，- 与 _ 等价；分隔符是 :=、==、=>、
	// : 或 =，前后可有空格与制表，不跨行。取值由 valueSpan 从匹配结尾读取。Authorization 与 Proxy-Authorization 也经这条规则。
	fieldPattern = regexp.MustCompile(`(?i)\b[0-9a-z_.\-]*(?:password|passwd|secret|token|api[_-]?key|access[_-]key|private[_-]key|` +
		`auth|credentials?|signature|authorization)["']?[ \t]*(?::=|==|=>|[:=])[ \t]*`)
	// vendorPattern 匹配已知服务商令牌前缀的串：GitHub、GitLab、OpenAI 与 Anthropic（sk-）、Stripe、Slack、npm、PyPI、Hugging Face
	// 之后至少 20 个令牌字符，AWS 访问密钥 ID（AKIA、ASIA 加 16 位），Google API 密钥（AIza 加至少 35 位），以及 JWT（eyJ 开头的
	// 两到三段 base64url）。前面须是单词边界，令牌字符取最长。
	vendorPattern = regexp.MustCompile(`\b(?:(?:ghp|gho|ghu|ghs|ghr)_|github_pat_|glpat-|sk-|sk_live_|sk_test_|rk_live_|rk_test_|` +
		`xox[abeprs]-|npm_|pypi-|hf_)[0-9A-Za-z_\-]{20,}|\b(?:AKIA|ASIA)[0-9A-Z]{16}\b|\bAIza[0-9A-Za-z_\-]{35,}|` +
		`\beyJ[0-9A-Za-z_\-]{10,}\.[0-9A-Za-z_\-]{10,}(?:\.[0-9A-Za-z_\-]+)?`)
)

// pemMarker 拼出 PEM 私钥块的边界行模式：五个连字符、BEGIN 或 END、以 PRIVATE KEY 为核心的大写标签、五个连字符。
// 标签覆盖 RSA、EC、OPENSSH、ENCRYPTED 与 PGP（PRIVATE KEY BLOCK）等；公钥与证书不含 PRIVATE KEY，不在其列。
func pemMarker(kind string) string {
	dashes := strings.Repeat("-", 5)
	return dashes + kind + ` [A-Z0-9 ]*PRIVATE KEY[A-Z0-9 ]*` + dashes
}

// Filter 按固定顺序过滤文字（见包说明），返回过滤后的文字；结果确定、幂等、是合法 UTF-8，至多 4000 个字符。
// scrub 为 nil 时跳过进程机密的抹除（没有进程机密可抹，例如测试），其余规则照常执行。
func Filter(text string, scrub Scrubber) string {
	out, _ := filter(text, scrub)
	return out
}

// filter 执行 Filter，另返回 ② 至 ⑦ 是否改动了文字（有省略或截断）：只去掉控制与格式字符不算省略。
func filter(text string, scrub Scrubber) (string, bool) {
	cleaned := stripControls(text)
	out, _ := truncate(redact(cleaned, scrub), maxBodyRunes, scrub)
	return out, out != cleaned
}

// redact 依次执行 ② 至 ⑥（⑥ 中的 PEM 私钥块紧接在 ② 之后，见包说明）；输入须已经过 ①。
func redact(text string, scrub Scrubber) string {
	text = replaceFences(text)
	text = pemPattern.ReplaceAllLiteralString(text, omitted)
	text = replaceSecrets(text, scrub)
	text = replaceURLs(text)
	text = replacePaths(text)
	return replaceKeyShapes(text)
}

// stripControls 执行 ①：CRLF 与单独的 CR 换成换行，去掉换行与制表之外的控制字符（C0、DEL、C1）与全部 Unicode 格式字符（Cf：
// 双向控制、零宽字符、BOM、软连字符、标签字符等）；非法 UTF-8 字节换成 U+FFFD（strings.Map 的行为）。
func stripControls(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
			return -1
		}
		return r
	}, text)
}

// replaceFences 执行 ②：自上而下找围栏的开头行，连同其后直到关闭行（没有关闭行时直到末尾）的各行一起换成「[代码块已省略：N 行]」，
// N 为两道围栏之间的行数。文字以换行结尾时结果同样以换行结尾。
func replaceFences(text string) string {
	lines := strings.Split(text, "\n")
	trailing := len(lines) > 1 && lines[len(lines)-1] == ""
	if trailing {
		lines = lines[:len(lines)-1]
	}
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		char, n, ok := fenceOpening(lines[i])
		if !ok {
			out = append(out, lines[i])
			continue
		}
		end := i + 1
		for end < len(lines) && !fenceClosing(lines[end], char, n) {
			end++
		}
		out = append(out, fmt.Sprintf("[代码块已省略：%d 行]", end-i-1))
		i = end
	}
	result := strings.Join(out, "\n")
	if trailing {
		result += "\n"
	}
	return result
}

// fenceOpening 判断一行是否为围栏的开头：按 CommonMark 允许的缩进，至多 3 个空格，之后是至少 3 个 ` 或至少 3 个 ~。返回围栏字符与个数。
// 制表缩进不算（CommonMark 把它当作 4 个空格，那是缩进代码块）。与 CommonMark 不同，信息串含反引号的行同样算开头（见包说明的幂等约束），
// 行首的 ```ls``` 这类行内代码因此也按代码块省略，偏向多省略。
func fenceOpening(line string) (byte, int, bool) {
	rest, ok := fenceIndent(line)
	if !ok || rest == "" || (rest[0] != '`' && rest[0] != '~') {
		return 0, 0, false
	}
	char := rest[0]
	n := runLength(rest, char)
	return char, n, n >= 3
}

// fenceClosing 判断一行是否关闭以 n 个 char 开头的围栏：至多 3 个空格的缩进，至少 n 个同一字符，其后只有空格与制表。
func fenceClosing(line string, char byte, n int) bool {
	rest, ok := fenceIndent(line)
	if !ok {
		return false
	}
	k := runLength(rest, char)
	return k >= n && strings.Trim(rest[k:], " \t") == ""
}

// fenceIndent 去掉行首至多 3 个空格；缩进超过 3 个空格时返回 false。
func fenceIndent(line string) (string, bool) {
	rest := strings.TrimLeft(line, " ")
	return rest, len(line)-len(rest) <= 3
}

// runLength 返回 s 开头连续的 char 的个数。
func runLength(s string, char byte) int {
	n := 0
	for n < len(s) && s[n] == char {
		n++
	}
	return n
}

// replaceSecrets 执行 ③：先经 Scrubber 抹去进程机密，再把每段恰好 48 个字母表字符（不区分大小写）的串换成「[已省略]」。
// 顺序不影响结果：两者都只把一段文字换成替代文字，替代文字不含字母表字符。
func replaceSecrets(text string, scrub Scrubber) string {
	if scrub != nil {
		text = scrub.Scrub(text)
	}
	return alphabetRun.ReplaceAllStringFunc(text, func(run string) string {
		if len(run) == shapeLen {
			return omitted
		}
		return run
	})
}

// replaceURLs 执行 ④：每个 scheme:// 起头的链接延伸到第一个空白、引号、尖括号、反引号或非 ASCII 标点为止。file:// 链接整体换成
// 「[路径已省略]」；其他链接去掉 userinfo（authority 中最后一个 @ 之前的部分），第一个 ? 或 # 起到链接结尾整体换成「[已省略]」，
// 保留 scheme、主机与路径。链接中再出现的 scheme:// 属于同一个链接。
func replaceURLs(text string) string {
	var b strings.Builder
	pos := 0
	for _, span := range linkSpans(text) {
		b.WriteString(text[pos:span.start])
		b.WriteString(redactLink(text[span.start:span.scheme], text[span.scheme:span.end]))
		pos = span.end
	}
	if pos == 0 {
		return text
	}
	b.WriteString(text[pos:])
	return b.String()
}

// link 是文字中一个链接的位置：start 是 scheme 的起点，scheme 是 :// 之后的位置，end 是链接的结束位置。
type link struct {
	start, scheme, end int
}

// linkSpans 按 ④ 的规则找出文字中的全部链接：每个 scheme:// 延伸到 linkEnd 为止，落在前一个链接之内的 scheme:// 属于同一个链接。
// ⑤ 用同一份结果避开链接，所以两步对「哪些文字属于链接」的判断一致。
func linkSpans(text string) []link {
	var spans []link
	pos := 0
	for _, m := range urlScheme.FindAllStringIndex(text, -1) {
		if m[0] < pos {
			continue
		}
		pos = linkEnd(text, m[1])
		spans = append(spans, link{start: m[0], scheme: m[1], end: pos})
	}
	return spans
}

// linkEnd 返回从 i 起链接的结束位置：第一个空白、" ' ` < > 或非 ASCII 标点（如「，」「。」「）」）之前。
func linkEnd(text string, i int) int {
	for i < len(text) {
		r, size := utf8.DecodeRuneInString(text[i:])
		if unicode.IsSpace(r) || strings.ContainsRune("\"'`<>", r) || (r >= utf8.RuneSelf && unicode.IsPunct(r)) {
			return i
		}
		i += size
	}
	return i
}

// redactLink 处理一个链接：prefix 是 scheme 与 ://，rest 是其后的部分。
func redactLink(prefix, rest string) string {
	if strings.EqualFold(prefix, "file://") {
		return omittedPath
	}
	authority := len(rest)
	if k := strings.IndexAny(rest, "/?#"); k >= 0 {
		authority = k
	}
	if at := strings.LastIndexByte(rest[:authority], '@'); at >= 0 {
		rest = rest[at+1:]
	}
	if k := strings.IndexAny(rest, "?#"); k >= 0 {
		rest = rest[:k] + omitted
	}
	return prefix + rest
}

// replacePaths 执行 ⑤：从左到右扫描，在路径起点（文字开头，或前一个字符不是 ASCII 字母、数字与 . _ ~ - / \ @ % + ] 之一）处识别
// 三种绝对路径并换成「[路径已省略]」：至少两段的 /段/段（第一段非空，其后的段可为空，如 /var/log/）、~/段 与 X:\…。
// 段由路径字符组成：空白、斜杠、" ' ` < > ( ) [ ] { } , ; | : 与非 ASCII 标点之外的字符（含中文），所以 PATH 中冒号之后的每一段、
// host:/path 与紧贴中文的路径都能识别，路径止于「，」「。」等标点。链接（linkSpans）整个跳过，路径也不能伸进其后的链接：
// 否则 C:\https://… 这样的路径会吞掉链接的 scheme，下一次过滤时链接中的其他部分就成了新的链接。
func replacePaths(text string) string {
	var b strings.Builder
	written := 0
	prev := rune(-1)
	spans := linkSpans(text)
	for i := 0; i < len(text); {
		for len(spans) > 0 && spans[0].end <= i {
			spans = spans[1:]
		}
		limit := len(text)
		if len(spans) > 0 {
			if spans[0].start <= i {
				i = spans[0].end
				prev, _ = utf8.DecodeLastRuneInString(text[:i])
				continue
			}
			limit = spans[0].start
		}
		if !continuesPath(prev) {
			if end := pathEnd(text[:limit], i); end > i {
				b.WriteString(text[written:i])
				b.WriteString(omittedPath)
				written, i = end, end
				prev = ']'
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		prev = r
		i += size
	}
	if written == 0 {
		return text
	}
	b.WriteString(text[written:])
	return b.String()
}

// continuesPath 判断前一个字符是否让其后的 /、~ 或盘符不能作为绝对路径的起点：ASCII 字母、数字与 . _ ~ - / \ @ % +
// （相对路径、URL 的主机与路径、单词的一部分），以及 ]（替代文字的结尾：一段串被换成替代文字之后，紧跟其后的 /a/b 在下一次过滤中
// 的判断须与这一次相同）。prev 为 -1 表示文字开头。
func continuesPath(prev rune) bool {
	return prev >= 0 && prev < utf8.RuneSelf &&
		(('a' <= prev && prev <= 'z') || ('A' <= prev && prev <= 'Z') || ('0' <= prev && prev <= '9') || strings.ContainsRune(`._~-/\@%+]`, prev))
}

// pathEnd 返回从 i 起的绝对路径的结束位置，i 处不是绝对路径时返回 -1。
func pathEnd(text string, i int) int {
	switch c := text[i]; {
	case c == '/':
		if end, slashes := segments(text, i); slashes >= 2 {
			return end
		}
	case c == '~' && i+1 < len(text) && text[i+1] == '/':
		if end, slashes := segments(text, i+1); slashes >= 1 {
			return end
		}
	case (('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')) && strings.HasPrefix(text[i+1:], `:\`):
		end := i + 3
		for end < len(text) {
			r, size := utf8.DecodeRuneInString(text[end:])
			if r != '/' && !isPathRune(r) {
				break
			}
			end += size
		}
		if end > i+3 {
			return end
		}
	}
	return -1
}

// segments 从 text[i] == '/' 起读取「/ 段」的序列：第一段须非空，其后的段可以为空。返回结束位置与读到的斜杠数；第一段为空时斜杠数为 0。
func segments(text string, i int) (int, int) {
	slashes := 0
	for i < len(text) && text[i] == '/' {
		j := i + 1
		for j < len(text) {
			r, size := utf8.DecodeRuneInString(text[j:])
			if !isPathRune(r) {
				break
			}
			j += size
		}
		if slashes == 0 && j == i+1 {
			return i, 0
		}
		slashes++
		i = j
	}
	return i, slashes
}

// isPathRune 判断字符能否出现在路径的一段中：不是空白、斜杠、" ' ` < > ( ) [ ] { } , ; | :，也不是非 ASCII 标点。反斜杠可以出现
// （Windows 路径的分隔符）。
func isPathRune(r rune) bool {
	if unicode.IsSpace(r) || r == '/' {
		return false
	}
	if r < utf8.RuneSelf {
		return !strings.ContainsRune("\"'`<>()[]{},;|:", r)
	}
	return !unicode.IsPunct(r)
}

// replaceKeyShapes 执行 ⑥ 中逐行的部分（PEM 私钥块已在 ② 之后替换）：敏感键值对与 Authorization 的取值换成「[已省略]」，
// 已知令牌前缀的串换成「[已省略]」。两者的命中若与某个链接的 scheme（链接起点到 :// 为止，见 schemeGuard）重叠则不替换。
func replaceKeyShapes(text string) string {
	text = replaceFieldValues(text)
	guard := schemeGuard(linkSpans(text))
	var b strings.Builder
	written := 0
	for _, m := range vendorPattern.FindAllStringIndex(text, -1) {
		if guard(m[0], m[1]) {
			continue
		}
		b.WriteString(text[written:m[0]])
		b.WriteString(omitted)
		written = m[1]
	}
	if written == 0 {
		return text
	}
	b.WriteString(text[written:])
	return b.String()
}

// schemeGuard 返回一个判断函数：[start, end) 是否与某个链接的 scheme 部分（链接起点到 :// 之后）重叠。调用时 start 须单调不减。
// ④ 之后的规则可以改写链接的主机、路径与其后的文字，但不能碰 scheme：scheme 允许字母、数字、+、- 与点，sk-… 这样的已知前缀的串
// 或以 token 结尾的「键名」都可能正是 scheme，改写它会拆掉这个链接，下一次过滤时链接中的其他部分（例如嵌在其中的 file://）就成了
// 新的链接，结果不幂等。这类粘在 scheme 上的串会留下，属于过滤不承诺覆盖的形状。
func schemeGuard(spans []link) func(start, end int) bool {
	return func(start, end int) bool {
		for len(spans) > 0 && spans[0].scheme <= start {
			spans = spans[1:]
		}
		return len(spans) > 0 && spans[0].start < end
	}
}

// replaceFieldValues 把 fieldPattern 命中的每个键值对的取值换成「[已省略]」：取值带引号时只换引号之内（没有闭合时到行尾），
// 否则换到行尾；取值为空时不改动。落在前一个取值之内的命中，以及键名与分隔符碰到链接 scheme 的命中（例如 mytoken://…，见 schemeGuard）跳过。
func replaceFieldValues(text string) string {
	var b strings.Builder
	written := 0
	guard := schemeGuard(linkSpans(text))
	for _, m := range fieldPattern.FindAllStringIndex(text, -1) {
		if m[0] < written || guard(m[0], m[1]) {
			continue
		}
		start, end := valueSpan(text, m[1])
		if start == end {
			continue
		}
		b.WriteString(text[written:start])
		b.WriteString(omitted)
		written = end
	}
	if written == 0 {
		return text
	}
	b.WriteString(text[written:])
	return b.String()
}

// valueSpan 返回从 i 起的取值要替换的范围：以 " 或 ' 开头时是引号之内（反斜杠转义下一个字符，遇到同一种引号或行尾为止），
// 否则是到行尾的全部文字。
func valueSpan(text string, i int) (int, int) {
	if i < len(text) && (text[i] == '"' || text[i] == '\'') {
		quote := text[i]
		j := i + 1
		for j < len(text) && text[j] != quote && text[j] != '\n' {
			if text[j] == '\\' && j+1 < len(text) && text[j+1] != '\n' {
				j++
			}
			j++
		}
		return i + 1, j
	}
	end := strings.IndexByte(text[i:], '\n')
	if end < 0 {
		return i, len(text)
	}
	return i, i + end
}

// truncate 执行 ⑦：s（已经过 ① 至 ⑥）超过 limit 个字符时截成至多 limit 个字符，以「…」结尾。候选切口依次是：留下 limit−1 个字符的
// 硬切口、从它逐字符后退至多 maxBackoff 次、最后一个空白之前、当前行的行首；第一个经 ② 至 ⑥ 复核不变的候选即为结果，保证结果幂等
// （切口处不会留下半截替代文字、令牌形状的串或已知前缀的串）。行首的候选只含完整的行，除 Scrubber 行为异常外总能通过复核；
// 都不通过时只返回「…」。返回值的第二项表示是否截断。
func truncate(s string, limit int, scrub Scrubber) (string, bool) {
	if utf8.RuneCountInString(s) <= limit {
		return s, false
	}
	end := 0
	for range limit - 1 {
		_, size := utf8.DecodeRuneInString(s[end:])
		end += size
	}
	// stable 以 s[:at] 加省略号为候选，复核它经 ② 至 ⑥ 不变。
	stable := func(at int) (string, bool) {
		candidate := s[:at] + ellipsis
		return candidate, redact(candidate, scrub) == candidate
	}
	at := end
	for range maxBackoff + 1 {
		if candidate, ok := stable(at); ok {
			return candidate, true
		}
		if at == 0 {
			break
		}
		_, size := utf8.DecodeLastRuneInString(s[:at])
		at -= size
	}
	if space := strings.LastIndexFunc(s[:end], unicode.IsSpace); space >= 0 {
		if candidate, ok := stable(space); ok {
			return candidate, true
		}
	}
	if candidate, ok := stable(strings.LastIndexByte(s[:end], '\n') + 1); ok {
		return candidate, true
	}
	return ellipsis, true
}
