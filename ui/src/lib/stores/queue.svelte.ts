import { BasePollStore } from './base-poll.svelte';
import { fetchQueue, postAction } from '#lib/api.js';
import type { QueueDetail } from '#lib/types.js';
import { type WSEvent } from './websocket.svelte';
import { reportFailure, reportSuccess } from './connection.svelte';
import { startTelemetry, stopTelemetry, setTotalRemainingBytes } from './telemetry.svelte';

class QueueStore extends BasePollStore {
	#queue = $state.raw<QueueDetail | null>(null);

	// Debounce: prevent overlapping poll() calls from piling up. #running is
	// the in-flight run, which includes any trailing re-poll a caller asked for
	// while it was busy; an overlapping poll() returns it, so awaiting poll()
	// settles only once the state that call wanted to see has been fetched.
	#running: Promise<void> | null = null;
	#pollDirty = false;

	get queue() { return this.#queue; }
	get error() { return this.errorState; }
	get isPolling() { return this.pollingState; }
	get currentPage() { return this.currentPageState; }
	get pageLimit() { return this.pageLimitState; }
	get searchText() { return this.searchTextState; }

	poll(): Promise<void> {
		if (this.#running) {
			this.#pollDirty = true;
			return this.#running;
		}
		this.#running = this.#pollLoop().finally(() => {
			this.#running = null;
		});
		return this.#running;
	}

	async #pollLoop() {
		do {
			this.#pollDirty = false;
			await this.#pollOnce();
		} while (this.#pollDirty);
	}

	async #pollOnce() {
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

	// pauseAll and resumeAll toggle the global pause and then re-poll rather
	// than waiting for the server's queue_updated broadcast, which can arrive
	// late or not at all (dropped socket); isPaused() reads the polled queue.
	async pauseAll() {
		await postAction('pause');
		await this.poll();
	}

	async resumeAll() {
		await postAction('resume');
		await this.poll();
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

export const pauseAll = () => store.pauseAll();
export const resumeAll = () => store.resumeAll();
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
