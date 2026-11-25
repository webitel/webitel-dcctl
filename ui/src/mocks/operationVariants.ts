import { HttpResponse } from 'msw';

import type {
	WebitelDcctlV1Operation as Operation,
	WebitelDcctlV1OperationState as State,
	WebitelDcctlV1OperationStep as Step,
} from '@/api/gen/_models';

/*
 * The gateway omits zero values: no `index: 0`, no `stage: 0`, no generation 0.
 * The builders leave them out as well, so the screens meet what the server sends.
 */

export const OPERATIONS_VARIANTS = [
	'normal',
	'live',
	'many',
	'empty',
	'unavailable',
] as const;

export type OperationsVariant = (typeof OPERATIONS_VARIANTS)[number];

/** Ids of the operations the mock knows. The ones that start with `watch-` are alive. */
export const MOCK_OPERATION_IDS = {
	running: '0b9f3c2e-5d1a-4c55-9a7e-2f4d8c1b6a10',
	failover: 'failover-forced',
	failed: 'switch-failed',
	interrupted: 'switch-interrupted',
	syncApps: 'sync-apps-done',
	progress: 'watch-progress',
	drop: 'watch-drop',
} as const;

const iso = (ms: number) => new Date(ms).toISOString();
const ago = (now: number, seconds: number) => iso(now - seconds * 1000);

const initiator = {
	user: 'support',
	address: '10.1.0.15',
	instance: {
		node: 'dcctl-a1',
		datacenter: 'datacenters/dc_a',
		address: '10.1.0.81:10023',
	},
};
const leader = {
	node: 'dcctl-b1',
	datacenter: 'datacenters/dc_b',
	address: '10.2.0.81:10023',
};

type StepSpec = Omit<Step, 'state' | 'start_time' | 'end_time' | 'detail'>;

/** The plan of a switch from dc_a to dc_b. */
const SWITCH_PLAN: readonly StepSpec[] = [
	{ type: 'APPS_PREFLIGHT' },
	{ index: 1, type: 'APPS_STOP', stage: 3, datacenter: 'datacenters/dc_a' },
	{ index: 2, type: 'APPS_STOP', stage: 2, datacenter: 'datacenters/dc_a' },
	{ index: 3, type: 'APPS_STOP', stage: 1, datacenter: 'datacenters/dc_a' },
	{ index: 4, type: 'PATRONI_DEMOTE', datacenter: 'datacenters/dc_a' },
	{ index: 5, type: 'PATRONI_PROMOTE', datacenter: 'datacenters/dc_b' },
	{ index: 6, type: 'GENERATION', point_of_no_return: true },
	{ index: 7, type: 'APPS_START', stage: 1, datacenter: 'datacenters/dc_b' },
	{ index: 8, type: 'APPS_START', stage: 2, datacenter: 'datacenters/dc_b' },
	{ index: 9, type: 'APPS_START', stage: 3, datacenter: 'datacenters/dc_b' },
	{ index: 10, type: 'VERIFY' },
];

const RUNNING_DETAIL = 'stopping 1 of 3 jobs';

/**
 * A switch after `done` steps: those are done, the next one runs, the rest
 * wait. `done` equal to the plan length is a finished operation. Times are
 * relative to `startedAt`, one `stepMs` per step.
 */
const switchAt = (
	id: string,
	done: number,
	startedAt: number,
	stepMs: number,
): Operation => {
	const finished = done >= SWITCH_PLAN.length;
	const steps = SWITCH_PLAN.map((spec, position): Step => {
		const start = startedAt + position * stepMs;
		if (position < done) {
			return {
				...spec,
				state: 'SUCCEEDED',
				start_time: iso(start),
				end_time: iso(start + stepMs),
			};
		}
		if (position === done) {
			return {
				...spec,
				state: 'RUNNING',
				start_time: iso(start),
				detail: spec.type === 'APPS_STOP' ? RUNNING_DETAIL : undefined,
			};
		}
		return { ...spec, state: 'PENDING' };
	});
	return {
		name: `operations/${id}`,
		type: 'SWITCH',
		state: finished ? 'SUCCEEDED' : 'RUNNING',
		target: 'datacenters/dc_b',
		initiator,
		leader,
		mode: 'QUORUM',
		previous_generation: '7',
		new_generation: '8',
		steps,
		create_time: iso(startedAt - 1000),
		update_time: iso(startedAt + Math.min(done, SWITCH_PLAN.length) * stepMs),
		end_time: finished
			? iso(startedAt + SWITCH_PLAN.length * stepMs)
			: undefined,
		switch_datacenter: { name: 'datacenters/dc_b' },
	};
};

