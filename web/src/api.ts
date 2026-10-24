// A typed client for the t3 public API (docs/api.md). Money is in integer
// cents throughout.

export interface User {
	id: string;
	username: string;
	role: 'trader' | 'admin' | 'market_maker';
	created_at: string;
}

/** A long-lived API token, described without revealing it. */
export interface ApiToken {
	/** The token's first characters, e.g. "t3_AbCd1234". */
	hint: string;
	created_at: string;
}

export interface Profile {
	user: User;
	api_token: ApiToken | null;
}

export interface Session {
	token: string;
	expires_at: string;
	user: User;
}

export interface Price {
	symbol: string;
	name: string;
	/** Last clearing price, or the opening price if it has never traded. */
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

/** An account's standing with the lending pool. A liquidating account can't trade while its shares are sold; a bankrupt one is closed. */
export type AccountStatus = 'active' | 'liquidating' | 'bankrupt';

export interface Portfolio {
	account_id: string;
	/** Total cash, including cash_held. */
	cash: number;
	/** Cash reserved for open orders. */
	cash_held: number;
	positions: Position[];
	market_value: number;
	/** The account's stake in the lending pool. */
	pool_value: number;
	/** What the account owes the lending pool, interest included. */
	owed: number;
	/** Cash plus holdings plus pool stake, less what is owed. */
	total_value: number;
	status: AccountStatus;
}

export interface Standing {
	/** Shared by equal values: two traders tied for first are both 1. */
	rank: number;
	username: string;
	total_value: number;
	/** total_value less the starting cash. */
	gain: number;
	/** What the trader's stake in the lending pool is worth, which total_value includes. */
	pool_value: number;
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

/** A trader closed by liquidation, with debt they couldn't repay. */
export interface Bankruptcy {
	username: string;
	tick: number;
	time: string;
	/** The debt left unpaid, which the pool's lenders lost. */
	written_off: number;
}

/** What borrowing at one leverage ratio costs, in basis points per day. */
export interface RatioRate {
	ratio: number;
	rate_bps: number;
}

/** The lending pool. Rates are in basis points per day. */
export interface PoolSummary {
	/** Cash plus lent. */
	value: number;
	/** Cash not lent out. */
	cash: number;
	/** Cash not lent out or set aside for open orders: what can be borrowed or withdrawn. */
	available: number;
	/** What borrowers owe, interest included. */
	lent: number;
	utilization_bps: number;
	/** What lenders earn now: the interest borrowers pay, spread over the whole pool. */
	lender_rate_bps: number;
	base_rate_bps: number;
	rates: RatioRate[];
	/** Equity, as a share of holdings, below which a borrower is liquidated. */
	maintenance_margin_bps: number;
	/** The most one order may borrow now, and that limit as a share of value. */
	max_trade_loan: number;
	max_trade_loan_bps: number;
	/** The most one trader may owe now, and that limit as a share of value. */
	max_trader_loan: number;
	max_trader_loan_bps: number;
	lenders: number;
	borrowers: number;
}

/** The signed-in trader's side of the pool. */
export interface PoolStake {
	status: AccountStatus;
	/** Cash not held for open orders. */
	cash_available: number;
	pool_shares: number;
	pool_value: number;
	owed: number;
}

export interface PoolView {
	pool: PoolSummary;
	you: PoolStake | null;
}

/** A market news story. */
export interface Article {
	id: string;
	tick: number;
	time: string;
	symbol: string;
	headline: string;
	body?: string;
	/** The symbol's price when the story broke. */
	price: number;
	/** The price the story moves it towards, or 0 for admin news, which has none. */
	target: number;
}

/** A trader as the admin page lists them. */
export interface AdminUser {
	id: string;
	username: string;
	status: 'active' | 'liquidating' | 'bankrupt';
	/** Blocked traders can't place orders or lend. */
	blocked: boolean;
	/** Total cash, including cash_held. */
	cash: number;
	cash_held: number;
	total_value: number;
}

/** One of a market maker's tunable settings, as the maker reports it. */
export interface MakerSetting {
	key: string;
	label: string;
	/** "fraction" values are shown as percentages. */
	unit: 'cents' | 'fraction' | 'ticks' | '';
	/** The maker's own value, used unless overridden. */
	default: number;
	min: number;
	max: number;
	integer: boolean;
	help?: string;
	/** The value it trades with. */
	value: number;
	overridden: boolean;
}

/** A market maker as the admin page lists them. */
export interface AdminMaker {
	id: string;
	username: string;
	/** Empty until the maker first syncs after a server start. */
	strategy: string;
	/** Synced within the last minute. */
	online: boolean;
	seen_at: string | null;
	paused: boolean;
	settings: MakerSetting[];
	/** Total cash, including cash_held. */
	cash: number;
	cash_held: number;
	total_value: number;
	holdings: { symbol: string; quantity: number; held: number }[];
}

/** The ledger's view of an account, as admin cash changes return it. */
export interface Account {
	cash: number;
	cash_held: number;
	status: AdminUser['status'];
	blocked: boolean;
}

export interface OrderRequest {
	symbol: string;
	direction: Direction;
	type: OrderType;
	quantity: number;
	limit_price?: number;
	max_cost?: number;
	time_in_force?: TimeInForce;
	/** A buy's leverage ratio: it borrows all but 1/leverage of its cost from the lending pool. */
	leverage?: number;
}

export interface Order extends Required<OrderRequest> {
	id: string;
	account_id: string;
	sequence: number;
	status: 'accepted';
}

/** One of your orders that hasn't filled or expired yet. */
export interface OpenOrder {
	id: string;
	symbol: string;
	direction: Direction;
	type: OrderType;
	/** Set on limit orders. */
	time_in_force?: TimeInForce;
	/** What was ordered; `remaining` is what is still unfilled. */
	quantity: number;
	remaining: number;
	limit_price: number;
	max_cost: number;
	leverage: number;
	/** What the order still sets aside, which cancelling frees. */
	cash_held: number;
	shares_held: number;
	created_at: string;
	/** "cancelling" once you've asked to cancel it, until the next auction removes it. */
	status: 'open' | 'cancelling';
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

