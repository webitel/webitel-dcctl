import { flushPromises } from '@vue/test-utils';
import { AxiosError } from 'axios';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { recordServerDate, resetServerClock } from '@/api/client/serverClock';
import type {
	WebitelDcctlV1Datacenter as Datacenter,
	WebitelDcctlV1Status as Status,
} from '@/api/gen/_models';
import { buildDatacenter } from '@/mocks/datacenterVariants';
import { useStatusStore } from '@/stores/status';
import { mountWithApp } from '@/tests/mountWithApp';
import DatacenterView from './DatacenterView.vue';

const getDatacenter = vi.hoisted(() => vi.fn());
vi.mock('@/api/gen/datacenter-service/datacenter-service', () => ({
	getDatacenterService: () => ({ getDatacenter }),
}));

const apiError = (status: number, id?: string) =>
	new AxiosError('failed', 'ERR_BAD_REQUEST', undefined, null, {
		status,
		data: id ? { id, code: status, detail: `mock ${id}` } : {},
		statusText: '',
		headers: {},
		config: {} as never,
	});

const build = (...args: Parameters<typeof buildDatacenter>): Datacenter => {
	const dc = buildDatacenter(...args);
	if (!dc) throw new Error('no datacenter');
	return dc;
};

type Mounted = Awaited<ReturnType<typeof mountWithApp>>;

const mountPage = async (path: string): Promise<Mounted> => {
	const mounted = await mountWithApp(DatacenterView, {}, undefined, path);
	await flushPromises();
	return mounted;
};

/**
 * The SDK loads its components lazily: wait until the header chips are
 * rendered and the tab has its table or says that it has nothing to show.
 */
const ready = async (mounted: Mounted) => {
	await vi.waitFor(() =>
		expect(mounted.wrapper.find('[data-test="dc-state"]').exists()).toBe(true),
	);
	await vi.waitFor(() =>
		expect(
			mounted.wrapper.find('.wt-table, [data-test$="-empty"]').exists(),
		).toBe(true),
	);
};

const mountWith = async (dc: Datacenter, path = '/datacenters/dc_a') => {
	getDatacenter.mockResolvedValue(dc);
	const mounted = await mountPage(path);
	await ready(mounted);
	return mounted;
};

const setStatus = (mounted: Mounted, status: Status) => {
	useStatusStore(mounted.pinia).status = status;
};

const text = (mounted: Mounted, selector: string) =>
	mounted.wrapper.find(selector).text();

beforeEach(() => {
	getDatacenter.mockReset();
	Element.prototype.scrollIntoView = vi.fn();
});

afterEach(() => {
	resetServerClock();
	vi.restoreAllMocks();
});

describe('without data', () => {
	it('shows loading, not an empty page, while the first request runs', async () => {
		getDatacenter.mockReturnValue(new Promise(() => {}));
		const { wrapper } = await mountPage('/datacenters/dc_a');

		expect(wrapper.find('[data-test="dc-loading"]').exists()).toBe(true);
		expect(wrapper.text()).not.toContain('No Patroni data');
		expect(wrapper.find('[data-test="dc-state"]').exists()).toBe(false);
	});

	it('shows why there is no data when the API is down', async () => {
		getDatacenter.mockRejectedValue(apiError(503, 'dcctl.unavailable'));
		const { wrapper } = await mountPage('/datacenters/dc_a');

		const alert = wrapper.find('[data-test="dc-unavailable"]');
		expect(alert.text()).toContain('Cannot get the datacenter');
		expect(alert.text()).toContain('mock dcctl.unavailable');
		expect(wrapper.find('[data-test="tab-patroni"]').exists()).toBe(false);
	});

	it('says that a datacenter is not in the config and links back', async () => {
		getDatacenter.mockRejectedValue(
			apiError(404, 'dcctl.datacenter.not_found'),
		);
		const { wrapper, router } = await mountPage('/datacenters/dc_x');

		expect(wrapper.find('[data-test="dc-not-found"]').text()).toContain(
			'Datacenter dc_x is not in the config',
		);
		expect(wrapper.find('[data-test="dc-back"]').attributes('href')).toBe(
			router.resolve({ name: 'overview', hash: '#datacenters' }).href,
		);
	});

	it('does not call any other 404 a missing datacenter', async () => {
		getDatacenter.mockRejectedValue(apiError(404));
		const { wrapper } = await mountPage('/datacenters/dc_a');

		expect(wrapper.find('[data-test="dc-not-found"]').exists()).toBe(false);
		expect(wrapper.find('[data-test="dc-unavailable"]').exists()).toBe(true);
	});
});

