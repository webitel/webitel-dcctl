<template>
  <div class="overview">
    <template v-if="store.status">
      <alarm-list />
      <datacenter-table />
      <coordination-block />
    </template>
    <div
      v-else-if="store.error"
      class="typo-body-1"
      role="alert"
      data-test="status-unavailable"
    >
      {{ $t('dcctl.overview.unavailable') }}: {{ apiErrorMessage(store.error) }}
    </div>
    <p
      v-else
      class="typo-body-1"
    >
      {{ $t('dcctl.overview.loading') }}
    </p>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, watch } from 'vue';
import { useRoute } from 'vue-router';

import { apiErrorMessage } from '@/api/client/apiError';
import AlarmList from '@/components/overview/AlarmList.vue';
import CoordinationBlock from '@/components/overview/CoordinationBlock.vue';
import DatacenterTable from '@/components/overview/DatacenterTable.vue';
import { useStatusStore } from '@/stores/status';

const store = useStatusStore();
const route = useRoute();

// The page scrolls inside the layout container, and the anchors exist only
// once the data is there, so the router's own hash scrolling is not enough.
const scrollToHash = async () => {
	if (!route.hash) return;
	await nextTick();
	document.getElementById(route.hash.slice(1))?.scrollIntoView();
};

const hasStatus = computed(() => store.status !== undefined);

onMounted(scrollToHash);
watch(() => route.hash, scrollToHash);
watch(hasStatus, (has) => has && scrollToHash());
</script>

<style scoped>
.overview {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-md);
  padding: var(--spacing-md);
}
</style>
