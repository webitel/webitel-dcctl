import { readdir, readFile, stat, writeFile } from 'node:fs/promises';
import { extname, join, relative } from 'node:path';
import { promisify } from 'node:util';
import { brotliCompress, constants, gzip } from 'node:zlib';
import type { Plugin } from 'vite';

const gzipAsync = promisify(gzip);
const brotliAsync = promisify(brotliCompress);

// Text formats only: woff2 and images are already compressed.
export const COMPRESSIBLE = new Set([
	'.js',
	'.mjs',
	'.css',
	'.svg',
	'.json',
	'.html',
	'.txt',
	'.ttf',
	'.otf',
	'.eot',
]);

async function* walk(dir: string): AsyncGenerator<string> {
	for (const entry of await readdir(dir, { withFileTypes: true })) {
		const path = join(dir, entry.name);
		if (entry.isDirectory()) yield* walk(path);
		else yield path;
	}
}

// precompressDir writes <file>.gz and <file>.br next to every compressible file of dir,
// the siblings the dcctl binary serves to clients that accept them.
export async function precompressDir(dir: string): Promise<void> {
	for await (const file of walk(dir)) {
		if (!COMPRESSIBLE.has(extname(file))) continue;
		const data = await readFile(file);
		const [gz, br] = await Promise.all([
			gzipAsync(data, { level: 9 }),
			brotliAsync(data, {
				params: {
					[constants.BROTLI_PARAM_QUALITY]: constants.BROTLI_MAX_QUALITY,
					[constants.BROTLI_PARAM_SIZE_HINT]: data.length,
				},
			}),
		]);
		await Promise.all([
			writeFile(`${file}.gz`, gz),
			writeFile(`${file}.br`, br),
		]);
	}
}

// checkDist lists what must not reach the package or is missing from it: source maps,
// env files, the mock worker, and a .gz/.br sibling of a compressible asset.
export async function checkDist(dist: string): Promise<string[]> {
	const files: string[] = [];
	for await (const file of walk(dist)) files.push(relative(dist, file));
	const present = new Set(files);

	const problems: string[] = [];
	if (!present.has('index.html')) problems.push('index.html is missing');
	if (!files.some((f) => f.startsWith('assets/')))
		problems.push('assets/ is empty');
	for (const file of files) {
		if (file.endsWith('.map')) problems.push(`source map: ${file}`);
		if (/(^|\/)\.env/.test(file)) problems.push(`env file: ${file}`);
		if (file.endsWith('mockServiceWorker.js'))
			problems.push(`mock worker: ${file}`);
		if (!file.startsWith('assets/') || !COMPRESSIBLE.has(extname(file)))
			continue;
		for (const ext of ['.gz', '.br']) {
			if (!present.has(file + ext)) problems.push(`missing ${ext}: ${file}`);
			else if ((await stat(join(dist, file + ext))).size === 0)
				problems.push(`empty ${ext}: ${file}`);
		}
	}
	return problems;
}

// precompress compresses the files of <outDir>/assets after the build, then checks the
// whole dist and fails the build on a problem.
export function precompress(): Plugin {
	let outDir = 'dist';
	return {
		name: 'dcctl:precompress',
		apply: 'build',
		configResolved(config) {
			outDir = join(config.root, config.build.outDir);
		},
		async closeBundle() {
			await precompressDir(join(outDir, 'assets'));
			const problems = await checkDist(outDir);
			if (problems.length > 0) {
				throw new Error(`dist check failed:\n  ${problems.join('\n  ')}`);
			}
		},
	};
}
