# Phase 0–1 验证报告

验证日期：2026-09-17（Asia/Shanghai）。范围：保存批准设计、竞品源码及离线体验、Codex/Claude 原生会话接口验证。结论：可以进入工程骨架阶段；尚未构成真实邮件控制产品。

## 核心结果

| 项目 | 实际证据 | 结论边界 |
| --- | --- | --- |
| Codex 原生协议 | 本机 0.154.0，实际握手、thread/start、turn/start、turn/completed | 已运行，不只是查帮助 |
| Codex 跨进程恢复 | 结束第一个服务，第二个服务 thread/resume；thread ID 不变，第二轮回忆合成标记，持久化历史至少两轮 | 已通过；不代表已接管桌面任务 |
| Claude 跨进程恢复 | 本机 2.1.263，stream-json 第一轮结果携带 session_id；新进程 --resume 后 ID 相同且能回忆合成标记 | 复测通过；首次尝试第二轮异常退出，原因尚未确定 |
| QQ 邮件连接 | imap.qq.com:993 与 smtp.qq.com:465，证书验证开启，均 TLSv1.3 成功 | 仅无登录握手；未收发真实邮件 |
| 竞品离线体验 | 固定提交的 7 个纯函数用例，3 项符合期望，4 项暴露输入处理缺陷 | 没有安装到个人 Agent，也未体验真实邮件闭环 |
| 桌面、阻塞问题、Hooks | 本地 schema 和官方文档提供候选入口 | 未完成实际兼容性验收，不对外承诺支持 |

测试环境：macOS arm64、Node.js v25.9.0、Codex 0.154.0、Claude Code 2.1.263。Codex 使用 ChatGPT 登录，Claude 使用 claude.ai 订阅登录。未新增或读取 API Key 内容；真实回合消耗现有订阅额度。Go 不在当前 PATH 中，当前研究不需要安装，工程实现前需准备工具链。

## Phase 0：竞品

