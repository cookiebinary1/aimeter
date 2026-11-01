# aimeter

[![CI](https://github.com/cookiebinary1/aimeter/actions/workflows/ci.yml/badge.svg)](https://github.com/cookiebinary1/aimeter/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/cookiebinary1/aimeter.svg)](https://pkg.go.dev/github.com/cookiebinary1/aimeter)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

TUI dashboard that surfaces API usage and quota gauges for a stack of AI
providers in one terminal view. Single static binary, no cgo, no runtime
dependencies. Alibaba monitoring can optionally use the official `bl` CLI.

![aimeter dashboard](docs/screenshot.png)

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
- Alibaba Token Plan

Refresh interval is 90 seconds.

## Stack

- Go 1.25 (module `github.com/cookiebinary1/aimeter`)
- [Bubble Tea](https://github.com/charmbracelet/bubbletea) — TUI runtime
- [Lip Gloss](https://github.com/charmbracelet/lipgloss) — styling
- [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) — pure-Go SQLite
  reader for the OMP credentials database

## Install

```sh
brew install cookiebinary1/tap/aimeter
# or, with a Go toolchain:
go install github.com/cookiebinary1/aimeter@latest
```

### Shell installer (macOS / Linux)

Install the latest release without Homebrew or a Go toolchain:

```sh
curl -fsSL https://raw.githubusercontent.com/cookiebinary1/aimeter/main/install.sh | sh
```

The installer detects amd64/arm64, downloads the matching GitHub Release,
verifies its SHA-256 against `checksums.txt`, and installs to `~/.local/bin`.
It requires `curl`, `tar`, and either `sha256sum` or `shasum`. It does not use
sudo or modify your shell configuration. Add `~/.local/bin` to your PATH if
needed, then run `aimeter`. Rerun the installer to update.

To inspect the script first, pin a release, or choose another directory:

```sh
curl -fsSL https://raw.githubusercontent.com/cookiebinary1/aimeter/main/install.sh -o install.sh
less install.sh
AIMETER_VERSION=v0.1.1 AIMETER_INSTALL_DIR="$HOME/.local/bin" sh install.sh
```

If the download or verification fails, your existing binary is left in place.
See [install.sh](install.sh) for the implementation.

### Windows

With Go 1.25 or later installed, run in PowerShell or Command Prompt:

```sh
go install github.com/cookiebinary1/aimeter@latest
```

Make sure the Go binary directory (normally `%USERPROFILE%\go\bin`) is on
PATH. Without Go, download the Windows amd64 or arm64 ZIP from
[GitHub Releases](https://github.com/cookiebinary1/aimeter/releases), extract
it, and run `.\aimeter.exe` from that folder in PowerShell. The shell installer
above is for macOS/Linux, not native Windows.

### Build from source

Or build from a checkout:

```sh
git clone https://github.com/cookiebinary1/aimeter
cd aimeter
go build -o ~/.local/bin/aimeter .
# Include local OMP credential discovery:
go build -tags omp -o ~/.local/bin/aimeter .
```

Builds and runs on macOS, Linux and Windows (amd64 and arm64).

After rebuilding, check `command -v aimeter`: a Homebrew binary earlier on PATH
can shadow `~/.local/bin/aimeter`. Updating shell environment variables does not
update an already installed binary.

The TUI is width-aware: every bar in the frame shares one width and one start
column, so the panels line up vertically whatever a single row's caption says.
A wide terminal spends the extra columns on the bars (up to a 220-column
frame); a narrow one drops the grey detail/reset captions and grows the bars to
fill the row instead of wrapping.

<img src="docs/screenshot-narrow.png" alt="aimeter in a narrow terminal" width="380">

### Usage

```sh
aimeter              # TUI dashboard (q quit, r refresh, auto-refresh 90s)
aimeter -once        # render the dashboard once, no TUI
aimeter -plain       # Markdown table — for scripts, logs and agents
aimeter -json        # machine-readable JSON — for programs
aimeter -show-creds  # which source each credential resolved from
aimeter -version     # version string
```

If `aimeter` is run with no flags but **without an interactive terminal**
(piped stdout/stdin, CI, cron, deployment scripts), it auto-detects this and
behaves as `-plain` — the same data, log-friendly. Explicit flags always
override.

### `-plain`: Markdown table

```text
| Provider     | Plan            | Window  | Used | Detail                      | Resets in |
| ------------ | --------------- | ------- | ---- | --------------------------- | --------- |
| Anthropic    |                 | 5h      | 56%  |                             | 46m       |
| Anthropic    |                 | 7d      | 7%   |                             | 6d 7h     |
| OpenAI Codex | prolite         | 7d      | 14%  |                             | 5d 17h    |
| Z.AI         | GLM Coding Lite | 5h      | 82%  | 1659 / 2000 credits         | 2h 42m    |
| Z.AI         | GLM Coding Lite | 7d      | 48%  | 4806 / 10000 credits        | 45h 41m   |
| MiniMax Code | Coding Plan     | 5h      | 21%  |                             | 56m       |
| MiniMax Code | Coding Plan     | 7d      | 3%   |                             | 6d 4h     |
| OpenRouter   | pay-as-you-go   | credits | 12%  | $1.22 / $10.00 · $8.78 left |           |
| ElevenLabs   | Starter         | chars   | 13%  | 11602 / 89240 chars         | 17d 21h   |
| Meshy        | 3D generation   | credits |      | 5500 credits left           |           |
```

The columns are fixed (`Provider`, `Plan`, `Window`, `Used`, `Detail`,
`Resets in`), so the output is stable to parse by column index, renders as a
real table when pasted into an issue, PR or chat, and stays readable as plain
text in a log. Balance-only services leave `Used` empty; a failed provider
puts its message in `Detail`. Pipes inside values are escaped.

### Automation and AI agents

Both machine modes are built for feeding other tools: a coding agent, a cron
job or a deployment step can run aimeter and branch on real numbers instead of
guessing whether a quota is about to run out.

Give an agent the table directly — it needs no parsing instructions, because
the header explains every column:

```sh
aimeter -plain | llm "Which provider is closest to its limit, and how long \
until it resets? Answer in one sentence."
```

Gate work on remaining quota before starting an expensive run:

```sh
# Refuse to start if the 5h Anthropic window is above 90%
used=$(aimeter -json | jq -r '.[] | select(.provider=="Anthropic")
                              | .gauges[] | select(.label=="5h") | .used')
if (( ${used%.*} > 90 )); then
  echo "Anthropic 5h window at ${used}% — deferring the batch job"
  exit 1
fi
```

Fail a healthcheck when any provider errors, e.g. after rotating a key:

```sh
aimeter -json | jq -e '[.[] | select(.error != null)] | length == 0' > /dev/null \
  || { echo "a provider failed to report"; exit 1; }
```

Post the current state to Slack every morning from cron — the Markdown table
renders as-is:

```sh
0 9 * * * aimeter -plain | slack-post '#ops'
```

`-json` gauge fields: `label`, `used` (0-100, or -1 for balance-only),
`detail`, `resets_at` (RFC 3339), `resets_in`, `window`. A failed provider
carries an `error` field instead of gauges.

## Configuration

No credentials live inside the app. Each provider resolves through a chain of
non-invasive, read-only sources — first hit wins:

Built-in providers without credentials are omitted. Configured providers remain
visible when authentication or an API request fails, with the error in their
panel. Custom entries also remain visible on failure.

| Priority | Source | Notes |
| --- | --- | --- |
| 1 | Environment | `ZAI_API_KEY`, `MINIMAX_API_KEY`, `OPENROUTER_API_KEY`, `ELEVENLABS_API_KEY`, `MESHY_API_KEY`, `ALIBABA_API_KEY` |
| 2 | `~/.config/aimeter/credentials.json` | JSON schema below; keep it `chmod 600` |
| 3 | macOS Keychain | `security find-generic-password -s aimeter -a <provider>` |
| 4 | OMP agent.db | only in `-tags omp` builds (personal-machine heuristics) |

OAuth providers rotate their tokens, so they prefer live sources over frozen
copies:

- **Anthropic**: config → Claude Code's own storage (macOS keychain entry, or
  `~/.claude/.credentials.json`) → OMP db (`-tags omp`). A stale OMP copy must
  not replace Claude Code's current login.
- **Codex**: config → `~/.codex/auth.json` (Codex CLI's own file)

Anthropic allows one usage request attempt every 15 minutes on this machine.
HTTP 429 can extend the wait through `Retry-After`. During the wait, aimeter
shows cached usage for up to two hours, marked with its age. Claude Code token
rotation preserves the last successful usage for the same account and
organization, identified through `~/.claude.json`. Without recent data, aimeter
shows the time until the next attempt. The cache lives in the OS user cache
directory; authentication errors remain specific to each token.

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
  "alibaba": {
    "api_key": "...",
    "base_url": "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1"
  },
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

### Alibaba Token Plan

Alibaba monitoring requires a Token Plan subscription, its API key, and an
**authenticated console session**. The model API key alone cannot read plan
usage. Use the official `bl` CLI login below; copying browser cookies is optional.
These instructions describe the current source build.

#### Recommended: official CLI login

1. Install [Bailian / Model Studio CLI](https://www.alibabacloud.com/help/en/model-studio/cli/installation)
   and make sure `bl` is on the PATH used to start aimeter. With Node.js 18.17+
   and npm installed:

   ```sh
   npm install -g bailian-cli
   bl --version
   ```

   The integration has been verified with `bl 2.7.2`.

2. Supply your **Token Plan** API key to aimeter. Add this provider to
   `~/.config/aimeter/credentials.json`, preserving your other entries:

   ```json
   {
     "alibaba": {
       "api_key": "YOUR_TOKEN_PLAN_API_KEY",
       "base_url": "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1"
     }
   }
   ```

   On macOS/Linux, run `chmod 600 ~/.config/aimeter/credentials.json`.
   `ALIBABA_API_KEY` is an alternative to `alibaba.api_key`. Without a resolved
   API key, the Alibaba panel is omitted even if `bl` is logged in.

3. Log in to the console **with the same account that owns your subscription**:

   ```sh
   bl auth login --console --config default --console-site international
   ```

   Complete login in the browser. This is separate from configuring a model API
   key with `bl auth login --api-key`. Aimeter uses the CLI's `default` profile,
   regardless of which profile is active in `bl`.

4. Verify quota access, then start aimeter:

   ```sh
   bl usage token-plan --config default --console-site international --console-region ap-southeast-1 --output json
   aimeter
   ```

   The quota command should return usage fields and `hasQuotaData: true`.
   Aimeter shows `Token Plan · bl` and the reported quota windows. A monthly
   ratio such as `0.0304` means approximately 3% usage.

For **Beijing**, set `alibaba.base_url` (or `ALIBABA_BASE_URL`) to
`https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1`.
Use `--console-site domestic` for login and `--console-site domestic
--console-region cn-beijing` for the quota check. An omitted base URL defaults
to Singapore. `ALIBABA_BASE_URL` overrides the configured URL.

In `-tags omp` builds, aimeter can also read the key (`access`) and endpoint
(`enterpriseUrl`) from OMP's `alibaba-coding-plan` record. Explicit environment
and aimeter configuration values take precedence. An inference-only OMP record
still needs a separate console login.

If `bl` is missing or its session expires, the Alibaba panel shows login
guidance. Check `bl --version`, repeat console login, and rerun the quota check
above. If usage is missing or belongs to another subscription, check the console
account, `default` profile, and region. No model inference call is needed to
verify quota access.

#### Alternative: explicit console Cookie

If you do not want to use `bl`, sign in to
[QwenCloud Token Plan](https://home.qwencloud.com/billing/subscription/token-plan-individual)
or the [Beijing console](https://bailian.console.aliyun.com/cn-beijing?tab=plan).
In browser developer tools, open Network, reload the page, select an authenticated
console request, and copy its **request Cookie header** into `alibaba.cookie` in
`credentials.json`, or set `ALIBABA_COOKIE`. Treat it as a password and renew it
when the session expires.

An explicit Cookie takes precedence over `bl`; `ALIBABA_COOKIE` takes precedence
over `alibaba.cookie`. Remove an expired Cookie to use the CLI login instead.
Aimeter does not copy the CLI's stored credentials. The Cookie path sends only
the console session to the regional console gateway and does not follow
redirects. Neither path uses the inference API key to query quotas.

The dashboard shows the reported `5h`, `7d`, and/or `monthly` windows. It does
not estimate spend, balances, or a monthly elapsed-time bar.

### Service costs

Panel headers show costs when there is enough room. Recognized Codex Plus
uses `~$20/mo list` (USD list price, verified 2026-10-07, not your invoice).
OpenRouter shows `usage-based`. Other plans show `cost unknown` because usage
APIs do not reliably identify their subscription price.

Set your actual payments in a top-level `costs` object in `credentials.json`:

```json
{
  "costs": {
    "anthropic": "€100/mo",
    "codex": "€23/mo",
    "zai": "$3/mo annual",
    "minimax": "$10/mo",
    "elevenlabs": "$5/mo",
    "meshy": "free"
  }
}
```

These are examples, not inferred payments. Supported keys are the provider
IDs above, `openrouter`, and `custom:<name>`. An empty string hides a cost.
Overrides take priority over list prices. JSON includes an additive `cost`
field; plain output includes the cost in the existing Plan column.
Credentials and costs are reread on every TUI refresh so OAuth token rotation
by Claude Code or Codex does not require restarting aimeter. Explicit token
copies in credentials.json still take priority and must be kept current.

### Expired Anthropic sessions

In the interactive dashboard, an explicit expired-token response from Anthropic
opens a confirmation prompt. Press `y` to renew the token, or `n`, Enter or Esc
to decline. Declining suppresses the prompt for that token for the current run.
Close Claude Code before confirming: renewal changes its credentials and a
concurrent renewal may invalidate its login. Aimeter re-reads credentials before
the request and before saving, but cannot eliminate races with Claude Code.

Renewal is supported for Claude Code's macOS Keychain entry and credentials file.
It preserves unrelated metadata and saves both rotated tokens and expiry.
Copied tokens in aimeter's configuration and OMP credentials are not renewed.
Non-interactive modes (`-once`, JSON and plain output) never renew tokens.
A failed renewal requires opening Claude Code and logging in again.

### Anthropic request frequency

Anthropic usage requests are limited to one attempt every 15 minutes on this
machine. The next allowed request time is saved in the OS cache directory, so
restarting aimeter, pressing `r`, running multiple instances or rotating an OAuth
token does not bypass the cooldown. Failed requests also start the cooldown.
An HTTP 429 extends it when `Retry-After` specifies a longer wait.

During the wait, aimeter displays cached usage with its age. Data older than two
hours is hidden. If no cached data exists, it displays the remaining wait.
This applies to interactive, one-shot, plain and JSON modes. Other providers keep
their existing refresh frequency. If the cooldown cannot be saved, aimeter skips
the Anthropic request rather than making an unthrottled request.

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

## Security

By default aimeter reads credentials and makes read-only requests over HTTPS,
using each credential only with its provider. Alibaba quota monitoring uses a
console session via the official `bl` CLI or an explicit Cookie, rather than
the inference API key. The interactive Anthropic renewal
prompt is the only flow that changes credentials: after explicit confirmation,
it sends the refresh token to Anthropic's OAuth endpoint and updates Claude
Code's existing credential storage. Secrets are never printed or passed as
subprocess command-line arguments.
It writes Anthropic usage, request cooldowns and retry times to a local cache,
but never the token.
Aimeter itself has no telemetry. The optional external `bl` CLI follows its
own configuration and network behavior. `-show-creds` deliberately prints
sources without values. `credentials.json` should be `chmod 600`; aimeter warns when it is
group- or world-readable.

## Project structure

```
.
├── main.go                 # provider fetchers + Bubble Tea UI
├── alibaba.go              # Token Plan CLI/console-session quota fetcher
├── credentials.go          # credential chain (env → config → keychain)
├── credentials_omp.go      # OMP agent.db fallbacks (build tag: omp)
├── credentials_default.go  # no-op stub for default builds
├── custom.go               # user-defined providers (JSON pointer extraction)
├── output.go               # -plain and -json renderers
├── install.sh              # checksum-verified macOS/Linux release installer
├── tests/test_install.py   # offline installer regression tests
├── *_test.go               # resolver, custom, output and layout tests
├── go.mod
├── go.sum
└── LICENSE
```

## Contributing

Pull requests welcome — see [CONTRIBUTING.md](./CONTRIBUTING.md) for the
build/test commands CI enforces and how to add a provider. Many services need
no code at all: describe them as a [custom provider](#custom-providers).

## License

MIT — see [LICENSE](./LICENSE).
