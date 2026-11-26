import { resourceId } from './datacenterSummary';

/** Error ids the dialog words itself, with the key under `dcctl.action.refusal`. */
const REFUSAL_KEYS: Readonly<Record<string, string>> = {
	'dcctl.preflight.failed': 'preflightFailed',
	'dcctl.confirmation.required': 'confirmationRequired',
	'dcctl.request.invalid': 'invalid',
	'dcctl.operation.running': 'operationRunning',
	'dcctl.operation.not_interrupted': 'operationNotInterrupted',
	'dcctl.operation.not_found': 'operationNotFound',
	'dcctl.datacenter.not_found': 'datacenterNotFound',
	'dcctl.lock.busy': 'lockBusy',
	'dcctl.lock.unavailable': 'lockUnavailable',
	'dcctl.leader.unavailable': 'leaderUnavailable',
	'dcctl.request_id.mismatch': 'requestIdMismatch',
};

/** The key of the message for an error id; `null` for an id the UI does not know, which shows the server text. */
export const refusalKey = (id: string | undefined): string | null =>
	(id && REFUSAL_KEYS[id]) || null;

/** Metadata of an error as `[name, value]` lines; resource names are cut to their id. */
export const metadataEntries = (
	metadata: Readonly<Record<string, string>> | undefined,
): [string, string][] =>
	Object.entries(metadata ?? {}).map(([name, value]) => [
		name,
		name === 'operation' || name === 'datacenter' ? resourceId(value) : value,
	]);
