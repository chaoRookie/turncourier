// 本文件守卫 `docs/zh-CN/plans/phase-04.md` 4b「已定的实现细节」中「主题标签的类型」一行：返回令牌明文的方法
// （Token.Reveal、Tag.Reveal 与 Tag.RevealToken）在产品代码中只能由 internal/security/token 与 internal/mail/renderer 引用。
// 这里用 go/parser 完整解析 cmd/ 与 internal/ 下全部非测试 .go 文件（不看构建约束，也不做类型检查），按名称判断：
// 凡是选择子名为 Reveal 或 RevealToken 的表达式都算，调用、方法值（f := tag.Reveal）与方法表达式一样，别的类型上的同名方法
// 也在禁止之列。不联网、不访问钥匙串、不执行子进程。
package docs_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
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
	fset := token.NewFileSet()
	scanned := make(map[string]bool)
	for _, root := range productRoots {
		err := filepath.WalkDir(filepath.Join(repoRoot, root), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := entry.Name()
			// 与 go 工具一致：以「.」或「_」开头的目录与文件、testdata 目录都不参与构建。
			ignored := strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
			if entry.IsDir() {
				if ignored || name == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if ignored || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			dir, err := filepath.Rel(repoRoot, filepath.Dir(path))
			if err != nil {
				return err
			}
			pkg := filepath.ToSlash(dir)
			scanned[pkg] = true
			if slices.Contains(revealPackages, pkg) {
				return nil
			}
			for _, ident := range revealSelectors(file) {
				t.Errorf("%s 引用了 %s：清单要求产品代码只在 %s 中引用返回令牌明文的方法",
					fset.Position(ident.Pos()), ident.Name, strings.Join(revealPackages, " 与 "))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("扫描 %s 失败：%v", root, err)
		}
	}
	for _, pkg := range []string{"cmd/turncourier", "internal/cli", "internal/security/token"} {
		if !scanned[pkg] {
			t.Errorf("没有扫描到 %s：检查的范围不对", pkg)
		}
	}
}

// TestRevealSelectorsDetector 用一段合成源码确认检测器认得调用、方法值与方法表达式，不把方法声明、接口方法与名字相近的
// 选择子算进去；仓库中暂时没有违规时，这条用例保证 TestRevealReferences 不是空转。
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
