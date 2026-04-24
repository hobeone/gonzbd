import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi } from 'vitest';
import ConfigSwitch from './ConfigSwitch.svelte';

describe('ConfigSwitch', () => {
	it('renders label', () => {
		render(ConfigSwitch, {
			section: 'downloads',
			keyword: 'pre_check',
			value: false,
			label: 'Pre-check articles'
		});
		expect(screen.getByLabelText('Pre-check articles')).toBeInTheDocument();
	});

	it('renders description when provided', () => {
		render(ConfigSwitch, {
			section: 'downloads',
			keyword: 'pre_check',
			value: false,
			label: 'Pre-check',
			description: 'STAT check before download'
		});
		expect(screen.getByText('STAT check before download')).toBeInTheDocument();
	});

	it('checkbox reflects value prop', () => {
		render(ConfigSwitch, {
			section: 'downloads',
			keyword: 'pre_check',
			value: true,
			label: 'Pre-check'
		});
		const checkbox = screen.getByLabelText('Pre-check') as HTMLInputElement;
		expect(checkbox.checked).toBe(true);
	});

	it('calls onupdate with toggled value on click', async () => {
		const onupdate = vi.fn();
		render(ConfigSwitch, {
			section: 'downloads',
			keyword: 'pre_check',
			value: false,
			label: 'Pre-check',
			onupdate
		});

		const checkbox = screen.getByLabelText('Pre-check');
		await fireEvent.click(checkbox);

		expect(onupdate).toHaveBeenCalledWith('downloads', 'pre_check', true);
	});
});
