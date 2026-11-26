<template>
  <div
    v-if="buttons.length"
    class="op-actions"
    data-test="op-actions"
  >
    <action-button
      v-for="item in buttons"
      :key="item.kind"
      :kind="item.kind"
      :availability="item.availability"
      :color="item.kind === 'resume' ? 'primary' : 'secondary'"
      @click="dialogKind = item.kind"
    />
    <action-dialog
      v-if="dialogKind"
      :kind="dialogKind"
      :operation="operation"
      @close="dialogKind = null"
      @done="emit('done')"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue';

import type { WebitelDcctlV1Operation as Operation } from '@/api/gen/_models';
import ActionButton from '@/components/action/ActionButton.vue';
import ActionDialog from '@/components/action/ActionDialog.vue';
import {
	type ActionKind,
	actionAvailability,
} from '@/utils/actionAvailability';

const props = defineProps<{ operation: Operation }>();

const emit = defineEmits<{
	/** An action went through: the operation changed. */
	done: [];
}>();

const KINDS: readonly ActionKind[] = ['resume', 'abandon'];

const buttons = computed(() =>
	KINDS.map((kind) => ({
		kind,
		availability: actionAvailability(kind, { operation: props.operation }),
	})).filter((item) => item.availability.visible),
);

const dialogKind = ref<ActionKind | null>(null);
</script>

<style scoped>
.op-actions {
  display: flex;
  flex-wrap: wrap;
  gap: var(--spacing-xs);
}
</style>
