import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

// Mock dependencies
vi.mock('$lib/api', () => ({
	postAction: vi.fn()
}));

let capturedWSHandler: ((event: any) => void) | null = null;
const unsubscribeMock = vi.fn();
vi.mock('./websocket.svelte', () => ({
	subscribeWS: vi.fn((handler: (event: any) => void) => {
		capturedWSHandler = handler;
		return unsubscribeMock;
	})
}));

import {
	getSpeedBytesPerSec,
	getSpeedHistory,
	getTotalRemainingBytes,
	getSpeedLimitBytesPerSec,
	getBandwidthMaxBytesPerSec,
	getBandwidthPerc,
	getServerStats,
	setBandwidthPerc,
	startTelemetry,
	stopTelemetry
} from './telemetry.svelte';
import { postAction } from '$lib/api';

describe('Telemetry Store', () => {
	beforeEach(() => {
		vi.useFakeTimers();
		vi.clearAllMocks();
		capturedWSHandler = null;
		stopTelemetry();
	});

	afterEach(() => {
		stopTelemetry();
		vi.useRealTimers();
	});

	it('returns default telemetry state initially', () => {
		expect(getSpeedBytesPerSec()).toBe(0);
		expect(getSpeedHistory()).toEqual([]);
		expect(getTotalRemainingBytes()).toBe(0);
		expect(getSpeedLimitBytesPerSec()).toBe(0);
		expect(getBandwidthMaxBytesPerSec()).toBe(0);
		expect(getBandwidthPerc()).toBe(100);
		expect(getServerStats()).toEqual([]);
	});

	it('subscribes to WebSocket on startTelemetry', () => {
		expect(capturedWSHandler).toBeNull();
		startTelemetry();
		expect(capturedWSHandler).not.toBeNull();
	});

	it('updates telemetry state on metrics events', () => {
		startTelemetry();
		expect(capturedWSHandler).not.toBeNull();

		capturedWSHandler!({
			event: 'metrics',
			speed: 1048576,
			remaining: 5242880,
			speed_limit: 2048576,
			bandwidth_max: 10485760,
			bandwidth_perc: 50,
			servers: [
				{
					name: 'Server1',
					active: true,
					connections: [
						{ id: 1, active: true, article: 'art1' }
					]
				}
			]
		});

		expect(getSpeedBytesPerSec()).toBe(1048576);
		expect(getTotalRemainingBytes()).toBe(5242880);
		expect(getSpeedLimitBytesPerSec()).toBe(2048576);
		expect(getBandwidthMaxBytesPerSec()).toBe(10485760);
		expect(getBandwidthPerc()).toBe(50);
		expect(getSpeedHistory()).toEqual([1048576]);
		expect(getServerStats()).toHaveLength(1);
		expect(getServerStats()[0].name).toBe('Server1');
	});

	it('slices speed history to max 60 entries', () => {
		startTelemetry();
		for (let i = 0; i < 70; i++) {
			capturedWSHandler!({
				event: 'metrics',
				speed: i
			});
		}
		expect(getSpeedHistory()).toHaveLength(60);
		expect(getSpeedHistory()[0]).toBe(10); // first 10 dropped
		expect(getSpeedHistory()[59]).toBe(69);
	});

	it('setBandwidthPerc posts action to API', async () => {
		await setBandwidthPerc(75);
		expect(postAction).toHaveBeenCalledWith('set_config', {
			section: 'downloads',
			keyword: 'bandwidth_perc',
			value: '75'
		});
	});

	it('unsubscribes and stops on stopTelemetry', () => {
		startTelemetry();
		expect(capturedWSHandler).not.toBeNull();
		stopTelemetry();
		expect(unsubscribeMock).toHaveBeenCalled();
	});
});
