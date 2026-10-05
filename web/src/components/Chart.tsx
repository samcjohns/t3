import { useEffect, useRef, useState } from 'preact/hooks';
import type { Candle, Interval, ValueCandle } from '../api';
import { axisMoney, money, shares, shortTime } from '../format';

const HEIGHT = 300;
const AXIS = 64; // right-hand price axis
const FOOT = 20; // time labels
const VOLUME = 0.18; // share of the plot given to volume bars

interface Props {
	/** Price candles get volume bars; value candles have no volume. */
	candles: (Candle | ValueCandle)[] | null;
	interval: Interval;
	empty?: string;
}

/** Candlesticks, with volume when the candles have it, sized to its container. */
export function Chart({ candles, interval, empty = 'No trades in this period yet.' }: Props) {
	const box = useRef<HTMLDivElement>(null);
	const [width, setWidth] = useState(0);
	const [hover, setHover] = useState<number | null>(null);

	useEffect(() => {
		const el = box.current!;
		const ro = new ResizeObserver(([e]) => setWidth(Math.floor(e.contentRect.width)));
		ro.observe(el);
		return () => ro.disconnect();
	}, []);

	if (!candles || !width) return <div class="chart" ref={box} />;
	if (!candles.length) {
		return (
			<div class="chart chart-empty" ref={box}>
				{empty}
			</div>
		);
	}

	const plotW = width - AXIS;
	const plotH = HEIGHT - FOOT;
	const hasVolume = 'volume' in candles[0];
	const priceH = plotH * (hasVolume ? 1 - VOLUME : 1) - 8;
	let lo = Math.min(...candles.map((c) => c.low));
	let hi = Math.max(...candles.map((c) => c.high));
	const pad = Math.max((hi - lo) * 0.08, hi * 0.004, 2);
	lo -= pad;
	hi += pad;
	const volume = (c: Candle | ValueCandle) => ('volume' in c ? c.volume : 0);
	const maxVol = Math.max(...candles.map(volume), 1);

	const step = plotW / candles.length;
	const body = Math.max(1, Math.min(14, step * 0.7));
	const x = (i: number) => i * step + step / 2;
	const y = (p: number) => 4 + ((hi - p) / (hi - lo)) * priceH;
	const volY = (v: number) => plotH - (v / maxVol) * plotH * VOLUME;

	const ticks = Array.from({ length: 5 }, (_, i) => lo + ((hi - lo) * (i + 0.5)) / 5);
	const label = axisMoney(lo, hi, (hi - lo) / 5);
	const labelEvery = Math.max(1, Math.ceil(90 / step));
	const withDate = interval === '1d' || interval === '1h';
	const last = candles[candles.length - 1];
	const shown = candles[hover ?? candles.length - 1];

	const onMove = (e: PointerEvent) => {
		const rect = (e.currentTarget as SVGElement).getBoundingClientRect();
		const i = Math.floor((e.clientX - rect.left) / step);
		setHover(i >= 0 && i < candles.length ? i : null);
	};

	return (
		<div class="chart" ref={box}>
			<div class="chart-readout">
				<span>{shortTime(shown.start, withDate)}</span>
				<span>O {money(shown.open)}</span>
				<span>H {money(shown.high)}</span>
				<span>L {money(shown.low)}</span>
				<span>C {money(shown.close)}</span>
				{'volume' in shown && <span>Vol {shares(shown.volume)}</span>}
			</div>
			<svg width={width} height={HEIGHT} onPointerMove={onMove} onPointerLeave={() => setHover(null)} role="img" aria-label="Price chart">
				{ticks.map((p) => (
					<g key={p}>
						<line class="grid" x1={0} x2={plotW} y1={y(p)} y2={y(p)} />
						<text class="axis" x={plotW + 6} y={y(p) + 4}>
							{label(p)}
						</text>
					</g>
				))}
				{candles.map((c, i) =>
					i % labelEvery === 0 ? (
						<text key={c.start} class="axis" x={x(i)} y={HEIGHT - 4} text-anchor="middle">
							{shortTime(c.start, withDate)}
						</text>
					) : null,
				)}
				{candles.map((c, i) => {
					const cls = c.close > c.open ? 'up' : c.close < c.open ? 'down' : 'flat';
					const top = y(Math.max(c.open, c.close));
					return (
						<g key={c.start} class={`candle ${cls}`} opacity={hover === null || hover === i ? 1 : 0.55}>
							{hasVolume && <rect class="vol" x={x(i) - body / 2} y={volY(volume(c))} width={body} height={plotH - volY(volume(c))} />}
							<line x1={x(i)} x2={x(i)} y1={y(c.high)} y2={y(c.low)} />
							<rect x={x(i) - body / 2} y={top} width={body} height={Math.max(1, y(Math.min(c.open, c.close)) - top)} />
						</g>
					);
				})}
				<line class="last" x1={0} x2={plotW} y1={y(last.close)} y2={y(last.close)} />
				<rect class="last-tag" x={plotW + 1} y={y(last.close) - 9} width={AXIS - 2} height={18} rx={3} />
				<text class="last-label" x={plotW + 6} y={y(last.close) + 4}>
					{label(last.close)}
				</text>
				{hover !== null && <line class="crosshair" x1={x(hover)} x2={x(hover)} y1={0} y2={plotH} />}
			</svg>
		</div>
	);
}
