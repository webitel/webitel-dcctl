import { useI18n } from 'vue-i18n';

/**
 * Label of an API enum value under `dcctl.<group>`. A value the UI does not
 * know yet (the API got a new one) is shown as it is rather than as a key.
 */
export const useEnumLabel = () => {
	const { t, te } = useI18n();
	return (group: string, value: string | undefined): string => {
		if (!value) return '';
		const key = `dcctl.${group}.${value}`;
		return te(key) ? t(key) : value;
	};
};