describe('header', () => {
	it('shows state, health and the reasons with their colours', async () => {
		const mounted = await mountWith(build('alarm', 'dc_a'));

		expect(text(mounted, '[data-test="dc-title"]')).toBe('dc_a');
		expect(text(mounted, '[data-test="dc-health"]')).toBe('Degraded');
		expect(mounted.wrapper.find('[data-test="dc-health"]').classes()).toContain(
			'p-chip-warning',
		);
		const reasons = text(mounted, '[data-test="health-reasons"]');
		expect(reasons).toContain('webitel-chat is not healthy');
		expect(reasons).toContain('filebeat is missing on 1 of 2 nodes');
	});

	it('paints a split datacenter red', async () => {
		const mounted = await mountWith(
			build('alarm', 'dc_b'),
			'/datacenters/dc_b',
		);

		expect(text(mounted, '[data-test="dc-state"]')).toBe('Database split');
		expect(mounted.wrapper.find('[data-test="dc-state"]').classes()).toContain(
			'p-chip-error',
		);
		expect(mounted.wrapper.find('[data-test="dc-health"]').classes()).toContain(
			'p-chip-error',
		);
	});

	it('shows unknown enum values as they are', async () => {
		const dc = {
			...build('normal', 'dc_a'),
			state: 'FUTURE_STATE',
			health: 'NEW_HEALTH',
		} as unknown as Datacenter;
		const mounted = await mountWith(dc);

		expect(text(mounted, '[data-test="dc-state"]')).toBe('FUTURE_STATE');
		expect(text(mounted, '[data-test="dc-health"]')).toBe('NEW_HEALTH');
	});

	it('keeps an unreachable active datacenter red once the status says it is active', async () => {
		getDatacenter.mockResolvedValue(build('unreachable', 'dc_a'));
		const mounted = await mountPage('/datacenters/dc_a');
		useStatusStore(mounted.pinia).status = {
			active_datacenter: 'datacenters/dc_a',
		};
		await ready(mounted);
		await flushPromises();

		expect(mounted.wrapper.find('[data-test="dc-state"]').classes()).toContain(
			'p-chip-error',
		);
	});

	it('does not show an active chip next to the ACTIVE state', async () => {
		const mounted = await mountWith(build('normal', 'dc_a'));
		setStatus(mounted, { active_datacenter: 'datacenters/dc_a' });
		await flushPromises();

		expect(text(mounted, '[data-test="dc-state"]')).toBe('Active');
		expect(mounted.wrapper.find('[data-test="dc-active"]').exists()).toBe(
			false,
		);
	});

	it('shows an active chip when the active datacenter is in another state', async () => {
		const mounted = await mountWith(build('unreachable', 'dc_a'));
		setStatus(mounted, { active_datacenter: 'datacenters/dc_a' });
		await flushPromises();

		expect(mounted.wrapper.find('[data-test="dc-active"]').exists()).toBe(true);
	});

	it('does not mark the datacenter active when the status is not there', async () => {
		const mounted = await mountWith(build('unreachable', 'dc_a'));

		expect(
			mounted.wrapper.find('[data-test="dc-state"]').classes(),
		).not.toContain('p-chip-error');
	});
});

describe('tabs in the URL', () => {
	it('opens Patroni by default', async () => {
		const mounted = await mountWith(build('normal', 'dc_a'));

		expect(mounted.wrapper.find('[data-test="tab-patroni"]').exists()).toBe(
			true,
		);
		expect(mounted.wrapper.find('[data-test="tab-apps"]').exists()).toBe(false);
	});

	it.each([
		['apps', 'tab-apps'],
		['info', 'tab-info'],
		['sources', 'tab-sources'],
		['patroni', 'tab-patroni'],
		['bogus', 'tab-patroni'],
	])('?tab=%s opens %s', async (tab, expected) => {
		const mounted = await mountWith(
			build('normal', 'dc_a'),
			`/datacenters/dc_a?tab=${tab}`,
		);

		expect(mounted.wrapper.find(`[data-test="${expected}"]`).exists()).toBe(
			true,
		);
	});

	it('puts the selected tab into the URL', async () => {
		const mounted = await mountWith(build('normal', 'dc_a'));

		const tabs = mounted.wrapper.findAll('.wt-tab');
		await tabs[3].trigger('click');
		await flushPromises();

		expect(mounted.router.currentRoute.value.query.tab).toBe('sources');
		expect(mounted.wrapper.find('[data-test="tab-sources"]').exists()).toBe(
			true,
		);
	});

	it('follows the URL when it changes', async () => {
		const mounted = await mountWith(build('normal', 'dc_a'));

		await mounted.router.push('/datacenters/dc_a?tab=info');
		await flushPromises();

		expect(mounted.wrapper.find('[data-test="tab-info"]').exists()).toBe(true);
	});
});

