<template>
  <div class="ops-page">
    <div class="ops-page__title">
      <h1 class="typo-heading-2">{{ $t('dcctl.operations.title') }}</h1>
      <span
        v-if="age !== null"
        class="typo-body-1"
        data-test="ops-age"
      >
        {{ $t('dcctl.operations.loadedAgo', { age: ageText(age) }) }}
      </span>
      <wt-button
        :disabled="refreshing"
        color="secondary"
        data-test="ops-refresh"
        @click="refresh"
      >
        {{ $t('dcctl.operations.refresh') }}
      </wt-button>
    </div>

    <template v-if="phase === 'ready'">
      <wt-message
        v-if="error"
        color="error"
        data-test="ops-refresh-error"
      >
        <strong>{{ $t('dcctl.operations.unavailable') }}.</strong>
        {{ apiErrorMessage(error) }}
      </wt-message>
      <p
        v-if="!operations.length"
        class="typo-body-1"
        data-test="ops-empty"
      >
        {{ $t('dcctl.operations.empty') }}
      </p>
      <wt-table
        v-else
        :headers="headers"
        :data="rows"
        data-key="id"
        :sortable="false"
        data-test="ops-table"
      >
        <template #type="{ item }">
          <router-link
            :to="{ name: 'operation', params: { id: item.id } }"
            class="typo-body-1-bold"
            data-test="ops-link"
          >
            {{ enumLabel('operationType', item.type) }}
          </router-link>
        </template>
        <template #state="{ item }">
          <wt-chip :color="toChipColor(operationStateColor(item.state))">
            {{ enumLabel('operationState', item.state) }}
          </wt-chip>
        </template>
        <template #mode="{ item }">
          <wt-chip
            v-if="item.mode"
            :color="toChipColor(modeColor(item.mode))"
          >
            {{ enumLabel('operationMode', item.mode) }}
          </wt-chip>
          <template v-else>{{ $t('dcctl.operations.noValue') }}</template>
        </template>
      </wt-table>
      <wt-message
        v-if="loadMoreError"
        color="error"
        data-test="ops-load-more-error"
      >
        <strong>{{ $t('dcctl.operations.loadMoreFailed') }}.</strong>
        {{ apiErrorMessage(loadMoreError) }}
      </wt-message>
      <div v-if="hasMore">
        <wt-button
          :loading="loadingMore"
          :disabled="loadingMore || refreshing"
          color="secondary"
          data-test="ops-load-more"
          @click="loadMore"
        >
          {{ $t('dcctl.operations.loadMore') }}
        </wt-button>
      </div>
    </template>

    <div
      v-else-if="phase === 'error'"
      class="typo-body-1"
      role="alert"
      data-test="ops-error"
    >
      {{ $t('dcctl.operations.unavailable') }}: {{ apiErrorMessage(error) }}
      <wt-button
        color="secondary"
        data-test="ops-retry"
        @click="refresh"
      >
        {{ $t('dcctl.operations.retry') }}
      </wt-button>
    </div>
    <p
      v-else
      class="typo-body-1"
      data-test="ops-loading"
    >
      {{ $t('dcctl.operations.loading') }}
    </p>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';

import { apiErrorMessage } from '@/api/client/apiError';
import { useAgeText } from '@/composables/useAgeText';
import { useEnumLabel } from '@/composables/useEnumLabel';
import { useOperationsList } from '@/composables/useOperationsList';
import { useTimeText } from '@/composables/useTimeText';
import { resourceId } from '@/utils/datacenterSummary';
import { durationMs } from '@/utils/operationView';
import {
	modeColor,
	operationStateColor,
	toChipColor,
} from '@/utils/statusMapping';

const { t } = useI18n();
const enumLabel = useEnumLabel();
const timeText = useTimeText();
const ageText = useAgeText();
const {
	operations,
	phase,
	error,
	loadMoreError,
	loadingMore,
	refreshing,
	hasMore,
	age,
	now,
	refresh,
	loadMore,
} = useOperationsList();

const headers = computed(() =>
	(
		[
			'type',
			'target',
			'state',
			'mode',
			'initiator',
			'started',
			'duration',
		] as const
	).map((key) => ({
		value: key,
		text: t(`dcctl.operations.columns.${key}`),
	})),
);

const noValue = computed(() => t('dcctl.operations.noValue'));

const rows = computed(() =>
	operations.value.map((operation) => {
		const duration = durationMs(
			operation.create_time,
			operation.end_time,
			now.value,
		);
		return {
			id: resourceId(operation.name),
			type: operation.type,
			target: resourceId(operation.target) || noValue.value,
			state: operation.state,
			mode: operation.mode,
			initiator: operation.initiator?.user || noValue.value,
			started: operation.create_time
				? timeText(operation.create_time)
				: noValue.value,
			duration:
				duration === null
					? noValue.value
					: ageText(Math.floor(duration / 1000)),
		};
	}),
);
</script>

<style scoped>
.ops-page {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-md);
  padding: var(--spacing-md);
}

.ops-page__title {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--spacing-md);
}
</style>
