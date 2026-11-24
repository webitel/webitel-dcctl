import { messages as sdkMessages } from '@webitel/ui-sdk/locale';

const en = {
	nav: {
		overview: 'Overview',
		datacenters: 'Datacenters',
		operations: 'Operations',
		journal: 'Journal',
	},
	login: {
		title: 'Sign in to dcctl',
		name: 'Name',
		submit: 'Sign in',
		invalid: 'Wrong name or password',
		failed: 'Cannot sign in right now',
	},
	placeholder: 'This screen is not implemented yet.',
};

const uk: typeof en = {
	nav: {
		overview: 'Огляд',
		datacenters: 'Датацентри',
		operations: 'Операції',
		journal: 'Журнал',
	},
	login: {
		title: 'Вхід до dcctl',
		name: "Ім'я",
		submit: 'Увійти',
		invalid: "Невірне ім'я або пароль",
		failed: 'Зараз неможливо увійти',
	},
	placeholder: 'Цей екран ще не реалізовано.',
};

export const SUPPORTED_LOCALES = ['en', 'uk'] as const;
export type AppLocale = (typeof SUPPORTED_LOCALES)[number];

/** SDK messages (components) plus the app's own under `dcctl`. */
export const messages = {
	en: { ...sdkMessages.en, dcctl: en },
	uk: { ...sdkMessages.uk, dcctl: uk },
};

export const detectLocale = (languages: readonly string[]): AppLocale => {
	for (const language of languages) {
		const base = language.toLowerCase().split('-')[0];
		const found = SUPPORTED_LOCALES.find((locale) => locale === base);
		if (found) return found;
	}
	return 'en';
};
