import { useMemo, useState } from 'react';
import { PieChart } from 'lucide-react';
import { Link } from 'react-router-dom';
import { useApi } from '../hooks/useApi';
import ReloadButton from '../components/ReloadButton';
import { fmtCount, fmtNumber } from '../lib/format';

type Bucket = 'week' | '5h';
type GroupBy = 'project' | 'session' | 'cwd';

type AttrSlice = {
  key: string;
  label: string;
  pct: number;
  measured_pct: number;
  estimated_pct: number;
  cw_tokens: number;
  turn_count: number;
};

type AttrWindow = {
  start_unix_ms: number;
  end_unix_ms: number;
  reset_unix_ms?: number;
  inferred?: boolean;
  partial?: boolean;
  in_progress?: boolean;
  hit_cap?: boolean;
  measured_pct: number;
  attributed_pct: number;
  peak_pct?: number;
  slices: AttrSlice[];
};

type AttrGroup = {
  key: string;
  project?: string;
  cwd?: string;
  pct: number;
  measured_pct: number;
  estimated_pct: number;
  peak_pct: number;
  share: number;
  cw_tokens: number;
  raw_tokens: number;
  turn_count: number;
  sessions: number;
  windows: number;
  first_ts_unix_ms?: number;
  last_ts_unix_ms?: number;
};

type AttributionResponse = {
  ok: boolean;
  bucket: Bucket;
  by: GroupBy;
  window_days: number;
  tokens_per_pct_cw?: number;
  windows: AttrWindow[];
  projects: AttrGroup[];
  sessions: AttrGroup[];
  cwds: AttrGroup[];
  total_pct: number;
  measured_pct: number;
  estimated_pct: number;
  unattributed_pct: number;
};

/** The sentinel the server uses for meter movement no turn of ours explains. */
const UNATTRIBUTED = '';
/** Server-side per-window tail fold. Unmapped by colorOf, so it lands in
 *  the same neutral slot as everything past the eighth series. */
const SERVER_OTHER = '__other__';
/** Mirrors store.UnknownCwd: a real, known session (or supervisor plus
 *  subagents) whose cwd was never captured. Distinct from UNATTRIBUTED,
 *  which is a fact about meter movement, not about any session - this is
 *  "we know exactly who spent it, just not where". Only meaningful when
 *  by === 'cwd'. */
const UNKNOWN_CWD = '__unknown_cwd__';

/** How many series get their own colour before the tail folds into "other". */
const SERIES_SLOTS = 8;

/** Sentinel "days" value standing in for `?window=current`: the single open
 *  limit window rather than a day-count lookback. Kept in the same RANGES
 *  table (and the same `days` state) as the real day counts so the segmented
 *  control's selection logic doesn't need a second, parallel notion of
 *  "what's picked" - only the fetch and the render below branch on it. -1 is
 *  safe as a sentinel because a real window_days is clamped to [1, 365]
 *  server-side, so it can never collide with a real value. */
const CURRENT_WINDOW = -1;

const RANGES: Record<Bucket, { days: number; label: string }[]> = {
  week: [
    { days: CURRENT_WINDOW, label: 'This week' },
    { days: 28, label: '4 weeks' },
    { days: 56, label: '8 weeks' },
    { days: 182, label: '6 months' },
  ],
  '5h': [
    { days: CURRENT_WINDOW, label: 'Current window' },
    { days: 2, label: '2 days' },
    { days: 7, label: '7 days' },
    { days: 30, label: '30 days' },
  ],
};

