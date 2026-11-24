import { defineConfig } from 'orval';

/*
 * Same pipeline as @webitel/api-services (swagger 2 -> OpenAPI 3 -> orval),
 * reduced to the wire pass: the dcctl API is snake_case on the wire, so there
 * is no camelCase model pass. Intermediate files live in .gen/ (see package.json).
 */
const workspace = './src/api/gen';
const input = './.gen/openapi.formatted.yaml';

export default defineConfig({
	dcctl: {
		input: { target: input },
		output: {
			workspace,
			target: '',
			client: 'axios',
			mode: 'tags-split',
			clean: true,
			indexFiles: true,
			schemas: './_models',
			override: {
				namingConvention: { enum: 'PascalCase' },
				mutator: {
					path: '../client/axiosMutator.ts',
					name: 'dcctlAxios',
				},
			},
			mock: {
				indexMockFiles: true,
				generators: [
					{
						type: 'msw',
						preferredContentType: 'application/json',
					},
				],
			},
		},
		hooks: {
			afterAllFilesWrite: 'npx biome check --write ./src/api/gen',
		},
	},
	dcctlZod: {
		input: { target: input },
		output: {
			workspace,
			target: '',
			fileExtension: '.zod.ts',
			client: 'zod',
			mode: 'tags-split',
			indexFiles: false,
			override: {
				namingConvention: { enum: 'PascalCase' },
				zod: {
					generate: {
						response: true,
						query: true,
						header: true,
						param: true,
						body: true,
					},
				},
			},
		},
		hooks: {
			afterAllFilesWrite: 'npx biome check --write ./src/api/gen',
		},
	},
});
