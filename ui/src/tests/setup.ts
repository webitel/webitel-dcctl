import { enableAutoUnmount } from '@vue/test-utils';
import { afterEach } from 'vitest';

// Components are attached to document.body; leaving them there leaks DOM and timers between tests.
enableAutoUnmount(afterEach);

afterEach(() => {
	sessionStorage.clear();
});
