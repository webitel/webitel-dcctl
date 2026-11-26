<template>
  <div
    class="dc-tab"
    data-test="tab-patroni"
  >
    <last-known-notice
      :source="sourceFor(datacenter, 'PATRONI')"
      :now-ms="nowMs"
    />
    <p
      v-if="!patroni"
      class="typo-body-1"
      data-test="patroni-empty"
    >
      {{ $t('dcctl.datacenter.patroni.empty') }}
    </p>
    <template v-else>
      <dl class="dc-facts typo-body-1">
        <dt>{{ $t('dcctl.datacenter.patroni.leader') }}</dt>
        <dd data-test="patroni-leader">
          <template v-if="patroni.leader">
            {{ patroni.leader }}
            <wt-chip
              v-if="patroni.role"
              :color="toChipColor('gray')"
            >
              {{ enumLabel('patroniRole', patroni.role) }}
            </wt-chip>
          </template>
          <template v-else-if="isStretch">{{ $t('dcctl.datacenter.patroni.leaderElsewhere') }}</template>
          <template v-else>{{ $t('dcctl.datacenter.noData') }}</template>
        </dd>

        <dt>{{ $t('dcctl.datacenter.patroni.timeline') }}</dt>
        <dd data-test="patroni-timeline">{{ patroni.timeline || '—' }}</dd>

        <dt>{{ $t('dcctl.datacenter.patroni.pause') }}</dt>
        <dd data-test="patroni-pause">
          <wt-chip
            v-if="patroni.paused"
            :color="toChipColor('yellow')"
          >
            {{ $t('dcctl.datacenter.patroni.paused') }}
          </wt-chip>
          <template v-else>{{ $t('dcctl.datacenter.patroni.notPaused') }}</template>
        </dd>

        <dt>{{ $t('dcctl.datacenter.patroni.upstream') }}</dt>
        <dd data-test="patroni-upstream">
          {{ resourceId(patroni.upstream_datacenter) || '—' }}
          <span v-if="patroni.standby_cluster_host">({{ patroni.standby_cluster_host }})</span>
        </dd>

        <dt>{{ $t('dcctl.datacenter.patroni.lag') }}</dt>
        <dd data-test="patroni-lag">{{ isActive ? '—' : lagText(patroni.lag_bytes) }}</dd>

        <dt>{{ $t('dcctl.datacenter.patroni.divergence') }}</dt>
        <dd data-test="patroni-divergence">
          <wt-chip
            v-if="divergenceView.kind !== 'unknown' && divergenceView.kind !== 'none'"
            :color="toChipColor(divergenceView.color)"
          >
            {{ divergenceText }}
          </wt-chip>
          <template v-else>{{ divergenceText }}</template>
        </dd>
      </dl>

      <h3 class="typo-heading-4">{{ $t('dcctl.datacenter.patroni.members') }}</h3>
      <p
        v-if="!members.length"
        class="typo-body-1"
        data-test="members-empty"
      >
        {{ $t('dcctl.datacenter.patroni.noMembers') }}
      </p>
      <wt-table
        v-else
        :headers="headers"
        :data="members"
        :row-class="rowClass"
        data-key="member"
        :sortable="false"
        data-test="members"
      >
        <template #member="{ item }">
          <span class="dc-cell">
            <span class="typo-body-1-bold">{{ item.member }}</span>
            <wt-chip
              v-if="item.member === patroni.leader"
              :color="toChipColor('blue')"
              data-test="leader-mark"
            >
              {{ $t('dcctl.datacenter.patroni.leaderMark') }}
            </wt-chip>
          </span>
        </template>
        <template #role="{ item }">
          {{ enumLabel('patroniRole', item.role) }}
        </template>
        <template #state="{ item }">
          <wt-chip
            :color="toChipColor(memberColor(item))"
            data-test="member-state"
          >
            {{ stateText(item) }}
          </wt-chip>
        </template>
        <template #timeline="{ item }">
          {{ item.timeline || '—' }}
        </template>
        <template #lag="{ item }">
          <span data-test="member-lag">{{ lagText(item.lag_bytes) }}</span>
        </template>
        <template #replication="{ item }">
          {{ item.replication_state || '—' }}
        </template>
        <template #usable="{ item }">
          <span data-test="member-usable">
            <template v-if="item.usable">{{ $t('dcctl.datacenter.patroni.usableYes') }}</template>
            <template v-else>
              {{ $t('dcctl.datacenter.patroni.usableNo') }}
              <span v-if="item.unusable_reason">({{ enumLabel('unusableReason', item.unusable_reason) }})</span>
            </template>
          </span>
        </template>
      </wt-table>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';

import type {
	WebitelDcctlV1Datacenter as Datacenter,
	WebitelDcctlV1DatacenterPatroniMember as Member,
} from '@/api/gen/_models';
import { useEnumLabel } from '@/composables/useEnumLabel';
import { useStatusStore } from '@/stores/status';
import { divergence, memberColor, sourceFor } from '@/utils/datacenterDetail';
import { resourceId } from '@/utils/datacenterSummary';
import { formatBytes } from '@/utils/formatBytes';
import { toChipColor } from '@/utils/statusMapping';
import LastKnownNotice from './LastKnownNotice.vue';
import './dcGrid.css';

const props = defineProps<{
	datacenter: Datacenter;
	/** Server time, ms. */
	nowMs: number;
	/** The datacenter is the active one: the lag behind itself means nothing. */
	isActive: boolean;
}>();

const { t, locale } = useI18n();
const enumLabel = useEnumLabel();
const statusStore = useStatusStore();

const isStretch = computed(() => statusStore.status?.profile === 'STRETCH');
const patroni = computed(() => props.datacenter.patroni);
const members = computed(() => patroni.value?.members ?? []);
const divergenceView = computed(() => divergence(patroni.value));

const headers = computed(() =>
	(
		[
			['member', 'member'],
			['host', 'host'],
			['role', 'role'],
			['state', 'state'],
			['timeline', 'timeline'],
			['lag', 'lag'],
			['replication', 'replication'],
			['usable', 'usable'],
		] as const
	).map(([value, key]) => ({
		value,
		text: t(`dcctl.datacenter.patroni.${key}`),
	})),
);

const rowClass = (member: Member) => `member-row member-row--${member.member}`;

/** A lag that is not known is not sent; a lag of 0 is. */
const lagText = (bytes: string | undefined): string =>
	formatBytes(bytes, locale.value) || '—';

const divergenceText = computed(() => {
	const view = divergenceView.value;
	switch (view.kind) {
		case 'diverged': {
			const bytes = formatBytes(view.bytes, locale.value);
			if (view.lsn && bytes) {
				return t('dcctl.datacenter.patroni.diverged', { lsn: view.lsn, bytes });
			}
			if (view.lsn) {
				return t('dcctl.datacenter.patroni.divergedAt', { lsn: view.lsn });
			}
			return t('dcctl.datacenter.patroni.divergedSize', { bytes });
		}
		case 'maybe':
			return t('dcctl.datacenter.patroni.mayHaveDiverged');
		case 'none':
			return t('dcctl.datacenter.patroni.notDiverged');
		case 'other':
			return view.raw ?? '—';
		default:
			return '—';
	}
});

/** A member that is not reachable shows why; otherwise the state as Patroni reports it. */
const stateText = (member: Member): string => {
	if (!member.reachable) {
		const reason = member.error ? enumLabel('sourceError', member.error) : '';
		return reason
			? `${t('dcctl.datacenter.patroni.notAnswering')} (${reason})`
			: t('dcctl.datacenter.patroni.notAnswering');
	}
	return member.patroni_state || '—';
};
</script>
