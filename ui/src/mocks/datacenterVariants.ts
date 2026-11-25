import { HttpResponse } from 'msw';

import type {
	WebitelDcctlV1Datacenter as Datacenter,
	WebitelDcctlV1DatacenterNomadJob as Job,
	WebitelDcctlV1DatacenterPatroniMember as Member,
	WebitelDcctlV1Source as Source,
} from '@/api/gen/_models';
import { example } from './example';
import { getStatusVariant, type StatusVariant } from './statusVariants';

type Variant = StatusVariant;

const iso = (ms: number) => new Date(ms).toISOString();
const ago = (now: number, seconds: number) => iso(now - seconds * 1000);

const observer = {
	node: 'dcctl-a1',
	datacenter: 'datacenters/dc_a',
	address: '10.1.0.81:10023',
};

const sources = (
	now: number,
	subnet: number,
	secondsAgo = 3,
): readonly Source[] => [
	{
		system: 'CONSUL',
		endpoint: `10.${subnet}.0.89:8500`,
		reachable: true,
		observe_time: ago(now, secondsAgo),
	},
	{
		system: 'NOMAD',
		endpoint: `https://10.${subnet}.0.91:4646`,
		reachable: true,
		observe_time: ago(now, secondsAgo),
	},
	{
		system: 'PATRONI',
		endpoint: `https://10.${subnet}.0.95:8008`,
		reachable: true,
		observe_time: ago(now, secondsAgo),
	},
];

const member = (
	now: number,
	name: string,
	host: string,
	patch: Partial<Member>,
): Member => ({
	member: name,
	host,
	api_uri: `https://${host}:8008`,
	reachable: true,
	role: 'REPLICA',
	patroni_state: 'streaming',
	timeline: '5',
	replication_state: 'streaming',
	xlog: {
		received_location: '83886176',
		replayed_location: '83886176',
		replay_time: ago(now, 1),
	},
	version: '4.1.0',
	usable: true,
	...patch,
});

const job = (patch: Partial<Job>): Job => ({
	type: 'SERVICE',
	state: 'HEALTHY',
	modify_index: '4100',
	...patch,
});

const infoChecks = (datacenter: string): Datacenter['info_checks'] => [
	{
		id: 'info.api',
		result: 'PASS',
		datacenter,
		detail: 'webitel-api: 2 instances, both in the active datacenter',
		facts: { instances: 2, datacenters: ['dc_a'] },
	},
	{
		id: 'info.services',
		result: 'PASS',
		datacenter,
		detail: 'cases: 2 of 2 passing, storage: 3 of 3 passing',
		facts: {
			cases: { passing: 2, total: 2 },
			storage: { passing: 3, total: 3 },
		},
	},
	{
		id: 'info.rabbitmq',
		result: 'PASS',
		datacenter,
		detail: 'rabbitmq: 3 of 3 passing',
		facts: { service: 'rabbitmq', passing: 3, total: 3 },
	},
];

const activeDatacenter = (now: number): Datacenter => ({
	name: 'datacenters/dc_a',
	state: 'ACTIVE',
	health: 'OK',
	observer,
	observe_time: ago(now, 3),
	sources: sources(now, 1),
	consul: {
		servers: [
			{ address: '10.1.0.89:8500', reachable: true, consul_datacenter: 'dc_a' },
			{ address: '10.1.0.90:8500', reachable: true, consul_datacenter: 'dc_a' },
			{ address: '10.1.0.91:8500', reachable: true, consul_datacenter: 'dc_a' },
		],
	},
	patroni: {
		leader: 'pg-a1',
		role: 'PRIMARY',
		timeline: '5',
		members: [
			member(now, 'pg-a1', '10.1.0.95', {
				role: 'PRIMARY',
				patroni_state: 'running',
				replication_state: undefined,
				xlog: { location: '83886176' },
			}),
			member(now, 'pg-a2', '10.1.0.96', {}),
			member(now, 'pg-a3', '10.1.0.97', { lag_bytes: '1024' }),
		],
	},
	nomad: {
		leader: '10.1.0.91:4647',
		ready_node_count: 3,
		job_counts: { healthy_count: 4 },
		stages: [
			{
				stage: 1,
				job_counts: { healthy_count: 1 },
				jobs: [
					job({
						job: 'freeswitch',
						stage: 1,
						type: 'SYSTEM',
						allocation_counts: { running: 3 },
						healthy_node_count: 3,
						expected_node_count: 3,
					}),
				],
			},
			{
				stage: 2,
				job_counts: { healthy_count: 1 },
				jobs: [
					job({
						job: 'webitel-engine',
						stage: 2,
						allocation_counts: { running: 2 },
					}),
				],
			},
			{
				stage: 3,
				job_counts: { healthy_count: 2 },
				jobs: [
					job({
						job: 'webitel-core-api',
						stage: 3,
						allocation_counts: { running: 2 },
					}),
					job({
						job: 'webitel-chat',
						stage: 3,
						allocation_counts: { running: 1 },
					}),
				],
			},
		],
		unmanaged_jobs: [{ job: 'node-exporter', nomad_status: 'running' }],
	},
	info_checks: infoChecks('datacenters/dc_a'),
});

