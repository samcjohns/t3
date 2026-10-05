import type { ComponentChildren } from 'preact';
import { useCallback, useEffect, useMemo, useState } from 'preact/hooks';
import type { Api, Candle, Fill, Interval, Portfolio, Trade } from './api';
import { AuthPanel } from './components/AuthPanel';
import { Chart } from './components/Chart';
import { Markets } from './components/Markets';
import { Fills, Positions, Tape } from './components/Tables';
import { Ticket } from './components/Ticket';
import { money, percent, shares, signedMoney, trendClass } from './format';
import { useHashSymbol, useNow, usePrices, useSession } from './hooks';

const INTERVALS: Interval[] = ['1m', '5m', '15m', '1h', '1d'];

export function App({ api }: { api: Api }) {
	const { snapshot, offline } = usePrices(api);
	const [session, setSession] = useSession(api);
	const [hashSymbol, select] = useHashSymbol();
	const [interval, setCandleInterval] = useState<Interval>('1m');
	const [candles, setCandles] = useState<Candle[] | null>(null);
	const [tape, setTape] = useState<Trade[]>([]);
	const [portfolio, setPortfolio] = useState<Portfolio | null>(null);
	const [fills, setFills] = useState<Fill[]>([]);
	const [tab, setTab] = useState<'positions' | 'fills' | 'tape'>('positions');
	const now = useNow(250);

	const prices = useMemo(() => new Map(snapshot?.prices.map((p) => [p.symbol, p])), [snapshot]);
	const symbol = prices.has(hashSymbol) ? hashSymbol : (snapshot?.prices[0]?.symbol ?? '');
	const price = prices.get(symbol);
	const tick = snapshot?.tick;

	// Chart and tape follow the selected symbol and refresh every auction.
	useEffect(() => {
		if (!symbol) return;
		let live = true;
		api.candles(symbol, interval).then((c) => live && setCandles(c), () => {});
		api.trades(symbol).then((t) => live && setTape(t), () => {});
		return () => {
			live = false;
		};
	}, [api, symbol, interval, tick]);

	// Clear the chart only when switching what it shows, not on each refresh.
	useEffect(() => setCandles(null), [symbol, interval]);

	const refreshAccount = useCallback(() => {
		if (!api.token) return;
		api.portfolio().then(setPortfolio, () => {});
		api.fills().then(setFills, () => {});
	}, [api]);

	useEffect(() => {
		if (session) refreshAccount();
		else {
			setPortfolio(null);
			setFills([]);
		}
	}, [session, tick, refreshAccount]);

	const signOut = () => {
		api.logout().catch(() => {});
		setSession(null);
	};

	const nextTick = snapshot ? Date.parse(snapshot.next_tick_at) : 0;
	const period = snapshot ? Math.max(1, nextTick - Date.parse(snapshot.as_of)) : 1;
	const remaining = snapshot ? Math.max(0, nextTick - now) : 0;

	return (
		<div class="app">
			<header class="top">
				<a class="brand" href="/">
					<b>t3</b>
					<span>virtual stock market</span>
				</a>
				<div class="auction" title="Orders are matched in a batch auction at every tick.">
					{offline ? (
						<span class="status-off">Market unreachable · retrying</span>
					) : snapshot ? (
						<>
							<span class="muted">Tick {snapshot.tick.toLocaleString()}</span>
							<span>Next auction {remaining > 0 ? `${Math.ceil(remaining / 1000)}s` : 'clearing…'}</span>
							<span class="meter" aria-hidden="true">
								<span style={{ width: `${(1 - remaining / period) * 100}%` }} />
							</span>
						</>
					) : (
						<span class="muted">Connecting…</span>
					)}
				</div>
				{session && (
					<div class="who">
						<span>{session.user.username}</span>
						<button class="link" onClick={signOut}>
							Sign out
						</button>
					</div>
				)}
			</header>

			{portfolio && (
				<section class="summary" aria-label="Account summary">
					<Stat label="Account value" value={money(portfolio.total_value)} big />
					<Stat label="Cash available" value={money(portfolio.cash - portfolio.cash_held)} />
					<Stat label="In open orders" value={money(portfolio.cash_held)} />
					<Stat label="Holdings" value={money(portfolio.market_value)} />
				</section>
			)}

			{snapshot && (
				<main class="desk">
					<Markets prices={snapshot.prices} selected={symbol} onSelect={select} />

					<section class="instrument">
						{price && (
							<header class="quote">
								<div>
									<h1>{price.symbol}</h1>
									<p class="muted">{price.name}</p>
								</div>
								<div class="quote-px">
									<span key={price.price} class={`flash ${trendClass(price.change)}`}>
										{money(price.price)}
									</span>
									<span class={trendClass(price.change)}>
										{signedMoney(price.change)} ({percent(price.change, price.previous_close)})
									</span>
									<span class="muted">Vol {shares(price.volume)} today</span>
								</div>
							</header>
						)}
						<div class="segmented small" role="radiogroup" aria-label="Candle interval">
							{INTERVALS.map((i) => (
								<button key={i} role="radio" aria-checked={interval === i} class={interval === i ? 'on' : ''} onClick={() => setCandleInterval(i)}>
									{i}
								</button>
							))}
						</div>
						<Chart candles={candles} interval={interval} />

						<div class="tabs" role="tablist">
							{session && (
								<>
									<Tab id="positions" tab={tab} setTab={setTab}>
										Positions
									</Tab>
									<Tab id="fills" tab={tab} setTab={setTab}>
										Your fills
									</Tab>
								</>
							)}
							<Tab id="tape" tab={session ? tab : 'tape'} setTab={setTab}>
								{symbol} trades
							</Tab>
						</div>
						<div class="table-wrap" role="tabpanel">
							{!session || tab === 'tape' ? (
								<Tape trades={tape} />
							) : !portfolio ? null : tab === 'positions' ? (
								<Positions portfolio={portfolio} prices={prices} onSelect={select} />
							) : (
								<Fills fills={fills} onSelect={select} />
							)}
						</div>
					</section>

					<aside class="side">
						{session && price ? (
							<>
								<h2 class="panel-title">Trade {price.symbol}</h2>
								<Ticket
									api={api}
									price={price}
									portfolio={portfolio}
									secondsToAuction={snapshot ? remaining / 1000 : null}
									onPlaced={refreshAccount}
								/>
							</>
						) : (
							<AuthPanel api={api} onSession={setSession} />
						)}
						<p class="about">
							Every {Math.round(period / 1000)} seconds, t3 clears all orders for each symbol in one batch auction, at a single price. The
							money is virtual.
						</p>
					</aside>
				</main>
			)}
		</div>
	);
}

function Stat({ label, value, big }: { label: string; value: string; big?: boolean }) {
	return (
		<div class={big ? 'stat big' : 'stat'}>
			<span>{label}</span>
			<b>{value}</b>
		</div>
	);
}

type TabId = 'positions' | 'fills' | 'tape';

function Tab({ id, tab, setTab, children }: { id: TabId; tab: TabId; setTab: (t: TabId) => void; children: ComponentChildren }) {
	return (
		<button role="tab" aria-selected={tab === id} class={tab === id ? 'on' : ''} onClick={() => setTab(id)}>
			{children}
		</button>
	);
}
