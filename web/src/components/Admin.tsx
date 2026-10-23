import { useEffect, useState } from 'preact/hooks';
import { type AdminMaker, type AdminUser, type Api, ApiError, type Direction, type MakerSetting, type OrderType, type PriceSnapshot, type Session } from '../api';
import { money, parseDollars, shares } from '../format';

interface Props {
	api: Api;
	session: Session | null;
	onSession: (s: Session) => void;
	snapshot: PriceSnapshot | null;
}

/** Admin tools: adjust traders' cash, moderate traders, control the market makers and break market-moving news. Only an admin sign-in can use them. */
export function Admin({ api, session, onSession, snapshot }: Props) {
	if (!session) return <AdminSignIn api={api} onSession={onSession} />;
	if (session.user.role !== 'admin') {
		return (
			<main class="board">
				<section class="panel">
					<h1 class="panel-title">Admin</h1>
					<p class="empty">You're signed in as {session.user.username}, who isn't an admin. Sign out, then sign in here as an admin.</p>
				</section>
			</main>
		);
	}
	return (
		<main class="board admin">
			<CashPanel api={api} tick={snapshot?.tick} />
			<MakersPanel api={api} snapshot={snapshot} />
			{snapshot && <NewsPanel api={api} snapshot={snapshot} />}
		</main>
	);
}

function AdminSignIn({ api, onSession }: { api: Api; onSession: (s: Session) => void }) {
	const [username, setUsername] = useState('admin');
	const [password, setPassword] = useState('');
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState('');

	const submit = async (e: Event) => {
		e.preventDefault();
		setBusy(true);
		setError('');
		try {
			const s = await api.login(username, password);
			if (s.user.role !== 'admin') {
				// Don't leave a trader signed in through the admin page.
				api.token = s.token;
				api.logout().catch(() => {});
				api.token = undefined;
				setError(`${s.user.username} isn't an admin.`);
				return;
			}
			onSession(s);
		} catch (err) {
			setError(err instanceof ApiError ? err.message : 'Something went wrong. Try again.');
		} finally {
			setBusy(false);
		}
	};

	return (
		<main class="board admin-signin">
			<form class="panel auth" onSubmit={submit}>
				<h2>Admin sign in</h2>
				<label>
					<span>Username</span>
					<input autocomplete="username" autocapitalize="none" spellcheck={false} value={username} onInput={(e) => setUsername(e.currentTarget.value)} required />
				</label>
				<label>
					<span>Password</span>
					<input type="password" autocomplete="current-password" value={password} onInput={(e) => setPassword(e.currentTarget.value)} required />
				</label>
				<button class="submit" type="submit" disabled={busy}>
					{busy ? 'One moment…' : 'Sign in'}
				</button>
				{error && (
					<p class="notice err" role="alert">
						{error}
					</p>
				)}
			</form>
		</main>
	);
}

