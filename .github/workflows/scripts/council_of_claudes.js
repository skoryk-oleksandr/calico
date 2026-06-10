// Council of Claudes — fan out a PR's diff to persona-based review agents on
// the kagent cluster (agents.tigera.ai) and post each persona's feedback back
// to the PR as a distinct review. Invoked from council_of_claudes.yml via
// actions/github-script.
//
// Each persona is gated on its own *_AGENT_URL / *_AGENT_TOKEN env vars
// (sourced from repo secrets). Any persona whose vars are unset is skipped
// with a warning, so the workflow is usable with only one persona configured.
//
// Agent call contract (verified against tigera/agents-menagerie + live tests):
//   POST <url> with Bearer <token>, JSON-RPC 2.0.
//   The public endpoint enforces a ~30s cap on a synchronous response, but a
//   full review generation takes longer, so we submit ASYNCHRONOUSLY:
//     1. message/send with params.configuration.blocking=false
//        -> returns immediately with result.id (a task id), state "submitted".
//     2. poll tasks/get { id } every few seconds until status.state is
//        terminal; on "completed", the review is result.artifacts[0].parts[0].text.
//   Each individual call is fast, so none hits the 30s cap.

// Each persona has a distinct in-comment identity: an emoji + a GitHub alert
// type (NOTE/TIP/CAUTION render as blue/green/red callouts) + a tagline, so the
// three are visually distinguishable at a glance even though they all post as
// github-actions[bot].
const PERSONAS = [
  { key: 'correctness',     title: 'Correctness',             emoji: '🔎', accent: 'NOTE',    tagline: 'bugs · completeness · concurrency · edge cases', urlEnv: 'CORRECTNESS_AGENT_URL',     tokenEnv: 'CORRECTNESS_AGENT_TOKEN' },
  { key: 'maintainability', title: 'Maintainability & Tests', emoji: '🧪', accent: 'TIP',     tagline: 'simplicity · tests · docs · idioms',            urlEnv: 'MAINTAINABILITY_AGENT_URL', tokenEnv: 'MAINTAINABILITY_AGENT_TOKEN' },
  { key: 'security',        title: 'Security',                emoji: '🛡️', accent: 'CAUTION', tagline: 'validation · secrets · authz · isolation',      urlEnv: 'SECURITY_AGENT_URL',        tokenEnv: 'SECURITY_AGENT_TOKEN' },
];

// Cap the diff we send to keep within model context. Large-PR handling is out
// of scope for the hackathon; oversized diffs are truncated with a note.
const MAX_DIFF_BYTES = 100 * 1024;

// Async task polling parameters.
const POLL_INTERVAL_MS = 5_000;
const MAX_POLL_MS = 10 * 60 * 1000; // give a slow generation up to 10 minutes
const RPC_TIMEOUT_MS = 30_000;      // each individual call is quick

const sleep = ms => new Promise(res => setTimeout(res, ms));

// Sanitize agent-controlled text before logging so it cannot inject
// ::workflow-command:: strings into the Actions log (matches the guard in
// cherry_pick_candidate.js).
const safe = s => String(s ?? '').replace(/::/g, ':​:');

// Strip a single markdown fence that wraps the *entire* response, while
// leaving any inner code/suggestion fences intact.
function stripOuterFence(text) {
  const t = (text || '').trim();
  const m = t.match(/^```(?:markdown|md)?\s*\n([\s\S]*?)\n```$/);
  return (m ? m[1] : t).trim();
}

// One JSON-RPC POST. Throws on non-2xx, network error, or a JSON-RPC error
// payload (a 200 response carrying an `error` field — otherwise it would be
// silently mistaken for a missing result).
async function rpc(url, token, body) {
  const r = await fetch(url, {
    method: 'POST',
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(RPC_TIMEOUT_MS),
  });
  if (!r.ok) throw new Error(`http ${r.status}`);
  const json = await r.json();
  if (json.error) {
    throw new Error(`json-rpc error ${json.error.code ?? '?'}: ${json.error.message ?? 'unknown'}`);
  }
  return json;
}

// Pull the agent's text out of a completed task: prefer the first artifact's
// text part, fall back to the final status message.
function extractText(task) {
  const fromArtifacts = task?.artifacts?.[0]?.parts?.find(p => p.text)?.text;
  if (fromArtifacts) return fromArtifacts;
  return task?.status?.message?.parts?.find(p => p.text)?.text || '';
}

