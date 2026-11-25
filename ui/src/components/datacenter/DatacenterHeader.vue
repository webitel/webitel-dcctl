<template>
  <header class="dc-header">
    <div class="dc-header__title">
      <h1
        class="typo-heading-2"
        data-test="dc-title"
      >
        {{ resourceId(datacenter.name) }}
      </h1>
      <wt-chip
        v-if="isActive && datacenter.state !== 'ACTIVE'"
        :color="toChipColor('blue')"
        data-test="dc-active"
      >
        {{ $t('dcctl.datacenter.active') }}
      </wt-chip>
      <wt-chip
        :color="toChipColor(stateColor(datacenter.state, { isActive }))"
        data-test="dc-state"
      >
        {{ enumLabel('state', datacenter.state) }}
      </wt-chip>
      <wt-chip
        :color="toChipColor(healthColor(datacenter.health))"
        data-test="dc-health"
      >
        {{ enumLabel('health', datacenter.health) }}
      </wt-chip>
      <wt-chip
        v-if="datacenter.needs_recovery && datacenter.state !== 'NEEDS_RECOVERY'"
        :color="toChipColor('yellow')"
        data-test="dc-needs-recovery"
      >
        {{ $t('dcctl.datacenter.needsRecovery') }}
      </wt-chip>
      <data-age
        :age="age"
        :stale="stale"
        :stale-after="staleAfter"
      />
    </div>
    <div
      v-if="datacenter.health_reasons?.length"
      class="dc-header__reasons"
    >
      <span class="typo-body-1-bold">{{ $t('dcctl.datacenter.healthReasons') }}</span>
      <ul
        class="typo-body-1"
        data-test="health-reasons"
      >
        <li
          v-for="reason in datacenter.health_reasons"
          :key="reason"
        >
          {{ reason }}
        </li>
      </ul>
    </div>
  </header>
</template>

<script setup lang="ts">
import type { WebitelDcctlV1Datacenter as Datacenter } from '@/api/gen/_models';
import DataAge from '@/components/status/DataAge.vue';
import { useEnumLabel } from '@/composables/useEnumLabel';
import { resourceId } from '@/utils/datacenterSummary';
import { healthColor, stateColor, toChipColor } from '@/utils/statusMapping';

defineProps<{
	datacenter: Datacenter;
	/** `status.active_datacenter` is this datacenter. */
	isActive: boolean;
	/** Seconds since the observer's last cycle; `null` when unknown. */
	age: number | null;
	stale: boolean;
	/** Age in seconds past which the data is stale. */
	staleAfter: number;
}>();

const enumLabel = useEnumLabel();
</script>

<style scoped>
.dc-header {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-xs);
}

.dc-header__title {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--spacing-sm);
}

.dc-header__title h1 {
  margin: 0;
}

.dc-header__reasons {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-2xs);
}

.dc-header__reasons ul {
  margin: 0;
  padding-left: var(--spacing-md);
}
</style>
