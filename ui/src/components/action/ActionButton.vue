<template>
  <wt-tooltip
    v-if="availability.visible"
    :disabled="!availability.reason"
  >
    <template #activator>
      <span
        class="action-button"
        :class="{ 'action-button--off': !!availability.reason }"
        :data-test="`action-${kind}`"
        :data-reason="availability.reason"
      >
        <wt-button
          :color="color"
          :disabled="!!availability.reason"
          :aria-description="reasonText"
          @click="emit('click')"
        >
          <slot>{{ $t(`dcctl.action.button.${kind}`) }}</slot>
        </wt-button>
      </span>
    </template>
    <span data-test="action-reason">{{ reasonText }}</span>
  </wt-tooltip>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';

import type { ActionKind, Availability } from '@/utils/actionAvailability';

const props = withDefaults(
	defineProps<{
		kind: ActionKind;
		availability: Availability;
		color?: 'primary' | 'secondary' | 'error';
	}>(),
	{ color: 'secondary' },
);

const emit = defineEmits<{ click: [] }>();

const { t } = useI18n();

/** The tooltip of a disabled button: why it cannot be pressed. */
const reasonText = computed(() =>
	props.availability.reason
		? t(`dcctl.action.unavailable.${props.availability.reason}`)
		: undefined,
);
</script>

<style scoped>
.action-button {
  display: inline-flex;
}

/* A disabled button swallows the pointer events: the tooltip must hear them on the wrapper. */
.action-button--off :deep(button) {
  pointer-events: none;
}
</style>
