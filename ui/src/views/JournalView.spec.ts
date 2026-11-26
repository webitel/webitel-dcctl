import { DOMWrapper, flushPromises } from '@vue/test-utils';
import { AxiosError } from 'axios';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type {
	WebitelDcctlV1JournalEntry as Entry,
	WebitelDcctlV1ListJournalEntriesResponse as Page,
} from '@/api/gen/_models';
import { apiFailure } from '@/tests/apiFailure';
import { mountWithApp } from '@/tests/mountWithApp';
import JournalView from './JournalView.vue';

const listJournalEntries = vi.hoisted(() => vi.fn());
vi.mock('@/api/gen/journal-service/journal-service', () => ({
	getJournalService: () => ({ listJournalEntries }),
}));

const entry = (id: string, extra: Entry = {}): Entry => ({
	name: `journalEntries/20261123101501000000000-${id}`,
	create_time: '2026-11-23T10:15:01Z',
	type: 'OPERATION',
	operation_type: 'SWITCH',
	datacenter: 'datacenters/dc_b',
	operation: 'operations/op-1',
	initiator: { user: 'support', instance: { node: 'dcctl-a1' } },
	result: 'OK',
	detail: `detail ${id}`,
	...extra,
});

const page = (entries: Entry[], next?: string): Page => ({
	journal_entries: entries,
	next_page_token: next,
});

type Mounted = Awaited<ReturnType<typeof mountWithApp>>;

const mountJournal = async () => {
	const mounted = await mountWithApp(JournalView, {}, undefined, '/journal');
	await flushPromises();
	return mounted;
};

// wt-table and routed views load lazily, which takes over a second on a busy runner.
const LAZY_WAIT = { timeout: 5000 };

const waitForTable = (mounted: Mounted) =>
	vi.waitFor(
		() =>
			expect(
				mounted.wrapper.find('[data-test="journal-table"] tbody tr').exists(),
			).toBe(true),
		LAZY_WAIT,
	);

const rows = (mounted: Mounted) =>
	mounted.wrapper.findAll('[data-test="journal-table"] tbody tr');

const expand = async (mounted: Mounted) => {
	await mounted.wrapper
		.find('[data-test="journal-table"] tbody tr button')
		.trigger('click');
	await vi.waitFor(() =>
		expect(mounted.wrapper.find('[data-test="journal-detail"]').exists()).toBe(
			true,
		),
	);
};

/** Opens the operation filter in the column header, types the value and applies it. */
const applyOperationFilter = async (mounted: Mounted, value: string) => {
	const icons = mounted.wrapper.findAll(
		'.wt-table-column-filter__wrapper button',
	);
	expect(icons).toHaveLength(3);
	await icons[2]?.trigger('click');
	const body = new DOMWrapper(document.body);
	await vi.waitFor(() =>
		expect(body.find('[data-test="filter-operation"] input').exists()).toBe(
			true,
		),
	);
	await body.find('[data-test="filter-operation"] input').setValue(value);
	await body.find('[data-test="filter-apply"]').trigger('click');
	await flushPromises();
};

beforeEach(() => {
	listJournalEntries.mockReset();
});

afterEach(() => {
	vi.useRealTimers();
});

