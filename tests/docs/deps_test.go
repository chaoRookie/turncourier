// Package docs_test 以 `go.mod` 的直接依赖和各包的实际导入为准，核对两份文档中的第三方依赖表：
// 每个直接依赖都要有一行、版本要对得上，并且每个直接导入该模块的仓库内包都要在「使用方」一栏中点名。
// 本文件只读取仓库里的文本，不联网、不访问钥匙串、不执行子进程。
package docs_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// dependencyTables 是含第三方依赖表的文档，路径相对仓库根目录。
var dependencyTables = []string{"docs/en/architecture.md", "docs/zh-CN/development.md"}

// repoRoot 是仓库根目录相对本包的路径。
const repoRoot = "../.."

// docRow 保存依赖表中一行里本测试要用到的两栏。
type docRow struct {
	version string
	usedBy  string
}

// TestDependencyTablesMatchCode 检查两份依赖表覆盖 go.mod 的全部直接依赖，版本一致，
// 且每个直接导入该模块的仓库内包都在「使用方」一栏中出现。
func TestDependencyTablesMatchCode(t *testing.T) {
	direct := directRequires(t, filepath.Join(repoRoot, "go.mod"))
	if len(direct) == 0 {
		t.Fatal("go.mod 中没有解析到直接依赖")
	}
	importers := moduleImporters(t, direct)
	for _, doc := range dependencyTables {
		rows := dependencyRows(t, filepath.Join(repoRoot, doc))
		for module, version := range direct {
			row, ok := rows[module]
			if !ok {
				t.Errorf("%s: 依赖表缺少 %s 一行", doc, module)
				continue
			}
			if !versionMatches(row.version, version) {
				t.Errorf("%s: %s 的版本栏为 %q，go.mod 中是 %s", doc, module, row.version, version)
			}
			for _, pkg := range importers[module] {
				if !strings.Contains(row.usedBy, "`"+pkg+"`") {
					t.Errorf("%s: %s 的使用方一栏没有点名直接导入它的 `%s`", doc, module, pkg)
				}
			}
		}
	}
}

// directRequires 读取 go.mod 的 require 块，返回不带 `// indirect` 标记的模块及其版本。
func directRequires(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 go.mod 失败：%v", err)
	}
	direct := make(map[string]string)
	inBlock := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "require (":
			inBlock = true
		case inBlock && line == ")":
			inBlock = false
		case inBlock && line != "" && !strings.HasPrefix(line, "//") && !strings.Contains(line, "// indirect"):
			if fields := strings.Fields(line); len(fields) >= 2 {
				direct[fields[0]] = fields[1]
			}
		}
	}
	return direct
}

// moduleImporters 遍历仓库中的非测试 Go 文件，返回每个直接依赖被哪些仓库内包目录直接导入；
// 目录以仓库根目录为基准、用斜杠分隔，与文档中的写法一致。
func moduleImporters(t *testing.T, direct map[string]string) map[string][]string {
	t.Helper()
	importers := make(map[string][]string)
	fset := token.NewFileSet()
	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != repoRoot && (strings.HasPrefix(name, ".") || name == "dist") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		dir, err := filepath.Rel(repoRoot, filepath.Dir(path))
		if err != nil {
			return err
		}
		pkg := filepath.ToSlash(dir)
		for _, spec := range file.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if module := owningModule(imported, direct); module != "" && !slices.Contains(importers[module], pkg) {
				importers[module] = append(importers[module], pkg)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 Go 源码失败：%v", err)
	}
	for module := range importers {
		slices.Sort(importers[module])
	}
	return importers
}

// owningModule 返回导入路径所属的直接依赖模块；取最长的匹配前缀，没有匹配时返回空串。
func owningModule(imported string, direct map[string]string) string {
	best := ""
	for module := range direct {
		if imported != module && !strings.HasPrefix(imported, module+"/") {
			continue
		}
		if len(module) > len(best) {
			best = module
		}
	}
	return best
}

// dependencyRows 解析文档中所有四栏、首栏为单个反引号标识符的表行，返回模块到该行的映射。
func dependencyRows(t *testing.T, path string) map[string]docRow {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", path, err)
	}
	rows := make(map[string]docRow)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 6 {
			continue
		}
		cells := make([]string, 4)
		for i := range cells {
			cells[i] = strings.TrimSpace(parts[i+1])
		}
		name, ok := strings.CutPrefix(cells[0], "`")
		if !ok {
			continue
		}
		name, ok = strings.CutSuffix(name, "`")
		if !ok || strings.Contains(name, "`") {
			continue
		}
		rows[name] = docRow{version: cells[1], usedBy: cells[3]}
	}
	return rows
}

// versionMatches 判断版本栏是否写明了 go.mod 中的版本；伪版本在文档中只写后 12 位，因此按子串比对。
func versionMatches(cell, version string) bool {
	for _, field := range strings.Fields(strings.ReplaceAll(cell, "`", " ")) {
		if len(field) >= 5 && strings.Contains(version, field) {
			return true
		}
	}
	return false
}
