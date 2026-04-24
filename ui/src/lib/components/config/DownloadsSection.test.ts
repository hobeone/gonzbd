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
			article_cache_size: '500M',
			max_art_tries: 3,
			pre_check: true
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
	});
});
