<template>
  <main class="login">
    <form class="login__form" @submit.prevent="submit">
      <h1 class="typo-heading-2">{{ $t('dcctl.login.title') }}</h1>
      <wt-input-text
        v-model="name"
        :label="$t('dcctl.login.name')"
        autocomplete="username"
        required
      />
      <wt-password
        v-model="password"
        :label="$t('vocabulary.password')"
        required
      />
      <wt-message v-if="error" color="error">{{ error }}</wt-message>
      <wt-button
        type="submit"
        wide
        :loading="isLoading"
        :disabled="!name || !password"
      >
        {{ $t('dcctl.login.submit') }}
      </wt-button>
    </form>
  </main>
</template>

<script setup lang="ts">
import { ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRoute, useRouter } from 'vue-router';

import { isUnauthorized } from '@/api/client/apiError';
import { notifyApiError } from '@/api/client/notifyApiError';
import { safeRedirect } from '@/router';
import { useAuthStore } from '@/stores/auth';

const auth = useAuthStore();
const route = useRoute();
const router = useRouter();
const { t } = useI18n();

const name = ref('');
const password = ref('');
const error = ref('');
const isLoading = ref(false);

const submit = async () => {
	if (isLoading.value) return;
	isLoading.value = true;
	error.value = '';
	try {
		await auth.login(name.value, password.value);
		await router.replace(safeRedirect(route.query.redirect));
	} catch (e) {
		if (isUnauthorized(e)) {
			// the same text for a wrong name and a wrong password
			error.value = t('dcctl.login.invalid');
		} else {
			error.value = t('dcctl.login.failed');
			notifyApiError(e);
		}
		password.value = '';
	} finally {
		isLoading.value = false;
	}
};
</script>

<style scoped>
.login {
  display: grid;
  place-items: center;
  min-height: 100vh;
  background: var(--wt-page-background-color);
}

.login__form {
  display: flex;
  flex-direction: column;
  gap: var(--spacing-sm);
  width: 360px;
  max-width: 100%;
  padding: var(--spacing-md);
  border-radius: var(--border-radius);
  background: var(--wt-layout-background-color);
}
</style>
