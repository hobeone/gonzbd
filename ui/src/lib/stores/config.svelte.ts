import { fetchJSON, setConfig } from '$lib/api';

interface ConfigState {
	data: Record<string, any> | null;
	loading: boolean;
	error: string | null;
	saving: boolean;
}

let config = $state<ConfigState>({
	data: null,
	loading: false,
	error: null,
	saving: false
});

export async function loadConfig() {
	if (config.loading) return;
	config.loading = true;
	config.error = null;
	try {
		const data = await fetchJSON<any>('/api?mode=get_config&output=json');
		config.data = data.config ?? data;
	} catch (e) {
		config.error = e instanceof Error ? e.message : String(e);
	} finally {
		config.loading = false;
	}
}

export function getConfig() {
	return config.data;
}

export function getConfigLoading() {
	return config.loading;
}

export function getConfigError() {
	return config.error;
}

export function isSaving() {
	return config.saving;
}

export async function updateField(section: string, keyword: string, value: string | number | boolean) {
	if (!config.data) return;

	// Optimistic update for flat sections
	let originalValue: any;
	const isFlat = !Array.isArray(config.data[section]);

	if (isFlat) {
		originalValue = config.data[section][keyword];
		config.data[section][keyword] = value;
	}

	config.saving = true;

	try {
		await setConfig(section, keyword, value);
		if (!isFlat) {
			// For non-flat sections (like servers), reload to get the updated state
			await loadConfig();
		}
	} catch (e) {
		// Revert on failure
		if (isFlat) {
			config.data[section][keyword] = originalValue;
		}
		config.error = `Failed to save ${keyword || section}: ${e instanceof Error ? e.message : String(e)}`;
	} finally {
		config.saving = false;
	}
}
