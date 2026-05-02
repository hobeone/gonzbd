import { fetchWarnings, postAction } from '$lib/api';

const POLL_INTERVAL = 5000;

class WarningStore {
	#warnings = $state<string[]>([]);
	#error = $state<string | null>(null);
	#toastMessage = $state<string | null>(null);
	#timer: ReturnType<typeof setInterval> | null = null;

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
			if (this.#warnings.length > prev && prev > 0) {
				this.#toastMessage = this.#warnings[this.#warnings.length - 1];
				setTimeout(() => (this.#toastMessage = null), 5000);
			}
		} catch (e) {
			this.#error = e instanceof Error ? e.message : String(e);
		}
	}

	start() {
		if (this.#timer) return;
		this.poll();
		this.#timer = setInterval(() => this.poll(), POLL_INTERVAL);
	}

	stop() {
		if (this.#timer) {
			clearInterval(this.#timer);
			this.#timer = null;
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
