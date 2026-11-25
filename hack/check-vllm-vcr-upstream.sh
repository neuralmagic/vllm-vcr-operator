#!/usr/bin/env bash
#
# check-vllm-vcr-upstream.sh -- report drift between this operator and the
# vllm-vcr release it translates against (the "pin", docs/upstream-vllm-vcr.md).
#
# The check has three surfaces:
#
#   pin     the pinned commit/tag versus the upstream branch (network, read-only)
#   flags   the `vllm-vcr play` flags the translator emits versus the simulator's
#           clap `Opt` struct (hack/vllm-vcr-play-flags.txt, src/lib.rs)
#   compat  the simulator's default vLLM line versus the release the operator
#           translates (compat.toml, hack/vllm-vcr-pin.env,
#           internal/names/names.go)
#
# Exit status: 0 in sync or skipped, 1 on drift, 2 on a broken invocation or a
# missing repository file. A surface whose input is unavailable (no network, no
# simulator checkout) is reported as `skip ...` and does not fail the run, so CI
# without network stays green. A surface that can be checked is always strict.
#
# The check never writes to the simulator checkout: it reads the working tree and
# uses `git ls-remote` (not `git fetch`), so no refs move. SSH remotes are refused;
# HTTPS is the only supported transport.
#
# Usage: hack/check-vllm-vcr-upstream.sh [options]
#   --ref REF           upstream ref to compare (default: main)
#   --no-fetch          never contact the remote; compare against local refs
#   --remote URL        simulator remote (default: the HTTPS GitHub URL)
#   --vllm-vcr-dir DIR  simulator checkout (default: $VLLM_VCR_DIR, then a
#                       sibling ../vllm-vcr checkout)
#   --opt-file FILE     simulator `Opt` source (default: <dir>/src/lib.rs)
#   --compat-file FILE  simulator compat.toml (default: <dir>/compat.toml)
#   --flags-file FILE   emitted play flags (default: hack/vllm-vcr-play-flags.txt)
#   --pin-file FILE     pin record (default: hack/vllm-vcr-pin.env)
#   --names-file FILE   names constant source (default: internal/names/names.go)
#   --verbose           also list upstream flags the translator does not emit
#   -h, --help          this text
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "$script_dir/.." && pwd)

ref=main
fetch=1
remote=https://github.com/neuralmagic/vllm-vcr.git
vllm_dir=${VLLM_VCR_DIR:-}
if [[ -z "$vllm_dir" && -d "$repo_root/../vllm-vcr" ]]; then
  vllm_dir="$repo_root/../vllm-vcr"
fi
opt_file=""
compat_file=""
flags_file=""
pin_file=""
names_file=""
verbose=0

usage() { sed -n '3,44p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

while (($# > 0)); do
	case $1 in
	--ref) ref=${2:?--ref needs a value}; shift 2 ;;
	--no-fetch) fetch=0; shift ;;
	--remote) remote=${2:?--remote needs a value}; shift 2 ;;
	--vllm-vcr-dir) vllm_dir=${2:?--vllm-vcr-dir needs a value}; shift 2 ;;
	--opt-file) opt_file=${2:?--opt-file needs a value}; shift 2 ;;
	--compat-file) compat_file=${2:?--compat-file needs a value}; shift 2 ;;
	--flags-file) flags_file=${2:?--flags-file needs a value}; shift 2 ;;
	--pin-file) pin_file=${2:?--pin-file needs a value}; shift 2 ;;
	--names-file) names_file=${2:?--names-file needs a value}; shift 2 ;;
	--verbose) verbose=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) echo "check-vllm-vcr-upstream: unknown argument $1 (try --help)" >&2; exit 2 ;;
	esac
done

case $remote in
git@* | ssh://*)
	echo "check-vllm-vcr-upstream: refusing an SSH remote ($remote): use the HTTPS URL" >&2
	exit 2
	;;
esac

opt_file=${opt_file:-$vllm_dir/src/lib.rs}
compat_file=${compat_file:-$vllm_dir/compat.toml}
flags_file=${flags_file:-$repo_root/hack/vllm-vcr-play-flags.txt}
pin_file=${pin_file:-$repo_root/hack/vllm-vcr-pin.env}
names_file=${names_file:-$repo_root/internal/names/names.go}

