import { render, screen } from '@testing-library/svelte';
import { describe, it, expect, vi } from 'vitest';
import GeneralSection from './GeneralSection.svelte';

// Mock sub-components to avoid shadcn complexity
vi.mock('$lib/components/ui/separator', () => ({
	Separator: function MockSeparator() {}
}));

describe('GeneralSection', () => {
	const mockConfig = {
		general: {
			host: '0.0.0.0',
			port: 8080,
			api_key: 'test-key-123',
			nzb_key: 'nzb-key-456',
			download_dir: '/tmp/incomplete',
			complete_dir: '/tmp/complete',
			log_level: 'info'
		}
	};

	it('renders the section heading', () => {
		render(GeneralSection, { configData: mockConfig, onFieldUpdate: vi.fn() });
		expect(screen.getByText('General Settings')).toBeInTheDocument();
	});

	it('renders section description', () => {
		render(GeneralSection, { configData: mockConfig, onFieldUpdate: vi.fn() });
		expect(screen.getByText(/Server connectivity and basic daemon tuning/)).toBeInTheDocument();
	});

	it('renders all 7 config input fields', () => {
		render(GeneralSection, { configData: mockConfig, onFieldUpdate: vi.fn() });
		expect(screen.getByText('Host')).toBeInTheDocument();
		expect(screen.getByText('Port')).toBeInTheDocument();
		expect(screen.getByText('API Key')).toBeInTheDocument();
		expect(screen.getByText('NZB Key')).toBeInTheDocument();
		expect(screen.getByText('Download Directory')).toBeInTheDocument();
		expect(screen.getByText('Complete Directory')).toBeInTheDocument();
		expect(screen.getByText('Log Level')).toBeInTheDocument();
	});

	it('displays current values in inputs', () => {
		render(GeneralSection, { configData: mockConfig, onFieldUpdate: vi.fn() });
		const hostInput = screen.getByDisplayValue('0.0.0.0');
		expect(hostInput).toBeInTheDocument();
	});
});
