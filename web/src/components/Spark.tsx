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
 * Spark wraps uPlot in a React component. Re-creates the chart whenever
 * the data shape changes; on resize we just reflow the existing instance.
 *
 * Theme: uPlot's CSS doesn't follow our Tailwind dark mode, so we read
 * the document's resolved color and pass it through to the axes/grid.
 * That keeps the chart legible on both themes.
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

  useEffect(() => {
    if (!containerRef.current) return;
    if (plotRef.current) {
      plotRef.current.destroy();
      plotRef.current = null;
    }

    const data: uPlot.AlignedData = [ts, ...series.map((s) => s.values)] as uPlot.AlignedData;

    const isDark = document.documentElement.classList.contains('dark');
    // Subtle grid: at default zinc-800 (#27272a) it overpowered the data
    // strokes on the dark cards. rgba avoids that on either theme.
    const grid = isDark ? 'rgba(255,255,255,0.05)' : 'rgba(0,0,0,0.06)';
    const axisColor = isDark ? '#a1a1aa' : '#71717a';

    // Annotation hooks: x-bands first (background), then vlines and hlines
    // on top of the series. uPlot exposes `valToPos` to convert data
    // coordinates to canvas pixels — the chart is already drawn at this
    // point in the `draw` hook.
    const drawAnnotations = (u: uPlot) => {
      const ctx = u.ctx;
      const { left, top, width, height: plotH } = u.bbox;
      ctx.save();
      ctx.beginPath();
      ctx.rect(left, top, width, plotH);
      ctx.clip();

      if (xBands?.length) {
        for (const b of xBands) {
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

      if (hLines?.length) {
        for (const h of hLines) {
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

      if (vLines?.length) {
        for (const v of vLines) {
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
    };
  }, [ts, series, height, ySuffix, yRange, yFormatter, vLines, hLines, xBands]);

  return <div ref={containerRef} className="w-full" />;
}
