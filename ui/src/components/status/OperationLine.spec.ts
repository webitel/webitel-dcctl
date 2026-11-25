import { flushPromises } from '@vue/test-utils';
import { describe, expect, it } from 'vitest';

import type { WebitelDcctlV1Operation as Operation } from '@/api/gen/_models';
import { useStatusStore } from '@/stores/status';
import { mountWithApp } from '@/tests/mountWithApp';
import OperationLine from './OperationLine.vue';

const operation = (overrides: Partial<Operation> = {}): Operation => ({
	name: 'operations/0b9f',
	type: 'SWITCH',
	state: 'RUNNING',
	target: 'datacenters/dc_b',
	steps: [
		{ index: 0, type: 'APPS_PREFLIGHT', state: 'SUCCEEDED' },
		{ index: 1, type: 'APPS_STOP', state: 'RUNNING' },
		{ index: 2, type: 'GENERATION', state: 'PENDING' },
	],
	...overrides,
});

const mountLine = async (op?: Operation) => {
	const { wrapper, pinia, router } = await mountWithApp(OperationLine);
	useStatusStore(pinia).status = op ? { operation: op } : {};
	await flushPromises();
	return { wrapper, router };
};

describe('OperationLine', () => {
	it('shows type, target, current step and a link for a running operation', async () => {
		const { wrapper } = await mountLine(operation());

		const text = wrapper.text();
		expect(text).toContain('Switch');
		expect(text).toContain('dc_b');
		expect(text).toContain('step 2 of 3: Stop apps');
		expect(text).toContain('running');
		expect(wrapper.find('a').attributes('href')).toBe('/dcctl/operations/0b9f');
	});

	it('is shown for an interrupted operation', async () => {
		const { wrapper } = await mountLine(
			operation({
				state: 'INTERRUPTED',
				steps: [
					{ index: 0, type: 'APPS_STOP', state: 'FAILED' },
					{ index: 1, type: 'GENERATION', state: 'PENDING' },
				],
			}),
		);

		expect(wrapper.find('[data-test="operation-line"]').exists()).toBe(true);
		expect(wrapper.text()).toContain('interrupted');
		expect(wrapper.text()).toContain('step 1 of 2: Stop apps');
	});

	it.each(['SUCCEEDED', 'FAILED', 'ABANDONED'] as const)(
		'is hidden for a %s operation',
		async (state) => {
			const { wrapper } = await mountLine(operation({ state }));

			expect(wrapper.find('[data-test="operation-line"]').exists()).toBe(false);
		},
	);

	it('is hidden when there is no operation', async () => {
		const { wrapper } = await mountLine(undefined);

		expect(wrapper.find('[data-test="operation-line"]').exists()).toBe(false);
	});

	it('does not break on an operation without steps', async () => {
		const { wrapper } = await mountLine(operation({ steps: undefined }));

		expect(wrapper.text()).toContain('no step yet');
	});

	it('disappears once the operation finishes', async () => {
		const { wrapper } = await mountLine(operation());
		expect(wrapper.find('[data-test="operation-line"]').exists()).toBe(true);

		const store = useStatusStore();
		store.status = { operation: operation({ state: 'SUCCEEDED' }) };
		await flushPromises();

		expect(wrapper.find('[data-test="operation-line"]').exists()).toBe(false);
	});
});
