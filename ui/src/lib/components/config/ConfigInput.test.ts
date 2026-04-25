import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi } from 'vitest';
import ConfigInput from './ConfigInput.svelte';

describe('ConfigInput', () => {
	it('renders label', () => {
		render(ConfigInput, {
			section: 'general',
			keyword: 'host',
			value: '0.0.0.0',
			label: 'Host'
		});
		expect(screen.getByLabelText('Host')).toBeInTheDocument();
	});

	it('renders description when provided', () => {
		render(ConfigInput, {
			section: 'general',
			keyword: 'host',
			value: '0.0.0.0',
			label: 'Host',
			description: 'Bind address'
		});
		expect(screen.getByText('Bind address')).toBeInTheDocument();
	});

	it('does not render description when not provided', () => {
		const { container } = render(ConfigInput, {
			section: 'general',
			keyword: 'host',
			value: '0.0.0.0',
			label: 'Host'
		});
		expect(container.querySelector('.text-muted-foreground')).toBeNull();
	});

	it('debounces input by 500ms before calling onupdate', async () => {
		vi.useFakeTimers();
		const onupdate = vi.fn();

		render(ConfigInput, {
			section: 'general',
			keyword: 'host',
			value: '0.0.0.0',
			label: 'Host',
			onupdate
		});

		const input = screen.getByLabelText('Host');
		await fireEvent.input(input, { target: { value: '127.0.0.1' } });

		// Should not be called immediately
		expect(onupdate).not.toHaveBeenCalled();

		// Should be called after 500ms
		await vi.advanceTimersByTimeAsync(500);
		expect(onupdate).toHaveBeenCalledWith('general', 'host', '127.0.0.1');

		vi.useRealTimers();
	});

	it('converts to number for type="number"', async () => {
		vi.useFakeTimers();
		const onupdate = vi.fn();

		render(ConfigInput, {
			section: 'general',
			keyword: 'port',
			value: 8080,
			label: 'Port',
			type: 'number',
			onupdate
		});

		const input = screen.getByLabelText('Port');
		await fireEvent.input(input, { target: { value: '9090' } });
		await vi.advanceTimersByTimeAsync(500);

		expect(onupdate).toHaveBeenCalledWith('general', 'port', 9090);

		vi.useRealTimers();
	});

	it('commits changes immediately on blur', async () => {
		const onupdate = vi.fn();

		render(ConfigInput, {
			section: 'general',
			keyword: 'host',
			value: '0.0.0.0',
			label: 'Host',
			onupdate
		});

		const input = screen.getByLabelText('Host');
		await fireEvent.input(input, { target: { value: '127.0.0.1' } });

		// Should not be called immediately on input
		expect(onupdate).not.toHaveBeenCalled();

		// Should be called immediately on blur
		await fireEvent.blur(input);
		expect(onupdate).toHaveBeenCalledWith('general', 'host', '127.0.0.1');
	});
});
