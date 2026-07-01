import { useEffect, useState } from 'react';
import { Settings as SettingsIcon, Save, Copy, Check, AlertCircle } from 'lucide-react';
import { apiGet, ApiError } from '../api/client';

type Config = {
  poll_interval_s: number;
  ingest_interval_s: number;
  aggregate_interval_s: number;
  plan_tier: string;
  join_the_pack: boolean;
  user_id: string;
  claude_binary: string;
  statusline_prefix: string;
  stale_after_s: number;
  active_session_threshold_s: number;
  recent_session_window_s: number;
  extractor_self_heal: boolean;
  trail_enabled: boolean;
  trail_mode: string;
  trail_interval_s: number;
  trail_window_s: number;
};

type Snippets = {
  statusline: string;
  stop_hook: string;
  user_prompt_hook: string;
  systemd_user_unit: string;
  systemd_user_timer: string;
};

type SettingsResponse = {
  config: Config;
  config_path: string;
  state_path: string;
  data_path: string;
  snippets: Snippets;
};

const PLAN_OPTIONS = [
  { value: 'unknown', label: 'Unknown' },
  { value: 'pro', label: 'Pro' },
  { value: 'max-5x', label: 'Max 5x' },
  { value: 'max-20x', label: 'Max 20x' },
];

// Flip to true to show the Community insights / Join the pack card.
// Gated until the upload server exists.
const SHOW_COMMUNITY_INSIGHTS = false;

