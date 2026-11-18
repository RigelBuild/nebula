import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

const manifest: { bin?: Record<string, string> } = JSON.parse(
	readFileSync(join(import.meta.dir, "package.json"), "utf8"),
);

// A bin without a shebang fails with ENOEXEC when run from node_modules/.bin.
describe("docs-bundle bin", () => {
	test.each(Object.entries(manifest.bin ?? {}))(
		"%s starts with the bun shebang",
		(_name, target) => {
			const source = readFileSync(join(import.meta.dir, target), "utf8");
			expect(source.split("\n", 1)[0]).toBe("#!/usr/bin/env bun");
		},
	);

	// npm publish rewrites a "./" bin path and warns; keep manifests in the form npm publishes.
	test.each(Object.entries(manifest.bin ?? {}))(
		"%s bin path is npm-normalized",
		(_name, target) => {
			expect(target.startsWith("./")).toBe(false);
		},
	);
});
