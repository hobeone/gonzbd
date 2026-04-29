import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi } from 'vitest';
import ServersSection from './ServersSection.svelte';

vi.mock('$lib/components/ui/separator', () => ({
	Separator: function MockSeparator() {}
}));

describe('ServersSection', () => {
	const defaultCallbacks = {
		onAddServer: vi.fn(),
		onEditServer: vi.fn(),
		onTestServer: vi.fn().mockResolvedValue({ ok: true, message: 'Connection successful!' }),
		onToggleServer: vi.fn()
	};

	const emptyConfig = { servers: [] };

	const populatedConfig = {
		servers: [
			{
				name: 'Eweka',
				host: 'news.eweka.nl',
				port: 563,
				ssl: true,
				ssl_verify: 2,
				username: 'testuser',
				password: 'secret',
				connections: 20,
				priority: 0,
				enable: true
			},
			{
				name: 'Backup',
				host: 'backup.example.com',
				port: 119,
				ssl: false,
				ssl_verify: 0,
				username: '',
				password: '',
				connections: 5,
				priority: 1,
				enable: false
			}
		]
	};

	it('renders the heading', () => {
		render(ServersSection, { configData: emptyConfig, ...defaultCallbacks });
		expect(screen.getByText('Usenet Servers')).toBeInTheDocument();
	});

	it('shows empty state when no servers', () => {
		render(ServersSection, { configData: emptyConfig, ...defaultCallbacks });
		expect(screen.getByText('No servers configured.')).toBeInTheDocument();
	});

	it('renders server names in table', () => {
		render(ServersSection, { configData: populatedConfig, ...defaultCallbacks });
		expect(screen.getByText('Eweka')).toBeInTheDocument();
		expect(screen.getByText('Backup')).toBeInTheDocument();
	});

	it('shows TLS badge for SSL servers', () => {
		render(ServersSection, { configData: populatedConfig, ...defaultCallbacks });
		expect(screen.getByText('TLS')).toBeInTheDocument();
	});

	it('shows Disabled badge for disabled servers', () => {
		render(ServersSection, { configData: populatedConfig, ...defaultCallbacks });
		expect(screen.getByText('Disabled')).toBeInTheDocument();
	});

	it('shows anonymous for servers without username', () => {
		render(ServersSection, { configData: populatedConfig, ...defaultCallbacks });
		expect(screen.getByText('anonymous')).toBeInTheDocument();
	});

	it('calls onAddServer when Add button clicked', async () => {
		const onAddServer = vi.fn();
		render(ServersSection, { configData: emptyConfig, ...defaultCallbacks, onAddServer });
		await fireEvent.click(screen.getByText('+ Add Server'));
		expect(onAddServer).toHaveBeenCalled();
	});

	it('calls onTestServer when Test button clicked', async () => {
		const onTestServer = vi.fn().mockResolvedValue({ ok: true, message: 'Connection successful!' });
		render(ServersSection, { configData: populatedConfig, ...defaultCallbacks, onTestServer });
		const testButtons = screen.getAllByTitle('Test connection');
		await fireEvent.click(testButtons[0]);
		expect(onTestServer).toHaveBeenCalledWith(populatedConfig.servers[0]);
	});

	it('shows inline success result after test', async () => {
		const onTestServer = vi.fn().mockResolvedValue({ ok: true, message: 'Connection successful!' });
		render(ServersSection, { configData: populatedConfig, ...defaultCallbacks, onTestServer });
		const testButtons = screen.getAllByTitle('Test connection');
		await fireEvent.click(testButtons[0]);
		// Wait for the promise to resolve and re-render
		await vi.waitFor(() => {
			expect(screen.getByText('Connection successful!')).toBeInTheDocument();
		});
	});

	it('shows inline failure result after test', async () => {
		const onTestServer = vi.fn().mockResolvedValue({ ok: false, message: 'Connection refused' });
		render(ServersSection, { configData: populatedConfig, ...defaultCallbacks, onTestServer });
		const testButtons = screen.getAllByTitle('Test connection');
		await fireEvent.click(testButtons[0]);
		await vi.waitFor(() => {
			expect(screen.getByText('Connection refused')).toBeInTheDocument();
		});
	});

	it('calls onToggleServer when checkbox is clicked', async () => {
		const onToggleServer = vi.fn();
		render(ServersSection, { configData: populatedConfig, ...defaultCallbacks, onToggleServer });
		const checkboxes = screen.getAllByRole('checkbox');
		// First checkbox is Eweka (enabled), clicking should toggle to false
		await fireEvent.click(checkboxes[0]);
		expect(onToggleServer).toHaveBeenCalledWith(populatedConfig.servers[0], false);
	});

	it('shows warning when all servers are disabled', () => {
		const allDisabledConfig = {
			servers: [
				{ ...populatedConfig.servers[0], enable: false },
				{ ...populatedConfig.servers[1], enable: false }
			]
		};
		render(ServersSection, { configData: allDisabledConfig, ...defaultCallbacks });
		expect(screen.getByText(/All servers are disabled/)).toBeInTheDocument();
	});

	it('does not show warning when at least one server is enabled', () => {
		render(ServersSection, { configData: populatedConfig, ...defaultCallbacks });
		expect(screen.queryByText(/All servers are disabled/)).not.toBeInTheDocument();
	});
});
