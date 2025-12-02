#!/bin/sh
# Script to write fake Prometheus metrics
# Usage: update-metrics.sh <metric_name> <value> <namespace> <ns_value> <persistentvolumeclaim> <pvc_value>
#
# Example:
#   update-metrics.sh kubelet_volume_stats_available_bytes 1073741824 namespace monitoring persistentvolumeclaim vali-vali-0

set -e

METRICS_DIR="${METRICS_DIR:-/var/metrics}"
METRICS_FILE="${METRICS_DIR}/metrics.txt"

# Parse arguments
if [ $# -lt 6 ]; then
    echo "Error: Insufficient arguments"
    echo "Usage: $0 <metric_name> <value> <namespace> <ns_value> <persistentvolumeclaim> <pvc_value>"
    exit 1
fi

METRIC_NAME="$1"
METRIC_VALUE="$2"

# Parse label arguments
shift 2
LABELS=""
while [ $# -gt 0 ]; do
    if [ $# -lt 2 ]; then
        echo "Error: Label name without value"
        exit 1
    fi

    LABEL_NAME="$1"
    LABEL_VALUE="$2"

    if [ -z "$LABELS" ]; then
        LABELS="${LABEL_NAME}=\"${LABEL_VALUE}\""
    else
        LABELS="${LABELS},${LABEL_NAME}=\"${LABEL_VALUE}\""
    fi

    shift 2
done

# Always add type=fake label
LABELS="${LABELS},type=\"fake\""

# Ensure metrics directory exists
mkdir -p "$METRICS_DIR"

# Initialize metrics file with headers if it doesn't exist
if [ ! -f "$METRICS_FILE" ]; then
    cat > "$METRICS_FILE" << 'EOF'
# HELP kubelet_volume_stats_available_bytes Fake available bytes in volumes
# TYPE kubelet_volume_stats_available_bytes gauge
# HELP kubelet_volume_stats_capacity_bytes Fake capacity bytes of volumes
# TYPE kubelet_volume_stats_capacity_bytes gauge
# HELP kubelet_volume_stats_inodes_free Fake free inodes in volumes
# TYPE kubelet_volume_stats_inodes_free gauge
# HELP kubelet_volume_stats_inodes Fake total inodes in volumes
# TYPE kubelet_volume_stats_inodes gauge
EOF
fi

# Create the metric line
METRIC_LINE="${METRIC_NAME}{${LABELS}} ${METRIC_VALUE}"

# Pattern to match this metric (name and labels, ignoring value)
METRIC_PATTERN="${METRIC_NAME}{${LABELS}}"

# Use a temp file to rebuild the metrics
TEMP_FILE="${METRICS_FILE}.tmp"

# Copy headers and non-matching metrics to temp file, update matching ones
FOUND=0
while IFS= read -r line; do
    # Keep comments and empty lines
    if [ -z "$line" ] || [ "${line#\#}" != "$line" ]; then
        echo "$line" >> "$TEMP_FILE"
        continue
    fi

    # Check if line starts with our metric pattern
    case "$line" in
        "${METRIC_PATTERN} "*)
            # This is the metric we're updating - replace it
            if [ $FOUND -eq 0 ]; then
                echo "$METRIC_LINE" >> "$TEMP_FILE"
                FOUND=1
            fi
            ;;
        *)
            # Keep other metrics
            echo "$line" >> "$TEMP_FILE"
            ;;
    esac
done < "$METRICS_FILE"

# If metric wasn't found, append it
if [ $FOUND -eq 0 ]; then
    echo "$METRIC_LINE" >> "$TEMP_FILE"
fi

# Replace the original file
mv "$TEMP_FILE" "$METRICS_FILE"

echo "Metric updated: $METRIC_LINE"
