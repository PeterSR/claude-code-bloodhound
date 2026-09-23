# Bloodhound

**Track your Claude Code subscription usage over time. Make educated decisions about how to use it today.**

Claude Code's subscription burns through opaque rate limits - a 5-hour session window and a weekly window, both expressed only as percentages in the `/usage` TUI panel.

Bloodhound watches the JSONL session logs Claude Code already writes to your disk, polls `/usage` on a schedule you control, and stores it all locally so you can answer questions like:

- When am I projected to hit my 5-hour or weekly limit?
- How much can I realistically do before the bucket runs out?
- What does my next prompt likely cost? (calibration: tokens per 1%)
- Should I `/compact` now? Was my last compaction warm or cold?
- Where am I leaking quota - idle gaps, cold compactions, or breakpoint rotations?
- Which sessions are eating the most of my week?

## Status

**v0.1 alpha - usable, but expect rough edges.** The data model and the daily-driver UI are in place:

- `Now` - gauges for session and week, time to natural reset, burn-rate projection (only when actionable), and a badge once you blow past 100% saying which of the three things that turned out to mean: extra usage billing, requests refused, or requests still going through at the lower priority Claude Code offers.
- `History` - `/usage` % over time, the calibration trend (tokens per 1%), and a weekday × hour heatmap of when you spend.
- `Sessions` - every session on disk, sortable, click-through to a per-turn view with classification (idle / rotation / restructure) and inline compactions.
- `Settings` - edit `config.json` from the UI, copy-paste statusline + hook + systemd snippets.
- `Debug` - last `/usage` raw dump, parser output, ingest counters, schema version, paths, and a Retrain button that triggers an on-demand extractor self-heal.

Two more pages exist behind direct URLs but stay out of the sidebar in v0.1 until the visuals get a polish pass: `/compactions` (every confirmed `/compact`, broken down by warm vs cold cache state) and `/leaks` (avoidable spend ranked - idle misses, rotations, restructures, cold compactions).

