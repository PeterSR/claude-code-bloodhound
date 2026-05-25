# Bloodhound

**Track your Claude Code subscription usage over time. Make educated decisions about how to use it today.**

Claude Code's subscription burns through opaque rate limits — a 5-hour session window and a weekly window, both expressed only as percentages in the `/usage` TUI panel. There's no API. No history. No way to know whether your next prompt will sail through or push you into a hard stop.

Bloodhound watches the JSONL session logs Claude Code already writes to your disk, polls `/usage` on a schedule you control, and stores it all locally so you can answer questions like:

- When am I projected to hit my 5-hour or weekly limit?
- How much can I realistically do before the bucket runs out?
- What does my next prompt likely cost? (calibration: tokens per 1%)
- Should I `/compact` now? Was my last compaction warm or cold?
- Where am I leaking quota — idle gaps, cold compactions, or breakpoint rotations?
- Which sessions are eating the most of my week?

## Status

**v0.1 alpha — usable, but expect rough edges.** The data model and the daily-driver UI are in place:

- `Now` — gauges for session and week, time to natural reset, burn-rate projection (only when actionable), and an "On extra usage" badge once you blow past 100%.
- `History` — `/usage` % over time, the calibration trend (tokens per 1%), and a weekday × hour heatmap of when you spend.
- `Sessions` — every session on disk, sortable, click-through to a per-turn view with classification (idle / rotation / restructure) and inline compactions.
- `Compactions` — every confirmed `/compact`, broken down by warm vs cold cache state.
- `Leaks` — avoidable spend ranked: idle misses, rotations, restructures, cold compactions, plus the worst-offending sessions.
- `Settings` — edit `config.json` from the UI, copy-paste statusline + hook + systemd snippets.
- `Debug` — last `/usage` raw dump, parser output, ingest counters, schema version, paths, and a Retrain button that lets the orchestrator self-heal the extractor on demand.

