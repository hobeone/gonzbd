import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import QueueRow from './QueueRow.svelte';
import type { QueueSlot } from '$lib/types';

vi.mock('$lib/stores/queue.svelte', () => ({
	pauseJob: vi.fn().mockResolvedValue(undefined),
	resumeJob: vi.fn().mockResolvedValue(undefined)
}));

vi.mock('$lib/api', () => ({
	fetchQueueJobDetail: vi.fn()
}));

// Captures the handler passed to subscribeWS so tests can simulate
// queue_updated events that drive drawer refreshes.
let capturedWSHandler: ((event: any) => void) | null = null;
vi.mock('$lib/stores/websocket.svelte', () => ({
	subscribeWS: vi.fn((handler: (event: any) => void) => {
		capturedWSHandler = handler;
		return () => {
			capturedWSHandler = null;
		};
	})
}));

import { pauseJob, resumeJob } from '$lib/stores/queue.svelte';
import { fetchQueueJobDetail } from '$lib/api';

describe('QueueRow', () => {
	const baseSlot: QueueSlot = {
		index: 0,
		nzo_id: '123',
		name: 'Test.NZB',
		filename: 'Test.NZB',
		cat: 'TV',
		priority: 'Normal',
		status: 'Downloading',
		size: '100 MB',
		sizeleft: '50 MB',
		percentage: '50.5',
		remaining_bytes: 52428800,
		bytes: 104857600,
		mb: 100,
		mbleft: 50,
		pp: '3',
		script: 'none',
		password: '',
		failed_bytes: 0,
		content_failed_bytes: 0,
		recovery_bytes: 0,
		recovery_files: 0,
		current_stage: 'download',
		articles_remaining: 100,
		eta_seconds: 0,
		current_file: ''
	};

	beforeEach(() => vi.clearAllMocks());

	it('renders progress bar and percentage', () => {
		render(QueueRow, { slot: baseSlot, onremove: () => {} });
		
		// Percentage text
		expect(screen.getByText('50.5%')).toBeInTheDocument();
		
		// Progress bar (Progress renders a plain div with role="progressbar")
		const progress = screen.getByRole('progressbar');
		expect(progress).toBeInTheDocument();
		expect(progress.getAttribute('aria-valuenow')).toBe('50.5');
	});

	it('applies pulse animation for active jobs', () => {
		const { container } = render(QueueRow, { slot: baseSlot, onremove: () => {} });
		
		// Find progress element
		const progress = container.querySelector('[data-slot="progress"]');
		expect(progress?.className).toContain('animate-pulse');
	});

	it('does not apply pulse animation for paused jobs', () => {
		const pausedSlot = { ...baseSlot, status: 'Paused' };
		const { container } = render(QueueRow, { slot: pausedSlot, onremove: () => {} });
		
		const progress = container.querySelector('[data-slot="progress"]');
		expect(progress?.className).not.toContain('animate-pulse');
	});

	it('does not apply pulse animation for queued jobs', () => {
		const queuedSlot = { ...baseSlot, status: 'Queued' };
		const { container } = render(QueueRow, { slot: queuedSlot, onremove: () => {} });
		
		const progress = container.querySelector('[data-slot="progress"]');
		expect(progress?.className).not.toContain('animate-pulse');
	});

	it('does not apply pulse animation for idle jobs', () => {
		const idleSlot = { ...baseSlot, status: 'Idle' };
		const { container } = render(QueueRow, { slot: idleSlot, onremove: () => {} });

		const progress = container.querySelector('[data-slot="progress"]');
		expect(progress?.className).not.toContain('animate-pulse');
	});

	it('renders job name', () => {
		const { container } = render(QueueRow, { slot: baseSlot, onremove: () => {} });
		expect(container.textContent).toContain('Test.NZB');
	});

	it('renders category', () => {
		const { container } = render(QueueRow, { slot: baseSlot, onremove: () => {} });
		expect(container.textContent).toContain('TV');
	});

	it('shows * for empty category', () => {
		const slot = { ...baseSlot, cat: '' };
		const { container } = render(QueueRow, { slot, onremove: () => {} });
		expect(container.textContent).toContain('*');
	});

	it('renders size', () => {
		const { container } = render(QueueRow, { slot: baseSlot, onremove: () => {} });
		expect(container.textContent).toContain('100 MB');
	});

	it('renders size left', () => {
		const { container } = render(QueueRow, { slot: baseSlot, onremove: () => {} });
		expect(container.textContent).toContain('50 MB');
	});

	it('delete button triggers onremove callback', async () => {
		const onremove = vi.fn();
		render(QueueRow, { slot: baseSlot, onremove });

		const deleteBtn = screen.getByTitle('Delete');
		await fireEvent.click(deleteBtn);

		expect(onremove).toHaveBeenCalled();
	});

	it('shows pause button title for active jobs', () => {
		render(QueueRow, { slot: baseSlot, onremove: () => {} });
		expect(screen.getByTitle('Pause')).toBeInTheDocument();
	});

	it('shows resume button title for paused jobs', () => {
		const pausedSlot = { ...baseSlot, status: 'Paused' };
		render(QueueRow, { slot: pausedSlot, onremove: () => {} });
		expect(screen.getByTitle('Resume')).toBeInTheDocument();
	});

	// ── togglePause ──

	it('clicking pause button calls pauseJob for active jobs', async () => {
		render(QueueRow, { slot: baseSlot, onremove: vi.fn() });

		const pauseBtn = screen.getByTitle('Pause');
		await fireEvent.click(pauseBtn);

		expect(pauseJob).toHaveBeenCalledWith('123');
		expect(resumeJob).not.toHaveBeenCalled();
	});

	it('clicking resume button calls resumeJob for paused jobs', async () => {
		const pausedSlot = { ...baseSlot, status: 'Paused' };
		render(QueueRow, { slot: pausedSlot, onremove: vi.fn() });

		const resumeBtn = screen.getByTitle('Resume');
		await fireEvent.click(resumeBtn);

		expect(resumeJob).toHaveBeenCalledWith('123');
		expect(pauseJob).not.toHaveBeenCalled();
	});

	// ── Warning display ──

	it('shows warning icon and text when slot.warning is set', () => {
		const warnSlot = { ...baseSlot, warning: 'Missing articles' };
		const { container } = render(QueueRow, { slot: warnSlot, onremove: vi.fn() });
		expect(container.textContent).toContain('Missing articles');
	});

	it('does not show warning icon when slot.warning is empty', () => {
		const { container } = render(QueueRow, { slot: baseSlot, onremove: vi.fn() });
		// No warning text or icon.
		expect(container.querySelector('.text-amber-600')).not.toBeInTheDocument();
	});

	// ── Fallback to filename ──

	it('uses filename when name is empty', () => {
		const slot = { ...baseSlot, name: '', filename: 'fallback.nzb' };
		const { container } = render(QueueRow, { slot, onremove: vi.fn() });
		expect(container.textContent).toContain('fallback.nzb');
	});

	// ── Drawer per-file table ──

	/**
	 * Regression test for the Svelte each_key_duplicate runtime error
	 * we hit in production: NZBs frequently contain multiple files with
	 * the same sanitized subject (e.g. main + sample with the same
	 * release name, or par2 blocks all sharing a single subject after
	 * sanitization). The drawer's keyed each block must use a stable
	 * identity that doesn't collide on duplicate names.
	 */
	it('drawer renders multiple files with duplicate names without each_key_duplicate', async () => {
		const dupName = 'Transfixed.25.04.26.Ariel.Demure.Getting.His.Attention.XXX.1080p.MP4-Narcos_[1a57ee682';
		vi.mocked(fetchQueueJobDetail).mockResolvedValue({
			status: true,
			queue: {
				slots: [
					{
						...baseSlot,
						files: [
							{ name: dupName, bytes: 1000, bytes_downloaded: 500, state: 'downloading' },
							{ name: dupName, bytes: 2000, bytes_downloaded: 0, state: 'queued' },
							{ name: dupName, bytes: 1500, bytes_downloaded: 1500, state: 'done' }
						]
					}
				]
			}
		} as any);

		// Capture any console.error from Svelte; if the keyed-each
		// machinery throws a duplicate-key error in dev mode it lands
		// here. The fix must keep this empty.
		const errSpy = vi.spyOn(console, 'error').mockImplementation(() => {});

		const { container } = render(QueueRow, { slot: baseSlot, onremove: () => {} });

		// Click the row to expand the drawer.
		const row = container.querySelector('tr[class*="cursor-pointer"]') as HTMLElement;
		expect(row).toBeTruthy();
		await fireEvent.click(row);

		// Wait for fetchQueueJobDetail to resolve and the drawer to
		// re-render. A microtask flush is enough since we mocked the
		// fetch as immediately-resolved.
		await vi.waitFor(() => {
			expect(fetchQueueJobDetail).toHaveBeenCalledWith('123');
		});
		await vi.waitFor(() => {
			// Three rows for three duplicate-named files; without the
			// fix Svelte either errors out or merges the rows.
			expect(screen.getAllByText(dupName)).toHaveLength(3);
		});

		// No each_key_duplicate (or any other Svelte error) was logged.
		const svelteErrors = errSpy.mock.calls.flat().filter(
			(arg) => typeof arg === 'string' && arg.includes('each_key_duplicate')
		);
		expect(svelteErrors).toEqual([]);
		errSpy.mockRestore();
	});

	/**
	 * Refresh-jank fix coverage: when a WS queue_updated event arrives
	 * and only some files' bytes_downloaded changed, the drawer should
	 * (a) call the detail endpoint at most once per FILES_REFRESH_MS
	 *     (currently 2 s) — proving the throttle still works, and
	 * (b) update the changed cell text without unmounting the row's
	 *     <tr> DOM node — proving the array is mutated in place rather
	 *     than wholesale replaced (keyed-each-by-index would reuse the
	 *     <tr> either way, but in-place update also avoids re-running
	 *     every cell expression on stable rows).
	 */
	it('drawer applies in-place updates without remounting unchanged rows', async () => {
		vi.useFakeTimers();
		try {
			const initialFiles = [
				{ name: 'a.mkv', bytes: 1000, bytes_downloaded: 100, state: 'downloading' as const },
				{ name: 'b.mkv', bytes: 1000, bytes_downloaded: 1000, state: 'done' as const }
			];
			vi.mocked(fetchQueueJobDetail).mockResolvedValue({
				status: true,
				queue: { slots: [{ ...baseSlot, files: initialFiles }] }
			} as any);

			const { container } = render(QueueRow, { slot: baseSlot, onremove: () => {} });

			const row = container.querySelector('tr[class*="cursor-pointer"]') as HTMLElement;
			await fireEvent.click(row);

			await vi.waitFor(() => {
				expect(fetchQueueJobDetail).toHaveBeenCalledTimes(1);
			});
			await vi.waitFor(() => {
				expect(screen.getByText('a.mkv')).toBeInTheDocument();
			});

			// Capture the file row DOM node BEFORE the refresh.
			const aRowBefore = screen.getByText('a.mkv').closest('tr');
			expect(aRowBefore).toBeTruthy();

			// Updated payload: only bytes_downloaded on file[0] moves.
			vi.mocked(fetchQueueJobDetail).mockResolvedValue({
				status: true,
				queue: {
					slots: [
						{
							...baseSlot,
							files: [
								{ name: 'a.mkv', bytes: 1000, bytes_downloaded: 500, state: 'downloading' as const },
								{ name: 'b.mkv', bytes: 1000, bytes_downloaded: 1000, state: 'done' as const }
							]
						}
					]
				}
			} as any);

			// Fire a burst of queue_updated events — the throttle must
			// coalesce them into one trailing fetch after 2 s.
			expect(capturedWSHandler).not.toBeNull();
			for (let i = 0; i < 5; i++) {
				capturedWSHandler!({ event: 'queue_updated' });
			}

			// Below the throttle window: still only the initial fetch.
			await vi.advanceTimersByTimeAsync(500);
			expect(fetchQueueJobDetail).toHaveBeenCalledTimes(1);

			// Cross the throttle window: exactly one additional fetch.
			await vi.advanceTimersByTimeAsync(2000);
			await vi.waitFor(() => {
				expect(fetchQueueJobDetail).toHaveBeenCalledTimes(2);
			});

			// The new bytes_downloaded value reaches the DOM.
			await vi.waitFor(() => {
				// Cell text uses formatSize: 500 bytes → "500 B".
				expect(screen.getByText('500 B')).toBeInTheDocument();
			});

			// And the <tr> DOM node for the unchanged row is the same
			// element reference: in-place mutation preserves identity.
			const aRowAfter = screen.getByText('a.mkv').closest('tr');
			expect(aRowAfter).toBe(aRowBefore);
		} finally {
			vi.useRealTimers();
		}
	});

	/**
	 * Production reproducer for "drawer flashes on every update": when
	 * the parent QueueTable re-fetches the queue (1 Hz) it passes a new
	 * `slot` reference to QueueRow, even when the underlying nzo_id is
	 * unchanged. The drawer's $effect must NOT re-fire its cleanup on
	 * slot-prop churn — doing so wipes the cached files array and the
	 * template briefly renders "Loading file list…" before the next
	 * fetch resolves.
	 *
	 * Asserts (a) only the throttled refresh fires fetchQueueJobDetail
	 * (slot replacements alone must not trigger fetches), and (b) the
	 * "Loading file list…" placeholder never re-appears once the table
	 * has rendered for the first time.
	 */
	it('drawer does not tear down when parent passes a fresh slot reference', async () => {
		vi.useFakeTimers();
		try {
			vi.mocked(fetchQueueJobDetail).mockResolvedValue({
				status: true,
				queue: { slots: [{ ...baseSlot, files: [
					{ name: 'a.mkv', bytes: 1000, bytes_downloaded: 100, state: 'downloading' as const }
				] }] }
			} as any);

			const { container, rerender } = render(QueueRow, {
				slot: baseSlot,
				onremove: () => {}
			});
			const row = container.querySelector('tr[class*="cursor-pointer"]') as HTMLElement;
			await fireEvent.click(row);

			await vi.waitFor(() => expect(fetchQueueJobDetail).toHaveBeenCalledTimes(1));
			await vi.waitFor(() => expect(screen.getByText('a.mkv')).toBeInTheDocument());

			// Simulate three queue-store polls at 1 Hz: each rerenders
			// QueueRow with a *new slot object* (same nzo_id) just like
			// the parent's queue.poll() does. No WS event fired, so no
			// drawer refresh is expected.
			for (let tick = 0; tick < 3; tick++) {
				const fresh: QueueSlot = {
					...baseSlot,
					remaining_bytes: baseSlot.remaining_bytes - tick * 1000,
					eta_seconds: 60 - tick
				};
				await rerender({ slot: fresh, onremove: () => {} });
				await vi.advanceTimersByTimeAsync(1000);

				// The table row must still be rendered — the drawer
				// must not have flashed back to the loading placeholder.
				expect(screen.queryByText('Loading file list…')).toBeNull();
				expect(screen.getByText('a.mkv')).toBeInTheDocument();
			}

			// Slot churn must not have triggered any extra fetches.
			expect(fetchQueueJobDetail).toHaveBeenCalledTimes(1);
		} finally {
			vi.useRealTimers();
		}
	});

	/**
	 * Length change between fetches (e.g. NZB metadata corrected) must
	 * fall back to wholesale replacement; otherwise the in-place loop
	 * would skip the new entries.
	 */
	it('drawer handles file-list length change by replacing the array', async () => {
		vi.useFakeTimers();
		try {
			vi.mocked(fetchQueueJobDetail).mockResolvedValue({
				status: true,
				queue: { slots: [{ ...baseSlot, files: [
					{ name: 'a.mkv', bytes: 1000, bytes_downloaded: 0, state: 'queued' as const }
				] }] }
			} as any);

			const { container } = render(QueueRow, { slot: baseSlot, onremove: () => {} });
			const row = container.querySelector('tr[class*="cursor-pointer"]') as HTMLElement;
			await fireEvent.click(row);
			await vi.waitFor(() => expect(fetchQueueJobDetail).toHaveBeenCalledTimes(1));
			await vi.waitFor(() => expect(screen.getByText('a.mkv')).toBeInTheDocument());

			// Refresh returns three files — length grew.
			vi.mocked(fetchQueueJobDetail).mockResolvedValue({
				status: true,
				queue: { slots: [{ ...baseSlot, files: [
					{ name: 'a.mkv', bytes: 1000, bytes_downloaded: 0, state: 'queued' as const },
					{ name: 'b.mkv', bytes: 1000, bytes_downloaded: 0, state: 'queued' as const },
					{ name: 'c.mkv', bytes: 1000, bytes_downloaded: 0, state: 'queued' as const }
				] }] }
			} as any);

			capturedWSHandler!({ event: 'queue_updated' });
			await vi.advanceTimersByTimeAsync(2500);
			await vi.waitFor(() => expect(fetchQueueJobDetail).toHaveBeenCalledTimes(2));
			await vi.waitFor(() => {
				expect(screen.getByText('b.mkv')).toBeInTheDocument();
				expect(screen.getByText('c.mkv')).toBeInTheDocument();
			});
		} finally {
			vi.useRealTimers();
		}
	});

	it('renders direct unpack status and progress bar when active', async () => {
		const activeSlot: QueueSlot = {
			...baseSlot,
			direct_unpack: {
				active: true,
				current_set: 'test_set',
				completed_volumes: 3,
				total_volumes: 10,
				success_sets: ['set_a'],
				failed_sets: ['set_b']
			}
		};

		const { container } = render(QueueRow, { slot: activeSlot, onremove: () => {} });

		const row = container.querySelector('tr[class*="cursor-pointer"]') as HTMLElement;
		expect(row).toBeTruthy();
		await fireEvent.click(row);

		expect(screen.getByText('Extracting: test_set')).toBeInTheDocument();
		expect(screen.getByText('(3/10 vols)')).toBeInTheDocument();
		expect(screen.getByText('30%')).toBeInTheDocument();

		const progress = screen.getAllByRole('progressbar');
		const duProgress = progress.find(p => p.getAttribute('aria-valuenow') === '30' || p.getAttribute('aria-valuenow') === '3');
		expect(duProgress).toBeTruthy();
	});

	it('renders direct unpack finished status when inactive', async () => {
		const finishedSlot: QueueSlot = {
			...baseSlot,
			direct_unpack: {
				active: false,
				success_sets: ['set_a', 'set_b'],
				failed_sets: ['set_c'],
				failed_reasons: {
					'set_c': 'volume set_c.part01.rar had failed/missing download articles'
				}
			}
		};

		const { container } = render(QueueRow, { slot: finishedSlot, onremove: () => {} });

		const row = container.querySelector('tr[class*="cursor-pointer"]') as HTMLElement;
		expect(row).toBeTruthy();
		await fireEvent.click(row);

		expect(screen.getByText(/Unpack failed: volume had failed or missing download blocks/)).toBeInTheDocument();
		expect(screen.getByText('(2 OK, 1 Failed)')).toBeInTheDocument();
	});

	/**
	 * #325: the drawer lists every file in the NZB at full declared size,
	 * but the row's size excludes par2 recovery volumes the job does not
	 * expect to fetch. Without stating the excluded amount the drawer simply
	 * totals more than the row above it and nothing explains the gap.
	 */
	describe('held-back subtotal', () => {
		const withFiles = (files: unknown[]) =>
			vi.mocked(fetchQueueJobDetail).mockResolvedValue({
				status: true,
				queue: { slots: [{ ...baseSlot, files }] }
			} as any);

		const openDrawer = async () => {
			const { container } = render(QueueRow, { slot: baseSlot, onremove: () => {} });
			const row = container.querySelector('tr[class*="cursor-pointer"]') as HTMLElement;
			expect(row).toBeTruthy();
			await fireEvent.click(row);
			await vi.waitFor(() => {
				expect(fetchQueueJobDetail).toHaveBeenCalledWith('123');
			});
		};

		it('states the bytes excluded from the row, summing held and skipped', async () => {
			withFiles([
				{ name: 'release.rar', bytes: 1000, bytes_downloaded: 1000, state: 'done' },
				{ name: 'x.vol000+01.par2', bytes: 300, bytes_downloaded: 0, state: 'held' },
				{ name: 'x.vol001+02.par2', bytes: 500, bytes_downloaded: 0, state: 'skipped' }
			]);
			await openDrawer();

			// 300 held + 500 skipped. Both are excluded from the row's size,
			// so both belong in the subtotal — counting only 'held' would
			// under-report a job whose volumes were ruled unnecessary.
			await vi.waitFor(() => {
				expect(screen.getByText(/incl\. .* held back/)).toBeInTheDocument();
			});
			expect(screen.getByText(/incl\. 800(\.0)? ?B held back/)).toBeInTheDocument();
		});

		it('says nothing when no file is held back', async () => {
			withFiles([
				{ name: 'release.rar', bytes: 1000, bytes_downloaded: 1000, state: 'done' },
				{ name: 'other.rar', bytes: 500, bytes_downloaded: 0, state: 'queued' }
			]);
			await openDrawer();

			await vi.waitFor(() => {
				expect(screen.getByText('other.rar')).toBeInTheDocument();
			});
			// A job with nothing held back reconciles already; the note would
			// be noise on every ordinary job in the queue.
			expect(screen.queryByText(/held back/)).not.toBeInTheDocument();
		});
	});

	/**
	 * The drawer's Health cell is the third consumer of the same comparison
	 * the two backend abort gates make. It must not reach a verdict those
	 * gates decline to reach.
	 *
	 * Two ways it used to diverge: it weighed *total* failed bytes (which
	 * counts a failed par2 file as damage) against a capacity figure that
	 * excludes the par2 index, and it read zero capacity as proof of no
	 * repair data even when the job carries a par2 file that simply was not
	 * named as a volume.
	 */
	describe('health verdict', () => {
		const healthOf = async (overrides: Partial<QueueSlot>) => {
			vi.mocked(fetchQueueJobDetail).mockResolvedValue({
				status: true,
				queue: { slots: [{ ...baseSlot, ...overrides }] }
			} as any);
			const slot = { ...baseSlot, ...overrides };
			const { container } = render(QueueRow, { slot, onremove: () => {} });
			const row = container.querySelector('tr[class*="cursor-pointer"]') as HTMLElement;
			await fireEvent.click(row);
			const label = await screen.findByText('Health');
			return label.parentElement?.querySelector('div') as HTMLElement;
		};

		it('calls content intact healthy when only a par2 file failed', async () => {
			// Index present, no recovery volumes, the index's own article
			// failed. Both abort gates let this job proceed.
			const health = await healthOf({
				failed_bytes: 50,
				content_failed_bytes: 0,
				recovery_bytes: 0,
				recovery_capacity_unknown: true
			});
			expect(health.textContent).toBe('Healthy');
			expect(health.className).not.toMatch(/text-red/);
		});

		it('withholds a verdict when capacity is unknown rather than absent', async () => {
			// A plainly-named par2 file: the PAR2 spec only recommends the
			// .volNNN+MM convention, so this may hold recovery slices the
			// classification cannot see.
			const health = await healthOf({
				failed_bytes: 200,
				content_failed_bytes: 200,
				recovery_bytes: 0,
				recovery_capacity_unknown: true
			});
			expect(health.textContent).toBe('Repair data unknown');
			expect(health.className).not.toMatch(/text-red/);
		});

		it('still condemns a job carrying no par2 at all', async () => {
			const health = await healthOf({
				failed_bytes: 200,
				content_failed_bytes: 200,
				recovery_bytes: 0
			});
			expect(health.textContent).toBe('No repair data');
			expect(health.className).toMatch(/text-red/);
		});

		it('compares content damage against recognized capacity', async () => {
			const repairable = await healthOf({
				failed_bytes: 500,
				content_failed_bytes: 200,
				recovery_bytes: 300
			});
			// 200 B of damaged content against 300 B of volumes. Weighing the
			// 500 B total instead would condemn it.
			expect(repairable.textContent).toBe('Repairable');
		});

		it('reports beyond repair when content damage exceeds capacity', async () => {
			const beyond = await healthOf({
				failed_bytes: 400,
				content_failed_bytes: 400,
				recovery_bytes: 300
			});
			expect(beyond.textContent).toBe('Beyond repair');
			expect(beyond.className).toMatch(/text-red/);
		});
	});
});

