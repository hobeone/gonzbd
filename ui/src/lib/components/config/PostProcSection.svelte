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
		<h3 class="text-lg font-medium">Post-Processing</h3>
		<p class="text-sm text-muted-foreground">Archive extraction and par2 repair behavior.</p>
	</div>
	<Separator />
	<div class="divide-y divide-gray-100">
		<ConfigSwitch section="postproc" keyword="enable_unrar" label="Enable RAR extraction" value={configData.postproc.enable_unrar} onupdate={onFieldUpdate} />
		<ConfigSwitch section="postproc" keyword="enable_7zip" label="Enable 7-Zip extraction" value={configData.postproc.enable_7zip} onupdate={onFieldUpdate} />
		<ConfigSwitch section="postproc" keyword="direct_unpack" label="Direct Unpack" value={configData.postproc.direct_unpack} description="Extract files while still downloading." onupdate={onFieldUpdate} />
		<ConfigSwitch section="postproc" keyword="enable_par_cleanup" label="Cleanup par2 files" value={configData.postproc.enable_par_cleanup} description="Delete verification files after successful repair." onupdate={onFieldUpdate} />
		<ConfigInput section="postproc" keyword="unrar_command" label="UnRAR path" value={configData.postproc.unrar_command} onupdate={onFieldUpdate} />
		<ConfigInput section="postproc" keyword="par2_command" label="par2 path" value={configData.postproc.par2_command} onupdate={onFieldUpdate} />
	</div>
</section>
