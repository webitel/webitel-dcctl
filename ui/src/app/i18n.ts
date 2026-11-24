import { createI18n } from 'vue-i18n';

import { detectLocale, messages } from './locale';

export const i18n = createI18n({
	legacy: false,
	locale: detectLocale(navigator.languages),
	fallbackLocale: 'en',
	messages,
});
