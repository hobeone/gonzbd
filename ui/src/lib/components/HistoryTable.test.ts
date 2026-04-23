import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import HistoryTable from './HistoryTable.svelte';
import { getHistorySlots, getHistory, getHistoryError, deleteHistoryItem } from '$lib/stores/history.svelte';

// Mock the history store
vi.mock('$lib/stores/history.svelte', () => ({
	getHistorySlots: vi.fn(),
	getHistory: vi.fn(),
	getHistoryError: vi.fn(),
	deleteHistoryItem: vi.fn()
}));

describe('HistoryTable', () => {
	const mockSlots = [
		{
			nzo_id: '456',
			name: 'Failed.Job',
			nzb_name: 'Failed.NZB',
			category: '*',
			status: 'Failed',
			size: '200 MB',
			completed: 1700000000,
			download_time: 100,
			bytes: 204800000,
			path: '/tmp/failed',
			fail_message: 'Too many articles failed'
		}
	];

	beforeEach(() => {
		vi.mocked(getHistorySlots).mockReturnValue(mockSlots as any);
		vi.mocked(getHistory).mockReturnValue({ noofslots: 1 } as any);
		vi.mocked(getHistoryError).mockReturnValue(null);
	});

	it('opens delete confirmation dialog when history row delete is clicked', async () => {
		render(HistoryTable);
		
		const deleteBtn = screen.getByTitle('Delete');
		await fireEvent.click(deleteBtn);

		// Check if the dialog appears
		expect(screen.getByRole('heading', { name: 'Delete History Item' })).toBeInTheDocument();
		expect(screen.getByText(/Are you sure you want to delete/)).toBeInTheDocument();
		
		// The job name should appear in the dialog description
		const matches = screen.getAllByText('Failed.Job');
		expect(matches.length).toBeGreaterThan(1);
	});

	it('calls deleteHistoryItem when confirmed', async () => {
		render(HistoryTable);
		
		await fireEvent.click(screen.getByTitle('Delete'));
		
		const confirmBtn = screen.getByRole('button', { name: 'Delete Item' });
		await fireEvent.click(confirmBtn);

		expect(deleteHistoryItem).toHaveBeenCalledWith('456');
	});
});
