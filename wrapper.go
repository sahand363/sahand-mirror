package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The rewrite logic lives in bash so the same functions can be exported to
// child bash processes AND copied into PATH shims (for sh/dash children such
// as `curl https://get.acme.sh | sh`).
const wrapperFuncs = `__sm_enc() {
  local LC_ALL=C s="$1" o="" c i
  for ((i = 0; i < ${#s}; i++)); do
    c="${s:i:1}"
    case "$c" in [a-zA-Z0-9.~_-]) o+="$c" ;; *) printf -v c '%%%02X' "'$c"; o+="$c" ;; esac
  done
  printf '%s' "$o"
}
# __sm_ip URL: IP-detection services (with or without scheme) -> hub /myip
__sm_ip() {
  local p="$1" h rest q=""
  [ -n "${__SM_IPH:-}" ] || return 1
  p="${p#http://}"; p="${p#https://}"
  h="${p%%[/?#]*}"; rest="${p:${#h}}"
  case "$rest" in *\?*) q="?${rest#*\?}"; rest="${rest%%\?*}" ;; esac
  rest="${rest%/}"; h="${h,,}"
  case " ${__SM_IPH} " in *" $h$rest "*) REPLY="${MIRROR_URL:-}/myip$q"; return 0 ;; esac
  return 1
}
# __sm_rw URL: sets REPLY to the hub URL; returns 0 when the URL targets the hub.
__sm_rw() {
  local u="$1" p low
  REPLY="$u"
  __sm_ip "$u" && return 0
  case "$u" in http://*|https://*) ;; *) return 1 ;; esac
  [ "${u#"${MIRROR_URL:-}"}" != "$u" ] && return 0
  p="${u#*://}"
  case "$p" in
    github.com/*)                     REPLY="${MIRROR_URL:-}/gh/web/${p#github.com/}" ;;
    www.github.com/*)                 REPLY="${MIRROR_URL:-}/gh/web/${p#www.github.com/}" ;;
    api.github.com/*)                 REPLY="${MIRROR_URL:-}/gh/api/${p#api.github.com/}" ;;
    raw.githubusercontent.com/*)      REPLY="${MIRROR_URL:-}/gh/raw/${p#raw.githubusercontent.com/}" ;;
    codeload.github.com/*)            REPLY="${MIRROR_URL:-}/gh/codeload/${p#codeload.github.com/}" ;;
    objects.githubusercontent.com/*|release-assets.githubusercontent.com/*) REPLY="${MIRROR_URL:-}/gh/objects/${p#*/}" ;;
    *)
      if [[ $'\n'"${__SM_EXT:-}"$'\n' == *$'\n'"$u"$'\n'* ]]; then
        REPLY="${MIRROR_URL:-}/ext?u=$(__sm_enc "$u")"
      else
        return 1
      fi ;;
  esac
  if [ -n "${__SM_PIN_TAG:-}" ]; then
    low="${REPLY,,}"
    case "$low" in
      "${MIRROR_URL,,}/gh/web/${__SM_PIN_REPO:-}/releases/latest") REPLY="${MIRROR_URL:-}/gh/web/${__SM_PIN_REPO:-}/releases/tag/${__SM_PIN_TAG:-}" ;;
      "${MIRROR_URL,,}/gh/web/${__SM_PIN_REPO:-}/releases/latest/download/"*) REPLY="${MIRROR_URL:-}/gh/web/${__SM_PIN_REPO:-}/releases/download/${__SM_PIN_TAG:-}/${REPLY##*/}" ;;
      "${MIRROR_URL,,}/gh/api/repos/${__SM_PIN_REPO:-}/releases/latest") REPLY="${MIRROR_URL:-}/gh/api/repos/${__SM_PIN_REPO:-}/releases/tags/${__SM_PIN_TAG:-}" ;;
    esac
  fi
  return 0
}
__sm_real() {
  local IFS=: d
  for d in $PATH; do
    [ "$d" = "${__SM_BIN:-}" ] && continue
    [ -x "$d/$1" ] && { printf '%s' "$d/$1"; return 0; }
  done
  return 1
}
__sm_run() {
  local tool="$1" real x hit=0 a=()
  shift
  for x in "$@"; do
    if __sm_rw "$x"; then hit=1; fi
    a+=("$REPLY")
  done
  real="$(__sm_real "$tool")" || { echo "sahand-mirror: $tool not found" >&2; return 127; }
  if [ "$hit" = 1 ]; then
    if [ "$tool" = curl ]; then
      [ -n "${__SM_KEY:-}" ] && a=(-H "X-Mirror-Key: ${__SM_KEY:-}" "${a[@]}")
      [ -n "${__SM_CA:-}" ] && a=(--cacert "${__SM_CA:-}" "${a[@]}")
    else
      [ -n "${__SM_KEY:-}" ] && a=("--header=X-Mirror-Key: ${__SM_KEY:-}" "${a[@]}")
      [ -n "${__SM_CA:-}" ] && a=("--ca-certificate=${__SM_CA:-}" "${a[@]}")
    fi
  fi
  [ -n "${__SM_TRACE:-}" ] && echo "[sahand-mirror] $tool ${a[*]}" >&2
  "$real" "${a[@]}"
}
curl() { __sm_run curl "$@"; }
wget() { __sm_run wget "$@"; }
# apt through the hub's caching proxy, for this install only
__sm_apt_prep() {
  local d="${__SM_BIN:-/tmp}/.apt" e="${__SM_APT_ETC:-/etc/apt}" f hp host port re hosts=() bad=()
  [ -e "$d/.done" ] && return 0
  mkdir -p "$d/sources.list.d" && : >"$d/.done"
  [ "${__SM_APT_HTTPS:-auto}" = keep ] && return 0
  mapfile -t hosts < <(cat "$e/sources.list" "$e"/sources.list.d/*.list "$e"/sources.list.d/*.sources 2>/dev/null | grep -v '^[[:space:]]*#' | grep -oE 'https://[^/[:space:]"]+' | sed 's#^https://##' | sort -u)
  for hp in "${hosts[@]}"; do
    hp="${hp##*@}"; host="${hp%%:*}"; port=443
    [ "$host" != "$hp" ] && port="${hp##*:}"
    timeout 4 bash -c "exec 3<>/dev/tcp/$host/$port" 2>/dev/null || bad+=("$host")
  done
  [ ${#bad[@]} -eq 0 ] && return 0
  re="https://([^/@ ]*@)?($(IFS='|'; echo "${bad[*]//./\\.}"))([:/ ]|\$)"
  if [ -f "$e/sources.list" ]; then grep -Ev "$re" "$e/sources.list" >"$d/sources.list"; else : >"$d/sources.list"; fi
  for f in "$e"/sources.list.d/*; do
    [ -f "$f" ] || continue
    case "$f" in
      *.list) grep -Ev "$re" "$f" >"$d/sources.list.d/${f##*/}" ;;
      *.sources) awk -v re="$re" 'BEGIN { RS = ""; ORS = "\n\n" } $0 !~ re' "$f" >"$d/sources.list.d/${f##*/}" ;;
    esac
  done
  : >"$d/.filtered"
  echo "[sahand-mirror] apt: unreachable https repos skipped for this install: ${bad[*]}" >&2
}
__sm_apt() {
  local tool="$1" real o=() sub="" x skip=0 rc
  shift
  real="$(__sm_real "$tool")" || { echo "sahand-mirror: $tool not found" >&2; return 127; }
  if [ -n "${__SM_APT_PROXY:-}" ]; then
    __sm_apt_prep
    o=(-o "Acquire::http::Proxy=${__SM_APT_PROXY}" -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=15 -o Acquire::Retries=1)
    if [ -e "${__SM_BIN:-/tmp}/.apt/.filtered" ]; then
      o+=(-o "Dir::Etc::SourceList=${__SM_BIN}/.apt/sources.list" -o "Dir::Etc::SourceParts=${__SM_BIN}/.apt/sources.list.d")
    fi
  fi
  for x in "$@"; do
    if [ "$skip" = 1 ]; then skip=0; continue; fi
    case "$x" in -o|-c|-t) skip=1 ;; -*) ;; *) sub="$x"; break ;; esac
  done
  [ -n "${__SM_TRACE:-}" ] && echo "[sahand-mirror] $tool ${o[*]} $*" >&2
  "$real" "${o[@]}" "$@"; rc=$?
  if [ "$rc" != 0 ] && [ "$sub" = update ] && [ -n "${__SM_APT_PROXY:-}" ]; then
    echo "[sahand-mirror] apt update reported errors (unreachable repos?); continuing with the lists apt has" >&2
    return 0
  fi
  return $rc
}
`

