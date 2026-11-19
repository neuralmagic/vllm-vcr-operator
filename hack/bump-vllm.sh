#!/usr/bin/env bash
# bump-vllm.sh moves this operator to a newer vLLM release.
#
# It automates the mechanical half: it discovers what the pinned vllm-vcr build
# supports, checks that the CPU frontend image for that release exists, rewrites
# every pin, and then runs the gates that tell you what still needs a human.
#
# It deliberately does NOT rewrite the vLLM flag surface. internal/vllm encodes
# the release's CLI spellings, defaults and config-file semantics; a new release
# changes them, and only a person can read the release's argparse definitions and
# decide what each change means. The script fails loudly when that drift shows up
# instead of pretending the bump is complete.
#
# Usage:
#   hack/bump-vllm.sh                 # report what a bump would do, change nothing
#   hack/bump-vllm.sh --apply         # rewrite the pins and run the gates
#   hack/bump-vllm.sh --to 0.34.0 --apply
#   hack/bump-vllm.sh --to 0.34.0 --simulator v0.3.0 --apply
#
# Options:
#   --to VERSION        target vLLM release (default: latest release of the line
#                       vllm-vcr's compat.toml marks as default)
#   --simulator TAG     vllm-vcr release tag (default: keep the current pin when
#                       its line already matches, otherwise the newest tag for the
#                       target line)
#   --apply             write the changes; without it nothing is modified
#   --skip-gates        rewrite pins without running tests (for scripting)
#   --allow-missing-frontend-image
#                       continue even if the CPU frontend image tag does not exist
#   --allow-missing-simulator
#                       continue even if no vllm-vcr build exists for the target
#                       line (the bump is then not runnable, only staged)
#
# Requires: git, curl, jq, go. Network access is used to read upstream metadata.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

apply=false
skip_gates=false
allow_missing_image=false
allow_missing_simulator=false
target_version=""
simulator_tag=""
compat_url="https://raw.githubusercontent.com/neuralmagic/vllm-vcr/main/compat.toml"
frontend_repo="vllm/vllm-openai-cpu"

usage() { sed -n '2,32p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }
die() { echo "bump-vllm: $*" >&2; exit 1; }
note() { echo "bump-vllm: $*"; }
have() { command -v "$1" >/dev/null 2>&1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --to) target_version="${2:?}"; shift 2 ;;
    --simulator) simulator_tag="${2:?}"; shift 2 ;;
    --apply) apply=true; shift ;;
    --skip-gates) skip_gates=true; shift ;;
    --allow-missing-frontend-image) allow_missing_image=true; shift ;;
    --allow-missing-simulator) allow_missing_simulator=true; shift ;;
    -h | --help) usage; exit 0 ;;
    *) die "unknown argument: $1 (try --help)" ;;
  esac
done

for bin in git curl jq go; do have "$bin" || die "$bin is required"; done
[[ -f hack/vllm-vcr-pin.env ]] || die "hack/vllm-vcr-pin.env is missing"
# shellcheck disable=SC1091
source hack/vllm-vcr-pin.env

current_version="$(sed -n 's/^VLLM_VCR_TARGET_VERSION=//p' hack/vllm-vcr-pin.env)"
current_line="$(sed -n 's/^VLLM_VCR_PIN_DEFAULT_LINE=//p' hack/vllm-vcr-pin.env)"
current_tag="$(sed -n 's/^VLLM_VCR_PIN_TAG=//p' hack/vllm-vcr-pin.env)"
note "current pin: vLLM ${current_version}, simulator ${current_tag} (line ${current_line})"

# 1. What does vllm-vcr support? compat.toml is the authority: its default line is
#    the release the simulator is built and captured against.
if [[ -z "$target_version" ]]; then
  compat="$(curl -fsSL "$compat_url")" || die "cannot read $compat_url"
  line="$(printf '%s\n' "$compat" | awk '
    /^\[\[line\]\]/ { reset() }
    function reset() { tag=""; def="" }
    /^line[[:space:]]*=/ { gsub(/.*=[[:space:]]*"|".*/, ""); tag=$0 }
    /default[[:space:]]*=[[:space:]]*true/ { if (tag != "") print tag }
  ' | head -1)"
  [[ -n "$line" ]] || die "compat.toml has no default line"
  note "vllm-vcr default line: ${line}"
  target_version="${line}.0"
  [[ "$target_version" == "$current_version" ]] && note "already on the newest supported line"