/** The swagger example of the standby datacenter, moved to `now`. */
const standbyDatacenter = (now: number): Datacenter => {
	const base = example<Datacenter>('/v1/datacenters/{datacenter}', 'get');
	return {
		...base,
		observer,
		observe_time: ago(now, 3),
		sources: sources(now, 2),
		patroni: {
			...base.patroni,
			lag_bytes: undefined,
			divergence_state: 'NOT_DIVERGED',
			members: base.patroni?.members?.map((m) => ({
				...m,
				lag_bytes: undefined,
				xlog: { ...m.xlog, replay_time: ago(now, 1) },
			})),
		},
		info_checks: infoChecks('datacenters/dc_b'),
	};
};

const NORMAL: Record<string, (now: number) => Datacenter> = {
	dc_a: activeDatacenter,
	dc_b: standbyDatacenter,
};

const alarmActive = (now: number): Datacenter => {
	const base = activeDatacenter(now);
	const drained = { node: 'nomad-a3', reason: 'DRAINING' } as const;
	return {
		...base,
		health: 'DEGRADED',
		health_reasons: [
			'webitel-chat is not healthy',
			'filebeat is missing on 1 of 2 nodes',
		],
		nomad: {
			...base.nomad,
			ready_node_count: 2,
			draining_node_count: 1,
			not_ready_nodes: [drained],
			job_counts: { healthy_count: 3, unhealthy_count: 2 },
			stages: [
				{
					stage: 1,
					job_counts: { healthy_count: 1, unhealthy_count: 1 },
					jobs: [
						// A drained node does not run the job and is not expected to.
						job({
							job: 'freeswitch',
							stage: 1,
							type: 'SYSTEM',
							allocation_counts: { running: 2, complete: 1 },
							healthy_node_count: 2,
							expected_node_count: 2,
							missing_nodes: [drained],
						}),
						job({
							job: 'filebeat',
							stage: 1,
							type: 'SYSTEM',
							state: 'UNHEALTHY',
							allocation_counts: { running: 1, failed: 1 },
							healthy_node_count: 1,
							expected_node_count: 2,
							missing_nodes: [{ node: 'nomad-a2', reason: 'NO_ALLOCATION' }],
						}),
					],
				},
				{
					stage: 2,
					job_counts: { healthy_count: 1 },
					jobs: base.nomad?.stages?.[1]?.jobs,
				},
				{
					stage: 3,
					job_counts: { healthy_count: 1, unhealthy_count: 1 },
					jobs: [
						job({
							job: 'webitel-core-api',
							stage: 3,
							allocation_counts: { running: 2 },
						}),
						job({
							job: 'webitel-chat',
							stage: 3,
							state: 'UNHEALTHY',
							allocation_counts: { running: 1, failed: 1 },
						}),
					],
				},
			],
		},
		info_checks: [
			...(base.info_checks ?? []).filter((c) => c.id !== 'info.services'),
			{
				id: 'info.services',
				result: 'WARN',
				datacenter: 'datacenters/dc_a',
				detail: 'chat: 1 of 2 passing',
				facts: { chat: { passing: 1, total: 2 } },
			},
		],
	};
};

const alarmStandby = (now: number): Datacenter => {
	const base = standbyDatacenter(now);
	return {
		...base,
		state: 'DB_SPLIT',
		health: 'DOWN',
		health_reasons: ['Patroni primary in a standby datacenter'],
		patroni: {
			leader: 'pg-b1',
			role: 'PRIMARY',
			timeline: '6',
			paused: true,
			divergence_state: 'DIVERGED',
			divergence_lsn: '0/5000060',
			divergence_bytes: '16777216',
			upstream_datacenter: 'datacenters/dc_a',
			members: [
				member(now, 'pg-b1', '10.2.0.95', {
					role: 'PRIMARY',
					patroni_state: 'running',
					timeline: '6',
					replication_state: undefined,
					xlog: { location: '100663296' },
				}),
				member(now, 'pg-b2', '10.2.0.96', {
					patroni_state: 'stopped',
					replication_state: 'stopped',
					// The gateway omits false: reachable and usable are absent.
					reachable: undefined,
					usable: undefined,
					error: 'TIMEOUT',
					unusable_reason: 'UNREACHABLE',
					lag_bytes: undefined,
				}),
			],
		},
	};
};

