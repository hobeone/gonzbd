import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi } from 'vitest';
import DownloadsSection from './DownloadsSection.svelte';

vi.mock('$lib/components/ui/separator', () => ({
	Separator: function MockSeparator() {}
}));

describe('DownloadsSection', () => {
	const mockConfig = {
		downloads: {
			bandwidth_max: '10M',
			min_free_space: '1G',
			write_cache_size: '500M',
			max_art_tries: 3,
			max_active_jobs: 4,
			top_only: false,
			pre_check: true,
			on_demand_par2: true,
			replace_illegal_with: '_',
			replace_spaces_with: '.',
			strip_diacritics: true,
			cleanup_list: ['^abc', 'xyz$']
		}
	};

	it('renders the section heading', () => {
		render(DownloadsSection, { configData: mockConfig, onFieldUpdate: vi.fn() });
		expect(screen.getByText('Download Settings')).toBeInTheDocument();
	});

	it('renders all config fields', () => {
		render(DownloadsSection, { configData: mockConfig, onFieldUpdate: vi.fn() });
		expect(screen.getByText('Maximum Bandwidth')).toBeInTheDocument();
		expect(screen.getByText('Minimum Free Space')).toBeInTheDocument();
		expect(screen.getByText('Article Cache')).toBeInTheDocument();
		expect(screen.getByText('Article Retries')).toBeInTheDocument();
		expect(screen.getByText('Maximum Active Jobs')).toBeInTheDocument();
		expect(screen.getByText('Top-only server mode')).toBeInTheDocument();
		expect(screen.getByText('Pre-check article availability')).toBeInTheDocument();
		expect(screen.getByText('On-demand par2')).toBeInTheDocument();
		expect(screen.getByText('Naming & Cleanup')).toBeInTheDocument();
		expect(screen.getByText('Replace Illegal Characters With')).toBeInTheDocument();
		expect(screen.getByText('Replace Spaces With')).toBeInTheDocument();
		expect(screen.getByText('Strip Diacritics')).toBeInTheDocument();
		expect(screen.getByText('Indexer/Spam Cleanup List')).toBeInTheDocument();
	});

	// Keyword-contract tests: assert every ConfigInput/ConfigSwitch passes the
	// correct section + keyword to onFieldUpdate. This is the class of bug that
	// caused 'invalid keyword article_cache_size in section downloads' — the
	// UI sent a keyword that didn't match the backend's json struct tag.
	//
	// For ConfigInput fields: fire an oninput event with a new value, then
	// onblur to trigger commit() immediately (bypasses the 500ms debounce).
	// For ConfigSwitch fields: fire onchange on the checkbox.
	//
	// Each case only checks section + keyword, not the value — the goal is
	// to catch name mismatches, not to retest ConfigInput/ConfigSwitch
	// internals.
	describe('keyword contracts', () => {
		const cases: Array<{
			keyword: string;
			inputId: string;
			newValue: string;
			isSwitch?: boolean;
		}> = [
			{ keyword: 'bandwidth_max',        inputId: 'downloads-bandwidth_max',        newValue: '20M'  },
			{ keyword: 'min_free_space',        inputId: 'downloads-min_free_space',        newValue: '2G'   },
			{ keyword: 'write_cache_size',      inputId: 'downloads-write_cache_size',      newValue: '750M' },
			{ keyword: 'max_art_tries',         inputId: 'downloads-max_art_tries',         newValue: '7'    },
			{ keyword: 'max_active_jobs',       inputId: 'downloads-max_active_jobs',       newValue: '8'    },
			{ keyword: 'top_only',               inputId: 'downloads-top_only',               newValue: '',    isSwitch: true },
			{ keyword: 'pre_check',             inputId: 'downloads-pre_check',             newValue: '',    isSwitch: true },
			{ keyword: 'on_demand_par2',        inputId: 'downloads-on_demand_par2',        newValue: '',    isSwitch: true },
			{ keyword: 'replace_illegal_with',  inputId: 'downloads-replace_illegal_with',  newValue: '-'   },
			{ keyword: 'replace_spaces_with',   inputId: 'downloads-replace_spaces_with',   newValue: '_'   },
			{ keyword: 'strip_diacritics',      inputId: 'downloads-strip_diacritics',      newValue: '',    isSwitch: true },
		];

		cases.forEach(({ keyword, inputId, newValue, isSwitch }) => {
			it(`fires section="downloads" keyword="${keyword}" on change`, async () => {
				const onFieldUpdate = vi.fn();
				const { container } = render(DownloadsSection, { configData: mockConfig, onFieldUpdate });

				const el = container.querySelector(`#${inputId}`) as HTMLElement;
				expect(el).not.toBeNull();

				if (isSwitch) {
					await fireEvent.change(el, { target: { checked: false } });
				} else {
					await fireEvent.input(el, { target: { value: newValue } });
					await fireEvent.blur(el);
				}

				expect(onFieldUpdate).toHaveBeenCalled();
				const [section, kw] = onFieldUpdate.mock.calls[0];
				expect(section).toBe('downloads');
				expect(kw).toBe(keyword);
			});
		});
	});
});
