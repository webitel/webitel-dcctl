import type {
	WebitelDcctlV1Datacenter as Datacenter,
	WebitelDcctlV1Operation as Operation,
	WebitelDcctlV1Status as Status,
} from '@/api/gen/_models';

import { resourceId } from './datacenterSummary';
import { isOperationShown } from './operationLine';

export type ActionKind =
	| 'switch'
	| 'failover'
	| 'demote'
	| 'repoint'
	| 'markRecovered'
	| 'syncApps'
	| 'resolve'
	| 'initArbiter'
	| 'resume'
	| 'abandon';

/** Why an action is not available; the key of the tooltip text `dcctl.action.unavailable.<reason>`. */
export type UnavailableReason =
	| 'statusUnknown'
	| 'alreadyActive'
	| 'needsRecovery'
	| 'activeUnreachable'
	| 'operationRunning'
	| 'roleNotPrimary'
	| 'roleNotStandbyLeader'
	| 'notNeedsRecovery'
	| 'notActive'
	| 'notInterrupted';

export interface Availability {
	/** Shown at all. A hidden action has no reason. */
	visible: boolean;
	/** Set when the action is shown but cannot be started. */
	reason?: UnavailableReason;
}

export interface ActionContext {
	status?: Status;
	/** The datacenter page the action is on, for datacenter actions. */
	datacenter?: Datacenter;
	/** `dc_b`: the datacenter the action targets. */
	datacenterId?: string;
	/** The operation page the action is on, for resume and abandon. */
	operation?: Operation;
}

const HIDDEN: Availability = { visible: false };
const OK: Availability = { visible: true };
const refuse = (reason: UnavailableReason): Availability => ({
	visible: true,
	reason,
});

const isActive = ({ status, datacenter, datacenterId }: ActionContext) =>
	(!!datacenterId && resourceId(status?.active_datacenter) === datacenterId) ||
	datacenter?.state === 'ACTIVE';

const needsRecovery = ({ status, datacenter, datacenterId }: ActionContext) =>
	datacenter?.needs_recovery === true ||
	datacenter?.state === 'NEEDS_RECOVERY' ||
	(status?.needs_recovery_datacenters ?? []).some(
		(name) => resourceId(name) === datacenterId,
	);

const activeIsUnreachable = ({ status }: ActionContext) => {
	const active = status?.active_datacenter;
	if (!active) return false;
	const dc = status?.datacenters?.find((item) => item.name === active);
	return dc?.state === 'UNREACHABLE';
};

const isStretch = ({ status }: ActionContext) => status?.profile === 'STRETCH';

const hasAlarm = (status: Status | undefined, type: string) =>
	(status?.alarms ?? []).some((alarm) => alarm.type === type);

/**
 * Whether an action can be started from what the screen knows, and why not.
 * The decision of the server (the dry run) is final: this only keeps buttons
 * from offering what is certainly refused.
 */
export const actionAvailability = (
	kind: ActionKind,
	ctx: ActionContext,
): Availability => {
	const { status, datacenter } = ctx;
	switch (kind) {
		case 'switch':
		case 'failover': {
			if (!status) return refuse('statusUnknown');
			if (isActive(ctx)) return refuse('alreadyActive');
			if (needsRecovery(ctx)) return refuse('needsRecovery');
			if (kind === 'switch' && activeIsUnreachable(ctx)) {
				return refuse('activeUnreachable');
			}
			if (isOperationShown(status.operation)) {
				return refuse('operationRunning');
			}
			return OK;
		}
		case 'demote':
		case 'repoint': {
			// Stretch has one Patroni cluster: nothing to demote or repoint.
			if (isStretch(ctx)) return HIDDEN;
			if (!status) return refuse('statusUnknown');
			if (isActive(ctx)) return refuse('alreadyActive');
			const role = datacenter?.patroni?.role;
			if (kind === 'demote' && role !== 'PRIMARY') {
				return refuse('roleNotPrimary');
			}
			if (kind === 'repoint' && role !== 'STANDBY_LEADER') {
				return refuse('roleNotStandbyLeader');
			}
			return isOperationShown(status.operation)
				? refuse('operationRunning')
				: OK;
		}
		case 'markRecovered': {
			if (isStretch(ctx)) return HIDDEN;
			if (!needsRecovery(ctx)) return refuse('notNeedsRecovery');
			return isOperationShown(status?.operation)
				? refuse('operationRunning')
				: OK;
		}
		case 'syncApps': {
			if (!status) return refuse('statusUnknown');
			if (!isActive(ctx)) return refuse('notActive');
			if (isOperationShown(status.operation)) {
				return refuse('operationRunning');
			}
			return OK;
		}
		case 'resume':
		case 'abandon': {
			const state = ctx.operation?.state;
			if (state === 'INTERRUPTED') return OK;
			return state === 'RUNNING' ? refuse('notInterrupted') : HIDDEN;
		}
		case 'resolve':
			return hasAlarm(status, 'CONFLICT') ? OK : HIDDEN;
		case 'initArbiter':
			return hasAlarm(status, 'ARBITER_UNINITIALIZED') ? OK : HIDDEN;
	}
};

/**
 * The target datacenter name is typed to confirm: always for these, never for
 * the rest. `resume` and `repoint` ask for it only when the dry run shows the
 * dangerous part (see `typedNameRequired`).
 */
const ALWAYS_TYPED = new Set<ActionKind>([
	'switch',
	'failover',
	'demote',
	'resolve',
]);
const CONDITIONALLY_TYPED = new Set<ActionKind>(['repoint', 'resume']);

export const isAlwaysTyped = (kind: ActionKind): boolean =>
	ALWAYS_TYPED.has(kind);
export const isConditionallyTyped = (kind: ActionKind): boolean =>
	CONDITIONALLY_TYPED.has(kind);
