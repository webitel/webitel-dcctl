import { describe, expect, it } from 'vitest';

import {
	allocationEntries,
	DATACENTER_TABS,
	divergence,
	jobColor,
	memberColor,
	sourceFor,
	sourceRow,
	systemJobCoverage,
	tabFromQuery,
} from './datacenterDetail';

describe('tabFromQuery', () => {
	it.each(DATACENTER_TABS)('accepts %s', (tab) => {
		expect(tabFromQuery(tab)).toBe(tab);
	});

	it('falls back to Patroni for a missing or unknown tab', () => {
		expect(tabFromQuery(undefined)).toBe('patroni');
		expect(tabFromQuery('bogus')).toBe('patroni');
		expect(tabFromQuery(['apps', 'info'])).toBe('patroni');
		expect(tabFromQuery(null)).toBe('patroni');
	});
});

describe('divergence', () => {
	it('is red with the point and the size when the datacenter diverged', () => {
		expect(
			divergence({
				divergence_state: 'DIVERGED',
				divergence_lsn: '0/5000060',
				divergence_bytes: '16777216',
			}),
		).toMatchObject({
			kind: 'diverged',
			color: 'red',
			lsn: '0/5000060',
			bytes: '16777216',
		});
	});

	it('is yellow when it may have diverged', () => {
		expect(divergence({ divergence_state: 'UNKNOWN' })).toMatchObject({
			kind: 'maybe',
			color: 'yellow',
		});
	});

	it('is not diverged only when the state says so', () => {
		expect(divergence({ divergence_state: 'NOT_DIVERGED' }).kind).toBe('none');
	});

	it('does not take an empty LSN for "not diverged"', () => {
		expect(divergence({ divergence_state: 'UNKNOWN' }).kind).not.toBe('none');
		expect(divergence({ divergence_state: 'DIVERGED' }).kind).toBe('diverged');
	});

	it.each([
		[{ divergence_state: 'DIVERGENCE_STATE_UNSPECIFIED' as const }],
		[{}],
		[undefined],
	])('has nothing to compare for %j', (patroni) => {
		expect(divergence(patroni).kind).toBe('unknown');
	});

	it('keeps a state it does not know and does not colour it', () => {
		expect(divergence({ divergence_state: 'FUTURE' } as never)).toMatchObject({
			kind: 'other',
			color: 'gray',
			raw: 'FUTURE',
		});
	});
});

describe('memberColor', () => {
	it('is red for a member that is not reachable, also when the flag is omitted', () => {
		expect(memberColor({ reachable: false })).toBe('red');
		expect(memberColor({})).toBe('red');
	});

	it('is yellow for an unusable member or a state that is not normal', () => {
		expect(memberColor({ reachable: true })).toBe('yellow');
		expect(
			memberColor({ reachable: true, usable: true, patroni_state: 'starting' }),
		).toBe('yellow');
	});

	it.each(['running', 'streaming'])('is green for %s', (patroni_state) => {
		expect(memberColor({ reachable: true, usable: true, patroni_state })).toBe(
			'green',
		);
	});

	it('is gray for a usable member that reports no state', () => {
		expect(memberColor({ reachable: true, usable: true })).toBe('gray');
	});
});

describe('jobColor', () => {
	it.each([
		['HEALTHY', 'green'],
		['UNHEALTHY', 'red'],
		['STOPPED', 'red'],
		['STOPPING', 'yellow'],
		['SOMETHING_NEW', 'gray'],
		[undefined, 'gray'],
	])('in the active datacenter %s is %s', (state, color) => {
		expect(jobColor(state as never, { expectRunning: true })).toBe(color);
	});

	it.each([
		['STOPPED', 'gray'],
		['STOPPING', 'yellow'],
		['HEALTHY', 'red'],
		['UNHEALTHY', 'red'],
		['SOMETHING_NEW', 'red'],
	])('in an inactive datacenter %s is %s', (state, color) => {
		expect(jobColor(state as never, { expectRunning: false })).toBe(color);
	});

	it.each([
		['HEALTHY', 'green'],
		['UNHEALTHY', 'red'],
		['STOPPING', 'yellow'],
		['STOPPED', 'gray'],
	])('while the role is unknown %s is %s', (state, color) => {
		expect(jobColor(state as never)).toBe(color);
	});
});

