# Changelog

All notable changes to this project are documented in this file.

The format is based on Keep a Changelog. TurnCourier has no releases yet, so every change is listed under `[Unreleased]`.

## [Unreleased]

TurnCourier is pre-alpha. It does not yet send or receive email and has no Agent adapters, Keychain storage or background service. Configuration, storage and the state machines exist as internal packages, but no command uses them yet.

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

#### Phase 3: configuration, storage and state machines

These packages are tested but not yet used by any command, so the `turncourier` binary links no new modules.

- `internal/task`: task states, events and the single transition table, plus which states accept and dispatch replies. `internal/queue`: reply queue states, events and transitions, separate from task states. Neither package does any I/O.
- `internal/config`: strict email address normalization; configuration file and data directory paths from `TURNCOURIER_CONFIG`, `TURNCOURIER_DATA_DIR` or the user config directory; strict TOML loading with defaults and the `TURNCOURIER_NOTIFY_EVENTS` override. TOML syntax and type errors are returned on their own, unknown and credential keys are reported together before any value is checked, and once the file decodes cleanly all remaining validation errors are returned together. Unknown keys (key names are case-sensitive), credential keys, invalid addresses, the bot address in the sender allowlist and config files writable by the group or other users are rejected. Error messages contain no local absolute paths.
- `configs/turncourier.example.toml`, loaded by tests so it stays in sync with validation.
- `internal/store/sqlite`: SQLite storage with embedded migrations tracked by `user_version`, private file permissions, WAL and `synchronous=FULL`; tasks with optimistic versioning and an event log; inbound reply deduplication by mailbox UID and Message-ID in the same transaction as enqueueing; FIFO dispatch with at most one reply in flight per task, enforced by the API and a partial unique index; crash recovery that marks in-flight replies uncertain and never resends automatically. Only metadata and body SHA-256 digests are stored, never email bodies.
- `tests/integration`: end-to-end lifecycle test combining the example configuration, a real SQLite database and both state machines.
- Dependencies `modernc.org/sqlite` v1.59.0 (pure Go, no CGO) and `github.com/BurntSushi/toml` v1.6.0. Only permissive licenses are accepted, and new dependencies must be justified in a plan or issue.
- `make modverify` runs `go mod verify` and is part of `make check`.
- Phase 3 plan `docs/zh-CN/plans/phase-03.md`.

### Changed

- Production code is no longer limited to the standard library: the two dependencies above are allowed under the new dependency rules in `CONTRIBUTING.md` and `docs/zh-CN/development.md`.
- `make fmt` and `make fmt-check` also cover `tests`.
- The `help` description and the message for planned commands no longer name a phase; the description now starts with `pre-alpha`.
- README, architecture, development and contribution documents describe the new packages, dependencies and checks.

[Unreleased]: https://github.com/chaoRookie/turncourier
