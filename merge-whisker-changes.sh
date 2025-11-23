#!/bin/bash
set -euo pipefail

# --- CONFIGURATION ---
COPILOT_BRANCH="oleks-whisker-improvements-backend"
UI_REMOTE="ronanc-tigera"
UI_REPO_URL="https://github.com/ronanc-tigera/calico.git"
UI_BRANCH="whisker-ui-new-features"
BASE_BRANCH="origin/master"
COMBINED_PREFIX="oleks-whisker-improvements-front-back"

# --- FUNCTIONS ---
function ensure_remote_exists() {
  local remote_name="$1"
  local remote_url="$2"
  if ! git remote | grep -q "^${remote_name}$"; then
    echo "Adding remote ${remote_name}..."
    git remote add "${remote_name}" "${remote_url}"
  fi
}

function fetch_all() {
  echo "Fetching branches..."
  git fetch origin "$COPILOT_BRANCH" --force
  git fetch "$UI_REMOTE" "$UI_BRANCH" --force
}

function next_version_name() {
  local prefix="$1"
  local latest_version
  latest_version=$(git branch --list "${prefix}-v*" | sed -E "s/.*-v([0-9]+).*/\1/" | sort -n | tail -1)
  if [[ -z "$latest_version" ]]; then
    echo "${prefix}-v1"
  else
    echo "${prefix}-v$((latest_version + 1))"
  fi
}

# --- MAIN EXECUTION ---
cd "$(git rev-parse --show-toplevel)"

echo "🧹 Stashing local changes (if any)..."
git stash push -u -m "temp-stash-for-merge" >/dev/null 2>&1 || true

ensure_remote_exists "$UI_REMOTE" "$UI_REPO_URL"
fetch_all

NEW_BRANCH=$(next_version_name "$COMBINED_PREFIX")
echo "Creating new branch: $NEW_BRANCH"

echo "Checking out base branch: $BASE_BRANCH"
git checkout -B "$NEW_BRANCH" "$BASE_BRANCH"

echo "Merging Copilot backend branch (preferring theirs)..."
git merge --no-ff "origin/$COPILOT_BRANCH" -X theirs -m "Merge Copilot backend branch: $COPILOT_BRANCH" || {
  echo "⚠️ Merge conflicts detected (Copilot). Please resolve manually."
  exit 1
}

echo "Merging UI engineer fork branch (preferring theirs)..."
git merge --no-ff "${UI_REMOTE}/${UI_BRANCH}" -X theirs -m "Merge UI engineer fork branch: ${UI_REMOTE}/${UI_BRANCH}" || {
  echo "⚠️ Merge conflicts detected (UI). Please resolve manually."
  exit 1
}

echo "✅ Combined branch '$NEW_BRANCH' created successfully."
echo "Restoring any local stashed changes..."
git stash pop >/dev/null 2>&1 || true

echo "You can now review your merged branch:"
echo "  git log --oneline --graph --decorate -10"
echo "  git push origin $NEW_BRANCH  # (optional)"
