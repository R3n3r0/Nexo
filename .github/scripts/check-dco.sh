#!/bin/sh
# Developer Certificate of Origin check (workflow dco.yml, make dco): every
# commit of BASE..HEAD but the merges carries a "Signed-off-by:" trailer
# naming its author, "Name <email>" as git records it (the comparison
# ignores case and the spaces at the ends). The sign-off certifies the DCO
# 1.1 and the license grant of CONTRIBUTING.md, "Licensing of
# contributions".
#
#   .github/scripts/check-dco.sh origin/main HEAD
#
# Fix a branch with "git rebase --signoff BASE" (or "git commit --amend -s"
# for the last commit), then force-push it.
set -eu

if [ $# -lt 1 ] || [ $# -gt 2 ]; then
    echo "usage: $0 BASE [HEAD]" >&2
    exit 2
fi
base=$1
head=${2:-HEAD}

lower() { tr '[:upper:]' '[:lower:]'; }

commits=$(git rev-list --no-merges --reverse "$base..$head")
if [ -z "$commits" ]; then
    echo "dco: no commit to check in $base..$head"
    exit 0
fi

n=0 bad=0
for c in $commits; do
    n=$((n + 1))
    author=$(git log -1 --format='%an <%ae>' "$c")
    want=$(printf '%s\n' "$author" | lower)
    if git log -1 --format='%(trailers:key=Signed-off-by,valueonly,unfold)' "$c" |
        sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' | lower | grep -qxF -- "$want"; then
        continue
    fi
    bad=$((bad + 1))
    echo "dco: $(git log -1 --format='%h %s' "$c")" >&2
    echo "     has no \"Signed-off-by: $author\"" >&2
done

if [ "$bad" -gt 0 ]; then
    echo "dco: $bad of $n commits are not signed off by their author." >&2
    echo "     Sign them off with \"git rebase --signoff $base\" and force-push the branch;" >&2
    echo "     the sign-off accepts the terms of CONTRIBUTING.md, \"Licensing of contributions\"." >&2
    exit 1
fi
echo "dco: all $n commits are signed off by their author"
