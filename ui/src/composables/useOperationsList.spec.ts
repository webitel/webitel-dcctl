import { createPinia, setActivePinia } from 'pinia';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { effectScope } from 'vue';

import type {
	WebitelDcctlV1Operation as Operation,
	WebitelDcctlV1ListOperationsResponse as Page,
} from '@/api/gen/_models';
import { useOperationsList } from './useOperationsList';

const listOperations = vi.hoisted(() => vi.fn());
vi.mock('@/api/gen/operation-service/operation-service', () => ({
	getOperationService: () => ({ listOperations }),
}));

const op = (name: string): Operation => ({ name: `operations/${name}` });
const page = (names: string[], next?: string): Page => ({
	operations: names.map(op),
	next_page_token: next,
});

const deferred = <T>() => {
	let resolve!: (value: T) => void;
	let reject!: (reason: unknown) => void;
	const promise = new Promise<T>((res, rej) => {
		resolve = res;
		reject = rej;
	});
	return { promise, resolve, reject };
};

const mount = () => {
	const scope = effectScope();
	const state = scope.run(() => useOperationsList()) as ReturnType<
		typeof useOperationsList
	>;
	return { scope, state };
};

const names = (state: ReturnType<typeof useOperationsList>) =>
	state.operations.value.map((item) => item.name);

beforeEach(() => {
	setActivePinia(createPinia());
	listOperations.mockReset();
});

afterEach(() => {
	vi.useRealTimers();
});

describe('useOperationsList', () => {
	it('is loading until the first page arrives, and not empty', async () => {
		listOperations.mockReturnValue(new Promise(() => undefined));
		const { state } = mount();
		expect(state.phase.value).toBe('loading');
		expect(state.operations.value).toEqual([]);
	});

	it('shows the first page with a timeout and no token', async () => {
		listOperations.mockResolvedValue(page(['a', 'b'], 'next'));
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('ready'));
		expect(names(state)).toEqual(['operations/a', 'operations/b']);
		expect(state.hasMore.value).toBe(true);
		expect(listOperations).toHaveBeenCalledWith(
			{ page_size: 50 },
			expect.objectContaining({
				timeout: expect.any(Number),
				signal: expect.any(AbortSignal),
			}),
		);
	});

	it('tells an empty list apart from a failure', async () => {
		listOperations.mockResolvedValue({});
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('ready'));
		expect(state.operations.value).toEqual([]);
		expect(state.hasMore.value).toBe(false);
	});

	it('reports an error and no data when the first page fails', async () => {
		listOperations.mockRejectedValue(new Error('Network Error'));
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('error'));
		expect((state.error.value as Error).message).toBe('Network Error');
	});

	it('loads more with the page token and appends', async () => {
		listOperations.mockResolvedValueOnce(page(['a'], 'p2'));
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('ready'));
		listOperations.mockResolvedValueOnce(page(['b', 'c']));
		await state.loadMore();
		expect(listOperations).toHaveBeenLastCalledWith(
			{ page_size: 50, page_token: 'p2' },
			expect.anything(),
		);
		expect(names(state)).toEqual([
			'operations/a',
			'operations/b',
			'operations/c',
		]);
		expect(state.hasMore.value).toBe(false);
	});

	it('does not repeat an operation that moved to the next page meanwhile', async () => {
		listOperations.mockResolvedValueOnce(page(['a', 'b'], 'p2'));
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('ready'));
		listOperations.mockResolvedValueOnce(page(['b', 'c']));
		await state.loadMore();
		expect(names(state)).toEqual([
			'operations/a',
			'operations/b',
			'operations/c',
		]);
	});

	it('does nothing without a next page, and never runs two page requests at once', async () => {
		listOperations.mockResolvedValueOnce(page(['a']));
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('ready'));
		await state.loadMore();
		expect(listOperations).toHaveBeenCalledTimes(1);

		listOperations.mockResolvedValueOnce(page(['a'], 'p2'));
		await state.refresh();
		const slow = deferred<Page>();
		listOperations.mockReturnValueOnce(slow.promise);
		const first = state.loadMore();
		const second = state.loadMore();
		slow.resolve(page(['b']));
		await Promise.all([first, second]);
		expect(listOperations).toHaveBeenCalledTimes(3);
	});

	it('ignores a request for more while the list is being refreshed', async () => {
		listOperations.mockResolvedValueOnce(page(['a'], 'p2'));
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('ready'));
		const slow = deferred<Page>();
		listOperations.mockReturnValueOnce(slow.promise);
		const refreshing = state.refresh();
		await state.loadMore();
		expect(listOperations).toHaveBeenCalledTimes(2);
		slow.resolve(page(['b']));
		await refreshing;
		expect(names(state)).toEqual(['operations/b']);
		expect(state.error.value).toBeNull();
	});

	it('keeps the loaded pages and says so when loading more fails', async () => {
		listOperations.mockResolvedValueOnce(page(['a'], 'p2'));
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('ready'));
		listOperations.mockRejectedValueOnce(new Error('boom'));
		await state.loadMore();
		expect(names(state)).toEqual(['operations/a']);
		expect((state.loadMoreError.value as Error).message).toBe('boom');
		expect(state.hasMore.value).toBe(true);
		listOperations.mockResolvedValueOnce(page(['b']));
		await state.loadMore();
		expect(state.loadMoreError.value).toBeNull();
		expect(names(state)).toEqual(['operations/a', 'operations/b']);
	});

	it('refresh starts over from the first page', async () => {
		listOperations.mockResolvedValueOnce(page(['a'], 'p2'));
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('ready'));
		listOperations.mockResolvedValueOnce(page(['b'], 'q2'));
		await state.refresh();
		expect(names(state)).toEqual(['operations/b']);
		expect(listOperations).toHaveBeenLastCalledWith(
			{ page_size: 50 },
			expect.anything(),
		);
	});

	it('keeps the old list with its age when a refresh fails', async () => {
		vi.useFakeTimers();
		vi.setSystemTime(Date.parse('2026-11-23T10:15:00Z'));
		listOperations.mockResolvedValueOnce(page(['a']));
		const { state } = mount();
		await vi.advanceTimersByTimeAsync(0);
		vi.advanceTimersByTime(9000);
		listOperations.mockRejectedValueOnce(new Error('down'));
		await state.refresh();
		expect(state.phase.value).toBe('ready');
		expect((state.error.value as Error).message).toBe('down');
		expect(names(state)).toEqual(['operations/a']);
		expect(state.age.value).toBe(9);
	});

	it('drops the answer of a page request when the list is refreshed meanwhile', async () => {
		listOperations.mockResolvedValueOnce(page(['a'], 'p2'));
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('ready'));
		const slow = deferred<Page>();
		listOperations.mockReturnValueOnce(slow.promise);
		const loading = state.loadMore();
		listOperations.mockResolvedValueOnce(page(['x']));
		await state.refresh();
		slow.resolve(page(['late']));
		await loading;
		expect(names(state)).toEqual(['operations/x']);
	});

	it('aborts the request in flight and drops its answer when the scope ends', async () => {
		const first = deferred<Page>();
		let signal: AbortSignal | undefined;
		listOperations.mockImplementation(
			(_params: unknown, options: { signal: AbortSignal }) => {
				signal = options.signal;
				return first.promise;
			},
		);
		const { scope, state } = mount();
		scope.stop();
		expect(signal?.aborted).toBe(true);
		first.resolve(page(['late']));
		await Promise.resolve();
		await Promise.resolve();
		expect(state.operations.value).toEqual([]);
		expect(state.phase.value).toBe('loading');
	});
});
