import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../public/admin.js', import.meta.url), 'utf8');
const render = source.slice(source.indexOf('function renderNodes(nodes) {'), source.indexOf('\nbindNodeVisibilityToggle('));
function renderNode(node) {
  const controls = []; const actions = [];
  const row = { dataset: {}, insertCell: () => ({ append: (...items) => controls.push(...items) }) };
  const presentation = () => ({ label: '', detail: '', tone: '' });
  vm.runInNewContext(`${render}\nrenderNodes(nodes);`, {
    nodes: [node], state: { nodes: [node], showHiddenNodes: false, monitoring: new Map() },
    $: () => ({ replaceChildren() {}, insertRow: () => row }), nodesForAdminList: nodes => nodes,
    cell: () => ({ classList: { add() {} } }), resourceModeLabel: () => '',
    dedicatedTenantPresentation: () => '',
    nodeResourcePresentation: presentation, agentContactPresentation: presentation,
    upgradePresentation: presentation, configurationPresentation: presentation,
    probePresentation: presentation, certificatePresentation: presentation,
    monitoringCell() {}, deliveries: {}, empty() {},
    button: (label, click) => ({ label, click }), deliveryControl: () => ({ label: '查看交付' }),
    visibilityControl: () => ({ label: '隐藏' }), openNodeAction: (kind, value) => actions.push([kind, value.id]),
  });
  return { controls, actions };
}
test('uncontacted draining node exposes an actionable withdrawal button', () => {
  const { controls, actions } = renderNode({ id: 'hk-01', status: 'draining', withdrawable: true });
  const withdraw = controls.find(item => item.label === '撤销登记');
  assert.ok(withdraw, 'withdrawal must remain available during draining');
  withdraw.click();
  assert.deepEqual(actions, [['withdraw-registration', 'hk-01']]);
});
test('contacted draining and retired nodes do not offer withdrawal', () => {
  for (const status of ['draining', 'retired']) {
    const { controls } = renderNode({ id: 'used-01', status, withdrawable: false });
    assert.equal(controls.some(item => item.label === '撤销登记'), false);
  }
});
