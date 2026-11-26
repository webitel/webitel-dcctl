import { flushPromises } from '@vue/test-utils';
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest';

import type { WebitelDcctlV1Operation as Operation } from '@/api/gen/_models';
import { setActionVariant } from '@/mocks/actionVariants';
import { MOCK_USER } from '@/mocks/handlers';
import { server } from '@/mocks/server';
import { mountWithApp } from '@/tests/mountWithApp';
import { waitForLazy } from '@/tests/waitForLazy';
import { encodeBasicToken } from '@/utils/basicAuth';
import OperationActions from './OperationActions.vue';

beforeAll(() => server.listen({ onUnhandledFrame: 'error' }));
afterEach(() => {
	server.resetHandlers();
	setActionVariant('ok');
});
afterAll(() => server.close());

const TOKEN = encodeBasicToken(MOCK_USER.name, MOCK_USER.password);

const interrupted = (extra: Operation = {}): Operation => ({
	name: 'operations/switch-interrupted',
	type: 'SWITCH',
	state: 'INTERRUPTED',
	target: 'datacenters/dc_b',
	steps: [
		{ type: 'APPS_PREFLIGHT', state: 'SUCCEEDED' },
		{
			index: 1,
			type: 'GENERATION',
			state: 'PENDING',
			point_of_no_return: true,
		},
	],
	...extra,
});

const mountActions = (operation: Operation) =>
	mountWithApp(
		OperationActions,
		{ operation },
		TOKEN,
		'/operations/switch-interrupted',
	);

type Mounted = Awaited<ReturnType<typeof mountActions>>;

const find = (mounted: Mounted, test: string) =>
	mounted.wrapper.find(`[data-test="${test}"]`);

const until = (mounted: Mounted, test: string) =>
	waitForLazy(() => expect(find(mounted, test).exists()).toBe(true));

const disabled = (mounted: Mounted, test: string) =>
	find(mounted, test).attributes('disabled') !== undefined;

describe('resume', () => {
	it('opens the dialog, runs it and reports that the operation changed', async () => {
		const mounted = await mountActions(interrupted());

		await find(mounted, 'action-resume').find('button').trigger('click');
		await until(mounted, 'action-outcome-ok');
		expect(find(mounted, 'action-title').text()).toBe('Resume the operation');
		// Not forced: no name to type.
		expect(find(mounted, 'action-typed').exists()).toBe(false);

		await find(mounted, 'action-run').trigger('click');
		await flushPromises();

		await waitForLazy(() =>
			expect(mounted.wrapper.emitted('done')).toHaveLength(1),
		);
		expect(find(mounted, 'action-dialog').exists()).toBe(false);
		expect(mounted.router.currentRoute.value.params.id).toBe(
			'switch-interrupted',
		);
	});

	it('asks for the isolation and the target name when it continues in forced mode', async () => {
		setActionVariant('confirmation');
		const mounted = await mountActions(interrupted());

		await find(mounted, 'action-resume').find('button').trigger('click');
		await until(mounted, 'action-outcome-confirm');

		expect(disabled(mounted, 'action-run')).toBe(true);
		expect(find(mounted, 'action-typed').text()).toContain(
			'Type dc_b to confirm',
		);
	});
});

describe('abandon', () => {
	it('warns that the point of no return is passed, from the steps that succeeded', async () => {
		const mounted = await mountActions(
			interrupted({
				steps: [
					{
						type: 'GENERATION',
						state: 'SUCCEEDED',
						point_of_no_return: true,
					},
				],
			}),
		);

		await find(mounted, 'action-abandon').find('button').trigger('click');
		await until(mounted, 'action-abandon-warning');

		expect(find(mounted, 'action-abandon-warning').text()).toContain(
			'The point of no return is passed',
		);
	});

	it('warns that it may have been passed while the step runs', async () => {
		const mounted = await mountActions(
			interrupted({
				steps: [
					{
						type: 'GENERATION',
						state: 'RUNNING',
						point_of_no_return: true,
					},
				],
			}),
		);

		await find(mounted, 'action-abandon').find('button').trigger('click');
		await until(mounted, 'action-abandon-warning');

		expect(find(mounted, 'action-abandon-warning').text()).toContain(
			'may have been passed',
		);
		expect(
			find(mounted, 'action-abandon-warning').attributes('data-level'),
		).toBe('maybe');
	});

	it('runs without typing a name and reports the change', async () => {
		const mounted = await mountActions(interrupted());

		await find(mounted, 'action-abandon').find('button').trigger('click');
		await until(mounted, 'action-outcome-ok');
		expect(find(mounted, 'action-typed').exists()).toBe(false);
		expect(disabled(mounted, 'action-run')).toBe(false);

		await find(mounted, 'action-run').trigger('click');
		await flushPromises();

		await waitForLazy(() =>
			expect(mounted.wrapper.emitted('done')).toHaveLength(1),
		);
	});

	it('keeps the dialog open when the operation is no longer interrupted', async () => {
		setActionVariant('operation-running');
		const mounted = await mountActions(interrupted());

		await find(mounted, 'action-abandon').find('button').trigger('click');
		await until(mounted, 'action-refusal');

		expect(find(mounted, 'action-refusal').text()).toContain(
			'Another operation is running or interrupted.',
		);
		expect(disabled(mounted, 'action-run')).toBe(true);
	});
});
