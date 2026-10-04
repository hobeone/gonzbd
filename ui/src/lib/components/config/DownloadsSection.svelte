<script lang="ts">
	import { Separator } from '#lib/components/ui/separator/index.js';
	import ConfigInput from './ConfigInput.svelte';
	import ConfigSelect from './ConfigSelect.svelte';
	import ConfigSwitch from './ConfigSwitch.svelte';
	import ConfigTextarea from './ConfigTextarea.svelte';

	let {
		configData,
		onFieldUpdate
	}: {
		configData: Record<string, any>;
		onFieldUpdate: (section: string, keyword: string, value: string | number | boolean) => void;
	} = $props();

	const unwantedActionOptions = [
		{ value: 'off', label: 'Off' },
		{ value: 'pause', label: 'Pause the job' },
		{ value: 'fail', label: 'Fail the job' }
	];
	const unwantedModeOptions = [
		{ value: 'blacklist', label: 'Blacklist (listed extensions are unwanted)' },
		{ value: 'whitelist', label: 'Whitelist (only listed extensions are allowed)' }
	];
</script>

<section class="space-y-6">
	<div>
		<h3 class="text-lg font-medium">Download Settings</h3>
		<p class="text-sm text-muted-foreground">Throttling, disk guards, and retry behavior.</p>
	</div>
	<Separator />
	<div class="divide-y divide-gray-100 dark:divide-gray-800">
		<ConfigInput section="downloads" keyword="bandwidth_max" label="Maximum Bandwidth" value={configData.downloads.bandwidth_max} description="Absolute ceiling (e.g. 10M, 500K)." onupdate={onFieldUpdate} />
		<ConfigInput section="downloads" keyword="min_free_space" label="Minimum Free Space" value={configData.downloads.min_free_space} description="Pause download if disk space drops below this (e.g. 1G)." onupdate={onFieldUpdate} />
		<ConfigInput section="downloads" keyword="write_cache_size" label="Article Cache" value={configData.downloads.write_cache_size} description="In-memory cache size (e.g. 500M)." requiresRestart={true} onupdate={onFieldUpdate} />
		<ConfigInput section="downloads" keyword="max_art_tries" label="Article Retries" type="number" value={configData.downloads.max_art_tries} description="Max attempts across all servers per article." onupdate={onFieldUpdate} />
		<ConfigInput section="downloads" keyword="max_active_jobs" label="Maximum Active Jobs" type="number" value={configData.downloads.max_active_jobs} description="Maximum number of jobs downloading or repairing concurrently." onupdate={onFieldUpdate} />
		<ConfigSwitch section="downloads" keyword="top_only" label="Top-only server mode" value={configData.downloads.top_only} description="Only use the highest-priority server group. Backup servers are never tried." onupdate={onFieldUpdate} />
		<ConfigSwitch section="downloads" keyword="pre_check" label="Pre-check article availability" value={configData.downloads.pre_check} description="STAT check before download (saves bandwidth)." onupdate={onFieldUpdate} />
		<ConfigSwitch section="downloads" keyword="on_demand_par2" label="On-demand par2" value={configData.downloads.on_demand_par2} description="Only download par2 recovery volumes if the download needs repair. The par2 index is always fetched. Saves bandwidth on intact downloads." onupdate={onFieldUpdate} />
		<Separator class="my-4" />
		<div>
			<h4 class="text-sm font-medium">Naming & Cleanup</h4>
			<p class="text-xs text-muted-foreground mb-4">Control how folders and files are named on disk.</p>
		</div>
		<ConfigInput section="downloads" keyword="replace_illegal_with" label="Replace Illegal Characters With" value={configData.downloads.replace_illegal_with} description="String used to replace invalid filesystem characters (default: _)." onupdate={onFieldUpdate} />
		<ConfigInput section="downloads" keyword="replace_spaces_with" label="Replace Spaces With" value={configData.downloads.replace_spaces_with} description="String used to replace spaces in names (e.g. _ or .)." onupdate={onFieldUpdate} />
		<ConfigSwitch section="downloads" keyword="strip_diacritics" label="Strip Diacritics" value={configData.downloads.strip_diacritics} description="Replace accented characters with ASCII (e.g. é -> e)." onupdate={onFieldUpdate} />
		<ConfigTextarea section="downloads" keyword="cleanup_list" label="Indexer/Spam Cleanup List" value={configData.downloads.cleanup_list} description="Regex patterns to strip from names (one per line).
		Examples: ^\[PRiVATE\]-? or (?i)-? ?\(Scenzbd\)$" onupdate={onFieldUpdate} />
		<Separator class="my-4" />
		<div>
			<h4 class="text-sm font-medium">Unwanted Extensions</h4>
			<p class="text-xs text-muted-foreground mb-4">Refuse posts that would deliver executables or other file types you never want.</p>
		</div>
		<ConfigSelect section="downloads" keyword="action_on_unwanted_extensions" label="Action on Unwanted Extensions" value={configData.downloads.action_on_unwanted_extensions} options={unwantedActionOptions} description="Checked against the filenames in the NZB when a job is added, and against the names inside RAR5 volumes and par2 files as they download. Pause: the job waits, labelled UNWANTED, until you resume it, which approves it. Fail: the job goes to history as failed; use Retry anyway to approve it. Unless off, unwanted files found after unpack are also deleted from jobs you did not approve." onupdate={onFieldUpdate} />
		<ConfigSelect section="downloads" keyword="unwanted_extensions_mode" label="Unwanted Extensions Mode" value={configData.downloads.unwanted_extensions_mode} options={unwantedModeOptions} description="A filename with no extension is never treated as unwanted." onupdate={onFieldUpdate} />
		<ConfigTextarea section="downloads" keyword="unwanted_extensions" label="Unwanted Extension List" value={configData.downloads.unwanted_extensions ?? []} description="One extension per line, without the dot. Glob patterns such as r[0-9][0-9] match the whole extension. Names inside RAR3, 7z, zip, nested or encrypted-header archives are not read while downloading; the after-unpack removal is the backstop for those." onupdate={onFieldUpdate} />
	</div>
</section>
