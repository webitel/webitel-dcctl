import type { WebitelDcctlV1JournalEntryType as EntryType } from '@/api/gen/_models';

/** What the journal can be narrowed by. Empty fields do not narrow. */
export interface JournalFilter {
	types: readonly EntryType[];
	/** `dc_b` or `datacenters/dc_b`. */
	datacenter: string;
	/** `<id>` or `operations/<id>`. */
	operation: string;
}

export const emptyJournalFilter: JournalFilter = {
	types: [],
	datacenter: '',
	operation: '',
};

/** The entry types a user can pick; `TYPE_UNSPECIFIED` is rejected by the server. */
export const JOURNAL_TYPES = [
	'OPERATION',
	'STEP',
	'SYSTEM',
	'ALARM',
] as const satisfies readonly EntryType[];

/** An AIP-160 string: double quotes, with `\` and `"` escaped. */
const quote = (value: string): string =>
	`"${value.replaceAll('\\', '\\\\').replaceAll('"', '\\"')}"`;

export const resourceNameOf = (prefix: string, value: string): string => {
	const trimmed = value.trim();
	if (!trimmed) return '';
	return trimmed.startsWith(`${prefix}/`) ? trimmed : `${prefix}/${trimmed}`;
};

/**
 * The `filter` of `ListJournalEntries`, or `undefined` when nothing narrows
 * the journal. Types are alternatives, the three fields are all required.
 */
export const composeJournalFilter = (
	filter: JournalFilter,
): string | undefined => {
	const types = filter.types.map((type) => `type = ${quote(type)}`);
	const groups: string[] = [];
	if (types.length === 1) groups.push(types[0] as string);
	if (types.length > 1) groups.push(`(${types.join(' OR ')})`);
	const datacenter = resourceNameOf('datacenters', filter.datacenter);
	if (datacenter) groups.push(`datacenter = ${quote(datacenter)}`);
	const operation = resourceNameOf('operations', filter.operation);
	if (operation) groups.push(`operation = ${quote(operation)}`);
	return groups.length ? groups.join(' AND ') : undefined;
};
