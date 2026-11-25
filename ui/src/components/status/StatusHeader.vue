<template>
  <div class="status-header">
    <span
      class="typo-body-1-bold"
      data-test="observer"
    >
      {{ observerText }}
    </span>
    <span
      v-if="profileText"
      class="typo-body-1"
      data-test="profile"
    >
      {{ profileText }}
    </span>
    <data-age
      :age="store.age"
      :stale="store.isStale"
    />
    <wt-chip
      v-if="store.error"
      :color="toChipColor('red')"
      :title="errorTitle"
      data-test="poll-error"
    >
      {{ $t('dcctl.header.pollFailed') }}
    </wt-chip>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';

import { apiErrorMessage } from '@/api/client/apiError';
import { useEnumLabel } from '@/composables/useEnumLabel';
import { useStatusStore } from '@/stores/status';
import { resourceId } from '@/utils/datacenterSummary';
import { toChipColor } from '@/utils/statusMapping';
import DataAge from './DataAge.vue';

const store = useStatusStore();
const { t } = useI18n();
const enumLabel = useEnumLabel();

const observerText = computed(() => {
	const datacenter = resourceId(store.status?.observer?.datacenter);
	return datacenter
		? t('dcctl.header.observer', { datacenter })
		: t('dcctl.header.observerUnknown');
});

const profileText = computed(() =>
	enumLabel('profileName', store.status?.profile),
);

/** The reason of the failure; the hint about the last data only when there is data. */
const errorTitle = computed(() =>
	[
		store.error ? apiErrorMessage(store.error) : '',
		store.status ? t('dcctl.header.pollFailedHint') : '',
	]
		.filter(Boolean)
		.join('. '),
);
</script>

<style scoped>
.status-header {
  display: flex;
  align-items: center;
  gap: var(--spacing-sm);
  margin-right: auto;
}
</style>
