import type {
	WebitelDcctlV1Datacenter,
	WebitelDcctlV1StatusAlarm,
} from '@/api/gen/_models';

/**
 * Active datacenter first, the rest by name. Returns a new array.
 * `activeDatacenter` is `status.active_datacenter`: during an outage the
 * active datacenter is UNREACHABLE, so its `state` cannot identify it.
 */
export const sortDatacenters = <
	T extends Pick<WebitelDcctlV1Datacenter, 'name'>,
>(
	datacenters: readonly T[],
	activeDatacenter?: string,
): T[] => {
	const isActive = (dc: T) =>
		activeDatacenter !== undefined && dc.name === activeDatacenter;
	return [...datacenters].sort((a, b) => {
		const activeDiff = Number(isActive(b)) - Number(isActive(a));
		if (activeDiff !== 0) return activeDiff;
		return (a.name ?? '').localeCompare(b.name ?? '');
	});
};

const severityRank = (severity?: string) => (severity === 'CRITICAL' ? 0 : 1);

/** Critical first; within a severity, the oldest first. Returns a new array. */
export const sortAlarms = <
	T extends Pick<WebitelDcctlV1StatusAlarm, 'severity' | 'start_time'>,
>(
	alarms: readonly T[],
): T[] =>
	[...alarms].sort((a, b) => {
		const rankDiff = severityRank(a.severity) - severityRank(b.severity);
		if (rankDiff !== 0) return rankDiff;
		return Date.parse(a.start_time ?? '') - Date.parse(b.start_time ?? '') || 0;
	});
