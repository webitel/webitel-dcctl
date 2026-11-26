<template>
  <div
    class="dc-tab"
    data-test="tab-sources"
  >
    <p
      v-if="!rows.length"
      class="typo-body-1"
      data-test="sources-empty"
    >
      {{ $t('dcctl.datacenter.sources.empty') }}
    </p>
    <wt-table
      v-else
      :headers="headers"
      :data="rows"
      :row-class="rowClass"
      data-key="key"
      :sortable="false"
      data-test="sources"
    >
      <template #system="{ item }">
        <span class="typo-body-1-bold">{{ enumLabel('sourceSystem', item.source.system) }}</span>
      </template>
      <template #endpoint="{ item }">
        {{ item.source.endpoint }}
      </template>
      <template #reachable="{ item }">
        <wt-chip
          :color="toChipColor(item.source.reachable ? 'green' : 'red')"
          :data-test="`source-reachability-${item.source.system}`"
        >
          {{ item.source.reachable
            ? $t('dcctl.datacenter.sources.reachable')
            : $t('dcctl.datacenter.sources.unreachable') }}
        </wt-chip>
      </template>
      <template #error="{ item }">
        <span :data-test="`source-error-${item.source.system}`">{{ item.source.error ? enumLabel('sourceError', item.source.error) : '—' }}</span>
      </template>
      <template #since="{ item }">
        <span :data-test="`source-since-${item.source.system}`">{{ item.source.unreachable_start_time ? formatTime(item.source.unreachable_start_time) : '—' }}</span>
      </template>
      <template #age="{ item }">
        <span
          class="dc-cell"
          :data-test="`source-age-${item.source.system}`"
          :data-stale="item.stale"
        >
          <wt-icon
            v-if="item.stale"
            icon="attention"
            size="sm"
          />
          <template v-if="item.age === null">{{ $t('dcctl.datacenter.sources.noTime') }}</template>
          <template v-else>{{ $t('dcctl.datacenter.sources.lastDataAge', { age: ageText(item.age) }) }}</template>
        </span>
      </template>
    </wt-table>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';

import type { WebitelDcctlV1Datacenter as Datacenter } from '@/api/gen/_models';
import { useAgeText } from '@/composables/useAgeText';
import { useEnumLabel } from '@/composables/useEnumLabel';
import { useTimeText } from '@/composables/useTimeText';
import { useStatusStore } from '@/stores/status';
import { sourceRow } from '@/utils/datacenterDetail';
import { toChipColor } from '@/utils/statusMapping';
import './dcGrid.css';

const props = defineProps<{
	datacenter: Datacenter;
	/** Server time, ms: ages must not depend on the browser clock. */
	nowMs: number;
}>();

const { t } = useI18n();
const enumLabel = useEnumLabel();
const ageText = useAgeText();
const formatTime = useTimeText();
const statusStore = useStatusStore();

const headers = computed(() => [
	{ value: 'system', text: t('dcctl.datacenter.sources.system') },
	{ value: 'endpoint', text: t('dcctl.datacenter.sources.endpoint') },
	{ value: 'reachable', text: t('dcctl.datacenter.sources.reachability') },
	{ value: 'error', text: t('dcctl.datacenter.sources.error') },
	{ value: 'since', text: t('dcctl.datacenter.sources.since') },
	{ value: 'age', text: t('dcctl.datacenter.sources.lastData') },
]);

const rows = computed(() =>
	(props.datacenter.sources ?? []).map((source) => ({
		key: source.system,
		source,
		...sourceRow(source, props.nowMs, statusStore.staleAfter),
	})),
);

const rowClass = (row: { source: { system?: string }; stale: boolean }) =>
	`source-row source-row--${row.source.system}${row.stale ? ' dc-row--stale' : ''}`;
</script>
