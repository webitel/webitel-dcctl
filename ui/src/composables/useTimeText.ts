import { useI18n } from 'vue-i18n';

import { formatTimeWithZone } from '@/utils/timeText';

/** A timestamp as a short date and a time with seconds in the browser's zone, labelled with its UTC offset. */
export const useTimeText = () => {
	const { locale } = useI18n();
	return (iso: string): string => formatTimeWithZone(iso, locale.value);
};
