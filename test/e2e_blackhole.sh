#!/usr/bin/env bash
# End-to-end check used for v0.1.0: run as root on a box where a hub (HUB, default http://127.0.0.1:8686)
# already mirrors 3x-ui (with get.acme.sh toggled on). It blackholes GitHub in /etc/hosts, runs the REAL
# 3x-ui install.sh download functions through the /i/3x-ui wrapper, then restores /etc/hosts.
set -u
HUB="${HUB:-http://127.0.0.1:8686}"
cp /etc/hosts /tmp/hosts.sm-bak && trap "cp /tmp/hosts.sm-bak /etc/hosts" EXIT
echo "0.0.0.0 github.com www.github.com api.github.com raw.githubusercontent.com objects.githubusercontent.com release-assets.githubusercontent.com codeload.github.com get.acme.sh" >> /etc/hosts
unset https_proxy HTTPS_PROXY http_proxy HTTP_PROXY ALL_PROXY all_proxy
curl -s "$HUB/i/3x-ui?trace=1" | sed '$d' > wrapper.sh          # wrapper minus its final eval line
cat >> wrapper.sh <<'INNER'
set -e
arch() { case "$(uname -m)" in x86_64) echo amd64;; aarch64) echo arm64;; *) echo armv7;; esac; }
# load the REAL functions from the REAL install.sh, fetched via its original GitHub URL
source <(curl -Ls https://raw.githubusercontent.com/mhsanaei/3x-ui/master/install.sh | sed -n '/^resolve_latest_tag()/,/^}/p; /^verify_release_checksum()/,/^}/p; /^require_repo_files()/,/^}/p')
green=""; red=""; yellow=""; plain=""
tag_version=$(resolve_latest_tag); echo "TAG=$tag_version"
curl -fLR --retry 5 -o /tmp/xui-arm64.tar.gz https://github.com/MHSanaei/3x-ui/releases/download/${tag_version}/x-ui-linux-$(arch).tar.gz
verify_release_checksum "https://github.com/MHSanaei/3x-ui/releases/download/${tag_version}/x-ui-linux-$(arch).tar.gz" /tmp/xui-arm64.tar.gz
script_ref="$tag_version"
require_repo_files "$script_ref" x-ui.sh && echo "require_repo_files OK"
curl -fLRo /tmp/x-ui.sh "https://raw.githubusercontent.com/MHSanaei/3x-ui/${script_ref}/x-ui.sh"; echo "x-ui.sh $(wc -c </tmp/x-ui.sh) bytes"
curl -fLRo /tmp/x-ui.service.debian "https://raw.githubusercontent.com/MHSanaei/3x-ui/${script_ref}/x-ui.service.debian"; echo "unit $(wc -c </tmp/x-ui.service.debian) bytes"
echo "--- API latest via plain grep (fallback path):"
curl -Ls "https://api.github.com/repos/MHSanaei/3x-ui/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/'
echo "--- dash child process (PATH shim), get.acme.sh piped into sh:"
curl -s https://get.acme.sh | sh -c 'grep -m1 -o "raw.githubusercontent.com/[^\"]*"'
sh -c 'curl -s https://raw.githubusercontent.com/acmesh-official/acme.sh/master/acme.sh | head -c 60; echo'
echo "--- curl, latest/download form (x-ui.sh geo update):"
curl -fsSL -o /tmp/geoip.dat https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geoip.dat; ls -la /tmp/geoip.dat
echo "--- pinned version via ?v= is tested separately; tarball:"
tar -tzf /tmp/xui-arm64.tar.gz | head -3
echo "--- real GitHub must be unreachable:"
/usr/bin/curl -s --max-time 5 -o /dev/null https://github.com/ && echo "WARNING: github.com reachable" || echo "github.com unreachable (ok)"
INNER
bash wrapper.sh