drift=()
skips=()

drifted() { drift+=("$1"); }
skipped() { skips+=("$1"); }

# --- pin record -----------------------------------------------------------------

if [[ ! -f $pin_file ]]; then
	echo "check-vllm-vcr-upstream: pin record $pin_file is missing: the operator does not record which simulator build it describes" >&2
	exit 2
fi
# shellcheck disable=SC1090
. "$pin_file"
for key in VLLM_VCR_PIN_COMMIT VLLM_VCR_PIN_TAG VLLM_VCR_PIN_DEFAULT_LINE VLLM_VCR_TARGET_VERSION; do
	if [[ -z ${!key:-} ]]; then
		echo "check-vllm-vcr-upstream: $pin_file does not set $key" >&2
		exit 2
	fi
done
pin_short=${VLLM_VCR_PIN_COMMIT:0:7}

# --- helpers --------------------------------------------------------------------

# release_version strips a leading "v" from a release tag and returns it only when
# the tag looks like a released version (X.Y.Z); "nightly"/"rc" yield nothing.
release_version() {
	local tag=$1
	tag=${tag#v}
	[[ $tag =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 0
	printf '%s\n' "$tag"
}

major_minor() { printf '%s\n' "${1%.*}"; }

# newest_release_tag prints the highest X.Y.Z release tag from stdin. Tracker tags
# ("nightly", "rc" tags, pre-releases) are not releases and are ignored.
newest_release_tag() {
	grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1
}

# opt_flags extracts the long-option names of a clap `#[derive(Args)]` struct.
# clap derives the flag from the field name (kebab-case) unless the attribute names
# it explicitly; `long = false` declares the opposite of what is checked here.
# Attributes are read one line at a time, which is how the pinned release writes
# them; a multi-line layout would surface as missing upstream flags, not as a
# silent pass.
opt_flags() {
	awk '
		/^pub struct Opt \{/ { in_opt = 1; next }
		in_opt && /^\}/ { exit }
		!in_opt { next }
		/^[[:space:]]*#\[arg\(/ {
			attr = $0
			if (attr ~ /long[[:space:]]*=[[:space:]]*false/) { pending = 0; next }
			if (match(attr, /long[[:space:]]*=[[:space:]]*"[^"]+"/)) {
				name = substr(attr, RSTART, RLENGTH)
				sub(/^[^"]*"/, "", name)
				sub(/".*$/, "", name)
				print "--" name
				pending = 0
				next
			}
			pending = (attr ~ /(^|[^[:alnum:]_])long([^[:alnum:]_]|$)/)
			next
		}
		pending && /^[[:space:]]*pub[[:space:]]+[[:alnum:]_]+[[:space:]]*:/ {
			field = $0
			sub(/^[[:space:]]*pub[[:space:]]+/, "", field)
			sub(/[[:space:]]*:.*$/, "", field)
			gsub(/_/, "-", field)
			print "--" field
			pending = 0
		}
	' "$1" | sort -u
}

# read_flag_list prints the non-comment lines of a one-flag-per-line list.
read_flag_list() { sed -e 's/#.*$//' -e 's/[[:space:]]//g' "$1" | grep -v '^$' | sort -u; }

# compat_default prints "<line>\t<tag>" for the compat.toml entry with
# default = true.
compat_default() {
	awk '
		/^\[\[vllm\]\]/ { line = ""; tag = ""; next }
		/^[[:space:]]*line[[:space:]]*=/ { line = $0; sub(/^[^=]*=[[:space:]]*"/, "", line); sub(/".*$/, "", line); next }
		/^[[:space:]]*tag[[:space:]]*=/ { tag = $0; sub(/^[^=]*=[[:space:]]*"/, "", tag); sub(/".*$/, "", tag); next }
		/^[[:space:]]*default[[:space:]]*=[[:space:]]*true/ && line != "" { print line "\t" tag; exit }
	' "$1"
}

# names_version extracts SupportedVLLMVersion from the Go source.
names_version() {
	sed -n 's/.*SupportedVLLMVersion[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "$1" | head -1
}

checkout_available() { git -C "$vllm_dir" rev-parse --git-dir >/dev/null 2>&1; }

checkout_head() {
	git -C "$vllm_dir" rev-parse --verify --quiet HEAD 2>/dev/null || true
}

echo "vllm-vcr upstream check (docs/upstream-vllm-vcr.md)"
echo "  pin:    $VLLM_VCR_PIN_COMMIT ($VLLM_VCR_PIN_TAG, verified ${VLLM_VCR_PIN_VERIFIED:-unknown})"

# --- surface: pin ---------------------------------------------------------------

ref_commit=""
tag_list=""
note_pin=""

if ((fetch)); then
	if ls_out=$(GIT_TERMINAL_PROMPT=0 timeout "${VLLM_VCR_FETCH_TIMEOUT:-60}" \
		git ls-remote "$remote" "refs/heads/$ref" "refs/tags/*" 2>&1); then
		ref_commit=$(printf '%s\n' "$ls_out" | awk -v want="refs/heads/$ref" '$2 == want { print $1 }' | head -1)
		tag_list=$(printf '%s\n' "$ls_out" | awk '$2 ~ /^refs\/tags\// { sub(/^refs\/tags\//, "", $2); sub(/\^\{\}$/, "", $2); print $2 }')
		if [[ -z $ref_commit ]]; then
			skipped "pin: $remote has no refs/heads/$ref"
		fi
	else
		skipped "pin: cannot reach $remote ($(printf '%s\n' "$ls_out" | head -1))"
	fi
elif checkout_available; then
	local_ref=""
	for candidate in "refs/remotes/upstream/$ref" "refs/remotes/origin/$ref" "$ref"; do
		if git -C "$vllm_dir" rev-parse --verify --quiet "$candidate^{commit}" >/dev/null 2>&1; then
			local_ref=$candidate
			break
		fi
	done
	if [[ -z $local_ref ]]; then
		skipped "pin: $vllm_dir has no local ref for $ref (use a fetch, or --ref)"
	else
		ref_commit=$(git -C "$vllm_dir" rev-parse "$local_ref^{commit}")
		tag_list=$(git -C "$vllm_dir" tag --list 'v*')
		note_pin="from $local_ref, not the remote (--no-fetch)"
	fi
else
	skipped "pin: no network fetch and no checkout at $vllm_dir"
fi

if [[ -n $ref_commit ]]; then
	newest_tag=$(printf '%s\n' "$tag_list" | newest_release_tag)
	printf '  ref:    %s -> %s (newest release tag %s)%s\n' \
		"$ref" "$ref_commit" "${newest_tag:-none}" \
		"${note_pin:+ [$note_pin]}"
	if [[ $ref_commit != "$VLLM_VCR_PIN_COMMIT" ]]; then
		drifted "pin: $ref is $ref_commit but the pin records $VLLM_VCR_PIN_COMMIT ($VLLM_VCR_PIN_TAG)"
	fi
	if [[ -n $newest_tag && $newest_tag != "$VLLM_VCR_PIN_TAG" ]]; then
		drifted "pin: the newest release tag is $newest_tag but the pin records $VLLM_VCR_PIN_TAG"
	fi
fi

if checkout_available; then
	echo "  checkout: $vllm_dir"
	head=$(checkout_head)
	if [[ -n $head && $head != "$VLLM_VCR_PIN_COMMIT" ]]; then
		echo "  note:   $vllm_dir HEAD is ${head:0:7}, not the pinned $pin_short; the flags and compat surfaces describe that working tree"
	fi
else
	skipped "checkout: no simulator checkout at $vllm_dir (flags and compat not checked)"
fi

# --- surface: flags -------------------------------------------------------------

if [[ ! -f $flags_file ]]; then
	echo "check-vllm-vcr-upstream: play-flag list $flags_file is missing" >&2
	exit 2
fi
if [[ -f $opt_file ]]; then
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	opt_flags "$opt_file" >"$tmp/upstream"
	read_flag_list "$flags_file" >"$tmp/emitted"
	if [[ ! -s $tmp/upstream ]]; then
		drifted "flags: $opt_file declares no long options, so the emitted play flags cannot be checked against it"
	else
		missing=$(comm -23 "$tmp/emitted" "$tmp/upstream" || true)
		extra=$(comm -13 "$tmp/emitted" "$tmp/upstream" || true)
		printf '  flags:  %s emitted play flags, %s defined by %s\n' \
			"$(wc -l <"$tmp/emitted" | tr -d ' ')" "$(wc -l <"$tmp/upstream" | tr -d ' ')" "$(basename -- "$(dirname -- "$opt_file")")/$(basename -- "$opt_file")"
		while IFS= read -r flag; do
			[[ -z $flag ]] && continue
			drifted "flags: the translator emits $flag, which the pinned simulator does not define"
		done <<<"$missing"
		if ((verbose)) && [[ -n $extra ]]; then
			echo "  note:   upstream flags the translator does not emit:"
			while IFS= read -r flag; do
				[[ -n $flag ]] && echo "            $flag"
			done <<<"$extra"
		fi
	fi
else
	skipped "flags: no Opt source at $opt_file"
fi

# --- surface: compat ------------------------------------------------------------

if [[ -f $compat_file && -f $names_file ]]; then
	target=$(names_version "$names_file")
	if [[ -z $target ]]; then
		echo "check-vllm-vcr-upstream: $names_file declares no SupportedVLLMVersion" >&2
		exit 2
	else
		default=$(compat_default "$compat_file")
		if [[ -z $default ]]; then
			drifted "compat: $compat_file declares no default vLLM line, so the operator's target cannot be verified"
		else
			default_line=${default%%$'\t'*}
			default_tag=${default#*$'\t'}
			compat_version=$(release_version "$default_tag")
			printf '  compat: default line %s -> %s; operator target %s (internal/names)\n' \
				"$default_line" "${default_tag:-unset}" "$target"
			if [[ $VLLM_VCR_TARGET_VERSION != "$target" ]]; then
				drifted "compat: $pin_file records target $VLLM_VCR_TARGET_VERSION but internal/names/names.go translates $target"
			fi
			if [[ -n $compat_version && $compat_version != "$target" ]]; then
				drifted "compat: the pinned simulator defaults to vLLM $default_tag but the operator translates $target"
			fi
			if [[ $(major_minor "$target") != "$default_line" ]]; then
				drifted "compat: the pinned simulator's default line is $default_line but the operator translates $target"
			fi
			if [[ $default_line != "$VLLM_VCR_PIN_DEFAULT_LINE" ]]; then
				drifted "compat: $compat_file defaults to line $default_line but $pin_file records $VLLM_VCR_PIN_DEFAULT_LINE"
			fi
			if [[ -n ${VLLM_VCR_PIN_DEFAULT_TAG:-} && $default_tag != "$VLLM_VCR_PIN_DEFAULT_TAG" ]]; then
				drifted "compat: $compat_file defaults to $default_tag but $pin_file records $VLLM_VCR_PIN_DEFAULT_TAG"
			fi
		fi
	fi
else
	missing_files=()
	[[ -f $compat_file ]] || missing_files+=("$compat_file")
	[[ -f $names_file ]] || missing_files+=("$names_file")
	skipped "compat: missing ${missing_files[*]}"
fi

# --- result ---------------------------------------------------------------------

for line in ${skips[@]+"${skips[@]}"}; do
	echo "skip $line"
done
for line in ${drift[@]+"${drift[@]}"}; do
	echo "DRIFT $line"
done

if ((${#drift[@]} > 0)); then
	echo "result: ${#drift[@]} drifted surface(s): re-read docs/upstream-vllm-vcr.md and adapt, or record the new pin"
	exit 1
fi
if ((${#skips[@]} > 0)); then
	echo "result: no drift in the surfaces that could be checked; ${#skips[@]} surface(s) skipped"
	exit 0
fi
echo "result: no drift"
