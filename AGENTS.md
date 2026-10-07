# AGENTS.md

Go 1.26+ proxy server providing OpenAI/Gemini/Claude/Codex compatible APIs with OAuth and round-robin load balancing.

## Repository
**discorev/CLIProxyAPI** (`origin`), a fork of router-for-me/CLIProxyAPI. Releases are tagged `vX.Y.Z-fork.N`. The dashboard fork is discorev/Cli-Proxy-API-Management-Center.

### Upstream
- `upstream`: router-for-me/CLIProxyAPI. Never push or open PRs there unless asked.
- Merge `upstream/main` into a branch off `main` and PR it into `origin/main`. Never rebase or force-push `main`.
- Conflicts: keep the divergences in **Fork notes** and take upstream elsewhere. Flag any new `router-for-me` URLs.
- Upstream's contributor-management workflows were deleted because they don't fit a single-maintainer fork. Keep them deleted on modify/delete conflicts:
  - `agents-md-guard` auto-closes PRs that touch `AGENTS.md`.
  - `auto-retarget-main-pr-to-dev` moves PRs to upstream's `dev` branch; the fork works on `main`.
  - `pr-path-guard` blocks `internal/translator/**` changes.
- Release the proxy before any dashboard release that depends on new proxy APIs.

## Fork notes
The fork exists to:
- make new Claude and Codex models usable as soon as they ship (see Model catalogs);
- let new Claude Code features work without a gateway change each time (see Claude native passthrough);
- make better use of multiple subscriptions (see `intelligent-fill`, the usage cache and reset auto-apply).

- **Distribution:** the fork publishes its own images, `ghcr.io/discorev/cli-proxy-api`, because upstream's images lack its changes. Containers run in UTC, and CI builds images only, with no binary releases.
- **Management panel:** defaults to the dashboard fork, and an explicit upstream panel repo is redirected to it. The fork's panel uses fork-only APIs, and the upstream panel lacks them.
- **Model catalogs:** fetched from `discorev/ai-models`, so new Claude and Codex models can be added as soon as they ship. Twice in a row, upstream's model file lagged new releases and the models were unusable through the router. That lag is why the fork was started.
- **Claude native passthrough:** genuine Claude Code / Agent SDK requests are forwarded unchanged except for auth. Server-side features such as the auto-mode classifier need the client's betas, `safeguards` field and real version to reach Anthropic intact.
- **`intelligent-fill` routing (the default strategy):** sends traffic to the subscription whose weekly window resets soonest, skipping subs with an exhausted window, so each sub's allowance is used before it resets. It currently runs as a custom Selector on the legacy `Pick` path, not in the scheduler fast path. This was a shortcut to keep the fork's diff out of upstream's scheduler code. It is known debt to revisit with proper scheduler integration, not a design to preserve.
- **Usage cache:** a single in-memory view per credential, fed by the upstream usage endpoints and response rate-limit headers. Both routing and the dashboard read it, so the endpoints are not polled twice.
- **Usage fetch rate limits are deliberate:** a 3-minute per-credential floor and 429 cooldown ladders (Claude 20→40→60 min). Claude's usage endpoint 429s without `Retry-After`, the lockout can last about an hour, and polling during it extends it. Do not shorten them.
- **Reset auto-apply / dry-run** (`reset-credits.*`, default off): spends banked resets before they'd be wasted. The rules differ per provider: a Codex reset restarts the weekly window, while a Claude grant clears usage without moving it. Claude grants normally wait until every Claude account is exhausted and more than an hour remains until natural recovery. To avoid wasting an expiring grant, it is spent earlier if its subscription is exhausted and it expires before recovery, or in its last 15 minutes if it does not require being at the limit. Dry-run logs decisions without sending anything. Tests must never hit real reset endpoints.
- **Codex HTTP clients over the upstream websocket** (`upstream.codex.http-websocket-pool`, default on; `helps/codex_ws_*.go`): Codex OAuth credentials default to `websockets: true`, and plain-HTTP requests that reach the Codex executor (any source format) are sent over a pool of upstream Responses websockets keyed by (credential, conversation). When a turn's translated input exactly extends the previous turn's input plus output on that socket, only the new items are sent with `previous_response_id`; this cuts request-side bandwidth and time to first token on long conversations. Anything uncertain is sent in full. Recoverable failures (lost previous response, closed socket, the 60-minute connection cap) are retried in full, or over HTTP, before any output is committed. The pool follows the credential the router picked and never influences selection.
- **Log allowlist:** the production log formatter silently drops fields that aren't allowlisted, so new structured log fields must be added to it.

