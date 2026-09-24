// 本文件守卫 `docs/zh-CN/plans/phase-04.md` 的「依赖方向（4a 结束时）」：那是跨包的架构不变量，
// 单个包的测试看不见它被谁导入，只有整仓扫描能拦住。这里用 go/parser 以 ImportsOnly 只读解析仓库中
// 全部非测试 .go 文件，不联网、不访问钥匙串、不执行子进程。
package docs_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// modulePath 是本仓库的模块路径，用来把导入路径换算成仓库内的包目录。
const modulePath = "github.com/chaoRookie/turncourier"

// layerRules 是清单列出的跨包禁令：pkg 目录（含子目录）下的包不得导入 banned 中的任一目录。
// `internal/mail/*` 只收发字节，游标与密码由调用方传入；`store/sqlite` 只依赖 `security/payload`，
// 不依赖令牌与配置（通知 ID 以 [12]byte 保存，域名与有效期由调用方传入）。
var layerRules = []struct {
	pkg    string
	banned []string
}{
	{pkg: "internal/mail", banned: []string{"internal/store", "internal/config", "internal/security"}},
	{pkg: "internal/store/sqlite", banned: []string{"internal/security/token", "internal/config"}},
}

// productRoots 是产品代码的顶层目录：4a 的装配者只有 internal/cli 与 tests/live，
// 而「同时导入邮件与存储」这一条只约束产品代码，4b 新建的 internal/app 才做这个装配。
var productRoots = []string{"cmd", "internal"}

// TestPackageDependencyDirection 断言清单的跨包禁令成立，且产品代码中没有包同时导入邮件与存储。
func TestPackageDependencyDirection(t *testing.T) {
	imports := repoPackageImports(t)
	if len(imports) == 0 {
		t.Fatal("没有扫描到任何包")
	}
	for dir, paths := range imports {
		for _, rule := range layerRules {
			if !underDir(dir, rule.pkg) {
				continue
			}
			for _, imported := range paths {
				for _, banned := range rule.banned {
					if underDir(imported, banned) {
						t.Errorf("%s 导入了 %s：清单要求 %s 不依赖 %s", dir, imported, rule.pkg, banned)
					}
				}
			}
		}
		if !isProduct(dir) {
			continue
		}
		mail, store := "", ""
		for _, imported := range paths {
			if underDir(imported, "internal/mail") {
				mail = imported
			}
			if underDir(imported, "internal/store") {
				store = imported
			}
		}
		if mail != "" && store != "" {
			t.Errorf("%s 同时导入了 %s 与 %s：4a 的产品代码中不应有包同时装配邮件与存储", dir, mail, store)
		}
	}
}

// repoPackageImports 扫描仓库中的非测试 Go 文件，返回每个包目录直接导入的仓库内包目录；
// 目录以仓库根目录为基准、用斜杠分隔，第三方与标准库的导入不计入。
func repoPackageImports(t *testing.T) map[string][]string {
	t.Helper()
	imports := make(map[string][]string)
	fset := token.NewFileSet()
	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if skipRepoDir(path, name) {
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
			local, ok := strings.CutPrefix(imported, modulePath+"/")
			if !ok {
				continue
			}
			if !slices.Contains(imports[pkg], local) {
				imports[pkg] = append(imports[pkg], local)
			}
		}
		if _, ok := imports[pkg]; !ok {
			imports[pkg] = nil
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 Go 源码失败：%v", err)
	}
	return imports
}

// underDir 判断包目录是否等于 prefix 或位于它之下。
func underDir(dir, prefix string) bool {
	return dir == prefix || strings.HasPrefix(dir, prefix+"/")
}

// isProduct 判断包目录是否属于产品代码（cmd 与 internal 下的包）。
func isProduct(dir string) bool {
	for _, root := range productRoots {
		if underDir(dir, root) {
			return true
		}
	}
	return false
}
