import { logger } from "@oh-my-pi/pi-utils";
import { runAuthGatewayCommand } from "../../../src/cli/auth-gateway-cli";

// A malformed broker URL fails the snapshot fetch at once with no socket errno, so `runServe`
// rejects without entering the refused-connection retry. The transports it sets must still apply to this record.
process.env.OMP_AUTH_BROKER_URL = "http://[";
process.env.OMP_AUTH_BROKER_TOKEN = "test-token";

await runAuthGatewayCommand({ action: "serve", flags: { gatewayToken: "test-token" } }).catch(() => undefined);
logger.info("gateway-serve-logging-probe");
