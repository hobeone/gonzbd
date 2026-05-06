/**
 * Keyboard shortcut registry and global handler.
 *
 * Components register shortcuts via registerShortcut() on mount
 * and call the returned cleanup function on destroy.
 * The global handler is wired up via <svelte:window onkeydown={handleGlobalShortcut}>
 * in the root page component.
 */

export type ShortcutDef = {
	/** The KeyboardEvent.key value, e.g. 'a', ' ', 'Escape', '?' */
	key: string;
	/** Optional modifier. 'ctrl' matches Ctrl on Win/Linux and Cmd on Mac. */
	mod?: 'ctrl' | 'alt' | 'shift';
	/** Human-readable description for the help overlay. */
	description: string;
	/** Callback invoked when the shortcut fires. */
	action: () => void;
};

type InternalShortcut = ShortcutDef & { _id: number };

let nextId = 0;
let shortcuts = $state<InternalShortcut[]>([]);

/**
 * Register a keyboard shortcut. Returns a cleanup function that
 * unregisters it — call this in onDestroy or the onMount return.
 */
export function registerShortcut(def: ShortcutDef): () => void {
	const id = nextId++;
	const internal: InternalShortcut = { ...def, _id: id };
	shortcuts.push(internal);
	return () => {
		shortcuts = shortcuts.filter((s) => s._id !== id);
	};
}

/**
 * Register multiple shortcuts at once. Returns a single cleanup function.
 */
export function registerShortcuts(defs: ShortcutDef[]): () => void {
	const ids: number[] = [];
	for (const def of defs) {
		const id = nextId++;
		const internal: InternalShortcut = { ...def, _id: id };
		shortcuts.push(internal);
		ids.push(id);
	}
	return () => {
		const idSet = new Set(ids);
		shortcuts = shortcuts.filter((s) => !idSet.has(s._id));
	};
}

/**
 * Returns the current list of registered shortcuts (for the help overlay).
 */
export function getShortcuts(): ShortcutDef[] {
	return shortcuts;
}

/**
 * Reset all shortcuts. Only use in tests.
 */
export function resetShortcuts(): void {
	shortcuts = [];
}

const INPUT_TAGS = new Set(['INPUT', 'TEXTAREA', 'SELECT']);

/**
 * Returns true if the keyboard event target is an editable field
 * where single-key shortcuts should be suppressed.
 */
function isEditableTarget(e: KeyboardEvent): boolean {
	const el = e.target as HTMLElement | null;
	if (!el) return false;
	return INPUT_TAGS.has(el.tagName) || el.isContentEditable;
}

/**
 * Global keydown handler — attach via <svelte:window onkeydown={handleGlobalShortcut}>.
 * Iterates registered shortcuts and fires the first match.
 */
export function handleGlobalShortcut(e: KeyboardEvent): void {
	// Don't fire on held-down key repeats
	if (e.repeat) return;

	const inInput = isEditableTarget(e);

	for (const s of shortcuts) {
		// Check modifier match
		let modMatch: boolean;
		if (s.mod === 'ctrl') {
			// 'ctrl' matches Ctrl (Win/Linux) or Cmd (Mac)
			modMatch = e.ctrlKey || e.metaKey;
		} else if (s.mod === 'alt') {
			modMatch = e.altKey;
		} else if (s.mod === 'shift') {
			modMatch = e.shiftKey;
		} else {
			// No modifier required — must NOT have Ctrl/Meta/Alt pressed
			modMatch = !e.ctrlKey && !e.metaKey && !e.altKey;
		}

		if (!modMatch) continue;

		// Check key match
		if (e.key !== s.key) continue;

		// For single-key shortcuts (no modifier), skip if user is typing
		if (!s.mod && inInput) continue;

		e.preventDefault();
		s.action();
		return;
	}
}
