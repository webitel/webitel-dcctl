import { enableAutoUnmount } from '@vue/test-utils';
import { afterEach } from 'vitest';

// Components are attached to document.body; leaving them there leaks DOM and timers between tests.
enableAutoUnmount(afterEach);

afterEach(() => {
	sessionStorage.clear();
});

// jsdom has no matchMedia; the SDK selects ask for it.
if (!window.matchMedia) {
	window.matchMedia = (query: string) =>
		({
			matches: false,
			media: query,
			onchange: null,
			addEventListener: () => undefined,
			removeEventListener: () => undefined,
			addListener: () => undefined,
			removeListener: () => undefined,
			dispatchEvent: () => false,
		}) as MediaQueryList;
}
