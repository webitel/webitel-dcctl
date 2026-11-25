import { HttpResponse } from 'msw';

import type {
	WebitelDcctlV1Datacenter as Datacenter,
	WebitelDcctlV1Operation as Operation,
	WebitelDcctlV1Source as Source,
	WebitelDcctlV1Status as Status,
} from '@/api/gen/_models';
import { example } from './example';

export const STATUS_VARIANTS = [
	'normal',
	'alarm',
	'operation',
	'unreachable',
	'stale',
	'unavailable',
	'stretch',
] as const;

export type StatusVariant = (typeof STATUS_VARIANTS)[number];

const iso = (ms: number) => new Date(ms).toISOString();

const ago = (now: number, seconds: number) => iso(now - seconds * 1000);

const withSources = (dc: Datacenter, now: number): Datacenter => ({
	...dc,
	sources: dc.sources?.map((source) => ({
		...source,
		observe_time: ago(now, 3),
	})),
});

/** The swagger example, moved to `now` and without its arbiter alarm. */
const normal = (now: number): Status => {
	const base = example<Status>('/v1/status', 'get');
	return {
		...base,
		observe_time: ago(now, 3),
		alarms: [],
		voters: base.voters?.map((voter) => ({
			...voter,
			reachable: true,
			error: undefined,
		})),
		datacenters: base.datacenters?.map((dc) => withSources(dc, now)),
	};
};

const markDatacenter = (
	status: Status,
	name: string,
	patch: Partial<Datacenter>,
): Datacenter[] =>
	(status.datacenters ?? []).map((dc) =>
		dc.name === name ? { ...dc, ...patch } : dc,
	);

const alarm = (now: number): Status => {
	const base = normal(now);
	return {
		...base,
		alarms: [
			{
				type: 'APPS_DEGRADED',
				severity: 'WARNING',
				datacenter: 'datacenters/dc_a',
				start_time: ago(now, 600),
				detail: '2 of 12 managed jobs are unhealthy',
			},
			{
				type: 'DB_SPLIT',
				severity: 'CRITICAL',
				datacenter: 'datacenters/dc_b',
				start_time: ago(now, 240),
				detail: 'dc_b has a Patroni primary while dc_a is active',
			},
			{
				type: 'VOTER_UNREACHABLE',
				severity: 'WARNING',
				voter_type: 'ARBITER',
				start_time: ago(now, 1200),
				detail: 'arbiter does not answer',
			},
		],
		datacenters: markDatacenter(base, 'datacenters/dc_b', {
			state: 'DB_SPLIT',
			health: 'DOWN',
			health_reasons: ['Patroni primary in a standby datacenter'],
			patroni: { leader: 'pg-b1', role: 'PRIMARY', timeline: '6' },
		}).map((dc) =>
			dc.name === 'datacenters/dc_a'
				? {
						...dc,
						health: 'DEGRADED' as const,
						nomad: {
							...dc.nomad,
							job_counts: { healthy_count: 10, unhealthy_count: 2 },
						},
					}
				: dc,
		),
	};
};

const operation = (now: number): Status => {
	const op = example<Operation>('/v1/operations/{operation}', 'get');
	return {
		...normal(now),
		operation: {
			...op,
			create_time: ago(now, 40),
			update_time: ago(now, 4),
		},
	};
};

const unreachable = (now: number): Status => {
	const base = normal(now);
	const lost = (source: Source): Source => ({
		...source,
		reachable: false,
		error: 'TIMEOUT',
		observe_time: ago(now, 420),
		unreachable_start_time: ago(now, 400),
	});
	return {
		...base,
		alarms: [
			{
				type: 'DATACENTER_UNREACHABLE',
				severity: 'CRITICAL',
				datacenter: 'datacenters/dc_a',
				start_time: ago(now, 400),
				detail: 'no source of dc_a answers',
			},
			{
				type: 'STALE',
				severity: 'WARNING',
				datacenter: 'datacenters/dc_b',
				start_time: ago(now, 60),
				detail: 'dc_b still points at dc_a as active',
			},
		],
		datacenters: markDatacenter(base, 'datacenters/dc_a', {
			state: 'UNREACHABLE',
			health: 'UNKNOWN',
			sources: base.datacenters
				?.find((dc) => dc.name === 'datacenters/dc_a')
				?.sources?.map(lost),
		}),
	};
};

const stale = (now: number): Status => ({
	...normal(now),
	observe_time: ago(now, 600),
});

/** One Patroni cluster across both datacenters: only the active one has the leader. */
const stretch = (now: number): Status => {
	const base = normal(now);
	return {
		...base,
		profile: 'STRETCH',
		datacenters: markDatacenter(base, 'datacenters/dc_b', {
			patroni: { timeline: '5', lag_bytes: '2048' },
		}),
	};
};

const BUILDERS: Record<
	Exclude<StatusVariant, 'unavailable'>,
	(now: number) => Status
> = { normal, alarm, operation, unreachable, stale, stretch };

/** A `GetStatus` body for a variant, with times relative to `now`. */
export const buildStatus = (
	variant: Exclude<StatusVariant, 'unavailable'>,
	now = Date.now(),
): Status => BUILDERS[variant](now);

let selected: StatusVariant = 'normal';

export const getStatusVariant = (): StatusVariant => selected;

export const setStatusVariant = (variant: StatusVariant) => {
	selected = variant;
};

export const isStatusVariant = (value: unknown): value is StatusVariant =>
	STATUS_VARIANTS.includes(value as StatusVariant);

/** Picks the variant from `?mock=<name>` of the page URL; unknown names are ignored. */
export const selectVariantFromUrl = (search: string) => {
	const value = new URLSearchParams(search).get('mock');
	if (isStatusVariant(value)) selected = value;
};

/** Response of the mocked `GET /v1/status` for the selected variant. */
export const currentStatusResponse = () => {
	if (selected === 'unavailable') {
		return HttpResponse.json(
			{ id: 'dcctl.unavailable', code: 503, detail: 'mock: API is down' },
			{ status: 503 },
		);
	}
	return HttpResponse.json(buildStatus(selected));
};
