import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi } from 'vitest';
import QueueRow from './QueueRow.svelte';
import type { QueueSlot } from '$lib/types';

// Mock the queue store
vi.mock('$lib/stores/queue.svelte', () => ({
	pauseJob: vi.fn(),
	resumeJob: vi.fn(),
	deleteJob: vi.fn()
}));

describe('QueueRow', () => {
	const mockSlot: QueueSlot = {
		nzo_id: '123',
		name: 'Test.NZB',
		filename: 'Test.NZB',
		category: '*',
		priority: 'Normal',
		status: 'Downloading',
		size: '100 MB',
		sizeleft: '50 MB',
		percentage: '50',
		remaining_bytes: 52428800,
		bytes: 104857600,
		mb: 100,
		mbleft: 50,
		pp: '3',
		script: 'none',
		password: ''
	};

	it('shows the job name', () => {
		render(QueueRow, { slot: mockSlot });
		expect(screen.getByText('Test.NZB')).toBeInTheDocument();
	});

	it('calls deleteJob with deleteFiles=true when checkbox is checked', async () => {
		const { deleteJob } = await import('$lib/stores/queue.svelte');
		render(QueueRow, { slot: mockSlot });
		
		await fireEvent.click(screen.getByTitle('Delete'));

		const checkbox = screen.getByLabelText('Also delete downloaded files from disk');
		await fireEvent.click(checkbox);
		
		const confirmBtn = screen.getByRole('button', { name: 'Delete Job' });
		await fireEvent.click(confirmBtn);

		expect(deleteJob).toHaveBeenCalledWith('123', true);
	});

	it('calls deleteJob with deleteFiles=false when checkbox is NOT checked', async () => {
		const { deleteJob } = await import('$lib/stores/queue.svelte');
		render(QueueRow, { slot: mockSlot });
		
		await fireEvent.click(screen.getByTitle('Delete'));

		const confirmBtn = screen.getByRole('button', { name: 'Delete Job' });
		await fireEvent.click(confirmBtn);

		expect(deleteJob).toHaveBeenCalledWith('123', false);
	});
});
