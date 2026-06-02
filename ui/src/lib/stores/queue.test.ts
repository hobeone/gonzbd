import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

// Mock dependencies.
vi.mock('$lib/api', () => ({
	fetchQueue: vi.fn(),
	postAction: vi.fn(),
	fetchServerStats: vi.fn().mockResolvedValue({ servers: [] })
}));

// Captures the handler passed to subscribeWS so tests can simulate WS events.
let capturedWSHandler: ((event: any) => void) | null = null;
vi.mock('./websocket.svelte', () => ({
	subscribeWS: vi.fn((handler: (event: any) => void) => {
		capturedWSHandler = handler;
		return vi.fn();
	}),
	getWSStatus: vi.fn().mockReturnValue(false)
}));

// Mock ConnectionStore — queue store reports success/failure to it.
vi.mock('./connection.svelte', () => ({
	reportFailure: vi.fn(),
	reportSuccess: vi.fn(),
	onReconnected: vi.fn(() => vi.fn())
}));

import {
	getQueue,
	getQueueSlots,
	getQueuePage,
	getQueueLimit,
	setQueuePage,
	setQueueLimit,
	getQueueSearch,
	setQueueSearch,
	isPaused,
	getError,
	isPolling,
	startPolling,
	stopPolling,
	refreshQueue,
	getSpeedBytesPerSec,
	getSpeedHistory,
	getTotalRemainingBytes,
	pauseJob,
	resumeJob,
	deleteJob
} from './queue.svelte';
import { fetchQueue, postAction } from '$lib/api';

const mockSlots = [
	{ nzo_id: 'nzo_1', name: 'Job1', status: 'Downloading', remaining_bytes: 1000 },
	{ nzo_id: 'nzo_2', name: 'Job2', status: 'Queued', remaining_bytes: 2000 }
];

function mockQueueOk(slots = mockSlots) {
	vi.mocked(fetchQueue).mockResolvedValue({
		status: true,
		queue: {
			slots,
			noofslots: slots.length,
			paused: false,
			speed: '1.0 M',
			timeleft: '00:10:00'
		}
	} as any);
}

