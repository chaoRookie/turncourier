# TurnCourier

[简体中文](README.zh-CN.md)

Email bridge for Codex and Claude Code, under development: the goal is to continue a local agent session by replying to an email.

## Status

Pre-alpha. The Go command-line program offers only `help`, `version` and a read-only `doctor`. The repository also has quality gates and CI.

Configuration, storage and the state machines are implemented and tested as internal packages, but no command uses them yet:

- `internal/config` loads and strictly validates the TOML configuration ([example](configs/turncourier.example.toml)).
- `internal/task` and `internal/queue` are the task and reply queue state machines.
- `internal/store/sqlite` stores tasks and reply metadata in SQLite, with migrations, deduplication, a FIFO reply queue and crash recovery. It stores no email bodies, only their SHA-256 digests.

TurnCourier cannot send or receive email yet. There are no agent adapters, no Keychain access and no background service. There are no releases or tags; the default branch is `main`.

The Phase 0–1 research probes (Node.js scripts, not product code) resumed the same Codex and Claude Code sessions from a new process on one Mac. Claude Code's first attempt exited abnormally on its second turn, for a reason not yet determined; the retest passed. No real mail round trip has been tested. See the [research report](docs/zh-CN/research/phase-01.md) (Chinese) for evidence and limits.

## Planned experience

> Planned. None of this is implemented.

You start a Codex or Claude Code task through TurnCourier on your Mac. By default, when a turn completes, the agent waits for input or the task fails, TurnCourier emails the status and the agent's reply from a dedicated QQ Mail account to your own mailbox. You answer by replying to that email, and after sender, thread and signed-token checks the reply becomes the next user message in the same agent session. Email cannot approve tool permissions. The first release targets macOS, Codex CLI, Claude Code CLI and QQ Mail; the mail authorization code is to be entered locally with `turncourier init` and stored in the macOS Keychain. The [design](docs/zh-CN/design.md) (Chinese) defines the scope, security boundaries and target directory tree.

## Quick start

Build from source. You need Go 1.27.1 (as declared in `go.mod`), `make` and `git`.

```sh
git clone https://github.com/chaoRookie/turncourier.git
cd turncourier
make build
./dist/turncourier help
./dist/turncourier version --json
./dist/turncourier doctor --json
```

If Go 1.27.1 is not on your `PATH`, either unpack it into `.local/toolchains/go` (ignored by Git) and run `export PATH="$PWD/.local/toolchains/go/bin:$PATH"`, or pass the toolchain to Make: `make build GO=/path/to/go`.

Text output is currently in Simplified Chinese. With `--json`, keys and status values are in English; `description` and `detail` text is in Chinese.

Example `doctor --json` on macOS with all three tools installed (your versions will differ):

```json
{
  "platform": "darwin",
  "supported": true,
  "ready": true,
  "tools": [
    {
      "name": "git",
      "status": "ok",
      "version": "git version 2.54.0"
    },
    {
      "name": "codex",
      "status": "ok",
      "version": "codex-cli 0.154.0"
    },
    {
      "name": "claude",
      "status": "ok",
      "version": "2.1.263 (Claude Code)"
    }
  ]
}
```

`ready` is `true` only on macOS when all three tools report `ok`. Any other tool status (`missing`, `error`, `timeout`, `invalid`, `cancelled`) comes with a short `detail` instead of `version`.

`doctor` runs only `git --version`, `codex --version` and `claude --version`. Each check has a 3-second timeout; on timeout the version process is killed (on Unix, its whole process group), and output is collected for at most 1 more second. The report contains no executable paths or raw error messages. It does not check logins, permissions, agent sessions or mailboxes.

## Commands

| Command | Description | Exit codes |
| --- | --- | --- |
| `help [--json]` | Usage, available commands and planned commands. Also runs with no arguments, `-h` or `--help`. | 0 |
| `version [--json]` | Version string. `make build` sets `0.1.0-dev`. `--version` is an alias. | 0 |
| `doctor [--json]` | Read-only check of the platform and the `git`, `codex` and `claude` versions. | 0 ready, 1 not ready |
| `init`, `run`, `tasks`, `logs`, `service` | Planned. Prints that the command is not implemented yet. | 2 |
| Any other command or argument | Usage error. | 2 |

Exit codes:

- `0`: success.
- `1`: `doctor` is not ready (the platform is not macOS, or `git`, `codex` or `claude` is missing, fails, times out, is interrupted or prints unexpected output), or writing the output failed.
- `2`: usage error, unknown command, or a planned command.
- If stdout is a closed pipe, the process is terminated by `SIGPIPE`, following Unix convention (shell exit status 141).

## Development

Contributors should read [CONTRIBUTING.md](CONTRIBUTING.md) and the [development guide](docs/zh-CN/development.md) (Chinese).