else
  # Accept "0.34.0" or a bare line "0.34".
  if [[ "$target_version" =~ ^([0-9]+\.[0-9]+)$ ]]; then
    line="$target_version"
    target_version="${line}.0"
  else
    line="$(printf '%s' "$target_version" | sed -E 's/^([0-9]+\.[0-9]+).*/\1/')"
  fi
fi

# 2. Does the CPU frontend image exist for that release? Without it there is
#    nothing to run, and the operator would only fail at Pod start.
tag_status="$(curl -fsS -o /dev/null -w '%{http_code}' "https://hub.docker.com/v2/repositories/${frontend_repo}/tags/v${target_version}" || true)"
if [[ "$tag_status" == "200" ]]; then
  note "frontend image: ${frontend_repo}:v${target_version} exists"
elif [[ "$allow_missing_image" == true ]]; then
  note "WARNING: ${frontend_repo}:v${target_version} not found; continuing because --allow-missing-frontend-image"
else
  die "${frontend_repo}:v${target_version} not found on Docker Hub: publish or build a CPU frontend for this release first (see docs/openshift.md)"
fi

# 3. Which simulator tag serves that line? Tags are published per line, e.g.
#    0.2.5-vllm0.33.
# Image tags are "<release>-vllm<line>", e.g. 0.2.5-vllm0.33. The floating
# "vllm0.33" tag also exists; it must not be chosen, because a pin has to name a
# release. The release tag the code carries is the "v<release>" form.
image_tag=""
if [[ -z "$simulator_tag" ]]; then
  token="$(curl -fsS "https://ghcr.io/token?scope=repository:neuralmagic/vllm-vcr:pull&service=ghcr.io" | jq -r .token)"
  image_tag="$(curl -fsS -H "Authorization: Bearer $token" \
    "https://ghcr.io/v2/neuralmagic/vllm-vcr/tags/list?n=500" |
    jq -r '.tags[]?' | grep -E "^[0-9]+\.[0-9]+\.[0-9]+-vllm${line}$" | sort -V | tail -1 || true)"
  if [[ -n "$image_tag" ]]; then
    simulator_tag="v${image_tag%%-vllm*}"
  fi
elif [[ "$simulator_tag" =~ ^[0-9] ]]; then
  # Accept an image tag or a bare release number; normalise to the release tag.
  image_tag="${simulator_tag}"
  simulator_tag="v${image_tag%%-vllm*}"
fi
[[ -n "$image_tag" ]] || image_tag="${simulator_tag#v}-vllm${line}"
if [[ -z "$simulator_tag" ]]; then
  note "no published vllm-vcr build for line ${line}"
  if [[ "$allow_missing_simulator" != true ]]; then
    die "a simulator must exist for the ${line} protocol line: the frontend and the simulator have to speak the same engine-core protocol. Build one, or stage the bump with --allow-missing-simulator."
  fi
  note "staging anyway because --allow-missing-simulator; this bump is not runnable yet"
  simulator_tag="$current_tag"
  image_tag="${current_tag#v}-vllm${line}"
else
  note "simulator release for line ${line}: ${simulator_tag} (image tag ${image_tag})"
fi

if [[ "$target_version" == "$current_version" && "$simulator_tag" == "$current_tag" ]]; then
  note "nothing to do: already on vLLM ${current_version} and simulator ${current_tag}"
  exit 0
fi

# 4. Show the plan.
tag_date="$(curl -fsS "https://api.github.com/repos/neuralmagic/vllm-vcr/releases/tags/${simulator_tag}" 2>/dev/null |
  jq -r '.published_at // empty' | cut -dT -f1 || true)"
cat <<EOF

plan
  vLLM release           ${current_version} -> ${target_version}
  vllm-vcr line          ${current_line} -> ${line}
  vllm-vcr tag           ${current_tag} -> ${simulator_tag}
  simulator image tag    ${image_tag}$([[ -n "$tag_date" ]] && echo " (${tag_date})")
  files
    internal/names/names.go          SupportedVLLMVersion, SupportedSimulatorTag
    hack/vllm-vcr-pin.env           target version, line, tag, dates
    config/manager/manager.yaml     image tags v<release>
    Dockerfile.supervisor           commented image tags
    docs/upstream-vllm-vcr.md       pin table

  NOT automated, by design: internal/vllm encodes this release's flags, defaults
  and config-file semantics. Re-read the new release's argparse surface and update
  it. The gates below fail until that is done.
