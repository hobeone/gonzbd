import type {
	QueueResponse,
	HistoryResponse,
	WarningsResponse,
	StatusResponse,
	VersionResponse
} from './types';

const API_BASE = '/api';

function apiUrl(mode: string, apiKey: string, params?: Record<string, string>): string {
	const search = new URLSearchParams({ mode, apikey: apiKey, output: 'json', ...params });
	return `${API_BASE}?${search}`;
}

async function fetchJSON<T>(url: string): Promise<T> {
	const res = await fetch(url);
	if (!res.ok) {
		throw new Error(`API ${res.status}: ${res.statusText}`);
	}
	return res.json() as Promise<T>;
}

export async function fetchVersion(apiKey: string): Promise<VersionResponse> {
	return fetchJSON<VersionResponse>(apiUrl('version', apiKey));
}

export async function fetchQueue(
	apiKey: string,
	start = 0,
	limit = 20
): Promise<QueueResponse> {
	return fetchJSON<QueueResponse>(
		apiUrl('queue', apiKey, { start: String(start), limit: String(limit) })
	);
}

export async function fetchHistory(
	apiKey: string,
	start = 0,
	limit = 20
): Promise<HistoryResponse> {
	return fetchJSON<HistoryResponse>(
		apiUrl('history', apiKey, { start: String(start), limit: String(limit) })
	);
}

export async function fetchWarnings(apiKey: string): Promise<WarningsResponse> {
	return fetchJSON<WarningsResponse>(apiUrl('warnings', apiKey));
}

export async function postAction(
	apiKey: string,
	mode: string,
	params?: Record<string, string>
): Promise<StatusResponse> {
	return fetchJSON<StatusResponse>(apiUrl(mode, apiKey, params));
}

export async function uploadNzb(apiKey: string, file: File): Promise<StatusResponse> {
	const form = new FormData();
	form.append('mode', 'addfile');
	form.append('apikey', apiKey);
	form.append('output', 'json');
	form.append('name', file, file.name);

	const res = await fetch(API_BASE, { method: 'POST', body: form });
	if (!res.ok) {
		throw new Error(`Upload ${res.status}: ${res.statusText}`);
	}
	return res.json() as Promise<StatusResponse>;
}
