import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import SidebarNav from './SidebarNav.svelte';

// Mock nested dialog components to avoid rendering their full trees.
vi.mock('./AddNzbDialog.svelte', () => ({
	default: function AddNzbDialogMock() {}
}));
vi.mock('./SettingsDialog.svelte', () => ({
	default: function SettingsDialogMock() {}
}));
vi.mock('./ServerStatusPanel.svelte', () => ({
	default: function ServerStatusPanelMock() {}
}));
vi.mock('./AboutDialog.svelte', () => ({
	default: function AboutDialogMock() {}
}));
vi.mock('$lib/api', () => ({
	postAction: vi.fn().mockResolvedValue({ status: true })
}));

import { postAction } from '$lib/api';

describe('SidebarNav', () => {
	beforeEach(() => {
		vi.clearAllMocks();
	});

	it('renders Go brand badge', () => {
		render(SidebarNav);
		expect(screen.getByText('Go')).toBeInTheDocument();
	});

	it('shows Pause button when not paused', () => {
		render(SidebarNav, { props: { paused: false } });
		expect(screen.getByRole('button', { name: 'Pause' })).toBeInTheDocument();
	});

	it('shows Resume button when paused', () => {
		render(SidebarNav, { props: { paused: true } });
		expect(screen.getByRole('button', { name: 'Resume' })).toBeInTheDocument();
	});

	it('clicking Pause calls postAction with pause', async () => {
		render(SidebarNav, { props: { paused: false } });
		const btn = screen.getByRole('button', { name: 'Pause' });
		await fireEvent.click(btn);
		expect(postAction).toHaveBeenCalledWith('pause');
	});

	it('clicking Resume calls postAction with resume', async () => {
		render(SidebarNav, { props: { paused: true } });
		const btn = screen.getByRole('button', { name: 'Resume' });
		await fireEvent.click(btn);
		expect(postAction).toHaveBeenCalledWith('resume');
	});

	it('calls onpausetoggle callback after toggle', async () => {
		const toggle = vi.fn();
		render(SidebarNav, { props: { paused: false, onpausetoggle: toggle } });
		const btn = screen.getByRole('button', { name: 'Pause' });
		await fireEvent.click(btn);
		expect(toggle).toHaveBeenCalled();
	});

	it('renders Add NZB button', () => {
		render(SidebarNav);
		expect(screen.getByRole('button', { name: 'Add NZB' })).toBeInTheDocument();
	});

	it('renders settings button', () => {
		render(SidebarNav);
		expect(screen.getByRole('button', { name: 'Settings' })).toBeInTheDocument();
	});
});
