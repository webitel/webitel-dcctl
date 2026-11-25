<template>
  <screen-placeholder :title="route.params.id as string" />
</template>

<script setup lang="ts">
import { useRoute } from 'vue-router';

import ScreenPlaceholder from './ScreenPlaceholder.vue';

const route = useRoute();
</script>
