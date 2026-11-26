/** `GMT+02:00` becomes `UTC+02:00`, a zero offset becomes `UTC`; the same in every language. */
const utcOffset = (date: Date, timeZone?: string): string => {
	const name =
		new Intl.DateTimeFormat('en-US', { timeZone, timeZoneName: 'longOffset' })
			.formatToParts(date)
			.find((part) => part.type === 'timeZoneName')?.value ?? 'GMT';
	const offset = name.replace('GMT', '').replace('−', '-');
	return offset === '' || offset === '+00:00' ? 'UTC' : `UTC${offset}`;
};

/** Short date and time with seconds in the given language, followed by the UTC offset of the zone. */
export const formatTimeWithZone = (
	iso: string,
	locale: string,
	timeZone?: string,
): string => {
	const date = new Date(iso);
	const text = new Intl.DateTimeFormat(locale, {
		dateStyle: 'short',
		timeStyle: 'medium',
		timeZone,
	}).format(date);
	return `${text} ${utcOffset(date, timeZone)}`;
};
