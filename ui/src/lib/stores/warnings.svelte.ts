import { fetchWarnings, postAction } from '$lib/api';
import { subscribeWS } from './websocket.svelte';
import { reportFailure, reportSuccess, onReconnected } from './connection.svelte';

class WarningStore {
	#warnings = $state<string[]>([]);
	#error = $state<string | null>(null);
	#toastMessage = $state<string | null>(null);
	#wsCleanup: (() => void) | null = null;
	#reconnectCleanup: (() => void) | null = null;

	get warnings() { return this.#warnings; }
	get warningCount() { return this.#warnings.length; }
	get error() { return this.#error; }
	get toastMessage() { return this.#toastMessage; }

	async poll() {
		try {
			const res = await fetchWarnings();
			const prev = this.#warnings.length;
			this.#warnings = res.warnings;
			this.#error = null;
			reportSuccess();
			if (this.#warnings.length > prev && prev > 0) {
				this.#toastMessage = this.#warnings[this.#warnings.length - 1];
				setTimeout(() => (this.#toastMessage = null), 5000);
			}
		} catch (e) {
			const msg = e instanceof Error ? e.message : String(e);
			this.#error = msg;
			reportFailure(msg);
		}
	}

	start() {
		if (this.#wsCleanup) return;
		// Initial fetch on page load.
		this.poll();

		// React to WS events instead of polling on a timer.
		this.#wsCleanup = subscribeWS((event) => {
			if (event.event === 'warnings_updated') {
				this.poll();
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
	}

	showToast(message: string) {
		this.#toastMessage = message;
		setTimeout(() => {
			if (this.#toastMessage === message) this.#toastMessage = null;
		}, 5000);
	}

	dismissToast() {
		this.#toastMessage = null;
	}

	async clear() {
		await postAction('warnings', { name: 'clear' });
		await this.poll();
	}
}

const store = new WarningStore();

// Exported wrapper functions to maintain API compatibility with components
export const startWarningsPolling = () => store.start();
export const stopWarningsPolling = () => store.stop();
export const getWarnings = () => store.warnings;
export const getWarningCount = () => store.warningCount;
export const getWarningsError = () => store.error;
export const getToastMessage = () => store.toastMessage;
export const showToast = (message: string) => store.showToast(message);
export const dismissToast = () => store.dismissToast();
export const clearWarnings = () => store.clear();
