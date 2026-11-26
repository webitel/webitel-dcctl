import { flushPromises } from '@vue/test-utils';
import { HttpResponse, http } from 'msw';
import {
	afterAll,
	afterEach,
	beforeAll,
	beforeEach,
	describe,
	expect,
	it,
} from 'vitest';

import type { WebitelDcctlV1Operation as Operation } from '@/api/gen/_models';
import { type ActionVariant, setActionVariant } from '@/mocks/actionVariants';
import { MOCK_USER } from '@/mocks/handlers';
import { MOCK_OPERATION_IDS } from '@/mocks/operationVariants';
import { server } from '@/mocks/server';
import { useAuthStore } from '@/stores/auth';
import { mountWithApp } from '@/tests/mountWithApp';
import { waitForLazy } from '@/tests/waitForLazy';
import type { ActionKind } from '@/utils/actionAvailability';
import { encodeBasicToken } from '@/utils/basicAuth';
import ActionDialog from './ActionDialog.vue';

const TOKEN = encodeBasicToken(MOCK_USER.name, MOCK_USER.password);

interface Posted {
	url: string;
	body: Record<string, unknown>;
}

let posted: Posted[] = [];

beforeAll(() => {
	server.listen({ onUnhandledFrame: 'error' });
	server.events.on('request:start', ({ request }) => {
		if (request.method !== 'POST') return;
		const copy = request.clone();
		void copy.json().then((body) => {
			posted.push({ url: new URL(request.url).pathname, body });
		});
	});
});
beforeEach(() => {
	posted = [];
});
afterEach(() => {
	server.resetHandlers();
	setActionVariant('ok');
});
afterAll(() => server.close());

type Props = {
	kind: ActionKind;
	datacenter?: string;
	operation?: Operation;
	candidates?: { id: string; role?: string }[];
};

const open = async (variant: ActionVariant, props: Props) => {
	setActionVariant(variant);
	const mounted = await mountWithApp(ActionDialog, props, TOKEN);
	return mounted;
};

type Mounted = Awaited<ReturnType<typeof open>>;

const find = (mounted: Mounted, test: string) =>
	mounted.wrapper.find(`[data-test="${test}"]`);

const until = (mounted: Mounted, test: string) =>
	waitForLazy(() => expect(find(mounted, test).exists()).toBe(true));

const runButton = (mounted: Mounted) => find(mounted, 'action-run');

const runDisabled = (mounted: Mounted) =>
	runButton(mounted).attributes('disabled') !== undefined;

const typeName = async (mounted: Mounted, name: string) => {
	const input = mounted.wrapper.find('[data-test="action-typed"] input');
	await input.setValue(name);
};

const accept = async (mounted: Mounted, field: string) => {
	await mounted.wrapper
		.find(`[data-test="confirm-${field}"] input`)
		.setValue(true);
};

const run = async (mounted: Mounted) => {
	await runButton(mounted).trigger('click');
	await flushPromises();
};

const interrupted: Operation = {
	name: 'operations/switch-interrupted',
	type: 'SWITCH',
	state: 'INTERRUPTED',
	target: 'datacenters/dc_b',
	steps: [{ type: 'GENERATION', state: 'PENDING', point_of_no_return: true }],
};

