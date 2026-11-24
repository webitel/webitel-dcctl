<script setup lang="ts">
defineProps<{ title: string }>();
</script>

<template>
  <section class="screen-placeholder">
    <h1 class="typo-heading-2">{{ title }}</h1>
    <p class="typo-body-1">{{ $t('dcctl.placeholder') }}</p>
  </section>
</template>

<style scoped>
.screen-placeholder {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-sm);
  padding: var(--spacing-md);
}
</style>
