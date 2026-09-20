# TurnCourier 项目规则

- 先读 `HANDOFF.md`，再核对 Git 状态。用户已批准 Phase 2：完成 Go 工程骨架、验证后公开到 `chaoRookie/turncourier`，无需重新询问这些已授权动作。
- 已批准设计在 `docs/zh-CN/design.md`，Phase 2 清单在 `docs/zh-CN/plans/phase-02.md`，Phase 3 清单在 `docs/zh-CN/plans/phase-03.md`，Phase 4 清单在 `docs/zh-CN/plans/phase-04.md`。当前已实现配置、存储、状态机、Keychain、令牌、正文加密、SMTP/IMAP 客户端与 `init`；尚未接入收发循环与 Agent，不能收发邮件。不把规划的邮箱/Agent 接入写成已支持。
- Go 标识符英文；每个手写具名函数（含测试）、类型、模块与包级说明须有必要中文注释。导出注释以名称开头，复杂逻辑说明错误、并发和安全边界。
- 目录按实际职责建立；README 保持实际树形目录图，完整目标树留在设计文档。不为美观建立空目录、动态插件或微服务。
- 通过格式、vet、race、静态检查、中文注释检查及覆盖率 80% 门槛；覆盖率计入 cmd/internal/tools 的全部手写 Go 代码。
- 公共 CI 不运行真实邮箱/模型调用，不存凭据。发布前扫描全部 Git 历史和待公开文件。
- 授权码最终由本地 init 写入 Keychain；不要求用户在聊天中发送，不把真实邮件或会话内容写进仓库。
- 每次续点需记录真实通过和未运行的验证。不得因缺少工具或时间就声称已通过。
- `.local/` 是忽略的工具链和工具目录，不入 Git；不要修改用户全局 Go 或 shell 配置。
