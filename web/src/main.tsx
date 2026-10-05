import { render } from 'preact';
import { Api } from './api';
import { App } from './App';
import './styles.css';

/**
 * The API address comes from /config.json, which the web container writes from
 * T3_API_URL at startup. The Vite dev server has no such file, so it falls back
 * to VITE_T3_API_URL or a local server.
 */
async function apiUrl(): Promise<string> {
	try {
		const res = await fetch('/config.json', { cache: 'no-cache' });
		if (res.ok) return (await res.json()).apiUrl;
	} catch {
		// fall through
	}
	return import.meta.env.VITE_T3_API_URL ?? 'http://localhost:8080';
}

const base = (await apiUrl()).replace(/\/+$/, '');
render(<App api={new Api(base)} />, document.getElementById('app')!);
