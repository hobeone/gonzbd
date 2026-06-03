<script lang="ts">
	import { Dialog } from 'bits-ui';
	import { getShortcuts, type ShortcutDef } from '$lib/shortcuts.svelte';

	let { open = $bindable(false) }: { open?: boolean } = $props();

	let shortcuts = $derived(getShortcuts());

	/**
	 * Format a shortcut key for display.
	 * Maps raw KeyboardEvent.key values to user-friendly labels.
	 */
	function formatKey(def: ShortcutDef): string {
		const parts: string[] = [];

		if (def.mod === 'ctrl') {
			// Show platform-appropriate modifier
			const isMac =
				typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.userAgent);
			parts.push(isMac ? '⌘' : 'Ctrl');
		} else if (def.mod === 'alt') {
			const isMac =
				typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.userAgent);
			parts.push(isMac ? '⌥' : 'Alt');
		} else if (def.mod === 'shift') {
			parts.push('Shift');
		}

		// Map special keys to readable labels
		const keyLabels: Record<string, string> = {
			' ': 'Space',
			Escape: 'Esc',
			ArrowUp: '↑',
			ArrowDown: '↓',
			ArrowLeft: '←',
			ArrowRight: '→',
			Delete: 'Del',
			Backspace: '⌫'
		};
		parts.push(keyLabels[def.key] ?? def.key.toUpperCase());

		return parts.join(' + ');
	}
</script>

<Dialog.Root bind:open>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50 backdrop-blur-xs" />
		<Dialog.Content
			class="fixed left-1/2 top-1/2 z-50 w-full max-w-sm -translate-x-1/2 -translate-y-1/2 rounded-3xl border border-m3-outline/20 bg-m3-surface text-m3-on-surface shadow-m3-3 outline-none"
		>
			<div class="border-b border-m3-outline/10 px-6 py-4">
				<Dialog.Title class="flex items-center gap-2.5 text-lg font-semibold tracking-tight">
					<span class="material-symbols-outlined text-m3-primary text-xl">keyboard</span>
					Keyboard Shortcuts
				</Dialog.Title>
			</div>

			<div class="px-6 py-4">
				<table class="w-full">
					<tbody>
						{#each shortcuts as shortcut (shortcut.key + (shortcut.mod ?? ''))}
							<tr class="group">
								<td class="py-1.5 pr-4">
									<kbd
										class="inline-flex min-w-[2rem] items-center justify-center rounded-lg border border-m3-outline bg-m3-surface-variant px-2 py-0.5 font-mono text-xs font-semibold text-m3-on-surface-variant"
									>
										{formatKey(shortcut)}
									</kbd>
								</td>
								<td
									class="py-1.5 text-sm text-m3-on-surface-variant"
								>
									{shortcut.description}
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>

			<div
				class="border-t border-m3-outline/10 px-6 py-4 text-center text-xs text-m3-on-surface-variant/70 font-medium"
			>
				Press <kbd
					class="rounded-md border border-m3-outline bg-m3-surface-variant px-1.5 py-0.5 font-mono text-[10px]"
					>?</kbd
				>
				or
				<kbd
					class="rounded-md border border-m3-outline bg-m3-surface-variant px-1.5 py-0.5 font-mono text-[10px]"
					>Esc</kbd
				> to close
			</div>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
