import { describe, expect, it } from 'vitest';

import {
	appsSummary,
	resourceId,
	sourcesSummary,
	unreachableSources,
} from './datacenterSummary';

describe('resourceId', () => {
	it('drops the collection prefix', () => {
		expect(resourceId('datacenters/dc_b')).toBe('dc_b');
		expect(resourceId('operations/0b9f')).toBe('0b9f');
	});
	it('keeps a bare name and tolerates undefined', () => {
		expect(resourceId('dc_b')).toBe('dc_b');
		expect(resourceId(undefined)).toBe('');
	});
});

describe('appsSummary', () => {
	it('is healthy over all managed jobs', () => {
		expect(
			appsSummary({
				nomad: {
					job_counts: {
						healthy_count: 10,
						unhealthy_count: 1,
						stopped_count: 2,
						stopping_count: 1,
					},
				},
			}),
		).toEqual({ healthy: 10, total: 14 });
	});

	it('is null when Nomad data is absent', () => {
		expect(appsSummary({})).toBeNull();
		expect(appsSummary({ nomad: {} })).toBeNull();
	});
});

describe('sourcesSummary', () => {
	it('counts reachable over all sources', () => {
		expect(
			sourcesSummary({
				sources: [
					{ reachable: true },
					{ reachable: false },
					{ reachable: true },
				],
			}),
		).toEqual({ reachable: 2, total: 3 });
	});
	it('is 0 of 0 without sources', () => {
		expect(sourcesSummary({})).toEqual({ reachable: 0, total: 0 });
	});
});

describe('unreachableSources', () => {
	const now = Date.parse('2026-11-23T10:15:00Z');

	it('lists the systems that do not answer with the age of their last data', () => {
		expect(
			unreachableSources(
				{
					sources: [
						{
							system: 'CONSUL',
							reachable: true,
							observe_time: '2026-11-23T10:14:59Z',
						},
						{
							system: 'NOMAD',
							reachable: false,
							observe_time: '2026-11-23T10:12:00Z',
						},
					],
				},
				now,
			),
		).toEqual([{ system: 'NOMAD', age: 180 }]);
	});

	it('has no age for a source that was never read', () => {
		expect(
			unreachableSources(
				{ sources: [{ system: 'PATRONI', reachable: false }] },
				now,
			),
		).toEqual([{ system: 'PATRONI', age: null }]);
	});
});
