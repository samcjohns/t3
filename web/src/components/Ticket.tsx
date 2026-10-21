import { useEffect, useState } from 'preact/hooks';
import {
	type Api,
	ApiError,
	type Direction,
	type OrderRequest,
	type OrderType,
	type PoolSummary,
	type Portfolio,
	type Price,
	type TimeInForce,
} from '../api';
import { bps, dollarsInput, money, parseDollars, shares } from '../format';

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

/** What the trader pays of a leveraged buy's cost: the pool lends the rest, rounded down, as the ledger does. */
const ownShare = (cost: number, leverage: number) => cost - Math.floor((cost * (leverage - 1)) / leverage);

export function Ticket({ api, price, portfolio, secondsToAuction, onPlaced }: Props) {
	const [side, setSide] = useState<Direction>('BUY');
	const [type, setType] = useState<OrderType>('LIMIT');
	const [tif, setTif] = useState<TimeInForce>('IOC');
	const [qty, setQty] = useState('10');
	const [limit, setLimit] = useState('');
	const [maxCost, setMaxCost] = useState('');
	const [leverage, setLeverage] = useState(1);
	const [pool, setPool] = useState<PoolSummary | null>(null);
	const [busy, setBusy] = useState(false);
	const [result, setResult] = useState<{ ok: boolean; text: string } | null>(null);

	// The pool's rates and cash change every auction, as prices do.
	useEffect(() => {
		let live = true;
		api.pool().then((v) => live && setPool(v.pool), () => {});
		return () => {
			live = false;
		};
	}, [api, price]);

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

	const lev = side === 'BUY' ? leverage : 1;
	const cashAvailable = portfolio ? portfolio.cash - portfolio.cash_held : 0;
	const pos = portfolio?.positions.find((p) => p.symbol === price.symbol);
	const sharesAvailable = pos ? pos.quantity - pos.held : 0;

	// The most shares the account can fund: everything it holds for a sell, what
	// its cash (with leverage, its share of the cost) covers at the last price
	// for a market buy, and at the entered limit for a limit buy. Null until a
	// limit buy has a limit to size against.
	// The most a leveraged buy may borrow: what the pool has free, within its
	// per-trade and per-trader limits.
	const borrowLimit = pool ? Math.max(0, Math.min(pool.available, pool.max_trade_loan, pool.max_trader_loan - (portfolio?.owed ?? 0))) : Infinity;
	const buyable = (unit: number) => {
		let n = Math.floor((cashAvailable * lev) / unit);
		if (lev > 1) n = Math.min(n, Math.floor(((borrowLimit + 1) * lev) / ((lev - 1) * unit)));
		while (n > 0 && (ownShare(n * unit, lev) > cashAvailable || n * unit - ownShare(n * unit, lev) > borrowLimit)) n--;
		return n;
	};
	let maxQty: number | null = null;
	if (portfolio) {
		if (side === 'SELL') maxQty = sharesAvailable;
		else if (type === 'MARKET') maxQty = price.price > 0 ? buyable(price.price) : 0;
		else if (limit.trim() !== '' && limitCents !== null && limitCents > 0) maxQty = buyable(limitCents);
	}
	const fillMax = () => {
		if (maxQty === null || maxQty < 1) return;
		setQty(String(maxQty));
		// A market buy fills as many shares as its max cost covers, so spend it all.
		if (side === 'BUY' && type === 'MARKET') setMaxCost(dollarsInput(Math.min(cashAvailable * lev, maxQty * price.price)));
	};

	let estimate: number | null = null;
	if (Number.isFinite(quantity)) {
		if (type === 'LIMIT' && limitCents !== null) estimate = quantity * limitCents;
		else if (type === 'MARKET') estimate = side === 'BUY' ? maxCostCents : quantity * price.price;
	}

	const own = estimate !== null && side === 'BUY' ? ownShare(estimate, lev) : null;
	const borrow = estimate !== null && own !== null ? estimate - own : 0;

	let problem = '';
	if (portfolio?.status === 'liquidating') problem = 'Your account is being liquidated, so it can’t trade.';
	else if (portfolio?.status === 'bankrupt') problem = 'Your account is bankrupt and closed.';
	else if (!Number.isFinite(quantity) || quantity < 1) problem = 'Enter a whole number of shares.';
	else if (type === 'LIMIT' && (limitCents === null || limitCents < 1)) problem = 'Enter a limit price.';
	else if (type === 'MARKET' && side === 'BUY' && (maxCostCents === null || maxCostCents < 1)) problem = 'Enter a maximum cost.';
	else if (portfolio && side === 'BUY' && own !== null && own > cashAvailable) problem = `You have ${money(cashAvailable)} available.`;
	else if (lev > 1 && portfolio && portfolio.pool_value > 0) problem = 'Withdraw your stake in the lending pool before borrowing.';
	else if (lev > 1 && pool && borrow > pool.available) problem = `The lending pool has only ${money(pool.available)} available to borrow.`;
	else if (lev > 1 && pool && borrow > pool.max_trade_loan)
		problem = `One order can borrow at most ${money(pool.max_trade_loan)}, ${bps(pool.max_trade_loan_bps)} of the pool.`;
	else if (lev > 1 && pool && portfolio && portfolio.owed + borrow > pool.max_trader_loan)
		problem = `You can owe the pool at most ${money(pool.max_trader_loan)}, ${bps(pool.max_trader_loan_bps)} of it. You owe ${money(portfolio.owed)}.`;
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
		if (lev > 1) order.leverage = lev;
		setBusy(true);
		setResult(null);
		try {
			await api.placeOrder(order);
			const when = secondsToAuction !== null ? `in about ${Math.max(1, Math.ceil(secondsToAuction))}s` : 'shortly';
			const levered = lev > 1 ? ` at ${lev}x leverage` : '';
			setResult({ ok: true, text: `${side === 'BUY' ? 'Buy' : 'Sell'} order for ${shares(quantity)} ${price.symbol}${levered} accepted. It goes to auction ${when}.` });
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

			{side === 'BUY' && pool && pool.rates.length > 0 && (
				<div class="leverage">
					<div class="segmented" role="radiogroup" aria-label="Leverage">
						{[1, ...pool.rates.map((r) => r.ratio)].map((r) => (
							<button
								key={r}
								type="button"
								role="radio"
								aria-checked={leverage === r}
								class={leverage === r ? 'on' : ''}
								title={r === 1 ? 'No leverage: pay the whole cost yourself' : `Pay 1/${r} of the cost and borrow the rest`}
								onClick={() => setLeverage(r)}
							>
								{r}x
							</button>
						))}
					</div>
					{lev > 1 && (
						<small class="hint">
							Borrows {bps(10_000 - 10_000 / lev)} of the cost from the lending pool at {bps(pool.rates.find((r) => r.ratio === lev)?.rate_bps ?? 0)} a
							day. Sales repay it first.
						</small>
					)}
				</div>
			)}

			<label>
				<span>Shares</span>
				<div class="with-max">
					<input inputMode="numeric" value={qty} onInput={(e) => setQty(e.currentTarget.value.trim())} />
					{portfolio && (
						<button
							type="button"
							class="max"
							disabled={maxQty === null || maxQty < 1}
							title={
								maxQty === null
									? 'Enter a limit price first'
									: side === 'SELL'
										? 'Sell every share you have available'
										: lev > 1
											? 'Buy as many shares as your cash and the pool’s loan limits allow'
											: 'Buy as many shares as your cash covers'
							}
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
				{lev > 1 && (
					<div>
						<dt>Borrowed from pool</dt>
						<dd>{estimate === null ? '—' : money(borrow)}</dd>
					</div>
				)}
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
