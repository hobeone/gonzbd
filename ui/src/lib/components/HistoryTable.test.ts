import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import HistoryTable from './HistoryTable.svelte';
import {
	getHistorySlots,
	getHistory,
	getHistoryError,
	deleteHistoryItem,
	getHistoryPage,
	getHistoryLimit,
	setHistoryPage,
	getHistoryFailedOnly,
	setHistoryFailedOnly,
	getHistorySearch,
	setHistorySearch
} from '$lib/stores/history.svelte';

// Mock the history store
vi.mock('$lib/stores/history.svelte', () => ({
	getHistorySlots: vi.fn(),
	getHistory: vi.fn(),
	getHistoryError: vi.fn(),
	deleteHistoryItem: vi.fn(),
	getHistoryPage: vi.fn().mockReturnValue(0),
	getHistoryLimit: vi.fn().mockReturnValue(10),
	setHistoryPage: vi.fn(),
	getHistoryFailedOnly: vi.fn().mockReturnValue(false),
	setHistoryFailedOnly: vi.fn(),
	getHistorySearch: vi.fn().mockReturnValue(''),
	setHistorySearch: vi.fn()
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
		vi.clearAllMocks();
		vi.mocked(getHistorySlots).mockReturnValue(mockSlots as any);
		vi.mocked(getHistory).mockReturnValue({ noofslots: 1 } as any);
		vi.mocked(getHistoryError).mockReturnValue(null);
		vi.mocked(getHistorySearch).mockReturnValue('');
		vi.mocked(getHistoryFailedOnly).mockReturnValue(false);
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

	it('calls deleteHistoryItem with deleteFiles=true when checkbox is checked', async () => {
		render(HistoryTable);
		
		await fireEvent.click(screen.getByTitle('Delete'));

		const checkbox = screen.getByLabelText('Also delete downloaded files from disk');
		await fireEvent.click(checkbox);
		
		const confirmBtn = screen.getByRole('button', { name: 'Delete Item' });
		await fireEvent.click(confirmBtn);

		expect(deleteHistoryItem).toHaveBeenCalledWith('456', true);
	});

	it('calls deleteHistoryItem with deleteFiles=false when NOT checked', async () => {
		render(HistoryTable);
		
		await fireEvent.click(screen.getByTitle('Delete'));
		
		const confirmBtn = screen.getByRole('button', { name: 'Delete Item' });
		await fireEvent.click(confirmBtn);

		expect(deleteHistoryItem).toHaveBeenCalledWith('456', false);
	});

		it('updates search automatically after typing (debounced)', async () => {
			vi.useFakeTimers();
			render(HistoryTable);

			const searchInput = screen.getByPlaceholderText('Search history...');

			// Type 't'
			await fireEvent.input(searchInput, { target: { value: 't' } });
			expect(setHistorySearch).not.toHaveBeenCalled();

			// Type 'te'
			await fireEvent.input(searchInput, { target: { value: 'te' } });

			// Advance time partially (150ms)
			await vi.advanceTimersByTimeAsync(150);
			expect(setHistorySearch).not.toHaveBeenCalled();

			// Type 'test'
			await fireEvent.input(searchInput, { target: { value: 'test' } });

			// Advance time fully (300ms)
			await vi.advanceTimersByTimeAsync(300);

			expect(setHistorySearch).toHaveBeenCalledWith('test');
			expect(setHistorySearch).toHaveBeenCalledTimes(1);

			vi.useRealTimers();
		});

		it('updates failed-only toggle when clicked', async () => {
		render(HistoryTable);

		const checkbox = screen.getByLabelText('Failed only');
		await fireEvent.click(checkbox);

		expect(setHistoryFailedOnly).toHaveBeenCalledWith(true);
		});
		});

