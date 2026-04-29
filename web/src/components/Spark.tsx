import { useEffect, useRef } from 'react';
import uPlot, { type Options as UPlotOptions } from 'uplot';
import 'uplot/dist/uPlot.min.css';

type Series = {
  label: string;
  values: (number | null)[];
  color: string;
  width?: number;
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
};

/**
 * Spark wraps uPlot in a React component. Re-creates the chart whenever
 * the data shape changes; on resize we just reflow the existing instance.
 *
 * Theme: uPlot's CSS doesn't follow our Tailwind dark mode, so we read
 * the document's resolved color and pass it through to the axes/grid.
 * That keeps the chart legible on both themes.
 */
export default function Spark({ ts, series, height = 220, ySuffix = '', yRange }: Props) {
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
    const grid = isDark ? '#27272a' : '#e4e4e7';
    const axisColor = isDark ? '#a1a1aa' : '#71717a';

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
          values: (_self, ticks) => ticks.map((v) => v + ySuffix),
        },
      ],
      series: [
        { label: 'time' },
        ...series.map((s) => ({
          label: s.label,
          stroke: s.color,
          width: s.width ?? 1.5,
          spanGaps: false,
        })),
      ],
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
  }, [ts, series, height, ySuffix, yRange]);

  return <div ref={containerRef} className="w-full" />;
}
