#!/usr/bin/env bash
#
# verify-dco.sh -- every commit in a range carries a Developer Certificate of
# Origin sign-off from its own author.
#
# The project takes contributions under the DCO (see CONTRIBUTING.md): the
# Signed-off-by trailer is the contributor's statement that they may submit the
# change. A commit whose trailer is missing, or names somebody other than its
# author, is not that statement, so it does not pass.
#
# Merge commits are exempt. They are created by the forge or by `git merge`, carry
# no change of their own, and requiring a trailer on them would make "merge main
# into my branch" fail for a reason unrelated to the contribution.
#
# Exit status: 0 when every commit is signed off, 1 on a missing or mismatched
# sign-off, 2 on a broken invocation.
#
# Usage: hack/verify-dco.sh [options]
#   --base REF   first commit NOT to check. It is compared with the merge base, so
#                commits already upstream are never re-checked. Default: origin/HEAD,
#                then origin/main, then main.
#   --head REF   last commit to check (default: HEAD)
#   -q, --quiet  print only failures
#   -h, --help   this text
#
# Examples:
#   hack/verify-dco.sh                       # the commits on this branch
#   hack/verify-dco.sh --base origin/main    # the same, stated explicitly
#   hack/verify-dco.sh --base <sha> --head <sha>
set -euo pipefail

base=""
head="HEAD"
quiet=0

usage() { sed -n '3,32p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

while (($# > 0)); do
	case $1 in
	--base) base=${2:?--base needs a value}; shift 2 ;;
	--head) head=${2:?--head needs a value}; shift 2 ;;
	-q | --quiet) quiet=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*)
		echo "unknown argument: $1" >&2
		usage >&2
		exit 2
		;;
	esac
done

say() { [[ $quiet -eq 1 ]] || echo "$*"; }
die() {
	echo "verify-dco: $*" >&2
	exit 2
}

git rev-parse --git-dir >/dev/null 2>&1 || die "not inside a git repository"

head_sha=$(git rev-parse --verify --quiet "${head}^{commit}") || die "cannot resolve --head ${head}"
head_sha_raw=$(git rev-parse "${head}")
# The first push to a new ref carries an all-zero "before"; there is no range then,
# and the head commit is the whole contribution.
if [[ "$head_sha_raw" =~ ^0+$ ]]; then
	die "--head ${head} is the null ref: pass the commit the push created"
fi

# Zero is also what a forge sends for "no previous ref" as --base.
if [[ "$base" =~ ^0+$ ]]; then
	base=""
fi

if [[ -z "$base" ]]; then
	for candidate in origin/HEAD origin/main main; do
		if git rev-parse --verify --quiet "${candidate}^{commit}" >/dev/null; then
			base="$candidate"
			break
		fi
	done
fi

if [[ -n "$base" ]]; then
	base_sha=$(git rev-parse --verify --quiet "${base}^{commit}") || die "cannot resolve --base ${base}"
	# Compare with the merge base so commits that are already upstream are not
	# re-checked: a rebased branch must not fail on somebody else's history.
	range_base=$(git merge-base "$base_sha" "$head_sha") || die "${base} and ${head} share no history"
else
	# No usable base: check the head commit alone rather than claiming a range that
	# was never established.
	range_base=""
fi

if [[ -n "$range_base" ]]; then
	mapfile -t commits < <(git rev-list --no-merges "${range_base}..${head_sha}")
else
	mapfile -t commits < <(git rev-list --no-merges -n 1 "$head_sha")
fi

if ((${#commits[@]} == 0)); then
	say "ok: no commits to check in the range"
	exit 0
fi

# signoff_emails prints the address in each Signed-off-by trailer of one commit.
signoff_emails() {
	git log -1 --format=%B "$1" |
		grep -i '^Signed-off-by: .*<[^>]*>' |
		sed -n 's/.*<\([^>]*\)>.*/\1/p' |
		tr '[:upper:]' '[:lower:]'
}

failed=0
for sha in "${commits[@]}"; do
	short=$(git rev-parse --short "$sha")
	subject=$(git log -1 --format=%s "$sha")
	author_email=$(git log -1 --format=%ae "$sha" | tr '[:upper:]' '[:lower:]')

	signed=0
	while IFS= read -r email; do
		[[ -n "$email" ]] || continue
		if [[ "$email" == "$author_email" ]]; then
			signed=1
			break
		fi
	done < <(signoff_emails "$sha")

	if [[ $signed -eq 1 ]]; then
		say "ok: ${short} ${subject}"
		continue
	fi

	failed=$((failed + 1))
	echo "fail: ${short} ${subject}" >&2
	if [[ -z "$(signoff_emails "$sha")" ]]; then
		echo "    no Signed-off-by trailer: ${author_email} has not certified the contribution" >&2
	else
		echo "    Signed-off-by names $(signoff_emails "$sha" | paste -sd, -), not the author ${author_email}" >&2
	fi
done

if ((failed > 0)); then
	cat >&2 <<EOF

${failed} commit(s) are not signed off. Sign the ones you authored:

    git rebase --signoff ${range_base:-${head_sha}~1}   # every commit in the range
    git commit --amend -s                          # the last commit only

The trailer must name the commit's own author, and must not be added to somebody
else's commit on their behalf (CONTRIBUTING.md).
EOF
	exit 1
fi

say "ok: ${#commits[@]} commit(s) signed off"
