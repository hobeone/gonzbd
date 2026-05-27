/**
 * ConnectionStore — centralized backend connectivity tracker.
 *
 * Owns all reconnection logic: when any store (queue, history, warnings)
 * or the WebSocket reports a failure, ConnectionStore tracks the state,
 * runs a lightweight health probe with exponential backoff, and signals
 * all consumers when connectivity is restored.
 *
 * Design:
 *  - HTTP failures: 2 consecutive → "disconnected" (avoids flashing on a single dropped request)
 *  - WebSocket close: immediate "disconnected" (authoritative signal)
 *  - Backoff: 1s → 2s → 4s → 8s → 16s → 32s → 60s (cap) with ±20% jitter
 *  - Health probe: GET /api?mode=version&output=json (lightweight, side-effect-free)
 *  - On reconnect: fires all registered onReconnected callbacks
 */

const MIN_DELAY = 1000;
const MAX_DELAY = 60000;
const JITTER = 0.2;
const FAILURE_THRESHOLD = 2;

type ReconnectCallback = () => void;

class ConnectionStore {
	#connected = $state(true);
	#authExpired = $state(false);
	#retryCount = $state(0);
	#lastError = $state<string | null>(null);
	#nextRetryAt = $state<number | null>(null); // timestamp (Date.now() + delay)
	#probing = $state(false);

	#retryTimer: ReturnType<typeof setTimeout> | null = null;
	#consecutiveFailures = 0;
	#callbacks = new Set<ReconnectCallback>();

	get connected() {
		return this.#connected;
	}
	get authExpired() {
		return this.#authExpired;
	}
	get retryCount() {
		return this.#retryCount;
	}
	get lastError() {
		return this.#lastError;
	}
	get nextRetryAt() {
		return this.#nextRetryAt;
	}
	get probing() {
		return this.#probing;
	}

	reportAuthExpired(): void {
		this.#authExpired = true;
		this.#connected = false;
		this.#lastError = 'Authentication expired';
		this.#consecutiveFailures = FAILURE_THRESHOLD;
		this.#clearTimer();
	}

	/**
	 * Called by any store when an HTTP fetch fails.
	 * Uses a threshold (2 failures) to avoid flashing on transient errors.
	 */
	reportFailure(error: string): void {
		this.#consecutiveFailures++;
		this.#lastError = error;

		if (this.#consecutiveFailures >= FAILURE_THRESHOLD && this.#connected) {
			this.#connected = false;
			this.#startBackoff();
		}
	}

	/**
	 * Called when the WebSocket closes. This is an authoritative signal
	 * that the backend is unreachable — bypasses the failure threshold
	 * and immediately shows the overlay.
	 */
	reportDisconnect(error: string): void {
		this.#lastError = error;
		this.#consecutiveFailures = FAILURE_THRESHOLD;
		if (this.#connected) {
			this.#connected = false;
			this.#startBackoff();
		}
	}

	/**
	 * Called by any store when an HTTP fetch or WS connection succeeds.
	 */
	reportSuccess(): void {
		if (!this.#connected) {
			this.#connected = true;
			this.#fireReconnected();
		}
		this.#authExpired = false;
		this.#consecutiveFailures = 0;
		this.#retryCount = 0;
		this.#lastError = null;
		this.#nextRetryAt = null;
		this.#clearTimer();
	}

	/**
	 * Manually trigger an immediate reconnection attempt (e.g. "Retry Now" button).
	 */
	retryNow(): void {
		this.#clearTimer();
		this.#probe();
	}

	/**
	 * Register a callback to be fired when connectivity is restored.
	 * Returns an unsubscribe function.
	 */
	onReconnected(callback: ReconnectCallback): () => void {
		this.#callbacks.add(callback);
		return () => {
			this.#callbacks.delete(callback);
		};
	}

	#startBackoff(): void {
		if (this.#retryTimer) return; // already scheduled
		this.#scheduleProbe();
	}

	#scheduleProbe(): void {
		const baseDelay = Math.min(MIN_DELAY * Math.pow(2, this.#retryCount), MAX_DELAY);
		const jitter = baseDelay * JITTER * (2 * Math.random() - 1); // ±20%
		const delay = Math.round(baseDelay + jitter);

		this.#nextRetryAt = Date.now() + delay;
		this.#retryTimer = setTimeout(() => {
			this.#retryTimer = null;
			this.#probe();
		}, delay);
	}

	async #probe(): Promise<void> {
		this.#probing = true;
		this.#nextRetryAt = null;

		try {
			const res = await fetch('/api?mode=version&output=json');
			
			if (!res || !res.url) {
				if (res && res.ok) {
					this.#probing = false;
					this.reportSuccess();
					return;
				}
			} else {
				// Detect redirect on health probe
				const reqURL = new URL('/api?mode=version&output=json', window.location.origin);
				const resURL = new URL(res.url);
				const crossOriginRedirect = resURL.origin !== reqURL.origin;
				const sameOriginAuthRedirect = res.redirected && !resURL.pathname.startsWith('/api');

				if (crossOriginRedirect || sameOriginAuthRedirect) {
					this.#probing = false;
					this.reportAuthExpired();

					// Rewrite any redirect parameters containing "/api" to point to the current SPA location
					for (const [key, value] of resURL.searchParams.entries()) {
						if (value.includes('/api')) {
							resURL.searchParams.set(key, window.location.href);
						}
					}

					setTimeout(() => {
						window.location.href = resURL.href;
					}, 1500);
					return;
				}
			}

			if (res && res.ok) {
				this.#probing = false;
				this.reportSuccess();
				return;
			}
		} catch {
			// Network error — still disconnected
		}

		this.#probing = false;
		this.#retryCount++;
		// Still disconnected, schedule next probe
		if (!this.#connected) {
			this.#scheduleProbe();
		}
	}

	#fireReconnected(): void {
		for (const cb of this.#callbacks) {
			try {
				cb();
			} catch (e) {
				console.error('onReconnected callback error:', e);
			}
		}
	}

	#clearTimer(): void {
		if (this.#retryTimer) {
			clearTimeout(this.#retryTimer);
			this.#retryTimer = null;
		}
	}
}

const store = new ConnectionStore();

export const isConnected = () => store.connected;
export const isAuthExpired = () => store.authExpired;
export const getRetryCount = () => store.retryCount;
export const getLastError = () => store.lastError;
export const getNextRetryAt = () => store.nextRetryAt;
export const isProbing = () => store.probing;
export const reportFailure = (error: string) => store.reportFailure(error);
export const reportDisconnect = (error: string) => store.reportDisconnect(error);
export const reportAuthExpired = () => store.reportAuthExpired();
export const reportSuccess = () => store.reportSuccess();
export const retryNow = () => store.retryNow();
export const onReconnected = (cb: () => void) => store.onReconnected(cb);