export default function Settings() {
  const [resp, setResp] = useState<SettingsResponse | null>(null);
  const [draft, setDraft] = useState<Config | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [savedAt, setSavedAt] = useState<number | null>(null);

  useEffect(() => {
    apiGet<SettingsResponse>('/settings')
      .then((r) => {
        setResp(r);
        setDraft(r.config);
      })
      .catch((e) => setError(e instanceof ApiError ? e.message : String(e)));
  }, []);

  async function save() {
    if (!draft) return;
    setSaving(true);
    setError(null);
    try {
      const res = await fetch('/api/settings', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(draft),
      });
      if (!res.ok) {
        const body = await res.text();
        throw new Error(body || `${res.status} ${res.statusText}`);
      }
      setSavedAt(Date.now());
    } catch (e: any) {
      setError(e.message);
    } finally {
      setSaving(false);
    }
  }

  return (
    <div>
      <div className="flex items-center gap-2 mb-2">
        <SettingsIcon className="size-5 text-zinc-500" />
        <h1 className="text-2xl font-semibold tracking-tight">Settings</h1>
      </div>
      <p className="text-zinc-600 dark:text-zinc-400 max-w-2xl mb-6">
        These map to <code className="font-mono text-xs">{resp?.config_path ?? 'config.json'}</code>.
        You can edit the file directly too — the daemon reads it at startup.
      </p>

      {error && (
        <div className="rounded-md border border-red-300 dark:border-red-700 bg-red-50 dark:bg-red-950/40 px-4 py-2 text-sm flex items-start gap-2 mb-4">
          <AlertCircle className="size-4 text-red-600 dark:text-red-400 shrink-0 mt-0.5" />
          <span className="text-red-700 dark:text-red-300">{error}</span>
        </div>
      )}

      {!resp || !draft ? (
        <div className="text-sm text-zinc-500">Loading…</div>
      ) : (
        <div className="space-y-6 max-w-3xl">
          <Card title="Daemon intervals (seconds)">
            <FieldRow>
              <Field label="Poll">
                <input type="number" className="text-input w-24" value={draft.poll_interval_s}
                  onChange={(e) => setDraft({ ...draft, poll_interval_s: parseInt(e.target.value || '0', 10) })} />
              </Field>
              <Field label="Ingest">
                <input type="number" className="text-input w-24" value={draft.ingest_interval_s}
                  onChange={(e) => setDraft({ ...draft, ingest_interval_s: parseInt(e.target.value || '0', 10) })} />
              </Field>
              <Field label="Aggregate">
                <input type="number" className="text-input w-24" value={draft.aggregate_interval_s}
                  onChange={(e) => setDraft({ ...draft, aggregate_interval_s: parseInt(e.target.value || '0', 10) })} />
              </Field>
              <Field label="Stale after">
                <input type="number" className="text-input w-24" value={draft.stale_after_s}
                  onChange={(e) => setDraft({ ...draft, stale_after_s: parseInt(e.target.value || '0', 10) })} />
              </Field>
            </FieldRow>
          </Card>

          <Card title="Now page (seconds)">
            <FieldRow>
              <Field label="Active session threshold">
                <input type="number" className="text-input w-28" value={draft.active_session_threshold_s}
                  onChange={(e) => setDraft({ ...draft, active_session_threshold_s: parseInt(e.target.value || '0', 10) })} />
              </Field>
              <Field label="Recent session window">
                <input type="number" className="text-input w-28" value={draft.recent_session_window_s}
                  onChange={(e) => setDraft({ ...draft, recent_session_window_s: parseInt(e.target.value || '0', 10) })} />
              </Field>
            </FieldRow>
            <p className="text-xs text-zinc-500 mt-3">
              Active threshold: a session whose last turn is within this many seconds gets the
              <strong> Active</strong> badge. Recent window: only sessions touched within this many
              seconds are listed at all.
            </p>
          </Card>

          <Card title="Extractor">
            <label className="flex items-start gap-2 text-sm">
              <input
                type="checkbox"
                className="mt-1"
                checked={draft.extractor_self_heal}
                onChange={(e) => setDraft({ ...draft, extractor_self_heal: e.target.checked })}
              />
              <div>
                <div>Self-heal on extraction failure</div>
                <div className="text-xs text-zinc-500 mt-0.5 max-w-xl">
                  When a poll fails to parse the <code className="font-mono text-xs">/usage</code> panel,
                  spawn <code className="font-mono text-xs">claude -p</code> to study the captured panel and
                  generate fresh extractor rules. Turn off if you prefer manual control — you can still
                  trigger a one-shot retrain from the Debug page.
                </div>
              </div>
            </label>
          </Card>

          <Card title="Trail — active-session tracker">
            <label className="flex items-start gap-2 text-sm mb-3">
              <input
                type="checkbox"
                className="mt-1"
                checked={draft.trail_enabled}
                onChange={(e) => setDraft({ ...draft, trail_enabled: e.target.checked })}
              />
              <div>
                <div>Enable Trail</div>
                <div className="text-xs text-zinc-500 mt-0.5 max-w-xl">
                  Passively summarises your recently-active sessions into per-session
                  briefs + open loops. Drives <code className="font-mono text-xs">claude</code> to
                  summarise, so it consumes usage — cost is attributed on the Trail page.
                </div>
              </div>
            </label>
            <FieldRow>
              <Field label="Mode">
                <select
                  className="text-input"
                  value={draft.trail_mode}
                  onChange={(e) => setDraft({ ...draft, trail_mode: e.target.value })}
                >
                  <option value="interactive">interactive (subscription)</option>
                  <option value="headless">headless (claude -p)</option>
                </select>
              </Field>
              <Field label="Interval (s)">
                <input type="number" className="text-input w-28" value={draft.trail_interval_s}
                  onChange={(e) => setDraft({ ...draft, trail_interval_s: parseInt(e.target.value || '0', 10) })} />
              </Field>
              <Field label="Active window (s, 0 = use Now threshold)">
                <input type="number" className="text-input w-28" value={draft.trail_window_s}
                  onChange={(e) => setDraft({ ...draft, trail_window_s: parseInt(e.target.value || '0', 10) })} />
              </Field>
            </FieldRow>
          </Card>

          <Card title="Statusline">
            <FieldRow>
              <Field label="Prefix">
                <input
                  className="text-input"
                  placeholder="🩸"
                  value={draft.statusline_prefix}
                  onChange={(e) => setDraft({ ...draft, statusline_prefix: e.target.value })}
                />
              </Field>
              <Field label="Claude binary path (override)">
                <input
                  className="text-input flex-1"
                  placeholder="(use $PATH)"
                  value={draft.claude_binary}
                  onChange={(e) => setDraft({ ...draft, claude_binary: e.target.value })}
                />
              </Field>
            </FieldRow>
          </Card>

          {/* Community insights / Join the pack — hidden until the
              upload server exists. Config fields stay in the schema
              (plan_tier, user_id, join_the_pack); only the UI is gated.
              Flip SHOW_COMMUNITY_INSIGHTS to true when ready. */}
          {SHOW_COMMUNITY_INSIGHTS && (
            <Card title="Community insights">
              <p className="text-xs text-zinc-500 mb-3">
                Plan tier is cosmetic — it only labels community-insights packets when you opt in.
                The Bloodhound community-insights server is not live yet, so this is a placeholder.
              </p>
              <FieldRow>
                <Field label="Plan tier">
                  <select
                    className="text-input"
                    value={draft.plan_tier}
                    onChange={(e) => setDraft({ ...draft, plan_tier: e.target.value })}
                  >
                    {PLAN_OPTIONS.map((o) => (
                      <option key={o.value} value={o.value}>{o.label}</option>
                    ))}
                  </select>
                </Field>
                <Field label="User ID (optional)">
                  <input
                    className="text-input flex-1"
                    placeholder="(blank = treat this device as its own user)"
                    value={draft.user_id}
                    onChange={(e) => setDraft({ ...draft, user_id: e.target.value })}
                  />
                </Field>
              </FieldRow>
              <label className="flex items-center gap-2 mt-3 text-sm">
                <input
                  type="checkbox"
                  checked={draft.join_the_pack}
                  onChange={(e) => setDraft({ ...draft, join_the_pack: e.target.checked })}
                />
                Join the pack — opt in to anonymized usage upload (no-op until v2)
              </label>
            </Card>
          )}

          <div className="sticky bottom-0 z-10 flex items-center gap-3 rounded-md border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 px-3 py-2 shadow-md">
            <button
              onClick={save}
              disabled={saving}
              className="inline-flex items-center gap-1.5 rounded-md bg-rose-500 hover:bg-rose-600 text-white px-3 py-1.5 text-sm font-medium disabled:opacity-60"
            >
              <Save className="size-3.5" />
              {saving ? 'Saving…' : 'Save'}
            </button>
            {savedAt && (
              <span className="text-xs text-emerald-600 dark:text-emerald-400 inline-flex items-center gap-1">
                <Check className="size-3.5" />
                Saved
              </span>
            )}
          </div>

          <Card title="Snippets">
            <p className="text-xs text-zinc-500 mb-3">
              Copy these into the indicated config files. Bloodhound never edits them
              automatically.
            </p>
            <div className="space-y-3">
              <Snippet label="Claude Code statusline (~/.config/claude/settings.json)" body={resp.snippets.statusline} />
              <Snippet label="Stop hook (~/.config/claude/settings.json)" body={resp.snippets.stop_hook} />
              <Snippet label="UserPromptSubmit hook (~/.config/claude/settings.json)" body={resp.snippets.user_prompt_hook} />
              <Snippet label="systemd user service (~/.config/systemd/user/bloodhound.service)" body={resp.snippets.systemd_user_unit} />
              <Snippet label="systemd user timer (~/.config/systemd/user/bloodhound.timer)" body={resp.snippets.systemd_user_timer} />
            </div>
          </Card>

          <Card title="Paths">
            <div className="text-xs font-mono space-y-1 text-zinc-600 dark:text-zinc-400">
              <div><span className="text-zinc-500">config: </span>{resp.config_path}</div>
              <div><span className="text-zinc-500">state:  </span>{resp.state_path}</div>
              <div><span className="text-zinc-500">data:   </span>{resp.data_path}</div>
            </div>
          </Card>
        </div>
      )}

      <style>{`
        .text-input {
          padding: 0.375rem 0.625rem;
          border-radius: 0.375rem;
          border: 1px solid rgb(212 212 216);
          background: white;
          font-size: 0.875rem;
          outline: none;
        }
        .dark .text-input {
          background: rgb(24 24 27);
          color: rgb(244 244 245);
          border-color: rgb(63 63 70);
        }
      `}</style>
    </div>
  );
}

