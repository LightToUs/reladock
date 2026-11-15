import test from 'node:test';
import assert from 'node:assert/strict';
import { sourceLoggingConfirmation, sourceLoggingPresentation } from './source-flow-logging.js';

test('source logging presentation distinguishes requested and applied state', () => {
  assert.equal(sourceLoggingPresentation({ desiredVersion: 2 }).key, 'disabled');
  assert.equal(sourceLoggingPresentation({ sourceLoggingEnabled: true, desiredVersion: 2, sourceLoggingAppliedVersion: 1 }).key, 'waiting-enable');
  assert.equal(sourceLoggingPresentation({ sourceLoggingEnabled: false, sourceLoggingAppliedEnabled: true, desiredVersion: 3, sourceLoggingAppliedVersion: 2 }).key, 'waiting-disable');
  assert.equal(sourceLoggingPresentation({ sourceLoggingEnabled: true, sourceLoggingAppliedEnabled: true, desiredVersion: 3, sourceLoggingAppliedVersion: 3 }).key, 'enabled');
  assert.equal(sourceLoggingPresentation({ sourceLoggingEnabled: true, sourceLoggingAppliedEnabled: true, desiredVersion: 3, sourceLoggingAppliedVersion: 3, sourceLoggingCollectorStatus: 'degraded' }).key, 'degraded');
  assert.equal(sourceLoggingPresentation({ sourceLoggingEnabled: true, sourceLoggingAppliedEnabled: true, sourceLoggingUnsupported: true }).key, 'unsupported');
});

test('confirmation states metadata privacy and overhead boundaries', () => {
  const text = sourceLoggingConfirmation(true);
  for (const value of ['来源 IP', '来源端口', '不记录包体', 'URL', 'DNS', 'SNI', '开销', '保留期', 'Agent']) assert.match(text, new RegExp(value));
});
