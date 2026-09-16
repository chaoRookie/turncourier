// 使用现有 Claude 订阅进行两轮合成会话，验证退出进程后的原会话恢复。
import { spawn } from 'node:child_process';
import { mkdtemp } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import assert from 'node:assert/strict';

if (!process.argv.includes('--live')) {
  console.log('真实会话测试需显式传入 --live；只使用现有订阅，不读取或配置 API Key。');
  process.exit(0);
}
const workspace = await mkdtemp(join(tmpdir(), 'turncourier-claude-probe-'));
const report = { probe: 'claude', checks: [], turns: [] };

// run 启动一次无工具的合成测试，限制时间和缓冲区，返回机器可读事件。
function run(prompt, sessionID) {
  const args = ['--safe-mode', '--print', '--output-format', 'stream-json', '--verbose',
    '--tools', '', '--strict-mcp-config', '--mcp-config', '{"mcpServers":{}}',
    '--permission-mode', 'dontAsk', '--permission-prompts', 'none', '--include-hook-events'];
  if (sessionID) args.push('--resume', sessionID);
  const env = { ...process.env };
  delete env.ANTHROPIC_API_KEY;
  delete env.ANTHROPIC_AUTH_TOKEN;
  return new Promise((resolve, reject) => {
    const child = spawn('claude', args, { cwd: workspace, env, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '';
    let stderrBytes = 0;
    const timeout = setTimeout(() => { child.kill('SIGKILL'); reject(new Error('Claude timeout')); }, 90000);
    child.stdout.on('data', chunk => {
      output += chunk;
      if (output.length > 2 * 1024 * 1024) { child.kill('SIGKILL'); reject(new Error('Output too large')); }
    });
    child.stderr.on('data', chunk => { stderrBytes += chunk.length; });
    child.on('error', error => { clearTimeout(timeout); reject(error); });
    child.on('close', code => {
      clearTimeout(timeout);
      try {
        const events = output.trim().split('\n').filter(Boolean).map(line => JSON.parse(line));
        const result = events.findLast(event => event.type === 'result');
        if (code !== 0 || !result || result.is_error) {
          reject(new Error(`Claude exit=${code}, subtype=${result?.subtype}, is_error=${result?.is_error}, stderrBytes=${stderrBytes}`));
        } else resolve({ events, result });
      } catch { reject(new Error('Claude returned invalid stream JSON')); }
    });
    child.stdin.end(prompt);
  });
}

try {
  const auth = await new Promise((resolve, reject) => {
    const child = spawn('claude', ['auth', 'status', '--json'], { stdio: ['ignore', 'pipe', 'ignore'] });
    let output = '';
    const timeout = setTimeout(() => { child.kill('SIGKILL'); reject(new Error('Auth status timeout')); }, 10000);
    child.stdout.on('data', chunk => {
      output += chunk;
      if (output.length > 65536) { child.kill('SIGKILL'); reject(new Error('Auth status output too large')); }
    });
    child.on('error', error => { clearTimeout(timeout); reject(error); });
    child.on('close', code => {
      clearTimeout(timeout);
      if (code !== 0) { reject(new Error('Auth status failed')); return; }
      try { resolve(JSON.parse(output)); } catch { reject(new Error('Invalid auth status JSON')); }
    });
  });
  assert.equal(auth.authMethod, 'claude.ai', '探针只允许现有订阅登录');
  report.checks.push('subscription-auth');
  const first = await run('This is a synthetic protocol test. Remember TC_BIRCH_58. Do not use any tools. Reply only STORED.');
  assert.ok(first.result.result.includes('STORED'));
  assert.ok(first.result.session_id);
  report.turns.push({ status: first.result.subtype, markerMatched: true,
    eventTypes: [...new Set(first.events.map(event => event.type))] });
  const second = await run('Return only the synthetic marker I asked you to remember. Do not use tools.', first.result.session_id);
  assert.equal(second.result.session_id, first.result.session_id);
  assert.ok(second.result.result.includes('TC_BIRCH_58'));
  report.turns.push({ status: second.result.subtype, markerMatched: true,
    eventTypes: [...new Set(second.events.map(event => event.type))] });
  report.checks.push('stream-json-final-result', 'resume-same-session-after-process-exit', 'context-retained');
  report.ok = true;
} catch (error) {
  report.ok = false;
  report.error = error.message;
  process.exitCode = 1;
}
console.log(JSON.stringify(report, null, 2));
