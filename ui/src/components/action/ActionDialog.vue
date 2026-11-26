<template>
  <wt-popup
    size="md"
    :closable="phase !== 'submitting'"
    data-test="action-dialog"
    @close="emit('close')"
  >
    <template #title>
      <span data-test="action-title">{{ title }}</span>
    </template>

    <template #main>
      <div class="action-dialog">
        <section
          v-if="kind === 'resolve'"
          class="action-dialog__section"
          data-test="action-winner"
        >
          <p class="typo-body-1">{{ $t('dcctl.action.dialog.chooseWinner') }}</p>
          <p
            v-if="!candidates?.length"
            class="typo-body-1"
          >
            {{ $t('dcctl.action.dialog.winnerNone') }}
          </p>
          <label
            v-for="candidate in candidates"
            :key="candidate.id"
            class="action-dialog__winner typo-body-1"
          >
            <input
              type="radio"
              name="winner"
              :value="candidate.id"
              :checked="winner === candidate.id"
              :disabled="phase === 'checking' || phase === 'submitting'"
              :data-test="`winner-${candidate.id}`"
              @change="chooseWinner(candidate.id)"
            >
            <strong>{{ candidate.id }}</strong>
            <span>
              {{ candidate.role
                ? $t('dcctl.action.dialog.winnerRole', { role: enumLabel('patroniRole', candidate.role) })
                : $t('dcctl.action.dialog.winnerRoleUnknown') }}
            </span>
          </label>
        </section>

        <p
          v-if="phase === 'checking'"
          class="typo-body-1"
          data-test="action-checking"
        >
          {{ $t('dcctl.action.dialog.checking') }}
        </p>

        <wt-message
          v-if="isAbandon"
          :color="abandonLevel === 'ahead' ? 'info' : 'error'"
          data-test="action-abandon-warning"
          :data-level="abandonLevel"
        >
          {{ $t(`dcctl.action.dialog.abandon.${abandonLevel}`) }}
        </wt-message>

        <wt-message
          v-if="refusal"
          color="error"
          data-test="action-refusal"
        >
          <strong>{{ refusalText }}</strong>
          <template v-if="refusal.api">
            <div
              v-for="(violation, index) in violations"
              :key="index"
              class="action-dialog__violation"
              :data-test="`violation-${violation.type}`"
            >
              <template v-if="violation.subject">{{ violation.subject }}: </template>{{ violation.text }}
            </div>
            <div
              v-if="refusal.api.detail && !violations.length"
              class="typo-body-2"
            >
              {{ refusal.api.detail }}
            </div>
            <div
              v-if="metadata.length"
              class="typo-body-2"
              data-test="refusal-metadata"
            >
              <span
                v-for="[name, value] in metadata"
                :key="name"
                class="action-dialog__meta"
              >{{ metaLabel(name) }}: {{ value }}</span>
            </div>
            <router-link
              v-if="runningOperation"
              :to="{ name: 'operation', params: { id: runningOperation } }"
              class="typo-body-1"
              data-test="refusal-operation"
              @click="emit('close')"
            >
              {{ $t('dcctl.action.dialog.operationOpen') }}
            </router-link>
            <div
              v-if="refusal.api.id"
              class="typo-body-2"
            >
              {{ refusal.api.id }}
            </div>
          </template>
        </wt-message>

        <template v-if="preview && phase !== 'choosing'">
          <wt-message
            v-if="outcome === 'ok'"
            color="success"
            data-test="action-outcome-ok"
          >
            {{ $t('dcctl.action.dialog.canRun') }}
          </wt-message>
          <wt-message
            v-else-if="outcome === 'confirm'"
            color="warn"
            data-test="action-outcome-confirm"
          >
            {{ $t('dcctl.action.dialog.canRunAfter') }}
          </wt-message>
          <wt-message
            v-else-if="outcome === 'blocked'"
            color="error"
            data-test="action-outcome-blocked"
          >
            {{ $t('dcctl.action.dialog.blocked') }}
          </wt-message>

          <wt-message
            v-if="blockers.length && outcome === 'confirm'"
            color="error"
            data-test="action-cannot-confirm"
          >
            {{ $t('dcctl.action.dialog.cannotConfirm') }}
          </wt-message>

          <wt-message
            v-if="factsChanged"
            color="warn"
            data-test="action-facts-changed"
          >
            {{ $t('dcctl.action.dialog.factsChanged') }}
          </wt-message>

          <section
            v-if="preview.checks.length"
            class="action-dialog__section"
          >
            <h4 class="typo-subtitle-1">{{ $t('dcctl.action.dialog.checks') }}</h4>
            <action-checks :checks="preview.checks" />
          </section>

          <section
            v-if="needs.length"
            class="action-dialog__section"
            data-test="action-confirmations"
          >
            <h4 class="typo-subtitle-1">{{ $t('dcctl.action.dialog.confirmations') }}</h4>
            <div
              v-for="need in needs"
              :key="need.field"
              class="action-dialog__need"
            >
              <wt-checkbox
                :selected="accepted.includes(need.field)"
                :label="confirmLabel(need)"
                :disabled="phase === 'submitting'"
                :data-test="`confirm-${need.field}`"
                @update:selected="setAccepted(need.field, $event === true)"
              />
              <action-checks
                :checks="need.checks"
                show-all
              />
            </div>
          </section>

          <section
            v-else-if="pendingConfirmations.length"
            class="action-dialog__section"
            data-test="action-pending"
          >
            <h4 class="typo-subtitle-1">{{ $t('dcctl.action.dialog.confirmations') }}</h4>
            <p class="typo-body-2">{{ $t('dcctl.action.dialog.pending') }}</p>
            <ul class="action-dialog__plain typo-body-1">
              <li
                v-for="need in pendingConfirmations"
                :key="need.field"
              >
                {{ confirmLabel(need) }}
              </li>
            </ul>
          </section>

          <section
            v-if="preview.mode"
            class="action-dialog__section"
          >
            <h4 class="typo-subtitle-1">{{ $t('dcctl.action.dialog.mode') }}</h4>
            <wt-chip
              :color="toChipColor(modeColor(preview.mode))"
              data-test="action-mode"
            >
              {{ enumLabel('operationMode', preview.mode) }}
            </wt-chip>
            <p
              v-if="preview.mode === 'FORCED'"
              class="typo-body-2"
            >
              {{ $t('dcctl.action.dialog.modeForced') }}
            </p>
          </section>

          <section class="action-dialog__section">
            <h4 class="typo-subtitle-1">{{ $t('dcctl.action.dialog.plan') }}</h4>
            <p
              v-if="!steps.length"
              class="typo-body-1"
            >
              {{ $t('dcctl.action.dialog.noPlan') }}
            </p>
            <ol
              v-else
              class="action-dialog__plan typo-body-1"
              data-test="action-plan"
            >
              <li
                v-for="step in steps"
                :key="step.index"
                data-test="action-plan-step"
              >
                {{ enumLabel('stepType', step.type) }}
                <template v-if="step.datacenter"> · {{ resourceId(step.datacenter) }}</template>
                <template v-if="step.type === 'APPS_STOP' || step.type === 'APPS_START'">
                  · {{ $t('dcctl.action.dialog.stepStage', { n: step.stage ?? 0 }) }}
                </template>
                <wt-chip
                  v-if="step.point_of_no_return"
                  :color="toChipColor('yellow')"
                >
                  {{ $t('dcctl.action.dialog.pointOfNoReturn') }}
                </wt-chip>
              </li>
            </ol>
          </section>
        </template>

        <wt-message
          v-if="isResume && typedNameRequired"
          color="warn"
          data-test="action-resume-forced"
        >
          {{ $t('dcctl.action.dialog.resumeForced') }}
        </wt-message>

        <wt-message
          v-if="typedNameRequired && !expectedName && phase !== 'choosing'"
          color="error"
          data-test="action-no-target-name"
        >
          {{ $t('dcctl.action.dialog.noTargetName') }}
        </wt-message>
        <section
          v-else-if="typedNameRequired && phase !== 'choosing'"
          class="action-dialog__section"
          data-test="action-typed"
        >
          <wt-input-text
            v-model="typed"
            :label="$t('dcctl.action.dialog.typeName', { name: expectedName })"
            :placeholder="$t('dcctl.action.dialog.typePlaceholder')"
            :disabled="phase === 'submitting'"
            data-test="action-typed-name"
          />
          <p
            v-if="typed && !typedMatches"
            class="typo-body-2"
            data-test="action-typed-mismatch"
          >
            {{ $t('dcctl.action.dialog.typeMismatch') }}
          </p>
        </section>
      </div>
    </template>

    <template #actions>
      <wt-button
        color="secondary"
        :disabled="phase === 'submitting'"
        data-test="action-cancel"
        @click="emit('close')"
      >
        {{ $t('dcctl.action.dialog.cancel') }}
      </wt-button>
      <wt-button
        v-if="phase === 'ready' && (refusal || outcome === 'blocked' || outcome === 'refused')"
        color="secondary"
        data-test="action-check-again"
        @click="check"
      >
        {{ $t('dcctl.action.dialog.checkAgain') }}
      </wt-button>
      <wt-button
        :disabled="!canSubmit"
        :loading="phase === 'submitting'"
        data-test="action-run"
        @click="submit"
      >
        {{ phase === 'submitting' ? $t('dcctl.action.dialog.running') : $t('dcctl.action.dialog.run') }}
      </wt-button>
    </template>
  </wt-popup>
