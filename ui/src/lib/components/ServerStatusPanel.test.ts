import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import ServerStatusPanel from './ServerStatusPanel.svelte';
import { getServerStats } from '$lib/stores/queue.svelte';
import type { ConnSnapshot, ServerSnapshot } from '$lib/types';

vi.mock('$lib/stores/queue.svelte', () => ({
	getServerStats: vi.fn()
}));

vi.mock('$lib/api', () => ({
	fetchConfig: vi.fn().mockResolvedValue({ config: { servers: [] } }),
	setConfig: vi.fn(),
	postAction: vi.fn()
}));

function conn(over: Partial<ConnSnapshot> = {}): ConnSnapshot {
	return {
		index: 0,
		article_id: '',
		subject: '',
		bytes: 0,
		since_unix: 0,
		in_flight: 0,
		connected: true,
		...over
	};
}

function server(conns: ConnSnapshot[], over: Partial<ServerSnapshot> = {}): ServerSnapshot {
	return {
		name: 'news.example.com',
		host: 'news.example.com',
		port: 563,
		ssl: true,
		priority: 0,
		pipelining: 4,
		max_connections: conns.length,
		// Busy CONNECTIONS, matching what the backend reports.
		active_conns: conns.filter((c) => c.article_id !== '').length,
		active: true,
		enabled: true,
		optional: false,
		required: false,
		penalty_until: 0,
		bps: 1024,
		total_bytes: 4096,
		connections: conns,
		...over
	};
}

describe('ServerStatusPanel capacity totals', () => {
	beforeEach(() => {
		vi.clearAllMocks();
	});

	it('leaves a disabled server out of the connection total', () => {
		vi.mocked(getServerStats).mockReturnValue([
			server([conn({ index: 0, article_id: 'a@h', subject: 'f.rar', in_flight: 1 }), conn({ index: 1 })]),
			// Disabled: 60 configured connections the daemon will never open.
			// It gets no connActivity entries at all, hence the empty list.
			// Server.Active() returns false whenever Enable is false, so a
			// realistic disabled snapshot carries active: false too.
			server([], {
				name: 'disabled.example.com',
				enabled: false,
				active: false,
				max_connections: 60,
				active_conns: 0
			})
		]);

		render(ServerStatusPanel, { open: true });

		expect(screen.getByText('1 / 2')).toBeTruthy();
		expect(screen.queryByText('1 / 62')).toBeNull();
	});

	it('shows a disabled server no connection capacity in its own row', () => {
		vi.mocked(getServerStats).mockReturnValue([
			server([], {
				name: 'disabled.example.com',
				enabled: false,
				active: false,
				max_connections: 60,
				active_conns: 0
			})
		]);

		render(ServerStatusPanel, { open: true });

		// "0/60 conns" would advertise 60 slots that were never allocated.
		expect(screen.queryByText('0/60 conns')).toBeNull();
		expect(screen.getByText('Disabled')).toBeTruthy();
	});

	it('counts an enabled server that is merely penalized', () => {
		vi.mocked(getServerStats).mockReturnValue([
			server([conn({ index: 0, article_id: 'a@h', subject: 'f.rar', in_flight: 1 })]),
			// Penalized, not disabled: its workers exist and it will come
			// back, so its capacity is still real.
			server([conn({ index: 0 })], {
				name: 'penalized.example.com',
				active: false,
				penalty_until: Math.floor(Date.now() / 1000) + 300,
				active_conns: 0
			})
		]);

		render(ServerStatusPanel, { open: true });

		expect(screen.getByText('1 / 2')).toBeTruthy();
	});
});

describe('ServerStatusPanel pipelining display', () => {
	beforeEach(() => {
		vi.clearAllMocks();
	});

	it('separates busy connections from articles in flight', () => {
		vi.mocked(getServerStats).mockReturnValue([
			server([
				conn({ index: 0, article_id: 'a@h', subject: 'file.rar', bytes: 100, in_flight: 4 }),
				conn({ index: 1 })
			])
		]);

		render(ServerStatusPanel, { open: true });

		// One of two connections is busy; it carries four articles.
		expect(screen.getByText('1 / 2')).toBeTruthy();
		expect(screen.getByText('4 articles in flight')).toBeTruthy();
	});

	it('omits the articles line when no connection is pipelining', () => {
		vi.mocked(getServerStats).mockReturnValue([
			server([conn({ index: 0, article_id: 'a@h', subject: 'file.rar', bytes: 100, in_flight: 1 })])
		]);

		render(ServerStatusPanel, { open: true });

		expect(screen.queryByText(/articles in flight/)).toBeNull();
	});

	it('badges a pipelined connection with its depth', async () => {
		vi.mocked(getServerStats).mockReturnValue([
			server([
				conn({ index: 0, article_id: 'a@h', subject: 'oldest.rar', bytes: 100, in_flight: 4 })
			])
		]);

		render(ServerStatusPanel, { open: true });
		// The server header is the only disclosure button in the panel.
		await fireEvent.click(screen.getByRole('button', { expanded: false }));

		// The row names the oldest article and says how many share the socket.
		expect(screen.getByText('oldest.rar')).toBeTruthy();
		expect(screen.getByText('×4')).toBeTruthy();
	});

	it('does not badge a connection carrying a single article', async () => {
		vi.mocked(getServerStats).mockReturnValue([
			server([conn({ index: 0, article_id: 'a@h', subject: 'solo.rar', bytes: 100, in_flight: 1 })])
		]);

		render(ServerStatusPanel, { open: true });
		// The server header is the only disclosure button in the panel.
		await fireEvent.click(screen.getByRole('button', { expanded: false }));

		expect(screen.getByText('solo.rar')).toBeTruthy();
		expect(screen.queryByText('×1')).toBeNull();
	});
});
