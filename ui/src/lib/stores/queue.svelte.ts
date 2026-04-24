import { fetchQueue, postAction } from '$lib/api';
import type { QueueDetail, QueueSlot } from '$lib/types';
import { subscribeWS } from './websocket.svelte';

const FALLBACK_POLL_INTERVAL = 30000;
const SPEED_HISTORY_SIZE = 60;

class QueueStore {
	#queue = $state<QueueDetail | null>(null);
	#polling = $state(false);
	#error = $state<string | null>(null);
	#fallbackTimer: ReturnType<typeof setInterval> | null = null;
	#wsCleanup: (() => void) | null = null;

	#currentPage = $state(0);
	#pageLimit = $state(10);
	#searchText = $state('');

	#speedBytesPerSec = $state(0);
	#speedHistory = $state<number[]>([]);
	#totalRemainingBytes = $state(0);

	get queue() { return this.#queue; }
	get error() { return this.#error; }
	get isPolling() { return this.#polling; }
	get currentPage() { return this.#currentPage; }
	get pageLimit() { return this.#pageLimit; }
	get searchText() { return this.#searchText; }
	get speedBytesPerSec() { return this.#speedBytesPerSec; }
	get speedHistory() { return this.#speedHistory; }
	get totalRemainingBytes() { return this.#totalRemainingBytes; }

	async poll() {
		try {
			const params: Record<string, string> = {};
			if (this.#searchText) params.search = this.#searchText;

			const res = await fetchQueue(this.#currentPage * this.#pageLimit, this.#pageLimit, params);
			this.#queue = res.queue;
			this.#totalRemainingBytes = res.queue.slots.reduce((sum, s) => sum + s.remaining_bytes, 0);
			this.#error = null;
		} catch (e) {
			this.#error = e instanceof Error ? e.message : String(e);
		}
	}

	start() {
		if (this.#polling) return;
		this.#polling = true;
		this.poll();

		this.#wsCleanup = subscribeWS((event) => {
			if (event.event === 'queue_updated') {
				this.poll();
			} else if (event.event === 'metrics') {
				this.#speedBytesPerSec = event.speed ?? 0;
				this.#totalRemainingBytes = event.remaining ?? 0;
				this.#speedHistory = [...this.#speedHistory.slice(-(SPEED_HISTORY_SIZE - 1)), this.#speedBytesPerSec];
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
		this.#polling = false;
	}

	setPage(page: number) {
		this.#currentPage = page;
		this.poll();
	}

	setLimit(limit: number) {
		this.#pageLimit = limit;
		this.#currentPage = 0;
		this.poll();
	}

	setSearch(search: string) {
		this.#searchText = search;
		this.#currentPage = 0;
		this.poll();
	}

	async pauseJob(nzoId: string) {
		await postAction('queue', { name: 'pause', value: nzoId });
		await this.poll();
	}

	async resumeJob(nzoId: string) {
		await postAction('queue', { name: 'resume', value: nzoId });
		await this.poll();
	}

	async deleteJob(nzoId: string, deleteFiles = false) {
		const params: Record<string, string> = { name: 'delete', value: nzoId };
		if (deleteFiles) {
			params.delete_files = '1';
		}
		await postAction('queue', params);
		await this.poll();
	}
}

const store = new QueueStore();

// Exported wrapper functions to maintain API compatibility with components
export const getQueue = () => store.queue;
export const getQueueSlots = () => store.queue?.slots ?? [];
export const getQueuePage = () => store.currentPage;
export const getQueueLimit = () => store.pageLimit;
export const setQueuePage = (p: number) => store.setPage(p);
export const setQueueLimit = (l: number) => store.setLimit(l);
export const getQueueSearch = () => store.searchText;
export const setQueueSearch = (s: string) => store.setSearch(s);
export const isPaused = () => store.queue?.paused ?? false;
export const getError = () => store.error;
export const isPolling = () => store.isPolling;
export const startPolling = () => store.start();
export const stopPolling = () => store.stop();
export const refreshQueue = () => store.poll();
export const getSpeedBytesPerSec = () => store.speedBytesPerSec;
export const getSpeedHistory = () => store.speedHistory;
export const getTotalRemainingBytes = () => store.totalRemainingBytes;

export function formatSpeed(bps: number): string {
	if (bps < 1024) return `${Math.round(bps)} B/s`;
	if (bps < 1024 * 1024) return `${(bps / 1024).toFixed(1)} KB/s`;
	return `${(bps / (1024 * 1024)).toFixed(1)} MB/s`;
}

export function formatSize(bytes: number): string {
	if (bytes < 1024) return `${bytes} B`;
	if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
	if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
	return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)} GB`;
}

export const pauseJob = (id: string) => store.pauseJob(id);
export const resumeJob = (id: string) => store.resumeJob(id);
export const deleteJob = (id: string, df?: boolean) => store.deleteJob(id, df);
