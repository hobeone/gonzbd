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
vi.mock('$lib/api', () => ({
	postAction: vi.fn().mockResolvedValue({ status: true })
}));

import { postAction } from '$lib/api';

describe('Navbar', () => {
	beforeEach(() => {
		vi.clearAllMocks();
	});

	it('renders SABnzbd title', () => {
		render(Navbar);
		expect(screen.getByText('SABnzbd')).toBeInTheDocument();
	});


	it('shows Pause button when not paused', () => {
		render(Navbar, { props: { paused: false } });
		expect(screen.getByText('Pause')).toBeInTheDocument();
	});

	it('shows Resume button when paused', () => {
		render(Navbar, { props: { paused: true } });
		expect(screen.getByText('Resume')).toBeInTheDocument();
	});

	it('clicking Pause calls postAction with pause', async () => {
		render(Navbar, { props: { paused: false } });
		const btn = screen.getByText('Pause');
		await fireEvent.click(btn);
		expect(postAction).toHaveBeenCalledWith('pause');
	});

	it('clicking Resume calls postAction with resume', async () => {
		render(Navbar, { props: { paused: true } });
		const btn = screen.getByText('Resume');
		await fireEvent.click(btn);
		expect(postAction).toHaveBeenCalledWith('resume');
	});

	it('calls onpausetoggle callback after toggle', async () => {
		const toggle = vi.fn();
		render(Navbar, { props: { paused: false, onpausetoggle: toggle } });
		const btn = screen.getByText('Pause');
		await fireEvent.click(btn);
		expect(toggle).toHaveBeenCalled();
	});

	it('renders + Add NZB button', () => {
		render(Navbar);
		expect(screen.getByText('+ Add NZB')).toBeInTheDocument();
	});

	it('renders settings button with title', () => {
		render(Navbar);
		expect(screen.getByTitle('Settings')).toBeInTheDocument();
	});
});
