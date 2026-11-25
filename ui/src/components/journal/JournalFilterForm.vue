<template>
  <form
    class="journal-filter"
    data-test="journal-filter-form"
    @submit.prevent
    @keydown.enter="applyOnEnter"
  >
    <template v-if="field === 'type'">
      <wt-checkbox
        v-for="type in JOURNAL_TYPES"
        :key="type"
        v-model:selected="types"
        :value="type"
        :label="enumLabel('journalType', type)"
        :data-test="`filter-type-${type}`"
      />
    </template>

    <template v-else-if="field === 'datacenter'">
      <wt-single-select
        v-if="options.length"
        v-model="datacenter"
        :options="options"
        option-value="id"
        option-label="label"
        :label="$t('dcctl.journal.filter.datacenter')"
        data-test="filter-datacenter"
      />
      <template v-else>
        <p class="typo-body-2">{{ $t('dcctl.journal.filter.datacenterNone') }}</p>
        <wt-input-text
          v-model="datacenter"
          :label="$t('dcctl.journal.filter.datacenter')"
          data-test="filter-datacenter-text"
        />
      </template>
    </template>

    <wt-input-text
      v-else
      v-model="operation"
      :label="$t('dcctl.journal.filter.operationHint')"
      data-test="filter-operation"
    />

    <div class="journal-filter__actions">
      <wt-button
        data-test="filter-apply"
        @click="apply"
      >
        {{ $t('dcctl.journal.filter.apply') }}
      </wt-button>
      <wt-button
        color="secondary"
        data-test="filter-reset"
        @click="reset"
      >
        {{ $t('dcctl.journal.filter.reset') }}
      </wt-button>
    </div>
  </form>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue';

import type { WebitelDcctlV1JournalEntryType as EntryType } from '@/api/gen/_models';
import { useEnumLabel } from '@/composables/useEnumLabel';
import {
	JOURNAL_TYPES,
	type JournalFilter,
	resourceNameOf,
} from '@/utils/journalFilter';

const props = defineProps<{
	/** The column the form belongs to: it edits only this field. */
	field: 'type' | 'datacenter' | 'operation';
	/** The filter that is applied now. */
	filter: JournalFilter;
	/** Names of the datacenters the status knows, `datacenters/dc_b`. */
	datacenters: readonly string[];
}>();

const emit = defineEmits<{ apply: [filter: JournalFilter] }>();

const enumLabel = useEnumLabel();

const types = ref<EntryType[]>([...props.filter.types]);
const datacenter = ref(props.filter.datacenter);
const operation = ref(props.filter.operation);

/** The applied datacenter stays a choice even when the status no longer lists it, typed as `dc_b` or in full. */
const options = computed(() => {
	const names = new Set(props.datacenters);
	const applied = resourceNameOf('datacenters', props.filter.datacenter);
	if (applied) names.add(applied);
	return props.datacenters.length
		? [...names].map((id) => ({ id, label: id.split('/').pop() ?? id }))
		: [];
});

/** The draft of this column's field only: the other columns keep what is applied. */
const draft = (): Partial<JournalFilter> => {
	switch (props.field) {
		case 'type':
			// The order of the entry types, not of the clicks: one choice is one filter.
			return {
				types: JOURNAL_TYPES.filter((type) => types.value.includes(type)),
			};
		case 'datacenter':
			return { datacenter: datacenter.value ?? '' };
		default:
			return { operation: operation.value };
	}
};

const apply = () => emit('apply', { ...props.filter, ...draft() });

/** Enter in a text field applies; on a button it is that button's own click. */
const applyOnEnter = (event: KeyboardEvent) => {
	if (!(event.target instanceof HTMLInputElement)) return;
	event.preventDefault();
	apply();
};

const reset = () => {
	types.value = [];
	datacenter.value = '';
	operation.value = '';
	apply();
};
</script>

<style scoped>
.journal-filter {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-sm);
  min-width: 14rem;
  padding: var(--spacing-sm);
}

.journal-filter__actions {
  display: flex;
  gap: var(--spacing-sm);
}
</style>
