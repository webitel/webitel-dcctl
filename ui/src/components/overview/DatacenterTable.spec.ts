import { flushPromises } from '@vue/test-utils';
import { describe, expect, it } from 'vitest';

import type { WebitelDcctlV1Status as Status } from '@/api/gen/_models';
import { buildStatus } from '@/mocks/statusVariants';
import { useStatusStore } from '@/stores/status';
import { mountWithApp } from '@/tests/mountWithApp';
import { waitForLazy } from '@/tests/waitForLazy';
import DatacenterTable from './DatacenterTable.vue';

const mountTable = async (status: Status, now = Date.now()) => {
	const { wrapper, pinia } = await mountWithApp(DatacenterTable);
	const store = useStatusStore(pinia);
	store.now = now;
	store.status = status;
	await flushPromises();
	await waitForLazy(() =>
		expect(wrapper.find('.wt-table').exists()).toBe(true),
	);
	return wrapper;
};

const rowOf = (wrapper: Awaited<ReturnType<typeof mountTable>>, dc: string) =>
	wrapper.find(`.dc-row--${dc}`);

describe('DatacenterTable', () => {
	it('lists the active datacenter first, then the rest by name', async () => {
		const wrapper = await mountTable({
			active_datacenter: 'datacenters/dc_b',
			datacenters: [
				{ name: 'datacenters/dc_c', state: 'STANDBY' },
				{ name: 'datacenters/dc_a', state: 'STANDBY' },
				{ name: 'datacenters/dc_b', state: 'ACTIVE' },
			],
		});

		const names = wrapper
			.findAll('[data-test="dc-name"]')
			.map((cell) => cell.text());
		expect(names).toEqual(['dc_b', 'dc_a', 'dc_c']);
	});

	it('keeps an unreachable active datacenter first and red', async () => {
		const wrapper = await mountTable(buildStatus('unreachable'));

		const names = wrapper
			.findAll('[data-test="dc-name"]')
			.map((cell) => cell.text());
		expect(names[0]).toBe('dc_a');
		expect(
			rowOf(wrapper, 'dc_a').find('[data-test="dc-state"]').classes(),
		).toContain('p-chip-error');
	});

	it('shows an unreachable standby datacenter gray', async () => {
		const wrapper = await mountTable({
			active_datacenter: 'datacenters/dc_a',
			datacenters: [
				{ name: 'datacenters/dc_a', state: 'ACTIVE' },
				{ name: 'datacenters/dc_b', state: 'UNREACHABLE' },
			],
		});

		expect(
			rowOf(wrapper, 'dc_b').find('[data-test="dc-state"]').classes(),
		).toContain('p-chip-secondary');
	});

	it.each([
		['ACTIVE', 'p-chip-info'],
		['DB_SPLIT', 'p-chip-error'],
		['SPLIT_BRAIN', 'p-chip-error'],
		['STALE', 'p-chip-warning'],
		['NEEDS_RECOVERY', 'p-chip-warning'],
		['STANDBY', 'p-chip-secondary'],
	] as const)('colours state %s as %s', async (state, chipClass) => {
		const wrapper = await mountTable({
			active_datacenter: 'datacenters/dc_x',
			datacenters: [{ name: 'datacenters/dc_b', state }],
		});

		expect(
			rowOf(wrapper, 'dc_b').find('[data-test="dc-state"]').classes(),
		).toContain(chipClass);
	});

	it.each([
		['OK', 'p-chip-success'],
		['DEGRADED', 'p-chip-warning'],
		['DOWN', 'p-chip-error'],
		['UNKNOWN', 'p-chip-secondary'],
	] as const)('colours health %s as %s', async (health, chipClass) => {
		const wrapper = await mountTable({
			datacenters: [{ name: 'datacenters/dc_b', health }],
		});

		expect(
			rowOf(wrapper, 'dc_b').find('[data-test="dc-health"]').classes(),
		).toContain(chipClass);
	});

	it('shows the Patroni leader with role, timeline and lag', async () => {
		const wrapper = await mountTable({
			datacenters: [
				{
					name: 'datacenters/dc_b',
					patroni: {
						leader: 'pg-b1',
						role: 'STANDBY_LEADER',
						timeline: '5',
						lag_bytes: '1024',
					},
				},
			],
		});

		const cell = rowOf(wrapper, 'dc_b').find('[data-test="dc-patroni"]').text();
		expect(cell).toContain('pg-b1');
		expect(cell).toContain('standby leader');
		expect(cell).toContain('timeline 5');
		expect(cell).toContain('lag 1 kB');
	});

	it('shows a lag of 0 of a caught-up standby, and no lag that is not known', async () => {
		const wrapper = await mountTable({
			datacenters: [
				{
					name: 'datacenters/dc_b',
					patroni: { leader: 'pg-b1', role: 'STANDBY_LEADER', lag_bytes: '0' },
				},
				{
					name: 'datacenters/dc_c',
					patroni: { leader: 'pg-c1', role: 'STANDBY_LEADER' },
				},
			],
		});

		expect(
			rowOf(wrapper, 'dc_b').find('[data-test="dc-patroni"]').text(),
		).toContain('lag 0 byte');
		expect(
			rowOf(wrapper, 'dc_c').find('[data-test="dc-patroni"]').text(),
		).not.toContain('lag');
	});

	it('shows healthy over all managed apps and reachable over all sources', async () => {
		const wrapper = await mountTable({
			datacenters: [
				{
					name: 'datacenters/dc_a',
					nomad: { job_counts: { healthy_count: 10, unhealthy_count: 2 } },
					sources: [
						{ system: 'CONSUL', reachable: true },
						{ system: 'NOMAD', reachable: true },
						{ system: 'PATRONI', reachable: false },
					],
				},
			],
		});

		const row = rowOf(wrapper, 'dc_a');
		expect(row.find('[data-test="dc-apps"]').text()).toBe('10 / 12');
		expect(row.find('[data-test="dc-sources"]').text()).toContain('2 / 3');
	});

	it('marks unreachable sources with the age of their last data', async () => {
		const now = Date.parse('2026-11-23T10:15:00Z');
		const wrapper = await mountTable(
			{
				datacenters: [
					{
						name: 'datacenters/dc_a',
						sources: [
							{
								system: 'NOMAD',
								reachable: false,
								observe_time: '2026-11-23T10:12:00Z',
							},
						],
					},
				],
			},
			now,
		);

		const text = rowOf(wrapper, 'dc_a').find('[data-test="dc-sources"]').text();
		expect(text).toContain('not answering');
		expect(text).toContain('Nomad');
		expect(text).toContain('3 min');
	});

	it('shows a dash for apps when Nomad data is missing', async () => {
		const wrapper = await mountTable({
			datacenters: [{ name: 'datacenters/dc_a' }],
		});

		expect(rowOf(wrapper, 'dc_a').find('[data-test="dc-apps"]').text()).toBe(
			'no data',
		);
	});

	it('links the datacenter name to its page', async () => {
		const wrapper = await mountTable({
			datacenters: [{ name: 'datacenters/dc_b' }],
		});

		expect(rowOf(wrapper, 'dc_b').find('a').attributes('href')).toBe(
			'/dcctl/datacenters/dc_b',
		);
	});

	it('has the anchor the alarms link to', async () => {
		const wrapper = await mountTable({
			datacenters: [{ name: 'datacenters/dc_a' }],
		});

		expect(wrapper.find('#datacenters').exists()).toBe(true);
	});

	it('shows a row for every datacenter of the real example', async () => {
		const wrapper = await mountTable(buildStatus('normal'));

		expect(wrapper.findAll('[data-test="dc-name"]')).toHaveLength(2);
	});
});
