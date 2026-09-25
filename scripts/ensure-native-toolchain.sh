#!/usr/bin/env bash
# Install what the in-process semantic-router plugin needs to build its Rust
# native libraries: a C compiler, cmake (onnx-binding's turbojpeg-sys), and
# Rust/cargo. Already-installed tools are left untouched.
#
# Usage:
#   scripts/ensure-native-toolchain.sh
#   TOOLCHAIN_AUTO_INSTALL=0 scripts/ensure-native-toolchain.sh   # report only
#
# Go and make are not installed here: both are version-sensitive base
# prerequisites, so a missing one is reported with the command to run.

set -euo pipefail

AUTO_INSTALL="${TOOLCHAIN_AUTO_INSTALL:-1}"
OPERATING_SYSTEM="$(uname -s)"

# A rustup install in this run lands here before any shell profile is reloaded.
export PATH="${HOME}/.cargo/bin:${PATH}"

MISSING_INSTRUCTIONS=()

have() { command -v "$1" >/dev/null 2>&1; }

have_c_compiler() { have cc || have clang || have gcc; }

report_missing() {
	MISSING_INSTRUCTIONS+=("$1")
}

# linux_install installs packages with whichever package manager is present.
linux_install() {
	local sudo_prefix=""
	if [[ "$(id -u)" != "0" ]]; then
		if have sudo; then
			sudo_prefix="sudo"
		else
			return 1
		fi
	fi

	if have apt-get; then
		$sudo_prefix apt-get update
		$sudo_prefix apt-get install -y "$@"
	elif have dnf; then
		$sudo_prefix dnf install -y "$@"
	elif have yum; then
		$sudo_prefix yum install -y "$@"
	elif have pacman; then
		$sudo_prefix pacman -Sy --noconfirm "$@"
	elif have apk; then
		$sudo_prefix apk add --no-cache "$@"
	else
		return 1
	fi
}

ensure_c_compiler() {
	if have_c_compiler; then
		echo "  cc/clang/gcc: present"
		return
	fi

	if [[ "$AUTO_INSTALL" != "1" ]]; then
		report_missing "C compiler (macOS: xcode-select --install | Linux: install build-essential)"
		return
	fi

	echo "  cc/clang/gcc: missing — installing"
	case "$OPERATING_SYSTEM" in
	Darwin)
		# The installer is a GUI flow, so it cannot finish inside this script.
		xcode-select --install >/dev/null 2>&1 || true
		report_missing "Xcode Command Line Tools: finish the 'xcode-select --install' dialog, then re-run"
		;;
	Linux)
		if have apt-get; then
			linux_install build-essential || report_missing "C compiler: sudo apt-get install -y build-essential"
		else
			linux_install gcc make || report_missing "C compiler: install gcc and make with your package manager"
		fi
		;;
	*)
		report_missing "C compiler: install a toolchain for $OPERATING_SYSTEM"
		;;
	esac
}

ensure_cmake() {
	if have cmake; then
		echo "  cmake: present"
		return
	fi

	if [[ "$AUTO_INSTALL" != "1" ]]; then
		report_missing "cmake (macOS: brew install cmake | Linux: install the cmake package)"
		return
	fi

	echo "  cmake: missing — installing"
	case "$OPERATING_SYSTEM" in
	Darwin)
		if have brew; then
			brew install cmake || report_missing "cmake: brew install cmake"
		else
			report_missing "cmake: install Homebrew (https://brew.sh) then 'brew install cmake'"
		fi
		;;
	Linux)
		linux_install cmake || report_missing "cmake: install the cmake package with your package manager"
		;;
	*)
		report_missing "cmake: install it for $OPERATING_SYSTEM"
		;;
	esac
}

ensure_rust() {
	if have cargo; then
		echo "  cargo: present ($(cargo --version 2>/dev/null | head -n 1))"
		return
	fi

	if [[ "$AUTO_INSTALL" != "1" ]]; then
		report_missing "Rust/cargo: https://rustup.rs"
		return
	fi

	echo "  cargo: missing — installing Rust via rustup"
	if ! have curl; then
		report_missing "Rust/cargo: install curl, then run the rustup installer from https://rustup.rs"
		return
	fi

	if curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --profile minimal; then
		# shellcheck source=/dev/null
		[[ -f "${HOME}/.cargo/env" ]] && . "${HOME}/.cargo/env"
		export PATH="${HOME}/.cargo/bin:${PATH}"
	fi

	if ! have cargo; then
		report_missing "Rust/cargo: rustup install did not expose cargo; see https://rustup.rs"
	fi
}

report_base_prerequisite() {
	local tool="$1" hint="$2"
	if have "$tool"; then
		echo "  $tool: present"
	else
		report_missing "$tool: $hint"
	fi
}

echo "Checking native build toolchain (semantic-router Rust libraries)..."
report_base_prerequisite make "install via Xcode Command Line Tools or your package manager"
report_base_prerequisite go "install Go 1.26.2+ from https://go.dev/dl/"
ensure_c_compiler
ensure_cmake
ensure_rust

if ((${#MISSING_INSTRUCTIONS[@]} > 0)); then
	echo ""
	echo "Native toolchain incomplete. Install the following, then re-run:"
	for instruction in "${MISSING_INSTRUCTIONS[@]}"; do
		echo "  - $instruction"
	done
	exit 1
fi

echo "Native build toolchain ready"
