import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

// Mock $lib/api before importing
vi.mock('$lib/api', () => ({
	fetchJSON: vi.fn(),
	setConfig: vi.fn()
}));

import {
	loadConfig,
	getConfig,
	getConfigLoading,
	getConfigError,
	isSaving,
	updateField
} from './config.svelte';
import { fetchJSON, setConfig } from '$lib/api';

describe('config store', () => {
	beforeEach(() => {
		vi.clearAllMocks();
	});

	describe('loadConfig', () => {
		it('fetches and stores config data', async () => {
			const mockConfig = {
				general: { host: '0.0.0.0', port: 8080 },
				downloads: { bandwidth_max: '10M' }
			};
			vi.mocked(fetchJSON).mockResolvedValue({ config: mockConfig });

			await loadConfig();

			expect(fetchJSON).toHaveBeenCalledWith('/api?mode=get_config&output=json');
			expect(getConfig()).toEqual(mockConfig);
			expect(getConfigError()).toBeNull();
		});

		it('sets error on failure', async () => {
			vi.mocked(fetchJSON).mockRejectedValue(new Error('Network error'));

			await loadConfig();

			expect(getConfigError()).toBe('Network error');
		});
	});

	describe('updateField', () => {
		it('calls setConfig with correct params', async () => {
			const mockConfig = {
				general: { host: '0.0.0.0', port: 8080 },
			};
			vi.mocked(fetchJSON).mockResolvedValue({ config: mockConfig });
			await loadConfig();

			vi.mocked(setConfig).mockResolvedValue({ status: true });

			await updateField('general', 'host', '127.0.0.1');

			expect(setConfig).toHaveBeenCalledWith('general', 'host', '127.0.0.1');
		});

		it('reverts value on setConfig failure for flat sections', async () => {
			const mockConfig = {
				general: { host: '0.0.0.0', port: 8080 },
			};
			vi.mocked(fetchJSON).mockResolvedValue({ config: mockConfig });
			await loadConfig();

			vi.mocked(setConfig).mockRejectedValue(new Error('Save failed'));

			await updateField('general', 'host', 'bad-host');

			// Value should be rolled back to original.
			expect(getConfig()?.general.host).toBe('0.0.0.0');
			expect(getConfigError()).toContain('Save failed');
		});

		it('reloads config for non-flat (array) sections on success', async () => {
			const mockConfig = {
				servers: [{ name: 'srv1' }],
			};
			vi.mocked(fetchJSON).mockResolvedValue({ config: mockConfig });
			await loadConfig();

			vi.mocked(setConfig).mockResolvedValue({ status: true });
			// fetchJSON will be called again for the reload.
			vi.mocked(fetchJSON).mockResolvedValue({ config: { servers: [{ name: 'srv1' }, { name: 'srv2' }] } });

			await updateField('servers', '', JSON.stringify([{ name: 'srv1' }, { name: 'srv2' }]));

			// setConfig called, then loadConfig called again.
			expect(setConfig).toHaveBeenCalled();
			expect(fetchJSON).toHaveBeenCalledTimes(2); // initial load + reload in updateField
		});
	});

	describe('loading/saving state', () => {
		it('getConfigLoading returns true during loadConfig', async () => {
			let resolvePromise: (v: any) => void;
			vi.mocked(fetchJSON).mockReturnValue(new Promise((r) => { resolvePromise = r; }));

			const p = loadConfig();
			expect(getConfigLoading()).toBe(true);

			resolvePromise!({ config: {} });
			await p;
			expect(getConfigLoading()).toBe(false);
		});

		it('isSaving returns true during updateField', async () => {
			vi.mocked(fetchJSON).mockResolvedValue({ config: { general: { a: 1 } } });
			await loadConfig();

			let resolvePromise: (v: any) => void;
			vi.mocked(setConfig).mockReturnValue(new Promise((r) => { resolvePromise = r; }));

			const p = updateField('general', 'a', 2);
			expect(isSaving()).toBe(true);

			resolvePromise!({ status: true });
			await p;
			expect(isSaving()).toBe(false);
		});

		it('concurrent loadConfig calls are deduplicated', async () => {
			let resolvePromise: (v: any) => void;
			vi.mocked(fetchJSON).mockReturnValue(new Promise((r) => { resolvePromise = r; }));

			const p1 = loadConfig();
			const p2 = loadConfig(); // Should be a no-op.

			resolvePromise!({ config: {} });
			await p1;
			await p2;

			expect(fetchJSON).toHaveBeenCalledTimes(1);
		});
	});
});
