const UNITS = ['byte', 'kilobyte', 'megabyte', 'gigabyte', 'terabyte'] as const;

/**
 * `1572864` -> `1.5 MB` in the given locale. Steps are 1024, so the SI names
 * of Intl stand for KiB/MiB. `bytes` is an int64 as the API sends it (a string);
 * an empty, malformed or negative value gives an empty string.
 */
export const formatBytes = (
	bytes: string | number | undefined,
	locale: string,
): string => {
	if (bytes === undefined || bytes === '') return '';
	const value = Number(bytes);
	if (!Number.isFinite(value) || value < 0) return '';
	let unit = 0;
	let scaled = value;
	while (scaled >= 1024 && unit < UNITS.length - 1) {
		scaled /= 1024;
		unit++;
	}
	return new Intl.NumberFormat(locale, {
		style: 'unit',
		unit: UNITS[unit],
		maximumFractionDigits: 1,
	}).format(scaled);
};
