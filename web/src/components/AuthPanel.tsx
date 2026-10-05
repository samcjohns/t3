import { useState } from 'preact/hooks';
import { type Api, ApiError, type Session } from '../api';

interface Props {
	api: Api;
	onSession: (s: Session) => void;
}

export function AuthPanel({ api, onSession }: Props) {
	const [mode, setMode] = useState<'login' | 'register'>('login');
	const [username, setUsername] = useState('');
	const [password, setPassword] = useState('');
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState('');

	const submit = async (e: Event) => {
		e.preventDefault();
		setBusy(true);
		setError('');
		try {
			if (mode === 'register') await api.register(username, password);
			onSession(await api.login(username, password));
		} catch (err) {
			setError(err instanceof ApiError ? err.message : 'Something went wrong. Try again.');
		} finally {
			setBusy(false);
		}
	};

	const registering = mode === 'register';
	return (
		<form class="auth" onSubmit={submit}>
			<h2>{registering ? 'Open an account' : 'Sign in to trade'}</h2>
			<p class="hint">{registering ? 'New accounts start with virtual cash to trade with.' : 'Browse the market freely. Sign in to place orders.'}</p>
			<label>
				<span>Username</span>
				<input
					autocomplete="username"
					autocapitalize="none"
					spellcheck={false}
					value={username}
					onInput={(e) => setUsername(e.currentTarget.value.toLowerCase())}
					required
				/>
				{registering && <small>3–32 characters: a–z, 0–9, _ or -</small>}
			</label>
			<label>
				<span>Password</span>
				<input
					type="password"
					autocomplete={registering ? 'new-password' : 'current-password'}
					value={password}
					onInput={(e) => setPassword(e.currentTarget.value)}
					minLength={registering ? 8 : undefined}
					required
				/>
				{registering && <small>At least 8 characters</small>}
			</label>
			<button class="submit" type="submit" disabled={busy}>
				{busy ? 'One moment…' : registering ? 'Create account' : 'Sign in'}
			</button>
			{error && (
				<p class="notice err" role="alert">
					{error}
				</p>
			)}
			<p class="switch">
				{registering ? 'Already have an account?' : 'New to t3?'}{' '}
				<button
					type="button"
					class="link"
					onClick={() => {
						setMode(registering ? 'login' : 'register');
						setError('');
					}}
				>
					{registering ? 'Sign in' : 'Create an account'}
				</button>
			</p>
		</form>
	);
}
