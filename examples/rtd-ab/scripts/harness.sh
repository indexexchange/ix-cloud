#!/usr/bin/env bash
# Runs the rtd-ab conformance harness against fresh containers of one image.
#
#   scripts/harness.sh <image> <go-image> <baked-pct>
#
# The container reads its mix and parameters from MODEL_PATH/ab-config.json. Runs:
#   1. shipped ./models/<version>/ (manifest.json + model_data/; control model_a 75%, test model_b 25%): no override
#   2. the same models with RTD_AB_TEST_PCT = 0, 10, 33.33, 100 (local-only
#      override; each value is aligned to the nearest 1/16)
#   3. no ab-config.json (empty folder): built-in models at the baked percentage
#   4. an invalid ab-config.json: one ERROR line, built-in models at the baked percentage
#   5. custom models: 50/50 mix and content sources that differ from the built-ins, so
#      the arms' results must follow the model files
# Every run checks the container's own log: at most 10 lines after 16,000+ requests,
# no request strings, and the expected WARN/ERROR lines.
set -euo pipefail

IMAGE=${1:?image}
GO_IMAGE=${2:?go image}
BAKED=${3:?baked pct}
SENTINEL=RTDABSENTINEL7f3a91c2
MAX_LOG_LINES=10

cd "$(dirname "$0")/.."
mkdir -p .build
docker run --rm -v "$PWD":/work -v rtd-ab-gomod:/go/pkg/mod -v rtd-ab-gobuild:/root/.cache/go-build -w /work \
  "$GO_IMAGE" go build -o /work/.build/testrunner ./cmd/testrunner

TMP=$(mktemp -d "${TMPDIR:-/tmp}/rtd-ab-harness.XXXXXX")
trap 'rm -rf "$TMP"' EXIT

fail=0
# run_one <label> <expect-pct> <models-dir> <warn:yes|no> <err:yes|no> <testrunner args> -- <docker args>
run_one() {
  local label=$1 pct=$2 models=$3 warn=$4 err=$5; shift 5
  local tr=() dk=()
  while [ $# -gt 0 ] && [ "$1" != "--" ]; do tr+=("$1"); shift; done
  [ $# -gt 0 ] && shift
  dk=("$@")
  local cid out rc lines
  cid=$(docker run -d --platform=linux/amd64 -p 50051:50051 -p 8080:8080 -v "$models:/models:ro" ${dk[@]+"${dk[@]}"} "$IMAGE")
  sleep 3
  echo "==> harness run: $label (expect-pct=$pct)"
  set +e
  docker run --rm --network host -v "$PWD":/work -w /work "$GO_IMAGE" /work/.build/testrunner \
    -addr localhost:50051 -metrics http://localhost:8080/metrics -fresh -expect-pct "$pct" \
    -golden internal/metrics/testdata/inherited_names.golden -sentinel "$SENTINEL" ${tr[@]+"${tr[@]}"}
  rc=$?
  set -e
  out=$(docker logs "$cid" 2>&1)
  lines=$(printf '%s\n' "$out" | wc -l | tr -d ' ')
  echo "  container log: $lines lines (limit $MAX_LOG_LINES)"
  if [ "$lines" -gt "$MAX_LOG_LINES" ]; then echo "  FAIL log volume: $lines lines"; rc=1; fi
  if printf '%s' "$out" | grep -q "$SENTINEL"; then echo "  FAIL sentinel token found in container log"; rc=1; fi
  if printf '%s' "$out" | grep -q "example"; then echo "  FAIL request-derived string 'example' found in container log"; rc=1; fi
  if [ "$warn" = yes ] && ! printf '%s' "$out" | grep -q "WARN RTD_AB_TEST_PCT"; then echo "  FAIL override active but no WARN line"; rc=1; fi
  if [ "$warn" = no ] && printf '%s' "$out" | grep -q "WARN"; then echo "  FAIL unexpected WARN"; rc=1; fi
  if [ "$err" = yes ] && [ "$(printf '%s' "$out" | grep -c "ERROR")" != 1 ]; then echo "  FAIL expected exactly one ERROR line"; rc=1; fi
  if [ "$err" = no ] && printf '%s' "$out" | grep -q "ERROR"; then echo "  FAIL unexpected ERROR line"; rc=1; fi
  docker rm -f "$cid" >/dev/null
  [ "$rc" -eq 0 ] || { echo "  RUN FAILED: $label"; fail=1; }
}

SHIPPED="$PWD/models"
V=$(ls "$SHIPPED" | tail -1)   # the newest version folder (the one the container resolves); all content is under $V/model_data
D="$V/model_data"
SRC=(-expect-source file -expect-models model_a,model_b)
run_one "shipped models, ab-config.json 75/25" 25 "$SHIPPED" no no "${SRC[@]}"
for pct in 0 10 33.33 100; do
  run_one "shipped models, override $pct" "$pct" "$SHIPPED" yes no "${SRC[@]}" -- -e "RTD_AB_TEST_PCT=$pct"
done

# No ab-config.json: the built-in models at the baked percentage.
mkdir -p "$TMP/empty"
run_one "no ab-config.json (built-in models, baked pct)" "$BAKED" "$TMP/empty" no no -no-model -expect-source builtin_absent -expect-models builtin,builtin

# Invalid ab-config.json: one ERROR line, then the same fallback.
cp -R "$SHIPPED" "$TMP/invalid"
printf '{"arms":' > "$TMP/invalid/$D/ab-config.json"
run_one "invalid ab-config.json (one ERROR, built-in models, baked pct)" "$BAKED" "$TMP/invalid" no yes -expect-source builtin_invalid -expect-models builtin,builtin

# Custom models: 50/50, and sources unlike the built-ins.
cp -R "$SHIPPED" "$TMP/custom"
cat > "$TMP/custom/$D/ab-config.json" <<'J'
{"version":1,"arms":{"control":{"model":"model_a","pct":50},"test":{"model":"model_b","pct":50}}}
J
echo '{"sources":["category","genre"]}' > "$TMP/custom/$D/model_a/content.json"
echo '{"sources":["category","keyword"]}' > "$TMP/custom/$D/model_b/content.json"
run_one "custom models 50/50 (sources follow the files)" 50 "$TMP/custom" no no "${SRC[@]}" \
  -ctl-sources category,genre -tst-sources category,keyword

[ "$fail" -eq 0 ] || { echo "harness: FAILED"; exit 1; }
echo "harness: all runs passed"
