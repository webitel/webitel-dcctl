import { describe, expect, it } from 'vitest';

import type {
	WebitelDcctlV1Datacenter as Datacenter,
	WebitelDcctlV1Operation as Operation,
	WebitelDcctlV1Status as Status,
} from '@/api/gen/_models';

import {
	type ActionContext,
	type ActionKind,
	actionAvailability,
	type UnavailableReason,
} from './actionAvailability';

const status = (extra: Status = {}): Status => ({
	active_datacenter: 'datacenters/dc_a',
	profile: 'WARM_STANDBY',
	datacenters: [
		{ name: 'datacenters/dc_a', state: 'ACTIVE' },
		{ name: 'datacenters/dc_b', state: 'STANDBY' },
	],
	...extra,
});

const standby = (extra: Datacenter = {}): Datacenter => ({
	name: 'datacenters/dc_b',
	state: 'STANDBY',
	...extra,
});

const onB = (extra: ActionContext = {}): ActionContext => ({
	status: status(),
	datacenter: standby(),
	datacenterId: 'dc_b',
	...extra,
});

const running: Operation = { name: 'operations/x', state: 'RUNNING' };

type Expected = 'ok' | 'hidden' | UnavailableReason;

const cases: [string, ActionKind, ActionContext, Expected][] = [
	['switch: standby, idle', 'switch', onB(), 'ok'],
	['switch: no status', 'switch', onB({ status: undefined }), 'statusUnknown'],
	[
		'switch: target is active',
		'switch',
		onB({ datacenterId: 'dc_a', datacenter: { state: 'ACTIVE' } }),
		'alreadyActive',
	],
	[
		'switch: needs recovery',
		'switch',
		onB({ datacenter: standby({ needs_recovery: true }) }),
		'needsRecovery',
	],
	[
		'switch: needs recovery from the status list',
		'switch',
		onB({
			status: status({ needs_recovery_datacenters: ['datacenters/dc_b'] }),
		}),
		'needsRecovery',
	],
	[
		'switch: active datacenter unreachable',
		'switch',
		onB({
			status: status({
				datacenters: [
					{ name: 'datacenters/dc_a', state: 'UNREACHABLE' },
					{ name: 'datacenters/dc_b', state: 'STANDBY' },
				],
			}),
		}),
		'activeUnreachable',
	],
	[
		'switch: operation running',
		'switch',
		onB({ status: status({ operation: running }) }),
		'operationRunning',
	],
	[
		'switch: operation interrupted',
		'switch',
		onB({ status: status({ operation: { state: 'INTERRUPTED' } }) }),
		'operationRunning',
	],
	[
		'switch: a finished operation does not block',
		'switch',
		onB({ status: status({ operation: { state: 'SUCCEEDED' } }) }),
		'ok',
	],
	[
		'failover: active datacenter unreachable is the point of it',
		'failover',
		onB({
			status: status({
				datacenters: [
					{ name: 'datacenters/dc_a', state: 'UNREACHABLE' },
					{ name: 'datacenters/dc_b', state: 'STANDBY' },
				],
			}),
		}),
		'ok',
	],
	[
		'failover: operation running',
		'failover',
		onB({ status: status({ operation: running }) }),
		'operationRunning',
	],
	[
		'failover: needs recovery',
		'failover',
		onB({ datacenter: standby({ state: 'NEEDS_RECOVERY' }) }),
		'needsRecovery',
	],
	[
		'demote: primary leader in a standby datacenter',
		'demote',
		onB({ datacenter: standby({ patroni: { role: 'PRIMARY' } }) }),
		'ok',
	],
	[
		'demote: leader is a standby leader',
		'demote',
		onB({ datacenter: standby({ patroni: { role: 'STANDBY_LEADER' } }) }),
		'roleNotPrimary',
	],
	['demote: role unknown', 'demote', onB(), 'roleNotPrimary'],
	[
		'demote: active datacenter',
		'demote',
		onB({
			datacenterId: 'dc_a',
			datacenter: { state: 'ACTIVE', patroni: { role: 'PRIMARY' } },
		}),
		'alreadyActive',
	],
	[
		'demote: stretch has no demote',
		'demote',
		onB({
			status: status({ profile: 'STRETCH' }),
			datacenter: standby({ patroni: { role: 'PRIMARY' } }),
		}),
		'hidden',
	],
	[
		'demote: operation running',
		'demote',
		onB({
			status: status({ operation: running }),
			datacenter: standby({ patroni: { role: 'PRIMARY' } }),
		}),
		'operationRunning',
	],
	[
		'repoint: operation interrupted',
		'repoint',
		onB({
			status: status({ operation: { state: 'INTERRUPTED' } }),
			datacenter: standby({ patroni: { role: 'STANDBY_LEADER' } }),
		}),
		'operationRunning',
	],
	[
		'markRecovered: operation running',
		'markRecovered',
		onB({
			status: status({ operation: running }),
			datacenter: standby({ needs_recovery: true }),
		}),
		'operationRunning',
	],
	[
		'repoint: standby leader',
		'repoint',
		onB({ datacenter: standby({ patroni: { role: 'STANDBY_LEADER' } }) }),
		'ok',
	],
	[
		'repoint: primary',
		'repoint',
		onB({ datacenter: standby({ patroni: { role: 'PRIMARY' } }) }),
		'roleNotStandbyLeader',
	],
	[
		'repoint: stretch has no repoint',
		'repoint',
		onB({ status: status({ profile: 'STRETCH' }) }),
		'hidden',
	],
	[
		'markRecovered: needs recovery',
		'markRecovered',
		onB({ datacenter: standby({ needs_recovery: true }) }),
		'ok',
	],
	['markRecovered: healthy', 'markRecovered', onB(), 'notNeedsRecovery'],
	[
		'markRecovered: stretch',
		'markRecovered',
		onB({
			status: status({ profile: 'STRETCH' }),
			datacenter: standby({ needs_recovery: true }),
		}),
		'hidden',
	],
	[
		'syncApps: active, idle',
		'syncApps',
		onB({ datacenterId: 'dc_a', datacenter: { state: 'ACTIVE' } }),
		'ok',
	],
	['syncApps: standby', 'syncApps', onB(), 'notActive'],
	[
		'syncApps: operation running',
		'syncApps',
		onB({
			datacenterId: 'dc_a',
			datacenter: { state: 'ACTIVE' },
			status: status({ operation: running }),
		}),
		'operationRunning',
	],
	[
		'resume: interrupted',
		'resume',
		{ operation: { state: 'INTERRUPTED' } },
		'ok',
	],
	[
		'resume: running',
		'resume',
		{ operation: { state: 'RUNNING' } },
		'notInterrupted',
	],
	['resume: finished', 'resume', { operation: { state: 'FAILED' } }, 'hidden'],
	[
		'abandon: interrupted',
		'abandon',
		{ operation: { state: 'INTERRUPTED' } },
		'ok',
	],
	[
		'abandon: running',
		'abandon',
		{ operation: { state: 'RUNNING' } },
		'notInterrupted',
	],
	[
		'abandon: abandoned already',
		'abandon',
		{ operation: { state: 'ABANDONED' } },
		'hidden',
	],
	[
		'resolve: conflict alarm',
		'resolve',
		{ status: status({ alarms: [{ type: 'CONFLICT' }] }) },
		'ok',
	],
	['resolve: no conflict', 'resolve', { status: status() }, 'hidden'],
	[
		'initArbiter: arbiter uninitialized',
		'initArbiter',
		{ status: status({ alarms: [{ type: 'ARBITER_UNINITIALIZED' }] }) },
		'ok',
	],
	['initArbiter: initialized', 'initArbiter', { status: status() }, 'hidden'],
];

describe('actionAvailability', () => {
	it.each(cases)('%s', (_name, kind, ctx, expected) => {
		const result = actionAvailability(kind, ctx);
		if (expected === 'hidden') {
			expect(result).toEqual({ visible: false });
		} else if (expected === 'ok') {
			expect(result).toEqual({ visible: true });
		} else {
			expect(result).toEqual({ visible: true, reason: expected });
		}
	});
});
