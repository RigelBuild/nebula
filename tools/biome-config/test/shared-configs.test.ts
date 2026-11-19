import { afterAll, describe, expect, test } from "bun:test";
import {
	mkdirSync,
	mkdtempSync,
	readFileSync,
	rmSync,
	writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const temporaryDirectories: string[] = [];

type BiomePreset = {
	[key: string]: unknown;
	formatter: { [key: string]: unknown; indentStyle: string };
	linter: {
		enabled: boolean;
		rules: { preset: string };
	};
};

function isBiomePreset(value: unknown): value is BiomePreset {
	return (
		typeof value === "object" &&
		value !== null &&
		"formatter" in value &&
		typeof value.formatter === "object" &&
		value.formatter !== null &&
		"indentStyle" in value.formatter &&
		typeof value.formatter.indentStyle === "string" &&
		"linter" in value &&
		typeof value.linter === "object" &&
		value.linter !== null &&
		"enabled" in value.linter &&
		typeof value.linter.enabled === "boolean" &&
		"rules" in value.linter &&
		typeof value.linter.rules === "object" &&
		value.linter.rules !== null &&
		"preset" in value.linter.rules &&
		typeof value.linter.rules.preset === "string"
	);
}

function parseBiomePreset(source: string): BiomePreset {
	const value: unknown = JSON.parse(source);
	if (!isBiomePreset(value)) throw new Error("Invalid Biome preset fixture");
	return value;
}

// A consumer is a temporary git repo that extends a copy of the preset. The
// copy lets a test mutate the preset without touching this source file.
function consumerFixture(prefix: string) {
	const directory = mkdtempSync(join(tmpdir(), prefix));
	temporaryDirectories.push(directory);
	// The preset enables VCS ignore files, so a consumer fixture must be a git repo.
	Bun.spawnSync({ cmd: ["git", "init", "-q", directory] });
	writeFileSync(join(directory, ".gitignore"), "node_modules/\ngenerated/\n");

	const presetDirectory = join(directory, "preset");
	mkdirSync(presetDirectory);
	const presetPath = join(presetDirectory, "biome.json");
	writeFileSync(
		presetPath,
		readFileSync(join(import.meta.dir, "..", "biome.json")),
	);

	const configPath = join(directory, "biome.json");
	const spaceConfigPath = join(directory, "space.json");
	const fixturePath = join(directory, "fixture.ts");
	writeFileSync(
		configPath,
		'{\n\t"root": true,\n\t"extends": ["./space.json", "./preset/biome.json"]\n}\n',
	);
	writeFileSync(
		spaceConfigPath,
		'{\n\t"root": false,\n\t"formatter": { "indentStyle": "space" }\n}\n',
	);
	return { directory, presetPath, configPath, fixturePath };
}

afterAll(() => {
	for (const directory of temporaryDirectories) {
		rmSync(directory, { recursive: true, force: true });
	}
});

describe("shared Biome preset", () => {
	test("the source preset parses as a Biome preset", () => {
		const preset = parseBiomePreset(
			readFileSync(join(import.meta.dir, "..", "biome.json"), "utf8"),
		);
		expect(preset.formatter.indentStyle).toBe("tab");
		expect(preset.linter.rules.preset).toBe("recommended");
	});

	test("shared preset preserves tabs and converts spaces to tabs", () => {
		const { directory, configPath, fixturePath } = consumerFixture(
			"biome-config-format-",
		);
		const tabInput = "export const preset = {\n\tshared: true,\n};\n";
		writeFileSync(fixturePath, tabInput);

		const tabResult = Bun.spawnSync({
			cmd: [
				"biome",
				"format",
				"--write",
				"--config-path",
				configPath,
				fixturePath,
			],
			cwd: directory,
			stdout: "pipe",
			stderr: "pipe",
		});
		expect(tabResult.exitCode).toBe(0);
		expect(readFileSync(fixturePath, "utf8")).toBe(tabInput);

		writeFileSync(
			fixturePath,
			"export const preset = {\n  shared: true,\n};\n",
		);
		const spaceResult = Bun.spawnSync({
			cmd: [
				"biome",
				"format",
				"--write",
				"--config-path",
				configPath,
				fixturePath,
			],
			cwd: directory,
			stdout: "pipe",
			stderr: "pipe",
		});
		expect(spaceResult.exitCode).toBe(0);
		expect(readFileSync(fixturePath, "utf8")).toBe(tabInput);
	});

	test("removing preset indentStyle restores the earlier space setting", () => {
		const { directory, presetPath, configPath, fixturePath } = consumerFixture(
			"biome-config-format-mutation-",
		);
		const originalPreset = readFileSync(presetPath, "utf8");
		const presetWithTabs = parseBiomePreset(originalPreset);
		const { formatter: _formatter, ...presetWithoutIndentStyle } =
			presetWithTabs;
		expect(presetWithoutIndentStyle).not.toEqual(presetWithTabs);

		const tabInput = "export const preset = {\n\tshared: true,\n};\n";
		writeFileSync(fixturePath, tabInput);
		writeFileSync(
			presetPath,
			`${JSON.stringify(presetWithoutIndentStyle, null, "\t")}\n`,
		);

		const formatResult = Bun.spawnSync({
			cmd: [
				"biome",
				"format",
				"--write",
				"--config-path",
				configPath,
				fixturePath,
			],
			cwd: directory,
			stdout: "pipe",
			stderr: "pipe",
		});
		expect(formatResult.exitCode).toBe(0);
		const formatted = readFileSync(fixturePath, "utf8");
		expect(formatted).not.toBe(tabInput);
		expect(formatted).toContain("  shared: true");
	});

	test("Biome CI accepts a consumer fixture extending the shared preset", () => {
		const { directory, configPath, fixturePath } = consumerFixture(
			"biome-config-preset-",
		);
		writeFileSync(
			fixturePath,
			"export const preset = {\n\tshared: true,\n};\n",
		);

		const result = Bun.spawnSync({
			cmd: ["biome", "ci", "--config-path", configPath, "."],
			cwd: directory,
			stdout: "pipe",
			stderr: "pipe",
		});
		expect(result.exitCode).toBe(0);
	});

	test("a consumer extending the preset skips gitignored files", () => {
		const { directory, configPath, fixturePath } =
			consumerFixture("biome-config-vcs-");
		writeFileSync(fixturePath, "export const ok = true;\n");
		mkdirSync(join(directory, "generated"));
		writeFileSync(
			join(directory, "generated", "bad.ts"),
			"export   const bad   =  1\n",
		);

		const result = Bun.spawnSync({
			cmd: ["biome", "ci", "--config-path", configPath, "."],
			cwd: directory,
			stdout: "pipe",
			stderr: "pipe",
		});
		expect(result.exitCode).toBe(0);
	});

	test("Biome CI rejects debugger under recommended lint rules", () => {
		const { directory, configPath, fixturePath } = consumerFixture(
			"biome-config-recommended-rule-",
		);
		writeFileSync(fixturePath, "debugger;\n");
		const invalidResult = Bun.spawnSync({
			cmd: ["biome", "ci", "--config-path", configPath, "."],
			cwd: directory,
			stdout: "pipe",
			stderr: "pipe",
		});
		expect(invalidResult.exitCode).not.toBe(0);

		writeFileSync(fixturePath, "const ready = true;\n");
		const validResult = Bun.spawnSync({
			cmd: ["biome", "ci", "--config-path", configPath, "."],
			cwd: directory,
			stdout: "pipe",
			stderr: "pipe",
		});
		expect(validResult.exitCode).toBe(0);
	});
});
