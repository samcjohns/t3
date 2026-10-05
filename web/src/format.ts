const usd = new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD' });
const count = new Intl.NumberFormat('en-US');
const pct = new Intl.NumberFormat('en-US', { style: 'percent', minimumFractionDigits: 2, maximumFractionDigits: 2, signDisplay: 'exceptZero' });

/** Formats integer cents as dollars, e.g. 123456 → "$1,234.56". */
export const money = (cents: number) => usd.format(cents / 100);

/** Like money, with an explicit sign, for changes. */
export const signedMoney = (cents: number) => (cents > 0 ? '+' : '') + money(cents);

export const shares = (n: number) => count.format(n);

export const percent = (change: number, base: number) => (base ? pct.format(change / base) : '—');

export const trendClass = (n: number) => (n > 0 ? 'up' : n < 0 ? 'down' : '');

/**
 * Parses a dollar amount such as "12", "12.5" or "1,012.50" into integer
 * cents without going through floating point. Returns null if malformed.
 */
export function parseDollars(input: string): number | null {
	const m = input.trim().replace(/^\$/, '').replace(/,/g, '').match(/^(\d+)(?:\.(\d{0,2}))?$/);
	if (!m) return null;
	return Number(m[1]) * 100 + Number((m[2] ?? '').padEnd(2, '0'));
}

/** Formats cents for an input field, e.g. 101050 → "1010.50". */
export const dollarsInput = (cents: number) => (cents / 100).toFixed(2);

export const clock = (iso: string) => new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });

export function shortTime(iso: string, withDate: boolean) {
	const d = new Date(iso);
	return withDate ? d.toLocaleDateString([], { month: 'short', day: 'numeric' }) : d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}
