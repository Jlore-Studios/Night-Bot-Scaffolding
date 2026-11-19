#!/usr/bin/env bash
# Copies the night bot into a repository of yours: .nightbot/ (the harness, its configuration,
# prompts and machine scripts), the two bot workflows, the selftest workflow and the build action.
# It fills in .nightbot/config.json from the repository and the flags, and changes nothing else.
#
#   ./install.sh /path/to/your/repo --bot-login my-bot --operator my-login [--ci-workflow CI]
#   ./install.sh /path/to/your/repo --upgrade
#
# --bot-login    the GitHub account (or <app-slug>[bot]) the bot comments and pushes as
# --operator     the person the bot asks for decisions and @-mentions; always trusted
# --repo         OWNER/NAME (default: read from the target's origin remote)
# --ci-workflow  the `name:` of the workflow whose failures the bot fixes (default: CI)
# --merge-queue  merge through a merge queue: `nightbot setup --repo-settings` then writes a ruleset
#                with the required checks and the queue (your CI must run on merge_group)
# --plan-gate    plan every item first; build nothing until an approver approves the plan
# --upstream R --fork F
#                fork mode (with the plan gate): build on the fork F of R and deliver to R by pull
#                request, which a person merges (.nightbot/docs/fork-mode.md)
# --upgrade      replace only the harness, the machine scripts and the build action, keeping your
#                configuration, prompts and workflows (diff them against template/ yourself)
# --force        overwrite an earlier install entirely (your edits to its files are lost)
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
target=""
bot_login=""
operator=""
repo=""
ci_workflow="CI"
merge_queue=false
plan_gate=false
upstream=""
fork=""
mode=install
while [ $# -gt 0 ]; do
  case "$1" in
    --bot-login) bot_login="${2:?--bot-login needs a value}"; shift 2 ;;
    --operator) operator="${2:?--operator needs a value}"; shift 2 ;;
    --repo) repo="${2:?--repo needs a value}"; shift 2 ;;
    --ci-workflow) ci_workflow="${2:?--ci-workflow needs a value}"; shift 2 ;;
    --merge-queue) merge_queue=true; shift ;;
    --plan-gate) plan_gate=true; shift ;;
    --upstream) upstream="${2:?--upstream needs OWNER/NAME}"; plan_gate=true; shift 2 ;;
    --fork) fork="${2:?--fork needs OWNER/NAME}"; shift 2 ;;
    --upgrade) mode=upgrade; shift ;;
    --force) mode=force; shift ;;
    -h|--help) sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    -*) echo "unknown flag $1" >&2; exit 2 ;;
    *) [ -z "$target" ] || { echo "one target directory only" >&2; exit 2; }; target="$1"; shift ;;
  esac
done
[ -n "$target" ] || { echo "usage: ./install.sh /path/to/your/repo --bot-login LOGIN --operator LOGIN" >&2; exit 2; }
target="$(cd "$target" && pwd)"
git -C "$target" rev-parse --show-toplevel >/dev/null 2>&1 || { echo "$target is not a git repository" >&2; exit 1; }

# Copies paths of the template into the target, leaving out what a test run or an editor left.
copy() {
  (cd "$here/template" && tar cf - --exclude='__pycache__' --exclude='*.pyc' --exclude='.DS_Store' "$@") \
    | (cd "$target" && tar xf -)
}

if [ "$mode" = upgrade ]; then
  [ -f "$target/.nightbot/config.json" ] || { echo "no .nightbot/config.json in $target: install first" >&2; exit 1; }
  rm -rf "$target/.nightbot/harness" "$target/.nightbot/machine" "$target/.github/actions/nightbot"
  copy .nightbot/harness .nightbot/machine .github/actions/nightbot
  echo "Upgraded the harness, the machine scripts and the build action in $target."
  echo "Compare your prompts, workflows and config with template/ for anything new:"
  echo "  diff -ru $here/template/.nightbot/prompts $target/.nightbot/prompts"
  echo "Then run the selftest: cd $target/.nightbot/harness && go test ./... && go run ./cmd/nightbot validate"
  exit 0
fi

login_re='^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\[bot\])?$'
[ -n "$bot_login" ] || { echo "--bot-login is required: the account the bot acts as (README.md, Accounts)" >&2; exit 2; }
[ -n "$operator" ] || { echo "--operator is required: your GitHub login" >&2; exit 2; }
[[ "$bot_login" =~ $login_re ]] || { echo "--bot-login $bot_login is not a GitHub login" >&2; exit 2; }
[[ "$operator" =~ $login_re ]] || { echo "--operator $operator is not a GitHub login" >&2; exit 2; }
if [ -z "$repo" ]; then
  url="$(git -C "$target" remote get-url origin 2>/dev/null || true)"
  repo="$(printf '%s' "$url" | sed -E 's#^(git@[^:]+:|ssh://[^/]+/|https?://[^/]+/)##; s#\.git$##')"
