import { flushPromises } from '@vue/test-utils';
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest';

import type {
	WebitelDcctlV1Datacenter as Datacenter,
	WebitelDcctlV1Status as Status,
} from '@/api/gen/_models';
import { setActionVariant } from '@/mocks/actionVariants';
import { MOCK_USER } from '@/mocks/handlers';
import { server } from '@/mocks/server';
import { buildStatus } from '@/mocks/statusVariants';
import { useStatusStore } from '@/stores/status';
import { mountWithApp } from '@/tests/mountWithApp';
import { waitForLazy } from '@/tests/waitForLazy';
import { encodeBasicToken } from '@/utils/basicAuth';
import DatacenterActions from './DatacenterActions.vue';

beforeAll(() => server.listen({ onUnhandledFrame: 'error' }));
afterEach(() => {
	server.resetHandlers();
	setActionVariant('ok');
});
afterAll(() => server.close());

const TOKEN = encodeBasicToken(MOCK_USER.name, MOCK_USER.password);

const standby = (extra: Datacenter = {}): Datacenter => ({
	name: 'datacenters/dc_b',
	state: 'STANDBY',
	...extra,
});

const mountActions = async (
	datacenter: Datacenter,
	id: string,
	status: Status | null = buildStatus('normal'),
) => {
	const mounted = await mountWithApp(
		DatacenterActions,
		{ datacenter, datacenterId: id },
		TOKEN,
	);
	useStatusStore(mounted.pinia).status = status ?? undefined;
	await flushPromises();
	return mounted;
};

type Mounted = Awaited<ReturnType<typeof mountActions>>;

const state = (mounted: Mounted, kind: string) => {
	const wrapper = mounted.wrapper.find(`[data-test="action-${kind}"]`);
	if (!wrapper.exists()) return 'absent';
	return wrapper.attributes('data-reason') ?? 'on';
};

const visible = (mounted: Mounted) =>
	(
		[
			'switch',
			'failover',
			'demote',
			'markRecovered',
			'repoint',
			'syncApps',
		] as const
	).map((kind) => [kind, state(mounted, kind)]);

describe('buttons on the page of a standby datacenter', () => {
	it('offers switch and failover; the rest is off with its reason', async () => {
		const mounted = await mountActions(standby(), 'dc_b');
		await mounted.wrapper.find('[data-test="action-more"]').trigger('click');

		expect(Object.fromEntries(visible(mounted))).toEqual({
			switch: 'on',
			failover: 'on',
			demote: 'roleNotPrimary',
			markRecovered: 'notNeedsRecovery',
			repoint: 'roleNotStandbyLeader',
			syncApps: 'notActive',
		});
	});

	it('offers demote for a primary leader and repoint for a standby leader', async () => {
		const primary = await mountActions(
			standby({ patroni: { role: 'PRIMARY' } }),
			'dc_b',
		);
		expect(state(primary, 'demote')).toBe('on');

		const follower = await mountActions(
			standby({ patroni: { role: 'STANDBY_LEADER' } }),
			'dc_b',
		);
		await follower.wrapper.find('[data-test="action-more"]').trigger('click');
		expect(state(follower, 'repoint')).toBe('on');
	});

	it('offers mark recovered for a datacenter that needs it, and not switch', async () => {
		const mounted = await mountActions(
			standby({ needs_recovery: true }),
			'dc_b',
		);

		expect(state(mounted, 'markRecovered')).toBe('on');
		expect(state(mounted, 'switch')).toBe('needsRecovery');
		expect(state(mounted, 'failover')).toBe('needsRecovery');
	});

	it('turns switch and failover off while an operation runs', async () => {
		const mounted = await mountActions(
			standby(),
			'dc_b',
			buildStatus('operation'),
		);

		expect(state(mounted, 'switch')).toBe('operationRunning');
		expect(state(mounted, 'failover')).toBe('operationRunning');
	});

	it('shows the buttons off, not gone, before the status arrives', async () => {
		const mounted = await mountActions(standby(), 'dc_b', null);

		expect(state(mounted, 'switch')).toBe('statusUnknown');
	});
});

describe('buttons on the page of the active datacenter', () => {
	it('offers apps sync and nothing that targets itself', async () => {
		const mounted = await mountActions(
			{ name: 'datacenters/dc_a', state: 'ACTIVE' },
			'dc_a',
		);
		await mounted.wrapper.find('[data-test="action-more"]').trigger('click');

		expect(state(mounted, 'syncApps')).toBe('on');
		expect(state(mounted, 'switch')).toBe('alreadyActive');
		expect(state(mounted, 'demote')).toBe('alreadyActive');
	});
});

describe('stretch profile', () => {
	it('hides demote, repoint and mark recovered', async () => {
		const mounted = await mountActions(
			standby({ needs_recovery: true, patroni: { role: 'PRIMARY' } }),
			'dc_b',
			buildStatus('stretch'),
		);
		await mounted.wrapper.find('[data-test="action-more"]').trigger('click');

		expect(state(mounted, 'switch')).not.toBe('absent');
		expect(state(mounted, 'failover')).not.toBe('absent');
		expect(state(mounted, 'demote')).toBe('absent');
		expect(state(mounted, 'repoint')).toBe('absent');
		expect(state(mounted, 'markRecovered')).toBe('absent');
	});
});

describe('opening the dialog', () => {
	it('opens it for the datacenter of the page and not for an unavailable button', async () => {
		const mounted = await mountActions(standby(), 'dc_b');

		await mounted.wrapper
			.find('[data-test="action-demote"] button')
			.trigger('click');
		expect(mounted.wrapper.find('[data-test="action-dialog"]').exists()).toBe(
			false,
		);

		await mounted.wrapper
			.find('[data-test="action-switch"] button')
			.trigger('click');
		await waitForLazy(() =>
			expect(
				mounted.wrapper.find('[data-test="action-outcome-ok"]').exists(),
			).toBe(true),
		);
		expect(mounted.wrapper.find('[data-test="action-title"]').text()).toBe(
			'Switch to dc_b',
		);
	});
});
