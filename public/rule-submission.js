// Keep the request key until persistence and the list refresh both succeed.
export async function submitRule(draft, input, create, refresh) {
  const body = JSON.stringify(input);
  if (!draft.pending || draft.pending.body !== body) draft.pending = { body, key: crypto.randomUUID() };
  const rule = await create(input, draft.pending.key);
  try { await refresh(); draft.pending = undefined; return { rule }; }
  catch (refreshError) { return { rule, refreshError }; }
}
