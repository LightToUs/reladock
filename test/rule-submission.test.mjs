import test from 'node:test';
import assert from 'node:assert/strict';
import { submitRule, submitRuleUpdate } from '../public/rule-submission.js';

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

test('a committed rule update stays successful when the overview refresh fails', async () => {
  const input = { id: 'rule-1', name: 'Updated', desiredVersion: 3 };
  const saved = { ...input, desiredVersion: 4 };
  const result = await submitRuleUpdate(
    input,
    async value => { assert.deepEqual(value, input); return saved; },
    async () => { throw new Error('overview offline'); },
  );

  assert.equal(result.rule, saved);
  assert.equal(result.refreshError.message, 'overview offline');
});

test('an older create refresh cannot clear a newer lost request idempotency key', async () => {
  const draft = {};
  let finishFirstRefresh;
  let secondKey;
  const first = submitRule(
    draft,
    { name: 'A' },
    async () => ({ id: 'a' }),
    () => new Promise(resolve => { finishFirstRefresh = resolve; }),
  );
  await new Promise(resolve => setImmediate(resolve));

  await assert.rejects(submitRule(
    draft,
    { name: 'B' },
    async (_, key) => { secondKey = key; throw new Error('connection lost'); },
    async () => {},
  ));
  finishFirstRefresh();
  await first;

  let retryKey;
  await submitRule(
    draft,
    { name: 'B' },
    async (_, key) => { retryKey = key; return { id: 'b' }; },
    async () => {},
  );
  assert.equal(retryKey, secondKey);
});
