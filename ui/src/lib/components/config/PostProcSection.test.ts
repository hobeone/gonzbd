import { render, screen } from '@testing-library/svelte';
import { describe, it, expect, vi } from 'vitest';
import PostProcSection from './PostProcSection.svelte';

vi.mock('$lib/components/ui/separator', () => ({
	Separator: function MockSeparator() {}
}));

describe('PostProcSection', () => {
	const mockConfig = {
		postproc: {
			enable_unrar: true,
			enable_7zip: false,
			direct_unpack: true,
			enable_par_cleanup: true,
			enable_rar_cleanup: true,
			unrar_command: '/usr/bin/unrar',
			par2_command: '/usr/bin/par2',
			sevenz_command: '/usr/bin/7zz'
		}
	};

	it('renders the section heading', () => {
		render(PostProcSection, { configData: mockConfig, onFieldUpdate: vi.fn() });
		expect(screen.getByText('Post-Processing')).toBeInTheDocument();
	});

	it('renders all config fields', () => {
		render(PostProcSection, { configData: mockConfig, onFieldUpdate: vi.fn() });
		expect(screen.getByText('Enable RAR extraction')).toBeInTheDocument();
		expect(screen.getByText('Enable 7-Zip extraction')).toBeInTheDocument();
		expect(screen.getByText('Direct Unpack')).toBeInTheDocument();
		expect(screen.getByText('Cleanup par2 files')).toBeInTheDocument();
		expect(screen.getByText('Cleanup archive files')).toBeInTheDocument();
		expect(screen.getByText('UnRAR path')).toBeInTheDocument();
		expect(screen.getByText('par2 path')).toBeInTheDocument();
		expect(screen.getByText('7-Zip path')).toBeInTheDocument();
	});

	it('renders auto-detect help text', () => {
		render(PostProcSection, { configData: mockConfig, onFieldUpdate: vi.fn() });
		expect(screen.getByText(/Leave paths empty to auto-detect/)).toBeInTheDocument();
	});
});