	constructor(readonly base: string) {}

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

	profile() {
		return this.request<Profile>('GET', '/v1/account/profile');
	}
	changePassword(current_password: string, new_password: string) {
		return this.request<void>('POST', '/v1/account/password', { current_password, new_password });
	}
	/** Issues a new API token, revoking any old one. The token is only ever returned here. */
	createApiToken() {
		return this.request<ApiToken & { token: string }>('POST', '/v1/account/api-token');
	}
	deleteApiToken() {
		return this.request<void>('DELETE', '/v1/account/api-token');
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
	async bankruptcies(limit = 100) {
		return (await this.request<{ bankruptcies: Bankruptcy[] }>('GET', `/v1/leaderboard/bankruptcies?limit=${limit}`)).bankruptcies;
	}
	pool() {
		return this.request<PoolView>('GET', '/v1/pool');
	}
	lend(amount: number) {
		return this.request<PoolView>('POST', '/v1/pool/lend', { amount });
	}
	/** Takes cash out of your pool stake, or all of it. */
	withdrawFromPool(amount: number | 'all') {
		return this.request<PoolView>('POST', '/v1/pool/withdraw', amount === 'all' ? { all: true } : { amount });
	}
	repay(amount: number) {
		return this.request<PoolView>('POST', '/v1/pool/repay', { amount });
	}
	/** News, newest first, for one symbol or all of them. */
	async news(symbol?: string, limit = 100) {
		const q = symbol ? `&symbol=${encodeURIComponent(symbol)}` : '';
		return (await this.request<{ articles: Article[] }>('GET', `/v1/news?limit=${limit}${q}`)).articles;
	}
	placeOrder(order: OrderRequest) {
		return this.request<Order>('POST', '/v1/orders', order);
	}
	async openOrders() {
		return (await this.request<{ orders: OpenOrder[] }>('GET', '/v1/orders')).orders;
	}
	/** Removes an open order at the next auction, freeing what it holds. Any part that fills in that auction first still counts. */
	cancelOrder(id: string) {
		return this.request<OpenOrder>('DELETE', `/v1/orders/${encodeURIComponent(id)}`);
	}

	async adminUsers() {
		return (await this.request<{ users: AdminUser[] }>('GET', '/v1/admin/users')).users;
	}
	/** Adds cash to an account, or removes it when amount is negative. */
	adjustCash(accountId: string, amount: number) {
		return this.request<Account>('POST', `/v1/admin/accounts/${encodeURIComponent(accountId)}/cash`, { amount });
	}
	/** Blocks a trader from placing orders and lending, cancelling their open orders, or lifts the block. */
	setBlocked(accountId: string, blocked: boolean) {
		return this.request<Account>('POST', `/v1/admin/accounts/${encodeURIComponent(accountId)}/block`, { blocked });
	}
	/** Cancels a trader's open orders and market sells all their shares. */
	liquidate(accountId: string) {
		return this.request<Account>('POST', `/v1/admin/accounts/${encodeURIComponent(accountId)}/liquidate`);
	}
	deleteUser(userId: string) {
		return this.request<void>('DELETE', `/v1/admin/users/${encodeURIComponent(userId)}`);
	}
	async adminMakers() {
		return (await this.request<{ makers: AdminMaker[] }>('GET', '/v1/admin/makers')).makers;
	}
	/** Pauses or resumes a market maker, or replaces its setting overrides. Omitted fields are unchanged. */
	setMakerControl(makerId: string, control: { paused?: boolean; overrides?: Record<string, number> }) {
		return this.request<AdminMaker>('POST', `/v1/admin/makers/${encodeURIComponent(makerId)}/control`, control);
	}
	/** Places an order for a market maker. It defaults to IOC; a market buy without max_cost may spend up to twice its quantity at the last price. */
	forceMakerOrder(makerId: string, order: Omit<OrderRequest, 'leverage'>) {
		return this.request<Order>('POST', `/v1/admin/makers/${encodeURIComponent(makerId)}/orders`, order);
	}
	/** Publishes a story and has the news market maker market buy `quantity` shares. */
	breakNews(symbol: string, quantity: number, headline: string) {
		return this.request<Article>('POST', '/v1/admin/news', { symbol, quantity, headline });
	}
}
