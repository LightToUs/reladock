import { randomBytes } from 'node:crypto';
import { writeFile } from 'node:fs/promises';
const password = randomBytes(24).toString('hex');
try {
  await writeFile(new URL('../.env', import.meta.url), `POSTGRES_PASSWORD=${password}\nDATABASE_URL=postgres://reladock:${password}@127.0.0.1:55434/reladock?sslmode=disable\nPORT=3001\n`, { flag: 'wx', mode: 0o600 });
  console.log('已创建本地数据库配置 .env。运行 npm run db:start，再运行 npm start。');
} catch (error) {
  if (error.code !== 'EEXIST') throw error;
  console.log('.env 已存在，保留原配置。');
}
