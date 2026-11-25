import { computed, onScopeDispose, ref, shallowRef } from 'vue';

import { serverOffsetMs } from '@/api/client/serverClock';
import type { WebitelDcctlV1Operation as Operation } from '@/api/gen/_models';
import { getOperationService } from '@/api/gen/operation-service/operation-service';

const PAGE_SIZE = 50;
const REQUEST_TIMEOUT_MS = 10_000;
const CLOCK_TICK_MS = 1000;

/**
 * Operations from newest to oldest, a page at a time. The list does not
 * refresh itself: it shows how old it is and `refresh()` starts over. A failed
 * refresh or page keeps what is loaded and reports the error beside it.
 */
export const useOperationsList = () => {
	const operations = shallowRef<readonly Operation[]>([]);
	const nextToken = ref<string>();
	const loaded = ref(false);
	/** Error of the last refresh; `null` after a successful one. */
	const error = ref<unknown>(null);
	const loadMoreError = ref<unknown>(null);
	const loadingMore = ref(false);
	const refreshing = ref(false);
	const loadedAt = ref<number | null>(null);
	const now = ref(Date.now());

	// Bumped by every start over and by the end of the scope: a late answer is dropped.
	let epoch = 0;
	let abort = new AbortController();

	const clock = setInterval(() => {
		now.value = Date.now();
	}, CLOCK_TICK_MS);

	const request = (pageToken?: string) =>
		getOperationService().listOperations(
			pageToken
				? { page_size: PAGE_SIZE, page_token: pageToken }
				: { page_size: PAGE_SIZE },
			{ timeout: REQUEST_TIMEOUT_MS, signal: abort.signal },
		);

	const refresh = async (): Promise<void> => {
		const session = ++epoch;
		abort.abort();
		abort = new AbortController();
		refreshing.value = true;
		loadingMore.value = false;
		loadMoreError.value = null;
		try {
			const result = await request();
			if (session !== epoch) return;
			operations.value = result.operations ?? [];
			nextToken.value = result.next_page_token || undefined;
			loaded.value = true;
			error.value = null;
			loadedAt.value = Date.now();
			now.value = loadedAt.value;
		} catch (cause) {
			if (session !== epoch) return;
			error.value = cause;
		} finally {
			if (session === epoch) refreshing.value = false;
		}
	};

	const loadMore = async (): Promise<void> => {
		const token = nextToken.value;
		if (!token || loadingMore.value || refreshing.value) return;
		const session = epoch;
		loadingMore.value = true;
		try {
			const result = await request(token);
			if (session !== epoch) return;
			// A new operation shifts the pages, so one may come twice.
			const known = new Set(operations.value.map((item) => item.name));
			operations.value = [
				...operations.value,
				...(result.operations ?? []).filter((item) => !known.has(item.name)),
			];
			nextToken.value = result.next_page_token || undefined;
			loadMoreError.value = null;
		} catch (cause) {
			if (session !== epoch) return;
			loadMoreError.value = cause;
		} finally {
			if (session === epoch) loadingMore.value = false;
		}
	};

	onScopeDispose(() => {
		epoch++;
		abort.abort();
		clearInterval(clock);
	});

	void refresh();

	const phase = computed<'loading' | 'ready' | 'error'>(() => {
		if (loaded.value) return 'ready';
		return error.value ? 'error' : 'loading';
	});
	/** Seconds since the list was loaded; `null` before the first page. */
	const age = computed(() =>
		loadedAt.value === null
			? null
			: Math.max(0, Math.floor((now.value - loadedAt.value) / 1000)),
	);

	return {
		operations,
		phase,
		error,
		loadMoreError,
		loadingMore,
		refreshing,
		hasMore: computed(() => nextToken.value !== undefined),
		age,
		now: computed(() => now.value + serverOffsetMs()),
		refresh,
		loadMore,
	};
};
