// Package imap 用 go/parser 解析、go/types 类型检查本包非测试源码，按白名单钉住功能测试无法证明的否定性质：
// imapclient 的函数只用 DialTLS；*imapclient.Client 只调用白名单方法；EXAMINE 只以只读选项发出；FETCH 只以 UID 集发出且正文项都带 Peek；
// 不出现调试输出与放宽证书校验的标识符；tls.Config 只在 tlsConfig 中构造一次。
package imap

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// 源码检查涉及的导入路径与本包路径。
const (
	imapPath       = "github.com/emersion/go-imap/v2"
	imapclientPath = "github.com/emersion/go-imap/v2/imapclient"
	packagePath    = "github.com/chaoRookie/turncourier/internal/mail/imap"
	tlsConfigFunc  = "tlsConfig" // 唯一允许构造 tls.Config 的函数
)

// 源码检查的白名单与禁用标识符。
var (
	// allowedClientMethods 是接收者为 *imapclient.Client 时允许调用或取方法值的方法。
	allowedClientMethods = []string{"Capability", "Caps", "WaitGreeting", "Login", "List", "Select", "UIDSearch", "Fetch", "Idle", "Logout", "Close", "Closed", "Mailbox"}
	// forbiddenIdents 是任何非测试文件都不得出现的标识符：调试输出会写出凭据，其余会放宽或绕过证书校验、泄露会话密钥。
	forbiddenIdents = []string{"DebugWriter", "InsecureSkipVerify", "KeyLogWriter", "VerifyPeerCertificate", "VerifyConnection"}
	// tlsConfigKeys 是 tls.Config 字面量允许且必须设置的字段。
	tlsConfigKeys = []string{"MinVersion", "RootCAs", "ServerName"}
)

// sourceImporter 从源码类型检查依赖包；各用例共用一个实例以复用已检查的包。
var sourceImporter = sync.OnceValue(func() types.Importer {
	return importer.ForCompiler(token.NewFileSet(), "source", nil)
})

// TestSourceRestrictions 解析并类型检查本包全部非测试文件，逐项报告违反源码约定的位置。
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
	problems, err := sourceProblems(fset, files)
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range problems {
		t.Error(problem)
	}
}

