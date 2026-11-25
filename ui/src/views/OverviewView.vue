<template>
  <div class="overview">
    <alarm-list />
    <datacenter-table />
    <coordination-block />
  </div>
</template>

<script setup lang="ts">
import AlarmList from '@/components/overview/AlarmList.vue';
import CoordinationBlock from '@/components/overview/CoordinationBlock.vue';
import DatacenterTable from '@/components/overview/DatacenterTable.vue';
</script>

<style scoped>
.overview {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-md);
  padding: var(--spacing-md);
}
</style>
