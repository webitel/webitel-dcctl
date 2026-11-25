import { describe, expect, it } from 'vitest';

import {
	healthColor,
	modeColor,
	operationStateColor,
	severityColor,
	stateColor,
	stepColor,
	toChipColor,
} from './statusMapping';

describe('severityColor', () => {
	it.each([
		['CRITICAL', 'red'],
		['WARNING', 'yellow'],
		['SEVERITY_UNSPECIFIED', 'gray'],
		[undefined, 'gray'],
	] as const)('%s -> %s', (severity, color) => {
		expect(severityColor(severity)).toBe(color);
	});
});

describe('healthColor', () => {
	it.each([
		['OK', 'green'],
		['DEGRADED', 'yellow'],
		['DOWN', 'red'],
		['UNKNOWN', 'gray'],
		['HEALTH_UNSPECIFIED', 'gray'],
		[undefined, 'gray'],
	] as const)('%s -> %s', (health, color) => {
		expect(healthColor(health)).toBe(color);
	});
});

describe('stateColor', () => {
	it.each([
		['ACTIVE', 'blue'],
		['STANDBY', 'gray'],
		['UNINITIALIZED', 'gray'],
		['STALE', 'yellow'],
		['NEEDS_RECOVERY', 'yellow'],
		['UNDETERMINED', 'yellow'],
		['SPLIT_BRAIN', 'red'],
		['DB_SPLIT', 'red'],
		['STATE_UNSPECIFIED', 'gray'],
		[undefined, 'gray'],
	] as const)('%s -> %s', (state, color) => {
		expect(stateColor(state)).toBe(color);
	});

	it('shows an unreachable datacenter gray, or red when it is the active one', () => {
		expect(stateColor('UNREACHABLE')).toBe('gray');
		expect(stateColor('UNREACHABLE', { isActive: false })).toBe('gray');
		expect(stateColor('UNREACHABLE', { isActive: true })).toBe('red');
	});
});

describe('stepColor', () => {
	it.each([
		['SUCCEEDED', undefined, 'green'],
		['SUCCEEDED', [], 'green'],
		['SUCCEEDED', ['slow'], 'yellow'],
		['RUNNING', undefined, 'blue'],
		['PENDING', undefined, 'gray'],
		['SKIPPED', undefined, 'gray'],
		['FAILED', undefined, 'red'],
		['FAILED', ['slow'], 'red'],
		['STATE_UNSPECIFIED', undefined, 'gray'],
	] as const)('%s with warnings %j -> %s', (state, warnings, color) => {
		expect(stepColor({ state, warnings })).toBe(color);
	});
});

describe('toChipColor', () => {
	it('maps every status colour to an SDK chip colour', () => {
		expect(toChipColor('red')).toBe('error');
		expect(toChipColor('yellow')).toBe('warning');
		expect(toChipColor('green')).toBe('success');
		expect(toChipColor('blue')).toBe('info');
		expect(toChipColor('gray')).toBe('secondary');
	});
});

describe('modeColor', () => {
	it('highlights FORCED in red and leaves the rest gray', () => {
		expect(modeColor('FORCED')).toBe('red');
		expect(modeColor('QUORUM')).toBe('gray');
		expect(modeColor(undefined)).toBe('gray');
	});
});

describe('operationStateColor', () => {
	it.each([
		['RUNNING', 'blue'],
		['SUCCEEDED', 'green'],
		['FAILED', 'red'],
		['INTERRUPTED', 'yellow'],
		['ABANDONED', 'gray'],
		['STATE_UNSPECIFIED', 'gray'],
		[undefined, 'gray'],
	] as const)('%s -> %s', (state, color) => {
		expect(operationStateColor(state)).toBe(color);
	});
});
