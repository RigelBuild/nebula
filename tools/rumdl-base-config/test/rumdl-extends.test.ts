import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const baseConfig = join(import.meta.dir, "..", "base.toml");
const rumdl = Bun.which("rumdl");
if (!rumdl) throw new Error("rumdl is not on PATH; run this test in the nebula devShell");

const temporaryDirectories: string[] = [];

afterAll(() => {
	for (const directory of temporaryDirectories) {
		rmSync(directory, { recursive: true, force: true });
	}
});

// Runs rumdl check on one markdown fixture in a fresh directory. The config
// is written as given; RUMDL_BASE_CONFIG points at the shipped base.toml.
function check(config: string, markdown: string) {
	const directory = mkdtempSync(join(tmpdir(), "rumdl-base-config-"));
	temporaryDirectories.push(directory);
	const configPath = join(directory, ".rumdl.toml");
	const markdownPath = join(directory, "fixture.md");
	writeFileSync(configPath, config);
	writeFileSync(markdownPath, markdown);

	const result = Bun.spawnSync({
		cmd: [rumdl, "check", "--no-cache", "--config", configPath, markdownPath],
		cwd: directory,
		env: { ...process.env, RUMDL_BASE_CONFIG: baseConfig },
		stdout: "pipe",
		stderr: "pipe",
	});
	return {
		status: result.exitCode,
		output: `${result.stdout.toString()}${result.stderr.toString()}`,
	};
}

// One line well past the default MD013 limit, with no other violation.
const longLine = "word ".repeat(40).trimEnd();

describe("shared rumdl base policy", () => {
	test("a consumer extending the base passes a long line", () => {
		const result = check(
			'extends = "$RUMDL_BASE_CONFIG"\n\n[MD041]\nenabled = false\n',
			`${longLine}\n`,
		);
		expect(result.status).toBe(0);
	});

	test("a consumer extending the base fails an indented code block", () => {
		const result = check(
			'extends = "$RUMDL_BASE_CONFIG"\n\n[MD041]\nenabled = false\n',
			"Paragraph.\n\n    indented code\n",
		);
		expect(result.status).not.toBe(0);
		expect(result.output).toContain("MD046");
	});

	test("the same long line without extends reports MD013", () => {
		const result = check("[MD041]\nenabled = false\n", `${longLine}\n`);
		expect(result.status).not.toBe(0);
		expect(result.output).toContain("MD013");
	});
});
