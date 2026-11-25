import { AxiosError } from 'axios';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { effectScope } from 'vue';

import type {
	WebitelDcctlV1JournalEntry as Entry,
	WebitelDcctlV1ListJournalEntriesResponse as Page,
} from '@/api/gen/_models';
import { apiFailure } from '@/tests/apiFailure';
import { useJournal } from './useJournal';

const listJournalEntries = vi.hoisted(() => vi.fn());
vi.mock('@/api/gen/journal-service/journal-service', () => ({
	getJournalService: () => ({ listJournalEntries }),
}));

const serverOffset = vi.hoisted(() => ({ ms: 0 }));
vi.mock('@/api/client/serverClock', () => ({
	serverOffsetMs: () => serverOffset.ms,
}));

const entry = (name: string): Entry => ({ name: `journalEntries/${name}` });
const page = (names: string[], next?: string): Page => ({
	journal_entries: names.map(entry),
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

type State = ReturnType<typeof useJournal>;

const mount = () => {
	const scope = effectScope();
	const state = scope.run(() => useJournal()) as State;
	return { scope, state };
};

const names = (state: State) => state.entries.value.map((item) => item.name);

const filterRejected = () =>
	apiFailure({
		id: 'dcctl.request.invalid',
		code: 400,
		detail: 'invalid request',
		violations: [
			{ type: 'field', subject: 'filter', description: 'type "X" is unknown' },
		],
	});

beforeEach(() => {
	listJournalEntries.mockReset();
	serverOffset.ms = 0;
});

afterEach(() => {
	vi.useRealTimers();
});

describe('useJournal', () => {
	it('is loading, not empty, until the first page arrives', () => {
		listJournalEntries.mockReturnValue(new Promise(() => undefined));
		const { state } = mount();
		expect(state.phase.value).toBe('loading');
		expect(state.entries.value).toEqual([]);
	});

	it('asks for the first page without a filter and a token', async () => {
		listJournalEntries.mockResolvedValue(page(['a', 'b'], 'next'));
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('ready'));
		expect(names(state)).toEqual(['journalEntries/a', 'journalEntries/b']);
		expect(state.hasMore.value).toBe(true);
		expect(listJournalEntries).toHaveBeenCalledWith(
			{ page_size: 50 },
			expect.objectContaining({
				timeout: expect.any(Number),
				signal: expect.any(AbortSignal),
			}),
		);
	});

	it('is ready and empty for a page without entries', async () => {
		listJournalEntries.mockResolvedValue({});
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('ready'));
		expect(state.entries.value).toEqual([]);
		expect(state.hasMore.value).toBe(false);
	});

	it('loads the next page by its token', async () => {
		listJournalEntries.mockResolvedValueOnce(page(['b', 'a'], 'next'));
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('ready'));
		listJournalEntries.mockResolvedValueOnce(page(['c']));
		await state.loadMore();
		expect(names(state)).toEqual([
			'journalEntries/b',
			'journalEntries/a',
			'journalEntries/c',
		]);
		expect(state.hasMore.value).toBe(false);
		expect(listJournalEntries).toHaveBeenLastCalledWith(
			{ page_size: 50, page_token: 'next' },
			expect.anything(),
		);
	});

	it('resets the pages and the token when the filter changes', async () => {
		listJournalEntries.mockResolvedValueOnce(page(['a'], 'token-1'));
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('ready'));

		const next = deferred<Page>();
		listJournalEntries.mockReturnValueOnce(next.promise);
		void state.applyFilter({
			types: ['ALARM', 'STEP'],
			datacenter: 'dc_b',
			operation: '',
		});
		// Nothing of the old filter stays under the new one while it loads.
		expect(state.entries.value).toEqual([]);
		expect(state.phase.value).toBe('loading');
		expect(state.hasMore.value).toBe(false);
		next.resolve(page(['x']));
		await vi.waitFor(() => expect(state.phase.value).toBe('ready'));
		expect(names(state)).toEqual(['journalEntries/x']);
		expect(listJournalEntries).toHaveBeenLastCalledWith(
			{
				page_size: 50,
				filter:
					'(type = "ALARM" OR type = "STEP") AND datacenter = "datacenters/dc_b"',
			},
			expect.anything(),
		);
		expect(state.filterText.value).toBe(
			'(type = "ALARM" OR type = "STEP") AND datacenter = "datacenters/dc_b"',
		);
	});

	it('asks for the next page with the filter of the first one', async () => {
		listJournalEntries.mockResolvedValueOnce(page(['a']));
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('ready'));
		listJournalEntries.mockResolvedValueOnce(page(['x'], 'token-2'));
		await state.applyFilter({
			types: ['SYSTEM'],
			datacenter: '',
			operation: '',
		});
		listJournalEntries.mockResolvedValueOnce(page(['y']));
		await state.loadMore();
		expect(listJournalEntries).toHaveBeenLastCalledWith(
			{ page_size: 50, filter: 'type = "SYSTEM"', page_token: 'token-2' },
			expect.anything(),
		);
	});

	it('drops the late answer of the old filter', async () => {
		const first = deferred<Page>();
		listJournalEntries.mockReturnValueOnce(first.promise);
		const { state } = mount();
		listJournalEntries.mockResolvedValueOnce(page(['new']));
		await state.applyFilter({
			types: ['ALARM'],
			datacenter: '',
			operation: '',
		});
		first.resolve(page(['old'], 'old-token'));
		await Promise.resolve();
		expect(names(state)).toEqual(['journalEntries/new']);
		expect(state.hasMore.value).toBe(false);
	});

	it('drops a late next page after the filter changed', async () => {
		listJournalEntries.mockResolvedValueOnce(page(['a'], 'next'));
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('ready'));
		const late = deferred<Page>();
		listJournalEntries.mockReturnValueOnce(late.promise);
		const loading = state.loadMore();
		listJournalEntries.mockResolvedValueOnce(page(['n']));
		await state.applyFilter({ types: [], datacenter: '', operation: 'op-1' });
		late.resolve(page(['stale']));
		await loading;
		expect(names(state)).toEqual(['journalEntries/n']);
		expect(state.loadingMore.value).toBe(false);
	});

	it('is an error, not an empty list, when the first page fails', async () => {
		listJournalEntries.mockRejectedValue(new AxiosError('Network Error'));
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('error'));
		expect(state.entries.value).toEqual([]);
		expect(state.filterRejected.value).toBe(false);
	});

	it('tells a filter the server rejected from other errors', async () => {
		listJournalEntries.mockResolvedValueOnce(page(['a']));
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('ready'));
		listJournalEntries.mockRejectedValueOnce(filterRejected());
		await state.applyFilter({
			types: ['ALARM'],
			datacenter: '',
			operation: '',
		});
		expect(state.phase.value).toBe('error');
		expect(state.filterRejected.value).toBe(true);
		expect(state.entries.value).toEqual([]);
	});

	it('retries the same filter with refresh', async () => {
		listJournalEntries.mockRejectedValueOnce(new AxiosError('Network Error'));
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('error'));
		listJournalEntries.mockResolvedValueOnce(page(['a']));
		await state.refresh();
		expect(state.phase.value).toBe('ready');
		expect(state.error.value).toBeNull();
		expect(names(state)).toEqual(['journalEntries/a']);
	});

	it('keeps the loaded entries when a refresh fails', async () => {
		listJournalEntries.mockResolvedValueOnce(page(['a'], 'next'));
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('ready'));
		listJournalEntries.mockRejectedValueOnce(new AxiosError('Network Error'));
		await state.refresh();
		expect(state.phase.value).toBe('ready');
		expect(state.error.value).toBeInstanceOf(AxiosError);
		expect(names(state)).toEqual(['journalEntries/a']);
		expect(state.refreshing.value).toBe(false);
	});

	it('keeps the pages and the token when the next page fails, and retries it', async () => {
		listJournalEntries.mockResolvedValueOnce(page(['a'], 'next'));
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('ready'));
		listJournalEntries.mockRejectedValueOnce(new AxiosError('Network Error'));
		await state.loadMore();
		expect(state.loadMoreError.value).toBeInstanceOf(AxiosError);
		expect(names(state)).toEqual(['journalEntries/a']);
		expect(state.hasMore.value).toBe(true);
		listJournalEntries.mockResolvedValueOnce(page(['b']));
		await state.loadMore();
		expect(state.loadMoreError.value).toBeNull();
		expect(names(state)).toEqual(['journalEntries/a', 'journalEntries/b']);
	});

	it('does not start a second next page while one loads', async () => {
		listJournalEntries.mockResolvedValueOnce(page(['a'], 'next'));
		const { state } = mount();
		await vi.waitFor(() => expect(state.phase.value).toBe('ready'));
		const slow = deferred<Page>();
		listJournalEntries.mockReturnValueOnce(slow.promise);
		const first = state.loadMore();
		await state.loadMore();
		expect(listJournalEntries).toHaveBeenCalledTimes(2);
		slow.resolve(page(['b']));
		await first;
	});

	it('drops the answer that arrives after the scope ends', async () => {
		const first = deferred<Page>();
		listJournalEntries.mockReturnValueOnce(first.promise);
		const { state, scope } = mount();
		scope.stop();
		first.resolve(page(['a']));
		await Promise.resolve();
		expect(state.entries.value).toEqual([]);
	});

	describe('clock', () => {
		const START = Date.parse('2026-11-23T10:00:00Z');

		const mountAtStart = async () => {
			vi.useFakeTimers();
			vi.setSystemTime(START);
			listJournalEntries.mockResolvedValue(page(['a']));
			const mounted = mount();
			await vi.advanceTimersByTimeAsync(0);
			return mounted;
		};

		it('moves every 10 s, not before', async () => {
			const { state } = await mountAtStart();
			expect(state.now.value).toBe(START);
			await vi.advanceTimersByTimeAsync(9_999);
			expect(state.now.value).toBe(START);
			await vi.advanceTimersByTimeAsync(1);
			expect(state.now.value).toBe(START + 10_000);
		});

		it('is the server time: the browser time plus the offset', async () => {
			serverOffset.ms = 5_000;
			const { state } = await mountAtStart();
			expect(state.now.value).toBe(START + 5_000);
			await vi.advanceTimersByTimeAsync(10_000);
			expect(state.now.value).toBe(START + 15_000);
		});

		it('stops with the scope', async () => {
			const { state, scope } = await mountAtStart();
			scope.stop();
			expect(vi.getTimerCount()).toBe(0);
			await vi.advanceTimersByTimeAsync(30_000);
			expect(state.now.value).toBe(START);
		});
	});
});