describe('dry run', () => {
	it('shows the plan and the checks and calls with validate_only first', async () => {
		const mounted = await open('ok', { kind: 'switch', datacenter: 'dc_b' });
		await until(mounted, 'action-outcome-ok');

		expect(
			mounted.wrapper.findAll('[data-test="action-plan-step"]'),
		).toHaveLength(6);
		expect(find(mounted, 'action-plan').text()).toContain('point of no return');
		// A warning is shown, passed checks are folded away.
		expect(find(mounted, 'check-coord.quorum').exists()).toBe(true);
		const rest = find(mounted, 'checks-rest');
		expect(rest.find('[data-test="check-coord.lock_free"]').exists()).toBe(
			true,
		);
		expect(rest.find('[data-test="check-coord.quorum"]').exists()).toBe(false);
		expect(find(mounted, 'action-confirmations').exists()).toBe(false);
		expect(posted).toHaveLength(1);
		expect(posted[0].url).toBe('/dcctl/api/v1/datacenters/dc_b:switch');
		expect(posted[0].body).toMatchObject({ validate_only: true });
		expect(posted[0].body).not.toHaveProperty('confirmation');
	});

	it('waits for the dry run before offering the action', async () => {
		const mounted = await open('ok', {
			kind: 'markRecovered',
			datacenter: 'dc_b',
		});
		expect(find(mounted, 'action-checking').exists()).toBe(true);
		expect(runDisabled(mounted)).toBe(true);
		await until(mounted, 'action-outcome-ok');
		expect(runDisabled(mounted)).toBe(false);
	});
});

describe('typed name guard', () => {
	it('keeps the action off until the exact datacenter name is typed', async () => {
		const mounted = await open('ok', { kind: 'switch', datacenter: 'dc_b' });
		await until(mounted, 'action-outcome-ok');

		expect(find(mounted, 'action-typed').text()).toContain(
			'Type dc_b to confirm',
		);
		expect(runDisabled(mounted)).toBe(true);

		await typeName(mounted, 'dc_a');
		expect(runDisabled(mounted)).toBe(true);
		expect(find(mounted, 'action-typed-mismatch').exists()).toBe(true);

		await typeName(mounted, 'dc_b');
		expect(runDisabled(mounted)).toBe(false);
		expect(find(mounted, 'action-typed-mismatch').exists()).toBe(false);
	});

	it.each([
		['switch', true],
		['failover', true],
		['demote', true],
		['markRecovered', false],
		['syncApps', false],
		['abandon', false],
	] as const)('%s asks for the name: %s', async (kind, asked) => {
		const mounted = await open('ok', {
			kind,
			datacenter: 'dc_b',
			operation: kind === 'abandon' ? interrupted : undefined,
		});
		await until(mounted, 'action-outcome-ok');
		expect(find(mounted, 'action-typed').exists()).toBe(asked);
	});

	it('asks for the name on repoint only when the dry run needs a discard bound', async () => {
		const quiet = await open('ok', { kind: 'repoint', datacenter: 'dc_b' });
		await until(quiet, 'action-outcome-ok');
		expect(find(quiet, 'action-typed').exists()).toBe(false);

		const discarding = await open('confirmation', {
			kind: 'repoint',
			datacenter: 'dc_b',
		});
		await until(discarding, 'action-outcome-confirm');
		expect(find(discarding, 'action-typed').exists()).toBe(true);
	});
});

describe('success', () => {
	it('runs the real call with the same request id and opens the operation', async () => {
		const mounted = await open('ok', { kind: 'switch', datacenter: 'dc_b' });
		await until(mounted, 'action-outcome-ok');
		await typeName(mounted, 'dc_b');

		await run(mounted);

		await waitForLazy(() =>
			expect(mounted.router.currentRoute.value.name).toBe('operation'),
		);
		expect(mounted.router.currentRoute.value.params.id).toBe(
			MOCK_OPERATION_IDS.progress,
		);
		expect(posted).toHaveLength(2);
		expect(posted[1].body).toMatchObject({ validate_only: false });
		expect(posted[1].body.request_id).toMatch(/^[0-9a-f-]{36}$/);
		expect(posted[1].body.request_id).toBe(posted[0].body.request_id);
		expect(mounted.wrapper.emitted('done')).toHaveLength(1);
		expect(mounted.wrapper.emitted('close')).toHaveLength(1);
	});
});

