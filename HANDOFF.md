# HANDOFF

**目标**：将 TurnCourier 建成公开开源的 Codex / Claude Code 邮件接续工具；Phase 3（配置、存储与状态机）、Phase 4a（邮件闭环的离线实现）与 L1 真机探测已完成，4b 任务契约已起草并经维护者确认（D6–D8，2026-09-24），当前处于 4b 逐任务实施之中：Task 1–7 已实现并合入工作分支，其中 Task 1、2 已全部完成（含独立复查），Task 3–7 还有审查、修复或复查没做完（见「未完成」）；维护者 2026-09-24 因额度用完暂停了本轮工作。L1b 待维护者执行。
**更新于**：2026-09-24 · claude-code（第二个云端会话，暂停前）
**项目目录**：本仓库根目录（含 `go.mod` 的目录）
**基线 commit**：main `2afc52c`（PR #15 合并：L1b 探测工具、4b 任务契约与 D6–D8 的确认；合并后 main 的 CI 与 Security 成功）。4b 的实现提交在分支 `claude/project-handoff-continuation-wt9hh9` 上逐个推送（暂停时为 `8855c62` 之后的交接提交），尚未开 PR（4b 的 PR 要等 Task 18 与 L1b）。以 `git log --oneline -3` 与 `git status --short` 核对。
**暂停原因**：维护者额度用完，主动暂停；没有技术阻塞。4b 正按 Task 1–18 推进，每个任务的实施说明、审查发现与修正记录写在 phase-04.md 对应任务之下（「尚未进行」的步骤都写明了）。等维护者的有两件事：一是 Task 3–4 审查发现的 B 组（解析规则的七处取舍，须在冻结解析规则之前决定，见 phase-04.md Task 4 之下）；二是在本机按「L1b」一节执行真机补采（新主题格式往返复核、别名与 `Return-Path` 样本）；它不阻塞 4b 的实现，但 4b 的 PR 合并之前必须完成。探测工具已为 L1b 改好。

## 已完成

