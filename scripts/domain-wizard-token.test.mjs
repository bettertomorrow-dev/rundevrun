// Tests private Cloudflare token delivery and cleanup in the domain setup helper.
import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync, readFileSync, mkdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';

const helper = fileURLToPath(new URL('../skills/rundevrun-domain-setup-wizard/scripts/create-cloudflare-token.mjs', import.meta.url));
const secret = 'dummy-one-time-secret-not-for-output';

function fixture(t, overrides = {}) {
  const root = mkdtempSync(path.join(tmpdir(), 'rdr-wizard-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const log = path.join(root, 'calls');
  const fake = `#!/usr/bin/env node
const fs = require('node:fs');
const args = process.argv.slice(2);
const program = require('node:path').basename(process.argv[1]);
fs.appendFileSync(process.env.CALL_LOG, JSON.stringify({program,args})+'\\n');
const output = x => console.log(JSON.stringify(x));
if(program === 'rdr') {
 if(args[1] === 'inspect') output({status:'ok',results:[{config:{version:1}}]});
 else if(args[1] === 'credential-store') {
  const input = fs.readFileSync(0,'utf8');
  if(input !== process.env.DUMMY_SECRET || args.includes(input)) process.exit(7);
  if(process.env.FAIL_STORE) {console.log(input);console.error(input);process.exit(9);}
  output({status:'ok',results:[{credential_ref:args[2]}]});
 } else process.exit(8);
} else if(args[0] === 'zones') output([{id:'a'.repeat(32),name:'example.test',status:process.env.PENDING_ZONE?'pending':'active',paused:false}]);
else if(args[2] === 'permission-groups') output([
 {id:'1'.repeat(32),name:'Zone Read',scopes:['com.cloudflare.api.account.zone']},
 {id:'2'.repeat(32),name:'DNS Write',scopes:['com.cloudflare.api.account.zone']},
 {id:'3'.repeat(32),name:'DNS Write',scopes:['com.cloudflare.api.account']}
]);
else if(args[2] === 'create') {
 if(process.env.FAIL_CREATE) {console.error(process.env.DUMMY_SECRET);process.exit(10);}
 output({id:'c'.repeat(32),value:process.env.DUMMY_SECRET,expires_on:null});
} else if(args[2] === 'delete' && args[3] === 'c'.repeat(32) && args[4] === '--force') {
 if(process.env.FAIL_DELETE) {console.error(process.env.DUMMY_SECRET);process.exit(11);}
 output({id:args[3]});
}
else process.exit(8);
`;
  const cf = path.join(root, 'cf'), rdr = path.join(root, 'rdr');
  for (const file of [cf, rdr]) writeFileSync(file, fake, { mode: 0o700 });
  const env = { ...process.env, XDG_CONFIG_HOME: path.join(root, 'config'), CALL_LOG: log, DUMMY_SECRET: secret, ...overrides };
  const invoke = (...extra) => spawnSync(process.execPath, [helper, '--zone', 'example.test', '--cf', cf, '--rdr', rdr, ...extra], { env, encoding: 'utf8' });
  const calls = () => readFileSync(log, 'utf8').trim().split('\n').map(JSON.parse);
  return { root, invoke, calls };
}

test('default invocation previews the exact zone policy without creating or storing a token', t => {
  const f = fixture(t), result = f.invoke();
  assert.equal(result.status, 0, result.stderr);
  const plan = JSON.parse(result.stdout);
  assert.equal(plan.status, 'planned');
  assert.deepEqual(plan.policy.policies[0].resources, { ['com.cloudflare.api.account.zone.'+'a'.repeat(32)]: '*' });
  assert.equal(plan.permissions.length, 2);
  assert(!f.calls().some(c => c.args.includes('create') || c.args.includes('credential-store')));
});

test('apply passes the one-time value only through rdr stdin and prints metadata', t => {
  const f = fixture(t), result = f.invoke('--apply');
  assert.equal(result.status, 0, result.stderr);
  assert.equal(JSON.parse(result.stdout).status, 'stored');
  assert(!result.stdout.includes(secret));
  assert(!result.stderr.includes(secret));
  assert(f.calls().some(c => c.args[1] === 'credential-store'));
  assert(!JSON.stringify(f.calls()).includes(secret));
  assert(!f.calls().some(c => c.args.includes('delete')));
});

test('storage failure revokes only the token just created and redacts CLI output', t => {
  const f = fixture(t, { FAIL_STORE: '1' }), result = f.invoke('--apply');
  assert.equal(result.status, 1);
  assert(!result.stdout.includes(secret) && !result.stderr.includes(secret));
  assert.match(result.stderr, /newly created token revoked/i);
  assert.deepEqual(f.calls().find(c => c.args.includes('delete')).args, ['user', 'tokens', 'delete', 'c'.repeat(32), '--force']);
});

test('an existing credential is preserved and prevents token creation', t => {
  const f = fixture(t);
  const credentials = path.join(f.root, 'config', 'rundevrun', 'credentials');
  mkdirSync(credentials, { recursive: true, mode: 0o700 });
  writeFileSync(path.join(credentials, 'cloudflare-dns-token'), 'existing', { mode: 0o600 });
  const result = f.invoke('--apply');
  assert.equal(result.status, 1);
  assert.match(result.stderr, /reference already exists/);
  assert(!f.calls().some(c => c.program === 'cf'));
  assert.equal(readFileSync(path.join(credentials, 'cloudflare-dns-token'), 'utf8'), 'existing');
});

test('failed revocation returns a recovery ID without disclosing the secret', t => {
  const f = fixture(t, { FAIL_STORE: '1', FAIL_DELETE: '1' }), result = f.invoke('--apply');
  assert.equal(result.status, 1);
  assert(!result.stdout.includes(secret) && !result.stderr.includes(secret));
  assert.match(result.stderr, new RegExp(`token ${'c'.repeat(32)} could not be revoked`));
});

test('pending zones and failed creation stop without storage or secret output', t => {
  for (const overrides of [{ PENDING_ZONE: '1' }, { FAIL_CREATE: '1' }]) {
    const f = fixture(t, overrides), result = f.invoke('--apply');
    assert.equal(result.status, 1);
    assert(!result.stdout.includes(secret) && !result.stderr.includes(secret));
    assert(!f.calls().some(c => c.args.includes('credential-store')));
  }
});