describe('confirmations', () => {
	it('collects the required confirmations and sends them with the amount shown', async () => {
		const mounted = await open('confirmation', {
			kind: 'failover',
			datacenter: 'dc_b',
		});
		await until(mounted, 'action-outcome-confirm');

		expect(find(mounted, 'confirm-isolation').exists()).toBe(true);
		expect(find(mounted, 'confirm-max_discard_bytes').text()).toContain('1 kB');
		expect(find(mounted, 'action-mode').text()).toBe('Forced');
		// The plan comes from the preflight of the refusal.
		expect(
			mounted.wrapper.findAll('[data-test="action-plan-step"]'),
		).toHaveLength(6);

		await typeName(mounted, 'dc_b');
		expect(runDisabled(mounted)).toBe(true);
		await accept(mounted, 'isolation');
		expect(runDisabled(mounted)).toBe(true);
		await accept(mounted, 'max_discard_bytes');
		expect(runDisabled(mounted)).toBe(false);

		await run(mounted);
		await waitForLazy(() =>
			expect(mounted.router.currentRoute.value.name).toBe('operation'),
		);
		expect(posted[1].body).toMatchObject({
			validate_only: false,
			confirmation: { isolation: true, max_discard_bytes: '1024' },
		});
	});

	it('shows the missing confirmations again when the real call finds more', async () => {
		const mounted = await open('confirmation-grew', {
			kind: 'failover',
			datacenter: 'dc_b',
		});
		await until(mounted, 'action-outcome-confirm');
		await typeName(mounted, 'dc_b');
		await accept(mounted, 'isolation');
		await accept(mounted, 'max_discard_bytes');

		await run(mounted);

		await until(mounted, 'action-facts-changed');
		expect(mounted.router.currentRoute.value.name).not.toBe('operation');
		expect(find(mounted, 'confirm-max_discard_bytes').text()).toContain('2 kB');
		// The isolation is the same fact and stays given; the amount grew and must be given again.
		expect(
			find(mounted, 'confirm-isolation').find('input').element.checked,
		).toBe(true);
		expect(
			find(mounted, 'confirm-max_discard_bytes').find('input').element.checked,
		).toBe(false);
		expect(runDisabled(mounted)).toBe(true);

		await accept(mounted, 'max_discard_bytes');
		expect(runDisabled(mounted)).toBe(false);
		await run(mounted);
		await waitForLazy(() =>
			expect(mounted.router.currentRoute.value.name).toBe('operation'),
		);
		expect(posted.at(-1)?.body).toMatchObject({
			confirmation: { isolation: true, max_discard_bytes: '2048' },
		});
	});
});

