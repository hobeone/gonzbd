import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi } from 'vitest';
import RSSSection from './RSSSection.svelte';

vi.mock('$lib/components/ui/separator', () => ({
	Separator: function MockSeparator() {}
}));

describe('RSSSection', () => {
	const defaultCallbacks = {
		onAddFeed: vi.fn(),
		onEditFeed: vi.fn(),
		onDeleteFeed: vi.fn()
	};

	const emptyConfig = { rss: [] };
	const populatedConfig = {
		rss: [
			{ name: 'NZBGeek', uri: 'https://api.nzbgeek.info/rss?t=5030', enable: true },
			{ name: 'Old Feed', uri: 'https://old.example.com/rss', enable: false }
		]
	};

	it('renders the heading', () => {
		render(RSSSection, { configData: emptyConfig, ...defaultCallbacks });
		expect(screen.getByText('RSS Feeds')).toBeInTheDocument();
	});

	it('shows empty state when no feeds', () => {
		render(RSSSection, { configData: emptyConfig, ...defaultCallbacks });
		expect(screen.getByText('No feeds configured.')).toBeInTheDocument();
	});

	it('renders feed names and URIs', () => {
		render(RSSSection, { configData: populatedConfig, ...defaultCallbacks });
		expect(screen.getByText('NZBGeek')).toBeInTheDocument();
		expect(screen.getByText('https://api.nzbgeek.info/rss?t=5030')).toBeInTheDocument();
	});

	it('calls onAddFeed when Add button clicked', async () => {
		const onAddFeed = vi.fn();
		render(RSSSection, { configData: emptyConfig, ...defaultCallbacks, onAddFeed });
		await fireEvent.click(screen.getByText('+ Add Feed'));
		expect(onAddFeed).toHaveBeenCalled();
	});
});
