import {
	afterAll,
	afterEach,
	beforeAll,
	beforeEach,
	describe,
	expect,
	it,
} from 'vitest';

import type {
	WebitelDcctlV1Error as ApiError,
	WebitelDcctlV1JournalEntry as Entry,
	WebitelDcctlV1ListJournalEntriesResponse as List,
} from '@/api/gen/_models';
import { encodeBasicToken } from '@/utils/basicAuth';
import { MOCK_USER } from './handlers';
import {
	buildJournal,
	JOURNAL_VARIANTS,
	selectJournalVariantFromUrl,
	setJournalVariant,
} from './journalVariants';
import { server } from './server';

const AUTH = `Basic ${encodeBasicToken(MOCK_USER.name, MOCK_USER.password)}`;
const API = 'http://localhost/dcctl/api';

const list = (query: Record<string, string> = {}) =>
	fetch(`${API}/v1/journalEntries?${new URLSearchParams(query)}`, {
		headers: { Authorization: AUTH },
	});

const entries = async (query: Record<string, string> = {}) =>
	((await (await list(query)).json()) as List).journal_entries ?? [];

beforeAll(() => server.listen({ onUnhandledFrame: 'error' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());
beforeEach(() => setJournalVariant('normal'));

describe('journal fixture', () => {
	const built = buildJournal(Date.now());

	it('has an entry of every type, system action and alarm result', () => {
		expect(new Set(built.map((entry) => entry.type))).toEqual(
			new Set(['OPERATION', 'STEP', 'SYSTEM', 'ALARM']),
		);
		const actions = built.flatMap((entry) =>
			entry.system_action ? [entry.system_action] : [],
		);
		for (const action of [
			'FENCE_APPS',
			'ALIGN_GENERATION',
			'ALIGN_OPERATION',
			'LOCK_LOST',
			'OUTCOME_UNKNOWN',
		]) {
			expect(actions).toContain(action);
		}
		const alarms = built.filter((entry) => entry.type === 'ALARM');
		expect(new Set(alarms.map((entry) => entry.result))).toEqual(
			new Set(['OK', 'WARNING', 'ERROR']),
		);
	});

	it('is newest first with unique names', () => {
		const times = built.map((entry) => Date.parse(entry.create_time as string));
		expect([...times].sort((a, b) => b - a)).toEqual(times);
		expect(new Set(built.map((entry) => entry.name)).size).toBe(built.length);
	});

	it('carries no explicit zero: the gateway omits it', () => {
		const zeros = (value: unknown): boolean =>
			value === 0 ||
			value === '0' ||
			(typeof value === 'object' &&
				value !== null &&
				Object.values(value).some(zeros));
		expect(built.some(zeros)).toBe(false);
	});

	it('has an error with detail where the result is ERROR', () => {
		expect(
			built.some((entry) => entry.type === 'SYSTEM' && entry.error?.detail),
		).toBe(true);
	});
});

describe('mocked GET /v1/journalEntries', () => {
	it('answers a first page with a token in the many variant', async () => {
		setJournalVariant('many');
		const first = (await (await list({ page_size: '50' })).json()) as List;
		expect(first.journal_entries).toHaveLength(50);
		const second = (await (
			await list({
				page_size: '50',
				page_token: first.next_page_token as string,
			})
		).json()) as List;
		const names = new Set(
			[...(first.journal_entries ?? []), ...(second.journal_entries ?? [])].map(
				(entry: Entry) => entry.name,
			),
		);
		expect(names.size).toBe(100);
	});

	it('has no token on the last page', async () => {
		const body = (await (await list()).json()) as List;
		expect(body.next_page_token).toBeUndefined();
	});

	it('applies the filter the UI composes', async () => {
		const found = await entries({
			filter:
				'(type = "ALARM" OR type = "STEP") AND datacenter = "datacenters/dc_b"',
		});
		expect(found.length).toBeGreaterThan(0);
		for (const entry of found) {
			expect(['ALARM', 'STEP']).toContain(entry.type);
			expect(entry.datacenter).toBe('datacenters/dc_b');
		}
	});

	it('returns nothing for a filter nothing matches, not an error', async () => {
		const response = await list({ filter: 'operation = "operations/none"' });
		expect(response.status).toBe(200);
		expect(((await response.json()) as List).journal_entries ?? []).toEqual([]);
	});

	it('reads an escaped value as the text it stands for', async () => {
		const response = await list({ filter: 'operation = "operations/a\\" b"' });
		expect(response.status).toBe(200);
	});

	it('rejects a filter it cannot read with a violation on `filter`', async () => {
		const response = await list({ filter: 'type = ALARM' });
		expect(response.status).toBe(400);
		const body = (await response.json()) as ApiError;
		expect(body.id).toBe('dcctl.request.invalid');
		expect(body.violations?.[0]).toMatchObject({
			type: 'field',
			subject: 'filter',
		});
	});

	it('rejects an unknown type like the server', async () => {
		expect((await list({ filter: 'type = "TYPO"' })).status).toBe(400);
	});

	it('rejects a page token of another filter', async () => {
		setJournalVariant('many');
		const first = (await (await list({ page_size: '10' })).json()) as List;
		const response = await list({
			page_size: '10',
			page_token: first.next_page_token as string,
			filter: 'type = "ALARM"',
		});
		expect(response.status).toBe(400);
	});

	it('is empty in the empty variant', async () => {
		setJournalVariant('empty');
		expect(await entries()).toEqual([]);
	});

	it('rejects any filter in the filter-error variant, but lists without one', async () => {
		setJournalVariant('filter-error');
		expect((await list({ filter: 'type = "ALARM"' })).status).toBe(400);
		expect((await list()).status).toBe(200);
	});

	it('is 503 in the unavailable variant', async () => {
		setJournalVariant('unavailable');
		const response = await list();
		expect(response.status).toBe(503);
		expect(((await response.json()) as ApiError).id).toBe('dcctl.unavailable');
	});

	it('wants the credentials like the rest of the API', async () => {
		const response = await fetch(`${API}/v1/journalEntries`);
		expect(response.status).toBe(401);
	});
});

describe('selectJournalVariantFromUrl', () => {
	it.each(JOURNAL_VARIANTS)('picks %s from ?journal=', async (variant) => {
		setJournalVariant('normal');
		selectJournalVariantFromUrl(`?journal=${variant}`);
		const response = await list();
		expect(response.status).toBe(variant === 'unavailable' ? 503 : 200);
	});

	it('ignores an unknown name', async () => {
		setJournalVariant('empty');
		selectJournalVariantFromUrl('?journal=nope');
		expect(await entries()).toEqual([]);
	});
});
