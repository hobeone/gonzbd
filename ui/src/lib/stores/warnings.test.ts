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
});
