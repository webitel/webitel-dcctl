<template>
  <wt-message
    v-if="source && !source.reachable"
    color="warn"
    data-test="last-known"
  >
    <template v-if="age === null">
      {{ $t('dcctl.datacenter.lastKnownNever') }}
    </template>
    <template v-else>
      {{ $t('dcctl.datacenter.lastKnown', { age: ageText(age) }) }}
    </template>
  </wt-message>
</template>

<script setup lang="ts">
import { computed } from 'vue';

import type { WebitelDcctlV1Source as Source } from '@/api/gen/_models';
import { useAgeText } from '@/composables/useAgeText';
import { ageSeconds } from '@/utils/dataAge';

const props = defineProps<{
	/** The source the values of the tab were read from. */
	source: Source | undefined;
	/** Server time, ms. */
	nowMs: number;
}>();

const ageText = useAgeText();
const age = computed(() => ageSeconds(props.source?.observe_time, props.nowMs));
</script>
