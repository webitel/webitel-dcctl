import { ChipColor } from '@webitel/ui-sdk/enums';

import type {
	WebitelDcctlV1DatacenterHealth as Health,
	WebitelDcctlV1OperationMode as Mode,
	WebitelDcctlV1OperationState as OperationState,
	WebitelDcctlV1StatusAlarmSeverity as Severity,
	WebitelDcctlV1DatacenterState as State,
	WebitelDcctlV1OperationStep as Step,
} from '@/api/gen/_models';

/** Semantic colour; screens map it to an SDK component colour via `toChipColor`. */
export type StatusColor = 'red' | 'yellow' | 'green' | 'blue' | 'gray';

export const severityColor = (severity?: Severity): StatusColor => {
	switch (severity) {
		case 'CRITICAL':
			return 'red';
		case 'WARNING':
			return 'yellow';
		default:
			return 'gray';
	}
};

export const healthColor = (health?: Health): StatusColor => {
	switch (health) {
		case 'OK':
			return 'green';
		case 'DEGRADED':
			return 'yellow';
		case 'DOWN':
			return 'red';
		default:
			return 'gray';
	}
};

/**
 * An unreachable datacenter is gray, except the active one: losing the
 * active datacenter is an outage.
 */
export const stateColor = (
	state?: State,
	{ isActive = false }: { isActive?: boolean } = {},
): StatusColor => {
	switch (state) {
		case 'ACTIVE':
			return 'blue';
		case 'SPLIT_BRAIN':
		case 'DB_SPLIT':
			return 'red';
		case 'STALE':
		case 'NEEDS_RECOVERY':
		case 'UNDETERMINED':
			return 'yellow';
		case 'UNREACHABLE':
			return isActive ? 'red' : 'gray';
		default:
			return 'gray';
	}
};

/** A step that succeeded with warnings is yellow. */
export const stepColor = (
	step: Pick<Step, 'state' | 'warnings'>,
): StatusColor => {
	switch (step.state) {
		case 'SUCCEEDED':
			return step.warnings?.length ? 'yellow' : 'green';
		case 'RUNNING':
			return 'blue';
		case 'FAILED':
			return 'red';
		default:
			return 'gray';
	}
};

/** INTERRUPTED waits for a decision, so it is yellow like other things that need attention. */
export const operationStateColor = (state?: OperationState): StatusColor => {
	switch (state) {
		case 'RUNNING':
			return 'blue';
		case 'SUCCEEDED':
			return 'green';
		case 'FAILED':
			return 'red';
		case 'INTERRUPTED':
			return 'yellow';
		default:
			return 'gray';
	}
};

export const modeColor = (mode?: Mode): StatusColor =>
	mode === 'FORCED' ? 'red' : 'gray';

const CHIP_COLORS: Record<StatusColor, ChipColor> = {
	red: ChipColor.ERROR,
	yellow: ChipColor.WARNING,
	green: ChipColor.SUCCESS,
	blue: ChipColor.INFO,
	gray: ChipColor.SECONDARY,
};

/** `wt-chip` colour for a status colour. */
export const toChipColor = (color: StatusColor): ChipColor =>
	CHIP_COLORS[color];
