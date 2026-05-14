import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import type { WSEvent } from './websocket.svelte';

// Mock getCookie
vi.mock('$lib/utils', () => ({
	getCookie: vi.fn().mockReturnValue(null)
}));

// Mock ConnectionStore — WS store reports success/failure to it.
vi.mock('./connection.svelte', () => ({
	reportFailure: vi.fn(),
	reportDisconnect: vi.fn(),
	reportSuccess: vi.fn(),
	onReconnected: vi.fn(() => vi.fn())
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

	it('getWSStatus returns true after open', async () => {
		const { subscribeWS, getWSStatus } = await import('./websocket.svelte');
		const handler = vi.fn();

		const unsub = subscribeWS(handler);
		expect(getWSStatus()).toBe(false);

		MockWebSocket.instances[0].simulateOpen();
		expect(getWSStatus()).toBe(true);

		unsub();
	});

	it('reports failure on close (ConnectionStore owns reconnect)', async () => {
		const { subscribeWS, getWSStatus } = await import('./websocket.svelte');
		const handler = vi.fn();

		const unsub = subscribeWS(handler);
		const ws = MockWebSocket.instances[0];
		ws.simulateOpen();
		expect(getWSStatus()).toBe(true);

		// Simulate disconnect — WS store no longer self-reconnects.
		// It reports failure to ConnectionStore which owns retry timing.
		ws.close();
		expect(getWSStatus()).toBe(false);

		// No new connection should be created by the WS store itself.
		vi.advanceTimersByTime(5000);
		expect(MockWebSocket.instances.length).toBe(1);

		unsub();
	});

	it('error triggers close', async () => {
		const { subscribeWS } = await import('./websocket.svelte');
		const handler = vi.fn();

		const unsub = subscribeWS(handler);
		const ws = MockWebSocket.instances[0];
		ws.simulateOpen();

		// Simulate error — should trigger close.
		if (ws.onerror) {
			ws.onerror(new Event('error'));
		}
		expect(ws.readyState).toBe(3); // closed

		unsub();
	});

	it('ignores invalid JSON messages', async () => {
		const { subscribeWS } = await import('./websocket.svelte');
		const handler = vi.fn();

		const unsub = subscribeWS(handler);
		const ws = MockWebSocket.instances[0];
		ws.simulateOpen();

		// Send invalid JSON — should not throw or call handler.
		if (ws.onmessage) {
			ws.onmessage(new MessageEvent('message', { data: 'not-json{' }));
		}
		expect(handler).not.toHaveBeenCalled();

		unsub();
	});

	it('clears reconnect timeout on last unsubscribe', async () => {
		const { subscribeWS } = await import('./websocket.svelte');
		const handler = vi.fn();

		const unsub = subscribeWS(handler);
		const ws = MockWebSocket.instances[0];
		ws.simulateOpen();

		// Close to trigger reconnect scheduling.
		ws.close();

		// Unsub before reconnect fires — should cancel the reconnect.
		unsub();

		// Advance past the reconnect delay.
		vi.advanceTimersByTime(5000);

		// No new socket should have been created (only the original).
		expect(MockWebSocket.instances.length).toBe(1);
	});
});
