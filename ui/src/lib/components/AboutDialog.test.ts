import { render, screen } from '@testing-library/svelte';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import AboutDialog from './AboutDialog.svelte';

vi.mock('#lib/api.js', () => ({
	fetchJSON: vi.fn()
}));

import { fetchJSON } from '#lib/api.js';

function about(over: Record<string, unknown> = {}) {
	return {
		about: {
			version: 'v1.2.3',
			commit: 'abc1234',
			commit_time: '2026-05-01T10:00:00Z',
			dirty: false,
			build_date: '2026-05-06T14:00:00Z',
			go_version: 'go1.27.1',
			local_ipv4: '',
			public_ipv4: '',
			public_ipv6: '',
			hostname: 'host',
			config_path: '/etc/gonzbd.yaml',
			download_dir: '/d',
			complete_dir: '/c',
			admin_dir: '/a',
			log_dir: '',
			dirscan_dir: '',
			script_dir: '',
			par2_path: '',
			unrar_path: '',
			sevenz_path: '',
			...over
		}
	};
}

const local = (iso: string) => new Date(iso).toLocaleString();

describe('AboutDialog build info', () => {
	beforeEach(() => {
		vi.clearAllMocks();
	});

	it('shows a linked commit, commit time and build time', async () => {
		vi.mocked(fetchJSON).mockResolvedValue(about());
		render(AboutDialog, { props: { open: true } });

		const link = await screen.findByRole('link', { name: 'abc1234' });
		expect(link).toHaveAttribute('href', 'https://github.com/hobeone/gonzbd/commit/abc1234');
		expect(screen.getByText('Committed')).toBeInTheDocument();
		expect(screen.getByText(local('2026-05-01T10:00:00Z'))).toBeInTheDocument();
		expect(screen.getByText('Built')).toBeInTheDocument();
		expect(screen.getByText(local('2026-05-06T14:00:00Z'))).toBeInTheDocument();
	});

	it('marks a dirty tree as modified', async () => {
		vi.mocked(fetchJSON).mockResolvedValue(about({ dirty: true }));
		render(AboutDialog, { props: { open: true } });

		expect(await screen.findByRole('link', { name: 'abc1234 (modified)' })).toBeInTheDocument();
	});

	it('hides each row only when its value is genuinely absent', async () => {
		vi.mocked(fetchJSON).mockResolvedValue(
			about({ commit: 'unknown', commit_time: '', build_date: 'unknown' })
		);
		render(AboutDialog, { props: { open: true } });

		expect(await screen.findByText('v1.2.3')).toBeInTheDocument();
		expect(screen.queryByText('Commit')).not.toBeInTheDocument();
		expect(screen.queryByText('Committed')).not.toBeInTheDocument();
		expect(screen.queryByText('Built')).not.toBeInTheDocument();
	});

	it('keeps the commit row when only the times are absent', async () => {
		vi.mocked(fetchJSON).mockResolvedValue(about({ commit_time: '', build_date: 'unknown' }));
		render(AboutDialog, { props: { open: true } });

		expect(await screen.findByRole('link', { name: 'abc1234' })).toBeInTheDocument();
		expect(screen.queryByText('Committed')).not.toBeInTheDocument();
		expect(screen.queryByText('Built')).not.toBeInTheDocument();
	});
});