describe('Queue Store', () => {
	beforeEach(async () => {
		vi.useFakeTimers();
		vi.clearAllMocks();
		stopPolling();
		// Reset singleton state.
		vi.mocked(fetchQueue).mockResolvedValue({
			status: true,
			queue: { slots: [], noofslots: 0, paused: false }
		} as any);
		setQueuePage(0);
		setQueueLimit(10);
		setQueueSearch('');
		await vi.advanceTimersByTimeAsync(0);
		vi.clearAllMocks();
	});

	afterEach(() => {
		stopPolling();
		vi.useRealTimers();
	});

	// ── Default state ──

	it('returns empty slots after reset', () => {
		expect(getQueueSlots()).toEqual([]);
	});

	it('defaults to page 0, limit 10', () => {
		expect(getQueuePage()).toBe(0);
		expect(getQueueLimit()).toBe(10);
	});

	it('defaults search to empty', () => {
		expect(getQueueSearch()).toBe('');
	});

	it('speed defaults to 0', () => {
		expect(getSpeedBytesPerSec()).toBe(0);
	});

	// ── Polling ──

	it('startPolling calls fetchQueue and sets isPolling', async () => {
		mockQueueOk();
		startPolling();
		await vi.advanceTimersByTimeAsync(0);

		expect(fetchQueue).toHaveBeenCalledTimes(1);
		expect(isPolling()).toBe(true);
	});

	it('poll stores the returned queue', async () => {
		mockQueueOk();
		startPolling();
		await vi.advanceTimersByTimeAsync(0);

		expect(getQueueSlots()).toHaveLength(2);
	});

	it('poll calculates totalRemainingBytes from slots', async () => {
		mockQueueOk();
		startPolling();
		await vi.advanceTimersByTimeAsync(0);

		expect(getTotalRemainingBytes()).toBe(3000); // 1000 + 2000
	});

	it('poll captures error', async () => {
		vi.mocked(fetchQueue).mockRejectedValue(new Error('timeout'));
		startPolling();
		await vi.advanceTimersByTimeAsync(0);

		expect(getError()).toBe('timeout');
	});

	it('poll clears error on success', async () => {
		vi.mocked(fetchQueue).mockRejectedValue(new Error('fail'));
		startPolling();
		await vi.advanceTimersByTimeAsync(0);
		expect(getError()).toBe('fail');

		// Explicitly re-poll (no fallback timer anymore — ConnectionStore
		// owns reconnection, and WS events trigger refetches).
		mockQueueOk();
		await refreshQueue();
		await vi.advanceTimersByTimeAsync(0);

		expect(getError()).toBeNull();
	});

	// ── Pagination ──

	it('setQueuePage triggers poll with offset', async () => {
		mockQueueOk();
		startPolling();
		await vi.advanceTimersByTimeAsync(0);
		vi.clearAllMocks();

		mockQueueOk();
		setQueuePage(3);
		await vi.advanceTimersByTimeAsync(0);

		expect(fetchQueue).toHaveBeenCalledWith(30, 10, expect.any(Object));
		expect(getQueuePage()).toBe(3);
	});

	it('setQueueLimit resets page to 0', () => {
		setQueuePage(5);
		mockQueueOk();
		setQueueLimit(25);

		expect(getQueuePage()).toBe(0);
		expect(getQueueLimit()).toBe(25);
	});

	it('setQueueSearch resets page and includes search param', async () => {
		mockQueueOk();
		startPolling();
		await vi.advanceTimersByTimeAsync(0);
		vi.clearAllMocks();

		mockQueueOk();
		setQueueSearch('linux');
		await vi.advanceTimersByTimeAsync(0);

		expect(getQueueSearch()).toBe('linux');
		expect(getQueuePage()).toBe(0);
		expect(fetchQueue).toHaveBeenCalledWith(0, 10, expect.objectContaining({ search: 'linux' }));
	});

	// ── Job actions ──

	it('pauseJob sends pause action and re-polls', async () => {
		vi.mocked(postAction).mockResolvedValue({ status: true });
		mockQueueOk();

		await pauseJob('nzo_1');

		expect(postAction).toHaveBeenCalledWith('queue', { name: 'pause', value: 'nzo_1' });
		expect(fetchQueue).toHaveBeenCalled();
	});

	it('resumeJob sends resume action and re-polls', async () => {
		vi.mocked(postAction).mockResolvedValue({ status: true });
		mockQueueOk();

		await resumeJob('nzo_1');

		expect(postAction).toHaveBeenCalledWith('queue', { name: 'resume', value: 'nzo_1' });
		expect(fetchQueue).toHaveBeenCalled();
	});

	it('deleteJob sends delete action and re-polls', async () => {
		vi.mocked(postAction).mockResolvedValue({ status: true });
		mockQueueOk();

		await deleteJob('nzo_1');

		expect(postAction).toHaveBeenCalledWith('queue', { name: 'delete', value: 'nzo_1' });
	});

	it('deleteJob with deleteFiles includes flag', async () => {
		vi.mocked(postAction).mockResolvedValue({ status: true });
		mockQueueOk();

		await deleteJob('nzo_1', true);

		expect(postAction).toHaveBeenCalledWith('queue', {
			name: 'delete',
			value: 'nzo_1',
			delete_files: '1'
		});
	});

	// ── Lifecycle ──

	it('stopPolling sets isPolling false', async () => {
		mockQueueOk();
		startPolling();
		await vi.advanceTimersByTimeAsync(0);
		expect(isPolling()).toBe(true);

		stopPolling();
		expect(isPolling()).toBe(false);
	});

	it('double startPolling is idempotent', async () => {
		mockQueueOk();
		startPolling();
		startPolling();
		await vi.advanceTimersByTimeAsync(0);

		expect(fetchQueue).toHaveBeenCalledTimes(1);
	});

	it('refreshQueue triggers immediate poll', async () => {
		mockQueueOk();
		await refreshQueue();
		await vi.advanceTimersByTimeAsync(0);

		expect(fetchQueue).toHaveBeenCalledTimes(1);
	});

	// ── isPaused ──

	it('isPaused returns false by default', () => {
		expect(isPaused()).toBe(false);
	});

	it('isPaused reflects queue paused state', async () => {
		vi.mocked(fetchQueue).mockResolvedValue({
			status: true,
			queue: { slots: [], noofslots: 0, paused: true }
		} as any);
		startPolling();
		await vi.advanceTimersByTimeAsync(0);

		expect(isPaused()).toBe(true);
	});

	// ── job_finalized event ──

	it('job_finalized optimistically drops the matching slot before refetch', async () => {
		mockQueueOk();
		startPolling();
		await vi.advanceTimersByTimeAsync(0);
		expect(getQueueSlots()).toHaveLength(2);

		// Make the next fetch hang so we can assert the optimistic update
		// happened *before* the refetch resolved.
		let resolveFetch: (v: any) => void = () => {};
		vi.mocked(fetchQueue).mockReturnValue(new Promise((r) => { resolveFetch = r; }));

		expect(capturedWSHandler).not.toBeNull();
		capturedWSHandler!({ event: 'job_finalized', nzo_id: 'nzo_1' });

		// Slot was dropped synchronously, even though refetch is still pending.
		expect(getQueueSlots()).toHaveLength(1);
		expect(getQueueSlots()[0].nzo_id).toBe('nzo_2');
		// totalRemainingBytes recomputed from the surviving slot.
		expect(getTotalRemainingBytes()).toBe(2000);

		resolveFetch({
			status: true,
			queue: { slots: [{ nzo_id: 'nzo_2', name: 'Job2', remaining_bytes: 2000 }], noofslots: 1, paused: false }
		});
		await vi.advanceTimersByTimeAsync(0);
	});

	it('job_finalized triggers a refetch', async () => {
		mockQueueOk();
		startPolling();
		await vi.advanceTimersByTimeAsync(0);
		vi.clearAllMocks();
		mockQueueOk();

		capturedWSHandler!({ event: 'job_finalized', nzo_id: 'nzo_1' });
		await vi.advanceTimersByTimeAsync(0);

		expect(fetchQueue).toHaveBeenCalledTimes(1);
	});

	it('job_finalized with unknown nzo_id leaves slots unchanged then refetches', async () => {
		mockQueueOk();
		startPolling();
		await vi.advanceTimersByTimeAsync(0);
		const slotsBefore = getQueueSlots();
		vi.clearAllMocks();
		mockQueueOk();

		capturedWSHandler!({ event: 'job_finalized', nzo_id: 'nzo_does_not_exist' });

		// Slots unchanged synchronously (same length, same ids).
		expect(getQueueSlots()).toHaveLength(slotsBefore.length);
		await vi.advanceTimersByTimeAsync(0);
		expect(fetchQueue).toHaveBeenCalledTimes(1);
	});
});
