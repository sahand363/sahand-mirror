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
# __sm_rw URL: sets REPLY to the hub URL; returns 0 when the URL targets the hub.
__sm_rw() {
  local u="$1" p low
  REPLY="$u"
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
`

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
	w("export __SM_BIN=\"$(mktemp -d /tmp/sahand-mirror.XXXXXX)\"\n")
	w("trap 'rm -rf \"$__SM_BIN\"' EXIT\n")
	if o.CAPEM != "" {
		w("export __SM_CA=\"$__SM_BIN/hub-ca.crt\"\ncat >\"$__SM_CA\" <<'SM_CA_EOF'\n%sSM_CA_EOF\n", o.CAPEM)
	} else {
		w("export __SM_CA=''\n")
	}
	b.WriteString(wrapperFuncs)
	b.WriteString("export -f __sm_enc __sm_rw __sm_real __sm_run curl wget\n")
	// PATH shims: any child (sh, python, make...) calling curl/wget is rewritten too
	b.WriteString(`for __t in curl wget; do
  { echo '#!/usr/bin/env bash'; declare -f __sm_enc __sm_rw __sm_real __sm_run; echo "__sm_run $__t \"\$@\""; } >"$__SM_BIN/$__t"
  chmod +x "$__SM_BIN/$__t"
done
export PATH="$__SM_BIN:$PATH"
`)
	w("echo -e \"\\033[1;36m[sahand-mirror]\\033[0m نصب %s از هاب داخلی: $MIRROR_URL\" >&2\n", o.App.ID)
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
