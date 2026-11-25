<template>
  <div class="journal-page">
    <div class="journal-page__title">
      <h1 class="typo-heading-2">{{ $t('dcctl.journal.title') }}</h1>
      <wt-button
        :disabled="refreshing"
        color="secondary"
        data-test="journal-refresh"
        @click="refresh"
      >
        {{ $t('dcctl.journal.refresh') }}
      </wt-button>
    </div>

    <div
      v-if="filterText"
      class="journal-page__filter typo-body-1"
      data-test="journal-active-filter"
    >
      <span>{{ $t('dcctl.journal.activeFilter', { filter: filterText }) }}</span>
      <wt-button
        color="secondary"
        data-test="journal-clear-filter"
        @click="clearFilter"
      >
        {{ $t('dcctl.journal.clearFilter') }}
      </wt-button>
    </div>

    <wt-message
      v-if="phase === 'error' && filterRejected"
      color="error"
      data-test="journal-filter-rejected"
    >
      <strong>{{ $t('dcctl.journal.filterRejected') }}.</strong>
      <template v-if="violations.length">
        <div
          v-for="violation in violations"
          :key="violation"
        >
          {{ violation }}
        </div>
      </template>
      <template v-else>{{ apiErrorMessage(error) }}</template>
    </wt-message>

    <div
      v-else-if="phase === 'error'"
      class="typo-body-1"
      role="alert"
      data-test="journal-error"
    >
      {{ $t('dcctl.journal.unavailable') }}: {{ apiErrorMessage(error) }}
      <wt-button
        color="secondary"
        data-test="journal-retry"
        @click="refresh"
      >
        {{ $t('dcctl.journal.retry') }}
      </wt-button>
    </div>

    <p
      v-else-if="phase === 'loading'"
      class="typo-body-1"
      data-test="journal-loading"
    >
      {{ $t('dcctl.journal.loading') }}
    </p>

    <template v-else>
      <wt-message
        v-if="error"
        color="error"
        data-test="journal-refresh-error"
      >
        <strong>{{ $t('dcctl.journal.unavailable') }}.</strong>
        {{ apiErrorMessage(error) }}
      </wt-message>
      <p
        class="typo-body-2"
        data-test="journal-incomplete"
      >
        {{ $t('dcctl.journal.incompleteNote') }}
      </p>
      <wt-table
        :headers="headers"
        :data="rows"
        :sortable="false"
        :row-expansion="true"
        data-test="journal-table"
      >
        <template #empty>
          <p
            class="typo-body-1"
            data-test="journal-empty"
          >
            <template v-if="filterText">
              {{ $t('dcctl.journal.emptyFiltered', { filter: filterText }) }}
            </template>
            <template v-else>{{ $t('dcctl.journal.empty') }}</template>
          </p>
        </template>
        <template #column-filter="{ header, formView, hide }">
          <journal-filter-form
            v-if="formView"
            :field="header.value"
            :filter="filter"
            :datacenters="datacenterNames"
            @apply="applyFrom($event, hide)"
          />
          <span
            v-else
            class="typo-body-2"
          >
            {{ filterPreview(header.value) }}
          </span>
        </template>
        <template #time="{ item }">
          <span
            class="journal-time"
            data-test="journal-time"
          >
            <span>{{ item.timeText }}</span>
            <span class="typo-body-2">{{ agoOf(item.time) }}</span>
          </span>
        </template>
        <template #type="{ item }">
          {{ enumLabel('journalType', item.type) }}
        </template>
        <template #operation="{ item }">
          <router-link
            v-if="item.operationId"
            :to="{ name: 'operation', params: { id: item.operationId } }"
            data-test="journal-operation-link"
          >
            {{ item.operationId }}
          </router-link>
          <template v-else>{{ noValue }}</template>
        </template>
        <template #initiator="{ item }">
          <span class="journal-initiator">
            <span>{{ item.initiatorText }}</span>
            <span
              v-if="item.initiator.node && item.initiator.kind !== 'instance'"
              class="typo-body-2"
            >
              {{ item.initiator.node }}
            </span>
          </span>
        </template>
        <template #action="{ item }">
          <span class="typo-body-1-bold">
            {{ enumLabel(item.actionGroup, item.action) }}
          </span>
        </template>
        <template #result="{ item }">
          <wt-chip :color="toChipColor(item.color)">
            {{ enumLabel(item.resultGroup, item.result) }}
          </wt-chip>
        </template>
        <template #expansion="{ item }">
          <journal-entry-detail :entry="item.entry" />
        </template>
      </wt-table>
      <wt-message
        v-if="loadMoreError"
        color="error"
        data-test="journal-load-more-error"
      >
        <strong>{{ $t('dcctl.journal.loadMoreFailed') }}.</strong>
        {{ apiErrorMessage(loadMoreError) }}
      </wt-message>
      <div v-if="hasMore">
        <wt-button
          :loading="loadingMore"
          :disabled="loadingMore || refreshing"
          color="secondary"
          data-test="journal-load-more"
          @click="loadMore"
        >
          {{ $t('dcctl.journal.loadMore') }}
        </wt-button>
      </div>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';

