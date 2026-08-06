#!/usr/bin/env bash
# Builds the tested Sandbox Runtime release from its verified upstream source.
set -o errexit
set -o nounset
set -o pipefail

readonly srt_version="0.0.70"
readonly srt_commit="44ab607c46f20381aeaf3e22ca0e0151d4c6b29c"
readonly source_sha256="5fc9680a0431bb9172eba591f5289756b8d57a5353941b139df4106c000979f0"
readonly source_url="https://github.com/anthropic-experimental/sandbox-runtime/archive/${srt_commit}.tar.gz"
readonly installer_schema="2"

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
platform="$(node -p 'process.platform')"
architecture="$(node -p 'process.arch')"
seccomp_helper=""
if [[ "${platform}" == "linux" ]]; then
	case "${architecture}" in
	x64 | arm64) seccomp_helper="node_modules/@anthropic-ai/sandbox-runtime/vendor/seccomp/${architecture}/apply-seccomp" ;;
	*)
		echo "unsupported Linux architecture for srt seccomp helper: ${architecture}" >&2
		exit 1
		;;
	esac
fi

destination="${1:-}"
if [[ -z "${destination}" || "${destination}" != /* ]]; then
	echo "usage: $0 /absolute/install/directory" >&2
	exit 2
fi

for command in curl node npm tar; do
	if ! command -v "${command}" >/dev/null 2>&1; then
		echo "required command not found: ${command}" >&2
		exit 1
	fi
done

installed_bin="${destination}/node_modules/.bin/srt"
installed_package="${destination}/node_modules/@anthropic-ai/sandbox-runtime/package.json"
installed_provenance="${destination}/INSTALL_PROVENANCE"
if [[ -e "${destination}" ]]; then
	if [[ -x "${installed_bin}" && -f "${installed_package}" && -f "${installed_provenance}" ]]; then
		installed_name="$(node -p "require(process.argv[1]).name" "${installed_package}")"
		installed_version="$(node -p "require(process.argv[1]).version" "${installed_package}")"
		if [[ "${installed_name}" == "@anthropic-ai/sandbox-runtime" && "${installed_version}" == "${srt_version}" ]] \
			&& grep -Fxq "installer_schema=${installer_schema}" "${installed_provenance}" \
			&& grep -Fxq "source_commit=${srt_commit}" "${installed_provenance}" \
			&& grep -Fxq "source_sha256=${source_sha256}" "${installed_provenance}" \
			&& grep -Fxq "platform=${platform}" "${installed_provenance}" \
			&& grep -Fxq "architecture=${architecture}" "${installed_provenance}" \
			&& { [[ -z "${seccomp_helper}" ]] || [[ -x "${destination}/${seccomp_helper}" ]]; }; then
			printf '%s\n' "${installed_bin}"
			exit 0
		fi
	fi
	echo "destination already exists and is not the tested srt ${srt_version} install: ${destination}" >&2
	exit 1
fi

tmp="$(mktemp -d "${TMPDIR:-/tmp}/prow-ai-dashboard-srt.XXXXXX")"
trap 'rm -rf "${tmp}"' EXIT
archive="${tmp}/source.tar.gz"

curl --fail --silent --show-error --location --retry 3 --output "${archive}" "${source_url}"
if command -v sha256sum >/dev/null 2>&1; then
	if ! printf '%s  %s\n' "${source_sha256}" "${archive}" | sha256sum -c - >/dev/null; then
		echo "srt source SHA-256 mismatch" >&2
		exit 1
	fi
else
	actual_sha256="$(shasum -a 256 "${archive}" | awk '{print $1}')"
	if [[ "${actual_sha256}" != "${source_sha256}" ]]; then
		echo "srt source SHA-256 mismatch: got ${actual_sha256}, want ${source_sha256}" >&2
		exit 1
	fi
fi

tar -xzf "${archive}" -C "${tmp}"
source_dir="${tmp}/sandbox-runtime-${srt_commit}"
package_json="${source_dir}/package.json"
if [[ ! -f "${package_json}" ]]; then
	echo "verified srt source archive did not contain package.json" >&2
	exit 1
fi
source_version="$(node -p "require(process.argv[1]).version" "${package_json}")"
if [[ "${source_version}" != "${srt_version}" ]]; then
	echo "srt source version mismatch: got ${source_version}, want ${srt_version}" >&2
	exit 1
fi

if [[ "${platform}" == "linux" ]]; then
	for command in gcc strip; do
		if ! command -v "${command}" >/dev/null 2>&1; then
			echo "required Linux srt build command not found: ${command}" >&2
			exit 1
		fi
	done
	node "${script_dir}/build-srt-seccomp.mjs" "${source_dir}" >&2
fi

(
	cd "${source_dir}"
	npm ci --ignore-scripts --no-audit --no-fund >&2
	npm run build >&2
	npm ci --ignore-scripts --no-audit --no-fund --omit=dev >&2
)

stage="${tmp}/install"
runtime_package="${stage}/node_modules/@anthropic-ai/sandbox-runtime"
mkdir -p "${stage}/node_modules" "${runtime_package}"
cp -R "${source_dir}/node_modules/." "${stage}/node_modules/"
cp "${source_dir}/package.json" "${source_dir}/package-lock.json" "${source_dir}/README.md" "${source_dir}/LICENSE" "${runtime_package}/"
cp -R "${source_dir}/dist" "${source_dir}/vendor" "${runtime_package}/"
chmod +x "${runtime_package}/dist/cli.js"
mkdir -p "${stage}/node_modules/.bin"
ln -s ../@anthropic-ai/sandbox-runtime/dist/cli.js "${stage}/node_modules/.bin/srt"
cat > "${stage}/INSTALL_PROVENANCE" <<EOF_PROVENANCE
package=@anthropic-ai/sandbox-runtime
version=${srt_version}
installer_schema=${installer_schema}
source_commit=${srt_commit}
source_sha256=${source_sha256}
platform=${platform}
architecture=${architecture}
EOF_PROVENANCE
staged_version="$(node -p "require(process.argv[1]).version" "${stage}/node_modules/@anthropic-ai/sandbox-runtime/package.json")"
if [[ "${staged_version}" != "${srt_version}" || ! -x "${stage}/node_modules/.bin/srt" ]] \
	|| { [[ -n "${seccomp_helper}" ]] && [[ ! -x "${stage}/${seccomp_helper}" ]]; }; then
	echo "built srt install failed verification" >&2
	exit 1
fi

mkdir -p "$(dirname "${destination}")"
mv "${stage}" "${destination}"
printf '%s\n' "${installed_bin}"