describe('Patroni tab', () => {
	it('lists members with role, state, timeline, lag and replication, and marks the leader', async () => {
		const mounted = await mountWith(build('normal', 'dc_a'));

		const leader = mounted.wrapper.find('.member-row--pg-a1');
		expect(leader.find('[data-test="leader-mark"]').exists()).toBe(true);
		expect(leader.text()).toContain('primary');
		expect(leader.text()).toContain('running');
		const replica = mounted.wrapper.find('.member-row--pg-a3');
		expect(replica.find('[data-test="leader-mark"]').exists()).toBe(false);
		expect(replica.text()).toContain('replica');
		expect(replica.text()).toContain('streaming');
		expect(replica.find('[data-test="member-lag"]').text()).toBe('1 kB');
		expect(replica.text()).toContain('5');
	});

	it('shows a lag the gateway omitted as 0, not as no data', async () => {
		const mounted = await mountWith(
			build('normal', 'dc_b'),
			'/datacenters/dc_b',
		);

		expect(
			mounted.wrapper
				.find('.member-row--pg-b1 [data-test="member-lag"]')
				.text(),
		).toBe('0 byte');
		expect(text(mounted, '[data-test="patroni-lag"]')).toBe('0 byte');
	});

	it('shows no lag for the active datacenter: it has no meaning there', async () => {
		const mounted = await mountWith(build('normal', 'dc_a'));
		setStatus(mounted, { active_datacenter: 'datacenters/dc_a' });
		await flushPromises();

		expect(text(mounted, '[data-test="patroni-lag"]')).toBe('—');
	});

	it('shows pause, the upstream datacenter and the standby leader', async () => {
		const mounted = await mountWith(
			build('normal', 'dc_b'),
			'/datacenters/dc_b',
		);

		expect(text(mounted, '[data-test="patroni-pause"]')).toBe('not paused');
		expect(text(mounted, '[data-test="patroni-upstream"]')).toContain('dc_a');
		expect(text(mounted, '[data-test="patroni-leader"]')).toContain(
			'standby leader',
		);
	});

	describe('divergence', () => {
		const withState = (state: string | undefined, extra = {}) => {
			const dc = build('normal', 'dc_b');
			return {
				...dc,
				patroni: { ...dc.patroni, divergence_state: state, ...extra },
			} as unknown as Datacenter;
		};

		it('shows a diverged datacenter in red with the point and the size, and a paused cluster', async () => {
			const mounted = await mountWith(
				build('alarm', 'dc_b'),
				'/datacenters/dc_b',
			);

			const cell = mounted.wrapper.find('[data-test="patroni-divergence"]');
			expect(cell.text()).toContain(
				'diverged at 0/5000060, 16 MB written after',
			);
			expect(cell.find('.p-chip').classes()).toContain('p-chip-error');
			expect(text(mounted, '[data-test="patroni-pause"]')).toBe('paused');
		});

		it('says that it may have diverged, in yellow, when the state is unknown, even without an LSN', async () => {
			const mounted = await mountWith(withState('UNKNOWN'));

			const cell = mounted.wrapper.find('[data-test="patroni-divergence"]');
			expect(cell.text()).toBe('may have diverged');
			expect(cell.find('.p-chip').classes()).toContain('p-chip-warning');
		});

		it('says it did not diverge only when the state says so', async () => {
			const mounted = await mountWith(withState('NOT_DIVERGED'));

			expect(text(mounted, '[data-test="patroni-divergence"]')).toBe(
				'not diverged',
			);
		});

		it.each([['DIVERGENCE_STATE_UNSPECIFIED'], [undefined]])(
			'shows a dash for %s (the active datacenter, stretch)',
			async (state) => {
				const mounted = await mountWith(withState(state));

				expect(text(mounted, '[data-test="patroni-divergence"]')).toBe('—');
			},
		);

		it('shows a state it does not know as it is', async () => {
			const mounted = await mountWith(withState('FUTURE_STATE'));

			expect(text(mounted, '[data-test="patroni-divergence"]')).toBe(
				'FUTURE_STATE',
			);
		});
	});

	it('shows a member that is not reachable, with the reason, though the gateway omitted the flag', async () => {
		const mounted = await mountWith(
			build('alarm', 'dc_b'),
			'/datacenters/dc_b',
		);

		const lost = mounted.wrapper.find('.member-row--pg-b2');
		expect(lost.find('[data-test="member-state"]').text()).toContain(
			'not answering (timeout)',
		);
		expect(lost.find('[data-test="member-state"]').classes()).toContain(
			'p-chip-error',
		);
		expect(lost.find('[data-test="member-usable"]').text()).toContain(
			'REST API does not answer',
		);
	});

	it('says in stretch that a part of the cluster without a leader has it in another datacenter', async () => {
		const mounted = await mountWith(
			build('stretch', 'dc_b'),
			'/datacenters/dc_b',
		);
		setStatus(mounted, { profile: 'STRETCH' });
		await flushPromises();

		expect(text(mounted, '[data-test="patroni-leader"]')).toBe(
			'leader is in another datacenter',
		);
		expect(mounted.wrapper.find('[data-test="leader-mark"]').exists()).toBe(
			false,
		);
		expect(mounted.wrapper.find('.member-row--pg-b1').text()).toContain(
			'quorum standby',
		);
	});

	it('shows unknown roles as they are', async () => {
		const dc = build('normal', 'dc_a');
		const patroni = {
			...dc.patroni,
			members: dc.patroni?.members?.map((m) => ({
				...m,
				role: 'MYSTERY',
			})),
		};
		const mounted = await mountWith({
			...dc,
			patroni,
		} as unknown as Datacenter);

		expect(text(mounted, '.member-row--pg-a2')).toContain('MYSTERY');
	});

	it('says there is no Patroni data instead of an empty member list', async () => {
		const mounted = await mountWith({
			...build('normal', 'dc_a'),
			patroni: undefined,
		});

		expect(text(mounted, '[data-test="patroni-empty"]')).toBe(
			'No Patroni data',
		);
		expect(mounted.wrapper.find('[data-test="members"]').exists()).toBe(false);
	});

	it('marks the values as the last known ones when Patroni does not answer', async () => {
		const mounted = await mountWith(build('unreachable', 'dc_a'));

		const notice = text(mounted, '[data-test="last-known"]');
		expect(notice).toContain('Not answering');
		expect(notice).toContain('min ago');
		expect(mounted.wrapper.find('.member-row--pg-a1').exists()).toBe(true);
	});

	it('shows no notice while Patroni answers', async () => {
		const mounted = await mountWith(build('normal', 'dc_a'));

		expect(mounted.wrapper.find('[data-test="last-known"]').exists()).toBe(
			false,
		);
	});
});

