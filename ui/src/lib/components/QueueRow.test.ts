import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import QueueRow from './QueueRow.svelte';
import type { QueueSlot } from '$lib/types';

vi.mock('$lib/stores/queue.svelte', () => ({
	pauseJob: vi.fn().mockResolvedValue(undefined),
	resumeJob: vi.fn().mockResolvedValue(undefined)
}));

vi.mock('$lib/api', () => ({
	fetchQueueJobDetail: vi.fn()
}));

vi.mock('$lib/stores/websocket.svelte', () => ({
	subscribeWS: vi.fn().mockReturnValue(() => {})
}));

import { pauseJob, resumeJob } from '$lib/stores/queue.svelte';
import { fetchQueueJobDetail } from '$lib/api';

describe('QueueRow', () => {
	const baseSlot: QueueSlot = {
		nzo_id: '123',
		name: 'Test.NZB',
		filename: 'Test.NZB',
		cat: 'TV',
		priority: 'Normal',
		status: 'Downloading',
		size: '100 MB',
		sizeleft: '50 MB',
		percentage: '50.5',
		remaining_bytes: 52428800,
		bytes: 104857600,
		mb: 100,
		mbleft: 50,
		pp: '3',
		script: 'none',
		password: '',
		failed_bytes: 0,
		par2_bytes: 0,
		par2_files: 0,
		current_stage: 'download',
		articles_remaining: 100,
		eta_seconds: 0,
		current_file: ''
	};

	beforeEach(() => vi.clearAllMocks());

	it('renders progress bar and percentage', () => {
		render(QueueRow, { slot: baseSlot, onremove: () => {} });
		
		// Percentage text
		expect(screen.getByText('50.5%')).toBeInTheDocument();
		
		// Progress bar (shadcn Progress uses bits-ui primitive which has progress role)
		const progress = screen.getByRole('progressbar');
		expect(progress).toBeInTheDocument();
		expect(progress.getAttribute('aria-valuenow')).toBe('50.5');
	});

	it('applies pulse animation for active jobs', () => {
		const { container } = render(QueueRow, { slot: baseSlot, onremove: () => {} });
		
		// Find progress element
		const progress = container.querySelector('[data-slot="progress"]');
		expect(progress?.className).toContain('animate-pulse');
	});

	it('does not apply pulse animation for paused jobs', () => {
		const pausedSlot = { ...baseSlot, status: 'Paused' };
		const { container } = render(QueueRow, { slot: pausedSlot, onremove: () => {} });
		
		const progress = container.querySelector('[data-slot="progress"]');
		expect(progress?.className).not.toContain('animate-pulse');
	});

	it('does not apply pulse animation for queued jobs', () => {
		const queuedSlot = { ...baseSlot, status: 'Queued' };
		const { container } = render(QueueRow, { slot: queuedSlot, onremove: () => {} });
		
		const progress = container.querySelector('[data-slot="progress"]');
		expect(progress?.className).not.toContain('animate-pulse');
	});

	it('does not apply pulse animation for idle jobs', () => {
		const idleSlot = { ...baseSlot, status: 'Idle' };
		const { container } = render(QueueRow, { slot: idleSlot, onremove: () => {} });

		const progress = container.querySelector('[data-slot="progress"]');
		expect(progress?.className).not.toContain('animate-pulse');
	});

	it('renders job name', () => {
		const { container } = render(QueueRow, { slot: baseSlot, onremove: () => {} });
		expect(container.textContent).toContain('Test.NZB');
	});

	it('renders category', () => {
		const { container } = render(QueueRow, { slot: baseSlot, onremove: () => {} });
		expect(container.textContent).toContain('TV');
	});

	it('shows * for empty category', () => {
		const slot = { ...baseSlot, cat: '' };
		const { container } = render(QueueRow, { slot, onremove: () => {} });
		expect(container.textContent).toContain('*');
	});

	it('renders size', () => {
		const { container } = render(QueueRow, { slot: baseSlot, onremove: () => {} });
		expect(container.textContent).toContain('100 MB');
	});

	it('renders size left', () => {
		const { container } = render(QueueRow, { slot: baseSlot, onremove: () => {} });
		expect(container.textContent).toContain('50 MB');
	});

	it('delete button triggers onremove callback', async () => {
		const onremove = vi.fn();
		render(QueueRow, { slot: baseSlot, onremove });

		const deleteBtn = screen.getByTitle('Delete');
		await fireEvent.click(deleteBtn);

		expect(onremove).toHaveBeenCalled();
	});

	it('shows pause button title for active jobs', () => {
		render(QueueRow, { slot: baseSlot, onremove: () => {} });
		expect(screen.getByTitle('Pause')).toBeInTheDocument();
	});

	it('shows resume button title for paused jobs', () => {
		const pausedSlot = { ...baseSlot, status: 'Paused' };
		render(QueueRow, { slot: pausedSlot, onremove: () => {} });
		expect(screen.getByTitle('Resume')).toBeInTheDocument();
	});

	// ── togglePause ──

	it('clicking pause button calls pauseJob for active jobs', async () => {
		render(QueueRow, { slot: baseSlot, onremove: vi.fn() });

		const pauseBtn = screen.getByTitle('Pause');
		await fireEvent.click(pauseBtn);

		expect(pauseJob).toHaveBeenCalledWith('123');
		expect(resumeJob).not.toHaveBeenCalled();
	});

	it('clicking resume button calls resumeJob for paused jobs', async () => {
		const pausedSlot = { ...baseSlot, status: 'Paused' };
		render(QueueRow, { slot: pausedSlot, onremove: vi.fn() });

		const resumeBtn = screen.getByTitle('Resume');
		await fireEvent.click(resumeBtn);

		expect(resumeJob).toHaveBeenCalledWith('123');
		expect(pauseJob).not.toHaveBeenCalled();
	});

	// ── Warning display ──

	it('shows warning icon and text when slot.warning is set', () => {
		const warnSlot = { ...baseSlot, warning: 'Missing articles' };
		const { container } = render(QueueRow, { slot: warnSlot, onremove: vi.fn() });
		expect(container.textContent).toContain('Missing articles');
	});

	it('does not show warning icon when slot.warning is empty', () => {
		const { container } = render(QueueRow, { slot: baseSlot, onremove: vi.fn() });
		// No warning text or icon.
		expect(container.querySelector('.text-amber-600')).not.toBeInTheDocument();
	});

	// ── Fallback to filename ──

	it('uses filename when name is empty', () => {
		const slot = { ...baseSlot, name: '', filename: 'fallback.nzb' };
		const { container } = render(QueueRow, { slot, onremove: vi.fn() });
		expect(container.textContent).toContain('fallback.nzb');
	});

	// ── Drawer per-file table ──

	/**
	 * Regression test for the Svelte each_key_duplicate runtime error
	 * we hit in production: NZBs frequently contain multiple files with
	 * the same sanitized subject (e.g. main + sample with the same
	 * release name, or par2 blocks all sharing a single subject after
	 * sanitization). The drawer's keyed each block must use a stable
	 * identity that doesn't collide on duplicate names.
	 */
	it('drawer renders multiple files with duplicate names without each_key_duplicate', async () => {
		const dupName = 'Transfixed.25.04.26.Ariel.Demure.Getting.His.Attention.XXX.1080p.MP4-Narcos_[1a57ee682';
		vi.mocked(fetchQueueJobDetail).mockResolvedValue({
			status: true,
			queue: {
				slots: [
					{
						...baseSlot,
						files: [
							{ name: dupName, bytes: 1000, bytes_downloaded: 500, state: 'downloading' },
							{ name: dupName, bytes: 2000, bytes_downloaded: 0, state: 'queued' },
							{ name: dupName, bytes: 1500, bytes_downloaded: 1500, state: 'done' }
						]
					}
				]
			}
		} as any);

		// Capture any console.error from Svelte; if the keyed-each
		// machinery throws a duplicate-key error in dev mode it lands
		// here. The fix must keep this empty.
		const errSpy = vi.spyOn(console, 'error').mockImplementation(() => {});

		const { container } = render(QueueRow, { slot: baseSlot, onremove: () => {} });

		// Click the row to expand the drawer.
		const row = container.querySelector('tr[class*="cursor-pointer"]') as HTMLElement;
		expect(row).toBeTruthy();
		await fireEvent.click(row);

		// Wait for fetchQueueJobDetail to resolve and the drawer to
		// re-render. A microtask flush is enough since we mocked the
		// fetch as immediately-resolved.
		await vi.waitFor(() => {
			expect(fetchQueueJobDetail).toHaveBeenCalledWith('123');
		});
		await vi.waitFor(() => {
			// Three rows for three duplicate-named files; without the
			// fix Svelte either errors out or merges the rows.
			expect(screen.getAllByText(dupName)).toHaveLength(3);
		});

		// No each_key_duplicate (or any other Svelte error) was logged.
		const svelteErrors = errSpy.mock.calls.flat().filter(
			(arg) => typeof arg === 'string' && arg.includes('each_key_duplicate')
		);
		expect(svelteErrors).toEqual([]);
		errSpy.mockRestore();
	});
});
