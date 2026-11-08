#!/usr/bin/env bash
# Verify the unit files embedded in the install scripts match deploy/*.service.
set -euo pipefail
cd "$(dirname "$0")"
rc=0
for k in worker hub; do
  if ! diff <(sed -n "/<<'UNIT'/,/^UNIT\$/{//!p}" "install-$k.sh") "mado-$k.service" >/dev/null; then
    echo "install-$k.sh: embedded unit differs from mado-$k.service" >&2; rc=1
  fi
  bash -n "install-$k.sh"
done
exit $rc
