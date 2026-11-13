#!/bin/sh
set -e

if ! command -V systemctl >/dev/null 2>&1; then
	echo "Not running SystemD, ignoring"
	exit 0
fi

systemctl daemon-reload
systemctl reset-failed

echo "WARN: the sundae user/group and /var/lib/sundae directory were not removed"
