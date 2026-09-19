# Changelog

All notable changes to this project are documented in this file.

The format is based on Keep a Changelog. TurnCourier has no releases yet, so every change is listed under `[Unreleased]`.

## [Unreleased]

TurnCourier is pre-alpha. It still does not send or receive email and has no Agent adapters and no background service. `turncourier init` now writes the configuration file and stores the mail authorization code and two keys in the macOS Keychain; the mail clients, tokens and body encryption exist as internal packages that no command calls yet.

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

When these were added, no command used them and the `turncourier` binary linked no new modules. Phase 4a changed that; see below.

- `internal/task`: task states, events and the single transition table, plus which states accept and dispatch replies. `internal/queue`: reply queue states, events and transitions, separate from task states. Neither package does any I/O.
- `internal/config`: strict email address normalization; configuration file and data directory paths from `TURNCOURIER_CONFIG`, `TURNCOURIER_DATA_DIR` or the user config directory; strict TOML loading with defaults and the `TURNCOURIER_NOTIFY_EVENTS` override. TOML syntax and type errors are returned on their own, unknown and credential keys are reported together before any value is checked, and once the file decodes cleanly all remaining validation errors are returned together. Unknown keys (key names are case-sensitive), credential keys, invalid addresses, the bot address in the sender allowlist and config files writable by the group or other users are rejected. Error messages contain no local absolute paths.
- `configs/turncourier.example.toml`, loaded by tests so it stays in sync with validation.
- `internal/store/sqlite`: SQLite storage with embedded migrations tracked by `user_version`, private file permissions, WAL and `synchronous=FULL`; tasks with optimistic versioning and an event log; inbound reply deduplication by mailbox UID and Message-ID in the same transaction as enqueueing; FIFO dispatch with at most one reply in flight per task, enforced by the API and a partial unique index; crash recovery that marks in-flight replies uncertain and never resends automatically. When this was added, only metadata and bare SHA-256 body digests were stored, never email bodies; Phase 4a changed both, see below.
- `tests/integration`: end-to-end lifecycle test combining the example configuration, a real SQLite database and both state machines.
- Dependencies `modernc.org/sqlite` v1.59.0 (pure Go, no CGO) and `github.com/BurntSushi/toml` v1.6.0. Only permissive licenses are accepted, and new dependencies must be justified in a plan or issue.
- `make modverify` runs `go mod verify` and is part of `make check`.
- Phase 3 plan `docs/zh-CN/plans/phase-03.md`.

#### Phase 4a: offline mail foundations

Phase 4 is split into 4a (offline), L1 (a manual probe the maintainer runs against a real mailbox) and 4b (built on the L1 results). This entry covers 4a. TurnCourier still cannot send or receive email.

