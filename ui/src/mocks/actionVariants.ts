import { HttpResponse, http } from 'msw';

import type {
	WebitelDcctlV1Error as ApiError,
	WebitelDcctlV1Check as Check,
	WebitelDcctlV1Confirmation as Confirmation,
	WebitelDcctlV1Operation as Operation,
	WebitelDcctlV1Preflight as Preflight,
	WebitelDcctlV1OperationStep as Step,
} from '@/api/gen/_models';
import type { ActionKind } from '@/utils/actionAvailability';
import { MOCK_OPERATION_IDS } from './operationVariants';

/*
 * The action endpoints are mocked from the contract: the server answers a
 * dry run (`validate_only`) with an operation without a name, a refusal with
 * an `Error` body. The variant picks the story; `?action=<name>` selects it.
 */
export const ACTION_VARIANTS = [
	'ok',
	'confirmation',
	'confirmation-grew',
	'preflight-failed',
	'lock-busy',
	'lock-busy-grew',
	'preflight-failed-real',
	'not-interrupted',
	'operation-running',
	'unavailable',
	'invalid',
] as const;

export type ActionVariant = (typeof ACTION_VARIANTS)[number];

let selected: ActionVariant = 'ok';

/** `lock-busy-grew`: the first dry run shows the small amount, every later check the large one. */
let dryRuns = 0;

export const setActionVariant = (variant: ActionVariant) => {
	selected = variant;
	dryRuns = 0;
};

export const isActionVariant = (value: unknown): value is ActionVariant =>
	ACTION_VARIANTS.includes(value as ActionVariant);

/** Picks the variant from `?action=<name>` of the page URL; unknown names are ignored. */
export const selectActionVariantFromUrl = (search: string) => {
	const value = new URLSearchParams(search).get('action');
	if (isActionVariant(value)) selected = value;
};

/** Amount the divergence check shows in a dry run, and the amount the real call finds when `confirmation-grew`. */
export const MOCK_DISCARD_BYTES = 1024;
export const MOCK_DISCARD_BYTES_GREW = 2048;
export const MOCK_DISCARD_BYTES_HUGE = 5_000_000_000;

const NEW_OPERATION = `operations/${MOCK_OPERATION_IDS.progress}`;

const PLAN: readonly Step[] = [
	{ type: 'APPS_PREFLIGHT', state: 'PENDING' },
	{
		index: 1,
		type: 'APPS_STOP',
		stage: 1,
		datacenter: 'datacenters/dc_a',
		state: 'PENDING',
	},
	{
		index: 2,
		type: 'PATRONI_PROMOTE',
		datacenter: 'datacenters/dc_b',
		state: 'PENDING',
	},
	{ index: 3, type: 'GENERATION', state: 'PENDING', point_of_no_return: true },
	{
		index: 4,
		type: 'APPS_START',
		stage: 1,
		datacenter: 'datacenters/dc_b',
		state: 'PENDING',
	},
	{ index: 5, type: 'VERIFY', state: 'PENDING' },
];

const passing: Check[] = [
	{
		id: 'coord.lock_free',
		result: 'PASS',
		effect: 'BLOCK',
		detail: 'no operation holds the lock',
	},
	{
		id: 'coord.no_operation',
		result: 'PASS',
		effect: 'BLOCK',
		detail: 'no operation is running',
	},
	{
		id: 'coord.quorum',
		result: 'WARN',
		effect: 'WARN_ONLY',
		detail: '1 of 3 voters does not answer',
		facts: { reachable: 2, total: 3 },
	},
];

const BYTES_KINDS: readonly ActionKind[] = ['failover', 'demote', 'repoint'];
const NO_ISOLATION_KINDS: readonly ActionKind[] = ['demote', 'repoint'];

const isolationCheck: Check = {
	id: 'coord.isolation',
	result: 'FAIL',
	effect: 'CONFIRM',
	confirmation: 'isolation',
	datacenter: 'datacenters/dc_a',
	detail: 'dc_a does not answer: its isolation is not known',
	facts: {
		reachable: false,
		last_known_state: 'ACTIVE',
		last_known_time: '2026-11-26T09:59:30Z',
	},
};

const divergenceCheck = (bytes: number): Check => ({
	id: 'patroni.divergence',
	result: 'FAIL',
	effect: 'CONFIRM',
	confirmation: 'max_discard_bytes',
	datacenter: 'datacenters/dc_a',
	detail: `dc_a diverged, a rewind discards ${bytes} bytes`,
	facts: {
		known: true,
		diverged: true,
		timeline: 7,
		divergence_lsn: '0/5000060',
		divergence_bytes: bytes,
	},
});

