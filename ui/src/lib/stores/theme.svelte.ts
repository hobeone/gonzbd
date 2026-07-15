/**
 * Theme store: manages light / dark / system preference.
 *
 * Persists the user's choice to localStorage under the key "theme".
 * The inline script in app.html reads the same key before first paint
 * to avoid a flash of the wrong theme.
 *
 * "system" (the default) follows the OS prefers-color-scheme media query
 * and listens for changes (e.g. macOS auto-switching at sunset).
 */

const STORAGE_KEY = 'theme';

export type ThemeMode = 'light' | 'dark' | 'system';

const CYCLE: ThemeMode[] = ['system', 'light', 'dark'];

let current = $state<ThemeMode>(readStored());

function readStored(): ThemeMode {
	if (typeof localStorage === 'undefined') return 'system';
	const v = localStorage.getItem(STORAGE_KEY);
	if (v === 'light' || v === 'dark') return v;
	return 'system';
}

function applyTheme(mode: ThemeMode) {
	const isDark =
		mode === 'dark' ||
		(mode === 'system' && window.matchMedia('(prefers-color-scheme: dark)').matches);

	document.documentElement.classList.toggle('dark', isDark);
}

/** Reactive getter for the current theme mode. */
export function getTheme(): ThemeMode {
	return current;
}

/** Set the theme to a specific mode. */
function setTheme(mode: ThemeMode) {
	current = mode;
	if (mode === 'system') {
		localStorage.removeItem(STORAGE_KEY);
	} else {
		localStorage.setItem(STORAGE_KEY, mode);
	}
	applyTheme(mode);
}

/** Cycle through system → light → dark → system. */
export function cycleTheme() {
	const idx = CYCLE.indexOf(current);
	setTheme(CYCLE[(idx + 1) % CYCLE.length]);
}

/**
 * Initialize the theme system. Call once from the root layout's onMount.
 * Sets up the OS media query listener so "system" mode reacts to changes.
 * Returns a cleanup function.
 */
export function initTheme(): () => void {
	// Apply in case the inline script in app.html didn't run (SSR).
	applyTheme(current);

	const mql = window.matchMedia('(prefers-color-scheme: dark)');
	const onChange = () => {
		if (current === 'system') applyTheme('system');
	};
	mql.addEventListener('change', onChange);
	return () => mql.removeEventListener('change', onChange);
}
