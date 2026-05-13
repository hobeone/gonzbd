import { render, screen } from '@testing-library/svelte';
import { describe, it, expect, vi } from 'vitest';
import DownloadsSection from './DownloadsSection.svelte';

vi.mock('$lib/components/ui/separator', () => ({
	Separator: function MockSeparator() {}
}));

describe('DownloadsSection', () => {
	const mockConfig = {
		downloads: {
			bandwidth_max: '10M',
			min_free_space: '1G',
			write_cache_size: '500M',
			max_art_tries: 3,
			pre_check: true,
			replace_illegal_with: '_',
			replace_spaces_with: '.',
			strip_diacritics: true,
			cleanup_list: ['^abc', 'xyz$']
		}
	};

	it('renders the section heading', () => {
		render(DownloadsSection, { configData: mockConfig, onFieldUpdate: vi.fn() });
		expect(screen.getByText('Download Settings')).toBeInTheDocument();
	});

	it('renders all config fields', () => {
		render(DownloadsSection, { configData: mockConfig, onFieldUpdate: vi.fn() });
		expect(screen.getByText('Maximum Bandwidth')).toBeInTheDocument();
		expect(screen.getByText('Minimum Free Space')).toBeInTheDocument();
		expect(screen.getByText('Article Cache')).toBeInTheDocument();
		expect(screen.getByText('Article Retries')).toBeInTheDocument();
		expect(screen.getByText('Pre-check article availability')).toBeInTheDocument();
		expect(screen.getByText('Naming & Cleanup')).toBeInTheDocument();
		expect(screen.getByText('Replace Illegal Characters With')).toBeInTheDocument();
		expect(screen.getByText('Replace Spaces With')).toBeInTheDocument();
		expect(screen.getByText('Strip Diacritics')).toBeInTheDocument();
		expect(screen.getByText('Indexer/Spam Cleanup List')).toBeInTheDocument();
	});
});
