// Package token 用 go/parser 与 go/types 检查本包非测试源码：Verify 调用常数时间比较函数，
// 任何 ==、!= 的操作数都不是字节数组，也不调用 bytes.Equal。功能测试无法区分常数时间比较与普通比较，这一性质只由本文件钉住。
package token

import (
	"go/ast"
	"go/importer"
	"go/parser"
	gotoken "go/token"
	"go/types"
	"path/filepath"
	"strings"
	"testing"
)

// TestConstantTimeTagComparison 对本包非测试文件做类型检查，逐项报告违反常数时间比较约定的位置。
func TestConstantTimeTagComparison(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := gotoken.NewFileSet()
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
	info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Uses: make(map[*ast.Ident]types.Object)}
	config := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	if _, err := config.Check("github.com/chaoRookie/turncourier/internal/security/token", fset, files, info); err != nil {
		t.Fatalf("type check: %v", err)
	}
	for _, problem := range constantTimeProblems(fset, files, info) {
		t.Error(problem)
	}
}

// constantTimeProblems 返回违反约定的位置说明：Verify 没有调用 hmac.Equal 或 subtle.ConstantTimeCompare；
// ==、!= 的操作数是字节数组、含字节数组的结构体或数组，或是由字节转换得到的字符串；调用了 bytes.Equal 等非常数时间比较函数。
func constantTimeProblems(fset *gotoken.FileSet, files []*ast.File, info *types.Info) []string {
	var problems []string
	verifyCompares := false
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.FuncDecl:
				if n.Recv == nil && n.Name.Name == "Verify" && n.Body != nil {
					verifyCompares = callsConstantTime(n.Body, info)
				}
			case *ast.BinaryExpr:
				if (n.Op == gotoken.EQL || n.Op == gotoken.NEQ) && (secretComparable(n.X, info) || secretComparable(n.Y, info)) {
					problems = append(problems, fset.Position(n.Pos()).String()+": bytes compared with "+n.Op.String())
				}
			case *ast.Ident:
				for _, name := range []string{"bytes.Equal", "bytes.Compare", "slices.Equal", "reflect.DeepEqual"} {
					if isFunc(info.Uses[n], name) {
						problems = append(problems, fset.Position(n.Pos()).String()+": uses "+name)
					}
				}
			}
			return true
		})
	}
	if !verifyCompares {
		problems = append(problems, "Verify does not call hmac.Equal or subtle.ConstantTimeCompare")
	}
	return problems
}

// callsConstantTime 判断函数体中是否调用了 hmac.Equal 或 subtle.ConstantTimeCompare。
func callsConstantTime(body *ast.BlockStmt, info *types.Info) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
			obj := info.Uses[sel.Sel]
			found = found || isFunc(obj, "crypto/hmac.Equal") || isFunc(obj, "crypto/subtle.ConstantTimeCompare")
		}
		return true
	})
	return found
}

// isFunc 判断 obj 是否为全名（包路径.函数名）为 name 的包级函数。
func isFunc(obj types.Object, name string) bool {
	fn, ok := obj.(*types.Func)
	return ok && fn.Pkg() != nil && fn.Pkg().Path()+"."+fn.Name() == name
}

// secretComparable 判断比较运算的操作数是否可能是标签字节：类型含字节数组，或是由字节切片、字节数组转换得到的字符串。
func secretComparable(expr ast.Expr, info *types.Info) bool {
	if holdsByteArray(info.TypeOf(expr)) {
		return true
	}
	call, ok := ast.Unparen(expr).(*ast.CallExpr)
	if !ok || len(call.Args) != 1 || !info.Types[call.Fun].IsType() {
		return false
	}
	switch arg := info.TypeOf(call.Args[0]).Underlying().(type) {
	case *types.Slice:
		return isByte(arg.Elem())
	case *types.Array:
		return isByte(arg.Elem())
	}
	return false
}

// holdsByteArray 判断类型是否为字节数组，或是含字节数组的结构体与数组；这类值的 == 比较不是常数时间的。
func holdsByteArray(typ types.Type) bool {
	if typ == nil {
		return false
	}
	switch u := typ.Underlying().(type) {
	case *types.Array:
		return isByte(u.Elem()) || holdsByteArray(u.Elem())
	case *types.Struct:
		for i := range u.NumFields() {
			if holdsByteArray(u.Field(i).Type()) {
				return true
			}
		}
	}
	return false
}

// isByte 判断类型的底层类型是否为 byte（uint8）。
func isByte(typ types.Type) bool {
	basic, ok := typ.Underlying().(*types.Basic)
	return ok && basic.Kind() == types.Uint8
}
