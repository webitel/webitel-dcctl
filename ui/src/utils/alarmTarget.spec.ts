import { describe, expect, it } from 'vitest';

import { alarmTarget } from './alarmTarget';

const op = 'operations/0b9f';

describe('alarmTarget', () => {
	it.each(['OPERATION_INTERRUPTED', 'UNCOMMITTED'])(
		'%s opens the current operation',
		(type) => {
			expect(alarmTarget({ type } as never, op)).toEqual({
				name: 'operation',
				params: { id: '0b9f' },
			});
		},
	);

	it('falls back to the operations list when there is no operation', () => {
		expect(alarmTarget({ type: 'UNCOMMITTED' } as never)).toEqual({
			name: 'operations',
		});
	});

	it('opens the datacenter page for a datacenter alarm', () => {
		expect(
			alarmTarget({ type: 'STALE', datacenter: 'datacenters/dc_b' } as never),
		).toEqual({ name: 'datacenter', params: { dc: 'dc_b' } });
	});

	it('opens the datacenter page of a datacenter voter alarm', () => {
		expect(
			alarmTarget({
				type: 'VOTER_UNREACHABLE',
				voter_type: 'DATACENTER',
				datacenter: 'datacenters/dc_a',
			} as never),
		).toEqual({ name: 'datacenter', params: { dc: 'dc_a' } });
	});

	it.each(['QUORUM_LOST', 'LEASE_LOST', 'ARBITER_UNINITIALIZED'])(
		'%s points at the coordination block',
		(type) => {
			expect(alarmTarget({ type } as never)).toEqual({
				name: 'overview',
				hash: '#coordination',
			});
		},
	);

	it('points an arbiter voter alarm at the coordination block', () => {
		expect(
			alarmTarget({
				type: 'VOTER_UNREACHABLE',
				voter_type: 'ARBITER',
			} as never),
		).toEqual({ name: 'overview', hash: '#coordination' });
	});

	it('sends a conflict to the coordination block until the resolve dialog exists', () => {
		expect(alarmTarget({ type: 'CONFLICT' } as never)).toEqual({
			name: 'overview',
			hash: '#coordination',
		});
	});

	it.each(['MULTIPLE_PRIMARIES', 'DB_LEADER_MISMATCH'])(
		'%s points at the datacenter table, which shows the Patroni leaders',
		(type) => {
			expect(alarmTarget({ type } as never)).toEqual({
				name: 'overview',
				hash: '#datacenters',
			});
		},
	);

	it.each(['DB_SPLIT', 'DATACENTER_UNREACHABLE', 'APPS_DEGRADED'])(
		'%s without a datacenter points at the datacenter table',
		(type) => {
			expect(alarmTarget({ type } as never)).toEqual({
				name: 'overview',
				hash: '#datacenters',
			});
		},
	);

	it('does not send an unknown alarm type to the coordination block', () => {
		expect(alarmTarget({ type: 'FUTURE_THING' } as never)).toEqual({
			name: 'overview',
		});
	});

	it('opens the datacenter page for an unknown type that names a datacenter', () => {
		expect(
			alarmTarget({
				type: 'FUTURE_THING',
				datacenter: 'datacenters/dc_b',
			} as never),
		).toEqual({ name: 'datacenter', params: { dc: 'dc_b' } });
	});
});
