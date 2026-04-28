import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

// Mock $lib/api before importing the store
vi.mock('$lib/api', () => ({
	fetchWarnings: vi.fn(),
	postAction: vi.fn()
}));

// We need to import after mocking
import {
	startWarningsPolling,
	stopWarningsPolling,
	getWarnings,
	getWarningCount,
	getWarningsError,
	getToastMessage,
	showToast,
	dismissToast,
	clearWarnings
} from './warnings.svelte';
import { fetchWarnings, postAction } from '$lib/api';

describe('warnings store', () => {
	beforeEach(() => {
		vi.useFakeTimers();
		vi.clearAllMocks();
		stopWarningsPolling();
		dismissToast();
	});

	afterEach(() => {
		stopWarningsPolling();
		vi.useRealTimers();
	});

	describe('showToast / dismissToast', () => {
		it('sets toast message', () => {
			showToast('test message');
			expect(getToastMessage()).toBe('test message');
		});

		it('auto-clears after 5000ms', () => {
			showToast('auto-clear test');
			expect(getToastMessage()).toBe('auto-clear test');

			vi.advanceTimersByTime(4999);
			expect(getToastMessage()).toBe('auto-clear test');

			vi.advanceTimersByTime(1);
			expect(getToastMessage()).toBeNull();
		});

		it('does not clear a different message on timeout', () => {
			showToast('first');
			vi.advanceTimersByTime(2000);

			// Replace with a new message before timeout
			showToast('second');

			// The first timer fires at 5000ms from first showToast
			vi.advanceTimersByTime(3000);
			// 'second' was set 3000ms ago, its timer hasn't fired yet
			expect(getToastMessage()).toBe('second');
		});

		it('dismissToast clears immediately', () => {
			showToast('dismiss me');
			dismissToast();
			expect(getToastMessage()).toBeNull();
		});
	});

	describe('clearWarnings', () => {
		it('calls postAction and re-polls', async () => {
			vi.mocked(postAction).mockResolvedValue({ status: true });
			vi.mocked(fetchWarnings).mockResolvedValue({ status: true, warnings: [] });

			await clearWarnings();

			expect(postAction).toHaveBeenCalledWith('warnings', { name: 'clear' });
			expect(fetchWarnings).toHaveBeenCalled();
		});
	});

	describe('polling lifecycle', () => {
		it('startWarningsPolling fetches immediately and sets interval', async () => {
			vi.mocked(fetchWarnings).mockResolvedValue({ status: true, warnings: ['warn1'] });

			startWarningsPolling();
			// First poll is immediate.
			await vi.advanceTimersByTimeAsync(0);

			expect(fetchWarnings).toHaveBeenCalledTimes(1);
			expect(getWarnings()).toEqual(['warn1']);
			expect(getWarningCount()).toBe(1);
		});

		it('polls on interval', async () => {
			vi.mocked(fetchWarnings).mockResolvedValue({ status: true, warnings: [] });

			startWarningsPolling();
			await vi.advanceTimersByTimeAsync(0);
			expect(fetchWarnings).toHaveBeenCalledTimes(1);

			// Advance past one interval.
			await vi.advanceTimersByTimeAsync(5000);
			expect(fetchWarnings).toHaveBeenCalledTimes(2);
		});

		it('stopWarningsPolling stops the interval', async () => {
			vi.mocked(fetchWarnings).mockResolvedValue({ status: true, warnings: [] });

			startWarningsPolling();
			await vi.advanceTimersByTimeAsync(0);
			stopWarningsPolling();

			await vi.advanceTimersByTimeAsync(10000);
			// Only the initial poll should have happened.
			expect(fetchWarnings).toHaveBeenCalledTimes(1);
		});

		it('double start is a no-op', async () => {
			vi.mocked(fetchWarnings).mockResolvedValue({ status: true, warnings: [] });

			startWarningsPolling();
			startWarningsPolling(); // Should be idempotent.
			await vi.advanceTimersByTimeAsync(0);

			expect(fetchWarnings).toHaveBeenCalledTimes(1);
		});
	});

	describe('poll error handling', () => {
		it('sets error state on fetch failure', async () => {
			vi.mocked(fetchWarnings).mockRejectedValue(new Error('Network down'));

			startWarningsPolling();
			await vi.advanceTimersByTimeAsync(0);

			expect(getWarningsError()).toBe('Network down');
		});

		it('clears error on successful fetch after failure', async () => {
			vi.mocked(fetchWarnings).mockRejectedValueOnce(new Error('fail'));
			vi.mocked(fetchWarnings).mockResolvedValueOnce({ status: true, warnings: [] });

			startWarningsPolling();
			await vi.advanceTimersByTimeAsync(0);
			expect(getWarningsError()).toBe('fail');

			await vi.advanceTimersByTimeAsync(5000);
			expect(getWarningsError()).toBeNull();
		});

		it('sets string error for non-Error throws', async () => {
			vi.mocked(fetchWarnings).mockRejectedValue('string error');

			startWarningsPolling();
			await vi.advanceTimersByTimeAsync(0);

			expect(getWarningsError()).toBe('string error');
		});
	});

	describe('new warning auto-toast', () => {
		it('shows toast when new warnings arrive after initial fetch', async () => {
			// First fetch — seed with existing warnings.
			vi.mocked(fetchWarnings).mockResolvedValueOnce({
				status: true,
				warnings: ['old warning']
			});

			startWarningsPolling();
			await vi.advanceTimersByTimeAsync(0);
			expect(getToastMessage()).toBeNull(); // No toast on initial load.

			// Second fetch — a new warning added.
			vi.mocked(fetchWarnings).mockResolvedValueOnce({
				status: true,
				warnings: ['old warning', 'new warning']
			});
			await vi.advanceTimersByTimeAsync(5000);

			expect(getToastMessage()).toBe('new warning');
		});

		it('auto-toast clears after 5000ms', async () => {
			vi.mocked(fetchWarnings).mockResolvedValueOnce({
				status: true,
				warnings: ['w1']
			});
			startWarningsPolling();
			await vi.advanceTimersByTimeAsync(0);

			vi.mocked(fetchWarnings).mockResolvedValueOnce({
				status: true,
				warnings: ['w1', 'w2']
			});
			await vi.advanceTimersByTimeAsync(5000);
			expect(getToastMessage()).toBe('w2');

			vi.advanceTimersByTime(5000);
			expect(getToastMessage()).toBeNull();
		});
	});
});
