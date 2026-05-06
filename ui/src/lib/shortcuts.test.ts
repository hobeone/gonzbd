import { describe, it, expect, vi, beforeEach } from 'vitest';
import {
	registerShortcut,
	registerShortcuts,
	getShortcuts,
	handleGlobalShortcut,
	resetShortcuts
} from './shortcuts.svelte';

/**
 * Create a synthetic KeyboardEvent with sensible defaults.
 */
function makeKeyEvent(
	key: string,
	opts: {
		ctrlKey?: boolean;
		metaKey?: boolean;
		altKey?: boolean;
		shiftKey?: boolean;
		repeat?: boolean;
		target?: Partial<HTMLElement>;
	} = {}
): KeyboardEvent {
	const event = new KeyboardEvent('keydown', {
		key,
		ctrlKey: opts.ctrlKey ?? false,
		metaKey: opts.metaKey ?? false,
		altKey: opts.altKey ?? false,
		shiftKey: opts.shiftKey ?? false,
		repeat: opts.repeat ?? false,
		bubbles: true,
		cancelable: true
	});

	// Override target if provided (KeyboardEvent target is read-only)
	if (opts.target) {
		Object.defineProperty(event, 'target', { value: opts.target, writable: false });
	}

	return event;
}

describe('shortcuts registry', () => {
	beforeEach(() => {
		resetShortcuts();
	});

	it('registers and unregisters a shortcut', () => {
		const unregister = registerShortcut({
			key: 'a',
			description: 'Test shortcut',
			action: () => {}
		});

		expect(getShortcuts()).toHaveLength(1);
		expect(getShortcuts()[0].key).toBe('a');

		unregister();
		expect(getShortcuts()).toHaveLength(0);
	});

	it('registers multiple shortcuts at once', () => {
		const unregister = registerShortcuts([
			{ key: 'a', description: 'A', action: () => {} },
			{ key: 'b', description: 'B', action: () => {} },
			{ key: 'c', description: 'C', action: () => {} }
		]);

		expect(getShortcuts()).toHaveLength(3);

		unregister();
		expect(getShortcuts()).toHaveLength(0);
	});
});

describe('handleGlobalShortcut', () => {
	beforeEach(() => {
		resetShortcuts();
	});

	it('fires the matching shortcut action', () => {
		const action = vi.fn();
		registerShortcut({ key: 'a', description: 'Test', action });

		const event = makeKeyEvent('a');
		handleGlobalShortcut(event);

		expect(action).toHaveBeenCalledOnce();
	});

	it('calls preventDefault on match', () => {
		const action = vi.fn();
		registerShortcut({ key: 'a', description: 'Test', action });

		const event = makeKeyEvent('a');
		const spy = vi.spyOn(event, 'preventDefault');
		handleGlobalShortcut(event);

		expect(spy).toHaveBeenCalledOnce();
	});

	it('does not fire on non-matching key', () => {
		const action = vi.fn();
		registerShortcut({ key: 'a', description: 'Test', action });

		handleGlobalShortcut(makeKeyEvent('b'));

		expect(action).not.toHaveBeenCalled();
	});

	it('does not fire on repeat events', () => {
		const action = vi.fn();
		registerShortcut({ key: 'a', description: 'Test', action });

		handleGlobalShortcut(makeKeyEvent('a', { repeat: true }));

		expect(action).not.toHaveBeenCalled();
	});

	it('skips single-key shortcuts when target is an INPUT', () => {
		const action = vi.fn();
		registerShortcut({ key: 'a', description: 'Test', action });

		const event = makeKeyEvent('a', {
			target: { tagName: 'INPUT', isContentEditable: false } as Partial<HTMLElement>
		});
		handleGlobalShortcut(event);

		expect(action).not.toHaveBeenCalled();
	});

	it('skips single-key shortcuts when target is a TEXTAREA', () => {
		const action = vi.fn();
		registerShortcut({ key: 'a', description: 'Test', action });

		const event = makeKeyEvent('a', {
			target: { tagName: 'TEXTAREA', isContentEditable: false } as Partial<HTMLElement>
		});
		handleGlobalShortcut(event);

		expect(action).not.toHaveBeenCalled();
	});

	it('skips single-key shortcuts when target is contentEditable', () => {
		const action = vi.fn();
		registerShortcut({ key: 'a', description: 'Test', action });

		const event = makeKeyEvent('a', {
			target: { tagName: 'DIV', isContentEditable: true } as Partial<HTMLElement>
		});
		handleGlobalShortcut(event);

		expect(action).not.toHaveBeenCalled();
	});

	it('fires modifier shortcuts even when in an input', () => {
		const action = vi.fn();
		registerShortcut({ key: 'n', mod: 'ctrl', description: 'Test', action });

		const event = makeKeyEvent('n', {
			ctrlKey: true,
			target: { tagName: 'INPUT', isContentEditable: false } as Partial<HTMLElement>
		});
		handleGlobalShortcut(event);

		expect(action).toHaveBeenCalledOnce();
	});

	it('matches ctrl mod with metaKey (Mac Cmd)', () => {
		const action = vi.fn();
		registerShortcut({ key: 'n', mod: 'ctrl', description: 'Test', action });

		handleGlobalShortcut(makeKeyEvent('n', { metaKey: true }));

		expect(action).toHaveBeenCalledOnce();
	});

	it('does not fire single-key shortcut when Ctrl is held', () => {
		const action = vi.fn();
		registerShortcut({ key: 'a', description: 'Test', action });

		handleGlobalShortcut(makeKeyEvent('a', { ctrlKey: true }));

		expect(action).not.toHaveBeenCalled();
	});

	it('does not fire single-key shortcut when Meta is held', () => {
		const action = vi.fn();
		registerShortcut({ key: 'a', description: 'Test', action });

		handleGlobalShortcut(makeKeyEvent('a', { metaKey: true }));

		expect(action).not.toHaveBeenCalled();
	});

	it('handles Space key correctly', () => {
		const action = vi.fn();
		registerShortcut({ key: ' ', description: 'Test', action });

		handleGlobalShortcut(makeKeyEvent(' '));

		expect(action).toHaveBeenCalledOnce();
	});

	it('fires first matching shortcut when multiple are registered', () => {
		const action1 = vi.fn();
		const action2 = vi.fn();
		registerShortcut({ key: 'a', description: 'First', action: action1 });
		registerShortcut({ key: 'a', description: 'Second', action: action2 });

		handleGlobalShortcut(makeKeyEvent('a'));

		expect(action1).toHaveBeenCalledOnce();
		expect(action2).not.toHaveBeenCalled();
	});
});
