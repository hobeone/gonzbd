import { fetchJSON, setConfig } from '$lib/api';

let configData = $state<Record<string, any> | null>(null);
let configLoading = $state(false);
let configError = $state<string | null>(null);
let configSaving = $state(false);

export async function loadConfig() {
	if (configLoading) return;
	configLoading = true;
	configError = null;
	try {
		const data = await fetchJSON<any>('/api?mode=get_config&output=json');
		configData = data.config ?? data;
	} catch (e) {
		configError = e instanceof Error ? e.message : String(e);
	} finally {
		configLoading = false;
	}
}

export function getConfig() {
	return configData;
}

export function getConfigLoading() {
	return configLoading;
}

export function getConfigError() {
	return configError;
}

export function isSaving() {
	return configSaving;
}

export async function updateField(section: string, keyword: string, value: string | number | boolean) {
	if (!configData) return;

	let originalValue: any;
	const isFlat = !Array.isArray(configData[section]);

	if (isFlat) {
		originalValue = configData[section][keyword];
		configData[section][keyword] = value;
	}

	configSaving = true;

	try {
		await setConfig(section, keyword, value);
		if (!isFlat) {
			await loadConfig();
		}
	} catch (e) {
		if (isFlat) {
			configData[section][keyword] = originalValue;
		}
		configError = `Failed to save ${keyword || section}: ${e instanceof Error ? e.message : String(e)}`;
	} finally {
		configSaving = false;
	}
}
