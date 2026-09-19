// Package smtp 用 go/parser 检查本包非测试源码：只使用 go-smtp 与 go-sasl 的白名单符号，不出现调试输出与放宽证书校验的标识符，
// tls.Config 只在 tlsConfig 中构造一次。功能测试无法证明「从不调用 Dial、DialStartTLS」这类否定性质，由本文件钉住。
package smtp

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// 源码检查的白名单与禁用标识符。
var (
	// allowedImports 是各受限模块允许使用的包级符号；键为导入路径，值的默认包名不同于路径末段时见 defaultNames。
	allowedImports = map[string][]string{
		"github.com/emersion/go-smtp": {"DialTLS", "Client", "DataCommand", "DataResponse", "SMTPError"},
		"github.com/emersion/go-sasl": {"NewPlainClient"},
	}
	// defaultNames 是未命名导入时各路径的包名。
	defaultNames = map[string]string{
		"github.com/emersion/go-smtp": "smtp",
		"github.com/emersion/go-sasl": "sasl",
		"crypto/tls":                  "tls",
	}
	// forbiddenIdents 是任何非测试文件都不得出现的标识符：调试输出会写出凭据，其余会放宽或绕过证书校验、泄露会话密钥。
	forbiddenIdents = []string{"DebugWriter", "InsecureSkipVerify", "KeyLogWriter", "VerifyPeerCertificate", "VerifyConnection"}
	// tlsConfigKeys 是 tls.Config 字面量允许且必须设置的字段。
	tlsConfigKeys = []string{"MinVersion", "RootCAs", "ServerName"}
)

// tlsConfigFunc 是唯一允许构造 tls.Config 的函数名。
const tlsConfigFunc = "tlsConfig"

// TestSourceRestrictions 解析本包全部非测试文件，逐项报告违反源码约定的位置。
func TestSourceRestrictions(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		t.Fatal("no non-test source files found")
	}
	for _, problem := range sourceProblems(fset, files) {
		t.Error(problem)
	}
}

// TestSourceProblemsDetectsViolations 用内存中的源码确认检查本身有效：每一类违规都被报告，合规源码没有报告。
func TestSourceProblemsDetectsViolations(t *testing.T) {
	const header = "package smtp\n\nimport (\n\t\"crypto/tls\"\n\tgosmtp \"github.com/emersion/go-smtp\"\n\t\"github.com/emersion/go-sasl\"\n)\n\n"
	const good = "func tlsConfig() *tls.Config {\n\treturn &tls.Config{ServerName: \"h\", RootCAs: nil, MinVersion: tls.VersionTLS12}\n}\n" +
		"func use() { _, _ = gosmtp.DialTLS(\"\", nil); _ = sasl.NewPlainClient(\"\", \"\", \"\") }\n"
	renamed := strings.Replace(header, "gosmtp \"", "mail \"", 1) + strings.ReplaceAll(good, "gosmtp.", "mail.")
	for _, tc := range []struct {
		name, src string
		want      string // 期望出现在某条报告中的片段；为空表示不应有报告
	}{
		{"compliant", header + good, ""},
		{"compliant with renamed import", renamed, ""},
		{"plaintext dial", header + good + "func bad() { _, _ = gosmtp.Dial(\"\") }\n", "gosmtp.Dial"},
		{"plaintext dial via renamed import", renamed + "func bad() { _, _ = mail.Dial(\"\") }\n", "mail.Dial"},
		{"STARTTLS dial", header + good + "func bad() { _, _ = gosmtp.DialStartTLS(\"\", nil) }\n", "gosmtp.DialStartTLS"},
		{"other sasl client", header + good + "func bad() { _ = sasl.NewLoginClient(\"\", \"\") }\n", "sasl.NewLoginClient"},
		{"key log writer", header + good + "func bad(c *tls.Conn) { c.KeyLogWriter = nil }\n", "KeyLogWriter"},
		{"debug writer", header + good + "func bad(c *gosmtp.Client) { c.DebugWriter = nil }\n", "DebugWriter"},
		{"skip verify", header + strings.Replace(good, "MinVersion:", "InsecureSkipVerify: true, MinVersion:", 1), "InsecureSkipVerify"},
		{"second literal", header + good + "func tlsConfig2() { _ = tls.Config{} }\n", "found 2 tls.Config literals"},
		{"config outside constructor", header + good + "func bad() *tls.Config { return new(tls.Config) }\n", "tls.Config used outside"},
		{"missing MinVersion", header + strings.Replace(good, ", MinVersion: tls.VersionTLS12", "", 1), "MinVersion"},
		{"no literal", header + "func use() { _, _ = gosmtp.DialTLS(\"\", nil) }\n", "found 0 tls.Config literals"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "src.go", tc.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			problems := sourceProblems(fset, []*ast.File{file})
			if tc.want == "" {
				if len(problems) != 0 {
					t.Errorf("unexpected problems: %q", problems)
				}
				return
			}
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tc.want) }) {
				t.Errorf("problems %q do not mention %q", problems, tc.want)
			}
		})
	}
}

