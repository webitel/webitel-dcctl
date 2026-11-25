import type { RouteLocationRaw } from 'vue-router';

import type { WebitelDcctlV1StatusAlarm as Alarm } from '@/api/gen/_models';
import { resourceId } from './datacenterSummary';

const COORDINATION: RouteLocationRaw = {
	name: 'overview',
	hash: '#coordination',
};

/**
 * Where an alarm is handled. `operationName` is `status.operation.name`.
 * `CONFLICT` has no page of its own yet: the resolve dialog comes with the
 * datacenter actions.
 */
export const alarmTarget = (
	alarm: Pick<Alarm, 'type' | 'datacenter' | 'voter_type'>,
	operationName?: string,
): RouteLocationRaw => {
	switch (alarm.type) {
		case 'OPERATION_INTERRUPTED':
		case 'UNCOMMITTED':
			return operationName
				? { name: 'operation', params: { id: resourceId(operationName) } }
				: { name: 'operations' };
		case 'CONFLICT':
			return { name: 'datacenters' };
		case 'QUORUM_LOST':
		case 'LEASE_LOST':
		case 'ARBITER_UNINITIALIZED':
			return COORDINATION;
		default:
			if (alarm.voter_type === 'ARBITER' || !alarm.datacenter) {
				return COORDINATION;
			}
			return {
				name: 'datacenter',
				params: { dc: resourceId(alarm.datacenter) },
			};
	}
};
