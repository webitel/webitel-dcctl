import { describe, expect, it } from 'vitest';

import { conflictCandidates } from './conflictCandidates';

describe('conflictCandidates', () => {
	it('lists the active datacenters of every record, valid or not', () => {
		// As the server reports a conflict: no record is effective, so none is valid.
		expect(
			conflictCandidates({
				voters: [
					{
						type: 'DATACENTER',
						valid: false,
						generation: '9',
						active_datacenter: 'datacenters/dc_b',
					},
					{
						type: 'DATACENTER',
						valid: false,
						generation: '9',
						active_datacenter: 'datacenters/dc_a',
					},
					{
						type: 'ARBITER',
						valid: false,
						generation: '9',
						active_datacenter: 'datacenters/dc_a',
					},
					{
						type: 'DATACENTER',
						valid: false,
						generation: '7',
						active_datacenter: 'datacenters/dc_c',
					},
					{ type: 'DATACENTER', generation: '9' },
				],
				datacenters: [
					{ name: 'datacenters/dc_a', patroni: { role: 'PRIMARY' } },
					{ name: 'datacenters/dc_b' },
				],
			}),
		).toEqual([
			{ id: 'dc_a', role: 'PRIMARY' },
			{ id: 'dc_b', role: undefined },
			{ id: 'dc_c', role: undefined },
		]);
	});

	it('keeps the conflicting records under an uncommitted higher generation', () => {
		expect(
			conflictCandidates({
				voters: [
					{ generation: '5', active_datacenter: 'datacenters/dc_a' },
					{ generation: '5', active_datacenter: 'datacenters/dc_b' },
					{ generation: '6', active_datacenter: 'datacenters/dc_b' },
				],
			}),
		).toEqual([
			{ id: 'dc_a', role: undefined },
			{ id: 'dc_b', role: undefined },
		]);
	});

	it('is empty without a status', () => {
		expect(conflictCandidates(undefined)).toEqual([]);
	});
});
