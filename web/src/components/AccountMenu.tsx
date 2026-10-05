import { useEffect, useRef, useState } from 'preact/hooks';
import { type Api, ApiError, type Profile, type User } from '../api';

interface Props {
	api: Api;
	user: User;
}

const date = (iso: string) => new Date(iso).toLocaleDateString([], { year: 'numeric', month: 'short', day: 'numeric' });

/** How long ago iso was, in its largest whole unit, e.g. "3 days". */
function age(iso: string, now = Date.now()) {
	const mins = Math.max(0, (now - Date.parse(iso)) / 60_000);
	const units: [string, number][] = [
		['year', 525_600],
		['month', 43_800],
		['day', 1_440],
		['hour', 60],
		['minute', 1],
	];
	for (const [unit, size] of units) {
		const n = Math.floor(mins / size);
		if (n >= 1) return `${n} ${unit}${n === 1 ? '' : 's'}`;
	}
	return 'under a minute';
}

const message = (err: unknown) => (err instanceof ApiError ? err.message : 'Something went wrong. Try again.');

/** The signed-in username, which opens a popup with account details, the API token and a password change. */
export function AccountMenu({ api, user }: Props) {
	const [open, setOpen] = useState(false);
	const root = useRef<HTMLDivElement>(null);
	const button = useRef<HTMLButtonElement>(null);

	useEffect(() => {
		if (!open) return;
		const onDown = (e: PointerEvent) => {
			if (!root.current?.contains(e.target as Node)) setOpen(false);
		};
		const onKey = (e: KeyboardEvent) => {
			if (e.key === 'Escape') {
				setOpen(false);
				button.current?.focus();
			}
		};
		document.addEventListener('pointerdown', onDown);
		document.addEventListener('keydown', onKey);
		return () => {
			document.removeEventListener('pointerdown', onDown);
			document.removeEventListener('keydown', onKey);
		};
	}, [open]);

	return (
		<div class="account" ref={root}>
			<button ref={button} class="account-btn" aria-expanded={open} aria-haspopup="dialog" onClick={() => setOpen(!open)}>
				{user.username}
				<span aria-hidden="true">▾</span>
			</button>
			{open && <AccountPopup api={api} user={user} />}
		</div>
	);
}

function AccountPopup({ api, user }: Props) {
	const [profile, setProfile] = useState<Profile | null>(null);
	const [error, setError] = useState('');

	useEffect(() => {
		api.profile().then(setProfile, (err) => setError(message(err)));
	}, [api]);

	const created = profile?.user.created_at ?? user.created_at;

	return (
		<div class="popup" role="dialog" aria-label="Your account">
			<section class="popup-head">
				<span class="avatar" aria-hidden="true">
					{user.username.slice(0, 1).toUpperCase()}
				</span>
				<div>
					<b>{user.username}</b>
					<p class="muted">{created ? `Joined ${date(created)} · ${age(created)} ago` : ' '}</p>
				</div>
			</section>

			<section class="popup-section">
				<h3>API token</h3>
				{error ? <p class="notice err">{error}</p> : !profile ? <p class="hint">Loading…</p> : <TokenPanel api={api} profile={profile} />}
			</section>

			<section class="popup-section">
				<h3>Change password</h3>
				<PasswordForm api={api} />
			</section>
		</div>
	);
}

