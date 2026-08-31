# The write half of stage 80's sandbox, appended to orchestrator-probe.sh when the
# proxy holds a GitHub credential and E2E_GITHUB_REPO names something to push to.
# Not a script of its own: it runs in the same shell as the probe, on the run's own
# $REPO and $ISSUE, and holds no credential either.

# 6. The write path, with a credential the proxy holds and this pod does not. The
#    branch is the one the run plan would push: implementer/issue-<n>.
#    --dry-run still does the ref discovery and the authentication, which is the
#    whole of what is under test; a clone of a public repository never
#    authenticates, so this is the assertion that the write path works.
git clone --depth 1 -q "https://x-access-token:$GH_TOKEN@github.com/$REPO.git" "$WORKSPACE/auth" \
  || { echo "!!! FAIL: clone of $REPO through the sentinel swap failed"; exit 1; }
# `|| exit` and not a bare cd: the read half already cloned into $WORKSPACE, so a
# failed cd would leave the push below running against *that* checkout and passing
# for the wrong reason.
cd "$WORKSPACE/auth" || { echo "!!! FAIL: no $WORKSPACE/auth after the clone"; exit 1; }
git push --dry-run origin "HEAD:refs/heads/implementer/issue-$ISSUE" \
  || { echo "!!! FAIL: push-dry-run through the swap failed"; exit 1; }
echo "PROBE git-push-dry-run ok   (implementer/issue-$ISSUE, git-receive-pack authenticated)"
