import { fetchHistory, postAction } from '$lib/api';
import type { HistoryDetail, HistorySlot } from '$lib/types';
import { refreshQueue } from './queue.svelte';
import { subscribeWS } from './websocket.svelte';

const FALLBACK_POLL_INTERVAL = 60000;

class HistoryStore {
	#history = $state<HistoryDetail | null>(null);
	#error = $state<string | null>(null);
	#fallbackTimer: ReturnType<typeof setInterval> | null = null;
	#wsCleanup: (() => void) | null = null;

	#historyPage = $state(0);
	#historyLimit = $state(10);
	#showFailedOnly = $state(false);
	#searchText = $state('');

	get history() { return this.#history; }
	get error() { return this.#error; }
	get page() { return this.#historyPage; }
	get limit() { return this.#historyLimit; }
	get failedOnly() { return this.#showFailedOnly; }
	get searchText() { return this.#searchText; }

	async poll() {
		try {
			const params: Record<string, string> = {};
			if (this.#showFailedOnly) params.status = 'Failed';
			if (this.#searchText) params.search = this.#searchText;

			const res = await fetchHistory(this.#historyPage * this.#historyLimit, this.#historyLimit, params);
			this.#history = res.history;
			this.#error = null;
		} catch (e) {
			this.#error = e instanceof Error ? e.message : String(e);
		}
	}

	start() {
		if (this.#fallbackTimer) return;
		this.poll();

		this.#wsCleanup = subscribeWS((event) => {
			if (event.event === 'history_updated') {
				this.poll();
			}
		});

		this.#fallbackTimer = setInterval(() => this.poll(), FALLBACK_POLL_INTERVAL);
	}

	stop() {
		if (this.#fallbackTimer) {
			clearInterval(this.#fallbackTimer);
			this.#fallbackTimer = null;
		}
		if (this.#wsCleanup) {
			this.#wsCleanup();
			this.#wsCleanup = null;
		}
	}

	setPage(page: number) {
		this.#historyPage = page;
		this.poll();
	}

	setLimit(limit: number) {
		this.#historyLimit = limit;
		this.#historyPage = 0;
		this.poll();
	}

	setFailedOnly(failed: boolean) {
		this.#showFailedOnly = failed;
		this.#historyPage = 0;
		this.poll();
	}

	setSearch(search: string) {
		this.#searchText = search;
		this.#historyPage = 0;
		this.poll();
	}

	async deleteItem(nzoId: string, deleteFiles = false) {
		const params: Record<string, string> = { name: 'delete', value: nzoId };
		if (deleteFiles) {
			params.delete_files = '1';
		}
		await postAction('history', params);
		await this.poll();
	}

	async purge(deleteFiles: boolean) {
		await postAction('history', {
			name: 'delete',
			value: 'all',
			delete_files: deleteFiles ? '1' : '0'
		});
		await this.poll();
	}

	async retryJob(nzoId: string) {
		await postAction('history', { name: 'retry', value: nzoId });
		await this.poll();
	}
}

const store = new HistoryStore();

export const getHistory = () => store.history;
export const getHistorySlots = () => store.history?.slots ?? [];
export const getHistoryPage = () => store.page;
export const getHistoryLimit = () => store.limit;
export const setHistoryPage = (p: number) => store.setPage(p);
export const setHistoryLimit = (l: number) => store.setLimit(l);
export const getHistoryFailedOnly = () => store.failedOnly;
export const setHistoryFailedOnly = (f: boolean) => store.setFailedOnly(f);
export const getHistorySearch = () => store.searchText;
export const setHistorySearch = (s: string) => store.setSearch(s);
export const getHistoryError = () => store.error;
export const startHistoryPolling = () => store.start();
export const stopHistoryPolling = () => store.stop();

export const retryHistoryJob = (id: string) => store.retryJob(id);

export async function deleteHistoryItem(nzoId: string, deleteFiles = false) {
	await store.deleteItem(nzoId, deleteFiles);
	await refreshQueue();
}

export async function purgeHistory(deleteFiles: boolean) {
	await store.purge(deleteFiles);
	await refreshQueue();
}