const failover = (now: number): Operation => {
	const start = now - 600_000;
	const at = (seconds: number) => iso(start + seconds * 1000);
	return {
		name: `operations/${MOCK_OPERATION_IDS.failover}`,
		type: 'FAILOVER',
		state: 'SUCCEEDED',
		target: 'datacenters/dc_b',
		initiator: { ...initiator, address: '10.2.0.15' },
		leader,
		mode: 'FORCED',
		previous_generation: '8',
		new_generation: '9',
		warnings: ['apps of dc_a were not stopped: the datacenter does not answer'],
		steps: [
			{
				type: 'APPS_PREFLIGHT',
				state: 'SUCCEEDED',
				start_time: at(1),
				end_time: at(3),
			},
			{
				index: 1,
				type: 'APPS_STOP',
				datacenter: 'datacenters/dc_a',
				state: 'SKIPPED',
				detail: 'datacenter does not answer',
			},
			{
				index: 2,
				type: 'PATRONI_FAILOVER',
				datacenter: 'datacenters/dc_b',
				state: 'SUCCEEDED',
				start_time: at(3),
				end_time: at(19),
				detail: 'promoted pg-b1 on timeline 6',
				point_of_no_return: true,
				warnings: ['pg-b1 was 4 KB behind the last known primary'],
			},
			{
				index: 3,
				type: 'GENERATION',
				state: 'SUCCEEDED',
				start_time: at(19),
				end_time: at(21),
			},
			{
				index: 4,
				type: 'APPS_START',
				stage: 1,
				datacenter: 'datacenters/dc_b',
				state: 'SUCCEEDED',
				start_time: at(21),
				end_time: at(40),
			},
		],
		create_time: at(0),
		update_time: at(40),
		end_time: at(40),
		failover_datacenter: { name: 'datacenters/dc_b' },
	};
};

const failed = (now: number): Operation => {
	const base = switchAt(MOCK_OPERATION_IDS.failed, 5, now - 1_800_000, 10_000);
	return {
		...base,
		state: 'FAILED',
		end_time: base.update_time,
		steps: base.steps?.map((step) =>
			step.state === 'RUNNING'
				? {
						...step,
						state: 'FAILED',
						end_time: base.update_time,
						detail: 'no standby caught up within 30 s',
					}
				: step,
		),
		error: {
			id: 'dcctl.preflight.failed',
			reason: 'PREFLIGHT_FAILED',
			detail: 'the checks of the plan failed',
			metadata: { datacenter: 'datacenters/dc_b' },
		},
	};
};

const interrupted = (now: number): Operation => {
	const base = switchAt(
		MOCK_OPERATION_IDS.interrupted,
		6,
		now - 240_000,
		10_000,
	);
	return {
		...base,
		state: 'INTERRUPTED',
		steps: base.steps?.map((step) =>
			step.state === 'RUNNING'
				? {
						...step,
						state: 'FAILED',
						detail: 'the leader instance lost its lease',
					}
				: step,
		),
		error: {
			id: 'dcctl.lock.lost',
			reason: 'LOCK_LOST',
			detail: 'the instance that ran the operation lost the lock',
		},
	};
};

const syncApps = (now: number): Operation => ({
	name: `operations/${MOCK_OPERATION_IDS.syncApps}`,
	type: 'SYNC_APPS',
	state: 'SUCCEEDED',
	target: 'datacenters/dc_a',
	initiator,
	leader: { ...leader, node: 'dcctl-a1', datacenter: 'datacenters/dc_a' },
	mode: 'QUORUM',
	steps: [
		{
			type: 'APPS_PREFLIGHT',
			state: 'SUCCEEDED',
			start_time: ago(now, 7_200),
			end_time: ago(now, 7_198),
		},
	],
	create_time: ago(now, 7_201),
	update_time: ago(now, 7_198),
	end_time: ago(now, 7_198),
});

