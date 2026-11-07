import { createHash } from 'node:crypto';

const upstream = 'markhuangai/opensource-repo-manager';
const image = 'ghcr.io/' + upstream;
const workflowRef = 'v1-prerelease';
const imageTag = '1-prerelease';
const sha = /^[a-f0-9]{40}$/;
const digest = /^sha256:[a-f0-9]{64}$/;
const maximumResponse = 1024 * 1024;
const maximumRecord = 2048;

class ChannelError extends Error {
  constructor(cause, details = {}) {
    super(cause);
    this.details = { cause, ...details };
  }
}

export async function resolveRuntime({
  core, repository, runId, runAttempt, actor, callerSha, workflowFile, token,
  apiUrl = 'https://api.github.com',
  registryUrl = 'https://ghcr.io',
  authUrl = 'https://ghcr.io/token',
  requestTimeoutMs = 20_000,
  timeoutMs = 120_000,
}) {
  const started = Date.now();
  const overall = AbortSignal.timeout(timeoutMs);
  const secrets = new Set([token].filter(Boolean));
  const context = {
    repository, actor, run_id: runId, run_attempt: runAttempt,
    caller_sha: callerSha, workflow_file: workflowFile,
    workflow_ref: workflowRef, image_tag: imageTag,
    request_timeout_ms: requestTimeoutMs, overall_timeout_ms: timeoutMs,
  };
  let stage = 'context';
  let selected = {};

  function redact(value) {
    if (typeof value === 'string') {
      for (const secret of secrets) value = value.replaceAll(secret, '[redacted]');
      return value.replace(/https?:\/\/[^\s"]+\?[^\s"]+/g, (url) => url.split('?')[0] + '?[redacted]');
    }
    if (Array.isArray(value)) return value.map(redact);
    if (value && typeof value === 'object') {
      return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, redact(item)]));
    }
    return value;
  }

  function log(level, event, details = {}) {
    const record = redact({
      event, level, ...context, ...selected, stage,
      elapsed_ms: Date.now() - started, ...details,
    });
    let text = JSON.stringify(record);
    for (let limit = 256; Buffer.byteLength(text) > maximumRecord; limit = Math.floor(limit / 2)) {
      let omitted = 0;
      const bounded = Object.fromEntries(Object.entries(record).map(([key, value]) => {
        if (['event', 'level', 'cause', 'next_action'].includes(key) || value === undefined) return [key, value];
        const encoded = JSON.stringify(value);
        if (Buffer.byteLength(encoded) <= limit || typeof value === 'number') return [key, value];
        const preview = Buffer.from(typeof value === 'string' ? value : encoded).subarray(0, limit).toString('utf8') + '…';
        omitted += Math.max(0, Buffer.byteLength(encoded) - Buffer.byteLength(JSON.stringify(preview)));
        return [key, preview];
      }));
      bounded.omitted_bytes = (record.omitted_bytes ?? 0) + omitted;
      text = JSON.stringify(bounded);
    }
    if (level === 'error') core.setFailed(text);
    else core.info(text);
  }

  function requireValue(condition, cause, details) {
    if (!condition) throw new ChannelError(cause, details);
  }

  async function request(name, url, { authorization, accept = 'application/vnd.github+json' } = {}) {
    stage = name;
    const requestStarted = Date.now();
    const signal = AbortSignal.any([overall, AbortSignal.timeout(requestTimeoutMs)]);
    let response;
    let bytes = 0;
    try {
      response = await fetch(url, {
        signal,
        headers: {
          accept,
          ...(authorization && { authorization: 'Bearer ' + authorization }),
        },
      });
      const chunks = [];
      const reader = response.body.getReader();
      try {
        while (true) {
          const { done, value } = await reader.read();
          if (done) break;
          bytes += value.length;
          if (bytes > maximumResponse) {
            await reader.cancel();
            throw new ChannelError('response_too_large', {
              limit_bytes: maximumResponse, observed_bytes: bytes, omitted_bytes: bytes,
            });
          }
          chunks.push(value);
        }
      } finally {
        reader.releaseLock();
      }
      const buffer = Buffer.concat(chunks);
      log('info', 'orm.channel.http', {
        method: 'GET', url, status: response.status, attempt: 1,
        request_id: response.headers.get('x-github-request-id'),
        duration_ms: Date.now() - requestStarted, response_bytes: bytes,
      });
      requireValue(response.ok, 'http_error', {
        status: response.status,
        ...(name === 'registry_auth' ? { omitted_bytes: bytes } : { body: buffer.toString('utf8') }),
        next_action: 'Check upstream access or availability, then start a new run.',
      });
      return { buffer, headers: response.headers };
    } catch (error) {
      const details = {
        method: 'GET', url, status: response?.status ?? null,
        attempt: 1, duration_ms: Date.now() - requestStarted,
      };
      if (error instanceof ChannelError) {
        throw new ChannelError(error.message, { ...details, ...error.details });
      }
      throw new ChannelError(overall.aborted ? 'overall_timeout' : signal.aborted ? 'request_timeout' : 'transport_error', {
        ...details,
        error_name: error.name,
        error_message: error.message,
        transport_code: error.cause?.code,
        next_action: 'Check upstream connectivity, then start a new run.',
      });
    }
  }

  async function json(name, path, options) {
    const response = await request(name, path, options);
    let data;
    try {
      data = JSON.parse(response.buffer.toString('utf8'));
    } catch {
      throw new ChannelError('malformed_json', {
        response_bytes: response.buffer.length,
        ...(name === 'registry_auth' ? { omitted_bytes: response.buffer.length } : { body: response.buffer.toString('utf8') }),
        next_action: 'Inspect the upstream response format before starting a new run.',
      });
    }
    requireValue(data !== null && typeof data === 'object' && !Array.isArray(data),
      'invalid_json_type', { expected: 'object', observed: data === null ? 'null' : Array.isArray(data) ? 'array' : typeof data });
    return { data, ...response };
  }

  const api = (path) => apiUrl + path;
  const github = (name, path, options = {}) => json(name, api(path), { authorization: token, ...options });

  try {
    requireValue(
      /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repository) &&
      Number.isSafeInteger(Number(runId)) && Number(runId) > 0 &&
      Number.isSafeInteger(Number(runAttempt)) && Number(runAttempt) > 0 &&
      typeof actor === 'string' && actor.length <= 100 && sha.test(callerSha) &&
      ['maintenance.yml', 'test-ai.yml'].includes(workflowFile) && typeof token === 'string' && token.length > 0,
      'invalid_context',
    );
    log('info', 'orm.channel.started', { outcome: 'resolving' });
    const { data: run } = await github('workflow_identity',
      '/repos/' + repository + '/actions/runs/' + runId + '/attempts/' + runAttempt);
    requireValue(
      run.id === Number(runId) && run.run_attempt === Number(runAttempt) &&
      typeof run.repository?.full_name === 'string' && run.repository.full_name.toLowerCase() === repository.toLowerCase(),
      'run_identity_mismatch', {
        expected: { repository, run_id: Number(runId), run_attempt: Number(runAttempt) },
        observed: { repository: run.repository?.full_name, run_id: run.id, run_attempt: run.run_attempt },
        next_action: 'Verify the current repository, run and attempt; do not reuse another run.',
      },
    );
    const workflowPath = upstream + '/.github/workflows/' + workflowFile;
    requireValue(Array.isArray(run.referenced_workflows), 'workflow_reference_missing', {
      expected: workflowPath + '@' + workflowRef,
      observed: typeof run.referenced_workflows,
      next_action: 'Check GitHub Actions run metadata before starting a new run.',
    });
    const references = run.referenced_workflows.filter((entry) =>
      entry.path === workflowPath + '@' + workflowRef && entry.ref === 'refs/tags/' + workflowRef);
    requireValue(references.length === 1 && sha.test(references[0].sha), 'workflow_reference_missing', {
      expected: workflowPath + '@' + workflowRef,
      observed_matches: references.length,
      next_action: 'Start a new caller run with the documented reusable workflow reference.',
    });
    selected.workflow_sha = references[0].sha;

    const { data: ref } = await github('git_alias', '/repos/' + upstream + '/git/ref/tags/' + workflowRef);
    requireValue(ref.ref === 'refs/tags/' + workflowRef && ref.object?.type === 'tag' && sha.test(ref.object.sha),
      'invalid_git_alias', {
        expected: { ref: 'refs/tags/' + workflowRef, type: 'tag' },
        observed: { ref: ref.ref, type: ref.object?.type, sha: ref.object?.sha },
      });
    const { data: tag } = await github('git_tag', '/repos/' + upstream + '/git/tags/' + ref.object.sha);
    requireValue(tag.tag === workflowRef && tag.object?.type === 'commit' && sha.test(tag.object.sha) &&
      typeof tag.message === 'string' && /^v1\.\d+\.\d+-rc\.\d+$/.test(tag.message.trim()), 'invalid_git_tag', {
        expected: { tag: workflowRef, type: 'commit', release: 'v1.<minor>.<patch>-rc.<number>' },
        observed: { tag: tag.tag, type: tag.object?.type, sha: tag.object?.sha, release: tag.message },
      });
    selected.release = tag.message.trim();
    selected.source_sha = tag.object.sha;
    requireValue(selected.source_sha === selected.workflow_sha, 'workflow_source_mismatch', {
      expected: selected.workflow_sha, observed: selected.source_sha,
      next_action: 'The channel advanced after workflow selection; start a new run.',
    });

    const { data: release } = await github('release', '/repos/' + upstream + '/releases/tags/' + selected.release);
    requireValue(release.tag_name === selected.release && release.draft === false &&
      release.prerelease === true && Boolean(release.published_at) && Number.isSafeInteger(release.id),
    'release_unpublished', {
      expected: { tag: selected.release, draft: false, prerelease: true, published: true },
      observed: { tag: release.tag_name, draft: release.draft, prerelease: release.prerelease, published_at: release.published_at },
      next_action: 'Wait for complete prerelease publication, then start a new run.',
    });
    selected.release_id = release.id;
    requireValue(Array.isArray(release.assets), 'receipt_missing', {
      expected: 'release assets array', observed: typeof release.assets,
    });
    const assets = release.assets.filter((asset) => asset.name === 'publication.json');
    requireValue(assets.length === 1 && Number.isSafeInteger(assets[0].id), 'receipt_missing', {
      next_action: 'Wait for the release publication receipt, then start a new run.',
    });
    const { data: receipt } = await github('receipt',
      '/repos/' + upstream + '/releases/assets/' + assets[0].id, { accept: 'application/octet-stream' });
    requireValue(receipt.version === 1 && receipt.release_id === release.id &&
      receipt.tag === selected.release && receipt.source_sha === selected.source_sha &&
      receipt.image === image && digest.test(receipt.digest), 'receipt_mismatch', {
      expected: { version: 1, release_id: release.id, tag: selected.release, source_sha: selected.source_sha, image },
      observed: { version: receipt.version, release_id: receipt.release_id, tag: receipt.tag, source_sha: receipt.source_sha, image: receipt.image, digest: receipt.digest },
      next_action: 'Inspect the upstream publication receipt; do not select a substitute image.',
    });
    selected.receipt_digest = receipt.digest;
    const { data: authorization } = await json('registry_auth',
      authUrl + '?service=ghcr.io&scope=repository:' + upstream + ':pull');
    requireValue(typeof authorization.token === 'string' && authorization.token.length > 0, 'registry_auth_invalid');
    secrets.add(authorization.token);
    const { buffer, headers } = await request('registry_manifest',
      registryUrl + '/v2/' + upstream + '/manifests/' + imageTag, {
        authorization: authorization.token,
        accept: 'application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json',
      });
    const actualDigest = 'sha256:' + createHash('sha256').update(buffer).digest('hex');
    selected.registry_digest = actualDigest;
    requireValue(actualDigest === receipt.digest &&
      (!headers.has('docker-content-digest') || headers.get('docker-content-digest') === actualDigest),
    'image_digest_mismatch', {
      expected: receipt.digest, observed: { body_digest: actualDigest, header_digest: headers.get('docker-content-digest') },
      next_action: 'Wait for matching channel aliases, then start a new run.',
    });
    let manifest;
    try {
      manifest = JSON.parse(buffer.toString('utf8'));
    } catch {
      throw new ChannelError('invalid_manifest', { expected: 'JSON image index' });
    }
    requireValue(manifest?.schemaVersion === 2 && Array.isArray(manifest.manifests) &&
      ['application/vnd.oci.image.index.v1+json', 'application/vnd.docker.distribution.manifest.list.v2+json'].includes(manifest.mediaType),
    'invalid_manifest', { expected: 'multi-architecture image index', observed: manifest?.mediaType });
    const runtimeImage = image + '@' + actualDigest;
    log('info', 'orm.channel.selected', { runtime_image: runtimeImage, outcome: 'completed' });
    core.setOutput('runtime-image', runtimeImage);
    return runtimeImage;
  } catch (error) {
    log('error', 'orm.channel.failed', {
      outcome: 'failed',
      next_action: 'Correct the reported upstream metadata or access failure, then start a new run.',
      ...(error instanceof ChannelError ? error.details : {
        cause: 'invalid_response', error_name: error.name, body: error.stack ?? error.message,
        next_action: 'Inspect upstream metadata types before starting a new run.',
      }),
    });
    return undefined;
  }
}
