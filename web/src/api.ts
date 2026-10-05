// A typed client for the t3 public API (docs/api.md). Money is in integer
// cents throughout.

export interface User {
	id: string;
	username: string;
	role: 'trader' | 'admin' | 'market_maker';
}

export interface Session {
	token: string;
	expires_at: string;
	user: User;
}

export interface Price {
	symbol: string;
	name: string;
	/** Last clearing price, or the reference price if it has never traded. */
	price: number;
	previous_close: number;
	change: number;
	/** Shares traded so far this UTC day. */
	volume: number;
	last_trade_tick: number;
}

export interface PriceSnapshot {
	tick: number;
	as_of: string;
	/** When the next auction clears and prices next change. */
	next_tick_at: string;
	prices: Price[];
}

export interface Candle {
	start: string;
	open: number;
	high: number;
	low: number;
	close: number;
	volume: number;
}

export type Interval = '1m' | '5m' | '15m' | '1h' | '1d';

export interface Trade {
	tick: number;
	time: string;
	symbol: string;
	price: number;
	quantity: number;
}

export type Direction = 'BUY' | 'SELL';
export type OrderType = 'LIMIT' | 'MARKET';
export type TimeInForce = 'GTC' | 'IOC';

export interface Fill extends Trade {
	order_id: string;
	direction: Direction;
}

export interface Position {
	symbol: string;
	quantity: number;
	held: number;
	last_price: number;
	value: number;
	/** What the shares cost at their average purchase price, or null if some predate the trade history. */
	cost_basis: number | null;
}

/** An account's total value (cash plus holdings) over one candle period. */
export interface ValueCandle {
	start: string;
	open: number;
	high: number;
	low: number;
	close: number;
}

export interface Portfolio {
	account_id: string;
	/** Total cash, including cash_held. */
	cash: number;
	/** Cash reserved for open orders. */
	cash_held: number;
	positions: Position[];
	market_value: number;
	total_value: number;
}

export interface Standing {
	/** Shared by equal values: two traders tied for first are both 1. */
	rank: number;
	username: string;
	total_value: number;
	/** total_value less the starting cash. */
	gain: number;
}

export interface Leaderboard {
	tick: number;
	/** What every trader starts with. */
	starting_cash: number;
	/** How many traders are ranked in all. */
	traders: number;
	standings: Standing[];
	/** The signed-in trader's own standing, even outside the top. */
	you: Standing | null;
}

export interface OrderRequest {
	symbol: string;
	direction: Direction;
	type: OrderType;
	quantity: number;
	limit_price?: number;
	max_cost?: number;
	time_in_force?: TimeInForce;
}

export interface Order extends Required<OrderRequest> {
	id: string;
	account_id: string;
	sequence: number;
	status: 'accepted';
}

export class ApiError extends Error {
	constructor(
		readonly status: number,
		readonly code: string,
		message: string,
	) {
		super(message);
	}
}

export class Api {
	/** Called when a request is rejected for a missing or expired token. */
	onUnauthorized?: () => void;
	token?: string;

	constructor(private readonly base: string) {}

	private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
		const headers: Record<string, string> = {};
		if (body !== undefined) headers['Content-Type'] = 'application/json';
		if (this.token) headers.Authorization = `Bearer ${this.token}`;
		let res: Response;
		try {
			res = await fetch(this.base + path, {
				method,
				headers,
				body: body === undefined ? undefined : JSON.stringify(body),
				// Revalidates with ETags, so unchanged data comes back as a cheap 304.
				cache: 'no-cache',
			});
		} catch {
			throw new ApiError(0, 'network_error', 'Cannot reach the market. Check your connection.');
		}
		if (res.status === 204) return undefined as T;
		const data = await res.json().catch(() => null);
		if (!res.ok) {
			const err = data?.error;
			if (res.status === 401 && this.token) this.onUnauthorized?.();
			if (res.status === 429) {
				throw new ApiError(429, 'rate_limited', `Too many requests. Try again in ${res.headers.get('Retry-After') ?? 'a few'} seconds.`);
			}
			throw new ApiError(res.status, err?.code ?? 'internal_error', err?.message ?? `Request failed (${res.status})`);
		}
		return data as T;
	}

	register(username: string, password: string) {
		return this.request<User>('POST', '/v1/auth/register', { username, password });
	}
	login(username: string, password: string) {
		return this.request<Session>('POST', '/v1/auth/login', { username, password });
	}
	logout() {
		return this.request<void>('POST', '/v1/auth/logout');
	}

	prices() {
		return this.request<PriceSnapshot>('GET', '/v1/market/prices');
	}
	async candles(symbol: string, interval: Interval, limit = 120) {
		const q = `interval=${interval}&limit=${limit}`;
		return (await this.request<{ candles: Candle[] }>('GET', `/v1/market/${encodeURIComponent(symbol)}/candles?${q}`)).candles;
	}
	async trades(symbol: string, limit = 40) {
		return (await this.request<{ trades: Trade[] }>('GET', `/v1/market/${encodeURIComponent(symbol)}/trades?limit=${limit}`)).trades;
	}

	portfolio() {
		return this.request<Portfolio>('GET', '/v1/account/portfolio');
	}
	async fills(limit = 100) {
		return (await this.request<{ trades: Fill[] }>('GET', `/v1/account/trades?limit=${limit}`)).trades;
	}
	async history(interval: Interval, limit = 120) {
		return (await this.request<{ candles: ValueCandle[] }>('GET', `/v1/account/history?interval=${interval}&limit=${limit}`)).candles;
	}
	leaderboard(limit = 100) {
		return this.request<Leaderboard>('GET', `/v1/leaderboard?limit=${limit}`);
	}
	placeOrder(order: OrderRequest) {
		return this.request<Order>('POST', '/v1/orders', order);
	}
}
