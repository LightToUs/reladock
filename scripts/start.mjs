import { readFile, mkdir } from 'node:fs/promises';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
const env = { ...process.env };
try {
  for (const line of (await readFile(new URL('../.env', import.meta.url), 'utf8')).split('\n')) {
    const match = /^([A-Z_]+)=(.*)$/.exec(line);
    if (match && !Object.hasOwn(env, match[1])) env[match[1]] = match[2];
  }
} catch (error) { if (error.code !== 'ENOENT') throw error; }
const args = process.argv.slice(2);
env.GOCACHE ??= fileURLToPath(new URL('../.cache/go-build', import.meta.url));
if (args[0] === 'test') {
  env.TEST_DATABASE_URL ??= env.DATABASE_URL;
  if (!env.TEST_DATABASE_URL) { console.error('测试需要 TEST_DATABASE_URL 或 .env 中的 DATABASE_URL。请先运行 npm run setup 和 npm run db:start。'); process.exit(1); }
}
const cwd = fileURLToPath(new URL('../', import.meta.url));
const binary = fileURLToPath(new URL('../.cache/reladock', import.meta.url));
if (!args.length) await mkdir(new URL('../.cache/', import.meta.url), { recursive: true });
let child;
let stopped = false;
for (const signal of ['SIGINT', 'SIGTERM']) process.on(signal, () => { stopped = true; child?.kill(signal); });
function launch(command, commandArgs, afterExit) {
  child = spawn(command, commandArgs, { cwd, env, stdio: 'inherit' });
  child.on('error', error => { console.error(error.message); process.exitCode = 1; });
  child.on('exit', code => { if (afterExit && code === 0 && !stopped) afterExit(); else process.exitCode = code ?? 1; });
}
if (args.length) launch('go', args);
else launch('go', ['build', '-o', binary, './cmd/reladock'], () => launch(binary, []));
