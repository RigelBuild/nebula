import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { delimiter, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

const shard = fileURLToPath(new URL('../bin/smoke/shard.mjs', import.meta.url));
const mutationSuites = ['smoke:mutation-proof', 'smoke:mutation-reproof', 'smoke:mutation-reproof-full-partition', 'smoke:mutation-reproof-full-workflow', 'smoke:mutation-coverage', 'smoke:mutation-metadata'];

test('the shipped shard skips mutation infrastructure and still executes ordinary checks', () => {
  const root = mkdtempSync(join(tmpdir(), 'cotal-ci-policy-'));
  try {
    const bin = join(root, 'bin');
    mkdirSync(bin);
    const calls = join(root, 'calls.jsonl');
    const child = join(root, 'record-call.mjs');
    writeFileSync(child, `import {appendFileSync} from 'node:fs';\nappendFileSync(${JSON.stringify(calls)},JSON.stringify(process.argv.slice(2))+'\\n');\nconsole.log('SUITE COMPLETE: 3 passed, 0 failed');\n`);
    const pnpm = join(bin, process.platform === 'win32' ? 'pnpm.cmd' : 'pnpm');
    writeFileSync(pnpm, process.platform === 'win32'
      ? `@echo off\r\n"${process.execPath}" "${child}" %*\r\n`
      : `#!/bin/sh\nexec '${process.execPath.replaceAll("'", "'\\''")}' '${child.replaceAll("'", "'\\''")}' "$@"\n`);
    chmodSync(pnpm, 0o700);
    // This tests the shipped selector, not process cleanup. Replace both reaper modules before
    // evaluation so neither PATH-based ps nor direct /proc enumeration can reach the host.
    const loader = join(root, 'fixture-loader.mjs');
    const preload = join(root, 'fixture-preload.mjs');
    const replacements = {
      [new URL('../bin/smoke/reap-smoke-brokers.mjs', import.meta.url).href]: `console.log('fixture: broker reaper isolated'); export const reapSmokeBrokers = () => ({supported:false,reaped:[],failed:[]}); export const reportReaped = () => {};`,
      [new URL('../bin/smoke/reap-seat-custodians.mjs', import.meta.url).href]: `console.log('fixture: seat reaper isolated'); export const reapRunCustodians = () => ({supported:false,reaped:[],failed:[]}); export const reportCustodians = () => {};`,
    };
    writeFileSync(loader, `const replacements = ${JSON.stringify(replacements)};\nexport async function load(url, context, nextLoad) {\n  if (Object.hasOwn(replacements, url)) return {format:'module',source:replacements[url],shortCircuit:true};\n  return nextLoad(url, context);\n}\n`);
    writeFileSync(preload, `import {register} from 'node:module';\nregister(new URL('./fixture-loader.mjs', import.meta.url));\n`);
    const list = join(root, 'suites.txt');
    writeFileSync(list, ['smoke:ordinary-before', ...mutationSuites, 'smoke:ordinary-after'].join('\n') + '\n');
    writeFileSync(join(root, 'package.json'), JSON.stringify({ private: true }));
    const result = spawnSync(process.execPath, ['--import', preload, shard, '0', '1'], {
      cwd: root,
      env: { ...process.env, PATH: bin + delimiter + process.env.PATH, COTAL_CI_SUITES: list, SMOKE_CI_JOBS: '1' },
      encoding: 'utf8', timeout: 30000,
    });
    const output = result.stdout + result.stderr;
    assert.equal(result.status, 0, output);
    assert.ok(output.includes('fixture: broker reaper isolated'), output);
    assert.ok(output.includes('fixture: seat reaper isolated'), output);
    assert.deepEqual(readFileSync(calls, 'utf8').trim().split('\n').map(line => JSON.parse(line)), [['--silent', 'smoke:ordinary-before'], ['--silent', 'smoke:ordinary-after']]);
    for (const suite of mutationSuites) assert.ok(output.includes(`skipped pnpm ${suite} (mutation execution disabled)`), output);
    assert.match(output, /passed \(2 smokes, 6 cells; 6 mutation suite\(s\) excluded by policy\)/);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
test('the shard pool fails when either reaper cannot kill an attributed process', () => {
  const root = mkdtempSync(join(tmpdir(), 'cotal-pool-reap-failure-'));
  try {
    const loader = join(root, 'fixture-loader.mjs');
    const preload = join(root, 'fixture-preload.mjs');
    const replacements = {
      [new URL('../bin/smoke/reap-smoke-brokers.mjs', import.meta.url).href]: `export const reapSmokeBrokers = () => ({supported:true,inspected:0,reaped:[],failed:process.env.TEST_REAPER_FAILURE === 'broker' ? [{pid:123,error:'EPERM'}] : [],ownedLive:0,unparseable:0,unclaimable:0}); export const reportReaped = () => {};`,
      [new URL('../bin/smoke/reap-seat-custodians.mjs', import.meta.url).href]: `export const reapRunCustodians = () => ({supported:true,inspected:0,reaped:[],failed:process.env.TEST_REAPER_FAILURE === 'custodian' ? [{pid:456,run:'fixture',error:'EPERM'}] : [],unattributed:0,otherRuns:0}); export const reportCustodians = () => {};`,
    };
    writeFileSync(loader, `const replacements = ${JSON.stringify(replacements)};\nexport async function load(url, context, nextLoad) {\n  if (Object.hasOwn(replacements, url)) return {format:'module',source:replacements[url],shortCircuit:true};\n  return nextLoad(url, context);\n}\n`);
    writeFileSync(preload, `import {register} from 'node:module';\nregister(new URL('./fixture-loader.mjs', import.meta.url));\n`);
    const pnpm = join(root, 'pnpm');
    writeFileSync(pnpm, `#!/bin/sh\n[ "$1" = "--silent" ] && shift\nexec '${process.execPath.replaceAll("'", "'\\''")}' "$@"\n`, { mode: 0o700 });
    writeFileSync(join(root, 'fixture.mjs'), `console.log('COTAL_SMOKE_SENTINEL cells=1 passed=1 failed=0');\n`);
    const pool = fileURLToPath(new URL('../bin/smoke/shard-pool.mjs', import.meta.url));
    const runner = join(root, 'run-pool.mjs');
    writeFileSync(runner, `import {runPool} from ${JSON.stringify(pool)};\nconst command = 'pnpm fixture.mjs';\nconst failures = [];\nfor (const kind of ['broker', 'custodian']) { process.env.TEST_REAPER_FAILURE = kind; const result = await runPool([command], {jobs:1,runMarker:'fixture',indices:new Map([[command,0]]),onStart:()=>{}}); failures.push(result.failure); }\nconsole.log('POOL_REAP_FAILURES:' + JSON.stringify(failures));\n`);
    const result = spawnSync(process.execPath, ['--import', preload, runner], {
      cwd: root,
      env: { ...process.env, PATH: root + delimiter + process.env.PATH },
      encoding: 'utf8', timeout: 30000,
    });
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.match(result.stdout, /POOL_REAP_FAILURES:\[\{"cmd":"pnpm fixture\\.mjs","reason":"broker reaper could not kill pid 123","status":1\},\{"cmd":"pnpm fixture\\.mjs","reason":"custodian reaper could not kill pid 456","status":1\}\]/);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