function CashPanel({ api, tick }: { api: Api; tick?: number }) {
	const [users, setUsers] = useState<AdminUser[] | null>(null);
	const [selected, setSelected] = useState('');
	const [amount, setAmount] = useState('');
	const [busy, setBusy] = useState(false);
	const [result, setResult] = useState<{ ok: boolean; text: string } | null>(null);
	const [confirm, setConfirm] = useState<'liquidate' | 'delete' | null>(null);

	const load = () =>
		api.adminUsers().then(
			(u) => setUsers(u),
			() => {},
		);
	// Values move every auction.
	useEffect(() => {
		load();
	}, [api, tick]);

	const user = users?.find((u) => u.id === selected);
	const cents = parseDollars(amount);

	const select = (id: string) => {
		setSelected(id);
		setConfirm(null);
	};

	const adjust = async (sign: 1 | -1) => {
		if (!user || !cents) return;
		setBusy(true);
		setResult(null);
		try {
			const a = await api.adjustCash(user.id, sign * cents);
			setResult({ ok: true, text: `${sign > 0 ? 'Added' : 'Removed'} ${money(cents)} ${sign > 0 ? 'to' : 'from'} ${user.username}. Cash is now ${money(a.cash)}.` });
			setAmount('');
			load();
		} catch (err) {
			setResult({ ok: false, text: err instanceof ApiError ? err.message : 'Something went wrong. Try again.' });
		} finally {
			setBusy(false);
		}
	};

	const moderate = async (action: 'block' | 'unblock' | 'liquidate' | 'delete') => {
		if (!user) return;
		setBusy(true);
		setResult(null);
		try {
			let text: string;
			if (action === 'delete') {
				await api.deleteUser(user.id);
				setSelected('');
				text = `Deleted ${user.username}. Their open orders are cancelled in the next auction.`;
			} else if (action === 'liquidate') {
				await api.liquidate(user.id);
				text = `Liquidating ${user.username}: their orders are cancelled and their shares sold at market, starting next auction.`;
			} else {
				await api.setBlocked(user.id, action === 'block');
				text = action === 'block' ? `Blocked ${user.username}. Their open orders are cancelled in the next auction.` : `Unblocked ${user.username}.`;
			}
			setResult({ ok: true, text });
			load();
		} catch (err) {
			setResult({ ok: false, text: err instanceof ApiError ? err.message : 'Something went wrong. Try again.' });
		} finally {
			setBusy(false);
			setConfirm(null);
		}
	};

	return (
		<section class="panel">
			<header class="board-head">
				<div>
					<h1 class="panel-title">Traders</h1>
					<p class="board-sub">Pick a trader, then add or remove cash, or moderate them. Cash held for open orders can't be removed.</p>
				</div>
			</header>
			<div class="admin-form">
				<label>
					<span>Trader</span>
					<select value={selected} onChange={(e) => select(e.currentTarget.value)}>
						<option value="">Choose a trader…</option>
						{users?.map((u) => (
							<option key={u.id} value={u.id}>
								{u.username}
							</option>
						))}
					</select>
				</label>
				<label>
					<span>Amount</span>
					<span class="affix">
						<i>$</i>
						<input inputMode="decimal" placeholder="0.00" value={amount} onInput={(e) => setAmount(e.currentTarget.value)} />
					</span>
				</label>
				<div class="btn-row">
					<button class="small-btn primary" disabled={busy || !user || !cents} onClick={() => adjust(1)}>
						Add cash
					</button>
					<button class="small-btn danger" disabled={busy || !user || !cents} onClick={() => adjust(-1)}>
						Remove cash
					</button>
				</div>
				<div class="btn-row">
					<button class="small-btn" disabled={busy || !user} onClick={() => moderate(user?.blocked ? 'unblock' : 'block')}>
						{user?.blocked ? 'Unblock trading' : 'Block trading'}
					</button>
					<button class="small-btn danger" disabled={busy || !user || user.status !== 'active'} onClick={() => setConfirm('liquidate')}>
						Force sell all
					</button>
					<button class="small-btn danger" disabled={busy || !user} onClick={() => setConfirm('delete')}>
						Delete trader
					</button>
				</div>
			</div>
			{user && confirm && (
				<div class="confirm">
					<span>
						{confirm === 'delete'
							? `Delete ${user.username}? They're signed out, their API token stops working and their open orders are cancelled. This can't be undone.`
							: `Sell everything ${user.username} owns? Their open orders are cancelled and every share is sold at market. Proceeds repay what they owe the lending pool.`}
					</span>
					<div class="btn-row">
						<button class="small-btn danger" disabled={busy} onClick={() => moderate(confirm)}>
							{confirm === 'delete' ? 'Delete' : 'Sell all'}
						</button>
						<button class="small-btn" disabled={busy} onClick={() => setConfirm(null)}>
							Cancel
						</button>
					</div>
				</div>
			)}
			{amount && cents === null && <p class="notice err">Enter a dollar amount, like 250 or 99.50.</p>}
			{result && (
				<p class={`notice ${result.ok ? 'ok' : 'err'}`} role="status">
					{result.text}
				</p>
			)}

			<div class="table-wrap">
				{!users ? (
					<p class="empty">Loading…</p>
				) : !users.length ? (
					<p class="empty">No traders yet.</p>
				) : (
					<table>
						<thead>
							<tr>
								<th>Trader</th>
								<th>Status</th>
								<th class="num">Cash</th>
								<th class="num">Available</th>
								<th class="num">Account value</th>
							</tr>
						</thead>
						<tbody>
							{users.map((u) => (
								<tr key={u.id} class={u.id === selected ? 'clickable on' : 'clickable'} onClick={() => select(u.id)}>
									<td>{u.username}</td>
									<td>{[u.status === 'active' ? '' : u.status, u.blocked ? 'blocked' : ''].filter(Boolean).join(', ') || 'active'}</td>
									<td class="num">{money(u.cash)}</td>
									<td class="num">{money(u.cash - u.cash_held)}</td>
									<td class="num">{money(u.total_value)}</td>
								</tr>
							))}
						</tbody>
					</table>
				)}
			</div>
		</section>
	);
}

