import { useI18n } from 'vue-i18n';

/** A timestamp as a short date and a time with seconds, in the current language. */
export const useTimeText = () => {
	const { locale } = useI18n();
	return (iso: string): string =>
		new Intl.DateTimeFormat(locale.value, {
			dateStyle: 'short',
			timeStyle: 'medium',
		}).format(new Date(iso));
};