function TokenPanel({ api, profile }: { api: Api; profile: Profile }) {
	const [token, setToken] = useState(profile.api_token);
	const [fresh, setFresh] = useState('');
	const [confirm, setConfirm] = useState<'regenerate' | 'revoke' | null>(null);
	const [busy, setBusy] = useState(false);
	const [copied, setCopied] = useState(false);
	const [error, setError] = useState('');
	const input = useRef<HTMLInputElement>(null);

	const run = async (action: () => Promise<void>) => {
		setBusy(true);
		setError('');
		try {
			await action();
			setConfirm(null);
		} catch (err) {
			setError(message(err));
		} finally {
			setBusy(false);
		}
	};
	const create = () =>
		run(async () => {
			const t = await api.createApiToken();
			setToken({ hint: t.hint, created_at: t.created_at });
			setFresh(t.token);
			setCopied(false);
		});
	const revoke = () =>
		run(async () => {
			await api.deleteApiToken();
			setToken(null);
			setFresh('');
		});
	const copy = async () => {
		try {
			await navigator.clipboard.writeText(fresh);
			setCopied(true);
		} catch {
			input.current?.select(); // clipboard blocked: select it for a manual copy
		}
	};

	return (
		<div class="token">
			<p class="hint">Lets a bot or script trade on this account. It can't change your password or tokens.</p>
			<dl class="api-info">
				<div>
					<dt>API URL</dt>
					<dd>
						<code>{api.base}</code>
					</dd>
				</div>
				<div>
					<dt>Send as</dt>
					<dd>
						<code>Authorization: Bearer &lt;token&gt;</code>
					</dd>
				</div>
			</dl>
			{fresh ? (
				<>
					<div class="copy-row">
						<input ref={input} readOnly value={fresh} aria-label="New API token" onFocus={(e) => e.currentTarget.select()} />
						<button class="small-btn" onClick={copy}>
							{copied ? 'Copied' : 'Copy'}
						</button>
					</div>
					<p class="notice warn">Copy it now. For your security it won't be shown again.</p>
				</>
			) : token ? (
				<p class="token-hint">
					<code>{token.hint}…</code>
					<span class="muted">created {date(token.created_at)}</span>
				</p>
			) : (
				<p class="muted">You don't have an API token.</p>
			)}

			{confirm ? (
				<div class="confirm">
					<span>{confirm === 'revoke' ? 'Revoke this token? Bots using it will stop working.' : 'Replace this token? The old one stops working.'}</span>
					<div class="btn-row">
						<button class={`small-btn ${confirm === 'revoke' ? 'danger' : 'primary'}`} disabled={busy} onClick={confirm === 'revoke' ? revoke : create}>
							{confirm === 'revoke' ? 'Revoke' : 'Regenerate'}
						</button>
						<button class="small-btn" onClick={() => setConfirm(null)}>
							Cancel
						</button>
					</div>
				</div>
			) : token ? (
				<div class="btn-row">
					<button class="small-btn" disabled={busy} onClick={() => setConfirm('regenerate')}>
						Regenerate
					</button>
					<button class="small-btn danger" disabled={busy} onClick={() => setConfirm('revoke')}>
						Revoke
					</button>
				</div>
			) : (
				<button class="small-btn primary" disabled={busy} onClick={create}>
					{busy ? 'Creating…' : 'Create API token'}
				</button>
			)}
			{error && <p class="notice err">{error}</p>}
		</div>
	);
}

function PasswordForm({ api }: { api: Api }) {
	const [current, setCurrent] = useState('');
	const [next, setNext] = useState('');
	const [again, setAgain] = useState('');
	const [busy, setBusy] = useState(false);
	const [result, setResult] = useState<{ ok: boolean; text: string } | null>(null);

	const mismatch = again !== '' && next !== again;
	const submit = async (e: Event) => {
		e.preventDefault();
		if (next !== again) return;
		setBusy(true);
		setResult(null);
		try {
			await api.changePassword(current, next);
			setCurrent('');
			setNext('');
			setAgain('');
			setResult({ ok: true, text: 'Password changed. Your other sign-ins were signed out; your API token still works.' });
		} catch (err) {
			setResult({ ok: false, text: message(err) });
		} finally {
			setBusy(false);
		}
	};

	return (
		<form class="password-form" onSubmit={submit}>
			<label>
				<span>Current password</span>
				<input type="password" autocomplete="current-password" value={current} onInput={(e) => setCurrent(e.currentTarget.value)} required />
			</label>
			<label>
				<span>New password</span>
				<input
					type="password"
					autocomplete="new-password"
					minLength={8}
					maxLength={128}
					value={next}
					onInput={(e) => setNext(e.currentTarget.value)}
					required
				/>
			</label>
			<label>
				<span>Confirm new password</span>
				<input type="password" autocomplete="new-password" value={again} onInput={(e) => setAgain(e.currentTarget.value)} required />
				{mismatch && <small class="down">Passwords don't match.</small>}
			</label>
			<button class="small-btn primary" type="submit" disabled={busy || mismatch}>
				{busy ? 'Changing…' : 'Change password'}
			</button>
			{result && <p class={`notice ${result.ok ? 'ok' : 'err'}`}>{result.text}</p>}
		</form>
	);
}