type Result = { ok: boolean; text: string } | null;

/** Runs one admin action at a time, keeping its outcome to show. */
function useAction() {
	const [busy, setBusy] = useState(false);
	const [result, setResult] = useState<Result>(null);
	const run = async (action: () => Promise<string>) => {
		setBusy(true);
		setResult(null);
		try {
			setResult({ ok: true, text: await action() });
		} catch (err) {
			setResult({ ok: false, text: err instanceof ApiError ? err.message : 'Something went wrong. Try again.' });
		} finally {
			setBusy(false);
		}
	};
	return { busy, result, run };
}

function Outcome({ result }: { result: Result }) {
	return result ? (
		<p class={`notice ${result.ok ? 'ok' : 'err'}`} role="status">
			{result.text}
		</p>
	) : null;
}

function makerStatus(m: AdminMaker) {
	const s = [m.paused ? 'paused' : '', !m.seen_at ? 'not connected' : !m.online ? 'offline' : ''].filter(Boolean).join(', ');
	return s || 'running';
}

function MakersPanel({ api, snapshot }: { api: Api; snapshot: PriceSnapshot | null }) {
	const [makers, setMakers] = useState<AdminMaker[] | null>(null);
	const [selected, setSelected] = useState('');

	const load = () =>
		api.adminMakers().then(
			(m) => setMakers(m),
			() => {},
		);
	// Balances move, and makers check in, every auction.
	useEffect(() => {
		load();
	}, [api, snapshot?.tick]);

	const maker = makers?.find((m) => m.id === selected);

	return (
		<section class="panel">
			<header class="board-head">
				<div>
					<h1 class="panel-title">Market makers</h1>
					<p class="board-sub">Pick a market maker to add or remove cash, force a trade, pause it or change its settings. Pausing and settings take effect from its next tick.</p>
				</div>
			</header>
			<div class="table-wrap">
				{!makers ? (
					<p class="empty">Loading…</p>
				) : !makers.length ? (
					<p class="empty">No market makers. The server creates them when T3_MARKET_MAKER_PASSWORD is set.</p>
				) : (
					<table>
						<thead>
							<tr>
								<th>Maker</th>
								<th>Strategy</th>
								<th>Status</th>
								<th class="num">Cash</th>
								<th class="num">Available</th>
								<th class="num">Account value</th>
							</tr>
						</thead>
						<tbody>
							{makers.map((m) => (
								<tr key={m.id} class={m.id === selected ? 'clickable on' : 'clickable'} onClick={() => setSelected(m.id)}>
									<td>{m.username}</td>
									<td>{m.strategy || '—'}</td>
									<td>{makerStatus(m)}</td>
									<td class="num">{money(m.cash)}</td>
									<td class="num">{money(m.cash - m.cash_held)}</td>
									<td class="num">{money(m.total_value)}</td>
								</tr>
							))}
						</tbody>
					</table>
				)}
			</div>
			{maker && (
				// Keyed so drafts don't carry over to another maker.
				<div key={maker.id}>
					<MakerCash api={api} maker={maker} onChange={load} />
					{snapshot && <MakerTrade api={api} maker={maker} snapshot={snapshot} onChange={load} />}
					<MakerBot api={api} maker={maker} onChange={load} />
				</div>
			)}
		</section>
	);
}

interface MakerProps {
	api: Api;
	maker: AdminMaker;
	onChange: () => void;
}

