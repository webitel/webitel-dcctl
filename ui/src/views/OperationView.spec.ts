import { flushPromises } from '@vue/test-utils';
import { AxiosError } from 'axios';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { recordServerDate, resetServerClock } from '@/api/client/serverClock';
import type {
	WebitelDcctlV1Datacenter as Datacenter,
	WebitelDcctlV1Operation as Operation,
	WebitelDcctlV1OperationStep as Step,
} from '@/api/gen/_models';
import { reconnectDelayMs } from '@/composables/useOperation';
import { buildDatacenter } from '@/mocks/datacenterVariants';
import { useStatusStore } from '@/stores/status';
import { mountWithApp } from '@/tests/mountWithApp';
import { waitForLazy } from '@/tests/waitForLazy';
import OperationView from './OperationView.vue';

const getOperation = vi.hoisted(() => vi.fn());
vi.mock('@/api/gen/operation-service/operation-service', () => ({
	getOperationService: () => ({ getOperation }),
}));

const getDatacenter = vi.hoisted(() => vi.fn());
vi.mock('@/api/gen/datacenter-service/datacenter-service', () => ({
	getDatacenterService: () => ({ getDatacenter }),
}));

const watch = vi.hoisted(() => vi.fn());
vi.mock('@/api/client/watchStream', async (importActual) => ({
	...(await importActual<typeof import('@/api/client/watchStream')>()),
	watchOperation: watch,
}));

const NOW = Date.parse('2026-11-23T10:15:30Z');
const at = (second: number) => new Date(NOW + second * 1000).toISOString();

const apiError = (status: number, id?: string) =>
	new AxiosError('failed', 'ERR_BAD_REQUEST', undefined, null, {
		status,
		data: id ? { id, code: status, detail: `mock ${id}` } : {},
		statusText: '',
		headers: {},
		config: {} as never,
	});

const operation = (extra: Operation = {}): Operation => ({
	name: 'operations/0b9f',
	type: 'SWITCH',
	state: 'RUNNING',
	target: 'datacenters/dc_b',
	mode: 'QUORUM',
	previous_generation: '7',
	new_generation: '8',
	initiator: {
		user: 'support',
		address: '10.1.0.15',
		instance: { node: 'dcctl-a1' },
	},
	leader: { node: 'dcctl-b1', datacenter: 'datacenters/dc_b' },
	create_time: at(-30),
	update_time: at(-1),
	...extra,
});

const steps: Step[] = [
	{
		type: 'APPS_PREFLIGHT',
		state: 'SUCCEEDED',
		start_time: at(-28),
		end_time: at(-26),
	},
	{
		index: 1,
		type: 'APPS_STOP',
		stage: 3,
		datacenter: 'datacenters/dc_a',
		state: 'RUNNING',
		start_time: at(-26),
		detail: 'stopping 1 of 3 jobs',
	},
	{
		index: 2,
		type: 'PATRONI_DEMOTE',
		datacenter: 'datacenters/dc_a',
		state: 'PENDING',
	},
	{ index: 3, type: 'GENERATION', state: 'PENDING', point_of_no_return: true },
];

interface Stream {
	push: (op: Operation) => void;
	end: () => void;
	fail: (cause: unknown) => void;
	signal: AbortSignal;
}
const streams: Stream[] = [];
const lastStream = () => streams[streams.length - 1] as Stream;

type Mounted = Awaited<ReturnType<typeof mountWithApp>>;

const mountPage = async (id = '0b9f') => {
	const mounted = await mountWithApp(
		OperationView,
		{},
		undefined,
		`/operations/${id}`,
	);
	await flushPromises();
	return mounted;
};

const text = (mounted: Mounted, selector: string) =>
	mounted.wrapper.find(selector).text().replace(/\s+/g, ' ');

const waitForSteps = (mounted: Mounted) =>
	waitForLazy(() =>
		expect(
			mounted.wrapper.find('[data-test="op-steps"] tbody tr').exists(),
		).toBe(true),
	);

