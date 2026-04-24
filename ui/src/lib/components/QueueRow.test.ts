import { render, screen } from '@testing-library/svelte';
import { describe, it, expect } from 'vitest';
import QueueRow from './QueueRow.svelte';
import type { QueueSlot } from '$lib/types';

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
		password: ''
	};

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
});
