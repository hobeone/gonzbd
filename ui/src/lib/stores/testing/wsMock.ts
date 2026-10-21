import { vi } from 'vitest';

export const capturedWSHandlers = new Set<(event: any) => void>();
export const capturedWSHandler = (event: any) => {
	capturedWSHandlers.forEach((h) => h(event));
};

export const capturedReconnectHandlers = new Set<() => void>();
export const triggerReconnected = () => {
	capturedReconnectHandlers.forEach((h) => h());
};

export function resetWSMock() {
	capturedWSHandlers.clear();
	capturedReconnectHandlers.clear();
}

export function createWSMock() {
	return {
		subscribeWS: vi.fn((handler: (event: any) => void) => {
			capturedWSHandlers.add(handler);
			return vi.fn(() => {
				capturedWSHandlers.delete(handler);
			});
		})
	};
}

export function createConnectionMock() {
	return {
		reportFailure: vi.fn(),
		reportSuccess: vi.fn(),
		onReconnected: vi.fn((cb: () => void) => {
			capturedReconnectHandlers.add(cb);
			return vi.fn(() => {
				capturedReconnectHandlers.delete(cb);
			});
		})
	};
}
