import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

// Mock dependencies before importing the store.
vi.mock('$lib/api', () => ({
	fetchHistory: vi.fn(),
	postAction: vi.fn()
}));

// Captures the handler passed to subscribeWS so tests can simulate WS events.
let capturedWSHandler: ((event: any) => void) | null = null;
vi.mock('./websocket.svelte', () => ({
	subscribeWS: vi.fn((handler: (event: any) => void) => {
		capturedWSHandler = handler;
		return vi.fn();
	})
}));

vi.mock('./queue.svelte', () => ({
	refreshQueue: vi.fn()
}));

// Mock ConnectionStore — history store reports success/failure to it.
vi.mock('./connection.svelte', () => ({
	reportFailure: vi.fn(),
	reportSuccess: vi.fn(),
	onReconnected: vi.fn(() => vi.fn())
}));

import {
	getHistory,
	getHistorySlots,
	getHistoryPage,
	getHistoryLimit,
	setHistoryPage,
	setHistoryLimit,
	getHistoryFailedOnly,
	setHistoryFailedOnly,
	getHistorySearch,
	setHistorySearch,
	getHistoryError,
	startHistoryPolling,
	stopHistoryPolling,
	retryHistoryJob,
	deleteHistoryItem,
	purgeHistory
} from './history.svelte';
import { fetchHistory, postAction } from '$lib/api';
import { refreshQueue } from './queue.svelte';

const mockSlots = [
	{ nzo_id: 'nzo_1', name: 'Job1', status: 'Completed', category: 'TV' },
	{ nzo_id: 'nzo_2', name: 'Job2', status: 'Failed', category: 'Movies' }
];

function mockHistoryOk(slots = mockSlots, total = 2) {
	vi.mocked(fetchHistory).mockResolvedValue({
		status: true,
		history: {
			slots,
			noofslots: total,
			total_size: '1 GB',
			last_history_update: 12345,
			ppslots: 0
		}
	} as any);
}

/** Flush microtask queue so poll() resolves. */
function flush(): Promise<void> {
	return new Promise((r) => setTimeout(r, 0));
}