export default function Attribution() {
  const [bucket, setBucket] = useState<Bucket>('week');
  const [by, setBy] = useState<GroupBy>('project');
  // Kept per bucket: "8 weeks" and "8 days" are not the same question, and
  // switching meters shouldn't silently reinterpret the range.
  //
  // Weekly defaults to the current window rather than a lookback: "how am I
  // doing right now" is the more common question than "how did the last two
  // months look", and it's the one range whose headline is directly
  // comparable to /usage's own number (bounded by 100, never summed across
  // several windows). 5h keeps a real day-range default instead - a 5-hour
  // window turns over so often that "current window" alone is a much
  // thinner slice of the story there, and a returning user is more often
  // mid-analysis of a burn pattern spanning several of them.
  const [weekDays, setWeekDays] = useState(CURRENT_WINDOW);
  const [fiveHDays, setFiveHDays] = useState(7);
  const days = bucket === 'week' ? weekDays : fiveHDays;
  const isCurrent = days === CURRENT_WINDOW;

  const { data, error, loading, refreshing, refresh } = useApi<AttributionResponse>(
    isCurrent
      ? `/attribution?bucket=${bucket}&by=${by}&window=current`
      : `/attribution?bucket=${bucket}&by=${by}&window_days=${days}`,
    60_000,
  );

  const groups = data ? (by === 'project' ? data.projects : by === 'cwd' ? data.cwds : data.sessions) : [];

  // Colour follows the entity across the whole range, not its rank inside
  // one window, so a project keeps its hue even in the weeks it barely
  // shows up. Everything past the eighth slot shares the neutral "other".
  // isColored is exposed alongside colorOf so callers can tell "this key
  // got a real colour" apart from "this key fell through to the shared
  // other-var(--series-other) default" - colorOf alone can't distinguish
  // the two, and the per-window fold below needs exactly that distinction.
  const { colorOf, isColored } = useMemo(() => {
    const m = new Map<string, string>();
    let slot = 0;
    for (const g of groups) {
      if (g.key === UNATTRIBUTED) continue;
      if (slot >= SERIES_SLOTS) break;
      slot += 1;
      m.set(g.key, `var(--series-${slot})`);
    }
    return {
      colorOf: (key: string) => m.get(key) ?? 'var(--series-other)',
      isColored: (key: string) => m.has(key),
    };
  }, [groups]);

  const legend = useMemo(
    () => groups.filter((g) => g.key !== UNATTRIBUTED).slice(0, SERIES_SLOTS),
    [groups],
  );
  const foldedCount = Math.max(0, groups.filter((g) => g.key !== UNATTRIBUTED).length - SERIES_SLOTS);

  const meter = bucket === 'week' ? 'weekly limit' : '5-hour limit';

  return (
    <div>
      <div className="flex items-center gap-2 mb-2">
        <PieChart className="size-5 text-zinc-600 dark:text-zinc-400" />
        <h1 className="text-2xl font-semibold tracking-tight">Attribution</h1>
        <div className="ml-auto">
          <ReloadButton refreshing={refreshing} onClick={refresh} />
        </div>
      </div>
      <p className="text-zinc-600 dark:text-zinc-400 max-w-3xl mb-4">
        Which session, and which working directory, actually spent your{' '}
        {meter}. Every time the /usage meter moves, that movement is split
        between the sessions that were running, weighted by what each one
        cost. Spend the meter hasn't reported yet is estimated and marked as
        such.
      </p>

      <div className="flex flex-wrap gap-3 items-center mb-5">
        <Segmented
          value={bucket}
          onChange={(v) => setBucket(v as Bucket)}
          options={[
            { value: 'week', label: 'Weekly limit' },
            { value: '5h', label: '5-hour limit' },
          ]}
        />
        <Segmented
          value={by}
          onChange={(v) => setBy(v as GroupBy)}
          options={[
            { value: 'project', label: 'By project' },
            { value: 'cwd', label: 'By working dir' },
            { value: 'session', label: 'By session' },
          ]}
        />
        <Segmented
          value={String(days)}
          onChange={(v) =>
            bucket === 'week' ? setWeekDays(Number(v)) : setFiveHDays(Number(v))
          }
          options={RANGES[bucket].map((r) => ({ value: String(r.days), label: r.label }))}
        />
      </div>

      {error && <div className="text-sm text-red-600 dark:text-red-400 mb-4">{error.message}</div>}
      {loading && !data && <div className="text-sm text-zinc-500">Loading…</div>}

      {data && data.windows.length === 0 && (
        <Empty
          hint={
            isCurrent
              ? `No ${meter} window is currently open right now. This section fills in the moment the daemon's next /usage poll opens one.`
              : 'Nothing attributed in this range yet. The aggregator fills this in as the daemon collects /usage readings alongside your sessions.'
          }
        />
      )}

      {data && data.windows.length > 0 && (
        <>
          {/* Tiles 1 and 4 are points of the meter itself (bounded by 100 in
              current-window mode, summable past it in multi-window mode);
              tiles 2 and 3 are a share OF TILE 1's number, a different and
              much smaller denominator. Four identically styled cards in a
              row invited reading all four against the same scale (a user
              asked what "Measured 100%" meant, on a week where the
              attributed total itself was well under 100 - the measured
              share of THAT was 100%, not 100% of the weekly limit). Measured
              and Estimated stay four-across for layout, but render
              `sub` (smaller, lighter number) and spell out their own
              denominator in both the value and the hint, tying back to the
              literal number tile 1 shows, in both current-window and
              multi-window mode alike. Total and Unattributed keep full
              weight and stay phrased as "points of the {meter}" so a reader
              never has to guess which of the two scales a given tile is on. */}
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4 mb-6">
            <Tile
              label={isCurrent ? `This ${bucket === 'week' ? 'week' : 'window'} so far` : `Total ${bucket === 'week' ? 'weekly' : '5h'} limit spent`}
              value={`${fmtPct(data.total_pct)}%`}
              hint={
                isCurrent
                  ? `points of the ${meter}: the open window only, so this reads the same as /usage`
                  : `points of the ${meter}, summed across ${data.windows.length} window${data.windows.length === 1 ? '' : 's'}`
              }
            />
            <Tile
              sub
              label="Measured"
              value={`${pctOf(data.measured_pct, data.total_pct)}% of that`}
              hint={`Share of the ${fmtPct(data.total_pct)}% attributed above that's anchored to observed meter movement, not of the ${meter} itself.`}
            />
            <Tile
              sub
              label="Estimated"
              value={`${pctOf(data.estimated_pct, data.total_pct)}% of that`}
              hint={
                data.tokens_per_pct_cw
                  ? `Share of the ${fmtPct(data.total_pct)}% attributed above, priced at ≈${fmtNumber(data.tokens_per_pct_cw)} weighted tokens per point.`
                  : `Share of the ${fmtPct(data.total_pct)}% attributed above; no calibration yet.`
              }
            />
            <Tile
              label="Unattributed"
              value={`${fmtPct(data.unattributed_pct)}%`}
              hint={`Points of the ${meter} that no session on this machine explains.`}
              accent={data.unattributed_pct > 0.05 * data.total_pct ? 'text-amber-500' : undefined}
            />
          </div>

          <Section
            title={bucket === 'week' ? 'Weekly windows' : '5-hour windows'}
            subtitle={
              isCurrent
                ? `The window that's currently open, filled toward its 100% cap and segmented by ${byNoun(by)}.`
                : `Each bar is one limit window filled toward its 100% cap, segmented by ${byNoun(by)}.`
            }
          >
            {isCurrent ? (
              // A vertical stack sized for a row of eight has nothing to
              // compare a single bar against, so it reads as a mistake
              // ("why is there only one") rather than a chart. A full-width
              // horizontal bar is the one-window analogue of the same
              // stacked-segment idea and fills its space on purpose instead
              // of floating alone in it.
              <CurrentWindowBar window={data.windows[0]} colorOf={colorOf} isColored={isColored} by={by} />
            ) : (
              <WindowStacks windows={data.windows} colorOf={colorOf} isColored={isColored} by={by} />
            )}
            <Legend items={legend} colorOf={colorOf} by={by} foldedCount={foldedCount} />
          </Section>

          <Section
            title={`By ${byNoun(by)}`}
            subtitle={
              isCurrent
                ? `Share of the ${bucket === 'week' ? 'weekly' : '5-hour'} limit's currently open window.`
                : bucket === 'week'
                  ? 'Share of the weekly limit, summed over every weekly window in range.'
                  : 'Share of the 5-hour limit. A total over 100% means the work spanned several windows; peak is the worst single one.'
            }
          >
            <GroupTable groups={groups} colorOf={colorOf} by={by} bucket={bucket} />
          </Section>
        </>
      )}
    </div>
  );
}

