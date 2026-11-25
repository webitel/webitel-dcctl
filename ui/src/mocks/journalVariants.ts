import { HttpResponse } from 'msw';

import type { WebitelDcctlV1JournalEntry as Entry } from '@/api/gen/_models';
import { JOURNAL_TYPES } from '@/utils/journalFilter';
import { MOCK_OPERATION_IDS } from './operationVariants';

/*
 * The gateway omits zero values, so the entries have no explicit zeros.
 */

export const JOURNAL_VARIANTS = [
	'normal',
	'many',
	'empty',
	'filter-error',
	'unavailable',
] as const;

export type JournalVariant = (typeof JOURNAL_VARIANTS)[number];

const instance = (node: string, datacenter: string) => ({
	node,
	datacenter: `datacenters/${datacenter}`,
	address: `10.${datacenter === 'dc_a' ? 1 : 2}.0.81:10023`,
});
const nodeA = instance('dcctl-a1', 'dc_a');
const nodeB = instance('dcctl-b1', 'dc_b');
const bySupport = { user: 'support', address: '10.1.0.15', instance: nodeA };
const bySystem = (observer = nodeA) => ({ system: true, instance: observer });

const op = (id: string) => `operations/${id}`;
const dc = (id: string) => `datacenters/${id}`;

/** `journalEntries/<yyyymmddhhmmss + 9 digits of nanoseconds>-<id>` like the server's keys. */
const entryName = (ms: number, id: string): string => {
	const digits = new Date(ms)
		.toISOString()
		.replace(/\D/g, '')
		.slice(0, 14)
		.concat(String(ms % 1000).padStart(3, '0'), '000000');
	return `journalEntries/${digits}-${id}`;
};

type Spec = Omit<Entry, 'name' | 'create_time'> & { secondsAgo: number };

