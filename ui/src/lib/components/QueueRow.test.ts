import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import QueueRow from './QueueRow.svelte';
import type { QueueSlot } from '$lib/types';

vi.mock('$lib/stores/queue.svelte', () => ({
	pauseJob: vi.fn().mockResolvedValue(undefined),
	resumeJob: vi.fn().mockResolvedValue(undefined)
}));

import { pauseJob, resumeJob } from '$lib/stores/queue.svelte';

describe('QueueRow', () => {
	const baseSlot: QueueSlot = {
		nzo_id: '123',
		name: 'Test.NZB',
		filename: 'Test.NZB',
		category: 'TV',
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
		par2_files: 0
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
		const slot = { ...baseSlot, category: '' };
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
});
