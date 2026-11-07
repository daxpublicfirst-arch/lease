#!/bin/sh
# Fixture tool: prints its lease environment and its arguments, then cats
# the first input file. Used by the README example and by --self-test.
echo "LEASE_OUT=$LEASE_OUT"
echo "LEASE_INPUT_0=$LEASE_INPUT_0"
echo "args: $*"
if [ -f "$LEASE_INPUT_0" ]; then
  cat "$LEASE_INPUT_0"
fi
