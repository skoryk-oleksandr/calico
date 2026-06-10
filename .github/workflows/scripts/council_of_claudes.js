// Council of Claudes — fan out a PR's diff to persona-based review agents on
// the kagent cluster (agents.tigera.ai) and post each persona's feedback back
// to the PR. Invoked from council_of_claudes.yml via actions/github-script.
//
// Each persona posts a HYBRID review:
//   - a sticky summary review (verdict + non-line findings), updated in place;
//   - inline review comments anchored to specific diff lines.
//
// Each persona is gated on its own *_AGENT_URL / *_AGENT_TOKEN env vars
// (sourced from repo secrets). Any persona whose vars are unset is skipped.
//
// Agent call contract (verified against tigera/agents-menagerie + live tests):
//   POST <url> with Bearer <token>, JSON-RPC 2.0. The public endpoint caps a
//   synchronous response at ~30s, so we submit ASYNCHRONOUSLY:
//     1. message/send with params.configuration.blocking=false -> task id.
//     2. poll tasks/get until terminal; on "completed" read
//        result.artifacts[0].parts[0].text.
//
// Persona output contract (markdown; see scripts/personas/*.md):
//   <summary markdown>
//   <!-- coc-finding file="path" line="N" -->
//   <finding markdown>
//   ...one marker per line-specific finding. Text before the first marker is
//   the summary. The diff sent to personas is annotated with [L<n>] new-side
//   line numbers so cited lines can be anchored.

// Each persona has a distinct in-comment identity: an emoji + a GitHub alert
// type (NOTE/TIP/CAUTION render as blue/green/red callouts) + a tagline.
const PERSONAS = [
  { key: 'correctness',     title: 'Correctness',             emoji: '🔎', accent: 'NOTE',    tagline: 'bugs · completeness · concurrency · edge cases', urlEnv: 'CORRECTNESS_AGENT_URL',     tokenEnv: 'CORRECTNESS_AGENT_TOKEN' },
  { key: 'maintainability', title: 'Maintainability & Tests', emoji: '🧪', accent: 'TIP',     tagline: 'simplicity · tests · docs · idioms',            urlEnv: 'MAINTAINABILITY_AGENT_URL', tokenEnv: 'MAINTAINABILITY_AGENT_TOKEN' },
  { key: 'security',        title: 'Security',                emoji: '🛡️', accent: 'CAUTION', tagline: 'validation · secrets · authz · isolation',      urlEnv: 'SECURITY_AGENT_URL',        tokenEnv: 'SECURITY_AGENT_TOKEN' },
];

// Cap the diff we send to keep within model context. Large-PR handling is out
// of scope for the hackathon; oversized diffs are truncated with a note.
const MAX_DIFF_BYTES = 100 * 1024;

// Calico's own context files (conventions / architecture) used to ground the
// review, read from the checked-out workspace. `always` files are injected on
// every PR; the rest only when a changed path matches one of `whenPathStartsWith`.
const CONTEXT_FILES = [
  { file: '.claude/CLAUDE.md',                                always: true },
  { file: '.github/copilot-instructions.md',                  always: true },
  { file: 'felix/CLAUDE.md',                                  whenPathStartsWith: ['felix/'] },
  { file: 'goldmane/CLAUDE.md',                               whenPathStartsWith: ['goldmane/'] },
  { file: '.github/instructions/bpf.instructions.md',         whenPathStartsWith: ['felix/bpf'] },
  { file: '.github/instructions/goldmane.instructions.md',    whenPathStartsWith: ['goldmane/'] },
  { file: '.github/instructions/helm-charts.instructions.md', whenPathStartsWith: ['charts/'] },
];
const MAX_CONTEXT_BYTES = 80 * 1024; // backstop so injected context can't blow up the message

// Read the relevant Calico context files from the workspace and format them as
// an authoritative preamble, scoped to the paths this PR touches.
function gatherContext(core, touchedPaths) {
  const fs = require('fs');
  const path = require('path');
  const ws = process.env.GITHUB_WORKSPACE || '.';
  const paths = [...touchedPaths];
  const chosen = CONTEXT_FILES.filter(c =>
    c.always || (c.whenPathStartsWith || []).some(pre => paths.some(tp => tp.startsWith(pre))));
  let total = 0;
  const blocks = [];
  const included = [];
  for (const c of chosen) {
    let content;
    try { content = fs.readFileSync(path.join(ws, c.file), 'utf8').trim(); }
    catch (e) { core.info(`  context: ${c.file} unavailable (${e.message}), skipping`); continue; }
    if (total + content.length > MAX_CONTEXT_BYTES) { core.info(`  context: budget reached, skipping ${c.file}`); continue; }
    total += content.length;
    blocks.push(`### ${c.file}\n\n${content}`);
    included.push(c.file);
  }
  if (!blocks.length) return '';
  core.info(`Injected ${blocks.length} Calico context file(s) (${total} bytes): ${included.join(', ')}`);
  return `## Project context — authoritative Calico conventions; apply these in your review\n\n${blocks.join('\n\n---\n\n')}\n\n---\n\n`;
}