const confirmChecks = (kind: ActionKind, bytes: number): Check[] => [
	...(NO_ISOLATION_KINDS.includes(kind) ? [] : [isolationCheck]),
	...(BYTES_KINDS.includes(kind) ? [divergenceCheck(bytes)] : []),
];

const preflight = (checks: Check[], mode: Preflight['mode']): Preflight => ({
	checks,
	steps: [...PLAN],
	mode,
});

const errorBody = (
	status: number,
	statusText: string,
	body: Pick<ApiError, 'id' | 'detail'> & Partial<ApiError>,
) =>
	HttpResponse.json({ code: status, status: statusText, ...body }, { status });

const confirmationRequired = (kind: ActionKind, bytes: number) => {
	const checks = [...passing, ...confirmChecks(kind, bytes)];
	return errorBody(400, 'Bad Request', {
		id: 'dcctl.confirmation.required',
		detail: 'the action needs confirmations',
		preflight: preflight(checks, 'FORCED'),
		violations: confirmChecks(kind, bytes).map((check) => ({
			type: 'confirmation',
			subject: check.confirmation,
			description: check.detail,
		})),
	});
};

/** The server adds the full preflight to every refusal of a preflight, not only to the two that are about it. */
const withPreflight = (
	kind: ActionKind,
	bytes: number,
	failing: Check,
): Preflight =>
	preflight(
		[
			...passing.filter((c) => c.id !== failing.id),
			failing,
			...confirmChecks(kind, bytes),
		],
		'QUORUM',
	);

const lockBusyPreflight = (kind: ActionKind, bytes: number) =>
	withPreflight(kind, bytes, {
		id: 'coord.lock_free',
		result: 'FAIL',
		effect: 'BLOCK',
		detail: 'dcctl-b1 holds the lock',
		facts: { holder: 'dcctl-b1' },
	});

const preflightFailed = (kind: ActionKind) => {
	const blocker: Check = {
		id: 'patroni.target_lag',
		result: 'FAIL',
		effect: 'BLOCK',
		datacenter: 'datacenters/dc_b',
		detail: 'dc_b lags 52428800 bytes, more than 16777216',
		facts: { lag_bytes: 52428800, limit_bytes: 16777216 },
	};
	const checks = [...passing, blocker, ...confirmChecks(kind, 4096)];
	return errorBody(400, 'Bad Request', {
		id: 'dcctl.preflight.failed',
		detail: 'the checks of the action failed',
		preflight: preflight(checks, 'QUORUM'),
		violations: [
			{
				type: 'check',
				subject: blocker.id,
				description: blocker.detail,
			},
			...confirmChecks(kind, 4096).map((check) => ({
				type: 'confirmation',
				subject: check.confirmation,
				description: check.detail,
			})),
		],
	});
};

const succeeded = (
	kind: ActionKind,
	validateOnly: boolean,
	operationId: string | undefined,
	confirmation: Confirmation | undefined,
	mode: Operation['mode'],
) => {
	// A dry run has what `answer.DryRun` gives: checks, steps and mode, no name, type or state.
	const preview: Operation = { mode, checks: [...passing], steps: [...PLAN] };
	if (validateOnly) return HttpResponse.json(preview);
	const base: Operation = { ...preview, type: 'SWITCH', state: 'RUNNING' };
	if (kind === 'abandon') {
		return HttpResponse.json({
			...base,
			name: `operations/${operationId}`,
			state: 'ABANDONED',
		});
	}
	return HttpResponse.json({
		...base,
		name: kind === 'resume' ? `operations/${operationId}` : NEW_OPERATION,
		confirmation,
	});
};

/** Confirmations cover the amount the story shows: the bound is at least the fact. */
const confirmed = (
	kind: ActionKind,
	confirmation: Confirmation | undefined,
	bytes: number,
): boolean => {
	const needsIsolation = !NO_ISOLATION_KINDS.includes(kind);
	if (needsIsolation && !confirmation?.isolation) return false;
	if (BYTES_KINDS.includes(kind)) {
		return Number(confirmation?.max_discard_bytes ?? -1) >= bytes;
	}
	return true;
};

