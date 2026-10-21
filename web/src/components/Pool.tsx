import { useCallback, useEffect, useState } from 'preact/hooks';
import { type Api, ApiError, type PoolView, type Session } from '../api';
import { bps, dollarsInput, money, parseDollars } from '../format';
import { AuthPanel } from './AuthPanel';

interface Props {
	api: Api;
	session: Session | null;
	onSession: (s: Session) => void;
	tick: number;
	/** Called after the account's cash changes, to refresh it elsewhere. */
	onChange: () => void;
}

/** The lending pool: how big it is, what it pays lenders, and what leverage costs. Signed-in traders lend, withdraw and repay here. */
export function Pool({ api, session, onSession, tick, onChange }: Props) {
	const [view, setView] = useState<PoolView | null>(null);
	const [failed, setFailed] = useState(false);

	const load = useCallback(() => {
		api.pool().then(
			(v) => {
				setView(v);
				setFailed(false);
			},
			() => setFailed(true),
		);
	}, [api]);
	useEffect(load, [load, tick, session]);

	const pool = view?.pool;

	return (
		<main class="board pool-page">
			<section class="panel">
				<header class="board-head">
					<div>
						<h1 class="panel-title">Lending pool</h1>
						<p class="board-sub">
							Lend cash to traders who buy with leverage. Lenders earn the interest borrowers pay, and share the loss when a borrower goes bankrupt.
						</p>
					</div>
				</header>
				{!pool ? (
					failed ? <p class="empty">Couldn't load the pool. Retrying at the next auction.</p> : <p class="empty">Loading…</p>
				) : (
					<>
						<div class="folio-head">
							<div>
								<h2 class="stat-label">Pool size</h2>
								<p class="hero">{money(pool.value)}</p>
								<p class="hero-change muted">
									{pool.lenders.toLocaleString()} {pool.lenders === 1 ? 'lender' : 'lenders'} · {pool.borrowers.toLocaleString()}{' '}
									{pool.borrowers === 1 ? 'borrower' : 'borrowers'}
								</p>
							</div>
							<dl class="folio-stats">
								<div>
									<dt>Lenders earn</dt>
									<dd class={pool.lender_rate_bps > 0 ? 'up' : ''}>{bps(pool.lender_rate_bps)} a day</dd>
								</div>
								<div>
									<dt>Lent out</dt>
									<dd>
										{money(pool.lent)} <span class="muted">({bps(pool.utilization_bps)})</span>
									</dd>
								</div>
								<div>
									<dt>Available</dt>
									<dd>{money(pool.available)}</dd>
								</div>
							</dl>
						</div>
						<p class="hint pool-explain">
							Lenders earn only on the part of the pool that's lent out, so the rate rises as more of it is borrowed. When nobody is borrowing,
							lenders earn nothing.
						</p>

						<h2 class="panel-title pool-rates-title">Borrowing</h2>
						<div class="table-scroll">
							<table>
								<thead>
									<tr>
										<th>Leverage</th>
										<th class="num">Interest a day</th>
										<th class="num">You pay</th>
										<th class="num">Borrowed</th>
									</tr>
								</thead>
								<tbody>
									{pool.rates.map((r) => (
										<tr key={r.ratio}>
											<td class="sym">{r.ratio}x</td>
											<td class="num">{bps(r.rate_bps)}</td>
											<td class="num">{bps(10_000 / r.ratio)} of the cost</td>
											<td class="num">{bps(10_000 - 10_000 / r.ratio)}</td>
										</tr>
									))}
								</tbody>
							</table>
						</div>
						<p class="hint">
							Pick a leverage ratio on the order ticket when you buy. One order can borrow up to {money(pool.max_trade_loan)} (
							{bps(pool.max_trade_loan_bps)} of the pool), and one trader can owe up to {money(pool.max_trader_loan)} (
							{bps(pool.max_trader_loan_bps)}). Sales repay your loan first. If your equity falls below{' '}
							{bps(pool.maintenance_margin_bps)} of your holdings, your shares are sold at market to repay it. If that isn't enough, the rest is
							written off and your account is closed for good.
						</p>
					</>
				)}
			</section>

			{!session ? (
				<section class="panel pool-signin">
					<p class="muted">Sign in to lend to the pool.</p>
					<AuthPanel api={api} onSession={onSession} />
				</section>
			) : (
				view?.you && (
					<Stake
						api={api}
						view={view}
						onDone={(v) => {
							setView(v);
							onChange();
						}}
					/>
				)
			)}
		</main>
	);
}

