import { flushPromises } from '@vue/test-utils';
import { describe, expect, it } from 'vitest';

import { mountWithApp } from '@/tests/mountWithApp';
import type { JournalFilter } from '@/utils/journalFilter';
import JournalFilterForm from './JournalFilterForm.vue';

const applied: JournalFilter = {
	types: ['ALARM'],
	datacenter: 'datacenters/dc_a',
	operation: 'operations/op-1',
};

const mountForm = async (
	field: 'type' | 'datacenter' | 'operation',
	props: Record<string, unknown> = {},
) => {
	const { wrapper } = await mountWithApp(JournalFilterForm, {
		field,
		filter: applied,
		datacenters: [],
		...props,
	});
	await flushPromises();
	return wrapper;
};

describe('JournalFilterForm', () => {
	it('applies the picked types in the order of the entry types', async () => {
		const wrapper = await mountForm('type', {
			filter: { ...applied, types: [] },
		});
		await wrapper.find('[data-test="filter-type-STEP"] input').setValue(true);
		await wrapper
			.find('[data-test="filter-type-OPERATION"] input')
			.setValue(true);
		await wrapper.find('[data-test="filter-apply"]').trigger('click');
		expect(wrapper.emitted('apply')?.[0]?.[0]).toEqual({
			types: ['OPERATION', 'STEP'],
			datacenter: 'datacenters/dc_a',
			operation: 'operations/op-1',
		});
	});

	it('starts from the applied types', async () => {
		const wrapper = await mountForm('type');
		const alarm = wrapper.find<HTMLInputElement>(
			'[data-test="filter-type-ALARM"] input',
		);
		expect(alarm.element.checked).toBe(true);
		const step = wrapper.find<HTMLInputElement>(
			'[data-test="filter-type-STEP"] input',
		);
		expect(step.element.checked).toBe(false);
	});

	it('applies the typed operation and keeps the other fields', async () => {
		const wrapper = await mountForm('operation', {
			filter: { ...applied, operation: '' },
		});
		await wrapper
			.find('[data-test="filter-operation"] input')
			.setValue('  op-2 ');
		await wrapper.find('[data-test="filter-apply"]').trigger('click');
		expect(wrapper.emitted('apply')?.[0]?.[0]).toEqual({
			types: ['ALARM'],
			datacenter: 'datacenters/dc_a',
			operation: '  op-2 ',
		});
	});

	it('applies on Enter in the operation field', async () => {
		const wrapper = await mountForm('operation');
		await wrapper
			.find('[data-test="filter-operation"] input')
			.trigger('keydown.enter');
		expect(wrapper.emitted('apply')).toHaveLength(1);
	});

	it('resets only its own field', async () => {
		const wrapper = await mountForm('operation');
		await wrapper.find('[data-test="filter-reset"]').trigger('click');
		expect(wrapper.emitted('apply')?.[0]?.[0]).toEqual({
			types: ['ALARM'],
			datacenter: 'datacenters/dc_a',
			operation: '',
		});
	});

	it('offers the known datacenters as a choice', async () => {
		const wrapper = await mountForm('datacenter', {
			datacenters: ['datacenters/dc_a', 'datacenters/dc_b'],
		});
		expect(wrapper.find('[data-test="filter-datacenter"]').exists()).toBe(true);
		expect(wrapper.find('[data-test="filter-datacenter-text"]').exists()).toBe(
			false,
		);
	});

	it('takes a datacenter id by hand while the datacenters are not known', async () => {
		const wrapper = await mountForm('datacenter', {
			filter: { ...applied, datacenter: '' },
		});
		expect(wrapper.text()).toContain('The datacenters are not known yet');
		await wrapper
			.find('[data-test="filter-datacenter-text"] input')
			.setValue('dc_b');
		await wrapper.find('[data-test="filter-apply"]').trigger('click');
		expect(wrapper.emitted('apply')?.[0]?.[0]).toMatchObject({
			datacenter: 'dc_b',
		});
	});

	it('lists an applied datacenter typed without the prefix once', async () => {
		const wrapper = await mountForm('datacenter', {
			filter: { ...applied, datacenter: 'dc_a' },
			datacenters: ['datacenters/dc_a', 'datacenters/dc_b'],
		});
		const select = wrapper.findComponent('[data-test="filter-datacenter"]');
		const { options } = (select as unknown as { vm: { $props: object } }).vm
			.$props as { options: { id: string }[] };
		expect(options.map((option) => option.id)).toEqual([
			'datacenters/dc_a',
			'datacenters/dc_b',
		]);
	});
});
