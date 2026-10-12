// Post-processing level, as the API defines it: a cumulative level 0-3, not
// a bitmask. Each level adds one stage to the one below it, so 3 means
// repair + unpack + delete sources. The backend rejects a category pp outside
// [0,3] (internal/config/validate.go), and the CategoryConfig.PP doc comment
// in internal/config/categories.go states the same table.
export type PPLevel = 0 | 1 | 2 | 3;

export interface PPLevelInfo {
	level: PPLevel;
	/** Short form, naming the stage this level adds. */
	label: string;
	/** Every stage the level runs. */
	description: string;
}

export const PP_LEVELS: readonly PPLevelInfo[] = [
	{ level: 0, label: 'Download', description: 'Download only' },
	{ level: 1, label: '+Repair', description: 'Repair' },
	{ level: 2, label: '+Unpack', description: 'Repair, Unpack' },
	{ level: 3, label: '+Delete', description: 'Repair, Unpack, Delete sources' }
];

// The level a new category starts at: BuiltinDefaultCategory's PP in
// internal/config/categories.go, which Default() seeds a fresh config with.
export const NEW_CATEGORY_PP: PPLevel = 3;

/** Reports whether pp is one of the four levels the API accepts. */
export function isPPLevel(pp: unknown): pp is PPLevel {
	return pp === 0 || pp === 1 || pp === 2 || pp === 3;
}

/**
 * Parses a level from its wire form. The queue API reports a job's pp as a
 * string; a category carries a number. Anything else is null.
 */
export function parsePPLevel(pp: string | number): PPLevel | null {
	const n = typeof pp === 'number' ? pp : /^[0-3]$/.test(pp) ? Number(pp) : NaN;
	return isPPLevel(n) ? n : null;
}

/** The stages a level runs. An invalid level runs none. */
export function ppStages(pp: string | number): { repair: boolean; unpack: boolean; del: boolean } {
	const level = parsePPLevel(pp) ?? -1;
	return { repair: level >= 1, unpack: level >= 2, del: level >= 3 };
}