const answer = async (
	kind: ActionKind,
	request: Request,
	params: Record<string, string | readonly string[] | undefined>,
) => {
	const body = (await request.json().catch(() => ({}))) as {
		validate_only?: boolean;
		confirmation?: Confirmation;
	};
	const validateOnly = body.validate_only === true;
	const operationId = params.operation ? String(params.operation) : undefined;

	switch (selected) {
		case 'preflight-failed':
			return preflightFailed(kind);
		case 'preflight-failed-real':
			// The state changed between the dry run and the real call.
			return validateOnly
				? succeeded(kind, true, operationId, undefined, 'QUORUM')
				: preflightFailed(kind);
		case 'not-interrupted':
			// The operation was finished by someone else meanwhile.
			return validateOnly
				? succeeded(kind, true, operationId, undefined, 'QUORUM')
				: errorBody(400, 'Bad Request', {
						id: 'dcctl.operation.not_interrupted',
						detail: 'the operation is not interrupted',
						metadata: { operation: NEW_OPERATION, state: 'RUNNING' },
						preflight: withPreflight(kind, MOCK_DISCARD_BYTES, {
							id: 'op.interrupted',
							result: 'FAIL',
							effect: 'BLOCK',
							detail: 'the operation is RUNNING',
						}),
					});
		case 'operation-running':
			return errorBody(400, 'Bad Request', {
				id: 'dcctl.operation.running',
				detail: 'an operation is running',
				metadata: { operation: NEW_OPERATION },
				preflight: withPreflight(kind, MOCK_DISCARD_BYTES, {
					id: 'coord.no_operation',
					result: 'FAIL',
					effect: 'BLOCK',
					detail: 'an operation is running',
					facts: { operation: NEW_OPERATION },
				}),
			});
		case 'unavailable':
			return errorBody(503, 'Service Unavailable', {
				id: 'dcctl.leader.unavailable',
				detail: 'the instance that leads the action does not answer',
				metadata: { instance: 'dcctl-b1' },
				preflight: withPreflight(kind, MOCK_DISCARD_BYTES, {
					id: 'coord.leader_reachable',
					result: 'FAIL',
					effect: 'BLOCK',
					detail: 'dcctl-b1 does not answer',
				}),
			});
		case 'invalid':
			return errorBody(400, 'Bad Request', {
				id: 'dcctl.request.invalid',
				detail: 'the request is invalid',
				violations: [
					{
						type: 'field',
						subject: 'request_id',
						description: 'must be a UUID',
					},
				],
			});
		case 'lock-busy':
			if (!validateOnly) {
				return errorBody(409, 'Conflict', {
					id: 'dcctl.lock.busy',
					detail: 'the lock is held by another instance',
					metadata: {
						holder: 'dcctl-b1',
						datacenter: 'datacenters/dc_b',
						operation: NEW_OPERATION,
					},
					preflight: lockBusyPreflight(kind, MOCK_DISCARD_BYTES),
				});
			}
			return succeeded(kind, true, operationId, undefined, 'QUORUM');
		case 'lock-busy-grew': {
			if (!validateOnly) {
				return errorBody(409, 'Conflict', {
					id: 'dcctl.lock.busy',
					detail: 'the lock is held by another instance',
					metadata: { holder: 'dcctl-b1', datacenter: 'datacenters/dc_b' },
					preflight: lockBusyPreflight(kind, MOCK_DISCARD_BYTES_HUGE),
				});
			}
			dryRuns += 1;
			const found =
				dryRuns === 1 ? MOCK_DISCARD_BYTES : MOCK_DISCARD_BYTES_HUGE;
			if (!confirmed(kind, body.confirmation, found)) {
				return confirmationRequired(kind, found);
			}
			return succeeded(kind, true, operationId, body.confirmation, 'FORCED');
		}
		case 'confirmation':
		case 'confirmation-grew': {
			const found =
				selected === 'confirmation-grew' && !validateOnly
					? MOCK_DISCARD_BYTES_GREW
					: MOCK_DISCARD_BYTES;
			if (!confirmed(kind, body.confirmation, found)) {
				return confirmationRequired(kind, found);
			}
			return succeeded(
				kind,
				validateOnly,
				operationId,
				body.confirmation,
				'FORCED',
			);
		}
		default:
			return succeeded(
				kind,
				validateOnly,
				operationId,
				body.confirmation,
				'QUORUM',
			);
	}
};

const handler =
	(kind: ActionKind) =>
	({
		request,
		params,
	}: {
		request: Request;
		params: Record<string, string | readonly string[] | undefined>;
	}) =>
		answer(kind, request, params);

/** The mocked action endpoints of `DatacenterService` and `OperationService`. */
export const actionHandlers = [
	http.post('*/v1/datacenters/:dc\\:switch', handler('switch')),
	http.post('*/v1/datacenters/:dc\\:failover', handler('failover')),
	http.post('*/v1/datacenters/:dc\\:demote', handler('demote')),
	http.post('*/v1/datacenters/:dc\\:repoint', handler('repoint')),
	http.post('*/v1/datacenters/:dc\\:markRecovered', handler('markRecovered')),
	http.post('*/v1/datacenters\\:syncApps', handler('syncApps')),
	http.post('*/v1/datacenters\\:resolveConflict', handler('resolve')),
	http.post('*/v1/arbiter\\:initialize', handler('initArbiter')),
	http.post('*/v1/operations/:operation\\:resume', handler('resume')),
	http.post('*/v1/operations/:operation\\:abandon', handler('abandon')),
];