function MakerCash({ api, maker, onChange }: MakerProps) {
	const [amount, setAmount] = useState('');
	const { busy, result, run } = useAction();
	const cents = parseDollars(amount);

	const adjust = (sign: 1 | -1) =>
		cents &&
		run(async () => {
			const a = await api.adjustCash(maker.id, sign * cents);
			setAmount('');
			onChange();
			return `${sign > 0 ? 'Added' : 'Removed'} ${money(cents)} ${sign > 0 ? 'to' : 'from'} ${maker.username}. Cash is now ${money(a.cash)}.`;
		});

	return (
		<>
			<h2 class="panel-title maker-section">{maker.username} · Cash</h2>
			<div class="admin-form">
				<label>
					<span>Amount</span>
					<span class="affix">
						<i>$</i>
						<input inputMode="decimal" placeholder="0.00" value={amount} onInput={(e) => setAmount(e.currentTarget.value)} />
					</span>
				</label>
				<div class="btn-row">
					<button class="small-btn primary" disabled={busy || !cents} onClick={() => adjust(1)}>
						Add cash
					</button>
					<button class="small-btn danger" disabled={busy || !cents} onClick={() => adjust(-1)}>
						Remove cash
					</button>
				</div>
			</div>
			{amount && cents === null && <p class="notice err">Enter a dollar amount, like 250 or 99.50.</p>}
			<Outcome result={result} />
		</>
	);
}

function MakerTrade({ api, maker, snapshot, onChange }: MakerProps & { snapshot: PriceSnapshot }) {
	const [symbol, setSymbol] = useState(snapshot.prices[0]?.symbol ?? '');
	const [direction, setDirection] = useState<Direction>('BUY');
	const [type, setType] = useState<OrderType>('MARKET');
	const [quantity, setQuantity] = useState('');
	const [limit, setLimit] = useState('');
	const { busy, result, run } = useAction();

	const price = snapshot.prices.find((p) => p.symbol === symbol)?.price ?? 0;
	const held = maker.holdings.find((h) => h.symbol === symbol);
	const qty = Number(quantity);
	const validQty = quantity !== '' && Number.isInteger(qty) && qty > 0;
	const limitCents = parseDollars(limit);
	const valid = validQty && (type === 'MARKET' || !!limitCents);

	const submit = (e: Event) => {
		e.preventDefault();
		if (!valid) return;
		run(async () => {
			await api.forceMakerOrder(maker.id, { symbol, direction, type, quantity: qty, ...(type === 'LIMIT' ? { limit_price: limitCents! } : {}) });
			setQuantity('');
			onChange();
			return `Placed. ${maker.username} will ${direction === 'BUY' ? 'buy' : 'sell'} up to ${shares(qty)} ${symbol} in the next auction; anything unfilled is cancelled.`;
		});
	};

	return (
		<>
			<h2 class="panel-title maker-section">{maker.username} · Force a trade</h2>
			<form class="admin-form" onSubmit={submit}>
				<label>
					<span>Symbol</span>
					<select value={symbol} onChange={(e) => setSymbol(e.currentTarget.value)}>
						{snapshot.prices.map((p) => (
							<option key={p.symbol} value={p.symbol}>
								{p.symbol} · {money(p.price)}
							</option>
						))}
					</select>
				</label>
				<label>
					<span>Shares</span>
					<input inputMode="numeric" placeholder="e.g. 500" value={quantity} onInput={(e) => setQuantity(e.currentTarget.value)} />
				</label>
				<div class="btn-row">
					<div class="segmented">
						{(['BUY', 'SELL'] as const).map((d) => (
							<button type="button" key={d} class={direction === d ? 'on' : ''} onClick={() => setDirection(d)}>
								{d === 'BUY' ? 'Buy' : 'Sell'}
							</button>
						))}
					</div>
					<div class="segmented">
						{(['MARKET', 'LIMIT'] as const).map((t) => (
							<button type="button" key={t} class={type === t ? 'on' : ''} onClick={() => setType(t)}>
								{t === 'MARKET' ? 'Market' : 'Limit'}
							</button>
						))}
					</div>
				</div>
				{type === 'LIMIT' && (
					<label>
						<span>Limit price</span>
						<span class="affix">
							<i>$</i>
							<input inputMode="decimal" placeholder={(price / 100).toFixed(2)} value={limit} onInput={(e) => setLimit(e.currentTarget.value)} />
						</span>
					</label>
				)}
				<p class="hint wide">
					{maker.username} holds {shares(held ? held.quantity - held.held : 0)} {symbol} and has {money(maker.cash - maker.cash_held)} available.
					{validQty && price > 0 && ` ${shares(qty)} ${symbol} ≈ ${money(qty * price)} at ${money(price)}.`}
					{type === 'MARKET' && direction === 'BUY' && ' A market buy spends at most twice that.'}
				</p>
				<button class="submit wide" type="submit" disabled={busy || !valid}>
					{busy ? 'Placing…' : `Force ${direction === 'BUY' ? 'buy' : 'sell'}`}
				</button>
			</form>
			{quantity && !validQty && <p class="notice err">Enter a whole number of shares.</p>}
			{type === 'LIMIT' && limit && limitCents === null && <p class="notice err">Enter a limit price, like 42.50.</p>}
			<Outcome result={result} />
		</>
	);
}

