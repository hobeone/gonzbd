import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi } from 'vitest';
import ConfigSwitch from './ConfigSwitch.svelte';

describe('ConfigSwitch', () => {
	it('renders label', () => {
		render(ConfigSwitch, {
			section: 'downloads',
			keyword: 'top_only',
			value: false,
			label: 'Top-only server mode'
		});
		expect(screen.getByLabelText('Top-only server mode')).toBeInTheDocument();
	});

	it('renders description when provided', () => {
		render(ConfigSwitch, {
			section: 'downloads',
			keyword: 'top_only',
			value: false,
			label: 'Top-only',
			description: 'Only use the highest-priority server group'
		});
		expect(screen.getByText('Only use the highest-priority server group')).toBeInTheDocument();
	});

	it('checkbox reflects value prop', () => {
		render(ConfigSwitch, {
			section: 'downloads',
			keyword: 'top_only',
			value: true,
			label: 'Top-only'
		});
		const checkbox = screen.getByLabelText('Top-only') as HTMLInputElement;
		expect(checkbox.checked).toBe(true);
	});

	it('calls onupdate with toggled value on click', async () => {
		const onupdate = vi.fn();
		render(ConfigSwitch, {
			section: 'downloads',
			keyword: 'top_only',
			value: false,
			label: 'Top-only',
			onupdate
		});

		const checkbox = screen.getByLabelText('Top-only');
		await fireEvent.click(checkbox);

		expect(onupdate).toHaveBeenCalledWith('downloads', 'top_only', true);
	});
});
