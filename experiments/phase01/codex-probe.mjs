// 验证 Codex stdio 协议及可选的两轮会话恢复；只输出合成测试的摘要。
import { spawn } from 'node:child_process';
import { createInterface } from 'node:readline';
import { mkdtemp } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import assert from 'node:assert/strict';

const live = process.argv.includes('--live');
const workspace = await mkdtemp(join(tmpdir(), 'turncourier-codex-probe-'));
const clients = [];
const report = { probe: 'codex', live, checks: [], turns: [] };

// connect 启动独立服务，按请求 ID 关联响应；未知服务器请求一律拒绝。
function connect() {
  const child = spawn('codex', ['app-server', '--stdio'], {
    cwd: workspace, stdio: ['pipe', 'pipe', 'pipe'],
  });
  let exited = false;
  const exitPromise = new Promise(resolve => child.once('exit', () => { exited = true; resolve(); }));
  let nextID = 1;
  const pending = new Map();
  const events = [];
  let stderrBytes = 0;
  child.stderr.on('data', chunk => { stderrBytes += chunk.length; });
  const lines = createInterface({ input: child.stdout });
  lines.on('line', line => {
    let message;
    try { message = JSON.parse(line); } catch { return; }
    if (message.method && message.id !== undefined) {
      send({ id: message.id, error: { code: -32601, message: 'Probe does not approve tools' } });
    } else if (message.id !== undefined && pending.has(message.id)) {
      const entry = pending.get(message.id);
      pending.delete(message.id);
      clearTimeout(entry.timer);
      if (message.error) entry.reject(new Error(JSON.stringify(message.error)));
      else entry.resolve(message.result);
    } else if (message.method) events.push(message);
  });
  child.on('error', error => {
    exited = true;
    for (const entry of pending.values()) { clearTimeout(entry.timer); entry.reject(error); }
    pending.clear();
  });
  child.on('exit', () => {
    for (const entry of pending.values()) {
      clearTimeout(entry.timer); entry.reject(new Error('app-server exited'));
    }
    pending.clear();
  });

  // send 写入一条 JSON 消息，不通过 shell 拼接用户内容。
  function send(message) { child.stdin.write(JSON.stringify(message) + '\n'); }

  // request 为每条 RPC 设置超时，错误仅包含协议结果，不记录进程环境。
  function request(method, params) {
    return new Promise((resolve, reject) => {
      const id = nextID++;
      const timer = setTimeout(() => {
        pending.delete(id); reject(new Error(`${method} timeout`));
      }, 30000);
      pending.set(id, { resolve, reject, timer });
      send({ id, method, params });
    });
  }

  // completed 等待指定回合终态，保留消息以防事件先于 RPC 响应到达。
  async function completed(turnID) {
    const deadline = Date.now() + 90000;
    while (Date.now() < deadline) {
      const event = events.find(item => item.method === 'turn/completed' && item.params.turn.id === turnID);
      if (event) return event;
      if (exited) throw new Error('app-server exited before turn completed');
      await new Promise(resolve => setTimeout(resolve, 100));
    }
    throw new Error('turn/completed timeout');
  }

  // close 只关闭本探针创建的进程，超时后强制结束，避免后台残留。
  async function close() {
    if (exited) return;
    const timeout = setTimeout(() => child.kill('SIGKILL'), 3000);
    child.stdin.end();
    await exitPromise;
    clearTimeout(timeout);
  }
  const client = { request, send, completed, close, events, stderrBytes: () => stderrBytes };
  clients.push(client);
  return client;
}

// initialize 完成官方要求的握手，使用稳定协议能力。
async function initialize(client) {
  await client.request('initialize', { clientInfo: { name: 'turncourier_probe', version: '0.1.0' } });
  client.send({ method: 'initialized', params: {} });
}

// runTurn 提交合成文本并核对最终消息；工具请求不会获得批准。
async function runTurn(client, threadID, prompt, expected) {
  const start = await client.request('turn/start', {
    threadId: threadID, input: [{ type: 'text', text: prompt }],
  });
  const event = await client.completed(start.turn.id);
  assert.equal(event.params.turn.status, 'completed');
  const items = client.events.filter(item => item.method === 'item/completed' &&
    item.params.turnId === start.turn.id && item.params.item.type === 'agentMessage');
  const final = items.findLast(item => item.params.item.phase === 'final_answer') ?? items.at(-1);
  const text = final?.params.item.text ?? '';
  assert.ok(text.includes(expected), '合成标记未出现在最终消息中');
  report.turns.push({ status: event.params.turn.status, markerMatched: true,
    messageSelection: final?.params.item.phase === 'final_answer' ? 'final_answer' : 'last-agent-message' });
}

try {
  const first = connect();
  let rejected = false;
  try { await first.request('thread/read', { threadId: 'invalid' }); }
  catch (error) { rejected = /Not initialized/i.test(error.message); }
  assert.ok(rejected, '未握手请求应被拒绝');
  report.checks.push('reject-before-initialize');
  await initialize(first);
  report.checks.push('initialize');
  const started = await first.request('thread/start', {
    cwd: workspace, sandbox: 'read-only', approvalPolicy: 'untrusted', ephemeral: !live,
    baseInstructions: 'This is a synthetic protocol test. Never use tools, files, network, skills or agents. Reply only with the requested short text.',
    developerInstructions: 'No tool calls. Do not inspect the machine. Only remember synthetic conversation text.',
  });
  assert.ok(started.thread.id);
  report.checks.push('thread-start');
  if (live) {
    await runTurn(first, started.thread.id, 'Remember the synthetic marker TC_MAPLE_47. Reply only STORED.', 'STORED');
    await first.close();
    const second = connect();
    await initialize(second);
    const resumed = await second.request('thread/resume', {
      threadId: started.thread.id, cwd: workspace, sandbox: 'read-only', approvalPolicy: 'untrusted',
    });
    assert.equal(resumed.thread.id, started.thread.id);
    report.checks.push('resume-same-thread-after-process-restart');
    await runTurn(second, started.thread.id, 'Return only the synthetic marker I asked you to remember.', 'TC_MAPLE_47');
    const read = await second.request('thread/read', { threadId: started.thread.id, includeTurns: true });
    assert.ok(read.thread.turns.length >= 2);
    report.checks.push('two-turns-persisted');
    await second.close();
  } else await first.close();
  report.ok = true;
} catch (error) {
  report.ok = false;
  report.error = error.message;
  process.exitCode = 1;
} finally {
  for (const client of clients) await client.close();
  console.log(JSON.stringify(report, null, 2));
}