// Async task polling parameters.
const POLL_INTERVAL_MS = 5_000;
const MAX_POLL_MS = 10 * 60 * 1000; // give a slow generation up to 10 minutes
const RPC_TIMEOUT_MS = 30_000;      // each individual call is quick

const sleep = ms => new Promise(res => setTimeout(res, ms));

// Sanitize agent-controlled text before logging so it cannot inject
// ::workflow-command:: strings into the Actions log.
const safe = s => String(s ?? '').replace(/::/g, ':​:');

// Strip a single markdown fence that wraps the *entire* response, while
// leaving any inner code/suggestion fences intact.
function stripOuterFence(text) {
  const t = (text || '').trim();
  const m = t.match(/^```(?:markdown|md)?\s*\n([\s\S]*?)\n```$/);
  return (m ? m[1] : t).trim();
}

// Annotate a unified diff with explicit new-file (RIGHT side) line numbers so
// the model can cite exact lines, and build the set of anchorable lines per
// file. Added ('+') and context (' ') lines get a [L<n>] prefix and are
// anchorable on the RIGHT side; deleted ('-') lines are left unmarked.
function annotateDiff(diff) {
  const anchors = new Map(); // path -> Set<number>
  const out = [];
  let path = null;
  let newLine = 0;
  const addAnchor = n => {
    if (!path) return;
    if (!anchors.has(path)) anchors.set(path, new Set());
    anchors.get(path).add(n);
  };
  for (const line of (diff || '').split('\n')) {
    const plus = line.match(/^\+\+\+ b\/(.*)$/);
    if (plus) { path = plus[1]; out.push(line); continue; }
    if (line.startsWith('+++ ')) { path = null; out.push(line); continue; } // e.g. /dev/null
    const hunk = line.match(/^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@/);
    if (hunk) { newLine = parseInt(hunk[1], 10); out.push(line); continue; }
    if (line.startsWith('diff --git') || line.startsWith('--- ') || line.startsWith('index ') ||
        line.startsWith('new file') || line.startsWith('deleted file') || line.startsWith('rename ') ||
        line.startsWith('similarity ') || line.startsWith('\\ ')) {
      out.push(line); continue;
    }
    if (line.startsWith('+')) { addAnchor(newLine); out.push(`[L${newLine}] ${line}`); newLine++; }
    else if (line.startsWith('-')) { out.push(`[----] ${line}`); }
    else if (line.startsWith(' ')) { addAnchor(newLine); out.push(`[L${newLine}] ${line}`); newLine++; } // context line
    else { out.push(line); } // trailing empty split element or unhandled metadata — never anchor/advance
  }
  return { annotated: out.join('\n'), anchors };
}

// Parse a persona's markdown into a summary plus line-anchored findings. Text
// before the first <!-- coc-finding ... --> marker is the summary; each marker
// plus the markdown after it (until the next marker) is one finding.
function parseFindings(text) {
  const re = /<!--\s*coc-finding\s+file="([^"]*)"\s+line="(\d+)"\s*-->/g;
  const matches = [];
  let m;
  while ((m = re.exec(text || '')) !== null) {
    matches.push({ start: m.index, end: re.lastIndex, file: m[1], line: parseInt(m[2], 10) });
  }
  if (matches.length === 0) return { summary: (text || '').trim(), findings: [] };
  const summary = text.slice(0, matches[0].start).trim();
  const findings = matches.map((mm, i) => ({
    file: mm.file,
    line: mm.line,
    body: text.slice(mm.end, i + 1 < matches.length ? matches[i + 1].start : text.length).trim(),
  }));
  return { summary, findings };
}

// One JSON-RPC POST. Throws on non-2xx, network error, or a JSON-RPC error
// payload (a 200 response carrying an `error` field).
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

// Pull the agent's text out of a completed task.
function extractText(task) {
  const fromArtifacts = task?.artifacts?.[0]?.parts?.find(p => p.text)?.text;
  if (fromArtifacts) return fromArtifacts;
  return task?.status?.message?.parts?.find(p => p.text)?.text || '';
}

