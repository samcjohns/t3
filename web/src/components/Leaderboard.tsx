import { useEffect, useState } from 'preact/hooks';
import type { Api, Leaderboard as Board, Standing } from '../api';
import { money, percent, signedMoney, trendClass } from '../format';

interface Props {
	api: Api;
	/** The signed-in trader, whose row is highlighted. */
	username?: string;
	tick: number;
}

/** Every trader ranked by account value. They all start with the same cash, so this is a fair race. */
export function Leaderboard({ api, username, tick }: Props) {
	const [board, setBoard] = useState<Board | null>(null);
	const [failed, setFailed] = useState(false);

	useEffect(() => {
		let live = true;
		api.leaderboard().then(
			(b) => {
				if (!live) return;
				setBoard(b);
				setFailed(false);
			},
			() => live && setFailed(true),
		);
		return () => {
			live = false;
		};
	}, [api, tick, username]);

	const you = board?.you;
	const youShown = !!you && board!.standings.some((s) => s.username === you.username);

	return (
		<main class="board">
			<section class="panel">
				<header class="board-head">
					<div>
						<h1 class="panel-title">Leaderboard</h1>
						<p class="board-sub">
							Ranked by account value: cash plus holdings at last prices.
							{board && ` Every trader starts with ${money(board.starting_cash)}.`}
						</p>
					</div>
					{you && board && (
						<dl class="folio-stats">
							<div>
								<dt>Your rank</dt>
								<dd>
									#{you.rank.toLocaleString()} <span class="muted">of {board.traders.toLocaleString()}</span>
								</dd>
							</div>
							<div>
								<dt>Your return</dt>
								<dd class={trendClass(you.gain)}>
									{signedMoney(you.gain)} ({percent(you.gain, board.starting_cash)})
								</dd>
							</div>
						</dl>
					)}
				</header>

				{!board ? (
					failed ? <p class="empty">Couldn't load the leaderboard. Retrying at the next auction.</p> : <p class="empty">Loading…</p>
				) : !board.standings.length ? (
					<p class="empty">No traders yet. Register to be the first.</p>
				) : (
					<div class="table-scroll">
						<table class="standings">
							<thead>
								<tr>
									<th class="num rank-col">Rank</th>
									<th>Trader</th>
									<th class="num">Account value</th>
									<th class="num">Return</th>
								</tr>
							</thead>
							<tbody>
								{board.standings.map((s) => (
									<Row key={s.username} s={s} start={board.starting_cash} mine={s.username === username} />
								))}
								{you && !youShown && (
									<>
										<tr class="gap" aria-hidden="true">
											<td colSpan={4}>⋯</td>
										</tr>
										<Row s={you} start={board.starting_cash} mine />
									</>
								)}
							</tbody>
						</table>
					</div>
				)}
				{board && board.traders > board.standings.length && (
					<p class="hint">
						Showing the top {board.standings.length} of {board.traders.toLocaleString()} traders.
					</p>
				)}
			</section>
		</main>
	);
}

function Row({ s, start, mine }: { s: Standing; start: number; mine: boolean }) {
	return (
		<tr class={mine ? 'mine' : ''} aria-current={mine || undefined}>
			<td class="num rank-col">
				<span class={s.rank <= 3 ? `medal m${s.rank}` : 'rank'}>{s.rank}</span>
			</td>
			<td class="sym">
				{s.username}
				{mine && <span class="you-tag">You</span>}
			</td>
			<td class="num">{money(s.total_value)}</td>
			<td class={`num ${trendClass(s.gain)}`}>
				{signedMoney(s.gain)}
				<small class="sub">{percent(s.gain, start)}</small>
			</td>
		</tr>
	);
}
