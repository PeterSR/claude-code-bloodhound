import { useEffect, useRef } from 'react';
import uPlot, { type Options as UPlotOptions } from 'uplot';
import 'uplot/dist/uPlot.min.css';

type Series = {
  label: string;
  values: (number | null)[];
  color: string;
  width?: number;
  /** uPlot dash pattern, e.g. [4, 4] for evenly dashed. */
  dash?: number[];
  /** Bridge nulls in this series. Useful for projection lines whose
   *  endpoints are sparse but logically continuous. Default: false. */
  spanGaps?: boolean;
  /** Render point markers on this series. Default: false (cleaner on
   *  dense sampling — pages that need clickable dots can opt in). */
  showPoints?: boolean;
};

/** Vertical reference (e.g. "now" or limit ETA). */
type VLine = {
  /** Unix seconds. */
  x: number;
  color: string;
  dash?: number[];
  label?: string;
  width?: number;
};

/** Horizontal reference (e.g. current pct or 100% cap). */
type HLine = {
  value: number;
  color: string;
  dash?: number[];
  label?: string;
};

/** Shaded x-range (e.g. saturated region). */
type XBand = {
  /** Unix seconds. */
  from: number;
  to: number;
  /** Any CSS color, including rgba for translucent fills. */
  color: string;
};

type Props = {
  /** Unix-second timestamps. */
  ts: number[];
  series: Series[];
  height?: number;
  /** Y axis label / suffix in tooltip. */
  ySuffix?: string;
  /** Force min/max on the Y axis. Useful for percentage charts. */
  yRange?: [number, number];
  /** Custom Y tick formatter. Defaults to compact (k/M/B) + ySuffix. */
  yFormatter?: (v: number) => string;
  vLines?: VLine[];
  hLines?: HLine[];
  xBands?: XBand[];
};

/** Compact tick labels: 1500 → "1.5k", 2_000_000 → "2M". Keeps small
 *  values verbatim so percentage axes still read "0", "20", "100". */
function formatCompact(v: number, suffix: string): string {
  const abs = Math.abs(v);
  let s: string;
  if (abs >= 1e9) s = trimZero((v / 1e9).toFixed(1)) + 'B';
  else if (abs >= 1e6) s = trimZero((v / 1e6).toFixed(1)) + 'M';
  else if (abs >= 1e3) s = trimZero((v / 1e3).toFixed(1)) + 'k';
  else s = String(v);
  return s + suffix;
}
function trimZero(s: string): string {
  return s.endsWith('.0') ? s.slice(0, -2) : s;
}

/**
 * Spark wraps uPlot in a React component. On data refresh we call
 * `setData()` in place rather than destroying the chart — this prevents
 * the layout flash that polling caused before. The chart only rebuilds
 * when the shape (series count/labels) or height actually changes.
 * Annotations are read through a ref so the live-update path picks them
 * up too.
 */