export interface Scenario {
	stepMs: number;
	startedAt: Map<string, number>;
	connections: Map<string, number>;
}

const scenario: Scenario = {
	stepMs: 3000,
	startedAt: new Map(),
	connections: new Map(),
};

/** Makes the live operations move at another pace and start over. Tests only. */
export const resetScenarios = (stepMs = 3000) => {
	scenario.stepMs = stepMs;
	scenario.startedAt.clear();
	scenario.connections.clear();
};

const isLive = (id: string) =>
	id === MOCK_OPERATION_IDS.progress || id === MOCK_OPERATION_IDS.drop;

const startOf = (id: string, now: number): number => {
	const known = scenario.startedAt.get(id);
	if (known !== undefined) return known;
	scenario.startedAt.set(id, now);
	return now;
};

const liveFrame = (id: string, done: number): Operation =>
	switchAt(id, done, startOf(id, Date.now()), scenario.stepMs);

const liveDone = (id: string, now: number): number =>
	Math.min(
		SWITCH_PLAN.length,
		Math.floor((now - startOf(id, now)) / scenario.stepMs),
	);

/** The operation as the mock knows it now; `undefined` for an unknown id. */
export const buildOperation = (
	id: string,
	now = Date.now(),
): Operation | undefined => {
	if (isLive(id)) return liveFrame(id, liveDone(id, now));
	switch (id) {
		case MOCK_OPERATION_IDS.running:
			return switchAt(id, 1, now - 14_000, 10_000);
		case MOCK_OPERATION_IDS.failover:
			return failover(now);
		case MOCK_OPERATION_IDS.failed:
			return failed(now);
		case MOCK_OPERATION_IDS.interrupted:
			return interrupted(now);
		case MOCK_OPERATION_IDS.syncApps:
			return syncApps(now);
		default:
			return undefined;
	}
};

const ALL_IDS = [
	MOCK_OPERATION_IDS.running,
	MOCK_OPERATION_IDS.failover,
	MOCK_OPERATION_IDS.failed,
	MOCK_OPERATION_IDS.interrupted,
	MOCK_OPERATION_IDS.syncApps,
];

const MANY = 120;
const TYPES = ['SWITCH', 'FAILOVER', 'SYNC_APPS', 'RESOLVE_CONFLICT'] as const;
const STATES: readonly State[] = [
	'SUCCEEDED',
	'SUCCEEDED',
	'FAILED',
	'ABANDONED',
];

/** Many finished operations, newest first, without steps: for paging. */
const manyOperations = (now: number): Operation[] =>
	Array.from({ length: MANY }, (_unused, position): Operation => {
		const n = MANY - position;
		const time = ago(now, 3600 * (position + 1));
		return {
			name: `operations/mock-${n}`,
			type: TYPES[position % TYPES.length],
			state: STATES[position % STATES.length],
			target: position % 2 === 0 ? 'datacenters/dc_b' : 'datacenters/dc_a',
			mode: position % 7 === 0 ? 'FORCED' : 'QUORUM',
			initiator: {
				...initiator,
				user: position % 3 === 0 ? 'admin' : 'support',
			},
			create_time: time,
			update_time: time,
			end_time: time,
		};
	});

const listFor = (variant: OperationsVariant, now: number): Operation[] => {
	const known = ALL_IDS.map((id) => buildOperation(id, now) as Operation);
	switch (variant) {
		case 'live':
			return [
				...[MOCK_OPERATION_IDS.progress, MOCK_OPERATION_IDS.drop].map(
					(id) => buildOperation(id, now) as Operation,
				),
				...known,
			];
		case 'many':
			return manyOperations(now);
		case 'empty':
			return [];
		default:
			return known;
	}
};

let selected: OperationsVariant = 'normal';

