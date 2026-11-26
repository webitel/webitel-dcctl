import { flushPromises } from '@vue/test-utils';
import { AxiosError } from 'axios';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import type {
	WebitelDcctlV1Operation as Operation,
	WebitelDcctlV1ListOperationsResponse as Page,
} from '@/api/gen/_models';
import { mountWithApp } from '@/tests/mountWithApp';
import { waitForLazy } from '@/tests/waitForLazy';
import OperationsView from './OperationsView.vue';

const listOperations = vi.hoisted(() => vi.fn());
vi.mock('@/api/gen/operation-service/operation-service', () => ({
	getOperationService: () => ({ listOperations }),
}));

const op = (id: string, extra: Operation = {}): Operation => ({
	name: `operations/${id}`,
	type: 'SWITCH',
	state: 'SUCCEEDED',
	target: 'datacenters/dc_b',
	mode: 'QUORUM',
	initiator: { user: 'support' },
	create_time: '2026-11-23T10:15:00Z',
	end_time: '2026-11-23T10:15:42Z',
	...extra,
});

const page = (operations: Operation[], next?: string): Page => ({
	operations,
	next_page_token: next,
});

type Mounted = Awaited<ReturnType<typeof mountWithApp>>;

const mountList = async () => {
	const mounted = await mountWithApp(
		OperationsView,
		{},
		undefined,
		'/operations',
	);
	await flushPromises();
	return mounted;
};

const waitForTable = (mounted: Mounted) =>
	waitForLazy(() =>
		expect(
			mounted.wrapper.find('[data-test="ops-table"] tbody tr').exists(),
		).toBe(true),
	);

const rowTexts = (mounted: Mounted) =>
	mounted.wrapper
		.findAll('[data-test="ops-table"] tbody tr')
		.map((row) => row.text());

beforeEach(() => {
	listOperations.mockReset();
});

describe('OperationsView states', () => {
	it('says it is loading and does not look empty while the first page is on its way', async () => {
		listOperations.mockReturnValue(new Promise(() => undefined));
		const { wrapper } = await mountList();
		expect(wrapper.find('[data-test="ops-loading"]').exists()).toBe(true);
		expect(wrapper.text()).not.toContain('No operations');
		expect(wrapper.find('[data-test="ops-table"]').exists()).toBe(false);
	});

	it('shows the error and a retry, not an empty list, when the API is down', async () => {
		listOperations.mockRejectedValue(new AxiosError('Network Error'));
		const { wrapper } = await mountList();
		const error = wrapper.find('[data-test="ops-error"]');
		expect(error.text()).toContain('Cannot get the operations');
		expect(error.text()).toContain('Network Error');
		expect(wrapper.text()).not.toContain('No operations');

		listOperations.mockResolvedValue(page([op('a')]));
		await wrapper.find('[data-test="ops-retry"]').trigger('click');
		await flushPromises();
		expect(wrapper.find('[data-test="ops-error"]').exists()).toBe(false);
	});

	it('says there are no operations only after a successful empty answer', async () => {
		listOperations.mockResolvedValue({});
		const { wrapper } = await mountList();
		expect(wrapper.find('[data-test="ops-empty"]').text()).toBe(
			'No operations yet',
		);
		expect(wrapper.find('[data-test="ops-error"]').exists()).toBe(false);
	});
});

describe('OperationsView list', () => {
	it('shows type, target, state, mode, initiator, start and duration, and links to the operation', async () => {
		listOperations.mockResolvedValue(
			page([op('0b9f', { type: 'FAILOVER', mode: 'FORCED', state: 'FAILED' })]),
		);
		const mounted = await mountList();
		await waitForTable(mounted);
		const [row] = rowTexts(mounted);
		expect(row).toContain('Failover');
		expect(row).toContain('dc_b');
		expect(row).toContain('Failed');
		expect(row).toContain('Forced');
		expect(row).toContain('support');
		expect(row).toContain('42 s');
		const link = mounted.wrapper.find('[data-test="ops-link"]');
		expect(link.attributes('href')).toBe('/dcctl/operations/0b9f');
	});

	it('shows a dash for what the gateway left out, never an empty cell or a made-up value', async () => {
		listOperations.mockResolvedValue(
			page([
				{ name: 'operations/x', type: 'INITIALIZE_ARBITER', state: 'RUNNING' },
			]),
		);
		const mounted = await mountList();
		await waitForTable(mounted);
		const [row] = rowTexts(mounted);
		expect(row).toContain('Initialize arbiter');
		expect((row?.match(/—/g) ?? []).length).toBeGreaterThanOrEqual(4);
	});

	it('shows an unknown state as it is instead of a message key', async () => {
		listOperations.mockResolvedValue(
			page([op('a', { state: 'PAUSED' as never })]),
		);
		const mounted = await mountList();
		await waitForTable(mounted);
		expect(rowTexts(mounted)[0]).toContain('PAUSED');
		expect(rowTexts(mounted)[0]).not.toContain('dcctl.');
	});
});

describe('OperationsView paging', () => {
	it('offers "Load more" only while there is a next page, and appends the next page', async () => {
		listOperations.mockResolvedValueOnce(page([op('a')], 'p2'));
		const mounted = await mountList();
		await waitForTable(mounted);
		const button = () => mounted.wrapper.find('[data-test="ops-load-more"]');
		expect(button().text()).toBe('Load more');

		listOperations.mockResolvedValueOnce(page([op('b', { type: 'DEMOTE' })]));
		await button().trigger('click');
		await flushPromises();
		expect(listOperations).toHaveBeenLastCalledWith(
			{ page_size: 50, page_token: 'p2' },
			expect.anything(),
		);
		await waitForLazy(() => expect(rowTexts(mounted)).toHaveLength(2));
		expect(button().exists()).toBe(false);
	});

	it('keeps the loaded rows and shows the failure when loading more fails', async () => {
		listOperations.mockResolvedValueOnce(page([op('a')], 'p2'));
		const mounted = await mountList();
		await waitForTable(mounted);
		listOperations.mockRejectedValueOnce(new AxiosError('Network Error'));
		await mounted.wrapper.find('[data-test="ops-load-more"]').trigger('click');
		await flushPromises();
		expect(
			mounted.wrapper.find('[data-test="ops-load-more-error"]').text(),
		).toContain('Cannot load more operations');
		expect(rowTexts(mounted)).toHaveLength(1);
		expect(mounted.wrapper.find('[data-test="ops-load-more"]').exists()).toBe(
			true,
		);
	});

	it('keeps the list and says the refresh failed when the API goes down later', async () => {
		listOperations.mockResolvedValueOnce(page([op('a')]));
		const mounted = await mountList();
		await waitForTable(mounted);
		listOperations.mockRejectedValueOnce(new AxiosError('Network Error'));
		await mounted.wrapper.find('[data-test="ops-refresh"]').trigger('click');
		await flushPromises();
		expect(
			mounted.wrapper.find('[data-test="ops-refresh-error"]').exists(),
		).toBe(true);
		expect(rowTexts(mounted)).toHaveLength(1);
		expect(mounted.wrapper.find('[data-test="ops-age"]').exists()).toBe(true);
	});
});
