<template>
  <div
    class="dc-tab"
    data-test="tab-apps"
  >
    <last-known-notice
      :source="sourceFor(datacenter, 'NOMAD')"
      :now-ms="nowMs"
    />
    <p
      v-if="!nomad"
      class="typo-body-1"
      data-test="nomad-empty"
    >
      {{ $t('dcctl.datacenter.apps.empty') }}
    </p>
    <template v-else>
      <h3 class="typo-heading-4">{{ $t('dcctl.datacenter.apps.nodes') }}</h3>
      <dl
        class="dc-facts typo-body-1"
        data-test="nodes"
      >
        <dt>{{ $t('dcctl.datacenter.apps.leader') }}</dt>
        <dd>{{ nomad.leader || $t('dcctl.datacenter.noData') }}</dd>
        <template
          v-for="item in nodeCounts"
          :key="item.key"
        >
          <dt>{{ $t(`dcctl.datacenter.apps.${item.key}`) }}</dt>
          <dd :data-test="`nodes-${item.key}`">
            <wt-chip :color="toChipColor(item.count > 0 && item.key !== 'ready' ? 'yellow' : 'gray')">
              {{ item.count }}
            </wt-chip>
          </dd>
        </template>
      </dl>
      <template v-if="notReadyNodes.length">
        <h4 class="typo-subtitle-1">{{ $t('dcctl.datacenter.apps.notReady') }}</h4>
        <ul
          class="dc-list typo-body-1"
          data-test="not-ready-nodes"
        >
          <li
            v-for="node in notReadyNodes"
            :key="node.node"
          >
            {{ node.node }}
            <wt-chip :color="toChipColor('yellow')">
              {{ enumLabel('nodeReason', node.reason) }}
            </wt-chip>
          </li>
        </ul>
      </template>

      <h3 class="typo-heading-4">{{ $t('dcctl.datacenter.apps.stages') }}</h3>
      <p
        v-if="!stages.length"
        class="typo-body-1"
        data-test="stages-empty"
      >
        {{ $t('dcctl.datacenter.apps.noStages') }}
      </p>
      <wt-expansion-panel
        v-for="stage in stages"
        :key="stage.stage"
        :data-test="`stage-${stage.stage}`"
      >
        <template #title>
          <span class="dc-cell">
            {{ $t('dcctl.datacenter.apps.stage', { n: stage.stage }) }}
            <wt-chip
              v-for="count in stageCounts(stage)"
              :key="count.key"
              :color="toChipColor(count.color)"
            >
              {{ count.text }}
            </wt-chip>
          </span>
        </template>
        <stage-jobs-table
          :stage="stage"
          :expect-running="expectRunning"
        />
      </wt-expansion-panel>

      <h3 class="typo-heading-4">{{ $t('dcctl.datacenter.apps.unmanaged') }}</h3>
      <p
        v-if="!unmanaged.length"
        class="typo-body-1"
        data-test="unmanaged-empty"
      >
        {{ $t('dcctl.datacenter.apps.noUnmanaged') }}
      </p>
      <wt-table
        v-else
        :headers="unmanagedHeaders"
        :data="unmanaged"
        data-key="job"
        :sortable="false"
        data-test="unmanaged"
      >
        <template #job="{ item }">
          <span class="typo-body-1-bold">{{ item.job }}</span>
        </template>
      </wt-table>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';

import type {
	WebitelDcctlV1Datacenter as Datacenter,
	WebitelDcctlV1DatacenterNomadStage as Stage,
} from '@/api/gen/_models';
import { useEnumLabel } from '@/composables/useEnumLabel';
import { sourceFor } from '@/utils/datacenterDetail';
import { toChipColor } from '@/utils/statusMapping';
import LastKnownNotice from './LastKnownNotice.vue';
import StageJobsTable from './StageJobsTable.vue';
import './dcGrid.css';

const props = defineProps<{
	datacenter: Datacenter;
	/** Server time, ms. */
	nowMs: number;
	/** `true` for the active datacenter, `false` for an inactive one, `undefined` while unknown. */
	expectRunning?: boolean;
}>();

const { t } = useI18n();
const enumLabel = useEnumLabel();

const nomad = computed(() => props.datacenter.nomad);
const stages = computed(() =>
	[...(nomad.value?.stages ?? [])].sort(
		(a, b) => (a.stage ?? 0) - (b.stage ?? 0),
	),
);
const unmanaged = computed(() => nomad.value?.unmanaged_jobs ?? []);
const notReadyNodes = computed(() => nomad.value?.not_ready_nodes ?? []);

// The gateway omits a zero: inside a Nomad block a missing count is 0.
const nodeCounts = computed(() => [
	{ key: 'ready', count: nomad.value?.ready_node_count ?? 0 },
	{ key: 'ineligible', count: nomad.value?.ineligible_node_count ?? 0 },
	{ key: 'draining', count: nomad.value?.draining_node_count ?? 0 },
	{ key: 'down', count: nomad.value?.down_node_count ?? 0 },
]);

const unmanagedHeaders = computed(() => [
	{ value: 'job', text: t('dcctl.datacenter.apps.job') },
	{ value: 'nomad_status', text: t('dcctl.datacenter.apps.nomadStatus') },
]);

const stageCounts = (stage: Stage) => {
	const counts = stage.job_counts;
	return [
		{ key: 'healthy', n: counts?.healthy_count, color: 'green' },
		{ key: 'unhealthy', n: counts?.unhealthy_count, color: 'red' },
		{ key: 'stopping', n: counts?.stopping_count, color: 'yellow' },
		{ key: 'stopped', n: counts?.stopped_count, color: 'gray' },
	]
		.filter((item) => item.n)
		.map((item) => ({
			key: item.key,
			color: item.color as 'green' | 'red' | 'yellow' | 'gray',
			text: t(`dcctl.datacenter.apps.counts.${item.key}`, { n: item.n }),
		}));
};
</script>

<style scoped>
.dc-list {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-xs);
  margin: 0;
  padding-left: var(--spacing-md);
}
</style>
