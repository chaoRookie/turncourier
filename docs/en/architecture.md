# TurnCourier Architecture

Status: pre-alpha. Configuration, storage and the state machines (Phase 3) are implemented as internal packages that no command uses yet.

This document covers two things: the code that exists in the repository today, and the architecture planned for later phases. Everything under [Planned architecture](#planned-architecture) is design intent. Apart from the parts that section marks as built in Phase 3, none of it is implemented.

The approved design in [docs/zh-CN/design.md](../zh-CN/design.md) (Chinese) is authoritative. Related documents:

- [Phase 2 plan](../zh-CN/plans/phase-02.md)
- [Phase 3 plan](../zh-CN/plans/phase-03.md): configuration, storage and state machines
- [Phase 0–1 research report](../zh-CN/research/phase-01.md)
- [Development guide](../zh-CN/development.md) (Chinese)

## Product intent

TurnCourier is meant to be a local program that connects Codex and Claude Code sessions to email:

- It starts an agent task on the local machine. Depending on configuration, it sends notifications for turn completion, waiting for input, errors and approval requests. Mail goes from a dedicated QQ Mail bot account to the user's personal inbox.
- A valid reply to the notification becomes the next user message in the original agent session.
- The first release is single-user, although the data model reserves an owner field. It targets macOS first and keeps the core cross-platform. It works with CLI sessions that TurnCourier starts itself. Desktop app compatibility is only an experiment.
- The plan is a single Go module with a background process and a CLI. launchd installation and a macOS menu bar come after the core is stable.

None of this workflow exists yet.

## Current scope

There are no releases and no tags, including `v0.1.0-alpha`. The default branch is `main`.

### Implemented

| Area | What exists |
| --- | --- |
| CLI | `turncourier help`, `version` and `doctor`, each with an optional `--json` flag. |
| Planned command stubs | `init`, `run`, `tasks`, `logs` and `service` print a "not implemented yet" message and exit with code 2. |
| Environment diagnostics | `doctor` reports the platform and runs `git`, `codex` and `claude` with `--version`. |
| Configuration | `internal/config` loads the TOML file, fills defaults, applies the one allowed environment override and validates everything. `configs/turncourier.example.toml` is the example, and tests load it. No command reads the configuration yet. |
| State machines | `internal/task` and `internal/queue` hold the task and reply queue transition tables. They are pure functions with no I/O. |
| Storage | `internal/store/sqlite` persists tasks, task events, inbound reply metadata and the reply queue, with embedded migrations, deduplication, FIFO dispatch and crash recovery. No command opens the database yet. |
| Integration tests | `tests/integration` runs the example configuration, a real SQLite database and both state machines through a full reply lifecycle. |
| Engineering checkers | `tools/commentcheck` checks for Chinese doc comments. `tools/covercheck` enforces the statement coverage threshold. |
| Quality gates | Makefile targets for gofmt, go vet, module checksum verification, comment checks, race tests with coverage, staticcheck, govulncheck, gitleaks and actionlint. |
| CI | GitHub Actions workflows for quality checks, security scans and a manually triggered macOS candidate build. |
| Research probes | Node.js scripts in `experiments/phase01/` that tested the Codex and Claude Code session interfaces and ran pure functions from a pinned competitor commit. They are research tools, not product adapters. |

CLI output meant for people is in Chinese. JSON field names are in English.

### Not implemented

- Sending or receiving email (SMTP, IMAP), MIME parsing, reply validation and email rendering.
- Codex and Claude Code adapters. TurnCourier does not start, resume or watch agent sessions.
- Wiring configuration and storage into commands. No command reads a configuration file or opens the database.
- macOS Keychain storage, encrypted storage of email bodies, the outgoing mail queue, signed tokens and token revocation.
- A background service, launchd integration, a menu bar, worktree management and telemetry.
- Release binaries. The manual candidate build uploads Actions artifacts that expire after 14 days. It does not create a GitHub Release.

## Packages and dependency direction

The module is `github.com/chaoRookie/turncourier`. It requires Go 1.27.1 (see `go.mod`). Besides the standard library, production code imports the two modules listed under [Dependencies](#dependencies).

```text
cmd/turncourier
  ├─► internal/cli          command routing, output, exit codes
  │     └─► internal/doctor   Checker and Report types
  └─► internal/doctor       doctor.New() wires the production checker

internal/store/sqlite       SQLite storage (modernc.org/sqlite)
  ├─► internal/task         task state machine, no I/O
  └─► internal/queue        reply queue state machine, no I/O

internal/config             TOML configuration (BurntSushi/toml)

tools/commentcheck          standalone checker, standard library only
tools/covercheck            standalone checker, standard library only
```

Dependencies point one way: `cmd` → `internal/cli` → `internal/doctor`, and `internal/store/sqlite` → `internal/task`, `internal/queue`. `cmd` also imports `internal/doctor` directly to build the production checker. `internal/doctor`, `internal/config`, `internal/task` and `internal/queue` import no other package from this module, so `config`, `task` and `queue` do not depend on each other or on storage. The store does not import `config`: callers normalize email addresses before passing them in. No command imports the configuration, state machine or storage packages yet, so the product binary does not link the third-party modules. Only `tests/integration` combines them. Nothing imports `cmd` or `tools`. The tools are never linked into the product binary, but their code is still included in the coverage total.

| Package | Responsibility |
| --- | --- |
| `cmd/turncourier` | Entry point. Turns SIGINT and SIGTERM into context cancellation, passes `doctor.New()` to `cli.Run` and exits with the returned code. |
| `internal/cli` | Parses `help`, `version`, `doctor` and `--json`. Rejects planned commands, unknown commands and extra arguments, and writes text or JSON output. `cli.Version` defaults to `0.1.0-dev` and can be set at build time with `-ldflags -X`. |
| `internal/config` | Resolves the configuration file and data directory paths, normalizes email addresses and loads the TOML file. It rejects unknown keys, credential keys, invalid addresses, a bot address in the sender allowlist, unknown or duplicate events, invalid ports, hosts and token lifetimes, and unsafe files. TOML syntax and type errors are returned on their own, without echoing the offending text. Unknown and credential keys are reported together before any value is checked. Once the file decodes cleanly, all remaining validation errors are returned together with `errors.Join`. No error contains local absolute paths. |
| `internal/task` | Task states (`CREATED`, `RUNNING`, `WAITING_INPUT`, `WAITING_APPROVAL`, `COMPLETED`, `FAILED`, `DELIVERY_UNCERTAIN`, `CLOSED`), events and the only transition table. It also decides which states accept replies and which can dispatch one. |
| `internal/queue` | Reply queue states (`QUEUED`, `DISPATCHING`, `ACKNOWLEDGED`, `UNCERTAIN`, `REJECTED`), events and the transition table, kept separate from task states. |
| `internal/store/sqlite` | Opens and migrates the database, creates task IDs, persists tasks with optimistic versioning, and deduplicates, queues, dispatches and recovers replies. Every state change runs in one transaction and goes through the state machines first. |
| `internal/doctor` | Read-only diagnostics. The platform, the `PATH` lookup, the version runner and the timeout are injectable, so tests run offline. |
| `tools/commentcheck` | Walks the source tree and fails if a package, type or named function has no Chinese doc comment. |
| `tools/covercheck` | Reads a Go cover profile and fails if statement-weighted coverage is below the minimum. |
| `tests/integration` | Test-only package. Loads a copy of the example configuration, opens a real database in a temporary directory and drives a task and its replies from start to close, including a simulated crash. |

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

## Persistence and recovery

These rules are implemented and tested in `internal/config` and `internal/store/sqlite`. No command uses them yet.

**Locations.** The configuration file is `$TURNCOURIER_CONFIG`, which must be an absolute path, or else `TurnCourier/turncourier.toml` under `os.UserConfigDir()` (on macOS, `~/Library/Application Support/TurnCourier/turncourier.toml`). The data directory is `$TURNCOURIER_DATA_DIR`, which must also be absolute, or else the directory that holds the configuration file. The database file is `turncourier.db` in the data directory. `TURNCOURIER_NOTIFY_EVENTS` (comma-separated) is the only setting an environment variable can override. The sender allowlist, addresses and token lifetime cannot be overridden this way.

**Configuration file.** It must be a regular file of at most 1 MiB. On Unix it must belong to the current user and must not be writable by group or others. Key names are case-sensitive, so a variant such as `ADDRESS` counts as an unknown key. Unknown keys and credential keys such as `password` or `token` (in any letter case) are rejected: the mail authorization code and signing key are meant for the Keychain, which is not wired up yet.

**Database.** On Unix the data directory must be `0700` and the database file `0600`. Both are created with those modes, and wider existing modes are refused. The connection uses WAL, `synchronous=FULL`, foreign keys and a 5-second busy timeout, and write transactions start with `BEGIN IMMEDIATE`. Each process uses a single connection, and all tables are `STRICT`. When several processes open a new database at once, switching the empty file to WAL can fail with `SQLITE_BUSY` without waiting for the busy timeout, so opening retries for up to 5 seconds. Migrations are embedded SQL files. Each one runs in a transaction together with the `user_version` update and reads the version again inside that transaction, so concurrent openers apply it only once. A database with a newer schema than the build knows is refused with its tables and data untouched, although opening it may already have switched the file to WAL mode.

**Durability.** `synchronous=FULL` makes committed data survive a process crash. On macOS it does not guarantee durability after a power loss or a kernel panic: the SQLite driver uses `F_FULLFSYNC` only when `PRAGMA fullfsync=1` is set, and a plain `fsync` there does not force the data onto stable storage. Turning on `fullfsync`, which holds the write lock longer, is to be evaluated in the later security and recovery phase.

**What is stored.** Tasks (a 10-character random ID, owner, agent, agent session ID, state, version and timestamps), task events, inbound reply metadata (mailbox account, UIDVALIDITY, UID, Message-ID and the SHA-256 digest of the body) and reply queue items. Times are UTC Unix milliseconds. There is no column for an email body or a credential. Encrypting pending bodies, with the key in the Keychain, must be built before any body is stored.

**Writes.** Every state change runs in one transaction. The new state comes from `internal/task` or `internal/queue`, and the task row is updated only if its version still matches, so a caller holding a stale version gets a conflict error. `start`, `reply_dispatched`, `delivery_unknown` and `delivery_confirmed` happen only as part of the matching storage operation. `fail` and `close` reject every queued reply of the task in the same transaction.

**Deduplication.** An inbound reply is unique by `(account, UIDVALIDITY, UID)` and by `(account, Message-ID)`. The same message recorded again returns the existing queue item. The same Message-ID with a different body digest, the same UID with a different Message-ID, or the same message for another task is a conflict and is not stored. The duplicate check, the inbound record and the queue item are written in one transaction. A reply to a task that does not accept replies is kept as `REJECTED`, with the reason `task_not_accepting`, `task_failed` or `task_closed`.

**Dispatch.** Replies leave the queue in local enqueue order, never by the sender's `Date` header, and only while the task is `COMPLETED` or `WAITING_INPUT`. Claiming a reply marks it `DISPATCHING`, records the state before dispatch and moves the task to `RUNNING`. A task has at most one reply in `DISPATCHING` or `UNCERTAIN` at a time. The API checks this, and a partial unique index enforces it in the database too. When the agent confirms receipt, the reply becomes `ACKNOWLEDGED`. If receipt cannot be confirmed, the reply becomes `UNCERTAIN` and a `RUNNING` task becomes `DELIVERY_UNCERTAIN`.

**Recovery.** At startup, `RecoverInFlight` turns every `DISPATCHING` reply into `UNCERTAIN` and returns all uncertain replies for a local check. It never dispatches anything again. Because it treats every `DISPATCHING` reply as left over from a crash, it must be called only by the single dispatching process, before that process starts dispatching, and while no other process holds a reply in flight; otherwise it would mark a reply that another process is still dispatching as `UNCERTAIN`. The single-instance lock that guarantees this belongs to the background service, which is not built yet. If the check finds the reply was delivered, the reply becomes `ACKNOWLEDGED` and a `DELIVERY_UNCERTAIN` task goes back to `RUNNING`. If it was not delivered, the reply returns to `QUEUED` with its original sequence number, and the task returns to `COMPLETED` or `WAITING_INPUT`, as it was before dispatch. If the task has been closed or has failed in the meantime, the reply is rejected instead. A reply can go back to the queue only if the task has recorded no event since the dispatch other than the uncertain delivery itself. Otherwise the agent has evidently handled it, and it must be resolved as delivered. There is no end-to-end exactly-once guarantee: the agent accepting a message and the SQLite commit cannot be one transaction, so an unconfirmed delivery waits for a local check instead of being resent.

## Dependencies

| Module | Version | License | Used by |
| --- | --- | --- | --- |
| `modernc.org/sqlite` | v1.59.0 | BSD-style | `internal/store/sqlite`. Pure Go SQLite, so builds keep `CGO_ENABLED=0`. |
| `github.com/BurntSushi/toml` | v1.6.0 | MIT | `internal/config`. The loader compares every key path from `MetaData.Keys()` against an allowlist, segment by segment and case-sensitively. `MetaData.Undecoded()` is not enough, because the library matches keys to struct fields case-insensitively. |

`modernc.org/sqlite` also brings in `dustin/go-humanize`, `google/uuid`, `mattn/go-isatty`, `ncruces/go-strftime`, `remyoudompheng/bigfft`, `golang.org/x/sys`, `modernc.org/libc`, `modernc.org/mathutil` and `modernc.org/memory`, all under MIT or BSD-style licenses.

Dependency rules:

- Only permissive licenses such as MIT, BSD, Apache-2.0 and ISC are accepted. A new dependency needs its reason and license stated in a plan or issue.
- Versions are pinned in `go.mod` and `go.sum`. `make check` runs `go mod verify`, and govulncheck and Dependabot cover the modules.
- Third-party license notices must be added before any binary that links these modules is released.

## Quality gates and CI

`make check` runs `fmt-check`, `vet`, `modverify`, `comments`, `test` and `lint`:

- `gofmt` must report no changes under `cmd`, `internal`, `tests` and `tools`, and `go vet ./...` must pass.
- `go mod verify` checks that the downloaded modules still match the hashes recorded in `go.sum`, so a tampered dependency cannot enter the build.
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

> **Planned.** Nothing in this section is implemented, except that Phase 3 built the task and queue state machines, TOML configuration loading and SQLite storage as internal packages; see [Current scope](#current-scope) and [Persistence and recovery](#persistence-and-recovery). It summarizes [docs/zh-CN/design.md](../zh-CN/design.md) and may change as later phases are verified. The full target directory tree is kept only in the design document. Directories are created when their code is written, not ahead of time.

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

Planned order of work: research, public engineering skeleton, configuration, storage and state machine, email round trip, Codex, Claude Code, security and recovery, real acceptance testing, release. The current phase is configuration, storage and the state machine: their packages exist, but no command uses them yet.
