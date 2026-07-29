import '@testing-library/jest-dom/vitest';
import { afterEach } from 'vitest';

// Suppress Svelte 5 "derived_inert" warnings emitted by bits-ui
// components (Dialog, Tabs) during jsdom test teardown. These are
// false positives: bits-ui internal $derived state gets read when the
// component is destroyed in the test environment, which doesn't happen
// in real browsers. Filtering keeps test output clean so real warnings
// remain visible.
const _origWarn = console.warn;
console.warn = (...args: unknown[]) => {
	if (typeof args[0] === 'string' && args[0].includes('derived_inert')) return;
	_origWarn.apply(console, args);
};

// bits-ui Dialog sets a setTimeout for body-scroll-lock cleanup.
// When JSDOM is torn down between tests, the timer fires and tries to
// access document.body which no longer exists, causing an unhandled
// "document is not defined" ReferenceError. Suppress this specific
// error since it's harmless in test environments.
const _origSetTimeout = globalThis.setTimeout;
(globalThis as any).setTimeout = function patchedSetTimeout(fn: any, ms?: number, ...args: any[]): any {
	const wrapped = typeof fn === 'function' ? function (this: unknown) {
		try {
			return fn.apply(this, args);
		} catch (e) {
			// Suppress the known bits-ui body-scroll-lock error
			if (e instanceof ReferenceError && (e as Error).message?.includes('document is not defined')) {
				return;
			}
			throw e;
		}
	} : fn;
	return _origSetTimeout(wrapped, ms);
};
if (typeof HTMLDialogElement !== 'undefined') {
	HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
		this.open = true;
	};
	HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
		this.open = false;
		this.dispatchEvent(new Event('close'));
	};
}