describe('allocationEntries', () => {
	it('lists the counts with the problem statuses first', () => {
		expect(
			allocationEntries({ running: 2, complete: 1, failed: 1, lost: 1 }),
		).toEqual([
			{ status: 'failed', count: 1, problem: true },
			{ status: 'lost', count: 1, problem: true },
			{ status: 'complete', count: 1, problem: false },
			{ status: 'running', count: 2, problem: false },
		]);
	});

	it('is empty without counts', () => {
		expect(allocationEntries(undefined)).toEqual([]);
	});
});

describe('systemJobCoverage', () => {
	it('is absent for a job that is not a system job', () => {
		expect(systemJobCoverage({ type: 'SERVICE' })).toBeNull();
	});

	it('is green when every expected node is healthy', () => {
		expect(
			systemJobCoverage({
				type: 'SYSTEM',
				healthy_node_count: 3,
				expected_node_count: 3,
			}),
		).toMatchObject({ healthy: 3, expected: 3, color: 'green', missing: [] });
	});

	it.each(['DRAINING', 'DOWN', 'INELIGIBLE'] as const)(
		'is yellow, not red, for a node that is %s',
		(reason) => {
			expect(
				systemJobCoverage({
					type: 'SYSTEM',
					healthy_node_count: 2,
					expected_node_count: 2,
					missing_nodes: [{ node: 'n3', reason }],
				})?.color,
			).toBe('yellow');
		},
	);

	it('is red for a node that should run the job and does not', () => {
		expect(
			systemJobCoverage({
				type: 'SYSTEM',
				healthy_node_count: 1,
				expected_node_count: 2,
				missing_nodes: [{ node: 'n2', reason: 'NO_ALLOCATION' }],
			}),
		).toMatchObject({ healthy: 1, expected: 2, color: 'red' });
	});

	it('is red when one missing node is a failure among drained ones', () => {
		expect(
			systemJobCoverage({
				type: 'SYSTEM',
				missing_nodes: [
					{ node: 'n3', reason: 'DRAINING' },
					{ node: 'n2', reason: 'NO_ALLOCATION' },
				],
			})?.color,
		).toBe('red');
	});

	it('is red for a reason it does not know', () => {
		expect(
			systemJobCoverage({
				type: 'SYSTEM',
				missing_nodes: [{ node: 'n2', reason: 'FUTURE' as never }],
			})?.color,
		).toBe('red');
	});

	it('takes omitted counts for 0, as the gateway omits zeros', () => {
		expect(systemJobCoverage({ type: 'SYSTEM' })).toMatchObject({
			healthy: 0,
			expected: 0,
			color: 'green',
		});
	});

	it('is yellow when fewer nodes are healthy than expected and nobody is listed', () => {
		expect(
			systemJobCoverage({
				type: 'SYSTEM',
				healthy_node_count: 1,
				expected_node_count: 2,
			})?.color,
		).toBe('yellow');
	});
});

describe('sources', () => {
	const NOW = Date.parse('2026-11-23T10:15:00Z');
	const sources = [
		{ system: 'CONSUL', reachable: true },
		{ system: 'PATRONI', reachable: false },
	] as const;

	it('finds the source of a system', () => {
		expect(sourceFor({ sources }, 'PATRONI')?.reachable).toBe(false);
		expect(sourceFor({ sources }, 'NOMAD')).toBeUndefined();
	});

	it('gives the age of the data of a source', () => {
		const row = sourceRow(
			{ system: 'NOMAD', observe_time: '2026-11-23T10:14:57Z' },
			NOW,
		);

		expect(row.age).toBe(3);
		expect(row.stale).toBe(false);
	});

	it('marks old data stale', () => {
		const row = sourceRow(
			{ system: 'NOMAD', observe_time: '2026-11-23T10:08:00Z' },
			NOW,
		);

		expect(row.age).toBe(420);
		expect(row.stale).toBe(true);
	});

	it('uses the threshold it is given', () => {
		const source = {
			system: 'NOMAD' as const,
			observe_time: '2026-11-23T10:14:30Z',
		};

		expect(sourceRow(source, NOW).stale).toBe(true);
		expect(sourceRow(source, NOW, 60).stale).toBe(false);
	});

	it('counts data without a time as stale', () => {
		const row = sourceRow({ system: 'NOMAD' }, NOW);

		expect(row.age).toBeNull();
		expect(row.stale).toBe(true);
	});
});