// Submit the review asynchronously and poll to completion. Returns the
// persona's markdown review, or null on failure.
async function reviewWithAgent({ core, title, url, token, messageText, msgId }) {
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
  const commit_id = pr.head.sha;

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

  // 2. Fetch the unified diff inline (size-guarded), then annotate with line
  //    numbers and build the anchorable-line index.
  const diffResp = await github.rest.pulls.get({
    owner, repo, pull_number, mediaType: { format: 'diff' },
  });
  let diff = diffResp.data;
  let truncated = false;
  if (Buffer.byteLength(diff, 'utf8') > MAX_DIFF_BYTES) {
    const buf = Buffer.from(diff, 'utf8').subarray(0, MAX_DIFF_BYTES);
    diff = new TextDecoder('utf-8').decode(buf, { stream: true });
    truncated = true;
    core.warning(`diff exceeds ${MAX_DIFF_BYTES} bytes, truncated (large-PR handling out of scope)`);
  }
  const { annotated, anchors } = annotateDiff(diff);

  // Ground the review in Calico's own context files, scoped to the paths this PR
  // touches. Parse both new (+++ b/) and old (--- a/) file headers so delete-only
  // changes still match path-scoped context; /dev/null headers lack the a//b/
  // prefix and are naturally excluded.
  const touchedPaths = new Set(
    (diff.match(/^(?:\+\+\+ b|--- a)\/.+$/gm) || []).map(l => l.replace(/^(?:\+\+\+ b|--- a)\//, '')),
  );
  const projectContext = gatherContext(core, touchedPaths);

  const messageText = projectContext +
    `PR #${pull_number}: ${pr.title}\n\n` +
    `Description:\n${pr.body || '(none)'}\n\n` +
    (truncated ? '(NOTE: the diff below was truncated for size.)\n\n' : '') +
    `The unified diff below is annotated: each new-side line is prefixed with its line number ` +
    `as [L<n>]. When a finding is tied to a specific line, cite that number in a coc-finding anchor.\n\n` +
    `Annotated unified diff:\n${annotated}`;

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

  // 4. Fetch existing reviews (for sticky summaries) and existing review
  //    comments (for inline cleanup) once. A comment "has replies" if some
  //    other comment is in_reply_to it — we never delete those.
  const existingReviews = await github.paginate(github.rest.pulls.listReviews, { owner, repo, pull_number });
  const existingComments = await github.paginate(github.rest.pulls.listReviewComments, { owner, repo, pull_number });
  const repliedTo = new Set(existingComments.map(c => c.in_reply_to_id).filter(id => id != null));

  let summaries = 0;
  let inlineTotal = 0;
  for (const res of results) {
    if (!res) continue;
    const p = res.persona;
    const inlineMarker = `<!-- council-of-claudes:inline:${p.key} -->`;
    const { summary, findings } = parseFindings(res.review);

    // Split findings into anchorable vs. unanchorable (folded into summary).
    const folded = [];
    const anchorable = [];
    for (const f of findings) {
      if (anchors.get(f.file)?.has(f.line)) anchorable.push(f);
      else folded.push(f);
    }

    // 4a. Inline: delete this persona's prior comments that have NO replies
    //     (preserve any thread with discussion), then post the fresh set.
    const priorInline = existingComments.filter(c => (c.body || '').includes(inlineMarker));
    for (const c of priorInline) {
      if (repliedTo.has(c.id)) continue; // keep — has a discussion thread
      try {
        await github.rest.pulls.deleteReviewComment({ owner, repo, comment_id: c.id });
      } catch (e) {
        core.info(`  ${p.title}: could not delete inline #${c.id} (${safe(e.message)})`);
      }
    }
    let inline = 0;
    for (const f of anchorable) {
      const body = `${p.emoji} **${p.title}** — ${f.body}\n\n${inlineMarker}`;
      try {
        await github.rest.pulls.createReviewComment({
          owner, repo, pull_number, commit_id, path: f.file, line: f.line, side: 'RIGHT', body,
        });
        inline++;
      } catch (e) {
        // Line wasn't commentable after all — fold it into the summary so it isn't lost.
        core.info(`  ${p.title}: inline anchor failed at ${safe(f.file)}:${f.line} (${safe(e.message)}), folding into summary`);
        folded.push(f);
      }
    }
    inlineTotal += inline;

    // 4b. Summary: sticky review, updated in place. Folded findings (no valid
    //     line anchor) are listed here so nothing is lost.
    const marker = `<!-- council-of-claudes:${p.key} -->`;
    let body =
      `### ${p.emoji} Council of Claudes — ${p.title}\n\n` +
      `> [!${p.accent}]\n> **${p.title} lens** · ${p.tagline}\n\n` +
      `${summary || '_No summary provided._'}\n`;
    if (folded.length) {
      body += `\n**Other notes** (not anchored to a diff line):\n` +
        folded.map(f => `- \`${f.file}:${f.line}\` — ${f.body.replace(/\s*\n+\s*/g, ' ')}`).join('\n') + '\n';
    }
    body += `\n<sub>🤖 Council of Claudes · ${inline} inline comment(s)</sub>\n${marker}`;

    const prior = existingReviews.filter(r => (r.body || '').includes(marker)).pop();
    try {
      if (prior) {
        await github.rest.pulls.updateReview({ owner, repo, pull_number, review_id: prior.id, body });
        core.info(`${p.title}: summary updated in place (#${prior.id}), ${inline} inline`);
      } else {
        await github.rest.pulls.createReview({ owner, repo, pull_number, commit_id, event: 'COMMENT', body });
        core.info(`${p.title}: summary posted (new), ${inline} inline`);
      }
      summaries++;
    } catch (e) {
      core.warning(`${p.title}: failed to post/update summary (${safe(e.message)})`);
    }
  }
  core.info(`Done: ${summaries}/${active.length} summaries, ${inlineTotal} inline comment(s) on PR #${pull_number}`);
};
