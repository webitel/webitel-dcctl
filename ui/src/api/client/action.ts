import type {
	WebitelDcctlV1Confirmation as Confirmation,
	WebitelDcctlV1Operation as Operation,
} from '@/api/gen/_models';
import { getDatacenterService } from '@/api/gen/datacenter-service/datacenter-service';
import { getOperationService } from '@/api/gen/operation-service/operation-service';
import type { ActionKind } from '@/utils/actionAvailability';

/** What an action acts on. Ids, not resource names: `dc_b`, `0b9f…`. */
export interface ActionTarget {
	kind: ActionKind;
	/** Datacenter actions: the target datacenter. */
	datacenter?: string;
	/** `resolve`: the datacenter whose record wins. */
	winner?: string;
	/** `resume` and `abandon`. */
	operation?: string;
}

export interface ActionCall {
	validate_only: boolean;
	confirmation?: Confirmation;
	/** Idempotency key; `resume` and `abandon` have none. */
	request_id?: string;
}

const datacenterName = (id: string | undefined) => `datacenters/${id ?? ''}`;

/** Sends the action to its endpoint. A `validate_only` call returns the operation without a name. */
export const callAction = (
	target: ActionTarget,
	{ validate_only, confirmation, request_id }: ActionCall,
	signal?: AbortSignal,
): Promise<Operation> => {
	const options = { signal };
	const body = { validate_only, confirmation, request_id };
	const dc = getDatacenterService();
	const operations = getOperationService();
	switch (target.kind) {
		case 'switch':
			return dc.switchDatacenter(target.datacenter ?? '', body, options);
		case 'failover':
			return dc.failoverDatacenter(target.datacenter ?? '', body, options);
		case 'demote':
			return dc.demoteDatacenter(target.datacenter ?? '', body, options);
		case 'repoint':
			return dc.repointDatacenter(target.datacenter ?? '', body, options);
		case 'markRecovered':
			return dc.markDatacenterRecovered(target.datacenter ?? '', body, options);
		case 'syncApps':
			return dc.syncApps(body, options);
		case 'initArbiter':
			return dc.initializeArbiter(body, options);
		case 'resolve':
			return dc.resolveConflict(
				{ ...body, winner: datacenterName(target.winner) },
				options,
			);
		case 'resume':
			return operations.resumeOperation(
				target.operation ?? '',
				{ validate_only, confirmation },
				options,
			);
		case 'abandon':
			return operations.abandonOperation(
				target.operation ?? '',
				{ validate_only, confirmation },
				options,
			);
	}
};
