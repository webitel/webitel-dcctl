import type { WebitelDcctlV1Datacenter as Datacenter } from '@/api/gen/_models';

/** `datacenters/dc_b` -> `dc_b`; the last path segment of a resource name. */
export const resourceId = (name: string | undefined): string =>
	name?.split('/').pop() ?? '';

/** Managed Nomad jobs: healthy over all; `null` when Nomad data is absent. */
export const appsSummary = (
	dc: Pick<Datacenter, 'nomad'>,
): { healthy: number; total: number } | null => {
	const counts = dc.nomad?.job_counts;
	if (!counts) return null;
	const healthy = counts.healthy_count ?? 0;
	return {
		healthy,
		total:
			healthy +
			(counts.unhealthy_count ?? 0) +
			(counts.stopped_count ?? 0) +
			(counts.stopping_count ?? 0),
	};
};

export const sourcesSummary = (
	dc: Pick<Datacenter, 'sources'>,
): { reachable: number; total: number } => {
	const sources = dc.sources ?? [];
	return {
		reachable: sources.filter((source) => source.reachable).length,
		total: sources.length,
	};
};

export const unreachableSources = (dc: Pick<Datacenter, 'sources'>) =>
	(dc.sources ?? [])
		.filter((source) => !source.reachable)
		.map((source) => source.system);
