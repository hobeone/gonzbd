import { render, screen, waitFor } from '@testing-library/svelte';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import BuildFooter from './BuildFooter.svelte';

vi.mock('#lib/api.js', () => ({
	fetchBuildInfo: vi.fn()
}));

import { fetchBuildInfo } from '#lib/api.js';
import type { BuildInfoResponse } from '#lib/api.js';

function info(over: Partial<BuildInfoResponse> = {}): BuildInfoResponse {
	return {
		status: true,
		version: 'v1.2.3',
		commit: 'abc1234',
		commit_time: '2026-05-01T10:00:00Z',
		dirty: false,
		build_date: '2026-05-06T14:00:00Z',
		go_version: 'go1.27.1',
		deps: [],
		...over
	};
}

describe('BuildFooter', () => {
	beforeEach(() => {
		vi.clearAllMocks();
	});

	it('renders version and short commit', async () => {
		vi.mocked(fetchBuildInfo).mockResolvedValue(info());
		render(BuildFooter);

		expect(await screen.findByText('v1.2.3 · abc1234')).toBeInTheDocument();
	});

	it('flags a modified tree', async () => {
		vi.mocked(fetchBuildInfo).mockResolvedValue(info({ dirty: true }));
		render(BuildFooter);

		expect(await screen.findByText('v1.2.3 · abc1234 (modified)')).toBeInTheDocument();
	});

	it('shows only the version when the commit is absent', async () => {
		vi.mocked(fetchBuildInfo).mockResolvedValue(
			info({ commit: '', commit_time: '', build_date: '' })
		);
		render(BuildFooter);

		const el = await screen.findByText('v1.2.3');
		expect(el).not.toHaveAttribute('title');
	});

	it('puts commit and build times in the tooltip', async () => {
		vi.mocked(fetchBuildInfo).mockResolvedValue(info());
		render(BuildFooter);

		const el = await screen.findByText('v1.2.3 · abc1234');
		expect(el.getAttribute('title')).toBe(
			`Committed ${new Date('2026-05-01T10:00:00Z').toLocaleString()}\nBuilt ${new Date('2026-05-06T14:00:00Z').toLocaleString()}`
		);
	});

	it('renders nothing when the fetch fails', async () => {
		vi.mocked(fetchBuildInfo).mockRejectedValue(new Error('boom'));
		render(BuildFooter);

		await waitFor(() => expect(fetchBuildInfo).toHaveBeenCalled());
		expect(screen.queryByTestId('build-footer')).not.toBeInTheDocument();
	});
});