export default function Spark({
  ts,
  series,
  height = 220,
  ySuffix = '',
  yRange,
  yFormatter,
  vLines,
  hLines,
  xBands,
}: Props) {
  const containerRef = useRef<HTMLDivElement>(null);
  const plotRef = useRef<uPlot | null>(null);
  const shapeKeyRef = useRef<string>('');
  const annoRef = useRef<{ vLines?: VLine[]; hLines?: HLine[]; xBands?: XBand[] }>({});

  useEffect(() => {
    if (!containerRef.current) return;

    const data: uPlot.AlignedData = [ts, ...series.map((s) => s.values)] as uPlot.AlignedData;
    const shapeKey = `${series.length}|${series.map((s) => `${s.label}:${s.color}`).join(',')}|${height}|${ySuffix}|${yRange?.join(',') ?? ''}`;

    // Fast path: same shape and chart already exists — push new data and
    // refresh annotation refs. uPlot redraws and our draw hook will read
    // the latest annotations from annoRef.
    annoRef.current = { vLines, hLines, xBands };
    if (plotRef.current && shapeKeyRef.current === shapeKey) {
      plotRef.current.setData(data);
      return;
    }

    if (plotRef.current) {
      plotRef.current.destroy();
      plotRef.current = null;
    }
    shapeKeyRef.current = shapeKey;

    const isDark = document.documentElement.classList.contains('dark');
    const grid = isDark ? 'rgba(255,255,255,0.05)' : 'rgba(0,0,0,0.06)';
    const axisColor = isDark ? '#a1a1aa' : '#71717a';

    const drawAnnotations = (u: uPlot) => {
      const { vLines: vs, hLines: hs, xBands: bs } = annoRef.current;
      const ctx = u.ctx;
      const { left, top, width, height: plotH } = u.bbox;
      ctx.save();
      ctx.beginPath();
      ctx.rect(left, top, width, plotH);
      ctx.clip();

      if (bs?.length) {
        for (const b of bs) {
          const x0 = u.valToPos(b.from, 'x', true);
          const x1 = u.valToPos(b.to, 'x', true);
          if (!Number.isFinite(x0) || !Number.isFinite(x1)) continue;
          ctx.fillStyle = b.color;
          ctx.fillRect(Math.min(x0, x1), top, Math.abs(x1 - x0), plotH);
        }
      }

      const drawDashed = (dash?: number[]) => {
        ctx.setLineDash(dash ?? []);
      };

      if (hs?.length) {
        for (const h of hs) {
          const y = u.valToPos(h.value, 'y', true);
          if (!Number.isFinite(y)) continue;
          ctx.strokeStyle = h.color;
          ctx.lineWidth = 1;
          drawDashed(h.dash);
          ctx.beginPath();
          ctx.moveTo(left, y);
          ctx.lineTo(left + width, y);
          ctx.stroke();
          if (h.label) {
            ctx.setLineDash([]);
            ctx.fillStyle = h.color;
            ctx.font = '10px ui-sans-serif, system-ui, sans-serif';
            ctx.textAlign = 'right';
            ctx.textBaseline = 'bottom';
            ctx.fillText(h.label, left + width - 4, y - 2);
          }
        }
      }

      if (vs?.length) {
        for (const v of vs) {
          const x = u.valToPos(v.x, 'x', true);
          if (!Number.isFinite(x)) continue;
          ctx.strokeStyle = v.color;
          ctx.lineWidth = v.width ?? 1;
          drawDashed(v.dash);
          ctx.beginPath();
          ctx.moveTo(x, top);
          ctx.lineTo(x, top + plotH);
          ctx.stroke();
          if (v.label) {
            ctx.setLineDash([]);
            ctx.fillStyle = v.color;
            ctx.font = '10px ui-sans-serif, system-ui, sans-serif';
            ctx.textAlign = 'left';
            ctx.textBaseline = 'top';
            ctx.fillText(v.label, x + 3, top + 2);
          }
        }
      }

      ctx.setLineDash([]);
      ctx.restore();
    };

    const opts: UPlotOptions = {
      width: containerRef.current.clientWidth,
      height,
      cursor: { drag: { x: true, y: false }, points: { size: 4 } },
      legend: { show: true },
      scales: {
        y: yRange ? { range: yRange } : {},
      },
      axes: [
        { stroke: axisColor, grid: { stroke: grid } },
        {
          stroke: axisColor,
          grid: { stroke: grid },
          size: 56,
          values: (_self, ticks) => {
            const fmt = yFormatter ?? ((v: number) => formatCompact(v, ySuffix));
            return ticks.map(fmt);
          },
        },
      ],
      series: [
        { label: 'time' },
        ...series.map((s) => ({
          label: s.label,
          stroke: s.color,
          width: s.width ?? 1.5,
          dash: s.dash,
          spanGaps: s.spanGaps ?? false,
          points: { show: s.showPoints ?? false },
        })),
      ],
      hooks: {
        draw: [drawAnnotations],
      },
    };

    plotRef.current = new uPlot(opts, data, containerRef.current);

    const ro = new ResizeObserver((entries) => {
      for (const entry of entries) {
        plotRef.current?.setSize({ width: entry.contentRect.width, height });
      }
    });
    ro.observe(containerRef.current);
    return () => {
      ro.disconnect();
      plotRef.current?.destroy();
      plotRef.current = null;
      shapeKeyRef.current = '';
    };
  }, [ts, series, height, ySuffix, yRange, yFormatter, vLines, hLines, xBands]);

  // Reserve the chart's vertical space so the surrounding layout doesn't
  // jump during the brief moment between destroy() and new uPlot().
  return <div ref={containerRef} className="w-full" style={{ minHeight: height }} />;
}
