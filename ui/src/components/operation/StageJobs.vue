<template>
  <div
    class="stage-jobs"
    data-test="stage-jobs"
  >
    <p
      v-if="notFound"
      class="typo-body-1"
      data-test="stage-jobs-not-found"
    >
      {{ $t('dcctl.operation.jobs.notFound', { name: datacenter }) }}
    </p>
    <template v-else-if="current">
      <wt-message
        v-if="error"
        color="warn"
        data-test="stage-jobs-error"
      >
        {{ $t('dcctl.operation.jobs.unavailable') }}: {{ apiErrorMessage(error) }}
        <template v-if="age !== null">
          {{ $t('dcctl.operation.jobs.age', { age: ageText(age) }) }}
        </template>
      </wt-message>
      <p class="typo-body-2">{{ $t('dcctl.operation.jobs.hint') }}</p>
      <p
        v-if="!stageData"
        class="typo-body-1"
        data-test="stage-jobs-empty"
      >
        {{ $t('dcctl.operation.jobs.noStage', { n: stage }) }}
      </p>
      <stage-jobs-table
        v-else
        :stage="stageData"
        :expect-running="expectRunning"
      />
    </template>
    <p
      v-else-if="error"
      class="typo-body-1"
      role="alert"
      data-test="stage-jobs-unavailable"
    >
      {{ $t('dcctl.operation.jobs.unavailable') }}: {{ apiErrorMessage(error) }}
    </p>
    <p
      v-else
      class="typo-body-1"
      data-test="stage-jobs-loading"
    >
      {{ $t('dcctl.operation.jobs.loading') }}
    </p>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue';

import { apiErrorMessage } from '@/api/client/apiError';
import StageJobsTable from '@/components/datacenter/StageJobsTable.vue';
import { useAgeText } from '@/composables/useAgeText';
import { useDatacenter } from '@/composables/useDatacenter';

const props = defineProps<{
	/** Datacenter id, `dc_a`. */
	datacenter: string;
	/** Stage number; the gateway omits 0, the caller passes 0 then. */
	stage: number;
	/** `true` for a step that starts apps, `false` for one that stops them. */
	expectRunning: boolean;
}>();

const ageText = useAgeText();
const {
	datacenter: current,
	error,
	notFound,
	age,
} = useDatacenter(() => props.datacenter);

// A stage with no field is stage 0.
const stageData = computed(() =>
	current.value?.nomad?.stages?.find(
		(item) => (item.stage ?? 0) === props.stage,
	),
);
</script>

<style scoped>
.stage-jobs {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-xs);
  padding: var(--spacing-xs) var(--spacing-md);
}
</style>
