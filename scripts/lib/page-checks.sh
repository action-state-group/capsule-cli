# shellcheck shell=bash
# page-checks.sh -- helpers the page checks share (check-html-render.sh,
# check-portability.sh). Source it from bash; it defines functions only.
#
#   find_chrome                 sets chrome to CHROME, or the first Chrome or
#                               Chromium found; returns 1 if none is
#   check WHAT CMD...           prints ok/FAIL for CMD and sets fail=1 on FAIL
#   contains FILE TEXT, lacks FILE TEXT, differs A B
#   render PAGE                 writes PAGE.dom: the DOM after the page's own
#                               scripts ran, in headless Chrome, offline
#   no_render_errors DOM        the error hook ran and saw nothing
#   page_bundle PAGE            prints the bundle JSON a page carries
#   replace_bundle PAGE JSON    prints PAGE with its bundle replaced by JSON's
#   rehash_bundle_csp ORIG EDIT re-pins EDIT's bundle script in its CSP

find_chrome() {
  chrome="${CHROME:-}"
  if [[ -z "$chrome" ]]; then
    local c
    for c in google-chrome google-chrome-stable chromium chromium-browser "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"; do
      if command -v "$c" >/dev/null 2>&1 || [[ -x "$c" ]]; then chrome="$c"; break; fi
    done
  fi
  [[ -n "$chrome" ]]
}

# shellcheck disable=SC2034 # read by the script that sources this file
fail=0
check() { # description, then a command that must succeed
  local what="$1"; shift
  if "$@"; then echo "ok   $what"; else echo "FAIL $what" >&2; fail=1; fi
}
contains() { grep -q -F -- "$2" "$1"; }
lacks() { ! contains "$@"; }
differs() { ! cmp -s "$1" "$2"; }

# render PAGE: the DOM after the page's own scripts ran. A copy of the page
# gets one script first in its head that records, by kind, every
# console.error, uncaught error and unhandled rejection into
# data-render-errors on <html>, which the DOM dump carries; it marks itself
# installed (data-render-hook), so a hook that never ran fails the check.
# Headless Chrome uses a throwaway profile next to the page, under the
# caller's temp directory (a --user-data-dir keeps it from exiting on
# macOS), never the system keychain (no macOS keychain dialog), and
# resolves no host name, so a page that needs the network renders without
# it; the alarm bounds a run that hangs anyway, which then fails for want of
# a DOM.
hook='<script>(function(){document.documentElement.setAttribute("data-render-hook","installed");var seen=[];function note(kind,detail){seen.push(kind+": "+String(detail));document.documentElement.setAttribute("data-render-errors",JSON.stringify(seen));}var error=console.error;console.error=function(){note("console.error",Array.prototype.join.call(arguments," "));return error.apply(console,arguments);};window.addEventListener("error",function(e){note("uncaught",e.message);});window.addEventListener("unhandledrejection",function(e){note("unhandled rejection",e.reason?e.reason.message||e.reason:e.reason);});})();</script>'
render() {
  local page
  page="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
  awk -v hook="$hook" '!done && (i = index($0, "<head>")) {$0 = substr($0, 1, i + 5) hook substr($0, i + 6); done = 1} {print}' "$page" >"$page.probe.html"
  perl -e 'alarm 60; exec @ARGV' "$chrome" --headless --no-sandbox --disable-gpu --no-first-run \
    --use-mock-keychain --password-store=basic --user-data-dir="$page.profile" --host-resolver-rules='MAP * ~NOTFOUND' \
    --virtual-time-budget=10000 --dump-dom "file://$page.probe.html" >"$page.dom" 2>/dev/null || true
  rm -rf "$page.profile"
}
no_render_errors() {
  if contains "$1" 'data-render-errors='; then
    grep -o 'data-render-errors="[^"]*"' "$1" | sed 's/^/     /' >&2
    return 1
  fi
  contains "$1" 'data-render-hook="installed"'
}

page_bundle() {
  perl -0ne 'print $1 if /window\.__BUNDLE__ = (.*?);<\/script>/s' "$1"
}
replace_bundle() {
  perl -0pe 'BEGIN{open my $f,"<",shift @ARGV or die; local $/; $j=<$f>; chomp $j; $j=~s/</\\u003c/g} s/(window\.__BUNDLE__ = ).*?(;<\/script>)/$1$j$2/s' "$2" "$1"
}

# rehash_bundle_csp ORIGINAL EDITED: the page's CSP pins the SHA-256 of every
# inline script, the bundle's included, so a page whose bundle text was edited
# does not run it at all. Whoever edits the file can edit the policy too: this
# puts the edited bundle script's hash where the original's was, so a check
# tests what the page itself says about an edited bundle.
rehash_bundle_csp() {
  perl -MDigest::SHA=sha256_base64 -e '
    sub h { my $b = sha256_base64($_[0]); $b .= "=" while length($b) % 4; return "sha256-$b" }
    local $/; open my $o, "<", $ARGV[0] or die; my $orig = <$o>; close $o;
    open my $e, "<", $ARGV[1] or die; my $page = <$e>; close $e;
    my ($old) = $orig =~ /<script>(window\.__BUNDLE__ = .*?;)<\/script>/s or die "no bundle script";
    my ($new) = $page =~ /<script>(window\.__BUNDLE__ = .*?;)<\/script>/s or die "no bundle script";
    my ($ho, $hn) = (h($old), h($new));
    $page =~ s/\Q$ho\E/$hn/ or die "the bundle script is not pinned in the CSP";
    open my $w, ">", $ARGV[1] or die; print $w $page; close $w;' "$1" "$2"
}
