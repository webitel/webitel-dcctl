import { watch } from 'vue';
import {
	createRouter,
	createWebHistory,
	type Router,
	type RouterHistory,
} from 'vue-router';

import { useAuthStore } from '@/stores/auth';

/** Where the UI is served by the dcctl binary. */
export const ROUTER_BASE = '/dcctl/';

/** Accepts only an in-app path, so `?redirect=` cannot send the user off site. */
export const safeRedirect = (value: unknown): string => {
	if (typeof value !== 'string') return '/';
	if (!value.startsWith('/') || value.startsWith('//')) return '/';
	if (value.includes('\\')) return '/';
	return value;
};

export const createAppRouter = (
	history: RouterHistory = createWebHistory(ROUTER_BASE),
) => {
	const router = createRouter({
		history,
		routes: [
			{
				path: '/login',
				name: 'login',
				component: () => import('@/views/LoginView.vue'),
				meta: { public: true },
			},
			{
				path: '/',
				component: () => import('@/layouts/AppShell.vue'),
				children: [
					{
						path: '',
						name: 'overview',
						component: () => import('@/views/OverviewView.vue'),
					},
					{
						path: 'datacenters',
						name: 'datacenters',
						component: () => import('@/views/DatacentersView.vue'),
					},
					{
						path: 'operations',
						name: 'operations',
						component: () => import('@/views/OperationsView.vue'),
					},
					{
						path: 'journal',
						name: 'journal',
						component: () => import('@/views/JournalView.vue'),
					},
				],
			},
			{ path: '/:pathMatch(.*)*', redirect: { name: 'overview' } },
		],
	});

	router.beforeEach((to) => {
		const auth = useAuthStore();
		if (to.meta.public) {
			if (auth.isAuthenticated) return safeRedirect(to.query.redirect);
			return true;
		}
		if (!auth.isAuthenticated) {
			return { name: 'login', query: { redirect: to.fullPath } };
		}
		return true;
	});

	return router;
};

/**
 * Opens the login form as soon as the credentials are gone, whether the user
 * pressed "Logout" or an API call got `401`.
 */
export const redirectToLoginOnLogout = (router: Router) => {
	const auth = useAuthStore();
	return watch(
		() => auth.isAuthenticated,
		(isAuthenticated) => {
			const current = router.currentRoute.value;
			if (isAuthenticated || current.meta.public) return;
			router.replace({ name: 'login', query: { redirect: current.fullPath } });
		},
	);
};