describe('Apps tab', () => {
	const open = (dc: Datacenter) => mountWith(dc, '/datacenters/dc_a?tab=apps');
	const job = (mounted: Mounted, name: string) =>
		mounted.wrapper.find(`.job-row--${name}`);

	it('lists stages in order with their jobs, state and allocations', async () => {
		const mounted = await open(build('normal', 'dc_a'));

		const stages = mounted.wrapper
			.findAll('.wt-expansion-panel')
			.map((panel) => panel.attributes('data-test'));
		expect(stages).toEqual(['stage-1', 'stage-2', 'stage-3']);
		const chat = job(mounted, 'webitel-chat');
		expect(chat.find('[data-test="job-state"]').text()).toBe('healthy');
		expect(chat.find('[data-test="job-allocations"]').text()).toBe('running 1');
		const stage3 = mounted.wrapper.find('[data-test="stage-3"]');
		expect(stage3.find('.job-row--webitel-core-api').exists()).toBe(true);
		expect(stage3.find('.job-row--webitel-chat').exists()).toBe(true);
		expect(
			mounted.wrapper
				.find('[data-test="stage-1"] .job-row--webitel-chat')
				.exists(),
		).toBe(false);
	});

	it('shows a system job with a drained node as healthy, 2 of 2, with a warning and the node', async () => {
		const mounted = await open(build('alarm', 'dc_a'));
		setStatus(mounted, { active_datacenter: 'datacenters/dc_a' });
		await flushPromises();

		const system = job(mounted, 'freeswitch');
		expect(system.find('[data-test="job-state"]').text()).toBe('healthy');
		expect(system.find('[data-test="job-state"]').classes()).toContain(
			'p-chip-success',
		);
		const coverage = system.find('[data-test="job-coverage"]');
		expect(coverage.text()).toBe('2 of 2');
		expect(coverage.classes()).toContain('p-chip-warning');
		expect(system.find('[data-test="missing-nodes"]').text()).toContain(
			'nomad-a3 (draining)',
		);
	});

	it('shows a system job that misses a node it should run on in red', async () => {
		const mounted = await open(build('alarm', 'dc_a'));

		const system = job(mounted, 'filebeat');
		expect(system.find('[data-test="job-state"]').text()).toBe('unhealthy');
		const coverage = system.find('[data-test="job-coverage"]');
		expect(coverage.text()).toBe('1 of 2');
		expect(coverage.classes()).toContain('p-chip-error');
		expect(system.find('[data-test="missing-nodes"]').text()).toContain(
			'nomad-a2 (no allocation)',
		);
	});

	it('shows a system job that runs everywhere in green', async () => {
		const mounted = await open(build('normal', 'dc_a'));

		const coverage = job(mounted, 'freeswitch').find(
			'[data-test="job-coverage"]',
		);
		expect(coverage.text()).toBe('3 of 3');
		expect(coverage.classes()).toContain('p-chip-success');
	});

	it('colours the jobs of the active datacenter by what they should do', async () => {
		const mounted = await open(build('alarm', 'dc_a'));
		setStatus(mounted, { active_datacenter: 'datacenters/dc_a' });
		await flushPromises();

		expect(
			job(mounted, 'webitel-engine').find('[data-test="job-state"]').classes(),
		).toContain('p-chip-success');
		expect(
			job(mounted, 'webitel-chat').find('[data-test="job-state"]').classes(),
		).toContain('p-chip-error');
	});

	it('shows stopped jobs of the active datacenter in red', async () => {
		const dc = build('normal', 'dc_a');
		const stages = dc.nomad?.stages?.map((stage) => ({
			...stage,
			jobs: stage.jobs?.map((j) => ({ ...j, state: 'STOPPED' })),
		}));
		const mounted = await open({
			...dc,
			nomad: { ...dc.nomad, stages },
		} as unknown as Datacenter);
		setStatus(mounted, { active_datacenter: 'datacenters/dc_a' });
		await flushPromises();

		expect(
			job(mounted, 'webitel-engine').find('[data-test="job-state"]').classes(),
		).toContain('p-chip-error');
	});

	it('shows stopped jobs of an inactive datacenter in grey and a running one in red', async () => {
		const mounted = await mountWith(
			build('normal', 'dc_b'),
			'/datacenters/dc_b?tab=apps',
		);
		setStatus(mounted, { active_datacenter: 'datacenters/dc_a' });
		await flushPromises();

		const stopped = job(mounted, 'freeswitch');
		expect(stopped.find('[data-test="job-state"]').classes()).toContain(
			'p-chip-secondary',
		);
		expect(stopped.text()).toContain('stop requested');

		const dc = build('normal', 'dc_b');
		const running = await mountWith(
			{
				...dc,
				nomad: {
					...dc.nomad,
					stages: dc.nomad?.stages?.map((stage) => ({
						...stage,
						jobs: stage.jobs?.map((j) => ({ ...j, state: 'HEALTHY' })),
					})),
				},
			} as unknown as Datacenter,
			'/datacenters/dc_b?tab=apps',
		);
		setStatus(running, { active_datacenter: 'datacenters/dc_a' });
		await flushPromises();
		expect(
			running.wrapper
				.find('.job-row--freeswitch [data-test="job-state"]')
				.classes(),
		).toContain('p-chip-error');
	});

	it('uses neutral job colours while the role of the datacenter is unknown', async () => {
		const mounted = await mountWith(
			build('normal', 'dc_b'),
			'/datacenters/dc_b?tab=apps',
		);

		expect(
			job(mounted, 'freeswitch').find('[data-test="job-state"]').classes(),
		).toContain('p-chip-secondary');
	});

	it('shows failed allocations first', async () => {
		const mounted = await open(build('alarm', 'dc_a'));

		expect(
			job(mounted, 'webitel-chat').find('[data-test="job-allocations"]').text(),
		).toBe('failed 1, running 1');
	});

	it('shows no nodes line for a job that is not a system job', async () => {
		const mounted = await open(build('normal', 'dc_a'));

		expect(
			job(mounted, 'webitel-engine').find('[data-test="job-nodes"]').text(),
		).toBe('—');
	});

	it('shows node counts the gateway omitted as 0, not as no data', async () => {
		const mounted = await open(build('normal', 'dc_a'));

		expect(text(mounted, '[data-test="nodes-ready"]')).toBe('3');
		expect(text(mounted, '[data-test="nodes-draining"]')).toBe('0');
		expect(text(mounted, '[data-test="nodes-down"]')).toBe('0');
		expect(text(mounted, '[data-test="nodes-ineligible"]')).toBe('0');
		expect(text(mounted, '[data-test="nodes"]')).not.toContain('no data');
	});

	it('counts nodes and lists the ones that are not ready', async () => {
		const mounted = await open(build('alarm', 'dc_a'));

		expect(text(mounted, '[data-test="nodes-ready"]')).toBe('2');
		expect(text(mounted, '[data-test="nodes-draining"]')).toBe('1');
		expect(text(mounted, '[data-test="not-ready-nodes"]')).toContain(
			'nomad-a3',
		);
		expect(text(mounted, '[data-test="not-ready-nodes"]')).toContain(
			'draining',
		);
	});

	it('lists unmanaged jobs', async () => {
		const mounted = await open(build('normal', 'dc_a'));

		expect(text(mounted, '[data-test="unmanaged"]')).toContain('node-exporter');
		expect(text(mounted, '[data-test="unmanaged"]')).toContain('running');
	});

	it('shows unknown job states, types and reasons as they are', async () => {
		const dc = build('alarm', 'dc_a');
		const stages = dc.nomad?.stages?.map((stage) => ({
			...stage,
			jobs: stage.jobs?.map((j) => ({
				...j,
				state: 'PAUSED',
				type: j.job === 'webitel-engine' ? 'FUTURE_TYPE' : j.type,
				missing_nodes: j.missing_nodes?.map((n) => ({
					...n,
					reason: 'FUTURE_REASON',
				})),
			})),
		}));
		const mounted = await open({
			...dc,
			nomad: { ...dc.nomad, stages },
		} as unknown as Datacenter);

		const system = job(mounted, 'freeswitch');
		expect(system.find('[data-test="job-state"]').text()).toBe('PAUSED');
		expect(job(mounted, 'webitel-engine').text()).toContain('FUTURE_TYPE');
		expect(system.find('[data-test="missing-nodes"]').text()).toContain(
			'nomad-a3 (FUTURE_REASON)',
		);
	});

	it('says there are no managed jobs instead of a healthy empty list', async () => {
		const dc = build('normal', 'dc_a');
		const mounted = await open({
			...dc,
			nomad: { ...dc.nomad, stages: [] },
		});

		expect(text(mounted, '[data-test="stages-empty"]')).toBe(
			'No managed jobs reported',
		);
	});

	it('says there is no Nomad data', async () => {
		const mounted = await open({
			...build('normal', 'dc_a'),
			nomad: undefined,
		});

		expect(text(mounted, '[data-test="nomad-empty"]')).toBe('No Nomad data');
	});
});

