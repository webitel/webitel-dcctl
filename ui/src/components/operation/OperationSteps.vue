<template>
  <section
    class="op-steps"
    :class="{ 'op-steps--stale': stale }"
    data-test="op-steps"
  >
    <h2 class="typo-heading-4">{{ $t('dcctl.operation.steps.title') }}</h2>
    <p
      v-if="!rows.length"
      class="typo-body-1"
      data-test="op-steps-empty"
    >
      {{ $t('dcctl.operation.steps.empty') }}
    </p>
    <wt-table
      v-else
      :headers="headers"
      :data="rows"
      :row-class="rowClass"
      :row-expansion="true"
      :row-expansion-disabled="isNotStage"
      :sortable="false"
    >
      <template #step="{ item }">
        <div
          v-if="item.pnr !== 'none'"
          class="op-pnr"
          data-test="pnr-line"
        >
          <span class="op-pnr__label typo-body-2-bold">
            {{ $t('dcctl.operation.steps.pointOfNoReturn') }}
            <template v-if="item.pnr === 'ahead'">
              — {{ $t('dcctl.operation.steps.pointAhead') }}
            </template>
            <template v-else-if="item.pnr === 'running'">
              — {{ $t('dcctl.operation.steps.pointRunning') }}
            </template>
            <template v-else-if="item.pnr === 'passed'">
              — {{ $t('dcctl.operation.steps.pointPassed') }}
            </template>
          </span>
        </div>
        <span class="op-step__name typo-body-1-bold">{{ enumLabel('stepType', item.type) }}</span>
        <span
          v-if="item.where"
          class="typo-body-2"
          data-test="step-where"
        >
          {{ item.where }}
        </span>
      </template>
      <template #state="{ item }">
        <span class="op-step__state">
          <wt-chip
            :color="toChipColor(item.color)"
            data-test="step-state"
          >
            {{ enumLabel('stepState', item.state) }}
          </wt-chip>
          <span
            v-if="item.lastKnown"
            class="typo-body-2"
            data-test="step-last-known"
          >
            {{ $t('dcctl.operation.steps.lastKnown') }}
          </span>
        </span>
      </template>
      <template #detail="{ item }">
        <span class="op-step__detail">
          <span v-if="item.detail">{{ item.detail }}</span>
          <ul
            v-if="item.warnings.length"
            class="op-step__warnings"
            data-test="step-warnings"
          >
            <li
              v-for="warning in item.warnings"
              :key="warning"
            >
              {{ warning }}
            </li>
          </ul>
        </span>
      </template>
      <template #expansion="{ item }">
        <stage-jobs
          v-if="item.stageRef"
          :datacenter="item.stageRef.datacenter"
          :stage="item.stageRef.stage"
          :expect-running="item.type === 'APPS_START'"
        />
      </template>
    </wt-table>
  </section>
</template>

<script setup lang="ts">
import { computed, reactive, shallowRef, watchEffect } from 'vue';
import { useI18n } from 'vue-i18n';

import type { WebitelDcctlV1Operation as Operation } from '@/api/gen/_models';
import { useAgeText } from '@/composables/useAgeText';
import { useEnumLabel } from '@/composables/useEnumLabel';
import {
	durationMs,
	type OrderedStep,
	orderedSteps,
	pointOfNoReturn,
	stageOfStep,
} from '@/utils/operationView';
import {
	type StatusColor,
	stepColor,
	toChipColor,
} from '@/utils/statusMapping';
import StageJobs from './StageJobs.vue';

const props = defineProps<{
	operation: Operation;
	/** Time of the data in server milliseconds: now while it is live, the last message while it is not. */
	nowMs: number;
	/** The live feed is gone: a step that ran may be over, and no step is shown as current. */
	stale: boolean;
}>();

const { t } = useI18n();
const enumLabel = useEnumLabel();
const ageText = useAgeText();

const headers = computed(() => [
	{ value: 'number', text: t('dcctl.operation.steps.columns.number') },
	{ value: 'step', text: t('dcctl.operation.steps.columns.step') },
	{ value: 'state', text: t('dcctl.operation.steps.columns.state') },
	{ value: 'duration', text: t('dcctl.operation.steps.columns.duration') },
	{ value: 'detail', text: t('dcctl.operation.steps.columns.detail') },
]);

const buildRow = (step: OrderedStep) => {
	const stageRef = stageOfStep(step);
	// A step still marked running under an operation that does not run is not
	// running: show it as last known and stop its clock at the last change.
	const idleRunning =
		step.state === 'RUNNING' &&
		(props.stale || props.operation.state !== 'RUNNING');
	const duration = durationMs(
		step.start_time,
		step.end_time ??
			(idleRunning && props.operation.state !== 'RUNNING'
				? props.operation.update_time
				: undefined),
		props.nowMs,
	);
	const where = [
		step.datacenter?.split('/').pop(),
		stageRef && t('dcctl.operation.steps.stage', { n: stageRef.stage }),
	]
		.filter(Boolean)
		.join(' · ');
	return {
		// wt-table tells expanded rows apart by `id`.
		id: step.index,
		index: step.index,
		number: step.index + 1,
		type: step.type,
		where,
		state: step.state,
		color: (idleRunning ? 'gray' : stepColor(step)) as StatusColor,
		lastKnown: idleRunning,
		duration:
			duration === null
				? t('dcctl.operations.noValue')
				: ageText(Math.floor(duration / 1000)),
		detail: step.detail,
		warnings: step.warnings ?? [],
		pnr: pointOfNoReturn(step, props.operation.state),
		stageRef,
	};
};

/*
 * wt-table keeps the expanded rows as the row objects themselves and drops the
 * expansion of a row once the table gets another object for it, so the jobs
 * of a stage would close at every message of the stream. One reactive object
 * per step, updated in place, keeps them open.
 */
const rowObjects = new Map<number, ReturnType<typeof buildRow>>();
const rows = shallowRef<ReturnType<typeof buildRow>[]>([]);
watchEffect(() => {
	const current = orderedSteps(props.operation).map((step) => {
		const fresh = buildRow(step);
		const known = rowObjects.get(step.index);
		if (!known) {
			const created = reactive(fresh);
			rowObjects.set(step.index, created);
			return created;
		}
		Object.assign(known, fresh);
		return known;
	});
	for (const key of rowObjects.keys()) {
		if (!current.some((row) => row.index === key)) rowObjects.delete(key);
	}
	rows.value = current;
});

const isNotStage = (row: object) => !(row as { stageRef: unknown }).stageRef;

const rowClass = (row: { pnr?: string }) =>
	row.pnr && row.pnr !== 'none' ? 'op-step op-step--pnr' : 'op-step';
</script>

<style scoped>
.op-steps {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-xs);
}

/* A step list that may be out of date is grey, like the age of old data elsewhere. */
.op-steps--stale {
  opacity: 0.7;
}

.op-pnr {
  display: block;
  margin-bottom: var(--spacing-xs);
  padding-top: var(--spacing-xs);
  border-top: 2px solid var(--error-color);
  color: var(--error-color);
}

.op-step__name {
  margin-right: var(--spacing-xs);
}

.op-step__state {
  display: inline-flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--spacing-xs);
}

.op-step__detail {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-xs);
}

.op-step__warnings {
  margin: 0;
  padding-left: var(--spacing-md);
  color: var(--warning-color);
}
</style>
