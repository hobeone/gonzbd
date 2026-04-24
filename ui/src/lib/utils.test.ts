import { describe, it, expect, vi, beforeEach } from 'vitest';
import { formatSpeed, formatSize, getCookie } from './utils';

describe('formatSpeed', () => {
	it('formats zero bytes', () => {
		expect(formatSpeed(0)).toBe('0 B/s');
	});

	it('formats bytes below 1 KB', () => {
		expect(formatSpeed(512)).toBe('512 B/s');
	});

	it('rounds fractional bytes', () => {
		expect(formatSpeed(1023.7)).toBe('1024 B/s');
	});

	it('formats exactly 1 KB', () => {
		expect(formatSpeed(1024)).toBe('1.0 KB/s');
	});

	it('formats KB range', () => {
		expect(formatSpeed(1536)).toBe('1.5 KB/s');
	});

	it('formats exactly 1 MB', () => {
		expect(formatSpeed(1048576)).toBe('1.0 MB/s');
	});

	it('formats MB range', () => {
		expect(formatSpeed(5 * 1024 * 1024)).toBe('5.0 MB/s');
	});

	it('formats large MB values', () => {
		expect(formatSpeed(100 * 1024 * 1024)).toBe('100.0 MB/s');
	});
});

describe('formatSize', () => {
	it('formats zero bytes', () => {
		expect(formatSize(0)).toBe('0 B');
	});

	it('formats bytes below 1 KB', () => {
		expect(formatSize(500)).toBe('500 B');
	});

	it('formats exactly 1 KB', () => {
		expect(formatSize(1024)).toBe('1.0 KB');
	});

	it('formats KB range', () => {
		expect(formatSize(1536)).toBe('1.5 KB');
	});

	it('formats exactly 1 MB', () => {
		expect(formatSize(1024 * 1024)).toBe('1.0 MB');
	});

	it('formats MB range', () => {
		expect(formatSize(500 * 1024 * 1024)).toBe('500.0 MB');
	});

	it('formats exactly 1 GB', () => {
		expect(formatSize(1073741824)).toBe('1.00 GB');
	});

	it('formats GB range', () => {
		expect(formatSize(2.5 * 1024 * 1024 * 1024)).toBe('2.50 GB');
	});
});

describe('getCookie', () => {
	beforeEach(() => {
		// Reset document.cookie mock
		Object.defineProperty(document, 'cookie', {
			writable: true,
			value: '',
		});
	});

	it('returns value when cookie exists', () => {
		document.cookie = 'sab_apikey=abc123';
		expect(getCookie('sab_apikey')).toBe('abc123');
	});

	it('returns null when cookie does not exist', () => {
		document.cookie = 'other=value';
		expect(getCookie('sab_apikey')).toBeNull();
	});

	it('returns null when no cookies exist', () => {
		document.cookie = '';
		expect(getCookie('sab_apikey')).toBeNull();
	});

	it('handles multiple cookies correctly', () => {
		document.cookie = 'first=one; sab_apikey=abc123; third=three';
		expect(getCookie('sab_apikey')).toBe('abc123');
	});

	it('handles cookie value containing equals sign', () => {
		document.cookie = 'token=abc=def=ghi';
		expect(getCookie('token')).toBe('abc=def=ghi');
	});

	it('does not match partial cookie names', () => {
		document.cookie = 'my_sab_apikey=wrong';
		expect(getCookie('sab_apikey')).toBeNull();
	});
});