describe('Info tab', () => {
	const open = (dc: Datacenter) => mountWith(dc, '/datacenters/dc_a?tab=info');

	it('shows every check with its result, detail and facts', async () => {
		const mounted = await open(build('alarm', 'dc_a'));

		const result = mounted.wrapper.find(
			'[data-test="check-result-info.services"]',
		);
		expect(result.text()).toBe('warning');
		expect(result.classes()).toContain('p-chip-warning');
		expect(text(mounted, '[data-test="check-detail-info.services"]')).toBe(
			'chat: 1 of 2 passing',
		);
		expect(text(mounted, '[data-test="check-facts-info.services"]')).toContain(
			'"passing": 1',
		);
		expect(text(mounted, '[data-test="check-result-info.rabbitmq"]')).toBe(
			'pass',
		);
	});

	it('shows an unknown result as it is', async () => {
		const dc = build('normal', 'dc_a');
		const mounted = await open({
			...dc,
			info_checks: [{ id: 'info.new', result: 'MAYBE', detail: 'x' }],
		} as unknown as Datacenter);

		expect(text(mounted, '[data-test="check-result-info.new"]')).toBe('MAYBE');
	});

	it('says there are no checks instead of passing silently', async () => {
		const mounted = await open({
			...build('normal', 'dc_a'),
			info_checks: undefined,
		});

		expect(text(mounted, '[data-test="info-empty"]')).toBe(
			'No information checks',
		);
	});
});