export const setOperationsVariant = (variant: OperationsVariant) => {
	selected = variant;
};

export const isOperationsVariant = (
	value: unknown,
): value is OperationsVariant =>
	OPERATIONS_VARIANTS.includes(value as OperationsVariant);

/** Picks the variant of the list from `?ops=<name>`; unknown names are ignored. */
export const selectOperationsVariantFromUrl = (search: string) => {
	const value = new URLSearchParams(search).get('ops');
	if (isOperationsVariant(value)) selected = value;
};

const DEFAULT_PAGE_SIZE = 50;

const unavailable = () =>
	HttpResponse.json(
		{ id: 'dcctl.unavailable', code: 503, detail: 'mock: API is down' },
		{ status: 503 },
	);

/** The page token is the offset of the next operation. */
export const currentOperationsResponse = (url: URL) => {
	if (selected === 'unavailable') return unavailable();
	const all = listFor(selected, Date.now());
	const offset = Number(url.searchParams.get('page_token') || 0);
	const size = Number(url.searchParams.get('page_size')) || DEFAULT_PAGE_SIZE;
	const page = all.slice(offset, offset + size);
	const next = offset + size;
	return HttpResponse.json({
		operations: page,
		next_page_token: next < all.length ? String(next) : undefined,
	});
};

const notFound = (id: string) =>
	HttpResponse.json(
		{
			id: 'dcctl.operation.not_found',
			code: 404,
			status: 'Not Found',
			detail: `operation ${id} not found`,
			metadata: { operation: `operations/${id}` },
		},
		{ status: 404 },
	);

export const currentOperationResponse = (id: string) => {
	if (selected === 'unavailable') return unavailable();
	const operation = buildOperation(id);
	return operation ? HttpResponse.json(operation) : notFound(id);
};

const encoder = new TextEncoder();
const line = (operation: Operation) =>
	encoder.encode(`${JSON.stringify({ result: operation })}\n`);

/** After this many messages the first connection of `watch-drop` breaks. */
export const DROP_AFTER_MESSAGES = 2;

/**
 * The mocked `GET /v1/operations/{id}:watch`: the current state at once, then
 * every change, as the gateway sends it (one JSON line per message). The
 * stream of a finished operation closes after the final state. `watch-drop`
 * breaks its first connection after a few messages and plays out normally on
 * the next one.
 */
export const watchResponse = (id: string, signal: AbortSignal) => {
	if (selected === 'unavailable') return unavailable();
	const operation = buildOperation(id);
	if (!operation) return notFound(id);
	const connection = (scenario.connections.get(id) ?? 0) + 1;
	scenario.connections.set(id, connection);
	const breaks = id === MOCK_OPERATION_IDS.drop && connection === 1;

	let timer: ReturnType<typeof setTimeout> | undefined;
	const stream = new ReadableStream<Uint8Array>({
		start(controller) {
			let sent = 0;
			let done = isLive(id) ? liveDone(id, Date.now()) : -1;
			const emit = (message: Operation) => {
				controller.enqueue(line(message));
				sent++;
			};
			const tick = () => {
				if (breaks && sent >= DROP_AFTER_MESSAGES) {
					controller.error(new TypeError('mock: connection dropped'));
					return;
				}
				done++;
				emit(liveFrame(id, done));
				if (done >= SWITCH_PLAN.length) {
					controller.close();
					return;
				}
				timer = setTimeout(tick, scenario.stepMs);
			};
			emit(operation);
			if (!isLive(id)) {
				// Fixtures do not move: the stream ends with a final state, stays open otherwise.
				if (
					operation.state !== 'RUNNING' &&
					operation.state !== 'INTERRUPTED'
				) {
					controller.close();
				}
				return;
			}
			if (done >= SWITCH_PLAN.length) {
				controller.close();
				return;
			}
			timer = setTimeout(tick, scenario.stepMs);
			signal.addEventListener('abort', () => clearTimeout(timer), {
				once: true,
			});
		},
		cancel() {
			clearTimeout(timer);
		},
	});
	return new HttpResponse(stream, {
		headers: { 'Content-Type': 'application/json' },
	});
};
