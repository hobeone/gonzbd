import type {
	QueueResponse,
	HistoryResponse,
	WarningsResponse,
	StatusResponse,
	VersionResponse,
	ConfigResponse
} from './types';
import { reportAuthExpired, isAuthExpired } from './stores/connection.svelte';

const API_BASE = '/api';

function apiUrl(mode: string, params?: Record<string, string>): string {
	const search = new URLSearchParams({ mode, output: 'json', ...params });
	return `${API_BASE}?${search}`;
}

function checkRedirect(res: Response, requestedUrl: string): boolean {
	// If the response URL is different from the requested URL, fetch followed a redirect.
	// Since Tinyauth (or other proxies) redirect to a login/auth page, we detect this here.
	if (!res || !res.url) return false;

	const reqURL = new URL(requestedUrl, window.location.origin);
	const resURL = new URL(res.url);

	const crossOriginRedirect = resURL.origin !== reqURL.origin;
	const sameOriginAuthRedirect = res.redirected && !resURL.pathname.startsWith('/api');

	if (crossOriginRedirect || sameOriginAuthRedirect) {
		// If we are already handling the redirect, silence subsequent calls
		if (isAuthExpired()) {
			return true;
		}

		reportAuthExpired();

		// Rewrite any redirect parameters containing "/api" to point to the current SPA location
		for (const [key, value] of resURL.searchParams.entries()) {
			if (value.includes('/api')) {
				resURL.searchParams.set(key, window.location.href);
			}
		}

		setTimeout(() => {
			window.location.href = resURL.href;
		}, 1500);
		return true;
	}

	return false;
}

export async function fetchJSON<T>(url: string): Promise<T> {
	const res = await fetch(url);
	if (checkRedirect(res, url)) {
		return new Promise(() => {}); // Never resolve or reject to suppress UI error states/toasts
	}
	if (!res.ok) {
		let message = `API ${res.status}: ${res.statusText}`;
		try {
			const data = await res.json();
			if (data && data.error) {
				message = data.error;
			}
		} catch (e) {
			// ignore parse errors
		}
		throw new Error(message);
	}
	return res.json() as Promise<T>;
}

export async function fetchVersion(): Promise<VersionResponse> {
	return fetchJSON<VersionResponse>(apiUrl('version'));
}

export async function fetchQueue(
	start = 0,
	limit = 10,
	params?: Record<string, string>
): Promise<QueueResponse> {
	return fetchJSON<QueueResponse>(
		apiUrl('queue', { start: String(start), limit: String(limit), ...params })
	);
}

/**
 * Fetch a single queue job's detail (slot fields plus per-file breakdown).
 * Used by the QueueRow expansion drawer to render per-file progress. The
 * default `mode=queue` listing does NOT include file arrays — this endpoint
 * is the only way to retrieve them.
 */
export async function fetchQueueJobDetail(nzoId: string): Promise<QueueResponse> {
	return fetchJSON<QueueResponse>(apiUrl('queue', { nzo_id: nzoId, files: '1' }));
}

export async function fetchHistory(
	start = 0,
	limit = 10,
	params?: Record<string, string>
): Promise<HistoryResponse> {
	return fetchJSON<HistoryResponse>(
		apiUrl('history', { start: String(start), limit: String(limit), ...params })
	);
}

export async function fetchWarnings(): Promise<WarningsResponse> {
	return fetchJSON<WarningsResponse>(apiUrl('warnings'));
}

export async function fetchScripts(): Promise<string[]> {
	const res = await fetchJSON<{ scripts: string[] }>(apiUrl('get_scripts'));
	return res.scripts;
}

export async function fetchCategories(): Promise<string[]> {
	const res = await fetchJSON<{ categories: string[] }>(apiUrl('get_cats'));
	return res.categories;
}

export async function setConfig(
	section: string,
	keyword: string,
	value: string | number | boolean
): Promise<StatusResponse> {
	const form = new FormData();
	form.append('section', section);
	form.append('keyword', keyword);
	form.append('value', String(value));

	// mode must be in the URL query string — the backend routes on
	// r.URL.Query().Get("mode"), not FormValue.
	const url = apiUrl('set_config');
	const res = await fetch(url, { method: 'POST', body: form });
	if (checkRedirect(res, url)) {
		return new Promise(() => {});
	}
	if (!res.ok) {
		let message = `Set Config ${res.status}: ${res.statusText}`;
		try {
			const data = await res.json();
			if (data && data.error) {
				message = data.error;
			}
		} catch (e) {
			// ignore parse errors
		}
		throw new Error(message);
	}
	return res.json() as Promise<StatusResponse>;
}

export async function postAction(
	mode: string,
	params?: Record<string, string>
): Promise<StatusResponse> {
	return fetchJSON<StatusResponse>(apiUrl(mode, params));
}

export async function uploadNzb(
	file: File,
	params?: Record<string, string>
): Promise<StatusResponse> {
	const form = new FormData();
	form.append('nzbfile', file, file.name);

	if (params) {
		for (const [k, v] of Object.entries(params)) {
			form.append(k, v);
		}
	}

	// mode must be in the URL query string — the backend routes on
	// r.URL.Query().Get("mode"), not FormValue.
	const url = apiUrl('addfile');
	const res = await fetch(url, { method: 'POST', body: form });
	if (checkRedirect(res, url)) {
		return new Promise(() => {});
	}
	if (!res.ok) {
		throw new Error(`Upload ${res.status}: ${res.statusText}`);
	}
	return res.json() as Promise<StatusResponse>;
}

export async function fetchConfig(): Promise<ConfigResponse> {
	return fetchJSON<ConfigResponse>(apiUrl('get_config'));
}