/** A rendered slice after foldOtherSlices: identical to what the server
 *  sent, except the merged "other" entry (key === SERVER_OTHER) carries
 *  foldedCount, how many distinct groups were summed into it - information
 *  the merge would otherwise throw away. */
type RenderSlice = AttrSlice & { foldedCount?: number };

/** Folds every slice that doesn't get its own colour into one "other"
 *  segment, so a window with a long uncoloured tail draws a single grey
 *  block instead of one sliver per group.
 *
 *  colorOf/isColored rank groups across the whole selected range, not per
 *  window, so a single window can easily hold far more uncoloured groups
 *  than coloured ones: measured on a real by=cwd, 8-week range, one window
 *  shipped 21 slices and only 4 matched a top-8 range group - the other 17
 *  each drew as their own 2px-gapped grey sliver. Stacked that thin and
 *  that densely, the gaps between unrelated slices read as the same hatch
 *  texture the caption reserves for the unattributed sentinel, which was
 *  actively misleading on an account where unattributed is 0%. The
 *  sentinel is left untouched by this fold: it's a different fact (meter
 *  movement no session explains) from "many small groups", and staying
 *  its own hatched segment is the point.
 *
 *  Also swallows whatever the server itself already folded past
 *  attrMaxSlices (key === SERVER_OTHER): that's just one more uncoloured
 *  entry from this function's point of view, so it merges into the same
 *  bucket with no special case, though its own foldedCount only ever
 *  contributes 1 - the server doesn't say how many groups its own fold
 *  represents, so a window that trips both folds slightly undercounts
 *  foldedCount by however many the server had already combined.
 *
 *  Re-sorted afterward so the stack still reads biggest-first once the
 *  fold changes what the largest remaining entries are. */
