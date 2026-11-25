<template>
  <div class="op-page">
    <div
      v-if="phase === 'notFound'"
      class="op-page__message"
      role="alert"
      data-test="op-not-found"
    >
      <p class="typo-body-1">
        {{ $t('dcctl.operation.notFound', { id: operationId }) }}
      </p>
      <router-link
        :to="{ name: 'operations' }"
        class="typo-body-1"
        data-test="op-back"
      >
        {{ $t('dcctl.operation.back') }}
      </router-link>
    </div>

    <template v-else-if="operation">
      <wt-message
        v-if="isReconnecting"
        color="warn"
        data-test="op-reconnecting"
      >
        <strong>{{ $t('dcctl.operation.connection.reconnecting') }}</strong>
        {{ age === null
          ? $t('dcctl.operation.connection.reconnectingNoAge')
          : $t('dcctl.operation.connection.reconnectingHint', { age: ageText(age) }) }}
        <template v-if="error">{{ apiErrorMessage(error) }}</template>
      </wt-message>
      <wt-message
        v-if="behind"
        color="warn"
        data-test="op-behind"
      >
        {{ $t('dcctl.operation.connection.behind') }}
      </wt-message>

      <operation-header
        :operation="operation"
        :now-ms="nowMs"
      >
        <template #connection>
          <wt-chip
            :color="toChipColor(connectionColor)"
            data-test="op-connection"
          >
            {{ $t(`dcctl.operation.connection.${connection}`) }}
          </wt-chip>
          <span
            v-if="age !== null && connection !== 'ended'"
            class="typo-body-1"
            data-test="op-age"
          >
            {{ $t('dcctl.operation.connection.updated', { age: ageText(age) }) }}
          </span>
        </template>
      </operation-header>

      <operation-steps
        :operation="operation"
        :now-ms="nowMs"
        :stale="isReconnecting"
      />
    </template>

    <div
      v-else-if="phase === 'error'"
      class="op-page__message typo-body-1"
      role="alert"
      data-test="op-unavailable"
    >
      {{ $t('dcctl.operation.unavailable') }}: {{ apiErrorMessage(error) }}
      {{ $t('dcctl.operation.unavailableRetry') }}
    </div>
    <p
      v-else
      class="typo-body-1"
      data-test="op-loading"
    >
      {{ $t('dcctl.operation.loading') }}
    </p>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue';
import { useRoute } from 'vue-router';

import { apiErrorMessage } from '@/api/client/apiError';
import { serverOffsetMs } from '@/api/client/serverClock';
import OperationHeader from '@/components/operation/OperationHeader.vue';
import OperationSteps from '@/components/operation/OperationSteps.vue';
import { useAgeText } from '@/composables/useAgeText';
import { useOperation } from '@/composables/useOperation';
import { useStatusStore } from '@/stores/status';
import { isBehind, isMissingFromStatus } from '@/utils/operationView';
import type { StatusColor } from '@/utils/statusMapping';
import { toChipColor } from '@/utils/statusMapping';

const route = useRoute();
const ageText = useAgeText();
const statusStore = useStatusStore();

// The view is still mounted for a moment while the route already points elsewhere: keep the last id.
const operationId = ref(String(route.params.id));
watch(
	() => route.params.id,
	(value) => {
		if (route.name === 'operation' && typeof value === 'string') {
			operationId.value = value;
		}
	},
);

const { operation, phase, error, connection, age, now, receivedAt, reconnect } =
	useOperation(operationId);

const isReconnecting = computed(() => connection.value === 'reconnecting');

// While the feed is gone the durations stop at the last message instead of counting on.
const nowMs = computed(() => {
	const base =
		isReconnecting.value && receivedAt.value !== null
			? receivedAt.value
			: now.value;
	return base + serverOffsetMs();
});

const connectionColor = computed<StatusColor>(() => {
	switch (connection.value) {
		case 'live':
			return 'green';
		case 'reconnecting':
			return 'yellow';
		default:
			return 'gray';
	}
});

// The status is polled on its own. The page is behind when the status already
// shows a later change of this operation, or, with a fresh status, no longer
// lists an operation the page shows as unfinished (a finished operation leaves
// the status). A half-open feed would otherwise show a stale step as current.
const behind = computed(() => {
	const shown = operation.value;
	const status = statusStore.status;
	if (!shown || !status) return false;
	if (isBehind(shown, status.operation)) return true;
	const trusted = !statusStore.error && !statusStore.isStale;
	return (
		trusted && isMissingFromStatus(shown, status.operation, status.observe_time)
	);
});
watch(
	() => statusStore.status,
	() => {
		if (behind.value) reconnect();
	},
);
</script>

<style scoped>
.op-page {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-md);
  padding: var(--spacing-md);
}

.op-page__message {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-xs);
}
</style>
