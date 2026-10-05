import { useCallback, useEffect, useState } from 'preact/hooks';
import type { Api, PriceSnapshot, Session } from './api';

/**
 * The live price snapshot, polled just after each auction clears. It is null
 * until the first load, and `offline` is set while the market can't be reached.
 */
export function usePrices(api: Api) {
	const [snapshot, setSnapshot] = useState<PriceSnapshot | null>(null);
	const [offline, setOffline] = useState(false);

	useEffect(() => {
		let timer = 0;
		let stopped = false;
		let lastTick = -1;
		let inFlight = false;

		const poll = async () => {
			// Showing the tab mid-request must not start a second polling loop.
			if (inFlight) return;
			inFlight = true;
			clearTimeout(timer);
			let wait = 5000;
			try {
				const snap = await api.prices();
				if (stopped) return;
				setOffline(false);
				if (snap.tick !== lastTick) {
					lastTick = snap.tick;
					setSnapshot(snap);
				}
				// Prices change once per auction, so wake just after the next one.
				// If this snapshot is stale, the next auction is overdue: retry soon.
				const untilNext = Date.parse(snap.next_tick_at) - Date.now();
				wait = untilNext > 0 ? untilNext + 400 : 1000;
			} catch {
				if (stopped) return;
				setOffline(true);
			} finally {
				inFlight = false;
			}
			// Hidden tabs poll slowly, and catch up as soon as they are shown.
			timer = window.setTimeout(poll, document.hidden ? 60_000 : Math.min(wait, 15_000));
		};
		const onVisible = () => {
			if (!document.hidden) poll();
		};

		poll();
		document.addEventListener('visibilitychange', onVisible);
		return () => {
			stopped = true;
			clearTimeout(timer);
			document.removeEventListener('visibilitychange', onVisible);
		};
	}, [api]);

	return { snapshot, offline };
}

/** The current time, updated every `ms` milliseconds. */
export function useNow(ms: number) {
	const [now, setNow] = useState(Date.now);
	useEffect(() => {
		const id = setInterval(() => setNow(Date.now()), ms);
		return () => clearInterval(id);
	}, [ms]);
	return now;
}

const SESSION_KEY = 't3.session';

function loadSession(): Session | null {
	try {
		const s: Session | null = JSON.parse(localStorage.getItem(SESSION_KEY) ?? 'null');
		return s && Date.parse(s.expires_at) > Date.now() ? s : null;
	} catch {
		return null;
	}
}

/** The signed-in session, persisted across reloads until its token expires. */
export function useSession(api: Api) {
	const [session, setSessionState] = useState(loadSession);
	api.token = session?.token;

	const setSession = useCallback(
		(s: Session | null) => {
			api.token = s?.token;
			try {
				if (s) localStorage.setItem(SESSION_KEY, JSON.stringify(s));
				else localStorage.removeItem(SESSION_KEY);
			} catch {
				// Storage can be unavailable (private windows); the session then lasts until reload.
			}
			setSessionState(s);
		},
		[api],
	);

	useEffect(() => {
		api.onUnauthorized = () => setSession(null);
	}, [api, setSession]);

	return [session, setSession] as const;
}

export type Page = 'trade' | 'portfolio';

/**
 * The page and symbol in the URL hash: #/ACME for a symbol's trading view, or
 * #/portfolio. Both survive reloads and can be linked.
 */
export function useRoute() {
	const read = () => {
		const path = decodeURIComponent(location.hash.replace(/^#\/?/, ''));
		return path === 'portfolio' ? { page: 'portfolio' as Page, symbol: '' } : { page: 'trade' as Page, symbol: path.toUpperCase() };
	};
	const [route, setRoute] = useState(read);
	useEffect(() => {
		const onHash = () => setRoute(read());
		addEventListener('hashchange', onHash);
		return () => removeEventListener('hashchange', onHash);
	}, []);
	/** Switches symbol within the trading view, without adding history entries. */
	const select = useCallback((s: string) => {
		history.replaceState(null, '', `#/${encodeURIComponent(s)}`);
		setRoute({ page: 'trade', symbol: s });
	}, []);
	/** Moves between pages, so the back button returns. */
	const open = useCallback((page: Page, symbol = '') => {
		location.hash = page === 'portfolio' ? '#/portfolio' : `#/${encodeURIComponent(symbol)}`;
	}, []);
	return { ...route, select, open };
}
