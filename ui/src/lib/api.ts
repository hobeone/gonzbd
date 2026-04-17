import type {
	QueueResponse,
	HistoryResponse,
	WarningsResponse,
	StatusResponse,
	VersionResponse
} from './types';

const API_BASE = '/api';

function apiUrl(mode: string, params?: Record<string, string>): string {
	const search = new URLSearchParams({ mode, output: 'json', ...params });
	return `${API_BASE}?${search}`;
}

export async function fetchJSON<T>(url: string): Promise<T> {
	const res = await fetch(url);
	if (!res.ok) {
		throw new Error(`API ${res.status}: ${res.statusText}`);
	}
	return res.json() as Promise<T>;
}

export async function fetchVersion(): Promise<VersionResponse> {
	return fetchJSON<VersionResponse>(apiUrl('version'));
}

export async function fetchQueue(start = 0, limit = 20): Promise<QueueResponse> {
	return fetchJSON<QueueResponse>(
		apiUrl('queue', { start: String(start), limit: String(limit) })
	);
}

export async function fetchHistory(start = 0, limit = 20): Promise<HistoryResponse> {
	return fetchJSON<HistoryResponse>(
		apiUrl('history', { start: String(start), limit: String(limit) })
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
	return fetchJSON<StatusResponse>(apiUrl('set_config', { section, keyword, value: String(value) }));
}

export async function postAction(
	mode: string,
	params?: Record<string, string>
): Promise<StatusResponse> {
	return fetchJSON<StatusResponse>(apiUrl(mode, params));
}

export async function uploadNzb(file: File): Promise<StatusResponse> {
	const form = new FormData();
	form.append('mode', 'addfile');
	form.append('output', 'json');
	form.append('nzbfile', file, file.name);

	const res = await fetch(API_BASE, { method: 'POST', body: form });
	if (!res.ok) {
		throw new Error(`Upload ${res.status}: ${res.statusText}`);
	}
	return res.json() as Promise<StatusResponse>;
}
