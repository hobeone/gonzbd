import type {
	QueueResponse,
	HistoryResponse,
	WarningsResponse,
	StatusResponse,
	VersionResponse,
	ConfigResponse
} from './types';
import { reportAuthExpired, isAuthExpired } from './stores/connection.svelte';
import { getRedirectUrl } from '$lib/utils';

const API_BASE = '/api';

function apiUrl(mode: string, params?: Record<string, string>): string {
	const search = new URLSearchParams({ mode, output: 'json', ...params });
	return `${API_BASE}?${search}`;
}

function checkRedirect(res: Response, requestedUrl: string): boolean {
	const redirectUrl = getRedirectUrl(res, requestedUrl);
	if (redirectUrl) {
		if (isAuthExpired()) {
			return true;
		}

		reportAuthExpired();

		setTimeout(() => {
			window.location.href = redirectUrl;
		}, 1500);
		return true;
	}

	// A same-origin 401 from /api (no redirect involved) means our session
	// cookie is stale — e.g. the backend restarted and issued a new
	// ephemeral session key. Reloading re-hits "/" and picks up a fresh
	// cookie automatically.
	if (res.status === 401) {
		if (isAuthExpired()) {
			return true;
		}

		reportAuthExpired();

		setTimeout(() => {
			window.location.reload();
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

export interface StatusOverviewGeneral {
	version: string;
	commit: string;
	build_date: string;
	go_version: string;
	uptime_seconds: number;
	hostname: string;
	local_ip: string;
	config_path: string;
	download_dir: string;
	complete_dir: string;
	admin_dir: string;
	script_dir: string;
	log_dir: string;
	par2: { path: string; version: string };
	unrar: { path: string; version: string };
	sevenzip: { path: string; version: string };
}

export interface StatusOverviewSystem {
	os: string;
	arch: string;
	article_cache_bytes: number;
	download_dir_free_bytes: number;
	min_free_space_bytes: number;
}

export interface StatusOverviewResponse {
	status: boolean;
	general: StatusOverviewGeneral;
	system: StatusOverviewSystem;
}

export interface CheckUpdateResult {
	status: 'up_to_date' | 'update_available' | 'unknown';
	latest_version?: string;
	reason?: string;
}

export async function fetchStatusOverview(): Promise<StatusOverviewResponse> {
	return fetchJSON<StatusOverviewResponse>(apiUrl('status_overview'));
}

export async function fetchCheckUpdate(): Promise<{ status: boolean; result: CheckUpdateResult }> {
	return fetchJSON(apiUrl('status', { name: 'check_update' }));
}

export interface TestConnectionResult {
	ok: boolean;
	latency_ms?: number;
	error?: string;
	likely_connection_limit?: boolean;
}

export async function testServerConnection(
	name: string
): Promise<{ status: boolean; result: TestConnectionResult }> {
	return fetchJSON(apiUrl('status', { name: 'test_connection', value: name }));
}

export interface TestDiskSpeedResult {
	ok: boolean;
	mb_per_sec?: number;
	error?: string;
}

export async function testDiskSpeed(): Promise<{ status: boolean; result: TestDiskSpeedResult }> {
	return fetchJSON(apiUrl('status', { name: 'test_disk_speed' }));
}

export interface BuildDependency {
	path: string;
	version: string;
}

export interface BuildInfoResponse {
	status: boolean;
	version: string;
	commit: string;
	build_date: string;
	go_version: string;
	deps: BuildDependency[];
}

export async function fetchBuildInfo(): Promise<BuildInfoResponse> {
	return fetchJSON<BuildInfoResponse>(apiUrl('status', { name: 'build_info' }));
}

export interface RedactedServerConfig {
	name: string;
	host: string;
	port: number;
	connections: number;
	ssl: boolean;
	ssl_verify: number;
	ssl_ciphers: string;
	priority: number;
	required: boolean;
	optional: boolean;
	retention: number;
	timeout: number;
	pipelining_requests: number;
	enable: boolean;
}

export interface RedactedConfig {
	servers?: RedactedServerConfig[];
	[key: string]: unknown;
}

export async function fetchRedactedConfig(): Promise<{ status: boolean; config: RedactedConfig }> {
	return fetchJSON(apiUrl('status', { name: 'config' }));
}
