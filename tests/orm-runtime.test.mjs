import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { spawn } from 'node:child_process';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

const upstream = 'markhuangai/opensource-repo-manager';
const repository = 'markhuangai/cpa-plugin-model-router';
const moduleUrl = new URL('../.github/orm/resolve-runtime.mjs', import.meta.url).href;
const root = fileURLToPath(new URL('..', import.meta.url));
const token = 'fixture-github-credential-canary';
const registryToken = 'fixture-registry-credential-canary';
const annotatedSha = 'c'.repeat(40);
const callerSha = 'd'.repeat(40);
const aliasPath = '/repos/' + upstream + '/git/ref/tags/v1-prerelease';
const tagPath = '/repos/' + upstream + '/git/tags/' + annotatedSha;
const assetPath = '/repos/' + upstream + '/releases/assets/19';
const manifestPath = '/v2/' + upstream + '/manifests/1-prerelease';

async function fixture(t, { source = 'a'.repeat(40), release = 'v1.0.0-rc.3', workflowFile = 'maintenance.yml' } = {}) {
  const directory = await mkdtemp(join(tmpdir(), 'orm-runtime-test-'));
  const state = {
    source, release, tagSha: annotatedSha, runId: 707, runAttempt: 2, repository, draft: false,
    published: '2026-11-07T01:54:33Z', overrides: {}, receipt: {}, requests: [],
    assets: [{ name: 'publication.json', id: 19 }],
    references: [{
      path: upstream + '/.github/workflows/' + workflowFile + '@v1-prerelease',
      ref: 'refs/tags/v1-prerelease', sha: annotatedSha,
    }],
  };
  const children = new Set();
  const timers = new Set();
  const server = createServer((request, response) => {
    const path = new URL(request.url, 'http://127.0.0.1').pathname;
    state.requests.push({ path, authorization: request.headers.authorization, accept: request.headers.accept });
    const manifest = state.manifest ?? JSON.stringify({
      schemaVersion: 2,
      mediaType: 'application/vnd.oci.image.index.v1+json',
      manifests: [{ digest: 'sha256:' + '1'.repeat(64), platform: { os: 'linux', architecture: 'amd64' } }],
      annotations: { 'org.opencontainers.image.revision': state.source },
    });
    const imageDigest = 'sha256:' + createHash('sha256').update(manifest).digest('hex');
    const receipt = {
      version: 1, release_id: 17, tag: state.release, source_sha: state.source,
      image: 'ghcr.io/' + upstream, digest: imageDigest, ...state.receipt,
    };
    const responses = {
      [aliasPath]: { ref: 'refs/tags/v1-prerelease', object: { type: 'tag', sha: state.tagSha } },
      ['/repos/' + upstream + '/git/tags/' + state.tagSha]: {
        sha: state.tagSha, tag: 'v1-prerelease', message: state.release,
        object: { type: 'commit', sha: state.source },
      },
      ['/repos/' + upstream + '/releases/tags/' + state.release]: {
        id: 17, tag_name: state.release, draft: state.draft, prerelease: true,
        published_at: state.published, assets: state.assets,
      },
      [assetPath]: receipt,
      '/download/publication.json': receipt,
      '/token': { token: registryToken },
    };
    if (/\/actions\/runs\/\d+\/attempts\/\d+$/.test(path)) {
      responses[path] = {
        id: state.runId, run_attempt: state.runAttempt,
        repository: { full_name: state.repository },
        referenced_workflows: state.references,
      };
    }
    function send() {
      if (response.destroyed) return;
      const override = state.overrides[path];
      if (override?.hang) return;
      if (override?.redirect) {
        response.writeHead(302, { location: override.redirect });
        response.end();
        return;
      }
      if (path.startsWith('/repos/') && request.headers.authorization !== 'Bearer ' + token) {
        response.writeHead(401);
        response.end('Missing fixture GitHub authentication');
        return;
      }
      if ((path === '/token' && request.headers.authorization) ||
          (path === manifestPath && request.headers.authorization !== 'Bearer ' + registryToken)) {
        response.writeHead(401);
        response.end('Wrong fixture registry authentication');
        return;
      }
      const body = override?.raw ?? (path === manifestPath ? state.registryManifest ?? manifest : JSON.stringify(responses[path]));
      response.writeHead(override?.status ?? (body === undefined ? 404 : 200), {
        'content-type': 'application/json',
        'x-github-request-id': 'fixture-request',
        ...(path === manifestPath && { 'docker-content-digest': state.headerDigest ?? imageDigest }),
      });
      response.end(body);
    }
    if (state.delayMs) {
      const timer = setTimeout(() => { timers.delete(timer); send(); }, state.delayMs);
      timers.add(timer);
    } else send();
  });
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  const baseUrl = 'http://127.0.0.1:' + server.address().port;
  t.after(async () => {
    for (const child of children) child.kill('SIGKILL');
    for (const timer of timers) clearTimeout(timer);
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
    await rm(directory, { recursive: true });
    assert.equal(children.size, 0);
    t.diagnostic('orm.fixture.cleanup passed');
  });

  async function run(overrides = {}) {
    const outputFile = join(directory, 'outputs');
    await writeFile(outputFile, '');
    const options = {
      repository, runId: 707, runAttempt: 2, actor: 'Z-M-Huang',
      callerSha, workflowFile, token,
      apiUrl: baseUrl, registryUrl: baseUrl, authUrl: baseUrl + '/token',
      requestTimeoutMs: 1000, timeoutMs: 5000, ...overrides,
    };
    const program = "import * as core from '@actions/core'; import { resolveRuntime } from " +
      JSON.stringify(moduleUrl) + '; await resolveRuntime({ ...JSON.parse(process.env.ORM_FIXTURE_OPTIONS), core });';
    const child = spawn(process.execPath, ['--input-type=module', '--eval', program], {
      cwd: root,
      env: {
        PATH: process.env.PATH,
        GITHUB_OUTPUT: outputFile,
        ORM_FIXTURE_OPTIONS: JSON.stringify(options),
        ACTIONS_STEP_DEBUG: 'false',
        FORCE_COLOR: '0',
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    children.add(child);
    let stdout = '';
    let stderr = '';
    child.stdout.on('data', (chunk) => { stdout += chunk; });
    child.stderr.on('data', (chunk) => { stderr += chunk; });
    const guard = setTimeout(() => child.kill('SIGKILL'), 8000);
    const exit = await new Promise((resolve, reject) => {
      child.once('error', reject);
      child.once('close', (code, signal) => resolve({ code, signal }));
    }).finally(() => { clearTimeout(guard); children.delete(child); });
    const records = stdout.split('\n').flatMap((line) => {
      const json = line.startsWith('::error::')
        ? line.slice('::error::'.length).replaceAll('%0D', '\r').replaceAll('%0A', '\n').replaceAll('%25', '%')
        : line;
      if (!json.startsWith('{')) return [];
      return [JSON.parse(json)];
    });
    return {
      ...exit, stdout, stderr, records,
      outputs: await readFile(outputFile, 'utf8'),
      selected: records.find((record) => record.event === 'orm.channel.selected'),
      failure: records.find((record) => record.event === 'orm.channel.failed'),
    };
  }
  return { state, run, baseUrl };
}

function failed(result, cause, stage) {
  assert.equal(result.code, 1, result.stderr);
  assert.equal(result.signal, null);
  assert.equal(result.outputs, '');
  assert.equal(result.selected, undefined);
  assert.equal(result.failure.cause, cause);
  if (stage) assert.equal(result.failure.stage, stage);
  assert.equal(result.failure.repository, repository);
  assert.equal(result.failure.actor, 'Z-M-Huang');
  assert.equal(result.failure.run_id, 707);
  assert.equal(result.failure.run_attempt, 2);
  assert.equal(result.failure.caller_sha, callerSha);
  assert.ok(result.failure.elapsed_ms >= 0);
  assert.ok(result.failure.next_action);
  assert.ok(!result.stdout.includes(token));
  assert.ok(!result.stdout.includes(registryToken));
}

test('selects the image behind the moving channel with real HTTP and native Actions output', async (t) => {
  const { state, run } = await fixture(t);
  const result = await run();
  assert.equal(result.code, 0, result.stderr);
  assert.equal(result.selected.source_sha, state.source);
  assert.equal(result.selected.workflow_sha, state.tagSha);
  assert.equal(result.selected.tag_sha, state.tagSha);
  assert.equal(result.selected.workflow_sha_kind, 'tag');
  assert.equal(result.selected.release, state.release);
  assert.equal(result.selected.receipt_digest, result.selected.registry_digest);
  assert.ok(result.outputs.includes('ghcr.io/' + upstream + '@' + result.selected.registry_digest));
  for (const path of [aliasPath, tagPath, assetPath, manifestPath]) {
    assert.equal(state.requests.filter((request) => request.path === path).length, 1);
  }
  assert.equal(result.records.filter((record) => record.event === 'orm.channel.selected').length, 1);
  assert.ok(!result.stdout.includes(token));
  assert.ok(!result.stdout.includes(registryToken));
});

test('accepts the recorded annotated tag SHA from the failed live run', async (t) => {
  const source = 'e538bdbfd239867029a49a4f003459551b2d0397';
  const tagSha = '2d5fcfb250c8a012f4000889c47dea788e59c78d';
  const { state, run } = await fixture(t, { source, workflowFile: 'test-ai.yml' });
  state.tagSha = tagSha;
  state.references[0].sha = tagSha;
  const result = await run();
  assert.equal(result.code, 0, result.stdout + result.stderr);
  assert.equal(result.selected.workflow_sha, tagSha);
  assert.equal(result.selected.tag_sha, tagSha);
  assert.equal(result.selected.source_sha, source);
  assert.equal(result.selected.workflow_sha_kind, 'tag');
  assert.ok(result.outputs.includes('ghcr.io/' + upstream + '@' + result.selected.registry_digest));
});

test('accepts a recorded commit SHA matching the verified tag target', async (t) => {
  const { state, run } = await fixture(t);
  state.references[0].sha = state.source;
  const result = await run();
  assert.equal(result.code, 0, result.stdout + result.stderr);
  assert.equal(result.selected.workflow_sha, state.source);
  assert.equal(result.selected.tag_sha, state.tagSha);
  assert.equal(result.selected.workflow_sha_kind, 'commit');
});

test('selects a newer prerelease without changing the resolver or caller', async (t) => {
  const { run } = await fixture(t, { source: 'b'.repeat(40), release: 'v1.1.0-rc.7' });
  const result = await run();
  assert.equal(result.code, 0, result.stderr);
  assert.equal(result.selected.release, 'v1.1.0-rc.7');
  assert.equal(result.selected.source_sha, 'b'.repeat(40));
});

test('uses the same channel policy for the manual AI workflow', async (t) => {
  const { run } = await fixture(t, { workflowFile: 'test-ai.yml' });
  const result = await run();
  assert.equal(result.code, 0, result.stderr);
  assert.equal(result.selected.workflow_file, 'test-ai.yml');
});

const rejected = [
  ['missing reusable-workflow identity', (s) => { s.references = []; }, 'workflow_reference_missing', 'workflow_identity'],
  ['a different run attempt', (s) => { s.runAttempt = 1; }, 'run_identity_mismatch', 'workflow_identity'],
  ['a different repository', (s) => { s.repository = 'someone/else'; }, 'run_identity_mismatch', 'workflow_identity'],
  ['an old workflow selected before the channel advanced', (s) => { s.references[0].sha = 'b'.repeat(40); }, 'workflow_source_mismatch', 'git_tag'],
  ['an earlier tag object even when its source commit is unchanged', (s) => { s.tagSha = 'e'.repeat(40); }, 'workflow_source_mismatch', 'git_tag'],
  ['a tag response with a different object identity', (s) => {
    s.overrides[tagPath] = { raw: JSON.stringify({
      sha: 'b'.repeat(40), tag: 'v1-prerelease', message: s.release,
      object: { type: 'commit', sha: s.source },
    }) };
  }, 'invalid_git_tag', 'git_tag'],
  ['an unpublished release', (s) => { s.published = null; }, 'release_unpublished', 'release'],
  ['a draft release', (s) => { s.draft = true; }, 'release_unpublished', 'release'],
  ['a missing receipt', (s) => { s.assets = []; }, 'receipt_missing', 'release'],
  ['a receipt from another source', (s) => { s.receipt.source_sha = 'b'.repeat(40); }, 'receipt_mismatch', 'receipt'],
  ['a receipt from another release', (s) => { s.receipt.release_id = 18; }, 'receipt_mismatch', 'receipt'],
  ['an image alias ahead of its release receipt', (s) => { s.registryManifest = '{"different":"image"}'; }, 'image_digest_mismatch', 'registry_manifest'],
  ['an inconsistent registry digest header', (s) => { s.headerDigest = 'sha256:' + 'f'.repeat(64); }, 'image_digest_mismatch', 'registry_manifest'],
  ['malformed API JSON', (s) => { s.overrides[aliasPath] = { raw: '{broken' }; }, 'malformed_json', 'git_alias'],
  ['an unexpected JSON type', (s) => { s.overrides[aliasPath] = { raw: '[]' }; }, 'invalid_json_type', 'git_alias'],
  ['a malformed registry index', (s) => { s.manifest = 'invalid-index'; }, 'invalid_manifest', 'registry_manifest'],
  ['an oversized response', (s) => { s.overrides[aliasPath] = { raw: 'x'.repeat(1024 * 1024 + 1) }; }, 'response_too_large', 'git_alias'],
];

for (const [name, configure, cause, stage] of rejected) {
  test('rejects ' + name + ' before producing an image output', async (t) => {
    const { state, run } = await fixture(t);
    configure(state);
    failed(await run(), cause, stage);
    const counts = new Map();
    for (const request of state.requests) counts.set(request.path, (counts.get(request.path) ?? 0) + 1);
    assert.ok([...counts.values()].every((count) => count === 1), 'resolver must not retry or fall back');
  });
}

test('a mismatch log alone identifies the versions and corrective action', async (t) => {
  const { state, run } = await fixture(t);
  state.references[0].sha = 'b'.repeat(40);
  const result = await run();
  failed(result, 'workflow_source_mismatch', 'git_tag');
  assert.deepEqual(result.failure.expected, { tag_sha: state.tagSha, source_sha: state.source });
  assert.equal(result.failure.observed, 'b'.repeat(40));
  assert.equal(result.failure.tag_sha, state.tagSha);
  assert.equal(result.failure.release, state.release);
  assert.match(result.failure.next_action, /start a new run/);
  assert.ok(!state.requests.some((request) => request.path === manifestPath));
});

test('preserves status, cause and correlation while redacting credentials and signed URLs', async (t) => {
  const { state, run } = await fixture(t);
  state.overrides[assetPath] = {
    status: 503,
    raw: JSON.stringify({ message: token + ' https://example.invalid/download?signature=signed-download-canary' }),
  };
  const result = await run();
  failed(result, 'http_error', 'receipt');
  assert.equal(result.failure.status, 503);
  assert.equal(result.failure.attempt, 1);
  assert.ok(result.failure.duration_ms >= 0);
  assert.match(result.failure.body, /\[redacted\]/);
  assert.ok(!result.stdout.includes('signed-download-canary'));
});

test('never logs a followed signed asset URL', async (t) => {
  const { state, run, baseUrl } = await fixture(t);
  state.overrides[assetPath] = { redirect: baseUrl + '/download/publication.json?signature=signed-asset-canary' };
  const result = await run();
  assert.equal(result.code, 0, result.stderr);
  assert.ok(state.requests.some((request) => request.path === '/download/publication.json'));
  assert.ok(!result.stdout.includes('signed-asset-canary'));
});

test('bounds diagnostic records and explicitly reports omitted content', async (t) => {
  const { state, run } = await fixture(t);
  state.overrides[assetPath] = { status: 502, raw: 'upstream detail '.repeat(1000) };
  const result = await run();
  failed(result, 'http_error', 'receipt');
  assert.equal(result.failure.status, 502);
  assert.ok(result.failure.omitted_bytes > 0);
  assert.ok(result.records.every((record) => Buffer.byteLength(JSON.stringify(record)) <= 2048));
});

test('retains diagnostic evidence when malformed tag metadata exceeds the record budget', async (t) => {
  const { state, run } = await fixture(t);
  state.overrides[tagPath] = {
    raw: JSON.stringify({
      sha: state.tagSha, tag: 'v1-prerelease', object: { type: 'commit', sha: state.source },
      message: 'malformed-release-canary '.repeat(150),
    }),
  };
  const result = await run();
  failed(result, 'invalid_git_tag', 'git_tag');
  assert.equal(result.failure.workflow_sha, state.tagSha);
  assert.equal(result.failure.workflow_ref, 'v1-prerelease');
  assert.equal(result.failure.image_tag, '1-prerelease');
  assert.equal(result.failure.expected.release, 'v1.<minor>.<patch>-rc.<number>');
  assert.match(result.failure.observed, /malformed-release-canary/);
  assert.ok(result.failure.omitted_bytes > 0);
  assert.ok(result.records.every((record) => Buffer.byteLength(JSON.stringify(record)) <= 2048));
});

test('bounds a stalled request and reports its real HTTP stage', async (t) => {
  const { state, run } = await fixture(t);
  state.overrides[aliasPath] = { hang: true };
  const result = await run({ requestTimeoutMs: 80, timeoutMs: 2000 });
  failed(result, 'request_timeout', 'git_alias');
  assert.equal(result.failure.status, null);
  assert.ok(result.failure.duration_ms >= 50);
});

test('the overall deadline includes all dependency exchanges', async (t) => {
  const { state, run } = await fixture(t);
  state.delayMs = 30;
  const result = await run({ requestTimeoutMs: 1000, timeoutMs: 100 });
  failed(result, 'overall_timeout');
  assert.ok(result.failure.elapsed_ms >= 70);
  assert.ok(state.requests.length < 8);
});