EOF

if [[ "$apply" != true ]]; then
  echo
  note "dry run: nothing changed. Re-run with --apply."
  exit 0
fi

# 5. Rewrite the pins.
today="$(date +%F)"
old_minor="$(printf '%s' "$current_version" | cut -d. -f1-2)"

python3 - "$target_version" "$simulator_tag" "$line" "$today" "$image_tag" <<'PY'
import pathlib, sys
target, tag, line, today, image_tag = sys.argv[1:6]
cur_minor = None

names = pathlib.Path('internal/names/names.go')
s = names.read_text()
import re
m = re.search(r'SupportedVLLMVersion = "([^"]+)"', s)
cur = m.group(1) if m else ''
s = re.sub(r'SupportedVLLMVersion = "[^"]+"', f'SupportedVLLMVersion = "{target}"', s, count=1)
s = re.sub(r'SupportedSimulatorTag = "[^"]+"', f'SupportedSimulatorTag = "{tag}"', s, count=1)
names.write_text(s)

env = pathlib.Path('hack/vllm-vcr-pin.env')
lines = []
seen_image = False
for raw in env.read_text().splitlines():
    if raw.startswith('VLLM_VCR_PIN_DEFAULT_LINE='):
        raw = f'VLLM_VCR_PIN_DEFAULT_LINE={line}'
    elif raw.startswith('VLLM_VCR_PIN_DEFAULT_TAG='):
        raw = f'VLLM_VCR_PIN_DEFAULT_TAG=v{target}'
    elif raw.startswith('VLLM_VCR_PIN_TAG='):
        raw = f'VLLM_VCR_PIN_TAG={tag}'
    elif raw.startswith('VLLM_VCR_PIN_TAG_DATE='):
        raw = f'VLLM_VCR_PIN_TAG_DATE={today}'
    elif raw.startswith('VLLM_VCR_PIN_VERIFIED='):
        raw = f'VLLM_VCR_PIN_VERIFIED={today}'
    elif raw.startswith('VLLM_VCR_PIN_IMAGE_TAG='):
        raw = f'VLLM_VCR_PIN_IMAGE_TAG={image_tag}'
        seen_image = True
    elif raw.startswith('VLLM_VCR_TARGET_VERSION='):
        raw = f'VLLM_VCR_TARGET_VERSION={target}'
    lines.append(raw)
env.write_text('\n'.join(lines) + '\n')

for path in ('config/manager/manager.yaml', 'Dockerfile.supervisor'):
    p = pathlib.Path(path)
    t = p.read_text()
    if cur:
        t = t.replace(f'v{cur}', f'v{target}')
    p.write_text(t)

doc = pathlib.Path('docs/upstream-vllm-vcr.md')
if doc.exists():
    t = doc.read_text()
    t = t.replace(f'v{cur}', f'v{target}')
    doc.write_text(t)
PY

# 6. Gates that tell you what a human still has to do.
status=0
if [[ "$skip_gates" == true ]]; then
  note "gates skipped (--skip-gates): the bump is unverified"
else
  note "gates"
  if ! go build ./... >/dev/null 2>&1; then
    note "FAIL build: the tree does not compile after the bump"
    status=1
  fi
  if ! go test ./internal/vllm/... ./internal/profile/... ./internal/translator/... 2>&1 | tail -30; then
    note "FAIL: vLLM-facing tests disagree with the new release."
    note "      Expected: internal/vllm's flag/default tables encode the old release."
    note "      Update them from the new release's argparse definitions, then re-run."
    status=1
  fi
  if ./hack/check-vllm-vcr-upstream.sh >/dev/null 2>&1; then
    note "upstream drift check: consistent"
  else
    note "FAIL upstream drift check: run ./hack/check-vllm-vcr-upstream.sh for the surface"
    status=1
  fi
fi

if [[ "$status" != 0 ]]; then
  note "pins updated, but the bump is NOT finished. Fix the failures above."
  exit 1
fi
note "pins updated and gates pass. Next:"
note "  1. re-record profile images on vLLM ${target_version} (old profiles are ineligible by design)"
note "  2. make test-e2e && make test-cluster"
note "  3. update docs/upstream-vllm-vcr.md with what the release changed"
