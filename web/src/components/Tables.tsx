import { useState } from 'preact/hooks';
import { ApiError, type Fill, type OpenOrder, type Portfolio, type Price, type Trade } from '../api';
import { clock, money, percent, shares, trendClass } from '../format';

export function Positions({ portfolio, prices, onSelect }: { portfolio: Portfolio; prices: Map<string, Price>; onSelect: (s: string) => void }) {
	if (!portfolio.positions.length) return <p class="empty">You don't own any shares yet. Pick a symbol and place a buy order.</p>;
	return (
		<table>
			<thead>
				<tr>
					<th>Symbol</th>
					<th class="num">Shares</th>
					<th class="num">In orders</th>
					<th class="num">Last</th>
					<th class="num">Today</th>
					<th class="num">Value</th>
				</tr>
			</thead>
			<tbody>
				{portfolio.positions.map((p) => {
					const px = prices.get(p.symbol);
					return (
						<tr key={p.symbol} class="clickable" onClick={() => onSelect(p.symbol)}>
							<td class="sym">{p.symbol}</td>
							<td class="num">{shares(p.quantity)}</td>
							<td class="num muted">{p.held ? shares(p.held) : '—'}</td>
							<td class="num">{p.last_price ? money(p.last_price) : '—'}</td>
							<td class={`num ${trendClass(px?.change ?? 0)}`}>{px ? percent(px.change, px.previous_close) : '—'}</td>
							<td class="num">{money(p.value)}</td>
						</tr>
					);
				})}
			</tbody>
		</table>
	);
}

export function Fills({ fills, onSelect }: { fills: Fill[]; onSelect: (s: string) => void }) {
	if (!fills.length) return <p class="empty">No fills yet. Orders fill when an auction clears them.</p>;
	return (
		<table>
			<thead>
				<tr>
					<th>Time</th>
					<th>Symbol</th>
					<th>Side</th>
					<th class="num">Shares</th>
					<th class="num">Price</th>
					<th class="num">Amount</th>
				</tr>
			</thead>
			<tbody>
				{fills.map((f, i) => (
					<tr key={`${f.order_id}:${f.tick}:${i}`} class="clickable" onClick={() => onSelect(f.symbol)}>
						<td class="muted">{clock(f.time)}</td>
						<td class="sym">{f.symbol}</td>
						<td class={f.direction === 'BUY' ? 'up' : 'down'}>{f.direction === 'BUY' ? 'Buy' : 'Sell'}</td>
						<td class="num">{shares(f.quantity)}</td>
						<td class="num">{money(f.price)}</td>
						<td class="num">{money(f.price * f.quantity)}</td>
					</tr>
				))}
			</tbody>
		</table>
	);
}

/** Your orders that haven't filled or expired, with what each holds and a way to cancel it. */
export function OpenOrders({ orders, onCancel, onSelect }: { orders: OpenOrder[]; onCancel: (id: string) => Promise<unknown>; onSelect: (s: string) => void }) {
	const [busy, setBusy] = useState('');
	const [error, setError] = useState('');
	if (!orders.length) return <p class="empty">No open orders. Limit orders that don't fill wait here until they do, or you cancel them.</p>;

	const cancel = async (e: Event, id: string) => {
		e.stopPropagation();
		setBusy(id);
		setError('');
		try {
			await onCancel(id);
		} catch (err) {
			setError(err instanceof ApiError ? err.message : 'The order could not be cancelled.');
		} finally {
			setBusy('');
		}
	};

	return (
		<>
			{error && (
				<p class="notice err" role="alert">
					{error}
				</p>
			)}
			<table>
				<thead>
					<tr>
						<th>Placed</th>
						<th>Symbol</th>
						<th>Side</th>
						<th>Type</th>
						<th class="num">Unfilled</th>
						<th class="num">Limit</th>
						<th class="num">Holds</th>
						<th class="num" aria-label="Cancel" />
					</tr>
				</thead>
				<tbody>
					{orders.map((o) => (
						<tr key={o.id} class="clickable" onClick={() => onSelect(o.symbol)}>
							<td class="muted">{clock(o.created_at)}</td>
							<td class="sym">{o.symbol}</td>
							<td class={o.direction === 'BUY' ? 'up' : 'down'}>
								{o.direction === 'BUY' ? 'Buy' : 'Sell'}
								{o.leverage > 1 && <span class="muted"> {o.leverage}x</span>}
							</td>
							<td>{o.type === 'MARKET' ? 'Market' : `Limit ${o.time_in_force}`}</td>
							<td class="num">
								{shares(o.remaining)}
								{o.remaining !== o.quantity && <span class="muted"> of {shares(o.quantity)}</span>}
							</td>
							<td class="num">{o.type === 'LIMIT' ? money(o.limit_price) : '—'}</td>
							<td class="num">{o.direction === 'BUY' ? money(o.cash_held) : `${shares(o.shares_held)} sh`}</td>
							<td class="num">
								{o.status === 'cancelling' ? (
									<span class="muted" title="It leaves the book at the next auction, unless it fills first.">
										Cancelling…
									</span>
								) : (
									<button class="small-btn danger" disabled={busy === o.id} onClick={(e) => cancel(e, o.id)}>
										{busy === o.id ? 'Cancelling…' : 'Cancel'}
									</button>
								)}
							</td>
						</tr>
					))}
				</tbody>
			</table>
		</>
	);
}

/** The public tape for one symbol. */
export function Tape({ trades }: { trades: Trade[] }) {
	if (!trades.length) return <p class="empty">No trades yet.</p>;
	return (
		<table>
			<thead>
				<tr>
					<th>Time</th>
					<th class="num">Tick</th>
					<th class="num">Shares</th>
					<th class="num">Price</th>
				</tr>
			</thead>
			<tbody>
				{trades.map((t, i) => (
					<tr key={`${t.tick}:${i}`}>
						<td class="muted">{clock(t.time)}</td>
						<td class="num muted">{t.tick}</td>
						<td class="num">{shares(t.quantity)}</td>
						<td class="num">{money(t.price)}</td>
					</tr>
				))}
			</tbody>
		</table>
	);
}
