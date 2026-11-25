import type { WebitelDcctlV1JournalEntry as Entry } from '@/api/gen/_models';

import { resourceId } from './datacenterSummary';
import type { StatusColor } from './statusMapping';

export type JournalInitiator = {
	/** `system`: dcctl acted by itself, `text` is empty and the label is the screen's. */
	kind: 'user' | 'system' | 'instance' | 'none';
	text: string;
	/** The instance that accepted the request or observed the event. */
	node: string;
};

export interface JournalRowModel {
	id: string;
	type: Entry['type'];
	time: string | undefined;
	datacenter: string;
	operationId: string;
	initiator: JournalInitiator;
	/** Label group of `action` under `dcctl.`; the group depends on the entry type. */
	actionGroup: string;
	action: string;
	resultGroup: 'journalResult' | 'journalAlarmResult';
	result: Entry['result'];
	color: StatusColor;
	detail: string;
}

export const journalResultColor = (result: Entry['result']): StatusColor => {
	switch (result) {
		case 'OK':
			return 'green';
		case 'WARNING':
			return 'yellow';
		case 'ERROR':
			return 'red';
		default:
			return 'gray';
	}
};

const initiatorOf = (initiator: Entry['initiator']): JournalInitiator => {
	const node = initiator?.instance?.node ?? '';
	if (initiator?.user) return { kind: 'user', text: initiator.user, node };
	if (initiator?.system) return { kind: 'system', text: '', node };
	if (node) return { kind: 'instance', text: node, node };
	return { kind: 'none', text: '', node: '' };
};

const actionOf = (
	entry: Entry,
): Pick<JournalRowModel, 'actionGroup' | 'action'> => {
	switch (entry.type) {
		case 'OPERATION':
			return {
				actionGroup: 'operationType',
				action: entry.operation_type ?? '',
			};
		case 'STEP':
			return { actionGroup: 'stepType', action: entry.step_type ?? '' };
		case 'SYSTEM':
			return { actionGroup: 'systemAction', action: entry.system_action ?? '' };
		case 'ALARM':
			return {
				actionGroup: 'overview.alarms.type',
				action: entry.alarm_type ?? '',
			};
		default:
			return { actionGroup: 'journalType', action: '' };
	}
};

/** One entry as a table row. For an alarm, the result says raised (by severity) or cleared. */
export const journalRow = (entry: Entry): JournalRowModel => ({
	id: (entry.name ?? '').replace(/^journalEntries\//, ''),
	type: entry.type,
	time: entry.create_time,
	datacenter: resourceId(entry.datacenter),
	operationId: resourceId(entry.operation),
	initiator: initiatorOf(entry.initiator),
	...actionOf(entry),
	resultGroup: entry.type === 'ALARM' ? 'journalAlarmResult' : 'journalResult',
	result: entry.result,
	color: journalResultColor(entry.result),
	detail: entry.detail ?? '',
});

export type Fact = readonly [key: string, text: string];

export interface EntryFacts {
	params: Fact[];
	confirmation: Fact[];
	error:
		| {
				id: string;
				reason: string;
				detail: string;
				metadata: Fact[];
		  }
		| undefined;
}

const text = (value: unknown): string =>
	typeof value === 'string' ? value : (JSON.stringify(value) ?? String(value));

const sortedFacts = (source: Record<string, unknown> | undefined): Fact[] =>
	Object.entries(source ?? {})
		.sort(([a], [b]) => a.localeCompare(b))
		.map(([key, value]) => [key, text(value)] as const);

/** Params, confirmations and error of an entry as plain text pairs. */
export const entryFacts = (
	entry: Pick<Entry, 'params' | 'confirmation' | 'error'>,
): EntryFacts => ({
	params: sortedFacts(entry.params),
	// The gateway omits false and 0: a present block lists all three.
	confirmation: !entry.confirmation
		? []
		: [
				['isolation', String(entry.confirmation.isolation ?? false)],
				['max_discard_bytes', entry.confirmation.max_discard_bytes ?? '0'],
				['not_max_lsn', String(entry.confirmation.not_max_lsn ?? false)],
			],
	error: entry.error && {
		id: entry.error.id ?? '',
		reason: entry.error.reason ?? '',
		detail: entry.error.detail ?? '',
		metadata: sortedFacts(entry.error.metadata),
	},
});
