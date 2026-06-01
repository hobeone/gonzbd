import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

// Stub window.location to capture redirect href assignments
const mockLocation = {
	href: 'http://localhost/app',
	origin: 'http://localhost',
	pathname: '/app',
	searchParams: new URLSearchParams()
};

const originalLocation = window.location;
delete (window as any).location;
window.location = mockLocation as any;

// Stub fetch
const mockFetch = vi.fn();
vi.stubGlobal('fetch', mockFetch);

// Import the store under test
import {
	isConnected,
	isAuthExpired,
	getLastError,
	getNextRetryAt,
	isProbing,
	reportFailure,
	reportDisconnect,
	reportAuthExpired,
	reportSuccess,
	retryNow,
	onReconnected
} from './connection.svelte';

describe('ConnectionStore', () => {
	beforeEach(() => {
		vi.useFakeTimers();
		vi.clearAllMocks();
		mockFetch.mockReset();
		
		// Reset location mockup
		mockLocation.href = 'http://localhost/app';
		
		// Restore to clean/success state before each test
		reportSuccess();
	});

	afterEach(() => {
		vi.useRealTimers();
	});

	it('should initialize with connected state', () => {
		expect(isConnected()).toBe(true);
		expect(isAuthExpired()).toBe(false);
		expect(getLastError()).toBeNull();
		expect(getNextRetryAt()).toBeNull();
		expect(isProbing()).toBe(false);
	});

	it('should require two failures to transition to disconnected', () => {
		reportFailure('first error');
		// Should still be connected
		expect(isConnected()).toBe(true);
		expect(getLastError()).toBe('first error');

		reportFailure('second error');
		// Should now be disconnected
		expect(isConnected()).toBe(false);
		expect(getLastError()).toBe('second error');
	});

	it('should disconnect immediately on websocket disconnect', () => {
		reportDisconnect('socket closed');
		expect(isConnected()).toBe(false);
		expect(getLastError()).toBe('socket closed');
	});

	it('should clear errors and reset on success', () => {
		reportDisconnect('socket closed');
		expect(isConnected()).toBe(false);

		reportSuccess();
		expect(isConnected()).toBe(true);
		expect(getLastError()).toBeNull();
	});

	it('should trigger registered onReconnected callbacks upon reconnection', () => {
		const callback = vi.fn();
		const unsubscribe = onReconnected(callback);

		// Disconnect first
		reportDisconnect('socket closed');
		expect(callback).not.toHaveBeenCalled();

		// Reconnect
		reportSuccess();
		expect(callback).toHaveBeenCalledTimes(1);

		// Disconnect and reconnect again
		reportDisconnect('socket closed');
		reportSuccess();
		expect(callback).toHaveBeenCalledTimes(2);

		// Unsubscribe and verify no more calls
		unsubscribe();
		reportDisconnect('socket closed');
		reportSuccess();
		expect(callback).toHaveBeenCalledTimes(2);
	});

	it('should set authExpired and disconnect on reportAuthExpired', () => {
		reportAuthExpired();
		expect(isConnected()).toBe(false);
		expect(isAuthExpired()).toBe(true);
		expect(getLastError()).toBe('Authentication expired');
	});

	it('should schedule exponential backoff health probes when disconnected', async () => {
		// Mock a failing fetch
		mockFetch.mockRejectedValue(new Error('Network error'));

		reportDisconnect('socket closed');
		expect(isConnected()).toBe(false);
		expect(getNextRetryAt()).not.toBeNull();

		const initialRetryAt = getNextRetryAt()!;
		expect(initialRetryAt).toBeGreaterThan(Date.now());

		// Advance timers to trigger the first probe
		await vi.runOnlyPendingTimersAsync();

		expect(mockFetch).toHaveBeenCalledWith('/api?mode=version&output=json');
		
		// Still disconnected, so next probe should be scheduled (with exponential backoff)
		expect(isConnected()).toBe(false);
		expect(getNextRetryAt()).not.toBeNull();
		expect(getNextRetryAt()!).toBeGreaterThan(initialRetryAt);
	});

	it('should reconnect on successful health probe', async () => {
		// Mock a successful fetch
		mockFetch.mockResolvedValue({
			ok: true,
			url: 'http://localhost/api?mode=version&output=json'
		});

		const callback = vi.fn();
		onReconnected(callback);

		reportDisconnect('socket closed');
		expect(isConnected()).toBe(false);

		// Advance timers to trigger probe
		await vi.runOnlyPendingTimersAsync();

		expect(isConnected()).toBe(true);
		expect(callback).toHaveBeenCalled();
	});

	it('should trigger authExpired and redirect on redirect responses during probe', async () => {
		// Mock a redirect response (response URL differs and not starting with /api)
		mockFetch.mockResolvedValue({
			ok: true,
			redirected: true,
			url: 'http://localhost/login?redirect=%2Fapi'
		});

		reportDisconnect('socket closed');
		expect(isConnected()).toBe(false);

		// Trigger probe
		await vi.runOnlyPendingTimersAsync();

		// Should flag auth expired immediately
		expect(isAuthExpired()).toBe(true);
		expect(isConnected()).toBe(false);

		// Redirection happens after 1.5s timeout
		await vi.advanceTimersByTimeAsync(1500);

		// Redirect URL should have been updated with the current SPA location
		expect(mockLocation.href).toContain('http://localhost/login');
		expect(mockLocation.href).toContain('redirect=http%3A%2F%2Flocalhost%2Fapp');
	});

	it('should cancel pending timers and probe immediately when retryNow is called', () => {
		mockFetch.mockResolvedValue({
			ok: true,
			url: 'http://localhost/api?mode=version&output=json'
		});

		reportDisconnect('socket closed');
		expect(getNextRetryAt()).not.toBeNull();

		retryNow();
		
		// Probing starts synchronously on call
		expect(isProbing()).toBe(true);
		expect(getNextRetryAt()).toBeNull();
	});
});
