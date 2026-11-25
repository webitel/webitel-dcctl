import { describe, expect, it } from 'vitest';

import {
	durationMs,
	generationRange,
	isBehind,
	isMissingFromStatus,
	isOlderOperation,
	isTerminalState,
	orderedSteps,
	pointOfNoReturn,
	stageOfStep,
} from './operationView';

describe('isTerminalState', () => {
	it.each([
		['SUCCEEDED', true],
		['FAILED', true],
		['ABANDONED', true],
		['RUNNING', false],
		['INTERRUPTED', false],
		['STATE_UNSPECIFIED', false],
		[undefined, false],
	])('%s -> %s', (state, expected) => {
		expect(isTerminalState(state as never)).toBe(expected);
	});
});

describe('isOlderOperation', () => {
	const at = (update_time?: string) => ({ update_time });

	it('is true for an earlier update time', () => {
		expect(
			isOlderOperation(at('2026-11-23T10:15:03Z'), at('2026-11-23T10:15:04Z')),
		).toBe(true);
	});

	it.each([
		['the same time', '2026-11-23T10:15:04Z', '2026-11-23T10:15:04Z'],
		['a later time', '2026-11-23T10:15:05Z', '2026-11-23T10:15:04Z'],
		['no time on the new one', undefined, '2026-11-23T10:15:04Z'],
		['no time on the shown one', '2026-11-23T10:15:03Z', undefined],
	])('is false for %s', (_name, next, current) => {
		expect(isOlderOperation(at(next), at(current))).toBe(false);
	});
});

describe('isBehind', () => {
	it('is true when the status shows a later change of the same operation', () => {
		expect(
			isBehind(
				{ name: 'operations/a', update_time: '2026-11-23T10:15:04Z' },
				{ name: 'operations/a', update_time: '2026-11-23T10:15:09Z' },
			),
		).toBe(true);
	});

	it.each([
		['the same change', 'operations/a', '2026-11-23T10:15:04Z'],
		['an earlier change', 'operations/a', '2026-11-23T10:15:01Z'],
		['another operation', 'operations/b', '2026-11-23T10:15:09Z'],
	])('is false for %s', (_name, name, time) => {
		expect(
			isBehind(
				{ name: 'operations/a', update_time: '2026-11-23T10:15:04Z' },
				{ name, update_time: time },
			),
		).toBe(false);
	});

	it('is false without a status operation', () => {
		expect(isBehind({ name: 'operations/a' }, undefined)).toBe(false);
	});
});

describe('orderedSteps', () => {
	it('sorts by index and gives a step without index (0) the first place', () => {
		const steps = orderedSteps({
			steps: [
				{ index: 2, type: 'VERIFY' },
				{ type: 'APPS_PREFLIGHT' },
				{ index: 1, type: 'APPS_STOP' },
			],
		});
		expect(steps.map((step) => [step.index, step.type])).toEqual([
			[0, 'APPS_PREFLIGHT'],
			[1, 'APPS_STOP'],
			[2, 'VERIFY'],
		]);
	});

	it('is empty without steps', () => {
		expect(orderedSteps({})).toEqual([]);
	});
});

describe('stageOfStep', () => {
	it('reads the stage and datacenter of an apps step', () => {
		expect(
			stageOfStep({
				type: 'APPS_STOP',
				stage: 3,
				datacenter: 'datacenters/dc_a',
			}),
		).toEqual({ datacenter: 'dc_a', stage: 3 });
	});

	it('takes an omitted stage as 0', () => {
		expect(
			stageOfStep({ type: 'APPS_START', datacenter: 'datacenters/dc_b' }),
		).toEqual({ datacenter: 'dc_b', stage: 0 });
	});

	it.each([
		[
			'a step of another type',
			{ type: 'GENERATION', datacenter: 'datacenters/dc_a' },
		],
		['an apps step without datacenter', { type: 'APPS_STOP', stage: 1 }],
		[
			'APPS_PREFLIGHT',
			{ type: 'APPS_PREFLIGHT', datacenter: 'datacenters/dc_a' },
		],
	])('is null for %s', (_name, step) => {
		expect(stageOfStep(step as never)).toBeNull();
	});
});

