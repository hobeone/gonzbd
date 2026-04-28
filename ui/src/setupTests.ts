import '@testing-library/jest-dom/vitest';

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
