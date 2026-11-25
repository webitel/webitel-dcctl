<template>
  <div
    v-if="operation"
    class="operation-line"
    data-test="operation-line"
  >
    <wt-chip :color="toChipColor(operation.state === 'INTERRUPTED' ? 'yellow' : 'blue')">
      {{ stateLabel }}
    </wt-chip>
    <span class="typo-body-1-bold">{{ enumLabel('operationType', operation.type) }}</span>
    <span class="typo-body-1">{{ $t('dcctl.operationLine.target', { target: resourceId(operation.target) }) }}</span>
    <span class="typo-body-1">{{ stepText }}</span>
    <router-link
      class="typo-body-1"
      :to="{ name: 'operation', params: { id: resourceId(operation.name) } }"
    >
      {{ $t('dcctl.operationLine.open') }}
    </router-link>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';

import { useEnumLabel } from '@/composables/useEnumLabel';
import { useStatusStore } from '@/stores/status';
import { resourceId } from '@/utils/datacenterSummary';
import { currentStep } from '@/utils/operationLine';
import { toChipColor } from '@/utils/statusMapping';

const store = useStatusStore();
const { t } = useI18n();
const enumLabel = useEnumLabel();

const operation = computed(() => store.activeOperation);

const stateLabel = computed(() =>
	enumLabel('operationLine.state', operation.value?.state),
);

const stepText = computed(() => {
	const op = operation.value;
	const step = op ? currentStep(op) : undefined;
	if (!op || !step) return t('dcctl.operationLine.noStep');
	return t('dcctl.operationLine.step', {
		number: (step.index ?? 0) + 1,
		total: op.steps?.length ?? 0,
		step: enumLabel('stepType', step.type),
	});
});
</script>

<style scoped>
.operation-line {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--spacing-sm);
  padding: var(--spacing-xs) var(--spacing-md);
  border-bottom: 1px solid var(--warning-color);
}
</style>
