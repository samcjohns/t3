import { useState } from 'preact/hooks';
import { money, share } from '../format';

export interface Slice {
	key: string;
	label: string;
	sub?: string;
	value: number;
	/** A CSS colour, normally a --series-N token. */
	color: string;
}

const SIZE = 200;
const THICK = 26;
const R = (SIZE - THICK - 8) / 2; // room for a hovered slice to grow
const CIRC = 2 * Math.PI * R;
const GAP = 2; // surface gap between slices

/** A donut of how value is split, with a legend that doubles as its labels. */
export function Allocation({ slices }: { slices: Slice[] }) {
	const [hover, setHover] = useState<string | null>(null);
	const total = slices.reduce((s, x) => s + Math.max(0, x.value), 0);
	if (!total) return <p class="empty">Nothing to show yet.</p>;

	const shown = slices.filter((s) => s.value > 0);
	const focus = shown.find((s) => s.key === hover);
	let offset = 0;

	return (
		<div class="alloc">
			<div class="donut">
				<svg width={SIZE} height={SIZE} viewBox={`0 0 ${SIZE} ${SIZE}`} role="img" aria-label="Allocation by value">
					<g transform={`rotate(-90 ${SIZE / 2} ${SIZE / 2})`}>
						{shown.map((s) => {
							const len = (s.value / total) * CIRC;
							const drawn = shown.length > 1 ? Math.max(len - GAP, 0.5) : len;
							const arc = (
								<circle
									key={s.key}
									class={hover && hover !== s.key ? 'slice dim' : 'slice'}
									cx={SIZE / 2}
									cy={SIZE / 2}
									r={R}
									stroke={s.color}
									stroke-width={hover === s.key ? THICK + 6 : THICK}
									stroke-dasharray={`${drawn} ${CIRC - drawn}`}
									stroke-dashoffset={-offset}
									onPointerEnter={() => setHover(s.key)}
									onPointerLeave={() => setHover(null)}
								>
									<title>
										{s.label}: {share(s.value, total)} · {money(s.value)}
									</title>
								</circle>
							);
							offset += len;
							return arc;
						})}
					</g>
				</svg>
				<div class="donut-center" aria-hidden="true">
					<span>{focus ? focus.label : 'Total'}</span>
					<b>{focus ? share(focus.value, total) : money(total)}</b>
					{focus && <small>{money(focus.value)}</small>}
				</div>
			</div>
			<ul class="legend">
				{shown.map((s) => (
					<li
						key={s.key}
						class={hover === s.key ? 'on' : ''}
						onPointerEnter={() => setHover(s.key)}
						onPointerLeave={() => setHover(null)}
					>
						<i style={{ background: s.color }} />
						<span class="legend-name">
							<b>{s.label}</b>
							{s.sub && <small>{s.sub}</small>}
						</span>
						<span class="legend-pct">{share(s.value, total)}</span>
						<span class="legend-val">{money(s.value)}</span>
					</li>
				))}
			</ul>
		</div>
	);
}
