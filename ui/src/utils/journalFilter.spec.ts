import { describe, expect, it } from 'vitest';

import {
	composeJournalFilter,
	emptyJournalFilter,
	type JournalFilter,
} from './journalFilter';

const filter = (part: Partial<JournalFilter>): JournalFilter => ({
	...emptyJournalFilter,
	...part,
});

describe('composeJournalFilter', () => {
	it('is undefined without any condition', () => {
		expect(composeJournalFilter(emptyJournalFilter)).toBeUndefined();
	});

	it('quotes a single type', () => {
		expect(composeJournalFilter(filter({ types: ['ALARM'] }))).toBe(
			'type = "ALARM"',
		);
	});

	it('joins several types with OR in parentheses', () => {
		expect(composeJournalFilter(filter({ types: ['STEP', 'ALARM'] }))).toBe(
			'(type = "STEP" OR type = "ALARM")',
		);
	});

	it('joins the groups with AND', () => {
		expect(
			composeJournalFilter({
				types: ['ALARM', 'SYSTEM'],
				datacenter: 'datacenters/dc_b',
				operation: 'operations/op-1',
			}),
		).toBe(
			'(type = "ALARM" OR type = "SYSTEM") AND datacenter = "datacenters/dc_b" AND operation = "operations/op-1"',
		);
	});

	it('adds the resource prefix to a bare datacenter and operation id', () => {
		expect(
			composeJournalFilter(filter({ datacenter: 'dc_b', operation: 'op-1' })),
		).toBe('datacenter = "datacenters/dc_b" AND operation = "operations/op-1"');
	});

	it('keeps a full resource name as it is', () => {
		expect(composeJournalFilter(filter({ operation: 'operations/op-1' }))).toBe(
			'operation = "operations/op-1"',
		);
	});

	it('trims the value and keeps spaces inside', () => {
		expect(composeJournalFilter(filter({ operation: '  my op  ' }))).toBe(
			'operation = "operations/my op"',
		);
	});

	it('escapes quotes so the value cannot leave its string', () => {
		expect(
			composeJournalFilter(filter({ operation: 'a" OR type = "ALARM' })),
		).toBe('operation = "operations/a\\" OR type = \\"ALARM"');
	});

	it('escapes backslashes, also in front of a quote', () => {
		expect(composeJournalFilter(filter({ operation: 'a\\b' }))).toBe(
			'operation = "operations/a\\\\b"',
		);
		expect(composeJournalFilter(filter({ operation: 'x\\"' }))).toBe(
			'operation = "operations/x\\\\\\""',
		);
	});

	it('ignores a value that is only spaces', () => {
		expect(composeJournalFilter(filter({ operation: '   ' }))).toBeUndefined();
	});
});
