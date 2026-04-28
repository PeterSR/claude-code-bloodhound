# Bloodhound

**Track your Claude Code subscription usage over time. Make educated decisions about how to use it today.**

Claude Code's subscription burns through opaque rate limits — a 5-hour session window and a weekly window, both expressed only as percentages in the `/usage` TUI panel. There's no API. No history. No way to know whether your next prompt will sail through or push you into a hard stop.

Bloodhound watches the JSONL session logs Claude Code already writes to your disk, polls `/usage` on a schedule you control, and stores it all locally so you can answer questions like:

- When am I projected to hit my 5-hour or weekly limit?
- How much can I realistically do before the bucket runs out?
- What does my next prompt likely cost?
- Should I `/compact` now? Was my last compaction warm or cold?
- Where am I leaking quota — idle gaps, cold compactions, or breakpoint rotations?
- Which projects are eating the most of my session?

## Status

Early. Architecture is settled and scaffolding is in place. v1 ships the ingestion pipeline, a one-page "Now" view, and a debug page. The other pages and the opt-in **Join the pack** community-insights upload land in later versions.

## How it works

Three jobs, scheduled by your OS (or a built-in `bloodhound daemon`):

- `bloodhound poll` — drives the Claude Code TUI in a pty, parses the `/usage` panel, persists the observation.
- `bloodhound ingest` — walks `~/.claude/projects/*.jsonl`, parses every assistant turn and compaction event into local SQLite.
- `bloodhound aggregate` — recomputes rolling metrics (tokens-per-1%, bucket fills, compaction analysis) on a slower cadence.

`bloodhound serve` runs the web UI at `http://127.0.0.1:7777`. It only reads from the local DB; the heavy work has already happened on the schedule.

A `bloodhound status` subcommand renders one line suitable for use as Claude Code's statusline command, so you can keep an eye on your quota without leaving the terminal.

## Currency

Your currency is the percentage shown in `/usage`. Bloodhound never asks you to guess your plan's token cap or pick a billing-weight mode — those are implementation details we figure out from your data.

## Privacy

Bloodhound is local-first. Your data stays on your machine. The future opt-in **Join the pack** upload ships only anonymized token counts, classifications, and timings — never your conversations, file paths, project names, or session identifiers.

## Install

Coming soon. The plan is per-OS installers (`bloodhound install`) that wire up systemd / launchd / Scheduled Tasks for you.

## License

MIT.