const wrapperFuncNames = "__sm_enc __sm_ip __sm_rw __sm_real __sm_run __sm_apt_prep __sm_apt"

type wrapperOpts struct {
	Hub      string // MIRROR_URL
	App      *App
	Key      string
	CAPEM    string // set when the hub is HTTPS with the internal CA
	PinRepo  string
	PinTag   string
	Env      map[string]string
	Args     []string
	Externs  []string // mirrored external URLs
	AptHint  string
	TraceAll bool
	AptProxy string // proxy URL for apt (empty = apt untouched)
	AptHTTPS string // auto | keep
}

// aptProxyURL builds the Acquire::http::Proxy value (key as basic-auth password).
func aptProxyURL(hub, key string) string {
	hub = strings.TrimRight(hub, "/")
	if !strings.HasPrefix(hub, "http://") {
		return "" // apt cannot use an https proxy reliably
	}
	if key != "" {
		return "http://sm:" + key + "@" + strings.TrimPrefix(hub, "http://") + "/"
	}
	return hub + "/"
}

var pipeShellRe = regexp.MustCompile(`\|\s*(sudo\s+)?(bash|sh)\s*$`)

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// recipeCommand swaps the script URL in the original command for the hub's
// copy of the edited recipe, keeping the command's shape (bash <(...) or | bash).
func recipeCommand(cmd, scriptURL, recipeURL string) string {
	if scriptURL == "" {
		return "bash <(curl -fsSL " + shQuote(recipeURL) + ")"
	}
	return strings.Replace(cmd, scriptURL, recipeURL, 1)
}

