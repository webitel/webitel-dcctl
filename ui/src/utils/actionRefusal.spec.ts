import { describe, expect, it } from 'vitest';

import { metadataEntries, refusalKey } from './actionRefusal';

describe('refusalKey', () => {
	it.each([
		['dcctl.lock.busy', 'lockBusy'],
		['dcctl.operation.running', 'operationRunning'],
		['dcctl.preflight.failed', 'preflightFailed'],
		['dcctl.leader.unavailable', 'leaderUnavailable'],
		['dcctl.something.new', null],
		['', null],
		[undefined, null],
	])('%s -> %s', (id, key) => {
		expect(refusalKey(id)).toBe(key);
	});
});

describe('metadataEntries', () => {
	it('cuts operation and datacenter names to ids and keeps the rest', () => {
		expect(
			metadataEntries({
				operation: 'operations/0b9f',
				datacenter: 'datacenters/dc_a',
				holder: 'dcctl-a1',
			}),
		).toEqual([
			['operation', '0b9f'],
			['datacenter', 'dc_a'],
			['holder', 'dcctl-a1'],
		]);
	});

	it('is empty without metadata', () => {
		expect(metadataEntries(undefined)).toEqual([]);
	});
});
