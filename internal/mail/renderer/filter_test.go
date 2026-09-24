// Package renderer 测试确定性的敏感信息过滤：七条规则逐条的正反用例（控制与格式字符、围栏代码块、进程机密与令牌形状的串、URL、
// 绝对路径、密钥形状、截断），规则之间可观察的先后，截断不在切口处制造新的敏感形状，「有省略」标志，以及每一条结果再过滤一次不变（幂等）。
// 令牌由 security/token 在运行时签发，机密与令牌前缀的串都在运行时拼出，源码中没有这类值的字面量。
package renderer

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/chaoRookie/turncourier/internal/security/token"
)

// omit、omitPath 是规格规定的替代文本，直接写出以钉住输出。
const (
	omit     = "[已省略]"
	omitPath = "[路径已省略]"
)

// filterCase 是过滤规则表中的一行：输入与期望输出。
type filterCase struct {
	name string
	in   string
	want string
}

// listScrubber 是测试用的 Scrubber：把给定的机密原样出现处换成「[已省略]」。
type listScrubber []string

// Scrub 依次把每个机密的全部出现处换成「[已省略]」。
func (s listScrubber) Scrub(text string) string {
	for _, secret := range s {
		text = strings.ReplaceAll(text, secret, omit)
	}
	return text
}

// cycleText 从 charset 中按位置循环取字符，拼出 n 个字符的串；offset 让不同调用得到不同的串。用于在运行时构造机密与令牌前缀之后的部分。
func cycleText(charset string, n, offset int) string {
	var b strings.Builder
	for i := range n {
		b.WriteByte(charset[(i*7+offset)%len(charset)])
	}
	return b.String()
}

// mixedChars 含全部字母与数字（包括字母表之外的 i、l、o、u），循环取出的串里连续的字母表字符不会长到 48 个。
const mixedChars = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"

// testSecrets 返回三份在运行时构造的进程机密：16 个小写字母的授权码与两把 43 个字符的 base64url 密钥文本。
func testSecrets() (authCode, signingKey, bodyKey string) {
	return cycleText("abcdefghijklmnopqrstuvwxyz", 16, 3), cycleText(mixedChars+"-_", 43, 5), cycleText(mixedChars+"-_", 43, 11)
}

// testScrubber 返回抹去 testSecrets 三份机密的 Scrubber。
func testScrubber() listScrubber {
	a, b, c := testSecrets()
	return listScrubber{a, b, c}
}

// issueText 用由 seed 重复而成的密钥为 taskID 签发一枚真实令牌，返回 48 个字符的令牌文本；不同的 seed 得到不同的令牌。
func issueText(tb testing.TB, taskID string, seed byte) string {
	tb.Helper()
	return issueTag(tb, taskID, seed).RevealToken()
}

