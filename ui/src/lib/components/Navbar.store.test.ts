import { render, screen, fireEvent, waitFor } from '@testing-library/svelte';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import Navbar from './Navbar.svelte';

// Unlike Navbar.test.ts, this drives the real queue store (only the network
// edge is mocked), so it shows <Navbar /> without a paused prop following the
// store the way the /status page renders it.
vi.mock('./AddNzbDialog.svelte', () => ({ default: function AddNzbDialogMock() {} }));
vi.mock('./SettingsDialog.svelte', () => ({ default: function SettingsDialogMock() {} }));
vi.mock('./ServerStatusPanel.svelte', () => ({ default: function ServerStatusPanelMock() {} }));
vi.mock('#lib/api.js', () => ({
	fetchQueue: vi.fn(),
	postAction: vi.fn()
}));
vi.mock('#lib/stores/websocket.svelte', () => ({ subscribeWS: vi.fn(() => vi.fn()) }));
vi.mock('#lib/stores/connection.svelte', () => ({
	reportFailure: vi.fn(),
	reportSuccess: vi.fn(),
	onReconnected: vi.fn(() => vi.fn())
}));

import { fetchQueue, postAction } from '#lib/api.js';

// serverPaused plays the server's global flag: postAction flips it and every
// queue fetch reports it.
let serverPaused = false;

describe('Navbar following the queue store', () => {
	beforeEach(() => {
		serverPaused = false;
		vi.mocked(postAction).mockImplementation(async (mode: string) => {
			if (mode === 'pause') serverPaused = true;
			if (mode === 'resume') serverPaused = false;
			return { status: true };
		});
		vi.mocked(fetchQueue).mockImplementation(
			async () =>
				({
					status: true,
					queue: { slots: [], noofslots: 0, paused: serverPaused, speed: '0', timeleft: '0:00:00' }
				}) as never
		);
	});

	it('without a paused prop the button flips to Resume after Pause and back', async () => {
		render(Navbar);
		await fireEvent.click(screen.getByText('Pause'));
		await waitFor(() => expect(screen.getByText('Resume')).toBeInTheDocument());
		expect(postAction).toHaveBeenLastCalledWith('pause');

		// The button stays disabled until the toggle's finally block runs.
		await waitFor(() => expect(screen.getByText('Resume').closest('button')).toBeEnabled());
		await fireEvent.click(screen.getByText('Resume'));
		await waitFor(() => expect(screen.getByText('Pause')).toBeInTheDocument());
		expect(postAction).toHaveBeenLastCalledWith('resume');
	});
});
