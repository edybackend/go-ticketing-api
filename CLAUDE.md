# Engineering rules for AI agents

This file is loaded by Claude Code in every session. It defines how changes are made in this repository.
Hooks in `.claude/settings.json` enforce the parts that can be automated.

## Workflow
1. **Spec first.** A new behavior starts as a spec in `specs/NNN-name.md` (context, behavior, acceptance criteria, out of scope).
2. **Small steps.** One concern per commit, [Conventional Commits](https://www.conventionalcommits.org/) (`feat(booking): ...`, `fix(stream): ...`).
3. **Definition of done** = the CI gates pass locally: `gofmt -l .` is empty, `go vet ./...`, `go test -race ./...`, `golangci-lint run`. The Stop hook blocks finishing while they are red.

## Layout
- `cmd/tickets` wires the service, the sweeper and graceful shutdown. No logic.
- `internal/booking` owns seats, holds and orders; it is the only package that talks to SQLite.
- `internal/stream` is the in-process pub/sub of seat changes; `internal/httpapi` maps HTTP and Server-Sent Events.
- `web/index.html` is the seat map: plain HTML and JavaScript, embedded with `go:embed`, no build step.

## Box office rules
- **Taking a seat is a compare-and-set.** One `UPDATE … WHERE status = 'available' OR (held and expired)` inside the transaction, and the rows affected must equal the seats asked for; otherwise roll back. Never read-then-write. Hook-enforced.
- **Holds are all or nothing** and **expire on their own**: an expired hold stops blocking immediately (reads treat it as available); the sweeper only tidies up and announces it.
- **Purchases are idempotent** (`orders.hold_id` is unique): repeating one returns the same order.
- **Publishing a change never blocks.** Subscribers have a small buffer; one that falls behind is dropped and reconnects.
- **Every goroutine can be stopped**: pass `ctx`, wait with `select`, never `time.Sleep` or `context.Background()` in `internal/` (hook). HTTP requests inherit the shutdown context so open streams end.

## Go conventions
- Errors are returned and wrapped with `%w`; sentinel errors map to HTTP status codes in one place (`respond`).
- Table-driven tests; `go test -race` must pass. `cmd/` + `internal/`, no `pkg/`.

## Never
- Edit `data/`, the SQLite files or `go.sum` by hand. Blocked by hook.
- Read or write secrets (`.env`, keys). Blocked by permissions and hook.
- Disable a linter or a test to make the build pass.
- Add a dependency without a reason stated in the commit message.
