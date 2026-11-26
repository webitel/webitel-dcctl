<template>
  <section class="alarm-list">
    <h2 class="typo-heading-3">{{ $t('dcctl.overview.alarms.title') }}</h2>
    <p
      v-if="!store.alarms.length"
      class="typo-body-1"
      data-test="no-alarms"
    >
      {{ $t('dcctl.overview.alarms.empty') }}
    </p>
    <ul
      v-else
      class="alarm-list__items"
    >
      <li
        v-for="(alarm, index) in store.alarms"
        :key="index"
        class="alarm-list__item"
        data-test="alarm"
      >
        <wt-chip
          :color="toChipColor(severityColor(alarm.severity))"
          data-test="alarm-severity"
        >
          {{ enumLabel('overview.alarms.severity', alarm.severity) }}
        </wt-chip>
        <router-link
          class="typo-body-1-bold"
          :to="alarmTarget(alarm, store.status?.operation?.name)"
        >
          {{ enumLabel('overview.alarms.type', alarm.type) }}
        </router-link>
        <span class="typo-body-1">{{ subject(alarm) }}</span>
        <span
          v-if="alarm.start_time"
          class="typo-body-1"
        >
          {{ $t('dcctl.overview.alarms.since', { time: formatTime(alarm.start_time) }) }}
        </span>
        <span
          class="typo-body-1 alarm-list__detail"
          data-test="alarm-detail"
        >{{ alarm.detail }}</span>
        <action-button
          v-if="alarm.type === 'CONFLICT'"
          kind="resolve"
          :availability="resolveAvailability"
          color="primary"
          @click="resolving = true"
        />
      </li>
    </ul>
    <action-dialog
      v-if="resolving"
      kind="resolve"
      :candidates="candidates"
      @close="resolving = false"
    />
  </section>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue';
import { useI18n } from 'vue-i18n';

import type { WebitelDcctlV1StatusAlarm as Alarm } from '@/api/gen/_models';
import ActionButton from '@/components/action/ActionButton.vue';
import ActionDialog from '@/components/action/ActionDialog.vue';
import { useEnumLabel } from '@/composables/useEnumLabel';
import { useTimeText } from '@/composables/useTimeText';
import { useStatusStore } from '@/stores/status';
import { actionAvailability } from '@/utils/actionAvailability';
import { alarmTarget } from '@/utils/alarmTarget';
import { conflictCandidates } from '@/utils/conflictCandidates';
import { resourceId } from '@/utils/datacenterSummary';
import { severityColor, toChipColor } from '@/utils/statusMapping';

const store = useStatusStore();
const { t } = useI18n();
const enumLabel = useEnumLabel();

/** The object of the alarm: a datacenter, or the arbiter. */
const subject = (alarm: Alarm): string => {
	if (alarm.datacenter) return resourceId(alarm.datacenter);
	return alarm.voter_type === 'ARBITER'
		? t('dcctl.overview.alarms.arbiter')
		: '';
};

const formatTime = useTimeText();

const resolving = ref(false);
const resolveAvailability = computed(() =>
	actionAvailability('resolve', { status: store.status }),
);
const candidates = computed(() => conflictCandidates(store.status));
</script>

<style scoped>
.alarm-list {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-xs);
}

.alarm-list__items {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-xs);
  margin: 0;
  padding: 0;
  list-style: none;
}

.alarm-list__item {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--spacing-sm);
}

.alarm-list__detail {
  flex: 1 1 20rem;
}
</style>
