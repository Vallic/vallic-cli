#!/usr/bin/env bash
#
# Installs the vallic CLI.
#
#   curl -fsSL https://raw.githubusercontent.com/Vallic/vallic-cli/main/installer.sh | bash
#
# What it needs: curl, and either sha256sum or shasum. Nothing else — no jq, no
# python, no unzip. A one-line installer that pulls in a package manager first
# is not a one-line installer.
#
# ## Where the version comes from, and why it is not GitHub
#
# The control plane is asked, not the releases API: `GET /api/vc/v1/cli/latest`
# answers the version it publishes and the SHA-256 of each build, to anybody,
# with no credential. Two reasons that is the right source.
#
# The checksum is one. GitHub holds the bytes, the control plane holds the
# digest, and they are different systems on purpose: a digest served by whoever
# served the binary can only say the download arrived intact, and says nothing
# about a release that was replaced at the source. An installer that trusted
# GitHub's own checksum would be verifying nothing an attacker could not also
# have written.
#
# The other is that the control plane decides what a customer should run. A tag
# can exist on GitHub — a release candidate, a build cut and then thought
# better of — without being the version anybody should install. `self-update`
# reads exactly this endpoint, so the binary this installs and the binary it
# upgrades itself to are chosen by the same authority.
#
# Override the control plane with VALLIC_API, which is what a test estate wants.
set -euo pipefail

API="${VALLIC_API:-https://console.vallic.com}"
BINARY=vallic

# Said on stderr, so a script that captures stdout gets nothing but the path.
say() { printf '%s\n' "$*" >&2; }
die() { printf 'installer: %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "this needs $1 on PATH."; }

# Go's own spelling of both halves, because that is how the manifest is keyed
# and how the release assets are named. uname says something different for
# every one of them.
platform() {
  local os arch
  os="$(uname -s)"
  arch="$(uname -m)"

  case "$os" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) die "no build for $os. Linux and macOS only; see the README for why Windows is absent." ;;
  esac

  case "$arch" in
    x86_64 | amd64) arch=amd64 ;;
    arm64 | aarch64) arch=arm64 ;;
    *) die "no build for $arch." ;;
  esac

  printf '%s/%s' "$os" "$arch"
}

# The manifest, with PHP's escaped slashes turned back into slashes.
#
# json_encode escapes every forward slash by default, so the real response says
# "linux\/amd64" and "https:\/\/github.com". Unescaping once here means the
# extraction below can be written against the shape a person would expect,
# rather than every pattern carrying backslashes nobody would guess at.
manifest() {
  curl -fsSL --max-time 30 "$API/api/vc/v1/cli/latest" | sed 's#\\/#/#g'
}

# One field out of one platform's entry.
#
# Deliberately narrow: it matches the object under this exact platform key and
# reads one string from it. A general JSON parser in sed is a bad idea and this
# is not one — if the shape changes, this stops finding anything and the
# installer says so rather than installing something unverified.
field() {
  local json="$1" platform="$2" key="$3" entry
  entry="$(printf '%s' "$json" | sed -n "s|.*\"$platform\":{\([^}]*\)}.*|\1|p")"

  printf '%s' "$entry" | sed -n "s|.*\"$key\":\"\([^\"]*\)\".*|\1|p"
}