## Commands
```bash
gofmt -w . # Format (required after Go changes)
go build -o cli-proxy-api ./cmd/server # Build
go run ./cmd/server # Run dev server
go test ./... # Run all tests
go test -v -run TestName ./path/to/pkg # Run single test
go build -o test-output ./cmd/server && rm test-output # Verify compile (REQUIRED after changes)
```
- Common flags: `--config <path>`, `--tui`, `--standalone`, `--local-model`, `--no-browser`, `--oauth-callback-port <port>`

## Config
- Default config: `config.yaml` (template: `config.example.yaml`)
- `.env` is auto-loaded from the working directory
- Auth material defaults under `auths/`
- Storage backends: file-based default; optional Postgres/git/object store (`PGSTORE_*`, `GITSTORE_*`, `OBJECTSTORE_*`)

## Architecture
- `cmd/server/` — Server entrypoint
- `internal/api/` — Gin HTTP API (routes, middleware, modules)
- `internal/api/modules/amp/` — Amp integration (Amp-style routes + reverse proxy)
- `internal/thinking/` — Main thinking/reasoning pipeline. `ApplyThinking()` (apply.go) parses suffixes (`suffix.go`, suffix overrides body), normalizes config to canonical `ThinkingConfig` (`types.go`), normalizes and validates centrally (`validate.go`/`convert.go`), then applies provider-specific output via `ProviderApplier`. Do not break this "canonical representation → per-provider translation" architecture.
- `internal/runtime/executor/` — Per-provider runtime executors (incl. Codex WebSocket)
- `internal/translator/` — Provider protocol translators (and shared `common`)
- `internal/registry/` — Model registry + remote updater (`StartModelsUpdater`); `--local-model` disables remote updates
- `internal/store/` — Storage implementations and secret resolution
- `internal/managementasset/` — Config snapshots and management assets
- `internal/cache/` — Request signature caching
- `internal/watcher/` — Config hot-reload and watchers
- `internal/wsrelay/` — WebSocket relay sessions
- `internal/usage/` — Usage and token accounting
- `internal/home/` — CLIProxyAPIHome control plane integration (bootstrap, RESP communication, dispatch coordination)
- `internal/tui/` — Bubbletea terminal UI (`--tui`, `--standalone`)
- `sdk/cliproxy/` — Embeddable SDK entry (service/builder/watchers/pipeline)
- `test/` — Cross-module integration tests

## Code Conventions
- Keep changes small and simple (KISS)
- Comments in English only
- If editing code that already contains non-English comments, translate them to English (don’t add new non-English comments)
- For user-visible strings, keep the existing language used in that file/area
- New Markdown docs should be in English unless the file is explicitly language-specific (e.g. `README_CN.md`)
- As a rule, do not make standalone changes to `internal/translator/`. You may modify it only as part of broader changes elsewhere.
- If a task requires changing only `internal/translator/`, run `gh repo view --json viewerPermission -q .viewerPermission` to confirm you have `WRITE`, `MAINTAIN`, or `ADMIN`. If you do, you may proceed; otherwise, file a GitHub issue including the goal, rationale, and the intended implementation code, then stop further work.
- `internal/runtime/executor/` should contain executors and their unit tests only. Place any helper/supporting files under `internal/runtime/executor/helps/`.
- Follow `gofmt`; keep imports goimports-style; wrap errors with context where helpful
- Do not use `log.Fatal`/`log.Fatalf` (terminates the process); prefer returning errors and logging via logrus
- Shadowed variables: use method suffix (`errStart := server.Start()`)
- Wrap defer errors: `defer func() { if err := f.Close(); err != nil { log.Errorf(...) } }()`
- Use logrus structured logging; avoid leaking secrets/tokens in logs
- Avoid panics in HTTP handlers; prefer logged errors and meaningful HTTP status codes
- Timeouts are allowed only during credential acquisition; after an upstream connection is established, do not set timeouts for any subsequent network behavior. Intentional exceptions that must remain allowed are the Codex websocket liveness deadlines in `internal/runtime/executor/codex_websockets_executor.go`, the wsrelay session deadlines in `internal/wsrelay/session.go`, the management APICall timeout in `internal/api/handlers/management/api_tools.go`, and the `cmd/fetch_antigravity_models` utility timeouts
- Avoid wall-clock `time.Sleep` in TTL, expiration, ordering, or cache-eviction unit tests due to platform timer granularity (e.g. Windows default timer resolution of ~15.6ms) and CI jitter under load; prefer controllable clocks (`nowFunc` / mock clock), explicit timestamp manipulation, or deterministic synchronization primitives.
- Note: if modifying features that involve CLIProxyAPIHome, check if corresponding updates are needed in the CLIProxyAPIHome repository.
- Endpoints under the `/v0/management` base URL are deprecated and no longer maintained. For any feature changes, do not modify endpoints under `/v0/management` unless necessary to fix compilation errors.
