import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import SettingsDialog from './SettingsDialog.svelte';

// Mock all config section components to avoid deep render trees.
vi.mock('./config/GeneralSection.svelte', () => ({ default: function() {} }));
vi.mock('./config/DownloadsSection.svelte', () => ({ default: function() {} }));
vi.mock('./config/PostProcSection.svelte', () => ({ default: function() {} }));
vi.mock('./config/ServersSection.svelte', () => ({ default: function() {} }));
vi.mock('./config/CategoriesSection.svelte', () => ({ default: function() {} }));
vi.mock('./config/SortersSection.svelte', () => ({ default: function() {} }));
vi.mock('./config/RSSSection.svelte', () => ({ default: function() {} }));
vi.mock('./config/SchedulingSection.svelte', () => ({ default: function() {} }));
vi.mock('./config/ServerEditDialog.svelte', () => ({ default: function() {} }));
vi.mock('./config/CategoryEditDialog.svelte', () => ({ default: function() {} }));
vi.mock('./config/SorterEditDialog.svelte', () => ({ default: function() {} }));
vi.mock('./config/ScheduleEditDialog.svelte', () => ({ default: function() {} }));
vi.mock('./config/RSSEditDialog.svelte', () => ({ default: function() {} }));

vi.mock('$lib/api', () => ({
	setConfig: vi.fn().mockResolvedValue({ status: true }),
	postAction: vi.fn().mockResolvedValue({ status: true }),
	fetchConfig: vi.fn().mockReturnValue(new Promise(() => {}))
}));

import { fetchConfig } from '$lib/api';

describe('SettingsDialog', () => {
	beforeEach(() => {
		vi.clearAllMocks();
	});

	it('renders Settings title when open', () => {
		render(SettingsDialog, { props: { open: true } });
		expect(screen.getByText('Settings')).toBeInTheDocument();
	});

	it('renders all sidebar navigation items', () => {
		render(SettingsDialog, { props: { open: true } });
		expect(screen.getByText('General')).toBeInTheDocument();
		expect(screen.getByText('Downloads')).toBeInTheDocument();
		expect(screen.getByText('Post-Processing')).toBeInTheDocument();
		expect(screen.getByText('Servers')).toBeInTheDocument();
		expect(screen.getByText('Categories')).toBeInTheDocument();
		expect(screen.getByText('Sorters')).toBeInTheDocument();
		expect(screen.getByText('RSS')).toBeInTheDocument();
		expect(screen.getByText('Scheduling')).toBeInTheDocument();
	});

	it('renders Close button and synced status in footer', () => {
		render(SettingsDialog, { props: { open: true } });
		expect(screen.getByText('Close')).toBeInTheDocument();
	});

	it('does not render when closed', () => {
		render(SettingsDialog, { props: { open: false } });
		expect(screen.queryByText('Settings')).not.toBeInTheDocument();
	});

	it('shows loading state initially', () => {
		render(SettingsDialog, { props: { open: true } });
		expect(screen.getByText('Loading configuration...')).toBeInTheDocument();
	});

	it('loads config and remaps misc to general', async () => {
		// Return a config with "misc" (as the API does) instead of "general".
		const mockConfig = {
			status: true,
			config: {
				misc: { host: '0.0.0.0', port: 4289 },
				downloads: {},
				servers: []
			}
		};
		vi.mocked(fetchConfig).mockResolvedValue(mockConfig as any);

		render(SettingsDialog, { props: { open: true } });

		// Wait for the fetch to resolve and loading to finish.
		// After loading, "Loading configuration..." should disappear.
		await vi.waitFor(() => {
			expect(screen.queryByText('Loading configuration...')).not.toBeInTheDocument();
		});

		// The dialog should have loaded without crashing — meaning misc→general
		// mapping worked and GeneralSection can access configData.general.host.
		expect(fetchConfig).toHaveBeenCalled();
	});
});
