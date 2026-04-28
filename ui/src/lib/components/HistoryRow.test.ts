import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import HistoryRow from './HistoryRow.svelte';
import type { HistorySlot } from '$lib/types';
import { retryHistoryJob } from '$lib/stores/history.svelte';

vi.mock('$lib/stores/history.svelte', () => ({
	retryHistoryJob: vi.fn()
}));

vi.mock('$lib/stores/warnings.svelte', () => ({
	showToast: vi.fn()
}));

describe('HistoryRow', () => {
	const baseSlot: HistorySlot = {
		nzo_id: '456',
		name: 'Completed.Job',
		nzb_name: 'Completed.NZB',
		category: 'TV',
		status: 'Completed',
		size: '200 MB',
		completed: 1700000000,
		download_time: 120,
		bytes: 209715200,
		path: '/data/completed/job',
		fail_message: '',
		storage: '30 days',
		script: 'none',
		script_log: '',
		script_line: '',
		meta: '',
		url_info: ''
	};

	beforeEach(() => {
		vi.clearAllMocks();
	});

	it('renders job name', () => {
		const { container } = render(HistoryRow, { slot: baseSlot, onremove: vi.fn() });
		// HistoryRow renders inside a <tr>, need a table wrapper for valid HTML
		expect(container.textContent).toContain('Completed.Job');
	});

	it('renders size', () => {
		const { container } = render(HistoryRow, { slot: baseSlot, onremove: vi.fn() });
		expect(container.textContent).toContain('200 MB');
	});

	it('renders category', () => {
		const { container } = render(HistoryRow, { slot: baseSlot, onremove: vi.fn() });
		expect(container.textContent).toContain('TV');
	});

	it('renders status', () => {
		const { container } = render(HistoryRow, { slot: baseSlot, onremove: vi.fn() });
		expect(container.textContent).toContain('Completed');
	});

	it('shows fail_message for failed jobs', () => {
		const failedSlot = { ...baseSlot, status: 'Failed', fail_message: 'Too many articles failed' };
		const { container } = render(HistoryRow, { slot: failedSlot, onremove: vi.fn() });
		expect(container.textContent).toContain('Too many articles failed');
	});

	it('shows retry button for failed jobs', () => {
		const failedSlot = { ...baseSlot, status: 'Failed', fail_message: 'Error' };
		render(HistoryRow, { slot: failedSlot, onremove: vi.fn() });
		expect(screen.getByTitle('Retry')).toBeInTheDocument();
	});

	it('does not show retry button for completed jobs', () => {
		render(HistoryRow, { slot: baseSlot, onremove: vi.fn() });
		expect(screen.queryByTitle('Retry')).not.toBeInTheDocument();
	});

	it('delete button triggers onremove callback', async () => {
		const onremove = vi.fn();
		render(HistoryRow, { slot: baseSlot, onremove });

		const deleteBtn = screen.getByTitle('Delete');
		await fireEvent.click(deleteBtn);

		expect(onremove).toHaveBeenCalled();
	});

	it('retry button calls retryHistoryJob', async () => {
		vi.mocked(retryHistoryJob).mockResolvedValue(undefined);
		const failedSlot = { ...baseSlot, status: 'Failed', fail_message: 'Error' };
		render(HistoryRow, { slot: failedSlot, onremove: vi.fn() });

		const retryBtn = screen.getByTitle('Retry');
		await fireEvent.click(retryBtn);

		expect(retryHistoryJob).toHaveBeenCalledWith('456');
	});

	// ── Edge cases ──

	it('shows * for empty category', () => {
		const slot = { ...baseSlot, category: '' };
		const { container } = render(HistoryRow, { slot, onremove: vi.fn() });
		expect(container.textContent).toContain('*');
	});

	it('shows -- when completed is 0', () => {
		const slot = { ...baseSlot, completed: 0 };
		const { container } = render(HistoryRow, { slot, onremove: vi.fn() });
		expect(container.textContent).toContain('--');
	});

	// ── Expanded detail view ──

	it('clicking the row expands detail panel', async () => {
		const { container } = render(HistoryRow, { slot: baseSlot, onremove: vi.fn() });
		const row = container.querySelector('tr')!;
		await fireEvent.click(row);

		// Expanded detail should show source info.
		expect(container.textContent).toContain('Source');
		expect(container.textContent).toContain('Completed.NZB');
	});

	it('expanded view shows path', async () => {
		const { container } = render(HistoryRow, { slot: baseSlot, onremove: vi.fn() });
		const row = container.querySelector('tr')!;
		await fireEvent.click(row);
		expect(container.textContent).toContain('/data/completed/job');
	});

	it('expanded view shows download stats', async () => {
		const { container } = render(HistoryRow, { slot: baseSlot, onremove: vi.fn() });
		const row = container.querySelector('tr')!;
		await fireEvent.click(row);
		expect(container.textContent).toContain('Download Stats');
		// 209715200 bytes / 120s = 1.7 MB/s
		expect(container.textContent).toContain('MB/s');
	});

	it('double-clicking the row collapses detail panel', async () => {
		const { container } = render(HistoryRow, { slot: baseSlot, onremove: vi.fn() });
		const row = container.querySelector('tr')!;
		await fireEvent.click(row);
		expect(container.textContent).toContain('Source');

		await fireEvent.click(row);
		expect(container.textContent).not.toContain('Source');
	});

	it('expanded view shows repair summary as N/A when empty', async () => {
		const { container } = render(HistoryRow, { slot: baseSlot, onremove: vi.fn() });
		const row = container.querySelector('tr')!;
		await fireEvent.click(row);
		expect(container.textContent).toContain('Repair Summary');
		expect(container.textContent).toContain('N/A');
	});
});