# Whether a package manager owns the file this is about to write.
#
# The question is about the *destination*, not about whatever happens to be
# first on PATH. Writing over Homebrew's or a snap's copy leaves a package whose
# manifest no longer describes what is on disk, and the answer there is to use
# that tool. A copy somewhere else on PATH is a different matter entirely and is
# only worth mentioning — see shadowed().
#
# `/usr/bin` is deliberately *not* here. It is where a distribution's packages
# land and also where people put binaries by hand, this cannot tell the two
# apart without shelling out to rpm or dpkg, and refusing on the guess is how
# an installer tells somebody to use a package manager that never installed it.
# Overwriting a hand-placed file is what they asked for; overwriting a packaged
# one is refused above by path, where the path is actually distinctive.
owned_by_package() {
  local target="$1"

  # Resolved, because Homebrew's bin is links into the Cellar.
  if command -v readlink >/dev/null 2>&1; then
    target="$(readlink -f "$target" 2>/dev/null || printf '%s' "$target")"
  fi

  case "$target" in
    */Cellar/* | /opt/homebrew/* | /home/linuxbrew/.linuxbrew/*)
      say "$target belongs to Homebrew."
      say "Upgrade it with: brew upgrade vallic"
      return 0
      ;;
    /snap/* | /var/lib/snapd/*)
      say "$target belongs to snap."
      say "Upgrade it with: snap refresh vallic"
      return 0
      ;;
    /nix/store/*)
      say "$target belongs to Nix, which does not permit writing over it."
      return 0
      ;;
  esac

  return 1
}

# Another copy earlier on PATH than the one just installed.
#
# Said rather than refused. Somebody with an old build in /usr/bin and a new one
# in ~/.local/bin will run the old one and have no idea why the version they
# just installed is not the version they get, and that is a worse outcome than
# a line of output nobody needed.
shadowed() {
  local installed="$1" first
  first="$(command -v "$BINARY" 2>/dev/null || true)"

  [ -n "$first" ] && [ "$first" != "$installed" ]
}

# Where to put it: asked for, or the first writable of the usual two.
#
# /usr/local/bin is on PATH almost everywhere and needs root almost nowhere on
# a developer's machine. ~/.local/bin is the fallback rather than the default
# because it is on PATH by default on fewer systems, and an installer whose
# binary is not on PATH has not finished.
install_dir() {
  if [ -n "${VALLIC_INSTALL_DIR:-}" ]; then
    printf '%s' "$VALLIC_INSTALL_DIR"
    return
  fi

  if [ -w /usr/local/bin ] 2>/dev/null; then
    printf '/usr/local/bin'
    return
  fi

  printf '%s/.local/bin' "${HOME}"
}

main() {
  need curl
  need uname

  local sha_tool
  if command -v sha256sum >/dev/null 2>&1; then
    sha_tool=sha256sum
  elif command -v shasum >/dev/null 2>&1; then
    sha_tool="shasum -a 256"
  else
    die "this needs sha256sum or shasum to verify the download."
  fi

  local key json version url want
  key="$(platform)"

  json="$(manifest)" || die "could not reach $API. Set VALLIC_API if your control plane is elsewhere."

  case "$json" in
    *'"published":true'*) ;;
    *) die "$API publishes no version of this binary yet." ;;
  esac

  version="$(printf '%s' "$json" | sed -n 's|.*"version":"\([^"]*\)".*|\1|p')"
  url="$(field "$json" "$key" url)"
  want="$(field "$json" "$key" sha256)"

  [ -n "$version" ] || die "$API did not name a version."

  # Both halves or neither. A URL with no checksum is a binary nobody
  # verified, which is the one thing this script exists to prevent.
  if [ -z "$url" ] || [ -z "$want" ]; then
    die "$API publishes no verifiable build for $key."
  fi

  local dir tmp
  dir="$(install_dir)"

  # Asked before anything is downloaded: a refusal that first spends somebody's
  # bandwidth is a slower way to say the same thing.
  if [ -e "$dir/$BINARY" ] && owned_by_package "$dir/$BINARY"; then
    exit 0
  fi

  mkdir -p "$dir" || die "cannot create $dir."
  [ -w "$dir" ] || die "cannot write to $dir. Re-run with sudo, or set VALLIC_INSTALL_DIR."

  # In the target directory, so the move at the end is a rename within one
  # filesystem and therefore atomic. A download to /tmp and a copy across
  # devices can leave half a binary at the destination.
  tmp="$(mktemp "$dir/.$BINARY.XXXXXX")" || die "cannot write a temporary file in $dir."
  trap 'rm -f "$tmp"' EXIT

  say "Downloading $BINARY $version for $key."
  curl -fsSL --max-time 300 -o "$tmp" "$url" || die "could not download $url"

  local got
  got="$($sha_tool "$tmp" | cut -d' ' -f1)"

  if [ "$got" != "$want" ]; then
    die "checksum mismatch: $API expects $want, the download is $got. Nothing was installed."
  fi

  chmod 0755 "$tmp"
  mv -f "$tmp" "$dir/$BINARY"
  trap - EXIT

  say "Installed $dir/$BINARY"

  case ":${PATH}:" in
    *":$dir:"*)
      if shadowed "$dir/$BINARY"; then
        say ""
        say "Note: $(command -v "$BINARY") comes first on your PATH, so that is what runs."
        say "Remove it, or put $dir earlier."
      else
        say "Run: $BINARY login"
      fi
      ;;
    *)
      say ""
      say "$dir is not on your PATH. Add it:"
      say "    export PATH=\"$dir:\$PATH\""
      ;;
  esac
}

main "$@"
