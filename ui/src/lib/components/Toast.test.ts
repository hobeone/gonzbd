import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import Toast from './Toast.svelte';
import { getToastMessage, dismissToast } from '$lib/stores/warnings.svelte';

vi.mock('$lib/stores/warnings.svelte', () => ({
	getToastMessage: vi.fn(),
	dismissToast: vi.fn()
}));

describe('Toast', () => {
	beforeEach(() => {
		vi.clearAllMocks();
	});

	it('renders toast message when present', () => {
		vi.mocked(getToastMessage).mockReturnValue('Disk space low');
		render(Toast);

		expect(screen.getByText('Disk space low')).toBeInTheDocument();
	});

	it('is hidden when no toast message', () => {
		vi.mocked(getToastMessage).mockReturnValue(null);
		const { container } = render(Toast);

		expect(container.querySelector('.fixed')).toBeNull();
	});

	it('dismiss button calls dismissToast', async () => {
		vi.mocked(getToastMessage).mockReturnValue('Warning message');
		render(Toast);

		const dismissBtn = screen.getByLabelText('Dismiss');
		await fireEvent.click(dismissBtn);

		expect(dismissToast).toHaveBeenCalled();
	});
});
