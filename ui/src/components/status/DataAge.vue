<template>
  <span
    class="data-age typo-body-1"
    :class="{ 'data-age--stale': stale }"
    :title="stale ? $t('dcctl.header.staleHint', { seconds: STALE_AFTER_SECONDS }) : undefined"
    data-test="data-age"
  >
    <wt-icon
      v-if="stale"
      icon="attention"
      size="sm"
    />
    <template v-if="age === null">{{ $t('dcctl.header.noData') }}</template>
    <template v-else>
      {{ $t('dcctl.header.updated', { age: text }) }}
    </template>
  </span>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';

import { formatAge, STALE_AFTER_SECONDS } from '@/utils/dataAge';

const props = defineProps<{
	/** Seconds since the last collection cycle; `null` before the first answer. */
	age: number | null;
	stale: boolean;
}>();

const { t } = useI18n();

const text = computed(() => {
	if (props.age === null) return '';
	const { unit, value } = formatAge(props.age);
	return t(`dcctl.header.ageUnit.${unit}`, { n: value });
});
</script>

<style scoped>
.data-age {
  display: inline-flex;
  align-items: center;
  gap: var(--spacing-xs);
}

.data-age--stale {
  color: var(--text-disabled-color);
}
</style>
