import { reapSmokeBrokers } from "./reap-smoke-brokers.mjs";
import { reapRunCustodians } from "./reap-seat-custodians.mjs";

const [runMarker, brokerScope] = process.argv.slice(2);
if (!runMarker || !brokerScope) {
  console.error("usage: reap-delegated-suite.mjs <run-marker> <broker-scope>");
  process.exit(2);
}

const brokerReport = reapSmokeBrokers({ scope: brokerScope });
const custodianReport = reapRunCustodians(runMarker);
console.log("__DELEGATED_REAP__" + JSON.stringify({
  brokers: {
    reaped: brokerReport.reaped.map(({ pid }) => pid),
    failed: brokerReport.failed.map(({ pid, error }) => ({ pid, error })),
  },
  custodians: {
    reaped: custodianReport.reaped.map(({ pid }) => pid),
    failed: custodianReport.failed.map(({ pid, error }) => ({ pid, error })),
  },
}));
if (brokerReport.failed.length > 0 || custodianReport.failed.length > 0) process.exitCode = 1;