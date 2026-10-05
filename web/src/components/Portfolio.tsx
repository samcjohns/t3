import { useEffect, useMemo, useState } from 'preact/hooks';
import type { Api, Interval, Portfolio as PortfolioData, Price, ValueCandle } from '../api';
import { money, percent, share, shares, shortTime, signedMoney, trendClass } from '../format';
import { Allocation, type Slice } from './Allocation';
import { Chart } from './Chart';
import { ValueChart } from './ValueChart';

const INTERVALS: Interval[] = ['1m', '5m', '15m', '1h', '1d'];
/** Categorical slots, in fixed order. A ninth holding folds into "Other". */
const SERIES = 8;

interface Props {
	api: Api;
	portfolio: PortfolioData;
	prices: Map<string, Price>;
	/** Listing order, which fixes each symbol's colour. */
	symbols: string[];
	tick: number;
	onSelect: (symbol: string) => void;
}

export function Portfolio({ api, portfolio, prices, symbols, tick, onSelect }: Props) {
	const [interval, setCandleInterval] = useState<Interval>('1m');
	const [style, setStyle] = useState<'line' | 'candles'>('line');
	const [history, setHistory] = useState<ValueCandle[] | null>(null);
	const [returns, setReturns] = useState<'total' | 'today'>('total');

	useEffect(() => {
		let live = true;
		api.history(interval).then((h) => live && setHistory(h), () => {});
		return () => {
			live = false;
		};
	}, [api, interval, tick]);
	useEffect(() => setHistory(null), [interval]);

	const holdings = useMemo(() => [...portfolio.positions].sort((a, b) => b.value - a.value), [portfolio]);

	// A symbol keeps its colour while the set of holdings is unchanged: colours
	// follow listing order, not value, so they never swap as prices move.
	const colors = useMemo(() => {
		const held = holdings.filter((p) => p.value > 0);
		const inSlots = held.length <= SERIES ? held : held.slice(0, SERIES - 1);
		const ordered = inSlots.map((p) => p.symbol).sort((a, b) => symbols.indexOf(a) - symbols.indexOf(b));
		return new Map(ordered.map((s, i) => [s, `var(--series-${i + 1})`]));
	}, [holdings, symbols]);

	const slices = useMemo(() => {
		const out: Slice[] = [];
		let other = 0;
		for (const p of holdings) {
			const color = colors.get(p.symbol);
			if (color) out.push({ key: p.symbol, label: p.symbol, sub: prices.get(p.symbol)?.name, value: p.value, color });
			else other += p.value;
		}
		if (other > 0) out.push({ key: 'other', label: 'Other', value: other, color: 'var(--other)' });
		out.push({ key: 'cash', label: 'Cash', sub: 'USD', value: portfolio.cash, color: 'var(--cash)' });
		return out.sort((a, b) => b.value - a.value);
	}, [holdings, colors, prices, portfolio.cash]);

	// Return on the holdings whose cost is known.
	const priced = holdings.filter((p) => p.cost_basis !== null);
	const cost = priced.reduce((s, p) => s + (p.cost_basis ?? 0), 0);
	const gain = priced.reduce((s, p) => s + p.value - (p.cost_basis ?? 0), 0);
	const today = holdings.reduce((s, p) => s + (prices.get(p.symbol)?.change ?? 0) * p.quantity, 0);

	const first = history?.[0];
	const windowChange = first ? portfolio.total_value - first.open : 0;
	const withDate = interval === '1d' || interval === '1h';

	return (
		<main class="folio">
			<section class="panel folio-value">
				<header class="folio-head">
					<div>
						<h1 class="panel-title">Portfolio value</h1>
						<p class="hero">{money(portfolio.total_value)}</p>
						{first && (
							<p class={`hero-change ${trendClass(windowChange)}`}>
								{signedMoney(windowChange)} ({percent(windowChange, first.open)})
								<span class="muted"> since {shortTime(first.start, withDate)}</span>
							</p>
						)}
					</div>
					<dl class="folio-stats">
						<div>
							<dt>Total return</dt>
							<dd class={trendClass(gain)}>
								{priced.length ? `${signedMoney(gain)} (${percent(gain, cost)})` : '—'}
							</dd>
						</div>
						<div>
							<dt>Today</dt>
							<dd class={trendClass(today)}>{signedMoney(today)}</dd>
						</div>
						<div>
							<dt>Invested</dt>
							<dd>{priced.length ? money(cost) : '—'}</dd>
						</div>
					</dl>
				</header>

				<div class="chart-controls">
					<div class="segmented small" role="radiogroup" aria-label="Candle interval">
						{INTERVALS.map((i) => (
							<button key={i} role="radio" aria-checked={interval === i} class={interval === i ? 'on' : ''} onClick={() => setCandleInterval(i)}>
								{i}
							</button>
						))}
					</div>
					<div class="segmented small" role="radiogroup" aria-label="Chart style">
						{(['line', 'candles'] as const).map((s) => (
							<button key={s} role="radio" aria-checked={style === s} class={style === s ? 'on' : ''} onClick={() => setStyle(s)}>
								{s === 'line' ? 'Line' : 'Candles'}
							</button>
						))}
					</div>
				</div>
				{!history ? (
					<div class="chart" />
				) : !history.length ? (
					<div class="chart chart-empty">Your value history starts once the market has traded.</div>
				) : style === 'line' ? (
					<ValueChart candles={history} interval={interval} />
				) : (
					<Chart candles={history} interval={interval} />
				)}
				<p class="hint">
					History is rebuilt from your fills and starts at your first trade. Deposits appear to have been there from the start.
				</p>
			</section>

			<section class="panel folio-alloc">
				<h2 class="panel-title">Allocation</h2>
				<Allocation slices={slices} />
			</section>

			<section class="panel folio-holdings">
				<header class="holdings-head">
					<h2 class="panel-title">Holdings</h2>
					<div class="segmented small" role="radiogroup" aria-label="Return shown">
						<button role="radio" aria-checked={returns === 'total'} class={returns === 'total' ? 'on' : ''} onClick={() => setReturns('total')}>
							Total return
						</button>
						<button role="radio" aria-checked={returns === 'today'} class={returns === 'today' ? 'on' : ''} onClick={() => setReturns('today')}>
							Today
						</button>
					</div>
				</header>
				{!holdings.length ? (
					<p class="empty">You hold only cash. Pick a symbol on the Trade page and place a buy order.</p>
				) : (
					<div class="table-scroll">
						<table class="holdings">
							<thead>
								<tr>
									<th>Asset</th>
									<th class="num">Shares</th>
									<th class="num">Avg cost</th>
									<th class="num">Price</th>
									<th class="num">Value</th>
									<th class="num">Allocation</th>
									<th class="num">{returns === 'total' ? 'Total return' : 'Today'}</th>
								</tr>
							</thead>
							<tbody>
								{holdings.map((p) => {
									const px = prices.get(p.symbol);
									const known = p.cost_basis !== null;
									const [chg, base] =
										returns === 'total'
											? known
												? [p.value - p.cost_basis!, p.cost_basis!]
												: [null, 0]
											: px
												? [px.change * p.quantity, px.previous_close * p.quantity]
												: [null, 0];
									const weight = portfolio.total_value ? p.value / portfolio.total_value : 0;
									return (
										<tr key={p.symbol} class="clickable" onClick={() => onSelect(p.symbol)} title={`Trade ${p.symbol}`}>
											<td>
												<span class="asset">
													<i style={{ background: colors.get(p.symbol) ?? 'var(--other)' }} />
													<span>
														<b class="sym">{p.symbol}</b>
														<small>{px?.name}</small>
													</span>
												</span>
											</td>
											<td class="num">
												{shares(p.quantity)}
												{p.held > 0 && <small class="sub">{shares(p.held)} in orders</small>}
											</td>
											<td class="num">{known && p.quantity ? money(Math.round(p.cost_basis! / p.quantity)) : '—'}</td>
											<td class="num">{p.last_price ? money(p.last_price) : '—'}</td>
											<td class="num">{money(p.value)}</td>
											<td class="num">
												{share(p.value, portfolio.total_value)}
												<span class="weight" aria-hidden="true">
													<span style={{ width: `${weight * 100}%`, background: colors.get(p.symbol) ?? 'var(--other)' }} />
												</span>
											</td>
											<td class={`num ${trendClass(chg ?? 0)}`}>
												{chg === null ? (
													<span class="muted" title="Some of these shares predate your trade history, so their cost is unknown.">
														—
													</span>
												) : (
													<>
														{signedMoney(chg)}
														<small class="sub">{percent(chg, base)}</small>
													</>
												)}
											</td>
										</tr>
									);
								})}
							</tbody>
						</table>
					</div>
				)}
			</section>
		</main>
	);
}
