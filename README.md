# TurnCourier

Email bridge for Codex and Claude Code.

[中文说明](README.zh-CN.md)

Status: design and Phase 0–1 feasibility research. This repository currently contains specifications and research probes, not an installable email bridge.

The intended workflow is to start a local coding task, receive an email when a turn finishes or needs input, and reply to continue the same session. The first release targets macOS, Codex CLI, Claude Code CLI and QQ Mail. Product implementation is planned in Go; the dependency-free Node.js probes below are research tools only.

## Documents

- [Approved design and target directory tree](docs/zh-CN/design.md)
- [Phase 0–1 findings, limitations and reproduction](docs/zh-CN/research/phase-01.md)
- [Probe usage](experiments/phase01/README.md)

## Current tree

```text
turncourier/
├── docs/zh-CN/
│   ├── design.md                 # Approved requirements and architecture
│   └── research/phase-01.md       # Evidence and implementation boundaries
├── experiments/phase01/
│   ├── README.md                 # Reproduction and scope
│   ├── codex-probe.mjs           # Native protocol and session resume
│   ├── claude-probe.mjs          # Structured output and session resume
│   └── competitor-probe.mjs      # Offline upstream behavior checks
├── README.md
├── README.zh-CN.md
└── .gitignore
```

Live probes use an existing CLI subscription login and synthetic prompts. They do not send mail. QQ credentials and real conversations must never be committed. Production code, release artifacts, CI and the Apache-2.0 license file are scheduled for the public engineering skeleton phase.
