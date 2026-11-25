<template>
  <wt-table
    :headers="jobHeaders"
    :data="rows"
    :row-class="jobRowClass"
    data-key="name"
    :sortable="false"
  >
    <template #name="{ item }">
      <span class="typo-body-1-bold">{{ item.name }}</span>
    </template>
    <template #type="{ item }">
      {{ enumLabel('jobType', item.type) }}
    </template>
    <template #state="{ item }">
      <span class="dc-cell">
        <wt-chip
          :color="toChipColor(jobColor(item.state, { expectRunning }))"
          data-test="job-state"
        >
          {{ enumLabel('jobState', item.state) }}
        </wt-chip>
        <span
          v-if="item.stop"
          class="typo-body-2"
        >
          {{ $t('dcctl.datacenter.apps.stopRequested') }}
        </span>
      </span>
    </template>
    <template #allocations="{ item }">
      <span
        class="dc-cell"
        data-test="job-allocations"
      >
        <span
          v-for="entry in item.allocations"
          :key="entry.status"
          :class="{ 'dc-problem': entry.problem }"
        >
          {{ entry.status }} {{ entry.count }}{{ entry.last ? '' : ', ' }}
        </span>
        <template v-if="!item.allocations.length">—</template>
      </span>
    </template>
    <template #nodes="{ item }">
      <span
        v-if="item.coverage"
        class="dc-cell"
        data-test="job-nodes"
      >
        <wt-chip
          :color="toChipColor(item.coverage.color)"
          data-test="job-coverage"
        >
          {{ $t('dcctl.datacenter.apps.systemNodes', { healthy: item.coverage.healthy, expected: item.coverage.expected }) }}
        </wt-chip>
        <span
          v-if="item.coverage.missing.length"
          class="typo-body-2"
          data-test="missing-nodes"
        >
          {{ $t('dcctl.datacenter.apps.missingOn') }}
          {{ missingText(item.coverage.missing) }}
        </span>
      </span>
      <span
        v-else
        data-test="job-nodes"
      >—</span>
    </template>
  </wt-table>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';

import type {
	WebitelDcctlV1DatacenterNomadNode as NomadNode,
	WebitelDcctlV1DatacenterNomadStage as Stage,
} from '@/api/gen/_models';
import { useEnumLabel } from '@/composables/useEnumLabel';
import {
	allocationEntries,
	jobColor,
	systemJobCoverage,
} from '@/utils/datacenterDetail';
import { toChipColor } from '@/utils/statusMapping';
import './dcGrid.css';

const props = defineProps<{
	stage: Stage;
	/** `true` when the jobs should be running, `false` when stopped, `undefined` while unknown. */
	expectRunning?: boolean;
}>();

const { t } = useI18n();
const enumLabel = useEnumLabel();

const jobHeaders = computed(() => [
	{ value: 'name', text: t('dcctl.datacenter.apps.job') },
	{ value: 'type', text: t('dcctl.datacenter.apps.type') },
	{ value: 'state', text: t('dcctl.datacenter.apps.state') },
	{ value: 'allocations', text: t('dcctl.datacenter.apps.allocations') },
	{ value: 'nodes', text: t('dcctl.datacenter.apps.nodesColumn') },
]);

const rows = computed(() =>
	(props.stage.jobs ?? []).map((job) => ({
		name: job.job,
		type: job.type,
		state: job.state,
		stop: job.stop,
		coverage: systemJobCoverage(job),
		allocations: allocationEntries(job.allocation_counts).map(
			(entry, index, all) => ({ ...entry, last: index === all.length - 1 }),
		),
	})),
);

const jobRowClass = (row: { name?: string }) => `job-row job-row--${row.name}`;

const missingText = (nodes: readonly NomadNode[]): string =>
	nodes
		.map((node) => {
			const reason = enumLabel('nodeReason', node.reason);
			return reason ? `${node.node} (${reason})` : node.node;
		})
		.join(', ');
</script>

<style scoped>
.dc-problem {
  color: var(--text-error-color);
  font-weight: 600;
}
</style>
