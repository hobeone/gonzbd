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
		onDeleteServer: vi.fn(),
		onTestServer: vi.fn()
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

	it('calls onDeleteServer when Delete button clicked', async () => {
		const onDeleteServer = vi.fn();
		render(ServersSection, { configData: populatedConfig, ...defaultCallbacks, onDeleteServer });
		const deleteButtons = screen.getAllByTitle('Delete server');
		await fireEvent.click(deleteButtons[0]);
		expect(onDeleteServer).toHaveBeenCalledWith('Eweka');
	});

	it('calls onTestServer when Test button clicked', async () => {
		const onTestServer = vi.fn();
		render(ServersSection, { configData: populatedConfig, ...defaultCallbacks, onTestServer });
		const testButtons = screen.getAllByTitle('Test connection');
		await fireEvent.click(testButtons[0]);
		expect(onTestServer).toHaveBeenCalledWith(populatedConfig.servers[0]);
	});
});