const lost = (dc: Datacenter, now: number): Datacenter => ({
	...dc,
	state: 'UNREACHABLE',
	health: 'UNKNOWN',
	observe_time: ago(now, 3),
	sources: dc.sources?.map((source) => ({
		...source,
		reachable: undefined,
		error: 'TIMEOUT',
		observe_time: ago(now, 420),
		unreachable_start_time: ago(now, 400),
	})),
});

const stale = (dc: Datacenter, now: number): Datacenter => ({
	...dc,
	observe_time: ago(now, 600),
});

const stretchDatacenter = (id: 'dc_a' | 'dc_b', now: number): Datacenter => {
	const isA = id === 'dc_a';
	const subnet = isA ? 1 : 2;
	return {
		name: `datacenters/${id}`,
		state: isA ? 'ACTIVE' : 'STANDBY',
		health: 'OK',
		observer,
		observe_time: ago(now, 3),
		sources: sources(now, subnet),
		patroni: {
			...(isA ? { leader: 'pg-a1', role: 'PRIMARY' as const } : {}),
			timeline: '5',
			lag_bytes: isA ? undefined : '2048',
			members: isA
				? [
						member(now, 'pg-a1', '10.1.0.95', {
							role: 'PRIMARY',
							patroni_state: 'running',
							replication_state: undefined,
							xlog: { location: '83886176' },
						}),
						member(now, 'pg-a2', '10.1.0.96', { role: 'SYNC_STANDBY' }),
					]
				: [
						member(now, 'pg-b1', '10.2.0.95', {
							role: 'QUORUM_STANDBY',
							lag_bytes: '2048',
						}),
						member(now, 'pg-b2', '10.2.0.96', { lag_bytes: '4096' }),
					],
		},
		nomad: {
			leader: `10.${subnet}.0.91:4647`,
			ready_node_count: 3,
			job_counts: isA ? { healthy_count: 1 } : { stopped_count: 1 },
			stages: [
				{
					stage: 3,
					job_counts: isA ? { healthy_count: 1 } : { stopped_count: 1 },
					jobs: [
						job({
							job: 'webitel-core-api',
							stage: 3,
							stop: isA ? undefined : true,
							state: isA ? 'HEALTHY' : 'STOPPED',
							allocation_counts: isA ? { running: 2 } : { complete: 2 },
						}),
					],
				},
			],
		},
		info_checks: infoChecks(`datacenters/${id}`),
	};
};

/**
 * A `GetDatacenter` body for a status variant, with times relative to `now`;
 * `undefined` for a datacenter that is not in the config.
 */
export const buildDatacenter = (
	variant: Exclude<Variant, 'unavailable'>,
	id: string,
	now = Date.now(),
): Datacenter | undefined => {
	if (variant === 'stretch') {
		return id === 'dc_a' || id === 'dc_b'
			? stretchDatacenter(id, now)
			: undefined;
	}
	const build = NORMAL[id];
	if (!build) return undefined;
	switch (variant) {
		case 'alarm':
			return id === 'dc_a' ? alarmActive(now) : alarmStandby(now);
		case 'unreachable':
			return id === 'dc_a' ? lost(build(now), now) : build(now);
		case 'stale':
			return stale(build(now), now);
		default:
			return build(now);
	}
};

const notFound = (id: string) =>
	HttpResponse.json(
		{
			id: 'dcctl.datacenter.not_found',
			code: 404,
			status: 'Not Found',
			detail: `datacenter ${id} not found`,
			metadata: { datacenter: `datacenters/${id}` },
		},
		{ status: 404 },
	);

/** Response of the mocked `GET /v1/datacenters/{dc}` for the selected variant. */
export const currentDatacenterResponse = (id: string) => {
	const variant = getStatusVariant();
	if (variant === 'unavailable') {
		return HttpResponse.json(
			{ id: 'dcctl.unavailable', code: 503, detail: 'mock: API is down' },
			{ status: 503 },
		);
	}
	const datacenter = buildDatacenter(variant, id);
	return datacenter ? HttpResponse.json(datacenter) : notFound(id);
};
