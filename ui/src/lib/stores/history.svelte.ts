import { BasePollStore } from './base-poll.svelte';
import { fetchHistory, postAction } from '$lib/api';
import type { HistoryDetail } from '$lib/types';
import { refreshQueue } from './queue.svelte';
import { type WSEvent } from './websocket.svelte';
import { reportFailure, reportSuccess } from './connection.svelte';

class HistoryStore extends BasePollStore {
	#history = $state.raw<HistoryDetail | null>(null);
	#showFailedOnly = $state(false);

	get history() { return this.#history; }
	get error() { return this.errorState; }
	get page() { return this.currentPageState; }
	get limit() { return this.pageLimitState; }
	get failedOnly() { return this.#showFailedOnly; }
	get searchText() { return this.searchTextState; }

	async poll() {
		try {
			const params: Record<string, string> = {};
			if (this.#showFailedOnly) params.status = 'Failed';
			if (this.searchTextState) params.search = this.searchTextState;

			const res = await fetchHistory(this.currentPageState * this.pageLimitState, this.pageLimitState, params);
			this.#history = res.history;
			this.errorState = null;
			reportSuccess();
		} catch (e) {
			const msg = e instanceof Error ? e.message : String(e);
			this.errorState = msg;
			reportFailure(msg);
		}
	}

	handleWSEvent(event: WSEvent) {
		if (event.event === 'history_updated' || event.event === 'job_finalized') {
			this.poll();
		}
	}

	setFailedOnly(failed: boolean) {
		this.#showFailedOnly = failed;
		this.currentPageState = 0;
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

export async function retryHistoryJob(id: string) {
	await store.retryJob(id);
	await refreshQueue();
}

export async function deleteHistoryItem(nzoId: string, deleteFiles = false) {
	await store.deleteItem(nzoId, deleteFiles);
	await refreshQueue();
}

export async function purgeHistory(deleteFiles: boolean) {
	await store.purge(deleteFiles);
	await refreshQueue();
}
