#!/usr/bin/env bash
# stop.sh — kill all kvnode processes
pkill -f 'kvnode' 2>/dev/null && echo "Cluster stopped." || echo "No cluster running."