fi
[[ "$repo" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || { echo "could not tell OWNER/NAME from origin ($repo); pass --repo" >&2; exit 2; }
[[ "$ci_workflow" =~ ^[A-Za-z0-9\ _.-]+$ ]] || { echo "--ci-workflow: letters, digits, spaces, _ . - only" >&2; exit 2; }
repo_re='^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$'
if [ -n "$upstream$fork" ]; then
  [[ "$upstream" =~ $repo_re && "$fork" =~ $repo_re ]] || { echo "--upstream and --fork both take OWNER/NAME" >&2; exit 2; }
fi
branch="$(git -C "$target" symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null | sed 's#^origin/##' || true)"
[ -n "$branch" ] || branch="$(git -C "$target" branch --show-current)"
[ -n "$branch" ] || branch=main

files=(.nightbot .github/actions/nightbot .github/workflows/nightbot-events.yml
  .github/workflows/nightbot-run.yml .github/workflows/nightbot-selftest.yml)
if [ "$plan_gate" = true ]; then files+=(.github/ISSUE_TEMPLATE/nightbot-request.yml); fi
for f in "${files[@]}"; do
  if [ -e "$target/$f" ] && [ "$mode" != force ]; then
    echo "$f exists in $target already: --upgrade replaces the harness alone, --force everything" >&2
    exit 1
  fi
done
for f in "${files[@]}"; do rm -rf "${target:?}/$f"; done
mkdir -p "$target/.github/workflows" "$target/.github/actions" "$target/.github/ISSUE_TEMPLATE"
copy "${files[@]}"

config="$target/.nightbot/config.json"
sed -i.bak \
  -e "s#\"OWNER/REPO\"#\"$repo\"#" \
  -e "s#\"BOT_LOGIN\"#\"$bot_login\"#" \
  -e "s#\"OPERATOR_LOGIN\"#\"$operator\"#" \
  -e "s#\"default_branch\": \"main\"#\"default_branch\": \"$branch\"#" \
  -e "s#\"ci_workflow\": \"CI\"#\"ci_workflow\": \"$ci_workflow\"#" \
  -e "s#\"merge_queue\": false#\"merge_queue\": $merge_queue#" \
  -e "s#\"plan_gate\": false#\"plan_gate\": $plan_gate#" \
  "$config"
if [ -n "$upstream" ]; then
  block="{ \"repo\": \"$upstream\", \"fork\": \"$fork\", \"max_open_prs\": 2, \"reviewers\": [], \"auto_open\": { \"labels\": [], \"assigned\": true, \"max_open\": 2 } }"
  sed -i.bak -e "s#\"upstream\": null#\"upstream\": $block#" "$config"
fi
if [ "$plan_gate" = true ]; then
  request="$target/.github/ISSUE_TEMPLATE/nightbot-request.yml"
  sed -i.bak -e "s#UPSTREAM_REPO#${upstream:-$repo}#g" "$request"
  rm -f "$request.bak"
fi
rm -f "$config.bak"
events="$target/.github/workflows/nightbot-events.yml"
sed -i.bak -e "s#workflows: \[CI\]#workflows: [\"$ci_workflow\"]#" -e "s#'BOT_LOGIN'#'$bot_login'#" "$events"
rm -f "$events.bak"
selftest="$target/.github/workflows/nightbot-selftest.yml"
sed -i.bak -e "s#branches: \[main\]#branches: [$branch]#" "$selftest"
rm -f "$selftest.bak"

echo "Installed the night bot into $target ($repo, default branch $branch)."
ci_file="$(grep -rlsE "^name: *[\"']?${ci_workflow}[\"']? *$" "$target/.github/workflows" | head -n1 || true)"
if [ -z "$ci_file" ]; then
  echo "note: no workflow in .github/workflows is named \"$ci_workflow\"; pass --ci-workflow with your CI's name, or the bot never sees red CI."
elif [ "$merge_queue" = true ] && ! grep -q 'merge_group' "$ci_file"; then
  echo "note: ${ci_file#"$target"/} does not run on merge_group; add \"merge_group:\" to its \"on:\", or the merge queue waits for its checks for ever."
fi
cat <<EOF

Next (README.md, "Quick start", has the details):
  1. .nightbot/config.json: "checks" (your test commands) and "required_checks" (the CI checks
     $branch must pass before anything merges).
  2. .nightbot/providers.json: enable the subscriptions you have.
  3. Secrets: NIGHTBOT_GITHUB_TOKEN (or a GitHub App: the variable NIGHTBOT_APP_ID and the secret
     NIGHTBOT_APP_PRIVATE_KEY), and each enabled provider's login secret.
  4. Commit and push, then once, with an admin token:
       cd .nightbot/harness && GITHUB_TOKEN=\$(gh auth token) go run ./cmd/nightbot setup --repo-settings
     On a public repository, also allow pull_request_target for .github/workflows/nightbot-events.yml
     alone in the Actions event policies: GitHub blocks it there by default from 2026-12-02.
  5. Label an issue bot:build, or comment "/nightbot build --force" on it. With the plan gate, open
     an issue from the request template instead: the bot plans it, and an approver approves.
EOF
if [ -n "$upstream" ]; then
  cat <<EOF

Fork mode: the bot builds on $fork and delivers to $upstream (.nightbot/docs/fork-mode.md).
  - Step 4 runs with the bot's own token in fork mode (NIGHTBOT_GITHUB_TOKEN=...): setup changes the
    fork's settings, which only its owner can.
  - Upstream's default branch is taken to be main; set upstream.default_branch if it is not.
  - Enable Actions on $fork, so $upstream's CI runs on the bot's pull requests there.
  - Set "approvers", "upstream.reviewers" and "upstream.auto_open.labels" in .nightbot/config.json.
  - "checks" run in the clone of $upstream; its toolchain goes at the marked spot in nightbot-run.yml.
EOF
fi
