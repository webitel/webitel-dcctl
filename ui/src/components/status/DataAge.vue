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
      :aria-label="$t('dcctl.header.staleShort')"
    />
    <template v-if="age === null">{{ $t('dcctl.header.noData') }}</template>
    <template v-else>
      {{ $t('dcctl.header.updated', { age: ageText(age) }) }}
    </template>
    <span v-if="stale && age !== null">{{ $t('dcctl.header.staleShort') }}</span>
  </span>
</template>

<script setup lang="ts">
import { useAgeText } from '@/composables/useAgeText';
import { STALE_AFTER_SECONDS } from '@/utils/dataAge';

defineProps<{
	/** Seconds since the last collection cycle; `null` before the first answer. */
	age: number | null;
	stale: boolean;
}>();

const ageText = useAgeText();
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
