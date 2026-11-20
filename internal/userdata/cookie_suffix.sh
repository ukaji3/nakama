# Defines nakama_set_cookie_suffix (Issue #118).
#
# Shared by the code-server user data and the update script. Browsers
# do not separate cookies by port, so two instances opened over SSM at
# localhost:8080 and localhost:8081 would overwrite each other's
# code-server session cookie. code-server 4.107.0 added cookie-suffix,
# which renames the cookie to code-server-session-<suffix>; nakama sets
# it to the instance name.
#
# Older code-server versions refuse to start on an option they do not
# know ("Unknown option"), so the option is only written for 4.107.0
# and later. A cookie-suffix the user set is left alone.
#
# Fragment contract: no shebang, no `set` of its own; the function
# returns 0 whatever happens, so it never aborts the enclosing script.
nakama_set_cookie_suffix() {
  local cfg="$1" name="$2" user="$3" ver
  ver="$(sudo -u "${user}" code-server --version 2>/dev/null | head -n1 | awk '{print $1}')" || ver=""
  if [ -z "${ver}" ]; then
    echo "nakama: could not read the code-server version; cookie-suffix left unset"
    return 0
  fi
  if [ "$(printf '%s\n%s\n' 4.107.0 "${ver}" | sort -V | head -n1)" != 4.107.0 ]; then
    echo "nakama: code-server ${ver} has no cookie-suffix (it needs 4.107.0 or later); left unset"
    return 0
  fi
  if [ ! -f "${cfg}" ]; then
    echo "nakama: ${cfg} not found; cookie-suffix left unset"
    return 0
  fi
  if grep -q '^cookie-suffix:' "${cfg}"; then
    echo "nakama: cookie-suffix is already set in ${cfg}; left as it is"
    return 0
  fi
  if printf 'cookie-suffix: %s\n' "${name}" >> "${cfg}"; then
    echo "nakama: set cookie-suffix to ${name} in ${cfg}"
  else
    echo "nakama: WARNING could not write cookie-suffix to ${cfg}"
  fi
  return 0
}