function Card({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-4">
      <div className="text-sm font-medium text-zinc-700 dark:text-zinc-300 mb-3">{title}</div>
      {children}
    </div>
  );
}

function FieldRow({ children }: { children: React.ReactNode }) {
  return <div className="flex flex-wrap items-end gap-3">{children}</div>;
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-1">
      <span className="text-[10px] uppercase tracking-wider text-zinc-500">{label}</span>
      {children}
    </div>
  );
}

function Snippet({ label, body }: { label: string; body: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <div className="rounded-md border border-zinc-200 dark:border-zinc-800 overflow-hidden">
      <div className="flex items-center justify-between bg-zinc-100 dark:bg-zinc-800/50 px-3 py-1.5 text-xs">
        <span className="text-zinc-600 dark:text-zinc-400 font-mono">{label}</span>
        <button
          onClick={async () => {
            await navigator.clipboard.writeText(body);
            setCopied(true);
            setTimeout(() => setCopied(false), 1200);
          }}
          className="inline-flex items-center gap-1 text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100"
        >
          {copied ? <Check className="size-3" /> : <Copy className="size-3" />}
          {copied ? 'Copied' : 'Copy'}
        </button>
      </div>
      <pre className="text-[11px] leading-snug bg-zinc-50 dark:bg-zinc-950 p-3 overflow-auto whitespace-pre-wrap break-words font-mono">
        {body}
      </pre>
    </div>
  );
}
