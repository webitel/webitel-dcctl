import { flushPromises } from '@vue/test-utils';
import { describe, expect, it } from 'vitest';

import type { WebitelDcctlV1Status as Status } from '@/api/gen/_models';
import { buildStatus } from '@/mocks/statusVariants';
import { useStatusStore } from '@/stores/status';
import { mountWithApp } from '@/tests/mountWithApp';
import { waitForLazy } from '@/tests/waitForLazy';
import CoordinationBlock from './CoordinationBlock.vue';

const mountBlock = async (status: Status) => {
	const { wrapper, pinia } = await mountWithApp(CoordinationBlock);
	useStatusStore(pinia).status = status;
	await flushPromises();
	await waitForLazy(() =>
		expect(wrapper.find('.wt-table').exists()).toBe(true),
	);
	return wrapper;
};

const field = (wrapper: Awaited<ReturnType<typeof mountBlock>>, name: string) =>
	wrapper.find(`[data-test="${name}"]`).text();

describe('CoordinationBlock', () => {
	it('has the anchor the alarms link to', async () => {
		const wrapper = await mountBlock({});

		expect(wrapper.find('#coordination').exists()).toBe(true);
	});

	it('shows generation, forced, quorum and lease', async () => {
		const wrapper = await mountBlock(buildStatus('normal'));

		expect(field(wrapper, 'generation')).toBe('7');
		expect(field(wrapper, 'forced')).toBe('no');
		expect(field(wrapper, 'quorum')).toContain('2 of 3 reachable, 2 needed');
		expect(field(wrapper, 'quorum')).toContain('quorum');
		expect(field(wrapper, 'lease')).toContain('holder dc_a');
	});

	it('stresses a forced generation', async () => {
		const wrapper = await mountBlock({
			...buildStatus('normal'),
			forced: true,
		});

		expect(field(wrapper, 'forced')).toContain('yes');
		expect(wrapper.find('[data-test="forced"]').classes()).toContain(
			'p-chip-error',
		);
	});

	it('lists the voters with reachability, generation and active datacenter', async () => {
		const wrapper = await mountBlock({
			voters: [
				{
					type: 'DATACENTER',
					datacenter: 'datacenters/dc_a',
					reachable: true,
					generation: '7',
					active_datacenter: 'datacenters/dc_a',
				},
				{
					type: 'ARBITER',
					reachable: false,
					error: 'TIMEOUT',
					generation: '6',
					active_datacenter: 'datacenters/dc_a',
				},
			],
		});

		const rows = wrapper.findAll('.voter-row');
		expect(rows).toHaveLength(2);
		expect(rows[0].text()).toContain('dc_a');
		expect(rows[0].text()).toContain('Reachable');
		expect(rows[1].text()).toContain('arbiter');
		expect(rows[1].text()).toContain('Unreachable');
		expect(rows[1].text()).toContain('6');
	});

	it('flags a lost lease', async () => {
		const wrapper = await mountBlock({
			lease: { holder: 'datacenters/dc_a', age: '45.000s', lost: true },
		});

		expect(field(wrapper, 'lease')).toContain('lost');
	});

	it('says there is no data, not that there is no quorum, when the objects are missing', async () => {
		const wrapper = await mountBlock({});

		expect(field(wrapper, 'lease')).toBe('no data');
		expect(field(wrapper, 'quorum')).toBe('no data');
	});

	it.each([
		['available quorum', { available: true, mode: 'QUORUM' }, 'p-chip-success'],
		[
			'forced-only mode',
			{ available: true, mode: 'FORCED_ONLY' },
			'p-chip-error',
		],
		['no majority', { available: false, mode: 'QUORUM' }, 'p-chip-error'],
	] as const)('shows %s as %s', async (_name, quorum, chipClass) => {
		const wrapper = await mountBlock({
			quorum: {
				...quorum,
				reachable_count: 1,
				required_count: 2,
				total_count: 3,
			},
		});

		expect(wrapper.find('[data-test="quorum"]').classes()).toContain(chipClass);
	});

	it('offers the arbiter initialization only while the arbiter is uninitialized', async () => {
		const without = await mountBlock(buildStatus('normal'));
		expect(without.find('[data-test="action-initArbiter"]').exists()).toBe(
			false,
		);

		const wrapper = await mountBlock({
			...buildStatus('normal'),
			alarms: [{ type: 'ARBITER_UNINITIALIZED', severity: 'CRITICAL' }],
		});
		expect(wrapper.find('[data-test="action-initArbiter"]').exists()).toBe(
			true,
		);
	});
});
