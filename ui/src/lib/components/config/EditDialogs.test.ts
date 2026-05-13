import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi, beforeEach } from 'vitest';

vi.mock('$lib/api', () => ({
	postAction: vi.fn().mockResolvedValue({ status: true }),
	fetchCategories: vi.fn().mockResolvedValue(['*', 'TV', 'Movies']),
	fetchScripts: vi.fn().mockResolvedValue(['cleanup.sh'])
}));

import ServerEditDialog from './ServerEditDialog.svelte';
import CategoryEditDialog from './CategoryEditDialog.svelte';
import ScheduleEditDialog from './ScheduleEditDialog.svelte';
import RSSEditDialog from './RSSEditDialog.svelte';

describe('ServerEditDialog', () => {
	beforeEach(() => vi.clearAllMocks());

	it('renders Add Server title when no server prop', () => {
		render(ServerEditDialog, {
			props: { open: true, onsave: vi.fn() }
		});
		expect(screen.getByText('Add Server')).toBeInTheDocument();
	});

	it('renders Edit Server title when server prop provided', () => {
		render(ServerEditDialog, {
			props: {
				open: true,
				server: {
					name: 'TestSrv', host: 'news.test.com', port: 119,
					username: '', password: '', connections: 8, ssl: false,
					ssl_verify: 2, ssl_ciphers: '', priority: 0,
					required: false, optional: true, retention: 0,
					timeout: 60, pipelining_requests: 2, enable: true
				},
				onsave: vi.fn()
			}
		});
		expect(screen.getByText('Edit Server')).toBeInTheDocument();
	});

	it('renders form fields: name, host, port, username, password', () => {
		render(ServerEditDialog, {
			props: { open: true, onsave: vi.fn() }
		});
		expect(screen.getByLabelText('Server Name')).toBeInTheDocument();
		expect(screen.getByLabelText('Host')).toBeInTheDocument();
		expect(screen.getByLabelText('Port')).toBeInTheDocument();
		expect(screen.getByLabelText('Username')).toBeInTheDocument();
		expect(screen.getByLabelText('Password')).toBeInTheDocument();
	});

	it('renders Connections and Priority fields', () => {
		render(ServerEditDialog, {
			props: { open: true, onsave: vi.fn() }
		});
		expect(screen.getByLabelText('Connections')).toBeInTheDocument();
		expect(screen.getByLabelText('Priority')).toBeInTheDocument();
	});

	it('renders SSL and Enabled checkboxes', () => {
		render(ServerEditDialog, {
			props: { open: true, onsave: vi.fn() }
		});
		expect(screen.getByText('SSL / TLS')).toBeInTheDocument();
		expect(screen.getByText('Enabled')).toBeInTheDocument();
	});

	it('renders Test Server, Cancel, Save buttons', () => {
		render(ServerEditDialog, {
			props: { open: true, onsave: vi.fn() }
		});
		expect(screen.getByText('Test Server')).toBeInTheDocument();
		expect(screen.getByText('Cancel')).toBeInTheDocument();
		expect(screen.getByText('Save')).toBeInTheDocument();
	});

	it('Save is disabled when host or name is empty', () => {
		render(ServerEditDialog, {
			props: { open: true, onsave: vi.fn() }
		});
		const saveBtn = screen.getByText('Save');
		expect(saveBtn).toBeDisabled();
	});

	it('does not render when closed', () => {
		render(ServerEditDialog, {
			props: { open: false, onsave: vi.fn() }
		});
		expect(screen.queryByText('Add Server')).not.toBeInTheDocument();
	});

	it('does not show Delete button when adding a new server', () => {
		render(ServerEditDialog, {
			props: { open: true, onsave: vi.fn(), ondelete: vi.fn() }
		});
		expect(screen.queryByText('Delete')).not.toBeInTheDocument();
	});

	it('shows Delete button when editing an existing server', () => {
		render(ServerEditDialog, {
			props: {
				open: true,
				server: {
					name: 'TestSrv', host: 'news.test.com', port: 119,
					username: '', password: '', connections: 8, ssl: false,
					ssl_verify: 2, ssl_ciphers: '', priority: 0,
					required: false, optional: true, retention: 0,
					timeout: 60, pipelining_requests: 2, enable: true
				},
				onsave: vi.fn(),
				ondelete: vi.fn()
			}
		});
		expect(screen.getByText('Delete')).toBeInTheDocument();
	});

	it('calls ondelete after confirmation', async () => {
		const ondelete = vi.fn();
		render(ServerEditDialog, {
			props: {
				open: true,
				server: {
					name: 'TestSrv', host: 'news.test.com', port: 119,
					username: '', password: '', connections: 8, ssl: false,
					ssl_verify: 2, ssl_ciphers: '', priority: 0,
					required: false, optional: true, retention: 0,
					timeout: 60, pipelining_requests: 2, enable: true
				},
				onsave: vi.fn(),
				ondelete
			}
		});
		await fireEvent.click(screen.getByText('Delete'));
		expect(screen.getByText('Yes, delete')).toBeInTheDocument();
		await fireEvent.click(screen.getByText('Yes, delete'));
		expect(ondelete).toHaveBeenCalledWith('TestSrv');
	});

	it('dismisses delete confirmation when No is clicked', async () => {
		render(ServerEditDialog, {
			props: {
				open: true,
				server: {
					name: 'TestSrv', host: 'news.test.com', port: 119,
					username: '', password: '', connections: 8, ssl: false,
					ssl_verify: 2, ssl_ciphers: '', priority: 0,
					required: false, optional: true, retention: 0,
					timeout: 60, pipelining_requests: 2, enable: true
				},
				onsave: vi.fn(),
				ondelete: vi.fn()
			}
		});
		await fireEvent.click(screen.getByText('Delete'));
		expect(screen.getByText('Yes, delete')).toBeInTheDocument();
		await fireEvent.click(screen.getByText('No'));
		expect(screen.queryByText('Yes, delete')).not.toBeInTheDocument();
		// Delete button should be back
		expect(screen.getByText('Delete')).toBeInTheDocument();
	});
});

