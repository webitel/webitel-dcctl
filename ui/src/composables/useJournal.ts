import { computed, onScopeDispose, ref, shallowRef } from 'vue';

import { getApiError } from '@/api/client/apiError';
import { serverOffsetMs } from '@/api/client/serverClock';
import type { WebitelDcctlV1JournalEntry as Entry } from '@/api/gen/_models';
import { getJournalService } from '@/api/gen/journal-service/journal-service';
import {
	composeJournalFilter,
	emptyJournalFilter,
	type JournalFilter,
} from '@/utils/journalFilter';

const PAGE_SIZE = 50;
const REQUEST_TIMEOUT_MS = 10_000;
const CLOCK_TICK_MS = 10_000;
const REQUEST_INVALID = 'dcctl.request.invalid';

/**
 * The journal from newest to oldest, a page at a time, narrowed by a filter.
 * Changing the filter starts over: the entries of the old filter are dropped
 * at once, so what is on screen always belongs to the filter shown. A failed
 * refresh or page keeps what is loaded and reports the error beside it.
 */
export const useJournal = () => {
	const entries = shallowRef<readonly Entry[]>([]);
	const nextToken = ref<string>();
	const filter = ref<JournalFilter>(emptyJournalFilter);
	const loaded = ref(false);
	/** Error of the last refresh; `null` after a successful one. */
	const error = ref<unknown>(null);
	const loadMoreError = ref<unknown>(null);
	const loadingMore = ref(false);
	const refreshing = ref(false);
	const now = ref(Date.now());

	// Bumped by every start over and by the end of the scope: a late answer is dropped.
	let epoch = 0;
	let abort = new AbortController();

	const clock = setInterval(() => {
		now.value = Date.now();
	}, CLOCK_TICK_MS);

	const filterText = computed(() => composeJournalFilter(filter.value));

	const request = (pageToken?: string) =>
		getJournalService().listJournalEntries(
			{
				page_size: PAGE_SIZE,
				...(filterText.value ? { filter: filterText.value } : {}),
				...(pageToken ? { page_token: pageToken } : {}),
			},
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
			entries.value = result.journal_entries ?? [];
			nextToken.value = result.next_page_token || undefined;
			loaded.value = true;
			error.value = null;
			now.value = Date.now();
		} catch (cause) {
			if (session !== epoch) return;
			error.value = cause;
		} finally {
			if (session === epoch) refreshing.value = false;
		}
	};

	/** Starts over with another filter; the page token of the old one is useless. */
	const applyFilter = (next: JournalFilter): Promise<void> => {
		filter.value = next;
		entries.value = [];
		nextToken.value = undefined;
		loaded.value = false;
		error.value = null;
		return refresh();
	};

	const loadMore = async (): Promise<void> => {
		const token = nextToken.value;
		if (!token || loadingMore.value || refreshing.value) return;
		const session = epoch;
		loadingMore.value = true;
		try {
			const result = await request(token);
			if (session !== epoch) return;
			// The token continues below the last key, so no entry comes twice.
			entries.value = [...entries.value, ...(result.journal_entries ?? [])];
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

	return {
		entries,
		phase,
		filter,
		filterText,
		error,
		/** The server did not accept the request: the filter is wrong, the journal is not empty. */
		filterRejected: computed(
			() => getApiError(error.value)?.id === REQUEST_INVALID,
		),
		loadMoreError,
		loadingMore,
		refreshing,
		hasMore: computed(() => nextToken.value !== undefined),
		now: computed(() => now.value + serverOffsetMs()),
		refresh,
		applyFilter,
		loadMore,
	};
};
