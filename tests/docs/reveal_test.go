// 本文件守卫 `docs/zh-CN/plans/phase-04.md` 4b「已定的实现细节」中「主题标签的类型」一行：返回令牌明文的方法
// （Token.Reveal、Tag.Reveal 与 Tag.RevealToken）在产品代码中只能由 internal/security/token 与 internal/mail/renderer 引用。
// 这里用 go/parser 完整解析 cmd/ 与 internal/ 下全部非测试 .go 文件（不看构建约束，也不做类型检查），按名称判断：
// 凡是选择子名为 Reveal 或 RevealToken 的表达式都算，调用、方法值（f := tag.Reveal）与方法表达式一样，别的类型上的同名方法
// 也在禁止之列。以「.」「_」开头的目录与 testdata 目录同样扫描：它们只是不参与 ./... 通配，被显式导入时照样编译进产品二进制；
// 以「.」「_」开头的文件被 go/build 忽略，不扫描。按名字的反射调用（reflect 的 MethodByName）与模板中的字段访问
// （{{.Reveal}}）不经过选择子，这里查不出来，仍靠代码审查。不联网、不访问钥匙串、不执行子进程。
package docs_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// revealMethods 是返回令牌明文的方法名。
var revealMethods = []string{"Reveal", "RevealToken"}

// revealPackages 是产品代码中允许引用这些方法的包目录：令牌包自身（标签由 Token.Reveal 拼出）与写入主题和页脚副本的渲染器。
var revealPackages = []string{"internal/security/token", "internal/mail/renderer"}

// TestRevealReferences 断言 cmd/ 与 internal/ 的非测试源码中，引用 Reveal 或 RevealToken 的表达式只出现在允许的包里；
// 另外确认扫描确实走到了这几个目录，免得路径写错时检查空转。
func TestRevealReferences(t *testing.T) {
	violations, scanned, err := revealViolations(repoRoot, productRoots)
	if err != nil {
		t.Fatalf("扫描失败：%v", err)
	}
	for _, v := range violations {
		t.Errorf("%s：清单要求产品代码只在 %s 中引用返回令牌明文的方法", v, strings.Join(revealPackages, " 与 "))
	}
	for _, pkg := range []string{"cmd/turncourier", "internal/cli", "internal/security/token"} {
		if !scanned[pkg] {
			t.Errorf("没有扫描到 %s：检查的范围不对", pkg)
		}
	}
}

// TestRevealViolationsTree 在临时目录中搭一棵合成源码树，钉住遍历与允许列表的语义：允许列表按包目录精确匹配
// （子目录不继承），构建约束不豁免，以「.」「_」开头的目录与 testdata 目录照常扫描（显式导入时会编译进产品），
// 测试文件与以「.」「_」开头的文件不计入（其中一个不是合法的 Go 源码，被读就会解析失败），解析失败的文件让扫描返回错误；
// 允许列表本身与契约逐字相同。
func TestRevealViolationsTree(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"internal/security/token/a.go":     "package token\n\nfunc a(x interface{ Reveal() string }) string { return x.Reveal() }\n",
		"internal/mail/renderer/a.go":      "package renderer\n\nfunc a(x interface{ RevealToken() string }) string { return x.RevealToken() }\n",
		"internal/security/token/sub/a.go": "package sub\n\nfunc a(x interface{ Reveal() string }) string { return x.Reveal() }\n",
		"internal/cli/a.go":                "package cli\n\nfunc a(x interface{ Reveal() string }) func() string { return x.Reveal }\n",
		"internal/cli/b_darwin.go":         "//go:build darwin\n\npackage cli\n\nfunc b(x interface{ RevealToken() string }) string { return x.RevealToken() }\n",
		"internal/cli/a_test.go":           "package cli\n\nfunc c(x interface{ Reveal() string }) string { return x.Reveal() }\n",
		"internal/cli/_skip.go":            "package cli\n\nfunc d(x interface{ Reveal() string }) string { return x.Reveal() }\n",
		"internal/cli/.skip.go":            "this is not Go\n",
		"internal/cli/testdata/a.go":       "package testdata\n\nfunc a(x interface{ Reveal() string }) string { return x.Reveal() }\n",
		"internal/_x/a.go":                 "package x\n\nfunc a(x interface{ Reveal() string }) string { return x.Reveal() }\n",
		"internal/.y/a.go":                 "package y\n\nfunc a(x interface{ RevealToken() string }) string { return x.RevealToken() }\n",
		"cmd/turncourier/main.go":          "package main\n\nfunc main() {}\n",
	}
	for rel, src := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, _, err := revealViolations(root, productRoots)
	if err != nil {
		t.Fatalf("扫描合成源码树失败：%v", err)
	}
	slices.Sort(got)
	want := []string{
		"internal/.y/a.go:3 RevealToken",
		"internal/_x/a.go:3 Reveal",
		"internal/cli/a.go:3 Reveal",
		"internal/cli/b_darwin.go:5 RevealToken",
		"internal/cli/testdata/a.go:3 Reveal",
		"internal/security/token/sub/a.go:3 Reveal",
	}
	if !slices.Equal(got, want) {
		t.Errorf("检测到 %v，期望 %v", got, want)
	}
	if want := []string{"internal/security/token", "internal/mail/renderer"}; !slices.Equal(revealPackages, want) {
		t.Errorf("允许列表为 %v，契约只允许 %v", revealPackages, want)
	}
	// 解析失败必须让扫描返回错误，而不是静默跳过这个文件。
	if err := os.WriteFile(filepath.Join(root, "internal", "cli", "bad.go"), []byte("package cli\n\nfunc (\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := revealViolations(root, productRoots); err == nil {
		t.Error("解析失败的文件没有让扫描返回错误")
	}
}