| Target | What it does |
| --- | --- |
| `make build` | Builds `dist/turncourier`. |
| `make fmt`, `make fmt-check` | Formats, or checks formatting of, `cmd`, `internal`, `tests` and `tools`. |
| `make vet` | `go vet ./...` |
| `make modverify` | `go mod verify`: module checksums must match `go.sum`. |
| `make comments` | `tools/commentcheck`: Chinese doc comments on packages, types and named functions. |
| `make test` | `go test -race` writing `coverage.out`; `tools/covercheck` fails below 80% statement coverage of all hand-written Go code, with no exclusions. |
| `make lint` | staticcheck v0.8.1, with ST1020–ST1022 enabled in `staticcheck.conf`. |
| `make check` | `fmt-check`, `vet`, `modverify`, `comments`, `test` and `lint`. |
| `make security` | govulncheck v1.8.0 and `secrets`. |
| `make secrets` | gitleaks v8.30.1 over all Git history, the staged changes and a snapshot of tracked and non-ignored files. Refuses to run if `.gitleaks.toml` or `.gitleaksignore` exists at the repository root. |
| `make workflows` | actionlint v1.7.12. |
| `make tools` | Installs the four quality tools. |

CI runs `make check`, `make security` and `make workflows`; run them locally before opening a pull request. Quality tools are installed on first use into `.local/bin/<tool>-<version>/`, which needs network access. The first `make check` also downloads the Go module dependencies; `modernc.org/sqlite` is large and takes several minutes to download and compile the first time.

Code rules:

- Besides the Go standard library, production code uses two third-party modules: `modernc.org/sqlite` v1.59.0 (pure Go SQLite, no CGO) and `github.com/BurntSushi/toml` v1.6.0. Versions are pinned in `go.mod` and `go.sum`. A new dependency needs its reason and license stated in a plan or issue, and only permissive licenses such as MIT, BSD, Apache-2.0 and ISC are accepted.
- Identifiers are in English.
- Every hand-written package, type (including local types) and named function (including methods and tests) has a Chinese comment, checked by `tools/commentcheck`.
- Comments on exported identifiers start with the identifier name, checked by staticcheck.

CI:

- `ci.yml` runs `make check`, `make build` and a `help`/`version` smoke test on ubuntu-24.04 and macos-15, plus `make workflows` on Linux.
- `security.yml` runs `make security` with full Git history on ubuntu-24.04.
- `build.yml` runs only when triggered manually. On macos-15 it runs `make check` and `make security`, then builds darwin arm64 and amd64 binaries with `SHA256SUMS` and uploads them as an Actions artifact kept for 14 days. It does not create a release.
- Actions are pinned to full commit SHAs and use `contents: read` permissions. Dependabot checks `github-actions` and `gomod` weekly.

## Documents

- [Design](docs/zh-CN/design.md) (Chinese): approved scope, architecture, security boundaries and target tree
- [Phase 2 plan](docs/zh-CN/plans/phase-02.md) (Chinese)
- [Phase 3 plan](docs/zh-CN/plans/phase-03.md) (Chinese): configuration, storage and state machines
- [Phase 4 plan](docs/zh-CN/plans/phase-04.md) (Chinese, decision draft): mail loop
- [Phase 0–1 research report](docs/zh-CN/research/phase-01.md) (Chinese)
- [Architecture](docs/en/architecture.md)
- [Research probes](experiments/phase01/README.md) (Chinese)
- [Security policy](SECURITY.md)
- [Code of conduct](CODE_OF_CONDUCT.md)
- [Changelog](CHANGELOG.md)

## Current tree

This is the tree as it exists now. Git-ignored local files such as `.local/`, `dist/` and `coverage.out` are omitted. The planned tree is in the [design](docs/zh-CN/design.md).