describe('refusals', () => {
	it('shows the failing check and the violations, and no way to confirm, on a failed preflight', async () => {
		const mounted = await open('preflight-failed', {
			kind: 'failover',
			datacenter: 'dc_b',
		});
		await until(mounted, 'action-outcome-blocked');

		expect(find(mounted, 'check-patroni.target_lag').text()).toContain(
			'dc_b lags 52428800 bytes',
		);
		expect(find(mounted, 'violation-check').text()).toContain(
			'patroni.target_lag',
		);
		// A confirmation violation is information: nothing to tick, and the action stays off.
		expect(find(mounted, 'violation-confirmation').exists()).toBe(true);
		expect(find(mounted, 'action-confirmations').exists()).toBe(false);
		expect(mounted.wrapper.find('[data-test^="confirm-"]').exists()).toBe(
			false,
		);
		expect(find(mounted, 'action-pending').exists()).toBe(true);
		await typeName(mounted, 'dc_b');
		expect(runDisabled(mounted)).toBe(true);
		expect(find(mounted, 'action-check-again').exists()).toBe(true);
		expect(posted).toHaveLength(1);
	});

	it('keeps the dialog open with a clear message when the lock is busy', async () => {
		const mounted = await open('lock-busy', {
			kind: 'switch',
			datacenter: 'dc_b',
		});
		await until(mounted, 'action-outcome-ok');
		await typeName(mounted, 'dc_b');

		await run(mounted);

		await until(mounted, 'action-refusal');
		expect(find(mounted, 'action-refusal').text()).toContain(
			'Another instance holds the lock of the operations.',
		);
		expect(find(mounted, 'refusal-metadata').text()).toContain(
			'Held by: dcctl-b1',
		);
		expect(find(mounted, 'refusal-metadata').text()).toContain(
			'Datacenter: dc_b',
		);
		expect(find(mounted, 'action-refusal').text()).toContain('dcctl.lock.busy');
		expect(mounted.router.currentRoute.value.name).not.toBe('operation');
		expect(mounted.wrapper.emitted('close')).toBeUndefined();
		// The preflight of the refusal shows the failing lock check: check again before trying again.
		expect(find(mounted, 'check-coord.lock_free').exists()).toBe(true);
		expect(runDisabled(mounted)).toBe(true);
		expect(find(mounted, 'action-check-again').exists()).toBe(true);
	});

	it('links to the running operation', async () => {
		const mounted = await open('operation-running', {
			kind: 'switch',
			datacenter: 'dc_b',
		});
		await until(mounted, 'action-refusal');

		expect(find(mounted, 'action-refusal').text()).toContain(
			'Another operation is running or interrupted.',
		);
		expect(find(mounted, 'refusal-operation').attributes('href')).toContain(
			`/operations/${MOCK_OPERATION_IDS.progress}`,
		);
		expect(runDisabled(mounted)).toBe(true);
		expect(find(mounted, 'action-check-again').exists()).toBe(true);
	});

	it('words a 503 and keeps the server detail out of the way', async () => {
		const mounted = await open('unavailable', {
			kind: 'switch',
			datacenter: 'dc_b',
		});
		await until(mounted, 'action-refusal');

		expect(find(mounted, 'action-refusal').text()).toContain(
			'The instance that leads this action does not answer.',
		);
		expect(find(mounted, 'action-refusal').text()).toContain(
			'Instance: dcctl-b1',
		);
		expect(runDisabled(mounted)).toBe(true);
	});

	it('lists the violations of an invalid request through the shared formatter', async () => {
		const mounted = await open('invalid', {
			kind: 'switch',
			datacenter: 'dc_b',
		});
		await until(mounted, 'action-refusal');

		expect(find(mounted, 'violation-field').text()).toBe(
			'request_id: must be a UUID',
		);
	});

	it('checks again after a refusal', async () => {
		const mounted = await open('operation-running', {
			kind: 'switch',
			datacenter: 'dc_b',
		});
		await until(mounted, 'action-refusal');

		setActionVariant('ok');
		await find(mounted, 'action-check-again').trigger('click');

		await until(mounted, 'action-outcome-ok');
		expect(find(mounted, 'action-refusal').exists()).toBe(false);
		expect(posted).toHaveLength(2);
	});
});

describe('resolve', () => {
	it('asks for the winner first, then runs the dry run for it', async () => {
		const mounted = await open('ok', {
			kind: 'resolve',
			candidates: [
				{ id: 'dc_a', role: 'PRIMARY' },
				{ id: 'dc_b', role: 'STANDBY_LEADER' },
			],
		});
		await until(mounted, 'action-winner');
		expect(posted).toHaveLength(0);
		expect(find(mounted, 'action-winner').text()).toContain('Patroni: primary');
		expect(runDisabled(mounted)).toBe(true);

		await find(mounted, 'winner-dc_b').setValue(true);
		await until(mounted, 'action-outcome-ok');
		expect(posted[0].url).toBe('/dcctl/api/v1/datacenters:resolveConflict');
		expect(posted[0].body).toMatchObject({
			winner: 'datacenters/dc_b',
			validate_only: true,
		});

		expect(find(mounted, 'action-typed').text()).toContain(
			'Type dc_b to confirm',
		);
		await typeName(mounted, 'dc_b');
		expect(runDisabled(mounted)).toBe(false);
	});
});

