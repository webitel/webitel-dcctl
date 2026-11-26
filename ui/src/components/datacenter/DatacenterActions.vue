<template>
  <div
    class="dc-actions"
    data-test="dc-actions"
  >
    <action-button
      v-for="item in primary"
      :key="item.kind"
      :kind="item.kind"
      :availability="item.availability"
      :color="colorOf(item.kind)"
      @click="dialogKind = item.kind"
    />
    <div
      v-if="more.length"
      ref="moreRoot"
      class="dc-actions__more"
    >
      <wt-button
        color="secondary"
        :aria-label="$t('dcctl.action.button.more')"
        data-test="action-more"
        @click="moreOpen = !moreOpen"
      >
        ⋯
      </wt-button>
      <div
        v-if="moreOpen"
        class="dc-actions__menu"
        data-test="action-menu"
      >
        <action-button
          v-for="item in more"
          :key="item.kind"
          :kind="item.kind"
          :availability="item.availability"
          @click="choose(item.kind)"
        />
      </div>
    </div>

    <action-dialog
      v-if="dialogKind"
      :kind="dialogKind"
      :datacenter="datacenterId"
      @close="dialogKind = null"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, useTemplateRef } from 'vue';

import type { WebitelDcctlV1Datacenter as Datacenter } from '@/api/gen/_models';
import ActionButton from '@/components/action/ActionButton.vue';
import ActionDialog from '@/components/action/ActionDialog.vue';
import { useStatusStore } from '@/stores/status';
import {
	type ActionKind,
	actionAvailability,
} from '@/utils/actionAvailability';

const props = defineProps<{
	datacenter: Datacenter;
	/** `dc_b` */
	datacenterId: string;
}>();

const store = useStatusStore();

const PRIMARY: readonly ActionKind[] = [
	'switch',
	'failover',
	'demote',
	'markRecovered',
];
const MORE: readonly ActionKind[] = ['repoint', 'syncApps'];

const entries = (kinds: readonly ActionKind[]) =>
	kinds
		.map((kind) => ({
			kind,
			availability: actionAvailability(kind, {
				status: store.status,
				datacenter: props.datacenter,
				datacenterId: props.datacenterId,
			}),
		}))
		.filter((item) => item.availability.visible);

const primary = computed(() => entries(PRIMARY));
const more = computed(() => entries(MORE));

const colorOf = (kind: ActionKind) => {
	if (kind === 'switch') return 'primary';
	return kind === 'failover' || kind === 'demote' ? 'error' : 'secondary';
};

const dialogKind = ref<ActionKind | null>(null);
const moreOpen = ref(false);
const moreRoot = useTemplateRef<HTMLElement>('moreRoot');
const closeOnOutside = (event: MouseEvent) => {
	if (!moreRoot.value?.contains(event.target as Node)) moreOpen.value = false;
};
onMounted(() => document.addEventListener('click', closeOnOutside));
onBeforeUnmount(() => document.removeEventListener('click', closeOnOutside));

const choose = (kind: ActionKind) => {
	moreOpen.value = false;
	dialogKind.value = kind;
};
</script>

<style scoped>
.dc-actions {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--spacing-xs);
}

.dc-actions__more {
  position: relative;
}

.dc-actions__menu {
  position: absolute;
  z-index: 10;
  top: 100%;
  right: 0;
  display: flex;
  flex-direction: column;
  gap: var(--spacing-xs);
  margin-top: var(--spacing-2xs);
  padding: var(--spacing-xs);
  border-radius: var(--border-radius);
  background: var(--content-wrapper-color, #fff);
  box-shadow: var(--elevation-10, 0 2px 8px rgb(0 0 0 / 20%));
}
</style>
