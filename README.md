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

## Configuration

Credentials are read from the active OMP coding-agent profile's SQLite database
(`ompDBPath()`), with a fallback search for an OpenRouter API key in `~/`. No
config file of its own — the binary follows the same `~/.omp/agent/` layout as
the hosting harness.

## Project structure

```
.
├── main.go       # single-binary Go program (provider fetchers + Bubble Tea UI)
├── go.mod
├── go.sum
└── LICENSE
```

## License

MIT — see [LICENSE](./LICENSE).
