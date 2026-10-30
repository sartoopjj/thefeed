#!/bin/bash
# Builds the Android AAR one ABI at a time and merges the results.
#
# gomobile builds all -target slices at once, and github.com/gotd/td/tg is
# one 881k-line package, so four ABIs means four copies of that compile in
# memory together. GOFLAGS=-p does not limit slices, only packages in one.
#
# Usage: scripts/gomobile-android-aar.sh <out.aar> <ldflags>
set -euo pipefail

out=$1
ldflags=$2
abis=(arm arm64 386 amd64)

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

for abi in "${abis[@]}"; do
  echo "== android/$abi"
  gomobile bind -target="android/$abi" -androidapi 24 \
    -ldflags="$ldflags" -o "$work/$abi/mobile.aar" ./mobile
done

merged="$work/merged"
mkdir -p "$merged"
unzip -q "$work/${abis[0]}/mobile.aar" -d "$merged"

# The Java side comes from the Go API alone, so every slice must carry the
# same classes. Compare names and CRCs, not bytes: zip timestamps differ.
classes() { unzip -p "$1" classes.jar > "$work/c.jar"; unzip -v "$work/c.jar" | awk '$8 ~ /\.class$/ {print $7, $8}' | sort; }
want=$(classes "$work/${abis[0]}/mobile.aar")
[ -n "$want" ] || { echo "no classes in the ${abis[0]} AAR" >&2; exit 1; }

for abi in "${abis[@]:1}"; do
  got=$(classes "$work/$abi/mobile.aar")
  [ "$got" = "$want" ] || { echo "android/$abi has different Java classes from ${abis[0]}" >&2; exit 1; }
  unzip -q "$work/$abi/mobile.aar" 'jni/*' -d "$merged"
done

n=$(ls "$merged/jni" | wc -l)
[ "$n" -eq "${#abis[@]}" ] || { echo "merged AAR has $n ABIs, want ${#abis[@]}: $(ls "$merged/jni")" >&2; exit 1; }

mkdir -p "$(dirname "$out")"
rm -f "$out"
out_abs="$(cd "$(dirname "$out")" && pwd)/$(basename "$out")"
(cd "$merged" && zip -qr "$out_abs" .)
ls "$merged/jni"