研究对象：[JessyTsui/Claude-Code-Remote](https://github.com/JessyTsui/Claude-Code-Remote)，MIT，固定提交 `cd172b2f3361ccb60d7e691dcca768980a2eb13d`，提交日期 2025-12-06。该版本包含邮件、Telegram、LINE、桌面通知，邮箱队列和已处理邮件记录。

前文“QQ 是差异点”的判断应撤回：[`claude-remote.js:635`](https://github.com/JessyTsui/Claude-Code-Remote/blob/cd172b2f3361ccb60d7e691dcca768980a2eb13d/claude-remote.js#L635) 已有 QQ 提供商分支，配置 SMTP 587、IMAP 993。也不能声称竞品完全没有队列或去重；源码中已有 JSON 队列状态和已处理 ID。

实际差异应是跨 Codex/Claude 的原生会话契约、可靠恢复、明确的权限边界、正确的邮箱解析与可复现测试。是否更可靠必须由测试证实，不能从语言或结构推断。

### 可复现的观察

运行 `competitor-probe.mjs`，输入全部为合成 `.invalid` 地址和普通文字：

| 用例 | 竞品结果 | TurnCourier 的要求 |
| --- | --- | --- |
| 精确白名单地址 | 接受 | 接受 |
| 包含白名单地址的长地址 | 接受 | 拒绝 |
| 显示名含白名单、实际邮箱不同 | 接受 | 解析实际 mailbox 后拒绝 |
| 标准主题提取 token | 正确 | 保留能力但增加签名与任务归属 |
| 删除 `>` 引用段 | 正确 | 覆盖更多 QQ 客户端格式 |
| `哈哈哈哈` | 被改成 `哈` | 保留原意 |
| `yes` 加换行与另一条指令 | `yes` 被删除 | 不按语义词表删除合法指令 |

源码调用处 [`relay-pty.js:428`](https://github.com/JessyTsui/Claude-Code-Remote/blob/cd172b2f3361ccb60d7e691dcca768980a2eb13d/src/relay/relay-pty.js#L428) 传入 `parsed.from.text`，匹配函数使用 `includes`。因此这是所检查路径的实际输入校验弱点；是否能完成整条攻击链仍受令牌等其他条件限制，本研究未作真实攻击测试。

[`relay-pty.js:190`](https://github.com/JessyTsui/Claude-Code-Remote/blob/cd172b2f3361ccb60d7e691dcca768980a2eb13d/src/relay/relay-pty.js#L190) 按重复字符模式压缩正文，不适合保留用户 Prompt。命令注入路径优先 tmux 并回退至其他终端方式。所检查源码会记录部分命令、令牌或正文调试内容；TurnCourier 默认只记状态元数据。

研究没有复制上游实现进产品，也没有运行安装器、终端注入或配置个人 Hooks。研究脚本运行时从用户指定 checkout 提取纯函数；未来若复用 MIT 源码，需保留对应版权和许可，不能仅覆盖为 Apache-2.0。

## Phase 1：Codex

以已安装二进制生成 JSON Schema，确认以下接口存在：

- `initialize`/`initialized`、`thread/start`、`thread/resume`、`thread/read`。
- `turn/start`、`turn/completed`、`item/completed`。
- `item/tool/requestUserInput`，以及 commandExecution、fileChange、permissions 的审批请求。

实测流程：未握手请求被拒绝 → 握手 → 创建只读测试会话 → 第一轮记住合成标记 → 收到 completed → 关闭进程 → 新进程握手并恢复原 thread → 第二轮最终回答复述标记 → 读取到至少两轮历史。

最终严格检查选择 `agentMessage.phase=final_answer`，两轮都通过，不以中间 commentary 代替最终答案。共执行两次无推理握手检查和两次两轮 live 试验；第二次 live 用于验证更严格的最终消息检查。探针终止自己创建的服务，CLI 保存的合成历史保留。

工程选择：自管 stdio app-server，以明确的 thread ID 驱动回合。`codex queue` 和 `exec resume` 的帮助信息证明命令存在，但本轮没有实际验证 queue 行为；不以这些辅助命令替代已验证的主协议。

注意两个边界：`requestUserInput` 是实验性能力，且官方说明连接器审批也可能走该入口；必须识别来源与待处理请求，不能把所有输入请求自动映射成可邮件答复的普通问题。独立 app-server 成功不证明可以观察或控制现有桌面会话。

来源：[官方 app-server 文档](https://learn.chatgpt.com/docs/app-server)，本机 `codex app-server generate-json-schema --experimental`。文档和本机字段可能随版本变化，首版 doctor 必须检查实际版本和能力，不仅比较版本字符串。

## Phase 1：Claude Code

使用 `--print --output-format stream-json --verbose`、safe-mode、空工具列表和空 MCP 配置。以第一轮的 `result.session_id` 显式 `--resume`，最终返回 session ID 不变并回忆合成标记。观察到 system、assistant、result、rate_limit_event 等类型；出现 rate_limit_event 不等于任务失败，需读取其语义及 result。

共执行两次两轮尝试。首次第一轮成功；第二轮进程退出码 1，但解析到的 subtype 为 success。初次探针未保存更充分的错误细节，不能追溯精确原因，也不能声称已修复。第二次完整两轮成功。工程 Adapter 必须结合进程退出码、`is_error`、result.subtype 与会话状态判断，未知失败不自动重发 Prompt。

工程选择：首个版本优先采用“单回合 headless 进程 + session resume”，便于 FIFO 与恢复。Hooks 用于补充生命周期，但本轮只核查其文档，未启动实际 Hook 回调；background supervisor、attach 等也只确认帮助存在，不作为首版已验证基础。

无交互运行会改变行为：`--permission-prompts none` 会移除 AskUserQuestion，无法据交互终端的 idle_prompt 推断 headless 一定停在可回答的结构化问题。第一条可交付路径是 Agent 在最终文字中提出问题，用户回复后用同 session 继续；中途阻塞问题另作能力验证。

来源：[官方 headless 文档](https://code.claude.com/docs/en/headless)、[官方 Hooks 文档](https://code.claude.com/docs/en/hooks)，以及本机 CLI 帮助。

## 设计自查与已落盘修正

1. worktree 是工作副本隔离，必须另保留 Agent 沙箱和工具权限；自然语言不天然安全。
2. 忙时队列与 Agent 状态分开，避免将 RUNNING 覆盖为 REPLY_QUEUED 而提前投递。
3. 默认元数据历史与恢复队列分开；待处理正文加密暂存，投递后清理，不能宣称完全不落盘。
4. Message-ID 去重无法提供跨 SMTP/SQLite/Agent 的 exactly-once；未知投递结果需人工核对。
5. 原始 Agent 输出可能含源码和路径；邮件默认经过确定性过滤，附件仅显式开启。
6. 源码或测试结果未采集时写“未采集”，不能从模型自然语言自动判定测试通过。

## 下一阶段入口与尚未验证项

可以进入 Phase 2：建立 Go 工程骨架、Apache-2.0 及治理文件、双语文档、基础 CI，扫描后公开到个人 GitHub 仓库。当前尚未创建 GitHub 远程仓库或推送。

实现优先顺序：邮件元数据与加密队列 → 解析和验证回归样本 → 两个原生 Adapter → QQ 真机往返 → 故障恢复与发布验收。首个演示采用“完成一回合—回邮件—原会话继续”，之后再接结构化等待输入。

下列不能写进首版已支持清单：真实 QQ 登录/收发及移动端引用格式；桌面控制；中途结构化问题答复；权限拒绝后的用户体验；断网/重复/重放/崩溃恢复；两个 Agent 各十轮的真实邮件验收。这些是后续验收项目，不是本轮通过项。邮箱授权码应在将来的本地 init 中输入，不应发送到聊天或写入仓库。
