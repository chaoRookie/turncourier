# Changelog

All notable changes to this project are documented in this file.

The format is based on Keep a Changelog. TurnCourier has no releases yet, so every change is listed under `[Unreleased]`.

## [Unreleased]

TurnCourier is pre-alpha. It does not yet send or receive email and has no Agent adapters, configuration, Keychain storage, SQLite storage or background service.

### Added

#### Phase 0–1: design and feasibility research

- Approved design in `docs/zh-CN/design.md`: product boundaries, module responsibilities, security and persistence rules, task and queue states, and the target directory tree.
- Feasibility report in `docs/zh-CN/research/phase-01.md`, covering:
  - Codex app-server handshake, turns and resuming the same thread from a new process;
  - Claude Code `stream-json` output and resuming the same session from a new process;
  - TLS handshakes with the QQ Mail IMAP and SMTP servers, without logging in or sending or receiving mail;
  - offline behavior checks against a pinned commit of Claude-Code-Remote.
- Research probes in `experiments/phase01/` (`codex-probe.mjs`, `claude-probe.mjs`, `competitor-probe.mjs`). They are research tools, not product code.

#### Phase 2: engineering skeleton

- Go module `github.com/chaoRookie/turncourier` targeting Go 1.27.1. Production code uses only the standard library.
- `turncourier` CLI with `help`, `version` and `doctor`, each with an optional `--json` flag. The planned commands `init`, `run`, `tasks`, `logs` and `service` report that they are not implemented and exit with code 2.
- Exit codes: `0` for success; `1` when `doctor` is not ready or output cannot be written; `2` for usage errors, unknown commands and planned commands. A closed stdout pipe terminates the process by `SIGPIPE` (exit status 141).
- Read-only `doctor`: checks the platform (only macOS is supported) and runs `git`, `codex` and `claude` with `--version`. Each tool has a 3-second timeout; on timeout the version process (on Unix, its whole process group) is killed and output is collected for at most one more second. Reports contain no executable paths or raw errors. It does not verify logins, permissions, Agent sessions or mailboxes.
- `tools/commentcheck`: requires Chinese comments on packages, types and named functions, including tests and methods.
- `tools/covercheck`: enforces at least 80% statement coverage over all handwritten Go code, with no exclusions.
- `Makefile` targets `build`, `fmt`, `fmt-check`, `vet`, `comments`, `test`, `lint`, `check`, `security`, `secrets`, `workflows` and `tools`. Quality tools are pinned (staticcheck v0.8.1, govulncheck v1.8.0, gitleaks v8.30.1, actionlint v1.7.12) and installed into `.local/bin/`.
- `staticcheck.conf` enabling ST1020, ST1021 and ST1022, so comments on exported identifiers start with the identifier name.
- `scripts/scan-secrets.sh`: gitleaks scan of the full Git history, the staged changes and a snapshot of tracked and untracked, non-ignored files. It refuses to run when `.gitleaks.toml` or `.gitleaksignore` exists at the repository root.
- GitHub Actions workflows, pinned to full commit SHAs with `contents: read` permissions:
  - `CI`: `make check`, build and `help`/`version` smoke test on ubuntu-24.04 and macos-15, plus `make workflows` on Linux;
  - `Security`: `make security` on ubuntu-24.04 with full history;
  - `Candidate build`: manual only; runs the quality and security gates on macos-15 and uploads darwin arm64/amd64 binaries with `SHA256SUMS` as an Actions artifact kept for 14 days. It does not create a release.
- Dependabot weekly update checks for GitHub Actions and Go modules.
- Apache-2.0 `LICENSE`, `CODE_OF_CONDUCT.md` (Contributor Covenant 2.1), `CONTRIBUTING.md`, `SECURITY.md` (GitHub private vulnerability reporting), issue forms and a pull request template.
- Bilingual `README.md` and `README.zh-CN.md`, English architecture overview `docs/en/architecture.md`, Chinese development guide `docs/zh-CN/development.md` and Phase 2 plan `docs/zh-CN/plans/phase-02.md`.
- `AGENTS.md`, `CLAUDE.md` and `HANDOFF.md` with project rules and task checkpoints for AI coding tools.

[Unreleased]: https://github.com/chaoRookie/turncourier