// revealViolations 扫描 root 下 roots 各目录中的全部非测试 .go 文件，返回允许的包之外引用 Reveal 或 RevealToken 的位置
// （相对 root 的斜杠路径、行号与方法名），以及扫描到的包目录。目录一律进入；只跳过以「.」「_」开头的文件
// （go/build 忽略它们）、非 .go 文件与测试文件。解析失败即返回错误，不静默跳过。
func revealViolations(root string, roots []string) ([]string, map[string]bool, error) {
	fset := token.NewFileSet()
	scanned := make(map[string]bool)
	var violations []string
	for _, top := range roots {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := entry.Name()
			if entry.IsDir() {
				// 以「.」「_」开头的目录与 testdata 只是不参与 ./... 通配，被显式导入时照样编译，因此照常进入。
				return nil
			}
			ignored := strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") // go/build 忽略这样的文件
			if ignored || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			dir, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			pkg := filepath.ToSlash(dir)
			scanned[pkg] = true
			if slices.Contains(revealPackages, pkg) {
				return nil
			}
			for _, ident := range revealSelectors(file) {
				line := fset.Position(ident.Pos()).Line
				violations = append(violations, fmt.Sprintf("%s/%s:%d %s", pkg, name, line, ident.Name))
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	return violations, scanned, nil
}

// TestRevealSelectorsDetector 用一段合成源码确认检测器认得调用、方法值与方法表达式，不把方法声明、接口方法与名字相近的
// 选择子算进去。它只覆盖语法树匹配这一步；遍历与允许列表由 TestRevealViolationsTree 覆盖。
func TestRevealSelectorsDetector(t *testing.T) {
	const src = `package p

type tag struct{}

func (tag) Reveal() string      { return "" }
func (tag) RevealToken() string { return "" }
func (tag) Revealed() string    { return "" }

type revealer interface{ Reveal() string }

func use(g tag, r revealer) {
	_ = g.Reveal()
	f := g.RevealToken
	_ = f
	_ = tag.Reveal
	_ = r.Reveal()
	_ = g.Revealed()
}
`
	file, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("解析合成源码失败：%v", err)
	}
	var names []string
	for _, ident := range revealSelectors(file) {
		names = append(names, ident.Name)
	}
	if want := []string{"Reveal", "RevealToken", "Reveal", "Reveal"}; !slices.Equal(names, want) {
		t.Errorf("检测到 %v，期望 %v", names, want)
	}
}

// revealSelectors 返回文件中名为 Reveal 或 RevealToken 的选择子：调用 x.Reveal()、方法值 f := x.Reveal 与方法表达式
// T.Reveal 在语法树中都是 *ast.SelectorExpr；方法与接口方法的声明不是选择子，不在其列。
func revealSelectors(file *ast.File) []*ast.Ident {
	var found []*ast.Ident
	ast.Inspect(file, func(node ast.Node) bool {
		if sel, ok := node.(*ast.SelectorExpr); ok && slices.Contains(revealMethods, sel.Sel.Name) {
			found = append(found, sel.Sel)
		}
		return true
	})
	return found
}
