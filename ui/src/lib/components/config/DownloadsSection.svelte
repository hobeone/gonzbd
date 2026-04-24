<script lang="ts">
	import { Separator } from '$lib/components/ui/separator';
	import ConfigInput from './ConfigInput.svelte';
	import ConfigSwitch from './ConfigSwitch.svelte';

	let {
		configData,
		onFieldUpdate
	}: {
		configData: Record<string, any>;
		onFieldUpdate: (section: string, keyword: string, value: string | number | boolean) => void;
	} = $props();
</script>

<section class="space-y-6">
	<div>
		<h3 class="text-lg font-medium">Download Settings</h3>
		<p class="text-sm text-muted-foreground">Throttling, disk guards, and retry behavior.</p>
	</div>
	<Separator />
	<div class="divide-y divide-gray-100">
		<ConfigInput section="downloads" keyword="bandwidth_max" label="Maximum Bandwidth" value={configData.downloads.bandwidth_max} description="Absolute ceiling (e.g. 10M, 500K)." onupdate={onFieldUpdate} />
		<ConfigInput section="downloads" keyword="min_free_space" label="Minimum Free Space" value={configData.downloads.min_free_space} description="Pause download if disk space drops below this (e.g. 1G)." onupdate={onFieldUpdate} />
		<ConfigInput section="downloads" keyword="article_cache_size" label="Article Cache" value={configData.downloads.article_cache_size} description="In-memory cache size (e.g. 500M)." onupdate={onFieldUpdate} />
		<ConfigInput section="downloads" keyword="max_art_tries" label="Article Retries" type="number" value={configData.downloads.max_art_tries} description="Max attempts across all servers per article." onupdate={onFieldUpdate} />
		<ConfigSwitch section="downloads" keyword="pre_check" label="Pre-check article availability" value={configData.downloads.pre_check} description="STAT check before download (saves bandwidth)." onupdate={onFieldUpdate} />
	</div>
</section>