const stepRows = (mounted: Mounted) =>
	mounted.wrapper.findAll('[data-test="op-steps"] tbody tr.op-step');

const mountWith = async (op: Operation) => {
	getOperation.mockResolvedValue(op);
	const mounted = await mountPage();
	await waitForSteps(mounted);
	return mounted;
};

beforeEach(() => {
	vi.useFakeTimers({
		toFake: [
			'setTimeout',
			'clearTimeout',
			'setInterval',
			'clearInterval',
			'Date',
		],
	});
	vi.setSystemTime(NOW);
	getOperation.mockReset();
	getDatacenter.mockReset();
	watch.mockReset();
	streams.length = 0;
	watch.mockImplementation(
		(
			_id: string,
			options: { signal: AbortSignal; onOperation: (op: Operation) => void },
		) =>
			new Promise<void>((resolve, reject) => {
				options.signal.addEventListener('abort', () =>
					reject(new DOMException('aborted', 'AbortError')),
				);
				streams.push({
					push: options.onOperation,
					end: resolve,
					fail: reject,
					signal: options.signal,
				});
			}),
	);
	Element.prototype.scrollIntoView = vi.fn();
});

afterEach(() => {
	resetServerClock();
	vi.useRealTimers();
	vi.restoreAllMocks();
});

describe('OperationView states', () => {
	it('says it is loading and shows no header or steps while the operation is read', async () => {
		getOperation.mockReturnValue(new Promise(() => undefined));
		const mounted = await mountPage();
		expect(mounted.wrapper.find('[data-test="op-loading"]').exists()).toBe(
			true,
		);
		expect(mounted.wrapper.find('[data-test="op-steps"]').exists()).toBe(false);
		expect(mounted.wrapper.text()).not.toContain('no steps');
	});

	it('shows the error and that it tries again when the API does not answer', async () => {
		getOperation.mockRejectedValue(new Error('Network Error'));
		const mounted = await mountPage();
		const message = text(mounted, '[data-test="op-unavailable"]');
		expect(message).toContain('Cannot get the operation');
		expect(message).toContain('Network Error');
		expect(message).toContain('Trying again');

		getOperation.mockResolvedValue(operation({ steps }));
		await vi.advanceTimersByTimeAsync(reconnectDelayMs(0));
		await flushPromises();
		expect(mounted.wrapper.find('[data-test="op-unavailable"]').exists()).toBe(
			false,
		);
		expect(mounted.wrapper.find('[data-test="op-title"]').exists()).toBe(true);
	});

	it('says the operation does not exist, with a way back, on 404', async () => {
		getOperation.mockRejectedValue(apiError(404, 'dcctl.operation.not_found'));
		const mounted = await mountPage('gone');
		expect(text(mounted, '[data-test="op-not-found"]')).toContain(
			'Operation gone does not exist',
		);
		expect(
			mounted.wrapper.find('[data-test="op-back"]').attributes('href'),
		).toBe('/dcctl/operations');
		expect(mounted.wrapper.find('[data-test="op-title"]').exists()).toBe(false);
	});

	it('shows the operation: header, warnings, error and steps', async () => {
		const mounted = await mountWith(
			operation({
				state: 'FAILED',
				steps,
				end_time: at(0),
				warnings: ['apps of dc_a were left without a redirect'],
				error: {
					id: 'dcctl.preflight.failed',
					detail: 'the checks of the plan failed',
					metadata: { datacenter: 'datacenters/dc_b' },
				},
			}),
		);
		expect(text(mounted, '[data-test="op-title"]')).toBe('Switch');
		expect(text(mounted, '[data-test="op-state"]')).toBe('Failed');
		expect(text(mounted, '[data-test="op-mode"]')).toBe('Quorum');
		expect(text(mounted, '[data-test="op-target"]')).toBe('dc_b');
		expect(text(mounted, '[data-test="op-generation"]')).toContain('7 → 8');
		expect(text(mounted, '[data-test="op-initiator"]')).toBe(
			'support (10.1.0.15) via dcctl-a1',
		);
		expect(text(mounted, '[data-test="op-leader"]')).toBe('dcctl-b1 (dc_b)');
		expect(mounted.wrapper.find('[data-test="op-created"]').exists()).toBe(
			true,
		);
		expect(mounted.wrapper.find('[data-test="op-finished"]').exists()).toBe(
			true,
		);
		expect(text(mounted, '[data-test="op-duration"]')).toBe('30 s');
		expect(text(mounted, '[data-test="op-warnings"]')).toContain(
			'left without a redirect',
		);
		const error = text(mounted, '[data-test="op-error"]');
		expect(error).toContain('dcctl.preflight.failed');
		expect(error).toContain('the checks of the plan failed');
		expect(error).toContain('datacenter: datacenters/dc_b');
	});

	it('shows no warnings or error blocks when the operation has none', async () => {
		const mounted = await mountWith(operation({ steps }));
		expect(mounted.wrapper.find('[data-test="op-warnings"]').exists()).toBe(
			false,
		);
		expect(mounted.wrapper.find('[data-test="op-error"]').exists()).toBe(false);
	});
});