/** Newest first: a failed switch with the alarms and what dcctl did around it. */
const NORMAL: readonly Spec[] = [
	{
		secondsAgo: 120,
		type: 'ALARM',
		alarm_type: 'OPERATION_INTERRUPTED',
		datacenter: dc('dc_b'),
		operation: op(MOCK_OPERATION_IDS.interrupted),
		initiator: bySystem(),
		result: 'ERROR',
		detail: 'the switch to dc_b is interrupted and waits for resume or abandon',
	},
	{
		secondsAgo: 125,
		type: 'SYSTEM',
		system_action: 'OUTCOME_UNKNOWN',
		datacenter: dc('dc_b'),
		operation: op(MOCK_OPERATION_IDS.interrupted),
		initiator: bySystem(nodeB),
		params: { key: 'dcctl/lock' },
		result: 'WARNING',
		detail: 'the release of the lock did not answer, its outcome is unknown',
	},
	{
		secondsAgo: 130,
		type: 'SYSTEM',
		system_action: 'LOCK_LOST',
		datacenter: dc('dc_b'),
		operation: op(MOCK_OPERATION_IDS.interrupted),
		initiator: bySystem(nodeB),
		result: 'ERROR',
		detail: 'the instance that ran the operation lost the lock',
		error: {
			id: 'dcctl.lock.lost',
			reason: 'LOCK_LOST',
			detail: 'session expired',
			metadata: {
				holder: 'dcctl-b1',
				operation: op(MOCK_OPERATION_IDS.interrupted),
			},
		},
	},
	{
		secondsAgo: 140,
		type: 'STEP',
		step_type: 'GENERATION',
		datacenter: dc('dc_b'),
		operation: op(MOCK_OPERATION_IDS.interrupted),
		initiator: bySupport,
		params: { index: 6, previous_generation: 7, new_generation: 8 },
		result: 'ERROR',
		detail: 'the generation was not written',
		error: {
			id: 'dcctl.unavailable',
			reason: 'UNAVAILABLE',
			detail: 'quorum of voters did not answer',
		},
	},
	{
		secondsAgo: 300,
		type: 'ALARM',
		alarm_type: 'VOTER_UNREACHABLE',
		initiator: bySystem(),
		result: 'OK',
		detail: 'arbiter answers again',
	},
	{
		secondsAgo: 600,
		type: 'ALARM',
		alarm_type: 'VOTER_UNREACHABLE',
		initiator: bySystem(),
		result: 'WARNING',
		detail: 'arbiter does not answer',
	},
	{
		secondsAgo: 900,
		type: 'SYSTEM',
		system_action: 'ALIGN_OPERATION',
		datacenter: dc('dc_a'),
		operation: op(MOCK_OPERATION_IDS.failover),
		initiator: bySystem(),
		params: { copy: 'dcctl/op/failover-forced', from: dc('dc_b') },
		result: 'OK',
		detail: 'the copy of the finished operation was written to dc_a',
	},
	{
		secondsAgo: 1200,
		type: 'SYSTEM',
		system_action: 'ALIGN_GENERATION',
		datacenter: dc('dc_a'),
		initiator: bySystem(),
		params: { previous_generation: 8, new_generation: 9 },
		result: 'WARNING',
		detail: 'dc_a was behind by one generation',
	},
	{
		secondsAgo: 1500,
		type: 'SYSTEM',
		system_action: 'FENCE_APPS',
		datacenter: dc('dc_a'),
		initiator: bySystem(),
		params: { jobs: ['webitel-core-api', 'opensips'] },
		result: 'OK',
		detail: 'stopped 2 managed jobs: the effective state names dc_b as active',
	},
	{
		secondsAgo: 1700,
		type: 'ALARM',
		alarm_type: 'CONFLICT',
		datacenter: dc('dc_a'),
		initiator: bySystem(nodeB),
		result: 'ERROR',
		detail: 'dc_a and dc_b disagree about the active datacenter',
	},
	{
		secondsAgo: 2400,
		type: 'OPERATION',
		operation_type: 'FAILOVER',
		datacenter: dc('dc_b'),
		operation: op(MOCK_OPERATION_IDS.failover),
		initiator: { ...bySupport, address: '10.2.0.15', instance: nodeB },
		params: { name: dc('dc_b'), mode: 'FORCED', new_generation: 9 },
		confirmation: { isolation: true, max_discard_bytes: '4096' },
		result: 'WARNING',
		detail: 'failover to dc_b finished with warnings',
	},
	{
		secondsAgo: 2410,
		type: 'STEP',
		step_type: 'PATRONI_FAILOVER',
		datacenter: dc('dc_b'),
		operation: op(MOCK_OPERATION_IDS.failover),
		initiator: bySupport,
		params: { index: 2, candidate: 'pg-b1' },
		result: 'OK',
		detail: 'promoted pg-b1 on timeline 6',
	},
	{
		secondsAgo: 2420,
		type: 'STEP',
		step_type: 'APPS_STOP',
		datacenter: dc('dc_a'),
		operation: op(MOCK_OPERATION_IDS.failover),
		initiator: bySupport,
		params: { index: 1, stage: 3 },
		result: 'WARNING',
		detail: 'datacenter does not answer',
	},
	{
		secondsAgo: 3600,
		type: 'OPERATION',
		operation_type: 'SYNC_APPS',
		datacenter: dc('dc_a'),
		operation: op(MOCK_OPERATION_IDS.syncApps),
		initiator: bySupport,
		result: 'OK',
		detail: 'sync apps of dc_a finished',
	},
];

const MANY = 150;
const MANY_ACTIONS: readonly Pick<
	Entry,
	'type' | 'step_type' | 'system_action' | 'alarm_type' | 'operation_type'
>[] = [
	{ type: 'STEP', step_type: 'APPS_STOP' },
	{ type: 'ALARM', alarm_type: 'STALE' },
	{ type: 'OPERATION', operation_type: 'SWITCH' },
	{ type: 'SYSTEM', system_action: 'ALIGN_GENERATION' },
];

const many = (): Spec[] =>
	Array.from({ length: MANY }, (_unused, position): Spec => {
		const action = MANY_ACTIONS[position % MANY_ACTIONS.length] as Spec;
		return {
			...action,
			secondsAgo: 60 * (position + 1),
			datacenter: dc(position % 2 === 0 ? 'dc_b' : 'dc_a'),
			operation: op(`mock-${MANY - position}`),
			initiator: position % 4 === 3 ? bySystem() : bySupport,
			result: position % 5 === 0 ? 'WARNING' : 'OK',
			detail: `mock entry ${MANY - position}`,
		};
	});