const unitLabel = { cents: ' ($)', fraction: ' (%)', ticks: ' (ticks)', '': '' };

/** A setting's value as the admin types it: dollars, percentages or plain numbers. */
function showSetting(s: MakerSetting, v: number) {
	if (s.unit === 'cents') return (v / 100).toFixed(2);
	if (s.unit === 'fraction') return String(+(v * 100).toPrecision(12));
	return String(v);
}

/** A setting's value for reading, e.g. "$500.00", "0.3%" or "10". */
function describeSetting(s: MakerSetting, v: number) {
	if (s.unit === 'cents') return money(v);
	if (s.unit === 'fraction') return `${+(v * 100).toPrecision(12)}%`;
	return v.toLocaleString();
}

/** Parses a typed setting, returning null unless it's a valid value. */
function parseSetting(s: MakerSetting, text: string): number | null {
	let v: number | null;
	if (s.unit === 'cents') v = parseDollars(text);
	else {
		v = text.trim() === '' ? NaN : Number(text);
		if (s.unit === 'fraction') v = +(v / 100).toPrecision(12);
	}
	if (v === null || !Number.isFinite(v) || v < s.min || v > s.max || (s.integer && !Number.isInteger(v))) return null;
	return v;
}

function MakerBot({ api, maker, onChange }: MakerProps) {
	const [drafts, setDrafts] = useState<Record<string, string>>({});
	const { busy, result, run } = useAction();

	const text = (s: MakerSetting) => drafts[s.key] ?? showSetting(s, s.value);
	const invalid = maker.settings.filter((s) => parseSetting(s, text(s)) === null);

	const togglePause = () =>
		run(async () => {
			const m = await api.setMakerControl(maker.id, { paused: !maker.paused });
			onChange();
			return m.paused ? `Paused. ${maker.username} stops trading from its next tick.` : `Resumed. ${maker.username} trades again from its next tick.`;
		});

	const save = (overrides: Record<string, number>) =>
		run(async () => {
			await api.setMakerControl(maker.id, { overrides });
			setDrafts({});
			onChange();
			return Object.keys(overrides).length
				? `Saved. ${maker.username} trades with these settings from its next tick.`
				: `Reset. ${maker.username} trades with its own defaults from its next tick.`;
		});

	const submit = (e: Event) => {
		e.preventDefault();
		if (invalid.length) return;
		const overrides: Record<string, number> = {};
		for (const s of maker.settings) {
			const v = parseSetting(s, text(s))!;
			if (v !== s.default) overrides[s.key] = v;
		}
		save(overrides);
	};

	return (
		<>
			<h2 class="panel-title maker-section">{maker.username} · Bot</h2>
			<div class="btn-row maker-status">
				<span class="hint">
					{maker.strategy ? `Runs the ${maker.strategy} strategy` : 'Strategy unknown'} · {makerStatus(maker)}
					{maker.seen_at && ` · last checked in ${new Date(maker.seen_at).toLocaleTimeString()}`}
				</span>
				<button class={`small-btn ${maker.paused ? 'primary' : 'danger'}`} disabled={busy} onClick={togglePause}>
					{maker.paused ? 'Resume trading' : 'Pause trading'}
				</button>
			</div>
			{!maker.settings.length ? (
				<p class="empty">
					{maker.seen_at
						? `${maker.username} has no settings to change.`
						: `${maker.username} hasn't checked in since the server started, so its settings aren't known yet. They show up here once it's running.`}
				</p>
			) : (
				<form class="admin-form" onSubmit={submit}>
					{maker.settings.map((s) => (
						<label key={s.key}>
							<span>
								{s.label}
								{unitLabel[s.unit]}
							</span>
							<input
								inputMode="decimal"
								class={invalid.includes(s) ? 'invalid' : ''}
								value={text(s)}
								onInput={(e) => setDrafts({ ...drafts, [s.key]: e.currentTarget.value })}
							/>
							<small>
								{s.help} Default {describeSetting(s, s.default)}, from {describeSetting(s, s.min)} to {describeSetting(s, s.max)}.
							</small>
						</label>
					))}
					<div class="btn-row">
						<button class="small-btn primary" type="submit" disabled={busy || !!invalid.length}>
							Save settings
						</button>
						<button class="small-btn" type="button" disabled={busy || !maker.settings.some((s) => s.overridden)} onClick={() => save({})}>
							Reset to defaults
						</button>
						<button class="small-btn" type="button" disabled={busy || !Object.keys(drafts).length} onClick={() => setDrafts({})}>
							Discard changes
						</button>
					</div>
				</form>
			)}
			{invalid.length > 0 && <p class="notice err">Check {invalid.map((s) => s.label).join(', ')}: each must be in its range{invalid.some((s) => s.integer) ? ', and whole numbers where required' : ''}.</p>}
			<Outcome result={result} />
		</>
	);
}

