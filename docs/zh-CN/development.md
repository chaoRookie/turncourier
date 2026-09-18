# 开发指南

状态：pre-alpha。命令只有 `help`、`version`、`doctor` 三个，另有一套质量门槛。配置、存储与状态机（Phase 3）已作为内部包实现并通过测试，但尚未接入任何命令；没有邮件收发、Agent 适配器、Keychain 或后台服务。规划内容见[设计文档](design.md)，当前代码结构与持久化语义见[英文架构文档](../en/architecture.md)，各阶段范围见实施清单（[Phase 2](plans/phase-02.md)、[Phase 3](plans/phase-03.md)）。

本文所有命令都在仓库根目录执行。

## 环境准备

### Go 1.27.1

`go.mod` 要求 Go 1.27.1。生产代码依赖两个第三方模块（见[第三方依赖](#第三方依赖)），首次运行 `go vet`、`go test` 或 `make check` 时，go 命令会从模块代理下载它们；`modernc.org/sqlite` 体积较大，首次下载和编译需要几分钟。

本机已经装有 Go 时，先确认版本：

```sh
go version
```

版本不符时，可以把官方归档解压到仓库内的 `.local/`，不改动系统 Go 和 shell 配置。以 Apple 芯片的 macOS 为例：

```sh
mkdir -p .local/toolchains
curl -fL -o .local/toolchains/go1.27.1.darwin-arm64.tar.gz https://go.dev/dl/go1.27.1.darwin-arm64.tar.gz
echo "ee215d57e0ec269c60cc9ceca68e6bda321ba9ee5afe24f4b0988703c2d87d12  .local/toolchains/go1.27.1.darwin-arm64.tar.gz" | shasum -a 256 -c -
tar -C .local/toolchains -xzf .local/toolchains/go1.27.1.darwin-arm64.tar.gz
```

校验必须输出 `OK`，否则删掉归档重新下载，不要解压。如果 `.local/toolchains/go` 已经存在，先用 `.local/toolchains/go/bin/go version` 看清版本，不要解压到旧目录上。

其他平台换成对应的归档名和校验值。下表数值于 2026-09-17 与 go.dev 发布清单核对；其他平台以 go.dev 下载页公布的 SHA-256 为准。升级 `go.mod` 中的 Go 版本时，同步更新本节。

| 归档 | SHA-256 |
| --- | --- |
| `go1.27.1.darwin-arm64.tar.gz` | `ee215d57e0ec269c60cc9ceca68e6bda321ba9ee5afe24f4b0988703c2d87d12` |
| `go1.27.1.darwin-amd64.tar.gz` | `8f8f52c6649542cf027bbc9b9c68d1ec042f9f34808a40413f0b8b3f66f3caa4` |
| `go1.27.1.linux-amd64.tar.gz` | `63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445` |

系统里没有 `shasum` 的 Linux 可以改用 `sha256sum -c -`。

在当前终端启用本地工具链，并确认它被 Git 忽略：

```sh
export PATH="$PWD/.local/toolchains/go/bin:$PATH"
go version
git check-ignore .local/toolchains/go/bin/go
```

`go version` 应输出 `go1.27.1`；`git check-ignore` 打印出路径，说明该文件已被忽略。

不修改全局配置：

- `export` 只对当前终端有效，不要写进 `~/.zshrc` 之类的启动文件。
- 不要为本项目执行 `go env -w`，它会写入用户级 Go 配置。
- 不想改 `PATH` 时，直接给 make 传入工具链：`make GO="$PWD/.local/toolchains/go/bin/go" check`。Makefile 运行 staticcheck 和 govulncheck 时会把这个工具链的 `bin` 目录放到 `PATH` 最前面，所以覆盖对它们同样生效。

`.local/` 整个被 `.gitignore` 忽略，其中 `toolchains/` 放 Go，`bin/<工具>-<版本>/` 放质量工具。

### 其他依赖

- `make`、`git`、`bash`、`tar`：Makefile 和密钥扫描脚本会用到。
- `doctor` 会查找 `git`、`codex`、`claude`，但测试不依赖本机是否安装了它们。
- Node.js 22 或更高版本只在运行 `experiments/phase01/` 的研究探针时需要，见[探针说明](../../experiments/phase01/README.md)。

## 第三方依赖

| 模块 | 版本 | 许可证 | 使用方 |
| --- | --- | --- | --- |
| `modernc.org/sqlite` | v1.59.0 | BSD 风格 | `internal/store/sqlite`；纯 Go 实现，构建保持 `CGO_ENABLED=0` |
| `github.com/BurntSushi/toml` | v1.6.0 | MIT | `internal/config`；用 `MetaData.Undecoded()` 精确拒绝未知键 |

`modernc.org/sqlite` 另外带入 `dustin/go-humanize`、`google/uuid`、`mattn/go-isatty`、`ncruces/go-strftime`、`remyoudompheng/bigfft`、`golang.org/x/sys`、`modernc.org/libc`、`modernc.org/mathutil`、`modernc.org/memory`，许可证均为 MIT 或 BSD 风格。选型理由与实测记录见 [Phase 3 实施清单](plans/phase-03.md) 的决策 D1。

依赖规则：

- 只接受 MIT、BSD、Apache-2.0、ISC 等宽松许可证。新增依赖须在实施清单或 issue 中说明理由与许可证，并在上表记录。
- 版本固定在 `go.mod` 与 `go.sum`。`make modverify`（`make check` 的一部分）执行 `go mod verify`，确认模块缓存中的依赖与 `go.sum` 记录的哈希一致，防止被篡改的依赖进入构建。
- govulncheck（`make security`）与 Dependabot 的 `gomod` 更新覆盖这些依赖。
- 目前没有命令导入配置、状态机或存储包，产品二进制不链接上述模块。正式发布链接它们的二进制之前，必须补齐第三方许可声明。

## 常用 make 目标

| 目标 | 作用 | 是否联网 |
| --- | --- | --- |
| `make build` | `go build -trimpath` 输出 `dist/turncourier`。版本号默认 `0.1.0-dev`，可用 `make build VERSION=0.1.0-dev.local` 覆盖 | 否 |
| `make fmt` | 用 `gofmt -w` 格式化 `cmd`、`internal`、`tests`、`tools` | 否 |
| `make fmt-check` | 列出未格式化的文件并失败；gofmt 本身出错也算失败 | 否 |
| `make vet` | `go vet ./...` | 首次下载模块依赖时 |
| `make modverify` | `go mod verify`，校验依赖与 `go.sum` 记录的哈希一致 | 首次下载模块依赖时 |
| `make comments` | `go run ./tools/commentcheck .`，检查中文注释 | 否 |
| `make test` | `go test -race -covermode=atomic -coverprofile=coverage.out ./...`，再用 `tools/covercheck` 要求覆盖率不低于 80% | 首次下载模块依赖时 |
| `make lint` | staticcheck v0.8.1 检查 `./...` | 首次安装或首次下载模块依赖时 |
| `make check` | 依次运行 `fmt-check`、`vet`、`modverify`、`comments`、`test`、`lint` | 首次下载模块依赖或安装 staticcheck 时 |
| `make secrets` | 用 gitleaks v8.30.1 执行 `scripts/scan-secrets.sh` | 首次安装时 |
| `make security` | 先执行 `make secrets`，再用 govulncheck v1.8.0 检查 `./...` | 是：govulncheck 每次运行都要查询 Go 漏洞数据库 |
| `make workflows` | actionlint v1.7.12 检查 `.github/workflows` | 首次安装时 |
| `make tools` | 一次装好上面四个质量工具 | 是 |

质量工具第一次用到时，由 Makefile 执行 `GOBIN=.local/bin/<工具>-<版本> go install <模块>@<版本>` 安装，需要能访问 Go 模块代理。版本号写在目录名里，Makefile 改了版本后会自动安装新版本，不会沿用旧的二进制。`dist/` 和 `coverage.out` 都被 Git 忽略。

提交 PR 前建议在本地跑一遍：

```sh
make fmt
make check
make security
make workflows
make build
./dist/turncourier help
./dist/turncourier version
./dist/turncourier doctor --json
```

在非 macOS 平台，或者 `git`、`codex`、`claude` 中任何一个缺失、超时或输出异常时，`doctor` 会返回退出码 1。这是预期行为，不代表本次改动有问题。`doctor` 通过也只说明基础依赖存在，不代表登录、权限或邮箱可用。

## 覆盖率门槛

`make test` 对模块内全部包（`cmd`、`internal`、`tools`）生成 `coverage.out`，然后运行 `go run ./tools/covercheck -min 80 coverage.out`。covercheck 没有排除参数，也不按目录或文件过滤，当前平台参与编译的全部手写非测试代码都计入；只在其他平台编译的文件不在本次统计内，例如 macOS 和 Linux 上的 `internal/doctor/process_other.go`。

计算方式：

1. 第一行必须是 `mode: set`、`mode: count` 或 `mode: atomic`。
2. 之后每行是一个代码块：`文件:起始行.起始列,结束行.结束列 语句数 执行次数`。
3. 文件和起止位置都相同的记录合并为一个块：语句数只算一次，任意一条记录的执行次数大于 0 就算已覆盖。同一个块的语句数前后不一致时报错。
4. 覆盖率 = 已覆盖块的语句数之和 ÷ 所有块的语句数之和 × 100。它按语句数加权，不是按文件或函数取平均。
5. 结果低于 `-min` 时失败，恰好 80% 算通过。`-min` 必须在 0 到 100 之间。

以下情况同样失败：文件为空或读不出来、模式头无效、记录格式或源码范围无效、`set` 模式的计数大于 1、语句总数溢出、没有可统计的语句。covercheck 本身在检查不通过时返回 1，参数错误时返回 2；经 `go run` 调用时，go 命令对这两种情况都返回 1。

查看各函数的覆盖情况：

```sh
go tool cover -func=coverage.out
```

是否达标以 covercheck 的输出为准。覆盖率不够时补测试，不要用构建约束隐藏代码，也不要缩小测试范围。

## 中文注释规则

- 标识符用英文。
- 每个手写具名函数（包括方法、测试函数和测试辅助函数）、每个类型（包括结构体、接口和函数体内的局部类型）、每个包都要有中文文档注释。匿名函数不要求。
- 导出标识符的注释以标识符名称开头，例如 `// Run 执行命令并返回退出码`。
- 逻辑复杂时，注释要说明错误处理、并发和安全边界。

### commentcheck 的检查范围

`make comments` 从仓库根目录递归检查。也可以直接运行：

```sh
go run ./tools/commentcheck .
```

- 参数最多一个目录，省略时检查当前目录。
- 跳过规则与 go 工具一致：跳过以 `.` 或 `_` 开头的子目录（例如 `.git`、`.local`），以及 `vendor` 和 `testdata`；跳过以 `.` 或 `_` 开头的文件和所有非 `.go` 文件。
- 不按构建约束过滤。像 `internal/doctor/process_other.go` 这样只在其他平台编译的文件也会检查。
- 生成文件豁免：包声明前带有标准 `// Code generated ... DO NOT EDIT.` 标记的文件不检查，也不计数。写法不标准的标记不豁免。
- `.go` 文件是符号链接时直接报错，不会跳过，因为这种文件会被编译，但无法确认它是否在检查范围内。
- 一个手写 Go 文件都没找到时视为失败，例如路径写错，或者传入的根目录本身是符号链接（遍历时不跟随）。根路径不存在、不是目录或源码有语法错误也会失败。
- 包注释按“目录 + 包名”汇总，可以写在包内任意文件里，例如 `doc.go`。包里有非测试文件时，只认非测试文件上的包注释，与 `go doc` 一致。外部测试包（`package xxx_test`）需要单独写中文包注释。
- 分组声明 `type ( ... )` 里的每个类型都要有自己的注释；只有分组里只有一个类型时，分组上方的注释才算数。
- “中文注释”指注释里至少有一个汉字。注释写得是否充分，仍要靠人工评审。
- commentcheck 本身的退出码：0 为通过，1 为发现问题或检查失败，2 为参数错误。

## staticcheck.conf

仓库根目录的 `staticcheck.conf` 内容是 `checks = ["inherit", "ST1020", "ST1021", "ST1022"]`：

- `inherit` 继承 staticcheck 的默认检查集。
- 额外开启默认关闭的 ST1020、ST1021、ST1022。它们分别要求导出函数（含方法）、导出类型、导出变量和常量的文档注释以标识符名称开头。

这样“导出注释以名称开头”就能在 `make lint` 中自动检查。staticcheck 只检查已有注释的开头；包、类型和具名函数是否写了中文注释由 commentcheck 检查，说明是否充分靠人工评审。

## 测试约定

- 单元测试和被测代码放在同一目录（`*_test.go`）。
- 跨包的集成测试放在 `tests/integration/`（包名 `integration_test`，只有测试文件，需要单独写中文包注释）。`lifecycle_test.go` 组合示例配置、临时目录中的真实 SQLite 与两个状态机，走完一条回复从入队、派发、模拟崩溃后恢复到任务关闭的完整流程。端到端和真实验收测试按设计将来也放在 `tests/` 下，有代码时再建对应目录。
- 测试必须离线：不访问网络、真实邮箱、模型或 Agent 会话，也不依赖本机是否装有 `git`、`codex`、`claude`。`internal/doctor` 的测试通过 `Checker` 注入 `GOOS`、`Lookup`、`Run` 和 `Timeout`，使用合成的版本字符串（见 `doctor_test.go` 中的 `healthyChecker`）。CLI 测试把输出写到 `bytes.Buffer`。
- 需要真实子进程时，使用测试辅助子进程模式，让测试二进制自己扮演被调用的命令。`internal/doctor/doctor_test.go` 中的 `TestMain` 发现环境变量 `TURNCOURIER_TEST_VERSION_PROCESS` 非空时不运行测试，而是按模式（`ok`、`error`、`large`、`wait`、`background`、`group`）模拟版本命令。测试用 `os.Executable()` 取得自身路径，用 `t.Setenv` 选择模式，通过 `TURNCOURIER_TEST_PID_FILE` 取回孙进程 PID，结束前清理遗留进程。现有这类变量都以 `TURNCOURIER_TEST_` 开头，正常测试流程不会设置它们。
- 平台相关的测试用构建约束，例如 `process_unix_test.go` 的 `//go:build unix`。平台缺少前提条件（例如不能创建符号链接）时，用 `t.Skip` 并写明原因。
- 临时文件放在 `t.TempDir()` 里。
- 配置测试把合成的 TOML 写入 `t.TempDir()` 并设为 0600，环境变量通过注入的 `getenv` 或 `t.Setenv` 提供，断言错误文本不含临时目录路径。
- SQLite 测试使用临时目录中的真实数据库，不用内存库或替身驱动。`t.TempDir()` 按 umask 创建（通常为 0755），会被数据目录的权限检查拒绝，因此数据目录用它下面尚不存在、由 `sqlite.Open` 以 0700 新建的子目录。时钟与随机源通过 `sqlite.Options` 注入。验证事务回滚时，在测试中直接对数据库创建触发器注入故障，例如 `CREATE TRIGGER boom BEFORE INSERT ON replies BEGIN SELECT RAISE(ABORT, 'boom'); END;`，再断言相关表的行数和状态没有变化。
- 测试数据全部使用合成内容。需要邮箱地址时，使用 `.invalid` 这类保留域名。

常用命令：

```sh
go test ./internal/doctor
go test -run TestCheckTimeout ./internal/doctor
go test -race ./tests/...
go test -race ./...
make test
```

覆盖率门槛只以 `make test` 的结果为准。

模糊测试：`internal/config` 的 `FuzzNormalizeAddress` 在普通 `go test` 与 CI 中只运行种子用例。需要运行模糊引擎时在本地执行：

```sh
go test -run='^$' -fuzz=FuzzNormalizeAddress -fuzztime=30s ./internal/config/
```

发现失败时，go 会把触发失败的输入写入 `internal/config/testdata/fuzz/FuzzNormalizeAddress/`，之后的 `go test` 会把它当作种子重跑。

## 配置文件与数据目录

目前没有命令读取配置或打开数据库，以下位置由 `internal/config` 与 `internal/store/sqlite` 实现，供后续命令使用：

| 项目 | 位置 |
| --- | --- |
| 配置文件 | `$TURNCOURIER_CONFIG`（须为绝对路径）；未设置时为 `os.UserConfigDir()` 下的 `TurnCourier/turncourier.toml`，macOS 上即 `~/Library/Application Support/TurnCourier/turncourier.toml` |
| 数据目录 | `$TURNCOURIER_DATA_DIR`（须为绝对路径）；未设置时为配置文件所在目录 |
| 数据库 | 数据目录下的 `turncourier.db` |

- 配置示例是 `configs/turncourier.example.toml`，测试会加载它，修改校验规则时同步修改示例。配置只保存账户与选项，出现 `password`、`token` 等凭据类键或未知键都会报错；授权码与签名密钥将由 `init` 写入 Keychain（尚未实现）。
- 配置文件须是不超过 1 MiB 的常规文件；在 Unix 上须归当前用户所有，且组和其他用户不可写。
- 环境变量只能覆盖 `TURNCOURIER_NOTIFY_EVENTS`（逗号分隔，空字符串视为未设置）。白名单、邮箱地址、令牌有效期等安全相关项不接受环境变量覆盖。
- 在 Unix 上，数据目录权限须为 0700、数据库文件须为 0600；缺失时按此权限创建，已有目录或文件权限更宽时拒绝打开。数据库只保存元数据与正文 SHA-256 摘要，不保存正文和凭据。
- `.gitignore` 已忽略 `turncourier.toml` 与 `*.db`，不要把本机配置或数据库提交进仓库。

## 隐私与凭据

- 不要在 issue、PR、提交、测试或日志中提交 QQ 邮箱授权码、令牌、真实邮件内容、Agent 会话记录或本机绝对路径。复现问题时优先使用合成数据。
- 按计划，授权码将来通过本地 `init` 写入 macOS Keychain。这一功能尚未实现，目前没有任何命令会读取或保存授权码。
- `.gitignore` 已经忽略 `.env`、`.env.*`、`*.db`、`*.sqlite*`、`*.log`、`turncourier.toml`、`.local/`、`dist/`、`coverage.out` 等文件。不要用 `git add -f` 强行加入。
- 公共 CI 不运行真实邮箱或模型调用，也不保存凭据。
- `experiments/phase01/` 的 live 探针会使用本机 CLI 登录并消耗订阅额度，不属于 `make check`，运行前先读[探针说明](../../experiments/phase01/README.md)。只做语法检查时可以离线运行，例如 `node --check experiments/phase01/codex-probe.mjs`。

## 密钥扫描

`make secrets` 运行 `scripts/scan-secrets.sh`，使用固定版本的 gitleaks 和它的默认规则，输出经 `--redact` 遮蔽。必须在 Git 仓库内执行。

| 范围 | 做法 |
| --- | --- |
| 全部 Git 历史 | `gitleaks git --log-opts='--all'`，覆盖所有引用 |
| 暂存区 | `gitleaks git --staged`。暂存区就是下一次提交的内容，可能和工作树不同 |
| 待公开的工作树快照 | 用 `git ls-files -z --cached --others --exclude-standard` 列出受跟踪和未被忽略的文件，打包复制到 `${TMPDIR:-/tmp}/turncourier-secret-scan.XXXXXX`，再用 `gitleaks dir` 扫描。脚本退出时只删除这个临时目录 |

- 被 Git 忽略的文件（例如 `.local/`、`dist/`、`.env`）不进入快照，这些文件本来就不应提交。
- 工作树里已删除但未暂存的路径不会被打包，它们的内容已由暂存区扫描和历史扫描覆盖。符号链接本身会保留在快照中。

脚本拒绝任何自定义配置：

- 启动时清除 `GITLEAKS_CONFIG` 和 `GITLEAKS_CONFIG_TOML` 环境变量。
- 只要仓库根目录有 `.gitleaks.toml` 或 `.gitleaksignore`，不论是工作树中的文件、符号链接，还是已经进入索引，脚本都直接失败。原因是 gitleaks 会自动读取这两个文件：`.gitleaks.toml` 如果没有声明 `[extend] useDefault = true`，会悄悄替换全部默认规则；`.gitleaksignore` 会按指纹放行发现，扫描结果仍显示通过。两种情况都会让“通过”失去意义。

遇到误报时，把合成测试数据改得不像凭据，不要添加上述文件。真实凭据一旦进入提交，先撤销或更换该凭据；只改写 Git 历史不能让已经泄露的凭据失效。

## 新增包或目录

完整的目标目录树只在[设计文档](design.md)中维护。目录随实现逐步创建，不为了好看建空目录、动态插件或微服务。新增包或目录时，在同一个变更里完成：

1. 写中文包注释，测试放在包内。新包会自动计入覆盖率。
2. 更新 `README.md` 和 `README.zh-CN.md` 中的实际文件树，两份保持一致，只列出真实存在的文件和目录。对照下面命令的输出：

   ```sh
   git ls-files --cached --others --exclude-standard | sort
   ```

   输出里没有 `.local/`、`dist/`、`coverage.out` 这类被忽略的内容，树形图里也不要出现。
3. 包职责或依赖方向变化时，同步更新[英文架构文档](../en/architecture.md)。
4. 新增第三方依赖按[第三方依赖](#第三方依赖)一节的规则处理。
5. 改动了 `.github/workflows/` 时，运行 `make workflows`。

## CI 工作流

| 工作流 | 触发方式 | 内容 |
| --- | --- | --- |
| `ci.yml`（CI） | 推送到 `main`、pull request、手动 | 在 `ubuntu-24.04` 和 `macos-15` 上运行 `make check`、`make build`，冒烟运行 `./dist/turncourier help` 和 `version`；只在 Linux 上额外运行 `make workflows`。同一引用有新运行时取消旧运行 |
| `security.yml`（Security） | 推送到 `main`、pull request、手动 | 在 `ubuntu-24.04` 上拉取完整历史，运行 `make security` |
| `build.yml`（Candidate build） | 仅手动 | 在 `macos-15` 上运行 `make check` 和 `make security`，然后构建候选二进制，见下一节 |

共同约束：

- 所有 Action 都固定到完整提交 SHA，后面注释对应的版本号。手动更新 Action 时保持这种写法，并运行 `make workflows`。
- 工作流权限为 `contents: read`，checkout 设置 `persist-credentials: false`，每个任务超时 15 分钟。
- `setup-go` 从 `go.mod` 读取 Go 版本，不启用缓存。
- 工作流不使用仓库 secrets，不连接真实邮箱，不调用模型。CI 的冒烟测试只运行 `help` 和 `version`，不运行 `doctor`；`doctor` 的结果取决于机器上装了哪些工具，请在本地运行。
- Dependabot 每周检查 `github-actions` 和 `gomod` 的更新，每类最多同时开 5 个 PR。

在本地复现 CI 的质量任务：

```sh
make check
make build
./dist/turncourier help
./dist/turncourier version
```

## 候选构建与发布

仓库公开后，可以在 GitHub Actions 页面手动运行 Candidate build。它先通过 `make check` 和 `make security`，然后以 `CGO_ENABLED=0`、`GOOS=darwin` 构建 `arm64` 和 `amd64` 两个二进制，版本号为 `0.1.0-dev.<提交 SHA 前 7 位>`，并生成 `SHA256SUMS`。这些文件作为名为 `turncourier-macos-candidate` 的 Actions artifact 上传，保留 14 天。

下载并解压 artifact 后，在解压目录中核对校验值：

```sh
shasum -a 256 -c SHA256SUMS
```

`gh run download`、`unzip` 和 Finder 解压会保留可执行权限；其他工具（例如 Python `zipfile`）可能丢失，需要 `chmod +x`。二进制只有链接器生成的临时签名，没有经过 Apple 公证，经浏览器下载时可能被 Gatekeeper 拦截。

候选构建不是正式发布：

- 它不创建 tag，也不创建 GitHub Release，artifact 到期后会被删除。
- 其中的二进制只有 `help`、`version`、`doctor`，没有经过任何邮件验收。
- 本地 `make build` 产出的 `0.1.0-dev` 同样不是发布版本。

完成真实邮箱验收之前，不打 `v0.1.0-alpha` 标签。按设计，发布前 Codex 和 Claude Code 各要完成连续十轮真实邮件交互，覆盖断网恢复、重复、伪造和过期邮件、忙碌时的 FIFO 顺序以及隐私过滤；编译成功不能代替真实验收。目前没有任何发布版本，默认分支为 `main`。
