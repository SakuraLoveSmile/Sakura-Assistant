#!/usr/bin/env bash
set -Eeuo pipefail
python3 -B -m unittest -v hub.installer.test_install
bash -n hub/installer/install.sh
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck hub/installer/test_install.sh
fi
printf 'installer regression tests passed\n'