- `turncourier init`: interactive setup on macOS only, in an interactive terminal only, never online. It creates the configuration file, creates the instance ID, reads the QQ Mail authorization code without echoing it (twice, to confirm) and registers a token signing key and a body encryption key in the Keychain. It never overwrites an existing configuration file or an already registered key, replaces the authorization code only after an explicit confirmation, can be run again after an interruption, and its error text contains no paths and no secrets. The environment diagnostics and the real round-trip test that the design asks for are Phase 4b.
- `internal/security/keychain`: a thin wrapper over `/usr/bin/security` with `Get`, `Set`, `Add` and `Delete`. Secrets travel only over the subprocess's stdin and stdout, never in arguments or environment variables; each write starts its own `security -i` process, sends one command and reads the value back to confirm it. Key entries are only ever created, never updated. Other platforms return "unsupported"; there is no plaintext fallback.
- `internal/security/token`: reply token v1, an HMAC-SHA256 tag truncated to 128 bits, encoded as 48 Crockford base32 characters. The token carries a version, a key id and a random notification id; the task, owner and expiry come from the database and enter the MAC. Tokens and keys redact themselves in `String`, `Format` and `LogValue`. The same package computes the keyed body digest now stored in `body_sha256`.
- `internal/security/payload`: AES-256-GCM over pending bodies, with associated data binding each ciphertext to its purpose, key id, task and sequence number.
- `internal/queue`: the outgoing notification state machine (`PENDING`, `SENDING`, `SENT`, `UNCERTAIN`, `ABANDONED`), separate from the reply queue table.
- Migration `0002`: adds a `folder` column to inbound records, so the UID key is `(account, folder, uid_validity, uid)` while deduplication by Message-ID is unchanged; adds the instance row, key metadata, outgoing notifications, encrypted pending bodies, fetch cursors and rejected inbound mail, with the constraints and triggers that keep a notification's token binding immutable and delete a body in the same transaction that finishes it. `0001_init.sql` is unchanged.
- `internal/store/sqlite`: creates and claims notifications serially, records delivery outcomes, and recovers in-flight ones without ever resending; stores the instance ID and key metadata; keeps fetch cursors and rejected inbound mail; and runs a best-effort `PRAGMA wal_checkpoint(TRUNCATE)` after a body is deleted and when the database is opened.
- `internal/mail/smtp`: submits one message over implicit TLS with a deadline on every step, and classifies the outcome as sent, definitely not sent, rejected or uncertain.
- `internal/mail/imap`: read-only collection over implicit TLS with `LIST`, `EXAMINE`, UID rescan, `BODY.PEEK` fetches and `IDLE`, plus a watchdog, backoff reconnects, a login rate limit and an auth-failure pause. Its offline fake server reproduces QQ's bare `* BAD` and half-open connections.
- `tests/docs`: checks the dependency tables in `docs/en/architecture.md` and `docs/zh-CN/development.md` against `go.mod` and the actual imports.
- `tests/live`: the L1 probes, run only by the maintainer. They need the `live` build tag, `TURNCOURIER_LIVE=1` and interactive confirmations, refuse to run when `CI` is set, and write only redacted structural samples to a directory outside the repository. `make vet` and `make lint` compile-check them; nothing runs them.
- Dependencies `golang.org/x/term` v0.46.0, `github.com/emersion/go-smtp` v0.25.0, `github.com/emersion/go-imap/v2` v2.0.0-beta.8, `github.com/emersion/go-message` v0.18.2, `github.com/emersion/go-sasl` and `golang.org/x/text` v0.42.0, with `golang.org/x/sys` raised to v0.48.0.

### Changed

- Production code is no longer limited to the standard library: the Phase 3 dependencies are allowed under the new dependency rules in `CONTRIBUTING.md` and `docs/zh-CN/development.md`.
- `make fmt` and `make fmt-check` also cover `tests`.
- The `help` description and the message for planned commands no longer name a phase; the description now starts with `pre-alpha`.
- README, architecture, development and contribution documents describe the new packages, dependencies and checks.
- `turncourier init` no longer reports that it is not implemented. The planned commands are now `run`, `tasks`, `logs` and `service`.
- The product binary links `BurntSushi/toml`, `modernc.org/sqlite` and `golang.org/x/term`, because `init` uses the configuration, the store and the terminal. It does not link the mail modules or `golang.org/x/text`; those enter in Phase 4b.
- `inbound_messages.body_sha256` keeps its name, published in `0001`, but from `0002` on it holds a keyed HMAC-SHA256 digest rather than a bare SHA-256.
- `make vet` and `make lint` each run a second time with `-tags live` over `tests/live`.

### Security

- The Keychain threat model is documented in `docs/zh-CN/design.md`, `SECURITY.md` and the output of `init`: entries written with `/usr/bin/security` can be read silently by any process of the same user, including a shell command an Agent runs if its sandbox allows it. Such a process can sign a valid token, forge a reply that passes the sender, thread, subject and token checks, inject content into any task and rewrite the local queue. TurnCourier does not defend against processes of the same user. The Keychain protects against plaintext in the configuration file, the repository, logs and backups; against other system users; and against offline disk access without the login password. Use a dedicated bot mailbox, and disable the authorization code in QQ Mail if it may have leaked.
- Pending bodies are the only content on disk, and only as ciphertext. `secure_delete` is `ON` rather than `FAST`, and a TRUNCATE checkpoint follows a cleanup, so neither the database file nor the WAL keeps a copy. Ciphertext in an APFS snapshot or a Time Machine backup is outside what SQLite can reach and stays decryptable while the key is in the Keychain. Restoring the data directory from an old backup can resend a notification that was already sent and re-dispatch a reply that was already acknowledged.
- SMTP and IMAP use implicit TLS only, and source tests pin that they never fall back to STARTTLS or plaintext and never enable the libraries' debug output, which would print credentials.
- A delivery whose outcome is unknown is never retried automatically, on either the reply or the notification side; it waits for a local check.

[Unreleased]: https://github.com/chaoRookie/turncourier
