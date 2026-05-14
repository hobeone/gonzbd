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
			prefer_7zip: false,
			enable_filejoin: true,
			enable_tsjoin: true,
			enable_recursive: true,
			direct_unpack: true,
			flat_unpack: false,
			overwrite_files: false,
			ignore_unrar_dates: false,
			par2_turbo: false,
			process_unpacked_par2: true,
			enable_par_cleanup: true,
			enable_rar_cleanup: true,
			deobfuscate_filenames: true,
			ignore_samples: false,
			folder_rename: false,
			cleanup_extensions: ['nfo', 'txt'],
			permissions: '',
			par2_command: '/usr/bin/par2',
			unrar_command: '/usr/bin/unrar',
			sevenz_command: '/usr/bin/7zz',
			password_file: '',
			script_can_fail: false,
			nice: '',
			ionice: '',
			extra_unrar_params: '',
			extra_par2_params: ''
		}
	};

	it('renders the section heading', () => {
		render(PostProcSection, { configData: mockConfig, onFieldUpdate: vi.fn() });
		expect(screen.getByText('Post-Processing')).toBeInTheDocument();
	});

	it('renders extraction group labels', () => {
		render(PostProcSection, { configData: mockConfig, onFieldUpdate: vi.fn() });
		expect(screen.getByText('Enable RAR extraction')).toBeInTheDocument();
		expect(screen.getByText('Enable 7-Zip extraction')).toBeInTheDocument();
		expect(screen.getByText('Prefer 7-Zip for RAR')).toBeInTheDocument();
		expect(screen.getByText('Enable file joining')).toBeInTheDocument();
		expect(screen.getByText('Enable TS joining')).toBeInTheDocument();
		expect(screen.getByText('Recursive unpacking')).toBeInTheDocument();
		expect(screen.getByText('Direct Unpack')).toBeInTheDocument();
		expect(screen.getByText('Flat unpack')).toBeInTheDocument();
		expect(screen.getByText('Overwrite existing files')).toBeInTheDocument();
		expect(screen.getByText('Ignore archive dates')).toBeInTheDocument();
	});

	it('renders par2 repair group labels', () => {
		render(PostProcSection, { configData: mockConfig, onFieldUpdate: vi.fn() });
		expect(screen.getByText('Use par2cmdline-turbo')).toBeInTheDocument();
		expect(screen.getByText('Process unpacked par2')).toBeInTheDocument();
	});

	it('renders cleanup and output group labels', () => {
		render(PostProcSection, { configData: mockConfig, onFieldUpdate: vi.fn() });
		expect(screen.getByText('Cleanup par2 files')).toBeInTheDocument();
		expect(screen.getByText('Cleanup archive files')).toBeInTheDocument();
		expect(screen.getByText('Deobfuscate filenames')).toBeInTheDocument();
		expect(screen.getByText('Delete sample files')).toBeInTheDocument();
		expect(screen.getByText('Folder rename (_UNPACK_ prefix)')).toBeInTheDocument();
		expect(screen.getByText('Cleanup extensions')).toBeInTheDocument();
		expect(screen.getByText('File permissions')).toBeInTheDocument();
	});

	it('renders external tools group labels', () => {
		render(PostProcSection, { configData: mockConfig, onFieldUpdate: vi.fn() });
		expect(screen.getByText('par2 path')).toBeInTheDocument();
		expect(screen.getByText('UnRAR path')).toBeInTheDocument();
		expect(screen.getByText('7-Zip path')).toBeInTheDocument();
		expect(screen.getByText('Password file')).toBeInTheDocument();
		expect(screen.getByText(/Leave paths empty to auto-detect/)).toBeInTheDocument();
	});

	it('renders advanced group labels', () => {
		render(PostProcSection, { configData: mockConfig, onFieldUpdate: vi.fn() });
		expect(screen.getByText('Script can fail')).toBeInTheDocument();
		expect(screen.getByText('nice arguments')).toBeInTheDocument();
		expect(screen.getByText('ionice arguments')).toBeInTheDocument();
		expect(screen.getByText('Extra UnRAR parameters')).toBeInTheDocument();
		expect(screen.getByText('Extra par2 parameters')).toBeInTheDocument();
	});

	it('renders group section headers', () => {
		render(PostProcSection, { configData: mockConfig, onFieldUpdate: vi.fn() });
		expect(screen.getByText('Extraction')).toBeInTheDocument();
		expect(screen.getByText('Par2 Repair')).toBeInTheDocument();
		expect(screen.getByText('Cleanup & Output')).toBeInTheDocument();
		expect(screen.getByText('External Tools')).toBeInTheDocument();
		expect(screen.getByText('Advanced')).toBeInTheDocument();
	});
});
