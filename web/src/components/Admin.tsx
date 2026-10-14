import { useEffect, useState } from 'preact/hooks';
import { type AdminUser, type Api, ApiError, type PriceSnapshot, type Session } from '../api';
import { money, parseDollars, percent, trendClass } from '../format';

interface Props {
	api: Api;
	session: Session | null;
	onSession: (s: Session) => void;
	snapshot: PriceSnapshot | null;
}

/** Admin tools: adjust traders' cash and break market-moving news. Only an admin sign-in can use them. */
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

	return (
		<section class="panel">
			<header class="board-head">
				<div>
					<h1 class="panel-title">Trader cash</h1>
					<p class="board-sub">Pick a trader, then add or remove cash. Cash held for open orders can't be removed.</p>
				</div>
			</header>
			<div class="admin-form">
				<label>
					<span>Trader</span>
					<select value={selected} onChange={(e) => setSelected(e.currentTarget.value)}>
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
			</div>
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
								<th class="num">Cash</th>
								<th class="num">Available</th>
								<th class="num">Account value</th>
							</tr>
						</thead>
						<tbody>
							{users.map((u) => (
								<tr key={u.id} class={u.id === selected ? 'clickable on' : 'clickable'} onClick={() => setSelected(u.id)}>
									<td>{u.username}</td>
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

function NewsPanel({ api, snapshot }: { api: Api; snapshot: PriceSnapshot }) {
	const [symbol, setSymbol] = useState(snapshot.prices[0]?.symbol ?? '');
	const [change, setChange] = useState('');
	const [headline, setHeadline] = useState('');
	const [busy, setBusy] = useState(false);
	const [result, setResult] = useState<{ ok: boolean; text: string } | null>(null);

	const price = snapshot.prices.find((p) => p.symbol === symbol);
	const pct = Number(change);
	const valid = change.trim() !== '' && Number.isFinite(pct) && pct !== 0 && Math.abs(pct) <= 50;
	const target = price && valid ? Math.max(1, Math.round(price.price * (1 + pct / 100))) : null;

	const submit = async (e: Event) => {
		e.preventDefault();
		if (!valid) return;
		setBusy(true);
		setResult(null);
		try {
			const a = await api.breakNews(symbol, pct, headline);
			setResult({ ok: true, text: `Published. The news market maker will move ${a.symbol} from ${money(a.price)} towards ${money(a.target)}.` });
			setChange('');
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
					<p class="board-sub">The headline goes on the News page, and the news market maker trades the price towards the target over the next auctions.</p>
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
					<span>Change (%)</span>
					<input inputMode="decimal" placeholder="e.g. 8 or -12.5" value={change} onInput={(e) => setChange(e.currentTarget.value)} required />
				</label>
				<label class="wide">
					<span>Headline</span>
					<input maxLength={200} placeholder={`${symbol} …`} value={headline} onInput={(e) => setHeadline(e.currentTarget.value)} required />
				</label>
				<p class="hint wide">
					{price && target !== null ? (
						<>
							{symbol} {money(price.price)} → {money(target)}{' '}
							<span class={trendClass(target - price.price)}>({percent(target - price.price, price.price)})</span>
						</>
					) : change ? (
						'Enter a non-zero change from -50 to 50.'
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
