<script lang="ts">
	import { Separator } from '$lib/components/ui/separator';
	import ConfigInput from './ConfigInput.svelte';

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
		<h3 class="text-lg font-medium">General Settings</h3>
		<p class="text-sm text-muted-foreground">Server connectivity and basic daemon tuning.</p>
	</div>
	<Separator />
	<div class="divide-y divide-gray-100">
		<ConfigInput section="general" keyword="host" label="Host" value={configData.general.host} description="Host or IP to bind the HTTP server to." onupdate={onFieldUpdate} />
		<ConfigInput section="general" keyword="port" label="Port" type="number" value={configData.general.port} description="TCP port for the web interface." onupdate={onFieldUpdate} />
		<ConfigInput section="general" keyword="api_key" label="API Key" value={configData.general.api_key} description="Full API authentication key." onupdate={onFieldUpdate} />
		<ConfigInput section="general" keyword="nzb_key" label="NZB Key" value={configData.general.nzb_key} description="Key for NZB uploads only." onupdate={onFieldUpdate} />
		<ConfigInput section="general" keyword="download_dir" label="Download Directory" value={configData.general.download_dir} description="Path for incomplete downloads." onupdate={onFieldUpdate} />
		<ConfigInput section="general" keyword="complete_dir" label="Complete Directory" value={configData.general.complete_dir} description="Path for finished downloads." onupdate={onFieldUpdate} />
		<ConfigInput section="general" keyword="log_level" label="Log Level" value={configData.general.log_level} description="Minimum level for logging (debug, info, warn, error)." onupdate={onFieldUpdate} />
	</div>
</section>
