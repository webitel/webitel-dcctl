<template>
  <screen-placeholder :title="route.params.dc as string" />
</template>

<script setup lang="ts">
import { useRoute } from 'vue-router';

import ScreenPlaceholder from './ScreenPlaceholder.vue';

const route = useRoute();
</script>