- [x] 用户批准设计、Phase 0–1 与 Phase 2；公开目标为个人账户 `chaoRookie` 下的 `turncourier`。Phase 0–1 详见 `docs/zh-CN/research/phase-01.md`，无需重复验证。
- [x] Go 1.27.1 工具链位于被忽略的 `.local/toolchains/go`，SHA-256 已校验；质量工具按版本装在 `.local/bin/<tool>-<version>/`。
- [x] Phase 2：CLI（help/version/doctor 及 `--json`）、doctor、commentcheck、covercheck、Makefile、三个工作流、Dependabot、密钥扫描脚本、治理文档与双语文档；多维审查后修复。详见 `docs/zh-CN/plans/phase-02.md`。
- [x] 公开仓库 https://github.com/chaoRookie/turncourier ；私密漏洞报告已启用；main 设必需检查；Dependabot PR #1–#3 已评审合并。
- [x] Phase 3（PR #6–#9）：状态机、严格配置、SQLite 存储（迁移、去重、FIFO、崩溃后转 UNCERTAIN 且永不自动重发）、集成测试与文档。验证记录见 `docs/zh-CN/plans/phase-03.md` 末尾。
- [x] Phase 4a（PR #12，合并为 `4f646c6`）：Keychain 封装、`turncourier init`、回复令牌、正文加密、迁移 `0002`、待发通知状态机与存储、SMTP 与 IMAP 客户端及离线假服务器、`tests/live` 探测工具与文档同步。
- [x] L1（PR #13、#14，2026-09-20 至 21，维护者本机）：第 1–5 步全部完成，并补采了假期自动回复与退信样本。结论见 `docs/zh-CN/plans/phase-04.md`「L1 结果」：QQ 改写 Message-ID（实际投递 ID 取自「已发送」副本）；D4 定稿为令牌放主题标签、只接受最新通知的令牌、`TokenTTL` 默认 72h；QQ 假期自动回复不带任何头部信号，维护者确认按主题前缀识别，并另设回环刹车；退信有两条强信号；两个 Agent 的 shell 都能静默读出 Keychain（D3 由推断变为实测）。
- [x] 2026-09-24（第一个云端会话）：
  - 探测工具为 L1b 修改（`tests/live`）：令牌进入主题标签、主题以原始 ASCII 标签开头不被折断、样本记录标签状态与 `Return-Path`、自动回复前缀（含全角冒号与「自动答复」）、逐封确认不回显标签，被拒邮件另查「已发送」副本（核实 D7 的前提；副本记录带 `searched`，把「补扫失败」与「确实没有副本」分开），样本格式 `turncourier-l1/4`。经独立质量审查、修复与两轮复查：第一轮 57 个变异、第二轮 18 个变异全部被杀死；第二轮另把「`]` 换成全角括号等」与「插入或替换多个字符」从截断改判为改写。契约、规程与审查记录见 phase-04.md「L1b」。
  - 起草「4b 任务契约」（phase-04.md）：新决策 D6–D8、已定的实现细节、门槛、文件结构、依赖方向、Task 1–18、完成标准。两名独立审查子代理（一致性与可行性、安全与正确性）共提出约 30 条意见，全部采纳，要点：
    - 任何一封来信都不能卡住收取循环（单封错误映射为原因码、已拒绝的不翻案、首次运行跳过历史）；
    - D6 改为「有证据才因缺少副本拒绝」，「已发送」不降级；
    - D7 在同一事务中核对并记录，「最新通知」包括 UNCERTAIN、按发出时刻；
    - D8 恢复 D4 的持久暂停，另加 24 小时上限拦慢循环，发信上限改为每小时 40 封；
    - 纯状态模式的主题不含 Agent 文本；过滤规则补强；发送循环遇限流整体暂停，坏数据不阻塞发送。

    修订经独立复查。复查又提出 4 条次要、7 条细节，也全部采纳（提交 `e0f5d3f`），要点：
    - 领取之后的每一次状态写入失败都先重试、再让 `Run` 返回错误，停在 SENDING 的通知不会让发送悄悄停摆；
    - 存储的校验错误一律包装 `ErrInvalidArgument`，只有它按「找不到」处理，数据库的短暂失败不会让「已发送」副本被跳过；
    - D7 的「最新」按发出时刻，核对不改变发出时刻；「已发送」补扫 10 分钟后仍没有副本的 UNCERTAIN 不再让上一条通知失效；
    - 发送循环按全局的连续失败次数指数暂停，5xx 在同一通知第 3 次尝试时才放弃，提交阶段的 550 也提示检查收件地址；
    - UIDVALIDITY 重置后拒绝不翻案，暂停解除后重新计数，「已发送」缺失按补扫失败告警，机密经 `Scrubber` 接口交给渲染器。

    第三次复查确认上述 11 条全部修复，又提出 2 条次要（1 条是此前漏掉的）与 7 条细节，也全部采纳（提交 `0787971`）：较新的通知仍是没有定论的 UNCERTAIN 时，对上一条通知的回复先延后、等副本或缺失证据出现再判，不再直接以 `token_superseded` 永久拒绝；「wrote:」引用头之后的行内逐段回复同样判为不确定；其余是「已发送」副本中的非法 ID、抹除机密不随授权码缓存清空、提交阶段 550 的告警措辞、证据时刻与存储共用一个时钟、核心接口清单、保留期与令牌有效期上限的依赖、哨兵改名为 `ErrInvalidArgument`。最后一次复查确认这 9 条全部修复，只余两处措辞（`fb9c828` 已改），4b 契约没有未处理的审查意见。
  - 文档同步：design.md 状态行与「接收通知的邮箱不要开启假期自动回复」；README 中英、architecture.md、development.md、CHANGELOG 的过时状态；architecture.md 中「令牌默认七天有效」「主题与页脚的令牌须一致」两处过时说法（代码默认值自 L1 起就是 72 小时，且只读主题）。