```text
turncourier/
├── .github/                         # GitHub configuration
│   ├── ISSUE_TEMPLATE/              # bug_report.yml, feature_request.yml, config.yml
│   ├── pull_request_template.md     # Pull request template
│   ├── dependabot.yml               # Weekly github-actions and gomod updates
│   └── workflows/                   # ci.yml, security.yml, build.yml (manual)
├── cmd/turncourier/                 # Executable
│   ├── main.go                      # Entry point: signal cancellation, exit code
│   └── main_test.go                 # Entry point tests
├── configs/                         # Configuration examples
│   └── turncourier.example.toml     # Loaded by tests to stay in sync with validation
├── internal/                        # Product packages
│   ├── cli/                         # Command-line interface
│   │   ├── cli.go                   # help, version, doctor; planned commands exit 2
│   │   └── cli_test.go              # Output, argument and exit code tests
│   ├── config/                      # TOML configuration, not used by commands yet
│   │   ├── address.go               # Strict email address normalization
│   │   ├── address_test.go          # Table cases and fuzz seeds
│   │   ├── config.go                # Config types, defaults, Load, validation
│   │   ├── config_test.go           # Defaults, rejected keys, env override
│   │   ├── paths.go                 # Config file and data directory paths
│   │   ├── paths_test.go            # Default and environment variable paths
│   │   ├── fileperm_unix.go         # Unix: config file owner and write bits
│   │   ├── fileperm_unix_test.go    # Unix: owner check, FIFO, unreadable parent
│   │   └── fileperm_other.go        # Other platforms: regular file and size
│   ├── doctor/                      # Environment diagnostics
│   │   ├── doctor.go                # Read-only platform and --version checks
│   │   ├── doctor_test.go           # Missing, failing, timeout and cancel tests
│   │   ├── process_unix.go          # Unix: kill the version process group
│   │   ├── process_other.go         # Other platforms: default cancellation
│   │   └── process_unix_test.go     # Process group termination test
│   ├── queue/                       # Reply queue state machine, no I/O
│   │   ├── state.go                 # Queue states, events, transition table
│   │   └── state_test.go            # Every state × event combination
│   ├── store/sqlite/                # SQLite storage, not used by commands yet
│   │   ├── store.go                 # Open, Close, connection parameters
│   │   ├── migrate.go               # Embedded migrations and user_version
│   │   ├── migrations/0001_init.sql # Initial schema
│   │   ├── perm_unix.go             # Unix: data directory and file modes
│   │   ├── perm_other.go            # Other platforms: no mode check
│   │   ├── id.go                    # Task ID generation
│   │   ├── tasks.go                 # Tasks and task events, versioned
│   │   ├── replies.go               # Reply dedup, queue, dispatch, recovery
│   │   ├── store_test.go            # Pragmas, path escaping, file modes
│   │   ├── migrate_test.go          # Migration version, rollback, too new
│   │   ├── id_test.go               # Task ID encoding tests
│   │   ├── tasks_test.go            # Task lifecycle and version conflicts
│   │   ├── replies_test.go          # Deduplication, conflicts, atomicity
│   │   └── dispatch_test.go         # FIFO, uncertain delivery, recovery
│   └── task/                        # Task state machine, no I/O
│       ├── state.go                 # Task states, events, transition table
│       └── state_test.go            # Every state × event combination
├── tests/integration/               # Cross-package tests
│   └── lifecycle_test.go            # Config, storage and state machines together
├── tools/                           # Engineering checkers run by make
│   ├── commentcheck/                # make comments
│   │   ├── main.go                  # Chinese doc comment checker
│   │   └── main_test.go             # Checker tests
│   └── covercheck/                  # make test
│       ├── main.go                  # 80% statement coverage gate
│       └── main_test.go             # Gate and profile parsing tests
├── scripts/                         # Scripts run by make
│   └── scan-secrets.sh              # gitleaks over history, index and worktree
├── docs/                            # Documentation
│   ├── en/architecture.md           # English architecture overview
│   └── zh-CN/                       # Chinese documents
│       ├── design.md                # Approved design and target tree
│       ├── development.md           # Development guide
│       ├── plans/                   # Phase checklists
│       │   ├── phase-02.md          # Phase 2: engineering skeleton
│       │   ├── phase-03.md          # Phase 3: config, storage, state machines
│       │   └── phase-04.md          # Phase 4: mail loop (decision draft)
│       └── research/phase-01.md     # Phase 0–1 findings and limits
├── experiments/phase01/             # Research probes, not product code
│   ├── README.md                    # How to run the probes
│   ├── codex-probe.mjs              # Codex app-server handshake and resume
│   ├── claude-probe.mjs             # Claude Code stream-json and resume
│   └── competitor-probe.mjs         # Offline checks of a pinned upstream project
├── .gitignore                       # Local state, credentials, build output
├── AGENTS.md                        # Project rules for coding agents
├── CHANGELOG.md                     # Change history
├── CLAUDE.md                        # Points Claude Code to AGENTS.md and HANDOFF.md
├── CODE_OF_CONDUCT.md               # Contributor Covenant 2.1
├── CONTRIBUTING.md                  # Contribution guide
├── HANDOFF.md                       # Task checkpoint shared across AI coding tools
├── LICENSE                          # Apache-2.0
├── Makefile                         # build, check, security, workflows
├── README.md                        # English README
├── README.zh-CN.md                  # Chinese README
├── SECURITY.md                      # Vulnerability reporting
├── go.mod                           # Module path, Go 1.27.1, dependencies
├── go.sum                           # Dependency checksums
└── staticcheck.conf                 # Enables ST1020–ST1022
```

## Security and privacy

- Report vulnerabilities through [GitHub private vulnerability reporting](https://github.com/chaoRookie/turncourier/security/advisories/new), not public issues. See [SECURITY.md](SECURITY.md). There are no supported releases; reproduce problems on the latest commit of `main`.
- Do not put QQ Mail authorization codes, tokens, real email content, agent session transcripts or absolute local paths in issues, pull requests or logs. Use synthetic data to reproduce problems.
- The `turncourier` command reads no configuration, credentials or sessions. The configuration file rejects credential keys; the mail authorization code is to be stored in the Keychain by `init`, which is not implemented. Public CI does not use real mailboxes or call models.

## License

[Apache-2.0](LICENSE)
