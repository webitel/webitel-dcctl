import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest';

import { encodeBasicToken } from '@/utils/basicAuth';
import { ageSeconds, isStaleAge } from '@/utils/dataAge';
import { buildDatacenter } from './datacenterVariants';
import { MOCK_USER } from './handlers';
import { server } from './server';
import { setStatusVariant } from './statusVariants';

const NOW = Date.parse('2026-11-23T10:15:00Z');

const build = (...args: Parameters<typeof buildDatacenter>) => {
	const dc = buildDatacenter(...args);
	if (!dc) throw new Error('no datacenter');
	return dc;
};

describe('datacenter variants', () => {
	it('normal: both datacenters have fresh data and every check passes', () => {
		for (const id of ['dc_a', 'dc_b']) {
			const dc = build('normal', id, NOW);

			expect(isStaleAge(ageSeconds(dc.observe_time, NOW))).toBe(false);
			expect(dc.sources?.every((source) => source.reachable)).toBe(true);
			expect(dc.info_checks?.every((check) => check.result === 'PASS')).toBe(
				true,
			);
		}
	});

	it('normal: the active datacenter has a primary, the standby a standby leader', () => {
		expect(build('normal', 'dc_a', NOW).patroni?.role).toBe('PRIMARY');
		expect(build('normal', 'dc_b', NOW).patroni?.role).toBe('STANDBY_LEADER');
	});

	it('alarm: a system job misses a drained node but is still healthy, another misses a node it should run on', () => {
		const dc = build('alarm', 'dc_a', NOW);
		const jobs = dc.nomad?.stages?.flatMap((stage) => stage.jobs ?? []) ?? [];
		const drained = jobs.find((job) => job.job === 'freeswitch');
		const failing = jobs.find((job) => job.job === 'filebeat');

		expect(dc.health).toBe('DEGRADED');
		expect(drained?.state).toBe('HEALTHY');
		expect(drained?.healthy_node_count).toBe(drained?.expected_node_count);
		expect(drained?.missing_nodes?.[0]?.reason).toBe('DRAINING');
		expect(failing?.state).toBe('UNHEALTHY');
		expect(failing?.missing_nodes?.[0]?.reason).toBe('NO_ALLOCATION');
	});

	it('alarm: the standby datacenter has diverged with a primary and a lost member', () => {
		const dc = build('alarm', 'dc_b', NOW);

		expect(dc.state).toBe('DB_SPLIT');
		expect(dc.patroni?.divergence_bytes).toBeDefined();
		expect(dc.patroni?.members?.some((m) => !m.reachable)).toBe(true);
	});

	it('unreachable: the active datacenter keeps its last data with old sources', () => {
		const dc = build('unreachable', 'dc_a', NOW);

		expect(dc.state).toBe('UNREACHABLE');
		expect(dc.patroni?.members?.length).toBeGreaterThan(0);
		expect(dc.sources?.every((source) => !source.reachable)).toBe(true);
		expect(ageSeconds(dc.sources?.[0]?.observe_time, NOW)).toBeGreaterThan(60);
	});

	it('stale: the last cycle finished long ago', () => {
		const dc = build('stale', 'dc_a', NOW);

		expect(isStaleAge(ageSeconds(dc.observe_time, NOW))).toBe(true);
	});

	it('stretch: the leader is in the active datacenter only', () => {
		const active = build('stretch', 'dc_a', NOW);
		const standby = build('stretch', 'dc_b', NOW);

		expect(active.patroni?.leader).toBe('pg-a1');
		expect(standby.patroni?.leader).toBeUndefined();
		expect(standby.patroni?.upstream_datacenter).toBeUndefined();
		expect(standby.patroni?.members?.length).toBeGreaterThan(0);
	});

	it.each(['normal', 'alarm', 'unreachable', 'stale', 'stretch'] as const)(
		'%s: sends no zero, empty or false value, like the gateway',
		(variant) => {
			for (const id of ['dc_a', 'dc_b']) {
				const body = JSON.parse(JSON.stringify(build(variant, id, NOW)));
				const explicit: string[] = [];
				const walk = (value: unknown, path: string) => {
					if (value === 0 || value === '0' || value === false || value === '') {
						explicit.push(path);
					} else if (value && typeof value === 'object') {
						for (const [key, child] of Object.entries(value)) {
							walk(child, `${path}.${key}`);
						}
					}
				};
				walk(body, id);
				expect(explicit).toEqual([]);
			}
		},
	);

	it('has nothing for a datacenter that is not in the config', () => {
		expect(buildDatacenter('normal', 'dc_x', NOW)).toBeUndefined();
	});
});

describe('mock GET /v1/datacenters/{dc}', () => {
	beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
	afterEach(() => {
		server.resetHandlers();
		setStatusVariant('normal');
	});
	afterAll(() => server.close());

	const get = (id: string) =>
		fetch(`http://localhost/api/v1/datacenters/${id}`, {
			headers: {
				Authorization: `Basic ${encodeBasicToken(MOCK_USER.name, MOCK_USER.password)}`,
			},
		});

	it('serves the datacenter for the selected variant', async () => {
		setStatusVariant('alarm');

		const body = await (await get('dc_b')).json();

		expect(body.state).toBe('DB_SPLIT');
	});

	it('serves 404 with the not-found id for an unknown datacenter', async () => {
		const response = await get('dc_x');

		expect(response.status).toBe(404);
		expect((await response.json()).id).toBe('dcctl.datacenter.not_found');
	});

	it('serves 503 for the unavailable variant', async () => {
		setStatusVariant('unavailable');

		expect((await get('dc_a')).status).toBe(503);
	});

	it('requires credentials', async () => {
		const response = await fetch('http://localhost/api/v1/datacenters/dc_a');

		expect(response.status).toBe(401);
	});
});
