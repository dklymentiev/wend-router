#!/bin/sh
# Install wend on Linux or macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/dklymentiev/wend-router/main/install.sh | sh
#
# With Go 1.21 or newer it builds wend from source; with Docker it runs it in a
# container; with neither it asks whether to fetch Go just for the build or to download
# the ready binary. Everything goes into one folder (~/wend; WEND_DIR to change it),
# because wend keeps its keys and settings next to the binary. Nothing is installed
# system-wide and nothing needs root. Then it starts wend and sends one test request.
#
# WEND_BASE: where the release files come from (a mirror, or a local copy).
set -eu

BASE="${WEND_BASE:-https://github.com/dklymentiev/wend-router/releases/latest/download}"
DIR="${WEND_DIR:-$HOME/wend}"
URL="http://127.0.0.1:8888"

say() { printf '%s\n' "$*"; }
die() { say "wend install: $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }
fetch() { curl -fsSL "$1" -o "$2" || die "could not download $1"; }

# Questions go to the terminal, never to stdin: under `curl | sh` stdin is this script.
ask() {
	printf '%s ' "$1"
	if { read -r answer </dev/tty; } 2>/dev/null; then :; else answer=; say "(no terminal, taking the default)"; fi
}

have curl || die "curl is needed"
case $(uname -s) in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) die "this script is for Linux and macOS; on Windows use install.ps1" ;;
esac
case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "no build for a $(uname -m) processor; build from source with Go" ;;
esac

tmp=$(mktemp -d)
trap 'chmod -R u+w "$tmp" 2>/dev/null; rm -rf "$tmp"' EXIT # Go leaves read-only files
mkdir -p "$DIR"

sha256() { if have sha256sum; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }
# every downloaded release file is checked against SHA256SUMS of the same release
checked() {
	fetch "$BASE/$1" "$tmp/$1"
	[ -f "$tmp/SHA256SUMS" ] || fetch "$BASE/SHA256SUMS" "$tmp/SHA256SUMS"
	want=$(awk -v f="$1" '$2 == f || $2 == "*" f {print $1}' "$tmp/SHA256SUMS")
	[ -n "$want" ] && [ "$want" = "$(sha256 "$tmp/$1")" ] || die "checksum of $1 does not match SHA256SUMS"
}
source_tree() {
	checked wend-src.tar.gz
	mkdir -p "$1" && tar -xzf "$tmp/wend-src.tar.gz" -C "$1"
}

# Go 1.21 and newer fetch the toolchain go.mod asks for on their own
go_ok() {
	have go || return 1
	minor=$(go env GOVERSION 2>/dev/null | sed -n 's/^go1\.\([0-9]*\).*/\1/p')
	[ -n "$minor" ] && [ "$minor" -ge 21 ]
}
build() { # $1: the go command
	say "Building wend from source with $("$1" env GOVERSION) ..."
	source_tree "$tmp/src"
	(cd "$tmp/src" && GOTOOLCHAIN=auto CGO_ENABLED=0 "$1" build -trimpath -o "$tmp/wend" ./cmd/wend) || die "the build failed"
	install_binary
}
install_binary() {
	chmod +x "$tmp/wend"
	mv -f "$tmp/wend" "$DIR/wend" # a rename, so a running old copy is not disturbed
}

stop_old() {
	[ -f "$DIR/wend.pid" ] && kill "$(cat "$DIR/wend.pid")" 2>/dev/null && sleep 1
	rm -f "$DIR/wend.pid"
	if curl -fs -m 2 "$URL/health" >/dev/null 2>&1; then
		die "something already answers on $URL; stop it first (or run wend on another port with ROUTER_ADDR)"
	fi
}
start_binary() {
	stop_old
	(cd "$DIR" && nohup ./wend >wend.log 2>&1 & echo $! >"$DIR/wend.pid")
}
start_docker() {
	# an earlier container goes down first; the source is unpacked over the old one,
	# which keeps a wend.env put next to compose.yaml
	if [ -f "$DIR/src/compose.yaml" ]; then (cd "$DIR/src" && docker compose -p wend down) || true; fi
	stop_old
	source_tree "$DIR/src"
	say "Building and starting the container ..."
	(cd "$DIR/src" && docker compose -p wend up -d --build) || die "docker compose failed"
}

how=
if go_ok; then
	how=go
elif have docker && docker info >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
	how=docker
else
	say "Neither Go nor Docker is here. wend can be"
	say "  1) built from source, with Go downloaded just for this build (about 70 MB, deleted after)"
	say "  2) downloaded ready-made: wend-$os-$arch, checked against SHA256SUMS"
	ask "Choose 1 or 2 [1]:"
	case ${answer:-1} in
		1) how=fetch-go ;;
		2) how=binary ;;
		*) die "nothing chosen" ;;
	esac
fi

case $how in
	go) build go; start_binary ;;
	fetch-go)
		gv=$(curl -fsSL "https://go.dev/VERSION?m=text" | head -n 1)
		say "Downloading $gv for this build ..."
		fetch "https://go.dev/dl/$gv.$os-$arch.tar.gz" "$tmp/go.tgz"
		tar -xzf "$tmp/go.tgz" -C "$tmp"
		GOPATH="$tmp/gopath" GOCACHE="$tmp/gocache" build "$tmp/go/bin/go"
		start_binary
		;;
	binary)
		checked "wend-$os-$arch"
		mv "$tmp/wend-$os-$arch" "$tmp/wend"
		install_binary
		start_binary
		;;
	docker) start_docker ;;
esac

say "Waiting for Wend router to start ..."
i=0
until curl -fs -m 2 "$URL/health" >/dev/null 2>&1; do
	i=$((i + 1))
	[ $i -lt 60 ] || die "wend did not start; see $DIR/wend.log"
	sleep 1
done

# One real request. Right after the start the router is still reading the providers'
# model lists, so give it a minute.
say "Sending a test request ..."
i=0
while :; do
	reply=$(curl -fs -m 60 "$URL/v1/chat/completions" -H "Content-Type: application/json" \
		-d '{"model":"no-think","max_tokens":80,"messages":[{"role":"user","content":"Say hello to a new user of wend in one short sentence."}]}' 2>/dev/null) && break
	i=$((i + 1))
	[ $i -lt 6 ] || { reply=; break; }
	sleep 10
done
if [ -n "$reply" ]; then
	model=$(printf '%s' "$reply" | sed -n 's/.*"model": *"\([^"]*\)".*/\1/p')
	text=$(printf '%s' "$reply" | sed -n 's/.*"content": *"\([^"]*\)".*/\1/p' | sed 's/\\n/ /g; s/^ *//')
	say "$model answered: $text"
else
	say "No model answered yet. Open the dashboard and add a provider key (some providers need none)."
fi

case $how in
	docker) stop_how="cd $DIR/src && docker compose -p wend down" ; start_how="cd $DIR/src && docker compose -p wend up -d" ;;
	*) stop_how="kill \$(cat $DIR/wend.pid)" ; start_how="cd $DIR && ./wend" ;;
esac
say ""
say "Wend router is running: $URL/dashboard"
say "  agents connect to $URL/v1 with any API key and the model \"auto\""
say "  stop:  $stop_how"
say "  start: $start_how"
[ "$how" = docker ] || say "  its keys, settings and log are in $DIR"
if [ "$os" = darwin ]; then open "$URL/dashboard" 2>/dev/null || true
elif [ -n "${DISPLAY:-}${WAYLAND_DISPLAY:-}" ] && have xdg-open; then xdg-open "$URL/dashboard" >/dev/null 2>&1 || true
fi
