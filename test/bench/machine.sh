#!/bin/sh
# Prints the machine a benchmark runs on, as a Markdown table: a benchmark
# result means little without it. make bench and make bench-media save it
# next to their results; docs/bench copies it above every table.
#
#   test/bench/machine.sh [OPENH264_LIBRARY]
set -u

row() { printf '| %s | %s |\n' "$1" "$2"; }
first() { sed -n "s/^$1[[:space:]]*:[[:space:]]*//p" | head -n 1; }

printf '| Item | Value |\n|------|-------|\n'
row Date "$(date -u '+%Y-%m-%d %H:%M UTC')"
case "$(uname -s)" in
Linux)
	cpu=$(first 'model name' </proc/cpuinfo)
	row CPU "${cpu:-unknown} ($(uname -m))"
	if command -v lscpu >/dev/null 2>&1; then
		l=$(LC_ALL=C lscpu)
		row Cores "$(echo "$l" | first 'CPU(s)') logical: $(echo "$l" | first 'Socket(s)') socket(s) × $(echo "$l" | first 'Core(s) per socket') core(s) × $(echo "$l" | first 'Thread(s) per core') thread(s)"
		row Caches "L1d $(echo "$l" | first 'L1d cache'), L2 $(echo "$l" | first 'L2 cache'), L3 $(echo "$l" | first 'L3 cache')"
		hv=$(echo "$l" | first 'Hypervisor vendor')
		if [ -n "$hv" ]; then
			row Virtualization "virtual machine ($hv hypervisor)"
		else
			row Virtualization "none detected"
		fi
	else
		row Cores "$(getconf _NPROCESSORS_ONLN) logical"
	fi
	flags=$(grep -m1 -o -w -E 'avx2|avx512f|avx512bw|bmi2|sha_ni|gfni|vaes|neon|asimd' /proc/cpuinfo | sort -u | tr '\n' ' ' | sed 's/ $//')
	row 'Instruction sets' "${flags:-none of AVX2, AVX-512, SHA-NI, GFNI}"
	row RAM "$(awk '/MemTotal/ {printf "%.1f GiB", $2 / 1048576}' /proc/meminfo)"
	os=$(sed -n 's/^PRETTY_NAME="\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' /etc/os-release 2>/dev/null)
	row OS "${os:-Linux}, kernel $(uname -r)"
	if command -v findmnt >/dev/null 2>&1; then
		row 'Work directory' "$(findmnt -n -o FSTYPE,SOURCE --target "${TMPDIR:-/tmp}" | tr -s ' ' | sed 's/ / on /') (${TMPDIR:-/tmp})"
	fi
	;;
Darwin)
	row CPU "$(sysctl -n machdep.cpu.brand_string) ($(uname -m))"
	row Cores "$(sysctl -n hw.logicalcpu) logical, $(sysctl -n hw.physicalcpu) physical"
	row RAM "$(($(sysctl -n hw.memsize) / 1073741824)) GiB"
	row OS "macOS $(sw_vers -productVersion), kernel $(uname -r)"
	;;
*)
	row OS "$(uname -sr) ($(uname -m))"
	;;
esac
if command -v go >/dev/null 2>&1; then
	row Go "$(go env GOVERSION) $(go env GOOS)/$(go env GOARCH), GOMAXPROCS ${GOMAXPROCS:-all}"
fi
if [ -n "${1:-}" ]; then
	row OpenH264 "$(basename "$1")"
fi