</template>

<script setup lang="ts">
import { computed, onMounted, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRouter } from 'vue-router';

import type { ActionTarget } from '@/api/client/action';
import { getApiError } from '@/api/client/apiError';
import type {
	WebitelDcctlV1Operation as Operation,
	WebitelDcctlV1DatacenterPatroniRole as Role,
} from '@/api/gen/_models';
import ActionChecks from '@/components/action/ActionChecks.vue';
import { useActionDialog } from '@/composables/useActionDialog';
import { useEnumLabel } from '@/composables/useEnumLabel';
import type { ActionKind } from '@/utils/actionAvailability';
import type { ConfirmationNeed } from '@/utils/actionPreview';
import { metadataEntries, refusalKey } from '@/utils/actionRefusal';
import { resourceId } from '@/utils/datacenterSummary';
import { formatBytes } from '@/utils/formatBytes';
import { abandonWarning, orderedSteps } from '@/utils/operationView';
import { modeColor, toChipColor } from '@/utils/statusMapping';
import { formatViolations } from '@/utils/violations';

export interface WinnerCandidate {
	/** `dc_a` */
	id: string;
	role?: Role;
}

const props = defineProps<{
	kind: ActionKind;
	/** `dc_b`: the target of a datacenter action. */
	datacenter?: string;
	/** `resume` and `abandon`: the operation page's operation. */
	operation?: Operation;
	/** `resolve`: the datacenters of the conflicting records. */
	candidates?: readonly WinnerCandidate[];
}>();

