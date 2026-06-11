#!/usr/bin/env bash
set -euo pipefail

usage() {
    echo "Usage: $0 --pattern <glob> --source <dir> --dest <dir>"
    exit 1
}

pattern=""
source_dir=""
dest_dir=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --pattern) pattern="$2"; shift 2 ;;
        --source)  source_dir="$2"; shift 2 ;;
        --dest)    dest_dir="$2"; shift 2 ;;
        *)         usage ;;
    esac
done

if [[ -z "$pattern" || -z "$source_dir" || -z "$dest_dir" ]]; then
    usage
fi

source_dir="${source_dir/#\~/$HOME}"
dest_dir="${dest_dir/#\~/$HOME}"

if [[ "$dest_dir" != /* ]]; then
    dest_dir="$source_dir/$dest_dir"
fi

if [[ ! -d "$source_dir" ]]; then
    echo "Error: source directory '$source_dir' does not exist."
    exit 1
fi

mkdir -p "$dest_dir"

shopt -s nullglob nocaseerror nocaseglob 2>/dev/null || true
files=("$source_dir"/$pattern)

if [[ ${#files[@]} -eq 0 ]]; then
    echo "No files matching '$pattern' found in '$source_dir'."
    exit 0
fi

moved=0
for f in "${files[@]}"; do
    [[ -f "$f" ]] || continue
    mv "$f" "$dest_dir/"
    moved=$((moved + 1))
done

echo "Moved $moved file(s) from '$source_dir' to '$dest_dir'."
