# AGENTS.md

Go 1.26+ proxy providing OpenAI/Gemini/Claude/Codex compatible APIs with OAuth and round-robin load balancing. GitHub: https://github.com/router-for-me/CLIProxyAPI. Those provider names are product facts.

Finish authorized work. User instructions outrank skills; if a skill would pause work, name and quote it. Do not add tests unless asked. After Go changes run `gofmt -w .` and `go build -o test-output ./cmd/server && rm test-output`. Run only the tests the change needs. Stop only when required authority is missing or a required gate is unsatisfied.

```bash
gofmt -w .
go build -o cli-proxy-api ./cmd/server
go run ./cmd/server
go test -v -run TestName ./path/to/pkg
```

Flags: `--config <path>`, `--tui`, `--standalone`, `--local-model`, `--no-browser`, `--oauth-callback-port <port>`. Default config `config.yaml` (`config.example.yaml`). `.env` loads from cwd. Auth under `auths/`. Optional stores: `PGSTORE_*`, `GITSTORE_*`, `OBJECTSTORE_*`.

Keep `internal/thinking/` as canonical `ThinkingConfig` then per-provider `ProviderApplier`. Do not make standalone `internal/translator/` edits; translator-only work requires `gh repo view --json viewerPermission -q .viewerPermission` of `WRITE`, `MAINTAIN`, or `ADMIN`, else file an issue with goal, rationale, and intended code and stop. Executors and their tests live in `internal/runtime/executor/`; helpers go in `internal/runtime/executor/helps/`.

Comments in English (translate existing non-English comments you touch). Keep user-visible string language. New Markdown is English unless the file is language-specific (`README_CN.md`). No `log.Fatal`/`log.Fatalf`. Wrap errors; logrus structured logs with no secrets. No panics in HTTP handlers. Shadowed vars use a method suffix (`errStart := server.Start()`). Wrap defer close errors. Timeouts only during credential acquisition, except Codex websocket liveness in `internal/runtime/executor/codex_websockets_executor.go`, wsrelay deadlines in `internal/wsrelay/session.go`, management APICall timeout in `internal/api/handlers/management/api_tools.go`, and `cmd/fetch_antigravity_models`.
