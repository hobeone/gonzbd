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
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50" />
		<Dialog.Content
			class="fixed left-1/2 top-1/2 z-50 w-full max-w-sm -translate-x-1/2 -translate-y-1/2 rounded-lg border bg-white shadow-lg dark:border-gray-700 dark:bg-gray-900"
		>
			<div class="border-b px-5 py-4 dark:border-gray-700">
				<Dialog.Title class="flex items-center gap-2 text-lg font-semibold">
					<svg
						xmlns="http://www.w3.org/2000/svg"
						viewBox="0 0 20 20"
						fill="currentColor"
						class="size-5 text-gray-400"
					>
						<path
							fill-rule="evenodd"
							d="M2 4.75A.75.75 0 0 1 2.75 4h14.5a.75.75 0 0 1 0 1.5H2.75A.75.75 0 0 1 2 4.75ZM2 10a.75.75 0 0 1 .75-.75h14.5a.75.75 0 0 1 0 1.5H2.75A.75.75 0 0 1 2 10Zm0 5.25a.75.75 0 0 1 .75-.75h14.5a.75.75 0 0 1 0 1.5H2.75a.75.75 0 0 1-.75-.75Z"
							clip-rule="evenodd"
						/>
					</svg>
					Keyboard Shortcuts
				</Dialog.Title>
			</div>

			<div class="px-5 py-3">
				<table class="w-full">
					<tbody>
						{#each shortcuts as shortcut (shortcut.key + (shortcut.mod ?? ''))}
							<tr class="group">
								<td class="py-1.5 pr-4">
									<kbd
										class="inline-flex min-w-[2rem] items-center justify-center rounded border border-gray-300 bg-gray-50 px-2 py-0.5 font-mono text-xs font-medium text-gray-700 dark:border-gray-600 dark:bg-gray-800 dark:text-gray-300"
									>
										{formatKey(shortcut)}
									</kbd>
								</td>
								<td
									class="py-1.5 text-sm text-gray-600 dark:text-gray-400"
								>
									{shortcut.description}
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>

			<div
				class="border-t px-5 py-3 text-center text-xs text-gray-400 dark:border-gray-700 dark:text-gray-500"
			>
				Press <kbd
					class="rounded border border-gray-300 bg-gray-50 px-1.5 py-0.5 font-mono text-[10px] dark:border-gray-600 dark:bg-gray-800"
					>?</kbd
				>
				or
				<kbd
					class="rounded border border-gray-300 bg-gray-50 px-1.5 py-0.5 font-mono text-[10px] dark:border-gray-600 dark:bg-gray-800"
					>Esc</kbd
				> to close
			</div>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