describe('OperationView steps', () => {
	it('lists the steps in plan order with state, duration and details', async () => {
		const mounted = await mountWith(operation({ steps: [...steps].reverse() }));
		const rows = stepRows(mounted).map((row) => row.text());
		expect(rows).toHaveLength(4);
		expect(rows[0]).toContain('Apps preflight');
		expect(rows[0]).toContain('Succeeded');
		expect(rows[0]).toContain('2 s');
		expect(rows[1]).toContain('Stop apps');
		expect(rows[1]).toContain('dc_a · stage 3');
		expect(rows[1]).toContain('Running');
		expect(rows[1]).toContain('stopping 1 of 3 jobs');
		expect(rows[2]).toContain('Demote Patroni');
		expect(rows[3]).toContain('Write generation');
	});

	it('has a label for the Patroni failover step and shows an unknown step type as it is', async () => {
		const mounted = await mountWith(
			operation({
				type: 'FAILOVER',
				mode: 'FORCED',
				steps: [
					{
						type: 'PATRONI_FAILOVER',
						state: 'RUNNING',
						datacenter: 'datacenters/dc_b',
					},
					{ index: 1, type: 'PATRONI_NEW_THING' as never, state: 'PENDING' },
				],
			}),
		);
		const rows = stepRows(mounted).map((row) => row.text());
		expect(rows[0]).toContain('Patroni failover');
		expect(rows[1]).toContain('PATRONI_NEW_THING');
		expect(text(mounted, '[data-test="op-title"]')).toBe('Failover');
		expect(text(mounted, '[data-test="op-mode"]')).toBe('Forced');
	});

	it('shows the warnings of a step that succeeded', async () => {
		const mounted = await mountWith(
			operation({
				steps: [
					{
						type: 'VERIFY',
						state: 'SUCCEEDED',
						warnings: ['pg-b1 is 4 KB behind'],
					},
				],
			}),
		);
		expect(text(mounted, '[data-test="step-warnings"]')).toContain(
			'pg-b1 is 4 KB behind',
		);
	});

	it('says there are no steps yet instead of an empty table', async () => {
		getOperation.mockResolvedValue(operation());
		const mounted = await mountPage();
		expect(text(mounted, '[data-test="op-steps-empty"]')).toContain('no steps');
		expect(mounted.wrapper.find('[data-test="op-steps"] table').exists()).toBe(
			false,
		);
	});
});