func buildWrapper(o wrapperOpts) string {
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }
	w("#!/usr/bin/env bash\n# sahand-mirror install wrapper for %s\n", o.App.ID)
	w("export MIRROR_URL=%s SAHAND_MIRROR=1\n", shQuote(strings.TrimRight(o.Hub, "/")))
	exts := append([]string(nil), o.Externs...)
	for _, e := range o.Externs {
		if strings.HasSuffix(e, "/") {
			exts = append(exts, strings.TrimSuffix(e, "/"))
		} else {
			exts = append(exts, e+"/")
		}
	}
	sort.Strings(exts)
	w("export __SM_EXT=%s\n", shQuote(strings.Join(exts, "\n")))
	w("export __SM_KEY=%s __SM_PIN_REPO=%s __SM_PIN_TAG=%s\n", shQuote(o.Key), shQuote(strings.ToLower(o.PinRepo)), shQuote(o.PinTag))
	trace := ""
	if o.TraceAll {
		trace = "1"
	}
	w("export __SM_TRACE=%s\n", shQuote(trace))
	w("export __SM_IPH=%s\n", shQuote(strings.Join(ipDetectEntries, " ")))
	https := o.AptHTTPS
	if https != "keep" {
		https = "auto"
	}
	w("export __SM_APT_PROXY=%s __SM_APT_HTTPS=%s\n", shQuote(o.AptProxy), shQuote(https))
	w("export __SM_BIN=\"$(mktemp -d /tmp/sahand-mirror.XXXXXX)\"\n")
	w("trap 'rm -rf \"$__SM_BIN\"' EXIT\n")
	if o.CAPEM != "" {
		w("export __SM_CA=\"$__SM_BIN/hub-ca.crt\"\ncat >\"$__SM_CA\" <<'SM_CA_EOF'\n%sSM_CA_EOF\n", o.CAPEM)
	} else {
		w("export __SM_CA=''\n")
	}
	b.WriteString(wrapperFuncs)
	b.WriteString("export -f " + wrapperFuncNames + " curl wget\n")
	if o.AptProxy != "" {
		b.WriteString("apt-get() { __sm_apt apt-get \"$@\"; }\napt() { __sm_apt apt \"$@\"; }\nexport -f apt-get apt\n")
	}
	// PATH shims: any child (sh, python, make...) calling curl/wget/apt-get is rewritten too
	tools := "curl wget"
	if o.AptProxy != "" {
		tools += " apt-get apt"
	}
	b.WriteString(`for __t in ` + tools + `; do
  __f=__sm_run; case "$__t" in apt|apt-get) __f=__sm_apt ;; esac
  { echo '#!/usr/bin/env bash'; declare -f ` + wrapperFuncNames + `; echo "$__f $__t \"\$@\""; } >"$__SM_BIN/$__t"
  chmod +x "$__SM_BIN/$__t"
done
export PATH="$__SM_BIN:$PATH"
`)
	w("echo -e \"\\033[1;36m[sahand-mirror]\\033[0m installing %s from internal hub: $MIRROR_URL\" >&2\n", o.App.ID)
	if o.AptProxy != "" {
		w("echo \"[sahand-mirror] apt-get/apt use the hub apt cache (this install only)\" >&2\n")
	}
	if o.AptHint != "" {
		w("echo \"[sahand-mirror] apt/OS mirror hint: %s\" >&2\n", strings.ReplaceAll(o.AptHint, "\"", ""))
	}
	keys := make([]string, 0, len(o.Env))
	for k := range o.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if envNameRe.MatchString(k) {
			w("export %s=%s\n", k, shQuote(o.Env[k]))
		}
	}
	cmd := o.App.Command
	if o.App.Recipe != "" {
		cmd = recipeCommand(cmd, firstURL(cmd), strings.TrimRight(o.Hub, "/")+"/i/"+o.App.ID+"/recipe")
	}
	if cmd == "" {
		w("echo 'sahand-mirror: no install command for this app; download files from %s/d' >&2\nexit 1\n", o.Hub)
		return b.String()
	}
	args := ""
	if len(o.Args) > 0 && pipeShellRe.MatchString(cmd) {
		args = " -s --" // `curl ... | bash` needs -s -- to pass arguments to the script
	}
	for _, a := range o.Args {
		args += " " + shQuote(a)
	}
	w("eval %s\n", shQuote(cmd+args))
	return b.String()
}