const toEntry =
	(now: number) =>
	(spec: Spec, position: number): Entry => {
		const { secondsAgo, ...rest } = spec;
		const ms = now - secondsAgo * 1000;
		return {
			...rest,
			name: entryName(ms, `${(position + 1).toString(16).padStart(8, '0')}`),
			create_time: new Date(ms).toISOString(),
		};
	};

/** The journal of the normal variant, newest first. */
export const buildJournal = (now: number): Entry[] => NORMAL.map(toEntry(now));

const entriesFor = (variant: JournalVariant, now: number): Entry[] => {
	switch (variant) {
		case 'many':
			return many().map(toEntry(now));
		case 'empty':
			return [];
		default:
			return buildJournal(now);
	}
};

let selected: JournalVariant = 'normal';

export const setJournalVariant = (variant: JournalVariant) => {
	selected = variant;
};

export const isJournalVariant = (value: unknown): value is JournalVariant =>
	JOURNAL_VARIANTS.includes(value as JournalVariant);

/** Picks the variant from `?journal=<name>`; unknown names are ignored. */
export const selectJournalVariantFromUrl = (search: string) => {
	const value = new URLSearchParams(search).get('journal');
	if (isJournalVariant(value)) selected = value;
};

interface ParsedFilter {
	types: ReadonlySet<string>;
	datacenter?: string;
	operation?: string;
}

const PAIR = /(\w+) = "((?:[^"\\]|\\.)*)"/g;

/**
 * Reads what the UI composes: `field = "value"` pairs joined by AND, OR and
 * parentheses, with `\"` and `\\` in values. Anything else is `null`, as the
 * server would reject it. It does not evaluate the grouping: types are
 * alternatives, the other fields are required.
 */
const parseFilter = (text: string): ParsedFilter | null => {
	const rest = text.replace(PAIR, '').replace(/\b(AND|OR)\b|[()\s]/g, '');
	if (rest) return null;
	const types = new Set<string>();
	const parsed: ParsedFilter = { types };
	for (const [, field, raw] of text.matchAll(PAIR)) {
		const value = (raw as string).replace(/\\(.)/g, '$1');
		if (field === 'type') {
			if (!(JOURNAL_TYPES as readonly string[]).includes(value)) return null;
			types.add(value);
		} else if (field === 'datacenter' || field === 'operation') {
			parsed[field] = value;
		} else {
			return null;
		}
	}
	return parsed;
};

const matches = (entry: Entry, filter: ParsedFilter): boolean =>
	(filter.types.size === 0 || filter.types.has(entry.type as string)) &&
	(filter.datacenter === undefined || entry.datacenter === filter.datacenter) &&
	(filter.operation === undefined || entry.operation === filter.operation);

const invalid = (description: string) =>
	HttpResponse.json(
		{
			id: 'dcctl.request.invalid',
			code: 400,
			status: 'Bad Request',
			detail: 'invalid request',
			violations: [{ type: 'field', subject: 'filter', description }],
		},
		{ status: 400 },
	);

const unavailable = () =>
	HttpResponse.json(
		{ id: 'dcctl.unavailable', code: 503, detail: 'mock: API is down' },
		{ status: 503 },
	);

const DEFAULT_PAGE_SIZE = 50;

/** The page token is the offset of the next entry and the filter it belongs to. */
export const currentJournalResponse = (url: URL) => {
	if (selected === 'unavailable') return unavailable();
	const text = url.searchParams.get('filter') ?? '';
	if (selected === 'filter-error' && text) {
		return invalid('mock: every filter is rejected in this variant');
	}
	const filter = parseFilter(text);
	if (!filter)
		return invalid(
			'the filter is not an AIP-160 expression of type, datacenter, operation',
		);

	const token = url.searchParams.get('page_token');
	let offset = 0;
	if (token) {
		const separator = token.indexOf('|');
		if (separator < 0 || token.slice(separator + 1) !== text) {
			return invalid(
				'differs from the filter of the request that returned page_token',
			);
		}
		offset = Number(token.slice(0, separator)) || 0;
	}
	const size = Number(url.searchParams.get('page_size')) || DEFAULT_PAGE_SIZE;
	const found = entriesFor(selected, Date.now()).filter((entry) =>
		matches(entry, filter),
	);
	const next = offset + size;
	return HttpResponse.json({
		journal_entries: found.slice(offset, next),
		next_page_token: next < found.length ? `${next}|${text}` : undefined,
	});
};
