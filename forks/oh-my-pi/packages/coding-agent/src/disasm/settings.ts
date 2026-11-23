import { register } from "../config/registry";

/** Backend for explicit analysis through read selectors and eval's dsm namespace; auto prefers idalib. */
export const cfgDisassembler = register({
	id: "disassembler.backend",
	type: "enum",
	values: ["auto", "ghidra", "ida", "none"] as const,
	default: "auto",
	ui: {
		tab: "tools",
		group: "Available Tools",
		label: "Disassembler",
		description:
			"Backend for analysis selectors in read and eval's dsm namespace; bare executable reads show metadata only. Auto uses IDA Pro when installed, else Ghidra (downloaded on first analysis)",
		options: [
			{
				value: "auto",
				label: "Auto",
				description: "IDA Pro when an install with idalib is found, otherwise Ghidra",
			},
			{
				value: "ghidra",
				label: "Ghidra",
				description:
					"Ghidra via PyGhidra; provisions Ghidra 12.2, a JDK 25 and PyGhidra on first analysis when not installed",
			},
			{
				value: "ida",
				label: "IDA Pro",
				description: "IDA Pro via idalib; analysis disabled when no install is found",
			},
			{ value: "none", label: "None", description: "Executable metadata only; analysis disabled" },
		],
	},
});

/** Maximum disassembler hosts per project; another open evicts the least recently used idle host. */
export const cfgDisasmMaxOpen = register({
	id: "disassembler.maxOpen",
	type: "number",
	default: 4,
	ui: {
		tab: "tools",
		group: "Disassembler",
		label: "Max Open Databases",
		description:
			"Most databases (omp.ida.* / omp.ghidra.* daemons in omp ps) open at once per project; opening another evicts an unreferenced idle one. Close retained dsm handles when done",
		options: [
			{ value: "2", label: "2" },
			{ value: "4", label: "4" },
			{ value: "8", label: "8" },
			{ value: "16", label: "16" },
		],
	},
});

/** Unreferenced idle seconds before a database saves and closes; zero disables idle-close. */
export const cfgDisasmIdleCloseSec = register({
	id: "disassembler.idleCloseSec",
	type: "number",
	default: 900,
	ui: {
		tab: "tools",
		group: "Disassembler",
		label: "Idle Close Timeout",
		description:
			"Save and close unreferenced databases idle longer than this many seconds (0 = never); retained dsm handles stay open. The exec namespace resets on reopen",
		options: [
			{ value: "0", label: "Never" },
			{ value: "300", label: "5 minutes" },
			{ value: "900", label: "15 minutes" },
			{ value: "1800", label: "30 minutes" },
			{ value: "3600", label: "1 hour" },
		],
	},
});

/** Maximum memory per disassembler host; empty disables the Linux systemd scope. */
export const cfgDisasmMemoryMax = register({
	id: "disassembler.memoryMax",
	type: "string",
	default: "6G",
	validate: raw => {
		if (typeof raw !== "string") throw new Error("disassembler.memoryMax must be a systemd memory value");
		if (raw === "" || raw === "infinity") return;
		const match = /^(\d+(?:\.\d+)?)([KMGTPE%]?)$/.exec(raw);
		const number = match?.[1];
		const suffix = match?.[2];
		let valid = false;
		if (number && suffix === "%") {
			valid = /^\d+(?:\.\d{1,2})?$/.test(number) && Number(number) > 0 && Number(number) <= 100;
		} else if (number && suffix !== undefined) {
			const [whole = "0", fraction = ""] = number.split(".");
			const scale = 1024n ** BigInt(" KMGTPE".indexOf(suffix || " "));
			const denominator = 10n ** BigInt(fraction.length);
			const bytes = ((BigInt(whole) * denominator + BigInt(fraction || "0")) * scale) / denominator;
			valid = bytes >= 1n && bytes <= 18446744073709551615n;
		}
		if (!valid) {
			throw new Error("disassembler.memoryMax must be empty, infinity, a positive size, or a percentage up to 100%");
		}
	},
	ui: {
		tab: "tools",
		group: "Disassembler",
		label: "Host Memory Limit",
		description: "Maximum memory for each disassembler host on Linux; empty disables the systemd scope",
	},
});