// sourceProblems 返回违反源码约定的位置说明：受限模块使用了白名单以外的符号；出现禁用标识符；
// tls.Config 字面量不是恰好一个、不在 tlsConfig 中或字段不是恰为 ServerName、RootCAs、MinVersion；tlsConfig 以外引用了 tls.Config。
func sourceProblems(fset *token.FileSet, files []*ast.File) []string {
	var problems []string
	literals := 0
	for _, file := range files {
		names := importNames(file)
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.Ident:
				if slices.Contains(forbiddenIdents, n.Name) {
					problems = append(problems, fmt.Sprintf("%s: forbidden identifier %s", fset.Position(n.Pos()), n.Name))
				}
			case *ast.SelectorExpr:
				pkg, ok := n.X.(*ast.Ident)
				if !ok {
					break
				}
				if allowed, restricted := allowedImports[names[pkg.Name]]; restricted && !slices.Contains(allowed, n.Sel.Name) {
					problems = append(problems, fmt.Sprintf("%s: %s.%s is not allowed", fset.Position(n.Pos()), pkg.Name, n.Sel.Name))
				}
			}
			return true
		})
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			inConstructor := isFunc && fn.Recv == nil && fn.Name.Name == tlsConfigFunc
			ast.Inspect(decl, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.SelectorExpr:
					if isTLSConfig(n, names) && !inConstructor {
						problems = append(problems, fmt.Sprintf("%s: tls.Config used outside %s", fset.Position(n.Pos()), tlsConfigFunc))
					}
				case *ast.CompositeLit:
					if sel, ok := n.Type.(*ast.SelectorExpr); ok && isTLSConfig(sel, names) {
						literals++
						if keys := literalKeys(n); !slices.Equal(keys, tlsConfigKeys) {
							problems = append(problems, fmt.Sprintf("%s: tls.Config literal sets %q, want exactly %q (MinVersion included)", fset.Position(n.Pos()), keys, tlsConfigKeys))
						}
					}
				}
				return true
			})
		}
	}
	if literals != 1 {
		problems = append(problems, fmt.Sprintf("found %d tls.Config literals, want exactly 1 in %s", literals, tlsConfigFunc))
	}
	return problems
}

// importNames 返回文件中各导入在源码里使用的包名到导入路径的映射。
func importNames(file *ast.File) map[string]string {
	names := map[string]string{}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := defaultNames[path]
		if name == "" {
			name = path[strings.LastIndex(path, "/")+1:]
		}
		if spec.Name != nil {
			name = spec.Name.Name
		}
		names[name] = path
	}
	return names
}

// isTLSConfig 判断选择器是否为 crypto/tls 包的 Config。
func isTLSConfig(sel *ast.SelectorExpr, names map[string]string) bool {
	pkg, ok := sel.X.(*ast.Ident)
	return ok && names[pkg.Name] == "crypto/tls" && sel.Sel.Name == "Config"
}

// literalKeys 返回复合字面量中按名称设置的字段，已排序；无名字段按位置设置时记为空字符串。
func literalKeys(lit *ast.CompositeLit) []string {
	var keys []string
	for _, elt := range lit.Elts {
		key := ""
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if ident, ok := kv.Key.(*ast.Ident); ok {
				key = ident.Name
			}
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
