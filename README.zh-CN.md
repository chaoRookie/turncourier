# TurnCourier

Email bridge for Codex and Claude Code。

[English](README.md)

当前完成已批准设计的落盘及 Phase 0–1 可行性研究，尚不是可安装的邮件控制产品。

目标体验：本机启动 Codex 或 Claude Code 任务，邮件接收结果与问题，回复后接续原会话。首版使用 Go，面向 macOS、两个 CLI 及 QQ 邮箱；当前 Node.js 脚本只用于独立接口验证。

- [设计文档与目标目录树](docs/zh-CN/design.md)
- [Phase 0–1 验证报告](docs/zh-CN/research/phase-01.md)
- [研究脚本说明](experiments/phase01/README.md)

## 当前实际文件树

```text
turncourier/
├── docs/zh-CN/
│   ├── design.md                 # 已批准需求、架构和工程规范
│   └── research/phase-01.md       # 证据、限制和下一阶段入口
├── experiments/phase01/
│   ├── README.md                 # 复现命令与验证边界
│   ├── codex-probe.mjs           # 协议握手与原会话恢复
│   ├── claude-probe.mjs          # 结构化输出与原会话恢复
│   └── competitor-probe.mjs      # 固定版本竞品的离线行为测试
├── README.md
├── README.zh-CN.md
└── .gitignore
```

研究探针使用已有 CLI 订阅登录和合成问题，不发送邮件。已验证两个 Agent 的跨进程会话接续；真实 QQ 邮件往返、桌面任务控制、二十轮可靠性验收仍需后续实施。公开工程骨架阶段再加入 Go 产品代码、CI、治理文件和 Apache-2.0 许可证全文。