const emit = defineEmits<{
	close: [];
	/** The action went through; the dialog has opened the operation page. */
	done: [operation: Operation];
}>();

const { t, te, locale } = useI18n();
const router = useRouter();
const enumLabel = useEnumLabel();

const base: ActionTarget = {
	kind: props.kind,
	datacenter: props.datacenter,
	operation: props.operation ? resourceId(props.operation.name) : undefined,
};

const {
	phase,
	preview,
	outcome,
	refusal,
	needs,
	pendingConfirmations,
	accepted,
	setAccepted,
	typed,
	typedNameRequired,
	expectedName,
	winner,
	factsChanged,
	result,
	canSubmit,
	blockers,
	check,
	submit,
	chooseWinner,
} = useActionDialog(base, resourceId(props.operation?.target));

onMounted(() => {
	if (props.kind !== 'resolve') void check();
});

watch(result, (operation) => {
	if (!operation) return;
	const id = resourceId(operation.name);
	void router.push(
		id ? { name: 'operation', params: { id } } : { name: 'operations' },
	);
	emit('done', operation);
	emit('close');
});

const title = computed(() =>
	t(`dcctl.action.title.${props.kind}`, {
		target: props.datacenter ?? resourceId(props.operation?.target),
	}),
);

const isAbandon = computed(() => props.kind === 'abandon' && !!props.operation);
const isResume = computed(() => props.kind === 'resume');
const abandonLevel = computed(() =>
	abandonWarning(props.operation ?? {}, preview.value?.checks),
);

const steps = computed(() => orderedSteps({ steps: preview.value?.steps }));
const typedMatches = computed(() => typed.value.trim() === expectedName.value);

const confirmLabel = (need: ConfirmationNeed): string =>
	t(`dcctl.action.confirm.${need.field}`, {
		bytes: formatBytes(need.bytes, locale.value),
	});

const metaLabel = (name: string): string =>
	te(`dcctl.action.refusal.meta.${name}`)
		? t(`dcctl.action.refusal.meta.${name}`)
		: name;

const refusalText = computed(() => {
	const error = refusal.value;
	if (!error) return '';
	const key = refusalKey(error.api?.id);
	if (key) return t(`dcctl.action.refusal.${key}`);
	return error.api
		? t('dcctl.action.refusal.unknown')
		: t('dcctl.action.refusal.network');
});

// One formatter for the violations: the dialog does not word them itself.
const violations = computed(() =>
	formatViolations(getApiError(refusal.value?.error)?.violations),
);
const metadata = computed(() => metadataEntries(refusal.value?.api?.metadata));
const runningOperation = computed(() =>
	refusal.value?.api?.id === 'dcctl.operation.running' ||
	refusal.value?.api?.id === 'dcctl.lock.busy'
		? resourceId(refusal.value.api.metadata?.operation)
		: '',
);
</script>

<style scoped>
.action-dialog {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-sm);
}

.action-dialog__section {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-xs);
}

.action-dialog__section h4 {
  margin: 0;
}

.action-dialog__winner {
  display: flex;
  align-items: center;
  gap: var(--spacing-xs);
}

.action-dialog__need {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-xs);
}

.action-dialog__plan {
  margin: 0;
  padding-left: var(--spacing-md);
}

.action-dialog__plain {
  margin: 0;
  padding-left: var(--spacing-md);
}

.action-dialog__violation {
  white-space: pre-wrap;
}

.action-dialog__meta {
  margin-right: var(--spacing-xs);
}
</style>
