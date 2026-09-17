// Package main 检查手写 Go 包、类型和具名函数的中文文档注释。
package main

import (
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// diagnostic 记录一个缺少中文文档的源码位置与对象。
type diagnostic struct {
	Position token.Position
	Message  string
}

// packageDocs 汇集同目录同包的文档，允许统一写在 doc.go 中。
// 包内有非测试文件时只采信非测试文件的说明，与 go doc 一致；只有测试文件的包沿用测试文件说明。
type packageDocs struct {
	Position    token.Position
	Name        string
	HasDocs     bool
	HasNonTest  bool
	NonTestDocs bool
}

// main 将命令行参数交给可测试入口并设置退出码。
func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 检查指定目录，参数错误返回 2，检查失败或未找到手写 Go 文件返回 1。
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("commentcheck", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "用法: commentcheck [源码目录，默认当前目录]")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 1 {
		flags.Usage()
		return 2
	}
	root := "."
	if flags.NArg() == 1 {
		root = flags.Arg(0)
	}
	diagnostics, files, err := check(root)
	if err != nil {
		fmt.Fprintf(stderr, "commentcheck: %v\n", err)
		return 1
	}
	for _, item := range diagnostics {
		fmt.Fprintf(stderr, "%s: %s\n", item.Position, item.Message)
	}
	if len(diagnostics) > 0 {
		return 1
	}
	if files == 0 {
		fmt.Fprintf(stderr, "commentcheck: 未找到手写 Go 文件: %s\n", root)
		return 1
	}
	fmt.Fprintf(stdout, "中文注释检查通过（%d 个手写 Go 文件）\n", files)
	return 0
}

// check 遍历源码目录；生成文件和 go 工具忽略的目录不参与检查。
// 与 go 工具一致，以「.」或「_」开头的文件不会被编译，先行跳过（如编辑器锁文件）；
// 其余符号链接形式的 Go 源码会被编译却不在检查范围内，因此直接报错而不静默跳过。
// 包说明按目录和包名汇集，因此外部测试包需要自己的中文包说明。
func check(root string) ([]diagnostic, int, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, 0, err
	}
	if !info.IsDir() {
		return nil, 0, fmt.Errorf("源码根路径不是目录: %s", root)
	}
	fset := token.NewFileSet()
	packages := make(map[string]*packageDocs)
	var diagnostics []diagnostic
	files := 0
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && skippedDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasPrefix(entry.Name(), ".") || strings.HasPrefix(entry.Name(), "_") {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("不支持符号链接形式的 Go 源码: %s", path)
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil {
			return parseErr
		}
		if ast.IsGenerated(file) {
			return nil
		}
		files++
		key := filepath.Dir(path) + "\x00" + file.Name.Name
		pkg := packages[key]
		if pkg == nil {
			pkg = &packageDocs{Position: fset.Position(file.Name.Pos()), Name: file.Name.Name}
			packages[key] = pkg
		}
		pkg.HasDocs = pkg.HasDocs || hasChinese(file.Doc)
		if !strings.HasSuffix(path, "_test.go") {
			if !pkg.HasNonTest {
				pkg.Position = fset.Position(file.Name.Pos())
				pkg.HasNonTest = true
			}
			pkg.NonTestDocs = pkg.NonTestDocs || hasChinese(file.Doc)
		}
		diagnostics = append(diagnostics, declarationDiagnostics(fset, file)...)
		return nil
	})
	if err != nil {
		return nil, files, err
	}
	for _, pkg := range packages {
		if !pkg.HasDocs || (pkg.HasNonTest && !pkg.NonTestDocs) {
			diagnostics = append(diagnostics, diagnostic{pkg.Position, "包 " + pkg.Name + " 缺少中文包级文档"})
		}
	}
	sort.Slice(diagnostics, func(i, j int) bool {
		left, right := diagnostics[i], diagnostics[j]
		if left.Position.Filename != right.Position.Filename {
			return left.Position.Filename < right.Position.Filename
		}
		if left.Position.Offset != right.Position.Offset {
			return left.Position.Offset < right.Position.Offset
		}
		return left.Message < right.Message
	})
	return diagnostics, files, nil
}

// declarationDiagnostics 检查顶层具名函数及所有类型声明（含函数体内的局部类型），不要求匿名回调单独注释。
func declarationDiagnostics(fset *token.FileSet, file *ast.File) []diagnostic {
	var result []diagnostic
	for _, decl := range file.Decls {
		if item, ok := decl.(*ast.FuncDecl); ok && !hasChinese(item.Doc) {
			result = append(result, diagnostic{fset.Position(item.Name.Pos()), "函数 " + item.Name.Name + " 缺少中文文档注释"})
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		item, ok := node.(*ast.GenDecl)
		if !ok || item.Tok != token.TYPE {
			return true
		}
		for _, spec := range item.Specs {
			typ := spec.(*ast.TypeSpec)
			doc := typ.Doc
			if doc == nil && len(item.Specs) == 1 {
				doc = item.Doc
			}
			if !hasChinese(doc) {
				result = append(result, diagnostic{fset.Position(typ.Name.Pos()), "类型 " + typ.Name.Name + " 缺少中文文档注释"})
			}
		}
		return true
	})
	return result
}

// hasChinese 判断文档是否含汉字；注释质量仍需人工评审。
func hasChinese(comment *ast.CommentGroup) bool {
	return comment != nil && strings.ContainsFunc(comment.Text(), func(r rune) bool {
		return unicode.Is(unicode.Han, r)
	})
}

// skippedDirectory 按 go 工具规则识别不参与构建的非根目录：以「.」或「_」开头的目录、vendor 和 testdata。
func skippedDirectory(name string) bool {
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return true
	}
	switch name {
	case "vendor", "testdata":
		return true
	default:
		return false
	}
}