describe('point of no return', () => {
	it('draws one line, above the step that carries it and nowhere else', async () => {
		const mounted = await mountWith(operation({ steps }));
		const lines = mounted.wrapper.findAll('[data-test="pnr-line"]');
		expect(lines).toHaveLength(1);
		const rows = stepRows(mounted);
		expect(rows[3]?.find('[data-test="pnr-line"]').exists()).toBe(true);
		expect(rows[3]?.classes()).toContain('op-step--pnr');
		for (const row of rows.slice(0, 3)) {
			expect(row.find('[data-test="pnr-line"]').exists()).toBe(false);
		}
		// The line is part of the step cell, before the step name.
		const cell = rows[3]?.find('[data-test="pnr-line"]')?.element.parentElement;
		expect(cell?.firstElementChild?.getAttribute('data-test')).toBe('pnr-line');
	});

	it('says it can still be rolled back while the step has not started', async () => {
		const mounted = await mountWith(operation({ steps }));
		expect(text(mounted, '[data-test="pnr-line"]')).toBe(
			'Point of no return — not passed',
		);
	});

	it('says it was passed only once the step has succeeded', async () => {
		const done = steps.map((step) =>
			step.point_of_no_return ? { ...step, state: 'SUCCEEDED' as const } : step,
		);
		const mounted = await mountWith(operation({ steps: done }));
		expect(text(mounted, '[data-test="pnr-line"]')).toBe(
			'Point of no return — passed: it only goes forward',
		);
	});

	it('claims nothing while the step runs', async () => {
		const running = steps.map((step) =>
			step.point_of_no_return ? { ...step, state: 'RUNNING' as const } : step,
		);
		const mounted = await mountWith(operation({ steps: running }));
		expect(text(mounted, '[data-test="pnr-line"]')).toBe(
			'Point of no return — step in progress',
		);
	});

	it('says it was not passed under a failed operation', async () => {
		const failed = steps.map((step) =>
			step.point_of_no_return ? { ...step, state: 'FAILED' as const } : step,
		);
		const mounted = await mountWith(
			operation({ state: 'FAILED', steps: failed }),
		);
		expect(text(mounted, '[data-test="pnr-line"]')).toBe(
			'Point of no return — not passed',
		);
	});

	it('claims neither when the step failed', async () => {
		const failed = steps.map((step) =>
			step.point_of_no_return ? { ...step, state: 'FAILED' as const } : step,
		);
		const mounted = await mountWith(operation({ steps: failed }));
		expect(text(mounted, '[data-test="pnr-line"]')).toBe('Point of no return');
	});

	it('draws no line when no step carries the point', async () => {
		const mounted = await mountWith(
			operation({
				steps: steps.map((step) => ({
					...step,
					point_of_no_return: undefined,
				})),
			}),
		);
		expect(mounted.wrapper.find('[data-test="pnr-line"]').exists()).toBe(false);
	});
});

describe('omitted zero values', () => {
	it('takes a step without index as the first, and generation 0 as 0', async () => {
		const mounted = await mountWith(
			operation({
				type: 'INITIALIZE',
				previous_generation: undefined,
				new_generation: '1',
				steps: [
					{ type: 'GENERATION', state: 'SUCCEEDED' },
					{ index: 1, type: 'VERIFY', state: 'PENDING' },
				],
			}),
		);
		const rows = stepRows(mounted);
		expect(rows[0]?.text()).toMatch(/^1\D/);
		expect(rows[1]?.text()).toMatch(/^2\D/);
		expect(text(mounted, '[data-test="op-generation"]')).toContain('0 → 1');
	});

	it('shows no generation when the operation writes none, and not yet written as such', async () => {
		const none = await mountWith(
			operation({
				previous_generation: undefined,
				new_generation: undefined,
				steps,
			}),
		);
		expect(none.wrapper.find('[data-test="op-generation"]').exists()).toBe(
			false,
		);
	});

	it('does not say "not yet written" for a generation that is written', async () => {
		const mounted = await mountWith(operation({ steps }));
		expect(text(mounted, '[data-test="op-generation"]')).not.toContain(
			'not written',
		);
	});
});