// TestSourceProblemsDetectsViolations 用内存中的源码确认检查本身有效：每一类违规都被报告，合规源码没有报告。
func TestSourceProblemsDetectsViolations(t *testing.T) {
	const header = "package imap\n\nimport (\n\t\"crypto/tls\"\n\n\t\"github.com/emersion/go-imap/v2\"\n\t\"github.com/emersion/go-imap/v2/imapclient\"\n)\n\n"
	const good = "func tlsConfig() *tls.Config {\n\treturn &tls.Config{ServerName: \"h\", RootCAs: nil, MinVersion: tls.VersionTLS12}\n}\n\n" +
		"func use(c *imapclient.Client) {\n" +
		"\t_, _ = imapclient.DialTLS(\"\", &imapclient.Options{TLSConfig: tlsConfig()})\n" +
		"\t_ = c.Select(\"INBOX\", &imap.SelectOptions{ReadOnly: true})\n" +
		"\t_ = c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{RFC822Size: true, BodySection: []*imap.FetchItemBodySection{{Peek: true}}})\n" +
		"\t<-c.Closed()\n" +
		"}\n"
	for _, tc := range []struct {
		name, src string
		want      string // 期望出现在某条报告中的片段；为空表示不应有报告
	}{
		{"compliant", header + good, ""},
		{"UnselectAndExpunge", header + good + "func bad(c *imapclient.Client) { _ = c.UnselectAndExpunge() }\n", "UnselectAndExpunge"},
		{"Noop", header + good + "func bad(c *imapclient.Client) { _ = c.Noop() }\n", "Noop"},
		{"Move", header + good + "func bad(c *imapclient.Client) { _ = c.Move(imap.UIDSetNum(1), \"Junk\") }\n", "Move"},
		{"method value", header + good + "func bad(c *imapclient.Client) { f := c.Store; _ = f }\n", "Store"},
		{"method expression", header + good + "func bad(c *imapclient.Client) { _ = (*imapclient.Client).Expunge(c) }\n", "Expunge"},
		{"promoted method", header + good + "type wrapped struct{ *imapclient.Client }\n\nfunc bad(w wrapped) { _ = w.UnselectAndExpunge() }\n", "UnselectAndExpunge"},
		{"promoted Select", header + good + "type wrapped struct{ *imapclient.Client }\n\nfunc bad(w wrapped) { _ = w.Select(\"INBOX\", nil) }\n", "read-only"},
		{"interface method", header + good + "func bad(c *imapclient.Client) {\n\tvar x interface{ UnselectAndExpunge() *imapclient.Command } = c\n\t_ = x.UnselectAndExpunge()\n}\n", "UnselectAndExpunge"},
		{"ReadOnly false", header + good + "func bad(c *imapclient.Client) { _ = c.Select(\"INBOX\", &imap.SelectOptions{ReadOnly: false}) }\n", "read-only"},
		{"nil select options", header + good + "func bad(c *imapclient.Client) { _ = c.Select(\"INBOX\", nil) }\n", "read-only"},
		{"select options variable", header + good + "func bad(c *imapclient.Client, o *imap.SelectOptions) { _ = c.Select(\"INBOX\", o) }\n", "read-only"},
		{"Select method value", header + good + "func bad(c *imapclient.Client) { f := c.Select; _ = f }\n", "must be called directly"},
		{"body section without Peek", header + good + "func bad() { _ = &imap.FetchItemBodySection{} }\n", "Peek"},
		{"body section with Peek false", header + good + "func bad() { _ = imap.FetchItemBodySection{Peek: false} }\n", "Peek"},
		{"binary section without Peek", header + good + "func bad() { _ = []*imap.FetchItemBinarySection{{}} }\n", "Peek"},
		{"fetch with a sequence set", header + good + "func bad(c *imapclient.Client) { _ = c.Fetch(imap.SeqSetNum(1), nil) }\n", "imap.UIDSet"},
		{"fetch with a NumSet", header + good + "func bad(c *imapclient.Client, s imap.NumSet) { _ = c.Fetch(s, nil) }\n", "imap.UIDSet"},
		{"plaintext dial", header + good + "func bad() { _, _ = imapclient.DialInsecure(\"\", nil) }\n", "DialInsecure"},
		{"STARTTLS dial", header + good + "func bad() { _, _ = imapclient.DialStartTLS(\"\", nil) }\n", "DialStartTLS"},
		{"dot import", strings.Replace(header, "\t\"github.com/emersion/go-imap/v2/imapclient\"", "\t. \"github.com/emersion/go-imap/v2/imapclient\"", 1) +
			strings.ReplaceAll(good, "imapclient.", "") + "func bad() { _ = New(nil, nil) }\n", "dot import"},
		{"debug writer", header + good + "func bad() { _ = imapclient.Options{DebugWriter: nil} }\n", "DebugWriter"},
		{"skip verify", header + strings.Replace(good, "MinVersion:", "InsecureSkipVerify: true, MinVersion:", 1), "InsecureSkipVerify"},
		{"key log writer", header + good + "func bad(c *tls.Config) { c.KeyLogWriter = nil }\n", "KeyLogWriter"},
		{"second literal", header + good + "func tlsConfig2() { _ = tls.Config{} }\n", "found 2 tls.Config literals"},
		{"config outside constructor", header + good + "func bad() *tls.Config { return new(tls.Config) }\n", "tls.Config used outside"},
		{"missing MinVersion", header + strings.Replace(good, ", MinVersion: tls.VersionTLS12", "", 1), "MinVersion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "src.go", tc.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			problems, err := sourceProblems(fset, []*ast.File{file})
			if err != nil {
				t.Fatal(err)
			}
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

// sourceProblems 类型检查 files 并返回违反源码约定的位置说明（已排序）：任何点导入；imapclient 中 DialTLS 以外的函数；
// *imapclient.Client 白名单以外的方法（含经嵌入提升的方法与同名同签名的接口方法）；Select 与 Fetch 不是直接调用；
// Select 的选项不是带 ReadOnly: true 的 imap.SelectOptions 字面量；Fetch 的第一个参数静态类型不是 imap.UIDSet；
// 正文或二进制数据项字面量没有 Peek: true；出现禁用标识符；tls.Config 字面量不是恰好一个、不在 tlsConfig 中
// 或字段不是恰为 ServerName、RootCAs、MinVersion；tlsConfig 以外引用了 tls.Config。
func sourceProblems(fset *token.FileSet, files []*ast.File) ([]string, error) {
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	conf := types.Config{Importer: sourceImporter()}
	if _, err := conf.Check(packagePath, fset, files, info); err != nil {
		return nil, err
	}
	clientPkg, err := sourceImporter().Import(imapclientPath)
	if err != nil {
		return nil, err
	}
	client := types.NewMethodSet(types.NewPointer(clientPkg.Scope().Lookup("Client").Type()))
	var problems []string
	report := func(pos token.Pos, format string, args ...any) {
		problems = append(problems, fmt.Sprintf("%s: %s", fset.Position(pos), fmt.Sprintf(format, args...)))
	}
	literals := 0
	for _, file := range files {
		for _, spec := range file.Imports {
			if spec.Name != nil && spec.Name.Name == "." {
				report(spec.Pos(), "dot import %s is not allowed", spec.Path.Value)
			}
		}
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			inConstructor := isFunc && fn.Recv == nil && fn.Name.Name == tlsConfigFunc
			called := map[*ast.SelectorExpr]bool{}
			ast.Inspect(decl, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.Ident:
					if slices.Contains(forbiddenIdents, n.Name) {
						report(n.Pos(), "forbidden identifier %s", n.Name)
					}
					switch obj := info.Uses[n].(type) {
					case *types.Func:
						if obj.Pkg() != nil && obj.Pkg().Path() == imapclientPath && obj.Signature().Recv() == nil && obj.Name() != "DialTLS" {
							report(n.Pos(), "imapclient.%s is not allowed; only DialTLS", obj.Name())
						}
					case *types.TypeName:
						if isNamed(obj.Type(), "crypto/tls", "Config") && !inConstructor {
							report(n.Pos(), "tls.Config used outside %s", tlsConfigFunc)
						}
					}
				case *ast.CallExpr:
					sel, ok := n.Fun.(*ast.SelectorExpr)
					if !ok || !isClientMethod(info, client, sel) {
						break
					}
					called[sel] = true
					switch sel.Sel.Name {
					case "Select":
						if len(n.Args) != 2 || !isReadOnlySelect(info, n.Args[1]) {
							report(n.Pos(), "Select must pass a read-only &imap.SelectOptions{ReadOnly: true} literal")
						}
					case "Fetch":
						if len(n.Args) == 0 || !isNamed(info.TypeOf(n.Args[0]), imapPath, "UIDSet") {
							report(n.Pos(), "Fetch must take an imap.UIDSet (a sequence set sends a non-UID FETCH)")
						}
					}
				case *ast.SelectorExpr:
					if !isClientMethod(info, client, n) {
						break
					}
					if !slices.Contains(allowedClientMethods, n.Sel.Name) {
						report(n.Pos(), "(*imapclient.Client).%s is not allowed", n.Sel.Name)
					}
					if (n.Sel.Name == "Select" || n.Sel.Name == "Fetch") && !called[n] {
						report(n.Pos(), "(*imapclient.Client).%s must be called directly so its arguments can be checked", n.Sel.Name)
					}
				case *ast.CompositeLit:
					typ := deref(info.TypeOf(n))
					switch {
					case isNamed(typ, imapPath, "FetchItemBodySection"), isNamed(typ, imapPath, "FetchItemBinarySection"):
						if !hasTrueField(info, n, "Peek") {
							report(n.Pos(), "%s literal without Peek: true would set \\Seen", types.TypeString(typ, nil))
						}
					case isNamed(typ, "crypto/tls", "Config"):
						literals++
						if !inConstructor {
							report(n.Pos(), "tls.Config used outside %s", tlsConfigFunc)
						}
						if keys := literalKeys(n); !slices.Equal(keys, tlsConfigKeys) {
							report(n.Pos(), "tls.Config literal sets %q, want exactly %q (MinVersion included)", keys, tlsConfigKeys)
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
	slices.Sort(problems)
	return problems, nil
}

// isClientMethod 判断选择器取的是否为 *imapclient.Client 的方法（方法值、方法调用或方法表达式）。按方法的声明接收者判断，
// 经嵌入字段提升的方法因此也算；接口方法与 client（*imapclient.Client 的方法集）中某个方法同名同签名时同样算，
// 因为 Client 或嵌入它的类型赋给该接口后，经接口调用发出的是同一条命令。
func isClientMethod(info *types.Info, client *types.MethodSet, sel *ast.SelectorExpr) bool {
	selection, ok := info.Selections[sel]
	if !ok || selection.Kind() == types.FieldVal {
		return false
	}
	fn := selection.Obj().(*types.Func)
	recv := fn.Signature().Recv().Type()
	if isNamed(deref(recv), imapclientPath, "Client") {
		return true
	}
	m := client.Lookup(nil, fn.Name())
	return types.IsInterface(recv) && m != nil && types.Identical(m.Obj().Type(), fn.Type())
}

// isReadOnlySelect 判断表达式是否为带 ReadOnly: true 的 &imap.SelectOptions{…} 字面量。
func isReadOnlySelect(info *types.Info, expr ast.Expr) bool {
	unary, ok := expr.(*ast.UnaryExpr)
	if !ok || unary.Op != token.AND {
		return false
	}
	lit, ok := unary.X.(*ast.CompositeLit)
	return ok && isNamed(info.TypeOf(lit), imapPath, "SelectOptions") && hasTrueField(info, lit, "ReadOnly")
}

// hasTrueField 判断复合字面量是否以常量 true 设置了字段 name。
func hasTrueField(info *types.Info, lit *ast.CompositeLit, name string) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == name {
			value := info.Types[kv.Value].Value
			return value != nil && value.Kind() == constant.Bool && constant.BoolVal(value)
		}
	}
	return false
}

// isNamed 判断 typ 是否为 pkg 包中名为 name 的具名类型。
func isNamed(typ types.Type, pkg, name string) bool {
	named, ok := typ.(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == pkg && named.Obj().Name() == name
}

// deref 去掉一层指针。
func deref(typ types.Type) types.Type {
	if ptr, ok := typ.(*types.Pointer); ok {
		return ptr.Elem()
	}
	return typ
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
