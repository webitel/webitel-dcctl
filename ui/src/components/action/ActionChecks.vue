<template>
  <div class="action-checks">
    <ul
      v-if="shown.length"
      class="action-checks__list"
    >
      <li
        v-for="check in shown"
        :key="check.id"
        class="action-checks__item"
        :data-test="`check-${check.id}`"
      >
        <wt-chip :color="toChipColor(checkColor(check.result))">
          {{ enumLabel('checkResult', check.result) }}
        </wt-chip>
        <span class="typo-body-1-bold">{{ check.id }}</span>
        <wt-chip
          v-if="check.result === 'FAIL' || check.result === 'WARN'"
          :color="toChipColor('gray')"
        >
          {{ enumLabel('checkEffect', check.effect) }}
        </wt-chip>
        <span class="typo-body-1 action-checks__detail">{{ check.detail }}</span>
        <details
          v-if="hasFacts(check)"
          class="typo-body-2 action-checks__facts"
        >
          <summary>{{ $t('dcctl.action.dialog.facts') }}</summary>
          <pre>{{ JSON.stringify(check.facts, null, 2) }}</pre>
        </details>
      </li>
    </ul>
    <details
      v-if="groups.rest.length && !showAll"
      class="typo-body-1"
      data-test="checks-rest"
    >
      <summary>{{ $t('dcctl.action.dialog.passedChecks', { n: groups.rest.length }) }}</summary>
      <action-checks
        :checks="groups.rest"
        show-all
      />
    </details>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue';

import type { WebitelDcctlV1Check as Check } from '@/api/gen/_models';
import { useEnumLabel } from '@/composables/useEnumLabel';
import { groupChecks } from '@/utils/actionPreview';
import { checkColor, toChipColor } from '@/utils/statusMapping';

const props = defineProps<{
	checks: readonly Check[];
	/** List every check, not only failures and warnings, with no fold. */
	showAll?: boolean;
}>();

const enumLabel = useEnumLabel();

const groups = computed(() => groupChecks(props.checks));
const shown = computed(() =>
	props.showAll
		? [...props.checks]
		: [...groups.value.fail, ...groups.value.warn],
);

const hasFacts = (check: Check): boolean =>
	!!check.facts && Object.keys(check.facts).length > 0;
</script>

<style scoped>
.action-checks {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-xs);
}

.action-checks__list {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-xs);
  margin: 0;
  padding: 0;
  list-style: none;
}

.action-checks__item {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--spacing-xs);
}

.action-checks__detail {
  flex: 1 1 100%;
}

.action-checks__facts pre {
  margin: var(--spacing-2xs) 0 0;
  white-space: pre-wrap;
}
</style>
