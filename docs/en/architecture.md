# TurnCourier Architecture

Status: pre-alpha. Phase 4 is split into 4a (offline), L1 (a manual probe the maintainer runs against a real mailbox) and 4b (built on the L1 results). Phase 4a is done: the Keychain wrapper, reply tokens, body encryption, migration `0002`, the outgoing notification state machine and its storage, the SMTP and IMAP clients and `turncourier init` are implemented, and `init` is the first command that uses the configuration and storage packages. TurnCourier still cannot send or receive email.

This document covers two things: the code that exists in the repository today, and the architecture planned for later phases. Everything under [Planned architecture](#planned-architecture) is design intent. Apart from the parts that section marks as built in Phase 3 or 4a, none of it is implemented.

The approved design in [docs/zh-CN/design.md](../zh-CN/design.md) (Chinese) is authoritative. Related documents:

- [Phase 2 plan](../zh-CN/plans/phase-02.md)
- [Phase 3 plan](../zh-CN/plans/phase-03.md): configuration, storage and state machines
- [Phase 4 plan](../zh-CN/plans/phase-04.md): the mail loop, split into 4a, L1 and 4b
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
| CLI | `turncourier help`, `version` and `doctor`, each with an optional `--json` flag, and `init`, which takes no arguments. |
| Planned command stubs | `run`, `tasks`, `logs` and `service` print a "not implemented yet" message and exit with code 2. |
| Environment diagnostics | `doctor` reports the platform and runs `git`, `codex` and `claude` with `--version`. |
| Interactive setup | `init` (macOS and an interactive terminal only, never online) creates the configuration file, creates the instance ID, reads the authorization code without echoing it and registers the token and payload keys in the Keychain. It never overwrites an existing configuration file or an already registered key, and it replaces the authorization code only after an explicit confirmation. |
| Configuration | `internal/config` loads the TOML file, fills defaults, applies the one allowed environment override and validates everything. `Render` and `CreateFile` let `init` write a configuration file atomically, with `0600` and without overwriting. `configs/turncourier.example.toml` is the example, and tests load it. |
| State machines | `internal/task` and `internal/queue` hold the task, reply queue and outgoing notification transition tables. They are pure functions with no I/O. |
| Storage | `internal/store/sqlite` persists tasks, task events, inbound reply metadata, the reply queue, the instance ID, key metadata, outgoing notifications, encrypted pending bodies, fetch cursors, rejected inbound mail and per-task mail pauses, with embedded migrations, deduplication, FIFO dispatch and crash recovery. Only `init` opens the database so far. |
| Keychain | `internal/security/keychain` is a thin wrapper over `/usr/bin/security`. Secrets travel only over the subprocess's stdin and stdout, never in arguments or environment variables. Other platforms return "unsupported"; there is no plaintext fallback. |
| Reply tokens | `internal/security/token` issues, parses and verifies version 1 reply tokens (HMAC-SHA256 truncated to 128 bits, 48 Crockford base32 characters) and computes the keyed body digest. It also defines the subject tag `[TC <task id> <token>]` that carries a token in a message: `NewTag` builds one, and `ParseSubject` accepts a decoded subject only when the strict grammar matches it exactly once. It does not parse MIME or headers, and nothing outside the package uses the tag yet. |
| Body encryption | `internal/security/payload` seals and opens pending bodies with AES-256-GCM. The associated data binds each ciphertext to its purpose, key id, task and sequence number. |
| Mail clients | `internal/mail/smtp` submits one message over implicit TLS with a deadline on every step and classifies the outcome. `internal/mail/imap` logs in over implicit TLS, lists folders, examines read-only, rescans by UID, fetches with `BODY.PEEK` (only the header in the Sent folder) and idles, with a watchdog, backoff reconnects and a login rate limit. Neither package touches storage or the configuration, and neither modifies the mailbox. |
| Integration tests | `tests/integration` runs the example configuration, a real SQLite database and both state machines through a full reply lifecycle, and a second test drives notifications, tokens, the keyed digest, body ciphertext and dispatch together. |
| Manual probes | `tests/live` holds the L1 probes. They need the `live` build tag, `TURNCOURIER_LIVE=1` and interactive confirmations, refuse to run when `CI` is set, and are never run by `make test` or CI. The redaction helpers in `sample.go` carry no build tag and are tested offline. |
| Engineering checkers | `tools/commentcheck` checks for Chinese doc comments. `tools/covercheck` enforces the statement coverage threshold. |
| Quality gates | Makefile targets for gofmt, go vet, module checksum verification, comment checks, race tests with coverage, staticcheck, govulncheck, gitleaks and actionlint. |
| CI | GitHub Actions workflows for quality checks, security scans and a manually triggered macOS candidate build. |
| Research probes | Node.js scripts in `experiments/phase01/` that tested the Codex and Claude Code session interfaces and ran pure functions from a pinned competitor commit. They are research tools, not product adapters. |

CLI output meant for people is in Chinese. JSON field names are in English.

### Not implemented

- Sending or receiving email in the product. The SMTP and IMAP clients exist, but no command calls them, and nothing renders a notification or parses an inbound message. MIME parsing, quote and signature parsing, reply validation, the thread binding rules and the deterministic filtering of outgoing content are all Phase 4b. L1 is done: it answered how QQ Mail rewrites Message-IDs, how its own clients format a reply, how IDLE behaves and whether an Agent's shell can read the Keychain. What remains of it (L1b) is a re-check of the token-in-subject format with real clients, plus alias and `Return-Path` samples; the 4b task contracts are drafted in the plan and confirmed by the maintainer.
- Codex and Claude Code adapters. TurnCourier does not start, resume or watch agent sessions.
- Wiring the mail loop into commands. `internal/app`, which is to assemble the send and receive loop, does not exist; `init` is the only command that reads a configuration file or opens the database.
- Token revocation, key rotation (only key id 1 is created), `init`'s environment diagnostics and its real round-trip test.
- A background service, launchd integration, a menu bar, worktree management and telemetry.
- Release binaries. The manual candidate build uploads Actions artifacts that expire after 14 days. It does not create a GitHub Release.

## Packages and dependency direction

The module is `github.com/chaoRookie/turncourier`. It requires Go 1.27.1 (see `go.mod`). Besides the standard library, the packages import the modules listed under [Dependencies](#dependencies).

```text
cmd/turncourier ─► internal/cli ─► internal/doctor
                        ├──────► internal/config
                        ├──────► internal/store/sqlite ─► internal/task
                        │                 ├───────────► internal/queue
                        │                 └───────────► internal/security/payload
                        ├──────► internal/security/keychain
                        └──────► golang.org/x/term, golang.org/x/sys/unix (termios, macOS only)

internal/security/token      standard library only
internal/security/payload    standard library only
internal/security/keychain   standard library only (runs /usr/bin/security)
internal/mail/smtp           ─► github.com/emersion/go-smtp, go-sasl
internal/mail/imap           ─► github.com/emersion/go-imap/v2 (imapclient), go-message
                                (imapclient imports go-message/mail)
internal/mail                standard library only (constants of the mail contract)
internal/mail/parser         ─► internal/mail, go-message (mail, charset), x/text

tests/live (sample.go, no build tag) ─► go-message, go-message/charset, x/text
tests/live (probes, live build tag)  ─► config, security/keychain, security/token, mail/*, modernc.org/sqlite
                                        (a read-only query for the instance ID)
tests/integration ─► config, store/sqlite, task, queue, security/token, security/payload
tests/fixtures/mail ─► x/text (assembles the synthetic mail samples; imported by tests only)
tests/qqsim         ─► go-imap/v2 (imapserver, imapmemserver), go-smtp (server), go-sasl (server auth),
                       mail/imap and mail/smtp (folder names, client configs), tests/fixtures/mail (the reply template)
                       (offline QQ Mail simulator; imported by tests only)

tools/commentcheck          standalone checker, standard library only
tools/covercheck            standalone checker, standard library only
```

Dependencies point one way. `cmd` imports `internal/cli`, plus `internal/doctor` and `internal/security/keychain` to build the production checker and the Keychain factory it passes in. `internal/cli` is the only production assembler: `init` needs the configuration, the store, the Keychain and the terminal at once. `internal/store/sqlite` imports `internal/task`, `internal/queue` and `internal/security/payload`, because a body has to be encrypted in the same transaction that assigns its sequence number. It does not import `internal/security/token`: a notification id (nid) is stored as a `[12]byte`. It also still does not import `internal/config`; the bot domain, the token lifetime and normalized addresses are passed in by the caller.

`internal/mail` and its subpackages `smtp`, `imap` and `parser` depend on neither storage, configuration nor `internal/security/*`. They move and parse bytes; cursors and passwords arrive as values or as functions, and the parser takes the footer constants from `internal/mail` rather than from the token package. The three `internal/security` packages do not import each other. At the end of Phase 4a no production package imports both a mail package and storage; `internal/app`, which will, is Phase 4b work. Outside the product binary, `tests/live` is the second assembler.

Nothing imports `cmd` or `tools`. The tools are never linked into the product binary, but their code is still included in the coverage total.

The product binary currently links `github.com/BurntSushi/toml`, `modernc.org/sqlite` with its dependencies, `golang.org/x/term` and `golang.org/x/sys`. It does not link the `emersion` mail modules or `golang.org/x/text`: those are used only by tests and `tests/live` until Phase 4b wires the mail loop into a command. `go version -m dist/turncourier` shows the current list.

| Package | Responsibility |
| --- | --- |
| `cmd/turncourier` | Entry point. Turns SIGINT and SIGTERM into context cancellation, assembles `cli.Deps` (the production checker, terminal, Keychain factory, environment lookup, random source and clock) and exits with the code `cli.Run` returns. |
| `internal/cli` | Parses `help`, `version`, `doctor`, `init` and `--json`. Rejects planned commands, unknown commands and extra arguments, and writes text or JSON output. `cli.Version` defaults to `0.1.0-dev` and can be set at build time with `-ldflags -X`. `init.go` holds the interactive flow, `terminal.go` the `x/term` terminal, with the echo switch in `terminal_darwin.go` and a stub for other platforms. |
| `internal/config` | Resolves the configuration file and data directory paths, normalizes email addresses and loads the TOML file. It rejects unknown keys, credential keys, invalid addresses, a bot address in the sender allowlist, unknown or duplicate events, invalid ports, hosts and token lifetimes, and unsafe files. TOML syntax and type errors are returned on their own, without echoing the offending text. Unknown and credential keys are reported together before any value is checked. Once the file decodes cleanly, all remaining validation errors are returned together with `errors.Join`. `Render` produces a configuration file in the same shape as the example, and `CreateFile` writes it atomically with `0600` and never overwrites. No error contains local absolute paths. |
| `internal/task` | Task states (`CREATED`, `RUNNING`, `WAITING_INPUT`, `WAITING_APPROVAL`, `COMPLETED`, `FAILED`, `DELIVERY_UNCERTAIN`, `CLOSED`), events and the only transition table. It also decides which states accept replies and which can dispatch one. |
| `internal/queue` | Two separate transition tables, neither reusing the other and neither doing any I/O: reply queue states (`QUEUED`, `DISPATCHING`, `ACKNOWLEDGED`, `UNCERTAIN`, `REJECTED`) in `state.go`, and outgoing notification states (`PENDING`, `SENDING`, `SENT`, `UNCERTAIN`, `ABANDONED`) in `outbox.go`. |
| `internal/store/sqlite` | Opens and migrates the database, creates task IDs, persists tasks with optimistic versioning, deduplicates, queues, dispatches and recovers replies, and holds the instance ID, key metadata, outgoing notifications, encrypted pending bodies, fetch cursors, rejected inbound mail and per-task mail pauses. Every change to the state of a task, a reply or a notification runs in one transaction and goes through the state machines first. A mail pause is not a state in either machine: `PauseMail` and `ResumeMail` write the task's single `mail_pauses` row directly. Every input validation error wraps `ErrInvalidArgument`, and `ValidMessageID` and `ValidSender` export the rules the store applies to those fields. |
| `internal/security/keychain` | Generic-password entries under the service `io.github.chaorookie.turncourier`. `Set` starts one `security -i` process per call and sends a single `add-generic-password -U … -X <hex>` line on stdin, then reads the value back to confirm it; `Add` is the same without `-U`, so it only ever creates. `Get` uses `find-generic-password -w` and reads only stdout. Exit code 44 means "not found". Keys are 32 random bytes stored as 43 `base64url` characters; `KeyCheck` derives the 8-byte value registered in the database. |
| `internal/security/token` | Reply token v1: 30 bytes (version, key id, a 12-byte random nid, a 16-byte truncated HMAC-SHA256 tag) encoded as 48 Crockford base32 characters. The task, owner and expiry are read from the notification and task rows and enter the MAC without travelling in the token. `Verify` checks the key id, the claims, the tag with `hmac.Equal` and only then the expiry, so a tampered expired token reports a bad signature. `Key.BodyDigest` computes the keyed body digest. `subject.go` holds the subject tag, the only place a reply's token is read from (D4). `NewTag` takes a task ID of 10 alphabet characters and a token that came from `Issue` or `Parse`. `ParseSubject` counts the non-overlapping matches of the strict grammar `[TC <10 alphabet characters> <48 alphabet characters>]` (lowercase only, exactly one U+0020 between the task ID and the token) in a decoded subject and repairs nothing: no match is `ErrTagMissing` if the subject has no `[TC` (ASCII case-insensitive) and `ErrTagDamaged` if it does, two or more are `ErrTagMultiple`, and a single match whose token does not parse is `ErrMalformed`. No error echoes the subject. Tokens, keys and subject tags redact themselves in `String`, `Format` and `LogValue`; only `Token.Reveal`, `Tag.Reveal` and `Tag.RevealToken` return the text, and `tests/docs/reveal_test.go` lets product code reference them only in this package and in `internal/mail/renderer`. |
| `internal/security/payload` | AES-256-GCM over pending bodies, with a random 96-bit nonce. The ciphertext is `0x01 ‖ nonce ‖ ciphertext ‖ tag`, 29 bytes longer than the plaintext, and the plaintext is capped at 1 MiB. The associated data binds the purpose (`r` for a reply, `n` for a notification), the key id, the task ID and the sequence number, so a ciphertext moved to another row will not open. |
| `internal/mail/smtp` | One message over implicit TLS through go-smtp's `DialTLS` only, never STARTTLS or plaintext, with debug output off because it would print credentials. Each step has its own deadline, the body is written in 64 KiB chunks, and the outcome is classified as sent, definitely not sent, rejected or uncertain. It does not render mail, retry or touch storage. |
| `internal/mail/imap` | Read-only mail collection: implicit TLS, `LIST`, `EXAMINE`, rescan by UID, `BODY.PEEK` fetches (`ScanHeaders` fetches only `BODY.PEEK[HEADER]`, at most 64 KiB per message) and `IDLE`, each command with a deadline whose expiry closes the connection. That is what stops QQ's bare `* BAD` and half-open connections from blocking forever. `watcher.go` runs the long-lived loop with backoff reconnects, a login rate limit and an auth-failure pause. With `Sent` set, each round scans the headers of `Sent Messages` first, then `INBOX` and `Junk`, and returns to `INBOX` to wait; a failed Sent scan skips Sent for that round (if the failure closed the connection, the round resumes at `INBOX` after the reconnect) and is retried the next round; unlike `Junk`, which is skipped for an hour after three consecutive failures, Sent is never degraded. A message that Sent lists but whose header cannot be fetched (no data, a `NIL` literal or a section other than `BODY[HEADER]`) fails the round rather than being skipped as deleted, because a completed Sent scan is the evidence that a notification's copy is missing. `Wake` ends an `IDLE` or poll wait early, `Scanned` reports that a folder's round completed and when it started (by the injected clock `Now`), `SkipHistory` lets a first run start at `UIDNEXT - 1` instead of fetching old mail, and a handler that returns `ErrDefer` ends that folder's round without a retry, so the deferred mail is delivered again next round. It never sets `\Seen`, moves, expunges or appends. |
| `internal/mail` | The mail contract (`gateway.go`): constants shared by the parser, the renderer and the code that assembles them, namely the footer marker line, the fixed sentence of the footer and the `X-TurnCourier-ID` header name. Constants only; it imports nothing. |
| `internal/mail/parser` | Parses inbound mail without judging it. `Parse` reads the header and MIME structure into a `Message` (`ParseHeader` reads the header only): the single From address, the decoded subject, the Message-ID and thread headers, the auto-reply signals, the top-level media type and the two IDs that match a Sent copy. `NewText` takes the first non-attachment `text/plain` part (nesting depth at most 8, at most 32 parts per level, at most 1 MiB decoded), decodes its charset (the GBK labels map to GB18030), normalizes line breaks and strips quotes and signatures by fixed rules. Inline replies, `>` lines that do not quote a TurnCourier notice, and any token-shaped string, `[TC`, footer marker or footer sentence left after stripping make it return `ErrUncertain`. `Classify` sorts a message into a human reply, an auto-reply or a bounce from the header signals, the subject prefix and the opening of the plain and HTML bodies, bounces first, and names the signal that matched; it is meant to run before any tag or token check. Errors are fixed sentinels that never echo message bytes; `Message` redacts itself in `String`, `Format`, `LogValue` and `MarshalJSON`. The output is deterministic, which the keyed body digest relies on. |
| `internal/doctor` | Read-only diagnostics. The platform, the `PATH` lookup, the version runner and the timeout are injectable, so tests run offline. |
| `tools/commentcheck` | Walks the source tree and fails if a package, type or named function has no Chinese doc comment. |
| `tools/covercheck` | Reads a Go cover profile and fails if statement-weighted coverage is below the minimum. |
| `tests/integration` | Test-only package. Loads a copy of the example configuration, opens a real database in a temporary directory and drives a task and its replies from start to close, including a simulated crash, plus a second lifecycle over notifications, tokens, the keyed digest and body ciphertext. |
| `tests/docs` | Test-only package. Reads `go.mod` and the imports of every non-test Go file, and fails when the dependency tables in this document and in `docs/zh-CN/development.md` miss a direct module, disagree with its pinned version, or fail to name a package that imports it directly. `imports_test.go` fails when a package breaks the dependency direction in the plan, for example when `internal/mail` imports storage. `reveal_test.go` parses every non-test Go file under `cmd/` and `internal/`, including directories whose names start with `.` or `_` and `testdata` directories, since an explicit import still compiles them into the binary. It fails when a selector named `Reveal` or `RevealToken` (a call, a method value or a method expression) appears outside `internal/security/token` and `internal/mail/renderer`. It cannot see a call made by name through reflection or a template field such as `{{.Reveal}}`; those are left to review. |
| `tests/fixtures/mail` | Test infrastructure, never linked into the binary. Synthetic mail samples as JSON templates (decoded text with declared charsets and transfer encodings, and placeholders for the task ID, the token and the delivered Message-ID) with their expected results, plus package `mailfixture`, which substitutes values issued at run time and assembles the raw bytes with the standard library and x/text. A template may also declare optional placeholders with defaults; `qq-app-reply` does so for the sender, the subject prefix and original subject, the body and the quoted original, which is how `tests/qqsim` composes replies. All placeholders are replaced in one pass, so a value is never re-read as a template, and values inside `text/html` parts are HTML-escaped. `README.md` records whether each sample follows a structure measured in L1 or one inferred from public material. |
| `tests/qqsim` | Test infrastructure, never linked into the binary: an offline QQ Mail simulator for the tests of `internal/app`, `internal/cli` and `tests/e2e`. It serves SMTP (go-smtp's server, AUTH PLAIN through go-sasl's server) and IMAP (`imapserver` over `imapmemserver`) with implicit TLS on random ports of `127.0.0.1`, with certificates from a CA generated at start that callers trust through `RootCAs`. It reproduces what L1 measured: one bot account; `INBOX`, `Junk` and `Sent Messages` sharing one UIDVALIDITY; accepted mail gets its `Message-Id` rewritten to `<tencent_…@qq.com>` with the original kept in `X-OQ-MSGID`, a copy in `Sent Messages` and a recipient copy on the `Delivered()` channel, and the 250 reply to DATA carries no ID. `Reply` composes a reply with the QQ Mail app's structure, and `Deliver` appends bytes to a folder. `SetFaults` injects every failure the 4b contract lists: 4xx and 5xx replies at AUTH, MAIL, RCPT, the DATA command and after the end of data; closing the connection after the end of data without a reply; a Sent copy written late (on an injected clock, or at `ReleaseSent`) or never; `Sent Messages` refusing EXAMINE or missing from LIST; and IMAP login failure. `DisconnectAll` drops every connection at once. IMAP clients cannot modify the mailbox. The server debug output, which would print the authorization code, stays off. |
| `tests/live` | Manual L1 probes, never run by `make test` or CI. `sample.go` carries no build tag and holds the pure redaction, classification and output-directory checks; the three probe files carry `//go:build live` and only do I/O and confirmations. |

Command parsing rules:

- Running with no arguments is the same as `help`. `--help` and `-h` also mean `help`, and `--version` means `version`.
- The only argument accepted after `help`, `version` or `doctor` is a single `--json`. `init` takes no arguments at all, `--json` included. An invalid command name is reported before an invalid argument.
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
| 0 | Success. For `doctor`, the report is ready. For `init`, the configuration file, the instance ID, the authorization code and both keys are in place, and the summary is on stdout. |
| 1 | `doctor` is not ready, `init` failed, or writing the output failed. Not ready means the platform is not macOS, or `git`, `codex` or `claude` is missing, timed out, was cancelled, failed to run or produced unexpected output. A not-ready report is still written in full. `init` prints one Chinese line on stderr saying why: not macOS, not an interactive terminal, an existing configuration file that does not load, key metadata whose Keychain entry is missing or no longer matches, three invalid answers in a row, or a Keychain or database error. Ctrl-C prints "已取消。". Error text contains no paths and no secrets. |
| 2 | Usage error: an unknown command, an unexpected argument (including `--json` after `init`), or a planned command (`run`, `tasks`, `logs`, `service`). |
| 141 | Not returned by the program. If stdout or stderr is a closed pipe, the process is killed by SIGPIPE, following Unix convention, and the shell reports 141 (128 + 13). |

## Persistence and recovery

These rules are implemented and tested in `internal/config` and `internal/store/sqlite`. Only `init` uses them so far.

**Locations.** The configuration file is `$TURNCOURIER_CONFIG`, which must be an absolute path, or else `TurnCourier/turncourier.toml` under `os.UserConfigDir()` (on macOS, `~/Library/Application Support/TurnCourier/turncourier.toml`). The data directory is `$TURNCOURIER_DATA_DIR`, which must also be absolute, or else the directory that holds the configuration file. The database file is `turncourier.db` in the data directory. `TURNCOURIER_NOTIFY_EVENTS` (comma-separated) is the only setting an environment variable can override. The sender allowlist, addresses and token lifetime cannot be overridden this way.

**Configuration file.** It must be a regular file of at most 1 MiB. On Unix it must belong to the current user and must not be writable by group or others. Key names are case-sensitive, so a variant such as `ADDRESS` counts as an unknown key. Unknown keys and credential keys such as `password` or `token` (in any letter case) are rejected: the mail authorization code and the keys belong in the Keychain, where `init` puts them. `init` writes this file itself, atomically, with `0600` and without ever overwriting an existing one.

**Database.** On Unix the data directory must be `0700` and the database file `0600`. Both are created with those modes, and wider existing modes are refused. The connection uses WAL, `synchronous=FULL`, foreign keys, `secure_delete`, and a 5-second busy timeout, and write transactions start with `BEGIN IMMEDIATE`. `secure_delete` is `ON`, not `FAST`, because `FAST` does not zero overflow pages, which is where a body of more than a few kilobytes lives. Each process uses a single connection, and all tables are `STRICT`. When several processes open a new database at once, switching the empty file to WAL can fail with `SQLITE_BUSY` without waiting for the busy timeout, so opening retries for up to 5 seconds. Migrations are embedded SQL files. Each one runs in a transaction together with the `user_version` update and reads the version again inside that transaction, so concurrent openers apply it only once. A database with a newer schema than the build knows is refused with its tables and data untouched, although opening it may already have switched the file to WAL mode.

**Durability.** `synchronous=FULL` makes committed data survive a process crash. On macOS it does not guarantee durability after a power loss or a kernel panic: the SQLite driver uses `F_FULLFSYNC` only when `PRAGMA fullfsync=1` is set, and a plain `fsync` there does not force the data onto stable storage. Turning on `fullfsync`, which holds the write lock longer, is to be evaluated in the later security and recovery phase.

**What is stored.** Migration `0001` created tasks (a 10-character random ID, owner, agent, agent session ID, state, version and timestamps), task events, inbound reply metadata and reply queue items. Times are UTC Unix milliseconds throughout. Migrations `0002` and `0003` add the rest of the mail loop:

| Table | Contents |
| --- | --- |
| `inbound_messages` | Rebuilt with a `folder` column. A UID is unique only within one folder (RFC 3501), so the UID key is now `(account, folder, uid_validity, uid)`. The `(account, message_id)` key is unchanged. `body_sha256` keeps its name, published in `0001`, but from `0002` on it holds the keyed digest. |
| `replies` | Rebuilt alongside `inbound_messages`, because it references that table and a migration runs inside a transaction, where foreign keys cannot be switched off. Its columns, the partial unique index for one in-flight reply per task and the `AUTOINCREMENT` sequences are all preserved; existing `inbound_messages` rows get the folder `INBOX`. |
| `instance` | One row with the instance ID: 10 random bytes as 16 lowercase Crockford base32 characters, created by `init` and stable for as long as the data directory is. |
| `crypto_keys` | Key metadata only, never key material: purpose (`token` or `payload`), key id 1–255, state (`active`, `retired`, `destroyed`), an 8-byte check value and timestamps. A partial unique index allows one `active` key per purpose. |
| `notifications` | One row per outgoing notification: task, event, the 12-byte nid, our Message-ID, the delivered Message-ID, the token key id and expiry, state, abandon reason, attempts, `not_before` and timestamps. A partial unique index allows at most one `SENDING` row globally, so sending is serial. Triggers make the fields that enter the token MAC immutable and let the delivered Message-ID be written only once. |
| `notification_payloads`, `reply_payloads` | The ciphertext of a pending body, with its key id. Triggers reject an insert unless the notification is `PENDING` or the reply is `QUEUED`, reject any update, and delete the row in the same transaction that moves the parent to a terminal state. |
| `fetch_cursors` | `(account, folder)` with its UIDVALIDITY and last handled UID. |
| `inbound_rejections` | Metadata for a message that was refused: account, folder, UID, Message-ID, sender, a reason code and the task, but never a subject or a body. The database only constrains the reason's shape (1–40 characters of `[a-z_]`); the list of valid codes lives in Go, so Phase 4b adds to it without a migration. Migration `0003` adds a partial index on `(account, folder, message_id)` for rows that have a Message-ID: after a UIDVALIDITY reset every UID changes but the Message-ID does not, so that is how a message refused before the reset is recognized. |
| `mail_pauses` | Added in `0003`. At most one row per task: the reason its mail trigger was paused (`hourly` or `daily`, the two limits of the loop brake), when it was paused and when it was resumed. A pause never lifts on its own; only a local resume does (the store call exists, the command does not yet). The resume time is kept so that the brake counts only replies accepted after it. |

There is still no column for a plaintext body or a credential.

**Pending bodies.** Only bodies that still have work to do are on disk, and only as ciphertext: a notification's content while it is `PENDING`, a reply's body while it is `QUEUED`. The key is read from the Keychain at startup and passed in through `sqlite.Options`; without it, every operation that would encrypt or decrypt a body fails rather than falling back to plaintext. A body is encrypted inside the same transaction that assigns its sequence number, because the sequence number is part of the associated data. When the parent row reaches a terminal state, a trigger deletes the ciphertext in that same transaction; `UNCERTAIN` keeps its body, because a local check may still put the item back in the queue. After a cleanup, and once more when the database is opened, the store makes a best-effort `PRAGMA wal_checkpoint(TRUNCATE)` so that no stale frame is left in the WAL file. That checkpoint temporarily sets `busy_timeout` to 0: it holds the single connection, so waiting for a reader would block everything else in the process. If it is busy it gives up immediately and leaves the work to the next one. What SQLite cannot reach is a copy outside the file: ciphertext left in an APFS snapshot or a Time Machine backup can still be decrypted while the payload key is in the Keychain.

**Keys and the Keychain.** The service is `io.github.chaorookie.turncourier` and the account is `<instance id>:<purpose>`: `qq-auth-code`, `token-key-<kid>` and `payload-key-<kid>`. Putting the instance ID in the account name keeps two data directories from overwriting each other's entries. Phase 4a only ever creates key id 1, so after `init` there are exactly three entries; a rotation would add an entry rather than replace one, which is why rotating the signing key cannot make an already queued ciphertext unreadable. The database records the purpose, the key id, the state and an 8-byte check value: HMAC-SHA256 keyed with the key material itself, over a fixed prefix, the purpose and the key id, so replacing the key changes the value. `init` compares that value with the entry in the Keychain: if the metadata is registered but the entry is missing, or the entry no longer matches, `init` refuses to regenerate or overwrite, because doing so would make pending data undecryptable.

**Outgoing notifications.** A notification is `PENDING`, `SENDING`, `SENT`, `UNCERTAIN` or `ABANDONED`. Claiming one is serial by construction — the partial unique index allows a single `SENDING` row — and it returns the decrypted content. Before claiming, the store abandons any `PENDING` notification whose token would expire within ten minutes, with the reason `expired`. If SMTP confirms delivery the row becomes `SENT`; if it is definitely not delivered it goes back to `PENDING` with a delay; and if the outcome cannot be known it becomes `UNCERTAIN`. A local check resolves it either way; once Phase 4b wires it in, the notification's copy in the Sent folder can also resolve it, but only as delivered. A notification resolved as delivered keeps the moment it became `UNCERTAIN` as its send time, so resolving it late changes neither which notification counts as the latest one sent nor how many were sent in the last hour. A notification is never resent automatically. Closing a task abandons its `PENDING` notifications with `task_closed` but leaves `SENDING` and `UNCERTAIN` alone, since their outcome is not yet known; a closed task's notification never returns to `PENDING`.

**Writes.** Every change to the state of a task, a reply or a notification runs in one transaction. The new state comes from `internal/task` or `internal/queue`, and the task row is updated only if its version still matches, so a caller holding a stale version gets a conflict error. `start`, `reply_dispatched`, `delivery_unknown` and `delivery_confirmed` happen only as part of the matching storage operation. `fail` and `close` reject every queued reply of the task in the same transaction. Mail pauses have no state machine: `PauseMail` checks that the task exists and writes its row in one transaction, leaving a pause that is already in place untouched, and `ResumeMail` is a single update of that task's row.

**Deduplication.** An inbound reply is unique by `(account, folder, UIDVALIDITY, UID)` and by `(account, Message-ID)`. The folder is part of the UID key only: the same message copied into both `INBOX` and `Junk` has two UIDs but one Message-ID, so it is still recognized as a duplicate. The body digest is the keyed HMAC-SHA256 described above, computed with the token key of the notification the reply refers to. The same message recorded again returns the existing queue item. The same Message-ID with a different body digest, the same UID with a different Message-ID, or the same message for another task is a conflict and is not stored. The duplicate check, the inbound record and the queue item are written in one transaction. A reply to a task that does not accept replies is kept as `REJECTED`, with the reason `task_not_accepting`, `task_failed` or `task_closed`.

**Dispatch.** Replies leave the queue in local enqueue order, never by the sender's `Date` header, and only while the task is `COMPLETED` or `WAITING_INPUT`. Claiming a reply marks it `DISPATCHING`, records the state before dispatch and moves the task to `RUNNING`. A task has at most one reply in `DISPATCHING` or `UNCERTAIN` at a time. The API checks this, and a partial unique index enforces it in the database too. When the agent confirms receipt, the reply becomes `ACKNOWLEDGED`. If receipt cannot be confirmed, the reply becomes `UNCERTAIN` and a `RUNNING` task becomes `DELIVERY_UNCERTAIN`.

**Recovery.** At startup, `RecoverInFlight` turns every `DISPATCHING` reply into `UNCERTAIN` and returns all uncertain replies for a local check. It never dispatches anything again. `RecoverSendingNotifications` is its counterpart on the outgoing side: it turns every `SENDING` notification into `UNCERTAIN` and returns all uncertain notifications for a local check, never resending anything. Both treat every in-flight row as left over from a crash, so each must be called only by the single process that does that work, before that process starts working, and while no other process holds an item in flight; otherwise they would mark an item another process is still handling as `UNCERTAIN`. The single-instance lock that guarantees this belongs to the background service, which is not built yet. If the check finds the reply was delivered, the reply becomes `ACKNOWLEDGED` and a `DELIVERY_UNCERTAIN` task goes back to `RUNNING`. If it was not delivered, the reply returns to `QUEUED` with its original sequence number, and the task returns to `COMPLETED` or `WAITING_INPUT`, as it was before dispatch. If the task has been closed or has failed in the meantime, the reply is rejected instead. A reply can go back to the queue only if the task has recorded no event since the dispatch other than the uncertain delivery itself. Otherwise the agent has evidently handled it, and it must be resolved as delivered. There is no end-to-end exactly-once guarantee: the agent accepting a message and the SQLite commit cannot be one transaction, so an unconfirmed delivery waits for a local check instead of being resent.

## Dependencies

| Module | Version | License | Used by |
| --- | --- | --- | --- |
| `modernc.org/sqlite` | v1.59.0 | BSD-style | `internal/store/sqlite`. Pure Go SQLite, so builds keep `CGO_ENABLED=0`. `tests/live` also opens the database read-only to read the instance ID. |
| `github.com/BurntSushi/toml` | v1.6.0 | MIT | `internal/config`. The loader compares every key path from `MetaData.Keys()` against an allowlist, segment by segment and case-sensitively. `MetaData.Undecoded()` is not enough, because the library matches keys to struct fields case-insensitively. |
| `golang.org/x/term` | v0.46.0 | BSD-3-Clause | `internal/cli`. Reads the authorization code without echoing it during `init`. |
| `github.com/emersion/go-smtp` | v0.25.0 | MIT | `internal/mail/smtp`, and its server side as the offline fake server in tests and in the QQ Mail simulator `tests/qqsim`. Chosen over the frozen `net/smtp` because it has command timeouts and because `CloseWithResponse` returns the server's DATA response, which L1 needs to find the ID QQ assigns. It does not itself refuse to send credentials over plaintext, so only `DialTLS` is allowed and a source test pins that. |
| `github.com/emersion/go-imap/v2` | v2.0.0-beta.8 | MIT | `internal/mail/imap` uses `imapclient`; `imapserver` and `imapmemserver` appear only in tests and in the QQ Mail simulator `tests/qqsim`. Still a beta with breaking changes between betas, so the version is exact, the library is wrapped and an upgrade PR gets a human review. |
| `github.com/emersion/go-message` | v0.18.2 | MIT | `internal/mail/parser` reads the header and MIME structure of inbound mail with it and decodes charsets through `charset`, where it maps the GBK labels to GB18030. It also reaches `internal/mail/imap` because `imapclient` imports `go-message/mail`. `tests/live` imports it directly, with `charset` for GB18030 and GBK decoding. |
| `github.com/emersion/go-sasl` | `b788ff22d5a6` | MIT | `internal/mail/smtp` uses `NewPlainClient` for `AUTH PLAIN`, and `tests/qqsim` uses `NewPlainServer` for the simulator's AUTH; go-imap/v2 also pulls it in. |
| `golang.org/x/text` | v0.42.0 | BSD-3-Clause | `internal/mail/parser` and `tests/live` (`sample.go`) import `encoding/simplifiedchinese` directly to map the GBK labels to the GB18030 decoder, and `go-message/charset` needs it too. `tests/fixtures/mail` uses its GB18030 encoder to assemble the synthetic regression samples. Pinned explicitly: the v0.14.0 that go-message asks for carries GO-2026-5970, fixed in v0.39.0. |
| `golang.org/x/sys` | v0.48.0 | BSD-3-Clause | `internal/cli` turns terminal echo off through termios on macOS. Also required by `golang.org/x/term` and `modernc.org/sqlite`. |

`modernc.org/sqlite` also brings in `dustin/go-humanize`, `google/uuid`, `mattn/go-isatty`, `ncruces/go-strftime`, `remyoudompheng/bigfft`, `modernc.org/libc`, `modernc.org/mathutil` and `modernc.org/memory`, all under MIT or BSD-style licenses. `golang.org/x/sys` is pinned at v0.48.0, which `golang.org/x/term` v0.46.0 requires.

The product binary links only `BurntSushi/toml`, `modernc.org/sqlite` with its dependencies, `golang.org/x/term` and `golang.org/x/sys`. The four mail modules and `golang.org/x/text` are used by tests and `tests/live` only; they enter the binary in Phase 4b, when `internal/app` wires the mail loop into a command. `go version -m dist/turncourier` is the check.

Dependency rules:

- Only permissive licenses such as MIT, BSD, Apache-2.0 and ISC are accepted. A new dependency needs its reason and license stated in a plan or issue.
- Versions are pinned in `go.mod` and `go.sum`. `make check` runs `go mod verify`, and govulncheck and Dependabot cover the modules.
- Third-party license notices must be added before any binary that links these modules is released.

## Quality gates and CI

`make check` runs `fmt-check`, `vet`, `modverify`, `comments`, `test` and `lint`:

- `gofmt` must report no changes under `cmd`, `internal`, `tests` and `tools`, and `go vet ./...` must pass. `vet` and `lint` each run a second time with `-tags live` over `tests/live`, so the probe code is compiled and checked even though it is never run.
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

> **Planned.** Nothing in this section is implemented, except what Phase 3 and Phase 4a built: the state machines, TOML configuration loading, SQLite storage, the Keychain wrapper, reply tokens, body encryption, the SMTP and IMAP clients and `init`; see [Current scope](#current-scope) and [Persistence and recovery](#persistence-and-recovery). It summarizes [docs/zh-CN/design.md](../zh-CN/design.md) and may change as later phases are verified. The full target directory tree is kept only in the design document. Directories are created when their code is written, not ahead of time.

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
- There is no end-to-end exactly-once guarantee. The agent accepting a message and the SQLite commit cannot be one transaction. If the acknowledgment is lost, the item goes to an uncertain state, and it is not resent until the agent side is shown to handle duplicates safely. SMTP can also accept a message without the local side getting confirmation, so duplicate notifications are still possible. A stable Message-ID of our own does not help on the recipient's side: L1 showed that QQ rewrites it and keeps ours only in `X-OQ-MSGID`, so thread binding uses the Message-ID QQ actually delivered.

### Security principles (planned)

- **Secrets.** The QQ Mail authorization code and the signing key live in the macOS Keychain. The TOML file holds only the account and options. Environment variables can temporarily override non-sensitive settings only. The authorization code will be entered locally during `init`, never in chat and never in the repository.
- **Sender identity.** The sender address is normalized and must exactly match the allowlist. A display name or a longer address that merely contains an allowed address is rejected. Sender headers can be forged, so they are never enough on their own.
- **Reply binding.** The thread reference headers, the short task ID and the signed token must all agree. If one is missing or they conflict, the reply is rejected. The task ID alone is never enough. The token travels in the subject tag, `[TC <task id> <token>]`, and a copy is repeated in the notification footer. The L1 probe settled this on 2026-09-20: a real QQ web reply can carry no quoted original at all, which loses a footer token, while the subject tag survived in every client tested. The subject is the only verification input; the footer copy is there for the reader and is never read back, because in a reply it always lands in the quoted region that the parser strips. Moving the token out of the body is a real cost: it now shows up in lock-screen and notification previews, in mailbox listings, in server-side subject indexes and in client full-text indexes, and it travels along the thread through forwards, bounces, auto-replies and the user's own replies. Expiry, revocation on task close and a dedicated bot mailbox are what bound that cost. The thread reference is the Message-ID QQ actually delivered, which is not the one we sent; `X-OQ-MSGID` maps it back. QQ adds neither `Authentication-Results` nor `Received` to same-domain mail, so neither can be used to judge where a reply came from: the sender allowlist cannot be verified locally and the token is the only unforgeable part. Tokens are bound to the task, user, expiry time and notification ID. They are valid for 72 hours by default (configurable from 1 to 720 hours; shortened from seven days after L1) and are revoked as soon as the task closes. A signature proves integrity; it does not encrypt anything. Tokens are bearer credentials and must not appear in public logs.
- **Deduplication.** Besides the Message-ID, each received record stores the mailbox account, UIDVALIDITY, UID and a body digest. The SQLite unique constraint and the enqueue happen in the same transaction. The same Message-ID with a different body is rejected and triggers a warning.
- **Data at rest.** By default only metadata history is kept. Undelivered replies and outgoing mail must keep their bodies so they can be recovered. That pending data is encrypted on disk with a key stored in the Keychain, and is deleted after completion. Users must be told about this exception.
- **Agent permissions.** A Git worktree isolates code changes. It is not an operating system sandbox. The agent's own sandbox and permission controls stay in place, email cannot set parameters that skip approvals, and approval events are sent for local handling.
- **Outgoing content.** Before sending, mail goes through deterministic filtering of sensitive information and trimming of code blocks and long content. Filtering cannot promise to catch every secret, so status-only notifications must be available. Diffs and reports are attached only if the user turns that on.
- **Telemetry.** Off by default and only sent if the user turns it on. It must never upload message bodies, code, paths, repository URLs, email addresses or identity information.

### Release criteria (planned)

Before `v0.1.0-alpha`, Codex and Claude Code must each complete ten consecutive real email rounds. Those rounds must cover recovery after a network outage, duplicate, forged and expired mail, FIFO while the agent is busy, and privacy filtering. The release would then publish macOS binaries with SHA-256 checksums, with cross-platform packages and Homebrew later. A successful build does not replace real acceptance testing.

Planned order of work: research, public engineering skeleton, configuration, storage and state machine, email round trip, Codex, Claude Code, security and recovery, real acceptance testing, release. The current phase is the email round trip: its offline half (4a) is built and tested, and the rest waits for the L1 probe against a real mailbox.
