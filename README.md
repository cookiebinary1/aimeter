# aimeter

TUI dashboard that surfaces API usage and quota gauges for a stack of AI
providers in one terminal view.

## What it shows

Per-provider panels with current utilization, quota windows, and time-until-reset
for:

- Anthropic
- Codex
- Z.ai
- MiniMax
- OpenRouter
- ElevenLabs
- Meshy

Refresh interval is 90 seconds.

## Stack

- Go 1.25 (module `aimeter`)
- [Bubble Tea](https://github.com/charmbracelet/bubbletea) — TUI runtime
- [Lip Gloss](https://github.com/charmbracelet/lipgloss) — styling
- [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) — pure-Go SQLite
  reader for the OMP credentials database

## Build & run

```sh
go build -o ~/.local/bin/aimeter .
~/.local/bin/aimeter
```

Or directly:

```sh
go run .
```

### Usage

```sh
aimeter              # TUI dashboard (q quit, r refresh, auto-refresh 90s)
aimeter -once        # render the dashboard once, no TUI
aimeter -plain       # one " | "-separated line per gauge — for bash scripts
aimeter -json        # machine-readable JSON — for scripts and AI agents
aimeter -show-creds  # which source each credential resolved from
```

`-plain` example line:

```text
Z.AI | GLM Coding Lite | 5h | 60% | 1207 / 2000 credits | resets in 3h 53m
```

`-json` gauge fields: `label`, `used` (0-100, or -1 for balance-only),
`detail`, `resets_at` (RFC 3339), `resets_in`, `window`. A failed provider
carries an `error` field instead of gauges.

## Configuration

No credentials live inside the app. Each provider resolves through a chain of
non-invasive, read-only sources — first hit wins:

| Priority | Source | Notes |
| --- | --- | --- |
| 1 | Environment | `ZAI_API_KEY`, `MINIMAX_API_KEY`, `OPENROUTER_API_KEY`, `ELEVENLABS_API_KEY`, `MESHY_API_KEY` |
| 2 | `~/.config/aimeter/credentials.json` | JSON schema below; keep it `chmod 600` |
| 3 | macOS Keychain | `security find-generic-password -s aimeter -a <provider>` |
| 4 | OMP agent.db | only in `-tags omp` builds (personal-machine heuristics) |

OAuth providers rotate their tokens, so they prefer live sources over frozen
copies:

- **Anthropic**: config → OMP db (`-tags omp`) → Claude Code's own storage
  (macOS keychain entry, or `~/.claude/.credentials.json`)
- **Codex**: config → `~/.codex/auth.json` (Codex CLI's own file)

`aimeter -show-creds` prints which source each provider resolved from — never
the key values themselves.

### credentials.json

```json
{
  "zai":        { "api_key": "..." },
  "minimax":    { "api_key": "..." },
  "openrouter": { "api_key": "..." },
  "elevenlabs": { "api_key": "..." },
  "meshy":      { "api_key": "..." },
  "anthropic":  { "access_token": "...", "email": "..." },
  "codex":      { "access_token": "...", "account_id": "..." },
  "custom": [
    {
      "name": "MyAI",
      "url": "https://api.myai.example/usage",
      "api_key": "sk-...",
      "label": "monthly",
      "used": "/data/used",
      "total": "/data/quota",
      "detail": "/data/plan"
    }
  ]
}
```

Every field is optional — provide only the providers you use. Group/world
readable permissions produce a warning at startup.

### Custom providers

For services aimeter does not know, add a `custom` entry: aimeter sends one
`GET` with an `Authorization: Bearer <api_key>` header and extracts values by
[RFC 6901 JSON pointer](https://datatracker.ietf.org/doc/html/rfc6901):

- `percent` — pointer to a 0-100 number, **or**
- `used` + `total` — pointers whose ratio becomes the percentage
- `label` (gauge caption, default `usage`), `detail` (extra text line) — optional

```json
{ "name": "MyAI", "url": "https://api.myai.example/usage", "api_key": "sk-...",
  "percent": "/data/usage_percent", "detail": "/data/plan_name" }
```

The gauge renders even without a reset window; entries missing `name` or `url`
are skipped with a warning.

## Project structure

```
.
├── main.go                # provider fetchers + Bubble Tea UI
├── credentials.go         # credential resolution chain (env → config → keychain)
├── credentials_omp.go     # OMP agent.db fallbacks (build tag: omp)
├── custom.go               # user-defined providers (JSON pointer extraction)
├── custom_test.go          # pointer parsing + fetch tests
├── credentials_default.go # no-op stub for default builds
├── credentials_test.go    # resolver chain tests
├── go.mod
├── go.sum
└── LICENSE
```

## License

MIT — see [LICENSE](./LICENSE).