import { apiErrorMessage, getApiError } from '@/api/client/apiError';
import JournalEntryDetail from '@/components/journal/JournalEntryDetail.vue';
import JournalFilterForm from '@/components/journal/JournalFilterForm.vue';
import { useAgeText } from '@/composables/useAgeText';
import { useEnumLabel } from '@/composables/useEnumLabel';
import { useJournal } from '@/composables/useJournal';
import { useTimeText } from '@/composables/useTimeText';
import { useStatusStore } from '@/stores/status';
import {
	emptyJournalFilter,
	type JournalFilter,
	resourceNameOf,
} from '@/utils/journalFilter';
import { journalRow } from '@/utils/journalView';
import { toChipColor } from '@/utils/statusMapping';

const { t } = useI18n();
const enumLabel = useEnumLabel();
const timeText = useTimeText();
const ageText = useAgeText();
const {
	entries,
	phase,
	filter,
	filterText,
	error,
	filterRejected,
	loadMoreError,
	loadingMore,
	refreshing,
	hasMore,
	now,
	refresh,
	applyFilter,
	loadMore,
} = useJournal();

const statusStore = useStatusStore();
const datacenterNames = computed(() =>
	(statusStore.datacenters ?? []).flatMap((dc) => (dc.name ? [dc.name] : [])),
);

const noValue = computed(() => t('dcctl.journal.noValue'));

const violations = computed(() =>
	(getApiError(error.value)?.violations ?? []).flatMap((violation) =>
		violation.description
			? [
					`${violation.subject ?? ''}: ${violation.description}`.replace(
						/^: /,
						'',
					),
				]
			: [],
	),
);

const clearFilter = () => applyFilter(emptyJournalFilter);
const applyFrom = (next: JournalFilter, hide: () => void) => {
	hide();
	void applyFilter(next);
};

const filterPreview = (field: string): string => {
	switch (field) {
		case 'type':
			return filter.value.types
				.map((type) => enumLabel('journalType', type))
				.join(', ');
		case 'datacenter':
			return resourceNameOf('datacenters', filter.value.datacenter);
		default:
			return resourceNameOf('operations', filter.value.operation);
	}
};

const headers = computed(() => [
	{ value: 'time', text: t('dcctl.journal.columns.time') },
	{
		value: 'type',
		text: t('dcctl.journal.columns.type'),
		filter: 'type',
		filtered: filter.value.types.length > 0,
	},
	{ value: 'initiator', text: t('dcctl.journal.columns.initiator') },
	{
		value: 'datacenter',
		text: t('dcctl.journal.columns.datacenter'),
		filter: 'datacenter',
		filtered: filter.value.datacenter.trim() !== '',
	},
	{
		value: 'operation',
		text: t('dcctl.journal.columns.operation'),
		filter: 'operation',
		filtered: filter.value.operation.trim() !== '',
	},
	{ value: 'action', text: t('dcctl.journal.columns.action') },
	{ value: 'result', text: t('dcctl.journal.columns.result') },
	{ value: 'detail', text: t('dcctl.journal.columns.detail') },
]);

const agoOf = (time: string | undefined): string => {
	const milliseconds = time ? Date.parse(time) : Number.NaN;
	if (Number.isNaN(milliseconds)) return '';
	const seconds = Math.max(0, Math.floor((now.value - milliseconds) / 1000));
	return t('dcctl.journal.ago', { age: ageText(seconds) });
};

const rows = computed(() =>
	entries.value.map((entry) => {
		const row = journalRow(entry);
		return {
			...row,
			entry,
			datacenter: row.datacenter || noValue.value,
			timeText: row.time ? timeText(row.time) : noValue.value,
			initiatorText:
				row.initiator.kind === 'system'
					? t('dcctl.journal.system')
					: row.initiator.text || noValue.value,
		};
	}),
);
</script>

<style scoped>
.journal-page {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-md);
  padding: var(--spacing-md);
}

.journal-page__title,
.journal-page__filter {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--spacing-md);
}

.journal-page__filter span {
  overflow-wrap: anywhere;
}

.journal-time,
.journal-initiator {
  display: flex;
  flex-direction: column;
}
</style>
