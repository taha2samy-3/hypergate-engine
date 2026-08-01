#!/usr/bin/env bash
set -e

REPORT_FILE="tests/cs_report.log"
ENGINE_PID=$(docker inspect --format '{{ .State.Pid }}' hyper-engine)

get_cs_totals() {
    cat /proc/$ENGINE_PID/task/*/status 2>/dev/null | awk '
        /^voluntary_ctxt_switches:/ {v+=$2}
        /^nonvoluntary_ctxt_switches:/ {nv+=$2}
        END {print v+0, nv+0}
    '
}

read V_START NV_START <<< $(get_cs_totals)
START_TIME=$(date +%s.%N)

"$@"

END_TIME=$(date +%s.%N)
read V_END NV_END <<< $(get_cs_totals)

DIFF_V=$((V_END - V_START))
DIFF_NV=$((NV_END - NV_START))

DURATION=$(awk "BEGIN {printf \"%.2fs\", $END_TIME - $START_TIME}")
TIMESTAMP=$(date "+%Y-%m-%d %H:%M:%S")
TEST_NAME="$*"

{
    echo "=========================================="
    echo "TIMESTAMP        : $TIMESTAMP"
    echo "COMMAND/TEST     : $TEST_NAME"
    echo "ENGINE PID       : $ENGINE_PID"
    echo "DURATION         : $DURATION"
    echo "Voluntary CS     : $DIFF_V"
    echo "Non-Voluntary CS : $DIFF_NV"
    echo "=========================================="
    echo ""
} | tee -a "$REPORT_FILE"

if [ "$GITHUB_ACTIONS" = "true" ] && [ -n "$GITHUB_STEP_SUMMARY" ]; then
    if [ ! -s "$GITHUB_STEP_SUMMARY" ] || ! grep -q "Context Switch & Performance Summary" "$GITHUB_STEP_SUMMARY"; then
        {
            echo "### 📊 Context Switch & Performance Summary"
            echo "| Test / Command | Duration | Voluntary CS | Non-Voluntary CS | Engine PID |"
            echo "| :--- | :---: | :---: | :---: | :---: |"
        } >> "$GITHUB_STEP_SUMMARY"
    fi
    echo "| \`$TEST_NAME\` | $DURATION | $DIFF_V | $DIFF_NV | $ENGINE_PID |" >> "$GITHUB_STEP_SUMMARY"
fi