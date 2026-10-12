import { describe, it, expect } from 'vitest';
import { PP_LEVELS, NEW_CATEGORY_PP, isPPLevel, parsePPLevel, ppStages } from './pp';

describe('pp level helpers', () => {
	it('lists the four levels in order', () => {
		expect(PP_LEVELS.map((l) => l.level)).toEqual([0, 1, 2, 3]);
	});

	it('defaults a new category to a valid level, matching the backend (3)', () => {
		expect(isPPLevel(NEW_CATEGORY_PP)).toBe(true);
		expect(NEW_CATEGORY_PP).toBe(3);
	});

	it('accepts only 0-3', () => {
		for (const ok of [0, 1, 2, 3]) expect(isPPLevel(ok)).toBe(true);
		// 4-7 are bitmask encodings the backend rejects.
		for (const bad of [-1, 4, 5, 6, 7, 1.5, '1', null, undefined]) expect(isPPLevel(bad)).toBe(false);
	});

	it('parses the string and number wire forms', () => {
		expect(parsePPLevel('2')).toBe(2);
		expect(parsePPLevel(0)).toBe(0);
		expect(parsePPLevel('7')).toBeNull();
		expect(parsePPLevel('')).toBeNull();
		expect(parsePPLevel(' 1')).toBeNull();
		expect(parsePPLevel(7)).toBeNull();
	});

	// Cumulative, not a bitmask: under a bitmask 2 would be unpack without
	// repair and 3 would be repair + unpack with no delete.
	it.each([
		[0, { repair: false, unpack: false, del: false }],
		[1, { repair: true, unpack: false, del: false }],
		[2, { repair: true, unpack: true, del: false }],
		[3, { repair: true, unpack: true, del: true }],
		['3', { repair: true, unpack: true, del: true }],
		[7, { repair: false, unpack: false, del: false }]
	])('level %s runs the stages below it', (pp, want) => {
		expect(ppStages(pp)).toEqual(want);
	});
});