- [x] 2026-09-24（第二个云端会话）：维护者确认 D6–D8，均采用建议方案（含 D8 对 D4 字面的那一处解释：M 超出时推迟发信并告警，不暂停任务）。确认记入 phase-04.md 末尾「已确认事项」第 2 条与 4b 各状态行；design.md、README 中英、architecture.md 与 CHANGELOG 中「等待确认」的说法同步改为已确认。经维护者同意开 PR #15 合并入 main（squash，`2afc52c`），同一 PR 补上 README 树形图漏列的 `tests/docs/imports_test.go`。
- [x] 4b Task 1（主题标签，`internal/security/token`）：实现、规格与质量审查、修复、独立复查与细节处理全部完成（`1878181`、`1a2b17a`、`567deff`）。审查补出的要点：守卫测试原先跳过以「.」「_」开头的目录与 `testdata`（显式导入时照样编进产品），现在一律扫描；`deps_test.go`、`imports_test.go` 同理改为只跳过根目录下的这类目录；`%w` 与 `%p` 一样会绕过脱敏，写进注释；解析出的任务 ID 不再与主题共用内存。
- [x] 4a 遗留缺陷修复（`5215fb5`）：服务器在正文字面量中途以 close_notify 正常关闭时，go-imap 把 `io.EOF` 当作字面量结束，`Session.body` 随后调用 `Next` 与解码协程争用读缓冲（数据竞争，负载高时 `make check` 会报）。读到的字节少于声明长度即按连接断开处理；假服务器新增 `faultCloseAfter`。
- [x] 4b Task 2（存储补充）：实现（`8006f73`）、修复（`25bc695`）与修复复查的细节（`e2a7d77`）都已合入，全部完成。修复要点：并入 6 个用例拦下 7 个存活变异；本地人工核对也以进入 UNCERTAIN 的时刻为 `sent_at`；`AbandonedSince`、`SentWithoutDeliveredID` 改为游标分页。审查意见中属于后续任务用法的（Task 5、9、10、11、13）已写入对应契约（`06ca434`、`1bde490`）。
- [x] 4b Task 3、4（解析器与合成样本、自动回复与退信判定）：实现已合入（`91377e4`、`291abe5`）；规格审查（修复后合入）与质量审查（可以合入）都已完成，发现记在 phase-04.md Task 4 之下：A 组 8 条直接修，B 组 7 条待维护者决定。修复与独立复查未做。
- [x] 4b Task 5（渲染器、内容过滤与 `notify.content`）：实现已合入（`360dbfc`），142 个变异中 139 个被拦下、3 个等价；实现中发现并修复一处私钥正文可能漏过过滤的顺序问题（PEM 规则提前到围栏之后）。规格与质量审查、独立复查都未做。
- [x] 4b Task 6（IMAP 扩展）：实现（`5003bd9`）、规格与质量审查、修复（`2e732d2`）都已合入。修复的要点是质量审查的一处主要问题：「已发送」中列出却取不到头部的副本原先被越过、随后仍算作一次完整补扫，会凭空产生 D6 的缺失证据，现在按本轮失败处理。修复的独立复查未做。审查中属于后续任务的约束已写入 Task 10、13、16 的契约与「风险与后续」（`8f22c01`）。另更正了 `5215fb5` 提交说明中「每次都报告竞争」的说法（实测不稳定，已由纯函数测试 `TestReadLiteral` 确定地钉住）。
- [x] 4b Task 7（QQ 行为模拟器 `tests/qqsim`）：实现已合入（`ca9d412`），29 个变异全部被拦下；规格审查已完成（修复后合入：1 条次要、6 条细节，记在 phase-04.md Task 7 之下），质量审查在暂停时仍在进行。

## 未完成

- [ ] **L1b（维护者本机执行）**：按 phase-04.md「L1b」一节的规程，新建 0700 输出目录，发 1 封新格式通知，用 QQ 邮箱 App 与网页版各回复一次（有别名的再用别名回复一次，可选 Foxmail/Apple Mail/Gmail），运行 `TestL1Replies`，把结论写进新增的「L1b 结果」一节。另有两个可选步骤：向同域不存在的地址发一封以核实 D7 的前提；再开一次假期自动回复看它的回复频率。L1b 是 4b Task 1 冻结主题解析器的门槛，4b 的 PR 合并之前必须完成。
- [ ] **4b 实现（暂停中）**：恢复时按这个顺序处理已有的积压，再往下推进——
  1. Task 7：规格审查已完成，发现记在 phase-04.md Task 7 之下（1 条次要：文件夹名须以 L1 实测的字面量钉住；6 条细节）；质量审查的结果若没有记在那里，就重新做质量审查 → 修复 → 独立复查。
  2. Task 3–4 的 A 组修复（phase-04.md Task 4 之下「审查发现」，质量审查的测试草稿条目已抄在那里）→ 独立复查；B 组先请维护者逐条决定，定了再改。
  3. Task 5 的规格与质量审查 → 修复 → 独立复查（核对 Task 3 审查要求的页脚整句断言）。
  4. Task 6 修复（`2e732d2`）的独立复查。
  5. Task 8（单实例锁与密钥读取，新建 `internal/app`）：`secrets.go` 导入存储，所以产品代码不能导入 `internal/mail/renderer`（`tests/docs/imports_test.go` 此时仍禁止同时导入邮件与存储，Task 9 才放开）；`*Secrets` 以方法集满足 `renderer.Scrubber`，编译期断言与「放进 `renderer.Options` 后打印」的测试写在 `_test.go` 里。
  6. 之后 Task 9–13（`internal/app`，基本是串行依赖：9 用 8 的密钥，11 用 5、7、8，13 装配全部）、14–18。

  做法：每个任务由子代理在 `.local/worktrees/` 下的独立 worktree 中实现（先写失败的测试），再由规格与质量两名审查子代理复核（含变异测试），修复后独立复查；主会话按顺序把提交 cherry-pick 到本分支，合入后跑一次 `make check` 再推送。阶段末整阶段审查后开 PR；合并后维护者执行 L2 验收。
