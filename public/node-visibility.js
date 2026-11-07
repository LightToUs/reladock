export function nodesForAdminList(nodes, showHidden) {
  return showHidden ? nodes : nodes.filter(node => !node.hidden);
}

export function bindNodeVisibilityToggle({ checkbox, onChange }) {
  checkbox.addEventListener('change', () => onChange(checkbox.checked));
}