Per-OS installers and the opt-in **Join the pack** community-insights upload are next — see [Privacy](#privacy) for what that's for and what it would and wouldn't share.

## How it works

Three jobs, scheduled either by your OS or by a built-in scheduler:

- `bloodhound poll` — drives the Claude Code TUI in a pty, parses the `/usage` panel, persists the observation.
- `bloodhound ingest` — walks `~/.claude/projects/*.jsonl`, parses every assistant turn and compaction event into local SQLite.
- `bloodhound aggregate` — recomputes rolling metrics (sessions, 5-hour buckets, tokens-per-1% calibration) on a slower cadence.

`bloodhound daemon` runs all three on configurable intervals (defaults: poll 5m, ingest 5m, aggregate 15m) and exposes a JSON API over a unix socket at `$XDG_RUNTIME_DIR/bloodhound/api.sock` — no TCP port to conflict with anything on your box.

`bloodhound-gui` is a native window that loads the dashboard and proxies its API calls to the daemon's socket. Pre-built for Linux x86_64; macOS / Windows / Linux-arm builds work in principle but aren't shipped yet (see [Platforms](#platforms)). It's a separate binary so the daemon can run headless on machines without a desktop environment.

A `bloodhound status` subcommand renders one line suitable for Claude Code's statusline so you can keep an eye on your quota without leaving the terminal.

## Currency

Your currency is the percentage shown in `/usage`. Bloodhound never asks you to guess your plan's token cap or pick a billing-weight mode — those are implementation details we figure out from your data. The calibration phase converts the opaque percentage into a tokens-per-1% estimate by pairing adjacent observations and dividing the cost-weighted spend in between by the percentage points the bucket moved. Saturated observations (≥99%, when you're on the pay-per-use Extra usage tier and the bucket has stopped moving) are excluded.

## Self-healing extractor

The `/usage` panel is a TUI, and Anthropic ships layout changes without warning. Bloodhound parses it with a regex DSL stored at `$XDG_STATE_HOME/bloodhound/extractors.json`. When the bundled defaults stop matching, the daemon auto-heals: it spawns a fresh `claude -p` as an orchestrator and hands it four MCP tools (`read_pty`, `send_keys`, `test_regex`, `save_extractor`) to drive a live pty session, re-learn the field positions, and persist a working extractor. Gated by `extractor_self_heal` in the config (default on). You can also trigger one on demand via the Retrain button on the Debug page or `POST /api/extractor/retrain`. The version number in the file is checked at load time; older files fall back gracefully to the bundled defaults until the next heal.

## Privacy

Bloodhound is local-first. Your data stays on your machine. There is no server today, and the daemon makes no outbound network calls beyond driving the local `claude` binary.

The "Join the pack" feature is the planned opt-in to that. Why have one at all: most of the interesting questions about Claude Code's subscription only get sharp answers when you can compare across installs. Examples:

- **Are tokens-per-1% the same for everyone?** If your calibration drifts from the pack median over weeks, Anthropic may be quietly differentiating cost weights by plan, account age, region, or load.
- **Does the 5-hour window really mean 5 hours?** Reset cadence and saturation rates pooled across many accounts make it visible if some users get longer-effective windows.
- **Where are the cliffs?** When the bundled extractor breaks because Anthropic redesigned the `/usage` panel, the pack notices in minutes instead of one user at a time.
- **Plan-tier comparisons.** Pro vs Max-5x vs Max-20x — what does each dollar actually buy in throughput once you account for cache hits and saturation?

Joining the pack would upload:

- **Anonymized observation counts:** `/usage` percentages, reset timestamps, saturation flags, time-of-day buckets.
- **Anonymized calibration deltas:** tokens-per-1% values without the underlying token totals or prompts they came from.
- **Plan tier** (if you set it in Settings) and an optional self-declared **user_id** so a single human across multiple machines doesn't double-count.
- **A random per-install `device_id`** so the same machine doesn't double-count on reconnect.
- **Build version + extractor version** so we know which clients are reporting.

It would not upload:

- Your conversations, prompts, completions, or any model output.
- File paths, project names, repository names, session UUIDs, or hostnames.
- Tool calls, environment variables, or anything outside Bloodhound's own observation database.
- IP-level identifiers beyond what the transport requires to deliver the packet.

This is all moot until the upload server exists. When it does, joining will be an explicit toggle in Settings, off by default, and you'll be able to see the exact JSON payload before it leaves your machine.

## Install

### Platforms

The daemon is pure-Go and ships pre-built for Linux, macOS, and Windows × amd64 / arm64. The native GUI is a Wails app, and right now it only ships pre-built for Linux x86_64. Daemon-only setups give you the full API — the GUI is just a viewer on top.

|                       | Daemon (pre-built) | GUI (pre-built) | Notes                                          |
| --------------------- | ------------------ | --------------- | ---------------------------------------------- |
| Linux x86_64          | ✅                  | ✅               | Primary development target.                    |
| Linux arm64           | ✅                  | build from source | Daemon tested in CI; GUI build unverified.    |
| macOS arm64 / amd64   | ✅                  | build from source | Daemon tested in CI; GUI build unverified.    |
| Windows amd64         | ✅                  | build from source | Daemon tested in CI; GUI build unverified.    |

**Help me test Bloodhound on your platform.** I develop on Linux x86_64, so that's the only combo I exercise end-to-end. If you try Bloodhound on macOS, Windows, or Linux arm64 — daemon-only or full GUI — open [an issue](https://github.com/PeterSR/claude-code-bloodhound/issues) with your `bloodhound doctor` output and what worked or broke. Platform fixes get fast-tracked, and credited.

First-launch friction to expect on unsigned binaries:

- **macOS Gatekeeper** refuses to run downloaded executables. Clear the quarantine bit: `xattr -d com.apple.quarantine bloodhound bloodhound-gui`. Or right-click the binary in Finder → Open → Open for a one-time bypass.
- **Windows SmartScreen** shows *Windows protected your PC* on first run. Click **More info → Run anyway**.

### Pre-built binary

Grab the right tarball or zip from the [Releases page](https://github.com/PeterSR/claude-code-bloodhound/releases), unpack, and drop `bloodhound` somewhere on your `$PATH`.

```bash
bloodhound doctor               # sanity-check paths, schema, claude binary
bloodhound install --enable     # write + start the systemd user unit (Linux)
```

On Linux, if you also unpacked the GUI tarball and ran its `install-gui.sh`, launch `bloodhound-gui` from your application menu. Otherwise — or on macOS / Windows where there is no GUI binary yet — the dashboard's API is reachable directly over the unix socket:

```bash
curl --unix-socket $XDG_RUNTIME_DIR/bloodhound/api.sock http://bh/api/now | jq
```

### From source

Requires Go 1.25+ and Node 20+.

```bash
git clone https://github.com/PeterSR/claude-code-bloodhound
cd claude-code-bloodhound
make install       # daemon → $GOPATH/bin/bloodhound (works on every platform)
bloodhound install --enable
```

Building the GUI from source needs platform-specific prerequisites:

- **Linux:** GTK 3 and WebKit2GTK 4.1 dev headers. Fedora: `sudo dnf install gtk3-devel webkit2gtk4.1-devel`. Ubuntu 24.04+ / Debian trixie: `sudo apt-get install libgtk-3-dev libwebkit2gtk-4.1-dev`. Then `make install-gui` drops `bloodhound-gui` into `~/.local/bin` and installs the `.desktop` entry.
- **macOS:** Xcode Command Line Tools (`xcode-select --install`). The Makefile target is Linux-shaped, so build the binary directly: `cd web && npm ci && npm run build && cd .. && go build -tags prod,production,desktop -o bloodhound-gui ./cmd/bloodhound-gui`. If you get it working — or it explodes — please report back.
- **Windows:** WebView2 runtime (pre-installed on Windows 10/11). PowerShell: `cd web; npm ci; npm run build; cd ..; go build -tags prod,production,desktop -o bloodhound-gui.exe ./cmd/bloodhound-gui`. Same "let me know how it went" applies.

For first-time setup the daemon will collect a few `/usage` polls before the gauges show anything useful; calibration needs at least two non-saturated observations in the same bucket. If the GUI launches before the daemon is running, it shows a setup wizard with the commands above.

## Paths

Bloodhound follows XDG conventions on Linux and the platform conventions on macOS / Windows.

| Kind | Linux | macOS | Windows |
|---|---|---|---|
| Database | `$XDG_DATA_HOME/bloodhound/bloodhound.db` | `~/Library/Application Support/bloodhound/` | `%LOCALAPPDATA%\bloodhound\` |
| Config | `$XDG_CONFIG_HOME/bloodhound/config.json` | `~/Library/Application Support/bloodhound/` | `%LOCALAPPDATA%\bloodhound\` |
| State (extractors, daemon log) | `$XDG_STATE_HOME/bloodhound/` | `~/Library/Application Support/bloodhound/` | `%LOCALAPPDATA%\bloodhound\` |

## Disclaimer

This is a tool for monitoring Claude Code, and — fittingly — Claude Code has been used to build it. The Go and TypeScript here are vetted by a human, but parts of the implementation, scaffolding, and copy were drafted with AI assistance.

It's also **alpha software**. Expect rough edges: things may misclassify, the schema may change, your local DB may need to be wiped between releases, and the bundled `/usage` extractor will break the next time Anthropic redesigns the panel. The daemon's self-heal usually catches that on the first failed poll; if it can't, the Debug page has a manual Retrain button. It does not modify your Claude Code installation, your JSONL session files, or anything outside its own state directory — but treat the numbers as informational, not as a substitute for Anthropic's own billing.

Bloodhound is not affiliated with or endorsed by Anthropic.

## License

MIT.