- [ ] macOS 断电持久性（`fullfsync`）留到安全与恢复阶段评估。
- [ ] 关注 actions/setup-go 补丁版本：7.0.0 打包的 undici、brace-expansion 有已公开安全公告（旧版同样受影响，本仓库输入不触及），上游已修复未发版；Dependabot 提出后按同样流程评审。
- [ ] Codex、Claude 适配器与后台服务属于后续阶段。真实邮箱各十轮验收之前不打 `v0.1.0-alpha`。

## 下一步

1. 读 `AGENTS.md`，核对 `git status --short`、`git log --oneline -3` 与 `gh run list --repo chaoRookie/turncourier --limit 5`。L1b 直接在 main 上执行（探测工具已随 PR #15 合并）。
2. `export PATH="$PWD/.local/toolchains/go/bin:$PATH"`，运行下方验证命令，确认仍全部通过。
3. 从 phase-04.md 中各任务之下的记录判断进度：有「实施说明」与「审查后的修正」且步骤已勾选的任务已完成；其余按顺序继续。每完成一个任务，把实施说明与验证结果写进 phase-04.md 对应任务之下，并更新本文件。
4. 若维护者已执行 L1b：把样本结论写进「L1b 结果」，按 4b「门槛」判断 Task 1 的文法能否冻结。

## 验证方式

```sh
export PATH="$PWD/.local/toolchains/go/bin:$PATH"
make check
make security
make workflows
make build
GOOS=linux go vet ./...
GOOS=windows go vet ./...
CGO_ENABLED=0 go test ./...
go version -m dist/turncourier
./dist/turncourier help
./dist/turncourier version
./dist/turncourier doctor --json
git diff --check
```

期望：
- `make check` 通过，覆盖率 >= 80%；
- gitleaks 与 govulncheck 无发现，actionlint 通过；
- `go version -m` 列出 `BurntSushi/toml`、`modernc.org/sqlite`、`golang.org/x/term`、`golang.org/x/sys`（4a 的 `init` 起链接），**不含** go-imap、go-smtp、go-message、go-sasl 与 `golang.org/x/text`（4b 接入命令后才进入）；
- `doctor --json` 在装有 git/codex/claude 的 macOS 上返回 0；在 Linux 上返回 1（平台不受支持）属预期。doctor 只检查 OS 与三个工具的 `--version`，不证明登录、邮箱或会话能力。

### 验证记录

