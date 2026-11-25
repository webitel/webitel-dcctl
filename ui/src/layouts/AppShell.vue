<template>
  <wt-page
    :navigation-rail="{ topItems, activeItemId }"
    @navigation-select="onSelect"
  >
    <template #header>
      <wt-app-header>
        <status-header />
        <span class="typo-body-1">{{ auth.userName }}</span>
        <wt-button
          color="secondary"
          variant="outlined"
          @click="auth.logout()"
        >
          {{ $t('vocabulary.logout') }}
        </wt-button>
      </wt-app-header>
    </template>
    <operation-line />
    <wt-layout>
      <wt-content-wrapper align="stretch">
        <router-view />
      </wt-content-wrapper>
    </wt-layout>
  </wt-page>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRoute, useRouter } from 'vue-router';

import OperationLine from '@/components/status/OperationLine.vue';
import StatusHeader from '@/components/status/StatusHeader.vue';
import { useAuthStore } from '@/stores/auth';
import { useStatusStore } from '@/stores/status';

const auth = useAuthStore();
const statusStore = useStatusStore();
const route = useRoute();
const router = useRouter();
const { t } = useI18n();

const SECTIONS = [
	{ id: 'overview', icon: 'home' },
	{ id: 'datacenters', icon: 'tile' },
	{ id: 'operations', icon: 'job' },
	{ id: 'journal', icon: 'log' },
] as const;

const topItems = computed(() =>
	SECTIONS.map(({ id, icon }) => ({ id, icon, label: t(`dcctl.nav.${id}`) })),
);
// Nested pages (a datacenter, an operation) keep their section highlighted.
const activeItemId = computed(() =>
	String(route.meta.section ?? route.name ?? ''),
);

onMounted(() => statusStore.start());
onBeforeUnmount(() => statusStore.stop());

const onSelect = ({ id }: { id: string }) => router.push({ name: id });
</script>
