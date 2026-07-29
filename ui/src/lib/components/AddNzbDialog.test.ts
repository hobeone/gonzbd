import { render, screen, cleanup } from '@testing-library/svelte';
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import AddNzbDialog from './AddNzbDialog.svelte';

vi.mock('$lib/api', () => ({
	uploadNzb: vi.fn().mockResolvedValue({ status: true }),
	postAction: vi.fn().mockResolvedValue({ status: true }),
	fetchCategories: vi.fn().mockResolvedValue(['*', 'TV', 'Movies'])
}));

describe('AddNzbDialog', () => {
	beforeEach(() => {
		vi.clearAllMocks();
	});

	afterEach(() => {
		cleanup();
	});

	it('renders dialog title when open', () => {
		render(AddNzbDialog, { props: { open: true } });
		expect(screen.getByText('Add NZB')).toBeInTheDocument();
	});

	it('renders description text', () => {
		render(AddNzbDialog, { props: { open: true } });
		expect(screen.getByText('Upload an NZB file or paste a URL.')).toBeInTheDocument();
	});

	it('renders File and URL tabs', () => {
		render(AddNzbDialog, { props: { open: true } });
		expect(screen.getByText('File')).toBeInTheDocument();
		expect(screen.getByText('URL')).toBeInTheDocument();
	});

	it('renders category select with default *', () => {
		render(AddNzbDialog, { props: { open: true } });
		const select = screen.getByLabelText('Category') as HTMLSelectElement;
		expect(select).toBeInTheDocument();
		expect(select.value).toBe('*');
	});

	it('renders password field', () => {
		render(AddNzbDialog, { props: { open: true } });
		expect(screen.getByLabelText('Password')).toBeInTheDocument();
	});

	it('renders Upload button (disabled when no file)', () => {
		render(AddNzbDialog, { props: { open: true } });
		const btn = screen.getByText('Upload');
		expect(btn).toBeInTheDocument();
		expect(btn).toBeDisabled();
	});

	it('renders drag-and-drop area with hint text', () => {
		render(AddNzbDialog, { props: { open: true } });
		expect(screen.getByText('Drop NZB file here or click to browse')).toBeInTheDocument();
		expect(screen.getByText('.nzb or .nzb.gz files')).toBeInTheDocument();
	});

	it('renders priority select with default -100 (Default)', () => {
		render(AddNzbDialog, { props: { open: true } });
		const prioritySelect = screen.getByLabelText('Priority') as HTMLSelectElement;
		expect(prioritySelect).toBeInTheDocument();
		expect(prioritySelect.value).toBe('-100');
	});

	it('renders Add Paused checkbox with default unchecked', () => {
		render(AddNzbDialog, { props: { open: true } });
		const pausedCheckbox = screen.getByLabelText('Add Paused') as HTMLInputElement;
		expect(pausedCheckbox).toBeInTheDocument();
		expect(pausedCheckbox.checked).toBe(false);
	});

	it('synchronizes Priority select and Add Paused checkbox', async () => {
		const { fireEvent } = await import('@testing-library/svelte');
		render(AddNzbDialog, { props: { open: true } });

		const prioritySelect = screen.getByLabelText('Priority') as HTMLSelectElement;
		const pausedCheckbox = screen.getByLabelText('Add Paused') as HTMLInputElement;

		// Select Paused (-2) in Priority select
		await fireEvent.change(prioritySelect, { target: { value: '-2' } });
		expect(pausedCheckbox.checked).toBe(true);

		// Select High (1) in Priority select
		await fireEvent.change(prioritySelect, { target: { value: '1' } });
		expect(pausedCheckbox.checked).toBe(false);

		// Check Add Paused checkbox
		await fireEvent.click(pausedCheckbox);
		expect(prioritySelect.value).toBe('-2');

		// Uncheck Add Paused checkbox
		await fireEvent.click(pausedCheckbox);
		expect(prioritySelect.value).toBe('-100');
	});

	it('passes priority parameter on URL submit when priority or paused is selected', async () => {
		const { fireEvent } = await import('@testing-library/svelte');
		const { postAction } = await import('$lib/api');

		render(AddNzbDialog, { props: { open: true } });

		// Switch to URL tab
		const urlTab = screen.getByText('URL');
		await fireEvent.click(urlTab);

		const urlInput = screen.getByPlaceholderText('https://example.com/file.nzb');
		await fireEvent.input(urlInput, { target: { value: 'https://example.com/test.nzb' } });

		const pausedCheckbox = screen.getByLabelText('Add Paused');
		await fireEvent.click(pausedCheckbox);

		const fetchBtn = screen.getByText('Fetch');
		await fireEvent.click(fetchBtn);

		expect(postAction).toHaveBeenCalledWith('addurl', {
			name: 'https://example.com/test.nzb',
			priority: '-2'
		});
	});

	it('does not render dialog content when closed', () => {
		render(AddNzbDialog, { props: { open: false } });
		expect(screen.queryByText('Add NZB')).not.toBeInTheDocument();
	});
});
