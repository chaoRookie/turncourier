# 开发指南

状态：pre-alpha。命令有 `help`、`version`、`doctor` 与 `init`，另有一套质量门槛。Phase 4 拆分为 4a（离线实现）、L1（维护者在本机执行的真机探测）与 4b（依据 L1 结果实现）；4a 已完成 Keychain 封装、回复令牌、正文加密、迁移 `0002`、待发通知状态机与存储、SMTP 与 IMAP 客户端及其离线假服务器、只由人工执行的 `tests/live` 探测工具，以及第一个使用配置与存储的命令 `init`。TurnCourier 仍不能收发邮件：没有通知渲染、入站验证与收发循环，也没有 Agent 适配器和后台服务。规划内容见[设计文档](design.md)，当前代码结构与持久化语义见[英文架构文档](../en/architecture.md)，各阶段范围见实施清单（[Phase 2](plans/phase-02.md)、[Phase 3](plans/phase-03.md)、[Phase 4](plans/phase-04.md)）。

本文所有命令都在仓库根目录执行。

## 环境准备

### Go 1.27.1

`go.mod` 要求 Go 1.27.1。仓库依赖若干第三方模块（见[第三方依赖](#第三方依赖)），首次运行 `go vet`、`go test` 或 `make check` 时，go 命令会从模块代理下载它们；`modernc.org/sqlite` 体积较大，首次下载和编译需要几分钟。

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
| `modernc.org/sqlite` | v1.59.0 | BSD 风格 | `internal/store/sqlite`；纯 Go 实现，构建保持 `CGO_ENABLED=0`。`tests/live` 另以只读方式打开数据库读取实例 ID |
| `github.com/BurntSushi/toml` | v1.6.0 | MIT | `internal/config`；用 `MetaData.Keys()` 列出全部键路径，与白名单逐段区分大小写比对来拒绝未知键（该库会把大小写变体匹配到字段，`MetaData.Undecoded()` 看不到它们） |
| `golang.org/x/term` | v0.46.0 | BSD-3-Clause | `internal/cli`；`init` 以不回显方式读入授权码 |
| `github.com/emersion/go-smtp` | v0.25.0 | MIT | `internal/mail/smtp` 用它的客户端发信，测试用它的服务端作离线假服务器。选它而不用已冻结的 `net/smtp`：它自带命令超时，`CloseWithResponse` 还能取回服务端的 DATA 响应文本（L1 需要它查找 QQ 分配的 ID）。它不阻止在明文连接上发送凭据，因此只允许 `DialTLS`，并由源码测试钉住 |
| `github.com/emersion/go-imap/v2` | v2.0.0-beta.8 | MIT | `internal/mail/imap` 只用 `imapclient`；`imapserver` 与 `imapmemserver` 只在测试中用作离线假服务器。仍是 beta，各 beta 之间有破坏性 API 变更，因此固定精确版本、封装在本包之后，升级 PR 须人工审阅 |
| `github.com/emersion/go-message` | v0.18.2 | MIT | `internal/mail/parser` 用它读取来信的头部与 MIME 结构，经 `charset` 解码字符集（并在其中把 GBK 系列标签映射到 GB18030）；随 `imapclient`（它导入 `go-message/mail`）进入 `internal/mail/imap`；`tests/live` 直接导入它与 `charset` 解码 GB18030、GBK |
| `github.com/emersion/go-sasl` | 伪版本 `b788ff22d5a6` | MIT | `internal/mail/smtp` 用 `NewPlainClient` 做 `AUTH PLAIN`；go-imap/v2 也会引入它 |
| `golang.org/x/text` | v0.42.0 | BSD-3-Clause | `internal/mail/parser` 与 `tests/live`（`sample.go`）直接导入 `encoding/simplifiedchinese`，把 GBK 系列标签映射到 GB18030 解码器，`go-message/charset` 也需要它；`tests/fixtures/mail` 用它的 GB18030 编码器组装合成回归样本。显式固定：go-message 要求的 v0.14.0 有模块级漏洞 GO-2026-5970，修复于 v0.39.0 |
| `golang.org/x/sys` | v0.48.0 | BSD-3-Clause | `internal/cli`；在 macOS 上经 termios 关闭终端回显。同时也是 `golang.org/x/term` 与 `modernc.org/sqlite` 的依赖 |

`modernc.org/sqlite` 另外带入 `dustin/go-humanize`、`google/uuid`、`mattn/go-isatty`、`ncruces/go-strftime`、`remyoudompheng/bigfft`、`modernc.org/libc`、`modernc.org/mathutil`、`modernc.org/memory`，许可证均为 MIT 或 BSD 风格；`golang.org/x/sys` 固定在 v0.48.0，这是 `golang.org/x/term` v0.46.0 的要求。选型理由与实测记录见 [Phase 3 实施清单](plans/phase-03.md) 的决策 D1 与 [Phase 4 实施清单](plans/phase-04.md) 的决策 D2。

依赖规则：

- 只接受 MIT、BSD、Apache-2.0、ISC 等宽松许可证。新增依赖须在实施清单或 issue 中说明理由与许可证，并在上表记录。
- 版本固定在 `go.mod` 与 `go.sum`。`make modverify`（`make check` 的一部分）执行 `go mod verify`，确认模块缓存中的依赖与 `go.sum` 记录的哈希一致，防止被篡改的依赖进入构建。
- govulncheck（`make security`）与 Dependabot 的 `gomod` 更新覆盖这些依赖。
- 产品二进制目前只链接 `BurntSushi/toml`、`modernc.org/sqlite`（及其依赖）、`golang.org/x/term` 与 `golang.org/x/sys`；四个邮件模块与 `golang.org/x/text` 只被测试和 `tests/live` 使用，要到 4b 由 `internal/app` 把收发循环接入命令后才进入二进制。用 `go version -m dist/turncourier` 核对。正式发布链接这些模块的二进制之前，必须补齐第三方许可声明。

## 常用 make 目标

| 目标 | 作用 | 是否联网 |
| --- | --- | --- |
| `make build` | `go build -trimpath` 输出 `dist/turncourier`。版本号默认 `0.1.0-dev`，可用 `make build VERSION=0.1.0-dev.local` 覆盖 | 否 |
| `make fmt` | 用 `gofmt -w` 格式化 `cmd`、`internal`、`tests`、`tools` | 否 |
| `make fmt-check` | 列出未格式化的文件并失败；gofmt 本身出错也算失败 | 否 |
| `make vet` | `go vet ./...`，再加 `go vet -tags live ./tests/live/`：探测代码只编译检查，从不运行 | 首次下载模块依赖时 |
| `make modverify` | `go mod verify`，校验依赖与 `go.sum` 记录的哈希一致 | 首次下载模块依赖时 |
| `make comments` | `go run ./tools/commentcheck .`，检查中文注释 | 否 |
| `make test` | `go test -race -covermode=atomic -coverprofile=coverage.out ./...`，再用 `tools/covercheck` 要求覆盖率不低于 80% | 首次下载模块依赖时 |
| `make lint` | staticcheck v0.8.1 检查 `./...`，再加 `staticcheck -tags live ./tests/live/` | 首次安装或首次下载模块依赖时 |
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
- 核对文档与代码是否一致的测试放在 `tests/docs/`（包名 `docs_test`）：`deps_test.go` 以 `go.mod` 的直接依赖与各包实际导入为准，核对中英文两份第三方依赖表；`imports_test.go` 整仓扫描非测试源码的导入，守住实施清单规定的包依赖方向；`reveal_test.go` 完整解析 `cmd/` 与 `internal/` 下全部非测试源码，确认返回令牌明文的 `Reveal`、`RevealToken` 只在 `internal/security/token` 与 `internal/mail/renderer` 中被引用（以「.」「_」开头的目录与 `testdata` 同样扫描，因为它们被显式导入时照样会编进产品）。
- 跨包的集成测试放在 `tests/integration/`（包名 `integration_test`，只有测试文件，需要单独写中文包注释）。`lifecycle_test.go` 组合示例配置、临时目录中的真实 SQLite 与两个状态机，走完一条回复从入队、派发、模拟崩溃后恢复到任务关闭的完整流程；`payload_test.go` 再走一遍通知、令牌、键控摘要、正文密文与派发的组合生命周期。端到端测试按设计将来也放在 `tests/` 下，有代码时再建对应目录；人工执行的真机探测已经在 `tests/live/`，见[真机探测](#真机探测testslive)。
- 测试必须离线：不访问网络、真实邮箱、模型或 Agent 会话，不读写真实钥匙串，也不依赖本机是否装有 `git`、`codex`、`claude`。`internal/doctor` 的测试通过 `Checker` 注入 `GOOS`、`Lookup`、`Run` 和 `Timeout`，使用合成的版本字符串（见 `doctor_test.go` 中的 `healthyChecker`）。CLI 测试把输出写到 `bytes.Buffer`，并注入终端、Keychain、环境变量、随机源与时钟的替身。唯一的例外是带 `live` 标签的 `tests/live`，它不随 `go test ./...` 编译。
- 需要真实子进程时，使用测试辅助子进程模式，让测试二进制自己扮演被调用的命令。`internal/doctor/doctor_test.go` 中的 `TestMain` 发现环境变量 `TURNCOURIER_TEST_VERSION_PROCESS` 非空时不运行测试，而是按模式（`ok`、`error`、`large`、`wait`、`background`、`group`）模拟版本命令。`internal/security/keychain/keychain_test.go` 同理：`TURNCOURIER_TEST_SECURITY_MODE` 非空时，测试二进制扮演 `/usr/bin/security`，用 `TURNCOURIER_TEST_SECURITY_STATE` 指向的文件保存假条目，把收到的参数与标准输入写进 `TURNCOURIER_TEST_SECURITY_LOG`，测试据此断言机密没有进入参数列表与环境变量。测试用 `os.Executable()` 取得自身路径，用 `t.Setenv` 选择模式，通过 `TURNCOURIER_TEST_PID_FILE` 取回孙进程 PID，结束前清理遗留进程。现有这类变量都以 `TURNCOURIER_TEST_` 开头，正常测试流程不会设置它们。
- 邮件客户端用离线假服务器测试，不连接任何真实服务器。SMTP 用 go-smtp 自带的服务端，IMAP 用 `imapmemserver` 加一层可注入故障的代理：`imapmemserver` 模拟不了 QQ 对不支持的命令只回无标签 `* BAD`，也模拟不了半开连接，这些由代理注入，代理同时记录客户端发出的每条命令（`LOGIN` 只记命令名）。两边的 TLS 证书都借用 `net/http/httptest` 的自签证书，只把它的根证书放进 `RootCAs`，不落盘、不联网；IMAP 的服务端配置要清掉 `NextProtos`，否则 httptest 的 `http/1.1` 会和 imapclient 协商的 ALPN `imap` 冲突而握手失败。
- 「从不做某事」这类否定性质由源码测试钉住，功能测试看不出差别：`internal/security/token/source_test.go` 用 `go/parser` 与 `go/types` 断言标签只经常数时间比较；`internal/mail/smtp/source_test.go` 与 `internal/mail/imap/source_test.go` 按白名单断言只调用 `DialTLS`（不调用 `Dial`、`DialStartTLS`）、不出现调试输出与放宽证书校验的标识符、`tls.Config` 只在包内构造一处，IMAP 另外断言 EXAMINE 只以只读选项发出、FETCH 的正文项都带 `Peek`。
- 平台相关的测试用构建约束，例如 `process_unix_test.go` 的 `//go:build unix`。平台缺少前提条件（例如不能创建符号链接）时，用 `t.Skip` 并写明原因。
- 临时文件放在 `t.TempDir()` 里。
- 配置测试把合成的 TOML 写入 `t.TempDir()` 并设为 0600，环境变量通过注入的 `getenv` 或 `t.Setenv` 提供，断言错误文本不含临时目录路径。
- SQLite 测试使用临时目录中的真实数据库，不用内存库或替身驱动。`t.TempDir()` 按 umask 创建（通常为 0755），会被数据目录的权限检查拒绝，因此数据目录用它下面尚不存在、由 `sqlite.Open` 以 0700 新建的子目录。时钟与随机源通过 `sqlite.Options` 注入。验证事务回滚时，在测试中直接对数据库创建触发器注入故障，例如 `CREATE TRIGGER boom BEFORE INSERT ON replies BEGIN SELECT RAISE(ABORT, 'boom'); END;`，再断言相关表的行数和状态没有变化。
- 测试数据全部使用合成内容。需要邮箱地址时，使用 `.invalid` 这类保留域名。
- 入站邮件的回归样本放在 `tests/fixtures/mail/`：每个 JSON 模板只保存头部与各部件解码后的文字，并声明字符集与传输编码；令牌、任务 ID 与实际投递 ID 用占位符 `{{TOKEN}}`、`{{TASK}}`、`{{DELIVERED}}`，测试把令牌用 `security/token` 在运行时签发，交给包 `mailfixture` 替换并组装成原始字节。不要保存 base64 的 `.eml`：那样无法替换占位符，而把令牌写进文件又违反密钥扫描的约定。模板格式与每个样本的来源见该目录的 `README.md`。

常用命令：

```sh
go test ./internal/doctor
go test -run TestCheckTimeout ./internal/doctor
go test -race ./tests/...
go test -race ./...
make test
```

覆盖率门槛只以 `make test` 的结果为准。

模糊测试：`internal/config` 的 `FuzzNormalizeAddress`、`internal/security/token` 的 `FuzzParse` 与 `FuzzParseSubject` 在普通 `go test` 与 CI 中只运行种子用例。需要运行模糊引擎时在本地执行（把包与目标名换成另一个即可）：

```sh
go test -run='^$' -fuzz=FuzzNormalizeAddress -fuzztime=30s ./internal/config/
```

发现失败时，go 会把触发失败的输入写入 `internal/config/testdata/fuzz/FuzzNormalizeAddress/`，之后的 `go test` 会把它当作种子重跑。

模糊引擎发现「新覆盖」的输入后会先最小化它，默认的 `-fuzzminimizetime`（60 秒）下，几 KB 的输入要逐字节尝试删除，这段时间里执行数会停在 0/秒，看起来像卡住，其实被测代码没有挂起。运行 `FuzzParseSubject` 这类接受长字符串的目标时，可以加 `-fuzzminimizetime=2s`。

## 真机探测（tests/live）

`tests/live/` 是 L1 真机探测工具：它会登录真实的机器人邮箱、发出真实邮件、读写真实钥匙串，只由维护者在本机人工执行。普通开发和 CI 都不会运行它。

`tests/live/` 里有两类文件：

- `sample.go` 与 `sample_test.go` **不带构建标签**。`sample.go` 是纯函数——样本脱敏归类、主题前缀白名单、Message-ID 按相等关系归类、输出目录校验——只处理传入的字节与值，不读配置、钥匙串、网络或环境变量。`sample_test.go` 用合成邮件字节离线测试它们，随 `make test` 在 CI 中运行，计入覆盖率与中文注释检查。
- `live_test.go`、`probe_test.go`、`keychain_test.go` 都以 `//go:build live` 开头。不带标签时 `go test ./...`、`make test` 与 CI 都不编译它们；`make vet` 与 `make lint` 带上标签只做编译检查，从不运行。

不会被误触发的四道关：

1. **双重开关。** 必须同时给出 `-tags live` 与 `TURNCOURIER_LIVE=1`。缺环境变量时 `TestMain` 打印「跳过」并以 0 退出，不运行任何测试（包列表模式下 `go test` 会丢弃通过的包的输出，想看到这行要加 `-v`）。
2. **拒绝 CI。** 环境变量 `CI` 非空时打印「拒绝在 CI 中运行」并以 1 退出。
3. **总确认。** 通过前两关后、访问钥匙串或网络之前，经 `/dev/tty` 列出机器人账户、接收地址、输出目录与本次 `-run` 选中的探测项，要求输入 `yes`。`TURNCOURIER_LIVE=1` 残留在 shell 中时，误运行的命令会停在这里。
4. **逐项确认。** 每封邮件发出前、每次写入真实钥匙串前再确认一次；打不开 `/dev/tty`（没有控制终端）即失败，回答其他内容则跳过该项并记为 `skipped`。

输出目录由 `TURNCOURIER_LIVE_OUT` 指定，必须是已存在的绝对路径目录、权限恰为 0700，且**不在本仓库工作树之内**（先解析符号链接再逐级向上按 inode 比对，所以仓库外指向仓库内的符号链接也会被拒绝）。`state.json` 与 `samples.jsonl` 以 0600 写在那里，`.gitignore` 另外兜底忽略这两个文件名。

| 探测 | 做什么 | 是否发信 |
| --- | --- | --- |
| `TestL1KeychainRoundTrip` | 在真实钥匙串中写入、读回、删除一个合成条目，并确认 `Add` 对已存在条目返回 `ErrExists` | 否 |
| `TestL1Capabilities` | 登录 IMAP，记录能力、文件夹列表与各文件夹的 UIDVALIDITY | 否 |
| `TestL1SendNotification` | 发出合成通知，再只读查找「已发送」中的副本，记录 QQ 分配的 Message-ID | 是 |
| `TestL1Replies` | 只读补扫 INBOX 与 Junk，对引用了探测邮件的来信输出脱敏样本 | 否 |
| `TestL1Idle` | 分预检、测量、FETCH 检查三个会话，记录 IDLE 推送延迟与服务器断开时间 | 可选 |

| 环境变量 | 作用 |
| --- | --- |
| `TURNCOURIER_LIVE` | 必须恰为 `1`，否则跳过 |
| `TURNCOURIER_LIVE_OUT` | 输出目录，见上 |
| `TURNCOURIER_LIVE_SEND_COUNT` | 发出的合成通知数，默认 1，至多 5 |
| `TURNCOURIER_LIVE_CC_BOT` | 设为 `1` 时同时抄送机器人自己，用于从收件箱取回投递副本的 ID |
| `TURNCOURIER_LIVE_IDLE_MINUTES` | IDLE 测量时长，默认 30，至多 60 |
| `TURNCOURIER_LIVE_IDLE_SELF_SEND` | 设为 `1` 时在测量中自发一封邮件并测推送延迟 |
| `TURNCOURIER_LIVE_KEEP` | 设为 `1` 时保留合成钥匙串条目，等待按回车再删除，便于在另一终端检查沙箱能否读到它 |

运行探测的全部命令都要带 `-count=1`，否则 `go test` 可能直接显示缓存结果，看起来像是跑过了。

自 L1b 起，`TestL1SendNotification` 发出的合成通知按 D4 定稿的格式把一次性令牌放在主题标签 `[TC <任务 ID> <令牌>]` 中（页脚仍有一份副本），逐封确认只回显标签之后的文字；样本另记录标签的状态、`Return-Path` 与自动回复的主题前缀，格式为 `turncourier-l1/4`。L1b 的规程见 [Phase 4 实施清单](plans/phase-04.md) 的「L1b」一节。

`samples.jsonl` 只记录结构特征（地址换成角色、不输出正文、显示名、日期与完整主题），但它仍然来自真实邮件。样本进入仓库之前，必须由维护者逐条审阅并按 4b 的要求转为合成回归样本。

## 配置文件与数据目录

`turncourier init` 是目前唯一读取配置并打开数据库的命令；它在下列位置创建配置文件、数据目录与数据库，收发循环仍未实现：

| 项目 | 位置 |
| --- | --- |
| 配置文件 | `$TURNCOURIER_CONFIG`（须为绝对路径）；未设置时为 `os.UserConfigDir()` 下的 `TurnCourier/turncourier.toml`，macOS 上即 `~/Library/Application Support/TurnCourier/turncourier.toml` |
| 数据目录 | `$TURNCOURIER_DATA_DIR`（须为绝对路径）；未设置时为配置文件所在目录 |
| 数据库 | 数据目录下的 `turncourier.db` |

- 配置示例是 `configs/turncourier.example.toml`，测试会加载它，修改校验规则时同步修改示例。配置只保存账户与选项，出现 `password`、`token` 等凭据类键（不区分大小写）或未知键都会报错，键名区分大小写，`ADDRESS` 这类大小写变体按未知键处理；授权码与两把密钥由 `turncourier init` 写入 Keychain。
- `init` 生成的配置与示例同格式：主机、端口、事件与有效期写默认值，地址按交互输入填写。它以 0600 原子创建配置文件，已存在时从不覆盖，也不修改。
- 配置文件须是不超过 1 MiB 的常规文件；在 Unix 上须归当前用户所有，且组和其他用户不可写。
- 环境变量只能覆盖 `TURNCOURIER_NOTIFY_EVENTS`（逗号分隔，空字符串视为未设置）。白名单、邮箱地址、令牌有效期等安全相关项不接受环境变量覆盖。
- 在 Unix 上，数据目录权限须为 0700、数据库文件须为 0600；缺失时按此权限创建，已有目录或文件权限更宽时拒绝打开。数据库保存元数据、键控正文摘要，以及待处理正文的密文：待发通知在 PENDING 时、回复在 QUEUED 时各存一份 AES-256-GCM 密文，进入终态的同一事务中由触发器删除。数据库不保存明文正文与凭据。
- 不要把数据目录恢复到旧版本的备份。「从不自动重发」和「不重复派发」都以数据库状态连续为前提：旧备份会让已发出的通知回到 PENDING 而重发，让已确认的回复回到 QUEUED 而再次派发。必须恢复时，先人工核对其中的 PENDING 通知与 QUEUED 回复，再启动程序。
- `.gitignore` 已忽略 `turncourier.toml` 与 `*.db`，不要把本机配置或数据库提交进仓库。

## Keychain 条目

授权码与两把密钥保存在 macOS 登录钥匙串的通用密码条目里，由 `internal/security/keychain` 经 `/usr/bin/security` 读写；机密只经子进程的标准输入与标准输出传递，不出现在参数、环境变量、错误文本或日志中。

| 用途 | service | account |
| --- | --- | --- |
| QQ 邮箱授权码 | `io.github.chaorookie.turncourier` | `<实例 ID>:qq-auth-code` |
| 令牌签名密钥 | 同上 | `<实例 ID>:token-key-<kid>` |
| 正文加密密钥 | 同上 | `<实例 ID>:payload-key-<kid>` |

实例 ID 是 16 位小写 Crockford base32，由 `init` 首次运行时生成并写入数据库的 `instance` 表；数据目录不变则实例 ID 不变。把它放进 account 名里，是为了让两个数据目录不会互相覆盖条目。kid 是十进制 1–255，4a 只生成 kid=1，因此 `init` 之后恰好三个条目。

- 查看某个条目是否存在（不打印机密，故意不加 `-w`）：

  ```sh
  security find-generic-password -s io.github.chaorookie.turncourier -a <account>
  ```

  实例 ID 由 `turncourier init` 在摘要中打印；数据目录已存在时重新运行 `init` 会打印同一个 ID，不会重新生成。
- 删除数据目录不会删除钥匙串条目。遗留条目按 account 逐个手工删除：

  ```sh
  security delete-generic-password -s io.github.chaorookie.turncourier -a <account>
  ```

- 密钥条目只以「不覆盖」的方式创建（`Add`，即不带 `-U` 的 `add-generic-password`），从不经 `Set` 覆盖。授权码仍用 `Set`（带 `-U`），因为它本来就需要更换。
- 数据库的 `crypto_keys` 表只登记用途、kid、状态与 8 字节校验值，不保存密钥材料。`init` 遇到下面两种不一致时拒绝继续，而不是重新生成或覆盖：
  - **有元数据但 Keychain 中缺少条目：** 重新生成会得到一把不同的密钥，数据库中待处理的密文将永远无法解密，键控摘要也对不上；
  - **条目与登记的校验值不符：** 说明 Keychain 中的这把密钥已被替换，覆盖它同样会让已有密文作废，而且会掩盖替换这一事实。

  两种情况都要人工处理：确认没有待处理数据后，按上面的命令删除相关条目与数据目录，再重新运行 `init`。Keychain 中已有同名但格式不符的条目时，`init` 也只提示按本节手工删除后重试。
- 威胁模型：经 `security` 创建的条目，受信任应用是 `/usr/bin/security`，同一用户下的任何进程（包括 Agent 执行的命令，只要其沙箱放行）都能静默读出这三个条目，读出后可以伪造通过全部校验的回复并改写本地队列。详见[设计文档](design.md)与 [SECURITY.md](../../SECURITY.md)。请使用专用的机器人邮箱，怀疑泄露时在 QQ 邮箱中停用该授权码。
- 开发与测试从不接触真实钥匙串：单元测试让测试二进制扮演假的 `security` 程序（见[测试约定](#测试约定)），真实钥匙串只出现在 `tests/live` 中由维护者人工执行的探测里。

## 隐私与凭据

- 不要在 issue、PR、提交、测试或日志中提交 QQ 邮箱授权码、令牌、真实邮件内容、Agent 会话记录或本机绝对路径。复现问题时优先使用合成数据。
- 授权码由 `turncourier init` 以不回显方式读入并写进 macOS Keychain，同时生成令牌签名密钥与正文加密密钥，三者都不进入配置文件或仓库。条目布局、查看与删除方式见[Keychain 条目](#keychain-条目)。
- 机密形状的测试向量一律在运行时构造（用 `strings.Repeat`、`bytes.Repeat` 生成低熵值，或按下标填充字节数组后再做十六进制、base64、base32 编码），源码中不写这类字符串字面量。必须出现的 account 名 `qq-auth-code` 本身就含 `auth`，参数列表中的逗号即可充当分隔符，只让变量名避开关键词不够。写有这类向量的改动在提交前运行 `make secrets`：扫描覆盖全部 Git 历史并拒绝 `.gitleaksignore`，这类字面量一旦提交就只能改写分支历史。
- `.gitignore` 已经忽略 `.env`、`.env.*`、`*.db`、`*.sqlite*`、`*.log`、`turncourier.toml`、`.local/`、`dist/`、`coverage.out`、`state.json`、`samples.jsonl` 等文件。不要用 `git add -f` 强行加入。
- 公共 CI 不运行真实邮箱或模型调用，也不保存凭据。真实邮箱与真实钥匙串只出现在 `tests/live` 中由维护者人工执行的探测里，见[真机探测](#真机探测testslive)。
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
- 其中的二进制只有 `help`、`version`、`doctor` 与 `init`，不能收发邮件，也没有经过任何邮件验收。
- 本地 `make build` 产出的 `0.1.0-dev` 同样不是发布版本。

完成真实邮箱验收之前，不打 `v0.1.0-alpha` 标签。按设计，发布前 Codex 和 Claude Code 各要完成连续十轮真实邮件交互，覆盖断网恢复、重复、伪造和过期邮件、忙碌时的 FIFO 顺序以及隐私过滤；编译成功不能代替真实验收。目前没有任何发布版本，默认分支为 `main`。
