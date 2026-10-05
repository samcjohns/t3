import { useEffect, useRef, useState } from 'preact/hooks';
import type { Interval, ValueCandle } from '../api';
import { axisMoney, money, percent, shortTime, signedMoney, trendClass } from '../format';

const HEIGHT = 300;
const AXIS = 64; // right-hand value axis
const FOOT = 20; // time labels

const INTERVAL_MS: Record<Interval, number> = { '1m': 60e3, '5m': 300e3, '15m': 900e3, '1h': 3600e3, '1d': 86400e3 };

interface Props {
	candles: ValueCandle[];
	interval: Interval;
}

/**
 * Account value as an area line, sized to its container. The line starts at
 * the first period's open and then follows each period's close, and is
 * coloured by whether the window ended up or down.
 */
export function ValueChart({ candles, interval }: Props) {
	const box = useRef<HTMLDivElement>(null);
	const [width, setWidth] = useState(0);
	const [hover, setHover] = useState<number | null>(null);

	useEffect(() => {
		const el = box.current!;
		const ro = new ResizeObserver(([e]) => setWidth(Math.floor(e.contentRect.width)));
		ro.observe(el);
		return () => ro.disconnect();
	}, []);

	if (!width || !candles.length) return <div class="chart" ref={box} />;

	const ms = INTERVAL_MS[interval];
	// Point 0 is the window's opening value; point i is the close of period i-1.
	const points = [
		{ at: Date.parse(candles[0].start), value: candles[0].open },
		...candles.map((c) => ({ at: Date.parse(c.start) + ms, value: c.close })),
	];
	const base = points[0].value;
	const trend = trendClass(points[points.length - 1].value - base) || 'flat';

	const plotW = width - AXIS;
	const plotH = HEIGHT - FOOT;
	let lo = Math.min(...candles.map((c) => c.low));
	let hi = Math.max(...candles.map((c) => c.high));
	const pad = Math.max((hi - lo) * 0.12, hi * 0.002, 100);
	lo -= pad;
	hi += pad;

	const step = plotW / (points.length - 1);
	const x = (i: number) => i * step;
	const y = (v: number) => 6 + ((hi - v) / (hi - lo)) * (plotH - 12);
	const line = points.map((p, i) => `${i ? 'L' : 'M'}${x(i).toFixed(1)},${y(p.value).toFixed(1)}`).join('');
	const area = `${line}L${x(points.length - 1)},${plotH}L0,${plotH}Z`;

	const ticks = Array.from({ length: 5 }, (_, i) => lo + ((hi - lo) * (i + 0.5)) / 5);
	const axisLabel = axisMoney(lo, hi, (hi - lo) / 5);
	const labelEvery = Math.max(1, Math.ceil(90 / step));
	const withDate = interval === '1d' || interval === '1h';
	const shown = points[hover ?? points.length - 1];
	const change = shown.value - base;
	const isNow = shown.at > Date.now();

	const onMove = (e: PointerEvent) => {
		const rect = (e.currentTarget as SVGElement).getBoundingClientRect();
		const i = Math.round((e.clientX - rect.left) / step);
		setHover(i >= 0 && i < points.length && e.clientX - rect.left <= plotW ? i : null);
	};

	return (
		<div class="chart" ref={box}>
			<div class="chart-readout">
				<span>{isNow ? 'Now' : shortTime(new Date(shown.at).toISOString(), withDate)}</span>
				<span class="readout-strong">{money(shown.value)}</span>
				<span class={trendClass(change)}>
					{signedMoney(change)} ({percent(change, base)})
				</span>
			</div>
			<svg
				class={`value-chart ${trend}`}
				width={width}
				height={HEIGHT}
				onPointerMove={onMove}
				onPointerLeave={() => setHover(null)}
				role="img"
				aria-label={`Account value from ${money(base)} to ${money(points[points.length - 1].value)}`}
			>
				<defs>
					<linearGradient id="value-fill" x1="0" x2="0" y1="0" y2="1">
						<stop offset="0" class="fill-top" />
						<stop offset="1" class="fill-bottom" />
					</linearGradient>
				</defs>
				{ticks.map((v) => (
					<g key={v}>
						<line class="grid" x1={0} x2={plotW} y1={y(v)} y2={y(v)} />
						<text class="axis" x={plotW + 6} y={y(v) + 4}>
							{axisLabel(v)}
						</text>
					</g>
				))}
				{points.map((p, i) =>
					i % labelEvery === 0 && x(i) > 20 && x(i) < plotW - 20 ? (
						<text key={p.at} class="axis" x={x(i)} y={HEIGHT - 4} text-anchor="middle">
							{shortTime(new Date(p.at).toISOString(), withDate)}
						</text>
					) : null,
				)}
				<line class="baseline" x1={0} x2={plotW} y1={y(base)} y2={y(base)} />
				<path class="area" d={area} fill="url(#value-fill)" />
				<path class="line" d={line} />
				{hover !== null && (
					<>
						<line class="crosshair" x1={x(hover)} x2={x(hover)} y1={0} y2={plotH} />
						<circle class="dot" cx={x(hover)} cy={y(shown.value)} r={5} />
					</>
				)}
			</svg>
		</div>
	);
}
