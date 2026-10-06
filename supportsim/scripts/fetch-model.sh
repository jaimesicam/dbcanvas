#!/bin/sh
# fetch-model.sh DIR — download the all-MiniLM-L6-v2 weights and vocabulary
# that internal/embed loads at runtime, into DIR (default ./model).
#
# The Dockerfile runs this in its build stage so the ~90 MB of weights are
# baked into the image but never committed to the repo (see NOTICE). The
# Hugging Face revision is pinned and each file's SHA-256 checked, so a build
# never silently picks up a re-uploaded checkpoint: the package's tests pin
# reference embedding values that a different checkpoint would not reproduce.
# Override MODEL_REVISION (and the checksums) deliberately to move to a new one.
set -eu

DIR="${1:-./model}"
REPO="sentence-transformers/all-MiniLM-L6-v2"
MODEL_REVISION="${MODEL_REVISION:-1110a243fdf4706b3f48f1d95db1a4f5529b4d41}"
SAFETENSORS_SHA256="${SAFETENSORS_SHA256:-53aa51172d142c89d9012cce15ae4d6cc0ca6895895114379cacb4fab128d9db}"
VOCAB_SHA256="${VOCAB_SHA256:-07eced375cec144d27c900241f3e339478dec958f92fddbc551f295c992038a3}"

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

fetch() { # fetch FILE SHA256
	url="https://huggingface.co/$REPO/resolve/$MODEL_REVISION/$1"
	echo "fetch-model: $url"
	curl -fsSL --retry 3 -o "$DIR/$1.part" "$url"
	got="$(sha256 "$DIR/$1.part")"
	if [ "$got" != "$2" ]; then
		echo "fetch-model: $1: sha256 $got, want $2" >&2
		rm -f "$DIR/$1.part"
		exit 1
	fi
	mv "$DIR/$1.part" "$DIR/$1"
}

mkdir -p "$DIR"
fetch model.safetensors "$SAFETENSORS_SHA256"
fetch vocab.txt "$VOCAB_SHA256"
echo "fetch-model: done -> $DIR"
