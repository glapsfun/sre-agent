#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'Bun CI setup: %s\n' "$*" >&2
  exit 1
}

[[ "${GITHUB_ACTIONS:-}" == true ]] || fail 'requires GITHUB_ACTIONS=true; no changes made.'
[[ "$(uname -s)" == Linux ]] || fail 'requires Linux; no changes made.'
runner_uid="$(id -u)"
[[ "$runner_uid" != 0 ]] || fail 'requires an unprivileged runner user.'
[[ -n "${GITHUB_ENV:-}" && -f "$GITHUB_ENV" && ! -L "$GITHUB_ENV" && -w "$GITHUB_ENV" ]] \
  || fail 'requires a regular writable GITHUB_ENV file.'
[[ "$(stat -c %u "$GITHUB_ENV")" == "$runner_uid" ]] || fail 'GITHUB_ENV has a different owner.'
[[ -d /run/systemd/system ]] || fail 'requires systemd as the host service manager.'
[[ -f /sys/fs/cgroup/cgroup.controllers ]] || fail 'requires cgroup v2.'

has_controllers() {
  local available=" $1 " controller
  for controller in cpu memory pids; do
    [[ "$available" == *" $controller "* ]] || return 1
  done
}
has_controllers "$(cat /sys/fs/cgroup/cgroup.controllers)" || fail 'host lacks cpu/memory/pids controllers.'

missing_packages=()
for package in bubblewrap util-linux dbus-user-session; do
  if [[ "$(dpkg-query -W -f='${Status}' "$package" 2>/dev/null || true)" != 'install ok installed' ]]; then
    missing_packages+=("$package")
  fi
done
if ((${#missing_packages[@]})); then
  sudo -n apt-get update
  sudo -n apt-get install -y --no-install-recommends "${missing_packages[@]}"
fi
for tool in bun bwrap prlimit systemctl systemd-run; do
  command -v "$tool" >/dev/null || fail "missing $tool after setup."
done
[[ "$(bun --version)" == 1.4.2 ]] || fail 'requires Bun 1.4.2.'
bwrap_help="$(bwrap --help)"
for flag in --size --unshare-all --unshare-user --unshare-cgroup --seccomp --clearenv --remount-ro; do
  [[ "$bwrap_help" == *"$flag "* ]] || fail "installed bubblewrap lacks $flag."
done
bwrap --version
prlimit --version

runtime_unit="user-runtime-dir@${runner_uid}.service"
manager_unit="user@${runner_uid}.service"
runtime_dir="/run/user/${runner_uid}"
for unit in "$runtime_unit" "$manager_unit"; do
  if ! systemctl is-active --quiet "$unit"; then
    sudo -n systemctl start "$unit"
  fi
done

delegated="$(systemctl show "$manager_unit" --property=DelegateControllers --value)"
if ! has_controllers "$delegated"; then
  # Restart only this CI user's manager, and never the cgroup running this job.
  if grep -Fq "/${manager_unit}" /proc/self/cgroup; then
    fail 'cannot restart the user manager containing this job.'
  fi
  drop_in_dir="/run/systemd/system/${manager_unit}.d"
  drop_in="${drop_in_dir}/90-sre-bun-ci.conf"
  drop_in_text=$'[Service]\nDelegate=cpu memory pids'
  if [[ -e "$drop_in" || -L "$drop_in" ]]; then
    [[ -f "$drop_in" && ! -L "$drop_in" && "$(cat "$drop_in")" == "$drop_in_text" ]] \
      || fail "refusing to overwrite $drop_in."
  else
    sudo -n install -d -m 0755 "$drop_in_dir"
    printf '%s\n' "$drop_in_text" | sudo -n tee "$drop_in" >/dev/null
  fi
  sudo -n systemctl daemon-reload
  sudo -n systemctl restart "$manager_unit"
fi

[[ -d "$runtime_dir" && ! -L "$runtime_dir" && "$(realpath -e "$runtime_dir")" == "$runtime_dir" ]] \
  || fail 'invalid user runtime directory.'
[[ "$(stat -c %u "$runtime_dir")" == "$runner_uid" && "$(stat -c %a "$runtime_dir")" == 700 ]] \
  || fail 'user runtime directory ownership or mode is unsafe.'
export XDG_RUNTIME_DIR="$runtime_dir"
export DBUS_SESSION_BUS_ADDRESS="unix:path=${runtime_dir}/bus"
if ((${#missing_packages[@]})); then
  systemctl --user daemon-reload
fi
if ! systemctl --user is-active --quiet dbus.socket; then
  systemctl --user start dbus.socket
fi
[[ -S "$runtime_dir/bus" && ! -L "$runtime_dir/bus" && "$(realpath -e "$runtime_dir/bus")" == "$runtime_dir/bus" ]] \
  || fail 'invalid user bus socket path.'
[[ "$(stat -c %u "$runtime_dir/bus")" == "$runner_uid" ]] || fail 'user bus socket has a different owner.'
systemctl --user show --property=Version --value

has_controllers "$(systemctl show "$manager_unit" --property=DelegateControllers --value)" \
  || fail 'user manager lacks cpu/memory/pids delegation.'
manager_group="$(systemctl show "$manager_unit" --property=ControlGroup --value)"
[[ "$manager_group" == "/user.slice/user-${runner_uid}.slice/${manager_unit}" ]] \
  || fail 'unexpected user manager cgroup path.'
has_controllers "$(cat "/sys/fs/cgroup${manager_group}/cgroup.controllers")" \
  || fail 'cpu/memory/pids controllers are unavailable in the user manager cgroup.'

for setting in "XDG_RUNTIME_DIR=$XDG_RUNTIME_DIR" "DBUS_SESSION_BUS_ADDRESS=$DBUS_SESSION_BUS_ADDRESS"; do
  if ! grep -Fxq "$setting" "$GITHUB_ENV"; then
    printf '%s\n' "$setting" >>"$GITHUB_ENV"
  fi
done
printf '%s\n' 'Bun CI prerequisites prepared; required runtime tests verify effective isolation and limits.'
