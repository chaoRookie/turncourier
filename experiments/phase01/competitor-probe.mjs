// 仅载入固定版本竞品的纯函数，用合成邮件体验解析与白名单，不启动网络或终端注入。
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { runInNewContext } from 'node:vm';
import { execFileSync } from 'node:child_process';
import assert from 'node:assert/strict';

const root = process.argv[2];
assert.ok(root, '需要提供竞品只读 checkout 路径');
const pinned = 'cd172b2f3361ccb60d7e691dcca768980a2eb13d';
const commit = execFileSync('git', ['-C', root, 'rev-parse', 'HEAD'], { encoding: 'utf8' }).trim();
assert.equal(commit, pinned, '源码版本变化后需要重新审查提取范围');
const source = readFileSync(join(root, 'src/relay/relay-pty.js'), 'utf8');

// section 根据已核对的相邻函数边界提取代码，不执行模块顶层的联网行为。
function section(start, end) {
  const begin = source.indexOf(start);
  const finish = source.indexOf(end, begin);
  assert.ok(begin >= 0 && finish > begin);
  return source.slice(begin, finish);
}
const functions = section('function isAllowed(', '// Extract Claude-Code-Remote token') + '\n' +
  section('function extractTokenFromSubject(', '// Clean email text') + '\n' +
  section('function cleanEmailText(', '// Unattended remote command injection');
const api = runInNewContext(functions + '\n({isAllowed,extractTokenFromSubject,cleanEmailText})', {
  ALLOWED_SENDERS: ['owner@example.invalid'],
  process: { env: { SMTP_USER: 'robot@example.invalid' } },
  // debug 忽略竞品调试输出，防止记录邮件内容。
  log: { debug() {} },
}, { timeout: 1000 });
const cases = [
  { name: 'valid-sender', actual: api.isAllowed('owner@example.invalid'), desired: true },
  { name: 'sender-substring', actual: api.isAllowed('owner@example.invalid.attacker.invalid'), desired: false },
  { name: 'sender-display-name', actual: api.isAllowed('"owner@example.invalid" <other@example.invalid>'), desired: false },
  { name: 'thread-token', actual: api.extractTokenFromSubject('Re: [Claude-Code-Remote #ABC12345]'), desired: 'ABC12345' },
  { name: 'quoted-reply', actual: api.cleanEmailText('继续测试\n> 历史内容'), desired: '继续测试' },
  { name: 'intentional-repetition', actual: api.cleanEmailText('哈哈哈哈'), desired: '哈哈哈哈' },
  { name: 'meaningful-prefix', actual: api.cleanEmailText('yes\nuse option two'), desired: 'yes\nuse option two' },
];
console.log(JSON.stringify({ probe: 'competitor-pure-functions', commit,
  cases: cases.map(item => ({ ...item, matchesDesired: item.actual === item.desired })),
  network: false, credentials: false, terminalInjection: false,
}, null, 2));
