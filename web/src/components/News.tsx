import { useEffect, useState } from 'preact/hooks';
import type { Api, Article } from '../api';
import { clock, money, percent, trendClass } from '../format';

interface Props {
	api: Api;
	symbols: string[];
	tick: number;
	onSelect: (symbol: string) => void;
}

/** Market news, newest first. Each story moves its symbol's price over the next few auctions. */
export function News({ api, symbols, tick, onSelect }: Props) {
	const [symbol, setSymbol] = useState('');
	const [articles, setArticles] = useState<Article[] | null>(null);
	const [failed, setFailed] = useState(false);

	useEffect(() => {
		let live = true;
		api.news(symbol || undefined).then(
			(a) => {
				if (!live) return;
				setArticles(a);
				setFailed(false);
			},
			() => live && setFailed(true),
		);
		return () => {
			live = false;
		};
	}, [api, tick, symbol]);

	// Switching filters shows a fresh list rather than the old one.
	useEffect(() => setArticles(null), [symbol]);

	return (
		<main class="board">
			<section class="panel">
				<header class="board-head">
					<div>
						<h1 class="panel-title">News</h1>
						<p class="board-sub">Stories that move the market. A price can take a few auctions to react.</p>
					</div>
					<label class="news-filter">
						<span>Symbol</span>
						<select value={symbol} onChange={(e) => setSymbol(e.currentTarget.value)}>
							<option value="">All</option>
							{symbols.map((s) => (
								<option key={s} value={s}>
									{s}
								</option>
							))}
						</select>
					</label>
				</header>

				{!articles ? (
					failed ? <p class="empty">Couldn't load the news. Retrying at the next auction.</p> : <p class="empty">Loading…</p>
				) : !articles.length ? (
					<p class="empty">No news{symbol && ` for ${symbol}`} yet.</p>
				) : (
					<ol class="news">
						{articles.map((a) => (
							<li key={a.id}>
								<time dateTime={a.time} title={new Date(a.time).toLocaleString()}>
									{when(a.time)}
								</time>
								<div class="news-story">
									<h2>{a.headline}</h2>
									{a.body && <p>{a.body}</p>}
									<p class="news-meta">
										<button class="link" onClick={() => onSelect(a.symbol)} title={`Trade ${a.symbol}`}>
											{a.symbol}
										</button>
										<span class="muted">at {money(a.price)}</span>
									</p>
								</div>
								<span class={`news-move ${trendClass(a.target - a.price)}`}>{percent(a.target - a.price, a.price)}</span>
							</li>
						))}
					</ol>
				)}
			</section>
		</main>
	);
}

/** The time of day for today's stories, with the date for older ones. */
function when(iso: string) {
	const d = new Date(iso);
	if (d.toDateString() === new Date().toDateString()) return clock(iso);
	return `${d.toLocaleDateString([], { month: 'short', day: 'numeric' })} ${d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}`;
}