describe('CategoryEditDialog', () => {
	beforeEach(() => vi.clearAllMocks());

	it('renders Add Category title when no category prop', () => {
		render(CategoryEditDialog, {
			props: { open: true, onsave: vi.fn() }
		});
		expect(screen.getByText('Add Category')).toBeInTheDocument();
	});

	it('renders form fields: Category Name, Folder / Path', () => {
		render(CategoryEditDialog, {
			props: { open: true, onsave: vi.fn() }
		});
		expect(screen.getByLabelText('Category Name')).toBeInTheDocument();
		expect(screen.getByLabelText('Folder / Path')).toBeInTheDocument();
	});

	it('does not render when closed', () => {
		render(CategoryEditDialog, {
			props: { open: false, onsave: vi.fn() }
		});
		expect(screen.queryByText('Add Category')).not.toBeInTheDocument();
	});
});

describe('ScheduleEditDialog', () => {
	beforeEach(() => vi.clearAllMocks());

	it('renders Add Schedule title when no schedule prop', () => {
		render(ScheduleEditDialog, {
			props: { open: true, onsave: vi.fn() }
		});
		expect(screen.getByText('Add Schedule')).toBeInTheDocument();
	});

	it('renders Cancel and Save buttons', () => {
		render(ScheduleEditDialog, {
			props: { open: true, onsave: vi.fn() }
		});
		expect(screen.getByText('Cancel')).toBeInTheDocument();
		expect(screen.getByText('Save')).toBeInTheDocument();
	});

	it('does not render when closed', () => {
		render(ScheduleEditDialog, {
			props: { open: false, onsave: vi.fn() }
		});
		expect(screen.queryByText('Add Schedule')).not.toBeInTheDocument();
	});
});


describe('RSSEditDialog', () => {
	beforeEach(() => vi.clearAllMocks());

	it('renders Add RSS Feed title when no feed prop', () => {
		render(RSSEditDialog, {
			props: { open: true, onsave: vi.fn() }
		});
		expect(screen.getByText('Add RSS Feed')).toBeInTheDocument();
	});

	it('renders Cancel and Save Feed buttons', () => {
		render(RSSEditDialog, {
			props: { open: true, onsave: vi.fn() }
		});
		expect(screen.getByText('Cancel')).toBeInTheDocument();
		expect(screen.getByText('Save Feed')).toBeInTheDocument();
	});

	it('does not render when closed', () => {
		render(RSSEditDialog, {
			props: { open: false, onsave: vi.fn() }
		});
		expect(screen.queryByText('Add RSS Feed')).not.toBeInTheDocument();
	});
});
