import { describe, expect, it } from 'vitest';

import { sortAlarms, sortDatacenters } from './ordering';

const names = (list: readonly { name?: string }[]) => list.map((dc) => dc.name);

describe('sortDatacenters', () => {
	it('puts the active datacenter first, then sorts by name', () => {
		const sorted = sortDatacenters(
			[
				{ name: 'datacenters/dc_c' },
				{ name: 'datacenters/dc_b' },
				{ name: 'datacenters/dc_a' },
			],
			'datacenters/dc_b',
		);

		expect(names(sorted)).toEqual([
			'datacenters/dc_b',
			'datacenters/dc_a',
			'datacenters/dc_c',
		]);
	});

	it('keeps the active datacenter first when it is UNREACHABLE', () => {
		const sorted = sortDatacenters(
			[
				{ name: 'datacenters/dc_a', state: 'STANDBY' },
				{ name: 'datacenters/dc_b', state: 'UNREACHABLE' },
				{ name: 'datacenters/dc_c', state: 'STANDBY' },
			],
			'datacenters/dc_b',
		);

		expect(names(sorted)).toEqual([
			'datacenters/dc_b',
			'datacenters/dc_a',
			'datacenters/dc_c',
		]);
	});

	it('sorts by name alone when there is no active datacenter', () => {
		const sorted = sortDatacenters([
			{ name: 'datacenters/dc_b' },
			{ name: 'datacenters/dc_a' },
		]);

		expect(names(sorted)).toEqual(['datacenters/dc_a', 'datacenters/dc_b']);
	});

	it('does not mutate the input', () => {
		const input = [{ name: 'datacenters/dc_b' }, { name: 'datacenters/dc_a' }];

		const sorted = sortDatacenters(input, 'datacenters/dc_a');

		expect(sorted).not.toBe(input);
		expect(names(input)).toEqual(['datacenters/dc_b', 'datacenters/dc_a']);
	});
});

describe('sortAlarms', () => {
	it('puts critical first, then the oldest first within a severity', () => {
		const input = [
			{
				severity: 'WARNING',
				start_time: '2026-11-23T10:00:00Z',
				detail: 'w-old',
			},
			{
				severity: 'CRITICAL',
				start_time: '2026-11-23T10:10:00Z',
				detail: 'c-new',
			},
			{
				severity: 'CRITICAL',
				start_time: '2026-11-23T10:05:00Z',
				detail: 'c-old',
			},
			{
				severity: 'WARNING',
				start_time: '2026-11-23T10:20:00Z',
				detail: 'w-new',
			},
		] as const;

		const sorted = sortAlarms(input);

		expect(sorted.map((a) => a.detail)).toEqual([
			'c-old',
			'c-new',
			'w-old',
			'w-new',
		]);
		expect(input.map((a) => a.detail)).toEqual([
			'w-old',
			'c-new',
			'c-old',
			'w-new',
		]);
	});

	it('puts an alarm without start_time after dated ones of the same severity', () => {
		const sorted = sortAlarms([
			{ severity: 'WARNING', detail: 'undated' },
			{
				severity: 'WARNING',
				start_time: '2026-11-23T10:00:00Z',
				detail: 'dated',
			},
			{ severity: 'CRITICAL', detail: 'critical-undated' },
		] as const);

		expect(sorted.map((a) => a.detail)).toEqual([
			'critical-undated',
			'dated',
			'undated',
		]);
	});

	it('orders undated alarms by datacenter, not by input order', () => {
		const sorted = sortAlarms([
			{ severity: 'WARNING', datacenter: 'datacenters/dc_b' },
			{ severity: 'WARNING', datacenter: 'datacenters/dc_a' },
		] as const);

		expect(sorted.map((a) => a.datacenter)).toEqual([
			'datacenters/dc_a',
			'datacenters/dc_b',
		]);
	});
});