describe('a running step of an operation that does not run', () => {
	it('is shown as last known in gray, with its duration stopped at the last change', async () => {
		const mounted = await mountWith(
			operation({ state: 'INTERRUPTED', update_time: at(-20), steps }),
		);
		const row = stepRows(mounted)[1];
		expect(row?.text()).toContain('last known');
		expect(
			row?.find('[data-test="step-state"]').classes().join(' '),
		).not.toContain('info');
		expect(row?.text()).toContain('6 s');
		await vi.advanceTimersByTimeAsync(10_000);
		await flushPromises();
		expect(stepRows(mounted)[1]?.text()).toContain('6 s');
	});
});

describe('stage expansion', () => {
	const dc = (id: string): Datacenter => {
		const built = buildDatacenter('normal', id);
		if (!built) throw new Error('no datacenter');
		return built;
	};

	const expand = async (mounted: Mounted, row: number) => {
		await stepRows(mounted)[row]?.find('button').trigger('click');
		await flushPromises();
	};

	it('opens the jobs of the stage of an apps step from the datacenter data', async () => {
		getDatacenter.mockResolvedValue(dc('dc_a'));
		const mounted = await mountWith(operation({ steps }));
		expect(getDatacenter).not.toHaveBeenCalled();
		await expand(mounted, 1);
		expect(getDatacenter).toHaveBeenCalledWith('dc_a', expect.anything());
		await waitForLazy(() =>
			expect(
				mounted.wrapper.find('[data-test="stage-jobs"] .wt-table').exists(),
			).toBe(true),
		);
		const jobs = text(mounted, '[data-test="stage-jobs"]');
		expect(jobs).toContain('webitel-core-api');
		expect(jobs).toContain('webitel-chat');
		expect(jobs).not.toContain('freeswitch');
	});

	it('keeps the stage open when the stream brings a change', async () => {
		getDatacenter.mockResolvedValue(dc('dc_a'));
		const mounted = await mountWith(operation({ steps }));
		await expand(mounted, 1);
		await waitForLazy(() =>
			expect(
				mounted.wrapper.find('[data-test="stage-jobs"] .wt-table').exists(),
			).toBe(true),
		);
		lastStream().push(
			operation({
				update_time: at(3),
				steps: steps.map((step) =>
					step.index === 1 ? { ...step, detail: 'stopping 2 of 3 jobs' } : step,
				),
			}),
		);
		await flushPromises();
		expect(stepRows(mounted)[1]?.text()).toContain('stopping 2 of 3 jobs');
		expect(mounted.wrapper.find('[data-test="stage-jobs"]').exists()).toBe(
			true,
		);
	});

	it('has no expander for a step that is not an apps step', async () => {
		const mounted = await mountWith(operation({ steps }));
		const button = stepRows(mounted)[3]?.find('button');
		expect(button?.classes()).toContain('wt-icon-btn--disabled');
		await button?.trigger('click');
		expect(mounted.wrapper.find('[data-test="stage-jobs"]').exists()).toBe(
			false,
		);
	});

	it('looks for stage 0 when the step has no stage, and says when the datacenter has none', async () => {
		getDatacenter.mockResolvedValue(dc('dc_a'));
		const mounted = await mountWith(
			operation({
				steps: [
					{
						type: 'APPS_STOP',
						datacenter: 'datacenters/dc_a',
						state: 'RUNNING',
					},
				],
			}),
		);
		expect(stepRows(mounted)[0]?.text()).toContain('stage 0');
		await expand(mounted, 0);
		await waitForLazy(() =>
			expect(
				mounted.wrapper.find('[data-test="stage-jobs-empty"]').exists(),
			).toBe(true),
		);
		expect(text(mounted, '[data-test="stage-jobs-empty"]')).toContain(
			'no jobs of stage 0',
		);
	});

	it('says the jobs are loading, then that the datacenter cannot be read', async () => {
		getDatacenter.mockReturnValue(new Promise(() => undefined));
		const mounted = await mountWith(operation({ steps }));
		await expand(mounted, 1);
		expect(
			mounted.wrapper.find('[data-test="stage-jobs-loading"]').exists(),
		).toBe(true);
		expect(
			mounted.wrapper.find('[data-test="stage-jobs-empty"]').exists(),
		).toBe(false);
	});

	it('shows the error instead of an empty stage when the datacenter call fails', async () => {
		getDatacenter.mockRejectedValue(new Error('Network Error'));
		const mounted = await mountWith(operation({ steps }));
		await expand(mounted, 1);
		const message = text(mounted, '[data-test="stage-jobs-unavailable"]');
		expect(message).toContain('Cannot get the datacenter');
		expect(message).toContain('Network Error');
		expect(
			mounted.wrapper.find('[data-test="stage-jobs-empty"]').exists(),
		).toBe(false);
	});

	it('says the datacenter is not in the config on 404', async () => {
		getDatacenter.mockRejectedValue(
			apiError(404, 'dcctl.datacenter.not_found'),
		);
		const mounted = await mountWith(operation({ steps }));
		await expand(mounted, 1);
		expect(text(mounted, '[data-test="stage-jobs-not-found"]')).toContain(
			'dc_a is not in the config',
		);
	});
});