describe('resume and abandon', () => {
	it('resume in forced mode needs the isolation and the target name', async () => {
		const mounted = await open('confirmation', {
			kind: 'resume',
			operation: interrupted,
		});
		await until(mounted, 'action-outcome-confirm');

		expect(find(mounted, 'action-resume-forced').exists()).toBe(true);
		expect(find(mounted, 'action-typed').text()).toContain(
			'Type dc_b to confirm',
		);
		await typeName(mounted, 'dc_b');
		await accept(mounted, 'isolation');
		expect(runDisabled(mounted)).toBe(false);

		await run(mounted);
		await waitForLazy(() =>
			expect(mounted.wrapper.emitted('done')).toHaveLength(1),
		);
		expect(posted[1].url).toBe(
			'/dcctl/api/v1/operations/switch-interrupted:resume',
		);
		expect(posted[1].body).toMatchObject({
			confirmation: { isolation: true },
		});
		expect(posted[1].body).not.toHaveProperty('request_id');
		expect(mounted.router.currentRoute.value.params.id).toBe(
			'switch-interrupted',
		);
	});

	it('abandon warns when the point of no return was passed', async () => {
		const passed: Operation = {
			...interrupted,
			steps: [
				{ type: 'GENERATION', state: 'SUCCEEDED', point_of_no_return: true },
			],
		};
		const mounted = await open('ok', { kind: 'abandon', operation: passed });
		await until(mounted, 'action-outcome-ok');
		expect(find(mounted, 'action-abandon-warning').text()).toContain(
			'The point of no return is passed',
		);

		const ahead = await open('ok', { kind: 'abandon', operation: interrupted });
		await until(ahead, 'action-outcome-ok');
		expect(find(ahead, 'action-abandon-warning').text()).toContain(
			'is not passed',
		);
	});
});

describe('facts that changed after a confirmation was given', () => {
	const discardBodies = () =>
		posted
			.map(
				(entry) =>
					(
						entry.body.confirmation as
							| { max_discard_bytes?: string }
							| undefined
					)?.max_discard_bytes,
			)
			.filter((value) => value !== undefined);

	it('drops the accepted amount when a refusal brings a larger one, and sends only what was shown', async () => {
		const mounted = await open('lock-busy-grew', {
			kind: 'demote',
			datacenter: 'dc_b',
		});
		await until(mounted, 'action-outcome-confirm');
		expect(find(mounted, 'confirm-max_discard_bytes').text()).toContain('1 kB');
		await typeName(mounted, 'dc_b');
		await accept(mounted, 'max_discard_bytes');
		expect(runDisabled(mounted)).toBe(false);

		await run(mounted);

		await until(mounted, 'action-facts-changed');
		expect(find(mounted, 'action-refusal').text()).toContain(
			'lock of the operations',
		);
		// The refusal carried a preflight with 5 GB: the tick is gone and the new amount is shown.
		expect(find(mounted, 'action-pending').text()).toContain('4.7 GB');
		expect(find(mounted, 'confirm-max_discard_bytes').exists()).toBe(false);
		expect(runDisabled(mounted)).toBe(true);
		expect(discardBodies()).toEqual(['1024']);
	});

	it('does not send the old tick with the new amount after checking again', async () => {
		const mounted = await open('lock-busy-grew', {
			kind: 'demote',
			datacenter: 'dc_b',
		});
		await until(mounted, 'action-outcome-confirm');
		await typeName(mounted, 'dc_b');
		await accept(mounted, 'max_discard_bytes');
		await run(mounted);
		await until(mounted, 'action-facts-changed');

		await find(mounted, 'action-check-again').trigger('click');
		await waitForLazy(() =>
			expect(find(mounted, 'action-refusal').exists()).toBe(false),
		);
		await until(mounted, 'action-outcome-confirm');

		expect(find(mounted, 'confirm-max_discard_bytes').text()).toContain(
			'4.7 GB',
		);
		expect(runDisabled(mounted)).toBe(true);
		// Only the amount of the first tick was ever sent.
		expect(discardBodies()).toEqual(['1024']);

		await accept(mounted, 'max_discard_bytes');
		expect(runDisabled(mounted)).toBe(false);
	});
});

