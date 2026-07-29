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

	it('does not render dialog content when closed', () => {
		render(AddNzbDialog, { props: { open: false } });
		expect(screen.queryByText('Add NZB')).not.toBeInTheDocument();
	});
});
