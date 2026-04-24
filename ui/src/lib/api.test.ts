import { describe, it, expect, vi, beforeEach } from 'vitest';
import { fetchQueue, fetchHistory } from './api';

// Mock global fetch
const mockFetch = vi.fn();
global.fetch = mockFetch;

describe('API Wrappers', () => {
	beforeEach(() => {
		mockFetch.mockReset();
		mockFetch.mockResolvedValue({
			ok: true,
			json: () => Promise.resolve({ status: true })
		});
	});

	it('fetchQueue constructs URL with pagination', async () => {
		await fetchQueue(10, 10);
		const url = new URL(mockFetch.mock.calls[0][0], 'http://localhost');
		expect(url.searchParams.get('mode')).toBe('queue');
		expect(url.searchParams.get('start')).toBe('10');
		expect(url.searchParams.get('limit')).toBe('10');
	});

	it('fetchQueue constructs URL with search', async () => {
		await fetchQueue(0, 10, { search: 'test' });
		const url = new URL(mockFetch.mock.calls[0][0], 'http://localhost');
		expect(url.searchParams.get('search')).toBe('test');
	});

	it('fetchHistory constructs URL with pagination and filters', async () => {
		await fetchHistory(20, 10, { status: 'Failed', search: 'my-job' });
		const url = new URL(mockFetch.mock.calls[0][0], 'http://localhost');
		expect(url.searchParams.get('mode')).toBe('history');
		expect(url.searchParams.get('start')).toBe('20');
		expect(url.searchParams.get('limit')).toBe('10');
		expect(url.searchParams.get('status')).toBe('Failed');
		expect(url.searchParams.get('search')).toBe('my-job');
	});
});
