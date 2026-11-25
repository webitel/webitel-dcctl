import { flushPromises } from '@vue/test-utils';
import { describe, expect, it } from 'vitest';

import type { WebitelDcctlV1Status as Status } from '@/api/gen/_models';
import { useStatusStore } from '@/stores/status';
import { mountWithApp } from '@/tests/mountWithApp';
import AlarmList from './AlarmList.vue';

const mountAlarms = async (status: Status) => {
	const { wrapper, pinia } = await mountWithApp(AlarmList);
	useStatusStore(pinia).status = status;
	await flushPromises();
	return wrapper;
};

describe('AlarmList', () => {
	it('shows critical alarms first and the oldest first within a level', async () => {
		const wrapper = await mountAlarms({
			alarms: [
				{
					type: 'APPS_DEGRADED',
					severity: 'WARNING',
					datacenter: 'datacenters/dc_a',
					start_time: '2026-11-23T10:00:00Z',
					detail: 'warn-old',
				},
				{
					type: 'DB_SPLIT',
					severity: 'CRITICAL',
					datacenter: 'datacenters/dc_b',
					start_time: '2026-11-23T10:10:00Z',
					detail: 'crit-new',
				},
				{
					type: 'DATACENTER_UNREACHABLE',
					severity: 'CRITICAL',
					datacenter: 'datacenters/dc_a',
					start_time: '2026-11-23T10:05:00Z',
					detail: 'crit-old',
				},
			],
		});

		const details = wrapper
			.findAll('[data-test="alarm"]')
			.map((row) => row.find('[data-test="alarm-detail"]').text());
		expect(details).toEqual(['crit-old', 'crit-new', 'warn-old']);
	});

	it('colours critical red and warning yellow', async () => {
		const wrapper = await mountAlarms({
			alarms: [
				{
					type: 'DB_SPLIT',
					severity: 'CRITICAL',
					datacenter: 'datacenters/dc_b',
				},
				{ type: 'STALE', severity: 'WARNING', datacenter: 'datacenters/dc_a' },
			],
		});

		const chips = wrapper.findAll('[data-test="alarm-severity"]');
		expect(chips[0].classes()).toContain('p-chip-error');
		expect(chips[1].classes()).toContain('p-chip-warning');
	});

	it('links a datacenter alarm to the datacenter page', async () => {
		const wrapper = await mountAlarms({
			alarms: [
				{
					type: 'DB_SPLIT',
					severity: 'CRITICAL',
					datacenter: 'datacenters/dc_b',
				},
			],
		});

		expect(wrapper.find('[data-test="alarm"] a').attributes('href')).toBe(
			'/dcctl/datacenters/dc_b',
		);
	});

	it('links an interrupted-operation alarm to the operation', async () => {
		const wrapper = await mountAlarms({
			operation: { name: 'operations/0b9f', state: 'INTERRUPTED' },
			alarms: [{ type: 'OPERATION_INTERRUPTED', severity: 'CRITICAL' }],
		});

		expect(wrapper.find('[data-test="alarm"] a').attributes('href')).toBe(
			'/dcctl/operations/0b9f',
		);
	});

	it('links a quorum alarm to the coordination block', async () => {
		const wrapper = await mountAlarms({
			alarms: [{ type: 'QUORUM_LOST', severity: 'CRITICAL' }],
		});

		expect(wrapper.find('[data-test="alarm"] a').attributes('href')).toBe(
			'/dcctl/#coordination',
		);
	});

	it('makes the alarm type the link, so every link says what it opens', async () => {
		const wrapper = await mountAlarms({
			alarms: [
				{
					type: 'DB_SPLIT',
					severity: 'CRITICAL',
					datacenter: 'datacenters/dc_b',
				},
			],
		});

		expect(wrapper.find('[data-test="alarm"] a').text()).toBe('Database split');
	});

	it('shows one row when there are no alarms', async () => {
		const wrapper = await mountAlarms({ alarms: [] });

		expect(wrapper.findAll('[data-test="alarm"]')).toHaveLength(0);
		expect(wrapper.text()).toContain('No alarms');
	});

	it('shows an unknown alarm type as it is', async () => {
		const wrapper = await mountAlarms({
			alarms: [{ type: 'FUTURE_THING' as never, severity: 'WARNING' }],
		});

		expect(wrapper.text()).toContain('FUTURE_THING');
	});
});
