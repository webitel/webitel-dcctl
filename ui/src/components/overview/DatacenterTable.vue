<template>
  <section
    id="datacenters"
    class="dc-table"
  >
    <h2 class="typo-heading-3">{{ $t('dcctl.overview.datacenters.title') }}</h2>
    <p
      v-if="!rows.length"
      class="typo-body-1"
    >
      {{ $t('dcctl.overview.datacenters.empty') }}
    </p>
    <wt-table
      v-else
      :headers="headers"
      :data="rows"
      :row-class="rowClass"
      data-key="name"
      :sortable="false"
    >
      <template #name="{ item }">
        <router-link
          :to="{ name: 'datacenter', params: { dc: item.id } }"
          class="typo-body-1-bold"
          data-test="dc-name"
        >
          {{ item.id }}
        </router-link>
        <wt-chip
          v-if="item.isActive"
          :color="toChipColor('blue')"
        >
          {{ $t('dcctl.overview.datacenters.active') }}
        </wt-chip>
      </template>
      <template #state="{ item }">
        <wt-chip
          :color="toChipColor(stateColor(item.state, { isActive: item.isActive }))"
          data-test="dc-state"
        >
          {{ enumLabel('state', item.state) }}
        </wt-chip>
        <span
          v-if="item.needsRecovery && item.state !== 'NEEDS_RECOVERY'"
          class="typo-body-2"
        >
          {{ $t('dcctl.overview.datacenters.needsRecovery') }}
        </span>
      </template>
      <template #health="{ item }">
        <wt-chip
          :color="toChipColor(healthColor(item.health))"
          data-test="dc-health"
        >
          {{ enumLabel('health', item.health) }}
        </wt-chip>
      </template>
      <template #patroni="{ item }">
        <span
          v-if="item.patroni"
          class="dc-table__cell"
          data-test="dc-patroni"
        >
          <span class="typo-body-1">{{ item.patroni.leader }}</span>
          <span class="typo-body-2">{{ patroniDetails(item.patroni) }}</span>
        </span>
        <span
          v-else
          class="typo-body-1"
          data-test="dc-patroni"
        >{{ $t('dcctl.overview.datacenters.noData') }}</span>
      </template>
      <template #apps="{ item }">
        <span
          class="typo-body-1"
          data-test="dc-apps"
        >{{ item.apps
          ? $t('dcctl.overview.datacenters.appsValue', item.apps)
          : $t('dcctl.overview.datacenters.noData') }}</span>
      </template>
      <template #sources="{ item }">
        <span
          class="dc-table__cell"
          data-test="dc-sources"
        >
          <span class="typo-body-1">{{ $t('dcctl.overview.datacenters.sourcesValue', item.sources) }}</span>
          <span
            v-if="item.unreachable.length"
            class="typo-body-2"
          >
            {{ $t('dcctl.overview.datacenters.unreachableSources', { systems: unreachableText(item.unreachable) }) }}
          </span>
        </span>
      </template>
    </wt-table>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';

import type { WebitelDcctlV1DatacenterPatroni as Patroni } from '@/api/gen/_models';
import { useAgeText } from '@/composables/useAgeText';
import { useEnumLabel } from '@/composables/useEnumLabel';
import { useStatusStore } from '@/stores/status';
import {
	appsSummary,
	resourceId,
	sourcesSummary,
	unreachableSources,
} from '@/utils/datacenterSummary';
import { formatBytes } from '@/utils/formatBytes';
import { healthColor, stateColor, toChipColor } from '@/utils/statusMapping';

const store = useStatusStore();
const { t, locale } = useI18n();
const enumLabel = useEnumLabel();
const ageText = useAgeText();

const headers = computed(() => [
	{ value: 'name', text: t('dcctl.overview.datacenters.name') },
	{ value: 'state', text: t('dcctl.overview.datacenters.state') },
	{ value: 'health', text: t('dcctl.overview.datacenters.health') },
	{ value: 'patroni', text: t('dcctl.overview.datacenters.patroni') },
	{ value: 'apps', text: t('dcctl.overview.datacenters.apps') },
	{ value: 'sources', text: t('dcctl.overview.datacenters.sources') },
]);

const rows = computed(() =>
	store.datacenters.map((dc) => ({
		...dc,
		id: resourceId(dc.name),
		isActive: dc.name === store.status?.active_datacenter,
		needsRecovery: dc.needs_recovery,
		apps: appsSummary(dc),
		sources: sourcesSummary(dc),
		unreachable: unreachableSources(dc, store.now),
	})),
);

const rowClass = (row: { id?: string }) => `dc-row dc-row--${row.id}`;

const patroniDetails = (patroni: Patroni): string =>
	[
		enumLabel('patroniRole', patroni.role),
		patroni.timeline
			? t('dcctl.overview.datacenters.timeline', { timeline: patroni.timeline })
			: '',
		patroni.lag_bytes
			? t('dcctl.overview.datacenters.lag', {
					bytes: formatBytes(patroni.lag_bytes, locale.value),
				})
			: '',
	]
		.filter(Boolean)
		.join(', ');

const unreachableText = (
	sources: { system?: string; age: number | null }[],
): string =>
	sources
		.map(({ system, age }) => {
			const name = enumLabel('sourceSystem', system);
			return age === null ? name : `${name} (${ageText(age)})`;
		})
		.join(', ');
</script>

<style scoped>
.dc-table {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-xs);
}

.dc-table__cell {
  display: flex;
  flex-direction: column;
}
</style>
