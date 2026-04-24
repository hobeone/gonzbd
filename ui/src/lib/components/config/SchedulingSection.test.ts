import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi } from 'vitest';
import SchedulingSection from './SchedulingSection.svelte';

vi.mock('$lib/components/ui/separator', () => ({
	Separator: function MockSeparator() {}
}));

describe('SchedulingSection', () => {
	const defaultCallbacks = {
		onAddSchedule: vi.fn(),
		onEditSchedule: vi.fn(),
		onDeleteSchedule: vi.fn()
	};

	const emptyConfig = { schedules: [] };
	const populatedConfig = {
		schedules: [
			{ name: 'Night Mode', hour: '22', minute: '00', dayofweek: 'Daily', action: 'speedlimit', enabled: true },
			{ name: 'Weekend Off', hour: '00', minute: '00', dayofweek: 'Sat', action: 'pause', enabled: false }
		]
	};

	it('renders the heading', () => {
		render(SchedulingSection, { configData: emptyConfig, ...defaultCallbacks });
		expect(screen.getByText('Schedules')).toBeInTheDocument();
	});

	it('shows empty state when no schedules', () => {
		render(SchedulingSection, { configData: emptyConfig, ...defaultCallbacks });
		expect(screen.getByText('No schedules configured.')).toBeInTheDocument();
	});

	it('renders schedule details', () => {
		render(SchedulingSection, { configData: populatedConfig, ...defaultCallbacks });
		expect(screen.getByText('Night Mode')).toBeInTheDocument();
		expect(screen.getByText('speedlimit')).toBeInTheDocument();
		expect(screen.getByText(/22:00/)).toBeInTheDocument();
	});

	it('calls onAddSchedule when Add button clicked', async () => {
		const onAddSchedule = vi.fn();
		render(SchedulingSection, { configData: emptyConfig, ...defaultCallbacks, onAddSchedule });
		await fireEvent.click(screen.getByText('+ Add Schedule'));
		expect(onAddSchedule).toHaveBeenCalled();
	});
});
