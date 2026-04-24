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
			{ nzo_id: '1', warning: 'Duplicate NZB' } as any,
			{ nzo_id: '2', warning: 'Duplicate NZB' } as any
		]);
		render(WarningsBanner);

		expect(screen.getByText('Duplicate NZBs found:')).toBeInTheDocument();
		expect(screen.getByText(/2 jobs added in paused state/)).toBeInTheDocument();
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