describe('pointOfNoReturn', () => {
	it('is none for an ordinary step', () => {
		expect(pointOfNoReturn({ state: 'RUNNING' })).toBe('none');
	});

	it.each([
		['PENDING', undefined, 'ahead'],
		['RUNNING', 'RUNNING', 'running'],
		['SUCCEEDED', 'RUNNING', 'passed'],
		['SUCCEEDED', 'SUCCEEDED', 'passed'],
		['FAILED', 'INTERRUPTED', 'unknown'],
		['SKIPPED', 'RUNNING', 'unknown'],
		['PENDING', 'FAILED', 'ahead'],
		['RUNNING', 'FAILED', 'ahead'],
		['FAILED', 'FAILED', 'ahead'],
	])(
		'marks a %s step of a %s operation as %s',
		(state, operationState, expected) => {
			expect(
				pointOfNoReturn(
					{ point_of_no_return: true, state } as never,
					operationState as never,
				),
			).toBe(expected);
		},
	);
});

describe('isMissingFromStatus', () => {
	const shown = {
		name: 'operations/a',
		state: 'RUNNING' as const,
		update_time: '2026-11-23T10:15:04Z',
	};

	it('is true when a status observed later lists no operation or another one', () => {
		expect(isMissingFromStatus(shown, undefined, '2026-11-23T10:15:09Z')).toBe(
			true,
		);
		expect(
			isMissingFromStatus(
				shown,
				{ name: 'operations/b' },
				'2026-11-23T10:15:09Z',
			),
		).toBe(true);
	});

	it.each([
		['lists the operation', { name: 'operations/a' }, '2026-11-23T10:15:09Z'],
		['was observed before the last change', undefined, '2026-11-23T10:15:01Z'],
		['has no observe time', undefined, undefined],
	])('is false when the status %s', (_name, fromStatus, observed) => {
		expect(isMissingFromStatus(shown, fromStatus, observed)).toBe(false);
	});

	it('is false for a page that already shows a final state', () => {
		expect(
			isMissingFromStatus(
				{ ...shown, state: 'FAILED' },
				undefined,
				'2026-11-23T10:15:09Z',
			),
		).toBe(false);
	});
});

describe('durationMs', () => {
	const NOW = Date.parse('2026-11-23T10:15:30Z');

	it('is the time between start and end', () => {
		expect(
			durationMs('2026-11-23T10:15:00Z', '2026-11-23T10:15:04Z', NOW),
		).toBe(4000);
	});

	it('counts a running step up to now', () => {
		expect(durationMs('2026-11-23T10:15:00Z', undefined, NOW)).toBe(30_000);
	});

	it('is null without a start', () => {
		expect(durationMs(undefined, '2026-11-23T10:15:04Z', NOW)).toBeNull();
	});

	it('never goes below zero when the clock runs behind', () => {
		expect(durationMs('2026-11-23T10:15:40Z', undefined, NOW)).toBe(0);
	});
});

describe('generationRange', () => {
	it('shows the previous and the new generation', () => {
		expect(
			generationRange({ previous_generation: '7', new_generation: '8' }),
		).toEqual({ from: '7', to: '8' });
	});

	it('takes an omitted previous generation as 0', () => {
		expect(generationRange({ new_generation: '1' })).toEqual({
			from: '0',
			to: '1',
		});
	});

	it('has no new generation yet when only the previous one is set', () => {
		expect(generationRange({ previous_generation: '7' })).toEqual({
			from: '7',
			to: null,
		});
	});

	it('is null for an operation that writes no generation', () => {
		expect(generationRange({})).toBeNull();
	});
});
