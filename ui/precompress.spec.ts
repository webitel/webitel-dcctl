import {
	mkdir,
	mkdtemp,
	readdir,
	readFile,
	rm,
	writeFile,
} from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { brotliDecompressSync, gunzipSync } from 'node:zlib';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { checkDist, precompressDir } from './precompress';

describe('precompressDir', () => {
	let dir: string;

	beforeEach(async () => {
		dir = await mkdtemp(join(tmpdir(), 'precompress-'));
		await mkdir(join(dir, 'nested'));
	});

	afterEach(() => rm(dir, { recursive: true, force: true }));

	it('writes .gz and .br siblings that decode to the original', async () => {
		const body = 'console.log("dcctl");'.repeat(50);
		await writeFile(join(dir, 'index-abc.js'), body);
		await writeFile(join(dir, 'nested', 'style.css'), body);

		await precompressDir(dir);

		for (const file of ['index-abc.js', join('nested', 'style.css')]) {
			const gz = await readFile(join(dir, `${file}.gz`));
			const br = await readFile(join(dir, `${file}.br`));
			expect(gunzipSync(gz).toString()).toBe(body);
			expect(brotliDecompressSync(br).toString()).toBe(body);
		}
	});

	it('skips formats that are already compressed', async () => {
		await writeFile(join(dir, 'font.woff2'), 'font');
		await writeFile(join(dir, 'logo.png'), 'png');

		await precompressDir(dir);

		expect((await readdir(dir)).sort()).toEqual([
			'font.woff2',
			'logo.png',
			'nested',
		]);
	});
});

describe('checkDist', () => {
	let dir: string;

	beforeEach(async () => {
		dir = await mkdtemp(join(tmpdir(), 'checkdist-'));
		await mkdir(join(dir, 'assets'));
		await writeFile(join(dir, 'index.html'), '<html></html>');
		await writeFile(join(dir, 'assets', 'app-abc.js'), 'x'.repeat(200));
		await precompressDir(join(dir, 'assets'));
	});

	afterEach(() => rm(dir, { recursive: true, force: true }));

	it('accepts a clean dist', async () => {
		expect(await checkDist(dir)).toEqual([]);
	});

	it('fails on a source map', async () => {
		await writeFile(join(dir, 'assets', 'app-abc.js.map'), '{}');
		expect(await checkDist(dir)).toContain('source map: assets/app-abc.js.map');
	});

	it('fails on an env file', async () => {
		await writeFile(join(dir, '.env'), 'A=1');
		expect(await checkDist(dir)).toContain('env file: .env');
	});

	it('fails on a missing sibling', async () => {
		await rm(join(dir, 'assets', 'app-abc.js.br'));
		expect(await checkDist(dir)).toContain('missing .br: assets/app-abc.js');
	});
});