describe('Sources tab', () => {
	const SKEW = 5 * 60 * 1000;
	const open = (dc: Datacenter) =>
		mountWith(dc, '/datacenters/dc_a?tab=sources');
	const cell = (mounted: Mounted, what: string, system: string) =>
		mounted.wrapper.find(`[data-test="source-${what}-${system}"]`);

	it('shows reachability, error, since and the age of the last data of a lost source', async () => {
		const mounted = await open(build('unreachable', 'dc_a'));

		const reach = cell(mounted, 'reachability', 'PATRONI');
		expect(reach.text()).toBe('not answering');
		expect(reach.classes()).toContain('p-chip-error');
		expect(cell(mounted, 'error', 'PATRONI').text()).toBe('timeout');
		expect(cell(mounted, 'since', 'PATRONI').text()).not.toBe('—');
		expect(cell(mounted, 'age', 'PATRONI').text()).toContain('7 min ago');
	});

	it('greys a stale source and leaves a fresh one alone', async () => {
		const dc = build('normal', 'dc_a');
		const old = new Date(Date.now() - 3600 * 1000).toISOString();
		const mounted = await open({
			...dc,
			sources: dc.sources?.map((source) =>
				source.system === 'NOMAD' ? { ...source, observe_time: old } : source,
			),
		});

		expect(cell(mounted, 'age', 'NOMAD').attributes('data-stale')).toBe('true');
		expect(mounted.wrapper.find('.source-row--NOMAD').classes()).toContain(
			'dc-row--stale',
		);
		expect(cell(mounted, 'age', 'NOMAD').text()).toContain('1 h ago');
		expect(cell(mounted, 'age', 'CONSUL').attributes('data-stale')).toBe(
			'false',
		);
		expect(mounted.wrapper.find('.source-row--CONSUL').classes()).not.toContain(
			'dc-row--stale',
		);
	});

	it.each([
		['behind', -SKEW],
		['ahead of', SKEW],
	])(
		'measures the age by the server clock when the browser is %s it',
		async (_n, skew) => {
			const serverNow = Date.now() - skew;
			recordServerDate(new Date(serverNow).toUTCString());
			const dc = build('normal', 'dc_a', serverNow);
			const mounted = await open({
				...dc,
				sources: dc.sources?.map((source) =>
					source.system === 'NOMAD'
						? {
								...source,
								observe_time: new Date(serverNow - 150_000).toISOString(),
							}
						: source,
				),
			});

			expect(cell(mounted, 'age', 'NOMAD').text()).toContain('2 min ago');
			expect(cell(mounted, 'age', 'CONSUL').text()).toMatch(/[2-5] s ago/);
			expect(cell(mounted, 'age', 'CONSUL').attributes('data-stale')).toBe(
				'false',
			);
		},
	);

	it('takes the stale threshold from the status', async () => {
		const dc = build('normal', 'dc_a');
		const mounted = await open({
			...dc,
			sources: dc.sources?.map((source) => ({
				...source,
				observe_time: new Date(Date.now() - 40_000).toISOString(),
			})),
		});
		expect(cell(mounted, 'age', 'CONSUL').attributes('data-stale')).toBe(
			'true',
		);

		setStatus(mounted, { interval: '30s', cycle_duration: '2s' });
		await flushPromises();

		expect(cell(mounted, 'age', 'CONSUL').attributes('data-stale')).toBe(
			'false',
		);
	});

	it('treats a source without a time as stale', async () => {
		const dc = build('normal', 'dc_a');
		const mounted = await open({
			...dc,
			sources: dc.sources?.map((source) => ({
				...source,
				observe_time: undefined,
			})),
		});

		expect(cell(mounted, 'age', 'CONSUL').text()).toContain('unknown');
		expect(cell(mounted, 'age', 'CONSUL').attributes('data-stale')).toBe(
			'true',
		);
	});

	const wrongConsul = () => {
		const dc = build('normal', 'dc_a');
		return {
			...dc,
			consul: {
				servers: [
					{
						address: '10.1.0.89:8500',
						reachable: true,
						consul_datacenter: 'dc_a',
					},
					{
						address: '10.1.0.90:8500',
						reachable: true,
						consul_datacenter: 'dc_z',
					},
				],
			},
		};
	};

	it('flags a Consul server of another datacenter', async () => {
		const mounted = await open(wrongConsul());

		const mismatches = mounted.wrapper.findAll('[data-test="consul-mismatch"]');
		expect(mismatches).toHaveLength(1);
		expect(mismatches[0].text()).toContain('does not match dc_a');
	});

	it('does not flag a differing Consul datacenter in stretch: one Consul reports one name', async () => {
		const mounted = await open(wrongConsul());
		setStatus(mounted, { profile: 'STRETCH' });
		await flushPromises();

		expect(mounted.wrapper.find('[data-test="consul-mismatch"]').exists()).toBe(
			false,
		);
	});

	it('shows unknown errors as they are', async () => {
		const dc = build('unreachable', 'dc_a');
		const mounted = await open({
			...dc,
			sources: dc.sources?.map((source) => ({
				...source,
				error: 'FUTURE_ERROR',
			})),
		} as unknown as Datacenter);

		expect(cell(mounted, 'error', 'PATRONI').text()).toBe('FUTURE_ERROR');
	});

	it.each([
		['NO_ENDPOINTS', 'no endpoints configured'],
		['NO_LEADER', 'no leader'],
	])('shows the new error %s in words', async (error, label) => {
		const dc = build('unreachable', 'dc_a');
		const mounted = await open({
			...dc,
			sources: dc.sources?.map((source) => ({ ...source, error })),
		} as unknown as Datacenter);

		expect(cell(mounted, 'error', 'PATRONI').text()).toBe(label);
	});

	it('says there are no sources instead of listing nothing', async () => {
		const mounted = await open({ ...build('normal', 'dc_a'), sources: [] });

		expect(text(mounted, '[data-test="sources-empty"]')).toBe(
			'No sources reported',
		);
	});
});

