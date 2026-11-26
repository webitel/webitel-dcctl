import type { RouteLocationRaw } from 'vue-router';

import type { WebitelDcctlV1StatusAlarm as Alarm } from '@/api/gen/_models';
import type { DatacenterTab } from './datacenterDetail';
import { resourceId } from './datacenterSummary';

const COORDINATION: RouteLocationRaw = {
	name: 'overview',
	hash: '#coordination',
};
const DATACENTER_TABLE: RouteLocationRaw = {
	name: 'overview',
	hash: '#datacenters',
};

/** The page of a datacenter, on the tab that shows the cause when there is one. */
const datacenterPage = (
	datacenter: string,
	tab?: DatacenterTab,
): RouteLocationRaw => ({
	name: 'datacenter',
	params: { dc: resourceId(datacenter) },
	...(tab && { query: { tab } }),
});

/**
 * Where an alarm is handled. `operationName` is `status.operation.name`.
 * Every known type is listed: a new type must not land in a wrong place
 * silently.
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
		case 'QUORUM_LOST':
		case 'LEASE_LOST':
		case 'ARBITER_UNINITIALIZED':
			return COORDINATION;
		case 'MULTIPLE_PRIMARIES':
		case 'DB_LEADER_MISMATCH':
			return DATACENTER_TABLE;
		case 'VOTER_UNREACHABLE':
			if (alarm.voter_type === 'DATACENTER' && alarm.datacenter) {
				return datacenterPage(alarm.datacenter, 'sources');
			}
			return COORDINATION;
		case 'DB_SPLIT':
		case 'DB_PRIMARY_MISSING':
		case 'DB_DEGRADED':
		case 'STANDBY_NOT_FOLLOWING':
			return alarm.datacenter
				? datacenterPage(alarm.datacenter, 'patroni')
				: DATACENTER_TABLE;
		case 'SPLIT_BRAIN':
		case 'APPS_DEGRADED':
			return alarm.datacenter
				? datacenterPage(alarm.datacenter, 'apps')
				: DATACENTER_TABLE;
		case 'DATACENTER_UNREACHABLE':
			return alarm.datacenter
				? datacenterPage(alarm.datacenter, 'sources')
				: DATACENTER_TABLE;
		case 'STALE':
			return alarm.datacenter
				? datacenterPage(alarm.datacenter)
				: DATACENTER_TABLE;
		default:
			return alarm.datacenter
				? datacenterPage(alarm.datacenter)
				: { name: 'overview' };
	}
};