function foldOtherSlices(slices: AttrSlice[], isColored: (key: string) => boolean): RenderSlice[] {
  const kept: RenderSlice[] = [];
  let other: RenderSlice | null = null;
  for (const s of slices) {
    if (s.key === UNATTRIBUTED || isColored(s.key)) {
      kept.push(s);
      continue;
    }
    if (!other) {
      other = {
        key: SERVER_OTHER,
        label: 'other',
        pct: 0,
        measured_pct: 0,
        estimated_pct: 0,
        cw_tokens: 0,
        turn_count: 0,
        foldedCount: 0,
      };
      kept.push(other);
    }
    other.pct += s.pct;
    other.measured_pct += s.measured_pct;
    other.estimated_pct += s.estimated_pct;
    other.cw_tokens += s.cw_tokens;
    other.turn_count += s.turn_count;
    other.foldedCount = (other.foldedCount ?? 0) + 1;
  }
  return kept.sort((a, b) => b.pct - a.pct);
}

/** One bar per limit window, stacked bottom-up biggest first. */
function WindowStacks({
  windows,
  colorOf,
  isColored,
  by,
}: {
  windows: AttrWindow[];
  colorOf: (key: string) => string;
  isColored: (key: string) => boolean;
  by: GroupBy;
}) {
  const TRACK = 170; // px; the full track height is 100% of the window
  return (
    <div className="overflow-x-auto">
      <div className="flex items-end gap-1.5 pt-2 pb-1 min-w-fit">
        {windows.map((w) => {
          const faded = w.partial || w.inferred;
          const slices = foldOtherSlices(w.slices, isColored);
          return (
            <div key={w.start_unix_ms} className="flex flex-col items-center gap-1 shrink-0">
              <div
                className="relative w-10 rounded bg-zinc-100 dark:bg-zinc-800 overflow-hidden"
                style={{ height: TRACK }}
                title={windowTitle(w)}
              >
                {/* 2px of surface between fills so adjacent segments never
                    read as one block; the topmost gets the rounded data end. */}
                <div className="absolute inset-x-0 bottom-0 flex flex-col-reverse gap-[2px]">
                  {slices.map((s, i) => (
                    <div
                      key={s.key || 'unattributed'}
                      className={`shrink-0 ${i === slices.length - 1 ? 'rounded-t-[3px]' : ''}`}
                      style={{
                        height: Math.max(1, (s.pct / 100) * TRACK),
                        background:
                          s.key === UNATTRIBUTED
                            ? // Hatched, not just grey: the gap is a different
                              // kind of thing from a small project, and the
                              // texture survives greyscale and colourblindness.
                              'repeating-linear-gradient(45deg, var(--series-other) 0 3px, transparent 3px 6px)'
                            : colorOf(s.key),
                        opacity: faded ? 0.45 : 1,
                      }}
                      title={sliceTitle(w, s, by)}
                    />
                  ))}
                </div>
                {w.hit_cap && (
                  <div className="absolute inset-x-0 top-0 h-[3px] bg-rose-500" title="Hit the cap" />
                )}
              </div>
              <div className="text-[10px] text-zinc-500 tabular-nums whitespace-nowrap">
                {fmtWindowLabel(w.start_unix_ms)}
              </div>
              <div className="text-[10px] text-zinc-400 tabular-nums">
                {Math.round(w.attributed_pct)}%
              </div>
            </div>
          );
        })}
      </div>
      <div className="text-[10px] text-zinc-400 mt-3 leading-relaxed max-w-3xl">
        Faded bars are estimated only (no /usage readings covered them) or
        partial (collection began mid-window). A rose line marks a window that
        hit the cap. Grey folds every group outside the legend's top 8 into
        one "other" segment per window; hatched segments are unattributed.
      </div>
    </div>
  );
}