describe('History Store', () => {
	beforeEach(async () => {
		vi.useFakeTimers();
		vi.clearAllMocks();
		stopHistoryPolling();
		// Reset singleton state — setters call poll(), so provide a mock response.
		vi.mocked(fetchHistory).mockResolvedValue({ status: true, history: { slots: [], noofslots: 0 } } as any);
		setHistoryPage(0);
		setHistoryLimit(10);
		setHistoryFailedOnly(false);
		setHistorySearch('');
		await vi.advanceTimersByTimeAsync(0);
		vi.clearAllMocks();
	});

	afterEach(() => {
		stopHistoryPolling();
		vi.useRealTimers();
	});

	// ── Default state ──

	it('has empty slots after reset', () => {
		expect(getHistorySlots()).toEqual([]);
	});

	it('returns empty slots when history is null', () => {
		expect(getHistorySlots()).toEqual([]);
	});

	it('defaults to page 0, limit 10', () => {
		expect(getHistoryPage()).toBe(0);
		expect(getHistoryLimit()).toBe(10);
	});

	it('defaults failedOnly to false', () => {
		expect(getHistoryFailedOnly()).toBe(false);
	});

	it('defaults search to empty string', () => {
		expect(getHistorySearch()).toBe('');
	});

	// ── Polling ──

	it('startHistoryPolling calls fetchHistory', async () => {
		mockHistoryOk();
		startHistoryPolling();
		await vi.advanceTimersByTimeAsync(0);

		expect(fetchHistory).toHaveBeenCalledTimes(1);
	});

	it('poll stores the returned history', async () => {
		mockHistoryOk();
		startHistoryPolling();
		await vi.advanceTimersByTimeAsync(0);

		expect(getHistory()).not.toBeNull();
		expect(getHistorySlots()).toHaveLength(2);
	});

	it('poll captures error', async () => {
		vi.mocked(fetchHistory).mockRejectedValue(new Error('network down'));
		startHistoryPolling();
		await vi.advanceTimersByTimeAsync(0);

		expect(getHistoryError()).toBe('network down');
	});

	it('poll clears error on success after failure', async () => {
		// First: error.
		vi.mocked(fetchHistory).mockRejectedValue(new Error('fail'));
		startHistoryPolling();
		await vi.advanceTimersByTimeAsync(0);
		expect(getHistoryError()).toBe('fail');

		// Explicitly re-poll (no fallback timer anymore).
		mockHistoryOk();
		setHistoryPage(0); // triggers poll
		await vi.advanceTimersByTimeAsync(0);

		expect(getHistoryError()).toBeNull();
	});

	// ── Pagination ──

	it('setHistoryPage triggers poll with offset', async () => {
		mockHistoryOk();
		startHistoryPolling();
		await vi.advanceTimersByTimeAsync(0);
		vi.clearAllMocks();

		mockHistoryOk();
		setHistoryPage(2);
		await vi.advanceTimersByTimeAsync(0);

		expect(fetchHistory).toHaveBeenCalledWith(20, 10, expect.any(Object));
		expect(getHistoryPage()).toBe(2);
	});

	it('setHistoryLimit resets page to 0', () => {
		setHistoryPage(3);
		mockHistoryOk();
		setHistoryLimit(25);

		expect(getHistoryPage()).toBe(0);
		expect(getHistoryLimit()).toBe(25);
	});

	// ── Filtering ──

	it('setHistoryFailedOnly resets page and includes status param', async () => {
		mockHistoryOk();
		startHistoryPolling();
		await vi.advanceTimersByTimeAsync(0);
		vi.clearAllMocks();

		mockHistoryOk();
		setHistoryFailedOnly(true);
		await vi.advanceTimersByTimeAsync(0);

		expect(getHistoryFailedOnly()).toBe(true);
		expect(getHistoryPage()).toBe(0);
		expect(fetchHistory).toHaveBeenCalledWith(0, 10, expect.objectContaining({ status: 'Failed' }));
	});

	it('setHistorySearch resets page and includes search param', async () => {
		mockHistoryOk();
		startHistoryPolling();
		await vi.advanceTimersByTimeAsync(0);
		vi.clearAllMocks();

		mockHistoryOk();
		setHistorySearch('my show');
		await vi.advanceTimersByTimeAsync(0);

		expect(getHistorySearch()).toBe('my show');
		expect(getHistoryPage()).toBe(0);
		expect(fetchHistory).toHaveBeenCalledWith(0, 10, expect.objectContaining({ search: 'my show' }));
	});

	// ── Job actions ──

	it('deleteHistoryItem sends correct params', async () => {
		vi.mocked(postAction).mockResolvedValue({ status: true });
		mockHistoryOk();

		await deleteHistoryItem('nzo_1');

		expect(postAction).toHaveBeenCalledWith('history', { name: 'delete', value: 'nzo_1' });
		expect(refreshQueue).toHaveBeenCalled();
	});

	it('deleteHistoryItem with deleteFiles includes flag', async () => {
		vi.mocked(postAction).mockResolvedValue({ status: true });
		mockHistoryOk();

		await deleteHistoryItem('nzo_1', true);

		expect(postAction).toHaveBeenCalledWith('history', {
			name: 'delete',
			value: 'nzo_1',
			delete_files: '1'
		});
	});

	it('purgeHistory sends delete all', async () => {
		vi.mocked(postAction).mockResolvedValue({ status: true });
		mockHistoryOk();

		await purgeHistory(false);

		expect(postAction).toHaveBeenCalledWith('history', {
			name: 'delete',
			value: 'all',
			delete_files: '0'
		});
		expect(refreshQueue).toHaveBeenCalled();
	});

	it('purgeHistory with deleteFiles sends flag', async () => {
		vi.mocked(postAction).mockResolvedValue({ status: true });
		mockHistoryOk();

		await purgeHistory(true);

		expect(postAction).toHaveBeenCalledWith('history', {
			name: 'delete',
			value: 'all',
			delete_files: '1'
		});
	});

	it('retryHistoryJob sends retry action', async () => {
		vi.mocked(postAction).mockResolvedValue({ status: true });
		mockHistoryOk();

		await retryHistoryJob('nzo_2');

		expect(postAction).toHaveBeenCalledWith('history', { name: 'retry', value: 'nzo_2' });
	});

	// ── Lifecycle ──

	it('stopHistoryPolling prevents further WS-triggered polls', async () => {
		mockHistoryOk();
		startHistoryPolling();
		await vi.advanceTimersByTimeAsync(0);
		vi.clearAllMocks();

		stopHistoryPolling();
	});

	it('calling startHistoryPolling twice does not double-subscribe', async () => {
		mockHistoryOk();
		startHistoryPolling();
		startHistoryPolling(); // Should be no-op.
		await vi.advanceTimersByTimeAsync(0);

		// Only 1 poll from the first start.
		expect(fetchHistory).toHaveBeenCalledTimes(1);
	});

	// ── job_finalized event ──

	it('job_finalized triggers a refetch (queue→history transition)', async () => {
		mockHistoryOk();
		startHistoryPolling();
		await vi.advanceTimersByTimeAsync(0);
		vi.clearAllMocks();
		mockHistoryOk();

		expect(capturedWSHandler).not.toBeNull();
		capturedWSHandler!({ event: 'job_finalized', nzo_id: 'nzo_1' });
		await vi.advanceTimersByTimeAsync(0);

		expect(fetchHistory).toHaveBeenCalledTimes(1);
	});

	it('history_updated still triggers a refetch (in-history mutations)', async () => {
		mockHistoryOk();
		startHistoryPolling();
		await vi.advanceTimersByTimeAsync(0);
		vi.clearAllMocks();
		mockHistoryOk();

		capturedWSHandler!({ event: 'history_updated' });
		await vi.advanceTimersByTimeAsync(0);

		expect(fetchHistory).toHaveBeenCalledTimes(1);
	});
});
