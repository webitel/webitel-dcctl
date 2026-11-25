import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest';

import { encodeBasicToken } from '@/utils/basicAuth';
import { ageSeconds, isStaleAge } from '@/utils/dataAge';
import { MOCK_USER } from './handlers';
import { server } from './server';
import {
	buildStatus,
	selectVariantFromUrl,
	setStatusVariant,
} from './statusVariants';

const NOW = Date.parse('2026-11-23T10:15:00Z');

describe('status variants', () => {
	it('normal: fresh data, no alarms, every voter reachable', () => {
		const status = buildStatus('normal', NOW);

		expect(isStaleAge(ageSeconds(status.observe_time, NOW))).toBe(false);
		expect(status.alarms).toEqual([]);
		expect(status.voters?.every((voter) => voter.reachable)).toBe(true);
	});

	it('alarm: a critical and warnings with a split datacenter', () => {
		const status = buildStatus('alarm', NOW);

		expect(status.alarms?.some((a) => a.severity === 'CRITICAL')).toBe(true);
		expect(status.alarms?.some((a) => a.severity === 'WARNING')).toBe(true);
		expect(
			status.datacenters?.find((dc) => dc.name === 'datacenters/dc_b')?.state,
		).toBe('DB_SPLIT');
	});

	it('operation: a running operation', () => {
		const status = buildStatus('operation', NOW);

		expect(status.operation?.state).toBe('RUNNING');
		expect(status.operation?.steps?.length).toBeGreaterThan(1);
	});

	it('unreachable: the active datacenter does not answer, with old source data', () => {
		const status = buildStatus('unreachable', NOW);
		const active = status.datacenters?.find(
			(dc) => dc.name === status.active_datacenter,
		);

		expect(active?.state).toBe('UNREACHABLE');
		expect(active?.sources?.every((source) => !source.reachable)).toBe(true);
		expect(ageSeconds(active?.sources?.[0]?.observe_time, NOW)).toBeGreaterThan(
			60,
		);
	});

	it('stale: the last cycle finished long ago', () => {
		const status = buildStatus('stale', NOW);

		expect(isStaleAge(ageSeconds(status.observe_time, NOW))).toBe(true);
	});
});

describe('mock GET /v1/status', () => {
	beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
	afterEach(() => {
		server.resetHandlers();
		setStatusVariant('normal');
	});
	afterAll(() => server.close());

	const get = () =>
		fetch('http://localhost/api/v1/status', {
			headers: {
				Authorization: `Basic ${encodeBasicToken(MOCK_USER.name, MOCK_USER.password)}`,
			},
		});

	it('serves the variant picked from the URL', async () => {
		selectVariantFromUrl('?mock=alarm');

		const body = await (await get()).json();

		expect(body.alarms.length).toBeGreaterThan(0);
	});

	it('ignores an unknown variant name', async () => {
		selectVariantFromUrl('?mock=bogus');

		const body = await (await get()).json();

		expect(body.alarms).toEqual([]);
	});

	it('serves 503 for the unavailable variant', async () => {
		setStatusVariant('unavailable');

		expect((await get()).status).toBe(503);
	});

	it('keeps requiring credentials', async () => {
		const response = await fetch('http://localhost/api/v1/status');

		expect(response.status).toBe(401);
	});
});
