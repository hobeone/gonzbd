import '@testing-library/jest-dom/vitest';
import { afterEach } from 'vitest';

// happy-dom implements HTMLDialogElement but not the top-layer behaviour of
// showModal()/close(), so both are no-ops that never flip the `open` property.
// Modal.svelte drives its bound `open` prop off that property and the native
// `close` event, so stub just enough of the lifecycle for components under test
// to open, close, and report their state.
if (typeof HTMLDialogElement !== 'undefined') {
	HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
		this.open = true;
	};
	HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
		this.open = false;
		this.dispatchEvent(new Event('close'));
	};
}
