import type {
	WebitelDcctlV1DatacenterPatroniRole as Role,
	WebitelDcctlV1Status as Status,
} from '@/api/gen/_models';

import { resourceId } from './datacenterSummary';

export interface ConflictCandidate {
	/** `dc_a` */
	id: string;
	/** Role of the Patroni leader of the datacenter, when the status has it. */
	role?: Role;
}

/**
 * Datacenters a conflict can be resolved for: the active datacenter of every
 * present voter record. `valid` is not looked at: in a conflict no record is
 * effective, so none is valid. Records of every generation are offered, as an
 * uncommitted record may sit above the conflicting ones; the server rejects a
 * winner that is not one of the conflicting records in the dry run.
 */
export const conflictCandidates = (
	status: Pick<Status, 'voters' | 'datacenters'> | undefined,
): ConflictCandidate[] => {
	const ids = new Set(
		(status?.voters ?? [])
			.map((voter) => resourceId(voter.active_datacenter))
			.filter(Boolean),
	);
	return [...ids].sort().map((id) => ({
		id,
		role: status?.datacenters?.find((dc) => resourceId(dc.name) === id)?.patroni
			?.role,
	}));
};