function NewsPanel({ api, snapshot }: { api: Api; snapshot: PriceSnapshot }) {
	const [symbol, setSymbol] = useState(snapshot.prices[0]?.symbol ?? '');
	const [quantity, setQuantity] = useState('');
	const [headline, setHeadline] = useState('');
	const [busy, setBusy] = useState(false);
	const [result, setResult] = useState<{ ok: boolean; text: string } | null>(null);

	const price = snapshot.prices.find((p) => p.symbol === symbol);
	const qty = Number(quantity);
	const valid = Number.isInteger(qty) && qty > 0 && qty <= 1_000_000;

	const submit = async (e: Event) => {
		e.preventDefault();
		if (!valid) return;
		setBusy(true);
		setResult(null);
		try {
			const a = await api.breakNews(symbol, qty, headline);
			setResult({ ok: true, text: `Published. The news market maker will market buy ${qty.toLocaleString()} ${a.symbol} in the next auction.` });
			setQuantity('');
			setHeadline('');
		} catch (err) {
			setResult({ ok: false, text: err instanceof ApiError ? err.message : 'Something went wrong. Try again.' });
		} finally {
			setBusy(false);
		}
	};

	return (
		<section class="panel">
			<header class="board-head">
				<div>
					<h1 class="panel-title">Break news</h1>
					<p class="board-sub">The headline goes on the News page, and the news market maker market buys the shares in the next auction.</p>
				</div>
			</header>
			<form class="admin-form" onSubmit={submit}>
				<label>
					<span>Symbol</span>
					<select value={symbol} onChange={(e) => setSymbol(e.currentTarget.value)}>
						{snapshot.prices.map((p) => (
							<option key={p.symbol} value={p.symbol}>
								{p.symbol} · {money(p.price)}
							</option>
						))}
					</select>
				</label>
				<label>
					<span>Shares to buy</span>
					<input inputMode="numeric" placeholder="e.g. 5000" value={quantity} onInput={(e) => setQuantity(e.currentTarget.value)} required />
				</label>
				<label class="wide">
					<span>Headline</span>
					<input maxLength={200} placeholder={`${symbol} …`} value={headline} onInput={(e) => setHeadline(e.currentTarget.value)} required />
				</label>
				<p class="hint wide">
					{price && valid ? (
						`${qty.toLocaleString()} ${symbol} ≈ ${money(qty * price.price)} at ${money(price.price)}`
					) : quantity ? (
						'Enter a whole number of shares from 1 to 1,000,000.'
					) : (
						' '
					)}
				</p>
				<button class="submit wide" type="submit" disabled={busy || !valid || !headline.trim()}>
					{busy ? 'Publishing…' : 'Publish news'}
				</button>
			</form>
			{result && (
				<p class={`notice ${result.ok ? 'ok' : 'err'}`} role="status">
					{result.text}
				</p>
			)}
		</section>
	);
}
