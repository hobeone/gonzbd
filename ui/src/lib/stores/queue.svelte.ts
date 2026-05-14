import { fetchQueue, postAction } from '$lib/api';
import type { QueueDetail, QueueSlot } from '$lib/types';
import { subscribeWS } from './websocket.svelte';
import { reportFailure, reportSuccess, onReconnected } from './connection.svelte';

const SPEED_HISTORY_SIZE = 60;

class QueueStore {
	#queue = $state<QueueDetail | null>(null);
	#polling = $state(false);
	#error = $state<string | null>(null);
	#wsCleanup: (() => void) | null = null;
	#reconnectCleanup: (() => void) | null = null;

	#currentPage = $state(0);
	#pageLimit = $state(10);
	#searchText = $state('');

	#speedBytesPerSec = $state(0);
	#speedHistory = $state<number[]>([]);
	#totalRemainingBytes = $state(0);
	#speedLimitBytesPerSec = $state(0);
	#bandwidthMaxBytesPerSec = $state(0);
	#bandwidthPerc = $state(100);

	// Debounce: prevent overlapping poll() calls from piling up.
	#pollInFlight = false;
	#pollDirty = false;

	get queue() { return this.#queue; }
	get error() { return this.#error; }
	get isPolling() { return this.#polling; }
	get currentPage() { return this.#currentPage; }
	get pageLimit() { return this.#pageLimit; }
	get searchText() { return this.#searchText; }
	get speedBytesPerSec() { return this.#speedBytesPerSec; }
	get speedHistory() { return this.#speedHistory; }
	get totalRemainingBytes() { return this.#totalRemainingBytes; }
	get speedLimitBytesPerSec() { return this.#speedLimitBytesPerSec; }
	get bandwidthMaxBytesPerSec() { return this.#bandwidthMaxBytesPerSec; }
	get bandwidthPerc() { return this.#bandwidthPerc; }

	async poll() {
		// If a poll is already in-flight, mark dirty so we re-poll when
		// the current one finishes. This prevents request pile-up from
		// rapid WebSocket events while ensuring data stays fresh.
		if (this.#pollInFlight) {
			this.#pollDirty = true;
			return;
		}
		this.#pollInFlight = true;
		try {
			const params: Record<string, string> = {};
			if (this.#searchText) params.search = this.#searchText;

			const res = await fetchQueue(this.#currentPage * this.#pageLimit, this.#pageLimit, params);
			this.#queue = res.queue;
			this.#totalRemainingBytes = res.queue.slots.reduce((sum, s) => sum + s.remaining_bytes, 0);
			this.#error = null;
			reportSuccess();
		} catch (e) {
			const msg = e instanceof Error ? e.message : String(e);
			this.#error = msg;
			reportFailure(msg);
		} finally {
			this.#pollInFlight = false;
			// If new events arrived during the fetch, do one more poll.
			if (this.#pollDirty) {
				this.#pollDirty = false;
				this.poll();
			}
		}
	}

	start() {
		if (this.#polling) return;
		this.#polling = true;
		this.poll();

		this.#wsCleanup = subscribeWS((event) => {
			if (event.event === 'queue_updated') {
				this.poll();
			} else if (event.event === 'job_finalized') {
				// Job moved from queue → history. Optimistically drop
				// the slot so the row disappears immediately, then refetch
				// for an authoritative view (other slots may have shifted
				// pages, status counts changed, etc.).
				if (event.nzo_id && this.#queue) {
					const before = this.#queue.slots.length;
					this.#queue = {
						...this.#queue,
						slots: this.#queue.slots.filter((s) => s.nzo_id !== event.nzo_id)
					};
					if (this.#queue.slots.length !== before) {
						this.#totalRemainingBytes = this.#queue.slots.reduce(
							(sum, s) => sum + s.remaining_bytes, 0);
					}
				}
				this.poll();
			} else if (event.event === 'metrics') {
				this.#speedBytesPerSec = event.speed ?? 0;
				this.#totalRemainingBytes = event.remaining ?? 0;
				this.#speedLimitBytesPerSec = event.speed_limit ?? 0;
				this.#bandwidthMaxBytesPerSec = event.bandwidth_max ?? 0;
				this.#bandwidthPerc = event.bandwidth_perc ?? 100;
				this.#speedHistory = [...this.#speedHistory.slice(-(SPEED_HISTORY_SIZE - 1)), this.#speedBytesPerSec];
			}
		});

		// When ConnectionStore detects reconnection, refetch immediately.
		this.#reconnectCleanup = onReconnected(() => this.poll());
	}

	stop() {
		if (this.#wsCleanup) {
			this.#wsCleanup();
			this.#wsCleanup = null;
		}
		if (this.#reconnectCleanup) {
			this.#reconnectCleanup();
			this.#reconnectCleanup = null;
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

	async setBandwidthPerc(perc: number) {
		await postAction('set_config', { section: 'downloads', keyword: 'bandwidth_perc', value: String(perc) });
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
export const getSpeedLimitBytesPerSec = () => store.speedLimitBytesPerSec;
export const getBandwidthMaxBytesPerSec = () => store.bandwidthMaxBytesPerSec;
export const getBandwidthPerc = () => store.bandwidthPerc;
export const setBandwidthPerc = (perc: number) => store.setBandwidthPerc(perc);

export { formatSpeed, formatSize } from '$lib/utils';

export const pauseJob = (id: string) => store.pauseJob(id);
export const resumeJob = (id: string) => store.resumeJob(id);
export const deleteJob = (id: string, df?: boolean) => store.deleteJob(id, df);