The opt-in **Join the pack** community-insights upload is next - see [Privacy](#privacy) for what that's for and what it would and wouldn't share.

## How it works

Three jobs, scheduled either by your OS or by a built-in scheduler:

- `bloodhound poll`: drives the Claude Code TUI in a pty, parses the `/usage` panel, persists the observation.
- `bloodhound ingest`: walks `~/.claude/projects/*.jsonl`, parses every assistant turn and compaction event into local SQLite.
- `bloodhound aggregate`: recomputes rolling metrics (sessions, 5-hour buckets, tokens-per-1% calibration, per-session limit attribution) on a slower cadence.

`bloodhound daemon` runs all three on configurable intervals (defaults: poll 5m, ingest 5m, aggregate 15m) and exposes a JSON API over a unix socket at `$XDG_RUNTIME_DIR/bloodhound/api.sock` - no TCP port to conflict with anything on your box.

`bloodhound-gui` is a native window that loads the dashboard and proxies its API calls to the daemon's socket. Pre-built for Linux x86_64; macOS / Windows / Linux-arm builds work in principle but aren't shipped yet (see [Platforms](#platforms)). It's a separate binary so the daemon can run headless on machines without a desktop environment.

A `bloodhound status` subcommand renders one line suitable for Claude Code's statusline so you can keep an eye on your quota without leaving the terminal.

## Attribution

`/usage` tells you 42% of the 5-hour window is gone; it does not tell you which of the four Claude Code windows you have open spent it. Bloodhound works that out.

Every time the meter moves between two observations, that movement is split across the sessions that were running in between, weighted by what each one cost, up to what those sessions could plausibly account for at what a point actually cost in that same window. So the numbers are anchored to what the panel actually reported rather than to a token-to-percent conversion, and every session in a weekly window adds up to that window's usage. Spend the meter has not reported yet (the tail since the last poll, a stretch where the bucket was saturated, history from before you installed bloodhound) is estimated instead, priced at whatever a point cost in the nearest window that could be reconciled, and stays labelled as an estimate everywhere it appears. Movement that local sessions can't plausibly account for (another box on the same account, a transcript that was never ingested) is reported as unattributed rather than quietly shared out onto whatever session happened to be running.

The Attribution page rolls this up by working directory and by session, over weekly or 5-hour windows. The Sessions list carries the same figures per row, and each session's detail page breaks its cost down window by window.

## Currency

Your currency is the percentage shown in `/usage`. Bloodhound never asks you to guess your plan's token cap or pick a billing-weight mode - those are implementation details we figure out from your data. The calibration phase converts the opaque percentage into a tokens-per-1% estimate by pairing adjacent observations and dividing the cost-weighted spend in between by the percentage points the bucket moved. Saturated observations (≥99%, when you're on the pay-per-use Extra usage tier and the bucket has stopped moving) are excluded.

## Self-healing extractor

The `/usage` panel is a TUI, and Anthropic ships layout changes without warning. Bloodhound parses it with a regex DSL stored at `$XDG_STATE_HOME/bloodhound/extractors.json`. When the bundled defaults stop matching, the daemon auto-heals: it spawns a second interactive `claude` in its own pty as an orchestrator and hands it four MCP tools (`read_pty`, `send_keys`, `test_regex`, `save_extractor`) to drive a live pty session, re-learn the field positions, and persist a working extractor. Running the orchestrator as a *second interactive* claude rather than `claude -p` means the heal cost lands on your subscription instead of the Agent SDK credit pool. Gated by `extractor_self_heal` in the config (default on). You can also trigger one on demand via the Retrain button on the Debug page or `POST /api/extractor/retrain`. The version number in the file is checked at load time; older files fall back gracefully to the bundled defaults until the next heal.

## Privacy

Bloodhound is local-first. Your data stays on your machine. There is no server today, and the daemon makes no outbound network calls beyond driving the local `claude` binary.

The "Join the pack" feature is the planned opt-in to that. Why have one at all: most of the interesting questions about Claude Code's subscription only get sharp answers when you can compare across installs. Examples:

- **Are tokens-per-1% the same for everyone?** If your calibration drifts from the pack median over weeks, Anthropic may be quietly differentiating cost weights by plan, account age, region, or load.
- **Does the 5-hour window really mean 5 hours?** Reset cadence and saturation rates pooled across many accounts make it visible if some users get longer-effective windows.
- **Where are the cliffs?** When the bundled extractor breaks because Anthropic redesigned the `/usage` panel, the pack notices in minutes instead of one user at a time.
- **Plan-tier comparisons.** Pro vs Max-5x vs Max-20x - what does each dollar actually buy in throughput once you account for cache hits and saturation?

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

The daemon is pure-Go and ships pre-built for Linux and macOS × amd64 / arm64. The native GUI is a Wails app and ships pre-built for Linux x86_64 only. Daemon-only setups give you the full API - the GUI is just a viewer on top.

Both binaries compile cleanly for every other platform listed below; what's missing is the *release pipeline* extension (macOS GUI needs a `macos-*` runner in the release workflow; Windows wants real-hardware validation before it gets a release artifact). The CI-verified column is what makes that gap fillable as soon as someone reports back from a real machine.

|                       | Daemon (pre-built) | GUI (pre-built) | CI-verified compile         |
| --------------------- | :----------------: | :-------------: | --------------------------- |
| Linux x86_64          | ✅                  | ✅               | daemon + GUI                |
| Linux arm64           | ✅                  | build from source | daemon + GUI               |
| macOS arm64 / amd64   | ✅                  | build from source | daemon + GUI               |
| Windows amd64         | build from source  | build from source | daemon + GUI               |

**Help me test Bloodhound on your platform.** I develop on Linux x86_64, so that's the only combo I exercise end-to-end. The other rows compile in CI but haven't been driven against a real `claude` session by anyone yet - especially Windows, where the daemon talks to Claude Code via a freshly-written ConPTY shim that mirrors the Unix pty path on paper but is unproven in practice. If you try Bloodhound on macOS, Windows, or Linux arm64 - daemon-only or full GUI - open [an issue](https://github.com/PeterSR/claude-code-bloodhound/issues) with your `bloodhound doctor` output and what worked or broke. Platform fixes get fast-tracked, and credited.

First-launch friction to expect on unsigned binaries:

- **macOS Gatekeeper** refuses to run downloaded executables. Clear the quarantine bit: `xattr -d com.apple.quarantine bloodhound bloodhound-gui`. Or right-click the binary in Finder → Open → Open for a one-time bypass.
- **Windows SmartScreen** shows *Windows protected your PC* on first run. Click **More info → Run anyway**.

### Pre-built binary

Grab the right tarball or zip from the [Releases page](https://github.com/PeterSR/claude-code-bloodhound/releases), unpack, and drop `bloodhound` somewhere on your `$PATH`.

```bash
bloodhound doctor               # sanity-check paths, schema, claude binary
bloodhound install --enable     # write + start the systemd user unit (Linux)
```

On Linux, if you also unpacked the GUI tarball and ran its `install-gui.sh`, launch `bloodhound-gui` from your application menu. Otherwise - or on macOS / Windows where there is no GUI binary yet - the dashboard's API is reachable directly over the unix socket:

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
- **macOS:** Xcode Command Line Tools (`xcode-select --install`). The Makefile target is Linux-shaped, so build the binary directly: `cd web && npm ci && npm run build && cd .. && go build -tags prod,production,desktop -o bloodhound-gui ./cmd/bloodhound-gui`. If you get it working - or it explodes - please report back.
- **Windows:** WebView2 runtime (pre-installed on Windows 10/11). PowerShell: `cd web; npm ci; npm run build; cd ..; go build -tags prod,production,desktop -o bloodhound-gui.exe ./cmd/bloodhound-gui`. Same "let me know how it went" applies.

For first-time setup the daemon will collect a few `/usage` polls before the gauges show anything useful; calibration needs at least two non-saturated observations in the same bucket. If the GUI launches before the daemon is running, it shows a setup wizard with the commands above.

## Paths

Bloodhound follows XDG conventions on Linux and the platform conventions on macOS / Windows.

| Kind | Linux | macOS | Windows |
|---|---|---|---|
| Database | `$XDG_DATA_HOME/bloodhound/bloodhound.db` | `~/Library/Application Support/bloodhound/` | `%LOCALAPPDATA%\bloodhound\` |
| Config | `$XDG_CONFIG_HOME/bloodhound/config.json` | `~/Library/Application Support/bloodhound/` | `%LOCALAPPDATA%\bloodhound\` |
| State (extractors, daemon log) | `$XDG_STATE_HOME/bloodhound/` | `~/Library/Application Support/bloodhound/` | `%LOCALAPPDATA%\bloodhound\` |

### Per-project settings

A project can keep a `.bloodhound/config.json` beside its code. It is a
different thing from the config above, not an override of it, and the two
share no keys. The global config is how the daemon operates: where the
database lives, which `claude` binary to drive, how often to poll. The project
file only says how bloodhound should behave toward sessions working in that
directory, which is what makes it safe to check in - it can make bloodhound
quieter or chattier in one project and can do nothing else.

```json
{
  "pressure":      { "message": "" },
  "writeup_nudge": { "enabled": false, "message": "" },
  "cache_nudge":   { "enabled": false, "message": "" },
  "wakeup":        { "mode": "off", "nudge_message": "",
                     "armed_message": "", "resume_message": "" }
}
```

One block per thing bloodhound can say. Everything that puts words into a
conversation is off by default.

`writeup_nudge` speaks up when a session's recent turns have outgrown its own
average, which is the run-up to a compaction, so the work can be written up
while the detail is still in the context rather than reconstructed from a
summary afterwards. It goes only to the session it is about.

`cache_nudge` is the one line bloodhound will say to a session that is *not*
working, and the exception is the whole point: it fires while the prompt cache
is still warm but inside its last stretch. The turn it starts is paid at
warm-cache rates, and the alternative is that the same context gets rebuilt
from nothing the next time anyone touches the session.

`wakeup.mode` is what happens at the far end of a warning. `nudge` adds a line
saying the work could pick up again when the window reopens, if something is
armed to wake it; bloodhound suggests and arms nothing. `resume` drops the
suggestion and takes the job: the session is noted down against the window
that is constraining it, and when that window reopens bloodhound writes to it
again. Promises are only made for resets within twelve hours, so a weekly
window normally gets neither - "I will write to you on Thursday" is a calendar
entry, not a resumption. `bloodhound wakeups` shows the promises outstanding.

`pressure` is wording only. A limit that stops every session on the machine is
not something a directory switches off, so the only question is what the
sentence says.

Every `message` field is a Go template that *replaces* what bloodhound would
have said, and is handed that sentence as `{{.Text}}`. There is no separate
key for adding to a message, because `"{{.Text}} Push the branch first."` says
it and also says which side it goes on. The other variables are `.Kind`
`.State` `.Bucket` `.Cwd` `.Dir` `.Project` `.Session` `.Pct` `.Reset`
`.ResetAt` `.ETA` `.ArmedAt` `.Waited` `.Now`. A template that does not parse,
or fails while rendering, never silences anything: it is reported by
`bloodhound project show` and `bloodhound doctor`, and the built-in wording
goes out in its place.

Except for `cache_nudge` and a promised wakeup, bloodhound only ever speaks
into a session that is already mid-turn. Delivering to one sitting at its
prompt would start a turn it was not going to take, and if its cache had gone
cold that turn re-pays the whole conversation prefix before reading a word,
charged to the very budget the warning was about. The two exceptions are the
cases where that same arithmetic points the other way.

`bloodhound project init` writes a starter file, `bloodhound project show
--preview` resolves which one governs a directory and renders what it would
actually say against made-up numbers. The search walks up from the working
directory to the nearest such file, stopping at your home directory, so a
session started in a subdirectory is still governed by its project.

### Schemas

Machine-readable JSON Schemas for the three files anyone edits by hand are in
[`schema/`](schema/): the global config, a project's `.bloodhound/config.json`,
and a `/usage` extractor ruleset. `bloodhound project init` writes the
`$schema` pointer into the file it creates, and both `config.json` and a
self-healed extractor gain one the first time bloodhound writes them, so
editors offer completion and flag a misspelled key at the moment it is typed.
A test walks the schemas and the Go structs together and fails if either grows
a field the other does not have.

## Disclaimer

This is a tool for monitoring Claude Code, and - fittingly - Claude Code has been used to build it. The Go and TypeScript here are vetted by a human, but parts of the implementation, scaffolding, and copy were drafted with AI assistance.

It's also **alpha software**. Expect rough edges: things may misclassify, the schema may change, your local DB may need to be wiped between releases, and the bundled `/usage` extractor will break the next time Anthropic redesigns the panel. The daemon's self-heal usually catches that on the first failed poll; if it can't, the Debug page has a manual Retrain button. It does not modify your Claude Code installation, your JSONL session files, or anything outside its own state directory - but treat the numbers as informational, not as a substitute for Anthropic's own billing.

Bloodhound is not affiliated with or endorsed by Anthropic.

## License

MIT.
