import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import ConfigTextarea from './ConfigTextarea.svelte';

describe('ConfigTextarea', () => {
	beforeEach(() => {
		vi.useFakeTimers();
	});

	afterEach(() => {
		vi.useRealTimers();
	});

	it('renders label', () => {
		render(ConfigTextarea, {
			props: { section: 'misc', keyword: 'host_whitelist', value: [], label: 'Allowed Hosts' }
		});
		expect(screen.getByText('Allowed Hosts')).toBeInTheDocument();
	});

	it('renders textarea with value from array prop', () => {
		render(ConfigTextarea, {
			props: { section: 'misc', keyword: 'host_whitelist', value: ['host1.com', 'host2.com'], label: 'Hosts' }
		});
		const textarea = screen.getByRole('textbox') as HTMLTextAreaElement;
		expect(textarea.value).toBe('host1.com\nhost2.com');
	});

	it('renders description when provided', () => {
		render(ConfigTextarea, {
			props: {
				section: 'misc', keyword: 'host_whitelist', value: [], label: 'Hosts',
				description: 'One host per line'
			}
		});
		expect(screen.getByText('One host per line')).toBeInTheDocument();
	});

	it('does not render description when not provided', () => {
		const { container } = render(ConfigTextarea, {
			props: { section: 'misc', keyword: 'host_whitelist', value: [], label: 'Hosts' }
		});
		expect(container.querySelector('.text-muted-foreground')).not.toBeInTheDocument();
	});

	it('calls onupdate on blur after typing', async () => {
		const onupdate = vi.fn();
		render(ConfigTextarea, {
			props: {
				section: 'misc', keyword: 'host_whitelist',
				value: ['old.com'], label: 'Hosts', onupdate
			}
		});
		const textarea = screen.getByRole('textbox') as HTMLTextAreaElement;

		// Simulate typing new content.
		await fireEvent.input(textarea, { target: { value: 'new.com\nextra.com' } });
		// Blur commits immediately.
		await fireEvent.blur(textarea);

		expect(onupdate).toHaveBeenCalledWith('misc', 'host_whitelist', '["new.com","extra.com"]');
	});

	it('does not call onupdate when value unchanged', async () => {
		const onupdate = vi.fn();
		render(ConfigTextarea, {
			props: {
				section: 'misc', keyword: 'test',
				value: ['a', 'b'], label: 'Test', onupdate
			}
		});
		const textarea = screen.getByRole('textbox') as HTMLTextAreaElement;

		// Type the same value.
		await fireEvent.input(textarea, { target: { value: 'a\nb' } });
		await fireEvent.blur(textarea);

		expect(onupdate).not.toHaveBeenCalled();
	});

	it('filters blank lines on commit', async () => {
		const onupdate = vi.fn();
		render(ConfigTextarea, {
			props: {
				section: 'misc', keyword: 'test',
				value: [], label: 'Test', onupdate
			}
		});
		const textarea = screen.getByRole('textbox') as HTMLTextAreaElement;

		await fireEvent.input(textarea, { target: { value: 'a\n\n  \nb' } });
		await fireEvent.blur(textarea);

		expect(onupdate).toHaveBeenCalledWith('misc', 'test', '["a","b"]');
	});

	it('debounces input with 1000ms delay', async () => {
		const onupdate = vi.fn();
		render(ConfigTextarea, {
			props: {
				section: 'misc', keyword: 'test',
				value: [], label: 'Test', onupdate
			}
		});
		const textarea = screen.getByRole('textbox') as HTMLTextAreaElement;

		await fireEvent.input(textarea, { target: { value: 'typed' } });

		// Before debounce fires.
		expect(onupdate).not.toHaveBeenCalled();

		// After 1000ms debounce.
		vi.advanceTimersByTime(1000);
		expect(onupdate).toHaveBeenCalledWith('misc', 'test', '["typed"]');
	});

	it('renders empty textarea for empty value array', () => {
		render(ConfigTextarea, {
			props: { section: 'misc', keyword: 'test', value: [], label: 'Empty' }
		});
		const textarea = screen.getByRole('textbox') as HTMLTextAreaElement;
		expect(textarea.value).toBe('');
	});
});
