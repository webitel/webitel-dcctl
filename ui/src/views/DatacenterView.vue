<template>
  <div class="dc-page">
    <div
      v-if="notFound"
      class="dc-page__message"
      role="alert"
      data-test="dc-not-found"
    >
      <p class="typo-body-1">
        {{ $t('dcctl.datacenter.notFound', { name: dcId }) }}
      </p>
      <router-link
        :to="{ name: 'overview', hash: '#datacenters' }"
        class="typo-body-1"
        data-test="dc-back"
      >
        {{ $t('dcctl.datacenter.back') }}
      </router-link>
    </div>

    <template v-else-if="datacenter">
      <wt-message
        v-if="error"
        color="error"
        data-test="dc-poll-error"
      >
        <strong>{{ $t('dcctl.datacenter.pollFailed') }}.</strong>
        {{ age === null
          ? $t('dcctl.datacenter.pollFailedNoAge')
          : $t('dcctl.datacenter.pollFailedHint', { age: ageText(age) }) }}
        {{ apiErrorMessage(error) }}
      </wt-message>
      <wt-message
        v-if="isStale"
        color="warn"
        data-test="dc-stale"
      >
        {{ age === null
          ? $t('dcctl.datacenter.staleNoTime')
          : $t('dcctl.datacenter.stale', { age: ageText(age) }) }}
      </wt-message>

      <datacenter-header
        :datacenter="datacenter"
        :is-active="isActive"
        :age="age"
        :stale="isStale"
        :stale-after="statusStore.staleAfter"
      />
      <wt-tabs
        :tabs="tabs"
        :current="currentTab"
        @change="selectTab"
      />
      <patroni-tab
        v-if="tab === 'patroni'"
        :datacenter="datacenter"
        :now-ms="nowMs"
        :is-active="isActive"
      />
      <apps-tab
        v-else-if="tab === 'apps'"
        :datacenter="datacenter"
        :now-ms="nowMs"
        :expect-running="expectRunning"
      />
      <info-tab
        v-else-if="tab === 'info'"
        :datacenter="datacenter"
        :now-ms="nowMs"
      />
      <sources-tab
        v-else
        :datacenter="datacenter"
        :now-ms="nowMs"
      />
    </template>

    <div
      v-else-if="error"
      class="dc-page__message typo-body-1"
      role="alert"
      data-test="dc-unavailable"
    >
      {{ $t('dcctl.datacenter.unavailable') }}: {{ apiErrorMessage(error) }}
    </div>
    <p
      v-else
      class="typo-body-1"
      data-test="dc-loading"
    >
      {{ $t('dcctl.datacenter.loading') }}
    </p>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRoute, useRouter } from 'vue-router';

import { apiErrorMessage } from '@/api/client/apiError';
import { serverOffsetMs } from '@/api/client/serverClock';
import AppsTab from '@/components/datacenter/AppsTab.vue';
import DatacenterHeader from '@/components/datacenter/DatacenterHeader.vue';
import InfoTab from '@/components/datacenter/InfoTab.vue';
import PatroniTab from '@/components/datacenter/PatroniTab.vue';
import SourcesTab from '@/components/datacenter/SourcesTab.vue';
import { useAgeText } from '@/composables/useAgeText';
import { useDatacenter } from '@/composables/useDatacenter';
import { useStatusStore } from '@/stores/status';
import {
	DATACENTER_TABS,
	type DatacenterTab,
	tabFromQuery,
} from '@/utils/datacenterDetail';
import { resourceId } from '@/utils/datacenterSummary';

const route = useRoute();
const router = useRouter();
const { t } = useI18n();
const ageText = useAgeText();
const statusStore = useStatusStore();

// The view is still mounted for a moment while the route already points elsewhere: keep the last name.
const dcId = ref(String(route.params.dc));
watch(
	() => route.params.dc,
	(value) => {
		if (route.name === 'datacenter' && typeof value === 'string') {
			dcId.value = value;
		}
	},
);
const { datacenter, error, notFound, now, age, isStale } = useDatacenter(
	dcId,
	() => statusStore.staleAfter,
);

// Server time: the browser clock may be off, and the ages must not depend on it.
const nowMs = computed(() => now.value + serverOffsetMs());
// The datacenter state ACTIVE says it too, before the status is there.
const isActive = computed(
	() =>
		resourceId(statusStore.status?.active_datacenter) === dcId.value ||
		datacenter.value?.state === 'ACTIVE',
);
// What the jobs should do is unknown until the status or the state tells the role.
const expectRunning = computed(() =>
	statusStore.status || datacenter.value?.state === 'ACTIVE'
		? isActive.value
		: undefined,
);

const tab = computed(() => tabFromQuery(route.query.tab));
const tabs = computed(() =>
	DATACENTER_TABS.map((value) => ({
		value,
		text: t(`dcctl.datacenter.tabs.${value}`),
	})),
);
const currentTab = computed(
	() => tabs.value.find((item) => item.value === tab.value) ?? tabs.value[0],
);

// The tab lives in the URL: a link from an alarm opens it, a reload keeps it.
const selectTab = (selected: { value: DatacenterTab }) => {
	router.replace({ query: { ...route.query, tab: selected.value } });
};
</script>

<style scoped>
.dc-page {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-md);
  padding: var(--spacing-md);
}

.dc-page__message {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-xs);
}
</style>
