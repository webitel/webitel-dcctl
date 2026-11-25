<template>
  <div
    class="journal-detail"
    data-test="journal-detail"
  >
    <dl class="journal-detail__facts typo-body-2">
      <dt>{{ $t('dcctl.journal.entry.name') }}</dt>
      <dd>{{ entry.name }}</dd>
      <template v-if="entry.initiator?.user">
        <dt>{{ $t('dcctl.journal.entry.user') }}</dt>
        <dd>{{ entry.initiator.user }}</dd>
      </template>
      <template v-if="entry.initiator?.address">
        <dt>{{ $t('dcctl.journal.entry.address') }}</dt>
        <dd>{{ entry.initiator.address }}</dd>
      </template>
      <template v-if="instance">
        <dt>{{ $t('dcctl.journal.entry.instance') }}</dt>
        <dd>{{ instance }}</dd>
      </template>
    </dl>

    <section v-if="facts.params.length">
      <h3 class="typo-body-2-bold">{{ $t('dcctl.journal.entry.params') }}</h3>
      <dl class="journal-detail__facts typo-body-2">
        <template
          v-for="[key, value] in facts.params"
          :key="key"
        >
          <dt>{{ key }}</dt>
          <dd :data-test="`fact-${key}`">{{ value }}</dd>
        </template>
      </dl>
    </section>

    <section v-if="facts.confirmation.length">
      <h3 class="typo-body-2-bold">{{ $t('dcctl.journal.entry.confirmation') }}</h3>
      <dl class="journal-detail__facts typo-body-2">
        <template
          v-for="[key, value] in facts.confirmation"
          :key="key"
        >
          <dt>{{ key }}</dt>
          <dd :data-test="`fact-${key}`">{{ value }}</dd>
        </template>
      </dl>
    </section>

    <section
      v-if="facts.error"
      data-test="journal-detail-error"
    >
      <h3 class="typo-body-2-bold">{{ $t('dcctl.journal.entry.error') }}</h3>
      <dl class="journal-detail__facts typo-body-2">
        <template v-if="facts.error.id">
          <dt>{{ $t('dcctl.journal.entry.errorId') }}</dt>
          <dd>{{ facts.error.id }}</dd>
        </template>
        <template v-if="facts.error.reason">
          <dt>{{ $t('dcctl.journal.entry.reason') }}</dt>
          <dd>{{ facts.error.reason }}</dd>
        </template>
        <template v-if="facts.error.detail">
          <dt>{{ $t('dcctl.journal.entry.detail') }}</dt>
          <dd>{{ facts.error.detail }}</dd>
        </template>
        <template
          v-for="[key, value] in facts.error.metadata"
          :key="key"
        >
          <dt>{{ key }}</dt>
          <dd>{{ value }}</dd>
        </template>
      </dl>
    </section>

    <p
      v-if="!hasFacts"
      class="typo-body-2"
    >
      {{ $t('dcctl.journal.entry.none') }}
    </p>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue';

import type { WebitelDcctlV1JournalEntry as Entry } from '@/api/gen/_models';
import { entryFacts } from '@/utils/journalView';

const props = defineProps<{ entry: Entry }>();

const facts = computed(() => entryFacts(props.entry));
const hasFacts = computed(
	() =>
		facts.value.params.length > 0 ||
		facts.value.confirmation.length > 0 ||
		facts.value.error !== undefined,
);
/** `dcctl-a1 (dc_a, 10.1.0.81:10023)`. */
const instance = computed(() => {
	const found = props.entry.initiator?.instance;
	if (!found?.node) return '';
	const where = [found.datacenter?.split('/').pop(), found.address]
		.filter(Boolean)
		.join(', ');
	return where ? `${found.node} (${where})` : found.node;
});
</script>

<style scoped>
.journal-detail {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-sm);
  padding: var(--spacing-sm) var(--spacing-md);
}

.journal-detail__facts {
  display: grid;
  grid-template-columns: max-content 1fr;
  gap: var(--spacing-2xs) var(--spacing-md);
  margin: 0;
}

.journal-detail__facts dt {
  font-family: monospace;
}

.journal-detail__facts dd {
  margin: 0;
  overflow-wrap: anywhere;
  white-space: pre-wrap;
}
</style>
