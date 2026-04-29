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

Pre-1.0, actively developed. The data model and the daily-driver UI are in place:

- `Now` — gauges for session and week, time to natural reset, burn-rate projection (only when actionable), and an "On extra usage" badge once you blow past 100%.
- `History` — `/usage` % over time, the calibration trend (tokens per 1%), and a weekday × hour heatmap of when you spend.
- `Sessions` — every session on disk, sortable, click-through to a per-turn view with classification (idle / rotation / restructure) and inline compactions.
- `Compactions` — every confirmed `/compact`, broken down by warm vs cold cache state.
- `Leaks` — avoidable spend ranked: idle misses, rotations, restructures, cold compactions, plus the worst-offending sessions.
- `Settings` — edit `config.json` from the UI, copy-paste statusline + hook + systemd snippets.
- `Debug` — last `/usage` raw dump, parser output, ingest counters, schema version, paths.

Per-OS installers and the opt-in **Join the pack** community-insights upload are next.

## How it works

Three jobs, scheduled either by your OS or by a built-in scheduler:

- `bloodhound poll` — drives the Claude Code TUI in a pty, parses the `/usage` panel, persists the observation.
- `bloodhound ingest` — walks `~/.claude/projects/*.jsonl`, parses every assistant turn and compaction event into local SQLite.
- `bloodhound aggregate` — recomputes rolling metrics (sessions, 5-hour buckets, tokens-per-1% calibration) on a slower cadence.

`bloodhound daemon` runs all three on configurable intervals (defaults: poll 5m, ingest 5m, aggregate 15m). `bloodhound serve` runs the web UI at `http://127.0.0.1:7777`; it only reads from the local DB.

A `bloodhound status` subcommand renders one line suitable for Claude Code's statusline so you can keep an eye on your quota without leaving the terminal.

## Currency

Your currency is the percentage shown in `/usage`. Bloodhound never asks you to guess your plan's token cap or pick a billing-weight mode — those are implementation details we figure out from your data. The calibration phase converts the opaque percentage into a tokens-per-1% estimate by pairing adjacent observations and dividing the cost-weighted spend in between by the percentage points the bucket moved. Saturated observations (≥99%, when you're on the pay-per-use Extra usage tier and the bucket has stopped moving) are excluded.

## Self-healing extractor

The `/usage` panel is a TUI, and Anthropic ships layout changes without warning. Bloodhound parses it with a regex DSL stored at `$XDG_STATE_HOME/bloodhound/extractors.json`. When the bundled defaults stop matching, you can regenerate them with `bloodhound poll --rebootstrap`, which uses `claude -p` to discover field positions on a fresh capture. The version number in the file is checked at load time; older files fall back gracefully to the bundled defaults.

## Privacy

Bloodhound is local-first. Your data stays on your machine. The future opt-in **Join the pack** upload ships only anonymized token counts, classifications, and timings — never your conversations, file paths, project names, or session identifiers. A random per-install `device_id` plus an optional user-pasted `user_id` lets the server dedupe accounts that span multiple machines without ever learning who you are.

## Install

Pre-built binaries are not yet published. Build from source:

```bash
git clone https://github.com/PeterSR/claude-code-bloodhound
cd claude-code-bloodhound
make build      # builds the React bundle and embeds it; produces ./bloodhound
./bloodhound doctor
./bloodhound daemon &
./bloodhound serve
```

Then open http://127.0.0.1:7777.

For first-time setup the daemon will collect a few `/usage` polls before the gauges show anything useful; calibration needs at least two non-saturated observations in the same bucket.

## Paths

Bloodhound follows XDG conventions on Linux and the platform conventions on macOS / Windows.

| Kind | Linux | macOS | Windows |
|---|---|---|---|
| Database | `$XDG_DATA_HOME/bloodhound/bloodhound.db` | `~/Library/Application Support/bloodhound/` | `%LOCALAPPDATA%\bloodhound\` |
| Config | `$XDG_CONFIG_HOME/bloodhound/config.json` | `~/Library/Application Support/bloodhound/` | `%LOCALAPPDATA%\bloodhound\` |
| State (extractors, daemon log) | `$XDG_STATE_HOME/bloodhound/` | `~/Library/Application Support/bloodhound/` | `%LOCALAPPDATA%\bloodhound\` |

## License

MIT.
