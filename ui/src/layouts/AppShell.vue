<template>
  <wt-page
    :navigation-rail="{ topItems, activeItemId }"
    @navigation-select="onSelect"
  >
    <template #header>
      <wt-app-header>
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
    <wt-layout>
      <wt-content-wrapper align="stretch">
        <router-view />
      </wt-content-wrapper>
    </wt-layout>
  </wt-page>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRoute, useRouter } from 'vue-router';

import { useAuthStore } from '@/stores/auth';

const auth = useAuthStore();
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
const activeItemId = computed(() => String(route.name ?? ''));

const onSelect = ({ id }: { id: string }) => router.push({ name: id });
</script>
