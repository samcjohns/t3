import type { Price } from '../api';
import { money, percent, trendClass } from '../format';

interface Props {
	prices: Price[];
	selected: string;
	onSelect: (symbol: string) => void;
}

export function Markets({ prices, selected, onSelect }: Props) {
	return (
		<nav class="markets" aria-label="Markets">
			<h2 class="panel-title">Markets</h2>
			<ul>
				{prices.map((p) => (
					<li key={p.symbol}>
						<button class={p.symbol === selected ? 'on' : ''} aria-current={p.symbol === selected} onClick={() => onSelect(p.symbol)}>
							<span class="sym">{p.symbol}</span>
							<span class="name">{p.name}</span>
							{/* Keyed by price so a change replays the flash. */}
							<span key={p.price} class={`px flash ${trendClass(p.change)}`}>
								{money(p.price)}
							</span>
							<span class={`chg ${trendClass(p.change)}`}>{percent(p.change, p.previous_close)}</span>
						</button>
					</li>
				))}
			</ul>
		</nav>
	);
}
