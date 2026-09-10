import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import WarningsBanner from './WarningsBanner.svelte';
import { getWarnings, getWarningCount, getWarningsError, clearWarnings } from '$lib/stores/warnings.svelte';
import { getQueueSlots } from '$lib/stores/queue.svelte';

vi.mock('$lib/stores/warnings.svelte', () => ({
	getWarnings: vi.fn(),
	getWarningCount: vi.fn(),
	getWarningsError: vi.fn(),
	clearWarnings: vi.fn()
}));

vi.mock('$lib/stores/queue.svelte', () => ({
	getQueueSlots: vi.fn()
}));

describe('WarningsBanner', () => {
	beforeEach(() => {
		vi.clearAllMocks();
		vi.mocked(getWarnings).mockReturnValue([]);
		vi.mocked(getWarningCount).mockReturnValue(0);
		vi.mocked(getWarningsError).mockReturnValue(null);
		vi.mocked(getQueueSlots).mockReturnValue([]);
	});

	it('is hidden when no warnings and no errors', () => {
		const { container } = render(WarningsBanner);
		expect(container.querySelector('.rounded-lg')).toBeNull();
	});

	it('shows duplicate NZB banner when queue has duplicate warnings', () => {
		vi.mocked(getQueueSlots).mockReturnValue([
			{ nzo_id: '1', status: 'Paused', warning: 'Duplicate NZB' } as any,
			{ nzo_id: '2', status: 'Paused', warning: 'Duplicate NZB' } as any
		]);
		render(WarningsBanner);

		expect(screen.getByText('Duplicate NZBs found:')).toBeInTheDocument();
		expect(screen.getByText(/2 jobs added in paused state/)).toBeInTheDocument();
	});

	it('counts duplicates whose warning is combined with a parse warning', () => {
		// The backend appends rather than overwrites, so a job that is both
		// malformed and a duplicate carries both clauses in one string. An
		// exact-equality count silently omitted these.
		vi.mocked(getQueueSlots).mockReturnValue([
			{ nzo_id: '1', status: 'Paused', warning: 'Duplicate NZB' } as any,
			{
				nzo_id: '2',
				status: 'Paused',
				warning:
					'NZB had malformed segments discarded at ingest: 1 unusable message-id; Duplicate NZB'
			} as any,
			{
				nzo_id: '3',
				status: 'Paused',
				warning: 'NZB had malformed segments discarded at ingest: 2 empty message-id'
			} as any,
			{ nzo_id: '4', status: 'Paused' } as any
		]);
		render(WarningsBanner);

		expect(screen.getByText('Duplicate NZBs found:')).toBeInTheDocument();
		expect(screen.getByText(/2 jobs added in paused state/)).toBeInTheDocument();
	});

	it('excludes forced duplicates, which are not paused', () => {
		// A forced duplicate is added and left running, so counting it under
		// "added in paused state" would be a false statement about the queue.
		vi.mocked(getQueueSlots).mockReturnValue([
			{ nzo_id: '1', status: 'Paused', warning: 'Duplicate NZB' } as any,
			{ nzo_id: '2', status: 'Downloading', warning: 'Duplicate NZB (Forced)' } as any,
			{
				nzo_id: '3',
				status: 'Downloading',
				warning:
					'NZB had malformed segments discarded at ingest: 1 empty message-id; Duplicate NZB (Forced)'
			} as any
		]);
		render(WarningsBanner);

		expect(screen.getByText(/1 job added in paused state/)).toBeInTheDocument();
	});

	it('stops counting a duplicate once the user resumes it', () => {
		// The backend never clears a job's Warning string once set (it's an
		// append-only audit trail), so resuming a duplicate-flagged job still
		// leaves "Duplicate NZB" in its warning. The banner must key off pause
		// state, not the warning text alone, or resuming never makes it go
		// away — which was the reported bug: the only control offered
		// (unpause) did nothing to this banner.
		vi.mocked(getQueueSlots).mockReturnValue([
			{ nzo_id: '1', status: 'Downloading', warning: 'Duplicate NZB' } as any
		]);
		const { container } = render(WarningsBanner);

		expect(screen.queryByText('Duplicate NZBs found:')).not.toBeInTheDocument();
		expect(container.querySelector('.rounded-lg')).toBeNull();
	});

	it('shows warning count when warnings exist', () => {
		vi.mocked(getWarningCount).mockReturnValue(3);
		vi.mocked(getWarnings).mockReturnValue(['warn1', 'warn2', 'warn3']);
		render(WarningsBanner);

		expect(screen.getByText(/3 warnings/)).toBeInTheDocument();
	});

	it('shows warning list items', () => {
		vi.mocked(getWarningCount).mockReturnValue(2);
		vi.mocked(getWarnings).mockReturnValue(['Disk almost full', 'Server timeout']);
		render(WarningsBanner);

		expect(screen.getByText('Disk almost full')).toBeInTheDocument();
		expect(screen.getByText('Server timeout')).toBeInTheDocument();
	});

	it('collapse/expand toggle works', async () => {
		vi.mocked(getWarningCount).mockReturnValue(1);
		vi.mocked(getWarnings).mockReturnValue(['test warning']);
		render(WarningsBanner);

		// Initially expanded
		expect(screen.getByText('test warning')).toBeInTheDocument();

		// Collapse
		const collapseBtn = screen.getByLabelText('Collapse warnings');
		await fireEvent.click(collapseBtn);

		expect(screen.queryByText('test warning')).not.toBeInTheDocument();

		// Expand again
		const expandBtn = screen.getByLabelText('Expand warnings');
		await fireEvent.click(expandBtn);

		expect(screen.getByText('test warning')).toBeInTheDocument();
	});

	it('clear all button calls clearWarnings', async () => {
		vi.mocked(getWarningCount).mockReturnValue(1);
		vi.mocked(getWarnings).mockReturnValue(['test']);
		vi.mocked(clearWarnings).mockResolvedValue(undefined);
		render(WarningsBanner);

		const clearBtn = screen.getByText('Clear all');
		await fireEvent.click(clearBtn);

		expect(clearWarnings).toHaveBeenCalled();
	});

	it('shows API error message', () => {
		vi.mocked(getWarningCount).mockReturnValue(1);
		vi.mocked(getWarningsError).mockReturnValue('Connection refused');
		render(WarningsBanner);

		expect(screen.getByText(/API error: Connection refused/)).toBeInTheDocument();
	});
});
