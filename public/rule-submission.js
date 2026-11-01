// Keep the request key until persistence and the list refresh both succeed.
export async function submitRule(draft, input, create, refresh) {
  const body = JSON.stringify(input);
  if (!draft.pending || draft.pending.body !== body) draft.pending = { body, key: crypto.randomUUID() };
  const request = draft.pending;
  const rule = await create(input, request.key);
  try {
    await refresh();
    if (draft.pending === request) draft.pending = undefined;
    return { rule };
  }
  catch (refreshError) { return { rule, refreshError }; }
}

export async function submitRuleUpdate(input, update, refresh) {
  const rule = await update(input);
  try { await refresh(); return { rule }; }
  catch (refreshError) { return { rule, refreshError }; }
}