// issueTag 用由 seed 重复而成的密钥为 taskID 签发一枚真实令牌，并与任务 ID 组成主题标签。
func issueTag(tb testing.TB, taskID string, seed byte) token.Tag {
	tb.Helper()
	k, err := token.NewKey(1, bytes.Repeat([]byte{seed}, token.KeyLen))
	if err != nil {
		tb.Fatalf("NewKey：%v", err)
	}
	var nid token.NID
	for i := range nid {
		nid[i] = seed + byte(i)
	}
	issued, err := token.Issue(k, nid, token.Claims{TaskID: taskID, Owner: "local", ExpiresAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		tb.Fatalf("Issue：%v", err)
	}
	tag, err := token.NewTag(taskID, issued)
	if err != nil {
		tb.Fatalf("NewTag：%v", err)
	}
	return tag
}

// pemLine 在运行时拼出 PEM 的边界行（五个连字符包着 BEGIN 或 END 与标签），源码中因此没有完整的私钥边界行。
func pemLine(kind, label string) string {
	return strings.Repeat("-", 5) + kind + " " + label + strings.Repeat("-", 5)
}

// pemBlock 拼出一个 PEM 块：BEGIN 行、body 各行与 END 行。
func pemBlock(label string, body ...string) string {
	lines := append([]string{pemLine("BEGIN", label)}, body...)
	return strings.Join(append(lines, pemLine("END", label)), "\n")
}

// runFilterCases 逐行运行规则表：Filter 的结果等于期望，对结果再过滤一次不变（幂等）。
func runFilterCases(t *testing.T, scrub Scrubber, cases []filterCase) {
	t.Helper()
	for _, tc := range cases {
		got := Filter(tc.in, scrub)
		if got != tc.want {
			t.Errorf("%s：Filter = %q\n期望 %q", tc.name, got, tc.want)
		}
		if again := Filter(got, scrub); again != got {
			t.Errorf("%s：不幂等，再过滤一次得到 %q", tc.name, again)
		}
	}
}

// same 返回输入与期望相同的一行规则表（反例：过滤不改动这段文字）。
func same(name, text string) filterCase {
	return filterCase{name: name, in: text, want: text}
}

// TestFilterControlAndFormat 覆盖规则 ①：去掉控制字符（保留换行与制表，CRLF 与单独的 CR 换成换行）与 Unicode 格式字符
// （双向控制、零宽字符、BOM、软连字符、标签字符），非法 UTF-8 字节换成 U+FFFD；普通空白、组合字符与表情保留。
func TestFilterControlAndFormat(t *testing.T) {
	runFilterCases(t, nil, []filterCase{
		{"NUL 与响铃", "a\x00b\x07c", "abc"},
		{"CRLF 换成换行", "a\r\nb", "a\nb"},
		{"单独的 CR 换成换行", "a\rb", "a\nb"},
		{"DEL 与 C1 控制字符", "a\x7fb\u0085c\u009bd", "abcd"},
		{"双向覆盖与嵌入", "abc\u202edef\u202a\u202c", "abcdef"},
		{"双向隔离", "\u2066x\u2067y\u2068z\u2069", "xyz"},
		{"零宽字符与 BOM", "a\u200bb\u200cc\u200dd\u2060e\ufeff", "abcde"},
		{"LRM 与 RLM", "a\u200eb\u200fc", "abc"},
		{"软连字符与标签字符", "a\u00adb\U000E0041c", "abc"},
		{"非法 UTF-8", "a\xffb\xc3", "a\ufffdb\ufffd"},
		same("保留换行与制表", "第一行\n\t第二行"),
		same("不换行空格与全角空格", "a\u00a0b\u3000c"),
		same("组合字符与表情", "e\u0301 \U0001F600"),
	})
}

// TestFilterFences 覆盖规则 ②：``` 与 ~~~ 围栏代码块按 CommonMark 允许的缩进识别（开头至多 3 个空格；关闭围栏须为同一字符、不短于开头、
// 其后只有空白），整块换成「[代码块已省略：N 行]」，N 为两道围栏之间的行数；没有闭合的一直到末尾。信息串含反引号的行同样是开头
// （与 CommonMark 不同，为了幂等，见 TestFilterCrossRuleStability）。
func TestFilterFences(t *testing.T) {
	runFilterCases(t, nil, []filterCase{
		{"反引号围栏", "前\n```go\nfmt.Println(1)\nx := 2\n```\n后", "前\n[代码块已省略：2 行]\n后"},
		{"波浪线围栏", "~~~\n代码\n~~~", "[代码块已省略：1 行]"},
		{"空代码块", "```\n```", "[代码块已省略：0 行]"},
		{"只被不短于开头的围栏关闭", "````\n```\n内\n```\n````", "[代码块已省略：3 行]"},
		{"关闭围栏可以更长", "```\n内\n`````", "[代码块已省略：1 行]"},
		{"波浪线不关闭反引号围栏", "```\na\n~~~\nb\n```", "[代码块已省略：3 行]"},
		{"缩进 1 到 3 个空格", " ```\na\n  ```\n   ~~~\nb\n   ~~~", "[代码块已省略：1 行]\n[代码块已省略：1 行]"},
		{"关闭围栏之后可有空白", "```\na\n```  \t\n后", "[代码块已省略：1 行]\n后"},
		{"没有闭合一直到末尾", "前\n```\na\nb", "前\n[代码块已省略：2 行]"},
		{"没有闭合且以换行结尾", "前\n```\na\n", "前\n[代码块已省略：1 行]\n"},
		{"带信息串的行不能关闭", "```\na\n```go\nb", "[代码块已省略：3 行]"},
		{"两个代码块", "```\na\n```\n中\n~~~\nb\nc\n~~~", "[代码块已省略：1 行]\n中\n[代码块已省略：2 行]"},
		{"波浪线围栏的信息串可含反引号", "~~~ a`b\nx\n~~~", "[代码块已省略：1 行]"},
		same("4 个空格缩进不是围栏", "    ```\n    code\n    ```"),
		same("制表缩进不是围栏", "\t```\ncode\n\t```"),
		same("两个反引号不是围栏", "``\ncode\n``"),
		{"信息串含反引号同样是围栏", "``` a`b\ncode", "[代码块已省略：1 行]"},
		same("行内代码", "运行 ```ls``` 查看"),
	})
}

// TestFilterSecretsAndShapes 覆盖规则 ③：Scrubber 抹去的进程机密（授权码与两把密钥的文本）与任何令牌形状的串（前后都不是字母表字符、
// 恰好 48 个字母表字符，不区分大小写）换成「[已省略]」；47、49 个字符、被字母表之外的字母隔开的串与任务 ID 不受影响。
func TestFilterSecretsAndShapes(t *testing.T) {
	authCode, signingKey, bodyKey := testSecrets()
	shape := issueText(t, testTask, 0x21)
	hex := cycleText("0123456789abcdef", 64, 1)
	runFilterCases(t, testScrubber(), []filterCase{
		{"授权码", "授权码是 " + authCode + " 。", "授权码是 " + omit + " 。"},
		{"两把密钥", "k1 " + signingKey + "\nk2 " + bodyKey, "k1 " + omit + "\nk2 " + omit},
		{"机密紧贴其他文字", "前" + authCode + "后", "前" + omit + "后"},
		{"令牌形状的串", "令牌 " + shape + " 结束", "令牌 " + omit + " 结束"},
		{"大写的令牌形状", strings.ToUpper(shape), omit},
		{"紧贴中文与标点", "（" + shape + "）中" + shape + "文", "（" + omit + "）中" + omit + "文"},
		{"被字母表之外的大写字母隔开", "I" + shape + "O", "I" + omit + "O"},
		same("47 个字母表字符", shape[:47]),
		same("49 个字母表字符", shape+"a"),
		same("中间有字母表之外的字母", shape[:20]+"i"+shape[21:]),
		same("任务 ID", "任务 "+testTask),
		same("64 位十六进制摘要", hex),
	})
	// 没有 Scrubber 时不抹除进程机密，令牌形状的串照样抹去。
	if got := Filter(authCode+" "+shape, nil); got != authCode+" "+omit {
		t.Errorf("Filter(…, nil) = %q", got)
	}
}

// TestFilterURLs 覆盖规则 ④：scheme:// 链接保留主机与路径，去掉 userinfo，查询串与片段整体换成「[已省略]」；链接止于空白、
// 引号、尖括号与非 ASCII 标点；file:// 链接整体按路径处理。没有 scheme:// 的文字不当作链接，链接中的路径不按绝对路径处理。
func TestFilterURLs(t *testing.T) {
	pw := cycleText(mixedChars, 12, 2)
	sig := cycleText(mixedChars, 40, 9)
	runFilterCases(t, nil, []filterCase{
		{"userinfo、查询串与片段", "见 https://alice:" + pw + "@example.com/a/b?x=1&y=2#frag 。", "见 https://example.com/a/b" + omit + " 。"},
		{"预签名链接", "https://example.com/p?X-Amz-Signature=" + sig, "https://example.com/p" + omit},
		{"OAuth 回调的片段", "https://example.com/cb#access_" + "token=" + sig, "https://example.com/cb" + omit},
		{"数据库连接串", "postgres://app:" + pw + "@db.example.invalid:5432/prod", "postgres://db.example.invalid:5432/prod"},
		{"userinfo 中含 @", "https://a@b@example.com/p", "https://example.com/p"},
		{"只有用户名", "ssh://git@example.com/repo.git", "ssh://example.com/repo.git"},
		{"中文标点结束链接", "https://example.com/p?q=1，然后", "https://example.com/p" + omit + "，然后"},
		{"尖括号结束链接", "<https://example.com/p?q=1>", "<https://example.com/p" + omit + ">"},
		{"引号结束链接", `"https://example.com/p?q=1" 与 'https://example.com/r#s'`, `"https://example.com/p` + omit + `" 与 'https://example.com/r` + omit + `'`},
		{"大写 scheme", "HTTPS://u:p@EXAMPLE.COM/P?Q", "HTTPS://EXAMPLE.COM/P" + omit},
		{"file 链接按路径处理", "打开 file:///Users/alice/报告.pdf 查看", "打开 " + omitPath + " 查看"},
		{"file 链接带主机", "file://server/share/x.txt", omitPath},
		{"FILE 大写", "FILE:///etc/hosts", omitPath},
		same("主机与路径保留", "https://example.com/Users/alice/x"),
		same("没有 scheme 的地址", "example.com/p?q=1"),
		same("只有 scheme", "https://"),
	})
}

// TestFilterPaths 覆盖规则 ⑤：不属于 scheme://host 的绝对路径（至少两段，可紧贴中文或跟在等号、冒号、引号、括号之后）、
// PATH 中冒号之后的每一段、host:/path、~/… 与 X:\… 换成「[路径已省略]」；相对路径、单段路径、日期与分数、URL 中的路径不受影响。
func TestFilterPaths(t *testing.T) {
	runFilterCases(t, nil, []filterCase{
		{"Unix 绝对路径", "/Users/alice/project/main.go", omitPath},
		{"句中路径", "打开 /etc/nginx/nginx.conf 查看", "打开 " + omitPath + " 查看"},
		{"紧贴中文", "文件在/Users/张三/报告.docx中", "文件在" + omitPath},
		{"中文标点结束路径", "路径：/srv/app/config，然后", "路径：" + omitPath + "，然后"},
		{"PATH 的每一段", "PATH=/usr/bin:/Users/alice/bin:/opt/tool/bin", "PATH=" + omitPath + ":" + omitPath + ":" + omitPath},
		{"host:/path", "scp app.tar deploy.example.invalid:/var/www/app", "scp app.tar deploy.example.invalid:" + omitPath},
		{"~ 路径", "cd ~/projects/demo && ls ~/x", "cd " + omitPath + " && ls " + omitPath},
		{"Windows 路径", `C:\Users\alice\x.txt`, omitPath},
		{"Windows 路径紧贴中文", `位于D:\data\x中`, "位于" + omitPath},
		{"Windows 小写盘符", `e:\a`, omitPath},
		{"引号与括号中的路径", `"/a/b" (/c/d) '/e/f'`, `"` + omitPath + `" (` + omitPath + `) '` + omitPath + `'`},
		{"以斜杠结尾的目录", "/var/log/", omitPath},
		{"等号之后", "--dir=/data/cache", "--dir=" + omitPath},
		same("相对路径", "src/main.go ./a/b ../c/d a/b/c"),
		same("单段绝对路径", "/tmp 与 /etc"),
		same("and/or、日期与分数", "and/or 2026/09/24 1/2/3"),
		same("两个斜杠开头", "//comment/x"),
		same("URL 中的路径", "https://example.com/a/b"),
		same("盘符之后没有内容", `C:\`),
		same("波浪线之后不是斜杠", "~user/x 与 ~"),
	})
}

// TestFilterKeyShapes 覆盖规则 ⑥：PEM 私钥块（没有结束行的一直到末尾）、Authorization 之后的取值、键名以所列名称结尾
// （不区分大小写，- 与 _ 等价）的键值对的取值（带引号时只换引号之内）、已知令牌前缀的串，都换成「[已省略]」。
func TestFilterKeyShapes(t *testing.T) {
	v := cycleText(mixedChars, 20, 4)
	b64 := func(n, offset int) string { return cycleText(mixedChars+"-_", n, offset) }
	upper := func(n int) string { return cycleText("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", n, 6) }
	cases := []filterCase{
		{"PEM 私钥块", pemBlock("RSA PRIVATE KEY", "AAAA", "BBBB"), omit},
		{"PEM 前后的文字保留", "密钥如下：\n" + pemBlock("OPENSSH PRIVATE KEY", "AAAA") + "\n完毕", "密钥如下：\n" + omit + "\n完毕"},
		{"没有结束行的 PEM", "前\n" + pemLine("BEGIN", "EC PRIVATE KEY") + "\nAAAA\n其余", "前\n" + omit},
		{"两个 PEM 块", pemBlock("PRIVATE KEY", "A") + "\n中\n" + pemBlock("ENCRYPTED PRIVATE KEY", "B"), omit + "\n中\n" + omit},
		{"PGP 私钥块", pemBlock("PGP PRIVATE KEY BLOCK", "A"), omit},
		same("公钥与证书", pemBlock("PUBLIC KEY", "AAAA")+"\n"+pemBlock("CERTIFICATE", "BBBB")),

		{"Authorization 头", "Authorization: Bearer " + v, "Authorization: " + omit},
		{"小写与代理头", "authorization: Basic " + v + "\nProxy-Authorization: Digest q", "authorization: " + omit + "\nProxy-Authorization: " + omit},
		{"curl 的 -H 参数", `curl -H "Authorization: Bearer ` + v + `" https://example.com`, `curl -H "Authorization: ` + omit},
		{"JSON 中的 Authorization", `{"Authorization": "Bearer ` + v + `", "Accept": "*/*"}`, `{"Authorization": "` + omit + `", "Accept": "*/*"}`},

		{"环境变量", "OPENAI_API_KEY=" + v, "OPENAI_API_KEY=" + omit},
		{"YAML", "db_password: " + v, "db_password: " + omit},
		{"JSON 保留引号与其余字段", `{"password": "` + v + `", "user": "bob"}`, `{"password": "` + omit + `", "user": "bob"}`},
		{"单引号", "export GITHUB_TOKEN='" + v + "'", "export GITHUB_TOKEN='" + omit + "'"},
		{"转义的引号", `password="a\"b" 其余`, `password="` + omit + `" 其余`},
		{"没有闭合的引号", `secret="` + v, `secret="` + omit},
		{"Go 的 :=", "apiKey := \"" + v + "\"", "apiKey := \"" + omit + "\""},
		{"箭头", "'passwd' => '" + v + "'", "'passwd' => '" + omit + "'"},
		{"比较", "if token == \"" + v + "\" {", "if token == \"" + omit + "\" {"},
		{"大小写与空白", "Client_Secret = " + v, "Client_Secret = " + omit},
		{"同一行两个键值", `password="a" token="b"`, `password="` + omit + `" token="` + omit + `"`},
		{"没有引号时取到行尾", "auth: user " + v + "\n下一行", "auth: " + omit + "\n下一行"},

		{"GitHub", "ghp_" + b64(36, 1), omit},
		{"GitHub 细粒度", "github_pat_" + b64(59, 2), omit},
		{"OpenAI", "sk-proj-" + b64(40, 3), omit},
		{"Anthropic", "sk-ant-api03-" + b64(40, 4), omit},
		{"Slack", "xoxb-" + b64(30, 5), omit},
		{"GitLab", "glpat-" + b64(20, 6), omit},
		{"Stripe", "sk_live_" + b64(24, 7), omit},
		{"npm", "npm_" + b64(36, 8), omit},
		{"Hugging Face", "hf_" + b64(34, 9), omit},
		{"Google", "AIza" + b64(35, 10), omit},
		{"AWS", "AKIA" + upper(16), omit},
		{"JWT", "eyJ" + b64(20, 11) + "." + b64(30, 12) + "." + b64(40, 13), omit},
		{"句中的令牌", "用 ghp_" + b64(36, 14) + " 登录", "用 " + omit + " 登录"},
		same("前缀之后太短", "sk-short ghp_abc AKIA"+upper(15)),
		same("前缀之后 19 个字符", "ghp_"+b64(19, 16)+" sk-"+b64(19, 17)),
		same("前缀在单词中间", "task-"+b64(30, 15)),
		same("AKIA 之后多一位", "AKIA"+upper(17)),
	}
	for _, name := range []string{"password", "passwd", "secret", "token", "api_key", "api-key", "apikey", "access_key", "access-key",
		"private_key", "private-key", "auth", "credential", "credentials", "signature"} {
		cases = append(cases, filterCase{"键名 " + name, "x_" + name + "=" + v, "x_" + name + "=" + omit})
	}
	cases = append(cases,
		same("键名只是包含", "password_hint=记得 tokens=5 author: bob authentication: on keyboard=1"),
		same("没有分隔符", "the password is "+v),
		same("取值为空", "password=\napi_key: \nsecret=\"\""),
	)
	runFilterCases(t, nil, cases)
}

// TestFilterTruncation 覆盖规则 ⑦：过滤后的文字超过 4000 个字符（按 Unicode 字符计）时截断，结果连同结尾的「…」至多 4000 个字符；
// 截断按过滤之后的长度判断；切口处不能留下半截机密、令牌形状的串或会在下一次过滤时变化的形状（结果仍幂等）。
func TestFilterTruncation(t *testing.T) {
	if maxBodyRunes != 4000 {
		t.Fatalf("maxBodyRunes = %d，契约规定为 4000", maxBodyRunes)
	}
	exact := strings.Repeat("中", maxBodyRunes)
	runFilterCases(t, nil, []filterCase{
		same("恰好 4000 个字符", exact),
		{"4001 个字符", exact + "文", strings.Repeat("中", maxBodyRunes-1) + "…"},
		{"ASCII 同样按字符计", strings.Repeat("ab ", 2000), strings.Repeat("ab ", 1333) + "…"},
		// 过滤前 6900 多个字符，代码块换成替代文字之后不到 4000 个，不截断。
		{"按过滤之后的长度判断", strings.Repeat("中", 3000) + "\n```\n" + strings.Repeat("x", 3000) + "\n```\n" + strings.Repeat("文", 900),
			strings.Repeat("中", 3000) + "\n[代码块已省略：1 行]\n" + strings.Repeat("文", 900)},
	})

	// 机密跨过切口：先抹除再截断，结果中不能留下机密的任何一段（先截断会留下前 9 个字符，Scrubber 认不出半截的机密）。
	authCode, _, _ := testSecrets()
	runFilterCases(t, testScrubber(), []filterCase{
		{"机密跨过切口", strings.Repeat("中", maxBodyRunes-10) + authCode + strings.Repeat("文", 100),
			strings.Repeat("中", maxBodyRunes-10) + omit + strings.Repeat("文", 4) + "…"},
	})

	// 硬切口会在切口处留下新的形状时，逐字符后退、在空白处切或退到行首，结果仍幂等（runFilterCases 同时核对）。
	shape := issueText(t, testTask, 0x31)
	run := strings.Repeat(shape, 2)[:60]
	runFilterCases(t, nil, []filterCase{
		// 硬切会把 60 个字母表字符切成恰好 48 个，形成令牌形状的串；后退一个字符即可。
		{"切出令牌形状", strings.Repeat("中", maxBodyRunes-49) + run, strings.Repeat("中", maxBodyRunes-49) + run[:47] + "…"},
		// 硬切会把「[已省略]」切成半截，下一次过滤时键值规则会把它换回「[已省略]」；后退到键名之后。
		{"切开替代文字", strings.Repeat("中", maxBodyRunes-12) + " password=" + strings.Repeat("x", 50) + "\n" + strings.Repeat("文", 50),
			strings.Repeat("中", maxBodyRunes-12) + " password…"},
		// 切口落在分隔符之后的一长串空格中：逐字符后退与在空白处切都会留下没有取值的「password=」，下一次过滤会把省略号当作取值，
		// 只能退到行首。
		{"切在键值对的分隔符之后", strings.Repeat("中", 3900) + "\npassword=" + strings.Repeat(" ", 100) + strings.Repeat("x", 200),
			strings.Repeat("中", 3900) + "\n…"},
		// 链接的 scheme 以 sk- 加 100 个字符开头时，已知前缀的规则避开它；切口落在 scheme 之内，链接不复存在，sk- 之后还剩 20 个以上字符的
		// 候选都会被当作已知前缀的串，逐字符后退 64 次仍不够，在 sk- 之前的空白处切。
		{"切在已知前缀的串中", strings.Repeat("中", 3905) + " sk-" + cycleText(mixedChars, 100, 5) + "https://h/p\n" + strings.Repeat("文", 100),
			strings.Repeat("中", 3905) + "…"},
		// AKIA 之后多一位时不是 AWS 密钥，硬切正好切出 AKIA 加 16 位；后退一个字符即可。
		{"切出已知前缀", strings.Repeat("中", maxBodyRunes-21) + "AKIA" + strings.Repeat("A", 17) + strings.Repeat("文", 30),
			strings.Repeat("中", maxBodyRunes-21) + "AKIA" + strings.Repeat("A", 15) + "…"},
	})
}

// tail 返回文字最后 n 个字符，用于错误信息。
func tail(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[len(runes)-n:])
}

// hasShape 判断文字中是否有前后都不是字母表字符、恰好 48 个字母表字符（不区分大小写）的串；与被测代码独立实现，逐字符计数。
func hasShape(text string) bool {
	run := 0
	for _, r := range text + " " {
		if isAlphabetChar(r) {
			run++
			continue
		}
		if run == 48 {
			return true
		}
		run = 0
	}
	return false
}

// isAlphabetChar 判断字符（ASCII 大写字母先折成小写）是否属于字母表。
func isAlphabetChar(r rune) bool {
	if 'A' <= r && r <= 'Z' {
		r += 'a' - 'A'
	}
	return r < 0x80 && strings.ContainsRune(alphabetChars, r)
}

// alphabetChars 是任务 ID 与令牌共用的小写字母表，测试独立写出，不引用被测包的常量。
const alphabetChars = "0123456789abcdefghjkmnpqrstvwxyz"

// TestFilterOrder 钉住规则之间可观察的先后：① 先于 ②（零宽字符之后的围栏照样识别）、② 先于 PEM 块（围栏中的 PEM 块按原行数计）、
// PEM 块先于 ④ 与 ⑤（紧贴 BEGIN 行的链接查询串或路径不会吞掉 BEGIN 行而让私钥正文漏过）、③ 先于 ⑤（路径中令牌形状的一段先被抹去）、
// ⑤ 先于 ⑥（路径中已知前缀的串随路径整体替换）、③ 先于 ④（紧贴 scheme 的令牌形状的串连同 scheme 一起抹去）、
// ③ 至 ⑥ 先于 ⑦（见 TestFilterTruncation 中跨过切口的机密）。④ 与 ⑤ 的先后不可观察：⑤ 用同一份链接识别避开链接，
// ④ 只改链接之内、⑤ 只改链接之外。
func TestFilterOrder(t *testing.T) {
	shape := issueText(t, testTask, 0x41)
	runFilterCases(t, nil, []filterCase{
		{"① 先于 ②", "\u200b```\ncode\n```", "[代码块已省略：1 行]"},
		{"② 先于 PEM", "```\n" + pemBlock("RSA PRIVATE KEY", "AAAA", "BBBB", "CCCC") + "\n```", "[代码块已省略：5 行]"},
		{"PEM 先于 ④", "https://example.com/p?x=" + pemBlock("RSA PRIVATE KEY", "MIIEsecretbody"), "https://example.com/p" + omit},
		{"PEM 先于 ⑤", "/srv/keys/" + pemBlock("EC PRIVATE KEY", "MIIEsecretbody"), omitPath + omit},
		{"③ 先于 ⑤", "/srv/" + shape, omitPath + omit},
		{"⑤ 先于 ⑥", "/srv/ghp_" + cycleText(mixedChars, 36, 3), omitPath},
		// 43 个字母表字母紧贴 https 共 48 个：③ 先把这一段（连同 scheme）当作令牌形状抹去，链接随之不复存在，④ 不再处理其后的查询串。
		// 这是按字面顺序的已知局限（粘在 scheme 上的串），先处理 ④ 会得到另一个结果。
		{"③ 先于 ④", cycleText("abcdefghjkmnpqrstvwxyz", 43, 0) + "https://h/p?q=1", omit + "://h/p?q=1"},
	})
}

// TestFilterCrossRuleStability 钉住幂等所需的几条约束：后面的规则改动文字之后，前面的规则在下一次过滤中的判断不能改变。
// 以反引号开头、信息串含反引号的行一律是围栏（否则键值规则删掉反引号后下一次成了围栏）；file:// 的 scheme 前须是单词边界
// （否则整体替换会让前面的 49 个字母表字符缩成 48 个）；PEM 块替换后前后拼成的链接在同一次过滤中处理；替代文字之后的 /a/b
// 不是路径起点（否则已知前缀的串被替换后，下一次会把它当作路径）；④ 之后的规则不碰链接的 scheme：已知前缀的串与以 token 结尾的
// 「键名」若正是 scheme 就留下，路径不伸进链接（否则链接被拆掉，其中的 file:// 在下一次过滤中成了新的链接）。
func TestFilterCrossRuleStability(t *testing.T) {
	digits := strings.Repeat("0123456789", 5)[:48]
	runFilterCases(t, nil, []filterCase{
		{"信息串含反引号的键值", "``` token=`x`\n其后", "[代码块已省略：1 行]"},
		same("scheme 前须是单词边界", digits+"file:///srv/app/x"),
		{"PEM 块两侧拼成链接", "https://example.com/p" + pemBlock("RSA PRIVATE KEY", "AAAA") + "?q=1", "https://example.com/p" + omit + omit},
		{"替代文字之后的斜杠", "ghp_" + cycleText(mixedChars+"-_", 36, 1) + "/srv/app", omit + "/srv/app"},
		same("已知前缀的串是链接的 scheme", "sk-"+cycleText(mixedChars, 24, 2)+"https://h/p/file:///srv/x"),
		same("以 token 结尾的 scheme", `C:\Userstoken://h/p`),
		same("路径不伸进链接", `C:\https://h/p/file:///srv/x`),
	})
}

// TestFilterOmittedFlag 断言「有省略」标志：只有规则 ② 至 ⑦ 改动了文字时为真；只去掉控制与格式字符、没有任何命中，或文字中
// 原本就是替代文字（例如转发来的已过滤内容）时为假。
func TestFilterOmittedFlag(t *testing.T) {
	authCode, _, _ := testSecrets()
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"普通文字", false},
		{"a\u200bb\x00c", false},
		{"password=" + omit + "\n" + omitPath + "\n[代码块已省略：3 行]", false},
		{"```\nx\n```", true},
		{"码 " + authCode, true},
		{"https://example.com/p?q=1", true},
		{"/a/b/c", true},
		{"token: x", true},
		{strings.Repeat("中", maxBodyRunes+1), true},
	} {
		if _, omitted := filter(tc.in, testScrubber()); omitted != tc.want {
			t.Errorf("filter(%q) 的省略标志为 %t，期望 %t", tail(tc.in, 30), omitted, tc.want)
		}
	}
}