- 2026-09-24 云端（第一个云端会话，分支 `claude/upbeat-lamport-jw9yo9`）：
  - 基线（main `7f5fcc6`）：`make check` 通过（覆盖率 93.2062%，2785/2988；staticcheck 须以 go1.27.1 重装，见「坑」）；`make secrets` 无泄漏；`make workflows`、`make build`、`GOOS=linux go vet ./...`、`GOOS=windows go vet ./...`、`CGO_ENABLED=0 go test ./...` 通过；`go version -m` 只列出 toml、x/term、sqlite 及其依赖，没有邮件模块；`help`、`version` 退出 0，Linux 上 `doctor --json` 退出 1（平台不受支持，属预期）。**govulncheck 未运行**：云端网络策略拒绝 vuln.go.dev。
  - 探测工具的修改与审查修复之后：`make check` 通过（覆盖率 93.38%）、`make secrets`、`go vet -tags live ./tests/live/`、两个平台的 vet 通过；变异测试 57 个全部被杀死；没有运行任何真机探测，没有登录邮箱、没有发信，也没有读写钥匙串。
  - 探测工具第二轮修复之后：`make check` 通过（覆盖率 93.39%）、`go test -cover ./tests/live/`（92.4%）、`make secrets`、`go vet -tags live ./tests/live/`、两个平台的 vet 通过；本轮 18 个变异全部被杀死；三个探测文件中没有不可见字符。
  - 远端：本分支没有开 PR，推送不会自动触发 CI，改为手动触发。`80a8627` 上 CI（run 35957494468）与 Security（run 35957496330）成功；最后一次改动代码的提交 `c5bcd3d` 之后，在 `e0f5d3f` 上再次触发 CI（run 35959190404：`quality (ubuntu-24.04)`、`quality (macos-15)`，含 CLI 冒烟与 actionlint）与 Security（run 35959192725：gitleaks 与 govulncheck），均成功。这补上了云端无法运行的 macOS 测试与 govulncheck。之后的提交只改文档。
- 2026-09-24 云端（第二个云端会话，分支 `claude/project-handoff-continuation-wt9hh9`）：每个任务的提交拣选到分支后都在合并后的代码上跑一次 `make check` 再推送，全部通过，总覆盖率依次为：Task 6 合入后 94.26%、Task 7 合入后 94.65%、Task 6 修复合入后 94.61%、Task 5 合入后 94.90%（4462/4702）；每次推送前 `make secrets` 无泄漏。各任务另跑的 `-race` 重复、两个平台的 vet、模糊测试与变异测试记在 phase-04.md 对应任务之下。**govulncheck 未运行**（云端网络策略拒绝 vuln.go.dev），本分支也还没有在远端触发 CI 与 Security。
- 2026-09-17 本地：`make check` 通过（覆盖率 97.35%，294/302）；`make security`、`make workflows`、`make build` 通过；`GOOS=linux/windows go vet ./...` 通过；冒烟退出码符合契约。
- Actions 固定 SHA 经 GitHub API 核对：公开时为 checkout v5.1.0、setup-go v6.5.0、upload-artifact v4.6.2；2026-09-18 升级为 checkout v7.0.1、setup-go v7.0.0、upload-artifact v7.0.1。
- 2026-09-17 远端：首次推送的 CI（`quality (ubuntu-24.04)`、`quality (macos-15)`）与 Security（`security`）成功；私密漏洞报告 `enabled: true`；GitHub 识别许可证 Apache-2.0。
- 2026-09-18 远端：Actions 升级合并后 main `9cc5bd7` 的 CI、Security 与 Candidate build 成功；下载产物 `shasum -c` 通过，可执行位保留，arm64 二进制版本为 `0.1.0-dev.9cc5bd7`。
- 2026-09-20 至 21 L1：维护者本机执行第 1–5 步，全部通过；样本写入仓库之外的 0700 目录，未进仓库。合成钥匙串条目检查后已删除，无残留。
- 2026-09-19 Phase 4a：本地 `make check`（覆盖率约 93.5%）、`make security`、`make workflows`、`make build`、两个平台的 vet 与 `CGO_ENABLED=0` 测试通过；PR #12 三项检查通过；合并后 main `4f646c6` 的 CI 与 Security 成功。
- 2026-09-19 Phase 3：本地 `make check`（覆盖率 93.28%，985/1056）、`make security`、`make workflows`、`make build`、两个平台的 vet 与 `CGO_ENABLED=0` 测试通过；PR #9 三项检查通过（质量任务约 2 分钟）；合并后 main `e046ff4` 的 CI 与 Security 成功。

## 关键文件

- `docs/zh-CN/design.md` — 产品范围、开发顺序、安全和持久化边界。
- `docs/zh-CN/plans/phase-04.md` — Phase 4：D1–D5、4a 契约与验证、L1 结果、L1b、4b 任务契约（D6–D8 已确认）与各任务的实施说明、审查发现和修正记录。
- `docs/zh-CN/plans/phase-03.md` — Phase 3 清单、已确认决策（D1、D2）、审查后修正与验证记录。
- `docs/zh-CN/development.md` — 工具链、make 目标、依赖政策、测试约定、真机探测。
- `docs/en/architecture.md` — 包依赖方向、持久化与恢复语义、已知局限。
- `internal/cli/cli.go` — 命令路由与退出码契约（0/1/2，管道断开按 SIGPIPE 惯例）。
- `internal/store/sqlite/` — 存储；`migrations/0001_init.sql` 与 `0002_mail.sql` 已发布，不得修改，改表结构只能写新迁移。
- `tests/live/` — L1/L1b 探测工具；`sample.go` 是不带构建标签、在 CI 中测试的纯函数。
- `Makefile`、`scripts/scan-secrets.sh`、`.github/workflows/` — 本地与 CI 共用门槛。