/** The single-window analogue of WindowStacks: current-window mode has
 *  exactly one window to show, and a lone narrow bar sitting in a chart
 *  sized for eight reads as a rendering mistake rather than a deliberate
 *  view. A full-width horizontal 100%-cap track fills the same space on
 *  purpose instead. Shares foldOtherSlices with WindowStacks so the two
 *  views can never disagree about what "other" means. */
function CurrentWindowBar({
  window: w,
  colorOf,
  isColored,
  by,
}: {
  window: AttrWindow;
  colorOf: (key: string) => string;
  isColored: (key: string) => boolean;
  by: GroupBy;
}) {
  const slices = foldOtherSlices(w.slices, isColored);
  const faded = w.partial || w.inferred;
  return (
    <div className="max-w-3xl">
      <div
        className="relative h-9 rounded-md bg-zinc-100 dark:bg-zinc-800 overflow-hidden"
        title={windowTitle(w)}
      >
        {/* Same 2px surface-gap convention as WindowStacks; this track
            fills left to right instead of bottom-up, so the rounded data
            end belongs to the first (biggest) segment instead of the last.
            inset-0 (not inset-y-0 left-0) is load-bearing: a flex container
            with no right edge has no width to resolve percentages against,
            so it shrinks to fit its content and every child's `${pct}%`
            resolves against that collapsed box instead of the track. The
            visible symptom was a bar reporting 88% while filling about 3%
            of the track: the segments were sized correctly relative to
            EACH OTHER, just against the wrong total width. */}
        <div className="absolute inset-0 flex gap-[2px]">
          {slices.map((s, i) => (
            <div
              key={s.key || 'unattributed'}
              className={`shrink-0 h-full ${i === 0 ? 'rounded-l-[7px]' : ''}`}
              style={{
                // The 0.4% floor keeps a sub-pixel slice from disappearing
                // outright; on a fixed-width track a slice under ~0.3% of
                // the cap otherwise rounds to 0px. It can push the drawn
                // total slightly past the true fill, but only by however
                // many slices got floored, and foldOtherSlices already caps
                // that count at the legend's top 8 plus one merged "other"
                // plus the unattributed sentinel - worst case, an ~4pp
                // overshoot on a track that's read at a glance, not
                // measured with a ruler. Losing several real contributors
                // to true-zero width would be the worse failure mode, so
                // the floor stays.
                width: `${Math.max(s.pct > 0 ? 0.4 : 0, s.pct)}%`,
                background:
                  s.key === UNATTRIBUTED
                    ? 'repeating-linear-gradient(45deg, var(--series-other) 0 3px, transparent 3px 6px)'
                    : colorOf(s.key),
                opacity: faded ? 0.45 : 1,
              }}
              title={sliceTitle(w, s, by)}
            />
          ))}
        </div>
        {w.hit_cap && (
          <div className="absolute inset-y-0 right-0 w-[3px] bg-rose-500" title="Hit the cap" />
        )}
      </div>
      <div className="flex items-center justify-between text-[10px] text-zinc-400 mt-1.5">
        <span>
          {fmtWindowLabel(w.start_unix_ms)} to now
          {faded && ' · estimated only'}
        </span>
        <span className="tabular-nums">{Math.round(w.attributed_pct)}% of the cap</span>
      </div>
      <div className="text-[10px] text-zinc-400 mt-3 leading-relaxed max-w-3xl">
        Grey folds every group outside the legend's top 8 into one "other"
        segment. Hatched segments are unattributed; a rose edge means this
        window hit the cap.
      </div>
    </div>
  );
}

