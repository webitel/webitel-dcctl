<template>
  <section
    id="coordination"
    class="coordination"
  >
    <h2 class="typo-heading-3">{{ $t('dcctl.overview.coordination.title') }}</h2>
    <dl class="coordination__facts typo-body-1">
      <dt>{{ $t('dcctl.overview.coordination.generation') }}</dt>
      <dd data-test="generation">{{ status?.generation }}</dd>

      <dt>{{ $t('dcctl.overview.coordination.forced') }}</dt>
      <dd>
        <wt-chip
          :color="toChipColor(status?.forced ? 'red' : 'gray')"
          data-test="forced"
        >
          {{ status?.forced
            ? $t('dcctl.overview.coordination.forcedYes')
            : $t('dcctl.overview.coordination.forcedNo') }}
        </wt-chip>
      </dd>

      <dt>{{ $t('dcctl.overview.coordination.quorum') }}</dt>
      <dd>
        <wt-chip
          v-if="status?.quorum"
          :color="toChipColor(quorumColor)"
          data-test="quorum"
        >
          {{ quorumText }}
        </wt-chip>
        <span
          v-else
          data-test="quorum"
        >{{ $t('dcctl.overview.coordination.noData') }}</span>
      </dd>

      <dt>{{ $t('dcctl.overview.coordination.profile') }}</dt>
      <dd>{{ enumLabel('profileName', status?.profile) }}</dd>

      <dt>{{ $t('dcctl.overview.coordination.lease') }}</dt>
      <dd data-test="lease">{{ leaseText }}</dd>
    </dl>

    <div
      v-if="initAvailability.visible"
      class="coordination__actions"
    >
      <action-button
        kind="initArbiter"
        :availability="initAvailability"
        color="primary"
        @click="initializing = true"
      />
    </div>
    <action-dialog
      v-if="initializing"
      kind="initArbiter"
      @close="initializing = false"
    />

    <h3 class="typo-heading-4">{{ $t('dcctl.overview.coordination.voters') }}</h3>
    <wt-table
      :headers="voterHeaders"
      :data="voters"
      :row-class="() => 'voter-row'"
      :sortable="false"
    >
      <template #name="{ item }">
        <span class="typo-body-1">{{ item.name }}</span>
      </template>
      <template #reachable="{ item }">
        <wt-chip :color="toChipColor(item.reachable ? 'green' : 'red')">
          {{ item.reachable
            ? $t('dcctl.overview.coordination.reachable')
            : $t('dcctl.overview.coordination.unreachable') }}
        </wt-chip>
      </template>
    </wt-table>
  </section>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue';
import { useI18n } from 'vue-i18n';

import ActionButton from '@/components/action/ActionButton.vue';
import ActionDialog from '@/components/action/ActionDialog.vue';
import { useAgeText } from '@/composables/useAgeText';
import { useEnumLabel } from '@/composables/useEnumLabel';
import { useStatusStore } from '@/stores/status';
import { actionAvailability } from '@/utils/actionAvailability';
import { durationSeconds } from '@/utils/dataAge';
import { resourceId } from '@/utils/datacenterSummary';
import { type StatusColor, toChipColor } from '@/utils/statusMapping';

const store = useStatusStore();
const { t } = useI18n();
const enumLabel = useEnumLabel();
const ageText = useAgeText();

const status = computed(() => store.status);

const initializing = ref(false);
const initAvailability = computed(() =>
	actionAvailability('initArbiter', { status: status.value }),
);

/** No majority, or only a forced write possible, is an emergency. */
const quorumColor = computed<StatusColor>(() => {
	const quorum = status.value?.quorum;
	return quorum?.available && quorum.mode !== 'FORCED_ONLY' ? 'green' : 'red';
});

const quorumText = computed(() => {
	const quorum = status.value?.quorum;
	if (!quorum) return '';
	const counts = t('dcctl.overview.coordination.quorumValue', {
		reachable: quorum.reachable_count ?? 0,
		total: quorum.total_count ?? 0,
		required: quorum.required_count ?? 0,
	});
	return `${enumLabel('quorumMode', quorum.mode)}: ${counts}`;
});

const leaseText = computed(() => {
	const lease = status.value?.lease;
	if (!lease?.holder) return t('dcctl.overview.coordination.noData');
	const age = durationSeconds(lease.age);
	return [
		t('dcctl.overview.coordination.leaseHolder', {
			holder: resourceId(lease.holder),
		}),
		age === null
			? ''
			: t('dcctl.overview.coordination.leaseAge', { age: ageText(age) }),
		lease.lost ? t('dcctl.overview.coordination.leaseLost') : '',
	]
		.filter(Boolean)
		.join(', ');
});

const voterHeaders = computed(() => [
	{ value: 'name', text: t('dcctl.overview.coordination.voter') },
	{ value: 'reachable', text: t('dcctl.overview.coordination.reachable') },
	{ value: 'generation', text: t('dcctl.overview.coordination.generation') },
	{ value: 'active', text: t('dcctl.overview.coordination.active') },
]);

const voters = computed(() =>
	(status.value?.voters ?? []).map((voter, index) => ({
		id: index,
		name:
			voter.type === 'ARBITER'
				? t('dcctl.overview.alarms.arbiter')
				: resourceId(voter.datacenter),
		reachable: voter.reachable,
		generation: voter.generation,
		active: resourceId(voter.active_datacenter),
	})),
);
</script>

<style scoped>
.coordination {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-xs);
}

.coordination__facts {
  display: grid;
  grid-template-columns: max-content 1fr;
  gap: var(--spacing-2xs) var(--spacing-md);
  margin: 0;
}

.coordination__actions {
  display: flex;
}

.coordination__facts dd {
  margin: 0;
}
</style>
