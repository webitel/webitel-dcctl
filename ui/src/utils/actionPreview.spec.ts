import { describe, expect, it } from 'vitest';

import type { WebitelDcctlV1Check as Check } from '@/api/gen/_models';

import {
	blockingChecks,
	buildConfirmation,
	discardBytes,
	groupChecks,
	previewOfOperation,
	previewOfPreflight,
	requiredConfirmations,
} from './actionPreview';

const isolation: Check = {
	id: 'coord.isolation',
	result: 'FAIL',
	effect: 'CONFIRM',
	confirmation: 'isolation',
};
const divergence = (facts: Check['facts']): Check => ({
	id: 'patroni.divergence',
	result: 'FAIL',
	effect: 'CONFIRM',
	confirmation: 'max_discard_bytes',
	facts,
});

describe('groupChecks', () => {
	it('puts failures first, then warnings, then the rest', () => {
		const checks: Check[] = [
			{ id: 'a', result: 'PASS' },
			{ id: 'b', result: 'FAIL' },
			{ id: 'c', result: 'WARN' },
			{ id: 'd', result: 'SKIP' },
		];
		const groups = groupChecks(checks);
		expect(groups.fail.map((c) => c.id)).toEqual(['b']);
		expect(groups.warn.map((c) => c.id)).toEqual(['c']);
		expect(groups.rest.map((c) => c.id)).toEqual(['a', 'd']);
	});
});

describe('discardBytes', () => {
	it.each([
		[{ divergence_bytes: 4096 }, '4096'],
		[{ lag_bytes: 12 }, '12'],
		[{ divergence_bytes: '77' }, '77'],
		[{ divergence_bytes: 0 }, '0'],
		[{ known: false }, null],
		[{ divergence_bytes: -1 }, null],
		[undefined, null],
	] as const)('%j -> %s', (facts, expected) => {
		expect(discardBytes(divergence(facts as Check['facts']))).toBe(expected);
	});
});

describe('requiredConfirmations', () => {
	it('groups failed confirm checks by field', () => {
		const needs = requiredConfirmations([
			isolation,
			divergence({ divergence_bytes: 100 }),
			{ ...isolation, id: 'other.isolation' },
			{ ...isolation, result: 'PASS', id: 'passed' },
			{ id: 'block', result: 'FAIL', effect: 'BLOCK' },
		]);
		expect(needs.map((n) => n.field)).toEqual([
			'isolation',
			'max_discard_bytes',
		]);
		expect(needs[0].checks).toHaveLength(2);
		expect(needs[1].bytes).toBe('100');
	});

	it('takes the largest amount when several checks share the bound', () => {
		const [need] = requiredConfirmations([
			divergence({ divergence_bytes: 100 }),
			divergence({ lag_bytes: 5000 }),
		]);
		expect(need.bytes).toBe('5000');
	});

	it('leaves out an amount that is not known: no bound covers it', () => {
		expect(requiredConfirmations([divergence({ known: false })])).toEqual([]);
	});
});

describe('blockingChecks', () => {
	it('lists blocking failures and unknown discard amounts', () => {
		const block: Check = { id: 'b', result: 'FAIL', effect: 'BLOCK' };
		const unknown = divergence({ known: false });
		const known = divergence({ divergence_bytes: 1 });
		expect(blockingChecks([block, unknown, known, isolation])).toEqual([
			block,
			unknown,
		]);
	});
});

describe('buildConfirmation', () => {
	const needs = requiredConfirmations([
		isolation,
		divergence({ divergence_bytes: 4096 }),
	]);

	it('sends only what is accepted, the bound as the shown amount', () => {
		expect(
			buildConfirmation(needs, new Set(['isolation', 'max_discard_bytes'])),
		).toEqual({ isolation: true, max_discard_bytes: '4096' });
		expect(buildConfirmation(needs, new Set(['isolation']))).toEqual({
			isolation: true,
		});
	});

	it('is absent when nothing is accepted', () => {
		expect(buildConfirmation(needs, new Set())).toBeUndefined();
	});
});

describe('previews', () => {
	it('reads an operation of a dry run', () => {
		expect(
			previewOfOperation({
				checks: [{ id: 'a' }],
				steps: [{ type: 'VERIFY' }],
				mode: 'QUORUM',
			}),
		).toEqual({
			checks: [{ id: 'a' }],
			steps: [{ type: 'VERIFY' }],
			mode: 'QUORUM',
		});
	});

	it('is empty for an operation without checks and steps', () => {
		expect(previewOfOperation({})).toEqual({
			checks: [],
			steps: [],
			mode: undefined,
		});
	});

	it('has no preview without preflight', () => {
		expect(previewOfPreflight(undefined)).toBeNull();
		expect(previewOfPreflight({ mode: 'FORCED' })?.mode).toBe('FORCED');
	});
});