describe('values the contract added', () => {
	it('shows the state UNDETERMINED in yellow, in words', async () => {
		const mounted = await mountWith({
			...build('normal', 'dc_a'),
			state: 'UNDETERMINED',
		} as unknown as Datacenter);

		const chip = mounted.wrapper.find('[data-test="dc-state"]');
		expect(chip.text()).toBe('Undetermined');
		expect(chip.classes()).toContain('p-chip-warning');
	});

	it('shows NO_ALLOCATION in words', async () => {
		const mounted = await mountWith(
			build('alarm', 'dc_a'),
			'/datacenters/dc_a?tab=apps',
		);

		expect(
			text(mounted, '.job-row--filebeat [data-test="missing-nodes"]'),
		).toContain('no allocation');
	});
});

describe('stale and failing polls', () => {
	it('warns that old data may not match the real state', async () => {
		const mounted = await mountWith(build('stale', 'dc_a'));

		expect(text(mounted, '[data-test="dc-stale"]')).toContain('10 min old');
		expect(mounted.wrapper.find('[data-test="data-age"]').classes()).toContain(
			'data-age--stale',
		);
	});

	it('takes the threshold from the status: the same age is fresh with a long interval', async () => {
		const dc = build('normal', 'dc_a');
		const mounted = await mountWith({
			...dc,
			observe_time: new Date(Date.now() - 40_000).toISOString(),
		});
		expect(mounted.wrapper.find('[data-test="dc-stale"]').exists()).toBe(true);

		setStatus(mounted, { interval: '30s', cycle_duration: '2s' });
		await flushPromises();

		expect(mounted.wrapper.find('[data-test="dc-stale"]').exists()).toBe(false);
	});

	it('shows no warning for fresh data', async () => {
		const mounted = await mountWith(build('normal', 'dc_a'));

		expect(mounted.wrapper.find('[data-test="dc-stale"]').exists()).toBe(false);
		expect(mounted.wrapper.find('[data-test="dc-poll-error"]').exists()).toBe(
			false,
		);
	});

	it('keeps the last data and says that the refresh failed', async () => {
		const mounted = await mountWith(build('normal', 'dc_a'));
		getDatacenter.mockRejectedValue(apiError(503, 'dcctl.unavailable'));

		vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
		document.dispatchEvent(new Event('visibilitychange'));
		vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible');
		document.dispatchEvent(new Event('visibilitychange'));
		await flushPromises();

		const banner = text(mounted, '[data-test="dc-poll-error"]');
		expect(banner).toContain('Cannot refresh the datacenter');
		expect(banner).toContain('mock dcctl.unavailable');
		expect(mounted.wrapper.find('.member-row--pg-a1').exists()).toBe(true);
	});
});

describe('another datacenter', () => {
	it('loads the datacenter of the route', async () => {
		await mountWith(build('normal', 'dc_b'), '/datacenters/dc_b');

		expect(getDatacenter.mock.calls[0][0]).toBe('dc_b');
	});

	it('shows loading again, not the old datacenter, after a switch', async () => {
		const mounted = await mountWith(build('normal', 'dc_a'));
		getDatacenter.mockReturnValue(new Promise(() => {}));

		await mounted.router.push('/datacenters/dc_b');
		await flushPromises();

		expect(mounted.wrapper.find('[data-test="dc-loading"]').exists()).toBe(
			true,
		);
		expect(mounted.wrapper.find('[data-test="dc-title"]').exists()).toBe(false);
	});
});