function Stake({ api, view, onDone }: { api: Api; view: PoolView; onDone: (v: PoolView) => void }) {
	const you = view.you!;
	const [amount, setAmount] = useState('');
	const [busy, setBusy] = useState(false);
	const [result, setResult] = useState<{ ok: boolean; text: string } | null>(null);
	const cents = parseDollars(amount);
	const borrowing = you.owed > 0;

	const run = async (action: () => Promise<PoolView>, done: (v: PoolView) => string) => {
		setBusy(true);
		setResult(null);
		try {
			const v = await action();
			setResult({ ok: true, text: done(v) });
			setAmount('');
			onDone(v);
		} catch (err) {
			setResult({ ok: false, text: err instanceof ApiError ? err.message : 'Something went wrong. Try again.' });
		} finally {
			setBusy(false);
		}
	};

	const canBorrow = Math.max(0, Math.min(view.pool.max_trader_loan - you.owed, view.pool.available));

	if (you.status === 'bankrupt') {
		return (
			<section class="panel">
				<p class="notice err">Your account went bankrupt and is closed. It can no longer trade or lend.</p>
			</section>
		);
	}

	return (
		<section class="panel">
			<header class="board-head">
				<div>
					<h2 class="panel-title">{borrowing ? 'Your loan' : 'Your stake'}</h2>
					<p class="board-sub">
						{borrowing
							? "You owe the pool, so you can't lend to it. Interest is added every auction until you repay."
							: 'Your stake grows with the interest borrowers pay. Only cash the pool hasn’t lent out can be withdrawn.'}
					</p>
				</div>
				<dl class="folio-stats">
					<div>
						<dt>{borrowing ? 'You owe' : 'In the pool'}</dt>
						<dd class={borrowing ? 'down' : ''}>{money(borrowing ? you.owed : you.pool_value)}</dd>
					</div>
					<div>
						<dt>Cash available</dt>
						<dd>{money(you.cash_available)}</dd>
					</div>
					{borrowing && (
						<div>
							<dt>Can still borrow</dt>
							<dd>{money(canBorrow)}</dd>
						</div>
					)}
				</dl>
			</header>
			{you.status === 'liquidating' && (
				<p class="notice err">Your equity fell below the maintenance margin. Your shares are being sold to repay your loan.</p>
			)}
			<div class="admin-form">
				<label>
					<span>Amount</span>
					<div class="with-max">
						<span class="affix">
							<i>$</i>
							<input inputMode="decimal" placeholder="0.00" value={amount} onInput={(e) => setAmount(e.currentTarget.value)} />
						</span>
						<button
							type="button"
							class="max"
							disabled={busy}
							title={borrowing ? 'Repay as much as you can' : 'Lend all your available cash'}
							onClick={() => setAmount(dollarsInput(borrowing ? Math.min(you.owed, you.cash_available) : you.cash_available))}
						>
							Max
						</button>
					</div>
				</label>
				<div class="btn-row">
					{borrowing ? (
						<button class="small-btn primary" disabled={busy || !cents} onClick={() => run(() => api.repay(cents!), (v) => `Repaid. You now owe ${money(v.you?.owed ?? 0)}.`)}>
							Repay
						</button>
					) : (
						<>
							<button
								class="small-btn primary"
								disabled={busy || !cents || you.status !== 'active'}
								onClick={() => run(() => api.lend(cents!), (v) => `Lent ${money(cents!)}. Your stake is worth ${money(v.you?.pool_value ?? 0)}.`)}
							>
								Lend
							</button>
							<button
								class="small-btn"
								disabled={busy || !cents || you.pool_value === 0}
								onClick={() => run(() => api.withdrawFromPool(cents!), () => `Withdrew ${money(cents!)}.`)}
							>
								Withdraw
							</button>
							<button
								class="small-btn"
								disabled={busy || you.pool_value === 0}
								onClick={() => run(() => api.withdrawFromPool('all'), () => 'Withdrew your whole stake.')}
							>
								Withdraw all
							</button>
						</>
					)}
				</div>
			</div>
			{amount && cents === null && <p class="notice err">Enter a dollar amount, like 250 or 99.50.</p>}
			{result && (
				<p class={`notice ${result.ok ? 'ok' : 'err'}`} role="status">
					{result.text}
				</p>
			)}
		</section>
	);
}
