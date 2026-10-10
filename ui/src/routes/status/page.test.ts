import { render, screen } from '@testing-library/svelte';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import Page from './+page.svelte';

vi.mock('#lib/components/Navbar.svelte', () => ({ default: function () {} }));
vi.mock('#lib/stores/queue.svelte.js', () => ({ getServerStats: vi.fn().mockReturnValue([]), refreshQueue: vi.fn() }));
vi.mock('#lib/stores/telemetry.svelte.js', () => ({
	startTelemetry: vi.fn(),
	stopTelemetry: vi.fn()
}));
vi.mock('#lib/api.js', () => ({
	fetchStatusOverview: vi.fn(),
	fetchCheckUpdate: vi.fn(),
	fetchBuildInfo: vi.fn(),
	fetchRedactedConfig: vi.fn(),
	testServerConnection: vi.fn(),
	testDiskSpeed: vi.fn()
}));

import {
	fetchStatusOverview,
	fetchCheckUpdate,
	fetchBuildInfo,
	fetchRedactedConfig
} from '#lib/api.js';

const COMMIT_TIME = '2026-05-01T10:00:00Z';
const BUILD_DATE = '2026-05-06T14:00:00Z';
const local = (iso: string) => new Date(iso).toLocaleString();

function meta(over: Record<string, unknown> = {}) {
	return {
		version: 'v1.2.3',
		commit: 'abc1234',
		commit_time: COMMIT_TIME,
		dirty: false,
		build_date: BUILD_DATE,
		...over
	};
}

function mockApi(over: Record<string, unknown> = {}) {
	vi.mocked(fetchStatusOverview).mockResolvedValue({
		status: true,
		general: {
			...meta(over),
			go_version: 'go1.27.1',
			uptime_seconds: 60,
			hostname: 'host',
			local_ip: '10.0.0.2',
			config_path: '/etc/gonzbd.yaml',
			par2: { path: '/usr/bin/par2', version: '1' },
			unrar: { path: '', version: '' },
			sevenzip: { path: '', version: '' }
		},
		system: { os: 'linux', arch: 'amd64', download_dir_free_bytes: 0, min_free_space_bytes: 0 }
	} as never);
	vi.mocked(fetchCheckUpdate).mockResolvedValue({ result: { status: 'unknown' } } as never);
	vi.mocked(fetchRedactedConfig).mockResolvedValue({ config: { servers: [] } } as never);
	vi.mocked(fetchBuildInfo).mockResolvedValue({
		status: true,
		...meta(over),
		go_version: 'go1.27.1',
		deps: []
	});
}

describe('status page build info', () => {
	beforeEach(() => {
		vi.clearAllMocks();
	});

	it('shows commit, commit date and build date', async () => {
		mockApi();
		render(Page);

		// The same label appears in General Info and Build Info.
		expect(await screen.findAllByText('v1.2.3 · abc1234')).toHaveLength(2);
		expect(screen.getByText('Commit date')).toBeInTheDocument();
		expect(screen.getByText(local(COMMIT_TIME))).toBeInTheDocument();
		expect(screen.getByText('Build date')).toBeInTheDocument();
		expect(screen.getByText(local(BUILD_DATE))).toBeInTheDocument();
	});

	it('marks a modified tree in both cards', async () => {
		mockApi({ dirty: true });
		render(Page);

		expect(await screen.findAllByText('v1.2.3 · abc1234 (modified)')).toHaveLength(2);
	});

	it('hides the date rows when the build recorded none', async () => {
		mockApi({ commit_time: '', build_date: '' });
		render(Page);

		expect(await screen.findAllByText('v1.2.3 · abc1234')).toHaveLength(2);
		expect(screen.queryByText('Commit date')).not.toBeInTheDocument();
		expect(screen.queryByText('Build date')).not.toBeInTheDocument();
	});
});
