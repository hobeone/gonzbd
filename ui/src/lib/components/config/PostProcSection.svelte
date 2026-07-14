<script lang="ts">
	import { Separator } from '$lib/components/ui/separator';
	import ConfigInput from './ConfigInput.svelte';
	import ConfigSwitch from './ConfigSwitch.svelte';
	import ConfigTextarea from './ConfigTextarea.svelte';

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
		<p class="text-sm text-muted-foreground">Archive extraction, par2 repair, and output behavior.</p>
	</div>
	<Separator />

	<!-- Extraction -->
	<div>
		<h4 class="text-sm font-semibold text-muted-foreground uppercase tracking-wider mb-2">Extraction</h4>
		<div class="divide-y divide-gray-100 dark:divide-gray-800">
			<ConfigSwitch section="postproc" keyword="enable_unrar" label="Enable RAR extraction" value={configData.postproc.enable_unrar} onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="use_go_rar" label="Use built-in RAR extractor" value={configData.postproc.use_go_rar} description="Use the pure-Go RAR extractor instead of the external unrar binary. No external tools required. Disable to fall back to the subprocess unrar command." onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="go_rar_fallback" label="Fallback to unrar on failure" value={configData.postproc.go_rar_fallback} description="If the built-in RAR extractor fails, automatically retry with the external unrar binary. Disable to treat go_unrar failures as final (no subprocess retry)." onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="enable_7zip" label="Enable 7-Zip extraction" value={configData.postproc.enable_7zip} onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="use_go_7z" label="Use built-in 7-Zip extractor" value={configData.postproc.use_go_7z} description="Use the pure-Go 7-Zip extractor instead of the external 7z binary. No external tools required. Disable to fall back to the subprocess 7z command." onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="go_7z_fallback" label="Fallback to 7z on failure" value={configData.postproc.go_7z_fallback} description="If the built-in 7-Zip extractor fails, automatically retry with the external 7z binary. Disable to treat go_7z failures as final (no subprocess retry)." onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="enable_tar" label="Enable tar extraction" value={configData.postproc.enable_tar} description="Extract plain .tar archives (not compressed variants like .tar.gz/.tgz)." onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="enable_filejoin" label="Enable file joining" value={configData.postproc.enable_filejoin} description="Join split files (.001, .002, ...) into a single file." onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="enable_tsjoin" label="Enable TS joining" value={configData.postproc.enable_tsjoin} description="Join split transport stream files (.ts.001, .ts.002, ...)." onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="enable_recursive" label="Recursive unpacking" value={configData.postproc.enable_recursive} description="Re-scan for archives inside extracted files, up to 3 levels deep." onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="direct_unpack" label="Direct Unpack" value={configData.postproc.direct_unpack} description="Extract RAR volumes while still downloading, overlapping I/O for faster completion. Requires 'Enable RAR extraction'." onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="flat_unpack" label="Flat unpack" value={configData.postproc.flat_unpack} description="Ignore archive-internal directory structure; extract all files into the job folder." onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="overwrite_files" label="Overwrite existing files" value={configData.postproc.overwrite_files} description="Allow extraction to overwrite files that already exist in the destination." onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="strict_sandbox" label="Strict OS sandboxing" value={configData.postproc.strict_sandbox} description="Abort external unpacker execution immediately if OS-level sandboxing (bwrap, sandbox-exec, jail) cannot be established. When disabled, falls back to un-sandboxed execution with post-extraction boundary checks. Defaults to off in the official Docker image, since bwrap can't create the namespaces it needs in a normal (non-privileged) container." onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="ignore_unrar_dates" label="Ignore archive dates" value={configData.postproc.ignore_unrar_dates} description="Discard in-archive modification timestamps; use extraction time instead." onupdate={onFieldUpdate} />
		</div>
	</div>

	<Separator />

	<!-- Par2 Repair -->
	<div>
		<h4 class="text-sm font-semibold text-muted-foreground uppercase tracking-wider mb-2">Par2 Repair</h4>
		<div class="divide-y divide-gray-100 dark:divide-gray-800">
			<ConfigSwitch section="postproc" keyword="use_go_par2" label="Use built-in par2 verifier" value={configData.postproc.use_go_par2} description="Use the native par2 engine for verification and repair instead of the external par2 binary. No external tools required. Disable to fall back to the subprocess par2 command." onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="go_par2_fallback" label="Fallback to par2 binary on failure" value={configData.postproc.go_par2_fallback} description="If the built-in par2 verifier fails, automatically retry with the external par2 binary. Disable to treat go_par2 failures as final (no subprocess retry)." onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="enable_quick_check" label="Enable quick CRC verify" value={configData.postproc.enable_quick_check} description="Pre-verify assembled files using CRC32 values from the par2 manifest. When all files pass, the expensive par2 repair subprocess is skipped. Disable to always run the full par2 verify/repair step." onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="par2_turbo" label="Use par2cmdline-turbo" value={configData.postproc.par2_turbo} description="Enable multi-threaded repair when the par2 binary supports it." onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="process_unpacked_par2" label="Process unpacked par2" value={configData.postproc.process_unpacked_par2} description="Use par2 files found inside extracted archives for filename recovery (deobfuscation)." onupdate={onFieldUpdate} />
			<details class="group py-2">
				<summary class="text-xs font-medium text-muted-foreground cursor-pointer hover:text-foreground">
					Advanced PAR2 Safety &amp; Memory Controls
				</summary>
				<div class="mt-2 divide-y divide-gray-100 dark:divide-gray-800 pl-2 border-l-2 border-muted">
					<ConfigInput section="postproc" keyword="par2_max_packet_body_size" label="Max PAR2 Packet Body Size (bytes)" placeholder="67108864" value={configData.postproc.par2_max_packet_body_size} description="Limits contiguous allocation for a single PAR2 packet body (min: 65536 [64 KiB], default: 67108864 [64 MiB]). Prevents OOM on corrupt files." onupdate={onFieldUpdate} />
					<ConfigInput section="postproc" keyword="par2_max_junk_scan_bytes" label="Max PAR2 Junk Scan Distance (bytes)" placeholder="65536" value={configData.postproc.par2_max_junk_scan_bytes} description="Maximum forward search distance when scanning for PAR2 magic headers (min: 1024 [1 KiB], default: 65536 [64 KiB])." onupdate={onFieldUpdate} />
				</div>
			</details>
		</div>
	</div>

	<Separator />

	<!-- Cleanup & Output -->
	<div>
		<h4 class="text-sm font-semibold text-muted-foreground uppercase tracking-wider mb-2">Cleanup &amp; Output</h4>
		<div class="divide-y divide-gray-100 dark:divide-gray-800">
			<ConfigSwitch section="postproc" keyword="enable_par_cleanup" label="Cleanup par2 files" value={configData.postproc.enable_par_cleanup} description="Delete verification files after successful repair." onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="enable_rar_cleanup" label="Cleanup archive files" value={configData.postproc.enable_rar_cleanup} description="Delete source RAR/7z/split files after successful extraction." onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="deobfuscate_filenames" label="Deobfuscate filenames" value={configData.postproc.deobfuscate_filenames} description="Rename obfuscated files (random hex strings, UUIDs) using the job name." onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="ignore_samples" label="Delete sample files" value={configData.postproc.ignore_samples} description="Remove files matching 'sample' or 'proof' patterns after extraction." onupdate={onFieldUpdate} />
			<ConfigSwitch section="postproc" keyword="folder_rename" label="Folder rename (_UNPACK_ prefix)" value={configData.postproc.folder_rename} description="Prefix destination with _UNPACK_ during processing and _FAILED_ on error. Prevents media managers from importing incomplete downloads." onupdate={onFieldUpdate} />
			<ConfigTextarea section="postproc" keyword="cleanup_extensions" label="Cleanup extensions" value={configData.postproc.cleanup_extensions ?? []} description="File extensions to delete after successful unpack (one per line, without leading dot). Common: nfo, txt, url, srr, sfv, nzb." onupdate={onFieldUpdate} />
			<ConfigInput section="postproc" keyword="permissions" label="File permissions" placeholder="e.g. 755 (empty = no chmod)" value={configData.postproc.permissions} description="Octal permission string applied to extracted files. Directories get the full value; files have execute bits stripped." onupdate={onFieldUpdate} />
		</div>
	</div>

	<Separator />

	<!-- External Tool Paths -->
	<div>
		<h4 class="text-sm font-semibold text-muted-foreground uppercase tracking-wider mb-2">External Tools</h4>
		<div class="divide-y divide-gray-100 dark:divide-gray-800">
			<div class="py-3">
				<p class="text-sm text-muted-foreground">
					Leave paths empty to auto-detect programs from the system PATH. Set an explicit path only when the program is installed outside the normal search path.
				</p>
			</div>
			<ConfigInput section="postproc" keyword="par2_command" label="par2 path" placeholder="auto-detect from PATH" value={configData.postproc.par2_command} onupdate={onFieldUpdate} />
			<ConfigInput section="postproc" keyword="unrar_command" label="UnRAR path" placeholder="auto-detect from PATH" value={configData.postproc.unrar_command} onupdate={onFieldUpdate} />
			<ConfigInput section="postproc" keyword="sevenz_command" label="7-Zip path" placeholder="auto-detect from PATH (7zz or 7z)" value={configData.postproc.sevenz_command} onupdate={onFieldUpdate} />
			<ConfigInput section="postproc" keyword="password_file" label="Password file" placeholder="path to passwords.txt" value={configData.postproc.password_file} description="Text file with one password per line, tried during encrypted archive extraction." onupdate={onFieldUpdate} />
		</div>
	</div>

	<Separator />

	<!-- Advanced / Tweaks -->
	<div>
		<h4 class="text-sm font-semibold text-muted-foreground uppercase tracking-wider mb-2">Advanced</h4>
		<div class="divide-y divide-gray-100 dark:divide-gray-800">
			<ConfigSwitch section="postproc" keyword="script_can_fail" label="Script can fail" value={configData.postproc.script_can_fail} description="Treat non-zero script exit codes as warnings instead of job failures." onupdate={onFieldUpdate} />
			<ConfigInput section="postproc" keyword="nice" label="nice arguments" placeholder="e.g. -n 15 (empty = disabled)" value={configData.postproc.nice} description="Prepended to all external tool commands to lower CPU priority." onupdate={onFieldUpdate} />
			<ConfigInput section="postproc" keyword="ionice" label="ionice arguments" placeholder="e.g. -c2 -n4 (empty = disabled)" value={configData.postproc.ionice} description="Prepended to all external tool commands to lower I/O priority." onupdate={onFieldUpdate} />
			<ConfigInput section="postproc" keyword="extra_unrar_params" label="Extra UnRAR parameters" placeholder="e.g. -ri5:100" value={configData.postproc.extra_unrar_params} description="Additional flags for unrar. Only -mlp, -om*, and -ri* are allowed." onupdate={onFieldUpdate} />
			<ConfigInput section="postproc" keyword="extra_par2_params" label="Extra par2 parameters" placeholder="e.g. -m512" value={configData.postproc.extra_par2_params} description="Additional flags for par2 repair invocations." onupdate={onFieldUpdate} />
		</div>
	</div>
</section>
