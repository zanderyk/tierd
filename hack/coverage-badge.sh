#!/usr/bin/env bash
# Copyright 2026 The tierd Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Writes a shields.io-style "coverage | NN.N%" SVG badge.
#
# Usage: hack/coverage-badge.sh <percent> <output.svg>
# e.g.   hack/coverage-badge.sh 48.0 coverage.svg
set -euo pipefail

pct="${1:?usage: $0 <percent> <output.svg>}"
out="${2:?usage: $0 <percent> <output.svg>}"

if ! [[ "$pct" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then
	echo "percent must be a number, got '$pct'" >&2
	exit 1
fi

# Color by whole-number coverage, matching shields.io's palette.
whole="${pct%%.*}"
if ((whole >= 80)); then
	color="#4c1"     # brightgreen
elif ((whole >= 70)); then
	color="#97ca00"  # green
elif ((whole >= 60)); then
	color="#a4a61d"  # yellowgreen
elif ((whole >= 50)); then
	color="#dfb317"  # yellow
else
	color="#e05d44"  # red
fi

label="coverage"
value="${pct}%"
# Approximate Verdana 11px width: ~7px per character plus padding.
lw=$((${#label} * 7 + 10))
vw=$((${#value} * 7 + 10))
w=$((lw + vw))

cat >"$out" <<EOF
<svg xmlns="http://www.w3.org/2000/svg" width="$w" height="20" role="img" aria-label="$label: $value">
  <title>$label: $value</title>
  <linearGradient id="s" x2="0" y2="100%">
    <stop offset="0" stop-color="#bbb" stop-opacity=".1"/>
    <stop offset="1" stop-opacity=".1"/>
  </linearGradient>
  <clipPath id="r"><rect width="$w" height="20" rx="3" fill="#fff"/></clipPath>
  <g clip-path="url(#r)">
    <rect width="$lw" height="20" fill="#555"/>
    <rect x="$lw" width="$vw" height="20" fill="$color"/>
    <rect width="$w" height="20" fill="url(#s)"/>
  </g>
  <g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">
    <text x="$((lw / 2))" y="15" fill="#010101" fill-opacity=".3">$label</text>
    <text x="$((lw / 2))" y="14">$label</text>
    <text x="$((lw + vw / 2))" y="15" fill="#010101" fill-opacity=".3">$value</text>
    <text x="$((lw + vw / 2))" y="14">$value</text>
  </g>
</svg>
EOF
