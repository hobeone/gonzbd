import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import QueueTable from './QueueTable.svelte';
import {
	getQueueSlots,
	getQueue,
	getError,
	deleteJob,
	getQueuePage,
	getQueueLimit,
	setQueuePage,
	getQueueSearch,
	setQueueSearch
} from '$lib/stores/queue.svelte';

// Mock the queue store
vi.mock('$lib/stores/queue.svelte', () => ({
	getQueueSlots: vi.fn(),
	getQueue: vi.fn(),
	getError: vi.fn(),
	deleteJob: vi.fn(),
	getQueuePage: vi.fn().mockReturnValue(0),
	getQueueLimit: vi.fn().mockReturnValue(10),
	setQueuePage: vi.fn(),
	getQueueSearch: vi.fn().mockReturnValue(''),
	setQueueSearch: vi.fn()
}));

describe('QueueTable', () => {
	const mockSlots = [
		{
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
		}
	];

	beforeEach(() => {
		vi.clearAllMocks();
		vi.mocked(getQueueSlots).mockReturnValue(mockSlots as any);
		vi.mocked(getQueue).mockReturnValue({ noofslots_total: 1 } as any);
		vi.mocked(getError).mockReturnValue(null);
		vi.mocked(getQueueSearch).mockReturnValue('');
	});


	it('opens delete confirmation dialog when row delete is clicked', async () => {
		render(QueueTable);
		
		const deleteBtn = screen.getByTitle('Delete');
		await fireEvent.click(deleteBtn);

		// Check if the dialog appears
		expect(screen.getByRole('heading', { name: 'Delete Job' })).toBeInTheDocument();
		expect(screen.getByText(/Are you sure you want to delete/)).toBeInTheDocument();
		
		// The job name should appear in the dialog description (multiple matches exist because of the row)
		const matches = screen.getAllByText('Test.NZB');
		expect(matches.length).toBeGreaterThan(1);
	});

	it('calls deleteJob with correct parameters from the table dialog', async () => {
		render(QueueTable);

		await fireEvent.click(screen.getByTitle('Delete'));

		const checkbox = screen.getByLabelText('Also delete downloaded files from disk');
		await fireEvent.click(checkbox);

		const confirmBtn = screen.getByRole('button', { name: 'Delete Job' });
		await fireEvent.click(confirmBtn);

		expect(deleteJob).toHaveBeenCalledWith('123', true);
	});

	it('updates search automatically after typing (debounced)', async () => {
		vi.useFakeTimers();
		render(QueueTable);

		const searchInput = screen.getByPlaceholderText('Search queue...');
		await fireEvent.input(searchInput, { target: { value: 'q-test' } });

		expect(setQueueSearch).not.toHaveBeenCalled();

		await vi.advanceTimersByTimeAsync(300);

		expect(setQueueSearch).toHaveBeenCalledWith('q-test');
		vi.useRealTimers();
	});

	it('shows empty state when no slots', () => {
		vi.mocked(getQueueSlots).mockReturnValue([]);
		vi.mocked(getQueue).mockReturnValue({ noofslots_total: 0 } as any);
		render(QueueTable);

		expect(screen.getByText(/Queue is empty/i)).toBeInTheDocument();
	});

	it('shows error banner when getError returns non-null', () => {
		vi.mocked(getError).mockReturnValue('Connection refused');
		render(QueueTable);

		expect(screen.getByText(/Connection refused/)).toBeInTheDocument();
	});
	});

