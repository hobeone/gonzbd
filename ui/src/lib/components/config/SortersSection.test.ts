import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi } from 'vitest';
import SortersSection from './SortersSection.svelte';

vi.mock('$lib/components/ui/separator', () => ({
	Separator: function MockSeparator() {}
}));

describe('SortersSection', () => {
	const defaultCallbacks = {
		onAddSorter: vi.fn(),
		onEditSorter: vi.fn(),
		onDeleteSorter: vi.fn()
	};

	const emptyConfig = { sorters: [] };
	const populatedConfig = {
		sorters: [
			{ name: 'TV Shows', sort_string: '%sn/Season %s/%sn - S%0sE%0e - %en.%ext', is_active: true },
			{ name: 'Movies (disabled)', sort_string: '%title (%y).%ext', is_active: false }
		]
	};

	it('renders the heading', () => {
		render(SortersSection, { configData: emptyConfig, ...defaultCallbacks });
		expect(screen.getByText('Sorters')).toBeInTheDocument();
	});

	it('shows empty state when no sorters', () => {
		render(SortersSection, { configData: emptyConfig, ...defaultCallbacks });
		expect(screen.getByText('No sorters configured.')).toBeInTheDocument();
	});

	it('renders sorter names and templates', () => {
		render(SortersSection, { configData: populatedConfig, ...defaultCallbacks });
		expect(screen.getByText('TV Shows')).toBeInTheDocument();
		expect(screen.getByText('%sn/Season %s/%sn - S%0sE%0e - %en.%ext')).toBeInTheDocument();
	});

	it('calls onAddSorter when Add button clicked', async () => {
		const onAddSorter = vi.fn();
		render(SortersSection, { configData: emptyConfig, ...defaultCallbacks, onAddSorter });
		await fireEvent.click(screen.getByText('+ Add Sorter'));
		expect(onAddSorter).toHaveBeenCalled();
	});
});
