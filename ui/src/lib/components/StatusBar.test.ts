import { render, screen } from '@testing-library/svelte';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import StatusBar from './StatusBar.svelte';
import {
	getSpeedBytesPerSec,
	getSpeedHistory,
	getTotalRemainingBytes,
	formatSpeed,
	formatSize,
	getQueueSlots,
	isPaused
} from '$lib/stores/queue.svelte';

vi.mock('$lib/stores/queue.svelte', () => ({
	getSpeedBytesPerSec: vi.fn(),
	getSpeedHistory: vi.fn(),
	getTotalRemainingBytes: vi.fn(),
	formatSpeed: vi.fn(),
	formatSize: vi.fn(),
	getQueueSlots: vi.fn(),
	isPaused: vi.fn()
}));

// Mock SpeedGraph to avoid canvas issues in jsdom
vi.mock('./SpeedGraph.svelte', () => ({
	default: function SpeedGraphMock() {}
}));

describe('StatusBar', () => {
	beforeEach(() => {
		vi.clearAllMocks();
		vi.mocked(getSpeedBytesPerSec).mockReturnValue(1048576);
		vi.mocked(getSpeedHistory).mockReturnValue([]);
		vi.mocked(getTotalRemainingBytes).mockReturnValue(104857600);
		vi.mocked(formatSpeed).mockReturnValue('1.0 MB/s');
		vi.mocked(formatSize).mockReturnValue('100.0 MB');
		vi.mocked(getQueueSlots).mockReturnValue([{ nzo_id: '1' }, { nzo_id: '2' }, { nzo_id: '3' }] as any);
		vi.mocked(isPaused).mockReturnValue(false);
	});

	it('displays formatted speed', () => {
		render(StatusBar);
		expect(screen.getByText('1.0 MB/s')).toBeInTheDocument();
	});

	it('displays item count with correct pluralization', () => {
		render(StatusBar);
		expect(screen.getByText('3 items')).toBeInTheDocument();
	});

	it('displays singular for 1 item', () => {
		vi.mocked(getQueueSlots).mockReturnValue([{ nzo_id: '1' }] as any);
		render(StatusBar);
		expect(screen.getByText('1 item')).toBeInTheDocument();
	});

	it('displays remaining size', () => {
		render(StatusBar);
		expect(screen.getByText('100.0 MB left')).toBeInTheDocument();
	});

	it('shows -- for ETA when speed is 0', () => {
		vi.mocked(getSpeedBytesPerSec).mockReturnValue(0);
		render(StatusBar);
		expect(screen.getByText('ETA: --')).toBeInTheDocument();
	});

	it('shows PAUSED label when paused', () => {
		vi.mocked(isPaused).mockReturnValue(true);
		render(StatusBar);
		expect(screen.getByText('PAUSED')).toBeInTheDocument();
	});

	it('does not show PAUSED when not paused', () => {
		vi.mocked(isPaused).mockReturnValue(false);
		render(StatusBar);
		expect(screen.queryByText('PAUSED')).not.toBeInTheDocument();
	});
});