describe('live updates', () => {
	it('watches the operation and shows each change of the stream', async () => {
		const mounted = await mountWith(operation({ steps }));
		expect(watch).toHaveBeenCalledWith('0b9f', expect.anything());
		expect(text(mounted, '[data-test="op-connection"]')).toBe(
			'Connecting to the live feed…',
		);

		lastStream().push(
			operation({
				update_time: at(2),
				steps: steps.map((step) =>
					step.index === 1
						? { ...step, state: 'SUCCEEDED' as const }
						: step.index === 2
							? { ...step, state: 'RUNNING' as const }
							: step,
				),
			}),
		);
		await flushPromises();
		expect(text(mounted, '[data-test="op-connection"]')).toBe('Live');
		const rows = stepRows(mounted).map((row) => row.text());
		expect(rows[1]).toContain('Succeeded');
		expect(rows[2]).toContain('Running');
	});

	it('moves to the final state and stops watching', async () => {
		const mounted = await mountWith(operation({ steps }));
		lastStream().push(
			operation({
				state: 'SUCCEEDED',
				update_time: at(5),
				end_time: at(5),
				steps: steps.map((step) => ({ ...step, state: 'SUCCEEDED' as const })),
			}),
		);
		await flushPromises();
		expect(text(mounted, '[data-test="op-state"]')).toBe('Succeeded');
		expect(text(mounted, '[data-test="op-connection"]')).toBe('Final state');
		expect(lastStream().signal.aborted).toBe(true);
		expect(mounted.wrapper.find('[data-test="op-reconnecting"]').exists()).toBe(
			false,
		);
	});

	it('ticks the duration of a running step while it is live', async () => {
		const mounted = await mountWith(operation({ steps }));
		const duration = () => stepRows(mounted)[1]?.text() ?? '';
		expect(duration()).toContain('26 s');
		await vi.advanceTimersByTimeAsync(4000);
		await flushPromises();
		expect(duration()).toContain('30 s');
	});

	it('counts durations by the server clock when the browser clock is wrong', async () => {
		recordServerDate(new Date(NOW + 60_000).toUTCString());
		const mounted = await mountWith(operation({ steps }));
		expect(stepRows(mounted)[1]?.text()).toContain('1 min');
	});

	it('aborts the stream when the page is left', async () => {
		const mounted = await mountWith(operation({ steps }));
		const stream = lastStream();
		mounted.wrapper.unmount();
		expect(stream.signal.aborted).toBe(true);
		await vi.advanceTimersByTimeAsync(60_000);
		expect(watch).toHaveBeenCalledTimes(1);
		expect(getOperation).toHaveBeenCalledTimes(1);
	});
});

