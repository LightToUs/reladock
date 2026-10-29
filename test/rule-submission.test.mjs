import test from 'node:test';
import assert from 'node:assert/strict';
import { submitRule } from '../public/rule-submission.js';

test('a failed list refresh preserves save success and the retry idempotency key', async () => {
  const draft = {};
  const keys = [];
  const input = { name: 'Customer relay', protocol: 'both', targetHost: '192.0.2.10', targetPort: 443, bandwidthMbps: 1000 };
  // Only the external HTTP boundary is replaced. Verify the key sent to it,
  // and that a GET failure cannot turn a committed POST into a save failure.
  const create = async (fields, key) => { assert.deepEqual(fields, input); keys.push(key); return { id: 'saved-rule', listenPort: 20000 }; };
  const first = await submitRule(draft, input, create, async () => { throw Error('overview offline'); });
  assert.equal(first.rule.id, 'saved-rule');
  assert.equal(first.refreshError.message, 'overview offline');
  const second = await submitRule(draft, input, create, async () => {});
  assert.equal(second.rule.id, 'saved-rule');
  assert.equal(keys[0], keys[1]);
  assert.equal(second.refreshError, undefined);
});

test('a lost save response reuses its key; changed inputs start a new request', async () => {
  const draft = {}; const keys = [];
  await assert.rejects(submitRule(draft, { name: 'A' }, async (_, key) => { keys.push(key); throw Error('connection lost'); }, async () => {}));
  await submitRule(draft, { name: 'A' }, async (_, key) => { keys.push(key); return { id: 'a' }; }, async () => {});
  await submitRule(draft, { name: 'B' }, async (_, key) => { keys.push(key); return { id: 'b' }; }, async () => {});
  assert.equal(keys[0], keys[1]);
  assert.notEqual(keys[1], keys[2]);
});
