# Machine-local rundevrun development toggle.
rdr-dev-on() {
  if [[ ${_RDR_DEV_ACTIVE:-0} == 1 ]]; then
    print 'rdr development mode is already on'
    return 0
  fi
  local devbin="$HOME/.local/opt/rundevrun-dev/bin"
  if [[ ! -x "$devbin/rdr" ]]; then
    print -u2 "Missing development binary: $devbin/rdr"
    return 1
  fi
  typeset -g _RDR_DEV_OLD_PATH="$PATH"
  typeset -g _RDR_DEV_HOME_WAS_SET=${+RDR_HOME}
  typeset -g _RDR_DEV_OLD_HOME="${RDR_HOME-}"
  typeset -g _RDR_DEV_ACTIVE=1
  export PATH="$devbin:$PATH"
  export RDR_HOME="$HOME/.local/state/rundevrun-dev"
  rehash
  command -v rdr
  print -r -- "RDR_HOME=$RDR_HOME"
}

rdr-dev-off() {
  if [[ ${_RDR_DEV_ACTIVE:-0} != 1 ]]; then
    print 'rdr development mode is already off'
    return 0
  fi
  export PATH="$_RDR_DEV_OLD_PATH"
  if [[ $_RDR_DEV_HOME_WAS_SET == 1 ]]; then
    export RDR_HOME="$_RDR_DEV_OLD_HOME"
  else
    unset RDR_HOME
  fi
  unset _RDR_DEV_ACTIVE _RDR_DEV_OLD_PATH _RDR_DEV_HOME_WAS_SET _RDR_DEV_OLD_HOME
  rehash
  if ! command -v rdr; then
    print 'Development mode off; no installed rdr found in PATH'
  fi
}