describe('after a drop', () => {
	it('says it reconnects with the age of the data and does not show the running step as current', async () => {
		const mounted = await mountWith(operation({ steps }));
		lastStream().push(operation({ update_time: at(1), steps }));
		await flushPromises();
		await vi.advanceTimersByTimeAsync(7000);
		lastStream().fail(new TypeError('network error'));
		await flushPromises();

		const banner = text(mounted, '[data-test="op-reconnecting"]');
		expect(banner).toContain('Connection lost. Reconnecting.');
		expect(banner).toMatch(/[67] s ago/);
		expect(text(mounted, '[data-test="op-connection"]')).toBe(
			'Connection lost. Reconnecting.',
		);
		const running = stepRows(mounted)[1];
		expect(running?.text()).toContain('last known');
		expect(
			running?.find('[data-test="step-state"]').classes().join(' '),
		).not.toContain('info');
		expect(mounted.wrapper.find('[data-test="op-steps"]').classes()).toContain(
			'op-steps--stale',
		);
	});

	it('stops counting the duration of the running step while the feed is gone', async () => {
		const mounted = await mountWith(operation({ steps }));
		getOperation.mockRejectedValue(new Error('still down'));
		lastStream().fail(new TypeError('network error'));
		await flushPromises();
		const before = stepRows(mounted)[1]?.text();
		await vi.advanceTimersByTimeAsync(5000);
		await flushPromises();
		expect(stepRows(mounted)[1]?.text()).toBe(before);
	});

	it('shows the new data and no banner once the stream is back', async () => {
		const mounted = await mountWith(operation({ steps }));
		lastStream().fail(new TypeError('network error'));
		await flushPromises();
		getOperation.mockResolvedValue(
			operation({
				update_time: at(20),
				steps: steps.map((step) =>
					step.index === 1 ? { ...step, state: 'SUCCEEDED' as const } : step,
				),
			}),
		);
		await vi.advanceTimersByTimeAsync(reconnectDelayMs(0));
		await flushPromises();
		expect(stepRows(mounted)[1]?.text()).toContain('Succeeded');
		lastStream().push(operation({ update_time: at(21), steps }));
		await flushPromises();
		expect(mounted.wrapper.find('[data-test="op-reconnecting"]').exists()).toBe(
			false,
		);
		expect(text(mounted, '[data-test="op-connection"]')).toBe('Live');
	});

	it('shows the final state reached while the stream was down, with no lost terminal state', async () => {
		const mounted = await mountWith(operation({ steps }));
		lastStream().fail(new TypeError('network error'));
		await flushPromises();
		getOperation.mockResolvedValue(
			operation({
				state: 'FAILED',
				update_time: at(30),
				end_time: at(30),
				error: { id: 'dcctl.lock.lost', detail: 'lost the lock' },
				steps: steps.map((step) => ({
					...step,
					state: step.index === 1 ? ('FAILED' as const) : step.state,
				})),
			}),
		);
		await vi.advanceTimersByTimeAsync(reconnectDelayMs(0));
		await flushPromises();
		expect(text(mounted, '[data-test="op-state"]')).toBe('Failed');
		expect(text(mounted, '[data-test="op-error"]')).toContain('lost the lock');
		expect(text(mounted, '[data-test="op-connection"]')).toBe('Final state');
		expect(mounted.wrapper.find('[data-test="op-reconnecting"]').exists()).toBe(
			false,
		);
		await vi.advanceTimersByTimeAsync(60_000);
		expect(watch).toHaveBeenCalledTimes(1);
	});
});

