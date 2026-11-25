<template>
  <div class="status-header">
    <span
      class="typo-body-1-bold"
      data-test="observer"
    >
      {{ observerText }}
    </span>
    <data-age
      :age="store.age"
      :stale="store.isStale"
    />
    <wt-chip
      v-if="store.error"
      :color="toChipColor('red')"
      :title="$t('dcctl.header.pollFailedHint')"
      data-test="poll-error"
    >
      {{ $t('dcctl.header.pollFailed') }}
    </wt-chip>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';

import { useStatusStore } from '@/stores/status';
import { resourceId } from '@/utils/datacenterSummary';
import { toChipColor } from '@/utils/statusMapping';
import DataAge from './DataAge.vue';

const store = useStatusStore();
const { t } = useI18n();

const observerText = computed(() => {
	const datacenter = resourceId(store.status?.observer?.datacenter);
	return datacenter
		? t('dcctl.header.observer', { datacenter })
		: t('dcctl.header.observerUnknown');
});
</script>

<style scoped>
.status-header {
  display: flex;
  align-items: center;
  gap: var(--spacing-sm);
  margin-right: auto;
}
</style>
