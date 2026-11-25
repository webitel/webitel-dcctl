import { describe, expect, it } from 'vitest';

import { currentStep, isOperationShown } from './operationLine';

describe('isOperationShown', () => {
	it.each([
		['RUNNING', true],
		['INTERRUPTED', true],
		['SUCCEEDED', false],
		['FAILED', false],
		['ABANDONED', false],
		[undefined, false],
	])('%s -> %s', (state, expected) => {
		expect(isOperationShown({ state } as never)).toBe(expected);
	});

	it('is false without an operation', () => {
		expect(isOperationShown(undefined)).toBe(false);
	});
});

describe('currentStep', () => {
	it('is the running step', () => {
		expect(
			currentStep({
				steps: [
					{ index: 0, state: 'SUCCEEDED' },
					{ index: 1, state: 'RUNNING' },
					{ index: 2, state: 'PENDING' },
				],
			} as never)?.index,
		).toBe(1);
	});

	it('is the failed step of an interrupted operation', () => {
		expect(
			currentStep({
				steps: [
					{ index: 0, state: 'SUCCEEDED' },
					{ index: 1, state: 'FAILED' },
					{ index: 2, state: 'PENDING' },
				],
			} as never)?.index,
		).toBe(1);
	});

	it('is the first pending step when nothing runs or failed', () => {
		expect(
			currentStep({
				steps: [
					{ index: 0, state: 'SUCCEEDED' },
					{ index: 1, state: 'PENDING' },
				],
			} as never)?.index,
		).toBe(1);
	});

	it('is undefined without steps', () => {
		expect(currentStep({} as never)).toBeUndefined();
	});
});
