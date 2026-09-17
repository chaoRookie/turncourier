# TurnCourier Architecture

Status: pre-alpha, Phase 2 engineering skeleton.

This document covers two things: the code that exists in the repository today, and the architecture planned for later phases. Everything under [Planned architecture](#planned-architecture) is design intent. None of it is implemented.

The approved design in [docs/zh-CN/design.md](../zh-CN/design.md) (Chinese) is authoritative. Related documents:

- [Phase 2 plan](../zh-CN/plans/phase-02.md)
- [Phase 0–1 research report](../zh-CN/research/phase-01.md)
- [Development guide](../zh-CN/development.md) (Chinese)

## Product intent

TurnCourier is meant to be a local program that connects Codex and Claude Code sessions to email:

- It starts an agent task on the local machine. Depending on configuration, it sends notifications for turn completion, waiting for input, errors and approval requests. Mail goes from a dedicated QQ Mail bot account to the user's personal inbox.
- A valid reply to the notification becomes the next user message in the original agent session.
- The first release is single-user, although the data model reserves an owner field. It targets macOS first and keeps the core cross-platform. It works with CLI sessions that TurnCourier starts itself. Desktop app compatibility is only an experiment.
- The plan is a single Go module with a background process and a CLI. launchd installation and a macOS menu bar come after the core is stable.

None of this workflow exists yet.

## Current scope (Phase 2)

There are no releases and no tags, including `v0.1.0-alpha`. The repository has only the `main` branch.

### Implemented

| Area | What exists |
| --- | --- |
| CLI | `turncourier help`, `version` and `doctor`, each with an optional `--json` flag. |
| Planned command stubs | `init`, `run`, `tasks`, `logs` and `service` print a "not implemented yet" message and exit with code 2. |
| Environment diagnostics | `doctor` reports the platform and runs `git`, `codex` and `claude` with `--version`. |
| Engineering checkers | `tools/commentcheck` checks for Chinese doc comments. `tools/covercheck` enforces the statement coverage threshold. |
| Quality gates | Makefile targets for gofmt, go vet, comment checks, race tests with coverage, staticcheck, govulncheck, gitleaks and actionlint. |
| CI | GitHub Actions workflows for quality checks, security scans and a manually triggered macOS candidate build. |
| Research probes | Node.js scripts in `experiments/phase01/` that tested the Codex and Claude Code session interfaces and ran pure functions from a pinned competitor commit. They are research tools, not product adapters. |

CLI output meant for people is in Chinese. JSON field names are in English.

### Not implemented

- Sending or receiving email (SMTP, IMAP), MIME parsing, reply validation and email rendering.
- Codex and Claude Code adapters. TurnCourier does not start, resume or watch agent sessions.
- Configuration files, macOS Keychain storage, SQLite persistence, queues, the task state machine, signed tokens and replay protection.
- A background service, launchd integration, a menu bar, worktree management and telemetry.
- Release binaries. The manual candidate build uploads Actions artifacts that expire after 14 days. It does not create a GitHub Release.

## Packages and dependency direction

The module is `github.com/chaoRookie/turncourier`. It requires Go 1.27.1 (see `go.mod`), and production code imports only the standard library.

```text
cmd/turncourier
  ├─► internal/cli          command routing, output, exit codes
  │     └─► internal/doctor   Checker and Report types
  └─► internal/doctor       doctor.New() wires the production checker

tools/commentcheck          standalone checker, standard library only
tools/covercheck            standalone checker, standard library only
```

Dependencies point one way: `cmd` → `internal/cli` → `internal/doctor`. `cmd` also imports `internal/doctor` directly to build the production checker. `internal/doctor` imports no other package from this module, and nothing imports `cmd` or `tools`. The tools are never linked into the product binary, but their code is still included in the coverage total.

| Package | Responsibility |
| --- | --- |
| `cmd/turncourier` | Entry point. Turns SIGINT and SIGTERM into context cancellation, passes `doctor.New()` to `cli.Run` and exits with the returned code. |
| `internal/cli` | Parses `help`, `version`, `doctor` and `--json`. Rejects planned commands, unknown commands and extra arguments, and writes text or JSON output. `cli.Version` defaults to `0.1.0-dev` and can be set at build time with `-ldflags -X`. |
| `internal/doctor` | Read-only diagnostics. The platform, the `PATH` lookup, the version runner and the timeout are injectable, so tests run offline. |
| `tools/commentcheck` | Walks the source tree and fails if a package, type or named function has no Chinese doc comment. |
| `tools/covercheck` | Reads a Go cover profile and fails if statement-weighted coverage is below the minimum. |

Command parsing rules:

- Running with no arguments is the same as `help`. `--help` and `-h` also mean `help`, and `--version` means `version`.
- The only argument accepted after a command is a single `--json`. An invalid command name is reported before an invalid argument.
- JSON output is one indented object. `doctor --json` returns `platform`, `supported`, `ready` and `tools`. Each tool entry has `name`, `status`, and either `version` or `detail`. The possible `status` values are `ok`, `missing`, `timeout`, `cancelled`, `error` and `invalid`.

## doctor safety boundary

`doctor` answers one question: are the basic dependencies present? `ready` is `true` only if the platform is `darwin` and all three tools returned a recognized version line. It does not check logins, permissions, agent sessions or mailboxes, and it does not read configuration, credentials or session data.

- **Fixed commands, no shell.** The tool list is fixed: `git`, `codex`, `claude`. Each name is resolved with `exec.LookPath`, so doctor runs the first match on `PATH` without checking where that binary came from. The only argument is `--version`. The process is started directly through `os/exec`, never through a shell.
- **No input, limited output.** stdin and stderr are connected to the null device. stdout is captured with a hard limit of 4096 bytes. Output over the limit fails the check; it is not truncated.
- **Strict version parsing.** The output must be valid UTF-8. After trimming, it must be a single line of at most 256 bytes with no control characters. It must also match the tool's expected form: `git version <n>`, `codex-cli <n>` or `<n> (Claude Code)`, where `<n>` starts with a digit. Anything else is reported as `invalid`.
- **Timeout and process-group cleanup.** Each tool gets 3 seconds. On Unix, the version process leads its own process group. On timeout or cancellation the whole group gets SIGKILL, so child processes that a wrapper script started in that group are killed too. `WaitDelay` then allows up to 1 more second to collect output, which puts a limit of about 4 seconds on each tool. If the direct child exited successfully while a background process still holds stdout, doctor stops waiting after that extra second and validates the output read so far as usual. That background process is not killed. On other platforms, only the direct child is killed.
- **Cancellation.** SIGINT or SIGTERM cancels the context. On Unix the version process is not in the terminal's foreground process group, so Ctrl-C reaches TurnCourier, which then ends the child's group. Tools that have not been checked yet are reported as `cancelled`, without a `PATH` lookup or a new process.
- **No path or error leaks.** A tool entry contains only its name, status, and either the validated version line or a fixed message. Executable paths and raw error text are never included. If writing output fails, a fixed message is printed without system error details.
- **Network.** doctor opens no network connections itself. TurnCourier has no control over what an external tool does when it handles `--version`.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success. For `doctor`, the report is ready. |
| 1 | `doctor` is not ready, or writing the output failed. Not ready means the platform is not macOS, or `git`, `codex` or `claude` is missing, timed out, was cancelled, failed to run or produced unexpected output. A not-ready report is still written in full. |
| 2 | Usage error: an unknown command, an unexpected argument, or a planned command (`init`, `run`, `tasks`, `logs`, `service`). |
| 141 | Not returned by the program. If stdout or stderr is a closed pipe, the process is killed by SIGPIPE, following Unix convention, and the shell reports 141 (128 + 13). |

## Quality gates and CI

`make check` runs `fmt-check`, `vet`, `comments`, `test` and `lint`:

- `gofmt` must report no changes under `cmd`, `internal` and `tools`, and `go vet ./...` must pass.
- `commentcheck` requires a Chinese doc comment on every hand-written package, on every type (including types declared inside functions) and on every named function or method, test code included. Generated files are exempt.
- `staticcheck` v0.8.1 runs its default checks plus ST1020, ST1021 and ST1022, which require exported identifiers' doc comments to start with the identifier name.
- `go test -race` runs with atomic coverage across every package in the module. `covercheck` then requires at least 80% statement-weighted coverage of all hand-written Go code built for the current platform, with no exclusions.

`make security` runs `govulncheck` v1.8.0 and the secret scan. The scan uses gitleaks v8.30.1 to check all Git history, the staging area and a snapshot of the working tree without ignored files. It refuses to run if `.gitleaks.toml` or `.gitleaksignore` exists at the repository root. `make workflows` runs actionlint v1.7.12.

| Workflow | Trigger | What it does |
| --- | --- | --- |
| `ci.yml` | Push to `main`, pull request, manual | On `ubuntu-24.04` and `macos-15`: `make check`, `make build` and a smoke test of `help` and `version`. On Linux, also `make workflows`. |
| `security.yml` | Push to `main`, pull request, manual | On `ubuntu-24.04`, with full history: `make security`. |
| `build.yml` | Manual only | On `macos-15`: `make check` and `make security`, then darwin arm64 and amd64 binaries plus `SHA256SUMS`, uploaded as an Actions artifact kept for 14 days. It creates no Release. |

Every action is pinned to a full commit SHA, and workflow permissions are `contents: read`. Workflows use no repository secrets and never call a real mailbox or model. Dependabot checks `github-actions` and `gomod` weekly.

Commands, the coverage calculation and the comment rules are covered in the [development guide](../zh-CN/development.md).

## Planned architecture

> **Planned.** Nothing in this section is implemented. It summarizes [docs/zh-CN/design.md](../zh-CN/design.md) and may change as later phases are verified. The full target directory tree is kept only in the design document. Directories are created when their code is written, not ahead of time.

### Module responsibilities (planned)

| Module | Planned responsibility |
| --- | --- |
| CLI | User interaction: `init`, `doctor`, `run`, `tasks`, `logs`, `service`. |
| app | Dependency wiring and lifecycle. |
| task | Task state machine and ownership. |
| agent adapter | Owns native agent sessions. Codex preferably uses a self-managed `app-server` over stdio JSON-RPC. Claude Code uses its machine-readable headless CLI with session resume, and Hooks as an extra event source. Simulated keystrokes are avoided. |
| mail | SMTP sending and IMAP receiving (IDLE when available, with reconnect and a rescan), parsing of MIME, quotes and signatures, and plain-text and HTML rendering. |
| queue | Persistent scheduling of replies and outgoing mail. |
| security | Signed tokens with expiry and revocation, deduplication and replay checks, and Keychain access. |
| store | SQLite migrations and transactions. |
| config | TOML configuration, with temporary overrides for non-sensitive settings. |
| worktree | Working copy lifecycle. |
| logging | Redacted logs. |
| telemetry | Anonymous metrics, only if the user turns them on. Nothing is sent while no collection service exists. |
| platform/darwin | macOS integration. |

### Event flow (planned)

```text
CLI → Task Manager → Agent Adapter → Codex / Claude Code
          │               │
          │        structured events
          ▼               ▼
       SQLite ← Queue ← Event Policy → SMTP → user inbox
          ▲       │                               │
          │       └─ deliver when agent is idle   │ reply
          └── validation and body parsing ← IMAP ←┘
```

1. The CLI creates a task. The task manager starts an agent session through an adapter and saves the agent session ID. Every later operation names that ID. Looking up the "most recent session" is not allowed.
2. The adapter turns native output into structured events. It must tell apart a natural-language question, a tool approval request, the end of a turn and a failure.
3. The event policy decides which events send mail. Every event type is configurable. By default, mail goes out for waiting for input, failures and completed turns. Each task gets one email thread, and the main agent summarizes sub-agent status instead of opening more threads.
4. A notification contains the agent's original reply, with no extra model summarization. File names and test results appear only when they come from verifiable events. Otherwise they are marked as not collected.
5. IMAP receives replies. Only the new text above the separator is used, with quotes and signatures removed. Attachments are never treated as instructions. If parsing is uncertain, nothing is executed. Auto-replies and bounces never trigger the agent.
6. Valid replies enter the queue. While the agent is busy, replies wait in local receive order. All consecutive replies are kept, and a duplicate email is not queued twice.

Phase 1 confirmed the first path to build: when a turn completes, send the agent's message, and let the reply start a new turn in the same session. Answering structured questions in the middle of a turn is later work. In Codex, `requestUserInput` can also carry connector approvals. Claude Code's non-interactive headless mode removes AskUserQuestion.

In the first version, replies are natural language only. Email cannot approve tool permissions, retry a task or stop a task.

### State machine highlights (planned)

```text
CREATED → RUNNING → COMPLETED → RUNNING (next valid reply)
             ├── WAITING_INPUT → RUNNING (answer to that question)
             ├── WAITING_APPROVAL (email cannot approve)
             └── FAILED (diagnostics kept; no retry by email in the first version)
Any state except CLOSED → CLOSED (closed locally or expired)
Delivery confirmation uncertain → DELIVERY_UNCERTAIN (recovered after a local check)
```

- `COMPLETED` means the turn has ended. The task can still continue.
- Queue states are separate from task states: `QUEUED`, `DISPATCHING`, `ACKNOWLEDGED`, `UNCERTAIN`, `REJECTED`. A queued reply must never replace `RUNNING`, or messages could be delivered in parallel.
- FIFO order comes from a local enqueue sequence number, never from the sender's `Date` header.
- After a restart, the daemon checks the agent session state before it takes anything off the queue.
- There is no end-to-end exactly-once guarantee. The agent accepting a message and the SQLite commit cannot be one transaction. If the acknowledgment is lost, the item goes to an uncertain state, and it is not resent until the agent side is shown to handle duplicates safely. SMTP can also accept a message without the local side getting confirmation. A stable Message-ID limits the damage, but duplicate notifications are still possible.

### Security principles (planned)

- **Secrets.** The QQ Mail authorization code and the signing key live in the macOS Keychain. The TOML file holds only the account and options. Environment variables can temporarily override non-sensitive settings only. The authorization code will be entered locally during `init`, never in chat and never in the repository.
- **Sender identity.** The sender address is normalized and must exactly match the allowlist. A display name or a longer address that merely contains an allowed address is rejected. Sender headers can be forged, so they are never enough on their own.
- **Reply binding.** The thread reference headers, the short task ID and the signed token must all agree. If one is missing or they conflict, the reply is rejected. Matching by subject alone is never allowed as a fallback. Tokens are bound to the task, user, expiry time and notification ID. They are valid for seven days by default (configurable) and are revoked as soon as the task closes. A signature proves integrity; it does not encrypt anything. Tokens are bearer credentials and must not appear in public logs.
- **Deduplication.** Besides the Message-ID, each received record stores the mailbox account, UIDVALIDITY, UID and a body digest. The SQLite unique constraint and the enqueue happen in the same transaction. The same Message-ID with a different body is rejected and triggers a warning.
- **Data at rest.** By default only metadata history is kept. Undelivered replies and outgoing mail must keep their bodies so they can be recovered. That pending data is encrypted on disk with a key stored in the Keychain, and is deleted after completion. Users must be told about this exception.
- **Agent permissions.** A Git worktree isolates code changes. It is not an operating system sandbox. The agent's own sandbox and permission controls stay in place, email cannot set parameters that skip approvals, and approval events are sent for local handling.
- **Outgoing content.** Before sending, mail goes through deterministic filtering of sensitive information and trimming of code blocks and long content. Filtering cannot promise to catch every secret, so status-only notifications must be available. Diffs and reports are attached only if the user turns that on.
- **Telemetry.** Off by default and only sent if the user turns it on. It must never upload message bodies, code, paths, repository URLs, email addresses or identity information.

### Release criteria (planned)

Before `v0.1.0-alpha`, Codex and Claude Code must each complete ten consecutive real email rounds. Those rounds must cover recovery after a network outage, duplicate, forged and expired mail, FIFO while the agent is busy, and privacy filtering. The release would then publish macOS binaries with SHA-256 checksums, with cross-platform packages and Homebrew later. A successful build does not replace real acceptance testing.

Planned order of work: research, public engineering skeleton, configuration, storage and state machine, email round trip, Codex, Claude Code, security and recovery, real acceptance testing, release. The current phase is the engineering skeleton.