describe('the status is ahead of the page', () => {
	it('warns and reads the operation again when the status shows a later change', async () => {
		const mounted = await mountWith(operation({ steps, update_time: at(-1) }));
		lastStream().push(operation({ steps, update_time: at(0) }));
		await flushPromises();
		const first = lastStream();
		let answer: (value: Operation) => void = () => undefined;
		getOperation.mockReturnValue(
			new Promise<Operation>((resolve) => {
				answer = resolve;
			}),
		);

		useStatusStore(mounted.pinia).status = {
			operation: operation({ update_time: at(9) }),
		};
		await flushPromises();
		expect(mounted.wrapper.find('[data-test="op-behind"]').exists()).toBe(true);
		expect(first.signal.aborted).toBe(true);

		answer(operation({ steps, update_time: at(9), warnings: ['fresh'] }));
		await flushPromises();
		expect(text(mounted, '[data-test="op-warnings"]')).toContain('fresh');
		expect(mounted.wrapper.find('[data-test="op-behind"]').exists()).toBe(
			false,
		);
	});

	it('warns and reads again when a fresh status no longer lists the unfinished operation', async () => {
		const mounted = await mountWith(operation({ steps, update_time: at(-1) }));
		lastStream().push(operation({ steps, update_time: at(0) }));
		await flushPromises();
		const first = lastStream();
		let answer: (value: Operation) => void = () => undefined;
		getOperation.mockReturnValue(
			new Promise<Operation>((resolve) => {
				answer = resolve;
			}),
		);
		useStatusStore(mounted.pinia).status = { observe_time: at(3) };
		await flushPromises();
		expect(mounted.wrapper.find('[data-test="op-behind"]').exists()).toBe(true);
		expect(first.signal.aborted).toBe(true);
		expect(text(mounted, '[data-test="op-connection"]')).toBe(
			'Connection lost. Reconnecting.',
		);
		answer(
			operation({
				state: 'SUCCEEDED',
				update_time: at(4),
				end_time: at(4),
				steps,
			}),
		);
		await flushPromises();
		expect(text(mounted, '[data-test="op-state"]')).toBe('Succeeded');
		expect(mounted.wrapper.find('[data-test="op-behind"]').exists()).toBe(
			false,
		);
	});

	it('does not trust a status that failed to refresh or predates the page', async () => {
		const mounted = await mountWith(operation({ steps, update_time: at(5) }));
		const store = useStatusStore(mounted.pinia);
		store.status = { observe_time: at(2) };
		await flushPromises();
		expect(mounted.wrapper.find('[data-test="op-behind"]').exists()).toBe(
			false,
		);
		store.error = new Error('down');
		store.status = { observe_time: at(30) };
		await flushPromises();
		expect(mounted.wrapper.find('[data-test="op-behind"]').exists()).toBe(
			false,
		);
	});

	it('says nothing when the status is about another operation or older', async () => {
		const mounted = await mountWith(operation({ steps, update_time: at(5) }));
		useStatusStore(mounted.pinia).status = {
			operation: operation({ name: 'operations/other', update_time: at(50) }),
		};
		await flushPromises();
		expect(mounted.wrapper.find('[data-test="op-behind"]').exists()).toBe(
			false,
		);
	});
});

describe('actions', () => {
	const button = (mounted: Mounted, kind: string) =>
		mounted.wrapper.find(`[data-test="action-${kind}"]`);

	it('offers resume and abandon for an interrupted operation', async () => {
		const mounted = await mountWith(operation({ state: 'INTERRUPTED', steps }));

		for (const kind of ['resume', 'abandon']) {
			expect(button(mounted, kind).exists()).toBe(true);
			expect(button(mounted, kind).attributes('data-reason')).toBeUndefined();
			expect(button(mounted, kind).find('button').attributes('disabled')).toBe(
				undefined,
			);
		}
	});

	it('shows them off, with the reason, while the operation runs', async () => {
		const mounted = await mountWith(operation({ steps }));

		for (const kind of ['resume', 'abandon']) {
			expect(button(mounted, kind).attributes('data-reason')).toBe(
				'notInterrupted',
			);
			expect(
				button(mounted, kind).find('button').attributes('disabled'),
			).toBeDefined();
		}
	});

	it('has no actions for a finished operation', async () => {
		const mounted = await mountWith(
			operation({ state: 'SUCCEEDED', steps, end_time: at(-1) }),
		);

		expect(mounted.wrapper.find('[data-test="op-actions"]').exists()).toBe(
			false,
		);
	});
});