// Submit the review asynchronously and poll to completion. Returns the
// persona's markdown review, or null on failure.
async function reviewWithAgent({ core, title, url, token, messageText, msgId }) {
  // 1. Submit non-blocking (up to 3 attempts with 5s/10s backoff).
  let taskId = null;
  for (let attempt = 1; attempt <= 3; attempt++) {
    try {
      const sub = await rpc(url, token, {
        jsonrpc: '2.0', id: msgId, method: 'message/send',
        params: {
          message: { messageId: msgId, role: 'user', kind: 'message', parts: [{ kind: 'text', text: messageText }] },
          configuration: { blocking: false, acceptedOutputModes: ['application/json', 'text/plain'] },
        },
      });
      taskId = sub.result?.id;
      if (taskId) break;
      core.info(`  ${title}: submit returned no task id (attempt ${attempt})`);
    } catch (e) {
      core.info(`  ${title}: submit attempt ${attempt} failed (${safe(e.message)})`);
    }
    if (attempt < 3) await sleep(attempt * 5000);
  }
  if (!taskId) return null;

  // 2. Poll tasks/get until the task reaches a terminal state. Transient poll
  //    errors are tolerated (keep polling until the overall deadline).
  const start = Date.now();
  while (Date.now() - start < MAX_POLL_MS) {
    await sleep(POLL_INTERVAL_MS);
    let task;
    try {
      const g = await rpc(url, token, { jsonrpc: '2.0', id: `${msgId}-get`, method: 'tasks/get', params: { id: taskId } });
      task = g.result;
    } catch (e) {
      core.info(`  ${title}: poll error (${safe(e.message)}), will retry`);
      continue;
    }
    const state = task?.status?.state;
    if (state === 'completed') return stripOuterFence(extractText(task));
    if (state === 'failed' || state === 'canceled' || state === 'rejected') {
      core.warning(`${title}: task ${state}`);
      return null;
    }
    // "submitted" / "working" -> keep polling
  }
  core.warning(`${title}: task did not complete within ${MAX_POLL_MS / 1000}s`);
  return null;
}

module.exports = async ({ github, context, core }) => {
  const { owner, repo } = context.repo;
  const pr = context.payload.pull_request;
  if (!pr) {
    core.warning('No pull_request in payload, skipping');
    return;
  }
  const pull_number = pr.number;

  // 1. Determine which personas are actually configured.
  const active = PERSONAS
    .map(p => ({ ...p, url: process.env[p.urlEnv], token: process.env[p.tokenEnv] }))
    .filter(p => {
      if (p.url && p.token) return true;
      core.warning(`${p.title}: ${p.urlEnv}/${p.tokenEnv} not set, skipping persona`);
      return false;
    });
  if (active.length === 0) {
    core.warning('No persona agents configured, nothing to do');
    return;
  }

  // 2. Fetch the unified diff inline (size-guarded).
  const diffResp = await github.rest.pulls.get({
    owner, repo, pull_number, mediaType: { format: 'diff' },
  });
  let diff = diffResp.data;
  let truncated = false;
  if (Buffer.byteLength(diff, 'utf8') > MAX_DIFF_BYTES) {
    // Slice on a byte boundary; TextDecoder with stream:true drops a trailing
    // partial multi-byte sequence rather than emitting a replacement char.
    const buf = Buffer.from(diff, 'utf8').subarray(0, MAX_DIFF_BYTES);
    diff = new TextDecoder('utf-8').decode(buf, { stream: true });
    truncated = true;
    core.warning(`diff exceeds ${MAX_DIFF_BYTES} bytes, truncated (large-PR handling out of scope)`);
  }

  const messageText =
    `PR #${pull_number}: ${pr.title}\n\n` +
    `Description:\n${pr.body || '(none)'}\n\n` +
    (truncated ? '(NOTE: the diff below was truncated for size.)\n\n' : '') +
    `Unified diff:\n${diff}`;

  // 3. Fan out to all configured personas in parallel; isolate failures.
  core.info(`Reviewing PR #${pull_number} with ${active.length} persona(s): ${active.map(p => p.title).join(', ')}`);
  const results = await Promise.all(active.map(async p => {
    const msgId = `council-${p.key}-${pull_number}-${process.env.GITHUB_RUN_ID}`;
    const review = await reviewWithAgent({ core, title: p.title, url: p.url, token: p.token, messageText, msgId });
    if (!review) {
      core.warning(`${p.title}: no review produced, skipping`);
      return null;
    }
    core.info(`${p.title}: received ${review.length} chars of feedback`);
    return { persona: p, review };
  }));

  // 4. Post one *sticky* PR review per persona. Each body carries a hidden
  //    per-persona marker; if a prior review with that marker exists we update
  //    it in place (so re-runs replace rather than accumulate), otherwise we
  //    create a new one. We replace (not merge): each run reflects the current
  //    diff. Staying in the reviews API also keeps the door open for inline
  //    line-anchored comments later (a comments[] array on the review).
  const existing = await github.paginate(github.rest.pulls.listReviews, { owner, repo, pull_number });
  let posted = 0;
  for (const res of results) {
    if (!res) continue;
    const p = res.persona;
    const marker = `<!-- council-of-claudes:${p.key} -->`;
    const body =
      `### ${p.emoji} Council of Claudes — ${p.title}\n\n` +
      `> [!${p.accent}]\n> **${p.title} lens** · ${p.tagline}\n\n` +
      `${res.review}\n\n` +
      `<sub>🤖 Council of Claudes</sub>\n${marker}`;
    // Most recent prior review carrying this persona's marker, if any.
    const prior = existing.filter(r => (r.body || '').includes(marker)).pop();
    try {
      if (prior) {
        await github.rest.pulls.updateReview({ owner, repo, pull_number, review_id: prior.id, body });
        core.info(`${res.persona.title}: review updated in place (#${prior.id})`);
      } else {
        await github.rest.pulls.createReview({
          owner, repo, pull_number, commit_id: pr.head.sha, event: 'COMMENT', body,
        });
        core.info(`${res.persona.title}: review posted (new)`);
      }
      posted++;
    } catch (e) {
      core.warning(`${res.persona.title}: failed to post/update review (${safe(e.message)})`);
    }
  }
  core.info(`Done: ${posted}/${active.length} persona review(s) posted to PR #${pull_number}`);
};
