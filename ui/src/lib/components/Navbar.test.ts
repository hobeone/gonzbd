import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import Navbar from './Navbar.svelte';

// Mock nested dialog components to avoid rendering their full trees.
vi.mock('./AddNzbDialog.svelte', () => ({
	default: function AddNzbDialogMock() {}
}));
vi.mock('./SettingsDialog.svelte', () => ({
	default: function SettingsDialogMock() {}
}));
vi.mock('#lib/stores/queue.svelte.js', async (importOriginal) => ({
	...(await importOriginal<typeof import('#lib/stores/queue.svelte.js')>()),
	pauseAll: vi.fn().mockResolvedValue(undefined),
	resumeAll: vi.fn().mockResolvedValue(undefined)
}));

import { pauseAll, resumeAll } from '#lib/stores/queue.svelte.js';

describe('Navbar', () => {
	beforeEach(() => {
		vi.clearAllMocks();
	});

	it('renders GoNZBD title', () => {
		render(Navbar);
		expect(screen.getByText('GoNZBD')).toBeInTheDocument();
	});


	it('shows Pause button when not paused', () => {
		render(Navbar, { props: { paused: false } });
		expect(screen.getByText('Pause')).toBeInTheDocument();
	});

	it('shows Resume button when paused', () => {
		render(Navbar, { props: { paused: true } });
		expect(screen.getByText('Resume')).toBeInTheDocument();
	});

	// pauseAll/resumeAll post the action and then re-poll the queue (see the
	// store tests), which is what flips the button; the component must go
	// through them rather than posting on its own.
	it('clicking Pause calls pauseAll only', async () => {
		render(Navbar, { props: { paused: false } });
		await fireEvent.click(screen.getByText('Pause'));
		expect(pauseAll).toHaveBeenCalledTimes(1);
		expect(resumeAll).not.toHaveBeenCalled();
	});

	it('clicking Resume calls resumeAll only', async () => {
		render(Navbar, { props: { paused: true } });
		await fireEvent.click(screen.getByText('Resume'));
		expect(resumeAll).toHaveBeenCalledTimes(1);
		expect(pauseAll).not.toHaveBeenCalled();
	});

	it('renders + Add NZB button', () => {
		render(Navbar);
		expect(screen.getByText('Add NZB')).toBeInTheDocument();
	});

	it('renders settings button with title', () => {
		render(Navbar);
		expect(screen.getByTitle('Settings')).toBeInTheDocument();
	});
});
