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
	it('lists the systems that do not answer', () => {
		expect(
			unreachableSources({
				sources: [
					{ system: 'CONSUL', reachable: true },
					{ system: 'NOMAD', reachable: false },
				],
			}),
		).toEqual(['NOMAD']);
	});
});
