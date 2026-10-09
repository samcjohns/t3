import { useEffect, useState } from 'preact/hooks';
import { type Api, ApiError, type Direction, type OrderRequest, type OrderType, type Portfolio, type Price, type TimeInForce } from '../api';
import { dollarsInput, money, parseDollars, shares } from '../format';

interface Props {
	api: Api;
	price: Price;
	portfolio: Portfolio | null;
	/** Seconds until the next auction, for the confirmation message. */
	secondsToAuction: number | null;
	onPlaced: () => void;
}

/** Headroom over the last price that a market buy's default max cost allows. */
const MARKET_SLIPPAGE = 1.05;
/**
 * How far past the last price a limit order is placed by default. The market
 * makers quote well within this, so the default order fills, and it pays the
 * auction's clearing price rather than its limit.
 */
const LIMIT_REACH = 0.01;

export function Ticket({ api, price, portfolio, secondsToAuction, onPlaced }: Props) {
	const [side, setSide] = useState<Direction>('BUY');
	const [type, setType] = useState<OrderType>('LIMIT');
	const [tif, setTif] = useState<TimeInForce>('IOC');
	const [qty, setQty] = useState('10');
	const [limit, setLimit] = useState('');
	const [maxCost, setMaxCost] = useState('');
	const [busy, setBusy] = useState(false);
	const [result, setResult] = useState<{ ok: boolean; text: string } | null>(null);

	// A new symbol starts from its own price.
	useEffect(() => {
		setLimit('');
		setMaxCost('');
		setResult(null);
	}, [price.symbol]);

	const quantity = /^\d+$/.test(qty) ? Number(qty) : NaN;
	const autoLimit = side === 'BUY' ? Math.ceil(price.price * (1 + LIMIT_REACH)) : Math.max(1, Math.floor(price.price * (1 - LIMIT_REACH)));
	const limitCents = limit.trim() === '' ? autoLimit : parseDollars(limit);
	const autoMaxCost = Number.isFinite(quantity) ? Math.ceil(quantity * price.price * MARKET_SLIPPAGE) : 0;
	const maxCostCents = maxCost.trim() === '' ? autoMaxCost : parseDollars(maxCost);

	const cashAvailable = portfolio ? portfolio.cash - portfolio.cash_held : 0;
	const pos = portfolio?.positions.find((p) => p.symbol === price.symbol);
	const sharesAvailable = pos ? pos.quantity - pos.held : 0;

	// The most shares the account can fund: everything it holds for a sell, what
	// its cash covers at the last price for a market buy, and at the entered
	// limit for a limit buy. Null until a limit buy has a limit to size against.
	let maxQty: number | null = null;
	if (portfolio) {
		if (side === 'SELL') maxQty = sharesAvailable;
		else if (type === 'MARKET') maxQty = price.price > 0 ? Math.floor(cashAvailable / price.price) : 0;
		else if (limit.trim() !== '' && limitCents !== null && limitCents > 0) maxQty = Math.floor(cashAvailable / limitCents);
	}
	const fillMax = () => {
		if (maxQty === null || maxQty < 1) return;
		setQty(String(maxQty));
		// A market buy fills as many shares as its max cost covers, so spend it all.
		if (side === 'BUY' && type === 'MARKET') setMaxCost(dollarsInput(cashAvailable));
	};

	let estimate: number | null = null;
	if (Number.isFinite(quantity)) {
		if (type === 'LIMIT' && limitCents !== null) estimate = quantity * limitCents;
		else if (type === 'MARKET') estimate = side === 'BUY' ? maxCostCents : quantity * price.price;
	}

	let problem = '';
	if (!Number.isFinite(quantity) || quantity < 1) problem = 'Enter a whole number of shares.';
	else if (type === 'LIMIT' && (limitCents === null || limitCents < 1)) problem = 'Enter a limit price.';
	else if (type === 'MARKET' && side === 'BUY' && (maxCostCents === null || maxCostCents < 1)) problem = 'Enter a maximum cost.';
	else if (portfolio && side === 'BUY' && estimate !== null && estimate > cashAvailable) problem = `You have ${money(cashAvailable)} available.`;
	else if (portfolio && side === 'SELL' && quantity > sharesAvailable) problem = `You have ${shares(sharesAvailable)} shares available to sell.`;

	const submit = async (e: Event) => {
		e.preventDefault();
		if (problem || busy) return;
		const order: OrderRequest = { symbol: price.symbol, direction: side, type, quantity };
		if (type === 'LIMIT') {
			order.limit_price = limitCents!;
			order.time_in_force = tif;
		} else if (side === 'BUY') {
			order.max_cost = maxCostCents!;
		}
		setBusy(true);
		setResult(null);
		try {
			await api.placeOrder(order);
			const when = secondsToAuction !== null ? `in about ${Math.max(1, Math.ceil(secondsToAuction))}s` : 'shortly';
			setResult({ ok: true, text: `${side === 'BUY' ? 'Buy' : 'Sell'} order for ${shares(quantity)} ${price.symbol} accepted. It goes to auction ${when}.` });
			onPlaced();
		} catch (err) {
			setResult({ ok: false, text: err instanceof ApiError ? err.message : 'The order could not be placed.' });
		} finally {
			setBusy(false);
		}
	};

	return (
		<form class={`ticket ${side === 'BUY' ? 'buy' : 'sell'}`} onSubmit={submit}>
			<div class="segmented sides" role="radiogroup" aria-label="Side">
				{(['BUY', 'SELL'] as const).map((d) => (
					<button key={d} type="button" role="radio" aria-checked={side === d} class={side === d ? `on ${d.toLowerCase()}` : ''} onClick={() => setSide(d)}>
						{d === 'BUY' ? 'Buy' : 'Sell'}
					</button>
				))}
			</div>

			<div class="segmented" role="radiogroup" aria-label="Order type">
				{(['LIMIT', 'MARKET'] as const).map((t) => (
					<button key={t} type="button" role="radio" aria-checked={type === t} class={type === t ? 'on' : ''} onClick={() => setType(t)}>
						{t === 'LIMIT' ? 'Limit' : 'Market'}
					</button>
				))}
			</div>

			<label>
				<span>Shares</span>
				<div class="with-max">
					<input inputMode="numeric" value={qty} onInput={(e) => setQty(e.currentTarget.value.trim())} />
					{portfolio && (
						<button
							type="button"
							class="max"
							disabled={maxQty === null || maxQty < 1}
							title={maxQty === null ? 'Enter a limit price first' : side === 'BUY' ? 'Buy as many shares as your cash covers' : 'Sell every share you have available'}
							onClick={fillMax}
						>
							{side === 'BUY' ? 'Max' : 'All'}
						</button>
					)}
				</div>
			</label>

			{type === 'LIMIT' ? (
				<>
					<label>
						<span>Limit price</span>
						<div class="affix">
							<i>$</i>
							<input inputMode="decimal" value={limit} placeholder={dollarsInput(autoLimit)} onInput={(e) => setLimit(e.currentTarget.value)} />
						</div>
						<small>Everyone in the auction trades at one clearing price, which can be better than your limit.</small>
					</label>
					<label>
						<span>Time in force</span>
						<select value={tif} onChange={(e) => setTif(e.currentTarget.value as TimeInForce)}>
							<option value="IOC">Next auction only (IOC)</option>
							<option value="GTC">Until filled (GTC)</option>
						</select>
					</label>
				</>
			) : side === 'BUY' ? (
				<label>
					<span>Maximum cost</span>
					<div class="affix">
						<i>$</i>
						<input inputMode="decimal" value={maxCost} placeholder={dollarsInput(autoMaxCost)} onInput={(e) => setMaxCost(e.currentTarget.value)} />
					</div>
					<small>Fills as many shares as this covers at the clearing price.</small>
				</label>
			) : (
				<p class="hint">Sells at whatever price the next auction clears at.</p>
			)}

			<dl class="ticket-sums">
				<div>
					<dt>{type === 'MARKET' ? (side === 'BUY' ? 'Spends at most' : 'Estimated proceeds') : side === 'BUY' ? 'Cost at limit' : 'Proceeds at limit'}</dt>
					<dd>{estimate === null ? '—' : money(estimate)}</dd>
				</div>
				{portfolio && (
					<div>
						<dt>{side === 'BUY' ? 'Cash available' : 'Shares available'}</dt>
						<dd>{side === 'BUY' ? money(cashAvailable) : shares(sharesAvailable)}</dd>
					</div>
				)}
			</dl>

			<button class="submit" type="submit" disabled={!!problem || busy}>
				{busy ? 'Placing…' : `${side === 'BUY' ? 'Buy' : 'Sell'} ${price.symbol}`}
			</button>
			{problem && quantity > 0 ? <p class="hint">{problem}</p> : null}
			{result && (
				<p class={`notice ${result.ok ? 'ok' : 'err'}`} role="status">
					{result.text}
				</p>
			)}
			{type === 'LIMIT' && tif === 'GTC' && <p class="hint">GTC orders stay in the book until they fill. There's no way to cancel them yet.</p>}
		</form>
	);
}