describe('refusals of the real call', () => {
	it('shows a preflight that failed after the dry run passed, and checks again', async () => {
		const mounted = await open('preflight-failed-real', {
			kind: 'switch',
			datacenter: 'dc_b',
		});
		await until(mounted, 'action-outcome-ok');
		await typeName(mounted, 'dc_b');

		await run(mounted);

		await until(mounted, 'action-outcome-blocked');
		expect(find(mounted, 'check-patroni.target_lag').exists()).toBe(true);
		expect(runDisabled(mounted)).toBe(true);

		setActionVariant('ok');
		await find(mounted, 'action-check-again').trigger('click');
		await until(mounted, 'action-outcome-ok');
		expect(find(mounted, 'action-refusal').exists()).toBe(false);
		expect(runDisabled(mounted)).toBe(false);
	});

	it('words an operation that is no longer interrupted, and checks again', async () => {
		const mounted = await open('not-interrupted', {
			kind: 'abandon',
			operation: interrupted,
		});
		await until(mounted, 'action-outcome-ok');

		await run(mounted);

		await until(mounted, 'action-refusal');
		expect(find(mounted, 'action-refusal').text()).toContain(
			'The operation is not interrupted.',
		);
		expect(find(mounted, 'refusal-metadata').text()).toContain(
			'State: RUNNING',
		);
		expect(runDisabled(mounted)).toBe(true);

		setActionVariant('ok');
		await find(mounted, 'action-check-again').trigger('click');
		await until(mounted, 'action-outcome-ok');
		expect(find(mounted, 'action-refusal').exists()).toBe(false);
	});

	it('leaves a 401 to the login redirect: no refusal in the dialog', async () => {
		server.use(
			http.post('*/v1/datacenters/:dc\\:markRecovered', () =>
				HttpResponse.json(
					{ id: 'dcctl.auth.required', code: 401, detail: 'sign in' },
					{ status: 401 },
				),
			),
		);
		const mounted = await open('ok', {
			kind: 'markRecovered',
			datacenter: 'dc_b',
		});
		await waitForLazy(() =>
			expect(useAuthStore(mounted.pinia).isAuthenticated).toBe(false),
		);
		expect(find(mounted, 'action-refusal').exists()).toBe(false);
		expect(find(mounted, 'action-outcome-ok').exists()).toBe(false);
	});
});

describe('confirmations the dialog cannot give', () => {
	it('blocks the action and says why when a confirm check names an unknown confirmation', async () => {
		server.use(
			http.post('*/v1/datacenters/:dc\\:switch', () =>
				HttpResponse.json(
					{
						id: 'dcctl.confirmation.required',
						code: 400,
						detail: 'needs a confirmation',
						preflight: {
							mode: 'QUORUM',
							checks: [
								{
									id: 'future.check',
									result: 'FAIL',
									effect: 'CONFIRM',
									confirmation: 'something_new',
									detail: 'needs something new',
								},
							],
						},
					},
					{ status: 400 },
				),
			),
		);
		const mounted = await open('ok', { kind: 'switch', datacenter: 'dc_b' });
		await until(mounted, 'action-cannot-confirm');

		await typeName(mounted, 'dc_b');
		expect(runDisabled(mounted)).toBe(true);
		expect(find(mounted, 'check-future.check').text()).toContain(
			'needs something new',
		);
	});

	it('says why when the name to type is not known', async () => {
		setActionVariant('confirmation');
		const mounted = await open('confirmation', {
			kind: 'resume',
			operation: { ...interrupted, target: undefined },
		});
		await until(mounted, 'action-outcome-confirm');

		expect(find(mounted, 'action-no-target-name').exists()).toBe(true);
		expect(find(mounted, 'action-typed').exists()).toBe(false);
		expect(runDisabled(mounted)).toBe(true);
	});
});
