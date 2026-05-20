# Fork PR permissions test (2026-05-20)

Smoke test for the backport-label workflows when triggered by a cross-fork
PR. The head repo (`skoryk-oleksandr/calico`) and the base repo
(`tigera/calico-oss-test`) are sibling forks in the `projectcalico/calico`
fork network, so this PR runs with a read-only `GITHUB_TOKEN` exactly as an
external contributor's PR would.

Expected behavior:
- `check_backport_labels.yml` runs and computes the gate from live PR labels.
- `createLabel` attempts return 403 → `labelCreationBlocked = true`, loop
  breaks cleanly; no phantom labels in the snapshot list.
- Comment writes return 403 → caught and logged via `core.info`, gate
  decision is unaffected.
- Gate fails until a maintainer (or any Calico team member) applies
  `skip-releases-backport` or a `backport/release-vX.Y` label; once a label
  is applied, the `labeled` event re-fires the workflow and the gate passes.
