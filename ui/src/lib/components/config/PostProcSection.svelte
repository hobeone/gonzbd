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
	<div class="divide-y divide-gray-100 dark:divide-gray-800">
		<ConfigSwitch section="postproc" keyword="enable_unrar" label="Enable RAR extraction" value={configData.postproc.enable_unrar} onupdate={onFieldUpdate} />
		<ConfigSwitch section="postproc" keyword="enable_7zip" label="Enable 7-Zip extraction" value={configData.postproc.enable_7zip} onupdate={onFieldUpdate} />
		<ConfigSwitch section="postproc" keyword="prefer_7zip" label="Prefer 7-Zip for RAR" value={configData.postproc.prefer_7zip} description="Use 7z instead of unrar for RAR archives even when unrar is available." onupdate={onFieldUpdate} />
		<ConfigSwitch section="postproc" keyword="direct_unpack" label="Direct Unpack" value={configData.postproc.direct_unpack} description="Extract files while still downloading." onupdate={onFieldUpdate} />
		<ConfigSwitch section="postproc" keyword="enable_par_cleanup" label="Cleanup par2 files" value={configData.postproc.enable_par_cleanup} description="Delete verification files after successful repair." onupdate={onFieldUpdate} />
		<ConfigSwitch section="postproc" keyword="enable_rar_cleanup" label="Cleanup archive files" value={configData.postproc.enable_rar_cleanup} description="Delete source RAR/7z/split files after successful extraction." onupdate={onFieldUpdate} />

		<div class="py-3">
			<p class="text-sm text-muted-foreground">
				Leave paths empty to auto-detect programs from the system PATH. Set an explicit path only when the program is installed outside the normal search path.
			</p>
		</div>

		<ConfigInput section="postproc" keyword="par2_command" label="par2 path" placeholder="auto-detect from PATH" value={configData.postproc.par2_command} onupdate={onFieldUpdate} />
		<ConfigInput section="postproc" keyword="unrar_command" label="UnRAR path" placeholder="auto-detect from PATH" value={configData.postproc.unrar_command} onupdate={onFieldUpdate} />
		<ConfigInput section="postproc" keyword="sevenz_command" label="7-Zip path" placeholder="auto-detect from PATH (7zz or 7z)" value={configData.postproc.sevenz_command} onupdate={onFieldUpdate} />
	</div>
</section>
