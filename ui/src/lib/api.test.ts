import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import {
	fetchJSON,
	fetchQueue,
	fetchHistory,
	fetchVersion,
	fetchWarnings,
	fetchScripts,
	fetchCategories,
	setConfig,
	postAction,
	uploadNzb
} from './api';

// Mock global fetch
const mockFetch = vi.fn();
global.fetch = mockFetch;

function mockOk(data: any) {
	mockFetch.mockResolvedValue({
		ok: true,
		status: 200,
		statusText: 'OK',
		json: () => Promise.resolve(data)
	});
}

function mockError(status: number, statusText: string, body?: any) {
	mockFetch.mockResolvedValue({
		ok: false,
		status,
		statusText,
		json: () => (body ? Promise.resolve(body) : Promise.reject(new Error('no body')))
	});
}

describe('API Wrappers', () => {
	beforeEach(() => {
		mockFetch.mockReset();
	});

	// ── fetchJSON ──────────────────────────────────────────

	describe('fetchJSON', () => {
		it('returns parsed JSON on success', async () => {
			mockOk({ version: '1.0.0' });
			const result = await fetchJSON('/api?mode=version&output=json');
			expect(result).toEqual({ version: '1.0.0' });
		});

		it('throws on HTTP 500 with status text', async () => {
			mockError(500, 'Internal Server Error', {});
			await expect(fetchJSON('/api?mode=version')).rejects.toThrow(
				'API 500: Internal Server Error'
			);
		});

		it('extracts error field from JSON error body', async () => {
			mockError(400, 'Bad Request', { error: 'missing mode parameter' });
			await expect(fetchJSON('/api')).rejects.toThrow('missing mode parameter');
		});

		it('falls back to status text when error body has no error field', async () => {
			mockError(404, 'Not Found', { message: 'not helpful' });
			await expect(fetchJSON('/api?mode=nope')).rejects.toThrow(
				'API 404: Not Found'
			);
		});
	});

	// ── fetchQueue ─────────────────────────────────────────

	describe('fetchQueue', () => {
		beforeEach(() => mockOk({ status: true, queue: { slots: [] } }));

		it('constructs URL with pagination', async () => {
			await fetchQueue(10, 10);
			const url = new URL(mockFetch.mock.calls[0][0], 'http://localhost');
			expect(url.searchParams.get('mode')).toBe('queue');
			expect(url.searchParams.get('start')).toBe('10');
			expect(url.searchParams.get('limit')).toBe('10');
		});

		it('constructs URL with search', async () => {
			await fetchQueue(0, 10, { search: 'test' });
			const url = new URL(mockFetch.mock.calls[0][0], 'http://localhost');
			expect(url.searchParams.get('search')).toBe('test');
		});

		it('uses defaults for start and limit', async () => {
			await fetchQueue();
			const url = new URL(mockFetch.mock.calls[0][0], 'http://localhost');
			expect(url.searchParams.get('start')).toBe('0');
			expect(url.searchParams.get('limit')).toBe('10');
		});
	});

	// ── fetchHistory ──────────────────────────────────────

	describe('fetchHistory', () => {
		beforeEach(() => mockOk({ status: true, history: { slots: [] } }));

		it('constructs URL with pagination and filters', async () => {
			await fetchHistory(20, 10, { status: 'Failed', search: 'my-job' });
			const url = new URL(mockFetch.mock.calls[0][0], 'http://localhost');
			expect(url.searchParams.get('mode')).toBe('history');
			expect(url.searchParams.get('start')).toBe('20');
			expect(url.searchParams.get('limit')).toBe('10');
			expect(url.searchParams.get('status')).toBe('Failed');
			expect(url.searchParams.get('search')).toBe('my-job');
		});
	});

	// ── fetchVersion ──────────────────────────────────────

	describe('fetchVersion', () => {
		it('returns version response', async () => {
			mockOk({ status: true, version: '4.3.0' });
			const result = await fetchVersion();
			expect(result).toEqual({ status: true, version: '4.3.0' });
		});

		it('sends mode=version', async () => {
			mockOk({ status: true, version: '4.3.0' });
			await fetchVersion();
			const url = new URL(mockFetch.mock.calls[0][0], 'http://localhost');
			expect(url.searchParams.get('mode')).toBe('version');
		});
	});

	// ── fetchWarnings ─────────────────────────────────────

	describe('fetchWarnings', () => {
		it('returns warnings array', async () => {
			mockOk({ status: true, warnings: ['disk full', 'slow'] });
			const result = await fetchWarnings();
			expect(result.warnings).toEqual(['disk full', 'slow']);
		});

		it('sends mode=warnings', async () => {
			mockOk({ status: true, warnings: [] });
			await fetchWarnings();
			const url = new URL(mockFetch.mock.calls[0][0], 'http://localhost');
			expect(url.searchParams.get('mode')).toBe('warnings');
		});
	});

	// ── fetchScripts ──────────────────────────────────────

	describe('fetchScripts', () => {
		it('unwraps scripts from nested response', async () => {
			mockOk({ scripts: ['cleanup.sh', 'notify.sh'] });
			const result = await fetchScripts();
			expect(result).toEqual(['cleanup.sh', 'notify.sh']);
		});

		it('sends mode=get_scripts', async () => {
			mockOk({ scripts: [] });
			await fetchScripts();
			const url = new URL(mockFetch.mock.calls[0][0], 'http://localhost');
			expect(url.searchParams.get('mode')).toBe('get_scripts');
		});
	});

	// ── fetchCategories ───────────────────────────────────

	describe('fetchCategories', () => {
		it('unwraps categories from nested response', async () => {
			mockOk({ categories: ['*', 'TV', 'Movies'] });
			const result = await fetchCategories();
			expect(result).toEqual(['*', 'TV', 'Movies']);
		});

		it('sends mode=get_cats', async () => {
			mockOk({ categories: [] });
			await fetchCategories();
			const url = new URL(mockFetch.mock.calls[0][0], 'http://localhost');
			expect(url.searchParams.get('mode')).toBe('get_cats');
		});
	});

	// ── setConfig ─────────────────────────────────────────

	describe('setConfig', () => {
		it('sends POST with FormData containing section/keyword/value', async () => {
			mockOk({ status: true });
			await setConfig('general', 'host', '0.0.0.0');

			expect(mockFetch).toHaveBeenCalledTimes(1);
			const [url, opts] = mockFetch.mock.calls[0];
			expect(url).toBe('/api');
			expect(opts.method).toBe('POST');

			const body = opts.body as FormData;
			expect(body.get('mode')).toBe('set_config');
			expect(body.get('section')).toBe('general');
			expect(body.get('keyword')).toBe('host');
			expect(body.get('value')).toBe('0.0.0.0');
		});

		it('converts boolean value to string', async () => {
			mockOk({ status: true });
			await setConfig('downloads', 'pre_check', true);

			const body = mockFetch.mock.calls[0][1].body as FormData;
			expect(body.get('value')).toBe('true');
		});

		it('converts numeric value to string', async () => {
			mockOk({ status: true });
			await setConfig('general', 'port', 8080);

			const body = mockFetch.mock.calls[0][1].body as FormData;
			expect(body.get('value')).toBe('8080');
		});

		it('throws on non-ok response', async () => {
			mockError(500, 'Internal Server Error');
			await expect(setConfig('general', 'host', '0.0.0.0')).rejects.toThrow(
				'Set Config 500: Internal Server Error'
			);
		});
	});

	// ── postAction ────────────────────────────────────────

	describe('postAction', () => {
		it('sends GET with mode and params', async () => {
			mockOk({ status: true });
			await postAction('pause');

			const url = new URL(mockFetch.mock.calls[0][0], 'http://localhost');
			expect(url.searchParams.get('mode')).toBe('pause');
			expect(url.searchParams.get('output')).toBe('json');
		});

		it('includes additional params', async () => {
			mockOk({ status: true });
			await postAction('queue', { name: 'delete', value: 'abc123' });

			const url = new URL(mockFetch.mock.calls[0][0], 'http://localhost');
			expect(url.searchParams.get('mode')).toBe('queue');
			expect(url.searchParams.get('name')).toBe('delete');
			expect(url.searchParams.get('value')).toBe('abc123');
		});
	});

	// ── uploadNzb ─────────────────────────────────────────

	describe('uploadNzb', () => {
		it('sends POST with file in FormData', async () => {
			mockOk({ status: true });
			const file = new File(['<nzb>test</nzb>'], 'test.nzb', {
				type: 'application/x-nzb'
			});

			await uploadNzb(file);

			expect(mockFetch).toHaveBeenCalledTimes(1);
			const [url, opts] = mockFetch.mock.calls[0];
			expect(url).toBe('/api');
			expect(opts.method).toBe('POST');

			const body = opts.body as FormData;
			expect(body.get('mode')).toBe('addfile');
			expect(body.get('output')).toBe('json');
			expect(body.get('nzbfile')).toBeInstanceOf(File);
		});

		it('throws on non-ok response', async () => {
			mockError(413, 'Payload Too Large');
			const file = new File(['big'], 'big.nzb');

			await expect(uploadNzb(file)).rejects.toThrow('Upload 413: Payload Too Large');
		});
	});
});
