import { BasePollStore } from './base-poll.svelte';
import { fetchQueue, postAction } from '$lib/api';
import type { QueueDetail } from '$lib/types';
import { type WSEvent } from './websocket.svelte';
import { reportFailure, reportSuccess } from './connection.svelte';
import { startTelemetry, stopTelemetry, setTotalRemainingBytes } from './telemetry.svelte';

class QueueStore extends BasePollStore {
	#queue = $state.raw<QueueDetail | null>(null);

	// Debounce: prevent overlapping poll() calls from piling up.
	#pollInFlight = false;
	#pollDirty = false;

	get queue() { return this.#queue; }
	get error() { return this.errorState; }
	get isPolling() { return this.pollingState; }
	get currentPage() { return this.currentPageState; }
	get pageLimit() { return this.pageLimitState; }
	get searchText() { return this.searchTextState; }

	async poll() {
		if (this.#pollInFlight) {
			this.#pollDirty = true;
			return;
		}
		this.#pollInFlight = true;
		try {
			const params: Record<string, string> = {};
			if (this.searchTextState) params.search = this.searchTextState;

			const res = await fetchQueue(this.currentPageState * this.pageLimitState, this.pageLimitState, params);
			this.#queue = res.queue;
			const totalRemaining = res.queue.slots.reduce((sum, s) => sum + s.remaining_bytes, 0);
			setTotalRemainingBytes(totalRemaining);
			this.errorState = null;
			reportSuccess();
		} catch (e) {
			const msg = e instanceof Error ? e.message : String(e);
			this.errorState = msg;
			reportFailure(msg);
		} finally {
			this.#pollInFlight = false;
			if (this.#pollDirty) {
				this.#pollDirty = false;
				this.poll();
			}
		}
	}

	start() {
		super.start();
		startTelemetry();
	}

	stop() {
		super.stop();
		stopTelemetry();
	}

	handleWSEvent(event: WSEvent) {
		if (event.event === 'queue_updated') {
			this.poll();
		} else if (event.event === 'job_finalized') {
			this.#handleJobFinalized(event.nzo_id);
		}
	}

	#handleJobFinalized(nzoId?: string) {
		if (nzoId && this.#queue) {
			const before = this.#queue.slots.length;
			this.#queue = {
				...this.#queue,
				slots: this.#queue.slots.filter((s) => s.nzo_id !== nzoId)
			};
			if (this.#queue.slots.length !== before) {
				const totalRemaining = this.#queue.slots.reduce(
					(sum, s) => sum + s.remaining_bytes, 0);
				setTotalRemainingBytes(totalRemaining);
			}
		}
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

export const pauseJob = (id: string) => store.pauseJob(id);
export const resumeJob = (id: string) => store.resumeJob(id);
export const deleteJob = (id: string, df?: boolean) => store.deleteJob(id, df);

// Re-export telemetry selectors and updates to maintain backward compatibility
export {
	getSpeedBytesPerSec,
	getSpeedHistory,
	getTotalRemainingBytes,
	getSpeedLimitBytesPerSec,
	getBandwidthMaxBytesPerSec,
	getBandwidthPerc,
	getServerStats,
	setBandwidthPerc
} from './telemetry.svelte';