describe('JournalView', () => {
	it('says it loads, not that the journal is empty, while the first page is on its way', async () => {
		listJournalEntries.mockReturnValue(new Promise(() => undefined));
		const { wrapper } = await mountJournal();
		expect(wrapper.find('[data-test="journal-loading"]').exists()).toBe(true);
		expect(wrapper.text()).not.toContain('no entries');
	});

	it('shows the error and a retry, not an empty journal, when the API is down', async () => {
		listJournalEntries.mockRejectedValue(new AxiosError('Network Error'));
		const { wrapper } = await mountJournal();
		const error = wrapper.find('[data-test="journal-error"]');
		expect(error.text()).toContain('Cannot get the journal');
		expect(error.text()).toContain('Network Error');
		expect(wrapper.find('[data-test="journal-empty"]').exists()).toBe(false);
		listJournalEntries.mockResolvedValue(page([entry('a')]));
		await wrapper.find('[data-test="journal-retry"]').trigger('click');
		await flushPromises();
		expect(wrapper.find('[data-test="journal-error"]').exists()).toBe(false);
		await vi.waitFor(
			() =>
				expect(wrapper.find('[data-test="journal-table"]').exists()).toBe(true),
			LAZY_WAIT,
		);
	});

	it('shows a filter the server rejected as such, keeps it visible and clearable', async () => {
		listJournalEntries.mockResolvedValueOnce(page([entry('a')]));
		const mounted = await mountJournal();
		await waitForTable(mounted);

		listJournalEntries.mockRejectedValueOnce(
			apiFailure({
				id: 'dcctl.request.invalid',
				code: 400,
				detail: 'invalid request',
				violations: [
					{
						type: 'field',
						subject: 'filter',
						description: 'the value of operation must be a quoted string',
					},
				],
			}),
		);
		await applyOperationFilter(mounted, 'op-1');

		const { wrapper } = mounted;
		const rejected = wrapper.find('[data-test="journal-filter-rejected"]');
		expect(rejected.text()).toContain('The server rejected the filter');
		expect(rejected.text()).toContain(
			'the value of operation must be a quoted string',
		);
		expect(wrapper.find('[data-test="journal-empty"]').exists()).toBe(false);
		expect(wrapper.find('[data-test="journal-error"]').exists()).toBe(false);
		expect(
			wrapper.find('[data-test="journal-active-filter"]').text(),
		).toContain('operation = "operations/op-1"');

		listJournalEntries.mockResolvedValueOnce(page([entry('a')]));
		await wrapper.find('[data-test="journal-clear-filter"]').trigger('click');
		await flushPromises();
		await waitForTable(mounted);
		expect(wrapper.find('[data-test="journal-filter-rejected"]').exists()).toBe(
			false,
		);
	});

	it('says the journal has no entries when it has none and no filter is on', async () => {
		listJournalEntries.mockResolvedValue(page([]));
		const { wrapper } = await mountJournal();
		expect(wrapper.find('[data-test="journal-empty"]').text()).toBe(
			'The journal has no entries',
		);
	});

	it('applies the filter of a header, names it when nothing matches, and clears it', async () => {
		listJournalEntries.mockResolvedValueOnce(page([entry('a')]));
		const mounted = await mountJournal();
		await waitForTable(mounted);

		listJournalEntries.mockResolvedValueOnce(page([]));
		await applyOperationFilter(mounted, 'a" OR x');

		expect(listJournalEntries).toHaveBeenLastCalledWith(
			{ page_size: 50, filter: 'operation = "operations/a\\" OR x"' },
			expect.anything(),
		);
		const empty = mounted.wrapper.find('[data-test="journal-empty"]');
		expect(empty.text()).toContain('No journal entries match the filter');
		expect(empty.text()).toContain('operation = "operations/a\\" OR x"');
		// The header filters stay: the empty result does not take them away.
		expect(
			mounted.wrapper.findAll('.wt-table-column-filter__wrapper button'),
		).toHaveLength(3);

		listJournalEntries.mockResolvedValueOnce(page([entry('a')]));
		await mounted.wrapper
			.find('[data-test="journal-clear-filter"]')
			.trigger('click');
		await flushPromises();
		expect(listJournalEntries).toHaveBeenLastCalledWith(
			{ page_size: 50 },
			expect.anything(),
		);
		await waitForTable(mounted);
		expect(rows(mounted)).toHaveLength(1);
	});

	it('lists the entries: type, event, initiator, result and detail', async () => {
		listJournalEntries.mockResolvedValue(
			page([
				entry('a'),
				entry('b', {
					type: 'SYSTEM',
					system_action: 'OUTCOME_UNKNOWN',
					operation_type: undefined,
					initiator: { system: true, instance: { node: 'dcctl-b1' } },
					result: 'ERROR',
					operation: undefined,
				}),
				entry('c', {
					type: 'ALARM',
					alarm_type: 'QUORUM_LOST',
					operation_type: undefined,
					initiator: { system: true },
					result: 'OK',
				}),
			]),
		);
		const mounted = await mountJournal();
		await waitForTable(mounted);
		const [first, second, third] = rows(mounted).map((row) => row.text());
		expect(first).toContain('Operation');
		expect(first).toContain('Switch');
		expect(first).toContain('support');
		expect(first).toContain('dc_b');
		expect(first).toContain('detail a');
		expect(second).toContain('Outcome unknown');
		expect(second).toContain('dcctl (system)');
		expect(second).toContain('dcctl-b1');
		expect(second).toContain('Error');
		expect(third).toContain('Quorum lost');
		expect(third).toContain('Cleared');
	});

	it('shows an event of a type the UI does not know as it is', async () => {
		listJournalEntries.mockResolvedValue(
			page([
				entry('a', { type: 'SYSTEM', system_action: 'BRAND_NEW' as never }),
			]),
		);
		const mounted = await mountJournal();
		await waitForTable(mounted);
		expect(rows(mounted)[0]?.text()).toContain('BRAND_NEW');
		expect(rows(mounted)[0]?.text()).not.toContain('dcctl.systemAction');
	});

	it('links the operation to its page', async () => {
		listJournalEntries.mockResolvedValue(
			page([
				entry('a', { operation: 'operations/op-1' }),
				entry('b', {
					operation: undefined,
					type: 'ALARM',
					alarm_type: 'STALE',
				}),
			]),
		);
		const mounted = await mountJournal();
		await waitForTable(mounted);
		const links = mounted.wrapper.findAll(
			'[data-test="journal-operation-link"]',
		);
		expect(links).toHaveLength(1);
		expect(links[0]?.text()).toBe('op-1');
		expect(links[0]?.attributes('href')).toBe('/dcctl/operations/op-1');
		await links[0]?.trigger('click');
		await vi.waitFor(
			() => expect(mounted.router.currentRoute.value.name).toBe('operation'),
			LAZY_WAIT,
		);
		expect(mounted.router.currentRoute.value.params.id).toBe('op-1');
	});

	it('expands a row to its params, confirmations and error as text', async () => {
		listJournalEntries.mockResolvedValue(
			page([
				entry('a', {
					params: { mode: 'QUORUM', note: '<img src=x onerror=alert(1)>' },
					confirmation: { isolation: true },
					result: 'ERROR',
					error: {
						id: 'dcctl.lock.lost',
						reason: 'LOCK_LOST',
						detail: 'session expired',
						metadata: { holder: 'dcctl-a1' },
					},
					initiator: {
						user: 'support',
						address: '10.1.0.15',
						instance: { node: 'dcctl-a1', datacenter: 'datacenters/dc_a' },
					},
				}),
			]),
		);
		const mounted = await mountJournal();
		await waitForTable(mounted);
		expect(mounted.wrapper.find('[data-test="journal-detail"]').exists()).toBe(
			false,
		);
		await expand(mounted);
		const detail = mounted.wrapper.find('[data-test="journal-detail"]');
		expect(detail.exists()).toBe(true);
		expect(detail.text()).toContain('QUORUM');
		expect(detail.text()).toContain('<img src=x onerror=alert(1)>');
		expect(detail.find('img').exists()).toBe(false);
		expect(detail.text()).toContain('session expired');
		expect(detail.text()).toContain('dcctl.lock.lost');
		expect(detail.text()).toContain('holder');
		expect(detail.text()).toContain('10.1.0.15');
		expect(detail.text()).toContain('max_discard_bytes');
	});

	it('shows omitted zeros of a present confirmation as zero', async () => {
		listJournalEntries.mockResolvedValue(
			page([entry('a', { confirmation: {} })]),
		);
		const mounted = await mountJournal();
		await waitForTable(mounted);
		await expand(mounted);
		const detail = mounted.wrapper.find('[data-test="journal-detail"]');
		expect(
			detail.find('[data-test="fact-max_discard_bytes"]').text(),
		).toContain('0');
		expect(detail.find('[data-test="fact-isolation"]').text()).toContain(
			'false',
		);
	});

	it('says an expanded entry without facts has none', async () => {
		listJournalEntries.mockResolvedValue(page([entry('a')]));
		const mounted = await mountJournal();
		await waitForTable(mounted);
		await expand(mounted);
		expect(
			mounted.wrapper.find('[data-test="journal-detail"]').text(),
		).toContain('No parameters, confirmations or error');
	});

	it('loads the next page by its token', async () => {
		listJournalEntries.mockResolvedValueOnce(
			page([entry('b'), entry('a')], 'next'),
		);
		const mounted = await mountJournal();
		await waitForTable(mounted);
		listJournalEntries.mockResolvedValueOnce(page([entry('c')]));
		await mounted.wrapper
			.find('[data-test="journal-load-more"]')
			.trigger('click');
		await flushPromises();
		expect(rows(mounted)).toHaveLength(3);
		expect(listJournalEntries).toHaveBeenLastCalledWith(
			{ page_size: 50, page_token: 'next' },
			expect.anything(),
		);
		expect(
			mounted.wrapper.find('[data-test="journal-load-more"]').exists(),
		).toBe(false);
	});

	it('keeps the loaded entries and says so when the next page fails', async () => {
		listJournalEntries.mockResolvedValueOnce(page([entry('a')], 'next'));
		const mounted = await mountJournal();
		await waitForTable(mounted);
		listJournalEntries.mockRejectedValueOnce(new AxiosError('Network Error'));
		await mounted.wrapper
			.find('[data-test="journal-load-more"]')
			.trigger('click');
		await flushPromises();
		expect(
			mounted.wrapper.find('[data-test="journal-load-more-error"]').text(),
		).toContain('Cannot load more entries');
		expect(rows(mounted)).toHaveLength(1);
		expect(
			mounted.wrapper.find('[data-test="journal-load-more"]').exists(),
		).toBe(true);
	});

	it('keeps the entries and shows the error when a refresh fails', async () => {
		listJournalEntries.mockResolvedValueOnce(page([entry('a')]));
		const mounted = await mountJournal();
		await waitForTable(mounted);
		listJournalEntries.mockRejectedValueOnce(new AxiosError('Network Error'));
		await mounted.wrapper
			.find('[data-test="journal-refresh"]')
			.trigger('click');
		await flushPromises();
		expect(
			mounted.wrapper.find('[data-test="journal-refresh-error"]').text(),
		).toContain('Network Error');
		expect(rows(mounted)).toHaveLength(1);
	});

	it('says that entries of an unreachable datacenter may be missing', async () => {
		listJournalEntries.mockResolvedValue(page([entry('a')]));
		const mounted = await mountJournal();
		await waitForTable(mounted);
		expect(
			mounted.wrapper.find('[data-test="journal-incomplete"]').text(),
		).toContain('may be missing');
	});

	it('shows the time of an entry and how long ago it was, by the server clock', async () => {
		listJournalEntries.mockResolvedValue(page([entry('a')]));
		const mounted = await mountJournal();
		await waitForTable(mounted);
		const time = mounted.wrapper.find('[data-test="journal-time"]');
		expect(time.text()).toMatch(/2026|11\/23\/26/);
		expect(time.text()).toMatch(/ago/);
	});

	it('keeps a row expanded when the clock ticks', async () => {
		vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'Date'] });
		vi.setSystemTime(Date.parse('2026-11-23T10:15:06Z'));
		listJournalEntries.mockResolvedValue(page([entry('a')]));
		const mounted = await mountJournal();
		await waitForTable(mounted);
		await expand(mounted);
		const before = mounted.wrapper.find('[data-test="journal-time"]').text();

		await vi.advanceTimersByTimeAsync(60_000);

		expect(mounted.wrapper.find('[data-test="journal-detail"]').exists()).toBe(
			true,
		);
		expect(mounted.wrapper.find('[data-test="journal-time"]').text()).not.toBe(
			before,
		);
	});
});
