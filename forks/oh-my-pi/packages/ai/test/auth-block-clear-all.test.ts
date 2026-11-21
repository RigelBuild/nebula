import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { AuthStorage, SqliteAuthCredentialStore } from "@oh-my-pi/pi-ai";
import { CredentialBlocks } from "../src/auth/blocks";
import { removeWithRetries } from "../../utils/src/temp";

const PROVIDER = "anthropic";
const PROVIDER_KEY = "anthropic:oauth";
const FUTURE_BLOCK_MS = 1_899_999_999_000;

/**
 * `CredentialBlocks.clearAll` is the reset-redeem clear. It must remove
 * unprotected scopes and keep `manual` and `account-policy` rows. This runs on
 * the real SQLite store and the real AuthStorage wiring.
 */
describe("CredentialBlocks.clearAll", () => {
	let tempDir = "";

	beforeEach(async () => {
		tempDir = await fs.mkdtemp(path.join(os.tmpdir(), "pi-ai-block-clear-all-"));
	});

	afterEach(async () => {
		if (tempDir) {
			await removeWithRetries(tempDir);
			tempDir = "";
		}
	});

	it("spares protected manual and account-policy blocks and clears the rest", async () => {
		const store = await SqliteAuthCredentialStore.open(path.join(tempDir, "agent.db"));
		await store.saveOAuth(PROVIDER, {
			access: "access-1",
			refresh: "refresh-1",
			expires: Date.now() + 3_600_000,
			accountId: "account-1",
			email: "1@example.com",
		});
		const [row] = store.listAuthCredentials(PROVIDER);
		if (!row) throw new Error("expected credential row");
		const storage = new AuthStorage(store);
		await storage.credentials.reload();
		try {
			for (const blockScope of ["manual", "account-policy", "tier:fable", ""]) {
				storage.blocks.upsert({
					credentialId: row.id,
					providerKey: PROVIDER_KEY,
					blockScope,
					blockedUntilMs: FUTURE_BLOCK_MS,
				});
			}
			// `clearAll` is not on the public BlocksApi; narrow with instanceof, which is checked at runtime.
			const blocks = storage.blocks;
			if (!(blocks instanceof CredentialBlocks)) throw new Error("expected CredentialBlocks");
			blocks.clearAll(PROVIDER, row.id);
			const remaining = store
				.listCredentialBlocks([row.id])
				.map(block => block.blockScope)
				.sort();
			expect(remaining).toEqual(["account-policy", "manual"]);
		} finally {
			storage.close();
		}
	});
});
