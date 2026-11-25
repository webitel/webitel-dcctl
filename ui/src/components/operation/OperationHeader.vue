<template>
  <header class="op-header">
    <div class="op-header__title">
      <h1
        class="typo-heading-2"
        data-test="op-title"
      >
        {{ enumLabel('operationType', operation.type) }}
      </h1>
      <wt-chip
        :color="toChipColor(operationStateColor(operation.state))"
        data-test="op-state"
      >
        {{ enumLabel('operationState', operation.state) }}
      </wt-chip>
      <wt-chip
        v-if="operation.mode"
        :color="toChipColor(modeColor(operation.mode))"
        data-test="op-mode"
      >
        {{ enumLabel('operationMode', operation.mode) }}
      </wt-chip>
      <slot name="connection" />
    </div>

    <dl
      class="dc-facts typo-body-1"
      data-test="op-facts"
    >
      <template v-if="operation.target">
        <dt>{{ $t('dcctl.operation.fields.target') }}</dt>
        <dd data-test="op-target">{{ resourceId(operation.target) }}</dd>
      </template>
      <template v-if="generation">
        <dt>{{ $t('dcctl.operation.fields.generation') }}</dt>
        <dd data-test="op-generation">
          {{ generation.from }} →
          {{ generation.to ?? $t('dcctl.operation.fields.generationUnknown') }}
        </dd>
      </template>
      <template v-if="operation.initiator">
        <dt>{{ $t('dcctl.operation.fields.initiator') }}</dt>
        <dd data-test="op-initiator">{{ initiatorText }}</dd>
      </template>
      <template v-if="operation.leader?.node">
        <dt>{{ $t('dcctl.operation.fields.leader') }}</dt>
        <dd data-test="op-leader">
          {{ operation.leader.node }}
          <template v-if="operation.leader.datacenter">
            ({{ resourceId(operation.leader.datacenter) }})
          </template>
        </dd>
      </template>
      <template v-if="operation.create_time">
        <dt>{{ $t('dcctl.operation.fields.created') }}</dt>
        <dd data-test="op-created">{{ timeText(operation.create_time) }}</dd>
      </template>
      <template v-if="operation.update_time">
        <dt>{{ $t('dcctl.operation.fields.updated') }}</dt>
        <dd data-test="op-updated">{{ timeText(operation.update_time) }}</dd>
      </template>
      <template v-if="operation.end_time">
        <dt>{{ $t('dcctl.operation.fields.finished') }}</dt>
        <dd data-test="op-finished">{{ timeText(operation.end_time) }}</dd>
      </template>
      <template v-if="duration !== null">
        <dt>{{ $t('dcctl.operation.fields.duration') }}</dt>
        <dd data-test="op-duration">{{ ageText(Math.floor(duration / 1000)) }}</dd>
      </template>
    </dl>

    <section
      v-if="operation.warnings?.length"
      class="op-header__block"
      data-test="op-warnings"
    >
      <h2 class="typo-heading-4">{{ $t('dcctl.operation.warnings') }}</h2>
      <wt-message
        v-for="warning in operation.warnings"
        :key="warning"
        color="warn"
      >
        {{ warning }}
      </wt-message>
    </section>

    <section
      v-if="operation.error"
      class="op-header__block"
      data-test="op-error"
    >
      <h2 class="typo-heading-4">{{ $t('dcctl.operation.error') }}</h2>
      <wt-message color="error">
        <span class="typo-body-1-bold">{{ operation.error.id }}</span>
        <span v-if="operation.error.detail"> — {{ operation.error.detail }}</span>
        <ul
          v-if="metadata.length"
          class="op-header__metadata"
        >
          <li
            v-for="[key, value] in metadata"
            :key="key"
          >
            {{ key }}: {{ value }}
          </li>
        </ul>
      </wt-message>
    </section>
  </header>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';

import type { WebitelDcctlV1Operation as Operation } from '@/api/gen/_models';
import { useAgeText } from '@/composables/useAgeText';
import { useEnumLabel } from '@/composables/useEnumLabel';
import { useTimeText } from '@/composables/useTimeText';
import { resourceId } from '@/utils/datacenterSummary';
import { durationMs, generationRange } from '@/utils/operationView';
import {
	modeColor,
	operationStateColor,
	toChipColor,
} from '@/utils/statusMapping';
import '@/components/datacenter/dcGrid.css';

const props = defineProps<{
	operation: Operation;
	/** Time of the data in server milliseconds. */
	nowMs: number;
}>();

const { t } = useI18n();
const enumLabel = useEnumLabel();
const timeText = useTimeText();
const ageText = useAgeText();

const generation = computed(() => generationRange(props.operation));
const duration = computed(() =>
	durationMs(
		props.operation.create_time,
		props.operation.end_time,
		props.nowMs,
	),
);

const initiatorText = computed(() => {
	const initiator = props.operation.initiator;
	const via = initiator?.instance?.node;
	return [
		initiator?.user,
		initiator?.address && `(${initiator.address})`,
		via && t('dcctl.operation.fields.via', { node: via }),
	]
		.filter(Boolean)
		.join(' ');
});

const metadata = computed(() =>
	Object.entries(props.operation.error?.metadata ?? {}),
);
</script>

<style scoped>
.op-header {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-sm);
}

.op-header__title {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--spacing-sm);
}

.op-header__block {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-xs);
}

.op-header__metadata {
  margin: 0;
  padding-left: var(--spacing-md);
}
</style>
