import { defineStore } from 'pinia';
import { computed, ref } from 'vue';

import { getStatusService } from '@/api/gen/status-service/status-service';
import { encodeBasicToken } from '@/utils/basicAuth';

export const AUTH_STORAGE_KEY = 'dcctl.auth';

interface Session {
	name: string;
	/** `Authorization: Basic` credentials of the tab (base64 `name:password`, decodable). */
	token: string;
}

const isSession = (value: unknown): value is Session =>
	typeof value === 'object' &&
	value !== null &&
	typeof (value as Session).name === 'string' &&
	typeof (value as Session).token === 'string';

// sessionStorage can throw (blocked storage, private mode); the app then works until reload.
const readSession = (): Session | null => {
	try {
		const raw = sessionStorage.getItem(AUTH_STORAGE_KEY);
		const parsed: unknown = raw ? JSON.parse(raw) : null;
		return isSession(parsed) ? parsed : null;
	} catch {
		return null;
	}
};

const writeSession = (session: Session | null): void => {
	try {
		if (session) {
			sessionStorage.setItem(AUTH_STORAGE_KEY, JSON.stringify(session));
		} else {
			sessionStorage.removeItem(AUTH_STORAGE_KEY);
		}
	} catch {
		// keep the in-memory session
	}
};

export const useAuthStore = defineStore('auth', () => {
	const session = ref<Session | null>(readSession());

	const isAuthenticated = computed(() => session.value !== null);
	const userName = computed(() => session.value?.name ?? '');
	/** Value of the `Authorization` header, `null` when logged out. */
	const authorization = computed(() =>
		session.value ? `Basic ${session.value.token}` : null,
	);

	const clear = () => {
		session.value = null;
		writeSession(null);
	};

	/**
	 * Verifies the credentials with `GET /status`. On any failure the session is
	 * dropped and the error is rethrown, so the form can tell `401` from the rest.
	 */
	const login = async (name: string, password: string) => {
		session.value = { name, token: encodeBasicToken(name, password) };
		try {
			await getStatusService().getStatus();
		} catch (error) {
			clear();
			throw error;
		}
		writeSession(session.value);
	};

	return {
		isAuthenticated,
		userName,
		authorization,
		login,
		logout: clear,
		clear,
	};
});
