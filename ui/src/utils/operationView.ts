import type {
	WebitelDcctlV1Operation as Operation,
	WebitelDcctlV1OperationState as State,
	WebitelDcctlV1OperationStep as Step,
} from '@/api/gen/_models';

import { resourceId } from './datacenterSummary';

/**
 * A finished operation never changes again. INTERRUPTED is not final: it
 * waits for `resume` or `abandon`, so the page keeps watching it.
 */
export const isTerminalState = (state: State | undefined): boolean =>
	state === 'SUCCEEDED' || state === 'FAILED' || state === 'ABANDONED';

/** `true` when `next` is an earlier version of `current`; versions without a time are never older. */
export const isOlderOperation = (
	next: Pick<Operation, 'update_time'>,
	current: Pick<Operation, 'update_time'>,
): boolean => {
	const nextTime = Date.parse(next.update_time ?? '');
	const currentTime = Date.parse(current.update_time ?? '');
	if (Number.isNaN(nextTime) || Number.isNaN(currentTime)) return false;
	return nextTime < currentTime;
};

/** The status already shows a later change of the operation than the page does. */
export const isBehind = (
	shown: Pick<Operation, 'name' | 'update_time'>,
	fromStatus: Pick<Operation, 'name' | 'update_time'> | undefined,
): boolean => {
	if (!fromStatus || fromStatus.name !== shown.name) return false;
	const shownTime = Date.parse(shown.update_time ?? '');
	const statusTime = Date.parse(fromStatus.update_time ?? '');
	if (Number.isNaN(shownTime) || Number.isNaN(statusTime)) return false;
	return statusTime > shownTime;
};

export type OrderedStep = Step & { index: number };

/** Steps in plan order. The gateway omits `index` 0, so a missing index is 0. */
export const orderedSteps = (
	operation: Pick<Operation, 'steps'>,
): OrderedStep[] =>
	(operation.steps ?? [])
		.map((step) => ({ ...step, index: step.index ?? 0 }))
		.sort((a, b) => a.index - b.index);

/**
 * The stage an apps step works on: its datacenter (`dc_a`) and stage number.
 * The gateway omits stage 0. Other steps have no stage.
 */
export const stageOfStep = (
	step: Pick<Step, 'type' | 'stage' | 'datacenter'>,
): { datacenter: string; stage: number } | null => {
	if (step.type !== 'APPS_STOP' && step.type !== 'APPS_START') return null;
	const datacenter = resourceId(step.datacenter);
	if (!datacenter) return null;
	return { datacenter, stage: step.stage ?? 0 };
};

/**
 * Where a step stands against the point of no return. Only a step that
 * succeeded has passed it. While it runs nothing is claimed, and in a failed
 * operation the point was not passed. A failed or skipped step in another
 * state of the operation does not say whether it was crossed.
 */
export const pointOfNoReturn = (
	step: Pick<Step, 'point_of_no_return' | 'state'>,
	operationState?: State,
): 'none' | 'ahead' | 'running' | 'passed' | 'unknown' => {
	if (!step.point_of_no_return) return 'none';
	if (step.state === 'SUCCEEDED') return 'passed';
	if (operationState === 'FAILED') return 'ahead';
	switch (step.state) {
		case 'PENDING':
			return 'ahead';
		case 'RUNNING':
			return 'running';
		default:
			return 'unknown';
	}
};

/**
 * The status is fresh and no longer lists the operation the page shows as
 * unfinished: it has finished since (only RUNNING and INTERRUPTED operations
 * are in the status). `observeTime` must be later than the page's last change,
 * or the status simply predates the operation.
 */
export const isMissingFromStatus = (
	shown: Pick<Operation, 'name' | 'state' | 'update_time'>,
	fromStatus: Pick<Operation, 'name'> | undefined,
	observeTime: string | undefined,
): boolean => {
	if (isTerminalState(shown.state)) return false;
	if (fromStatus?.name === shown.name) return false;
	const observed = Date.parse(observeTime ?? '');
	const changed = Date.parse(shown.update_time ?? '');
	if (Number.isNaN(observed) || Number.isNaN(changed)) return false;
	return observed > changed;
};

/** Milliseconds from start to end, or to `nowMs` while it runs; `null` when it has not started. */
export const durationMs = (
	start: string | undefined,
	end: string | undefined,
	nowMs: number,
): number | null => {
	const startTime = Date.parse(start ?? '');
	if (Number.isNaN(startTime)) return null;
	const endTime = Date.parse(end ?? '');
	return Math.max(0, (Number.isNaN(endTime) ? nowMs : endTime) - startTime);
};

/**
 * Generation the operation moves from and to. The gateway omits 0, so a
 * missing previous generation is `0` (INITIALIZE); a missing new one is not
 * known yet. `null` when the operation writes no generation.
 */
export const generationRange = (
	operation: Pick<Operation, 'previous_generation' | 'new_generation'>,
): { from: string; to: string | null } | null => {
	if (!operation.previous_generation && !operation.new_generation) return null;
	return {
		from: operation.previous_generation || '0',
		to: operation.new_generation || null,
	};
};
