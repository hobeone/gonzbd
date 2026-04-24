import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import type { WSEvent } from './websocket.svelte';

// Mock getCookie
vi.mock('$lib/utils', () => ({
	getCookie: vi.fn().mockReturnValue(null)
}));

// Create a mock WebSocket class
class MockWebSocket {
	static instances: MockWebSocket[] = [];
	url: string;
	onopen: ((ev: Event) => void) | null = null;
	onclose: ((ev: CloseEvent) => void) | null = null;
	onmessage: ((ev: MessageEvent) => void) | null = null;
	onerror: ((ev: Event) => void) | null = null;
	readyState = 0;

	constructor(url: string) {
		this.url = url;
		MockWebSocket.instances.push(this);
	}

	close() {
		this.readyState = 3;
		if (this.onclose) {
			this.onclose(new CloseEvent('close'));
		}
	}

	// Simulate receiving a message
	simulateMessage(data: WSEvent) {
		if (this.onmessage) {
			this.onmessage(new MessageEvent('message', { data: JSON.stringify(data) }));
		}
	}

	// Simulate successful connection
	simulateOpen() {
		this.readyState = 1;
		if (this.onopen) {
			this.onopen(new Event('open'));
		}
	}
}

// Stub WebSocket globally before imports
vi.stubGlobal('WebSocket', MockWebSocket);

describe('websocket store', () => {
	beforeEach(() => {
		vi.useFakeTimers();
		MockWebSocket.instances = [];
	});

	afterEach(() => {
		vi.useRealTimers();
		vi.resetModules();
	});

	it('connects on first subscriber', async () => {
		const { subscribeWS } = await import('./websocket.svelte');
		const handler = vi.fn();

		const unsub = subscribeWS(handler);

		expect(MockWebSocket.instances.length).toBe(1);
		expect(MockWebSocket.instances[0].url).toContain('/api/ws');

		unsub();
	});

	it('forwards parsed JSON messages to handlers', async () => {
		const { subscribeWS } = await import('./websocket.svelte');
		const handler = vi.fn();

		const unsub = subscribeWS(handler);
		const ws = MockWebSocket.instances[0];
		ws.simulateOpen();

		const event: WSEvent = { event: 'queue_update', speed: 1048576 };
		ws.simulateMessage(event);

		expect(handler).toHaveBeenCalledWith(event);

		unsub();
	});

	it('last unsubscribe closes socket', async () => {
		const { subscribeWS } = await import('./websocket.svelte');
		const handler1 = vi.fn();
		const handler2 = vi.fn();

		const unsub1 = subscribeWS(handler1);
		const unsub2 = subscribeWS(handler2);
		const ws = MockWebSocket.instances[0];

		// First unsub shouldn't close
		unsub1();
		expect(ws.readyState).not.toBe(3);

		// Last unsub should close
		unsub2();
		expect(ws.readyState).toBe(3);
	});

	it('appends apikey to URL when cookie exists', async () => {
		const { getCookie } = await import('$lib/utils');
		vi.mocked(getCookie).mockReturnValue('test-api-key');

		const { subscribeWS } = await import('./websocket.svelte');
		const handler = vi.fn();

		const unsub = subscribeWS(handler);

		expect(MockWebSocket.instances[0].url).toContain('apikey=test-api-key');

		unsub();
	});
});