## 坑 / 已排除的方向

- 按已批准设计的顺序推进：配置/存储/状态机 → 邮件闭环 → Codex → Claude → 安全与恢复 → 真实邮箱验收 → 发布。不要为了让维护者早点试收发邮件而压缩阶段或做「最小闭环」演示；维护者问时间点不等于要求改优先级。每阶段先写清单、确认决策，再逐任务实施和审查。
- 不再围绕已批准设计做选择题问答；常规实现决定自行完成。对外发布动作（推送、开或合并 PR、改仓库设置）仍需用户在当次对话确认。
- `make secrets` 扫描全部历史（含本地未推送分支）。清单或测试里不要写密钥形状的字面量，测试向量在运行时构造；推送前先跑 `make secrets`，命中只存在于未推送提交时，压缩或改写这些本地提交后再推送。
- 本机 Go 与质量工具下载很慢，已安装的不要重复下载；modernc.org/sqlite 首次下载约 11 分钟。Makefile 以版本目录判断是否需要安装。
- **云端容器（claude.ai/code）**：系统自带的 `/usr/local/go` 是 1.24，进入仓库后按 `go.mod` 自动下载 1.27.1 工具链；本次把它软链到 `.local/toolchains/go`，照常 `export PATH`。但 `make` 首次安装质量工具时 `go install tool@version` 不看本仓库的 `go.mod`，会自动换成 go1.26.8 编译 staticcheck，而它解析不了 Go 1.27 标准库中的泛型方法（报 `method must have no type parameters`）；须以 `GOTOOLCHAIN=go1.27.1` 重装。云端的网络策略拒绝 `vuln.go.dev`，govulncheck 在云端无法运行（不要绕过），交给远端 Security 工作流。
- BurntSushi/toml 在精确匹配失败时按不区分大小写把键匹配到字段，并记为已解码，`Undecoded()` 看不到这些变体；配置改用 `MetaData.Keys()` 对照白名单。新增配置字段时必须同步 `knownKeys`。
- `RecoverInFlight` 把全部 DISPATCHING 当作崩溃遗留，只能由唯一的派发进程在开始派发前调用；CLI 等其他进程不得调用。
- 「未送达」重新排队只允许在任务自派发以来没有其他事件时进行，否则必须核对为已送达，这是防重复投递的关键判定，不要放宽。
- gitleaks 仓库为 gitleaks/gitleaks，Go 模块路径仍为 `github.com/zricethezav/gitleaks/v8`。仓库根目录不得添加 `.gitleaks.toml` 或 `.gitleaksignore`，扫描脚本会拒绝运行。
- 审查中推翻的条目不要重复处理：生成文件豁免是计划要求；upload-artifact 的 zip 保留 755，无需额外打 tar 包。
- actionlint 不校验按 SHA 固定的 action 的 `with` 输入；升级 Action 时须对比新旧 `action.yml`，build.yml 只能手动触发 Candidate build 验证。
- main 分支保护要求分支最新（strict），必需检查为 `quality (ubuntu-24.04)`、`quality (macos-15)`、`security`；修改工作流 job 名称时须同步更新分支保护。多个 PR 需逐个「更新分支 → 等三项检查 → 合并」。
- Git 全局未配置提交身份。提交使用单次配置：`git -c user.name=chaoRookie -c user.email=179816769+chaoRookie@users.noreply.github.com commit ...`；不要改全局身份。
- 文档、提交与续点中不要写本机绝对路径、用户名或订阅档位；gitleaks 默认规则不会拦截这类信息。
- 所有 QQ 凭据通过本地 `turncourier init` 输入，不发送到聊天、不进入测试或 Git。真实邮箱地址同样不写进文档、提交与续点。worktree 不是 OS 沙箱，邮件文字不能绕过 Agent 权限。
- QQ 邮箱新版把 IMAP/SMTP 开关挪到了「设置 → 账号与安全」，不在左侧设置列表里；授权码粘贴时常带空格或换行，`init` 会以「不含空白的可打印 ASCII」为由拒绝。IMAP 登录被拒时 `internal/mail/imap` 按设计不保留服务器响应文本，查不到具体原因，先核对 IMAP 服务是否真的开启、再重录授权码。
- L1 已证实的三条，实施 4b 时不要再当作未知：QQ 会改写发出邮件的 Message-ID（`X-OQ-MSGID` 保留我方 ID，实际投递 ID 从「已发送」副本取）；机器人自己的 Junk 里确实会落邮件，Junk 补扫是必需路径；同域投递不带 `Received` 与 `Authentication-Results`。另有一条是维护者人工观察而非探测实测：通知会落进**收件人**的垃圾箱——那只影响送达率，别拿它论证机器人端的 Junk 补扫。
- 回复主题的前缀至少有 `Re:` 与 `回复：` 两种（成因未确定，可能是客户端差异，也可能是对方的主题设置），主题解析器不能按前缀白名单匹配，只能按 `[TC …]` 标签本身定位。
- 令牌改到主题之后，自动回复与退信的过滤从纵深防御变成承重规则：白名单地址的假期自动回复会把带令牌的主题原样带回，四项校验全过。过滤必须排在验证顺序最前面；QQ 的假期自动回复只能按主题前缀识别（已确认的破例），另有回环刹车兜底。
- 同域不存在的地址在 SMTP 提交时就被 QQ 以 550 拒绝（`ErrRejected`），不产生退信；退信样本要用外域的保留域名。
- **并行子代理的 worktree**：放在被忽略的 `.local/worktrees/<名字>`（`git worktree add -b task/N .local/worktrees/tN <基点>`），子代理在里面运行 make 时加 `TOOL_BIN=<仓库>/.local/bin`，否则会重新下载并用错 Go 版本编译 staticcheck。不要用 Agent 工具的 `isolation: "worktree"`：它从旧的 main 建 worktree、放在未跟踪的 `.claude/worktrees/` 下（本机已把 `/.claude/` 写进 `.git/info/exclude`），子代理在里面 `git merge` 还可能被权限分类器拒绝。
- **cherry-pick 与 rebase 也要带单次身份**：`git -c user.name=chaoRookie -c user.email=179816769+chaoRookie@users.noreply.github.com cherry-pick …`，否则提交者会变成容器默认的身份（`1878181`、`8006f73` 两个提交就是这样，已推送，未改写）。
- **结束一轮对话前工作区必须干净且已推送**（云端环境的停止钩子会检查）：子代理不要在主目录里改文件，主会话的改动先提交再结束。
- **go-imap 的字面量读取器把中途的 `io.EOF` 当作字面量结束**：凡是「部分读取字面量、再由 `Next` 丢弃其余部分」的写法，都要先确认读到的字节数达到声明长度（或上限），否则与解码协程争用读缓冲；Task 6 的 `ScanHeaders` 同样适用。
- **worktree 里的分支只在本地**：云端容器被回收时，`.local/worktrees/` 与其中的 `task/*`、`fix/*` 分支一起消失。子代理的提交要拣选到工作分支并推送之后才算保住；审查报告与测试草稿放在会话的临时目录里，也会随会话结束消失，所以暂停前要把结论写进 phase-04.md（本次已这样做）。
- **云端的停止钩子会提示「Unverified」**：它要求提交者邮箱为 `noreply@anthropic.com` 并建议改写提交。本仓库的约定是上面那个单次身份（维护者选定），所以没有照做，GitHub 上显示 Unverified 是预期的。若维护者改变主意，只改以后的提交，不改写已推送的历史。
- **`tests/qqsim` 的 `Deliver` 有一处限制**：imapmemserver 取回邮件时会重新解析头部，头部无法解析的字节经 IMAP 取回是空的。需要投递畸形头部的测试（例如 Task 9 的畸形来信）直接构造 `imap.Batch`。
- 在 Go 源码里写 `"\u200b"`、`"\ufffd"` 这类转义时，编辑工具可能把它们直接写成不可见字符的原文（本次有 4 行测试源码如此，提交前扫描发现并改回转义）。改动含这类转义的文件后，提交前扫一遍 Unicode 类别为 Cf、Co、Cs 的字符与 U+FFFD。