function Legend({
  items,
  colorOf,
  by,
  foldedCount,
}: {
  items: AttrGroup[];
  colorOf: (key: string) => string;
  by: GroupBy;
  foldedCount: number;
}) {
  if (items.length === 0) return null;
  return (
    <div className="flex flex-wrap gap-x-4 gap-y-1.5 mt-4">
      {items.map((g) => (
        <div key={g.key} className="flex items-center gap-1.5 text-xs text-zinc-600 dark:text-zinc-400">
          <span
            className="size-2.5 rounded-sm shrink-0"
            style={{ backgroundColor: colorOf(g.key) }}
            aria-hidden
          />
          <span className="truncate max-w-[16rem]" title={g.key}>
            {displayKey(g, by)}
          </span>
        </div>
      ))}
      {foldedCount > 0 && (
        <div className="flex items-center gap-1.5 text-xs text-zinc-500">
          <span
            className="size-2.5 rounded-sm shrink-0"
            style={{ backgroundColor: 'var(--series-other)' }}
            aria-hidden
          />
          other ({foldedCount})
        </div>
      )}
    </div>
  );
}

function GroupTable({
  groups,
  colorOf,
  by,
  bucket,
}: {
  groups: AttrGroup[];
  colorOf: (key: string) => string;
  by: GroupBy;
  bucket: Bucket;
}) {
  const max = groups.reduce((m, g) => Math.max(m, g.pct), 0);
  return (
    <div className="overflow-x-auto rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900">
      <table className="w-full text-sm">
        <thead className="text-left text-xs uppercase tracking-wider text-zinc-500 border-b border-zinc-200 dark:border-zinc-800">
          <tr>
            <th className="px-3 py-2 font-medium">
              {by === 'project' ? 'Project' : by === 'cwd' ? 'Working dir' : 'Session'}
            </th>
            <th className="px-3 py-2 font-medium text-right">
              % of {bucket === 'week' ? 'week' : '5h'}
            </th>
            <th className="px-3 py-2 font-medium w-40" />
            <th className="px-3 py-2 font-medium text-right">Peak window</th>
            <th className="px-3 py-2 font-medium text-right">Est.</th>
            <th className="px-3 py-2 font-medium text-right">
              {by === 'session' ? 'Windows' : 'Sessions'}
            </th>
            <th className="px-3 py-2 font-medium text-right">Turns</th>
            <th className="px-3 py-2 font-medium text-right">Weighted tokens</th>
          </tr>
        </thead>
        <tbody>
          {groups.map((g) => {
            const unattributed = g.key === UNATTRIBUTED;
            const unknownCwd = by === 'cwd' && g.key === UNKNOWN_CWD;
            return (
              <tr
                key={g.key || 'unattributed'}
                className="border-b border-zinc-100 dark:border-zinc-800/60 hover:bg-zinc-50 dark:hover:bg-zinc-800/40"
              >
                <td className="px-3 py-2">
                  <div className="flex items-center gap-2">
                    <span
                      className="size-2.5 rounded-sm shrink-0"
                      style={
                        unattributed
                          ? {
                              background:
                                'repeating-linear-gradient(45deg, var(--series-other) 0 2px, transparent 2px 4px)',
                            }
                          : { backgroundColor: colorOf(g.key) }
                      }
                      aria-hidden
                    />
                    {by === 'session' && !unattributed ? (
                      <Link
                        to={`/sessions/${g.key}`}
                        className="font-mono text-xs hover:text-rose-500 truncate max-w-xs"
                        title={g.key}
                      >
                        {displayKey(g, by)}
                      </Link>
                    ) : (
                      <span
                        className={`font-mono text-xs truncate max-w-md ${unattributed || unknownCwd ? 'text-zinc-500 italic' : ''}`}
                        title={unattributed ? 'unattributed' : unknownCwd ? 'cwd not captured for these sessions' : g.key}
                      >
                        {displayKey(g, by)}
                      </span>
                    )}
                  </div>
                  {(by === 'session' || by === 'cwd') && g.project && (
                    <div className="text-[10px] text-zinc-500 ml-4.5 truncate max-w-xs" title={g.project}>
                      {stripProject(g.project)}
                    </div>
                  )}
                </td>
                <td className="px-3 py-2 text-right tabular-nums font-medium">{fmtPct(g.pct)}%</td>
                <td className="px-3 py-2">
                  <div className="h-1.5 rounded-full bg-zinc-100 dark:bg-zinc-800 overflow-hidden">
                    <div
                      className="h-full rounded-full"
                      style={{
                        width: `${max > 0 ? (g.pct / max) * 100 : 0}%`,
                        backgroundColor: unattributed ? 'var(--series-other)' : colorOf(g.key),
                      }}
                    />
                  </div>
                </td>
                <td className="px-3 py-2 text-right tabular-nums text-zinc-500">
                  {fmtPct(g.peak_pct)}%
                </td>
                <td className="px-3 py-2 text-right tabular-nums text-zinc-500">
                  {g.estimated_pct > 0 ? `${fmtPct(g.estimated_pct)}%` : '—'}
                </td>
                <td className="px-3 py-2 text-right tabular-nums text-zinc-500">
                  {by === 'session' ? fmtCount(g.windows) : fmtCount(g.sessions)}
                </td>
                <td className="px-3 py-2 text-right tabular-nums text-zinc-500">
                  {g.turn_count > 0 ? fmtCount(g.turn_count) : '—'}
                </td>
                <td className="px-3 py-2 text-right tabular-nums text-zinc-500">
                  {g.cw_tokens > 0 ? fmtNumber(g.cw_tokens) : '—'}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
      {groups.length === 0 && (
        <div className="text-center text-sm text-zinc-500 py-8">Nothing attributed yet.</div>
      )}
    </div>
  );
}

function Segmented({
  value,
  onChange,
  options,
}: {
  value: string;
  onChange: (v: string) => void;
  options: { value: string; label: string }[];
}) {
  return (
    <div className="inline-flex rounded-md border border-zinc-300 dark:border-zinc-700 overflow-hidden">
      {options.map((o) => (
        <button
          key={o.value}
          onClick={() => onChange(o.value)}
          className={[
            'px-3 py-1.5 text-xs transition',
            value === o.value
              ? 'bg-zinc-900 text-white dark:bg-zinc-100 dark:text-zinc-900'
              : 'bg-white dark:bg-zinc-900 text-zinc-600 dark:text-zinc-400 hover:bg-zinc-50 dark:hover:bg-zinc-800',
          ].join(' ')}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

function Tile({
  label,
  value,
  hint,
  accent,
  sub,
}: {
  label: string;
  value: string;
  hint?: string;
  accent?: string;
  // sub marks a tile as qualifying another one (Measured/Estimated qualify
  // the headline total) rather than standing as a peer stat of its own: a
  // smaller, lighter number is the visual half of telling the two kinds of
  // tile apart, alongside the "of that" / "of the limit" wording carried in
  // value and hint. Without some visual difference, four identically
  // rendered cards read as four numbers on the same scale even once the
  // copy says otherwise.
  sub?: boolean;
}) {
  return (
    <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-4">
      <div className="text-xs uppercase tracking-wider text-zinc-500">{label}</div>
      <div
        className={`${sub ? 'text-lg font-medium text-zinc-700 dark:text-zinc-300' : 'text-2xl font-semibold'} tabular-nums mt-1 ${accent || ''}`}
      >
        {value}
      </div>
      {hint && <div className="text-[11px] text-zinc-500 mt-1 leading-snug">{hint}</div>}
    </div>
  );
}

function Section({
  title,
  subtitle,
  children,
}: {
  title: string;
  subtitle?: string;
  children: React.ReactNode;
}) {
  return (
    <div className="mb-8">
      <h2 className="text-lg font-semibold tracking-tight">{title}</h2>
      {subtitle && <p className="text-xs text-zinc-500 mb-3 max-w-3xl">{subtitle}</p>}
      {children}
    </div>
  );
}

function Empty({ hint }: { hint: string }) {
  return (
    <div className="rounded-lg border border-dashed border-zinc-300 dark:border-zinc-700 p-8 text-center text-sm text-zinc-500 max-w-3xl">
      {hint}
    </div>
  );
}

/** The noun for a grouping mode, used in section titles and subtitles. */
function byNoun(by: GroupBy): string {
  if (by === 'cwd') return 'working directory';
  if (by === 'session') return 'session';
  return 'project';
}

function displayKey(g: AttrGroup, by: GroupBy): string {
  if (g.key === UNATTRIBUTED) return 'unattributed';
  if (by === 'cwd') return g.key === UNKNOWN_CWD ? 'unknown directory' : stripCwd(g.key);
  return by === 'project' ? stripProject(g.key) : g.key.slice(0, 8);
}

/** Projects arrive as the sanitized absolute path; show the tail of it. */
function stripProject(p: string): string {
  return p.replace(/^-?home-[^-]+-dev-/, '').replace(/^-+/, '');
}

/** Cwds arrive as a real absolute path; shorten the home directory the way
 *  a shell prompt would, so a long path doesn't dominate the table. */
function stripCwd(p: string): string {
  return p.replace(/^\/home\/[^/]+/, '~').replace(/^\/Users\/[^/]+/, '~');
}

function windowTitle(w: AttrWindow): string {
  const parts = [
    `${fmtWindowLabel(w.start_unix_ms)}: ${fmtPct(w.attributed_pct)}% attributed`,
  ];
  if (w.measured_pct > 0) parts.push(`${fmtPct(w.measured_pct)}% measured`);
  if (w.inferred) parts.push('estimated only');
  if (w.partial) parts.push('partial');
  if (w.in_progress) parts.push('in progress');
  if (w.hit_cap) parts.push('hit the cap');
  return parts.join(' · ');
}

function sliceTitle(w: AttrWindow, s: RenderSlice, by: GroupBy): string {
  const name =
    s.key === UNATTRIBUTED
      ? 'unattributed'
      : s.key === SERVER_OTHER
        ? // foldedCount is how many groups foldOtherSlices summed into this
          // one segment - the count the merge would otherwise have thrown
          // away, and the reason a single grey block still says something.
          `other (${s.foldedCount ?? 0} group${s.foldedCount === 1 ? '' : 's'})`
        : by === 'project'
          ? stripProject(s.label)
          : by === 'cwd'
            ? s.key === UNKNOWN_CWD
              ? 'unknown directory'
              : stripCwd(s.label)
            : s.label.slice(0, 8);
  const bits = [`${name}: ${fmtPct(s.pct)}%`];
  if (s.turn_count > 0) bits.push(`${fmtCount(s.turn_count)} turns`);
  if (s.estimated_pct > 0) bits.push(`${fmtPct(s.estimated_pct)}% estimated`);
  return `${fmtWindowLabel(w.start_unix_ms)} · ${bits.join(' · ')}`;
}

function fmtWindowLabel(ms: number): string {
  const d = new Date(ms);
  return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
}

function fmtPct(n: number): string {
  if (n === 0) return '0';
  if (n < 0.1) return n.toFixed(2);
  if (n < 10) return n.toFixed(1);
  return String(Math.round(n));
}

function pctOf(part: number, whole: number): string {
  if (whole <= 0) return '0';
  return String(Math.round((part / whole) * 100));
}
