import { useI18n } from 'vue-i18n';

import { formatAge } from '@/utils/dataAge';

/** `4 s`, `3 min`, `2 h` in the current language. */
export const useAgeText = () => {
	const { t } = useI18n();
	return (seconds: number): string => {
		const { unit, value } = formatAge(seconds);
		return t(`dcctl.header.ageUnit.${unit}`, { n: value });
	};
};
