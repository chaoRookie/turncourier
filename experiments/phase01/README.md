# Phase 0–1 研究探针

这些脚本验证接口或执行竞品纯函数，不是产品 Adapter。需要 Node.js 22 或更高版本，以及要验证的 CLI。所有命令从仓库根目录运行。

## Codex

```sh
node experiments/phase01/codex-probe.mjs
node experiments/phase01/codex-probe.mjs --live
```

默认模式只验证 stdio 握手与临时会话创建，不发起模型回合。`--live` 会使用本机 Codex 配置和登录，发起两轮合成消息：第一轮保存标记，退出 app-server；第二个进程显式恢复同一 thread ID，并检查最终回答能回忆标记。仅在已登录 ChatGPT 且愿意消耗少量订阅额度时运行 live 模式。

使用临时目录、read-only 沙箱，不批准服务器工具请求。不传模型名以保留用户默认配置。测试后终止自己创建的服务；live 会话记录仍可能由 Codex 保留，不自动删除用户的 CLI 历史。临时工作目录由系统后续清理。

预期 `ok: true`，live 的两个回合均为 `completed`，存在 `resume-same-thread-after-process-restart` 和 `two-turns-persisted`。

## Claude Code

```sh
node experiments/phase01/claude-probe.mjs --live
```

要求 `claude auth status` 确认 `claude.ai` 登录。脚本清除子进程中的 API Key 与 Auth Token 环境覆盖，使用 safe-mode 禁用自定义内容、空工具列表与空 MCP 配置；不读取 Keychain 凭据内容或邮箱信息。发起两轮独立进程调用，第二轮 `--resume` 第一轮返回的 session ID。

预期 `ok: true`、两轮 `success`、`resume-same-session-after-process-exit` 和 `context-retained`。本测试不验证交互式 Hooks、AskUserQuestion 或后台 supervisor。合成会话可能由 Claude 保存，不自动删除。

## 竞品

在仓库外创建临时 checkout，固定为以下提交，然后运行：

```sh
git clone https://github.com/JessyTsui/Claude-Code-Remote.git /tmp/turncourier-upstream-example
git -C /tmp/turncourier-upstream-example switch --detach cd172b2f3361ccb60d7e691dcca768980a2eb13d
node experiments/phase01/competitor-probe.mjs /tmp/turncourier-upstream-example
```

目标目录须不存在；可自行换成唯一临时路径。探针校验 HEAD，提取已人工核对的纯函数，在不提供网络或文件访问函数的 VM 上下文中调用。不启动上游守护进程，不安装 Hooks，不向真实会话注入文字。Node VM 不是安全沙箱，因此仍要求只对已核对的固定提交运行。

输出的 `matchesDesired: false` 是研究发现，不表示探针没有运行。当前固定版本的 7 个行为用例中，白名单子串、显示名匹配、重复正文改写和肯定词删除共 4 项与 TurnCourier 的期望不符。测试仅提供离线体验，不能宣称真实 QQ 邮件端到端已通过。

## 检查与数据处理

```sh
node --check experiments/phase01/codex-probe.mjs
node --check experiments/phase01/claude-probe.mjs
node --check experiments/phase01/competitor-probe.mjs
```

探针只输出摘要和合成内容的匹配结果，不输出邮箱授权码、CLI 身份令牌或完整会话。异常输出也需在分享前审阅。Go 工具链与真实邮箱凭据不是运行这些研究脚本的前提。
