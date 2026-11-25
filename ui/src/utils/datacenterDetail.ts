import type {
	WebitelDcctlV1Datacenter as Datacenter,
	WebitelDcctlV1DatacenterNomadJob as Job,
	WebitelDcctlV1DatacenterNomadJobState as JobState,
	WebitelDcctlV1DatacenterPatroniMember as Member,
	WebitelDcctlV1DatacenterPatroni as Patroni,
	WebitelDcctlV1Source as Source,
	WebitelDcctlV1SourceSystem as SourceSystem,
} from '@/api/gen/_models';
import { ageSeconds, isStaleAge } from './dataAge';
import type { StatusColor } from './statusMapping';

export const DATACENTER_TABS = ['patroni', 'apps', 'info', 'sources'] as const;
export type DatacenterTab = (typeof DATACENTER_TABS)[number];

/** The tab of `?tab=`; a missing or unknown value opens the first one. */
export const tabFromQuery = (value: unknown): DatacenterTab =>
	DATACENTER_TABS.find((tab) => tab === value) ?? 'patroni';

export interface DivergenceView {
	/** `diverged`: red; `maybe`: yellow; `none`: not diverged; `unknown`: nothing to compare; `other`: a value the UI does not know. */
	kind: 'diverged' | 'maybe' | 'none' | 'unknown' | 'other';
	color: StatusColor;
	lsn: string | undefined;
	bytes: string | undefined;
	/** The raw state, for `other`. */
	raw: string | undefined;
}

/**
 * Divergence of the datacenter from the active leader by `divergence_state`.
 * The state is authoritative: an empty `divergence_lsn` does not mean the
 * datacenter did not diverge. No state (the active datacenter, stretch) is
 * `unknown`: nothing to compare.
 */
export const divergence = (
	patroni:
		| Pick<Patroni, 'divergence_state' | 'divergence_lsn' | 'divergence_bytes'>
		| undefined,
): DivergenceView => {
	const state: string | undefined = patroni?.divergence_state;
	const base = {
		lsn: patroni?.divergence_lsn,
		bytes: patroni?.divergence_bytes,
		raw: state,
	};
	switch (state) {
		case 'DIVERGED':
			return { ...base, kind: 'diverged', color: 'red' };
		case 'UNKNOWN':
			return { ...base, kind: 'maybe', color: 'yellow' };
		case 'NOT_DIVERGED':
			return { ...base, kind: 'none', color: 'gray' };
		case undefined:
		case 'DIVERGENCE_STATE_UNSPECIFIED':
			return { ...base, kind: 'unknown', color: 'gray' };
		default:
			return { ...base, kind: 'other', color: 'gray' };
	}
};

const HEALTHY_MEMBER_STATES = ['running', 'streaming'];

/**
 * The gateway omits `false`: a member in the list that is not marked
 * reachable (or usable) is not.
 */
export const memberColor = (
	member: Pick<Member, 'reachable' | 'usable' | 'patroni_state'>,
): StatusColor => {
	if (!member.reachable) return 'red';
	if (!member.usable) return 'yellow';
	if (!member.patroni_state) return 'gray';
	return HEALTHY_MEMBER_STATES.includes(member.patroni_state)
		? 'green'
		: 'yellow';
};

/**
 * Colour of a job by what its datacenter should do. `expectRunning` is
 * `true` for the active datacenter, `false` for an inactive one, where
 * anything but a stopped job is what the split-brain alarm is about, and
 * `undefined` while the role is unknown (neutral colours).
 */
export const jobColor = (
	state: JobState | undefined,
	{ expectRunning }: { expectRunning?: boolean } = {},
): StatusColor => {
	if (expectRunning === false) {
		if (state === 'STOPPED') return 'gray';
		return state === 'STOPPING' ? 'yellow' : 'red';
	}
	switch (state) {
		case 'HEALTHY':
			return 'green';
		case 'UNHEALTHY':
			return 'red';
		case 'STOPPED':
			return expectRunning ? 'red' : 'gray';
		case 'STOPPING':
			return 'yellow';
		default:
			return 'gray';
	}
};

const PROBLEM_ALLOCATIONS = ['failed', 'lost'];

/**
 * Allocation counts by Nomad `ClientStatus`, failed and lost first, the rest
 * by name. The status names are Nomad's and are shown as they are.
 */
export const allocationEntries = (
	counts: Readonly<Record<string, number>> | undefined,
) =>
	Object.entries(counts ?? {})
		.map(([status, count]) => ({
			status,
			count,
			problem: PROBLEM_ALLOCATIONS.includes(status),
		}))
		.sort(
			(a, b) =>
				Number(b.problem) - Number(a.problem) ||
				a.status.localeCompare(b.status),
		);

const EXPECTED_MISSING_REASONS = ['DRAINING', 'DOWN', 'INELIGIBLE'];

/**
 * Healthy over expected nodes of a system job, with the nodes that miss it;
 * `null` for any other job. A missing count is 0: the gateway omits zeros.
 * A drained, down or ineligible node is not expected to run the job (yellow);
 * a node that should run it and does not is a failure (red, also for a reason
 * the UI does not know).
 */
export const systemJobCoverage = (
	job: Pick<
		Job,
		'type' | 'healthy_node_count' | 'expected_node_count' | 'missing_nodes'
	>,
) => {
	if (job.type !== 'SYSTEM') return null;
	const healthy = job.healthy_node_count ?? 0;
	const expected = job.expected_node_count ?? 0;
	const missing = job.missing_nodes ?? [];
	let color: StatusColor = 'green';
	if (missing.some((n) => !EXPECTED_MISSING_REASONS.includes(n.reason ?? ''))) {
		color = 'red';
	} else if (missing.length > 0 || healthy < expected) {
		// Fewer healthy nodes than expected with nobody listed: say so.
		color = 'yellow';
	}
	return { healthy, expected, color, missing };
};

export const sourceFor = (
	dc: Pick<Datacenter, 'sources'>,
	system: SourceSystem,
): Source | undefined => dc.sources?.find((source) => source.system === system);

/** Age of the last data read from a source, by the server clock (`nowMs`). */
export const sourceRow = (
	source: Source,
	nowMs: number,
	staleAfter?: number,
) => {
	const age = ageSeconds(source.observe_time, nowMs);
	return { age, stale: isStaleAge(age, staleAfter) };
};
