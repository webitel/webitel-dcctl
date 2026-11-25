<template>
  <div
    class="dc-tab"
    data-test="tab-info"
  >
    <last-known-notice
      :source="sourceFor(datacenter, 'CONSUL')"
      :now-ms="nowMs"
    />
    <p
      v-if="!checks.length"
      class="typo-body-1"
      data-test="info-empty"
    >
      {{ $t('dcctl.datacenter.info.empty') }}
    </p>
    <wt-table
      v-else
      :headers="headers"
      :data="checks"
      data-key="id"
      :sortable="false"
    >
      <template #id="{ item }">
        <span class="typo-body-1-bold">{{ item.id }}</span>
      </template>
      <template #result="{ item }">
        <wt-chip
          :color="toChipColor(checkColor(item.result))"
          :data-test="`check-result-${item.id}`"
        >
          {{ enumLabel('checkResult', item.result) }}
        </wt-chip>
      </template>
      <template #detail="{ item }">
        <div :data-test="`check-detail-${item.id}`">{{ item.detail }}</div>
        <details
          v-if="hasFacts(item.facts)"
          class="typo-body-2"
        >
          <summary>{{ $t('dcctl.datacenter.info.facts') }}</summary>
          <pre :data-test="`check-facts-${item.id}`">{{ JSON.stringify(item.facts, null, 2) }}</pre>
        </details>
      </template>
    </wt-table>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';

import type {
	WebitelDcctlV1CheckResult as CheckResult,
	WebitelDcctlV1Datacenter as Datacenter,
} from '@/api/gen/_models';
import { useEnumLabel } from '@/composables/useEnumLabel';
import { sourceFor } from '@/utils/datacenterDetail';
import { type StatusColor, toChipColor } from '@/utils/statusMapping';
import LastKnownNotice from './LastKnownNotice.vue';
import './dcGrid.css';

const props = defineProps<{
	datacenter: Datacenter;
	/** Server time, ms. */
	nowMs: number;
}>();

const { t } = useI18n();
const enumLabel = useEnumLabel();

const headers = computed(() => [
	{ value: 'id', text: t('dcctl.datacenter.info.check') },
	{ value: 'result', text: t('dcctl.datacenter.info.result') },
	{ value: 'detail', text: t('dcctl.datacenter.info.detail') },
]);

const checks = computed(() => props.datacenter.info_checks ?? []);

const checkColor = (result?: CheckResult): StatusColor => {
	switch (result) {
		case 'PASS':
			return 'green';
		case 'FAIL':
			return 'red';
		case 'WARN':
			return 'yellow';
		default:
			return 'gray';
	}
};

const hasFacts = (facts: object | undefined): boolean =>
	!!facts && Object.keys(facts).length > 0;
</script>

<style scoped>
pre {
  margin: var(--spacing-xs) 0 0;
  white-space: pre-wrap;
}
</style>
