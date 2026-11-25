import { describe, expect, it } from 'vitest';

import type { WebitelDcctlV1JournalEntry as Entry } from '@/api/gen/_models';
import { entryFacts, journalResultColor, journalRow } from './journalView';

const at = '2026-11-23T10:15:01Z';

describe('journalRow', () => {
	it('shows an operation entry with the operation type as the action', () => {
		const row = journalRow({
			name: 'journalEntries/20261123101501000000000-5c1e9a2b',
			create_time: at,
			type: 'OPERATION',
			operation_type: 'SWITCH',
			datacenter: 'datacenters/dc_b',
			operation: 'operations/op-1',
			initiator: { user: 'support', instance: { node: 'dcctl-a1' } },
			result: 'OK',
			detail: 'switch started',
		});
		expect(row).toMatchObject({
			id: '20261123101501000000000-5c1e9a2b',
			datacenter: 'dc_b',
			operationId: 'op-1',
			actionGroup: 'operationType',
			action: 'SWITCH',
			resultGroup: 'journalResult',
			color: 'green',
			detail: 'switch started',
			initiator: { kind: 'user', text: 'support', node: 'dcctl-a1' },
		});
	});

	it.each([
		['STEP', { step_type: 'APPS_STOP' }, 'stepType', 'APPS_STOP'],
		['SYSTEM', { system_action: 'LOCK_LOST' }, 'systemAction', 'LOCK_LOST'],
		[
			'ALARM',
			{ alarm_type: 'VOTER_UNREACHABLE' },
			'overview.alarms.type',
			'VOTER_UNREACHABLE',
		],
	] as const)(
		'takes the action of a %s entry from its own field',
		(type, fields, group, action) => {
			const row = journalRow({ name: 'journalEntries/1-a', type, ...fields });
			expect(row.actionGroup).toBe(group);
			expect(row.action).toBe(action);
		},
	);

	it('reads the result of an alarm as raised or cleared', () => {
		const row = journalRow({
			name: 'journalEntries/1-a',
			type: 'ALARM',
			alarm_type: 'QUORUM_LOST',
			result: 'ERROR',
		});
		expect(row.resultGroup).toBe('journalAlarmResult');
		expect(row.color).toBe('red');
	});

	it('names the system and the observing instance for what dcctl did itself', () => {
		const row = journalRow({
			name: 'journalEntries/1-a',
			type: 'SYSTEM',
			system_action: 'FENCE_APPS',
			initiator: { system: true, instance: { node: 'dcctl-a1' } },
		});
		expect(row.initiator).toEqual({
			kind: 'system',
			text: '',
			node: 'dcctl-a1',
		});
	});

	it('falls back to the instance when there is no user and no system flag', () => {
		const row = journalRow({
			name: 'journalEntries/1-a',
			initiator: { instance: { node: 'dcctl-b1' } },
		});
		expect(row.initiator).toEqual({
			kind: 'instance',
			text: 'dcctl-b1',
			node: 'dcctl-b1',
		});
	});

	it('has no initiator and no operation when the entry has none', () => {
		const row = journalRow({ name: 'journalEntries/1-a' });
		expect(row.initiator.kind).toBe('none');
		expect(row.operationId).toBe('');
		expect(row.datacenter).toBe('');
	});
});

describe('journalResultColor', () => {
	it.each([
		['OK', 'green'],
		['WARNING', 'yellow'],
		['ERROR', 'red'],
		['RESULT_UNSPECIFIED', 'gray'],
		[undefined, 'gray'],
		['SOMETHING_NEW', 'gray'],
	])('%s is %s', (result, color) => {
		expect(journalResultColor(result as Entry['result'])).toBe(color);
	});
});

describe('entryFacts', () => {
	it('lists params as text, nested values as JSON, sorted by key', () => {
		const facts = entryFacts({
			params: { mode: 'QUORUM', jobs: ['a', 'b'], new_generation: 8, ok: true },
		});
		expect(facts.params).toEqual([
			['jobs', '["a","b"]'],
			['mode', 'QUORUM'],
			['new_generation', '8'],
			['ok', 'true'],
		]);
	});

	it('keeps markup in a value as text', () => {
		const facts = entryFacts({
			params: { note: '<img src=x onerror=alert(1)>' },
		});
		expect(facts.params).toEqual([['note', '<img src=x onerror=alert(1)>']]);
	});

	it('lists every confirmation of a present block, with the omitted ones as zero', () => {
		const facts = entryFacts({
			confirmation: { isolation: true, max_discard_bytes: '4096' },
		});
		expect(facts.confirmation).toEqual([
			['isolation', 'true'],
			['max_discard_bytes', '4096'],
			['not_max_lsn', 'false'],
		]);
		expect(entryFacts({ confirmation: {} }).confirmation).toEqual([
			['isolation', 'false'],
			['max_discard_bytes', '0'],
			['not_max_lsn', 'false'],
		]);
	});

	it('has the error with its metadata', () => {
		const facts = entryFacts({
			error: {
				id: 'dcctl.lock.lost',
				reason: 'LOCK_LOST',
				detail: 'lost',
				metadata: { holder: 'dcctl-a1' },
			},
		});
		expect(facts.error).toEqual({
			id: 'dcctl.lock.lost',
			reason: 'LOCK_LOST',
			detail: 'lost',
			metadata: [['holder', 'dcctl-a1']],
		});
	});

	it('is empty for an entry without params, confirmation and error', () => {
		expect(entryFacts({})).toEqual({
			params: [],
			confirmation: [],
			error: undefined,
		});
	});
});
